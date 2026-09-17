package local

import (
	"os"
	"strings"
	"testing"
)

// TestRuntimeDockerfiles_ReleaseFetchesRetry: every curl release-asset
// fetch in the runtime images must carry bounded retry semantics — the
// delivery pool died on a transient GitHub 500 serving mise (2026-09-16,
// run 35159837345); curl --fail alone is fatal on a single 5xx, and the
// same CI step builds BOTH images (opencode's tarball fetch is the same
// failure class one file over).
func TestRuntimeDockerfiles_ReleaseFetchesRetry(t *testing.T) {
	totalHits := 0
	for _, f := range []string{"../runtimes/base/Dockerfile", "../runtimes/opencode/Dockerfile"} {
		raw, err := os.ReadFile(f)
		if err != nil {
			t.Skipf("dockerfile not present in this checkout: %v", err)
		}
		lines := strings.Split(string(raw), "\n")
		hits := 0
		for i, ln := range lines {
			if !strings.Contains(ln, "curl --fail") {
				continue
			}
			window := ln
			if i+1 < len(lines) {
				window += "\n" + lines[i+1]
			}
			if !strings.Contains(window, "--retry 5") {
				t.Fatalf("%s:%d: fetch lacks the numeric --retry 5 (a bare --retry substring lets retries silently vanish): %s", f, i+1, ln)
			}
			if !strings.Contains(window, "--retry-all-errors") {
				t.Fatalf("%s:%d: fetch lacks --retry-all-errors (5xx and the release-flip 404 window are not retried): %s", f, i+1, ln)
			}
			hits++
		}
		if hits == 0 {
			t.Fatalf("%s: no curl --fail fetches found — pin is vacuous, update it", f)
		}
		totalHits += hits
	}
	// Floor: base carries 5 fetches, opencode 1 — a fetch added without
	// retries still trips above; silent shrinkage below six trips here.
	if totalHits < 6 {
		t.Fatalf("expected at least 6 pinned fetch sites across the runtime images, found %d", totalHits)
	}
}
