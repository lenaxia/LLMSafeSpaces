// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package repolint

import "testing"

// Pins for the numbering-gate recheck decision (#1838 hard layer).
// The workflow's per-PR action is this pure function; the bash side
// only gathers inputs (merge-tree evaluation, prior check-run state)
// and executes the action. Clear-on-heal is a pinned behavior per the
// design ratification: a gate that reddens but never clears pins a
// healed PR forever — the freeze class #1838 exists to end, recreated
// by the fix itself.

func TestNumberingGateDecision(t *testing.T) {
	cases := []struct {
		name           string
		mergeTreeClean bool
		headHasRedGate bool
		want           NumberingGateAction
	}{
		{"collision always reddens", false, false, NumberingGateRedden},
		{"collision reddens even over a prior red", false, true, NumberingGateRedden},
		{"clean with prior red CLEARS (clear-on-heal)", true, true, NumberingGateClear},
		{"clean with no prior red is noop", true, false, NumberingGateNoop},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := NumberingGateDecision(tc.mergeTreeClean, tc.headHasRedGate)
			if got != tc.want {
				t.Fatalf("NumberingGateDecision(clean=%v, priorRed=%v) = %v, want %v",
					tc.mergeTreeClean, tc.headHasRedGate, got, tc.want)
			}
		})
	}
}

func TestNumberingGateAction_String(t *testing.T) {
	for a, want := range map[NumberingGateAction]string{
		NumberingGateNoop:       "noop",
		NumberingGateRedden:     "redden",
		NumberingGateClear:      "clear",
		NumberingGateAction(99): "unknown",
	} {
		if got := a.String(); got != want {
			t.Errorf("%d.String() = %q, want %q", a, got, want)
		}
	}
}
