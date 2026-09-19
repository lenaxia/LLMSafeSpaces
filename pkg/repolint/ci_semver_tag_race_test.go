// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package repolint_test

// The 2026-09-19 v0.34.5 release incident (ops-prod #2539): ci.yml and
// release.yml BOTH trigger on `push: tags: v*.*.*`, and ci.yml's
// manifest-merge jobs emitted the SAME semver tags (0.34.5/0.34/0) and
// `latest` — so every release raced an unsigned dev rebuild against the
// Release workflow's canonical, cosign-attested index push. A tag-HEAD
// fetch minutes after a release could resolve to the wrong artifact
// (exactly what happened at 09:39Z).
//
// Structural fix: release.yml is the ONLY writer of semver + latest
// tags. ci.yml's merge jobs push sha-<commit>, ts-<timestamp>, and
// dev-on-main only — unique-per-run tags that can never masquerade as
// a release pin. These pins hold the invariant: reverting the fix
// fails them; deleting release.yml's semver emission (leaving NOBODY
// publishing release tags) also fails them.

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// metadataTagBlocks extracts every `tags: |` multiline block body from
// a workflow file (the indented lines following the key).
func metadataTagBlocks(t *testing.T, src string) []string {
	t.Helper()
	var blocks []string
	lines := strings.Split(src, "\n")
	for i, line := range lines {
		if strings.TrimSpace(line) != "tags: |" {
			continue
		}
		var body []string
		for j := i + 1; j < len(lines) && strings.HasPrefix(lines[j], "            "); j++ {
			body = append(body, lines[j])
		}
		if len(body) > 0 {
			blocks = append(blocks, strings.Join(body, "\n"))
		}
	}
	return blocks
}

const (
	ciPath      = ".github/workflows/ci.yml"
	releasePath = ".github/workflows/release.yml"
)

func readWorkflow(t *testing.T, rel string) string {
	t.Helper()
	// repoRoot walk-up (same pattern as binary_integration_test.go).
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			break
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("repo root not found")
		}
		dir = parent
	}
	data, err := os.ReadFile(filepath.Join(dir, rel))
	if err != nil {
		t.Fatalf("%s: %v", rel, err)
	}
	return string(data)
}

// TestCIMergeJobs_NeverEmitSemverOrLatest: the race class is closed at
// the source — ci.yml must not emit any tag a release consumer could
// pin by version.
func TestCIMergeJobs_NeverEmitSemverOrLatest(t *testing.T) {
	ci := readWorkflow(t, ciPath)
	if strings.Contains(ci, "type=semver") {
		t.Errorf("ci.yml emits type=semver tags: the Release workflow is the ONLY semver writer — a semver line here re-opens the v0.34.5 tag race (ops-prod #2539)")
	}
	if strings.Contains(ci, "value=latest") {
		t.Errorf("ci.yml emits a latest tag: latest is release.yml-owned — a ci.yml latest line re-opens the tag race (ops-prod #2539)")
	}
}

// TestCIMergeJobs_StillPushTraceableTags: the fix must not over-delete.
// Every ci.yml metadata block still pushes the unique-per-run tags
// (sha-<commit> for provenance rollback, ts-<timestamp> for newest-wins
// pinning) and the dev channel on main runs.
func TestCIMergeJobs_StillPushTraceableTags(t *testing.T) {
	ci := readWorkflow(t, ciPath)

	blocks := metadataTagBlocks(t, ci)
	if len(blocks) < 7 {
		t.Fatalf("expected ≥7 metadata tag blocks in ci.yml (one per image), found %d — the merge jobs moved?", len(blocks))
	}
	for i, body := range blocks {
		if !strings.Contains(body, "type=sha,prefix=sha-") {
			t.Errorf("ci.yml metadata block %d lost its sha tag (provenance rollback pin)", i+1)
		}
		if !strings.Contains(body, "type=raw,value=ts-") {
			t.Errorf("ci.yml metadata block %d lost its ts- tag (newest-wins pin)", i+1)
		}
	}
	if !strings.Contains(ci, "type=raw,value=dev,enable={{is_default_branch}}") {
		t.Error("ci.yml lost its dev-on-main channel — dev is the integration channel for unreleased main")
	}
}

// activeLines counts ACTIVE (non-comment) lines matching the pattern —
// a Contains check is comment-blind: commenting out every emission
// line would pass it while releases silently lose their pins (r1
// review finding, demonstrated by mutation).
func activeLines(src string, pattern string) int {
	re := regexp.MustCompile(`(?m)^\s*` + pattern + `\s*$`)
	n := 0
	for _, line := range strings.Split(src, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "#") {
			continue
		}
		if re.MatchString(line) {
			n++
		}
	}
	return n
}

// TestReleaseWorkflow_RemainsTheSemverWriter: the invariant has two
// sides. If release.yml ever drops its semver emission (including by
// COMMENTING the lines out — comment-blind pins were an r1 finding),
// NOBODY publishes version tags — the pin fails loudly instead of
// releases silently losing their pins.
func TestReleaseWorkflow_RemainsTheSemverWriter(t *testing.T) {
	rel := readWorkflow(t, releasePath)
	if n := activeLines(rel, `type=semver,pattern=\{\{version\}\}`); n < 7 {
		t.Errorf("release.yml has only %d ACTIVE type=semver,pattern={{version}} lines (want ≥7, one per image) — releases would publish NO version tag for some images; the race fix must keep release.yml as the sole (and active) semver writer", n)
	}
	if n := activeLines(rel, `type=raw,value=latest(,enable=.+)?$`); n < 7 {
		t.Errorf("release.yml has only %d ACTIVE latest lines (want ≥7) — the race fix must keep release.yml as the sole (and active) latest writer", n)
	}
}

// TestCIWorkflow_NoTagTrigger: ci.yml must not trigger on tag pushes at
// all — the Release workflow publishes every tag a released commit
// needs (semver, latest, sha-, ts-), so a CI tag run would
// deterministically collide with the cosign-attested pushes on every
// shared tag (ops-prod #2539's r1 residual: the sha-/ts- collision is
// deterministic on tag events).
func TestCIWorkflow_NoTagTrigger(t *testing.T) {
	ci := readWorkflow(t, ciPath)
	for _, line := range strings.Split(ci, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "#") {
			continue
		}
		if strings.Contains(trimmed, "tags:") && strings.Contains(trimmed, "v*") {
			t.Errorf("ci.yml triggers on tag pushes (%q) — release.yml publishes ALL tags for released commits (semver, latest, sha-, ts-); a CI tag run collides deterministically. Drop the tag trigger (ops-prod #2539).", trimmed)
		}
	}
}
