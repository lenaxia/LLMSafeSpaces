// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package board

// plan.go — #1747 batch materialization: import_plan creates one epic
// + its tasks + dependency edges from a single payload ATOMICALLY
// (all-or-nothing), with a DRY-RUN validate mode rendering the graph,
// warnings, and the initial ready set BEFORE any write — the user's
// plan-approval moment. Graph semantics live here, in pkg/board, as
// the single source (guards at write; the reconciler re-derives
// readiness from the same rules) — never handler-side.

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// ImportPlan materializes (or validates) one plan payload in a project.
func (s *Service) ImportPlan(ctx context.Context, actor Actor, projectID, shortIDOrID string, in PlanInput, dryRun bool, requestID string) (*ImportResult, error) {
	p, err := s.store.resolveProject(ctx, s.q(), actor.UserID, pick(projectID, shortIDOrID))
	if err != nil {
		return nil, err
	}
	if err := requireOwner(actor, p.UserID); err != nil {
		return nil, err
	}
	if err := requireManagerAction(actor, p); err != nil {
		return nil, err
	}

	validation := s.validatePlan(in)
	if dryRun {
		return &ImportResult{DryRun: true, Plan: validation, ProjectID: p.ID, Warnings: validation.Warnings}, nil
	}
	if !validation.Valid {
		return nil, &GuardError{Code: "validation", Detail: validation.Error}
	}

	epicID := uuid.NewString()
	out := &ImportResult{DryRun: false, ProjectID: p.ID, TaskKeys: map[string]string{}, Warnings: validation.Warnings}
	ew := &eventWriter{svc: s, projectID: p.ID, requestID: requestID, action: "import_plan", itemKind: "epic", itemID: epicID, actor: actor}
	_, dup, err := ew.run(ctx, func(tx pgx.Tx) (map[string]any, error) {
		shortID, err := mintUnique(ctx, txQ{tx}, "epics", PrefixEpic, ` AND project_id=$2`, p.ID)
		if err != nil {
			return nil, err
		}
		out.EpicShortID = shortID
		if _, err := tx.Exec(ctx, `
			INSERT INTO epics (id, project_id, short_id, title, outcome, status, project_name, tags, quantum)
			VALUES ($1, $2, $3, $4, $5, 'draft', $6, $7, $8)`,
			epicID, p.ID, shortID, firstNonEmpty(in.EpicTitle, "Imported plan"), in.Outcome, p.Name, tagsOrEmpty(in.Tags), s.cfg.DRRQuantum); err != nil {
			return nil, err
		}
		ids := map[string]string{}
		for _, n := range in.Tasks {
			taskID := uuid.NewString()
			tshort, err := mintUnique(ctx, txQ{tx}, "tasks", PrefixTask, ` AND project_id=$2`, p.ID)
			if err != nil {
				return nil, err
			}
			if _, err := tx.Exec(ctx, `
				INSERT INTO tasks (id, project_id, epic_id, short_id, title, description,
				                   project_name, epic_name, tags, acceptance_criteria)
				VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)`,
				taskID, p.ID, epicID, tshort, n.Title, n.Description,
				p.Name, firstNonEmpty(in.EpicTitle, "Imported plan"), tagsOrEmpty(n.Tags), criteriaJSON(n.AcceptanceCriteria)); err != nil {
				return nil, err
			}
			ids[n.Key] = taskID
			out.TaskKeys[n.Key] = tshort
		}
		for _, e := range validation.Graph.Edges {
			if _, err := tx.Exec(ctx, `
				INSERT INTO board_dependencies (task_id, depends_on_task_id, created_by)
				VALUES ($1, $2, $3) ON CONFLICT DO NOTHING`, ids[e.From], ids[e.To], p.UserID); err != nil {
				return nil, mapDepErr(err, p.ID)
			}
		}
		for _, e := range validation.Graph.Edges {
			if err := rederiveBlocked(ctx, txQ{tx}, ids[e.From]); err != nil {
				return nil, err
			}
		}
		return map[string]any{"epic_id": epicID, "epic_short_id": shortID, "tasks": out.TaskKeys}, nil
	})
	if err != nil {
		return nil, err
	}
	_ = dup
	out.EpicID = epicID
	ready, err := s.EpicInitialReadySet(ctx, epicID)
	if err == nil {
		out.InitialReadySet = ready
	}
	s.pokeDrive(p.ID)
	return out, nil
}

// validatePlan runs the full guard set over a payload WITHOUT touching
// the database: key integrity, self-edges (rejected), duplicate edges
// (deduped — idempotent no-ops), acyclicity (Go DFS), size caps, and
// renders the graph + warnings + initial ready set (keys, plan order).
func (s *Service) validatePlan(in PlanInput) *PlanValidation {
	v := &PlanValidation{Valid: true}
	seen := map[string]bool{}
	for _, n := range in.Tasks {
		if n.Key == "" {
			v.Valid = false
			v.Error = "every plan task needs a key"
			return v
		}
		if seen[n.Key] {
			v.Valid = false
			v.Error = fmt.Sprintf("duplicate task key %q", n.Key)
			return v
		}
		if n.Title == "" {
			v.Valid = false
			v.Error = fmt.Sprintf("task %q needs a title", n.Key)
			return v
		}
		seen[n.Key] = true
		v.Graph.Nodes = append(v.Graph.Nodes, PlanNode{Key: n.Key, Title: n.Title})
	}
	if len(in.Tasks) > s.cfg.MaxNodesPerEpic {
		v.Valid = false
		v.Error = fmt.Sprintf("plan holds %d tasks (max %d per epic) — split the plan", len(in.Tasks), s.cfg.MaxNodesPerEpic)
		return v
	}
	if len(in.Tasks) == 0 {
		v.Warnings = append(v.Warnings, "plan carries no tasks — only the epic will be created")
	}

	edgeSeen := map[string]bool{}
	deg := map[string]int{}
	adj := map[string][]string{} // from → to (depends on)
	for _, n := range in.Tasks {
		for _, dep := range n.DependsOn {
			if !seen[dep] {
				v.Valid = false
				v.Error = fmt.Sprintf("task %q depends on unknown key %q", n.Key, dep)
				return v
			}
			if dep == n.Key {
				v.Valid = false
				v.Error = fmt.Sprintf("task %q cannot depend on itself", n.Key)
				return v
			}
			pair := n.Key + "\x1f" + dep
			if edgeSeen[pair] {
				v.Warnings = append(v.Warnings, fmt.Sprintf("duplicate edge %s→%s deduplicated", n.Key, dep))
				continue
			}
			edgeSeen[pair] = true
			deg[n.Key]++
			deg[dep]++
			if deg[n.Key] > s.cfg.MaxEdgesPerNode || deg[dep] > s.cfg.MaxEdgesPerNode {
				v.Valid = false
				v.Error = fmt.Sprintf("edge cap reached (max %d per node) at %s→%s", s.cfg.MaxEdgesPerNode, n.Key, dep)
				return v
			}
			v.Graph.Edges = append(v.Graph.Edges, PlanEdge{From: n.Key, To: dep})
			adj[n.Key] = append(adj[n.Key], dep)
		}
	}
	if cycle := findPlanCycle(in.Tasks, adj); cycle != "" {
		v.Valid = false
		v.Error = fmt.Sprintf("dependency edge would create a cycle through task %q", cycle)
		return v
	}
	// Isolated nodes are legal — noted, not refused.
	blocked := map[string]bool{}
	for _, e := range v.Graph.Edges {
		blocked[e.From] = true
	}
	for _, n := range in.Tasks {
		if !blocked[n.Key] {
			v.Warnings = append(v.Warnings, fmt.Sprintf("task %q has no prerequisites (ready immediately)", n.Key))
			v.InitialReadySet = append(v.InitialReadySet, n.Key)
		}
	}
	return v
}

// findPlanCycle DFS-walks the plan graph; returns a key on a cycle, ""
// when acyclic.
func findPlanCycle(tasks []PlanTaskInput, adj map[string][]string) string {
	const (
		white = 0
		gray  = 1
		black = 2
	)
	color := map[string]int{}
	var visit func(k string) string
	visit = func(k string) string {
		color[k] = gray
		for _, next := range adj[k] {
			switch color[next] {
			case gray:
				return next
			case white:
				if found := visit(next); found != "" {
					return found
				}
			}
		}
		color[k] = black
		return ""
	}
	for _, n := range tasks {
		if color[n.Key] == white {
			if found := visit(n.Key); found != "" {
				return found
			}
		}
	}
	return ""
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}
