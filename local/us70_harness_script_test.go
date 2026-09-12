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
	"fmt"
	"net/http"
	"net/http/httptest"
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

// TestUS70WorkspaceNamesAreUUIDs executes the lib's ws_id against several
// indexes and pins the UUID contract: production mints workspaceID =
// uuid.New() and the CR name IS that UUID, so every API workspace op
// resolves WHERE workspaces.id = $1 (uuid column) against the CR name.
// The 2026-08-31 pool AC-1 failure was exactly this shape violation
// (non-UUID WS_BASE → bind_env could never resolve).
func TestUS70WorkspaceNamesAreUUIDs(t *testing.T) {
	bash := requireBash(t)
	script := mustRead(t, us70CommonScript)
	wsID := regexp.MustCompile(`(?m)^ws_id\(\) \{.*\}$`).FindString(script)
	if wsID == "" {
		t.Fatal("us70-common.sh must define ws_id() on a single line")
	}
	out, err := exec.Command(bash, "-c",
		`WS_BASE='e2e5d000-0000-4000-8000-000000000000'; `+wsID+`; `+
			`for i in 1 2 101 9999; do ws_id "$i"; done`).CombinedOutput()
	if err != nil {
		t.Fatalf("execute ws_id: %v\n%s", err, out)
	}
	re := regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)
	lines := strings.FieldsFunc(string(out), func(r rune) bool { return r == '\n' || r == '\r' })
	if len(lines) != 4 {
		t.Fatalf("ws_id must print 4 lines, got %d: %q", len(lines), string(out))
	}
	for _, line := range lines {
		if !re.MatchString(line) {
			t.Fatalf("ws_id output %q is not a valid UUID — API workspace ops resolve workspaces.id (uuid) by CR name", line)
		}
	}
	if !strings.Contains(script, "INSERT INTO workspaces (id, name, user_id") {
		t.Fatal("us70-common.sh must seed the workspaces metadata row — WorkspaceAccessMiddleware resolves through PostgreSQL; a CR-only workspace cannot bind")
	}
}

// TestUS70Harness_SessionAuthPins pins the DEK-gate fix layer: secret
// authoring (bind_env, AC-F create/bind) must ride the JWT session —
// CreateSecret resolves the user DEK through the session cache and an
// API-key request carries none (ErrDEKUnavailable -> 500). The lib must
// register/login (OWNER_ID is the API-minted users.id UUID, not the
// username) and the scripts' secret-creating calls must use AUTH_TOKEN.
func TestUS70Harness_SessionAuthPins(t *testing.T) {
	lib := mustRead(t, us70CommonScript)
	for _, pin := range []string{
		"login_harness_user()",
		"seed_session()",
		"OWNER_ID=$(curl",
		"Bearer ${AUTH_TOKEN:?}",
	} {
		if !strings.Contains(lib, pin) {
			t.Fatalf("us70-common.sh must keep %q — the DEK-gated authoring path depends on it", pin)
		}
	}
	if strings.Contains(lib, "password_hash, role)") {
		t.Fatal("the lib must not psql-insert the user row (dummy hash): registration through the API provisions user_keys + a real hash")
	}
	delivery := mustRead(t, us70DeliveryScript)
	sfCreate := strings.Count(delivery, "Bearer ${AUTH_TOKEN}")
	if sfCreate < 2 {
		t.Fatalf("AC-F secret create + bind must use the JWT session (got %d AUTH_TOKEN uses)", sfCreate)
	}
}

// TestUS70Harness_SessionBootstrap_UnhappyPaths executes the extracted
// session helpers against a dead port: login must yield an empty
// AUTH_TOKEN without killing the caller (the register fallback depends
// on that), and seed_session must die loudly naming the failure.
func TestUS70Harness_SessionBootstrap_UnhappyPaths(t *testing.T) {
	bash := requireBash(t)
	lib := mustRead(t, us70CommonScript)
	extract := func(name string) string {
		m := regexp.MustCompile(`(?s)(?m)^` + name + `\(\) \{.*?^\}`).FindString(lib)
		if m == "" {
			t.Fatalf("us70-common.sh must define %s()", name)
		}
		return m
	}

	out, err := exec.Command(bash, "-c",
		`PORTFWD_PORT=1; AUTH_TOKEN=sentinel; `+extract("login_harness_user")+`; printf '|%s', "${AUTH_TOKEN}"`).CombinedOutput()
	if err != nil {
		t.Fatalf("login against a dead port must not kill the caller: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "|,") && !strings.HasSuffix(strings.TrimSpace(string(out)), ",") {
		t.Fatalf("failed login must leave AUTH_TOKEN empty, got: %q", out)
	}

	out, err = exec.Command(bash, "-c",
		`set -u; PORTFWD_PORT=1; USER_ID=dead; API_KEY=k; PGPOD=x; PG_PWD=x; `+
			`die() { printf '%s' "$*" >&2; exit 1; }; `+
			extract("login_harness_user")+`; `+extract("seed_session")+
			`; seed_session dead-user 2>&1; exit $?`).CombinedOutput()
	if err == nil {
		t.Fatalf("seed_session with both register and login failing must exit non-zero, got: %q", out)
	}
	if !strings.Contains(string(out), "harness register/login failed") {
		t.Fatalf("the loud failure must name the problem, got: %q", out)
	}
}

// TestUS70Harness_SessionBootstrap_FakeAPI drives the extracted session
// helpers against a scripted fake API — the register-failure and
// /auth/me-failure die paths, deterministically (the e2e unhappy rows
// for the bootstrap, executable without a cluster).
func TestUS70Harness_SessionBootstrap_FakeAPI(t *testing.T) {
	bash := requireBash(t)
	lib := mustRead(t, us70CommonScript)
	extract := func(name string) string {
		m := regexp.MustCompile(`(?s)(?m)^` + name + `\(\) \{.*?^\}`).FindString(lib)
		if m == "" {
			t.Fatalf("us70-common.sh must define %s()", name)
		}
		return m
	}

	runSeedSession := func(handler http.HandlerFunc) (string, error) {
		srv := httptest.NewServer(http.HandlerFunc(handler))
		defer srv.Close()
		port := srv.URL[strings.LastIndex(srv.URL, ":")+1:]
		cmd := exec.Command(bash, "-c",
			`set -u; PORTFWD_PORT=`+port+`; USER_ID=fake; API_KEY=k; PGPOD=x; PG_PWD=x; HARNESS_PASSWORD=fake-pw-2026; `+
				`die() { printf '%s' "$*" >&2; exit 1; }; `+
				extract("login_harness_user")+`; `+extract("seed_session")+
				`; seed_session fake-user 2>&1; exit $?`)
		out, err := cmd.CombinedOutput()
		return string(out), err
	}

	t.Run("register 500 and login 401 die loudly", func(t *testing.T) {
		out, err := runSeedSession(func(w http.ResponseWriter, r *http.Request) {
			if strings.HasSuffix(r.URL.Path, "/login") {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			w.WriteHeader(http.StatusInternalServerError)
		})
		if err == nil {
			t.Fatalf("must exit non-zero, got: %q", out)
		}
		if !strings.Contains(out, "harness register/login failed") {
			t.Fatalf("must name the failure, got: %q", out)
		}
	})

	t.Run("auth/me failure dies naming the id resolution", func(t *testing.T) {
		out, err := runSeedSession(func(w http.ResponseWriter, r *http.Request) {
			switch {
			case strings.HasSuffix(r.URL.Path, "/login"):
				_, _ = w.Write([]byte(`{"token":"jwt"}`))
			case strings.HasSuffix(r.URL.Path, "/me"):
				w.WriteHeader(http.StatusInternalServerError)
			default:
				w.WriteHeader(http.StatusCreated)
				_, _ = w.Write([]byte(`{}`))
			}
		})
		if err == nil {
			t.Fatalf("must exit non-zero, got: %q", out)
		}
		if !strings.Contains(out, "could not resolve the harness user's id") {
			t.Fatalf("must name the id-resolution failure, got: %q", out)
		}
	})
}

// TestUS70Harness_BindEnv_ReloginsOn401 pins the JWT-expiry robustness:
// one 401 triggers a re-login and the retried bind succeeds — pool
// dwells can outlive a short token TTL.
func TestUS70Harness_BindEnv_ReloginsOn401(t *testing.T) {
	bash := requireBash(t)
	lib := mustRead(t, us70CommonScript)
	bindFn := regexp.MustCompile(`(?s)(?m)^bind_env\(\) \{.*?^\}`).FindString(lib)
	if bindFn == "" {
		t.Fatal("us70-common.sh must define bind_env()")
	}
	logins := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/login"):
			logins++
			_, _ = w.Write([]byte(`{"token":"jwt-refreshed"}`))
		case r.Method == http.MethodPut && strings.Contains(r.URL.Path, "/env"):
			if r.Header.Get("Authorization") == "Bearer jwt-refreshed" {
				w.WriteHeader(http.StatusNoContent)
				return
			}
			w.WriteHeader(http.StatusUnauthorized)
		}
	}))
	defer srv.Close()
	port := srv.URL[strings.LastIndex(srv.URL, ":")+1:]

	out, err := exec.Command(bash, "-c",
		`set -u; PORTFWD_PORT=`+port+`; USER_ID=fake; AUTH_TOKEN=jwt-1; API_KEY=k; HARNESS_PASSWORD=fake-pw-2026; `+
			`die() { printf '%s' "$*" >&2; exit 1; }; `+
			regexp.MustCompile(`(?s)(?m)^login_harness_user\(\) \{.*?^\}`).FindString(lib)+`; `+
			bindFn+`; bind_env ws1 VAR v 2>&1; exit $?`).CombinedOutput()
	if err != nil {
		t.Fatalf("the retried bind must succeed after re-login: %v\n%s", err, out)
	}
	if logins < 1 {
		t.Fatalf("a 401 must trigger exactly one re-login; logins=%d", logins)
	}
}

// TestUS70Harness_FailureDiagnostics pins the diagnose-dump contract:
// a wait_phase timeout must dump CR status, pod describe, container logs,
// controller tail, and events — pool runs 4-5 each burned ~40 minutes on
// illegible failures before this existed.
func TestUS70Harness_FailureDiagnostics(t *testing.T) {
	lib := mustRead(t, us70CommonScript)
	for _, pin := range []string{
		"diagnose_workspace()",
		"wait_phase",
		"--- CR status",
		"--- container logs",
		"--- controller tail",
		"--- recent events",
	} {
		if !strings.Contains(lib, pin) {
			t.Fatalf("us70-common.sh must keep %q — failures must be legible in the run log", pin)
		}
	}
	if !strings.Contains(lib, `diagnose_workspace "${ws}"`) {
		t.Fatal("wait_phase's timeout path must call diagnose_workspace")
	}
}

// TestUS70Harness_ScaleResourcesQuoted pins the pool-run-7 lesson:
// SCALE_RES is a shell string interpolated into the CR heredoc — the
// cpuLimit value MUST reach the API as a YAML string ("1"), not the
// integer the double-quoted assignment produced (CRD validation rejected
// spec.resources.cpuLimit integer at the first batch workspace).
func TestUS70Harness_ScaleResourcesQuoted(t *testing.T) {
	src := mustRead(t, us70DeliveryScript)
	if !strings.Contains(src, "cpuLimit: 1000m") {
		t.Fatal("cpuLimit must stay unit-suffixed — bare numerics coerce to JSON integers on the apply path (pool runs 7+9)")
	}
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
		// The #1087 ≥1h suspend gate: on schedule triggers inputs are
		// empty so the || fallback pins AC-2 to 3600. The literal
		// `SUSPEND_SECONDS: 3600` pin was retired when dispatch got the
		// dwell_seconds knob (fast-iteration cycles default to 60).
		"SUSPEND_SECONDS: ${{ inputs.dwell_seconds || '3600' }}",
		"local/lib/gvisor.sh",
		// The capacity-appropriate runner + full-scale leg (pool runs
		// 10-16 proved the 2-core GitHub-hosted ceiling; the dind set
		// is repo-scoped to this repo — see ops-prod 2baafa74).
		"runs-on: lenaxia-dind-runner",
		// Calibrated 2026-09-03 (#1252): knee on the dind runner class
		// measured ~55 concurrent gVisor workspaces across runs
		// 33733697430/33773343318; 40 = 0.72x knee. The || '40' fallback
		// is load-bearing: on the Sunday cron the inputs context is empty
		// (defaults apply to workflow_dispatch only) and without it the
		// script default (100) would run a 100-wave (r21 blocker 2).
		"RESUME_SCALE: ${{ inputs.resume_scale || '40' }}",
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

// TestUS70NotifyRows_Pin pins the US-70.3 Part D rows' key asserts so they
// cannot be silently dropped from the delivery suite (same philosophy as
// the faults pins): the AC-3 30s budget literal, the revoke DELETE + audit
// query, the resync-endpoint row (pod :4097, not_modified, 429, pull_failed),
// the pod port-forward helper, and the api-scale-to-0 block.
func TestUS70NotifyRows_Pin(t *testing.T) {
	src := mustRead(t, us70DeliveryScript)
	for _, pin := range []string{
		// AC-3: the 30s budget literal + the anchored-seq compare.
		`AC3_BUDGET_MS="${AC3_BUDGET_MS:-30000}"`,
		"spawned_seq",
		// AC-5/AC-6: the ForceRevoke DELETE + the audit query + absence guard.
		"-X DELETE",
		"/api/v1/secrets/",
		"secret_audit_log WHERE action='revoke'",
		"env_absent_from_child",
		// AC-11: the pod resync endpoint row.
		"/v1/resync-secrets",
		"not_modified",
		"rate_limited",
		"pull_failed",
		// AC-4-lite: mid-apply pod delete + monotonic seq.
		"kc delete pod",
	} {
		if !strings.Contains(src, pin) {
			t.Fatalf("delivery script must keep %q — the US-70.3 notify/reconcile rows depend on it", pin)
		}
	}
	lib := mustRead(t, us70CommonScript)
	for _, pin := range []string{
		// resync_pod: the pod port-forward + workspace-password channel.
		"resync_pod()",
		"kc port-forward \"pod/${pod}\"",
		"4097",
		"workspace-pw-",
		"-u \"opencode:${RESC_PW}\"",
		// api outage helpers for the AC-8 network-layer block.
		"api_down()",
		"api_up()",
		"api_portforward_restart()",
		"--replicas=0",
	} {
		if !strings.Contains(lib, pin) {
			t.Fatalf("us70-common.sh must keep %q — the US-70.3 helpers depend on it", pin)
		}
	}
}

// TestUS70NotifyHelpers_SpawnedSeq executes spawned_seq with a fake kc
// returning a per-workspace spawnedRev: "seq:hash:hash" parses to the seq,
// a bare/legacy hash yields EMPTY (never a fabricated 0), and so does an
// absent rev.
func TestUS70NotifyHelpers_SpawnedSeq(t *testing.T) {
	bash := requireBash(t)
	lib := mustRead(t, us70CommonScript)
	fn := regexp.MustCompile(`(?s)(?m)^spawned_seq\(\) \{.*?^\}`).FindString(lib)
	if fn == "" {
		t.Fatal("us70-common.sh must define spawned_seq()")
	}
	script := `set -u
rev_for() { case "$1" in
  a) printf '12:man:con' ;;
  b) printf 'deadbeeflegacy' ;;
  c) printf '' ;;
  *) printf '99:man:con' ;;
esac }
kc() { printf '%s' "$(rev_for "$3")"; }
` + fn + `
printf 'a=%s|b=%s|c=%s\n' "$(spawned_seq a)" "$(spawned_seq b)" "$(spawned_seq c)"`
	out, err := exec.Command(bash, "-c", script).CombinedOutput()
	if err != nil {
		t.Fatalf("execute spawned_seq: %v\n%s", err, out)
	}
	got := strings.TrimSpace(string(out))
	if got != "a=12|b=|c=" {
		t.Fatalf("spawned_seq must parse the seq prefix and yield empty for legacy/absent revs, got %q", got)
	}
}

// TestUS70NotifyHelpers_EnvAbsentGuard executes env_absent_from_child with
// a fake agent_environ: present-var and empty-environ (mid-restart) must
// both be NON-zero (absence is only assertible on a live, readable child),
// a live environ without the var must be zero.
func TestUS70NotifyHelpers_EnvAbsentGuard(t *testing.T) {
	bash := requireBash(t)
	lib := mustRead(t, us70CommonScript)
	fn := regexp.MustCompile(`(?s)(?m)^env_absent_from_child\(\) \{.*?^\}`).FindString(lib)
	if fn == "" {
		t.Fatal("us70-common.sh must define env_absent_from_child()")
	}
	script := `set -u
mode="$1"
agent_environ() { case "$mode" in
  live-with-var) printf 'PATH=/usr/bin\nSD_X=secret\n' ;;
  live-clean)    printf 'PATH=/usr/bin\nOTHER=1\n' ;;
  restarting)    printf '' ;;
esac }
` + fn + `
if env_absent_from_child w "SD_X="; then echo absent; else echo not-absent; fi`
	cases := map[string]string{
		"live-with-var": "not-absent",
		"live-clean":    "absent",
		"restarting":    "not-absent",
	}
	for mode, want := range cases {
		out, err := exec.Command(bash, "-c", script, "env-absent", mode).CombinedOutput()
		if err != nil {
			t.Fatalf("execute env_absent_from_child (%s): %v\n%s", mode, err, out)
		}
		if got := strings.TrimSpace(string(out)); got != want {
			t.Fatalf("env_absent_from_child(%s) = %q, want %q — an unreadable environ must never read as absent", mode, got, want)
		}
	}
}

// TestUS70ReconcileInterval_WorkflowLockstep pins the fast reconcile loop
// wiring: BOTH the nightly and the pool helm install must set
// LLMSAFESPACES_SECRETS_RECONCILE_INTERVAL via api.extraEnv (single --set
// string — helm's mergeMaps CLOBBERS list entries across separate --set
// flags, so both key paths must ride one flag), the value must equal the
// delivery script's RECONCILE_INTERVAL_S default the AC-8/AC-10 budgets
// derive from, and the workflow step env must pass the same number.
func TestUS70ReconcileInterval_WorkflowLockstep(t *testing.T) {
	// Indexes 0/1 are the V2 delivery flags (OPENCODE_V2_DELIVERY /
	// AGENTD_STATE_AUTHORITY — the pool runs the V2 regime, and
	// local/authority-flip.sh's helm step owns that layout); the reconcile
	// interval rides index 2.
	const helmSet = `--set "api.extraEnv[2].name=LLMSAFESPACES_SECRETS_RECONCILE_INTERVAL,api.extraEnv[2].value=5s"`
	for _, wf := range []string{
		filepath.Join("..", ".github", "workflows", "e2e-nightly.yml"),
		us70PoolWorkflow,
	} {
		src := mustRead(t, wf)
		if !strings.Contains(src, helmSet) {
			t.Fatalf("%s must carry %s (single-flag form) — the reconcile loop period is the AC-8/AC-10 budget basis", wf, helmSet)
		}
		// A split form (name and value on separate --set flags) renders only
		// the LAST list entry under helm's map-merge — refuse it explicitly.
		if strings.Contains(src, "--set \"api.extraEnv[0].name=LLMSAFESPACES_SECRETS_RECONCILE_INTERVAL\"") {
			t.Fatalf("%s splits the extraEnv set across --set flags — helm clobbers list entries that way", wf)
		}
		if !strings.Contains(src, "RECONCILE_INTERVAL_S: 5") {
			t.Fatalf("%s must pass RECONCILE_INTERVAL_S: 5 to the delivery suite (must match the helm set)", wf)
		}
	}
	script := mustRead(t, us70DeliveryScript)
	if !strings.Contains(script, "RECONCILE_INTERVAL_S") {
		t.Fatal("delivery script must derive its AC-8/AC-10 budgets from RECONCILE_INTERVAL_S")
	}
	lib := mustRead(t, us70CommonScript)
	def := extractSingle(t, lib,
		regexp.MustCompile(`RECONCILE_INTERVAL_S="\$\{RECONCILE_INTERVAL_S:-(\d+)\}"`),
		"lib RECONCILE_INTERVAL_S default")
	if def != "5" {
		t.Fatalf("lib RECONCILE_INTERVAL_S default must stay 5 (matching the workflows' 5s helm set), got %s", def)
	}
}

// TestUS69EvidenceLegScript (#1218/#1219): the Epic 69 pool leg must be
// syntactically valid, source the shared harness, and fail (not silently
// pass) on probe errors — the matrix outcome is data, harness failure is
// not.

func TestUS69EvidenceLegScript(t *testing.T) {
	if testing.Short() {
		t.Skip("script lint only")
	}
	// The test binary's cwd is this package dir; the scripts sit in the
	// same directory.
	for _, script := range []string{"us-69-evidence-leg.sh", "authority-flip.sh"} {
		t.Run(script, func(t *testing.T) {
			out, err := exec.Command("bash", "-n", script).CombinedOutput()
			if err != nil {
				t.Fatalf("syntax: %v: %s", err, out)
			}
		})
	}
	src, err := os.ReadFile("us-69-evidence-leg.sh")
	if err != nil {
		t.Fatal(err)
	}
	text := string(src)
	for _, want := range []string{
		"local/lib/us70-common.sh",
		"local/spike-admission-id.sh",
		"local/authority-flip.sh",
		"set -euo pipefail",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("evidence leg missing %q", want)
		}
	}
	// The workspace ID must be a VALID UUID literal (pool run
	// 33574913376: a WS_BASE string-mangle produced a 12-char first
	// group and Postgres' uuid column rejected the metadata seed). Pin
	// the shape so regeneration-by-mangling cannot silently return.
	m := regexp.MustCompile(`WS_ADMIT="\$\{WS_ADMIT:-([0-9a-f]+(?:-[0-9a-f]+)*)\}"`).FindStringSubmatch(text)
	if m == nil {
		t.Fatal("WS_ADMIT default not found — the pin cannot be verified")
	}
	if !regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`).MatchString(m[1]) {
		t.Fatalf("WS_ADMIT default must be a valid UUID literal (uuid-typed workspaces.id), got %q", m[1])
	}

	// Session-create robustness (pool run 33578936807: bare curl exit 22
	// killed the script before die could diagnose): the create must
	// capture code+body, retry once, and print the body on failure —
	// never a bare -f pipefail exit.
	for _, pin := range []string{
		`-w '%{http_code}' -X POST`,
		`for attempt in 1 2`,
		`session create failed (HTTP ${SC})`,
	} {
		if !strings.Contains(text, pin) {
			t.Errorf("evidence leg session-create lost its capture/retry/diagnose path: %q", pin)
		}
	}

	// Failure-path semantics: a probe ERROR fails the step (never a
	// silent pass — the matrix outcome is data, harness failure is not),
	// and a park/unpark round-trip mismatch is fatal.
	for _, mustDie := range []string{
		`die "admission-ID probe errored`,
		`die "park/unpark round-trip mismatch`,
		`die "no workspace password secret"`,
	} {
		if !strings.Contains(text, mustDie) {
			t.Errorf("evidence leg lost its failure path: %q", mustDie)
		}
	}
}

// TestUS70AC13WaveBoot pins the AC-13 batch structure (pool runs 14-15):
// workspaces boot in waves (BOOT_WAVE at a time, each wave fully Active
// before the next — 25 concurrent boots crash-loop the API pod and starve
// the local-path provisioner on the 2-core runner), and the convergence
// checks run only after every wave is Active (sampling mid-storm nil-clears
// the controller mirror via scrape timeout).
func TestUS70AC13WaveBoot(t *testing.T) {
	script := mustRead(t, us70DeliveryScript)
	waveSeed := strings.Index(script, `seed_workspace "${WSBATCH[n - 1]}"`)
	convergedLoop := `secrets_converged "${ws}" 300 || die "AC-13: ${ws} pre-suspend unhealthy"`
	convergedIdx := strings.Index(script, convergedLoop)
	if waveSeed == -1 || convergedIdx == -1 {
		t.Fatal("AC-13 wave-boot/converged loop bodies not found — harness structure drifted; update this pin")
	}
	// The seed loop must be wave-scoped (indexed WSBATCH access, not the
	// flat for-each that booted the whole batch at once).
	flatSeed := strings.Index(script, "seed_workspace \"${ws}\"")
	if flatSeed != -1 {
		t.Fatalf("AC-13 must seed in waves (BOOT_WAVE), found a flat batch seed loop at byte %d — 25 concurrent boots saturate the 2-core runner's control plane (pool run 15)", flatSeed)
	}
	// Every wave's Active wait must complete before the convergence checks.
	lastActiveWait := strings.LastIndex(script, "wait_phase \"${WSBATCH[n - 1]}\" Active")
	if lastActiveWait == -1 || lastActiveWait > convergedIdx {
		t.Fatal("AC-13 convergence checks must run after all boot waves reached Active — sampling mid-storm measures the runner, not the product")
	}
}

// TestUS70PoolCertManagerWebhookRetry pins the cert-manager step's
// dump-then-bounce structure (runs 19-21; run 21's dump pinned the root
// cause: pods created in kind's post-creation window project a stale
// kube-root CA — cainjector crash-loops on x509-unknown-authority, the
// webhook then healthz-500s forever). The step must dump BOTH
// deployments' evidence, then bounce both pods (deletion clears
// crash-loop backoff and re-projects the fresh CA — the run-21
// webhook-only rollout restart provably could not recover this), and
// verify recovery with condition waits (rollout status trips instantly
// on the stale ProgressDeadlineExceeded condition).
func TestUS70PoolCertManagerWebhookRetry(t *testing.T) {
	src := mustRead(t, us70PoolWorkflow)
	for _, pin := range []string{
		"rollout status deployment/cert-manager-webhook --timeout=600s",
		`describe pods -l 'app.kubernetes.io/name in (cainjector,webhook)'`,
		"logs deployment/cert-manager-cainjector --tail=30",
		"logs deployment/cert-manager-webhook --tail=30",
		"get events --sort-by=.lastTimestamp",
	} {
		if !strings.Contains(src, pin) {
			t.Fatalf("pool workflow cert-manager step must contain %q — the runs-19/20/21 failure path must stay legible (found missing)", pin)
		}
	}
	evidence := strings.Index(src, `describe pods -l 'app.kubernetes.io/name in (cainjector,webhook)'`)
	bounce := strings.Index(src, `delete pod -l 'app.kubernetes.io/name in (cainjector,webhook)'`)
	cainjectorWait := strings.Index(src, "wait --for=condition=Available deployment/cert-manager-cainjector")
	webhookWait := strings.Index(src, "wait --for=condition=Available deployment/cert-manager-webhook")
	if evidence == -1 || bounce == -1 || cainjectorWait == -1 || webhookWait == -1 ||
		evidence >= bounce || bounce >= cainjectorWait || cainjectorWait >= webhookWait {
		t.Fatalf("cert-manager recovery ordering must be evidence(%d) < bounce(%d) < cainjector-wait(%d) < webhook-wait(%d) — evidence after the bounce destroys the failure state, and the cainjector (the component whose crash-loop starves the webhook's CA secret) must recover first", evidence, bounce, cainjectorWait, webhookWait)
	}
	if strings.Contains(src, "rollout restart deployment/cert-manager-webhook") {
		t.Fatal("the webhook-only rollout restart is the run-21 recovery that provably failed (cainjector stays crash-looping; rollout status trips on the stale deadline condition) — the bounce must replace it")
	}
	// The generic dump must list pods cluster-wide AND untruncated: runs
	// 19/20 failed in cert-manager while the dump covered only the app
	// namespace, and the pool's own AC-13 creates 100+ pods — a head-N
	// pipe cuts exactly the rows a scale failure needs.
	if !strings.Contains(src, "\n          kubectl get pods -A -o wide || true\n") {
		t.Fatal("the generic failure dump must list pods in ALL namespaces, untruncated and abort-safe — the app-namespace-only dump was the runs-19/20 blind spot, and a head-N pipe or bare line cuts the scale-failure rows")
	}
}

func TestUS70_StopwatchWorkersAreSetESafe(t *testing.T) {
	// The resume stopwatch workers are ( ... ) & subshells that inherit
	// set -Eeuo pipefail. Two patterns killed every worker instantly on
	// the stopwatch's first executions (runs 33806590231-33809514014):
	//   1. `curl -sf ...` unguarded — any non-2xx activate exits the worker
	//      before a single phase poll
	//   2. `[[ "$p" == "Active" ]] && break` — a false test makes the whole
	//      statement exit 1, and set -e kills the worker on the FIRST poll
	//      of a not-yet-Active workspace (i.e. every workspace, at t0)
	// Both leave no .ms file, so the p95 sentinel-fills to 999999. Pin the
	// safe forms structurally.
	src := mustRead(t, us70DeliveryScript)
	if strings.Contains(src, `]] && break`) {
		t.Fatalf("delivery script must not use `[[ ... ]] && break` (set -e kills the statement on a false test); use if/then")
	}
	if !strings.Contains(src, `activate" >/dev/null 2>&1 || true`) {
		t.Fatalf("stopwatch activate curl must be `|| true`-guarded — the phase poll is the source of truth, a non-2xx activate must not kill the worker")
	}
	if !strings.Contains(src, `if [[ "$p" == "Active" ]]; then break; fi`) {
		t.Fatalf("stopwatch phase poll must use the if/then break form")
	}
	if !strings.Contains(src, `> "${TDIR}/${ws}.ms"`) {
		t.Fatalf("stopwatch workers must write one integer to TDIR/<ws>.ms (never `wait $pid` stdout capture)")
	}
}

// TestUS70SweepSelection pins the sweep id-selection arithmetic against
// names rendered with the REAL ws_id() format (r25: two regex attempts
// matched zero names — a 32-char prefix + %04d, not the 36-char base).
func TestUS70SweepSelection(t *testing.T) {
	render := func(id int) string {
		base := "e2e5d000-0000-4000-8000-000000000000"
		return fmt.Sprintf("%s%04d", base[:32], id)
	}
	sweepPre := func(name string) bool { // pre-wave: ids 90-92
		n := strings.TrimPrefix(name, "workspace/")
		if !strings.HasPrefix(n, "e2e5d000-0000-4000-8000-") {
			return false
		}
		suffix := n[len(n)-4:]
		for _, c := range suffix {
			if c < '0' || c > '9' {
				return false
			}
		}
		id, _ := strconv.Atoi(suffix)
		return id >= 90 && id <= 92
	}
	sweepPost := func(name string) bool { // post-wave: ids >= 101
		n := strings.TrimPrefix(name, "workspace/")
		if !strings.HasPrefix(n, "e2e5d000-0000-4000-8000-") {
			return false
		}
		suffix := n[len(n)-4:]
		for _, c := range suffix {
			if c < '0' || c > '9' {
				return false
			}
		}
		id, _ := strconv.Atoi(suffix)
		return id >= 101
	}
	// render sanity: the exact production shape
	if render(90) != "e2e5d000-0000-4000-8000-000000000090" || render(101) != "e2e5d000-0000-4000-8000-000000000101" {
		t.Fatalf("ws_id render drift: %q %q", render(90), render(101))
	}
	// pre-wave selects exactly {90,91,92} of ids 0..300
	var pre []int
	for id := 0; id <= 300; id++ {
		if sweepPre(render(id)) {
			pre = append(pre, id)
		}
	}
	if len(pre) != 3 || pre[0] != 90 || pre[1] != 91 || pre[2] != 92 {
		t.Fatalf("pre-wave selection = %v, want [90 91 92]", pre)
	}
	// post-wave selects every id >= 101 (incl. 200 at scale 100, and 1000+)
	for _, id := range []int{101, 140, 199, 200, 240, 999, 1000} {
		if !sweepPost(render(id)) {
			t.Fatalf("post-wave must select id %d", id)
		}
	}
	for _, id := range []int{0, 1, 9, 90, 92, 100} {
		if sweepPost(render(id)) {
			t.Fatalf("post-wave must NOT select id %d", id)
		}
	}
	// BOTH production awks are extracted from the script and executed
	// (r26: five rounds of inert sweeps — pins must run production code,
	// not reimplementations).
	script, err := os.ReadFile("us-70-secret-delivery-e2e.sh")
	if err != nil {
		t.Fatalf("script not readable from test cwd (r31: fail, never silent-skip): %v", err)
	}
	preRe := regexp.MustCompile(`(?s)PRE_SWEPT=.*?awk -F/ '(.*?)'`)
	mPre := preRe.FindStringSubmatch(string(script))
	if mPre == nil {
		t.Fatal("pre-wave awk not found in us-70-secret-delivery-e2e.sh — did the sweep change shape?")
	}
	preProgram := mPre[1]
	for _, tc := range []struct {
		id   int
		want bool
	}{
		{89, false}, {90, true}, {91, true}, {92, true}, {93, false}, {100, false}, {101, false}, {200, false},
	} {
		cmd := exec.Command("awk", "-F/", preProgram)
		cmd.Stdin = strings.NewReader("workspace/" + render(tc.id) + "\n")
		out, _ := cmd.Output()
		got := strings.TrimSpace(string(out)) != ""
		if got != tc.want {
			t.Fatalf("PRODUCTION pre-wave awk for id %d: got %v want %v", tc.id, got, tc.want)
		}
	}
	postRe := regexp.MustCompile(`(?s)POST_SWEPT=.*?awk -F/ '(.*?)'`)
	mPost := postRe.FindStringSubmatch(string(script))
	if mPost == nil {
		t.Fatal("post-wave awk not found in us-70-secret-delivery-e2e.sh")
	}
	postProgram := mPost[1]
	// count-expression pin (r32 wording corrected): TEXTUALLY asserts the
	// production expressions (reverting either count line to wc -l FAILS
	// — proven by falsification both ways) and separately executes both
	// forms to demonstrate the undercount. Not extract-and-execute like
	// the awk pins — the guarantee is identical, the mechanism is not.
	if !regexp.MustCompile(`(?s)post-wave sweep deleted: \$\(printf '%s\\n' "\$\{POST_SWEPT\}" \| grep -c \.`).MatchString(string(script)) {
		t.Fatal("post-wave production count expression not found (or regressed to wc -l) — the off-by-one class returns")
	}
	if !regexp.MustCompile(`(?m)PRE_N=\$\(printf '%s' "\$\{PRE_SWEPT\}" \| grep -c \.`).MatchString(string(script)) {
		t.Fatal("pre-wave production count expression not found")
	}
	// execute both forms: old undercounts, production is exact
	sh := exec.Command("bash", "-c", `N=$(printf '%s' "a
b
c"); old=$(printf '%s' "$N" | wc -l); new=$(printf '%s\n' "$N" | grep -c .); echo "$old $new"`)
	out, err := sh.Output()
	if err != nil {
		t.Fatalf("count probe failed: %v", err)
	}
	lines := strings.Fields(string(out))
	if len(lines) != 2 {
		t.Fatalf("count pin output shape: %v", lines)
	}
	if lines[0] != "2" {
		t.Fatalf("wc -l undercount not reproduced: %s", lines[0])
	}
	if lines[1] != "3" {
		t.Fatalf("production count form broken: %s", lines[1])
	}

	for _, tc := range []struct {
		name string
		want bool
	}{
		{"workspace/" + render(92), false},
		{"workspace/" + render(93), false},
		{"workspace/" + render(101), true},
		{"workspace/" + render(200), true},
		{"workspace/other-000000000200", false},
	} {
		cmd := exec.Command("awk", "-F/", postProgram)
		cmd.Stdin = strings.NewReader(tc.name + "\n")
		out, _ := cmd.Output()
		got := strings.TrimSpace(string(out)) != ""
		if got != tc.want {
			t.Fatalf("awk post-wave selection for %s: got %v want %v", tc.name, got, tc.want)
		}
	}
}

// TestUS70AC1BRow_XDGLayerClassification executes the AC-1b row's ACTUAL
// probe text (extracted from the script — r2: an inline re-implementation
// stays green while the script drifts) against a temp dir: a writable
// regular file must classify `file`, a read-only file `readonly` (the
// EACCES class 1d0e5be1 exists to close), a symlink `symlink` (the
// 0.27.5-era mechanism), an absent path `missing`.
func TestUS70AC1BRow_XDGLayerClassification(t *testing.T) {
	bash := requireBash(t)
	src := mustRead(t, us70DeliveryScript)
	// The remote probe is the double-quoted sh -c payload of the XDG_KIND
	// capture line; its own quoting is single-quoted, so [^"]+ is exact.
	m := regexp.MustCompile(`XDG_KIND=\$\(kc exec[^\n]* -- sh -c "([^"]+)"`).
		FindStringSubmatch(src)
	if m == nil {
		t.Fatalf("AC-1b XDG_KIND probe not found in %s in the expected capture form", us70DeliveryScript)
	}
	probe := m[1]
	dir := t.TempDir()
	writable := filepath.Join(dir, "writable.json")
	readOnly := filepath.Join(dir, "readonly.json")
	link := filepath.Join(dir, "symlink.json")
	absent := filepath.Join(dir, "absent.json")
	for _, p := range []string{writable, readOnly} {
		if err := os.WriteFile(p, []byte("{}"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Chmod(readOnly, 0o444); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(writable, link); err != nil {
		t.Fatal(err)
	}
	// The row expands ${XDG_CFG} in the OUTER shell before the remote
	// sh -c sees it (single quotes preserve the literal) — replicate
	// that substitution per fixture, exactly as kc exec would deliver.
	var got []string
	for _, p := range []string{writable, readOnly, link, absent} {
		out, err := exec.Command(bash, "-c", strings.ReplaceAll(probe, "${XDG_CFG}", p)).CombinedOutput()
		if err != nil {
			t.Fatalf("execute extracted probe (%q) on %s: %v\n%s", probe, p, err, out)
		}
		got = append(got, strings.TrimSpace(string(out)))
	}
	joined := strings.Join(got, " ")
	if joined != "file readonly symlink missing" {
		t.Fatalf("extracted probe must classify writable/readonly/symlink/absent in order, got %q (probe=%q)", joined, probe)
	}
}

// TestUS70AC1BRow_SeedGrepSemantics executes the AC-1b seeded-copy grep
// semantics as the script runs them (r2 finding: `grep -c` exits 1 on
// zero matches, so the guard must distinguish transport failure from a
// legitimate zero count — the `; true` normalization). Pins BOTH arms:
// count 0 → the content die's condition fires; count ≥1 → passes; any
// nonzero command exit WITHOUT normalization would invert the arms.
func TestUS70AC1BRow_SeedGrepSemantics(t *testing.T) {
	bash := requireBash(t)
	src := mustRead(t, us70DeliveryScript)
	m := regexp.MustCompile(`XDG_SEED=\$\(kc exec[^\n]* -- sh -c "(grep -c [^"]+)"`).
		FindStringSubmatch(src)
	if m == nil {
		t.Fatalf("AC-1b XDG_SEED grep not found in %s in the expected capture form", us70DeliveryScript)
	}
	cmd := m[1]
	if !strings.Contains(cmd, "; true") {
		t.Fatalf("the seeded-copy grep must normalize the remote exit (`; true`) — grep -c exits 1 on zero matches and would fire the exec-failure die on the CONTENT path: %q", cmd)
	}
	dir := t.TempDir()
	seeded := filepath.Join(dir, "seeded.json")
	bare := filepath.Join(dir, "bare.json")
	if err := os.WriteFile(seeded, []byte(`{"providers":{"ac1b-stub":{}}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(bare, []byte(`{}`), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		path string
		want bool
	}{
		{seeded, true},
		{bare, false},
	} {
		out, err := exec.Command(bash, "-c", fmt.Sprintf("%s; echo exit=$?", strings.ReplaceAll(cmd, "${XDG_CFG}", tc.path))).CombinedOutput()
		if err != nil {
			t.Fatalf("execute seeded-grep: %v\n%s", err, out)
		}
		text := strings.TrimSpace(string(out))
		n, perr := strconv.Atoi(strings.Split(text, "\nexit=")[0])
		if perr != nil {
			t.Fatalf("seeded-grep output not a count: %q", text)
		}
		if (n >= 1) != tc.want {
			t.Fatalf("seeded-grep on %s: count=%d, want seeded=%v (output %q)", tc.path, n, tc.want, text)
		}
	}
}

// TestUS70AC1BRow_CopyContractPins pins the AC-1b row's copy-contract
// structurally: the symlink-equality form that 1d0e5be1 invalidated must
// stay gone, and the copy-contract assertions (writable-file case arms
// and the seeded-copy grep) must stay present — the row was silently red
// for two days precisely because nothing pinned it.
func TestUS70AC1BRow_CopyContractPins(t *testing.T) {
	src := mustRead(t, us70DeliveryScript)
	if strings.Contains(src, "readlink -f /home/sandbox/.config/opencode/opencode.json") {
		t.Fatalf("AC-1b must not carry the symlink-equality form — 1d0e5be1 replaced the symlink with a copy; the row pins the copy contract")
	}
	for _, pin := range []string{
		`[ -f '${XDG_CFG}' ] && [ -w '${XDG_CFG}' ]`,
		"XDG registry layer at ${XDG_CFG} is a READ-ONLY regular file",
		"the 0.27.5-era mechanism",
		"XDG registry layer MISSING at ${XDG_CFG}",
		"not seeded from the live config",
	} {
		if !strings.Contains(src, pin) {
			t.Fatalf("AC-1b copy-contract pin missing from %s: %q", us70DeliveryScript, pin)
		}
	}
}

// TestUS70AC1BRow_CaseBlockExecutes pins the row's central decision
// expression by EXECUTION (r3: the structural string pins don't run the
// logic — a mutated case arm could keep the strings elsewhere): the
// script's actual `case "${XDG_KIND}" in … esac` block is extracted and
// run against every classification input with a stubbed die, asserting
// each arm's message. `file` must fall through clean.
func TestUS70AC1BRow_CaseBlockExecutes(t *testing.T) {
	bash := requireBash(t)
	src := mustRead(t, us70DeliveryScript)
	caseBlock := regexp.MustCompile("(?s)case \"\\$\\{XDG_KIND\\}\" in\n.*?\nesac\n").FindString(src)
	if caseBlock == "" {
		t.Fatalf("AC-1b XDG_KIND case block not found in %s", us70DeliveryScript)
	}
	wants := []struct{ kind, msg string }{
		{"file", ""}, // clean fall-through
		{"readonly", "READ-ONLY regular file"},
		{"symlink", "the 0.27.5-era mechanism"},
		{"missing", "never installed the copy"},
		{"weird", "unexpected"},
	}
	for _, w := range wants {
		script := `die() { echo "DIE:$*"; exit 1; }
XDG_CFG=/probe/path
XDG_KIND='` + w.kind + `'
` + caseBlock + "\n" + `echo OK
`
		out, err := exec.Command(bash, "-c", script).CombinedOutput()
		got := strings.TrimSpace(string(out))
		if w.msg == "" {
			if err != nil || got != "OK" {
				t.Fatalf("classification %q must fall through clean, got err=%v out=%q", w.kind, err, got)
			}
			continue
		}
		if err == nil || !strings.Contains(got, w.msg) {
			t.Fatalf("classification %q must die with %q, got err=%v out=%q", w.kind, w.msg, err, got)
		}
	}
}

// TestUS70AC1BRow_GuardDiscipline pins the r13/r30 capture guards
// structurally (r3: reverting both `if !` guards left the suite green —
// nothing else would catch the silent leg-killer regression).
func TestUS70AC1BRow_GuardDiscipline(t *testing.T) {
	src := mustRead(t, us70DeliveryScript)
	for _, pin := range []string{
		`if ! XDG_KIND=$(kc exec`,
		`if ! XDG_SEED=$(kc exec`,
	} {
		if !strings.Contains(src, pin) {
			t.Fatalf("AC-1b capture guard missing from %s: %q — an unguarded kc exec under set -Eeuo pipefail kills the whole pool leg on a transient failure (r13/r30)", us70DeliveryScript, pin)
		}
	}
}

// TestUS70AC1BRow_SeedDecisionExecutes runs the script's ACTUAL seed
// guard + decision expression (r4: `-ge 1`→`-ge 2` mutation left the
// suite green — the threshold was Go-re-implemented, not executed).
// A fake kc serves the captured value; every outcome is asserted
// against the script's own die branches.
func TestUS70AC1BRow_SeedDecisionExecutes(t *testing.T) {
	bash := requireBash(t)
	src := mustRead(t, us70DeliveryScript)
	block := regexp.MustCompile(`(?s)if ! XDG_SEED=\$\(kc exec.*?\nfi\n\[\[.*?\n.*?\n`).
		FindString(src)
	if block == "" {
		t.Fatalf("AC-1b seed guard+decision block not found in %s", us70DeliveryScript)
	}
	for _, tc := range []struct {
		name, kcOut, want string
		kcExit            int
	}{
		{"seeded (count 1) passes", "1", "", 0},
		{"seeded (count 3) passes", "3", "", 0},
		{"not seeded (count 0) dies with the content message", "0", "not seeded from the live config", 0},
		{"transport failure dies with the exec message", "Error from server: timeout", "kc exec failed grepping", 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			script := `set -u
die() { echo "DIE:$*"; exit 1; }
POD1B=pod-fake
XDG_CFG=/probe/path
kc() { printf '%s\n' '` + tc.kcOut + `'; return ` + strconv.Itoa(tc.kcExit) + `; }
` + block + "\necho OK\n"
			out, err := exec.Command(bash, "-c", script).CombinedOutput()
			got := strings.TrimSpace(string(out))
			if tc.want == "" {
				if err != nil || got != "OK" {
					t.Fatalf("must pass clean, got err=%v out=%q", err, got)
				}
				return
			}
			if err == nil || !strings.Contains(got, tc.want) {
				t.Fatalf("must die with %q, got err=%v out=%q", tc.want, err, got)
			}
		})
	}
}

// TestUS70MockLLM_ToolEchoPins pins the AC-1e mock's tools-echo contract
// (the row was born unsatisfiable in 75a3f802: the mock always replied
// the canned marker, so the llmsafespaces_ grep could never match —
// invisible because AC-1b died earlier in every run since). The echo
// block is EXTRACTED from the serve.py embedded in the delivery script
// (r5: an inline Go/python duplicate stays green while the script
// drifts — the exact anti-pattern this PR's r1→r2 history outlawed) and
// EXECUTED against the three discriminating request shapes; the
// MOCK-TURN-OK marker must stay first (AC-1d greps it as a substring).
func TestUS70MockLLM_ToolEchoPins(t *testing.T) {
	src := mustRead(t, us70DeliveryScript)
	// The echo block inside do_POST: from the marker assignment through
	// the except-pass that bounds it. Executed verbatim (dedented).
	block := regexp.MustCompile("(?s)[ ]+marker = \"MOCK-TURN-OK\".*?\n[ ]+except Exception:\n[ ]+pass\n").
		FindString(src)
	if block == "" {
		t.Fatalf("mock serve.py echo block not found in %s — the tools-echo contract is AC-1e's grep target", us70DeliveryScript)
	}
	// Dedent by the COMMON leading-whitespace prefix only (the block's
	// internal try/for nesting must survive).
	lines := strings.Split(strings.TrimPrefix(block, "\n"), "\n")
	minIndent := -1
	for _, l := range lines {
		if strings.TrimSpace(l) == "" {
			continue
		}
		n := len(l) - len(strings.TrimLeft(l, " "))
		if minIndent < 0 || n < minIndent {
			minIndent = n
		}
	}
	var dedentB strings.Builder
	for _, l := range lines {
		if len(l) >= minIndent {
			l = l[minIndent:]
		}
		dedentB.WriteString(l + "\n")
	}
	dedented := dedentB.String()
	wrapper := "import json\n" +
		"def render(raw):\n" +
		"    body = raw\n" +
		indentLines(dedented, 4) +
		"    return marker\n" +
		"import sys\n" +
		"for line in sys.stdin:\n" +
		"    print(render(line.strip().encode()))\n" +
		"    print(\"---END---\")\n"
	if _, lookErr := exec.LookPath("python3"); lookErr != nil {
		t.Skipf("python3 unavailable — structural extraction was still enforced above")
	}
	cmd := exec.Command("python3", "-c", wrapper)
	cmd.Stdin = strings.NewReader(`{"tools":[{"type":"function","function":{"name":"llmsafespaces_session_list"}},{"type":"function","function":{"name":"bash"}}]}
{"tools":[{"name":"llmsafespaces_dev_preview_url"}]}
{"messages":[]}
`)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("the EXTRACTED echo block failed to execute (%v) — this pin exists to catch exactly that regression: %s", err, out)
	}
	blocks := strings.Split(strings.TrimSpace(string(out)), "---END---")
	results := make([]string, 0, 3)
	for _, b := range blocks {
		if strings.TrimSpace(b) != "" {
			results = append(results, strings.TrimSpace(b))
		}
	}
	if len(results) != 3 {
		t.Fatalf("expected 3 outputs, got %d: %q", len(results), string(out))
	}
	if !strings.Contains(results[0], "llmsafespaces_session_list") || !strings.HasPrefix(results[0], "MOCK-TURN-OK") {
		t.Fatalf("chat-completions shape must echo function names after the marker, got %q", results[0])
	}
	if !strings.Contains(results[1], "llmsafespaces_dev_preview_url") {
		t.Fatalf("flat-name shape must echo the tool name, got %q", results[1])
	}
	if results[2] != "MOCK-TURN-OK" {
		t.Fatalf("tools-less request must yield the bare marker (the V2-steer regression shape AC-1e must catch), got %q", results[2])
	}
}

// indentLines prefixes every non-empty line with n spaces.
func indentLines(s string, n int) string {
	pad := strings.Repeat(" ", n)
	var b strings.Builder
	for _, line := range strings.Split(s, "\n") {
		if strings.TrimSpace(line) == "" {
			b.WriteString("\n")
			continue
		}
		b.WriteString(pad + line + "\n")
	}
	return b.String()
}
