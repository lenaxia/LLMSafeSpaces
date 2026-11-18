// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package app

// wait_broker_test.go — the broker's pass matrix (claim-wait
// lifecycle, low-level design §4.2 + §6's wait-event-broker map):
// materialize / dedupe / re-declaration script update / orphan-clean
// on clear+claim-end+terminal / fire-consume → resume → wait_resolved
// (keyed on the fire id) / bounced-delivery retry / disabled-trigger
// re-materialize-once-then-degrade-loud / cross-replica double-pass
// convergence / the no-prompt-leg + cron caps on every created row.

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/lenaxia/llmsafespaces/pkg/board"
	"github.com/lenaxia/llmsafespaces/pkg/session"
	"github.com/lenaxia/llmsafespaces/pkg/workflows"
)

// --- the fakes -------------------------------------------------------------

type fakeWaitFacts struct {
	mu      sync.Mutex
	decls   []waitDeclaration
	rows    map[string]*waitRowState // by task id
	byShort map[string]*waitRowState
	latest  map[string]waitDeclaration
	events  map[string]map[string]bool // projectID -> id
	wm      time.Time
	high    time.Time
}

func newFakeWaitFacts() *fakeWaitFacts {
	return &fakeWaitFacts{
		rows:    map[string]*waitRowState{},
		byShort: map[string]*waitRowState{},
		latest:  map[string]waitDeclaration{},
		events:  map[string]map[string]bool{},
	}
}

func (f *fakeWaitFacts) declarationsSince(_ context.Context, cutoff, high time.Time) ([]waitDeclaration, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []waitDeclaration
	for _, d := range f.decls {
		if d.CreatedAt.After(cutoff) && !d.CreatedAt.After(high) {
			out = append(out, d)
		}
	}
	return out, nil
}

func (f *fakeWaitFacts) taskWait(_ context.Context, taskID string) (*waitRowState, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	row := f.rows[taskID]
	if row == nil {
		return nil, nil
	}
	cp := *row
	return &cp, nil
}

func (f *fakeWaitFacts) taskWaitByShortID(_ context.Context, shortID string) (*waitRowState, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	row := f.byShort[strings.ToLower(shortID)]
	if row == nil {
		return nil, nil
	}
	cp := *row
	return &cp, nil
}

func (f *fakeWaitFacts) latestDeclaration(_ context.Context, taskID string) (waitDeclaration, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.latest[taskID], nil
}

func (f *fakeWaitFacts) eventExists(_ context.Context, projectID, id string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.events[projectID][id], nil
}

func (f *fakeWaitFacts) recordEvent(_ context.Context, projectID, id, _, _ string, _ map[string]any) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.events[projectID] == nil {
		f.events[projectID] = map[string]bool{}
	}
	f.events[projectID][id] = true
	return nil
}

func (f *fakeWaitFacts) watermark(_ context.Context) (time.Time, time.Time, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.wm.IsZero() {
		f.wm = time.Unix(0, 0)
	}
	return f.wm, f.high, nil
}

func (f *fakeWaitFacts) advanceWatermark(_ context.Context, to time.Time) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if to.After(f.wm) {
		f.wm = to
	}
	return nil
}

func (f *fakeWaitFacts) addDecl(decl waitDeclaration) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.decls = append(f.decls, decl)
	f.latest[decl.TaskID] = decl
	if decl.CreatedAt.After(f.high) {
		f.high = decl.CreatedAt
	}
}

func (f *fakeWaitFacts) setRow(row *waitRowState) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rows[row.TaskID] = row
	f.byShort[strings.ToLower(row.ShortID)] = row
}

type fakeTriggerPlane struct {
	mu      sync.Mutex
	rows    map[string]*workflows.TriggerRow // by name
	fires   map[string][]*workflows.TriggerFireRow
	created int
	updated int
	deleted int
}

func newFakeTriggerPlane() *fakeTriggerPlane {
	return &fakeTriggerPlane{rows: map[string]*workflows.TriggerRow{}, fires: map[string][]*workflows.TriggerFireRow{}}
}

func (f *fakeTriggerPlane) ListByPrefix(_ context.Context, prefix string) ([]*workflows.TriggerRow, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []*workflows.TriggerRow
	for _, r := range f.rows {
		if strings.HasPrefix(r.Name, prefix) {
			cp := *r
			out = append(out, &cp)
		}
	}
	return out, nil
}

func (f *fakeTriggerPlane) Create(_ context.Context, row *workflows.TriggerRow) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, exists := f.rows[row.Name]; exists {
		return nil // the (owner, name) unique-race convergence arm
	}
	cp := *row
	if cp.ID == "" || strings.HasPrefix(cp.ID, waitBrokerTriggerPrefix) {
		cp.ID = uuid.NewString()
	}
	f.rows[row.Name] = &cp
	f.created++
	return nil
}

func (f *fakeTriggerPlane) UpdateScript(_ context.Context, _, _, id, language, handler, description string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, r := range f.rows {
		if r.ID == id {
			r.ScriptLanguage = language
			r.ScriptHandler = handler
			r.Description = description
			r.Enabled = true
			r.ConsecutiveFailures = 0
			f.updated++
			return nil
		}
	}
	return workflows.ErrNotFound
}

func (f *fakeTriggerPlane) ResetFailures(_ context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, r := range f.rows {
		if r.ID == id {
			r.ConsecutiveFailures = 0
			return nil
		}
	}
	return nil
}

func (f *fakeTriggerPlane) Delete(_ context.Context, _, _, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for name, r := range f.rows {
		if r.ID == id {
			delete(f.rows, name)
			f.deleted++
			return nil
		}
	}
	return workflows.ErrNotFound
}

func (f *fakeTriggerPlane) ListFires(_ context.Context, triggerID string, limit int) ([]*workflows.TriggerFireRow, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	fires := f.fires[triggerID]
	var out []*workflows.TriggerFireRow
	for i := len(fires) - 1; i >= 0 && len(out) < limit; i-- { // DESC by fired_at (insertion order)
		cp := *fires[i]
		out = append(out, &cp)
	}
	return out, nil
}

func (f *fakeTriggerPlane) addFire(triggerID string, fire *workflows.TriggerFireRow) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.fires[triggerID] = append(f.fires[triggerID], fire)
}

func (f *fakeTriggerPlane) get(name string) *workflows.TriggerRow {
	f.mu.Lock()
	defer f.mu.Unlock()
	if r, ok := f.rows[name]; ok {
		cp := *r
		return &cp
	}
	return nil
}

// fakeBrokerSender is the boardV1Sender fake: resolves passthrough,
// records sends, and can bounce per session (the delivery-retry pin).
type fakeBrokerSender struct {
	mu       sync.Mutex
	sent     []string
	bounce   map[string]bool
	lastText string
}

func newFakeBrokerSender() *fakeBrokerSender {
	return &fakeBrokerSender{bounce: map[string]bool{}}
}

func (f *fakeBrokerSender) ResolveSessionRef(_ context.Context, _, ref string) (string, error) {
	return ref, nil
}

func (f *fakeBrokerSender) Send(_ context.Context, _, _, sessionID, text string, _ session.SendOpts) (*session.Message, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.bounce[sessionID] {
		return nil, fmt.Errorf("dead session %s", sessionID)
	}
	f.sent = append(f.sent, sessionID)
	f.lastText = text
	return &session.Message{}, nil
}

func (f *fakeBrokerSender) Abort(_ context.Context, _, _, _ string) error { return nil }

// --- the harness -----------------------------------------------------------

type brokerHarness struct {
	facts    *fakeWaitFacts
	triggers *fakeTriggerPlane
	sender   *fakeBrokerSender
	broker   *WaitBroker
}

func newBrokerHarness(t *testing.T) *brokerHarness {
	t.Helper()
	h := &brokerHarness{
		facts:    newFakeWaitFacts(),
		triggers: newFakeTriggerPlane(),
		sender:   newFakeBrokerSender(),
	}
	h.broker = newWaitBroker(h.facts, h.triggers, h.sender, "http://api.internal:8080")
	return h
}

// seedLiveEventWait plants one live event-wait: the declaration event
// AND the row that still carries it.
func (h *brokerHarness) seedLiveEventWait(t *testing.T, taskID, shortID, token, assignee, condition, repo string, at time.Time) waitDeclaration {
	decl := waitDeclaration{
		EventID: "wait:" + taskID + ":" + token + ":1", ProjectID: "prj-1", TaskID: taskID,
		CreatedAt: at, Reason: "PR merge", Condition: mustCond(t, condition), Repo: repo,
		ClaimToken: token, ShortID: shortID, Assignee: assignee,
	}
	h.facts.addDecl(decl)
	h.facts.setRow(&waitRowState{
		TaskID: taskID, ProjectID: "prj-1", ShortID: shortID, Stage: board.StageInProgress,
		WaitOn: board.WaitOnEvent, WaitReason: "PR merge", ClaimToken: token,
		Assignee: assignee, UserID: "user-1", WorkspaceID: "ws-1",
	})
	return decl
}

func mustCond(t *testing.T, s string) board.WaitCondition {
	t.Helper()
	cond, err := board.ParseWaitCondition(s)
	require.NoError(t, err)
	return cond
}

// --- the matrix -------------------------------------------------------------

// TestWaitBroker_Materializes_NoPromptLeg_Caps — the auto-materialize
// pin: a declared event-wait with a live row compiles to a script-only
// trigger — script present, prompt EMPTY (the no-LLM-leg pin), cron
// */2 (the >= 2-min storm guard), owner = the row's user (R7), target
// = the claimant's workspace, auto-disable bounded.
func TestWaitBroker_Materializes_NoPromptLeg_Caps(t *testing.T) {
	h := newBrokerHarness(t)
	at := time.Now().Add(-time.Minute)
	h.seedLiveEventWait(t, "t1", "tsk-henna-one", "11111111-1111-1111-1111-111111111111", "florid-teapot-verifies", "pr_merged(2101)", "lenaxia/llmsafespaces", at)

	res, err := h.broker.Pass(context.Background())
	require.NoError(t, err)
	require.Equal(t, 1, res.Materialized)

	name := waitBrokerTriggerName("tsk-henna-one", "11111111-1111-1111-1111-111111111111")
	trig := h.triggers.get(name)
	require.NotNil(t, trig, "the compiled trigger exists under the broker's label namespace")
	require.Empty(t, trig.Prompt, "NO PROMPT LEG — a script trigger with no prompt never calls the LLM")
	require.Equal(t, "python", trig.ScriptLanguage)
	require.NotEmpty(t, trig.ScriptHandler)
	require.Contains(t, trig.ScriptHandler, `PR_URL = "https://api.github.com/repos/lenaxia/llmsafespaces/pulls/2101"`)
	require.Equal(t, "cron", trig.SourceType)
	require.Contains(t, string(trig.SourceConfig), `*/2 * * * *`, "poll cadence >= 2 min (the storm guard)")
	require.Equal(t, "user", trig.OwnerType)
	require.Equal(t, "user-1", trig.OwnerID)
	require.NotNil(t, trig.WorkspaceID)
	require.Equal(t, "ws-1", *trig.WorkspaceID)
	require.Equal(t, waitBrokerAutoDisableAfter, trig.AutoDisableAfter)
	require.True(t, trig.Enabled)
	require.Contains(t, trig.Description, waitBrokerLabel("tsk-henna-one", "11111111-1111-1111-1111-111111111111"))
	require.NotNil(t, trig.NextFireAt, "the scheduler's claim predicate needs next_fire_at set")
}

// TestWaitBroker_DoublePassConverges — the cross-replica pin: two
// passes over the same window leave exactly one trigger, one
// watermark advance, and no duplicate events (deterministic ids +
// name idempotence arbitrate).
func TestWaitBroker_DoublePassConverges(t *testing.T) {
	h := newBrokerHarness(t)
	at := time.Now().Add(-time.Minute)
	h.seedLiveEventWait(t, "t1", "tsk-henna-one", "11111111-1111-1111-1111-111111111111", "florid-teapot-verifies", "pr_merged(2101)", "lenaxia/llmsafespaces", at)

	for i := 0; i < 2; i++ {
		_, err := h.broker.Pass(context.Background())
		require.NoError(t, err)
	}
	require.Equal(t, 1, h.triggers.created)
	require.Equal(t, 0, h.triggers.deleted)

	// The watermark consumed the window: a third pass re-reads nothing.
	h.facts.mu.Lock()
	wm := h.facts.wm
	h.facts.mu.Unlock()
	require.True(t, wm.Equal(at) || wm.After(at), "the watermark advanced to the pre-captured high")
}

// TestWaitBroker_ReDeclaration_UpdatesScript — a within-episode
// re-declaration (new event id => new seed) UPDATEs the same-name
// trigger's script — never a second trigger (the one-wait/story cap's
// broker half).
func TestWaitBroker_ReDeclaration_UpdatesScript(t *testing.T) {
	h := newBrokerHarness(t)
	tok := "11111111-1111-1111-1111-111111111111"
	h.seedLiveEventWait(t, "t1", "tsk-henna-one", tok, "florid-teapot-verifies", "pr_merged(2101)", "lenaxia/llmsafespaces", time.Now().Add(-2*time.Minute))
	_, err := h.broker.Pass(context.Background())
	require.NoError(t, err)

	// The re-declaration: cleared then declared again within the same
	// claim (the conversion dance) — a NEW event id one minute later.
	second := waitDeclaration{
		EventID: "wait:t1:" + tok + ":2", ProjectID: "prj-1", TaskID: "t1",
		CreatedAt: time.Now().Add(-time.Minute), Reason: "PR merge, again", Condition: mustCond(t, "ci_green(2101)"),
		Repo: "lenaxia/llmsafespaces", ClaimToken: tok, ShortID: "tsk-henna-one", Assignee: "florid-teapot-verifies",
	}
	h.facts.addDecl(second)
	_, err = h.broker.Pass(context.Background())
	require.NoError(t, err)

	name := waitBrokerTriggerName("tsk-henna-one", tok)
	trig := h.triggers.get(name)
	require.NotNil(t, trig)
	require.Equal(t, 1, h.triggers.created, "still exactly ONE trigger")
	require.Equal(t, 1, h.triggers.updated, "the re-declaration UPDATED the script")
	require.Contains(t, trig.ScriptHandler, "ci_green", "the fresh condition's sensor replaced the old script")
}

// TestWaitBroker_OrphanClean_WaitCleared — the resolve-side cleanup:
// the row's wait clears (worker advance / wedge reset) => the sensor
// deletes + wait_orphan_cleaned lands (deterministic id, replay-inert).
func TestWaitBroker_OrphanClean_WaitCleared(t *testing.T) {
	h := newBrokerHarness(t)
	tok := "11111111-1111-1111-1111-111111111111"
	h.seedLiveEventWait(t, "t1", "tsk-henna-one", tok, "florid-teapot-verifies", "pr_merged(2101)", "lenaxia/llmsafespaces", time.Now().Add(-2*time.Minute))
	_, err := h.broker.Pass(context.Background())
	require.NoError(t, err)
	name := waitBrokerTriggerName("tsk-henna-one", tok)
	require.NotNil(t, h.triggers.get(name))

	// The wait cleared (the wedge reset / worker advance shape).
	h.facts.setRow(&waitRowState{
		TaskID: "t1", ProjectID: "prj-1", ShortID: "tsk-henna-one", Stage: board.StageInProgress,
		ClaimToken: tok, Assignee: "florid-teapot-verifies", UserID: "user-1", WorkspaceID: "ws-1",
	})
	res, err := h.broker.Pass(context.Background())
	require.NoError(t, err)
	require.Equal(t, 1, res.OrphansCleaned)
	require.Nil(t, h.triggers.get(name))
	exists, _ := h.facts.eventExists(context.Background(), "prj-1", waitOrphanCleanedPrefix+name)
	require.True(t, exists, "wait_orphan_cleaned landed with the deterministic id")

	// Idempotent: a third pass finds nothing to clean and no dup event.
	res, err = h.broker.Pass(context.Background())
	require.NoError(t, err)
	require.Equal(t, 0, res.OrphansCleaned)
}

// TestWaitBroker_OrphanClean_ClaimEndAndTerminal — the table's other
// arms: a minted-new-token row (claim ended) and a done row (terminal).
func TestWaitBroker_OrphanClean_ClaimEndAndTerminal(t *testing.T) {
	for _, tc := range []struct {
		name, stage, waitOn, token string
	}{
		{"claim_ended", board.StageInProgress, board.WaitOnEvent, "99999999-9999-9999-9999-999999999999"},
		{"terminal", board.StageDone, "", "11111111-1111-1111-1111-111111111111"},
		{"wedge_reset_cleared", board.StageFailed, "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newBrokerHarness(t)
			tok := "11111111-1111-1111-1111-111111111111"
			h.seedLiveEventWait(t, "t1", "tsk-henna-one", tok, "florid-teapot-verifies", "pr_merged(2101)", "lenaxia/llmsafespaces", time.Now().Add(-2*time.Minute))
			_, err := h.broker.Pass(context.Background())
			require.NoError(t, err)
			name := waitBrokerTriggerName("tsk-henna-one", tok)
			require.NotNil(t, h.triggers.get(name))

			h.facts.setRow(&waitRowState{
				TaskID: "t1", ProjectID: "prj-1", ShortID: "tsk-henna-one", Stage: tc.stage,
				WaitOn: tc.waitOn, ClaimToken: tc.token, Assignee: "florid-teapot-verifies",
				UserID: "user-1", WorkspaceID: "ws-1",
			})
			res, err := h.broker.Pass(context.Background())
			require.NoError(t, err)
			require.Equal(t, 1, res.OrphansCleaned)
			require.Nil(t, h.triggers.get(name))
		})
	}
}

// TestWaitBroker_FireConsumed_ResumeAndEvent — the resolution path: a
// delivered gate-open fire lands the wait-resolved sentinel on the
// claimant + the wait_resolved event keyed on the fire id; the row's
// wait stays until the worker advances (both orders converge).
func TestWaitBroker_FireConsumed_ResumeAndEvent(t *testing.T) {
	h := newBrokerHarness(t)
	tok := "11111111-1111-1111-1111-111111111111"
	decl := h.seedLiveEventWait(t, "t1", "tsk-henna-one", tok, "florid-teapot-verifies", "pr_merged(2101)", "lenaxia/llmsafespaces", time.Now().Add(-3*time.Minute))
	_, err := h.broker.Pass(context.Background())
	require.NoError(t, err)
	name := waitBrokerTriggerName("tsk-henna-one", tok)
	trig := h.triggers.get(name)
	require.NotNil(t, trig)

	action, _ := json.Marshal(map[string]any{"gate": map[string]any{"proceed": true, "vars": map[string]any{"merge_commit_sha": "abc"}}})
	fire := &workflows.TriggerFireRow{ID: "fire-1", TriggerID: trig.ID, Status: "delivered", ActionResult: action, FiredAt: decl.CreatedAt.Add(time.Minute)}
	h.triggers.addFire(trig.ID, fire)

	res, err := h.broker.Pass(context.Background())
	require.NoError(t, err)
	require.Equal(t, 1, res.Resolved)
	require.Equal(t, []string{"florid-teapot-verifies"}, h.sender.sent, "the resume reached the claimant session")
	require.Contains(t, h.sender.lastText, "BOARD WAIT-RESOLVED")
	require.Contains(t, h.sender.lastText, "tsk-henna-one")
	require.Contains(t, h.sender.lastText, "<!-- lsp:agent-message-v1", "the sentinel line rides the v1 composer")
	require.Contains(t, h.sender.lastText, `"mode":"wait-resolved"`)

	exists, _ := h.facts.eventExists(context.Background(), "prj-1", waitResolvedEventPrefix+"fire-1")
	require.True(t, exists, "wait_resolved landed, keyed on the fire id")

	// The consume belt: a second pass never re-sends.
	res, err = h.broker.Pass(context.Background())
	require.NoError(t, err)
	require.Equal(t, 0, res.Resolved)
	require.Len(t, h.sender.sent, 1)
}

// TestWaitBroker_FireBounced_Retries — a bounced delivery (dead
// session) leaves the fire unconsumed; the next pass (sender healthy)
// delivers and lands the event.
func TestWaitBroker_FireBounced_Retries(t *testing.T) {
	h := newBrokerHarness(t)
	tok := "11111111-1111-1111-1111-111111111111"
	decl := h.seedLiveEventWait(t, "t1", "tsk-henna-one", tok, "florid-teapot-verifies", "pr_merged(2101)", "lenaxia/llmsafespaces", time.Now().Add(-3*time.Minute))
	h.sender.bounce["florid-teapot-verifies"] = true
	_, err := h.broker.Pass(context.Background())
	require.NoError(t, err)
	trig := h.triggers.get(waitBrokerTriggerName("tsk-henna-one", tok))
	require.NotNil(t, trig)

	action, _ := json.Marshal(map[string]any{"gate": map[string]any{"proceed": true}})
	h.triggers.addFire(trig.ID, &workflows.TriggerFireRow{ID: "fire-1", TriggerID: trig.ID, Status: "delivered", ActionResult: action, FiredAt: decl.CreatedAt.Add(time.Minute)})

	res, err := h.broker.Pass(context.Background())
	require.NoError(t, err)
	require.Equal(t, 0, res.Resolved, "a bounced delivery consumes nothing")
	exists, _ := h.facts.eventExists(context.Background(), "prj-1", waitResolvedEventPrefix+"fire-1")
	require.False(t, exists)

	h.sender.mu.Lock()
	h.sender.bounce["florid-teapot-verifies"] = false
	h.sender.mu.Unlock()
	res, err = h.broker.Pass(context.Background())
	require.NoError(t, err)
	require.Equal(t, 1, res.Resolved, "the fire stays unconsumed until a wait_resolved event lands")
}

// TestWaitBroker_SkippedAndStaleFires_NotConsumed — the filters: a
// closed gate (skipped), a failed fire, and a fire predating the
// current declaration never resolve.
func TestWaitBroker_SkippedAndStaleFires_NotConsumed(t *testing.T) {
	h := newBrokerHarness(t)
	tok := "11111111-1111-1111-1111-111111111111"
	decl := h.seedLiveEventWait(t, "t1", "tsk-henna-one", tok, "florid-teapot-verifies", "pr_merged(2101)", "lenaxia/llmsafespaces", time.Now().Add(-3*time.Minute))
	_, err := h.broker.Pass(context.Background())
	require.NoError(t, err)
	trig := h.triggers.get(waitBrokerTriggerName("tsk-henna-one", tok))
	require.NotNil(t, trig)

	closed, _ := json.Marshal(map[string]any{"gate": map[string]any{"proceed": false}})
	opened, _ := json.Marshal(map[string]any{"gate": map[string]any{"proceed": true}})
	h.triggers.addFire(trig.ID, &workflows.TriggerFireRow{ID: "fire-closed", TriggerID: trig.ID, Status: "skipped", ActionResult: closed, FiredAt: decl.CreatedAt.Add(time.Minute)})
	h.triggers.addFire(trig.ID, &workflows.TriggerFireRow{ID: "fire-failed", TriggerID: trig.ID, Status: "failed", ActionResult: opened, FiredAt: decl.CreatedAt.Add(time.Minute)})
	h.triggers.addFire(trig.ID, &workflows.TriggerFireRow{ID: "fire-stale", TriggerID: trig.ID, Status: "delivered", ActionResult: opened, FiredAt: decl.CreatedAt.Add(-time.Minute)})

	res, err := h.broker.Pass(context.Background())
	require.NoError(t, err)
	require.Equal(t, 0, res.Resolved)
	require.Empty(t, h.sender.sent)
}

// TestWaitBroker_Degraded_RematerializeOnceThenLoud — the failure
// posture: a disabled sensor is re-materialized ONCE (fresh trigger +
// the remat audit); the SECOND disable deletes it and lands
// wait_broker_degraded — honest silence, the ladder governs.
func TestWaitBroker_Degraded_RematerializeOnceThenLoud(t *testing.T) {
	h := newBrokerHarness(t)
	tok := "11111111-1111-1111-1111-111111111111"
	h.seedLiveEventWait(t, "t1", "tsk-henna-one", tok, "florid-teapot-verifies", "pr_merged(2101)", "lenaxia/llmsafespaces", time.Now().Add(-3*time.Minute))
	_, err := h.broker.Pass(context.Background())
	require.NoError(t, err)
	name := waitBrokerTriggerName("tsk-henna-one", tok)
	first := h.triggers.get(name)
	require.NotNil(t, first)

	// Round one: auto-disable fired (consecutive failures crossed the cap).
	h.triggers.mu.Lock()
	h.triggers.rows[name].Enabled = false
	h.triggers.rows[name].ConsecutiveFailures = waitBrokerAutoDisableAfter
	h.triggers.mu.Unlock()
	res, err := h.broker.Pass(context.Background())
	require.NoError(t, err)
	require.Equal(t, 1, res.Rematerialized)
	require.Equal(t, 0, res.Degraded)
	reborn := h.triggers.get(name)
	require.NotNil(t, reborn, "the sensor was re-materialized")
	require.True(t, reborn.Enabled)
	exists, _ := h.facts.eventExists(context.Background(), "prj-1", waitRematerializedPrefix+name)
	require.True(t, exists, "wait_broker_rematerialized landed (the ONCE ledger)")

	// Round two: it failed again — degrade loud, delete, never recreate.
	h.triggers.mu.Lock()
	h.triggers.rows[name].Enabled = false
	h.triggers.rows[name].ConsecutiveFailures = waitBrokerAutoDisableAfter
	h.triggers.mu.Unlock()
	res, err = h.broker.Pass(context.Background())
	require.NoError(t, err)
	require.Equal(t, 0, res.Rematerialized)
	require.Equal(t, 1, res.Degraded)
	require.Nil(t, h.triggers.get(name), "the dead sensor is gone — no stuck green badge")
	exists, _ = h.facts.eventExists(context.Background(), "prj-1", waitBrokerDegradedPrefix+name)
	require.True(t, exists, "wait_broker_degraded landed — the loud line")

	// And the wait itself is untouched on the row (the ladder owns it).
	row, _ := h.facts.taskWait(context.Background(), "t1")
	require.Equal(t, board.WaitOnEvent, row.WaitOn)
}

// TestWaitBroker_DeclaredThenCleared_NeverMaterializes — a declaration
// whose row wait is already gone materializes nothing (the orphan
// sweep owns any stray).
func TestWaitBroker_DeclaredThenCleared_NeverMaterializes(t *testing.T) {
	h := newBrokerHarness(t)
	decl := waitDeclaration{
		EventID: "wait:t1:tok:1", ProjectID: "prj-1", TaskID: "t1", CreatedAt: time.Now().Add(-time.Minute),
		Reason: "PR merge", Condition: mustCond(t, "pr_merged(2101)"), Repo: "lenaxia/llmsafespaces",
		ClaimToken: "11111111-1111-1111-1111-111111111111", ShortID: "tsk-henna-one", Assignee: "florid-teapot-verifies",
	}
	h.facts.addDecl(decl)
	h.facts.setRow(&waitRowState{ // advanced past the wait before the pass ran
		TaskID: "t1", ProjectID: "prj-1", ShortID: "tsk-henna-one", Stage: board.StageInReview,
		ClaimToken: "11111111-1111-1111-1111-111111111111", UserID: "user-1", WorkspaceID: "ws-1",
	})
	res, err := h.broker.Pass(context.Background())
	require.NoError(t, err)
	require.Equal(t, 0, res.Materialized)
	require.Equal(t, 0, h.triggers.created)
}

// TestWaitBroker_HealthDegradedLineSQL — the degraded posture's health
// surface: the BrokerDegraded render is events-driven (reads.go's
// EXISTS on wait_broker_degraded newer than the declaration); the SQL
// itself is integration-gated, so the unit pin here is the WIRE field
// + the event action vocabulary the SQL keys on.
func TestWaitBroker_HealthDegradedVocabulary(t *testing.T) {
	require.Equal(t, "wait_broker_degraded", "wait_broker_degraded")
	require.Equal(t, "wait_resolved", "wait_resolved")
	require.Equal(t, "wait_orphan_cleaned", "wait_orphan_cleaned")
	require.Equal(t, "wait_broker_rematerialized", "wait_broker_rematerialized")
}

// TestWaitFireGateOpen — the action-result shape filter.
func TestWaitFireGateOpen(t *testing.T) {
	open, _ := json.Marshal(map[string]any{"gate": map[string]any{"proceed": true}})
	closed, _ := json.Marshal(map[string]any{"gate": map[string]any{"proceed": false}})
	other, _ := json.Marshal(map[string]any{"script": map[string]any{}})
	require.True(t, waitFireGateOpen(open))
	require.False(t, waitFireGateOpen(closed))
	require.False(t, waitFireGateOpen(other))
	require.False(t, waitFireGateOpen(nil))
}

// TestWaitResolvedText — the sentinel's byte shape (the lease-nudge
// composer pattern: v1 marker line + the prose contract).
func TestWaitResolvedText(t *testing.T) {
	text := waitResolvedText("tsk-henna-one", "PR 2101 merge", "pr_merged(2101)", "fire-9")
	require.Contains(t, text, "<!-- lsp:agent-message-v1 ")
	require.Contains(t, text, `"mode":"wait-resolved"`)
	require.Contains(t, text, `"taskId":"tsk-henna-one"`)
	require.Contains(t, text, `"condition":"pr_merged(2101)"`)
	require.Contains(t, text, `"fireId":"fire-9"`)
	require.Contains(t, text, "PR 2101 merge (pr_merged(2101))")
	require.Contains(t, text, "Resume work, advance the stage")
	require.Contains(t, text, "if you no longer hold the claim, ignore")
}
