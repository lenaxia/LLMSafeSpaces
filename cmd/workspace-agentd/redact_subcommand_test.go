// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// withStdin replaces os.Stdin with content for the duration of fn.
func withStdin(t *testing.T, content string, fn func()) {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	if _, err := io.WriteString(w, content); err != nil {
		t.Fatalf("write stdin pipe: %v", err)
	}
	_ = w.Close()

	old := os.Stdin
	os.Stdin = r
	t.Cleanup(func() { os.Stdin = old; _ = r.Close() })

	fn()
}

// captureStdout replaces os.Stdout with a pipe for the duration of fn and
// returns everything written.
func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	old := os.Stdout
	os.Stdout = w

	done := make(chan string, 1)
	go func() {
		data, _ := io.ReadAll(r)
		done <- string(data)
	}()

	fn()

	os.Stdout = old
	_ = w.Close()
	return <-done
}

func TestRunRedactCommand_BuiltInRulesWithoutConfig(t *testing.T) {
	// The default config path does not exist in the test environment —
	// NewRedactorFromFile must degrade to the 16 built-in rules.
	input := "Authorization: Bearer sk-ant-api03-abcdef123456\n"

	var rc int
	var out string
	withStdin(t, input, func() {
		out = captureStdout(t, func() {
			rc = runRedactCommand([]string{"-config", filepath.Join(t.TempDir(), "absent.json")})
		})
	})

	if rc != 0 {
		t.Errorf("runRedactCommand() rc = %d, want 0", rc)
	}
	if strings.Contains(out, "sk-ant-api03-abcdef123456") {
		t.Errorf("secret leaked through redact: %q", out)
	}
	if !strings.Contains(out, "[REDACTED]") {
		t.Errorf("expected redaction marker in output: %q", out)
	}
}

func TestRunRedactCommand_CustomPatternFromConfig(t *testing.T) {
	dir := t.TempDir()
	config := filepath.Join(dir, "patterns.json")
	payload := `[{"regex":"hunter2","replacement":"[REDACTED]"}]`
	if err := os.WriteFile(config, []byte(payload), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	var rc int
	var out string
	withStdin(t, "password=hunter2\n", func() {
		out = captureStdout(t, func() {
			rc = runRedactCommand([]string{"-config", config})
		})
	})

	if rc != 0 {
		t.Errorf("runRedactCommand() rc = %d, want 0", rc)
	}
	if strings.Contains(out, "hunter2") {
		t.Errorf("custom-pattern secret leaked: %q", out)
	}
	if !strings.Contains(out, "[REDACTED]") {
		t.Errorf("expected redaction marker in output: %q", out)
	}
}

func TestRunRedactCommand_MalformedConfigFails(t *testing.T) {
	dir := t.TempDir()
	config := filepath.Join(dir, "patterns.json")
	if err := os.WriteFile(config, []byte("not json"), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	var rc int
	withStdin(t, "x\n", func() {
		_ = captureStdout(t, func() {
			rc = runRedactCommand([]string{"-config", config})
		})
	})

	if rc != 1 {
		t.Errorf("runRedactCommand() malformed config rc = %d, want 1", rc)
	}
}

func TestRunRedactCommand_InvalidRegexFails(t *testing.T) {
	dir := t.TempDir()
	config := filepath.Join(dir, "patterns.json")
	payload := `[{"regex":"([unclosed","replacement":"x"}]`
	if err := os.WriteFile(config, []byte(payload), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	var rc int
	withStdin(t, "x\n", func() {
		_ = captureStdout(t, func() {
			rc = runRedactCommand([]string{"-config", config})
		})
	})

	if rc != 1 {
		t.Errorf("runRedactCommand() invalid regex rc = %d, want 1", rc)
	}
}

func TestRunRedactCommand_PassthroughOfCleanText(t *testing.T) {
	input := "the quick brown fox jumps over the lazy dog\n"

	var rc int
	var out string
	withStdin(t, input, func() {
		out = captureStdout(t, func() {
			rc = runRedactCommand([]string{"-config", filepath.Join(t.TempDir(), "absent.json")})
		})
	})

	if rc != 0 {
		t.Errorf("runRedactCommand() rc = %d, want 0", rc)
	}
	if out != input {
		t.Errorf("clean text not passed through byte-identical: got %q want %q", out, input)
	}
}

func TestRunRedactCommand_DefaultConfigPath(t *testing.T) {
	// No -config flag: the flag default is /sandbox-cfg/redact-patterns.json,
	// which does not exist here — built-in rules apply, exit 0.
	var rc int
	withStdin(t, fmt.Sprintf("token=abc\n"), func() {
		_ = captureStdout(t, func() {
			rc = runRedactCommand(nil)
		})
	})

	if rc != 0 {
		t.Errorf("runRedactCommand(nil) rc = %d, want 0 (built-in rules)", rc)
	}
}
