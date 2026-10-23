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

// projectEchoResponse is the project tier (name — the one prose field;
// tags are deliberately NOT echo-diffed in v1: short structured
// strings whose corruption is caught by the tag-channel scan, not
// prose round-trips).
type projectEchoResponse struct {
	board.Project
	ContentEchoDiff []string `json:"content_echo_diff,omitempty"`
}

func projectEchoDiff(submittedName string, stored *board.Project) []string {
	if submittedName != "" && stored.Name != submittedName {
		return []string{"name"}
	}
	return nil
}
