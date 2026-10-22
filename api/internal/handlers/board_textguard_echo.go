// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package handlers

// board_textguard_echo.go — #1837 P1b echo tier: a stored row that
// differs from the SUBMITTED text is diff-flagged in the response
// (content_echo_diff), never silent. The structural detectors reject
// the known corruption class up front; this belt catches any storage/
// canonicalization drift that slips past them (the review's scope
// note: generation corruption is rejected by the heuristics; echo
// diffing covers the remaining write-path drift).

import "github.com/lenaxia/llmsafespaces/pkg/board"

// taskEchoResponse flattens the task row and adds content_echo_diff
// only when a submitted text field stored differently (omitempty keeps
// the response byte-identical when nothing drifted).
type taskEchoResponse struct {
	board.Task
	ContentEchoDiff []string `json:"content_echo_diff,omitempty"`
	// MutatedCriterion echoes the setCriteriaMet write (#1837 P2b):
	// index, the met flag written, and the criterion TEXT — a
	// check-off/uncheck never lands un-attributed.
	MutatedCriterion *MutatedCriterion `json:"mutated_criterion,omitempty"`
}

// MutatedCriterion is the #1837 P2b setCriteriaMet echo.
type MutatedCriterion struct {
	Index int    `json:"index"`
	Met   bool   `json:"met"`
	Text  string `json:"text"`
}

// mutatedCriterion builds the echo from the submitted check and the
// stored row (nil when no check was submitted or the row's set no
// longer holds the index — the service's bounds guard makes the latter
// unreachable, the belt keeps the renderer nil-safe).
func mutatedCriterion(check *board.CriteriaCheck, stored *board.Task) *MutatedCriterion {
	if check == nil || stored == nil || check.Index < 0 || check.Index >= len(stored.AcceptanceCriteria) {
		return nil
	}
	return &MutatedCriterion{Index: check.Index, Met: check.Met, Text: stored.AcceptanceCriteria[check.Index].Text}
}

// epicEchoResponse is the epic tier of the same echo.
type epicEchoResponse struct {
	board.Epic
	ContentEchoDiff []string `json:"content_echo_diff,omitempty"`
}

// taskEchoDiff compares submitted (non-empty = patched) text fields
// against the stored row.
func taskEchoDiff(submittedTitle, submittedDescription string, stored *board.Task) []string {
	var diffs []string
	if submittedTitle != "" && stored.Title != submittedTitle {
		diffs = append(diffs, "title")
	}
	if submittedDescription != "" && stored.Description != submittedDescription {
		diffs = append(diffs, "description")
	}
	return diffs
}

// epicEchoDiff is the epic tier (title/outcome).
func epicEchoDiff(submittedTitle, submittedOutcome string, stored *board.Epic) []string {
	var diffs []string
	if submittedTitle != "" && stored.Title != submittedTitle {
		diffs = append(diffs, "title")
	}
	if submittedOutcome != "" && stored.Outcome != submittedOutcome {
		diffs = append(diffs, "outcome")
	}
	return diffs
}
