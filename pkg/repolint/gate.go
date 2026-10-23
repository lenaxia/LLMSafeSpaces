// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package repolint

// NumberingGateAction is what the numbering-gate recheck loop
// (.github/workflows/numbering-gate.yml) must do for one open PR after
// re-evaluating its merge tree against the current main tip.
type NumberingGateAction int

const (
	// NumberingGateNoop: the merge tree is clean and this head carries
	// no red numbering-gate run — nothing to post.
	NumberingGateNoop NumberingGateAction = iota
	// NumberingGateRedden: the merge tree the merge button WOULD create
	// carries duplicate numbered artifacts — post a failing check run
	// on the PR head naming the colliding files and the healing lever.
	NumberingGateRedden
	// NumberingGateClear: the merge tree is clean now, but this head
	// still carries a red numbering-gate run from an earlier main tip
	// (the duplicate was healed by a main advance or a sibling
	// renumber). Post a success run with the same name so the stale
	// red stops pinning the PR — clear-on-heal is a pinned behavior,
	// not an afterthought: a gate that only reddens recreates the
	// freeze class it exists to prevent.
	NumberingGateClear
)

// String names the action for logs and test output.
func (a NumberingGateAction) String() string {
	switch a {
	case NumberingGateNoop:
		return "noop"
	case NumberingGateRedden:
		return "redden"
	case NumberingGateClear:
		return "clear"
	default:
		return "unknown"
	}
}

// NumberingGateDecision decides the recheck action for one open PR.
//
//   - mergeTreeClean: the PR's refs/pull/N/merge tree passed
//     `repolint -numbering-only` (migrations sequence + worklog
//     uniqueness) against the CURRENT main tip.
//   - headHasRedGate: the PR's CURRENT head SHA already carries a
//     numbering-gate check run with conclusion=failure (queried via
//     the check-runs API before deciding).
//
// The decision matrix:
//
//	merge tree | prior red on head | action
//	-----------+-------------------+-------------
//	collides   | (any)             | Redden
//	clean      | yes               | Clear
//	clean      | no                | Noop
func NumberingGateDecision(mergeTreeClean, headHasRedGate bool) NumberingGateAction {
	if !mergeTreeClean {
		return NumberingGateRedden
	}
	if headHasRedGate {
		// Clear-on-heal: the tree is clean against the current main
		// tip but the head still shows a red numbering-gate run from
		// an earlier tip. Post the success run that unblocks the PR.
		return NumberingGateClear
	}
	return NumberingGateNoop
}
