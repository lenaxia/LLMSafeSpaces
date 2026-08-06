// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package workflows

import (
	"encoding/json"
)

// Spec is the parsed and validated DAG stored in workflows.spec_json.
// It is the execution-ready form: defaults merged, node types validated,
// edges checked for cycles/dangling/condition-coverage.
type Spec struct {
	Nodes []SpecNode `json:"nodes"`
	Edges []SpecEdge `json:"edges"`
}

// SpecNode is a single node in the workflow DAG.
type SpecNode struct {
	ID          string          `json:"id"`
	Type        string          `json:"type"`
	Data        json.RawMessage `json:"data"`
	MaxAttempts int             `json:"maxAttempts,omitempty"`
	Timeout     string          `json:"timeout,omitempty"`
}

// SpecEdge is a directed edge in the DAG. SourceHandle carries the
// condition-branch id (for condition nodes) or is empty (for all other types).
type SpecEdge struct {
	Source       string `json:"source"`
	Target       string `json:"target"`
	SourceHandle string `json:"sourceHandle,omitempty"`
}

// ConditionNodeData is the typed shape of SpecNode.Data for condition nodes.
type ConditionNodeData struct {
	Conditions []ConditionCase `json:"conditions"`
}

// ConditionCase is one branch of a condition node.
type ConditionCase struct {
	ID         string `json:"id"`
	Expression string `json:"expression"`
}

// AgentNodeData is the typed shape of SpecNode.Data for agent nodes.
type AgentNodeData struct {
	Agent                   string          `json:"agent,omitempty"`
	Prompt                  string          `json:"prompt"`
	System                  string          `json:"system,omitempty"`
	OutputSchema            json.RawMessage `json:"outputSchema,omitempty"`
	EnforceStructuredOutput bool            `json:"enforceStructuredOutput,omitempty"`
	Session                 string          `json:"session,omitempty"`
	SessionID               string          `json:"sessionId,omitempty"`
}

// ScriptNodeData is the typed shape of SpecNode.Data for script nodes.
type ScriptNodeData struct {
	Language string `json:"language"`
	Handler  string `json:"handler"`
}

// HTTPNodeData is the typed shape of SpecNode.Data for http nodes.
type HTTPNodeData struct {
	Method  string            `json:"method,omitempty"`
	URL     string            `json:"url"`
	Headers map[string]string `json:"headers,omitempty"`
	Body    string            `json:"body,omitempty"`
	Timeout string            `json:"timeout,omitempty"`
}

// DefaultsBlock carries workflow-level defaults that are merged into
// each node's config (node-level wins). Only maxAttempts and timeout
// may be defaulted; behavioral fields must be per-node.
type DefaultsBlock struct {
	MaxAttempts *int   `json:"maxAttempts,omitempty"`
	Timeout     string `json:"timeout,omitempty"`
}

// ParseSpec parses raw JSON bytes into a Spec. Does NOT validate —
// call ValidateSpec for that. Returns an error only on JSON parse failure.
func ParseSpec(raw json.RawMessage) (*Spec, error) {
	var s Spec
	if err := json.Unmarshal(raw, &s); err != nil {
		return nil, err
	}
	return &s, nil
}
