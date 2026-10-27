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
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// planEpicArm is a plan payload's resolved epic arm (design 0070 §1a/§3):
// exactly one of create (a new epic's definition) or attach (an existing
// epic's id).
type planEpicArm struct {
	create *EpicInput
	attach string
}

// resolvePlanEpicArm enforces the exactly-one-of contract: a payload
// carries EITHER a new-epic arm (the epic object, or the legacy
// epicTitle/outcome spelling the REST/external surfaces keep) OR an
// existing epicId to attach its tasks to. Both → 400; neither → 400;
// two spellings of the create arm → 400. Never a silent pick.
func resolvePlanEpicArm(in PlanInput) (planEpicArm, error) {
	epicObj := in.Epic != nil
	legacy := strings.TrimSpace(in.EpicTitle) != ""
	attach := strings.TrimSpace(in.EpicID) != ""
	creates := 0
	if epicObj {
		creates++
	}
	if legacy {
		creates++
	}
	switch {
	case creates > 0 && attach:
		return planEpicArm{}, &GuardError{Code: GuardInput,
			Detail: "a plan carries either a NEW epic (the epic object, or the legacy epicTitle/outcome triple) OR an existing epicId to attach its tasks to — not both"}
	case creates > 1:
		return planEpicArm{}, &GuardError{Code: GuardInput,
			Detail: "a plan's new-epic arm is spelled once: the epic object OR the legacy epicTitle/outcome triple, not both"}
	case creates == 0 && !attach:
		return planEpicArm{}, &GuardError{Code: GuardInput,
			Detail: "a plan needs an epic arm: the epic object (create a new epic with these tasks) or epicId (attach these tasks to an existing epic) — neither is present"}
	}
	if attach {
		return planEpicArm{attach: strings.TrimSpace(in.EpicID)}, nil
	}
	if epicObj {
		return planEpicArm{create: &EpicInput{Title: in.Epic.Title, Outcome: in.Epic.Outcome, Tags: in.Epic.Tags}}, nil
	}
	return planEpicArm{create: &EpicInput{Title: in.EpicTitle, Outcome: in.Outcome, Tags: in.Tags}}, nil
}

// ImportPlan materializes (or validates) one plan payload in a project.
// The plan's epic arm (design 0070 §1a) selects the mode: a new epic
// created with its tasks in the one transaction, or tasks attached to
// an EXISTING epic (§3) — all-or-nothing in both modes.
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
	arm, err := resolvePlanEpicArm(in)
	if err != nil {
		return nil, err
	}
	// #1948: a terminal target fails the WHOLE transaction — the guard
	// fires before any write, so nothing strands (all-or-nothing).
	// import_plan's create arm mints a FRESH draft epic inside its one
	// transaction (only the archived project can fire there); the
	// ATTACH arm resolves the existing epic up front so the same
	// starvation guard covers it — a terminal epic would leave every
	// attached task silently unfed. Replay-first (the #1948 precedent's
	// discipline, service.go's CreateTask arm): the terminal-refusal is
	// gated on replay == nil — a requestId that already landed renders
	// the original outcome even after the target terminalized, never a
	// guard error on the state the original write left behind. The
	// resolution and the belongs-to-project check stay PRE-replay:
	// they are static identity facts, not state.
	var existing *Epic
	if arm.attach != "" {
		existing, err = s.store.resolveEpic(ctx, s.q(), "", arm.attach)
		if err != nil {
			return nil, err
		}
		if existing.ProjectID != p.ID {
			return nil, &GuardError{Code: GuardInput,
				Detail: "epic belongs to another project — a plan attaches tasks within the URL project only"}
		}
	}
	replay, err := s.replayOf(ctx, p.ID, requestID, "import_plan")
	if err != nil {
		return nil, err
	}
	if replay == nil {
		if err := refuseArchivedProjectCreateTarget(p); err != nil {
			return nil, err
		}
		if existing != nil {
			if err := refuseTerminalEpicCreateTarget(existing); err != nil {
				return nil, err
			}
		}
	}

	validation := s.validatePlan(in, arm)
	if dryRun {
		return &ImportResult{DryRun: true, Plan: validation, ProjectID: p.ID, Warnings: validation.Warnings}, nil
	}
	if !validation.Valid {
		return nil, &GuardError{Code: "validation", Detail: validation.Error}
	}

	epicID := uuid.NewString()
	out := &ImportResult{DryRun: false, ProjectID: p.ID, TaskKeys: map[string]string{}, Warnings: validation.Warnings}
	ew := &eventWriter{svc: s, projectID: p.ID, requestID: requestID, action: "import_plan", itemKind: "epic", itemID: epicID, actor: actor}
	outcome, dup, err := ew.run(ctx, func(tx pgx.Tx) (map[string]any, error) {
		// r1 (#1948 review): the pre-write guard is check-then-act —
		// re-check the target status under its row lock INSIDE the
		// write transaction (the TaskStart in-tx precedent), so a
		// concurrent terminalization either waits for this commit or
		// aborts it — a mid-flight terminal never strands nodes.
		// The failpoint between the two checks is the deterministic
		// race seam the kill-pin arms.
		if s.failpoint != nil {
			if ferr := s.failpoint("import_plan.recheck"); ferr != nil {
				return nil, ferr
			}
		}
		var projStatus string
		if err := tx.QueryRow(ctx,
			`SELECT status FROM projects WHERE id=$1 FOR UPDATE`, p.ID).Scan(&projStatus); err != nil {
			return nil, err
		}
		if err := refuseArchivedProjectCreateStatus(p.ShortID, projStatus); err != nil {
			return nil, err
		}
		epicName := ""
		if arm.attach != "" {
			// The attach arm's in-tx re-check: the epic row itself,
			// under lock, before a single task lands (all-or-nothing —
			// a terminalized target aborts the whole plan).
			var status string
			if err := tx.QueryRow(ctx,
				`SELECT status FROM epics WHERE id=$1 FOR UPDATE`, existing.ID).Scan(&status); err != nil {
				return nil, err
			}
			if err := refuseTerminalEpicCreateStatus(existing.ShortID, status); err != nil {
				return nil, err
			}
			epicID = existing.ID
			epicName = existing.Title
			out.EpicShortID = existing.ShortID
		} else {
			shortID, err := mintUnique(ctx, txQ{tx}, "epics", PrefixEpic, taskMintOwnerScope, actor.UserID)
			if err != nil {
				return nil, err
			}
			out.EpicShortID = shortID
			epicName = firstNonEmpty(arm.create.Title, "Imported plan")
			if _, err := tx.Exec(ctx, `
			INSERT INTO epics (id, project_id, short_id, title, outcome, status, project_name, tags, quantum)
			VALUES ($1, $2, $3, $4, $5, 'draft', $6, $7, $8)`,
				epicID, p.ID, shortID, epicName, arm.create.Outcome, p.Name, tagsOrEmpty(arm.create.Tags), s.cfg.DRRQuantum); err != nil {
				return nil, err
			}
		}
		ids := map[string]string{}
		for _, n := range in.Tasks {
			taskID := uuid.NewString()
			tshort, err := mintUnique(ctx, txQ{tx}, "tasks", PrefixTask, taskMintOwnerScope, actor.UserID)
			if err != nil {
				return nil, err
			}
			if _, err := tx.Exec(ctx, `
			INSERT INTO tasks (id, project_id, epic_id, short_id, title, description,
			                   project_name, epic_name, tags, acceptance_criteria)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)`,
				taskID, p.ID, epicID, tshort, n.Title, n.Description,
				p.Name, epicName, tagsOrEmpty(n.Tags), criteriaJSON(n.AcceptanceCriteria)); err != nil {
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
		return map[string]any{"epic_id": epicID, "epic_short_id": out.EpicShortID, "tasks": out.TaskKeys}, nil
	})
	if err != nil {
		return nil, err
	}
	_ = dup
	// Replay render (ew.run's duplicate discipline, the CreateTask
	// precedent): the outcome map is the ORIGINAL's recorded payload
	// when the requestId already landed — render from it, so a retry
	// returns the original result, never a phantom fresh-uuid epic or
	// an empty task map, whatever the target's CURRENT state.
	if eid, _ := outcome["epic_id"].(string); eid != "" {
		epicID = eid
	}
	if sid, _ := outcome["epic_short_id"].(string); sid != "" {
		out.EpicShortID = sid
	}
	if keys, ok := outcome["tasks"].(map[string]any); ok && len(keys) > 0 {
		out.TaskKeys = make(map[string]string, len(keys))
		for k, v := range keys {
			if sv, ok := v.(string); ok {
				out.TaskKeys[k] = sv
			}
		}
	}
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
// Arm-aware (§1a): the CREATE arm validates its epic definition; the
// ATTACH arm has none to validate (the existing epic is guarded at
// resolution, not here).
func (s *Service) validatePlan(in PlanInput, arm planEpicArm) *PlanValidation {
	v := &PlanValidation{Valid: true}
	// #1837 P1b: caps + corruption heuristics on every text field the
	// plan materializes (dryRun reports them the same way — a plan
	// that would store corruption never reaches the approval moment).
	if arm.create != nil {
		if err := ValidateEpicInput(*arm.create); err != nil {
			v.Valid = false
			v.Error = err.Error()
			return v
		}
	}
	seen := map[string]bool{}
	for _, n := range in.Tasks {
		if err := ValidateTaskInput(TaskInput{Title: n.Title, Description: n.Description, Tags: n.Tags, AcceptanceCriteria: n.AcceptanceCriteria}); err != nil {
			v.Valid = false
			v.Error = fmt.Sprintf("task %q: %s", n.Key, err.Error())
			return v
		}
		// #1849: the plan materializes tasks — the seed contract rides
		// the dry-run too (the approval moment never blesses a
		// seedless node).
		if err := validateTaskSeed(TaskInput{Title: n.Title, Description: n.Description, AcceptanceCriteria: n.AcceptanceCriteria}); err != nil {
			v.Valid = false
			v.Error = fmt.Sprintf("task %q: %s", n.Key, err.Error())
			return v
		}
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
