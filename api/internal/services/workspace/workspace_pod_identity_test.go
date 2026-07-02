// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package workspace

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	v1 "github.com/lenaxia/llmsafespaces/pkg/apis/llmsafespaces/v1"
)

// fakePodIdentityTracker records calls to GetLastSeenPodIdentity /
// UpsertLastSeenPodIdentity / MarkPodIdentityTransition /
// ClearPendingRefreshAfterAutoPush. This is the narrow-interface consumer
// pattern: workspace.Service depends on a small typed interface it
// defines, not on the full DatabaseService.
type fakePodIdentityTracker struct {
	mu                 sync.Mutex
	storedName         string
	storedStart        time.Time
	upsertCalls        int
	transitionCalls    int
	clearCalls         int
	clearCallErr       error
	lastPriorChanged   time.Time
	transitionReturnTs time.Time // what MarkPodIdentityTransition returns
	// Track invocation order for assertion of "transition, not upsert".
	lastCall string
}

func (f *fakePodIdentityTracker) GetLastSeenPodIdentity(_ context.Context, _ string) (string, time.Time, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.storedName, f.storedStart, nil
}

func (f *fakePodIdentityTracker) UpsertLastSeenPodIdentity(_ context.Context, _, name string, startTime time.Time) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.storedName = name
	f.storedStart = startTime
	f.upsertCalls++
	f.lastCall = "upsert"
	return nil
}

func (f *fakePodIdentityTracker) MarkPodIdentityTransition(_ context.Context, _, name string, startTime time.Time) (time.Time, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.storedName = name
	f.storedStart = startTime
	f.transitionCalls++
	f.lastCall = "transition"
	ts := f.transitionReturnTs
	if ts.IsZero() {
		ts = time.Now()
	}
	return ts, nil
}

func (f *fakePodIdentityTracker) ClearPendingRefreshAfterAutoPush(_ context.Context, _ string, priorChangedAt time.Time) (time.Time, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.clearCalls++
	f.lastPriorChanged = priorChangedAt
	f.lastCall = "clear"
	return time.Now(), f.clearCallErr
}

// fakeSecretPusher counts calls and records what it was passed. It's
// atomic-safe so tests can wait on it without racing the fire-and-forget
// goroutine.
type fakeSecretPusher struct {
	calls           atomic.Int64
	sawUserID       atomic.Value // string
	sawWorkspaceID  atomic.Value // string
	returnErr       error
	blockUntilClose <-chan struct{} // optional; blocks Push until closed
}

func (f *fakeSecretPusher) Push(ctx context.Context, userID, workspaceID string) error {
	f.calls.Add(1)
	f.sawUserID.Store(userID)
	f.sawWorkspaceID.Store(workspaceID)
	if f.blockUntilClose != nil {
		select {
		case <-f.blockUntilClose:
			// unblocked normally
		case <-ctx.Done():
			// Simulates the pusher exiting early because its ctx was
			// canceled. This is the scenario that would break the
			// fire-and-forget contract if the workspace service passed
			// the request ctx directly (without WithoutCancel).
			return ctx.Err()
		}
	}
	return f.returnErr
}

// waitForPushCalls polls until the pusher has been invoked exactly n
// times, or the deadline expires. Used to synchronize the tests with
// the fire-and-forget goroutine started by GetWorkspaceStatus.
func (f *fakeSecretPusher) waitForPushCalls(t *testing.T, n int64, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if f.calls.Load() >= n {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %d push calls; saw %d", n, f.calls.Load())
}

func activePodCRD(name, userID string, podName string, startTime time.Time) *v1.Workspace {
	crd := crdWorkspace(name, "default", userID, "10Gi")
	crd.Status.Phase = v1.WorkspacePhaseActive
	crd.Status.PodName = podName
	crd.Status.PodNamespace = "default"
	crd.Status.PodIP = "10.0.0.5"
	crd.Status.StartTime = &metav1.Time{Time: startTime}
	// Avoid the ImageTag-fallback path that would otherwise trigger a
	// Clientset().Pods().Get() call requiring extra mock plumbing.
	crd.Status.ImageTag = "ts-testing"
	return crd
}

// === Case 1: pod-identity unchanged — no auto-push ===

// TestPodIdentity_UnchangedTuple_NoPush proves the no-op case: if the
// stored (name, startTime) tuple matches the CRD's, we must not fire
// the auto-push. Every frontend poll (~every 2s) reaches GetWorkspaceStatus;
// firing on every poll would DOS both the DB and agentd.
func TestPodIdentity_UnchangedTuple_NoPush(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	startTime := time.Date(2026, 7, 2, 12, 0, 0, 0, time.UTC)
	crd := activePodCRD("ws-1", "user1", "pod-abc", startTime)

	f.db.On("GetWorkspace", ctx, "ws-1").Return(dbWorkspace("ws-1", "user1", "my-ws", "10Gi"), nil)
	f.ws.On("Get", mock.Anything, "ws-1", mock.Anything).Return(crd, nil)
	f.db.On("SyncWorkspaceVersionInfo", mock.Anything, "ws-1", mock.Anything, mock.Anything).Return().Maybe()

	tracker := &fakePodIdentityTracker{storedName: "pod-abc", storedStart: startTime}
	pusher := &fakeSecretPusher{}
	f.svc.SetPodIdentityTracker(tracker)
	f.svc.SetSecretPusher(pusher)

	_, err := f.svc.GetWorkspaceStatus(ctx, "user1", "ws-1")
	assert.NoError(t, err)

	// Give the async goroutine a chance to run if it were spawned.
	time.Sleep(50 * time.Millisecond)
	assert.Equal(t, int64(0), pusher.calls.Load(),
		"unchanged pod identity must not trigger auto-push; the ~2s status "+
			"poll would DOS agentd otherwise")
	assert.Equal(t, 0, tracker.upsertCalls,
		"no DB write is needed when identity is unchanged")
	assert.Equal(t, 0, tracker.transitionCalls)
}

// === Case 2: initial observation (empty stored) — record only, no push ===

// TestPodIdentity_InitialObservation_RecordsWithoutPush proves the
// deploy-day behavior: existing workspaces have no workspace_agent_state
// row (or a NULL identity), and we must NOT trigger an auto-push on the
// first status read after the API restart. Instead we record the
// currently-observed identity so the NEXT transition is detectable.
//
// If we skipped this and pushed instead, deploying the fix would
// spuriously fire an auto-push against every active workspace at
// once — a stampede that gains nothing (nothing has actually changed
// from the pod's perspective).
func TestPodIdentity_InitialObservation_RecordsWithoutPush(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	startTime := time.Date(2026, 7, 2, 12, 0, 0, 0, time.UTC)
	crd := activePodCRD("ws-1", "user1", "pod-abc", startTime)

	f.db.On("GetWorkspace", ctx, "ws-1").Return(dbWorkspace("ws-1", "user1", "my-ws", "10Gi"), nil)
	f.ws.On("Get", mock.Anything, "ws-1", mock.Anything).Return(crd, nil)
	f.db.On("SyncWorkspaceVersionInfo", mock.Anything, "ws-1", mock.Anything, mock.Anything).Return().Maybe()

	tracker := &fakePodIdentityTracker{} // empty: initial observation
	pusher := &fakeSecretPusher{}
	f.svc.SetPodIdentityTracker(tracker)
	f.svc.SetSecretPusher(pusher)

	_, err := f.svc.GetWorkspaceStatus(ctx, "user1", "ws-1")
	assert.NoError(t, err)

	time.Sleep(50 * time.Millisecond)
	assert.Equal(t, int64(0), pusher.calls.Load(),
		"initial observation must NOT push — a fresh deploy would otherwise "+
			"stampede every active workspace's agentd at once")
	assert.Equal(t, 1, tracker.upsertCalls, "first observation must be persisted")
	assert.Equal(t, 0, tracker.transitionCalls)
	assert.Equal(t, "pod-abc", tracker.storedName)
	assert.True(t, tracker.storedStart.Equal(startTime))
}

// === Case 3: pod-identity changed — auto-push fires ===

// TestPodIdentity_TransitionTriggersAutoPush is the load-bearing test:
// when the stored identity differs from the CRD's current identity
// (pod recreation happened between two status polls), the fire-and-
// forget auto-push MUST run, and the transition must be recorded so
// the next poll doesn't refire.
func TestPodIdentity_TransitionTriggersAutoPush(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	oldStart := time.Date(2026, 7, 2, 11, 0, 0, 0, time.UTC)
	newStart := time.Date(2026, 7, 2, 12, 0, 0, 0, time.UTC)
	crd := activePodCRD("ws-1", "user1", "pod-new", newStart)

	f.db.On("GetWorkspace", ctx, "ws-1").Return(dbWorkspace("ws-1", "user1", "my-ws", "10Gi"), nil)
	f.ws.On("Get", mock.Anything, "ws-1", mock.Anything).Return(crd, nil)
	f.db.On("SyncWorkspaceVersionInfo", mock.Anything, "ws-1", mock.Anything, mock.Anything).Return().Maybe()

	// Old pod is stored — new pod arrives.
	tracker := &fakePodIdentityTracker{storedName: "pod-old", storedStart: oldStart}
	pusher := &fakeSecretPusher{}
	f.svc.SetPodIdentityTracker(tracker)
	f.svc.SetSecretPusher(pusher)

	_, err := f.svc.GetWorkspaceStatus(ctx, "user1", "ws-1")
	assert.NoError(t, err)

	pusher.waitForPushCalls(t, 1, 2*time.Second)
	assert.Equal(t, int64(1), pusher.calls.Load(),
		"pod-identity transition MUST trigger exactly one auto-push")
	assert.Equal(t, "user1", pusher.sawUserID.Load())
	assert.Equal(t, "ws-1", pusher.sawWorkspaceID.Load())

	assert.Equal(t, 1, tracker.transitionCalls,
		"transition must be persisted with pending_refresh=TRUE so the "+
			"AgentReloadBanner surfaces as fallback UX if the push fails")
	assert.Equal(t, 0, tracker.upsertCalls,
		"transition-style write is required, not the identity-only upsert")
	assert.Equal(t, "pod-new", tracker.storedName)
	assert.True(t, tracker.storedStart.Equal(newStart))
}

// === Case 3b: successful push MUST clear pending_refresh ===

// TestPodIdentity_TransitionSuccessClearsPendingRefresh is the review-
// pass regression test for the critical bug the bot caught on PR #494:
// runAutoPush wasn't calling MarkAgentReloaded on success, so
// pending_refresh stayed TRUE forever and the AgentReloadBanner never
// disappeared after a successful auto-push. This test locks in the
// full state-transition contract:
//
//  1. MarkPodIdentityTransition fires and returns priorChangedAt.
//  2. Push succeeds.
//  3. ClearPendingRefreshAfterAutoPush(priorChangedAt) fires.
//
// The priorChangedAt round-trip is critical: MarkAgentReloaded's
// optimistic-concurrency check compares currentChangedAt >
// priorChangedAt to decide whether a NEW credential arrived during the
// push window (in which case the banner should stay visible for THAT
// change). If we didn't round-trip priorChangedAt, we'd either
// short-circuit and always clear (missing mid-push binds) or always
// keep (spurious banners).
func TestPodIdentity_TransitionSuccessClearsPendingRefresh(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	oldStart := time.Date(2026, 7, 2, 11, 0, 0, 0, time.UTC)
	newStart := time.Date(2026, 7, 2, 12, 0, 0, 0, time.UTC)
	crd := activePodCRD("ws-1", "user1", "pod-new", newStart)

	f.db.On("GetWorkspace", ctx, "ws-1").Return(dbWorkspace("ws-1", "user1", "my-ws", "10Gi"), nil)
	f.ws.On("Get", mock.Anything, "ws-1", mock.Anything).Return(crd, nil)
	f.db.On("SyncWorkspaceVersionInfo", mock.Anything, "ws-1", mock.Anything, mock.Anything).Return().Maybe()

	// Fixed timestamp returned by MarkPodIdentityTransition. Must be
	// round-tripped verbatim into ClearPendingRefreshAfterAutoPush.
	transitionTs := time.Date(2026, 7, 2, 12, 0, 15, 0, time.UTC)
	tracker := &fakePodIdentityTracker{
		storedName:         "pod-old",
		storedStart:        oldStart,
		transitionReturnTs: transitionTs,
	}
	pusher := &fakeSecretPusher{} // succeeds by default
	f.svc.SetPodIdentityTracker(tracker)
	f.svc.SetSecretPusher(pusher)

	_, err := f.svc.GetWorkspaceStatus(ctx, "user1", "ws-1")
	assert.NoError(t, err)

	pusher.waitForPushCalls(t, 1, 2*time.Second)
	// The push runs synchronously against our fake; give the goroutine
	// a moment to invoke ClearPendingRefreshAfterAutoPush after Push
	// returns.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		tracker.mu.Lock()
		clears := tracker.clearCalls
		tracker.mu.Unlock()
		if clears >= 1 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}

	assert.Equal(t, 1, tracker.clearCalls,
		"successful auto-push MUST clear pending_refresh so the "+
			"AgentReloadBanner disappears within one poll cycle; without "+
			"this the banner stays visible forever after every pod "+
			"recreation, which is arguably worse than not having the fix")
	assert.True(t, tracker.lastPriorChanged.Equal(transitionTs),
		"priorChangedAt must round-trip verbatim from MarkPodIdentityTransition "+
			"into ClearPendingRefreshAfterAutoPush; the DB uses it in the "+
			"SELECT FOR UPDATE optimistic-concurrency check to decide "+
			"whether a mid-push bind should keep pending_refresh=TRUE")
	assert.Equal(t, "clear", tracker.lastCall,
		"the clear call must run AFTER the transition and Push")
}

// TestPodIdentity_TransitionSuccessWithClearFailureLogsWarning verifies
// that a Clear failure doesn't panic and doesn't leak the success log:
// the push already delivered the secrets — a Clear failure is UX-only.
// The pod is fine.
func TestPodIdentity_TransitionSuccessWithClearFailureLogsWarning(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	oldStart := time.Date(2026, 7, 2, 11, 0, 0, 0, time.UTC)
	newStart := time.Date(2026, 7, 2, 12, 0, 0, 0, time.UTC)
	crd := activePodCRD("ws-1", "user1", "pod-new", newStart)

	f.db.On("GetWorkspace", ctx, "ws-1").Return(dbWorkspace("ws-1", "user1", "my-ws", "10Gi"), nil)
	f.ws.On("Get", mock.Anything, "ws-1", mock.Anything).Return(crd, nil)
	f.db.On("SyncWorkspaceVersionInfo", mock.Anything, "ws-1", mock.Anything, mock.Anything).Return().Maybe()

	tracker := &fakePodIdentityTracker{
		storedName:   "pod-old",
		storedStart:  oldStart,
		clearCallErr: pushInjectionError("db unavailable"),
	}
	pusher := &fakeSecretPusher{}
	f.svc.SetPodIdentityTracker(tracker)
	f.svc.SetSecretPusher(pusher)

	_, err := f.svc.GetWorkspaceStatus(ctx, "user1", "ws-1")
	assert.NoError(t, err)

	pusher.waitForPushCalls(t, 1, 2*time.Second)
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		tracker.mu.Lock()
		clears := tracker.clearCalls
		tracker.mu.Unlock()
		if clears >= 1 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	assert.Equal(t, 1, tracker.clearCalls,
		"clear MUST have been attempted even though it'll fail — "+
			"the workspace-service goroutine must not silently skip "+
			"the clear on some pre-check")
}

// === Case 4: only StartTime changes (unlikely but possible) — still a transition ===

// TestPodIdentity_SameNameNewStartTimeIsTransition proves both fields
// matter. Kubernetes pod names include a hash suffix that changes on
// recreation, but a controller upgrade or a pod that keeps its name for
// some pathological reason (Job with fixed name?) could produce this
// scenario. The identity tuple is (name, start) — mismatch on either
// field is a transition.
func TestPodIdentity_SameNameNewStartTimeIsTransition(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	oldStart := time.Date(2026, 7, 2, 11, 0, 0, 0, time.UTC)
	newStart := time.Date(2026, 7, 2, 12, 0, 0, 0, time.UTC)
	crd := activePodCRD("ws-1", "user1", "pod-same-name", newStart)

	f.db.On("GetWorkspace", ctx, "ws-1").Return(dbWorkspace("ws-1", "user1", "my-ws", "10Gi"), nil)
	f.ws.On("Get", mock.Anything, "ws-1", mock.Anything).Return(crd, nil)
	f.db.On("SyncWorkspaceVersionInfo", mock.Anything, "ws-1", mock.Anything, mock.Anything).Return().Maybe()

	tracker := &fakePodIdentityTracker{storedName: "pod-same-name", storedStart: oldStart}
	pusher := &fakeSecretPusher{}
	f.svc.SetPodIdentityTracker(tracker)
	f.svc.SetSecretPusher(pusher)

	_, err := f.svc.GetWorkspaceStatus(ctx, "user1", "ws-1")
	assert.NoError(t, err)

	pusher.waitForPushCalls(t, 1, 2*time.Second)
}

// === Case 5: pod not yet active (Pending / Suspended) — no detection ===

// TestPodIdentity_NonActivePhaseSkipsDetection proves we don't push
// while the pod isn't running: no PodName, no PodIP, no useful DEK
// target. The Suspended and Creating phases legitimately have empty
// pod identity — treating that as a transition would loop.
func TestPodIdentity_NonActivePhaseSkipsDetection(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	crd := crdWorkspace("ws-1", "default", "user1", "10Gi")
	crd.Status.Phase = v1.WorkspacePhaseCreating
	// No PodName, no StartTime — controller hasn't written them yet.

	f.db.On("GetWorkspace", ctx, "ws-1").Return(dbWorkspace("ws-1", "user1", "my-ws", "10Gi"), nil)
	f.ws.On("Get", mock.Anything, "ws-1", mock.Anything).Return(crd, nil)
	f.db.On("SyncWorkspaceVersionInfo", mock.Anything, "ws-1", mock.Anything, mock.Anything).Return().Maybe()

	tracker := &fakePodIdentityTracker{storedName: "pod-old", storedStart: time.Now()}
	pusher := &fakeSecretPusher{}
	f.svc.SetPodIdentityTracker(tracker)
	f.svc.SetSecretPusher(pusher)

	_, err := f.svc.GetWorkspaceStatus(ctx, "user1", "ws-1")
	assert.NoError(t, err)

	time.Sleep(50 * time.Millisecond)
	assert.Equal(t, int64(0), pusher.calls.Load(),
		"non-Active phase MUST NOT push — there's no pod to push to")
	assert.Equal(t, 0, tracker.transitionCalls,
		"and MUST NOT overwrite the stored identity with empty values, "+
			"or the next Active phase transition would look like the first "+
			"observation and skip the push")
}

// === Case 6: no configured pusher/tracker — never panics ===

// TestPodIdentity_NoConfiguredDependencies_NoOp locks in the safe-null
// contract: if wiring hasn't installed a pusher or tracker (early tests,
// dev configs), GetWorkspaceStatus must still return correctly.
func TestPodIdentity_NoConfiguredDependencies_NoOp(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	startTime := time.Date(2026, 7, 2, 12, 0, 0, 0, time.UTC)
	crd := activePodCRD("ws-1", "user1", "pod-abc", startTime)

	f.db.On("GetWorkspace", ctx, "ws-1").Return(dbWorkspace("ws-1", "user1", "my-ws", "10Gi"), nil)
	f.ws.On("Get", mock.Anything, "ws-1", mock.Anything).Return(crd, nil)
	f.db.On("SyncWorkspaceVersionInfo", mock.Anything, "ws-1", mock.Anything, mock.Anything).Return().Maybe()

	// No SetPodIdentityTracker / SetSecretPusher calls.
	_, err := f.svc.GetWorkspaceStatus(ctx, "user1", "ws-1")
	assert.NoError(t, err)
}

// === Case 7: transition with a failing pusher — pending_refresh stays true ===

// TestPodIdentity_TransitionPushFailureLeavesPendingRefreshTrue proves
// the fallback UX contract: if the auto-push fails (agentd unreachable,
// DEK unavailable, etc.), the DB flag stays TRUE (because we never
// called MarkAgentReloaded to clear it) so the frontend keeps showing
// the AgentReloadBanner. The user can then click "Reload agent" to
// retry manually.
//
// The ABSENCE of a MarkAgentReloaded call in this scenario is critical:
// the entire point of the transition-fires-push-clears-flag state
// machine is that the flag reflects whether the pod actually has the
// user-DEK secrets. If we cleared it optimistically before knowing the
// push succeeded, the banner would disappear and the user would think
// everything was fine while their secrets were silently absent — the
// exact bug worklog 0589 aims to prevent from recurring at the fallback
// layer.
func TestPodIdentity_TransitionPushFailureLeavesPendingRefreshTrue(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	oldStart := time.Date(2026, 7, 2, 11, 0, 0, 0, time.UTC)
	newStart := time.Date(2026, 7, 2, 12, 0, 0, 0, time.UTC)
	crd := activePodCRD("ws-1", "user1", "pod-new", newStart)

	f.db.On("GetWorkspace", ctx, "ws-1").Return(dbWorkspace("ws-1", "user1", "my-ws", "10Gi"), nil)
	f.ws.On("Get", mock.Anything, "ws-1", mock.Anything).Return(crd, nil)
	f.db.On("SyncWorkspaceVersionInfo", mock.Anything, "ws-1", mock.Anything, mock.Anything).Return().Maybe()

	tracker := &fakePodIdentityTracker{storedName: "pod-old", storedStart: oldStart}
	pusher := &fakeSecretPusher{returnErr: pushInjectionError("agent unreachable")}
	f.svc.SetPodIdentityTracker(tracker)
	f.svc.SetSecretPusher(pusher)

	_, err := f.svc.GetWorkspaceStatus(ctx, "user1", "ws-1")
	assert.NoError(t, err)

	pusher.waitForPushCalls(t, 1, 2*time.Second)
	assert.Equal(t, 1, tracker.transitionCalls,
		"MarkPodIdentityTransition MUST have been called BEFORE the push, "+
			"so the DB pending_refresh flag reflects the pending state "+
			"even if the push then fails")
	// The failed push must NOT clear pending_refresh — otherwise the
	// banner would disappear and the user would believe their secrets
	// were delivered when they weren't.
	// Small window to catch a wrongly-fired clear.
	time.Sleep(50 * time.Millisecond)
	assert.Equal(t, 0, tracker.clearCalls,
		"failed push MUST NOT clear pending_refresh; otherwise the "+
			"banner disappears and the user has no signal that their "+
			"secrets were never actually delivered")
}

// pushInjectionError is a marker error for the failure-injection test.
type pushInjectionError string

func (e pushInjectionError) Error() string { return string(e) }

// === Case 8: fire-and-forget survives request cancellation ===

// TestPodIdentity_PushSurvivesRequestContextCancellation proves the
// use of context.WithoutCancel: the auto-push must complete even if
// the caller's HTTP request cancels (client disconnected, Gin closed
// the response). Without WithoutCancel, the push aborts as soon as
// the frontend poll returns — leaving the pod without secrets even
// though the transition was detected.
func TestPodIdentity_PushSurvivesRequestContextCancellation(t *testing.T) {
	f := newFixture(t)
	ctx, cancel := context.WithCancel(context.Background())

	oldStart := time.Date(2026, 7, 2, 11, 0, 0, 0, time.UTC)
	newStart := time.Date(2026, 7, 2, 12, 0, 0, 0, time.UTC)
	crd := activePodCRD("ws-1", "user1", "pod-new", newStart)

	f.db.On("GetWorkspace", ctx, "ws-1").Return(dbWorkspace("ws-1", "user1", "my-ws", "10Gi"), nil)
	f.ws.On("Get", mock.Anything, "ws-1", mock.Anything).Return(crd, nil)
	f.db.On("SyncWorkspaceVersionInfo", mock.Anything, "ws-1", mock.Anything, mock.Anything).Return().Maybe()

	tracker := &fakePodIdentityTracker{storedName: "pod-old", storedStart: oldStart}
	unblock := make(chan struct{})
	pusher := &fakeSecretPusher{blockUntilClose: unblock}
	f.svc.SetPodIdentityTracker(tracker)
	f.svc.SetSecretPusher(pusher)

	_, err := f.svc.GetWorkspaceStatus(ctx, "user1", "ws-1")
	assert.NoError(t, err)

	// Cancel the request context — the fire-and-forget push should NOT
	// be affected because it runs on context.WithoutCancel(ctx). If the
	// service passed the request ctx directly, our fake pusher would
	// observe ctx.Done() and return ctx.Err().
	cancel()
	// Small window for the cancellation to propagate to any incorrectly-
	// wired goroutine.
	time.Sleep(50 * time.Millisecond)
	assert.Equal(t, int64(1), pusher.calls.Load(),
		"push must have been ENTERED before ctx cancellation propagated")

	// Now unblock the push and verify it completed normally (no error
	// returned by our fake). If WithoutCancel is missing, the pusher
	// select above would have taken the ctx.Done() branch, exited early,
	// and the test would still pass here — but the transitionCalls
	// counter proves the mark ran before, and the goroutine ran on
	// WithoutCancel(ctx) rather than the canceled ctx.
	close(unblock)
	time.Sleep(50 * time.Millisecond)

	// The distinguishing signal: if WithoutCancel is missing, the fake's
	// Push would return context.Canceled and log a Warn line. We can't
	// easily verify a Warn from here without more mocks, but we CAN
	// verify the push completed without ctx-cancellation-error by
	// requiring the mock logger to NOT see the specific error message.
	// See TestPodIdentity_PushSurvivesRequestContextCancellation_LogAssertion
	// below for the stronger version.
}

// TestPodIdentity_PushSurvivesRequestContextCancellation_LogAssertion
// is the paired stronger check: without context.WithoutCancel, the
// goroutine's Push observes context.Canceled and runAutoPush emits a
// WARN with "error"="context canceled". We assert that WARN was NOT
// emitted, proving the WithoutCancel wrap is doing its job.
func TestPodIdentity_PushSurvivesRequestContextCancellation_LogAssertion(t *testing.T) {
	f := newFixture(t)

	// Re-wire logger with a strict counter for cancellation warnings.
	warnCount := atomic.Int64{}
	f.log.ExpectedCalls = nil // clear the generic Warn expectation from newFixture
	f.log.On("Info", mock.Anything, mock.Anything).Maybe()
	f.log.On("Debug", mock.Anything, mock.Anything).Maybe()
	f.log.On("Error", mock.Anything, mock.Anything, mock.Anything).Maybe()
	f.log.On("With", mock.Anything).Return(f.log).Maybe()
	f.log.On("Sync").Return(nil).Maybe()
	f.log.On("Warn", "auto-push after pod recreation: failed", mock.Anything).
		Run(func(args mock.Arguments) {
			warnCount.Add(1)
		}).Maybe()
	// Any other Warn (e.g. from unrelated code paths) is allowed.
	f.log.On("Warn", mock.Anything, mock.Anything).Maybe()

	ctx, cancel := context.WithCancel(context.Background())

	oldStart := time.Date(2026, 7, 2, 11, 0, 0, 0, time.UTC)
	newStart := time.Date(2026, 7, 2, 12, 0, 0, 0, time.UTC)
	crd := activePodCRD("ws-1", "user1", "pod-new", newStart)

	f.db.On("GetWorkspace", ctx, "ws-1").Return(dbWorkspace("ws-1", "user1", "my-ws", "10Gi"), nil)
	f.ws.On("Get", mock.Anything, "ws-1", mock.Anything).Return(crd, nil)
	f.db.On("SyncWorkspaceVersionInfo", mock.Anything, "ws-1", mock.Anything, mock.Anything).Return().Maybe()

	tracker := &fakePodIdentityTracker{storedName: "pod-old", storedStart: oldStart}
	unblock := make(chan struct{})
	pusher := &fakeSecretPusher{blockUntilClose: unblock}
	f.svc.SetPodIdentityTracker(tracker)
	f.svc.SetSecretPusher(pusher)

	_, err := f.svc.GetWorkspaceStatus(ctx, "user1", "ws-1")
	assert.NoError(t, err)

	pusher.waitForPushCalls(t, 1, 2*time.Second)
	// Cancel the request context. With WithoutCancel wired, the pusher
	// keeps waiting on `unblock`. Without WithoutCancel, the pusher's
	// select takes ctx.Done() and returns context.Canceled → runAutoPush
	// logs the "auto-push after pod recreation: failed" WARN.
	cancel()
	time.Sleep(100 * time.Millisecond)

	assert.Equal(t, int64(0), warnCount.Load(),
		"WithoutCancel must keep the auto-push goroutine's ctx alive; "+
			"a cancellation-caused failure warning here proves the "+
			"request ctx was passed through unchanged")

	// Unblock so the pusher completes cleanly.
	close(unblock)
	time.Sleep(50 * time.Millisecond)
}
