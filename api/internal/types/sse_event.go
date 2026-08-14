// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package types

type WorkspaceSSEEvent struct {
	EventID     uint64      `json:"event_id,omitempty"`
	WorkspaceID string      `json:"workspace_id,omitempty"`
	Type        string      `json:"type"`
	Phase       string      `json:"phase,omitempty"`
	SessionID   string      `json:"session_id,omitempty"`
	RequestID   string      `json:"request_id,omitempty"`
	Status      string      `json:"status,omitempty"`
	EventType   string      `json:"event_type,omitempty"`
	Data        interface{} `json:"data,omitempty"`
	// SnapshotOK is set only on agent.input.snapshot_complete markers:
	// true when the pending-set fetch from the pod succeeded (the staged
	// set is authoritative), false when it failed or timed out (clients
	// must keep their existing pending state). nil on all other events.
	SnapshotOK *bool `json:"snapshot_ok,omitempty"`
	// SnapshotID identifies a single snapshot attempt. Set on
	// agent.input.snapshot_begin and the matching snapshot_complete so
	// clients can key per-flight staging — two concurrent flights for one
	// workspace (workspace SSE + user stream connecting together) must not
	// consume each other's staged events. Empty on legacy markers.
	SnapshotID string `json:"snapshot_id,omitempty"`
}
