// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package agent

import (
	"reflect"
	"testing"
)

// US-65.1 abstraction: the AgentConfigWriter seam.
//
// design/0049 §5 specifies the seam shape: Apply(AgentConfigInput)
// (restartRequired bool, err error). The platform calls Apply and
// branches on restartRequired — it does not know WHY a restart is
// needed. The opencode implementation owns deep-merge semantics,
// OPENCODE_CONFIG path, disabled_providers, and the always-restart
// return value.
//
// These tests pin the shape of the seam (interface + input types) so
// the opencode implementation can be wired in afterwards. They fail
// red until pkg/agent/agentconfig.go is implemented.

func TestAgentConfigInput_NilFieldsMeanLeaveUnchanged(t *testing.T) {
	// Every field on AgentConfigInput is a pointer. nil = leave the
	// corresponding source on the writer unchanged. This matches the
	// existing setter+Rebuild call pattern: each caller updates ONE
	// source per call (relay injector touches only relay; reload
	// handler touches providers + MCP servers; pre-boot relay touches
	// only relay). A full-replace input would force every caller to
	// read+preserve state it does not own — inverting the writer's
	// role as state-holder.
	in := AgentConfigInput{}

	if in.Providers != nil {
		t.Errorf("zero-value AgentConfigInput.Providers = %v, want nil", in.Providers)
	}
	if in.Model != nil {
		t.Errorf("zero-value AgentConfigInput.Model = %v, want nil", in.Model)
	}
	if in.Relay != nil {
		t.Errorf("zero-value AgentConfigInput.Relay = %v, want nil", in.Relay)
	}
	if in.MCPServers != nil {
		t.Errorf("zero-value AgentConfigInput.MCPServers = %v, want nil", in.MCPServers)
	}
	if in.AdminPrompt != nil {
		t.Errorf("zero-value AgentConfigInput.AdminPrompt = %v, want nil", in.AdminPrompt)
	}
	if in.AllowedDirs != nil {
		t.Errorf("zero-value AgentConfigInput.AllowedDirs = %v, want nil", in.AllowedDirs)
	}
}

func TestModelSelection_FullyQualifiedForm(t *testing.T) {
	// ModelSelection is the "providerID/modelID" string opencode
	// expects. Platform code (applyWorkspaceConfig) resolves the
	// providerID before calling Apply; the writer does not look it
	// up.
	//
	// Empty string = no model source. The writer preserves whatever
	// model it already had (set at boot from workspace-config.json).
	ms := ModelSelection("openai/gpt-4o")
	if string(ms) != "openai/gpt-4o" {
		t.Errorf("ModelSelection value = %q, want %q", ms, "openai/gpt-4o")
	}
}

func TestRelayState_ZeroValueIsDisabled(t *testing.T) {
	// RelayState describes what the relay injector discovered. A
	// pointer-to-zero-value means "relay disabled" (URL empty,
	// no models); nil means "leave the existing relay source
	// unchanged". The two are distinct and load-bearing.
	rs := RelayState{}
	if rs.URL != "" {
		t.Errorf("zero-value RelayState.URL = %q, want empty", rs.URL)
	}
	if len(rs.Models) != 0 {
		t.Errorf("zero-value RelayState.Models = %v, want empty", rs.Models)
	}
}

func TestRelayState_URLAndModels(t *testing.T) {
	rs := RelayState{
		URL: "https://relay.example.test/path",
		Models: []RelayModel{
			{ID: "glm-5-free", Name: "GLM-5 Free", ContextLimit: 200000, OutputLimit: 100000},
		},
	}
	if rs.URL != "https://relay.example.test/path" {
		t.Errorf("URL = %q", rs.URL)
	}
	if len(rs.Models) != 1 || rs.Models[0].ID != "glm-5-free" {
		t.Errorf("Models = %+v", rs.Models)
	}
}

func TestAgentConfigInput_PartialUpdates(t *testing.T) {
	// Each caller constructs an AgentConfigInput that fills in only
	// the source it knows about. The writer merges the input onto
	// its existing state.
	t.Run("providers only", func(t *testing.T) {
		in := AgentConfigInput{Providers: &AgentProvidersChange{
			Formatted: []byte(`{"provider":{"openai":{"options":{"apiKey":"sk-test"}}}}`),
		}}
		if in.Providers == nil {
			t.Fatal("Providers must be set")
		}
		if in.Model != nil {
			t.Error("Model should be nil")
		}
		if in.Relay != nil {
			t.Error("Relay should be nil")
		}
		if in.MCPServers != nil {
			t.Error("MCPServers should be nil")
		}
	})

	t.Run("relay only", func(t *testing.T) {
		in := AgentConfigInput{Relay: &RelayState{
			URL:    "https://relay.example.test",
			Models: []RelayModel{{ID: "m1"}},
		}}
		if in.Relay == nil {
			t.Fatal("Relay must be set")
		}
		if in.Providers != nil {
			t.Error("Providers should be nil")
		}
	})

	t.Run("MCP servers only", func(t *testing.T) {
		in := AgentConfigInput{MCPServers: &MCPServerChange{
			Servers: []MCPServerEntry{{Name: "wiki", Transport: "http", URL: "https://wiki.example.com/mcp"}},
		}}
		if in.MCPServers == nil {
			t.Fatal("MCPServers must be set")
		}
		if len(in.MCPServers.Servers) != 1 {
			t.Errorf("Servers len = %d, want 1", len(in.MCPServers.Servers))
		}
	})

	t.Run("relay clear", func(t *testing.T) {
		// A non-nil pointer with empty URL = "clear the relay source".
		// Distinct from nil = "leave unchanged". Used when a future
		// caller wants to disable relay after it was set.
		in := AgentConfigInput{Relay: &RelayState{}}
		if in.Relay == nil {
			t.Fatal("Relay must be non-nil to signal clear")
		}
		if in.Relay.URL != "" {
			t.Errorf("URL = %q, want empty for clear", in.Relay.URL)
		}
	})
}

func TestMCPServerEntry_FieldsRoundTrip(t *testing.T) {
	// MCPServerEntry mirrors the fields the opencode adapter needs to
	// render one MCP server entry. It is agent-neutral: opencode's
	// remote/local rendering happens inside the adapter.
	srv := MCPServerEntry{
		Name:      "github",
		Transport: "stdio",
		Command:   "npx",
		Args:      []string{"-y", "server-github"},
		Env:       map[string]string{"GITHUB_TOKEN": "{env:GITHUB_TOKEN}"},
	}
	if srv.Name != "github" {
		t.Errorf("Name = %q", srv.Name)
	}
	if srv.Transport != "stdio" {
		t.Errorf("Transport = %q", srv.Transport)
	}
	if !reflect.DeepEqual(srv.Args, []string{"-y", "server-github"}) {
		t.Errorf("Args = %v", srv.Args)
	}
}

// fakeConfigWriter is a minimal in-memory implementation used to
// verify the interface contract compiles and behaves as documented.
// Real implementation lives in pkg/agent/opencode/.
type fakeConfigWriter struct {
	applyCount    int
	lastInput     AgentConfigInput
	restartResult bool
	applyErr      error
	relaySet      bool
}

func (f *fakeConfigWriter) Apply(in AgentConfigInput) (bool, error) {
	f.applyCount++
	f.lastInput = in
	if in.Relay != nil {
		f.relaySet = in.Relay.URL != ""
	}
	return f.restartResult, f.applyErr
}

func (f *fakeConfigWriter) HasRelay() bool {
	return f.relaySet
}

// Verify *fakeConfigWriter satisfies the interface.
var _ AgentConfigWriter = (*fakeConfigWriter)(nil)

func TestAgentConfigWriter_InterfaceContract(t *testing.T) {
	// Platform code holds an AgentConfigWriter (interface), calls
	// Apply, and branches on restartRequired. It does NOT know the
	// concrete type. This test verifies the interface is usable as
	// documented in design 0049 §5.
	var w AgentConfigWriter = &fakeConfigWriter{restartResult: true}

	restart, err := w.Apply(AgentConfigInput{
		Relay: &RelayState{URL: "https://relay.example", Models: []RelayModel{{ID: "m"}}},
	})
	if err != nil {
		t.Fatalf("Apply returned err: %v", err)
	}
	if !restart {
		t.Error("restartRequired = false, want true (opencode implementation always restarts)")
	}
	if !w.HasRelay() {
		t.Error("HasRelay = false after Apply with non-empty relay URL")
	}
}

func TestAgentConfigWriter_NilSafeApply(t *testing.T) {
	// A nil AgentConfigInput is a no-op: the writer has nothing to
	// update. This matches the existing pattern where callers guard
	// with `if formatted != nil && deps.AgentConfigWriter != nil`.
	//
	// The interface implementation MUST handle this without panic.
	w := &fakeConfigWriter{restartResult: true}
	restart, err := w.Apply(AgentConfigInput{})
	if err != nil {
		t.Fatalf("Apply(empty) returned err: %v", err)
	}
	if !restart {
		t.Error("Apply(empty) restart = false; opencode always restarts even when nothing changed")
	}
}

func TestAgentConfigWriter_NilSafeHasRelay(t *testing.T) {
	// HasRelay must return false on a fresh writer.
	w := &fakeConfigWriter{}
	if w.HasRelay() {
		t.Error("HasRelay = true on fresh writer, want false")
	}
}
