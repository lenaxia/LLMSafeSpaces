// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.uber.org/zap"
)

func TestWriteRedactWrapper_CreatesExecutableScript(t *testing.T) {
	dir := t.TempDir()

	if err := writeRedactWrapper(dir, "/agentd/usr/local/bin/workspace-agentd"); err != nil {
		t.Fatalf("writeRedactWrapper() error = %v", err)
	}

	path := filepath.Join(dir, "redact")
	info, err := os.Stat(path)
		if err != nil {
		t.Fatalf("wrapper not created: %v", err)
	}
	if info.Mode().Perm()&0o111 == 0 {
		t.Errorf("wrapper is not executable: mode %v", info.Mode().Perm())
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("failed to read wrapper: %v", err)
	}
	content := string(data)
	if !strings.HasPrefix(content, "#!/bin/sh\n") {
		t.Errorf("wrapper missing sh shebang: %q", content)
	}
	want := `exec '/agentd/usr/local/bin/workspace-agentd' redact "$@"`
	if !strings.Contains(content, want) {
		t.Errorf("wrapper missing exec line %q in: %q", want, content)
	}
}

func TestWriteRedactWrapper_QuotingKeepsPathOneArgv(t *testing.T) {
	dir := t.TempDir()
	// A path with every shell-dangerous character must still reach the
	// exec as a single argument: spaces split argv, $ interpolates
	// inside double quotes, quotes terminate early.
	hostile := `/opt/we ird$PATH/agent's-bin/workspace-agentd`

	if err := writeRedactWrapper(dir, hostile); err != nil {
		t.Fatalf("writeRedactWrapper() error = %v", err)
	}

	data, err := os.ReadFile(filepath.Join(dir, "redact"))
	if err != nil {
		t.Fatalf("failed to read wrapper: %v", err)
	}
	content := string(data)
	line := ""
	for _, l := range strings.Split(content, "\n") {
		if strings.HasPrefix(l, "exec ") {
			line = l
		}
	}
	if line == "" {
		t.Fatalf("no exec line in wrapper: %q", content)
	}
	quoted := shellSingleQuote(hostile)
	if !strings.Contains(line, " "+quoted+" ") {
		t.Errorf("exec line does not carry the safely-quoted path %q: %q", quoted, line)
	}
}

func TestWriteRedactWrapper_IdempotentNoTempResidue(t *testing.T) {
	dir := t.TempDir()

	for i := 0; i < 2; i++ {
		if err := writeRedactWrapper(dir, "/usr/local/bin/workspace-agentd"); err != nil {
			t.Fatalf("writeRedactWrapper() call %d error = %v", i+1, err)
		}
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir() error = %v", err)
	}
	if len(entries) != 1 {
		t.Errorf("expected exactly the redact wrapper in dir, got %d entries: %v", len(entries), entries)
	}
}

func TestWriteRedactWrapper_CreatesMissingParentDirs(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "sandbox-runtime", "bin")

	if err := writeRedactWrapper(dir, "/usr/local/bin/workspace-agentd"); err != nil {
		t.Fatalf("writeRedactWrapper() error = %v", err)
	}

	if _, err := os.Stat(filepath.Join(dir, "redact")); err != nil {
		t.Fatalf("wrapper not created in nested dir: %v", err)
	}
}

func TestEnsureRedactWrapper_BestEffortOnUnwritableDir(t *testing.T) {
	t.Setenv("LLMSAFESPACES_REDACT_WRAPPER_PATH", filepath.Join(t.TempDir(), "no-such-parent", "bin", "redact"))
	// The parent-of-parent exists but is a file: MkdirAll fails.
	root := t.TempDir()
	blocker := filepath.Join(root, "no-such-parent")
	if err := os.WriteFile(blocker, []byte("x"), 0o644); err != nil {
		t.Fatalf("failed to create blocker file: %v", err)
	}
	t.Setenv("LLMSAFESPACES_REDACT_WRAPPER_PATH", filepath.Join(blocker, "bin", "redact"))

	// Must not panic and must not exit the process — the wrapper is UX
	// preservation (design 0053 S2); a failed write degrades to the
	// documented Warn, boot continues (ensureMiseShims precedent).
	ensureRedactWrapper(zap.NewNop())
}

func TestRedactWrapperPath_EnvOverride(t *testing.T) {
	t.Setenv("LLMSAFESPACES_REDACT_WRAPPER_PATH", "")
	if got := redactWrapperPath(); got != "/sandbox-runtime/bin/redact" {
		t.Errorf("default redactWrapperPath() = %q, want /sandbox-runtime/bin/redact", got)
	}

	t.Setenv("LLMSAFESPACES_REDACT_WRAPPER_PATH", "/tmp/custom/redact")
	if got := redactWrapperPath(); got != "/tmp/custom/redact" {
		t.Errorf("overridden redactWrapperPath() = %q, want /tmp/custom/redact", got)
	}
}

func TestPrependPathEnv_PrependsToExistingPath(t *testing.T) {
	env := []string{"HOME=/home/sandbox", "PATH=/usr/local/bin:/usr/bin:/bin"}

	got := prependPathEnv(env, "/sandbox-runtime/bin")

	if len(got) != 2 {
		t.Fatalf("env entry count changed: %v", got)
	}
	if got[1] != "PATH=/sandbox-runtime/bin:/usr/local/bin:/usr/bin:/bin" {
		t.Errorf("PATH = %q, want /sandbox-runtime/bin prepended", got[1])
	}
	if got[0] != "HOME=/home/sandbox" {
		t.Errorf("unrelated entry mutated: %q", got[0])
	}
	if env[1] != "PATH=/usr/local/bin:/usr/bin:/bin" {
		t.Errorf("input slice mutated: %q", env[1])
	}
}

func TestPrependPathEnv_AddsPathWhenAbsent(t *testing.T) {
	env := []string{"HOME=/home/sandbox"}

	got := prependPathEnv(env, "/sandbox-runtime/bin")

	found := false
	for _, e := range got {
		if e == "PATH=/sandbox-runtime/bin" {
			found = true
		}
	}
	if !found {
		t.Errorf("PATH not added: %v", got)
	}
}

func TestPrependPathEnv_NoOpWhenAlreadyFirst(t *testing.T) {
	env := []string{"PATH=/sandbox-runtime/bin:/usr/bin:/bin"}

	got := prependPathEnv(env, "/sandbox-runtime/bin")

	if got[0] != "PATH=/sandbox-runtime/bin:/usr/bin:/bin" {
		t.Errorf("already-first PATH mutated: %q", got[0])
	}
}

func TestDefaultOpencodeCmdFactory_PathIncludesWrapperDir(t *testing.T) {
	t.Setenv("LLMSAFESPACES_REDACT_WRAPPER_PATH", "/sandbox-runtime/bin/redact")
	t.Setenv("PATH", "/usr/local/bin:/usr/bin:/bin")

	cmd := defaultOpencodeCmdFactory()

	found := false
	for _, e := range cmd.Env {
		if strings.HasPrefix(e, "PATH=") {
			found = true
			if !strings.HasPrefix(e, "PATH=/sandbox-runtime/bin:") {
				t.Errorf("opencode child PATH does not start with wrapper dir: %q", e)
			}
		}
	}
	if !found {
		t.Error("opencode child env has no PATH entry")
	}
}
