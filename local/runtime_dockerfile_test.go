package local

import (
	"os"
	"strings"
	"testing"
)

// TestRuntimeBaseDockerfile_ReleaseFetchesRetry: every curl release-asset
// fetch in runtimes/base/Dockerfile must carry retry semantics — the
// delivery pool died on a transient GitHub 500 serving mise (2026-09-16,
// run 35159837345); curl --fail alone is fatal on a single 5xx.
func TestRuntimeBaseDockerfile_ReleaseFetchesRetry(t *testing.T) {
	raw, err := os.ReadFile("../runtimes/base/Dockerfile")
	if err != nil {
		t.Skipf("dockerfile not present in this checkout: %v", err)
	}
	src := string(raw)
	lines := strings.Split(src, "\n")
	hits := 0
	for i, ln := range lines {
		if !strings.Contains(ln, "curl --fail") {
			continue
		}
		// The flags may ride the same line or the continuation; the
		// fetch spans to the URL line — check the two-line window.
		window := ln
		if i+1 < len(lines) {
			window += "\n" + lines[i+1]
		}
		if !strings.Contains(window, "--retry") || !strings.Contains(window, "--retry-all-errors") {
			t.Fatalf("line %d: curl --fail fetch lacks --retry/--retry-all-errors (transient release-asset 5xx is fatal): %s", i+1, ln)
		}
		hits++
	}
	if hits == 0 {
		t.Fatal("no curl --fail fetches found — pin is vacuous, update it")
	}
}
