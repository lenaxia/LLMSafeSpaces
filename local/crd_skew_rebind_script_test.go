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
		// The patch must remove the marker in the SAME write (a stale
		// marker on a bound object lies about the waiting state).
		{`\"$MARKER_ANNOTATION\":null`, "the bind patch must drop the waiting marker"},
		// The webhook-admitted shapes only.
		{`opencode|pi)`, "rebind harness values are the webhook-admitted set"},
		// Enumerate-only default: running the script cold must be safe.
		{`enumerate-only`, "default mode lists, never patches"},
		// The doc block carries the runbook order: CRD FIRST, then bind.
		{`Apply the current CRD FIRST`, "the usage header must teach CRD-before-rebind (a bind cannot converge on a stale CRD)"},
	}
	for _, r := range rows {
		if !strings.Contains(s, r.fragment) {
			t.Errorf("fragment missing from %s\n  fragment: %s\n  why: %s", crdSkewRebindScript, r.fragment, r.why)
		}
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
