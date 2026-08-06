// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package workflows

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/lenaxia/llmsafespaces/pkg/types"
	"github.com/lenaxia/llmsafespaces/pkg/workflows/exprlang"
)

// ValidationError describes a single problem found during DAG validation.
type ValidationError struct {
	Code   string `json:"code"`
	NodeID string `json:"nodeId,omitempty"`
	Detail string `json:"detail,omitempty"`
}

func (e ValidationError) Error() string {
	if e.NodeID != "" {
		return fmt.Sprintf("[%s] node %s: %s", e.Code, e.NodeID, e.Detail)
	}
	return fmt.Sprintf("[%s] %s", e.Code, e.Detail)
}

// ValidateSpec validates a workflow DAG spec and applies defaults merging.
// It mutates the spec in place (defaults applied to nodes that omit maxAttempts/timeout).
// predecessorSchemas maps node ID → that node's outputSchema (JSON Schema bytes),
// used for expr-lang type-checking condition expressions against the upstream node's
// output shape. nil/empty schemas skip type-checking for that predecessor.
//
// Returns a slice of ValidationErrors (empty if valid). The spec is mutated
// (defaults merged) even if errors are found — the caller should discard on error.
func ValidateSpec(spec *Spec, predecessorSchemas map[string]json.RawMessage, defaults DefaultsBlock) []ValidationError {
	if spec == nil || len(spec.Nodes) == 0 {
		return []ValidationError{{Code: "empty_spec", Detail: "workflow spec has no nodes"}}
	}

	var errs []ValidationError

	// 1. Check for duplicate node IDs + build lookup.
	nodeMap := make(map[string]*SpecNode, len(spec.Nodes))
	for i := range spec.Nodes {
		n := &spec.Nodes[i]
		if _, exists := nodeMap[n.ID]; exists {
			errs = append(errs, ValidationError{Code: "duplicate_node_id", NodeID: n.ID, Detail: "node ID appears more than once"})
		}
		nodeMap[n.ID] = n
	}

	// 2. Validate node types + node data.
	for i := range spec.Nodes {
		n := &spec.Nodes[i]
		if !types.ValidNodeType(n.Type) {
			errs = append(errs, ValidationError{Code: "invalid_node_type", NodeID: n.ID, Detail: fmt.Sprintf("unsupported node type %q (valid: script, agent, http, condition)", n.Type)})
			continue
		}
		if dErrs := validateNodeData(n); len(dErrs) > 0 {
			errs = append(errs, dErrs...)
		}
	}

	// 3. Check edge references.
	for _, e := range spec.Edges {
		if _, ok := nodeMap[e.Source]; !ok {
			errs = append(errs, ValidationError{Code: "dangling_edge", Detail: fmt.Sprintf("edge source %q does not exist", e.Source)})
		}
		if _, ok := nodeMap[e.Target]; !ok {
			errs = append(errs, ValidationError{Code: "dangling_edge", Detail: fmt.Sprintf("edge target %q does not exist", e.Target)})
		}
	}

	// Short-circuit: if we have dangling edges or duplicate IDs, the graph
	// analysis below is unreliable. Return early.
	if len(errs) > 0 {
		return errs
	}

	// 4. Detect cycles via topological sort (Kahn's algorithm).
	if cycle := detectCycle(spec); cycle {
		errs = append(errs, ValidationError{Code: "cycle", Detail: "workflow contains a cycle"})
		return errs
	}

	// 5. Find start node(s) — nodes with no incoming edges.
	starts := findStartNodes(spec)
	if len(starts) == 0 {
		errs = append(errs, ValidationError{Code: "no_start", Detail: "no start node found (every node has an incoming edge — possible cycle or missing entry point)"})
	} else if len(starts) > 1 {
		ids := make([]string, len(starts))
		copy(ids, starts)
		errs = append(errs, ValidationError{Code: "multiple_starts", Detail: fmt.Sprintf("multiple start nodes (no incoming edges): %s", strings.Join(ids, ", "))})
	}

	// 6. Check for unreachable nodes.
	reachable := bfsReachable(spec, starts)
	for i := range spec.Nodes {
		n := &spec.Nodes[i]
		if !reachable[n.ID] {
			errs = append(errs, ValidationError{Code: "unreachable_node", NodeID: n.ID, Detail: "node is not reachable from any start node"})
		}
	}

	// 7. Condition node branch-coverage check.
	for i := range spec.Nodes {
		n := &spec.Nodes[i]
		if n.Type != types.NodeTypeCondition {
			continue
		}
		cErrs := validateConditionBranches(n, spec)
		errs = append(errs, cErrs...)
	}

	// 8. Condition expression type-checking (if predecessor schemas provided).
	if predecessorSchemas != nil {
		for i := range spec.Nodes {
			n := &spec.Nodes[i]
			if n.Type != types.NodeTypeCondition {
				continue
			}
			eErrs := validateConditionExprTypes(n, spec, nodeMap, predecessorSchemas)
			errs = append(errs, eErrs...)
		}
	}

	// 9. Merge defaults (mutates spec in place).
	applyDefaults(spec, defaults)

	return errs
}

func validateNodeData(n *SpecNode) []ValidationError {
	switch n.Type {
	case types.NodeTypeScript:
		var d ScriptNodeData
		if err := json.Unmarshal(n.Data, &d); err != nil {
			return []ValidationError{{Code: "invalid_node_data", NodeID: n.ID, Detail: fmt.Sprintf("cannot parse script data: %v", err)}}
		}
		if d.Handler == "" {
			return []ValidationError{{Code: "invalid_node_data", NodeID: n.ID, Detail: "script node missing required field 'handler'"}}
		}
		if d.Language == "" {
			return []ValidationError{{Code: "invalid_node_data", NodeID: n.ID, Detail: "script node missing required field 'language'"}}
		}
	case types.NodeTypeAgent:
		var d AgentNodeData
		if err := json.Unmarshal(n.Data, &d); err != nil {
			return []ValidationError{{Code: "invalid_node_data", NodeID: n.ID, Detail: fmt.Sprintf("cannot parse agent data: %v", err)}}
		}
		if d.Prompt == "" {
			return []ValidationError{{Code: "invalid_node_data", NodeID: n.ID, Detail: "agent node missing required field 'prompt'"}}
		}
	case types.NodeTypeHTTP:
		var d HTTPNodeData
		if err := json.Unmarshal(n.Data, &d); err != nil {
			return []ValidationError{{Code: "invalid_node_data", NodeID: n.ID, Detail: fmt.Sprintf("cannot parse http data: %v", err)}}
		}
		if d.URL == "" {
			return []ValidationError{{Code: "invalid_node_data", NodeID: n.ID, Detail: "http node missing required field 'url'"}}
		}
	case types.NodeTypeCondition:
		var d ConditionNodeData
		if err := json.Unmarshal(n.Data, &d); err != nil {
			return []ValidationError{{Code: "invalid_node_data", NodeID: n.ID, Detail: fmt.Sprintf("cannot parse condition data: %v", err)}}
		}
		if len(d.Conditions) == 0 {
			return []ValidationError{{Code: "invalid_node_data", NodeID: n.ID, Detail: "condition node has no conditions"}}
		}
		for _, c := range d.Conditions {
			if c.ID == "" {
				return []ValidationError{{Code: "invalid_node_data", NodeID: n.ID, Detail: "condition case missing 'id'"}}
			}
			if c.Expression == "" {
				return []ValidationError{{Code: "invalid_node_data", NodeID: n.ID, Detail: fmt.Sprintf("condition case %q missing 'expression'", c.ID)}}
			}
		}
	}
	return nil
}

func validateConditionBranches(n *SpecNode, spec *Spec) []ValidationError {
	var condData ConditionNodeData
	if err := json.Unmarshal(n.Data, &condData); err != nil {
		return nil // already caught by validateNodeData
	}

	// Collect outgoing edges with sourceHandle from this condition node.
	handleEdges := make(map[string]bool)
	for _, e := range spec.Edges {
		if e.Source == n.ID && e.SourceHandle != "" {
			handleEdges[e.SourceHandle] = true
		}
	}

	var errs []ValidationError
	for _, c := range condData.Conditions {
		if !handleEdges[c.ID] {
			errs = append(errs, ValidationError{
				Code:   "missing_branch_edge",
				NodeID: n.ID,
				Detail: fmt.Sprintf("condition branch %q has no outgoing edge with sourceHandle=%q", c.ID, c.ID),
			})
		}
	}
	if !handleEdges["otherwise"] {
		errs = append(errs, ValidationError{
			Code:   "missing_branch_edge",
			NodeID: n.ID,
			Detail: "condition node has no 'otherwise' edge (required for the implicit default branch)",
		})
	}
	return errs
}

func validateConditionExprTypes(n *SpecNode, spec *Spec, nodeMap map[string]*SpecNode, schemas map[string]json.RawMessage) []ValidationError {
	var condData ConditionNodeData
	if err := json.Unmarshal(n.Data, &condData); err != nil {
		return nil
	}

	// Find the predecessor node (the one whose output feeds this condition).
	var predSchema json.RawMessage
	for _, e := range spec.Edges {
		if e.Target == n.ID {
			if schema, ok := schemas[e.Source]; ok {
				predSchema = schema
			}
			break
		}
	}

	if len(predSchema) == 0 {
		return nil // no schema available — skip type-check
	}

	var errs []ValidationError
	for _, c := range condData.Conditions {
		if err := exprlang.CompileCondition(c.Expression, predSchema); err != nil {
			errs = append(errs, ValidationError{
				Code:   "expr_type_error",
				NodeID: n.ID,
				Detail: fmt.Sprintf("condition %q expression fails type-check: %v", c.ID, err),
			})
		}
	}
	return errs
}

// detectCycle returns true if the DAG contains a cycle. Uses Kahn's algorithm:
// compute in-degrees, repeatedly remove nodes with in-degree 0, if any remain
// there's a cycle.
func detectCycle(spec *Spec) bool {
	inDegree := make(map[string]int, len(spec.Nodes))
	for i := range spec.Nodes {
		inDegree[spec.Nodes[i].ID] = 0
	}
	for _, e := range spec.Edges {
		if e.Source != e.Target { // skip self-loops for in-degree; caught separately
			inDegree[e.Target]++
		}
	}
	for _, e := range spec.Edges {
		if e.Source == e.Target {
			return true // self-loop is a cycle
		}
	}

	// Kahn's: remove zero-in-degree nodes iteratively.
	queue := make([]string, 0)
	for id, deg := range inDegree {
		if deg == 0 {
			queue = append(queue, id)
		}
	}
	removed := 0
	for len(queue) > 0 {
		node := queue[0]
		queue = queue[1:]
		removed++
		for _, e := range spec.Edges {
			if e.Source == node {
				inDegree[e.Target]--
				if inDegree[e.Target] == 0 {
					queue = append(queue, e.Target)
				}
			}
		}
	}
	return removed < len(spec.Nodes)
}

func findStartNodes(spec *Spec) []string {
	hasIncoming := make(map[string]bool, len(spec.Nodes))
	for _, e := range spec.Edges {
		hasIncoming[e.Target] = true
	}
	var starts []string
	for i := range spec.Nodes {
		if !hasIncoming[spec.Nodes[i].ID] {
			starts = append(starts, spec.Nodes[i].ID)
		}
	}
	return starts
}

func bfsReachable(spec *Spec, starts []string) map[string]bool {
	adj := make(map[string][]string)
	for _, e := range spec.Edges {
		adj[e.Source] = append(adj[e.Source], e.Target)
	}
	reachable := make(map[string]bool, len(spec.Nodes))
	queue := make([]string, 0, len(starts))
	for _, s := range starts {
		if !reachable[s] {
			reachable[s] = true
			queue = append(queue, s)
		}
	}
	for len(queue) > 0 {
		node := queue[0]
		queue = queue[1:]
		for _, neighbor := range adj[node] {
			if !reachable[neighbor] {
				reachable[neighbor] = true
				queue = append(queue, neighbor)
			}
		}
	}
	return reachable
}

func applyDefaults(spec *Spec, defaults DefaultsBlock) {
	for i := range spec.Nodes {
		n := &spec.Nodes[i]
		if defaults.MaxAttempts != nil && n.MaxAttempts == 0 {
			n.MaxAttempts = *defaults.MaxAttempts
		}
		if defaults.Timeout != "" && n.Timeout == "" {
			n.Timeout = defaults.Timeout
		}
	}
}
