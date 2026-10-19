// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package board

// worker_compat.go — the frozen WIRE CONTRACT of the Task Board
// worker-tool seam (#1746 N-1 compatibility discipline).
//
// Under agentdDelivery.updatePolicy OnRefresh (#1746's product default)
// a platform upgrade rolls the API to N while running workspace pods
// keep speaking agentd N-1 until their next natural pod recreation —
// the user owns when their sessions are interrupted. The worker tools
// (board_heartbeat, board_report_throttle, board_move_stage,
// board_read) ride the versioned HTTP routes
// /internal/v1/automation/board/*, so that skew is a TESTED MATRIX
// rather than an assumption:
//
//   - the AGENTD side (pkg/agent/opencode loopback client) is pinned to
//     emit EXACTLY the latest frozen set — a wire-form change on the
//     client reddens the pin and becomes a conscious contract bump;
//   - the API side (api/internal/handlers board seam) is pinned to
//     ACCEPT EVERY frozen set still in workerSeamWireSets — removing or
//     reshaping a route/field an in-flight N-1 agentd still uses
//     reddens the pin.
//
// Maintenance rule (additive only): agentd's wire form for a given
// release is frozen at that release. When a future agentd changes the
// seam, do NOT edit an existing set — append a new *WorkerSeamWireSet
// to workerSeamWireSets and move the client pin to it. An old set drops
// from the slice only when that agentd version is out of the supported
// skew window. The route paths are versioned (/internal/v1): a breaking
// change is a /internal/v2 route, never a mutation of v1.

// WorkerSeamRequest is one frozen HTTP request of the worker-tool seam:
// the exact method, path (query included), and JSON body agentd emits.
// Headers are uniform across the seam and frozen with it: Authorization
// "Bearer <pod SA token>" and Content-Type "application/json" on
// bodied calls.
type WorkerSeamRequest struct {
	// Tool names the agentd MCP tool that emits the request (docs and
	// failure messages; not on the wire).
	Tool   string
	Method string
	Path   string
	// Body is the exact JSON body ("" = none). agentd stamps the pod's
	// resolved workspace as "workspaceID" into every bodied call — the
	// SA-token identity is the only truth, the stamp is the resolver's
	// in-body convention.
	Body string
}

// WorkerSeamWireSet is one agentd release's frozen worker-tool wire
// form. AgentdVersion identifies the emitting agentd (the version a
// running pod keeps running under OnRefresh until its next refresh).
type WorkerSeamWireSet struct {
	AgentdVersion string
	Requests      []WorkerSeamRequest
}

// WorkerSeamWireV1 is agentd v0.39.0's worker-tool wire form (main
// 84c32279 — pkg/agent/opencode loopback.go boardWorkerPost /
// BoardTaskGet), frozen at #1746: four routes, camelCase bodies passed
// through verbatim from the MCP tool args, workspaceID stamped.
var WorkerSeamWireV1 = &WorkerSeamWireSet{
	AgentdVersion: "v0.39.0",
	Requests: []WorkerSeamRequest{
		{
			Tool:   "board_read",
			Method: "GET",
			Path:   "/internal/v1/automation/board/tasks/tsk-aaaa1111?workspaceID=11111111-1111-1111-1111-111111111101",
		},
		{
			Tool:   "board_heartbeat",
			Method: "POST",
			Path:   "/internal/v1/automation/board/tasks/heartbeat",
			Body:   `{"claimToken":"tok-1","observedProgressAt":"2026-10-19T00:00:00Z","shortId":"tsk-aaaa1111","workspaceID":"11111111-1111-1111-1111-111111111101"}`,
		},
		{
			Tool:   "board_report_throttle",
			Method: "POST",
			Path:   "/internal/v1/automation/board/tasks/throttle",
			Body:   `{"claimToken":"tok-1","retryAfterSeconds":900,"shortId":"tsk-aaaa1111","workspaceID":"11111111-1111-1111-1111-111111111101"}`,
		},
		{
			Tool:   "board_move_stage",
			Method: "POST",
			Path:   "/internal/v1/automation/board/tasks/move-stage",
			Body:   `{"claimToken":"tok-1","requestId":"req-1","shortId":"tsk-aaaa1111","toStage":"in_review","workspaceID":"11111111-1111-1111-1111-111111111101"}`,
		},
	},
}

// workerSeamWireSets lists every frozen wire set the API(N) must still
// accept — the N-1 skew matrix. Append on agentd wire changes; never
// edit in place.
var workerSeamWireSets = []*WorkerSeamWireSet{WorkerSeamWireV1}

// WorkerSeamWireSets returns the supported frozen wire sets (oldest
// first). Exported for the seam's compat pins on both sides.
func WorkerSeamWireSets() []*WorkerSeamWireSet {
	out := make([]*WorkerSeamWireSet, len(workerSeamWireSets))
	copy(out, workerSeamWireSets)
	return out
}
