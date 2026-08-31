// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package session

// Event, EventType, InputKind, InputOption, ToolRef, InputRequest, and
// Error are generated into contract_gen.go from the frozen ABI schema
// (ADR 0056 T3). Only the send-side options below — which have no schema
// counterpart — are hand-written.

// Admission is a delivery mode on SendOpts (design 0049 §4.4). The zero value
// means an immediate/default send; steer injects at the next safe boundary
// without aborting in-flight tools; queue promotes when the agent would idle.
type Admission string

const (
	AdmissionSteer Admission = "steer"
	AdmissionQueue Admission = "queue"
)

// SendOpts parameterize a message send, including steering admission.
type SendOpts struct {
	Model     *ModelRef `json:"model,omitempty"`
	Admission Admission `json:"admission,omitempty"`
}
