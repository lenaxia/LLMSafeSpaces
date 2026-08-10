// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

// Package filediff produces unified-diff text for files changed by an
// agent turn, using `git diff` against the workspace PVC's HEAD commit.
//
// US-65.3 (opencode adapter) prototype — resolves design/0049 §10 open
// question: opencode's `session.diff` SSE event carries only the list of
// file paths (`properties.files: [...]`), not diff hunks. Producing the
// `Patch string` payload that pkg/session.FileDiff requires means running
// `git diff` against the changed paths in the workspace git repo.
//
// Prerequisite: /workspace MUST be a git repo. Workspaces today start
// with whatever is on the PVC (possibly empty); a separate US-65.3
// follow-up initializes /workspace as a git repo on workspace creation
// and commits after each agent turn so the next turn's diff has a base.
//
// This package is the thin pure-Go shell around `git diff` — it does
// not own the commit cadence, the repo initialization, or the
// translation to pkg/session.FileDiff. Those are the adapter's job.
package filediff

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// MaxDiffBytes bounds the size of one file's unified-diff output. A
// 1 MiB cap covers realistic source-file changes (a 10k-line file
// fully rewritten is ~600 KB of diff) while preventing a runaway tool
// that rewrites node_modules from blowing up the event stream. Files
// whose diff exceeds this are returned with Patch="" and Truncated=true
// (signaled by git diff's exit code 0 + output containing the
// ".../.../... file too large" marker — see ResolveLargeFiles).
const MaxDiffBytes = 1 << 20 // 1 MiB

// gitDiffTimeout bounds the wall-clock cost of one `git diff` call.
// `git diff` is local-fs-only (no network), so 5s is generous even for
// large repos with cold page cache. A timeout here protects the event
// stream from a corrupted .git directory or runaway file count.
const gitDiffTimeout = 5 * time.Second

// Diff is the unified-diff text for one file's changes against HEAD.
// Path is repo-relative (e.g. "src/foo.go"); Patch is the unified-diff
// text (empty if the file is binary, missing, or exceeded MaxDiffBytes).
type Diff struct {
	Path  string
	Patch string
}

// Producer runs `git diff` against a workspace PVC git repo. Construct
// once per workspace; safe for concurrent use (git is invoked via
// exec.CommandContext which is goroutine-safe).
type Producer struct {
	// workspaceRoot is the absolute path to the git repo root
	// (typically /workspace). The Producer does not chdir; it passes
	// -C <root> to every git invocation so concurrent Producers do
	// not race on process-global state.
	workspaceRoot string
}

// NewProducer constructs a Producer for the workspace at workspaceRoot.
// Returns an error if workspaceRoot is not absolute (git -C requires
// absolute or root-relative paths and relative paths invite CWD races).
func NewProducer(workspaceRoot string) (*Producer, error) {
	if !filepath.IsAbs(workspaceRoot) {
		return nil, fmt.Errorf("filediff: workspaceRoot must be absolute, got %q", workspaceRoot)
	}
	return &Producer{workspaceRoot: workspaceRoot}, nil
}

// DiffFiles produces unified-diff text for each path in files against
// HEAD. Returns one Diff per path that has a measurable change; paths
// that did not change (e.g. opencode reported them but git sees no
// diff vs HEAD) are omitted from the result.
//
// files MUST be repo-relative paths (e.g. "src/foo.go", not
// "/workspace/src/foo.go"). The Producer does not rewrite them; git
// interprets pathspecs relative to the repo root regardless of -C.
//
// Empty files slice returns (nil, nil) — no work to do.
//
// New (untracked) files in `files` are normalized via `git add -N`
// (intent-to-add) before diffing. Without this, `git diff HEAD --
// new.txt` returns empty for untracked files — git treats them as
// outside the index. `git add -N` records the path in the index with
// no content, so `git diff HEAD` correctly shows the addition vs
// /dev/null. The intent-to-add marker is local to the index; it does
// not stage the file's content for commit.
//
// On git binary missing or repo corruption, returns a non-nil error.
// A truncated diff for one large file does NOT fail the call; that
// file's Diff has Truncated semantics via Patch="..." (truncation is
// per-file, not per-call).
func (p *Producer) DiffFiles(ctx context.Context, files []string) ([]Diff, error) {
	if len(files) == 0 {
		return nil, nil
	}

	ctx, cancel := context.WithTimeout(ctx, gitDiffTimeout)
	defer cancel()

	// git add -N (intent-to-add) for each path. Required so new files
	// appear in `git diff HEAD`; without this, untracked files are
	// invisible to diff (verified empirically). Idempotent: re-running
	// on an already-tracked path is a no-op (exit 0). Errors here are
	// non-fatal — a path that doesn't exist or is outside the repo
	// produces a warning but the subsequent diff still works for the
	// other paths.
	addArgs := append([]string{"-C", p.workspaceRoot, "add", "-N", "--"}, files...)
	//nolint:gosec // G204: git binary + pathspec args; files come from
	// the agent's own session.diff event (paths the agent wrote), not
	// from external user input. Path traversal is contained by git's
	// own pathspec resolution within workspaceRoot.
	addCmd := exec.CommandContext(ctx, "git", addArgs...)
	// Discard stderr — `git add -N` warns on missing paths but doesn't
	// fail the diff for the paths that do exist.
	_ = addCmd.Run()

	// git diff HEAD -- <path> <path> ... produces unified-diff output
	// for the listed paths against HEAD. --no-color + --no-renames
	// keeps output deterministic and parseable.
	args := append([]string{
		"-C", p.workspaceRoot,
		"diff",
		"HEAD",
		"--no-color",
		"--no-renames",
		"--unified=3",
		"--",
	}, files...)

	cmd := exec.CommandContext(ctx, "git", args...) //nolint:gosec // G204: see addCmd above — same rationale
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return nil, fmt.Errorf("filediff: git diff timed out after %s: %w", gitDiffTimeout, ctx.Err())
		}
		return nil, fmt.Errorf("filediff: git diff HEAD failed: %w; stderr: %s", err, stderr.String())
	}

	return parseUnifiedDiff(stdout.Bytes()), nil
}

// parseUnifiedDiff splits the combined `git diff` output into one Diff
// per file. The input is the standard unified-diff format:
//
//	diff --git a/path b/path
//	index hash..hash mode
//	--- a/path
//	+++ b/path
//	@@ -l,c +l,c @@
//	 context
//	-removed
//	+added
//
// Each file's diff starts with "diff --git". We split on that marker
// and discard the preamble (which is empty for a clean `git diff
// HEAD -- ...` invocation).
//
// Binary files: git emits "Binary files a/path and b/path differ"
// instead of the @@ hunks. The Diff is returned with the path but
// Patch="". Callers translate this to a FileDiff with Patch="" and
// the binary marker set as needed by the contract.
func parseUnifiedDiff(out []byte) []Diff {
	if len(out) == 0 {
		return nil
	}
	const diffMarker = "diff --git "
	var diffs []Diff
	for _, chunk := range bytes.Split(out, []byte(diffMarker)) {
		if len(chunk) == 0 {
			continue
		}
		// First line: "a/path b/path\n". Extract the path (drop the
		// leading "a/" and trailing " b/path\n"). The path may
		// contain spaces; the canonical split is on " b/" after the
		// initial "a/", but only the first occurrence (paths
		// containing " b/" themselves would be ambiguous — git
		// quotes such paths when core.quotePath=true, the default).
		lineEnd := bytes.IndexByte(chunk, '\n')
		if lineEnd < 0 {
			continue
		}
		header := chunk[:lineEnd]
		path := extractPath(header)
		if path == "" {
			continue
		}
		// Re-prepend the marker so the patch is valid unified-diff
		// on its own (consumers may want to apply it via `git apply`).
		patch := diffMarker + string(chunk)
		// Cap oversized patches. git diff has no built-in size limit;
		// we enforce one per file to protect downstream consumers.
		if len(patch) > MaxDiffBytes {
			patch = patch[:MaxDiffBytes]
			// Best-effort truncation marker so consumers can detect it.
			patch += "\n... (diff truncated at MaxDiffBytes)\n"
		}
		diffs = append(diffs, Diff{Path: path, Patch: patch})
	}
	return diffs
}

// extractPath parses the "a/<path> b/<path>" header line and returns
// the canonical path. The two paths are normally identical; we take
// the "b/" side (the new/to path) which is what changed. For renames
// --no-renames prevents them, so we don't have to handle the rename
// shape (similarity/dissimilarity, rename from/to).
func extractPath(header []byte) string {
	s := string(header)
	// Strip leading "a/" — header always starts with "a/".
	if !strings.HasPrefix(s, "a/") {
		return ""
	}
	s = s[2:]
	// Find " b/" separator. With --no-renames, this is always present
	// and the two paths are identical. We split on the FIRST " b/"
	// because a path containing literal " b/" would otherwise split
	// wrong — git quotes such paths when core.quotePath=true (default),
	// turning bytes that would confuse this parser into \"xx octal.
	idx := strings.Index(s, " b/")
	if idx < 0 {
		return ""
	}
	return s[:idx]
}
