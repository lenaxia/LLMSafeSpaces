package handlers

import (
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/lenaxia/llmsafespaces/api/internal/services/eventbroker"
	k8smocks "github.com/lenaxia/llmsafespaces/mocks/kubernetes"
	v1 "github.com/lenaxia/llmsafespaces/pkg/apis/llmsafespaces/v1"
	"github.com/lenaxia/llmsafespaces/pkg/interfaces"
	k8sfake "k8s.io/client-go/kubernetes/fake"
)

type ctxCapturingLogger struct {
	mu    sync.Mutex
	warns []ctxLogEntry
}

type ctxLogEntry struct {
	msg string
	kv  []interface{}
}

func (l *ctxCapturingLogger) Debug(msg string, kv ...interface{}) {}
func (l *ctxCapturingLogger) Info(msg string, kv ...interface{})  {}
func (l *ctxCapturingLogger) Warn(msg string, kv ...interface{}) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.warns = append(l.warns, ctxLogEntry{msg, kv})
}
func (l *ctxCapturingLogger) Error(msg string, err error, kv ...interface{})    {}
func (l *ctxCapturingLogger) Fatal(msg string, err error, kv ...interface{})    {}
func (l *ctxCapturingLogger) With(kv ...interface{}) interfaces.LoggerInterface { return l }
func (l *ctxCapturingLogger) Sync() error                                       { return nil }

func (l *ctxCapturingLogger) getWarns() []ctxLogEntry {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]ctxLogEntry{}, l.warns...)
}

func newCtxTestEnv(t *testing.T, log *ctxCapturingLogger) *testEnv {
	t.Helper()
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(func() { backend.Close() })

	transport := &redirectTransport{server: backend}
	httpClient := &http.Client{Transport: transport, Timeout: 5 * time.Second}

	k8sMock := k8smocks.NewMockKubernetesClient()
	llmMock := k8smocks.NewMockLLMSafespacesV1Interface()
	wsMock := k8smocks.NewMockWorkspaceInterface()

	k8sMock.On("LlmsafespacesV1").Return(llmMock, nil)
	llmMock.On("Workspaces", "default").Return(wsMock)

	fakeClientset := k8sfake.NewSimpleClientset()
	k8sMock.On("Clientset").Return(fakeClientset)

	handler, err := NewProxyHandler(k8sMock, log, "default", httpClient, nil)
	if err != nil {
		t.Fatalf("NewProxyHandler: %v", err)
	}

	return &testEnv{
		handler:   handler,
		k8sMock:   k8sMock,
		llmMock:   llmMock,
		wsMock:    wsMock,
		clientset: fakeClientset,
		backend:   backend,
		log:       nil,
	}
}

func TestPersistContextFromEvent_MissingTokens_LogsWarn(t *testing.T) {
	si := newContextUsedSessionIndex()
	log := &ctxCapturingLogger{}
	env := newCtxTestEnv(t, log)
	env.handler.SetSessionIndex(si)
	env.handler.userBroker = eventbroker.NewUserEventBroker()
	env.setupWorkspacePodWithT(t, "ws-1", "10.0.0.1", string(v1.WorkspacePhaseActive), "ws-1")
	env.setupPasswordWithT(t, "ws-1", "test-password")
	env.setupWorkspaceWithT(t, "ws-1", 5)

	env.handler.onRawEvent("ws-1", "session.next.step.ended", `{
		"type": "session.next.step.ended",
		"properties": {
			"sessionID": "ses_abc"
		}
	}`)

	_, ok := si.get("ws-1", "ses_abc")
	if ok {
		t.Fatal("UpsertContextUsed must NOT be called when tokens are missing")
	}

	warns := log.getWarns()
	found := false
	for _, w := range warns {
		if w.msg == "persistContextFromEvent: step.ended event missing tokens — opencode wire shape may have changed" {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("expected warn about missing tokens, got: %+v", warns)
	}
}

func TestPersistContextFromEvent_UnparseableEvent_LogsWarn(t *testing.T) {
	si := newContextUsedSessionIndex()
	log := &ctxCapturingLogger{}
	env := newCtxTestEnv(t, log)
	env.handler.SetSessionIndex(si)

	env.handler.persistContextFromEvent("ws-1", "not valid json {{{")

	warns := log.getWarns()
	if len(warns) == 0 {
		t.Fatal("expected a warn log for unparseable event, got none")
	}
}

func TestPersistContextFromEvent_EmptySessionID_LogsWarn(t *testing.T) {
	si := newContextUsedSessionIndex()
	log := &ctxCapturingLogger{}
	env := newCtxTestEnv(t, log)
	env.handler.SetSessionIndex(si)
	env.handler.userBroker = eventbroker.NewUserEventBroker()
	env.setupWorkspacePodWithT(t, "ws-1", "10.0.0.1", string(v1.WorkspacePhaseActive), "ws-1")
	env.setupPasswordWithT(t, "ws-1", "test-password")
	env.setupWorkspaceWithT(t, "ws-1", 5)

	env.handler.onRawEvent("ws-1", "session.next.step.ended", `{
		"type": "session.next.step.ended",
		"properties": {
			"sessionID": "",
			"tokens": {"input": 100, "cache": {"read": 0, "write": 0}}
		}
	}`)

	warns := log.getWarns()
	if len(warns) == 0 {
		t.Fatal("expected a warn log for empty sessionID, got none")
	}
}
