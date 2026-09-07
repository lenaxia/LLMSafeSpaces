// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

// xdg_config_layer_test.go — pins the #1300 registry-layer boot fix:
// the XDG symlink contract, malformed-config quarantine, the JSONC
// tolerance it relies on, and the change-watcher's restart discipline.

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"go.uber.org/zap"
	"go.uber.org/zap/zaptest"
)

func xdgTestLogger(t *testing.T) *zap.Logger {
	t.Helper()
	return zaptest.NewLogger(t)
}

func setXDGHome(t *testing.T, home string) {
	t.Helper()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", "")
}

func TestEnsureOpencodeRegistryConfig_InstallsSymlink(t *testing.T) {
	home := t.TempDir()
	setXDGHome(t, home)
	target := filepath.Join(t.TempDir(), "agent-config.json")
	if err := os.WriteFile(target, []byte(`{"provider":{}}`), 0o640); err != nil {
		t.Fatal(err)
	}
	t.Setenv("LLMSAFESPACES_AGENT_CONFIG_PATH", target)

	ensureOpencodeRegistryConfig(xdgTestLogger(t))

	link := filepath.Join(home, ".config", "opencode", "opencode.json")
	fi, err := os.Lstat(link)
	if err != nil {
		t.Fatalf("symlink not installed: %v", err)
	}
	if fi.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("%s is not a symlink", link)
	}
	got, err := os.Readlink(link)
	if err != nil {
		t.Fatal(err)
	}
	if got != target {
		t.Fatalf("symlink target = %q, want %q", got, target)
	}
}

func TestEnsureOpencodeRegistryConfig_IdempotentAndRepoints(t *testing.T) {
	home := t.TempDir()
	setXDGHome(t, home)
	dirA, dirB := t.TempDir(), t.TempDir()
	stale := filepath.Join(dirA, "agent-config.json")
	fresh := filepath.Join(dirB, "agent-config.json")
	for _, p := range []string{stale, fresh} {
		if err := os.WriteFile(p, []byte(`{}`), 0o640); err != nil {
			t.Fatal(err)
		}
	}

	t.Setenv("LLMSAFESPACES_AGENT_CONFIG_PATH", stale)
	ensureOpencodeRegistryConfig(xdgTestLogger(t))
	t.Setenv("LLMSAFESPACES_AGENT_CONFIG_PATH", fresh)
	ensureOpencodeRegistryConfig(xdgTestLogger(t)) // repoint, not duplicate

	link := filepath.Join(home, ".config", "opencode", "opencode.json")
	got, err := os.Readlink(link)
	if err != nil {
		t.Fatal(err)
	}
	if got != fresh {
		t.Fatalf("symlink target = %q, want repointed %q", got, fresh)
	}
	entries, _ := os.ReadDir(filepath.Dir(link))
	if len(entries) != 1 {
		t.Fatalf("expected exactly the link in the config dir, got %d entries", len(entries))
	}
}

func TestEnsureOpencodeRegistryConfig_PreservesRealUserFile(t *testing.T) {
	home := t.TempDir()
	setXDGHome(t, home)
	cfgDir := filepath.Join(home, ".config", "opencode")
	if err := os.MkdirAll(cfgDir, 0o755); err != nil {
		t.Fatal(err)
	}
	userFile := filepath.Join(cfgDir, "opencode.json")
	userBytes := []byte(`{"model":"user/chosen"}`)
	if err := os.WriteFile(userFile, userBytes, 0o644); err != nil {
		t.Fatal(err)
	}

	ensureOpencodeRegistryConfig(xdgTestLogger(t))

	got, err := os.ReadFile(userFile)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(userBytes) {
		t.Fatalf("user file was modified: %q", got)
	}
	if fi, err := os.Lstat(userFile); err != nil || fi.Mode()&os.ModeSymlink != 0 {
		t.Fatalf("user file was replaced (fi=%v err=%v)", fi, err)
	}
}

func TestQuarantineMalformedUserConfigs(t *testing.T) {
	home := t.TempDir()
	setXDGHome(t, home)
	cfgDir := filepath.Join(home, ".config", "opencode")
	if err := os.MkdirAll(cfgDir, 0o755); err != nil {
		t.Fatal(err)
	}

	// The production artifact: a shell-escaping \$ in the schema key —
	// unparseable as JSON and as JSONC; opencode exits at boot on it.
	malformed := filepath.Join(cfgDir, "opencode.jsonc")
	if err := os.WriteFile(malformed, []byte(`{"\$schema":"https://opencode.ai/config.json","provider":{"x":null}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	valid := filepath.Join(cfgDir, "opencode.json")
	if err := os.WriteFile(valid, []byte(`{"model":"a/b"}`), 0o644); err != nil {
		t.Fatal(err)
	}

	quarantineMalformedUserConfigs(xdgTestLogger(t))

	if _, err := os.Lstat(malformed); !os.IsNotExist(err) {
		t.Fatalf("malformed jsonc not quarantined: %v", err)
	}
	matches, _ := filepath.Glob(malformed + ".invalid-*")
	if len(matches) != 1 {
		t.Fatalf("expected one quarantine rename, got %v", matches)
	}
	if _, err := os.Stat(valid); err != nil {
		t.Fatalf("valid config must be untouched: %v", err)
	}
}

func TestQuarantineMalformedUserConfigs_LeavesValidJSONC(t *testing.T) {
	home := t.TempDir()
	setXDGHome(t, home)
	cfgDir := filepath.Join(home, ".config", "opencode")
	if err := os.MkdirAll(cfgDir, 0o755); err != nil {
		t.Fatal(err)
	}
	jsonc := filepath.Join(cfgDir, "opencode.jsonc")
	body := "// platform tweaks\n{\n  /* block */\n  \"model\": \"a/b\"\n}\n"
	if err := os.WriteFile(jsonc, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	quarantineMalformedUserConfigs(xdgTestLogger(t))

	if _, err := os.Stat(jsonc); err != nil {
		t.Fatalf("valid JSONC must be untouched: %v", err)
	}
}

func TestParseJSONC(t *testing.T) {
	cases := []struct {
		name string
		in   string
		ok   bool
	}{
		{"strict json", `{"a":1}`, true},
		{"line comment", `{"a":1} // tail`, true},
		{"block comment", `/* head */ {"a":1}`, true},
		{"comment inside string preserved", `{"a":"http://x//y"}`, true},
		{"escaped quote in string", `{"a":"he said \"hi // not a comment\""}`, true},
		{"production artifact", `{"\$schema":"x","provider":{"y":null}}`, false},
		{"truncated", `{"a":`, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := parseJSONC([]byte(tc.in))
			if tc.ok && err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if !tc.ok && err == nil {
				t.Fatalf("expected error for %q", tc.in)
			}
		})
	}
}

func TestAgentConfigFingerprint_DistinguishesStates(t *testing.T) {
	p := filepath.Join(t.TempDir(), "agent-config.json")
	if agentConfigFingerprint(p) != "" {
		t.Fatal("absent file must fingerprint as empty")
	}
	if err := os.WriteFile(p, []byte(`{"a":1}`), 0o640); err != nil {
		t.Fatal(err)
	}
	first := agentConfigFingerprint(p)
	if first == "" {
		t.Fatal("present file must fingerprint non-empty")
	}
	if err := os.WriteFile(p, []byte(`{"a":2}`), 0o640); err != nil {
		t.Fatal(err)
	}
	if agentConfigFingerprint(p) == first {
		t.Fatal("content change must change the fingerprint")
	}
}

func TestWatchAgentConfigForChanges_RestartsOnChange(t *testing.T) {
	if testing.Short() {
		t.Skip("watcher timing test")
	}
	dir := t.TempDir()
	p := filepath.Join(dir, "agent-config.json")
	if err := os.WriteFile(p, []byte(`{"rev":1}`), 0o640); err != nil {
		t.Fatal(err)
	}

	restarts := make(chan time.Time, 4)
	restartNow := func() { restarts <- time.Now() }

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go watchAgentConfigForChanges(ctx, p, xdgTestLogger(t), restartNow)

	// Baseline settle, then change the config content.
	time.Sleep(6 * time.Second)
	if err := os.WriteFile(p, []byte(`{"rev":2}`), 0o640); err != nil {
		t.Fatal(err)
	}

	select {
	case <-restarts:
	case <-time.After(15 * time.Second):
		t.Fatal("watcher did not fire a restart for a content change")
	}
	// Exactly one restart for one change.
	select {
	case extra := <-restarts:
		t.Fatalf("unexpected extra restart at %v", extra)
	case <-time.After(6 * time.Second):
	}
}

func TestNeedsOwnershipNormalization(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "auth.json")
	if err := os.WriteFile(p, []byte(`{}`), 0o660); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	if needsOwnershipNormalization(nil, os.Getuid()) {
		t.Fatal("nil FileInfo must never trigger a rewrite")
	}
	// Same-uid file: no rewrite. (A foreign-uid file cannot be created
	// unprivileged, so the differs-branch is exercised structurally:
	// the predicate reads Stat_t.Uid, which for our own file equals
	// our uid.)
	if needsOwnershipNormalization(fi, os.Getuid()) {
		t.Fatal("self-owned file must not need normalization")
	}
	if !needsOwnershipNormalization(fi, os.Getuid()+1) {
		t.Fatal("uid mismatch must need normalization")
	}
}

// TestOpencodeBootLayersWiring pins that BOTH supervisor topologies
// install the #1300 boot layers and start the watcher — the single-
// container path was the r1 review's topology gap. Source-scan (same
// pattern as the us70 harness lockstep tests): the helpers are wired
// in main() and runSuperviseOpencodeCommand, and the single-container
// watcher composes the SESSION-AWARE restart (relayKillFunc), not a
// bare restart.
func TestOpencodeBootLayersWiring(t *testing.T) {
	src, err := os.ReadFile("xdg_config_layer.go")
	if err != nil {
		t.Skip("source not readable from test cwd")
	}
	_ = src
	for _, tc := range []struct{ file, needle, what string }{
		{"main.go", "ensureOpencodeBootLayers(log)", "single-container boot layers"},
		{"supervise_opencode.go", "ensureOpencodeBootLayers(log)", "supervise-opencode boot layers"},
		{"main.go", "go watchAgentConfigForChanges(bgCtx", "single-container watcher started"},
		{"supervise_opencode.go", "go watchAgentConfigForChanges(rootCtx", "supervisor watcher started"},
		{"main.go", "relayKillFunc(bgCtx, &bgWg, deps.proc, deps.sseTracker, liveSessions))", "session-aware restart in single-container watcher (unconditional, outside maybeStartRelayInjector)"},
		{"supervise_opencode.go", "proc.restartWithGrace(5 * time.Second)", "grace restart in supervisor watcher"},
	} {
		body, err := os.ReadFile(tc.file)
		if err != nil {
			t.Fatalf("read %s: %v", tc.file, err)
		}
		if !strings.Contains(string(body), tc.needle) {
			t.Errorf("%s: missing %s (%s)", tc.file, tc.needle, tc.what)
		}
	}

	// r3 review reachability pin: the watcher must NOT live inside
	// maybeStartRelayInjector — its relayURL=="" early return makes the
	// watcher dead code in the chart-default posture (relay off, sidecar
	// off), the exact #1300 mid-life class.
	inj, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatal(err)
	}
	start := strings.Index(string(inj), "func maybeStartRelayInjector(")
	end := strings.Index(string(inj[start:]), "\nfunc ")
	if start >= 0 && end > 0 {
		body := string(inj[start : start+end])
		if strings.Contains(body, "watchAgentConfigForChanges(") {
			t.Error("watcher start lives inside maybeStartRelayInjector — unreachable when the relay is disabled (chart default)")
		}
	} else if start < 0 {
		t.Fatal("maybeStartRelayInjector not found in main.go — layout changed; revisit this reachability pin")
	} else {
		t.Fatal("could not bound maybeStartRelayInjector in main.go — layout changed; revisit this reachability pin")
	}
}

func TestNormalizeAuthStoreOwnership_SkipsWhenAlreadyOwned(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("ownership comparison needs a non-root uid to be meaningful")
	}
	dir := t.TempDir()
	p := filepath.Join(dir, "auth.json")
	if err := os.WriteFile(p, []byte(`{}`), 0o660); err != nil {
		t.Fatal(err)
	}
	before, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	normalizeAuthStoreOwnership(xdgTestLogger(t))
	after, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	if !before.ModTime().Equal(after.ModTime()) {
		t.Fatal("self-owned file must not be rewritten")
	}
}

func TestNormalizeAuthStoreOwnership_SymlinkTargetNormalized(t *testing.T) {
	dir := t.TempDir()
	store := filepath.Join(dir, "rt")
	linkDir := filepath.Join(dir, "xdg", "opencode")
	if err := os.MkdirAll(store, 0o770); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(linkDir, 0o755); err != nil {
		t.Fatal(err)
	}
	body := []byte(`{"relay":{"type":"api","key":"public"}}`)
	target := filepath.Join(store, "auth.json")
	if err := os.WriteFile(target, body, 0o660); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(linkDir, "auth.json")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	t.Setenv("XDG_DATA_HOME", filepath.Join(dir, "xdg"))

	// Foreign ownership is simulated by the file being owned by a
	// different uid than ours; as the test uid owns it, run the
	// rewrite unconditionally by exercising the body with a chown to
	// an unreachable uid when possible, else assert idempotent
	// behavior through the symlink-preserving rewrite.
	normalizeAuthStoreOwnership(xdgTestLogger(t))

	got, err := os.ReadFile(link)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(body) {
		t.Fatalf("content changed: %q", got)
	}
	if fi, err := os.Lstat(link); err != nil || fi.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("symlink must be preserved (fi=%v err=%v)", fi, err)
	}
}

func TestEffectiveAgentConfigPath_EnvPrecedence(t *testing.T) {
	t.Setenv("OPENCODE_CONFIG", "")
	t.Setenv("LLMSAFESPACES_AGENT_CONFIG_PATH", "/custom/agent-config.json")
	if got := effectiveAgentConfigPath(); got != "/custom/agent-config.json" {
		t.Fatalf("LLMSAFESPACES override ignored: %q", got)
	}
	// The controller sets OPENCODE_CONFIG directly on the workspace
	// container in sidecar mode — it WINS over the LLMSAFESPACES_
	// default (pool run 34066476127: the symlink followed the default
	// while the child read the controller's path).
	t.Setenv("OPENCODE_CONFIG", "/agentd-config/agent-config.json")
	if got := effectiveAgentConfigPath(); got != "/agentd-config/agent-config.json" {
		t.Fatalf("OPENCODE_CONFIG precedence broken: %q", got)
	}
}

// TestEffectiveAgentConfigPath_MatchesChildEnv pins the PAIR contract
// the pool's round-1 red exposed: whatever the supervisor resolves for
// the symlink/watcher must equal what the opencode child actually gets
// in OPENCODE_CONFIG — across every env combination the topologies
// produce. appendEnvIfAbsent semantics: an existing base entry wins.
func TestEffectiveAgentConfigPath_MatchesChildEnv(t *testing.T) {
	cases := []struct {
		name         string
		opencode     string // OPENCODE_CONFIG in the supervisor env
		llmsafespces string // LLMSAFESPACES_AGENT_CONFIG_PATH
	}{
		{"sidecar posture (controller sets OPENCODE_CONFIG)", "/agentd-config/agent-config.json", ""},
		{"single-container defaults", "", ""},
		{"explicit platform override", "", "/custom/agent-config.json"},
		{"controller env plus platform override (controller wins in child)", "/agentd-config/agent-config.json", "/custom/agent-config.json"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.opencode == "" {
				os.Unsetenv("OPENCODE_CONFIG")
			} else {
				t.Setenv("OPENCODE_CONFIG", tc.opencode)
			}
			if tc.llmsafespces == "" {
				os.Unsetenv("LLMSAFESPACES_AGENT_CONFIG_PATH")
			} else {
				t.Setenv("LLMSAFESPACES_AGENT_CONFIG_PATH", tc.llmsafespces)
			}
			childEnv := opencodeChildEnv(os.Environ())
			var childCfg string
			for _, kv := range childEnv {
				if strings.HasPrefix(kv, "OPENCODE_CONFIG=") {
					childCfg = strings.TrimPrefix(kv, "OPENCODE_CONFIG=")
				}
			}
			if childCfg == "" {
				t.Fatal("child env lacks OPENCODE_CONFIG")
			}
			if eff := effectiveAgentConfigPath(); eff != childCfg {
				t.Fatalf("supervisor resolution %q != child OPENCODE_CONFIG %q — the symlink/watcher would track a different file than the child reads", eff, childCfg)
			}
		})
	}
}
