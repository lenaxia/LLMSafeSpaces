// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package app

// wait_broker.go — the claim-wait lifecycle's event-resolution half
// (low-level design §4; tsk-pristine-henna-loses): a STATELESS
// reconciler, the board-reconciler posture — no in-memory scheduling
// state, one Postgres pass (30s cadence) + idempotent side effects:
//
//	1. read wait_declared board events past a watermark (the RC2
//	   pattern; deterministic ids make replays inert);
//	2. ensure the compiled trigger exists for each live event-wait
//	   (label select then create — idempotent; a re-declaration
//	   UPDATEs the same-name trigger's script so the fresh seed
//	   reaches the sensor under the same ownership);
//	3. delete broker-owned triggers whose row wait is GONE (cleared /
//	   claim-end / wedge / terminal) + wait_orphan_cleaned event;
//	4. consume delivered-but-unconsumed fires (trigger_fires audit)
//	   => send the wait-resolved sentinel to the claimant session +
//	   wait_resolved event (keyed on the fire id — the consume
//	   dedupe belt; the row clears on the worker's advance or the
//	   wedge reset — both orders converge);
//	5. advance the watermark.
//
// The BROKER consumes fires and sends the resume itself — the trigger
// is a script-gated sensor with NO prompt leg (a script trigger with
// no prompt never calls the LLM), so resolution is deterministic and
// testable end-to-end. Failure posture: a disabled
// (autoDisableAfter) or repeatedly-failing trigger is re-materialized
// ONCE (deterministic remat event), then wait_broker_degraded (loud
// event + the health surface line) — the wait falls to the ladder;
// honest silence, never a stuck green badge. Cross-replica:
// deterministic trigger names + the (owner, name) unique constraint
// arbitrate (two replicas converge).
//
// pkg/board stays POOR: it records the wait_declared event and knows
// nothing about triggers, GitHub, or resolution; the broker owns the
// trigger lifecycle through its own label namespace (wait-broker-*).

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/lenaxia/llmsafespaces/pkg/board"
	"github.com/lenaxia/llmsafespaces/pkg/types"
	"github.com/lenaxia/llmsafespaces/pkg/workflows"
)

// waitBrokerInterval is the broker's pass cadence (the design's 30s —
// the reconciler sweep's default posture; the SENSORS poll at */2, the
// broker only reconciles their lifecycle).
const waitBrokerInterval = 30 * time.Second

// waitBrokerSweepScope is the broker's watermark row in board_sweeps
// (the reconciler's 'default' scope, its own).
const waitBrokerSweepScope = "wait-broker"

// waitBrokerFiresScan is how many recent fires per trigger one pass
// inspects (fires are DESC by fired_at; the consume belt keys on the
// fire id so older consumed fires are inert on rescan).
const waitBrokerFiresScan = 25

// waitBrokerSurface is the broker's actor-surface literal on the
// events it writes (additive vocabulary beside the reconciler's — the
// audit trail names its writer honestly).
const waitBrokerSurface = "wait_broker"

// Deterministic event-id shapes (replays inert under ON CONFLICT DO
// NOTHING — the reconciler's convergence discipline).
const (
	waitResolvedEventPrefix  = "wait-resolved:"
	waitOrphanCleanedPrefix  = "wait-orphan-cleaned:"
	waitRematerializedPrefix = "wait-broker-remat:"
	waitBrokerDegradedPrefix = "wait-broker-degraded:"
)

// The broker's board-event action vocabulary — SHARED constants, not
// scattered literals: the recordEvent call sites use them, and the
// health-surface pins assert pkg/board's read edge references the same
// spelling (a rename anywhere reddens a test, review r0 F4 — the
// tautology class stays closed).
const (
	EventActionWaitResolved       = "wait_resolved"
	EventActionWaitOrphanCleaned  = "wait_orphan_cleaned"
	EventActionWaitBrokerRemat    = "wait_broker_rematerialized"
	EventActionWaitBrokerDegraded = "wait_broker_degraded"
)

// waitDeclaration is one wait_declared board event the broker
// consumed (the payload's condition is DATA; nothing executable ever
// crossed the worker surface).
type waitDeclaration struct {
	EventID    string
	ProjectID  string
	TaskID     string
	CreatedAt  time.Time
	Reason     string
	Condition  board.WaitCondition
	Repo       string
	ClaimToken string
	ShortID    string
	Assignee   string
}

// waitRowState is the row truth one pass keys its liveness decisions
// on (stage + wait_on + the claim the declaration anchored).
type waitRowState struct {
	TaskID      string
	ProjectID   string
	ShortID     string
	Stage       string
	WaitOn      string
	WaitReason  string
	ClaimToken  string
	Assignee    string
	UserID      string
	WorkspaceID string
}

// liveEventWait reports whether the row still carries THE declaration's
// event-wait: in_progress, wait_on='event', and the same claim token
// (a mint/reset/depose/wedge writes a new token or clears the wait —
// every one of those ends THIS episode's trigger ownership).
func (w *waitRowState) liveEventWait(claimToken string) bool {
	return w != nil &&
		w.Stage == board.StageInProgress &&
		w.WaitOn == board.WaitOnEvent &&
		w.ClaimToken != "" && claimToken != "" &&
		w.ClaimToken == claimToken
}

// waitFacts is the broker's board-truth seam (narrow so the pass
// matrix runs on a fake; production binds pgxpool — the RC2 pattern).
type waitFacts interface {
	// declarationsSince scans wait_declared events in (cutoff, high].
	declarationsSince(ctx context.Context, cutoff, high time.Time) ([]waitDeclaration, error)
	// taskWait reads the row's wait facts by task id.
	taskWait(ctx context.Context, taskID string) (*waitRowState, error)
	// taskWaitByShortID reads the row's wait facts by short id.
	taskWaitByShortID(ctx context.Context, shortID string) (*waitRowState, error)
	// latestDeclaration is the row's most recent wait_declared event
	// (zero value when none) — BOTH the fire-freshness belt (a
	// re-declared wait does not consume its predecessor's stale fires)
	// and the resume prose's condition/reason source.
	latestDeclaration(ctx context.Context, taskID string) (waitDeclaration, error)
	// eventExists reports whether a board event id already landed (the
	// consume/remat/degrade dedupe belts).
	eventExists(ctx context.Context, projectID, id string) (bool, error)
	// recordEvent writes one broker audit event (deterministic id; ON
	// CONFLICT DO NOTHING — replays are inert convergence).
	recordEvent(ctx context.Context, projectID, id, action, itemID string, payload map[string]any) error
	// watermark seeds+reads the broker's cursor and captures the
	// pre-pass event high-water (the reconciler's r1-C3 discipline:
	// events committed by concurrent writers DURING the pass stay
	// strictly greater than the returned high, so advancing to it never
	// skips them).
	watermark(ctx context.Context) (cutoff, high time.Time, err error)
	// advanceWatermark advances the cursor monotonically.
	advanceWatermark(ctx context.Context, to time.Time) error
}

// waitFactsDB is the production waitFacts over the board pool.
type waitFactsDB struct{ pool *pgxpool.Pool }

// waitDeclPayload is the wait_declared event payload's wire shape
// (pkg/board's declareWaitTx writes it; mirrored here so the broker
// stays decoupled from pkg/board internals).
type waitDeclPayload struct {
	WaitOn     string `json:"wait_on"`
	WaitReason string `json:"wait_reason"`
	Repo       string `json:"repo"`
	ShortID    string `json:"short_id"`
	Assignee   string `json:"assignee"`
	ClaimToken string `json:"claim_token"`
	Condition  struct {
		Primitive string `json:"primitive"`
		Arg       string `json:"arg"`
		Text      string `json:"text"`
	} `json:"condition"`
	Actor struct {
		ClaimToken string `json:"claim_token"`
	} `json:"actor"`
}

func (d *waitFactsDB) declarationsSince(ctx context.Context, cutoff, high time.Time) ([]waitDeclaration, error) {
	rows, err := d.pool.Query(ctx, `
		SELECT id, project_id::text, item_id::text, payload, created_at
		FROM board_events
		WHERE action='wait_declared' AND item_kind='task'
		  AND created_at > $1 AND created_at <= $2
		ORDER BY created_at, id`, cutoff, high)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []waitDeclaration
	for rows.Next() {
		var decl waitDeclaration
		var payload waitDeclPayload
		if err := rows.Scan(&decl.EventID, &decl.ProjectID, &decl.TaskID, &payload, &decl.CreatedAt); err != nil {
			return nil, err
		}
		decl.Reason = payload.WaitReason
		decl.Repo = payload.Repo
		decl.ShortID = payload.ShortID
		decl.Assignee = payload.Assignee
		decl.ClaimToken = payload.ClaimToken
		if decl.ClaimToken == "" {
			decl.ClaimToken = payload.Actor.ClaimToken // the pre-repo-payload belt
		}
		decl.Condition = board.WaitCondition{Primitive: payload.Condition.Primitive, Arg: payload.Condition.Arg}
		// The grammar re-parse: a hand-edited event row can never arm
		// an unknown primitive (the closed vocabulary is enforced at
		// the verb edge AND here).
		if _, err := board.ParseWaitCondition(decl.Condition.String()); err != nil {
			return nil, fmt.Errorf("wait broker: wait_declared %s carries an unparseable condition: %w", decl.EventID, err)
		}
		out = append(out, decl)
	}
	return out, rows.Err()
}

const waitRowStateSelect = `
	SELECT t.id::text, t.project_id::text, t.short_id, t.stage,
	       COALESCE(t.wait_on, ''), COALESCE(t.wait_reason, ''),
	       COALESCE(t.claim_token::text, ''),
	       COALESCE(t.assignee_session, ''), p.user_id, p.target_workspace_id::text
	FROM tasks t JOIN projects p ON p.id = t.project_id`

func (d *waitFactsDB) scanWaitRow(row pgx.Row) (*waitRowState, error) {
	var w waitRowState
	err := row.Scan(&w.TaskID, &w.ProjectID, &w.ShortID, &w.Stage, &w.WaitOn, &w.WaitReason, &w.ClaimToken, &w.Assignee, &w.UserID, &w.WorkspaceID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil // row gone — the orphan sweep's truth
	}
	if err != nil {
		return nil, err
	}
	return &w, nil
}

func (d *waitFactsDB) taskWait(ctx context.Context, taskID string) (*waitRowState, error) {
	return d.scanWaitRow(d.pool.QueryRow(ctx, waitRowStateSelect+` WHERE t.id = $1`, taskID))
}

func (d *waitFactsDB) taskWaitByShortID(ctx context.Context, shortID string) (*waitRowState, error) {
	return d.scanWaitRow(d.pool.QueryRow(ctx, waitRowStateSelect+` WHERE lower(t.short_id) = lower($1)`, shortID))
}

func (d *waitFactsDB) latestDeclaration(ctx context.Context, taskID string) (waitDeclaration, error) {
	var decl waitDeclaration
	var payload waitDeclPayload
	err := d.pool.QueryRow(ctx, `
		SELECT id, project_id::text, item_id::text, payload, created_at
		FROM board_events
		WHERE action='wait_declared' AND item_kind='task' AND item_id = $1::uuid
		ORDER BY created_at DESC LIMIT 1`, taskID).
		Scan(&decl.EventID, &decl.ProjectID, &decl.TaskID, &payload, &decl.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return waitDeclaration{}, nil
	}
	if err != nil {
		return waitDeclaration{}, err
	}
	decl.Reason = payload.WaitReason
	decl.Repo = payload.Repo
	decl.ShortID = payload.ShortID
	decl.Assignee = payload.Assignee
	decl.ClaimToken = payload.ClaimToken
	decl.Condition = board.WaitCondition{Primitive: payload.Condition.Primitive, Arg: payload.Condition.Arg}
	return decl, nil
}

func (d *waitFactsDB) eventExists(ctx context.Context, projectID, id string) (bool, error) {
	var one int
	err := d.pool.QueryRow(ctx,
		`SELECT 1 FROM board_events WHERE project_id=$1::uuid AND id=$2`, projectID, id).Scan(&one)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

func (d *waitFactsDB) recordEvent(ctx context.Context, projectID, id, action, itemID string, payload map[string]any) error {
	if payload == nil {
		payload = map[string]any{}
	}
	b, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	actor, err := json.Marshal(map[string]string{"kind": board.ActorSystem, "surface": waitBrokerSurface})
	if err != nil {
		return err
	}
	var item any
	if itemID != "" {
		item = itemID
	}
	_, err = d.pool.Exec(ctx, `
		INSERT INTO board_events (id, project_id, item_kind, item_id, action, actor, payload)
		VALUES ($1, $2::uuid, 'task', $3, $4, $5, $6)
		ON CONFLICT (project_id, id) DO NOTHING`,
		id, projectID, item, action, actor, b)
	return err
}

func (d *waitFactsDB) watermark(ctx context.Context) (time.Time, time.Time, error) {
	if _, err := d.pool.Exec(ctx, `
		INSERT INTO board_sweeps (scope, last_event_at) VALUES ($1, to_timestamp(0))
		ON CONFLICT (scope) DO NOTHING`, waitBrokerSweepScope); err != nil {
		return time.Time{}, time.Time{}, err
	}
	var cutoff time.Time
	if err := d.pool.QueryRow(ctx,
		`SELECT last_event_at FROM board_sweeps WHERE scope=$1`, waitBrokerSweepScope).Scan(&cutoff); err != nil {
		return time.Time{}, time.Time{}, err
	}
	var high *time.Time
	if err := d.pool.QueryRow(ctx, `SELECT max(created_at) FROM board_events`).Scan(&high); err != nil {
		return time.Time{}, time.Time{}, err
	}
	if high == nil {
		return cutoff, cutoff, nil
	}
	return cutoff, *high, nil
}

func (d *waitFactsDB) advanceWatermark(ctx context.Context, to time.Time) error {
	_, err := d.pool.Exec(ctx, `
		UPDATE board_sweeps SET last_event_at=$2, updated_at=now()
		WHERE scope=$1 AND last_event_at < $2`, waitBrokerSweepScope, to)
	return err
}

// TriggerPlane is the broker's trigger-lifecycle seam (create/delete/
// fires — the platform trigger store; narrow so the pass matrix runs
// on a fake).
type TriggerPlane interface {
	ListByPrefix(ctx context.Context, prefix string) ([]*workflows.TriggerRow, error)
	Create(ctx context.Context, row *workflows.TriggerRow) error
	UpdateScript(ctx context.Context, ownerType, ownerID, id, language, handler, description string) error
	ResetFailures(ctx context.Context, id string) error
	Delete(ctx context.Context, ownerType, ownerID, id string) error
	ListFires(ctx context.Context, triggerID string, limit int) ([]*workflows.TriggerFireRow, error)
}

// triggerPlaneStore is the production TriggerPlane over the workflows
// store (the same store the automation handlers ride).
type triggerPlaneStore struct{ store *workflows.Store }

func (t *triggerPlaneStore) ListByPrefix(ctx context.Context, prefix string) ([]*workflows.TriggerRow, error) {
	return t.store.ListTriggersByPrefix(ctx, prefix, 500)
}

func (t *triggerPlaneStore) Create(ctx context.Context, row *workflows.TriggerRow) error {
	err := t.store.CreateTrigger(ctx, row)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			// Cross-replica convergence: the deterministic name lost the
			// (owner, name) race another replica already won — inert.
			return nil
		}
	}
	return err
}

func (t *triggerPlaneStore) UpdateScript(ctx context.Context, ownerType, ownerID, id, language, handler, description string) error {
	enabled := true
	_, err := t.store.UpdateTrigger(ctx, ownerType, ownerID, id, &workflows.TriggerUpdate{
		Script:      &workflows.TriggerScript{Language: language, Handler: handler},
		Description: &description,
		Enabled:     &enabled, // a re-declaration re-arms a disabled sensor (fresh episode)
	})
	return err
}

func (t *triggerPlaneStore) ResetFailures(ctx context.Context, id string) error {
	return t.store.ResetTriggerFailures(ctx, id)
}

func (t *triggerPlaneStore) Delete(ctx context.Context, ownerType, ownerID, id string) error {
	return t.store.DeleteTrigger(ctx, ownerType, ownerID, id)
}

func (t *triggerPlaneStore) ListFires(ctx context.Context, triggerID string, limit int) ([]*workflows.TriggerFireRow, error) {
	return t.store.ListTriggerFires(ctx, triggerID, limit, 0)
}

// WaitBrokerResult carries the pass counters (observability only).
type WaitBrokerResult struct {
	Materialized   int
	UpdatedScripts int
	OrphansCleaned int
	Resolved       int
	Rematerialized int
	Degraded       int
}

// WaitBroker is the stateless event-wait reconciler.
type WaitBroker struct {
	facts         waitFacts
	triggers      TriggerPlane
	sender        boardV1Sender
	boardBaseURL  string // the story_done sensor's in-cluster default (belt; the pod's LLMSAFESPACE_API_URL wins)
	clock         func() time.Time
	interval      time.Duration
	logger        *slog.Logger
	deliverResume func(ctx context.Context, userID, workspaceID, session, text string) error
}

// NewWaitBroker constructs the broker over its seams (production:
// pgxpool facts + the workflows-store trigger plane + the phrase-
// resolving V1 sender the board seams share).
func NewWaitBroker(pool *pgxpool.Pool, store *workflows.Store, sender boardV1Sender, boardBaseURL string) *WaitBroker {
	return newWaitBroker(&waitFactsDB{pool: pool}, &triggerPlaneStore{store: store}, sender, boardBaseURL)
}

// newWaitBroker binds the broker over its seams (the fake-injectable
// core beneath NewWaitBroker — the pass-matrix suite rides this).
func newWaitBroker(facts waitFacts, triggers TriggerPlane, sender boardV1Sender, boardBaseURL string) *WaitBroker {
	b := &WaitBroker{
		facts:        facts,
		triggers:     triggers,
		sender:       sender,
		boardBaseURL: boardBaseURL,
		clock:        time.Now,
		interval:     waitBrokerInterval,
		logger:       slog.Default(),
	}
	b.deliverResume = func(ctx context.Context, userID, workspaceID, session, text string) error {
		return deliverBoardTextV1(ctx, b.sender, "wait-resolved resume", userID, workspaceID, session, text, boardDirectiveFastFail)
	}
	return b
}

// Start runs the broker loop (the reconciler Start posture: tick,
// pass, warn-and-converge on error, stop on ctx end).
func (b *WaitBroker) Start(ctx context.Context) error {
	t := time.NewTicker(b.interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
			if _, err := b.Pass(ctx); err != nil {
				slog.WarnContext(ctx, "board: wait-broker pass failed (next pass converges)", "error", err.Error())
			}
		}
	}
}

// waitResolvedSentinel is the resume message's agentmessage-v1 payload
// (the lease-nudge composer pattern — additive keys, plain-prose
// fallback for grammar-ignorant workers).
type waitResolvedSentinel struct {
	Mode      string `json:"mode"`
	TaskID    string `json:"taskId"`
	Condition string `json:"condition"`
	FireID    string `json:"fireId"`
}

// waitResolvedText composes the resume sentinel: the v1 marker line,
// then the prose contract (design §4.4 — resume, advance, or
// re-declare; ignore if the claim is gone).
func waitResolvedText(shortID, reason, condition, fireID string) string {
	text := agentMessageSentinelLine(waitResolvedSentinel{
		Mode:      "wait-resolved",
		TaskID:    shortID,
		Condition: condition,
		FireID:    fireID,
	})
	what := condition
	if reason != "" {
		what = reason + " (" + condition + ")"
	}
	return text + fmt.Sprintf("BOARD WAIT-RESOLVED: your event-wait on task %s resolved — %s. "+
		"Resume work, advance the stage, or re-declare a wait if more is still needed; "+
		"if you no longer hold the claim, ignore this message (the row moved on).",
		shortID, what)
}

// waitFireGateOpen reports whether a delivered fire's audit shows the
// gate OPENED ({gate:{proceed:true}} — engine_gate.go's action shape;
// a closed or failed gate is never a resolution).
func waitFireGateOpen(action json.RawMessage) bool {
	if len(action) == 0 {
		return false
	}
	var payload struct {
		Gate struct {
			Proceed bool `json:"proceed"`
		} `json:"gate"`
	}
	if err := json.Unmarshal(action, &payload); err != nil {
		return false
	}
	return payload.Gate.Proceed
}

// Pass runs one full reconciliation (the five steps; each side effect
// idempotent, every event id deterministic — a crash between passes
// replays to the same state).
func (b *WaitBroker) Pass(ctx context.Context) (WaitBrokerResult, error) {
	var res WaitBrokerResult
	cutoff, high, err := b.facts.watermark(ctx)
	if err != nil {
		return res, fmt.Errorf("wait broker: watermark: %w", err)
	}

	// --- 1+2: declarations => ensure the compiled triggers -----------
	decls, err := b.facts.declarationsSince(ctx, cutoff, high)
	if err != nil {
		return res, err
	}
	owned, err := b.triggers.ListByPrefix(ctx, waitBrokerTriggerPrefix)
	if err != nil {
		return res, fmt.Errorf("wait broker: trigger scan: %w", err)
	}
	byName := make(map[string]*workflows.TriggerRow, len(owned))
	for _, t := range owned {
		byName[t.Name] = t
	}
	for _, decl := range decls {
		row, err := b.facts.taskWait(ctx, decl.TaskID)
		if err != nil {
			return res, err
		}
		if !row.liveEventWait(decl.ClaimToken) {
			continue // declared then advanced/cleared — nothing to own
		}
		shortID := decl.ShortID
		if shortID == "" {
			shortID = row.ShortID
		}
		name := waitBrokerTriggerName(shortID, decl.ClaimToken)
		marker := waitBrokerMarkerPath(shortID, decl.ClaimToken)
		script, rerr := renderWaitGateScript(decl.Condition, decl.Repo, decl.EventID, marker, b.boardBaseURL, shortID)
		if rerr != nil {
			return res, rerr
		}
		if existing, ok := byName[name]; ok {
			if existing.ScriptHandler != script {
				// A within-episode re-declaration: the fresh seed must
				// reach the sensor under the SAME ownership (never a
				// second trigger — the one-wait-per-story cap's broker
				// half).
				if err := b.triggers.UpdateScript(ctx, existing.OwnerType, existing.OwnerID, existing.ID, "python", script, waitBrokerTriggerDescription(decl, shortID, decl.ClaimToken)); err != nil {
					return res, fmt.Errorf("wait broker: re-declare update %s: %w", name, err)
				}
				if err := b.triggers.ResetFailures(ctx, existing.ID); err != nil {
					b.logger.WarnContext(ctx, "wait broker: failure-reset missed on re-declare", "trigger", name, "error", err.Error())
				}
				byName[name].ScriptHandler = script
				res.UpdatedScripts++
			}
			continue
		}
		created, err := b.createTrigger(ctx, decl, row, shortID, name, script)
		if err != nil {
			return res, fmt.Errorf("wait broker: materialize %s: %w", name, err)
		}
		res.Materialized++
		// The created row joins the owned set for the phases below WITH
		// its store-issued uuid id (the fires scan keys on it — a name
		// is not a uuid; trigger_fires.trigger_id is uuid NOT NULL).
		byName[name] = created
		owned = append(owned, created)
	}

	// --- 3: orphan clean (row wait GONE => delete + audit) -----------
	gone := make(map[string]bool, len(owned)) // phase-B deletions (a set — NEVER a mutation of the row copies: Enabled is live truth phase D keys on)
	for _, trig := range owned {
		shortID, hash8, ok := parseWaitBrokerTriggerName(trig.Name)
		if !ok {
			continue // not ours to parse (foreign prefix collision)
		}
		row, err := b.facts.taskWaitByShortID(ctx, shortID)
		if err != nil {
			return res, err
		}
		live := row.liveEventWait(row.ClaimToken) && claimTokenHash8(row.ClaimToken) == hash8
		if live {
			continue
		}
		if err := b.triggers.Delete(ctx, trig.OwnerType, trig.OwnerID, trig.ID); err != nil && !errors.Is(err, workflows.ErrNotFound) {
			return res, fmt.Errorf("wait broker: orphan delete %s: %w", trig.Name, err)
		}
		reason := "cleared_or_claim_ended"
		if row == nil {
			reason = "row_gone"
		} else if row.Stage != board.StageInProgress {
			reason = "terminal_or_moved"
		} else if row.WaitOn != board.WaitOnEvent {
			reason = "wait_cleared" // cleared / wedge reset / conversion
		} else if claimTokenHash8(row.ClaimToken) != hash8 {
			reason = "claim_ended"
		}
		if row != nil {
			if err := b.facts.recordEvent(ctx, row.ProjectID, waitOrphanCleanedPrefix+trig.Name, EventActionWaitOrphanCleaned, row.TaskID,
				map[string]any{"task_id": row.TaskID, "trigger": trig.Name, "reason": reason}); err != nil {
				return res, err
			}
		}
		b.logger.InfoContext(ctx, "board: wait-broker orphan trigger cleaned", "trigger", trig.Name, "reason", reason)
		res.OrphansCleaned++
		gone[trig.Name] = true // skip the phases below
	}

	// --- 4: fire consume => resume sentinel + wait_resolved ----------
	for _, trig := range owned {
		if gone[trig.Name] {
			continue
		}
		shortID, _, ok := parseWaitBrokerTriggerName(trig.Name)
		if !ok {
			continue
		}
		row, err := b.facts.taskWaitByShortID(ctx, shortID)
		if err != nil {
			return res, err
		}
		if !row.liveEventWait(row.ClaimToken) {
			continue
		}
		decl, err := b.facts.latestDeclaration(ctx, row.TaskID)
		if err != nil {
			return res, err
		}
		fires, err := b.triggers.ListFires(ctx, trig.ID, waitBrokerFiresScan)
		if err != nil {
			return res, fmt.Errorf("wait broker: fires scan %s: %w", trig.Name, err)
		}
		for _, fire := range fires {
			if fire.Status != "delivered" || !waitFireGateOpen(fire.ActionResult) {
				continue // skipped = gate closed; failed = accounted below
			}
			if !decl.CreatedAt.IsZero() && !fire.FiredAt.After(decl.CreatedAt) {
				continue // stale fire predating the current declaration
			}
			resolvedID := waitResolvedEventPrefix + fire.ID
			if done, err := b.facts.eventExists(ctx, row.ProjectID, resolvedID); err != nil {
				return res, err
			} else if done {
				continue // consumed (the fire-id belt: replays and re-deliveries never re-send)
			}
			reason := row.WaitReason
			if reason == "" {
				reason = decl.Reason
			}
			if err := b.deliverResume(ctx, row.UserID, row.WorkspaceID, row.Assignee,
				waitResolvedText(row.ShortID, reason, decl.Condition.String(), fire.ID)); err != nil {
				// Bounced delivery (dead/unresolvable session): retry
				// next pass — the fire stays unconsumed until a
				// wait_resolved event lands.
				b.logger.WarnContext(ctx, "board: wait-broker resume delivery bounced (retrying next pass)",
					"trigger", trig.Name, "fire", fire.ID, "error", err.Error())
				continue
			}
			if err := b.facts.recordEvent(ctx, row.ProjectID, resolvedID, EventActionWaitResolved, row.TaskID,
				map[string]any{
					"task_id": row.TaskID, "fire_id": fire.ID, "trigger": trig.Name,
					"condition": decl.Condition.String(), "repo": decl.Repo, "delivered_to": row.Assignee,
				}); err != nil {
				return res, err
			}
			res.Resolved++
		}
	}

	// --- 5: degrade posture (disabled/failing => remat once, then loud)
	for _, trig := range owned {
		if gone[trig.Name] {
			continue
		}
		shortID, hash8, ok := parseWaitBrokerTriggerName(trig.Name)
		if !ok {
			continue
		}
		if trig.Enabled && trig.ConsecutiveFailures < waitBrokerAutoDisableAfter {
			continue // healthy
		}
		row, err := b.facts.taskWaitByShortID(ctx, shortID)
		if err != nil {
			return res, err
		}
		if !row.liveEventWait(row.ClaimToken) || claimTokenHash8(row.ClaimToken) != hash8 {
			continue // the orphan sweep already owns it
		}
		name := trig.Name
		cause := fmt.Sprintf("consecutive_failures=%d", trig.ConsecutiveFailures)
		if !trig.Enabled {
			cause = "auto_disabled"
		}
		rematID := waitRematerializedPrefix + name
		rematted, err := b.facts.eventExists(ctx, row.ProjectID, rematID)
		if err != nil {
			return res, err
		}
		if !rematted {
			// Re-materialize ONCE: delete + create fresh (counters and
			// enabled state reset with the row), audit the round.
			if err := b.triggers.Delete(ctx, trig.OwnerType, trig.OwnerID, trig.ID); err != nil && !errors.Is(err, workflows.ErrNotFound) {
				return res, fmt.Errorf("wait broker: remat delete %s: %w", name, err)
			}
			if err := b.rematerializeTrigger(ctx, row, shortID, name); err != nil {
				return res, fmt.Errorf("wait broker: remat create %s: %w", name, err)
			}
			if err := b.facts.recordEvent(ctx, row.ProjectID, rematID, EventActionWaitBrokerRemat, row.TaskID,
				map[string]any{"task_id": row.TaskID, "trigger": name, "cause": cause}); err != nil {
				return res, err
			}
			b.logger.WarnContext(ctx, "board: wait-broker sensor re-materialized (round one)", "trigger", name, "cause", cause)
			res.Rematerialized++
			continue
		}
		degradedID := waitBrokerDegradedPrefix + name
		degraded, err := b.facts.eventExists(ctx, row.ProjectID, degradedID)
		if err != nil {
			return res, err
		}
		if degraded {
			continue
		}
		// Round two: honest silence — delete the dead sensor, say so
		// loudly, leave the wait to the ladder (never a stuck green
		// badge; the health surface renders the degraded line).
		if err := b.triggers.Delete(ctx, trig.OwnerType, trig.OwnerID, trig.ID); err != nil && !errors.Is(err, workflows.ErrNotFound) {
			return res, fmt.Errorf("wait broker: degrade delete %s: %w", name, err)
		}
		if err := b.facts.recordEvent(ctx, row.ProjectID, degradedID, EventActionWaitBrokerDegraded, row.TaskID,
			map[string]any{
				"task_id": row.TaskID, "trigger": name, "cause": cause,
				"hint": "the event-wait's sensor failed twice; the wait stays on the row and the lease ladder governs it",
			}); err != nil {
			return res, err
		}
		b.logger.ErrorContext(ctx, "board: wait-broker DEGRADED — sensor failed after re-materialization; the wait falls to the ladder",
			"trigger", name, "cause", cause, "task", row.ShortID)
		res.Degraded++
	}

	if err := b.facts.advanceWatermark(ctx, high); err != nil {
		return res, fmt.Errorf("wait broker: watermark advance: %w", err)
	}
	return res, nil
}

// createTrigger materializes the sensor (label select happened above;
// the create is idempotent by the deterministic name) and RETURNS the
// built row — its store-issued uuid id is the fires-scan key (a name
// is NOT a uuid: trigger_fires.trigger_id is uuid NOT NULL, 000016).
func (b *WaitBroker) createTrigger(ctx context.Context, decl waitDeclaration, row *waitRowState, shortID, name, script string) (*workflows.TriggerRow, error) {
	src, _ := json.Marshal(map[string]string{"expr": waitBrokerCronExpr})
	_, next, err := workflows.NextCronFireFromConfig(src, b.clock())
	if err != nil {
		return nil, fmt.Errorf("cron %s: %w", waitBrokerCronExpr, err)
	}
	ws := row.WorkspaceID
	trig := &workflows.TriggerRow{
		ID:           uuid.NewString(),
		OwnerType:    types.WorkflowOwnerUser,
		OwnerID:      row.UserID, // R7: the workspace owner owns the trigger (workers delegate as the owning user)
		Name:         name,
		Description:  waitBrokerTriggerDescription(decl, shortID, decl.ClaimToken),
		Enabled:      true,
		SourceType:   types.TriggerSourceCron,
		SourceConfig: src,
		WorkspaceID:  &ws, // the routine target: the claimant's pod (the marker + the pod env live there)
		// Prompt stays EMPTY — the no-prompt-leg pin: a script trigger
		// with no prompt never calls the LLM; the broker is the only
		// consumer of the sensor's fires.
		ScriptLanguage:      "python",
		ScriptHandler:       script,
		AutoDisableAfter:    waitBrokerAutoDisableAfter,
		ConsecutiveFailures: 0,
		NextFireAt:          &next,
		CreatedAt:           b.clock(),
		UpdatedAt:           b.clock(),
	}
	if err := b.triggers.Create(ctx, trig); err != nil {
		return nil, err
	}
	return trig, nil
}

// rematerializeTrigger recreates the sensor for an ALREADY-declared
// wait (the declaration facts re-derived from the row's latest
// wait_declared event — the marker path keeps the episode's dedupe
// state, so the fresh sensor re-observes before it can re-fire).
func (b *WaitBroker) rematerializeTrigger(ctx context.Context, row *waitRowState, shortID, name string) error {
	decl, err := b.facts.latestDeclaration(ctx, row.TaskID)
	if err != nil {
		return err
	}
	if decl.EventID == "" {
		return fmt.Errorf("no wait_declared event found for %s", name)
	}
	if decl.ClaimToken == "" {
		decl.ClaimToken = row.ClaimToken
	}
	script, err := renderWaitGateScript(decl.Condition, decl.Repo, decl.EventID,
		waitBrokerMarkerPath(shortID, row.ClaimToken), b.boardBaseURL, shortID)
	if err != nil {
		return err
	}
	// The rebuilt row's uuid id is the store's truth again; the next
	// pass's ListByPrefix re-reads it (the remat arm needs no local row).
	_, err = b.createTrigger(ctx, decl, row, shortID, name, script)
	return err
}

// waitBrokerTriggerDescription renders the ownership prose (the
// canonical slash label + the reason; the name is the ownership key).
func waitBrokerTriggerDescription(decl waitDeclaration, shortID, claimToken string) string {
	return fmt.Sprintf("%s — platform wait-broker sensor for %s (condition %s%s, reason %q); broker-owned: managed by the wait-broker lifecycle, do not edit",
		waitBrokerLabel(shortID, claimToken), shortID, decl.Condition.String(), repoSuffix(decl.Repo), decl.Reason)
}

func repoSuffix(repo string) string {
	if repo == "" {
		return ""
	}
	return " on " + repo
}

// parseWaitBrokerTriggerName splits wait-broker-<short_id>-<hash8>
// back into its parts (the trailing 8-hex run is the hash; everything
// between the prefix and the LAST dash-run is the short id).
func parseWaitBrokerTriggerName(name string) (shortID, hash8 string, ok bool) {
	if !strings.HasPrefix(name, waitBrokerTriggerPrefix) {
		return "", "", false
	}
	rest := name[len(waitBrokerTriggerPrefix):]
	idx := strings.LastIndex(rest, "-")
	if idx < 0 || len(rest)-idx-1 != 8 {
		return "", "", false
	}
	shortID, hash8 = rest[:idx], rest[idx+1:]
	for i := 0; i < len(hash8); i++ {
		c := hash8[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return "", "", false
		}
	}
	return shortID, hash8, true
}
