package app

import (
	"testing"

	k8sfake "k8s.io/client-go/kubernetes/fake"

	"github.com/lenaxia/llmsafespaces/api/internal/handlers"
	lmocks "github.com/lenaxia/llmsafespaces/mocks/logger"
)

// TestPodAutomationHandler_LoggerWired mirrors the #407
// PodBootstrapHandler guard: app.go constructs PodAutomationHandler via
// NewPodAutomationHandlerFromClientset and must call SetLogger in the
// same breath, or every 5xx on the automation surface degrades to a
// generic error with no underlying cause (the observability gap that
// turned the 2026-06-24 outage into a 30-minute diagnosis). The exact
// app.New sequence needs PostgreSQL/Redis; the construction + wiring
// pair is the unit under guard.
func TestPodAutomationHandler_LoggerWired(t *testing.T) {
	fakeClientset := k8sfake.NewSimpleClientset()
	dbSvc := &fakeAppDBLookup{}

	h := handlers.NewPodAutomationHandlerFromClientset(
		fakeClientset, dbSvc,
		handlers.NewUserTriggersHandler(nil, nil, nil),
		handlers.NewUserWorkflowsHandler(nil, nil),
		nil, // workflow-target lookup (wfStore) wired in app.go
		"test-namespace",
	)
	if h.HasLogger() {
		t.Fatalf("freshly-constructed PodAutomationHandler must not have a logger before SetLogger is called")
	}
	h.SetLogger(lmocks.NewMockLogger())
	if !h.HasLogger() {
		t.Fatalf("SetLogger must populate the handler's logger so 5xx errors include the underlying cause")
	}
}
