// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package local_test

// us68_attachments_script_test.go — pin tests for the Epic 68 attachment
// e2e script (local/us-68-attachments-e2e.sh) and the nightly workflow's
// F8 step, same philosophy as us70_harness_script_test.go: the artifacts
// are CI glue on a real kind cluster; what is pinnable deterministically
// is the structure past failures actually broke — here, the two #1456
// defects:
//
//   - the sidecar gate probed only `.spec.containers[*].name`, but since
//     #980 the agentd sidecar is a NATIVE sidecar (init container,
//     restartPolicy Always — controller/internal/workspace/agentd_sidecar.go),
//     so sidecar-mode nightly pods were misread as single-container and
//     row E2 died at the sidecar's read-only /workspace upload path;
//   - the nightly's F8 step discovered the valkey Service via metadata
//     labels (`get svc -l app=valkey`), but the Service in
//     local/postgres-redis.yaml carries NO metadata labels (app: valkey
//     lives on the Deployment and in the Service's selector) — F8
//     silently SKIPped on every run in history.
//
// The behavior pins EXECUTE the script's / step's real code with a fake
// kubectl/kc (never a re-implementation — the r2/r4 lesson from the
// us-70 pins).

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

const us68AttachmentsScript = "us-68-attachments-e2e.sh"

var us68NightlyWorkflow = filepath.Join("..", ".github", "workflows", "e2e-nightly.yml")

func TestUS68AttachmentsScript_BashSyntax(t *testing.T) {
	bash := requireBash(t)
	out, err := exec.Command(bash, "-n", us68AttachmentsScript).CombinedOutput()
	if err != nil {
		t.Fatalf("bash -n failed: %s", out)
	}
}

// TestUS68SidecarGate_ProbesInitContainers structurally pins the gate's
// jsonpath: it must cover BOTH container lists. A containers-only probe
// can never see the #980 native sidecar (an init container named agentd)
// — that is the #1456 misdetection.
func TestUS68SidecarGate_ProbesInitContainers(t *testing.T) {
	src := mustRead(t, us68AttachmentsScript)
	const probe = `CONTAINER_NAMES=$(kc -n "${NS}" get pod "${POD_A}" -o jsonpath='{.spec.containers[*].name} {.spec.initContainers[*].name}')`
	if !strings.Contains(src, probe) {
		t.Fatalf("sidecar gate must probe BOTH container lists:\n  %s\n— a containers-only probe cannot see the native sidecar (init container, #980) and misreads sidecar pods as single-container (#1456)", probe)
	}
	// The containers-only probe form must be gone entirely.
	if strings.Contains(src, `get pod "${POD_A}" -o jsonpath='{.spec.containers[*].name}'`) {
		t.Fatal("sidecar gate must not keep a containers-only jsonpath probe — the agentd sidecar is an init container (#1456)")
	}
	if !strings.Contains(src, `if [[ "${CONTAINER_NAMES}" == *"agentd"* ]]`) {
		t.Fatal("sidecar gate decision must match on the combined container-name list")
	}
}

// TestUS68SidecarGate_DetectsNativeSidecar executes the script's ACTUAL
// gate block with a fake kc across the feature-detect's three legs:
// pre-0060 (5xx → loud skip + exit 0), 0060-landed (201 → falls through
// to E2/E10/E11), and broken (500 → hard die). Single-container pods
// still fall through. Run 35697148238's adjudication: the gate
// distinguishes designed outcomes from broken ones, never absorbing
// the latter.
func TestUS68SidecarGate_DetectsNativeSidecar(t *testing.T) {
	bash := requireBash(t)
	src := mustRead(t, us68AttachmentsScript)
	gate := regexp.MustCompile(`(?s)(?m)CONTAINER_NAMES=\$\(kc.*?ok "sidecar mode \+ design 0060 uploads[^\n]*\nfi\n`).FindString(src)
	if gate == "" {
		t.Fatal("sidecar gate block not found in us-68-attachments-e2e.sh — did the gate change shape?")
	}

	run := func(containers, uploadStatuses, wsFiles string) (string, error) {
		script := `set -u
NS=ns; POD_A=pod-a; WS_A=ws-a; KEY_A=key
kc() { printf '%s\n' '` + containers + `'; }
warn() { printf 'WARN %s\n' "$*"; }
log()  { printf 'LOG %s\n' "$*"; }
ok()   { printf 'OK %s\n' "$*"; }
die()  { printf 'DIE %s\n' "$*" >&2; exit 1; }
_UPSEQ="` + uploadStatuses + `"
_UPN=0
upload_do() {
	_UPN=$((_UPN + 1))
	UPLOAD_STATUS=$(printf '%s' "${_UPSEQ}" | cut -d'|' -f${_UPN})
	[ -z "${UPLOAD_STATUS}" ] && UPLOAD_STATUS=$(printf '%s' "${_UPSEQ}" | cut -d'|' -f$((${_UPN} - 1)))
	BODY=stub-body
}
exec_ws() { printf '%s\n' '` + wsFiles + `'; }
sleep() { :; }
` + gate + `
echo GATE-FELL-THROUGH`
		out, err := exec.Command(bash, "-c", script).CombinedOutput()
		return string(out), err
	}

	t.Run("pre-0060 sidecar (503) -> loud skip, exit 0", func(t *testing.T) {
		out, err := run("workspace platform-init platform-dirs workspace-setup credential-setup agentd", "503", "")
		if err != nil {
			t.Fatalf("pre-0060 sidecar path must exit 0 (the nightly proceeds past the skip), got: %v\n%s", err, out)
		}
		for _, want := range []string{
			"agentd SIDECAR mode detected",
			"upload rejected cleanly with 503 — pre-0060 sidecar",
			"SKIPPED E2/E10/E11",
		} {
			if !strings.Contains(out, want) {
				t.Fatalf("pre-0060 sidecar path must print %q, got: %q", want, out)
			}
		}
		if strings.Contains(out, "GATE-FELL-THROUGH") {
			t.Fatalf("pre-0060 sidecar mode must NOT fall through to the rows: %q", out)
		}
	})

	t.Run("0060-landed sidecar (201) -> falls through to the rows", func(t *testing.T) {
		out, err := run("workspace platform-init platform-dirs workspace-setup credential-setup agentd", "201", "")
		if err != nil {
			t.Fatalf("0060-landed sidecar must fall through to E2/E10/E11, got: %v\n%s", err, out)
		}
		if !strings.Contains(out, "design 0060 stage-and-signal landed") {
			t.Fatalf("the 201 leg must name design 0060, got: %q", out)
		}
		if !strings.Contains(out, "GATE-FELL-THROUGH") {
			t.Fatalf("the 201 leg MUST fall through to the rows (not silently skip) — an exit-0 regression is the silent-skip class: %q", out)
		}
		if strings.Contains(out, "single-container mode confirmed") {
			t.Fatalf("the 201 fall-through must NOT print the false single-container verdict: %q", out)
		}
		if !strings.Contains(out, "sidecar mode + design 0060 uploads") {
			t.Fatalf("the 201 path must print the accurate sidecar verdict: %q", out)
		}
	})

	t.Run("502 persistent (retries to same) -> loud skip", func(t *testing.T) {
		out, err := run("workspace agentd", "502|502", "")
		if err != nil {
			t.Fatalf("persistent 502 must be the designed pre-0060 clean-fail, got: %v\n%s", err, out)
		}
		if !strings.Contains(out, "upload rejected cleanly with 502") {
			t.Fatalf("502 leg must print the clean-fail verdict, got: %q", out)
		}
		if strings.Contains(out, "GATE-FELL-THROUGH") {
			t.Fatalf("persistent 502 must skip, not fall through: %q", out)
		}
	})

	t.Run("502 transient (retries to 201) -> falls through", func(t *testing.T) {
		out, err := run("workspace agentd", "502|201", "")
		if err != nil {
			t.Fatalf("a transient 502 blip that clears to 201 must fall through, got: %v\n%s", err, out)
		}
		if !strings.Contains(out, "GATE-FELL-THROUGH") {
			t.Fatalf("the retry-to-success path must reach the rows, got: %q", out)
		}
		if !strings.Contains(out, "design 0060 stage-and-signal landed") {
			t.Fatalf("the 201-after-retry must name design 0060, got: %q", out)
		}
	})

	t.Run("503 with leaked files -> RO-mount die", func(t *testing.T) {
		out, err := run("workspace agentd", "503", "abc123-sidecar.txt")
		if err == nil {
			t.Fatalf("files on disk despite the clean-fail must die, got: %q", out)
		}
		if !strings.Contains(out, "wrote files despite RO mount") {
			t.Fatalf("the RO-mount die must name the violation, got: %q", out)
		}
	})

	t.Run("broken shape (500) -> hard fail", func(t *testing.T) {
		out, err := run("workspace agentd", "500", "")
		if err == nil {
			t.Fatalf("a 500 upload is BROKEN, not a designed outcome — must die, got: %q", out)
		}
		if !strings.Contains(out, "neither designed-success") {
			t.Fatalf("the death must name the broken shape, got: %q", out)
		}
	})

	t.Run("single-container pod (init containers, no agentd) falls through", func(t *testing.T) {
		out, err := run("workspace platform-init platform-dirs workspace-setup credential-setup", "201", "")
		if err != nil {
			t.Fatalf("single-container path must fall through clean, got: %v\n%s", err, out)
		}
		if !strings.Contains(out, "single-container mode confirmed") || !strings.Contains(out, "GATE-FELL-THROUGH") {
			t.Fatalf("single-container pods must proceed to the rows, got: %q", out)
		}
	})

	// The old "upload accepted must die" test is superseded by the
	// 0060-landed leg above (201 now falls through by design).
}

// us68F8Step slices the nightly workflow down to the F8 step body so
// pins cannot accidentally target another step.
func us68F8Step(t *testing.T) string {
	t.Helper()
	src := mustRead(t, us68NightlyWorkflow)
	start := strings.Index(src, "Valkey migrate-rule connectivity (F8 runtime verification)")
	if start < 0 {
		t.Fatal("F8 step not found in e2e-nightly.yml — was it renamed?")
	}
	end := strings.Index(src[start:], "\n      - name: ")
	if end < 0 {
		t.Fatal("F8 step end boundary not found in e2e-nightly.yml")
	}
	return src[start : start+end]
}

// TestUS68CleanupDeletesSeededWorkspaces pins the trap-based hygiene
// (nightly 35437562027 adjudication): the attachment step's two seeded
// workspaces are deleted on EVERY exit path — sidecar skip, row death,
// green completion. Pre-#1463 the job died at this step so nothing
// downstream ever ran with those pods standing; now that the nightly
// proceeds, the two leaked workspace pods were ≈ exactly the CPU margin
// us-70's AC-1c batch was missing (FailedScheduling: Insufficient cpu,
// 5 standing workspace pods on the 1-node kind runner).
func TestUS68CleanupDeletesSeededWorkspaces(t *testing.T) {
	src := mustRead(t, us68AttachmentsScript)
	cleanup := regexp.MustCompile(`(?s)(?m)^cleanup\(\) \{.*?^\}`).FindString(src)
	if cleanup == "" {
		t.Fatal("us-68-attachments-e2e.sh must define cleanup() — the EXIT trap's hygiene lives there")
	}
	if !strings.Contains(src, "trap cleanup EXIT") {
		t.Fatal("the cleanup trap must stay registered (trap cleanup EXIT)")
	}
	// Anchored to an UNCOMMENTED line start: a commented-out
	// `# trap cleanup EXIT` satisfies a substring pin (the #1486 merge
	// verification caught exactly this class) — the hardening from the
	// ea6146ac adjudication.
	if !regexp.MustCompile(`(?m)^trap cleanup EXIT$`).MatchString(src) {
		t.Fatal("trap cleanup EXIT must exist as a real, uncommented line — a commented trap would silently disable the hygiene while passing a substring pin")
	}
	for _, pin := range []string{
		`kc -n "${NS}" delete workspace "${WS_A}" --ignore-not-found --wait=false >/dev/null 2>&1 || true`,
		`kc -n "${NS}" delete workspace "${WS_B}" --ignore-not-found --wait=false >/dev/null 2>&1 || true`,
		// the port-forward teardown must survive the cleanup extension
		`kill "${PF_PID}" 2>/dev/null || true`,
	} {
		if !strings.Contains(cleanup, pin) {
			t.Fatalf("cleanup() must keep %q on every exit path — the seeded workspaces must not leak into downstream suites (nightly 35437562027 census)", pin)
		}
	}
	// kubectl delete defaults to --wait=true (blocks on finalizers); a
	// wedged finalizer must never stall the EXIT trap (review r1).
	for _, pin := range []string{
		`kc -n "${NS}" delete workspace "${WS_A}" --ignore-not-found --wait=false`,
	} {
		if !strings.Contains(cleanup, pin) {
			t.Fatalf("cleanup() deletes must be --wait=false — kubectl's default --wait=true hangs the trap on a wedged finalizer")
		}
	}
}

// TestUS68Cleanup_Executes runs the script's REAL cleanup() against a
// tracing fake kc: both seeded workspaces are deleted, and the
// die-before-seed path (WS vars unset) must not explode under set -u.
func TestUS68Cleanup_Executes(t *testing.T) {
	bash := requireBash(t)
	src := mustRead(t, us68AttachmentsScript)
	cleanup := regexp.MustCompile(`(?s)(?m)^cleanup\(\) \{.*?^\}`).FindString(src)
	if cleanup == "" {
		t.Fatal("cleanup() not found — did the trap hygiene change shape?")
	}

	dir := t.TempDir()
	trace := filepath.Join(dir, "kc-trace")
	out, err := exec.Command(bash, "-c",
		`set -u; NS=ns; WS_A=ws-a; WS_B=ws-b; PF_PID=
kc() { printf '%s\n' "$*" >> '`+trace+`'; }
`+cleanup+`
cleanup`).CombinedOutput()
	if err != nil {
		t.Fatalf("cleanup() must run clean, got: %v\n%s", err, out)
	}
	raw, rerr := os.ReadFile(trace)
	if rerr != nil {
		t.Fatalf("kc trace unreadable: %v", rerr)
	}
	for _, want := range []string{"delete workspace ws-a --ignore-not-found --wait=false", "delete workspace ws-b --ignore-not-found --wait=false"} {
		if !strings.Contains(string(raw), want) {
			t.Fatalf("cleanup() must issue %q, trace:\n%s", want, raw)
		}
	}

	// Early-death path: WS_A/WS_B unset (a die before seeding) must not
	// explode under set -u and must not invoke kc.
	trace2 := filepath.Join(dir, "kc-trace2")
	out, err = exec.Command(bash, "-c",
		`set -u; NS=ns
kc() { printf '%s\n' "$*" >> '`+trace2+`'; }
`+cleanup+`
cleanup; echo CLEAN-OK`).CombinedOutput()
	if err != nil || !strings.Contains(string(out), "CLEAN-OK") {
		t.Fatalf("cleanup() must tolerate unset WS_A/WS_B (die-before-seed), got: %v\n%s", err, out)
	}
	if _, serr := os.Stat(trace2); !os.IsNotExist(serr) {
		t.Fatalf("cleanup() with no seeded workspaces must not touch the cluster")
	}
}

// TestUS68CleanupExecuteSmoke runs the REAL script under the shared
// ExecuteSmoke shims with the kubectl trace armed: the shim-driven row
// death fires the EXIT trap, and both seeded workspaces must be deleted
// by that trap (the seed-time pre-clean already accounts for one delete
// each in the trace — the trap's delete is the SECOND).
func TestUS68CleanupExecuteSmoke(t *testing.T) {
	if testing.Short() {
		t.Skip("execution smokes spawn many shim processes")
	}
	traceFile := filepath.Join(t.TempDir(), "kc-trace")
	combined, exitVal := runScriptUnderShims(t, us68AttachmentsScript, "Active",
		map[string]string{"SMOKE_KC_TRACE": traceFile})
	assertSmokeTraversal(t, us68AttachmentsScript, combined, exitVal, "✗", "all green")
	raw, err := os.ReadFile(traceFile)
	if err != nil {
		t.Fatalf("kubectl trace unreadable — did the shim tracing break?: %v", err)
	}
	for _, want := range []string{
		"delete workspace e2e0a000-0000-0000-0000-0000000000a1",
		"delete workspace e2e0b000-0000-0000-0000-0000000000b2",
	} {
		if n := strings.Count(string(raw), want); n < 2 {
			t.Fatalf("the EXIT trap must delete %s after the script's death (seed pre-clean + trap = ≥2 deletes, got %d); trace:\n%s", want, n, raw)
		}
	}
}

// TestUS68NightlyF8_ServiceLookupByName pins the #1456 F8 fix: the valkey
// Service in local/postgres-redis.yaml carries NO metadata labels, so the
// old `-l app=valkey` discovery matched nothing and F8 silently SKIPped
// on every nightly in history. The lookup must be by NAME, with the
// SKIP-on-absent guard retained and the why-comment alongside.
func TestUS68NightlyF8_ServiceLookupByName(t *testing.T) {
	step := us68F8Step(t)
	if !strings.Contains(step, "get svc valkey") {
		t.Fatal("F8 must look up the valkey Service BY NAME (`get svc valkey`) — the Service carries no metadata labels, so label-based discovery matches nothing (#1456)")
	}
	if strings.Contains(step, "-l app=valkey") {
		t.Fatal("F8 must not discover the valkey Service via metadata labels — svc/valkey has none; this is the silent-skip regression (#1456)")
	}
	if !strings.Contains(step, "SKIP: valkey Service not found") {
		t.Fatal("F8 must keep the loud SKIP-on-absent guard (Service genuinely missing must skip, not fail)")
	}
	if !strings.Contains(step, "no metadata labels") {
		t.Fatal("F8 must carry the why-comment (the Service carries no metadata labels) so the by-name lookup is not 'simplified' back to a label query")
	}
}

// us68F8StepScript extracts the F8 step's runnable body: everything after
// `run: |` with the 10-space YAML indentation stripped and GitHub's
// `${{ env.NS }}` rendering applied — the same script text the runner
// executes.
func us68F8StepScript(t *testing.T) string {
	t.Helper()
	step := us68F8Step(t)
	runAt := strings.Index(step, "run: |")
	if runAt < 0 {
		t.Fatal("F8 step has no run: | block")
	}
	body := step[runAt+len("run: |"):]
	body = regexp.MustCompile(`(?m)^ {10}`).ReplaceAllString(body, "")
	body = strings.ReplaceAll(body, "${{ env.NS }}", "llmsafespaces")
	return body
}

// us68FakeKubectl writes a fake kubectl (+ a no-op sleep so the completion
// loop cannot burn real seconds) and returns a PATH with the stub dir
// prepended. Every invocation appends to $FAKE_TRACE; behavior is driven
// by FAKE_SVC / FAKE_PHASE / FAKE_LOGS.
func us68FakeKubectl(t *testing.T) (path string, trace string) {
	t.Helper()
	dir := t.TempDir()
	trace = filepath.Join(dir, "trace")
	fake := `#!/usr/bin/env bash
printf '%s\n' "$*" >> "${FAKE_TRACE:?}"
case "$*" in
  *"get svc valkey"*)
    [[ "${FAKE_SVC:-}" == "absent" ]] && exit 1
    printf '10.96.0.42'
    ;;
  *"run valkey-migrate-probe"*) : ;;
  *"describe pod valkey-migrate-probe"*) printf 'DESCRIBE-OUTPUT\n' ;;
  *"get events"*) printf 'EVENTS-OUTPUT\n' ;;
  *"get networkpolicy"*) printf 'NETPOL-OUTPUT\n' ;;
  *"delete pod valkey-migrate-probe"*) : ;;
  *"get pod valkey-migrate-probe"*) printf '%s\n' "${FAKE_PHASE:-Pending}" ;;
  *"logs valkey-migrate-probe"*) [[ -n "${FAKE_LOGS:-}" ]] && printf '%s\n' "${FAKE_LOGS}" ;;
  *) printf 'fake kubectl: unexpected invocation: %s\n' "$*" >&2; exit 9 ;;
esac
`
	if err := os.WriteFile(filepath.Join(dir, "kubectl"), []byte(fake), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "sleep"), []byte("#!/usr/bin/env bash\n:"), 0o755); err != nil {
		t.Fatal(err)
	}
	return dir, trace
}

// runUS68F8Step executes the REAL F8 step body against the fake kubectl.
func runUS68F8Step(t *testing.T, svc, phase, logs string) (string, string, error) {
	t.Helper()
	bash := requireBash(t)
	dir, trace := us68FakeKubectl(t)
	script := "set -u; export PATH=" + shQuote(dir) + ":$PATH FAKE_TRACE=" + shQuote(trace) +
		" FAKE_SVC=" + svc + " FAKE_PHASE=" + shQuote(phase) + " FAKE_LOGS=" + shQuote(logs) + "\n" +
		us68F8StepScript(t)
	out, err := exec.Command(bash, "-c", script).CombinedOutput()
	traceRaw, terr := os.ReadFile(trace)
	if terr != nil {
		t.Fatalf("kubectl trace unreadable: %v", terr)
	}
	return string(out), string(traceRaw), err
}

// TestUS68NightlyF8_DiagnosticsBeforeDelete pins the #1459 withdrawal
// delta 2: on a failing verdict the step must dump describe + events
// BEFORE deleting the probe pod — a delete-first dump describes a corpse
// (in the pre-hardening step the delete preceded all diagnostics, making
// them dead weight).
func TestUS68NightlyF8_DiagnosticsBeforeDelete(t *testing.T) {
	t.Run("blocked: describe + events run, both before the delete", func(t *testing.T) {
		out, trace, err := runUS68F8Step(t, "present", "Succeeded", "BLOCKED")
		if err == nil {
			t.Fatalf("blocked must still fail the step, got: %q", out)
		}
		for _, want := range []string{"DESCRIBE-OUTPUT", "EVENTS-OUTPUT"} {
			if !strings.Contains(out, want) {
				t.Fatalf("failing verdict must dump %q before cleanup, got: %q", want, out)
			}
		}
		describe := strings.Index(trace, "describe pod valkey-migrate-probe")
		events := strings.Index(trace, "get events")
		del := strings.Index(trace, "delete pod valkey-migrate-probe")
		if describe < 0 || events < 0 || del < 0 || describe > del || events > del {
			t.Fatalf("describe(%d) and events(%d) must be invoked before delete(%d); trace:\n%s", describe, events, del, trace)
		}
	})

	t.Run("reachable: probe pod still cleaned up", func(t *testing.T) {
		out, trace, err := runUS68F8Step(t, "present", "Succeeded", "REACHABLE")
		if err != nil {
			t.Fatalf("reachable must exit 0, got: %v\n%s", err, out)
		}
		if !strings.Contains(trace, "delete pod valkey-migrate-probe") {
			t.Fatalf("a green verdict must still delete the probe pod, trace:\n%s", trace)
		}
	})
}

// TestUS68NightlyF8_ThreeWayVerdict pins the #1459 withdrawal delta 1:
// the F8 step must classify on RESULT emptiness (never a stale PHASE
// sample — the phase is re-fetched AFTER the logs read):
//
//   - REACHABLE            -> OK, exit 0
//   - verdict, no REACHABLE (BLOCKED...) -> hard FAIL + datastore
//     NetworkPolicy dump, exit 1
//   - no verdict (empty logs: the probe never ran to completion — image
//     pull / scheduling / transient API error) -> WARN + describe/events,
//     exit 0 — a no-verdict leg must not hold the ten downstream rows
//     hostage while a real verdict still hard-fails.
func TestUS68NightlyF8_ThreeWayVerdict(t *testing.T) {
	t.Run("no verdict (probe never completed) -> WARN, exit 0, no netpol dump", func(t *testing.T) {
		out, trace, err := runUS68F8Step(t, "present", "Pending", "")
		if err != nil {
			t.Fatalf("a no-verdict leg must not fail the step (downstream rows still run), got: %v\n%s", err, out)
		}
		if !strings.Contains(out, "WARN: valkey migrate probe produced no verdict") {
			t.Fatalf("no verdict must WARN loudly naming F8 as unverified, got: %q", out)
		}
		if !strings.Contains(out, "DESCRIBE-OUTPUT") || !strings.Contains(out, "EVENTS-OUTPUT") {
			t.Fatalf("the no-verdict leg must dump describe/events (scheduling/pull evidence), got: %q", out)
		}
		if strings.Contains(out, "NETPOL-OUTPUT") {
			t.Fatalf("the datastore NetworkPolicy dump belongs to the REAL-verdict fail leg, got: %q", out)
		}
		if !strings.Contains(trace, "delete pod valkey-migrate-probe") {
			t.Fatalf("the probe pod must still be cleaned up, trace:\n%s", trace)
		}
	})

	t.Run("terminal phase but empty logs -> still the no-verdict WARN leg", func(t *testing.T) {
		// Classification is on RESULT emptiness, not the phase sample: a
		// pod that completed without leaving a verdict in its logs
		// (log-collection anomaly) is UNVERIFIED, not BLOCKED.
		out, _, err := runUS68F8Step(t, "present", "Succeeded", "")
		if err != nil {
			t.Fatalf("empty logs classify as no-verdict regardless of phase, got: %v\n%s", err, out)
		}
		if !strings.Contains(out, "WARN: valkey migrate probe produced no verdict") {
			t.Fatalf("expected the no-verdict WARN, got: %q", out)
		}
	})

	t.Run("real BLOCKED verdict -> FAIL + datastore NetworkPolicy dump", func(t *testing.T) {
		out, _, err := runUS68F8Step(t, "present", "Succeeded", "BLOCKED")
		if err == nil {
			t.Fatalf("a real verdict must hard-fail, got: %q", out)
		}
		if !strings.Contains(out, "FAIL: migrate-labeled pod could not reach Valkey:6379") {
			t.Fatalf("expected the FAIL verdict, got: %q", out)
		}
		if !strings.Contains(out, "NETPOL-OUTPUT") {
			t.Fatalf("a real failing verdict must dump the datastore NetworkPolicies, got: %q", out)
		}
	})

	t.Run("phase is re-fetched AFTER the logs read", func(t *testing.T) {
		// The wait loop's PHASE sample can be stale (pod completing just
		// after the loop window). The step must re-read the phase after
		// the logs: the LAST pod fetch in the trace follows the logs call.
		_, trace, err := runUS68F8Step(t, "present", "Pending", "")
		if err != nil {
			t.Fatalf("no-verdict leg must exit 0, got: %v", err)
		}
		logsAt := strings.LastIndex(trace, "logs valkey-migrate-probe")
		lastPhase := strings.LastIndex(trace, "get pod valkey-migrate-probe")
		if logsAt < 0 || lastPhase < logsAt {
			t.Fatalf("the phase must be re-fetched after the logs read (logs@%d, last pod fetch@%d); trace:\n%s", logsAt, lastPhase, trace)
		}
	})

	// Structural anchors: the classification expressions themselves.
	step := us68F8Step(t)
	for _, pin := range []string{
		`if [[ "$RESULT" == *"REACHABLE"* ]]`,
		`[[ -n "$RESULT" ]]`,
	} {
		if !strings.Contains(step, pin) {
			t.Fatalf("F8 verdict classification must keep %q — classification is on RESULT emptiness (#1459 delta 1)", pin)
		}
	}
}

// TestUS68NightlyF8_StepExecutes runs the REAL F8 step script against a
// fake kubectl across the reachable / blocked / absent-Service outcomes —
// the step's verdict logic executed, not re-implemented (the r2/r4 lesson).
func TestUS68NightlyF8_StepExecutes(t *testing.T) {
	t.Run("reachable -> OK, exit 0", func(t *testing.T) {
		out, trace, err := runUS68F8Step(t, "present", "Succeeded", "REACHABLE")
		if err != nil {
			t.Fatalf("reachable must exit 0, got: %v\n%s", err, out)
		}
		if !strings.Contains(out, "OK: migrate-labeled pod reached Valkey:6379 (F8)") {
			t.Fatalf("reachable must print the OK verdict, got: %q", out)
		}
		if !strings.Contains(trace, "run valkey-migrate-probe") {
			t.Fatalf("the probe pod must be created, trace: %q", trace)
		}
		if strings.Contains(trace, "describe pod valkey-migrate-probe") {
			t.Fatalf("a green verdict needs no describe diagnostics, trace: %q", trace)
		}
	})

	t.Run("blocked -> FAIL, exit 1", func(t *testing.T) {
		out, _, err := runUS68F8Step(t, "present", "Succeeded", "BLOCKED")
		if err == nil {
			t.Fatalf("a real BLOCKED verdict must fail the step, got: %q", out)
		}
		if !strings.Contains(out, "FAIL: migrate-labeled pod could not reach Valkey:6379") {
			t.Fatalf("blocked must print the FAIL verdict, got: %q", out)
		}
	})

	t.Run("valkey Service absent -> loud SKIP, exit 0", func(t *testing.T) {
		out, trace, err := runUS68F8Step(t, "absent", "", "")
		if err != nil {
			t.Fatalf("absent Service must SKIP clean, got: %v\n%s", err, out)
		}
		if !strings.Contains(out, "SKIP: valkey Service not found") {
			t.Fatalf("absent Service must print the SKIP message, got: %q", out)
		}
		if strings.Contains(trace, "run valkey-migrate-probe") {
			t.Fatalf("no probe pod may be created when the Service is absent, trace: %q", trace)
		}
	})
}

// TestUS68SidecarGate_E10LeakFilterPinsProbeFile pins the r1 blocker
// fix: E10's leak filter must exclude the gate's own probe file — the
// 201 fall-through's first full run would have died at E10 otherwise.
func TestUS68SidecarGate_E10LeakFilterPinsProbeFile(t *testing.T) {
	src := mustRead(t, us68AttachmentsScript)
	if !strings.Contains(src, "grep -v sidecar.txt") {
		t.Fatal("E10's leak filter must exclude the gate's probe file (grep -v sidecar.txt) — without it the 201 fall-through dies at E10 (r1's blocker)")
	}
}
