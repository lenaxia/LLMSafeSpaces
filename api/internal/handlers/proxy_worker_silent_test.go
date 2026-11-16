// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package handlers

// proxy_worker_silent_test.go — the #2047 silent-hang pins (parent
// #2040): the 2026-11-15 incident's workers were SILENT, not busy —
// session_hung (busy-age evidence) was structurally blind to them and
// zero alerts fired while 3 of 4 board slots were held 4+ hours. The
// worker_silent arm: a claimed in_progress board task whose assignee's
// last WORKER-activity stamp is past the threshold AND whose task
// progress clock (post-#2043 split semantics) is equally stale alerts
// SESSION-SCOPED, notify-only, with the task facts the on-call needs
// to see WHICH slot is held.

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lenaxia/llmsafespaces/api/internal/services/eventbroker"
	"github.com/lenaxia/llmsafespaces/pkg/types"
)

// fakeQuietLookup scripts the board seam: per-workspace facts plus the
// asked-workspace tape.
type fakeQuietLookup struct {
	mu    sync.Mutex
	facts map[string][]QuietTaskFact
	asked []string
}

func (f *fakeQuietLookup) InProgressAssignees(_ context.Context, workspaceID string) []QuietTaskFact {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.asked = append(f.asked, workspaceID)
	return append([]QuietTaskFact{}, f.facts[workspaceID]...)
}

func (f *fakeQuietLookup) set(workspaceID string, facts ...QuietTaskFact) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.facts == nil {
		f.facts = map[string][]QuietTaskFact{}
	}
	f.facts[workspaceID] = append([]QuietTaskFact{}, facts...)
}

// silentFakeAlerts records record/resolve calls so the persistence leg
// is observable without a database (the d6 fake pattern, extended with
// the session-scoped resolve + task facts).
type silentFakeAlerts struct {
	mu       sync.Mutex
	recorded []string // "record:<ws>:<session>:<alert>" / "resolve:<ws>:<session>"
	facts    []map[string]string
}

func (f *silentFakeAlerts) RecordAlert(workspaceID, sessionID, alert string, _ int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.recorded = append(f.recorded, "record:"+workspaceID+":"+sessionID+":"+alert)
}

func (f *silentFakeAlerts) RecordTaskAlert(workspaceID, sessionID, alert string, _ int, facts map[string]string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.recorded = append(f.recorded, "record:"+workspaceID+":"+sessionID+":"+alert)
	f.facts = append(f.facts, facts)
}

func (f *silentFakeAlerts) ResolveWorkspace(workspaceID string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.recorded = append(f.recorded, "resolve:"+workspaceID)
}

func (f *silentFakeAlerts) ResolveSession(workspaceID, sessionID string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.recorded = append(f.recorded, "resolve:"+workspaceID+":"+sessionID)
}

func (f *silentFakeAlerts) ListByWorkspace(_ context.Context, _ string, _ int) ([]types.SessionAlert, error) {
	return nil, nil
}
func (f *silentFakeAlerts) Start() error { return nil }
func (f *silentFakeAlerts) Stop() error  { return nil }

func (f *silentFakeAlerts) calls() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string{}, f.recorded...)
}

// newSilentEnv wires the minimal evaluator environment: a handler with
// a user broker (SSE capture) and the scripted lookup. No statusz /
// phase machinery — the silent arm never touches the pod.
func newSilentEnv(t *testing.T, lookup *fakeQuietLookup) (*testEnv, *eventbroker.UserEventBroker, *silentFakeAlerts) {
	t.Helper()
	env := newTestEnv(t)
	broker := eventbroker.NewUserEventBroker()
	env.handler.userBroker = broker
	env.handler.SetQuietBoardLookup(lookup)
	alerts := &silentFakeAlerts{}
	env.handler.sessionAlerts = alerts
	return env, broker, alerts
}

// silentFact builds one in_progress fact: quiet seconds (nil = the
// unknown-stamp shape), optional busy truth, and a task progress clock
// aged progressAgo (nil = no progress evidence).
func silentFact(session string, quiet *int, busy bool, taskShortID string, progressAgo *time.Duration) QuietTaskFact {
	f := QuietTaskFact{
		TaskShortID:     taskShortID,
		ProjectID:       "prj-silent",
		LeaseState:      "live",
		AssigneeSession: session,
		QuietSeconds:    quiet,
		Busy:            busy,
	}
	if progressAgo != nil {
		at := time.Now().Add(-*progressAgo)
		f.LastProgressAt = &at
	}
	return f
}

func quietSecs(seconds int) *int { return &seconds }

func mustDrainAlert(t *testing.T, sub *eventbroker.Subscriber) map[string]any {
	t.Helper()
	select {
	case evt := <-sub.Ch:
		require.Equal(t, "workspace.alert", evt.Type)
		require.Equal(t, "worker_silent", evt.Status)
		data, ok := evt.Data.(map[string]any)
		require.True(t, ok, "alert data must be a map")
		return data
	case <-time.After(2 * time.Second):
		t.Fatal("expected a worker_silent alert event")
		return nil
	}
}

func assertNoAlert(t *testing.T, sub *eventbroker.Subscriber) {
	t.Helper()
	select {
	case evt := <-sub.Ch:
		t.Fatalf("no worker_silent alert expected: got %+v", evt)
	default:
	}
}

// TestWorkerSilent_FiresOnSilentInProgressWorker is the born-red pin:
// a claimed in_progress task whose assignee's worker-activity stamp is
// 45m stale AND whose task progress clock is 45m stale alerts —
// session-scoped, with the task facts, persisted.
func TestWorkerSilent_FiresOnSilentInProgressWorker(t *testing.T) {
	lookup := &fakeQuietLookup{}
	progress := 45 * time.Minute
	lookup.set("ws-1", silentFact("ses-silent", quietSecs(45*60), false, "tsk-silent-one", &progress))

	env, broker, alerts := newSilentEnv(t, lookup)
	sub, err := broker.SubscribeWorkspace("ws-1")
	require.NoError(t, err)
	defer broker.UnsubscribeWorkspace("ws-1", sub)

	env.handler.escalateSilentWorkers([]string{"ws-1"})

	data := mustDrainAlert(t, sub)
	assert.Equal(t, "worker_silent", data["alert"])
	assert.Equal(t, "tsk-silent-one", data["task_short_id"], "the alert names WHICH slot is held")
	assert.Equal(t, "prj-silent", data["project_id"])
	assert.Equal(t, "live", data["lease_state"])
	assert.Equal(t, "notify_only", data["policy"])
	assert.Equal(t, 2700, data["assignee_quiet_seconds"])

	assert.Equal(t, []string{"record:ws-1:ses-silent:worker_silent"}, alerts.calls(),
		"the alert must be persisted with the session scope")
	require.Len(t, alerts.facts, 1)
	assert.Equal(t, "tsk-silent-one", alerts.facts[0]["task_short_id"])
	assert.Equal(t, "prj-silent", alerts.facts[0]["project_id"])
	assert.Equal(t, "live", alerts.facts[0]["lease_state"])
}

// TestWorkerSilent_ActiveWorkerNeverAlerts: fresh worker activity
// (quiet well under the threshold) is healthy — no alert, ever.
func TestWorkerSilent_ActiveWorkerNeverAlerts(t *testing.T) {
	lookup := &fakeQuietLookup{}
	progress := 4 * time.Minute
	lookup.set("ws-1", silentFact("ses-active", quietSecs(4*60), false, "tsk-active", &progress))

	env, broker, alerts := newSilentEnv(t, lookup)
	sub, err := broker.SubscribeWorkspace("ws-1")
	require.NoError(t, err)
	defer broker.UnsubscribeWorkspace("ws-1", sub)

	env.handler.escalateSilentWorkers([]string{"ws-1"})
	assertNoAlert(t, sub)
	assert.Empty(t, alerts.calls())
}

// TestWorkerSilent_UnknownStampNeverAlerts: a session this process has
// never observed (hub restart, unindexed) has NO stamp — that is
// unknown, not quiet, and unknown NEVER alerts.
func TestWorkerSilent_UnknownStampNeverAlerts(t *testing.T) {
	lookup := &fakeQuietLookup{}
	progress := 4 * time.Hour
	lookup.set("ws-1", silentFact("ses-unknown", nil, false, "tsk-unknown", &progress))

	env, broker, alerts := newSilentEnv(t, lookup)
	sub, err := broker.SubscribeWorkspace("ws-1")
	require.NoError(t, err)
	defer broker.UnsubscribeWorkspace("ws-1", sub)

	env.handler.escalateSilentWorkers([]string{"ws-1"})
	assertNoAlert(t, sub)
	assert.Empty(t, alerts.calls())
}

// TestWorkerSilent_BusyWorkerNeverAlerts: derived-busy is ALIVE
// (2026-10-29 ruling — mid-tool-call silence must not alert) however
// stale the frame index reads.
func TestWorkerSilent_BusyWorkerNeverAlerts(t *testing.T) {
	lookup := &fakeQuietLookup{}
	progress := 45 * time.Minute
	lookup.set("ws-1", silentFact("ses-busy", quietSecs(45*60), true, "tsk-busy", &progress))

	env, broker, alerts := newSilentEnv(t, lookup)
	sub, err := broker.SubscribeWorkspace("ws-1")
	require.NoError(t, err)
	defer broker.UnsubscribeWorkspace("ws-1", sub)

	env.handler.escalateSilentWorkers([]string{"ws-1"})
	assertNoAlert(t, sub)
	assert.Empty(t, alerts.calls())
}

// TestWorkerSilent_FreshTaskProgressNeverAlerts: the two-legged gate —
// quiet frames alone do not alert while the task's progress clock
// (post-#2043: claim mints + worker heartbeats) is fresh.
func TestWorkerSilent_FreshTaskProgressNeverAlerts(t *testing.T) {
	lookup := &fakeQuietLookup{}
	progress := 5 * time.Minute
	lookup.set("ws-1", silentFact("ses-progress-fresh", quietSecs(45*60), false, "tsk-fresh", &progress))

	env, broker, alerts := newSilentEnv(t, lookup)
	sub, err := broker.SubscribeWorkspace("ws-1")
	require.NoError(t, err)
	defer broker.UnsubscribeWorkspace("ws-1", sub)

	env.handler.escalateSilentWorkers([]string{"ws-1"})
	assertNoAlert(t, sub)
	assert.Empty(t, alerts.calls())
}

// TestWorkerSilent_NoProgressEvidenceNeverAlerts: a NULL progress
// clock is evidence-unavailable (the #1685 fail-toward-silence
// precedent) — never a silent verdict.
func TestWorkerSilent_NoProgressEvidenceNeverAlerts(t *testing.T) {
	lookup := &fakeQuietLookup{}
	lookup.set("ws-1", silentFact("ses-noprogress", quietSecs(45*60), false, "tsk-noprogress", nil))

	env, broker, alerts := newSilentEnv(t, lookup)
	sub, err := broker.SubscribeWorkspace("ws-1")
	require.NoError(t, err)
	defer broker.UnsubscribeWorkspace("ws-1", sub)

	env.handler.escalateSilentWorkers([]string{"ws-1"})
	assertNoAlert(t, sub)
	assert.Empty(t, alerts.calls())
}

// TestWorkerSilent_ResolvesOnResumedActivity mirrors the d6 resolution
// flow: after an alert, a sweep observing positive liveness (worker
// activity resumed) emits workspace.alert_resolved, persists the
// session-scoped resolution, and drops the cooldown — a fresh silence
// re-alerts immediately instead of waiting out a stale cooldown.
func TestWorkerSilent_ResolvesOnResumedActivity(t *testing.T) {
	origCooldown := silentWorkerCooldown
	silentWorkerCooldown = time.Hour // deterministic: cooldown spans the test
	t.Cleanup(func() { silentWorkerCooldown = origCooldown })

	lookup := &fakeQuietLookup{}
	stale := 45 * time.Minute
	fresh := 4 * time.Minute
	lookup.set("ws-1", silentFact("ses-recover", quietSecs(45*60), false, "tsk-recover", &stale))

	env, broker, alerts := newSilentEnv(t, lookup)
	sub, err := broker.SubscribeWorkspace("ws-1")
	require.NoError(t, err)
	defer broker.UnsubscribeWorkspace("ws-1", sub)

	env.handler.escalateSilentWorkers([]string{"ws-1"})
	mustDrainAlert(t, sub)

	// The worker wakes: fresh frames AND fresh task progress.
	lookup.set("ws-1", silentFact("ses-recover", quietSecs(4*60), false, "tsk-recover", &fresh))
	env.handler.escalateSilentWorkers([]string{"ws-1"})

	select {
	case evt := <-sub.Ch:
		require.Equal(t, "workspace.alert_resolved", evt.Type)
		require.Equal(t, "worker_silent", evt.Status)
		require.Equal(t, "ses-recover", evt.SessionID, "the resolve is session-scoped")
		data, ok := evt.Data.(map[string]any)
		require.True(t, ok)
		assert.Equal(t, "worker_silent", data["alert"])
	case <-time.After(2 * time.Second):
		t.Fatal("resumed activity must emit workspace.alert_resolved")
	}
	assert.Contains(t, alerts.calls(), "resolve:ws-1:ses-recover",
		"the resolution must be persisted session-scoped")

	// Cooldown dropped: a fresh silence re-alerts immediately.
	lookup.set("ws-1", silentFact("ses-recover", quietSecs(45*60), false, "tsk-recover", &stale))
	env.handler.escalateSilentWorkers([]string{"ws-1"})
	mustDrainAlert(t, sub)
}

// TestWorkerSilent_CooldownPreventsRepeats: still silent inside the
// cooldown window — no re-alert AND no resolution (the silence
// persists), exactly the d6 discipline.
func TestWorkerSilent_CooldownPreventsRepeats(t *testing.T) {
	origCooldown := silentWorkerCooldown
	silentWorkerCooldown = time.Hour
	t.Cleanup(func() { silentWorkerCooldown = origCooldown })

	lookup := &fakeQuietLookup{}
	stale := 45 * time.Minute
	lookup.set("ws-1", silentFact("ses-quiet", quietSecs(45*60), false, "tsk-quiet", &stale))

	env, broker, alerts := newSilentEnv(t, lookup)
	sub, err := broker.SubscribeWorkspace("ws-1")
	require.NoError(t, err)
	defer broker.UnsubscribeWorkspace("ws-1", sub)

	env.handler.escalateSilentWorkers([]string{"ws-1"})
	mustDrainAlert(t, sub)

	env.handler.escalateSilentWorkers([]string{"ws-1"})
	select {
	case evt := <-sub.Ch:
		t.Fatalf("cooldown must suppress repeats: got %+v", evt)
	default:
	}
	assert.Equal(t, []string{"record:ws-1:ses-quiet:worker_silent"}, alerts.calls(),
		"exactly one persisted alert per cooldown window")
}

// TestWorkerSilent_CooldownExpiry: past the cooldown window, a still-
// silent worker re-alerts (the condition persists — recovery detection
// must not ride the cooldown). The floor moves with session_hung's
// configured cooldown, so BOTH knobs drop for the window test.
func TestWorkerSilent_CooldownExpiry(t *testing.T) {
	origCooldown := silentWorkerCooldown
	origBusyCooldown := busyAlertCooldown
	silentWorkerCooldown = 50 * time.Millisecond
	busyAlertCooldown = 50 * time.Millisecond
	t.Cleanup(func() {
		silentWorkerCooldown = origCooldown
		busyAlertCooldown = origBusyCooldown
	})

	lookup := &fakeQuietLookup{}
	stale := 45 * time.Minute
	lookup.set("ws-1", silentFact("ses-stuck", quietSecs(45*60), false, "tsk-stuck", &stale))

	env, broker, _ := newSilentEnv(t, lookup)
	sub, err := broker.SubscribeWorkspace("ws-1")
	require.NoError(t, err)
	defer broker.UnsubscribeWorkspace("ws-1", sub)

	env.handler.escalateSilentWorkers([]string{"ws-1"})
	mustDrainAlert(t, sub)

	time.Sleep(80 * time.Millisecond)
	env.handler.escalateSilentWorkers([]string{"ws-1"})
	mustDrainAlert(t, sub) // re-fires: still silent
}

// TestWorkerSilent_ThreeWorkersThreeDistinctAlerts: the fatigue
// contract — 3 silent workers in one workspace produce 3 distinct
// SESSION-SCOPED alerts (one per session), not one workspace-wide
// storm.
func TestWorkerSilent_ThreeWorkersThreeDistinctAlerts(t *testing.T) {
	lookup := &fakeQuietLookup{}
	stale := 4 * time.Hour
	lookup.set("ws-1",
		silentFact("ses-s1", quietSecs(4*60*60), false, "tsk-h1", &stale),
		silentFact("ses-s2", quietSecs(4*60*60), false, "tsk-h2", &stale),
		silentFact("ses-s3", quietSecs(4*60*60), false, "tsk-h3", &stale),
	)

	env, broker, alerts := newSilentEnv(t, lookup)
	sub, err := broker.SubscribeWorkspace("ws-1")
	require.NoError(t, err)
	defer broker.UnsubscribeWorkspace("ws-1", sub)

	env.handler.escalateSilentWorkers([]string{"ws-1"})

	bySession := map[string]string{}
	for range 3 {
		select {
		case evt := <-sub.Ch:
			require.Equal(t, "worker_silent", evt.Status)
			data, ok := evt.Data.(map[string]any)
			require.True(t, ok)
			bySession[evt.SessionID] = data["task_short_id"].(string)
		case <-time.After(2 * time.Second):
			t.Fatal("each silent worker gets its own alert")
		}
	}
	assert.Equal(t, map[string]string{
		"ses-s1": "tsk-h1", "ses-s2": "tsk-h2", "ses-s3": "tsk-h3",
	}, bySession, "3 silent workers → 3 distinct session-scoped alerts")
	assert.Len(t, alerts.calls(), 3)
	assertNoAlert(t, sub) // exactly three, no storm
}

// TestWorkerSilent_UnknownStampNeverResolves: unknown is not evidence
// of ANYTHING — an alerted session whose stamp is lost (hub restart)
// is neither re-alerted nor resolved by the unknown shape; the
// read-side stale-heal bounds the dangler.
func TestWorkerSilent_UnknownStampNeverResolves(t *testing.T) {
	origCooldown := silentWorkerCooldown
	silentWorkerCooldown = time.Hour
	t.Cleanup(func() { silentWorkerCooldown = origCooldown })

	lookup := &fakeQuietLookup{}
	stale := 45 * time.Minute
	lookup.set("ws-1", silentFact("ses-lost", quietSecs(45*60), false, "tsk-lost", &stale))

	env, broker, alerts := newSilentEnv(t, lookup)
	sub, err := broker.SubscribeWorkspace("ws-1")
	require.NoError(t, err)
	defer broker.UnsubscribeWorkspace("ws-1", sub)

	env.handler.escalateSilentWorkers([]string{"ws-1"})
	mustDrainAlert(t, sub)

	lookup.set("ws-1", silentFact("ses-lost", nil, false, "tsk-lost", &stale))
	env.handler.escalateSilentWorkers([]string{"ws-1"})
	select {
	case evt := <-sub.Ch:
		t.Fatalf("unknown stamp must neither alert nor resolve: got %+v", evt)
	default:
	}
	assert.NotContains(t, alerts.calls(), "resolve:ws-1:ses-lost")
}

// TestWorkerSilent_ThresholdFloor: the fatigue guard — the threshold
// cannot be configured below the 30m floor, and the cooldown cannot
// drop below session_hung's (busyAlertCooldown).
func TestWorkerSilent_ThresholdFloor(t *testing.T) {
	origThreshold := silentWorkerOlderThan
	origCooldown := silentWorkerCooldown
	silentWorkerOlderThan = time.Minute // misconfigured below the floor
	silentWorkerCooldown = time.Second  // misconfigured below session_hung's
	t.Cleanup(func() {
		silentWorkerOlderThan = origThreshold
		silentWorkerCooldown = origCooldown
	})

	assert.GreaterOrEqual(t, silentWorkerThreshold(), 30*time.Minute,
		"the effective threshold is floored at 30m")
	assert.GreaterOrEqual(t, silentWorkerCooldownWindow(), busyAlertCooldown,
		"the effective cooldown is floored at session_hung's")

	// 20m of silence must not alert under a 1m configured threshold:
	// the floor holds at evaluation time too.
	lookup := &fakeQuietLookup{}
	stale := 20 * time.Minute
	lookup.set("ws-1", silentFact("ses-floor", quietSecs(20*60), false, "tsk-floor", &stale))
	env, broker, alerts := newSilentEnv(t, lookup)
	sub, err := broker.SubscribeWorkspace("ws-1")
	require.NoError(t, err)
	defer broker.UnsubscribeWorkspace("ws-1", sub)

	env.handler.escalateSilentWorkers([]string{"ws-1"})
	assertNoAlert(t, sub)
	assert.Empty(t, alerts.calls())
}

// TestWorkerSilent_NilLookupInert: no seam wired (board disabled,
// pre-wiring boot) — the arm is inert, never panics, never alerts.
func TestWorkerSilent_NilLookupInert(t *testing.T) {
	env := newTestEnv(t)
	broker := eventbroker.NewUserEventBroker()
	env.handler.userBroker = broker
	sub, err := broker.SubscribeWorkspace("ws-1")
	require.NoError(t, err)
	defer broker.UnsubscribeWorkspace("ws-1", sub)

	assert.NotPanics(t, func() { env.handler.escalateSilentWorkers([]string{"ws-1"}) })
	assertNoAlert(t, sub)
}
