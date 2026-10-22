// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package handlers

// board_textguard_echo_test.go — #1837 P1b handler tier: (a) the seam
// and REST reject corrupted text with the text_rejected code (the
// service validation surfaced through renderBoardError), and (b) the
// ECHO DIFF-FLAGS stored != submitted — a stored row whose text fields
// differ from what was submitted is never silent (the response carries
// content_echo_diff).

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lenaxia/llmsafespaces/pkg/board"
)

func TestBoardTextGuard_SeamRejectsCorruption(t *testing.T) {
	// The service-level rejection (pkg/board textguard_test pins the
	// detectors; the CI integration suite pins them end-to-end through
	// the real service) renders through the seam as 400 + the
	// text_rejected code — never a silent 2xx store.
	mgr := newFakeBoardManager()
	mgr.errFor["create_task"] = &board.GuardError{Code: board.GuardTextRejected,
		Detail: "description: degenerate repetition — a 25-byte fragment tiles the content 50 times (floor 50); suspected emission corruption"}
	r := newBoardAutomationRouterWithManager(t, newFakeBoardService(), mgr)

	body := `{"sessionId":"ses-1","workspaceID":"ws-1","title":"T","description":"` +
		strings.Repeat("Retry the loop step again. ", 50) + `"}`
	w := doBoard(t, r, http.MethodPost, "/internal/v1/automation/board/epics/epc-bbbb2222/tasks", body)
	require.Equal(t, http.StatusBadRequest, w.Code)
	var resp struct {
		Error string `json:"error"`
		Code  string `json:"code"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.Equal(t, board.GuardTextRejected, resp.Code)
	assert.Contains(t, resp.Error, "repetition")
}

func TestBoardTextGuard_EchoDiffFlagsDrift(t *testing.T) {
	// The fake returns a STORED description different from the
	// submitted one (drift) — the response must diff-flag it.
	mgr := newFakeBoardManager()
	mgr.driftTask = &board.Task{ID: "t1", ShortID: "tsk-cccc3333", ProjectID: "p1", EpicID: "e1",
		Title: "stored title", Description: "stored description", Stage: board.StageBacklog, Tags: []string{}}
	r := newBoardAutomationRouterWithManager(t, newFakeBoardService(), mgr)

	w := doBoard(t, r, http.MethodPatch, "/internal/v1/automation/board/tasks/tsk-cccc3333",
		`{"sessionId":"ses-1","workspaceID":"ws-1","title":"submitted title","description":"submitted description"}`)
	require.Equal(t, http.StatusOK, w.Code)

	var resp struct {
		Title           string   `json:"title"`
		Description     string   `json:"description"`
		ContentEchoDiff []string `json:"content_echo_diff"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	require.Equal(t, []string{"description", "title"}, normalizeEchoDiff(resp.ContentEchoDiff),
		"stored != submitted must be diff-flagged, never silent")
}

func TestBoardTextGuard_EchoCleanWhenIdentical(t *testing.T) {
	mgr := newFakeBoardManager()
	mgr.driftTask = &board.Task{ID: "t1", ShortID: "tsk-cccc3333", ProjectID: "p1", EpicID: "e1",
		Title: "T", Description: "D", Stage: board.StageBacklog, Tags: []string{}}
	r := newBoardAutomationRouterWithManager(t, newFakeBoardService(), mgr)

	w := doBoard(t, r, http.MethodPatch, "/internal/v1/automation/board/tasks/tsk-cccc3333",
		`{"sessionId":"ses-1","workspaceID":"ws-1","title":"T","description":"D"}`)
	require.Equal(t, http.StatusOK, w.Code)

	var resp map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	_, hasDiff := resp["content_echo_diff"]
	assert.False(t, hasDiff, "no drift — the echo key is absent (response shape unchanged)")
}

func normalizeEchoDiff(in []string) []string {
	out := append([]string(nil), in...)
	for i := 0; i < len(out); i++ {
		for j := i + 1; j < len(out); j++ {
			if out[j] < out[i] {
				out[i], out[j] = out[j], out[i]
			}
		}
	}
	return out
}
