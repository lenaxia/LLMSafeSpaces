// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package local

// Pins for local/us-72-relay-only-flip-drill.sh — US-72.5's scripted
// rollback drill (design 0058 §D6.1: flip → validate → rollback →
// validate → flip). The drill runs on the harness/pool cluster (first
// recorded execution rides the reviewer's runner or the #1456 wiring
// lane — the established disposition); these pins hold its shape (the
// 1505/1507 pin precedents).

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

const us72FlipDrill = "us-72-relay-only-flip-drill.sh"

func mustReadUS72Flip(t *testing.T) string {
	t.Helper()
	data, err := os.ReadFile(us72FlipDrill)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func TestUS72FlipDrill_BashSyntax(t *testing.T) {
	bash := requireBashLocal(t)
	out, err := exec.Command(bash, "-n", us72FlipDrill).CombinedOutput()
	if err != nil {
		t.Fatalf("bash -n failed: %s", out)
	}
}

// The D6.1 loop's four legs in order: flip on → token-only canary →
// rollback (with the positive control) → flip on again. A drill missing
// any leg — or running them out of order — does not exercise the
// rollback.
func TestUS72FlipDrill_FourLegsInOrder(t *testing.T) {
	src := mustReadUS72Flip(t)
	legs := []string{
		`R1 — flip ON`,
		`R2 — token-only canary`,
		`R3 — rollback`,
		`R4 — flip ON again`,
	}
	last := -1
	for _, leg := range legs {
		i := strings.Index(src, leg)
		if i < 0 {
			t.Errorf("drill must contain leg %q", leg)
			continue
		}
		if i <= last {
			t.Errorf("leg %q out of order (offset %d after %d)", leg, i, last)
		}
		last = i
	}
}

// The canary sweep is the drill's security verdict: it must grep EVERY
// uid-1000-readable surface (the design 0058 §1.2 inventory) for the
// planted bytes, and the rollback leg must assert the canary RETURNS
// (the positive control that proves the sweep can fail).
func TestUS72FlipDrill_CanarySweepAndPositiveControl(t *testing.T) {
	src := mustReadUS72Flip(t)
	for _, marker := range []string{
		`/sandbox-runtime/agent-config.json`,
		`/agentd-config/agent-config.json`,
		`/workspace/.local/opencode/auth.json`,
		`/sandbox-cfg/secrets.json`,
		`/sandbox-runtime/rt/secrets.json`,
		`/sandbox-runtime/rt/auth.json`,
		`/proc/self/environ`,
		`CANARY`,
		`apiKey is the token`,
		`baseURL points at the relay router`,
		`rollback restores the raw-key path`,
		`CredentialsStaged`,
	} {
		if !strings.Contains(src, marker) {
			t.Errorf("drill must exercise %q — the flip drill's verdict without it is decorative", marker)
		}
	}
}

// The flips must REUSE the standing install's values — a bare upgrade
// would reset the harness's pinned images and break the cluster (the
// drill mutates a standing release, it does not install a fresh one).
func TestUS72FlipDrill_ReusesStandingValues(t *testing.T) {
	src := mustReadUS72Flip(t)
	if !strings.Contains(src, "--reuse-values") {
		t.Error("every helm upgrade in the drill must --reuse-values — otherwise the harness install's pinned values are reset mid-drill")
	}
}

// flip() must re-establish the API port-forward after the rollouts: the
// api pod template is gated on the flag, so EVERY flip rolls the
// deployment and severs the forward (kubectl binds to one pod and never
// re-resolves — the r3 fatal finding). Without this call every API
// request after the first flip dies with HTTP 000 and R3/R4 — the
// rollback positive control — are unreachable.
func TestUS72FlipDrill_FlipRestoresPortForward(t *testing.T) {
	src := mustReadUS72Flip(t)
	flipStart := strings.Index(src, "flip() {")
	if flipStart < 0 {
		t.Fatal("flip() not found")
	}
	flipEnd := strings.Index(src[flipStart:], "\n}")
	flipBody := src[flipStart : flipEnd+flipStart]
	if !strings.Contains(flipBody, "api_portforward_restart") {
		t.Error("flip() must call api_portforward_restart after the rollouts — every flip severs the harness forward (the api template is flag-gated)")
	}
}

// Per-script workspace isolation (the #1342 note).
func TestUS72FlipDrill_WorkspaceIsolation(t *testing.T) {
	src := mustReadUS72Flip(t)
	if !strings.Contains(src, `WS_BASE="e2e72500-`) {
		t.Error("drill must set its own WS_BASE unconditionally (per-script isolation)")
	}
}

// The sweep predicate is rc-sensitive (grep -c prints 0 and exits 1 on
// ZERO matches — the healthy case; a naive rc check counts clean files
// as hits, the r1 class (e)). This pin executes the sweep's counting
// branch against a mock directory: clean file -> 0 contribution, dirty
// file -> +1, unreadable file -> pass-for-that-path.
func TestUS72FlipDrill_SweepPredicateMockTable(t *testing.T) {
	dir := t.TempDir()
	clean := filepath.Join(dir, "clean.json")
	dirty := filepath.Join(dir, "dirty.json")
	locked := filepath.Join(dir, "locked.json")
	if err := os.WriteFile(clean, []byte("clean content"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dirty, []byte("apiKey sk-US72-CANARY-0wiggle8harbor"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(locked, []byte("clean"), 0o000); err != nil {
		t.Fatal(err)
	}
	script := "set -euo pipefail\n" +
		"CANARY_KEY=sk-US72-CANARY-0wiggle8harbor\n" +
		"hits=0\n" +
		"for p in " + clean + " " + dirty + " " + locked + "; do\n" +
		"  out=$(grep -ac \"${CANARY_KEY}\" \"$p\" 2>/dev/null || true)\n" +
		"  [[ \"${out}\" =~ ^[0-9]+$ ]] && hits=$((hits + out))\n" +
		"done\n" +
		"echo $hits\n"
	out, err := exec.Command("bash", "-c", script).CombinedOutput()
	if err != nil {
		t.Fatalf("sweep counting run failed: %v: %s", err, out)
	}
	if got := strings.TrimSpace(string(out)); got != "1" {
		t.Errorf("sweep hits = %q, want 1 (clean=0 contribution, dirty=+1, unreadable=pass) — the predicate drifted from the drill's", got)
	}
	// The drill's actual loop must be byte-equivalent in its counting
	// branch (the pin above is worthless if the drill diverges).
	src := mustReadUS72Flip(t)
	for _, marker := range []string{
		`out=$(grep -ac "'"${CANARY_KEY}"'" "$p" 2>/dev/null || true)`,
		`[[ "${out}" =~ ^[0-9]+$ ]] && hits=$((hits + out))`,
	} {
		if !strings.Contains(src, marker) {
			t.Errorf("drill's sweep must contain the pinned counting branch %q", marker)
		}
	}
}

// The drill's jq extraction must select the drill provider's options BY
// KEY — the provider section is keyed by slug, and .provider[][] would
// be a jq type error (the second [] iterates strings — r1 class (d)).
func TestUS72FlipDrill_JqPathShape(t *testing.T) {
	if _, err := exec.LookPath("jq"); err != nil {
		t.Skip("jq not on PATH")
	}
	src := mustReadUS72Flip(t)
	if !strings.Contains(src, `.provider["us72drill"].options[$f] // ""`) {
		t.Fatal(`drill must extract via .provider["us72drill"].options[$f] (keyed, not double-iterated)`)
	}
	const cfg = `{"provider":{"us72drill":{"options":{"apiKey":"tok-123","baseURL":"http://llm-relay-router.llm-relay.svc"}}}}`
	out, err := exec.Command("bash", "-c",
		"echo '"+cfg+"' | jq -r '.provider[\"us72drill\"].options.apiKey // \"\"'").CombinedOutput()
	if err != nil {
		t.Fatalf("jq extraction failed: %v: %s", err, out)
	}
	if got := strings.TrimSpace(string(out)); got != "tok-123" {
		t.Errorf("jq apiKey extraction = %q, want tok-123", got)
	}
}

// The helpers' optional-argument handling is set -u sensitive: a
// 2-argument call to a function whose $3 has no default kills the whole
// script before any curl spawns (the r2 finding — the bind call was
// exactly that shape). This pin extracts BOTH helper prologues from the
// drill and executes them under `set -u` with 2-arg + 3-arg shapes
// against a stub curl.
func TestUS72FlipDrill_HelpersTolerateOptionalBody(t *testing.T) {
	src := mustReadUS72Flip(t)
	for _, fn := range []string{"api()", "api_authed()"} {
		i := strings.Index(src, "\n"+fn+" {")
		if i < 0 {
			t.Fatalf("helper %s not found in the drill", fn)
		}
		end := strings.Index(src[i:], "\n}\n")
		body := src[i+1 : i+end+2]
		// Neutralize the network + die for the probe: a stub curl that
		// always succeeds with 200, and mktemp/die shims.
		shim := "set -u\ndie() { echo \"DIED: $*\"; exit 9; }\n" +
			"mktemp() { echo /tmp/stub; }\n" +
			"curl() { echo 200; }\n" +
			"API_KEY=k; AUTH_TOKEN=t; PORTFWD_PORT=1\n" +
			body + "\n" +
			"api POST /x >/dev/null 2>&1 || api_authed POST /x >/dev/null 2>&1\n" +
			"echo OK-2ARG\n"
		out, err := exec.Command("bash", "-c", shim).CombinedOutput()
		if err != nil {
			t.Errorf("helper %s under set -u with a 2-arg call: %v: %s (the ${3:-} default is missing)", fn, err, out)
		}
		if !strings.Contains(string(out), "OK-2ARG") {
			t.Errorf("helper %s did not survive the 2-arg call: %s", fn, out)
		}
	}
}
