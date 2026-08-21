// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package repolint

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeTempReleaseWorkflow(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, ".github", "workflows", "release.yml")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

const minimalReleaseWF = `
env:
  REGISTRY: ghcr.io
  API_IMAGE: lenaxia/llmsafespaces/api
  AGENTD_IMAGE: lenaxia/llmsafespaces/agentd

jobs:
  merge-api:
    runs-on: ubuntu-latest
  merge-agentd:
    runs-on: ubuntu-latest
  sign-images:
    needs: [merge-api, merge-agentd]
    runs-on: ubuntu-latest
    steps:
      - run: |
          for img in api agentd; do
            case "$img" in
              api) image_ref="${REGISTRY}/${API_IMAGE}:${VERSION}" ;;
              agentd) image_ref="${REGISTRY}/${AGENTD_IMAGE}:${VERSION}" ;;
            esac
            cosign sign --yes "${image_ref}"
          done
  scan-images:
    needs: [merge-api, merge-agentd]
    runs-on: ubuntu-latest
    steps:
      - run: |
          for img in api agentd; do
            true
          done
  generate-sbom:
    needs: [merge-api, merge-agentd]
    runs-on: ubuntu-latest
    steps:
      - run: |
          for img in api agentd; do
            true
          done
  create-release:
    needs: [merge-api, merge-agentd]
    runs-on: ubuntu-latest
    steps:
      - run: |
          cat > release-body.md <<EOF
          | API | ghcr.io/lenaxia/llmsafespaces/api:${VERSION} |
          | Agentd | ghcr.io/lenaxia/llmsafespaces/agentd:${VERSION} |
          EOF
`

// RED-state fixture: exactly the v0.19.1 incident shape — agentd missing
// from the scan loop, the SBOM loop, the release table, and the needs
// chains.
const v0191IncidentWF = `
env:
  REGISTRY: ghcr.io
  API_IMAGE: lenaxia/llmsafespaces/api
  AGENTD_IMAGE: lenaxia/llmsafespaces/agentd

jobs:
  merge-api:
    runs-on: ubuntu-latest
  merge-agentd:
    runs-on: ubuntu-latest
  sign-images:
    needs: [merge-api, merge-agentd]
    steps:
      - run: |
          for img in api agentd; do
            true
          done
  scan-images:
    needs: [merge-api]
    steps:
      - run: |
          for img in api; do
            true
          done
  generate-sbom:
    needs: [merge-api]
    steps:
      - run: |
          for img in api; do
            true
          done
  create-release:
    needs: [merge-api]
    steps:
      - run: |
          cat > release-body.md <<EOF
          | API | ghcr.io/lenaxia/llmsafespaces/api:${VERSION} |
          EOF
`

func TestReleaseArtifactCompleteness_CleanWorkflow(t *testing.T) {
	dir := writeTempReleaseWorkflow(t, minimalReleaseWF)
	if fails := RunReleaseArtifactCompleteness(dir); len(fails) != 0 {
		t.Fatalf("expected clean, got: %s", strings.Join(fails, "; "))
	}
}

func TestReleaseArtifactCompleteness_V0191IncidentShapeIsRed(t *testing.T) {
	// The exact defect class that shipped v0.19.1 without agentd must be
	// caught — multiple distinct failure lines.
	dir := writeTempReleaseWorkflow(t, v0191IncidentWF)
	fails := RunReleaseArtifactCompleteness(dir)
	if len(fails) < 4 {
		t.Fatalf("expected >=4 failures (scan loop, sbom loop, table, needs), got %d: %v", len(fails), fails)
	}
	joined := strings.Join(fails, "\n")
	for _, want := range []string{
		"scan-images does not process agentd",
		"generate-sbom does not process agentd",
		"create-release image table missing lenaxia/llmsafespaces/agentd",
		"create-release does not need merge-agentd",
		"scan-images does not need merge-agentd",
		"generate-sbom does not need merge-agentd",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("missing expected failure %q in:\n%s", want, joined)
		}
	}
}

func TestReleaseArtifactCompleteness_ActualRepoWorkflow(t *testing.T) {
	// The repo's real release.yml must pass its own invariant (this is
	// what repolint enforces on every commit; the test pins it for CI).
	root, err := resolveRepoRootForTest()
	if err != nil {
		t.Skipf("cannot locate repo root: %v", err)
	}
	if fails := RunReleaseArtifactCompleteness(root); len(fails) != 0 {
		t.Fatalf("repo release.yml violates artifact invariants: %s", strings.Join(fails, "; "))
	}
}

func resolveRepoRootForTest() (string, error) {
	// cwd is pkg/repolint → root is two levels up; verify by workflow file.
	dir, err := os.Getwd()
	if err != nil {
		return "", err
	}
	root := filepath.Dir(filepath.Dir(dir))
	if _, err := os.Stat(filepath.Join(root, ".github", "workflows", "release.yml")); err != nil {
		return "", err
	}
	return root, nil
}
