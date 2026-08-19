// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package repolint

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestAgentImportCheck_RealRepo_PassesWithKnownLeaks(t *testing.T) {
	root := repoRoot(t)
	rep, err := AgentImportCheck(root)
	if err != nil {
		t.Fatalf("AgentImportCheck: %v", err)
	}
	if !rep.OK() {
		t.Fatalf("real repo has new agent-import leaks beyond the documented knownLeaks:\n%s",
			rep.String())
	}
}

func TestAgentImportCheck_FlaggedNewViolation(t *testing.T) {
	root := t.TempDir()
	newFile := filepath.Join(root, "api", "internal", "handlers", "evil.go")
	if err := os.MkdirAll(filepath.Dir(newFile), 0o755); err != nil {
		t.Fatal(err)
	}
	body := "package handlers\n\n" +
		"import (\n" +
		"\t\"github.com/lenaxia/llmsafespaces/pkg/agent/opencode\"\n" +
		")\n\n" +
		"var _ = opencode.Register\n"
	if err := os.WriteFile(newFile, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	rep, err := AgentImportCheck(root)
	if err != nil {
		t.Fatalf("AgentImportCheck: %v", err)
	}
	if rep.OK() {
		t.Fatalf("expected the new violation to be flagged; got OK")
	}
	found := false
	for _, v := range rep.Violations {
		if v.File == "api/internal/handlers/evil.go" {
			found = true
		}
	}
	if !found {
		t.Fatalf("evil.go not in violations: %v", rep.Violations)
	}
}

func TestAgentImportCheck_AllowedDirectoriesPass(t *testing.T) {
	root := t.TempDir()
	for _, allowed := range []string{
		filepath.Join(root, "api", "internal", "app", "wiring.go"),
		filepath.Join(root, "cmd", "workspace-agentd", "main.go"),
		filepath.Join(root, "cmd", "controller", "main.go"),
	} {
		if err := os.MkdirAll(filepath.Dir(allowed), 0o755); err != nil {
			t.Fatal(err)
		}
		body := "package " + filepath.Base(filepath.Dir(allowed)) + "\n\n" +
			"import (\n" +
			"\t\"github.com/lenaxia/llmsafespaces/pkg/agent/opencode\"\n" +
			")\n\n" +
			"var _ = opencode.Register\n"
		if err := os.WriteFile(allowed, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	rep, err := AgentImportCheck(root)
	if err != nil {
		t.Fatalf("AgentImportCheck: %v", err)
	}
	if !rep.OK() {
		t.Fatalf("allowed construction/wiring sites must not be flagged:\n%s",
			rep.String())
	}
}

func TestAgentImportCheck_TestFilesAreExcluded(t *testing.T) {
	root := t.TempDir()
	testFile := filepath.Join(root, "api", "internal", "handlers", "evil_test.go")
	if err := os.MkdirAll(filepath.Dir(testFile), 0o755); err != nil {
		t.Fatal(err)
	}
	body := "package handlers\n\n" +
		"import (\n" +
		"\t\"github.com/lenaxia/llmsafespaces/pkg/agent/opencode\"\n" +
		")\n\n" +
		"var _ = opencode.Register\n"
	if err := os.WriteFile(testFile, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	rep, err := AgentImportCheck(root)
	if err != nil {
		t.Fatalf("AgentImportCheck: %v", err)
	}
	if !rep.OK() {
		t.Fatalf("test files must not be flagged by the boundary rule:\n%s",
			rep.String())
	}
}

// TestKnownLeaksStillMatchReality verifies that the dated allowlist still
// describes the real repository. If a leak is fixed (its retiring story
// landed), the entry MUST be removed from agentImportKnownLeaks at the same
// time — otherwise the list rots into permanent forgiveness. This test
// fails closed in both directions: missing entry → real leak flagged; stale
// entry → orphan detected.
func TestKnownLeaksStillMatchReality(t *testing.T) {
	root := repoRoot(t)
	// Walk all non-test .go files and record which import the forbidden
	// package and are NOT in the allowed prefixes.
	actualLeaks := map[string]bool{}
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		if filepath.Ext(path) != ".go" {
			return nil
		}
		if isExcludedPath(root, path) {
			return nil
		}
		if isTestFile(filepath.ToSlash(path)) {
			return nil
		}
		imports, ierr := importsOf(path)
		if ierr != nil {
			return nil
		}
		if !containsString(imports, AgentImportForbiddenPath) {
			return nil
		}
		rel, rerr := filepath.Rel(root, path)
		if rerr != nil {
			return nil
		}
		norm := filepath.ToSlash(rel)
		if anyPrefix(norm, agentImportAllowedPrefixes) {
			return nil
		}
		actualLeaks[norm] = true
		return nil
	})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}

	for leaked := range actualLeaks {
		if _, ok := agentImportKnownLeaks[leaked]; !ok {
			t.Errorf("REAL LEAK not in knownLeaks: %s — add it with a retiring-story citation or fix the import", leaked)
		}
	}
	for listed := range agentImportKnownLeaks {
		if !actualLeaks[listed] {
			t.Errorf("STALE knownLeaks entry: %s no longer imports the forbidden package — remove it (the retiring story landed)", listed)
		}
	}
}

func isTestFile(path string) bool {
	return strings.HasSuffix(path, "_test.go")
}

// TestAgentImportCheck_SubPackageEscapeCaught pins the prefix-match
// hardening from #947 review: an exact-match implementation let platform
// code import pkg/agent/opencode/wire (a subpackage) untouched.
func TestAgentImportCheck_SubPackageEscapeCaught(t *testing.T) {
	dir := t.TempDir()
	platform := filepath.Join(dir, "api", "internal", "services", "sse")
	if err := os.MkdirAll(platform, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(platform, "tracker.go"), []byte(
		"package sse\n\nimport (\n\t\"github.com/lenaxia/llmsafespaces/pkg/agent/opencode/wire\"\n)\n\nvar _ = wire.ParseStepUsage\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	rep, err := AgentImportCheck(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Violations) != 1 {
		t.Fatalf("subpackage import must be flagged; got %d violations", len(rep.Violations))
	}
}

// TestAgentImportCheck_SeamSelfImportAllowed: files inside the seam may
// import their own subpackages (opencode/adapter.go -> opencode/wire).
func TestAgentImportCheck_SeamSelfImportAllowed(t *testing.T) {
	dir := t.TempDir()
	seam := filepath.Join(dir, "pkg", "agent", "opencode")
	if err := os.MkdirAll(seam, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(seam, "adapter.go"), []byte(
		"package opencode\n\nimport (\n\t\"github.com/lenaxia/llmsafespaces/pkg/agent/opencode/wire\"\n)\n\nvar _ = wire.ParseStepUsage\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	rep, err := AgentImportCheck(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Violations) != 0 {
		t.Fatalf("seam self-import must be allowed; got %d violations", len(rep.Violations))
	}
}
