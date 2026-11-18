// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package local_test

// crd_skew_rebind_script_test.go — structure pins for
// local/crd-skew-rebind.sh (issue #2092's RECOVERY arm), the same
// philosophy as dev_preview_script_test.go: the script is operator glue
// against a live cluster; what is pinnable deterministically is the
// structure past failures actually broke — bash syntax, the wedge
// filter's discrimination (born-unbound ONLY, never legacy), the
// read-back verify without which a pruned bind reads as success, and
// the operator remediation strings.

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

const crdSkewRebindScript = "crd-skew-rebind.sh"

func TestCRDSkewRebindScript_BashSyntax(t *testing.T) {
	cmd := exec.Command("bash", "-n", crdSkewRebindScript)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("bash -n failed: %v\n%s", err, out)
	}
}

func TestCRDSkewRebindScript_LoadBearingFragments(t *testing.T) {
	raw, err := os.ReadFile(crdSkewRebindScript)
	if err != nil {
		t.Fatalf("read %s: %v", crdSkewRebindScript, err)
	}
	s := string(raw)

	rows := []struct {
		fragment string
		why      string
	}{
		// The wedge filter must discriminate: born-unbound ONLY.
		// Sweeping legacy marker-less empties into the patch loop would
		// fight the controller's backfill (and stamp pi over objects
		// that have only ever run opencode).
		{`.spec.harness == ""`, "wedge filter: only empty-harness objects"},
		{`(.metadata.annotations[$marker] // "") == "true"`, "wedge filter: the born-unbound MARKER must gate the set (legacy empties excluded)"},
		{`llmsafespaces.dev/harness-unbound`, "the marker annotation key must be the platform constant"},
		// The read-back verify is the whole point (#2092): the pruning
		// apiserver accepts the bind and drops it — a script without
		// this check reports success on exactly the clusters it exists
		// for.
		{`jsonpath='{.spec.harness}'`, "re-bind must VERIFY persistence (read-back)"},
		{`spec.harness reads back empty`, "the verify failure must name the read-back symptom"},
		{`kubectl apply -f helm/crds/workspace.yaml`, "the verify failure must name the operator remediation"},
		// Review r2's two-patch discipline (the marker-consumption
		// finding): the BIND patch is spec-only, the read-back verify
		// sits between it and the marker cleanup — a pruned bind must
		// leave the marker in place, so the source ORDER is load-bearing.
		{`-p "{\"spec\":{\"harness\":\"$HARNESS\"}}"`, "patch 1: the bind writes spec.harness ONLY (marker untouched)"},
		{`\"$MARKER_ANNOTATION\":null`, "patch 2: the marker cleanup after the verified bind"},
		// The webhook-admitted shapes only.
		{`opencode|pi)`, "rebind harness values are the webhook-admitted set"},
		// Enumerate-only default: running the script cold must be safe.
		{`enumerate-only`, "default mode lists, never patches"},
		// Enumeration failure is LOUD (review r2): no `|| true` on the
		// kubectl pipeline — a bad context must never read as "none".
		{`the wedged set is UNKNOWN, not empty`, "an enumeration failure names the unknown, never a false all-clear"},
		// The doc block carries the runbook order: CRD FIRST, then bind.
		{`Apply the current CRD FIRST`, "the usage header must teach CRD-before-rebind (a bind cannot converge on a stale CRD)"},
	}
	for _, r := range rows {
		if !strings.Contains(s, r.fragment) {
			t.Errorf("fragment missing from %s\n  fragment: %s\n  why: %s", crdSkewRebindScript, r.fragment, r.why)
		}
	}
	// The ORDER: bind patch, then verify, then marker cleanup (a cleanup
	// that precedes the verify is the marker-consumption bug the API
	// tier fixed in review r2 — the script must not reintroduce it).
	bindIdx := strings.Index(s, `-p "{\"spec\":{\"harness\":\"$HARNESS\"}}"`)
	verifyIdx := strings.Index(s, "jsonpath='{.spec.harness}'")
	cleanupIdx := strings.Index(s, `\"$MARKER_ANNOTATION\":null`)
	if bindIdx >= verifyIdx || verifyIdx >= cleanupIdx {
		t.Errorf("re-bind order broken: bind(%d) must precede verify(%d) must precede marker cleanup(%d)",
			bindIdx, verifyIdx, cleanupIdx)
	}
}

// TestCRDSkewRebindScript_HarnessValidation: an invalid --rebind value
// must refuse before any cluster call.
func TestCRDSkewRebindScript_HarnessValidation(t *testing.T) {
	cmd := exec.Command("bash", crdSkewRebindScript, "--rebind=bogus")
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("expected exit 1 for --rebind=bogus, got 0:\n%s", out)
	}
	if !strings.Contains(string(out), "opencode or pi") {
		t.Fatalf("expected the harness-set error naming the admitted values, got:\n%s", out)
	}
}

// TestCRDSkewRebindScript_EnumerationFailureFailsLoud (review r2, the
// rebind-script-masks-enum-failure finding): a kubectl enumeration
// failure — bad context, RBAC refusal, apiserver hiccup — must exit
// non-zero with the UNKNOWN verdict, never the pre-fix `|| true` shape
// of "none" + exit 0 (a false all-clear from the recovery runbook's
// own sweep, the exact tool an operator reaches for mid-incident).
// Executed end-to-end under a failing kubectl shim (the
// runScriptUnderShims smoke discipline, specialized to this script).
func TestCRDSkewRebindScript_EnumerationFailureFailsLoud(t *testing.T) {
	shimDir := t.TempDir()
	shim := "#!/usr/bin/env bash\necho 'The connection to the server api.bad-cluster:6443 was refused' >&2\nexit 1\n"
	if err := os.WriteFile(filepath.Join(shimDir, "kubectl"), []byte(shim), 0o755); err != nil {
		t.Fatalf("write kubectl shim: %v", err)
	}

	cmd := exec.Command("bash", crdSkewRebindScript)
	cmd.Env = append(os.Environ(),
		"PATH="+shimDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("a failing kubectl must exit non-zero, got 0:\n%s", out)
	}
	combined := string(out)
	if !strings.Contains(combined, "enumeration failed") {
		t.Fatalf("the failure must be the named enumeration error:\n%s", combined)
	}
	if !strings.Contains(combined, "UNKNOWN, not empty") {
		t.Fatalf("the failure must state the wedged set is UNKNOWN (never a false all-clear):\n%s", combined)
	}
	if strings.Contains(combined, "none — no born-unbound workspaces") {
		t.Fatalf("a failing enumeration must NEVER report the all-clear:\n%s", combined)
	}
}
