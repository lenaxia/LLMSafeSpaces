// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package local_test

// exchange_dir_script_test.go — structure pins for
// local/exchange-dir-e2e.sh (design 0070 §2.4a), the same philosophy
// as dev_preview_script_test.go: the script is CI glue on a real kind
// cluster; what is pinnable deterministically is the structure past
// failures actually broke — bash syntax, the assertion rows that must
// exist so they cannot be silently dropped, and the wiring facts the
// rows depend on (the mux port-forward, the workspace-password
// secret, the topology probe).
//
// The script itself is the ONLY dual-uid EXECUTION of the exchange
// contract in the repo (single-uid test processes cannot cross uids;
// the agentd sidecar image is FROM scratch — no shell to exec), which
// is exactly why its rows are pinned here.

import (
	"os"
	"os/exec"
	"strings"
	"testing"
)

func exchangeDirScript(t *testing.T) string {
	t.Helper()
	path := "../local/exchange-dir-e2e.sh"
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("script missing: %v", err)
	}
	return path
}

func TestExchangeDirScript_BashSyntax(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash unavailable")
	}
	out, err := exec.Command("bash", "-n", exchangeDirScript(t)).CombinedOutput()
	if err != nil {
		t.Fatalf("bash -n failed: %v\n%s", err, out)
	}
}

func TestExchangeDirScript_AssertionRows(t *testing.T) {
	body, err := os.ReadFile(exchangeDirScript(t))
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	s := string(body)
	for _, row := range []struct{ label, needle string }{
		{"X1 topology probe", `TOPOLOGY="sidecar"`},
		{"X1 single-container arm", `TOPOLOGY="single-container"`},
		{"X2 mode row", `stat -c '%a' /exchange`},
		{"X2 wants 770", `[[ "${X2_MODE}" == "770" ]]`},
		{"X3 runtime-side write", `> '${PROBE}'`},
		{"X3 mux port-forward", `:4097`},
		{"X3 mux auth", `-u "opencode:${WS_PW}"`},
		{"X3 no-warning assertion", `grep -q 'cleanup_warning'`},
		{"X3 file-gone assertion", `GONE from the runtime container's own view`},
		{"X3 survival failure", `the payload file SURVIVED its successful call`},
		{"X4 nested write", `umask 022; mkdir -p`},
		{"X4 refusal assertion", `no subdirectories`},
		{"X4 read-time gate prose", `refused at read time`},
		{"cleanup deletes the workspace", `delete workspace "${WS}"`},
	} {
		if !strings.Contains(s, row.needle) {
			t.Errorf("%s: script lost its row (needle %q not found) — the dual-uid execution coverage it pins is silently gone", row.label, row.needle)
		}
	}
}

func TestExchangeDirScript_MuxPortIsTheUserMux(t *testing.T) {
	// The port-forward must target 4097 — the agentd user mux serving
	// /v1/mcp (the same surface dev-preview-tunnel-e2e.sh drives). A
	// drifted port silently turns every X3/X4 row into a curl-dead
	// retry loop.
	body, err := os.ReadFile(exchangeDirScript(t))
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !strings.Contains(string(body), `"${pfport}:4097"`) {
		t.Error("mcp_call must port-forward to the user mux on 4097")
	}
}
