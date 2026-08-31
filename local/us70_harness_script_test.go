// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package local_test

// us70_harness_script_test.go — pin tests for the US-70.0 delivery-harness
// scripts (local/lib/us70-common.sh, local/lib/gvisor.sh,
// local/us-70-faults-e2e.sh, local/us-70-secret-delivery-e2e.sh) and the
// pool workflow, same philosophy as s5_kind_script_test.go: the scripts
// are CI glue on a real kind cluster; what is pinnable deterministically
// is the structure past failures actually broke — bash syntax, the runsc
// checksum guard, the fault-seam names, and the presence of the 401 /
// key-corruption assertions so rows cannot be silently dropped.

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

const (
	us70CommonScript   = "lib/us70-common.sh"
	us70GvisorScript   = "lib/gvisor.sh"
	us70FaultsScript   = "us-70-faults-e2e.sh"
	us70DeliveryScript = "us-70-secret-delivery-e2e.sh"
)

var us70PoolWorkflow = filepath.Join("..", ".github", "workflows", "us-70-delivery-pool.yml")

func mustRead(t *testing.T, path string) string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(raw)
}

func TestUS70Scripts_BashSyntax(t *testing.T) {
	bash := requireBash(t)
	for _, script := range []string{us70CommonScript, us70GvisorScript, us70FaultsScript, us70DeliveryScript} {
		t.Run(script, func(t *testing.T) {
			out, err := exec.Command(bash, "-n", script).CombinedOutput()
			if err != nil {
				t.Fatalf("bash -n failed: %s", out)
			}
		})
	}
}

// extractUS70Guard pulls the checksum-validation regex from lib/gvisor.sh.
// The guard is re-exposed as a function of $EXPECTED so the test executes
// the script's OWN regex, not a copy (the run-8 incident class, PR #1178).
func extractUS70Guard(t *testing.T) string {
	t.Helper()
	src := mustRead(t, us70GvisorScript)
	const marker = `[[ "$EXPECTED" =~ ^[0-9a-f]{128}$ ]]`
	if !strings.Contains(src, marker) {
		t.Fatalf("checksum guard not found in %s — it must keep the regex form", us70GvisorScript)
	}
	return marker
}

func TestUS70GvisorGuard_AcceptsValidChecksum(t *testing.T) {
	bash := requireBash(t)
	guard := extractUS70Guard(t)

	const realChecksumFile = "84936438d583ec976800f464e75a83e1515f0890b451b9b4db219c4472b54ca9b106a6772ee683f1e64cce2128871d7637b14d800591f8451b8137f6c39fb2ef  runsc"

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "runsc.sha512"), []byte(realChecksumFile+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	script := "cd " + dir + "\nEXPECTED=$(cut -d\" \" -f1 runsc.sha512)\n" + guard + " || exit 1\nexit 0\n"
	out, err := exec.Command(bash, "-c", script).CombinedOutput()
	if err != nil {
		t.Fatalf("guard REJECTED a valid 128-hex checksum: %s", out)
	}
}

func TestUS70GvisorGuard_RejectsMalformed(t *testing.T) {
	bash := requireBash(t)
	guard := extractUS70Guard(t)

	cases := map[string]string{
		"empty":            "",
		"short-but-hex":    strings.Repeat("a", 66),
		"too-long-hex":     strings.Repeat("a", 129),
		"non-hex-128chars": strings.Repeat("z", 128),
	}
	for name, expected := range cases {
		t.Run(name, func(t *testing.T) {
			script := "EXPECTED=" + shQuote(expected) + "\n" + guard + " && exit 1\nexit 0\n"
			out, err := exec.Command(bash, "-c", script).CombinedOutput()
			if err != nil {
				t.Fatalf("harness failed: %s", out)
			}
		})
	}
}

func TestUS70FaultsScript_FaultSeamNamesInLockstep(t *testing.T) {
	src := mustRead(t, us70FaultsScript)
	if !strings.Contains(src, "LLMSAFESPACES_FAULT_INJECTION") {
		t.Fatalf("env var name LLMSAFESPACES_FAULT_INJECTION missing from %s — it must stay in lockstep with the API seam", us70FaultsScript)
	}
	wf := mustRead(t, us70PoolWorkflow)
	if !strings.Contains(wf, "set env deployment/llmsafespaces-api LLMSAFESPACES_FAULT_INJECTION") {
		t.Fatalf("pool workflow must arm the seam via `kubectl set env deployment/llmsafespaces-api LLMSAFESPACES_FAULT_INJECTION` (post-delivery arming step)")
	}
	if strings.Contains(wf, "api.e2eFaultInjection") {
		t.Fatalf("pool workflow must NOT set api.e2eFaultInjection at helm install — the delivery suite would exhaust the fault budget before the faults suite runs")
	}

	// The rule count must come from ONE number: the workflow's FAULT_COUNT
	// env literal must equal the faults script's FAULT_COUNT default.
	wfCount := extractSingle(t, mustRead(t, us70PoolWorkflow),
		regexp.MustCompile(`(?m)^\s*FAULT_COUNT:\s*"?(\d+)"?\s*$`), "workflow FAULT_COUNT env literal")
	scriptCount := extractSingle(t, src,
		regexp.MustCompile(`FAULT_COUNT="\$\{FAULT_COUNT:-(\d+)\}"`), "faults script FAULT_COUNT default")
	if wfCount != scriptCount {
		t.Fatalf("fault count drift: workflow FAULT_COUNT=%s but faults script default FAULT_COUNT=%s — both must come from the same number", wfCount, scriptCount)
	}
}

// extractUS70Count is shared count-literal extraction for the lockstep test.
func extractSingle(t *testing.T, src string, re *regexp.Regexp, what string) string {
	t.Helper()
	m := re.FindStringSubmatch(src)
	if m == nil {
		t.Fatalf("%s not found — the pin cannot be verified", what)
	}
	if _, err := strconv.Atoi(m[1]); err != nil {
		t.Fatalf("%s is not numeric: %q", what, m[1])
	}
	return m[1]
}

func TestUS70PoolWorkflow_Pins(t *testing.T) {
	src := mustRead(t, us70PoolWorkflow)
	for _, pin := range []string{
		"set env deployment/llmsafespaces-api LLMSAFESPACES_FAULT_INJECTION",
		"timeout-minutes: 300",
		"SUSPEND_SECONDS: 3600",
		"local/lib/gvisor.sh",
	} {
		if !strings.Contains(src, pin) {
			t.Fatalf("pool workflow must contain %q (found missing)", pin)
		}
	}
	if strings.Contains(src, "api.e2eFaultInjection") {
		t.Fatalf("pool workflow must not arm the fault seam at helm install (api.e2eFaultInjection) — the delivery suite stays seam-inert")
	}
}

func TestUS70Scripts_SourceCommonLib(t *testing.T) {
	for _, script := range []string{us70FaultsScript, us70DeliveryScript} {
		t.Run(script, func(t *testing.T) {
			src := mustRead(t, script)
			if !strings.Contains(src, "lib/us70-common.sh") {
				t.Fatalf("%s must source lib/us70-common.sh (shared harness helpers)", script)
			}
		})
	}
}

func TestUS70FaultsScript_401AndCorruptionRowsPresent(t *testing.T) {
	src := mustRead(t, us70FaultsScript)
	if !strings.Contains(src, `"%{http_code}"`) && !strings.Contains(src, "%{http_code}") {
		t.Fatalf("faults script must assert the SA-token 401 via %%{http_code}")
	}
	if strings.Count(src, "401") < 1 {
		t.Fatalf("faults script must contain the 401 assertion")
	}
	if strings.Count(src, "UPDATE user_keys") < 2 {
		t.Fatalf("faults script must carry the key-row corruption UPDATE and its restore pair (found %d)", strings.Count(src, "UPDATE user_keys"))
	}
	if !strings.Contains(src, `decode('00','hex')`) {
		t.Fatalf("faults script must corrupt wrapped_dek via decode('00','hex')")
	}
}
