package types

import (
	"encoding/json"
	"testing"
)

func TestValidWorkflowName(t *testing.T) {
	valid := []string{"a", "my-workflow", "My_Workflow 2", "process_meetings_v1", "1leading-digit-ok"}
	invalid := []string{"", "-leading-dash", "_leading-underscore", "$dollar", "tab\tinside", "newline\ninside"}
	for _, n := range valid {
		if !ValidWorkflowName(n) {
			t.Errorf("expected %q to be valid, got invalid", n)
		}
	}
	for _, n := range invalid {
		if ValidWorkflowName(n) {
			t.Errorf("expected %q to be invalid, got valid", n)
		}
	}
}

func TestValidWorkflowSlug(t *testing.T) {
	valid := []string{"a", "my-workflow", "process-meetings-v1", "abc123", "1ok"}
	invalid := []string{"", "-leading", "UPPER", "under_score", "space inside"}
	for _, s := range valid {
		if !ValidWorkflowSlug(s) {
			t.Errorf("expected %q to be valid, got invalid", s)
		}
	}
	for _, s := range invalid {
		if ValidWorkflowSlug(s) {
			t.Errorf("expected %q to be invalid, got valid", s)
		}
	}
}

func TestValidWorkflowStatus(t *testing.T) {
	for _, s := range []string{WorkflowStatusDraft, WorkflowStatusActive, WorkflowStatusArchive} {
		if !ValidWorkflowStatus(s) {
			t.Errorf("expected %q valid", s)
		}
	}
	for _, s := range []string{"", "published", "deleted"} {
		if ValidWorkflowStatus(s) {
			t.Errorf("expected %q invalid", s)
		}
	}
}

func TestValidWorkflowOwnerType(t *testing.T) {
	for _, s := range []string{WorkflowOwnerUser, WorkflowOwnerOrg} {
		if !ValidWorkflowOwnerType(s) {
			t.Errorf("expected %q valid", s)
		}
	}
	for _, s := range []string{"", "admin", "platform"} {
		if ValidWorkflowOwnerType(s) {
			t.Errorf("expected %q invalid (admin/platform deferred to v2)", s)
		}
	}
}

func TestValidOnMissingWorkspace(t *testing.T) {
	for _, s := range []string{OnMissingAbort, OnMissingCreate} {
		if !ValidOnMissingWorkspace(s) {
			t.Errorf("expected %q valid", s)
		}
	}
	for _, s := range []string{"", "skip", "wait", "clone"} {
		if ValidOnMissingWorkspace(s) {
			t.Errorf("expected %q invalid", s)
		}
	}
}

func TestValidTriggerSourceType(t *testing.T) {
	for _, s := range []string{TriggerSourceCron, TriggerSourceWebhook} {
		if !ValidTriggerSourceType(s) {
			t.Errorf("expected %q valid", s)
		}
	}
	for _, s := range []string{"", "manual"} {
		if ValidTriggerSourceType(s) {
			t.Errorf("expected %q invalid (manual is not a source type — D5)", s)
		}
	}
}

func TestValidMemoryMode(t *testing.T) {
	for _, m := range []string{MemoryNone, MemoryLastResult} {
		if !ValidMemoryMode(m) {
			t.Errorf("expected %q valid", m)
		}
	}
	for _, m := range []string{"", "session_chain", "all"} {
		if ValidMemoryMode(m) {
			t.Errorf("expected %q invalid", m)
		}
	}
}

func TestValidCaptureMode(t *testing.T) {
	for _, c := range []string{CaptureErrorsOnly, CaptureFull} {
		if !ValidCaptureMode(c) {
			t.Errorf("expected %q valid", c)
		}
	}
	for _, c := range []string{"", "transcript", "errors"} {
		if ValidCaptureMode(c) {
			t.Errorf("expected %q invalid", c)
		}
	}
}

func TestValidPreserveSession(t *testing.T) {
	for _, p := range []string{PreserveNever, PreserveAlways, PreserveOnFailure} {
		if !ValidPreserveSession(p) {
			t.Errorf("expected %q valid", p)
		}
	}
	for _, p := range []string{"", "sometimes", "on_success"} {
		if ValidPreserveSession(p) {
			t.Errorf("expected %q invalid", p)
		}
	}
}

func TestValidNodeType(t *testing.T) {
	for _, s := range []string{NodeTypeScript, NodeTypeAgent, NodeTypeHTTP, NodeTypeCondition} {
		if !ValidNodeType(s) {
			t.Errorf("expected %q valid", s)
		}
	}
	for _, s := range []string{"", "transform", "parallel", "delay", "mcp_call"} {
		if ValidNodeType(s) {
			t.Errorf("expected %q invalid (deferred to v2)", s)
		}
	}
}

func TestValidRunStatus(t *testing.T) {
	for _, s := range []string{RunStatusQueued, RunStatusRunning, RunStatusSucceeded, RunStatusFailed, RunStatusCanceled, RunStatusTimedOut} {
		if !ValidRunStatus(s) {
			t.Errorf("expected %q valid", s)
		}
	}
	for _, s := range []string{"", "paused", "completed"} {
		if ValidRunStatus(s) {
			t.Errorf("expected %q invalid", s)
		}
	}
}

func TestIsTerminalRunStatus(t *testing.T) {
	terminal := []string{RunStatusSucceeded, RunStatusFailed, RunStatusCanceled, RunStatusTimedOut}
	nonTerminal := []string{RunStatusQueued, RunStatusRunning, ""}
	for _, s := range terminal {
		if !IsTerminalRunStatus(s) {
			t.Errorf("expected %q terminal", s)
		}
	}
	for _, s := range nonTerminal {
		if IsTerminalRunStatus(s) {
			t.Errorf("expected %q non-terminal", s)
		}
	}
}

func TestValidNodeRunStatus(t *testing.T) {
	for _, s := range []string{NodeRunStatusPending, NodeRunStatusRunning, NodeRunStatusSucceeded, NodeRunStatusFailed, NodeRunStatusSkipped} {
		if !ValidNodeRunStatus(s) {
			t.Errorf("expected %q valid", s)
		}
	}
	for _, s := range []string{"", "queued"} {
		if ValidNodeRunStatus(s) {
			t.Errorf("expected %q invalid", s)
		}
	}
}

func TestValidWebhookIdempotencyMode(t *testing.T) {
	for _, s := range []string{WebhookIdempotencyHeader, WebhookIdempotencyHash, WebhookIdempotencyDisabled} {
		if !ValidWebhookIdempotencyMode(s) {
			t.Errorf("expected %q valid", s)
		}
	}
	for _, s := range []string{"", "body"} {
		if ValidWebhookIdempotencyMode(s) {
			t.Errorf("expected %q invalid", s)
		}
	}
}

func TestValidTriggerFireStatus(t *testing.T) {
	for _, s := range []string{TriggerFireFired, TriggerFireDelivered, TriggerFireFailed, TriggerFireValidationError, TriggerFireRateLimited, TriggerFireSkipped, TriggerFireAutoDisabled} {
		if !ValidTriggerFireStatus(s) {
			t.Errorf("expected %q valid", s)
		}
	}
	for _, s := range []string{"", "pending", "running"} {
		if ValidTriggerFireStatus(s) {
			t.Errorf("expected %q invalid", s)
		}
	}
}

func TestValidRunErrorCode(t *testing.T) {
	codes := []string{
		RunErrorCodeNodeFailed, RunErrorCodeWorkspaceUnavailable, RunErrorCodeCanceled,
		RunErrorCodeTimedOut, RunErrorCodeValidationError, RunErrorCodeSchemaMismatch,
		RunErrorCodeOutputOversize, RunErrorCodeAgentNotFound, RunErrorCodeSessionNotFound,
		RunErrorCodeSecretNotFound, RunErrorCodeScriptFailed, RunErrorCodeScriptOutputInvalid,
		RunErrorCodeAPIRestart,
	}
	for _, c := range codes {
		if !ValidRunErrorCode(c) {
			t.Errorf("expected %q valid", c)
		}
	}
	for _, c := range []string{"", "unknown", "broken"} {
		if ValidRunErrorCode(c) {
			t.Errorf("expected %q invalid", c)
		}
	}
}

func TestWorkflowResponse_JSONRoundTrip(t *testing.T) {
	resp := WorkflowResponse{
		ID:          "wf_123",
		OwnerType:   WorkflowOwnerUser,
		Name:        "my-workflow",
		Slug:        "my-workflow",
		SpecYAML:    "name: test\n",
		Status:      WorkflowStatusDraft,
		InputSchema: json.RawMessage(`{"type":"object","properties":{"x":{"type":"string"}}}`),
	}
	b, err := json.Marshal(resp)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(b, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if decoded["id"] != "wf_123" {
		t.Errorf("id mismatch: %v", decoded["id"])
	}
	if decoded["ownerType"] != "user" {
		t.Errorf("ownerType mismatch: %v", decoded["ownerType"])
	}
	if decoded["status"] != "draft" {
		t.Errorf("status mismatch: %v", decoded["status"])
	}
}

func TestCreateWorkflowRequest_Binding(t *testing.T) {
	body := `{"name":"my-workflow","specYaml":"name: test\n","inputSchema":{"type":"object"}}`
	var req CreateWorkflowRequest
	if err := json.Unmarshal([]byte(body), &req); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if req.Name != "my-workflow" {
		t.Errorf("name: %q", req.Name)
	}
	if req.SpecYAML != "name: test\n" {
		t.Errorf("specYaml: %q", req.SpecYAML)
	}
}

func TestUpdateWorkflowRequest_PartialUpdate(t *testing.T) {
	body := `{"status":"active"}`
	var req UpdateWorkflowRequest
	if err := json.Unmarshal([]byte(body), &req); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if req.Status == nil || *req.Status != "active" {
		t.Errorf("status not set: %v", req.Status)
	}
	if req.Name != nil {
		t.Errorf("name should be nil (not provided): %v", req.Name)
	}
	if req.SpecYAML != nil {
		t.Errorf("specYaml should be nil: %v", req.SpecYAML)
	}
}

func TestTriggerResponse_JSONRoundTrip(t *testing.T) {
	resp := TriggerResponse{
		ID:           "trg_456",
		OwnerType:    WorkflowOwnerOrg,
		Name:         "nightly-backup",
		Enabled:      true,
		SourceType:   TriggerSourceCron,
		SourceConfig: json.RawMessage(`{"expr":"0 2 * * *","tz":"UTC"}`),
		WorkspaceID:  "ws_1",
		Prompt:       "Run the nightly backup and summarize results.",
		MemoryMode:   MemoryLastResult,
		CaptureMode:  CaptureFull,
	}
	b, err := json.Marshal(resp)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(b, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if decoded["sourceType"] != "cron" {
		t.Errorf("sourceType: %v", decoded["sourceType"])
	}
	if decoded["prompt"] != "Run the nightly backup and summarize results." {
		t.Errorf("prompt: %v", decoded["prompt"])
	}
	if decoded["memoryMode"] != "last_result" {
		t.Errorf("memoryMode: %v", decoded["memoryMode"])
	}
}

func TestWorkflowRunResponse_ErrorFields(t *testing.T) {
	resp := WorkflowRunResponse{
		ID:        "run_789",
		Status:    RunStatusFailed,
		ErrorCode: RunErrorCodeWorkspaceUnavailable,
		Error:     json.RawMessage(`{"detail":"workspace failed to activate within 120s"}`),
	}
	if resp.ErrorCode != RunErrorCodeWorkspaceUnavailable {
		t.Errorf("expected WorkspaceUnavailable, got %s", resp.ErrorCode)
	}
}

func TestTypedSourceConfigs(t *testing.T) {
	cron := CronSourceConfig{Expr: "0 2 * * *", TZ: "UTC"}
	b, _ := json.Marshal(cron)
	var decoded map[string]any
	json.Unmarshal(b, &decoded)
	if decoded["expr"] != "0 2 * * *" {
		t.Errorf("cron expr: %v", decoded["expr"])
	}
}
