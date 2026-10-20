// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package opencode

// loopback_board_wire_1746_test.go — the agentd arm of the worker-tool
// seam's skew matrix (#1746): the loopback client's board worker calls
// (board_heartbeat / board_report_throttle / board_move_stage /
// board_read → /internal/v1/automation/board/*) must emit EXACTLY the
// NEWEST frozen wire set in pkg/board/worker_compat.go.
//
// Why exact-equality rather than "contains": under
// agentdDelivery.updatePolicy OnRefresh, a platform upgrade leaves
// running pods on agentd N-1 against API N until their next natural
// recreation — the ONLY thing making that skew safe is the wire form
// being frozen per release. If this pin reddens, the client's wire form
// changed: either revert the change, or append a NEW frozen wire set
// (board.WorkerSeamWireVn) plus its API-side acceptance arm — never
// edit the frozen fixtures in place. The mirror arm lives in
// api/internal/handlers/board_worker_compat_1746_test.go.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/lenaxia/llmsafespaces/pkg/board"
)

// wireRecorder captures every request the client emits, in order.
type wireRecorder struct {
	mu       sync.Mutex
	requests []board.WorkerSeamRequest
}

func (w *wireRecorder) serve(t *testing.T) http.HandlerFunc {
	return func(rw http.ResponseWriter, r *http.Request) {
		body := ""
		if r.Body != nil {
			raw := make([]byte, r.ContentLength)
			if r.ContentLength > 0 {
				_, _ = r.Body.Read(raw)
				body = string(raw)
			}
		}
		w.mu.Lock()
		w.requests = append(w.requests, board.WorkerSeamRequest{
			Method: r.Method,
			Path:   r.URL.RequestURI(), // path + query, the frozen form
			Body:   body,
		})
		w.mu.Unlock()
		rw.WriteHeader(http.StatusOK)
		_, _ = rw.Write([]byte(`{}`))
	}
}

// emitWorkerSeam drives the loopback client's four worker-tool calls
// with the exact tool-argument bodies agentd v0.39.0's MCP layer passes
// through (cmd/workspace-agentd mcp_tools.go: verbatim args; the client
// stamps the pod's workspaceID — the frozen fixtures' stamp source).
func emitWorkerSeam(t *testing.T, c *Client) {
	t.Helper()
	ctx := context.Background()
	const sa = "sa-token"
	const ws = "11111111-1111-1111-1111-111111111101"

	_, err := c.BoardTaskGet(ctx, sa, ws, "tsk-aaaa1111")
	require.NoError(t, err, "board_read item form")
	_, err = c.BoardHeartbeat(ctx, sa, ws, json.RawMessage(
		`{"shortId":"tsk-aaaa1111","claimToken":"tok-1","observedProgressAt":"2026-10-19T00:00:00Z"}`))
	require.NoError(t, err, "board_heartbeat")
	_, err = c.BoardReportThrottle(ctx, sa, ws, json.RawMessage(
		`{"shortId":"tsk-aaaa1111","claimToken":"tok-1","retryAfterSeconds":900}`))
	require.NoError(t, err, "board_report_throttle")
	_, err = c.BoardMoveStage(ctx, sa, ws, json.RawMessage(
		`{"shortId":"tsk-aaaa1111","claimToken":"tok-1","toStage":"in_review","requestId":"req-1"}`))
	require.NoError(t, err, "board_move_stage")
}

// TestLoopbackBoardWorkerWire_MatchesFrozenContract: the emitted wire
// form equals the newest frozen set, request for request — method,
// path+query, and the exact stamped body bytes (json.Marshal of the
// workspaceID-stamped map: keys sorted, no whitespace — deterministic,
// so byte equality is the honest pin).
func TestLoopbackBoardWorkerWire_MatchesFrozenContract(t *testing.T) {
	rec := &wireRecorder{}
	srv := httptest.NewServer(rec.serve(t))
	t.Cleanup(srv.Close)
	c := NewLoopbackClient(srv.URL, "pw")

	emitWorkerSeam(t, c)

	sets := board.WorkerSeamWireSets()
	frozen := sets[len(sets)-1] // the newest frozen set = this agentd's contract
	require.Len(t, rec.requests, len(frozen.Requests),
		"the four worker-tool calls emit exactly the frozen request count")

	// Emitted order == frozen order (read → heartbeat → throttle → move).
	for i, want := range frozen.Requests {
		got := rec.requests[i]
		require.Equal(t, want.Method, got.Method, "%s: method drifted", want.Tool)
		require.Equal(t, want.Path, got.Path, "%s: path+query drifted", want.Tool)
		require.Equal(t, want.Body, got.Body, "%s: stamped body drifted", want.Tool)
	}
}
