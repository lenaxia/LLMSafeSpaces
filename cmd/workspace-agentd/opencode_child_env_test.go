// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"strings"
	"testing"

	"github.com/lenaxia/llmsafespaces/pkg/agentd"
)

// Design 0053 §4.1/S3: entrypoint-opencode.sh's opencode env exports are
// absorbed by the supervisor spawn seam (#942 containment — opencode
// env-var names are runtime knowledge, never controller knowledge).

func envOf(env []string, name string) (string, bool) {
	for _, e := range env {
		if strings.HasPrefix(e, name+"=") {
			return strings.TrimPrefix(e, name+"="), true
		}
	}
	return "", false
}

func TestOpencodeChildEnv_AppliesEntrypointDefaults(t *testing.T) {
	oldPw := bootAgentPassword
	t.Cleanup(func() { bootAgentPassword = oldPw })
	bootAgentPassword = "sekrit"

	env := opencodeChildEnv([]string{"PATH=/usr/bin", "HOME=/home/sandbox"})

	if v, ok := envOf(env, "OPENCODE_CONFIG"); !ok || v != agentd.AgentConfigPath {
		t.Errorf("OPENCODE_CONFIG = %q ok=%v, want default %q", v, ok, agentd.AgentConfigPath)
	}
	if v, ok := envOf(env, "XDG_DATA_HOME"); !ok || v != "/workspace/.local" {
		t.Errorf("XDG_DATA_HOME = %q ok=%v, want /workspace/.local", v, ok)
	}
	if v, ok := envOf(env, "OPENCODE_EXPERIMENTAL_EVENT_SYSTEM"); !ok || v != "true" {
		t.Errorf("OPENCODE_EXPERIMENTAL_EVENT_SYSTEM = %q ok=%v, want true", v, ok)
	}
	if v, ok := envOf(env, "OPENCODE_SERVER_PASSWORD"); !ok || v != "sekrit" {
		t.Errorf("OPENCODE_SERVER_PASSWORD = %q ok=%v, want boot password", v, ok)
	}
	for _, keep := range []string{"PATH=/usr/bin", "HOME=/home/sandbox"} {
		if _, ok := envOf(env, strings.SplitN(keep, "=", 2)[0]); !ok {
			t.Errorf("base entry %s dropped", keep)
		}
	}
}

func TestOpencodeChildEnv_SidecarEnvWins(t *testing.T) {
	oldPw := bootAgentPassword
	t.Cleanup(func() { bootAgentPassword = oldPw })
	bootAgentPassword = "single-container-pw"

	base := []string{
		"OPENCODE_CONFIG=/agentd-config/agent-config.json",
		"XDG_DATA_HOME=/workspace/.local",
		"OPENCODE_EXPERIMENTAL_EVENT_SYSTEM=true",
		"OPENCODE_SERVER_PASSWORD=from-controller",
	}

	env := opencodeChildEnv(append([]string{}, base...))

	if v, _ := envOf(env, "OPENCODE_CONFIG"); v != "/agentd-config/agent-config.json" {
		t.Errorf("sidecar OPENCODE_CONFIG overwritten: %q", v)
	}
	if v, _ := envOf(env, "OPENCODE_SERVER_PASSWORD"); v != "from-controller" {
		t.Errorf("sidecar OPENCODE_SERVER_PASSWORD overwritten: %q", v)
	}
	if len(env) != len(base) {
		t.Errorf("env grew appending duplicates: %v", env)
	}
}

func TestOpencodeChildEnv_NoPasswordNoEntry(t *testing.T) {
	oldPw := bootAgentPassword
	t.Cleanup(func() { bootAgentPassword = oldPw })
	bootAgentPassword = ""

	env := opencodeChildEnv(nil)

	if _, ok := envOf(env, "OPENCODE_SERVER_PASSWORD"); ok {
		t.Error("OPENCODE_SERVER_PASSWORD must be absent when no boot password (supervise-opencode mode)")
	}
}

func TestOpencodeChildEnv_ConfigHonorsSidecarOverride(t *testing.T) {
	t.Setenv("LLMSAFESPACES_AGENT_CONFIG_PATH", "/agentd-config/agent-config.json")

	env := opencodeChildEnv(nil)

	if v, _ := envOf(env, "OPENCODE_CONFIG"); v != "/agentd-config/agent-config.json" {
		t.Errorf("OPENCODE_CONFIG = %q, want sidecar relocation", v)
	}
}
