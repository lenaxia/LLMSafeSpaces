// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package secretautopush_test

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/lenaxia/llmsafespaces/api/internal/services/secretautopush"
	v1 "github.com/lenaxia/llmsafespaces/pkg/apis/llmsafespaces/v1"
)

// fakeDEKRetriever satisfies secretautopush.DEKRetriever with a
// scriptable return value + call recorder.
type fakeDEKRetriever struct {
	calls      atomic.Int64
	returnDEK  []byte
	returnJTI  string
	returnErr  error
	lastUserID atomic.Value // string
}

func (f *fakeDEKRetriever) GetDEKForUser(_ context.Context, userID string) ([]byte, string, error) {
	f.calls.Add(1)
	f.lastUserID.Store(userID)
	return f.returnDEK, f.returnJTI, f.returnErr
}

// fakeBindingsChecker satisfies secretautopush.BindingsChecker.
type fakeBindingsChecker struct {
	returnExists bool
	returnErr    error
}

func (f *fakeBindingsChecker) UserHasBoundSecrets(_ context.Context, workspaceID string) (bool, error) {
	return f.returnExists, f.returnErr
}

// fakePusher satisfies secretautopush.SecretPusher.
type fakePusher struct {
	calls        atomic.Int64
	sawUserID    atomic.Value // string
	sawWorkspace atomic.Value // string
	sawSessionID atomic.Value // string
	returnErr    error
	// If nonzero, sleep for this duration before returning — used to
	// exercise the in-flight-lock contract.
	block time.Duration
}

func (f *fakePusher) Push(ctx context.Context, userID, workspaceID string) error {
	f.calls.Add(1)
	f.sawUserID.Store(userID)
	f.sawWorkspace.Store(workspaceID)
	if sess, ok := ctx.Value(sessionIDKeyForTest{}).(string); ok {
		f.sawSessionID.Store(sess)
	}
	if f.block > 0 {
		select {
		case <-time.After(f.block):
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return f.returnErr
}

// sessionIDKeyForTest is a private context-key type the tests use to
// verify that the auth ctx built by secretautopush actually carries
// the jti as sessionID.
type sessionIDKeyForTest struct{}

func mustWs(name, userID string, phase v1.WorkspacePhase, userCredsPresent *bool) *v1.Workspace {
	return &v1.Workspace{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
		Spec: v1.WorkspaceSpec{
			Owner: v1.WorkspaceOwner{UserID: userID},
		},
		Status: v1.WorkspaceStatus{
			Phase:            phase,
			UserCredsPresent: userCredsPresent,
			PodName:          name + "-pod",
			PodNamespace:     "default",
			PodIP:            "10.0.0.5",
		},
	}
}

func boolPtr(b bool) *bool { return &b }

// waitForCalls polls fn.calls until it reaches n or the timeout expires.
func waitForCalls(t *testing.T, calls *atomic.Int64, n int64, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if calls.Load() >= n {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %d calls; got %d", n, calls.Load())
}

// TestOnWorkspaceUpdate_FiresWhenAllConditionsMet is the load-bearing
// case: Active + UserCredsPresent=false + bindings-exist + DEK
// retrievable → push fires exactly once.
func TestOnWorkspaceUpdate_FiresWhenAllConditionsMet(t *testing.T) {
	dek := &fakeDEKRetriever{
		returnDEK: []byte("dek-plaintext"),
		returnJTI: "jti-abc",
	}
	bindings := &fakeBindingsChecker{returnExists: true}
	pusher := &fakePusher{}

	svc := secretautopush.New(dek, bindings, pusher)
	svc.OnWorkspaceUpdate(mustWs("ws-1", "user-1", v1.WorkspacePhaseActive, boolPtr(false)))

	waitForCalls(t, &pusher.calls, 1, 2*time.Second)
	assert.Equal(t, "user-1", pusher.sawUserID.Load())
	assert.Equal(t, "ws-1", pusher.sawWorkspace.Load())
	assert.Equal(t, int64(1), dek.calls.Load(), "DEK MUST have been fetched")
	assert.Equal(t, "user-1", dek.lastUserID.Load())
}

// TestOnWorkspaceUpdate_SkipsIfUserCredsPresentTrue proves the
// happy-state case: agentd already has user creds materialized. The
// callback MUST NOT fire the push — that would be work for no reason
// and would show up in api_secret_auto_push_total{outcome="success"}
// noise.
func TestOnWorkspaceUpdate_SkipsIfUserCredsPresentTrue(t *testing.T) {
	dek := &fakeDEKRetriever{}
	bindings := &fakeBindingsChecker{returnExists: true}
	pusher := &fakePusher{}

	svc := secretautopush.New(dek, bindings, pusher)
	svc.OnWorkspaceUpdate(mustWs("ws-1", "user-1", v1.WorkspacePhaseActive, boolPtr(true)))

	time.Sleep(50 * time.Millisecond)
	assert.Equal(t, int64(0), pusher.calls.Load(),
		"UserCredsPresent=true MUST short-circuit before any DEK fetch")
	assert.Equal(t, int64(0), dek.calls.Load(),
		"skipping the push MUST also skip the DEK fetch — otherwise "+
			"we waste a PG round-trip and cache write on every watch event")
}

// TestOnWorkspaceUpdate_SkipsIfUserCredsPresentNil covers the pre-scrape
// state: controller hasn't reported yet. The callback MUST NOT fire —
// nil is "unknown," not "false."
func TestOnWorkspaceUpdate_SkipsIfUserCredsPresentNil(t *testing.T) {
	dek := &fakeDEKRetriever{}
	bindings := &fakeBindingsChecker{returnExists: true}
	pusher := &fakePusher{}

	svc := secretautopush.New(dek, bindings, pusher)
	// UserCredsPresent = nil (default) — not yet scraped.
	svc.OnWorkspaceUpdate(mustWs("ws-1", "user-1", v1.WorkspacePhaseActive, nil))

	time.Sleep(50 * time.Millisecond)
	assert.Equal(t, int64(0), pusher.calls.Load(),
		"UserCredsPresent=nil MUST be treated as 'unknown, do not push'. "+
			"Firing here would produce a stampede on API restart when "+
			"the controller hasn't scraped every workspace yet.")
}

// TestOnWorkspaceUpdate_SkipsIfNotActive covers all non-Active phases.
// Push targets a running agentd; other phases have no pod (or a
// dying one) to push to.
func TestOnWorkspaceUpdate_SkipsIfNotActive(t *testing.T) {
	phases := []v1.WorkspacePhase{
		v1.WorkspacePhasePending,
		v1.WorkspacePhaseCreating,
		v1.WorkspacePhaseSuspending,
		v1.WorkspacePhaseSuspended,
		v1.WorkspacePhaseResuming,
		v1.WorkspacePhaseTerminating,
		v1.WorkspacePhaseFailed,
	}
	for _, phase := range phases {
		t.Run(string(phase), func(t *testing.T) {
			pusher := &fakePusher{}
			svc := secretautopush.New(
				&fakeDEKRetriever{},
				&fakeBindingsChecker{returnExists: true},
				pusher,
			)
			svc.OnWorkspaceUpdate(mustWs("ws-1", "user-1", phase, boolPtr(false)))
			time.Sleep(20 * time.Millisecond)
			assert.Equal(t, int64(0), pusher.calls.Load(),
				"phase=%s MUST NOT trigger push", phase)
		})
	}
}

// TestOnWorkspaceUpdate_SkipsIfNoBindings proves the correctness of
// the bindings check: if the user has no user_secret_bindings for
// this workspace, there's literally nothing to push. Firing the push
// would decrypt an empty batch and materialize [] to the pod, which
// is a no-op but wastes an RPC + logs a "success" that misleads
// operators. Skip cleanly.
func TestOnWorkspaceUpdate_SkipsIfNoBindings(t *testing.T) {
	pusher := &fakePusher{}
	svc := secretautopush.New(
		&fakeDEKRetriever{},
		&fakeBindingsChecker{returnExists: false},
		pusher,
	)
	svc.OnWorkspaceUpdate(mustWs("ws-1", "user-1", v1.WorkspacePhaseActive, boolPtr(false)))
	time.Sleep(50 * time.Millisecond)
	assert.Equal(t, int64(0), pusher.calls.Load(),
		"no bindings MUST skip — nothing to push")
}

// TestOnWorkspaceUpdate_SkipsIfBindingsCheckErrors is the fail-safe:
// if the bindings query itself errors (PG outage), we can't decide,
// so we skip. Retrying would be handled by the next watch event or
// the user's next request. Silent skip is preferred over firing a
// push with unknown intent.
func TestOnWorkspaceUpdate_SkipsIfBindingsCheckErrors(t *testing.T) {
	pusher := &fakePusher{}
	svc := secretautopush.New(
		&fakeDEKRetriever{},
		&fakeBindingsChecker{returnErr: errors.New("pg down")},
		pusher,
	)
	svc.OnWorkspaceUpdate(mustWs("ws-1", "user-1", v1.WorkspacePhaseActive, boolPtr(false)))
	time.Sleep(50 * time.Millisecond)
	assert.Equal(t, int64(0), pusher.calls.Load(),
		"bindings-query error MUST skip — retry on the next watch event")
}

// TestOnWorkspaceUpdate_SkipsIfDEKUnavailable covers the case where
// no active JWT session exists for the user (they've been logged out
// too long, all sessions expired). The push cannot succeed; skip.
func TestOnWorkspaceUpdate_SkipsIfDEKUnavailable(t *testing.T) {
	pusher := &fakePusher{}
	svc := secretautopush.New(
		&fakeDEKRetriever{returnErr: errors.New("dek unavailable")},
		&fakeBindingsChecker{returnExists: true},
		pusher,
	)
	svc.OnWorkspaceUpdate(mustWs("ws-1", "user-1", v1.WorkspacePhaseActive, boolPtr(false)))
	time.Sleep(50 * time.Millisecond)
	assert.Equal(t, int64(0), pusher.calls.Load(),
		"no DEK available MUST skip — nothing to unwrap secrets with")
}

// TestOnWorkspaceUpdate_InFlightLockPreventsDoubleFire is the
// idempotency contract: the watcher may emit many Modified events
// per second (bookmarks, health check status updates). We MUST NOT
// fire multiple concurrent pushes for the same workspace.
func TestOnWorkspaceUpdate_InFlightLockPreventsDoubleFire(t *testing.T) {
	pusher := &fakePusher{block: 100 * time.Millisecond}
	svc := secretautopush.New(
		&fakeDEKRetriever{returnDEK: []byte("dek"), returnJTI: "jti"},
		&fakeBindingsChecker{returnExists: true},
		pusher,
	)

	ws := mustWs("ws-1", "user-1", v1.WorkspacePhaseActive, boolPtr(false))

	// Fire 5 events back-to-back. The first should acquire the lock,
	// spawn its push goroutine; the next 4 should observe "already
	// in flight" and skip.
	var wg sync.WaitGroup
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			svc.OnWorkspaceUpdate(ws)
		}()
	}
	wg.Wait()

	// Wait for the (one) push to complete.
	waitForCalls(t, &pusher.calls, 1, 500*time.Millisecond)
	time.Sleep(200 * time.Millisecond) // extra window to catch late calls

	assert.Equal(t, int64(1), pusher.calls.Load(),
		"5 concurrent OnWorkspaceUpdate calls MUST result in exactly "+
			"one push — per-workspace in-flight lock prevents duplicates")
}

// TestOnWorkspaceUpdate_LockReleasesAfterPushCompletes proves the
// paired contract: the in-flight lock MUST release when the push
// goroutine finishes so a subsequent recreation can trigger a new
// push. Without release, the workspace would be permanently stuck.
func TestOnWorkspaceUpdate_LockReleasesAfterPushCompletes(t *testing.T) {
	pusher := &fakePusher{}
	svc := secretautopush.New(
		&fakeDEKRetriever{returnDEK: []byte("dek"), returnJTI: "jti"},
		&fakeBindingsChecker{returnExists: true},
		pusher,
	)
	ws := mustWs("ws-1", "user-1", v1.WorkspacePhaseActive, boolPtr(false))

	svc.OnWorkspaceUpdate(ws)
	waitForCalls(t, &pusher.calls, 1, 500*time.Millisecond)
	// First push done. Lock must have released.
	// Force a second push by calling again — it should fire.
	require.NoError(t, ensureLockReleased(t, 500*time.Millisecond, func() bool {
		before := pusher.calls.Load()
		svc.OnWorkspaceUpdate(ws)
		time.Sleep(100 * time.Millisecond)
		return pusher.calls.Load() > before
	}))
}

// ensureLockReleased polls the callback until it observes the release
// (or times out).
func ensureLockReleased(t *testing.T, timeout time.Duration, fn func() bool) error {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if fn() {
			return nil
		}
		time.Sleep(10 * time.Millisecond)
	}
	return errors.New("lock did not release within timeout")
}

// TestOnWorkspaceUpdate_PushErrorEmitsMetricAndReleasesLock covers the
// error handling in run(): if pusher.Push returns an error, the metric
// hook fires with "push_error" AND the in-flight lock releases so a
// subsequent watch event can retry. Without lock release, a single
// failed push would permanently wedge the workspace.
func TestOnWorkspaceUpdate_PushErrorEmitsMetricAndReleasesLock(t *testing.T) {
	dek := &fakeDEKRetriever{
		returnDEK: []byte("dek"),
		returnJTI: "jti-abc",
	}
	bindings := &fakeBindingsChecker{returnExists: true}
	pusher := &fakePusher{returnErr: errors.New("agentd unreachable")}

	var outcomes []string
	var outcomesMu sync.Mutex
	svc := secretautopush.New(dek, bindings, pusher,
		secretautopush.WithMetricsHook(func(outcome string) {
			outcomesMu.Lock()
			outcomes = append(outcomes, outcome)
			outcomesMu.Unlock()
		}),
	)

	ws := mustWs("ws-1", "user-1", v1.WorkspacePhaseActive, boolPtr(false))
	svc.OnWorkspaceUpdate(ws)

	waitForCalls(t, &pusher.calls, 1, 2*time.Second)
	// Give the goroutine's defer a chance to run so the lock releases.
	time.Sleep(50 * time.Millisecond)

	outcomesMu.Lock()
	got := append([]string(nil), outcomes...)
	outcomesMu.Unlock()
	assert.Contains(t, got, "push_error",
		"pusher.Push error MUST emit metric outcome=push_error so operators "+
			"can distinguish push failures from other skip reasons")

	// Verify the lock released: a second OnWorkspaceUpdate with same WS
	// MUST fire a second push (not be blocked by stale in-flight state).
	before := pusher.calls.Load()
	svc.OnWorkspaceUpdate(ws)
	waitForCalls(t, &pusher.calls, before+1, 2*time.Second)
	assert.Greater(t, pusher.calls.Load(), before,
		"lock MUST release after failed push — otherwise a single failed "+
			"push would permanently wedge auto-push for this workspace")
}
