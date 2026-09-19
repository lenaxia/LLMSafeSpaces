// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package local_test

// Shared machinery for the harness-execution smokes (the class of
// runtime defect source-text needles cannot catch — #1474 rounds 4-6):
// deterministic curl/kubectl/sleep shims and a runner asserting the
// script traverses to its verdict gate without runtime-abort signatures
// and without leaking the one-time webhook secret.

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

const smokeCurlShim = `#!/usr/bin/env bash
# Deterministic response surface: -o/-w honored like real curl.
method="GET"; wfmt=""; path=""; outfile=""
prev=""
for a in "$@"; do
  case "${prev}" in
    -X) method="${a}" ;;
    -w) wfmt="${a}" ;;
    -o) outfile="${a}" ;;
  esac
  case "${a}" in http://*) path="${a}" ;; esac
  prev="${a}"
done
body='{"id":"smoke","trigger":{"id":"smoke"},"name":"x","enabled":false,"nextFireAt":"2026-10-01T03:00:00Z","consecutiveFailures":1,"fires":[],"runs":[]}'
code=200
case "${path}" in
  */livez) body="ok" ;;
  */hooks/*) code=202 ;;
  */runs) [[ "${method}" == "POST" ]] && code=202 ;;
  */rotate-secret) body='{"webhookSecret":"whsec_smoke","webhookUrl":"/api/v1/hooks/smoke"}' ;;
  */auth/login) body='{"token":"smoke-token"}' ;;
  */auth/me) body='{"user":{"id":"smoke-owner"}}' ;;
  *)
    [[ "${method}" == "POST" ]] && code=201
    ;;
esac
if [[ -n "${outfile}" ]]; then
  printf '%s' "${body}" > "${outfile}"
fi
if [[ -n "${wfmt}" ]]; then
  # The harness passes -w '\n%{http_code}' as literal backslash-n (real
  # curl expands it); match the literal, emit a real newline.
  if [[ "${wfmt}" == '\n%{http_code}' ]]; then
    [[ -z "${outfile}" ]] && printf '%s\n' "${body}"
    printf '%s' "${code}"
  elif [[ "${wfmt}" == *'%{http_code}'* ]]; then
    printf '%s' "${code}"
  else
    [[ -z "${outfile}" ]] && printf '%s' "${body}"
  fi
else
  [[ -z "${outfile}" ]] && printf '%s' "${body}"
fi
exit 0
`

const smokeKubectlShim = `#!/usr/bin/env bash
# Satisfies the harness's kubectl surface: phase polls answer
# SMOKE_PHASE_ANSWER, the postgres-pod lookup answers a name, secret
# reads answer base64("smoke-pwd"), the workspace pod resolves, the
# in-pod registry exec answers 1417's hardcoded mock admission, and
# the rest no-op. (Auth shapes pinned by us70-common's seed_session,
# registry_admits, pod_of.)
for a in "$@"; do
  case "${a}" in
    jsonpath='{.status.phase}'*) printf '%s\n' "${SMOKE_PHASE_ANSWER:-Active}"; exit 0 ;;
    jsonpath='{.status.podName}'*) printf 'pod-smoke-0\n'; exit 0 ;;
    jsonpath='{.items[0].metadata.name}'*) printf 'postgres-smoke-0\n'; exit 0 ;;
    # secret reads: base64("smoke-pwd") — empty output would leave PG_PWD
    # unset (base64 -d of "" succeeds, so the || default never fires).
    jsonpath='{.data.'*) printf 'c21va2UtcHdk\n'; exit 0 ;;
  esac
done
for a in "$@"; do
  # seed_session's psql exec: rc-0 silence (its INSERT checks rc only).
  [[ "${a}" == "psql" ]] && exit 0
done
for a in "$@"; do
  # registry_admits' in-pod exec: 1417's hardcoded mock slug/model pair.
  if [[ "${a}" == "exec" ]]; then
    printf '{"data":[{"providerID":"mock1417","id":"mock-1417-model"}]}'
    exit 0
  fi
done
exit 0
`

// writeSmokeShims materializes the curl/kubectl/sleep shims into dir;
// phaseAnswer feeds wait_phase (Active vs Ready per script).
func writeSmokeShims(t *testing.T, dir, phaseAnswer string) {
	t.Helper()
	shims := map[string]string{
		"curl":    smokeCurlShim,
		"kubectl": smokeKubectlShim,
		"sleep":   "#!/usr/bin/env bash\nexit 0\n",
	}
	for name, body := range shims {
		require.NoError(t, os.WriteFile(filepath.Join(dir, name), []byte(body), 0o755))
	}
	_ = phaseAnswer // via env below
}

// runScriptUnderShims executes one harness script with the shims on
// PATH (fast wait budgets, phase answer) and returns the combined
// output plus the exit value. It does NOT assert — callers assert
// their script's own gate markers.
func runScriptUnderShims(t *testing.T, script, phaseAnswer string, extraEnv map[string]string) (string, int) {
	t.Helper()
	shimDir := t.TempDir()
	writeSmokeShims(t, shimDir, phaseAnswer)

	cmd := exec.Command("bash", script)
	env := append(os.Environ(),
		"PATH="+shimDir+string(os.PathListSeparator)+os.Getenv("PATH"),
		"SMOKE_PHASE_ANSWER="+phaseAnswer,
		"FIRE_WAIT_S=1", "RUN_WAIT_S=1", "R4_WAIT_S=1",
	)
	for k, v := range extraEnv {
		env = append(env, k+"="+v)
	}
	cmd.Env = env
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	done := make(chan struct{})
	var exitVal int
	if err := cmd.Start(); err != nil {
		t.Fatalf("start %s: %v", script, err)
	}
	go func() {
		err := cmd.Wait()
		if err != nil {
			if ee, ok := err.(*exec.ExitError); ok {
				exitVal = ee.ExitCode()
			} else {
				exitVal = -1
			}
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(180 * time.Second):
		_ = cmd.Process.Kill()
		t.Fatalf("%s did not terminate within 180s (hung poll loop?)", script)
	}
	return out.String(), exitVal
}

// assertSmokeTraversal is the shared verdict: the script reached ITS
// gate (fail marker with non-zero exit, or green marker with zero),
// with no runtime-abort signatures and no webhook-secret leakage.
func assertSmokeTraversal(t *testing.T, script, combined string, exitVal int, failMarker, greenMarker string) {
	t.Helper()
	failed := strings.Contains(combined, failMarker) && exitVal != 0
	green := strings.Contains(combined, greenMarker) && exitVal == 0
	if !failed && !green {
		t.Fatalf("%s never reached its verdict gate (exit=%d):\n%s", script, exitVal, smokeTail(combined))
	}
	for _, banned := range []string{"unbound variable", "command not found", "permission denied", "substitution"} {
		if strings.Contains(combined, banned) {
			t.Fatalf("%s: runtime abort signature %q found:\n%s", script, banned, smokeTail(combined))
		}
	}
	// Credential hygiene (#1474 r5 finding 3's class, pinned): the
	// one-time webhook secret never reaches the log.
	if strings.Contains(combined, "whsec_") {
		t.Fatalf("%s: webhook secret material leaked into script output:\n%s", script, smokeTail(combined))
	}
}

func smokeTail(s string) string {
	lines := strings.Split(s, "\n")
	if len(lines) > 25 {
		lines = lines[len(lines)-25:]
	}
	return strings.Join(lines, "\n")
}
