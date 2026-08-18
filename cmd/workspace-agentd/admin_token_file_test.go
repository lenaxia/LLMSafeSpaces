package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// #887 D5.1: the admin-mux bearer token must be deliverable file-only
// (AGENTD_ADMIN_TOKEN_FILE) so it never enters the environment opencode
// inherits and passes to every tool process (extendEnv spawn).

func TestResolveAdminToken_FilePreferredOverEnv(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "admin-token")
	if err := os.WriteFile(path, []byte("file-token"), 0o400); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AGENTD_ADMIN_TOKEN_FILE", path)
	t.Setenv("AGENTD_ADMIN_TOKEN", "env-token")

	if got := adminToken(); got != "file-token" {
		t.Errorf("adminToken() = %q, want file value %q (file must win over env)", got, "file-token")
	}
}

func TestResolveAdminToken_EnvFallback(t *testing.T) {
	t.Setenv("AGENTD_ADMIN_TOKEN", "env-token")
	if got := adminToken(); got != "env-token" {
		t.Errorf("adminToken() = %q, want env value %q (legacy pods deliver via env)", got, "env-token")
	}
}

func TestResolveAdminToken_None(t *testing.T) {
	// Explicit unset: a workspace pod running the tests carries the real
	// token in its own environment.
	t.Setenv("AGENTD_ADMIN_TOKEN", "")
	t.Setenv("AGENTD_ADMIN_TOKEN_FILE", "")
	if got := adminToken(); got != "" {
		t.Errorf("adminToken() = %q, want empty (dev mode, gate off)", got)
	}
}

func TestReadAdminTokenFile_TrimsWhitespace(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "admin-token")
	if err := os.WriteFile(path, []byte("  tok \n"), 0o400); err != nil {
		t.Fatal(err)
	}
	got, err := readAdminTokenFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if got != "tok" {
		t.Errorf("readAdminTokenFile() = %q, want trimmed %q", got, "tok")
	}
}

func TestReadAdminTokenFile_Error(t *testing.T) {
	if _, err := readAdminTokenFile("/nonexistent/admin-token"); err == nil {
		t.Error("expected error for unreadable file")
	}
}

// The scrub: buildEnvFrom must drop agentd-only credential vars from the
// environment handed to opencode (and thereby to every tool process).
func TestBuildEnvFrom_ScrubsAdminTokenVars(t *testing.T) {
	dir := t.TempDir()
	secretsEnv := filepath.Join(dir, "secrets-env")
	if err := os.WriteFile(secretsEnv, []byte("export UNITTEST_SCRUB_PROBE_7f3a=probe-ok\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AGENTD_ADMIN_TOKEN", "leak-me")
	t.Setenv("AGENTD_ADMIN_TOKEN_FILE", "/sandbox-cfg/admin-token")
	t.Setenv("UNITTEST_HARMLESS_7f3a", "keep")

	env := buildEnvFrom(secretsEnv)

	for _, e := range env {
		if e == "AGENTD_ADMIN_TOKEN=leak-me" {
			t.Error("AGENTD_ADMIN_TOKEN leaked into opencode spawn env")
		}
		if e == "AGENTD_ADMIN_TOKEN_FILE=/sandbox-cfg/admin-token" {
			t.Error("AGENTD_ADMIN_TOKEN_FILE leaked into opencode spawn env")
		}
	}
	sawProbe, sawHarmless := false, false
	for _, e := range env {
		if e == "UNITTEST_SCRUB_PROBE_7f3a=probe-ok" {
			sawProbe = true
		}
		if e == "UNITTEST_HARMLESS_7f3a=keep" {
			sawHarmless = true
		}
	}
	if !sawProbe || !sawHarmless {
		t.Errorf("scrub must be surgical: probe=%v HARMLESS_VAR=%v (both should be present)", sawProbe, sawHarmless)
	}
}

// F2 regression: the no-secrets-env early-return path must ALSO scrub —
// a pod with no user env-secrets is the common case, and the unscrubbed
// parent env leaked both admin vars on it.
func TestBuildEnvFrom_NoSecretsFile_StillScrubs(t *testing.T) {
	t.Setenv("AGENTD_ADMIN_TOKEN", "leak-me")
	t.Setenv("AGENTD_ADMIN_TOKEN_FILE", "/sandbox-cfg/admin-token")

	env := buildEnvFrom("/nonexistent/secrets-env")
	for _, e := range env {
		if e == "AGENTD_ADMIN_TOKEN=leak-me" || e == "AGENTD_ADMIN_TOKEN_FILE=/sandbox-cfg/admin-token" {
			t.Fatalf("admin var leaked via the no-file early-return path: %s", e)
		}
	}
}

// F2 regression: the bash-spawn-failure path must ALSO scrub. A mere bad
// file does NOT reach it (bash's `source` failure doesn't abort the -c
// script; `env -0` still runs and bash exits 0 — verified). The genuine
// trigger is exec-level failure: PATH lookup fails for bash itself.
func TestBuildEnvFrom_SourceFailure_StillScrubs(t *testing.T) {
	dir := t.TempDir()
	envFile := filepath.Join(dir, "secrets-env")
	require.NoError(t, os.WriteFile(envFile, []byte("export X=1\n"), 0o600))
	t.Setenv("AGENTD_ADMIN_TOKEN", "leak-me")
	realPath := os.Getenv("PATH")
	t.Setenv("PATH", "") // bash unresolvable → Start error → failure return

	scrubbed := buildEnvFrom(envFile)
	for _, e := range scrubbed {
		if e == "AGENTD_ADMIN_TOKEN=leak-me" {
			t.Fatalf("admin var leaked via the spawn-failure path: %s", e)
		}
	}

	// Sanity that we actually took the failure path: with PATH restored,
	// the same file sources fine and yields the secret var.
	t.Setenv("PATH", realPath)
	ok := buildEnvFrom(envFile)
	sawX := false
	for _, e := range ok {
		if e == "X=1" {
			sawX = true
		}
	}
	require.True(t, sawX, "control: with PATH restored the file must source (proves the first call took the failure path)")
}
