// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package types

import (
	"encoding/json"
	"testing"
)

func TestValidMCPServerName(t *testing.T) {
	valid := []string{"a", "github-tools", "wiki_server", "server-1", "ABC123", "x" + repeat("_", 61) + "y"}
	for _, n := range valid {
		if !ValidMCPServerName(n) {
			t.Errorf("expected %q to be valid", n)
		}
	}
	invalid := []string{"", "-leading", "has space", "dot.dot", "ünïcödé", repeat("a", 65)}
	for _, n := range invalid {
		if ValidMCPServerName(n) {
			t.Errorf("expected %q to be invalid", n)
		}
	}
}

func TestValidMCPServerTransport(t *testing.T) {
	for _, tr := range []string{MCPServerTransportHTTP, MCPServerTransportSSE, MCPServerTransportStdio} {
		if !ValidMCPServerTransport(tr) {
			t.Errorf("expected %q valid", tr)
		}
	}
	for _, tr := range []string{"", "ws", "local", "remote", "HTTP"} {
		if ValidMCPServerTransport(tr) {
			t.Errorf("expected %q invalid", tr)
		}
	}
}

func TestMCPServerSecretPayloadRoundTrip(t *testing.T) {
	p := &MCPServerSecretPayload{
		Env:     map[string]string{"GITHUB_TOKEN": "ghp_x"},
		Headers: map[string]string{"Authorization": "Bearer y"},
	}
	b, err := p.Encode()
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	got, err := DecodeMCPServerSecretPayload(b)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Env["GITHUB_TOKEN"] != "ghp_x" || got.Headers["Authorization"] != "Bearer y" {
		t.Fatalf("round trip lost data: %+v", got)
	}
}

// TestMCPServerResponse_NeverLeaksSecret asserts the API response shape never
// serializes ciphertext or key version — the core security invariant (D5).
// The response type has no ciphertext/key fields at all, so this is a structural
// guard: confirm the JSON tags are absent.
func TestMCPServerResponse_NeverLeaksSecret(t *testing.T) {
	resp := MCPServerResponse{
		ID: "id", Name: "n", Transport: MCPServerTransportHTTP, URL: "https://x",
		HasSecret: true, Enabled: true,
	}
	b, err := json.Marshal(resp)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	str := string(b)
	for _, leak := range []string{"ciphertext", "keyVersion", "key_version", "apiKey", "secret"} {
		if contains(str, leak) {
			t.Errorf("response leaked %q: %s", leak, str)
		}
	}
}

func TestMCPServerResponse_HasSecretField(t *testing.T) {
	resp := MCPServerResponse{HasSecret: true}
	if !resp.HasSecret {
		t.Errorf("HasSecret should be true")
	}
}

// --- org policy accessors (Epic 53) ---

func TestOrgPolicyValues_IsUserMcpAllowed_DefaultsLocked(t *testing.T) {
	if (&OrgPolicyValues{}).IsUserMcpAllowed() {
		t.Errorf("IsUserMcpAllowed must default to false (locked)")
	}
	on := true
	if !(&OrgPolicyValues{AllowUserMcpServers: &on}).IsUserMcpAllowed() {
		t.Errorf("IsUserMcpAllowed should be true when enabled")
	}
}

func TestOrgPolicyValues_MaxMcpServers_Default(t *testing.T) {
	if got := (&OrgPolicyValues{}).MaxMcpServers(); got != DefaultMaxMcpServersPerWorkspace {
		t.Errorf("default quota = %d, want %d", got, DefaultMaxMcpServersPerWorkspace)
	}
	five := 5
	if got := (&OrgPolicyValues{MaxMcpServersPerWorkspace: &five}).MaxMcpServers(); got != 5 {
		t.Errorf("set quota = %d, want 5", got)
	}
	neg := -1
	if got := (&OrgPolicyValues{MaxMcpServersPerWorkspace: &neg}).MaxMcpServers(); got != 0 {
		t.Errorf("negative quota should clamp to 0, got %d", got)
	}
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (indexOf(s, sub) >= 0)
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}

func repeat(s string, n int) string {
	out := ""
	for i := 0; i < n; i++ {
		out += s
	}
	return out
}
