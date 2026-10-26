// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package app

// board_worker_comms.go — the #1747/#1959 worker-directive seams, wired
// on the V1 delivery path (adapter.Send — opencode POST
// /session/:id/message), the SAME live route the platform's own
// session-message machinery (llmsafespaces_send_message) rides:
//
//   - Aborter (delete of an in-flight claim): adapter.Abort — the V1
//     session abort, the only interrupt endpoint.
//   - Messenger (pause-must-pause): V1 Send of the halt/resume
//     directive, fired DETACHED — a busy target queues the POST
//     server-side and lands it at the turn boundary ("finish the
//     current thought, then stop" — never a mid-generation cut) — so
//     the pause/resume request never blocks on the worker's turn.
//   - BounceRedispatcher (#1959): V1 Send of the re-dispatch turn to
//     the ORIGINAL implementer's EXISTING session (never a session:new
//     spawn; the message starts a fresh turn there), detached with a
//     BOUNDED fast-fail wait: an error observed within the window is
//     the seam's synchronous liveness answer (dead/reaped session →
//     immediate 4xx → pkg/board's fresh-worker fallback); a window
//     expiry means the turn is running in-flight — the reviewer's
//     verdict request never blocks on Round 2.
//
// #755 (CLOSED SEV1) / #1959 root cause: these seams previously rode
// adapter.SendAsync — opencode's V2 prompt route
// (POST /api/session/:sid/prompt, delivery:queue) — which is DEAD on
// the pinned opencode 1.18.x: the entire v2/ route group was deleted
// in 1.18.10, so every V2 admission 404s and every bounce re-dispatch
// deterministically fell back to a fresh spawn (the owner's token-drain
// concern). The seams are now pinned to V1 STRUCTURALLY: boardV1Sender
// exposes only the V1-carrying method, so the dead queue path cannot
// be rewired into these constructors by accident — the wiring-level
// pin the fake-based TestBounce1959_* suites could not be (they faked
// the adapter and never saw the HTTP route).

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/lenaxia/llmsafespaces/pkg/agent"
	"github.com/lenaxia/llmsafespaces/pkg/board"
	"github.com/lenaxia/llmsafespaces/pkg/session"
	"github.com/lenaxia/llmsafespaces/pkg/shortid"
)

// boardV1Sender is the delivery dependency of the worker-comms seams,
// GROWN in #1959c: the adapter's V1 legs (Send — opencode POST
// /session/:id/message; Abort — the V1 session interrupt) PLUS the
// phrase-registry resolver. The #755/#1959 narrowing discipline holds
// and is STRENGTHENED: a bare agent.Adapter no longer satisfies the
// seam (it has no ResolveSessionRef), so the constructors physically
// cannot be wired with an unresolved sender — posting the platform
// PHRASE verbatim to opencode's RAW-keyed routes (the #1959 root cause
// 2: /session/florid-teapot-verifies/message → 404 → the 10s fast-fail
// → the fresh-worker fallback) is now a COMPILE ERROR, not just a test.
type boardV1Sender interface {
	// ResolveSessionRef maps a session ref (PHRASE or raw) to the id
	// opencode's routes key on. Raw/legacy refs pass through unchanged.
	ResolveSessionRef(ctx context.Context, workspaceID, ref string) (string, error)
	Send(ctx context.Context, userID, workspaceID, sessionID, text string, opts session.SendOpts) (*session.Message, error)
	Abort(ctx context.Context, userID, workspaceID, sessionID string) error
}

// The ONLY production implementation is boardResolvingSender (asserted
// below). The pre-#1959c assertion `var _ boardV1Sender =
// agent.Adapter(nil)` is deliberately GONE — that is the wiring pin:
// the seams can no longer hold a bare adapter.
var _ boardV1Sender = (*boardResolvingSender)(nil)

// boardPhraseLookup is the platform's phrase registry consult (#1959c):
// phrase → RAW harness session id within one workspace (the session_index
// layer — the SAME registry the SessionRefResolution middleware
// (api/internal/middleware/session_ref.go) and the pod-side replica
// (cmd/workspace-agentd/session_shortids.go) read; FindSessionByShortID's
// exact query). "" with nil error = no live session holds the phrase.
//
// Honest-scope note (#1959c lane): the charter's first sketch resolved
// via the adapter's session LIST (GET /session + ParseSessionListWire) —
// checked, and the production opencode list wire carries NO phrase
// field (the phrase is platform-layer identity, #1816; the list is
// raw-keyed ids + titles). The platform's own phrase registry is the
// only phrase→raw authority reachable from the api side without an
// HTTP self-hop, so the resolver leg binds HERE.
type boardPhraseLookup func(ctx context.Context, workspaceID, phrase string) (rawID string, err error)

// boardPhraseLookupFor binds the pool-backed registry consult. The pgx
// ErrNoRows miss translates to ("", nil) — the documented miss contract
// ("" = no live session holds the phrase), so ResolveSessionRef's miss
// branch (the "resolves to no live session" liveness error) is the
// branch production actually takes; a raw wrapped ErrNoRows would
// bypass it with an opaque "no rows in result set" (review r1
// Robustness).
func boardPhraseLookupFor(pool *pgxpool.Pool) boardPhraseLookup {
	return func(ctx context.Context, workspaceID, phrase string) (string, error) {
		var raw string
		err := pool.QueryRow(ctx,
			`SELECT session_id FROM session_index WHERE workspace_id=$1 AND lower(short_id)=$2`,
			workspaceID, shortid.Normalize(phrase)).Scan(&raw)
		if errors.Is(err, pgx.ErrNoRows) {
			return "", nil
		}
		return raw, err
	}
}

// boardResolvingSender is the #1959c production sender: the adapter's
// V1 legs plus the phrase-registry resolution, so a PHRASE-addressed
// target (the engine's stored assignee/implementer_session — durable
// identity, #1850) reaches opencode's RAW-keyed routes as its raw
// harness id. Construction is the ONLY way the board seams may hold an
// adapter: the constructors' dependency carries the resolver, so a bare
// agent.Adapter (no resolution) cannot be wired in by accident.
type boardResolvingSender struct {
	delegate agent.Adapter
	lookup   boardPhraseLookup
}

// newBoardResolvingSender builds the production delivery dependency.
func newBoardResolvingSender(delegate agent.Adapter, lookup boardPhraseLookup) *boardResolvingSender {
	return &boardResolvingSender{delegate: delegate, lookup: lookup}
}

// Send delivers over the adapter's V1 route (delegation).
func (s *boardResolvingSender) Send(ctx context.Context, userID, workspaceID, sessionID, text string, opts session.SendOpts) (*session.Message, error) {
	return s.delegate.Send(ctx, userID, workspaceID, sessionID, text, opts)
}

// Abort interrupts over the adapter's V1 abort route (delegation).
func (s *boardResolvingSender) Abort(ctx context.Context, userID, workspaceID, sessionID string) error {
	return s.delegate.Abort(ctx, userID, workspaceID, sessionID)
}

// ResolveSessionRef maps a board-comms session ref to the id opencode's
// routes key on. PHRASE-form refs (#1756 3-word shape, case-insensitive)
// consult the platform registry; RAW (ses_…) and legacy forms pass
// through byte-identical — the adapter stays the authority for raw
// addressing (the #1663 lesson: resolution must never make a live
// session unaddressable), and raw consults are free.
func (s *boardResolvingSender) ResolveSessionRef(ctx context.Context, workspaceID, ref string) (string, error) {
	if !shortid.IsShortIDForm(ref) {
		return ref, nil
	}
	phrase := shortid.Normalize(ref)
	raw, err := s.lookup(ctx, workspaceID, phrase)
	if err != nil {
		return "", fmt.Errorf("board: session phrase lookup for %s: %w", phrase, err)
	}
	if raw == "" {
		return "", fmt.Errorf("board: session phrase %s resolves to no live session in workspace %s", phrase, workspaceID)
	}
	return raw, nil
}

// Delivery knobs (package vars so the wiring tests can bound them):
//
//   - boardV1FastFail bounds how long the re-dispatch seam WAITS for a
//     synchronous delivery verdict before declaring the turn
//     in-flight. A dead/reaped session 4xx's in well under a second
//     (pod resolve + one POST), so 10s is generous headroom for a slow
//     k8s resolve; a live session's re-dispatch turn routinely
//     outlasts it — the caller (the reviewer's verdict request) must
//     not block on Round 2.
//   - boardV1DeliveryBudget bounds the DETACHED send itself: a worker
//     turn — or a busy-target queue wait at the turn boundary — can
//     run tens of minutes; the budget exists only so a wedged POST
//     cannot leak the goroutine forever.
var (
	boardV1FastFail       = 10 * time.Second
	boardV1DeliveryBudget = 30 * time.Minute
)

// deliverBoardTextV1 delivers text to an existing session over the V1
// route, DETACHED — the llmsafespaces_send_message discipline (the
// seam the platform itself proved live through #755): the V1 POST
// queues server-side on a busy target and lands at the turn boundary,
// and context.WithoutCancel keeps the delivery alive after the caller
// returns. The alternative — a synchronous Send in the caller's ctx —
// would both block the caller for the whole delivered turn (the
// reviewer's verdict request hanging on Round 2; the pause request
// hanging on the worker's turn) and CANCEL the delivered turn
// mid-flight when that request ends. This mirrors pkg/board's own
// delivery detachment (engine.go's spawn/nudge goroutines): a delivery
// that never lands heals by lease lapse.
//
// #1959c: the ref is RESOLVED before the post — opencode's routes are
// RAW-keyed (ses_ ids); the engine's stored assignee/implementer
// identity is the platform PHRASE (durable, #1850/#1816), and a phrase
// posted verbatim is a deterministic 404 (the second live fallback on
// 0.44.3). Resolution runs INSIDE the detached goroutine so its verdict
// rides the same fast-fail window as the POST's: an unresolvable phrase
// (reaped/archived original — or a registry outage) returns as the
// seam's synchronous liveness error, exactly the input pkg/board's
// fresh-worker fallback keys on, while a hanging registry consult can
// never block the reviewer's verdict request past the window.
//
// fastFail > 0 waits that long for a delivery verdict: an error
// observed in the window is returned synchronously (the seam's
// liveness answer — pkg/board's fresh-worker fallback keys on it); a
// window expiry — or the caller's ctx ending — returns nil (in-flight;
// never a fallback on a timing artifact). fastFail <= 0 is pure
// fire-and-forget (the directive messenger's shape: every caller
// discards the error).
func deliverBoardTextV1(ctx context.Context, sender boardV1Sender, what, userID, workspaceID, sessionID, text string, fastFail time.Duration) error {
	done := make(chan error, 1)
	go func() { //nolint:gosec,contextcheck // G118/contextcheck: deliberate — the delivered turn outlives the caller's request context by design (pkg/board engine.go's delivery detachment; lease lapse heals)
		sctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), boardV1DeliveryBudget)
		defer cancel()
		raw, err := sender.ResolveSessionRef(sctx, workspaceID, sessionID)
		if err != nil {
			// Observability for the arms whose caller is gone by now
			// (fire-and-forget directives; window-expired deliveries).
			// The re-dispatch fallback decision itself is pkg/board's,
			// keyed on the synchronous return below.
			slog.WarnContext(sctx, "board: session ref did not resolve for V1 delivery",
				"what", what, "task_scope", workspaceID, "session", sessionID, "err", err.Error())
			done <- err
			return
		}
		_, err = sender.Send(sctx, userID, workspaceID, raw, text, session.SendOpts{})
		if err != nil {
			// Observability for the arms whose caller is gone by now
			// (fire-and-forget directives; window-expired deliveries).
			// The re-dispatch fallback decision itself is pkg/board's,
			// keyed on the synchronous return below.
			slog.WarnContext(sctx, "board: detached V1 delivery failed",
				"what", what, "task_scope", workspaceID, "session", sessionID, "resolved", raw, "err", err.Error())
		}
		done <- err
	}()
	if fastFail <= 0 {
		return nil
	}
	timer := time.NewTimer(fastFail)
	defer timer.Stop()
	select {
	case err := <-done:
		return err
	case <-timer.C:
		return nil // the turn is running — delivery in-flight
	case <-ctx.Done():
		return nil // the caller left; the detached delivery carries on
	}
}

// haltDirectiveText / resumeDirectiveText render the worker-facing
// directive messages (the charter envelope's voice).
func haltDirectiveText(shortID string) string {
	return fmt.Sprintf("BOARD PAUSE DIRECTIVE: your task %s has been PAUSED by the owner. "+
		"Finish your current thought, then STOP and wait. Do not move the task's stage and do not "+
		"start new work while paused (forward writes will be refused with 409 task_paused). "+
		"A resume directive will follow when work continues.", shortID)
}

func resumeDirectiveText(shortID string) string {
	return fmt.Sprintf("BOARD RESUME DIRECTIVE: your task %s was resumed — continue working. "+
		"If your claim lapsed while paused, your next write returns 409 claim_token: re-read the "+
		"task with board_read; the engine may already have spawned a fresh worker.", shortID)
}

// boardScopeResolver resolves (owner, target workspace) for a task —
// the adapter's addressing keys. Injected as a function (not the pool)
// so the wiring tests drive the production closures end-to-end without
// a database (the #1959 gap: the fake-adapter pins could not see the
// HTTP route the production adapter actually rides).
type boardScopeResolver func(ctx context.Context, taskRef string) (userID, workspaceID string, err error)

// boardScopeFor binds the pool-backed production resolver.
func boardScopeFor(pool *pgxpool.Pool) boardScopeResolver {
	return func(ctx context.Context, taskRef string) (string, string, error) {
		return boardTaskScope(ctx, pool, taskRef)
	}
}

// boardTaskScope resolves (owner, target workspace) for a task by id or
// short id — the adapter's addressing keys.
func boardTaskScope(ctx context.Context, pool *pgxpool.Pool, taskRef string) (userID, workspaceID string, err error) {
	err = pool.QueryRow(ctx, `
		SELECT p.user_id, p.target_workspace_id::text
		FROM tasks t JOIN projects p ON p.id = t.project_id
		WHERE t.id::text = $1 OR lower(t.short_id) = lower($1)
		LIMIT 1`, taskRef).Scan(&userID, &workspaceID)
	return userID, workspaceID, err
}

// newBoardAborter builds the delete-of-in-flight-claim interrupt seam
// (scope via the injected resolver — the #1963 constructor shape, so the
// wiring pins drive the production closure without a database). The
// assignee ref is RESOLVED before the V1 abort (#1959c — the same
// phrase-vs-raw disease: an abort posted to a phrase id verbatim 404s
// and the interrupt silently never lands).
func newBoardAborter(scope boardScopeResolver, sender boardV1Sender) board.WorkerAborter {
	return func(ctx context.Context, taskID, assigneeSession string) error {
		userID, workspaceID, err := scope(ctx, taskID)
		if err != nil {
			return fmt.Errorf("board: abort scope: %w", err)
		}
		raw, err := sender.ResolveSessionRef(ctx, workspaceID, assigneeSession)
		if err != nil {
			return fmt.Errorf("board: abort resolve: %w", err)
		}
		return sender.Abort(ctx, userID, workspaceID, raw)
	}
}

// newBoardMessenger builds the halt/resume directive seam: the V1 Send
// fires DETACHED (fire-and-forget) — a busy worker queues the POST
// server-side and sees the directive at its turn boundary, exactly the
// pause-must-pause contract; the pause/resume request returns without
// waiting on any worker's turn (the callers' best-effort contract
// keeps every error discarded — delivery failures surface as the
// detached goroutine's warn line, never a blocked or failed pause).
func newBoardMessenger(scope boardScopeResolver, sender boardV1Sender) board.WorkerMessenger {
	return func(ctx context.Context, sessionID, directive, taskShortID string) error {
		userID, workspaceID, err := scope(ctx, taskShortID)
		if err != nil {
			return fmt.Errorf("board: directive scope: %w", err)
		}
		text := resumeDirectiveText(taskShortID)
		if directive == board.DirectiveHalt {
			text = haltDirectiveText(taskShortID)
		}
		return deliverBoardTextV1(ctx, sender, directive+" directive", userID, workspaceID, sessionID, text, 0)
	}
}

// newBoardRedispatcher builds the #1959 bounce re-dispatch seam: the
// re-dispatch turn (fresh bounce claim token + embedded verdict chain,
// engine-rendered) delivered to the ORIGINAL implementer's EXISTING
// session over the V1 route — detached with a bounded fast-fail wait.
// An immediate V1 4xx on a dead session (archived/reaped) is returned
// synchronously — the seam's liveness answer pkg/board's
// fresh-implementer fallback keys on — while a live session's Round-2
// turn runs in-flight, never blocking the reviewer's verdict request
// and never dying with it.
func newBoardRedispatcher(scope boardScopeResolver, sender boardV1Sender) board.BounceRedispatcher {
	return func(ctx context.Context, sessionID, text, taskShortID string) error {
		userID, workspaceID, err := scope(ctx, taskShortID)
		if err != nil {
			return fmt.Errorf("board: re-dispatch scope: %w", err)
		}
		return deliverBoardTextV1(ctx, sender, "bounce re-dispatch", userID, workspaceID, sessionID, text, boardV1FastFail)
	}
}
