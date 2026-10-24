// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package repolint_test

// The committed stub-execution leg of the #1918 contract: structural
// pins and `bash -n` cannot catch a respelled jq filter or permission
// literal inside the npm auth preflight — a typo there false-positives
// the preflight on every release (converting a working token into a
// permanently red SDK leg). This file executes the REAL preflight run
// block from release.yml against stubbed npm/jq: the stub npm pins the
// exact `access ls-packages --json` invocation, the stub jq accepts
// ONLY the canonical filter spelling, and every behavior branch
// (auth-fail / invisible / read-only / ok / garbage) must exit and
// message exactly as the contract requires.

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestReleaseWorkflow_NpmPreflightScriptBehavior: executes the REAL
// preflight run block from release.yml against stubbed `npm`/`jq` —
// the committed version of the branch-logic validation the #1918
// review demanded. A respelled jq filter or permission literal in the
// workflow would sail through the structural pins and `bash -n` yet
// false-positive the preflight on every release; here the stub jq
// accepts ONLY the canonical filter spelling, and every behavior
// branch (auth-fail / invisible / read-only / ok / garbage) must exit
// and message exactly as the contract requires.
func TestReleaseWorkflow_NpmPreflightScriptBehavior(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not on PATH; skipping preflight behavior check")
	}
	wf := parseReleaseWorkflow(t)
	job, ok := wf.Jobs["publish-typescript-sdk"]
	if !ok {
		t.Fatalf("release.yml has no publish-typescript-sdk job — renamed? extend the pin, do not skip it")
	}
	script := ""
	for _, s := range job.Steps {
		if strings.Contains(s.Name, "Preflight npm auth") {
			script = s.Run
		}
	}
	if script == "" {
		t.Fatalf("publish-typescript-sdk has no 'Preflight npm auth' step — the next dead token fails as an inscrutable E404 again (#1918)")
	}

	const canonicalFilter = `."@llmsafespaces/sdk" // "missing"`

	stubNpm := `#!/usr/bin/env bash
# Test stub: pins the invocation shape, plays the registry per $PREFLIGHT_CASE.
set -u
[ "$1 $2 $3" = "access ls-packages --json" ] || { echo "stub npm: unexpected argv: $*" >&2; exit 64; }
case "${PREFLIGHT_CASE:-}" in
  auth-fail)  echo "stub npm: E404 registry says no" >&2; exit 1 ;;
  invisible)  echo '{}' ;;
  read-only)  echo '{"@llmsafespaces/sdk":"read-only"}' ;;
  ok)         echo '{"@llmsafespaces/sdk":"read-write"}' ;;
  garbage)    echo 'not-json-at-all' ;;
  *)          echo "stub npm: unknown PREFLIGHT_CASE '$PREFLIGHT_CASE'" >&2; exit 64 ;;
esac
`
	stubJq := `#!/usr/bin/env bash
# Test stub: accepts ONLY the canonical #1918 filter; single-line JSON object in, value or "missing" out.
set -euo pipefail
[ "$#" -eq 2 ] || { echo "stub jq: want exactly 2 args" >&2; exit 3; }
[ "$1" = "-r" ] || { echo "stub jq: want -r" >&2; exit 3; }
[ "$2" = '` + canonicalFilter + `' ] || { echo "stub jq: filter respelled from canonical: $2" >&2; exit 3; }
input=$(cat)
printf '%s' "$input" | grep -Eq '^[[:space:]]*\{.*\}[[:space:]]*$' || { echo "stub jq: not a JSON object: $input" >&2; exit 2; }
if printf '%s' "$input" | grep -Eq '"@llmsafespaces/sdk"[[:space:]]*:[[:space:]]*"[^"]*"'; then
  printf '%s' "$input" | sed -E 's/.*"@llmsafespaces\/sdk"[[:space:]]*:[[:space:]]*"([^"]*)".*/\1/'
else
  echo missing
fi
`

	dir := t.TempDir()
	binDir := filepath.Join(dir, "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "sdks", "typescript"), 0o755); err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string]string{"npm": stubNpm, "jq": stubJq} {
		p := filepath.Join(binDir, name)
		if err := os.WriteFile(p, []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
	}

	cases := []struct {
		name       string
		caseEnv    string
		wantExit   int // 0 or non-zero sentinel -1
		wantOut    []string
		notWantOut []string
	}{
		{name: "auth-fail", caseEnv: "auth-fail", wantExit: 1, wantOut: []string{"NPM_TOKEN", "E404 registry says no"}},
		{name: "package invisible (mis-scoped token)", caseEnv: "invisible", wantExit: 1, wantOut: []string{"mis-scoped", "@llmsafespaces/sdk"}},
		{name: "read-only token", caseEnv: "read-only", wantExit: 1, wantOut: []string{"read-only", "read-write"}},
		{name: "ok", caseEnv: "ok", wantExit: 0, wantOut: []string{"preflight OK"}, notWantOut: []string{"::error::"}},
		{name: "garbage registry output fails closed", caseEnv: "garbage", wantExit: -1, notWantOut: []string{"preflight OK"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cmd := exec.Command("bash", "-c", script)
			cmd.Dir = dir
			cmd.Env = append(os.Environ(), "PATH="+binDir+string(os.PathListSeparator)+os.Getenv("PATH"), "PREFLIGHT_CASE="+tc.caseEnv)
			out, err := cmd.CombinedOutput()
			got := string(out)
			exit := 0
			if ee, ok := err.(*exec.ExitError); ok {
				exit = ee.ExitCode()
			} else if err != nil {
				t.Fatalf("bash failed to run: %v", err)
			}
			if tc.wantExit == 0 && exit != 0 {
				t.Errorf("case %s: exit %d, want 0\noutput:\n%s", tc.caseEnv, exit, got)
			}
			if tc.wantExit != 0 && exit == 0 {
				t.Errorf("case %s: exit 0, want non-zero\noutput:\n%s", tc.caseEnv, got)
			}
			for _, w := range tc.wantOut {
				if !strings.Contains(got, w) {
					t.Errorf("case %s: output missing %q\noutput:\n%s", tc.caseEnv, w, got)
				}
			}
			for _, w := range tc.notWantOut {
				if strings.Contains(got, w) {
					t.Errorf("case %s: output must not contain %q\noutput:\n%s", tc.caseEnv, w, got)
				}
			}
		})
	}
}
