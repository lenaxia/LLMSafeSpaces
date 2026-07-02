// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package workspace_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	imocks "github.com/lenaxia/llmsafespaces/api/internal/mocks"
	"github.com/lenaxia/llmsafespaces/api/internal/services/agentpush"
	"github.com/lenaxia/llmsafespaces/api/internal/services/workspace"
	v1 "github.com/lenaxia/llmsafespaces/pkg/apis/llmsafespaces/v1"
	pkginterfaces "github.com/lenaxia/llmsafespaces/pkg/interfaces"
	"github.com/lenaxia/llmsafespaces/pkg/types"

	kmocks "github.com/lenaxia/llmsafespaces/mocks/kubernetes"
	lmocks "github.com/lenaxia/llmsafespaces/mocks/logger"
)

// TestIntegration_PodRecreationTriggersFullAutoPush is the regression
// test for #493 through the full wiring: workspace.Service detects the
// pod-identity transition, fires the real agentpush.Service, which
// runs the real InjectSecrets shim + HTTP POST to a mock agentd, then
// clears pending_refresh on success. This is the assertion the design
// worklog's "Definition of done" section actually cares about — the
// unit tests prove pieces work in isolation; this one proves the pieces
// wire together correctly.
//
// Fake DB, real workspace.Service, real agentpush.Service, mock agentd
// via httptest.
func TestIntegration_PodRecreationTriggersFullAutoPush(t *testing.T) {
	// --- Mock agentd. Records every POST /v1/reload-secrets so we can
	// assert the user-DEK secrets actually reached the pod.
	var agentdReceived atomic.Int64
	var lastBody atomic.Value // []byte
	agentd := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/v1/reload-secrets", r.URL.Path)
		body, _ := io.ReadAll(r.Body)
		lastBody.Store(body)
		agentdReceived.Add(1)
		_, _ = w.Write([]byte(`{"reloaded":2,"restarted":false}`))
	}))
	defer agentd.Close()

	// --- Fake injector that returns a fixed user-DEK payload. This is
	// the SecretService in production; we use a fake here to focus the
	// assertion on the wiring, not on the secrets package internals
	// (those are covered by pkg/secrets tests).
	injector := &fakeInjector{
		payload: []byte(`[{"type":"env-secret","name":"OPENAI_KEY","value":"sk-..."}]`),
	}

	// --- Real agentpush.Service. Uses a resolver that always returns
	// the mock agentd's host, and an HTTP client that rewrites every
	// request to hit the mock (since agentpush hardcodes port 4097).
	pusher := agentpush.New(
		injector,
		agentpush.WithPodIPResolver(&staticResolver{ip: "10.0.0.5"}),
		agentpush.WithHTTPClient(&http.Client{Transport: &rewritingTransport{target: agentd.URL}}),
	)

	// --- workspace.Service backed by mocked K8s + DB, but with the
	// real agentpush.Service as its pusher.
	svc, dbMock, wsMock, trackerRecorder := newIntegrationSvc(t)
	svc.SetSecretPusher(&pushAdapter{pusher: pusher})
	svc.SetPodIdentityTracker(trackerRecorder)

	oldStart := time.Date(2026, 7, 2, 11, 0, 0, 0, time.UTC)
	newStart := time.Date(2026, 7, 2, 12, 0, 0, 0, time.UTC)

	// Prior observation: an OLD pod was seen. Now the CRD reports a new pod.
	trackerRecorder.setStored("pod-old", oldStart)

	crd := &v1.Workspace{
		ObjectMeta: metav1.ObjectMeta{Name: "ws-1", Namespace: "default"},
		Spec: v1.WorkspaceSpec{
			Owner:   v1.WorkspaceOwner{UserID: "user-1"},
			Storage: v1.WorkspaceStorageConfig{Size: "10Gi"},
		},
		Status: v1.WorkspaceStatus{
			Phase:        v1.WorkspacePhaseActive,
			PodName:      "pod-new",
			PodNamespace: "default",
			PodIP:        "10.0.0.5",
			StartTime:    &metav1.Time{Time: newStart},
			ImageTag:     "ts-testing",
		},
	}

	dbMock.On("GetWorkspace", mock.Anything, "ws-1").
		Return(&types.WorkspaceMetadata{ID: "ws-1", UserID: "user-1", Name: "my-ws", StorageSize: "10Gi", CreatedAt: time.Now(), UpdatedAt: time.Now()}, nil)
	wsMock.On("Get", mock.Anything, "ws-1", mock.Anything).Return(crd, nil)
	dbMock.On("SyncWorkspaceVersionInfo", mock.Anything, "ws-1", mock.Anything, mock.Anything).Return().Maybe()

	// Set up auth context — the SAME wiring the router applies for
	// GET /status. Without WithAuth, the fake injector wouldn't see
	// sessionID/matchedSigningKey and this test would silently pass
	// even for a broken auth handoff.
	ctx := agentpush.WithAuth(context.Background(), "sess-42", []byte("signing-key"))

	// Trigger: one status read that observes the pod-identity transition.
	_, err := svc.GetWorkspaceStatus(ctx, "user-1", "ws-1")
	require.NoError(t, err)

	// Wait for the fire-and-forget goroutine to complete.
	waitFor(t, 3*time.Second, func() bool {
		return agentdReceived.Load() >= 1 && trackerRecorder.clearCount() >= 1
	})

	// The end-to-end assertions that prove #493 is closed:
	assert.Equal(t, int64(1), agentdReceived.Load(),
		"mock agentd MUST have received exactly one POST /v1/reload-secrets")
	body := lastBody.Load().([]byte)
	assert.Contains(t, string(body), "OPENAI_KEY",
		"the user-DEK payload MUST have reached agentd — this is the "+
			"exact material that was going missing on every pod recreation "+
			"before the fix (#493)")

	// InjectSecrets was called with the sessionID + matchedSigningKey
	// the router put in ctx via WithAuth.
	assert.Equal(t, "sess-42", injector.sawSessionID)
	assert.Equal(t, []byte("signing-key"), injector.sawSigningKey)

	// pending_refresh was flipped TRUE then cleared — the banner appears
	// briefly then goes away, which is the intended UX.
	assert.Equal(t, 1, trackerRecorder.transitionCount(),
		"transition must have been recorded so agentNeedsRefresh briefly appears")
	assert.Equal(t, 1, trackerRecorder.clearCount(),
		"pending_refresh MUST have been cleared after the successful push — "+
			"otherwise the banner stays visible forever, which was the "+
			"bug the review pass caught before this test was added")
}

// TestIntegration_PodRecreationFailureLeavesPendingRefresh proves the
// paired failure path: when agentd returns 5xx (or is unreachable), the
// transition write happens (pending_refresh=TRUE), but Clear does NOT.
// The AgentReloadBanner correctly stays visible as the manual escape.
func TestIntegration_PodRecreationFailureLeavesPendingRefresh(t *testing.T) {
	// Mock agentd that returns 5xx on every request.
	agentd := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":"agentd is upset"}`))
	}))
	defer agentd.Close()

	pusher := agentpush.New(
		&fakeInjector{payload: []byte(`[]`)},
		agentpush.WithPodIPResolver(&staticResolver{ip: "10.0.0.5"}),
		agentpush.WithHTTPClient(&http.Client{Transport: &rewritingTransport{target: agentd.URL}}),
	)

	svc, dbMock, wsMock, trackerRecorder := newIntegrationSvc(t)
	svc.SetSecretPusher(&pushAdapter{pusher: pusher})
	svc.SetPodIdentityTracker(trackerRecorder)

	trackerRecorder.setStored("pod-old", time.Date(2026, 7, 2, 11, 0, 0, 0, time.UTC))
	crd := &v1.Workspace{
		ObjectMeta: metav1.ObjectMeta{Name: "ws-1", Namespace: "default"},
		Spec:       v1.WorkspaceSpec{Owner: v1.WorkspaceOwner{UserID: "user-1"}, Storage: v1.WorkspaceStorageConfig{Size: "10Gi"}},
		Status: v1.WorkspaceStatus{
			Phase: v1.WorkspacePhaseActive, PodName: "pod-new", PodNamespace: "default",
			PodIP:     "10.0.0.5",
			StartTime: &metav1.Time{Time: time.Date(2026, 7, 2, 12, 0, 0, 0, time.UTC)},
			ImageTag:  "ts-testing",
		},
	}
	dbMock.On("GetWorkspace", mock.Anything, "ws-1").
		Return(&types.WorkspaceMetadata{ID: "ws-1", UserID: "user-1", Name: "my-ws", StorageSize: "10Gi", CreatedAt: time.Now(), UpdatedAt: time.Now()}, nil)
	wsMock.On("Get", mock.Anything, "ws-1", mock.Anything).Return(crd, nil)
	dbMock.On("SyncWorkspaceVersionInfo", mock.Anything, "ws-1", mock.Anything, mock.Anything).Return().Maybe()

	ctx := agentpush.WithAuth(context.Background(), "sess-42", []byte("signing-key"))
	_, err := svc.GetWorkspaceStatus(ctx, "user-1", "ws-1")
	require.NoError(t, err)

	// Give the goroutine a chance to run — Push will fail on 5xx.
	waitFor(t, 3*time.Second, func() bool {
		return trackerRecorder.transitionCount() >= 1
	})
	// Small extra window to catch a wrongly-fired clear.
	time.Sleep(200 * time.Millisecond)

	assert.Equal(t, 1, trackerRecorder.transitionCount(),
		"transition must be recorded even though the push fails")
	assert.Equal(t, 0, trackerRecorder.clearCount(),
		"failed push MUST NOT clear pending_refresh — the banner stays "+
			"visible as the manual escape hatch")
}

// --- shared helpers below ---

// newIntegrationSvc builds a real workspace.Service backed by mocked
// DB + K8s. Returns the service + the two mocks + the tracker helper.
func newIntegrationSvc(t *testing.T) (*workspace.Service, *imocks.MockDatabaseService, *kmocks.MockWorkspaceInterface, *recordingTracker) {
	t.Helper()

	log := lmocks.NewMockLogger()
	log.On("Info", mock.Anything, mock.Anything).Maybe()
	log.On("Debug", mock.Anything, mock.Anything).Maybe()
	log.On("Warn", mock.Anything, mock.Anything).Maybe()
	log.On("Error", mock.Anything, mock.Anything, mock.Anything).Maybe()
	log.On("With", mock.Anything).Return(log).Maybe()
	log.On("Sync").Return(nil).Maybe()

	k8s := kmocks.NewMockKubernetesClient()
	v1i := kmocks.NewMockLLMSafespacesV1Interface()
	ws := kmocks.NewMockWorkspaceInterface()
	db := &imocks.MockDatabaseService{}
	met := &imocks.MockMetricsService{}
	met.On("RecordRequest", mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything).Maybe()

	k8s.On("LlmsafespacesV1").Return(v1i, nil)
	v1i.On("Workspaces", "default").Return(ws)

	svc, err := workspace.New(pkginterfaces.LoggerInterface(log), k8s, db, nil, met, &workspace.Config{Namespace: "default"})
	require.NoError(t, err)

	return svc, db, ws, newRecordingTracker()
}

// recordingTracker satisfies workspace.PodIdentityTracker and lets the
// test read what the workspace service wrote.
type recordingTracker struct {
	mu               sync.Mutex
	storedName       string
	storedStart      time.Time
	transitionCalls  int
	upsertCalls      int
	clearCalls       int
	transitionReturn time.Time
}

func newRecordingTracker() *recordingTracker {
	return &recordingTracker{transitionReturn: time.Now()}
}

func (r *recordingTracker) setStored(name string, startTime time.Time) {
	r.mu.Lock()
	r.storedName, r.storedStart = name, startTime
	r.mu.Unlock()
}

func (r *recordingTracker) transitionCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.transitionCalls
}

func (r *recordingTracker) clearCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.clearCalls
}

func (r *recordingTracker) GetLastSeenPodIdentity(_ context.Context, _ string) (string, time.Time, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.storedName, r.storedStart, nil
}

func (r *recordingTracker) UpsertLastSeenPodIdentity(_ context.Context, _, name string, startTime time.Time) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.storedName, r.storedStart = name, startTime
	r.upsertCalls++
	return nil
}

func (r *recordingTracker) MarkPodIdentityTransition(_ context.Context, _, name string, startTime time.Time) (time.Time, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.storedName, r.storedStart = name, startTime
	r.transitionCalls++
	return r.transitionReturn, nil
}

func (r *recordingTracker) ClearPendingRefreshAfterAutoPush(_ context.Context, _ string, _ time.Time) (time.Time, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.clearCalls++
	return time.Now(), nil
}

// fakeInjector records what it was passed so we can assert the auth
// context traveled from router → workspace service → pusher correctly.
type fakeInjector struct {
	payload       []byte
	sawSessionID  string
	sawSigningKey []byte
	mu            sync.Mutex
}

func (f *fakeInjector) InjectSecrets(_ context.Context, _, sessionID string, matchedSigningKey []byte, _ string) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sawSessionID = sessionID
	f.sawSigningKey = matchedSigningKey
	return f.payload, nil
}

type staticResolver struct{ ip string }

func (s *staticResolver) GetWorkspacePodIP(_ context.Context, _, _ string) (string, error) {
	return s.ip, nil
}

// pushAdapter wraps *agentpush.Service to satisfy workspace.SecretPusher
// (drops the Result).
type pushAdapter struct{ pusher *agentpush.Service }

func (p *pushAdapter) Push(ctx context.Context, userID, workspaceID string) error {
	_, err := p.pusher.Push(ctx, userID, workspaceID)
	return err
}

// rewritingTransport lets the test hit httptest.Server despite agentpush
// hardcoding :4097 on the URL.
type rewritingTransport struct{ target string }

func (t *rewritingTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	host := strings.TrimPrefix(t.target, "http://")
	host = strings.TrimPrefix(host, "https://")
	newURL := *r.URL
	newURL.Scheme = "http"
	newURL.Host = host
	r2 := r.Clone(r.Context())
	r2.URL = &newURL
	r2.Host = host
	return http.DefaultTransport.RoundTrip(r2)
}

func waitFor(t *testing.T, timeout time.Duration, pred func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if pred() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("waitFor: predicate never became true within %s", timeout)
}

// Silence "imported and not used" if the compiler ever changes.
var _ = json.Marshal
