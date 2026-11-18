// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package app

// wait_broker_scripts_test.go — the three in-tree gate templates'
// pins (claim-wait lifecycle, low-level design §4.3 + §6):
//
//   - BYTE pins: every template renders deterministically (the exact
//     compiled shape — URLs, seed, marker path, the template version —
//     so a template change is a review-visible diff, never a drift).
//   - EXECUTABLE pins: the rendered handlers run under python3 against
//     stub servers — the fire-once discipline (Pattern 3: two polls of
//     an unchanged satisfied observation => exactly ONE proceed; the
//     marker persists past proceed), the re-declaration re-arm (a new
//     seed fires again on an unchanged condition), and the quiet
//     poller posture (transient errors skip, never fail).
//
// The executable pins skip when python3 is absent (the byte pins are
// unconditional — scriptwrap's own runtime contract already gates on
// the interpreter).

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/lenaxia/llmsafespaces/pkg/board"
)

func renderForTest(t *testing.T, primitive, arg, repo, eventID, marker, boardBase string) string {
	t.Helper()
	script, err := renderWaitGateScript(board.WaitCondition{Primitive: primitive, Arg: arg}, repo, eventID, marker, boardBase, "tsk-pristine-henna-loses")
	require.NoError(t, err)
	return script
}

func TestWaitGateScriptRender_BytePins(t *testing.T) {
	marker := waitBrokerMarkerPath("tsk-pristine-henna-loses", "11111111-1111-1111-1111-111111111111")

	// pr_merged: the exact compiled shape (URL from the declaration's
	// repo — the broker never guesses; seed = template version + event
	// id; the marker path is the flat per-episode exchange file).
	pr := renderForTest(t, "pr_merged", "2101", "lenaxia/llmsafespaces", "wait:T1:tok:123", marker, "")
	require.Contains(t, pr, `PR_URL = "https://api.github.com/repos/lenaxia/llmsafespaces/pulls/2101"`)
	require.Contains(t, pr, `REPO = "lenaxia/llmsafespaces"`)
	require.Contains(t, pr, `PR = "2101"`)
	require.Contains(t, pr, `MARKER = "`+marker+`"`)
	require.Contains(t, pr, `SEED = "`+waitGateSeed("wait:T1:tok:123")+`"`)
	require.Contains(t, pr, waitGateTemplateVersion, "the template version rides the rendered script")
	require.NotContains(t, pr, "__", "no placeholder survives rendering")
	require.Contains(t, pr, "skipped_error", "the quiet poller posture is compiled in")

	// ci_green: the PR fetch + the head-SHA check-runs digest.
	ci := renderForTest(t, "ci_green", "2101", "lenaxia/llmsafespaces", "wait:T1:tok:456", marker, "")
	require.Contains(t, ci, `PR_URL = "https://api.github.com/repos/lenaxia/llmsafespaces/pulls/2101"`)
	require.Contains(t, ci, `CHECK_URL = "https://api.github.com/repos/lenaxia/llmsafespaces/commits/%s/check-runs"`)
	require.Contains(t, ci, `OK = ("success", "skipped", "neutral")`)
	require.Contains(t, ci, `SEED = "`+waitGateSeed("wait:T1:tok:456")+`"`)
	require.NotContains(t, ci, "__")

	// story_done: the in-cluster board read (the broker's rendered
	// default; the pod's own env wins at fire time).
	sd := renderForTest(t, "story_done", "tsk-ebony-javelin-tramps", "", "wait:T1:tok:789", marker, "http://api.internal:8080")
	require.Contains(t, sd, `BOARD_URL = "http://api.internal:8080/internal/v1/automation/board/tasks/tsk-ebony-javelin-tramps"`)
	require.Contains(t, sd, `base = os.environ.get("LLMSAFESPACE_API_URL") or "http://api.internal:8080"`)
	require.Contains(t, sd, `LLMSAFESPACE_BOOTSTRAP_TOKEN_FILE`)
	require.Contains(t, sd, `/var/run/bootstrap/token`)
	require.Contains(t, sd, `SEED = "`+waitGateSeed("wait:T1:tok:789")+`"`)
	require.NotContains(t, sd, "__")
}

func TestWaitGateScriptRender_Reproducible(t *testing.T) {
	marker := waitBrokerMarkerPath("tsk-x", "22222222-2222-2222-2222-222222222222")
	for _, prim := range []string{"pr_merged", "ci_green", "story_done"} {
		a := renderForTest(t, prim, "2101", "lenaxia/llmsafespaces", "wait:E:tok:1", marker, "http://api")
		b := renderForTest(t, prim, "2101", "lenaxia/llmsafespaces", "wait:E:tok:1", marker, "http://api")
		require.Equal(t, a, b, "%s: identical inputs render identical bytes (cross-replica convergence prerequisite)", prim)
	}
}

func TestRenderWaitGateScript_RefusesUnknownPrimitive(t *testing.T) {
	_, err := renderWaitGateScript(board.WaitCondition{Primitive: "rm_rf", Arg: "/"}, "o/r", "evt", "/tmp/m", "http://api", "tsk-x")
	require.Error(t, err)
	require.Contains(t, err.Error(), "closed vocabulary")
}

func TestClaimTokenHash8_NameShapes(t *testing.T) {
	h := claimTokenHash8("11111111-1111-1111-1111-111111111111")
	require.Len(t, h, 8)
	name := waitBrokerTriggerName("tsk-pristine-henna-loses", "11111111-1111-1111-1111-111111111111")
	require.True(t, strings.HasPrefix(name, waitBrokerTriggerPrefix))
	short, hash, ok := parseWaitBrokerTriggerName(name)
	require.True(t, ok)
	require.Equal(t, "tsk-pristine-henna-loses", short)
	require.Equal(t, h, hash)
	// Different tokens never collide on the name (episode separation).
	require.NotEqual(t, waitBrokerTriggerName("tsk-x", "11111111-1111-1111-1111-111111111111"),
		waitBrokerTriggerName("tsk-x", "33333333-3333-3333-3333-333333333333"))
	// Refusals: foreign names, wrong suffix length, non-hex.
	_, _, ok = parseWaitBrokerTriggerName("user-cron-nightly")
	require.False(t, ok)
	_, _, ok = parseWaitBrokerTriggerName(waitBrokerTriggerPrefix + "tsk-x-abc")
	require.False(t, ok)
	_, _, ok = parseWaitBrokerTriggerName(waitBrokerTriggerPrefix + "tsk-x-zzzzzzzz")
	require.False(t, ok)
	// The DB's name grammar admits every rendered name (the CHECK's
	// shape — no slashes, bounded length).
	require.Len(t, name, len(waitBrokerTriggerPrefix)+len("tsk-pristine-henna-loses")+1+8)
}

// --- the executable harness (scriptwrap's contract, mirrored) --------

func python3Available() bool {
	_, err := exec.LookPath("python3")
	return err == nil
}

// runGateHandler executes one rendered handler under python3 with the
// fire envelope on stdin (the wrapper scriptwrap generates — import
// handler, JSON in, JSON out) and returns the parsed {proceed, vars}.
func runGateHandler(t *testing.T, handlerSrc, markerPath string) map[string]any {
	t.Helper()
	dir := t.TempDir()
	handler := filepath.Join(dir, "handler.py")
	wrapper := filepath.Join(dir, "wrapper.py")
	require.NoError(t, os.WriteFile(handler, []byte(handlerSrc), 0o600))
	require.NoError(t, os.WriteFile(wrapper, []byte(`import json, sys, os
sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
from handler import handler
_input = json.loads(sys.stdin.read())
_result = handler(_input)
sys.stdout.write(json.dumps(_result))
`), 0o600))
	envelope, _ := json.Marshal(map[string]any{"source": "cron", "received_at": "2026-11-18T00:00:00Z"})
	cmd := exec.Command("python3", wrapper) //nolint:gosec // G204: fixed interpreter on a temp wrapper the test itself wrote
	cmd.Stdin = strings.NewReader(string(envelope))
	cmd.Env = append(os.Environ(), "GITHUB_TOKEN=", "GH_TOKEN=")
	out, err := cmd.Output()
	require.NoError(t, err, "handler output: %s", string(out))
	var parsed map[string]any
	require.NoError(t, json.Unmarshal(out, &parsed))
	return parsed
}

func proceedOf(t *testing.T, res map[string]any) bool {
	t.Helper()
	v, ok := res["proceed"].(bool)
	require.True(t, ok, "the handler must return a bool proceed: %v", res)
	return v
}

// TestGateTemplate_StoryDone_SatisfiedState_FiresOnce — THE design
// pin (§4.3): two polls, unchanged satisfied observation => exactly
// one proceed; the marker persists past proceed (a third poll still
// skips).
func TestGateTemplate_StoryDone_SatisfiedState_FiresOnce(t *testing.T) {
	if !python3Available() {
		t.Skip("python3 absent — the executable gate pins need the interpreter")
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/internal/v1/automation/board/tasks/tsk-ebony-javelin-tramps", r.URL.Path)
		_ = json.NewEncoder(w).Encode(map[string]any{"stage": "done", "updated_at": "2026-11-18T01:02:03Z"})
	}))
	defer srv.Close()

	marker := filepath.Join(t.TempDir(), "wait-broker-story-done.marker")
	script := renderForTest(t, "story_done", "tsk-ebony-javelin-tramps", "", "wait:E1:tok:1", marker, srv.URL)

	require.True(t, proceedOf(t, runGateHandler(t, script, marker)), "poll 1: the satisfied state proceeds")
	require.False(t, proceedOf(t, runGateHandler(t, script, marker)), "poll 2: unchanged observation skips (the marker dedupe)")
	require.False(t, proceedOf(t, runGateHandler(t, script, marker)), "poll 3: the marker PERSISTS past proceed — never re-armed")
	_, err := os.Stat(marker)
	require.NoError(t, err, "the marker file outlives its fire (the dedupe state)")
}

// TestGateTemplate_StoryDone_UnsatisfiedMarksQuietly — an unsatisfied
// observation still marks (the transition baseline), and the later
// transition into done fires exactly once.
func TestGateTemplate_StoryDone_UnsatisfiedThenSatisfied(t *testing.T) {
	if !python3Available() {
		t.Skip("python3 absent — the executable gate pins need the interpreter")
	}
	stage := "in_progress"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"stage": stage, "updated_at": "2026-11-18T01:02:03Z"})
	}))
	defer srv.Close()

	marker := filepath.Join(t.TempDir(), "m.marker")
	script := renderForTest(t, "story_done", "tsk-ebony-javelin-tramps", "", "wait:E1:tok:2", marker, srv.URL)

	require.False(t, proceedOf(t, runGateHandler(t, script, marker)), "not done yet")
	require.False(t, proceedOf(t, runGateHandler(t, script, marker)), "still not done — quiet")
	stage = "done"
	require.True(t, proceedOf(t, runGateHandler(t, script, marker)), "the transition fires")
	require.False(t, proceedOf(t, runGateHandler(t, script, marker)), "and only once")
}

// TestGateTemplate_PRMerged_MergedFiresOnce — the merge observation:
// unmerged (null SHA) marks the baseline; the merge fires once with
// the SHA in vars; the poll after skips.
func TestGateTemplate_PRMerged_MergedFiresOnce(t *testing.T) {
	if !python3Available() {
		t.Skip("python3 absent — the executable gate pins need the interpreter")
	}
	merged := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		resp := map[string]any{"number": 2101, "merged": merged}
		if merged {
			resp["merge_commit_sha"] = "abc123def456"
		}
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()
	old := waitBrokerGitHubAPI
	waitBrokerGitHubAPI = srv.URL
	t.Cleanup(func() { waitBrokerGitHubAPI = old })

	marker := filepath.Join(t.TempDir(), "m.marker")
	script := renderForTest(t, "pr_merged", "2101", "lenaxia/llmsafespaces", "wait:E1:tok:3", marker, "")

	require.False(t, proceedOf(t, runGateHandler(t, script, marker)), "unmerged: quiet (the null-SHA baseline marks)")
	require.False(t, proceedOf(t, runGateHandler(t, script, marker)), "still unmerged: quiet")
	merged = true
	res := runGateHandler(t, script, marker)
	require.True(t, proceedOf(t, res), "the merge fires")
	vars, _ := res["vars"].(map[string]any)
	require.Equal(t, "abc123def456", vars["merge_commit_sha"], "vars carry the merge SHA")
	require.Equal(t, "2101", vars["pr"])
	require.False(t, proceedOf(t, runGateHandler(t, script, marker)), "the long-merged PR fires ONCE — never every 2-min poll")
}

// TestGateTemplate_CIGreen_AllGreenFiresOnce — pending/failing checks
// stay quiet; the all-green transition fires once; a later FAILURE
// state change re-arms (the digest moves) but does not proceed.
func TestGateTemplate_CIGreen_AllGreenFiresOnce(t *testing.T) {
	if !python3Available() {
		t.Skip("python3 absent — the executable gate pins need the interpreter")
	}
	conclusions := []string{"failure"}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/pulls/2101") {
			_ = json.NewEncoder(w).Encode(map[string]any{"number": 2101, "head": map[string]any{"sha": "deadbeef"}})
			return
		}
		runs := make([]map[string]any, len(conclusions))
		for i, c := range conclusions {
			runs[i] = map[string]any{"name": fmt.Sprintf("check-%d", i), "conclusion": c}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"total": len(runs), "check_runs": runs})
	}))
	defer srv.Close()
	old := waitBrokerGitHubAPI
	waitBrokerGitHubAPI = srv.URL
	t.Cleanup(func() { waitBrokerGitHubAPI = old })

	marker := filepath.Join(t.TempDir(), "m.marker")
	script := renderForTest(t, "ci_green", "2101", "lenaxia/llmsafespaces", "wait:E1:tok:4", marker, "")

	require.False(t, proceedOf(t, runGateHandler(t, script, marker)), "failing checks: quiet")
	conclusions = []string{"success", "skipped", "neutral"}
	res := runGateHandler(t, script, marker)
	require.True(t, proceedOf(t, res), "every conclusion success|skipped|neutral fires")
	vars, _ := res["vars"].(map[string]any)
	require.Equal(t, "deadbeef", vars["head_sha"])
	require.Equal(t, "3", vars["checks_total"])
	require.False(t, proceedOf(t, runGateHandler(t, script, marker)), "unchanged green: skip")

	// total==0 never proceeds (no checks is not green).
	conclusions = []string{}
	require.False(t, proceedOf(t, runGateHandler(t, script, marker)), "zero checks: not green — and the digest change does not proceed")
}

// TestGateTemplate_ReDeclaredSeed_Refires — the re-declaration belt:
// a NEW event id (fresh seed) re-arms the sensor even though the
// condition and the marker's observed state are unchanged — never the
// fire-never stale-suppression shape.
func TestGateTemplate_ReDeclaredSeed_Refires(t *testing.T) {
	if !python3Available() {
		t.Skip("python3 absent — the executable gate pins need the interpreter")
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"number": 2101, "merged": true, "merge_commit_sha": "abc123def456"})
	}))
	defer srv.Close()
	old := waitBrokerGitHubAPI
	waitBrokerGitHubAPI = srv.URL
	t.Cleanup(func() { waitBrokerGitHubAPI = old })

	marker := filepath.Join(t.TempDir(), "m.marker")
	first := renderForTest(t, "pr_merged", "2101", "lenaxia/llmsafespaces", "wait:E1:tok:5", marker, "")
	require.True(t, proceedOf(t, runGateHandler(t, first, marker)))
	require.False(t, proceedOf(t, runGateHandler(t, first, marker)), "same declaration: once")

	redeclared := renderForTest(t, "pr_merged", "2101", "lenaxia/llmsafespaces", "wait:E2:tok:6", marker, "")
	require.True(t, proceedOf(t, runGateHandler(t, redeclared, marker)),
		"a re-declared wait (new seed) fires again on the unchanged condition — same marker file, new signature")
	require.False(t, proceedOf(t, runGateHandler(t, redeclared, marker)))
}

// TestGateTemplate_QuietPoller — transport failures skip the fire
// (proceed=false with the skipped_error var) and write NO marker: the
// schedule is the retry.
func TestGateTemplate_QuietPoller(t *testing.T) {
	if !python3Available() {
		t.Skip("python3 absent — the executable gate pins need the interpreter")
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()
	old := waitBrokerGitHubAPI
	waitBrokerGitHubAPI = srv.URL
	t.Cleanup(func() { waitBrokerGitHubAPI = old })

	marker := filepath.Join(t.TempDir(), "m.marker")
	script := renderForTest(t, "pr_merged", "2101", "lenaxia/llmsafespaces", "wait:E1:tok:7", marker, "")
	res := runGateHandler(t, script, marker)
	require.False(t, proceedOf(t, res), "a 500 skips — never a failed fire")
	require.Contains(t, res["vars"], "skipped_error")
	_, err := os.Stat(marker)
	require.True(t, os.IsNotExist(err), "no observation landed — no marker written")
}
