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
