// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package board

// delete.go — the destructive replacement for the ELIMINATED cancel
// (#1747 round 3 + refinements 6261349020/6261560142). Delete removes
// the OBJECT, not the history: every delete event embeds a full
// snapshot of the final object state (per-task on cascade — never an
// aggregate blob; nothing else versions task bodies). Dependent edges
// cascade away with the row; each ORPHANED dependent (a task that lost
// its depends_on edge) is listed in the response and gets its own
// "unblocked_by_delete" audit event — affected-party visibility,
// warning-only, never a refusal. NO auto-reconnect: A→B→C, delete B ⇒
// C becomes ready, no A→C appears. The optional reconnect=true flag on
// delete_task lets orphans inherit the deleted node's direct upstream
// edges (deduplicated, cycle-guarded) when intent says so.
//
// Ordering inside the transaction (the r1 fix): inherited edges are
// inserted BEFORE the row delete (the cycle guard must see the full
// live graph); orphan EVENTS + the is_blocked rederive run AFTER the
// delete — the FK cascade is what removes the stale edges, so an early
// rederive would still see them and leave the orphan blocked.

import (
	"context"
	"encoding/json"

	"github.com/jackc/pgx/v5"
)

// DeleteTask removes a task. Idle rows go straight; an in-flight row
// (live claim) gets a clean worker abort FIRST (the existing interrupt
// seam — best-effort; the row's removal is the authority), then the
// delete. Edges cascade via FK; orphans are surfaced + audited.
func (s *Service) DeleteTask(ctx context.Context, actor Actor, taskID, shortIDOrID string, reconnect bool, requestID string) (*DeleteResult, error) {
	t, p, err := s.taskProjectForActor(ctx, actor, taskID, shortIDOrID)
	if err != nil {
		return nil, err
	}
	abortWorker := t.Stage == StageInProgress && t.AssigneeSession != "" && s.aborter != nil

	out := &DeleteResult{ItemKind: "task", ID: t.ID, ShortID: t.ShortID}
	ew := &eventWriter{svc: s, projectID: p.ID, requestID: requestID, action: "delete_task", itemKind: "task", itemID: t.ID, actor: actor}
	_, dup, err := ew.run(ctx, func(tx pgx.Tx) (map[string]any, error) {
		// Orphans + upstream BEFORE the edge cascade takes the rows.
		orphans, upstream, err := edgesAround(ctx, tx, t.ID)
		if err != nil {
			return nil, err
		}
		out.OrphanedDependents = orphans.ids
		// Optional reconnect BEFORE the delete: the inherited edges
		// reference rows that persist (the deleted node's upstream),
		// and the cycle guard must see the full live graph.
		if reconnect {
			pairs, err := reconnectOrphansTx(ctx, tx, p, orphans, upstream)
			if err != nil {
				return nil, err
			}
			out.Reconnected = pairs
		}
		if _, err := tx.Exec(ctx, `DELETE FROM tasks WHERE id=$1`, t.ID); err != nil {
			return nil, err
		}
		// Orphan events + rederive AFTER the delete: the FK cascade has
		// taken the stale edges, so the rederive converges (with
		// reconnect, the inherited edges remain and keep the orphan
		// blocked-by-upstream — the intent).
		if err := writeOrphanEvents(ctx, tx, p.ID, t, orphans, actor); err != nil {
			return nil, err
		}
		return map[string]any{
			"snapshot": snapshotOf(t), "id": t.ID, "short_id": t.ShortID,
			"orphaned_dependents": out.OrphanedDependents, "reconnected": out.Reconnected,
		}, nil
	})
	if err != nil {
		return nil, err
	}
	_ = dup
	// Clean abort AFTER commit (#1747 round 3, r2 ordering fix): the
	// in-flight worker's turn is cut only once the row is REALLY gone —
	// a failed transaction must never cut a worker for a live task. The
	// lease/token died with the row, so the abort is cleanup, not the
	// authority.
	if abortWorker {
		_ = s.aborter(ctx, t.ID, t.AssigneeSession)
	}
	return out, nil
}

// DeleteEpic cascades: every task under the epic is deleted (each with
// its OWN snapshot event — no aggregate blob), then the epic. Refuses
// while any claim under the epic is in-flight unless force=true (force
// aborts each worker first). Orphans (cross-epic dependents OUTSIDE the
// epic) are surfaced + audited per deleted task.
func (s *Service) DeleteEpic(ctx context.Context, actor Actor, epicID, shortIDOrID string, force bool, requestID string) (*DeleteResult, error) {
	e, err := s.store.resolveEpic(ctx, s.q(), "", pick(epicID, shortIDOrID))
	if err != nil {
		return nil, err
	}
	p, err := s.store.resolveProject(ctx, s.q(), "", e.ProjectID)
	if err != nil {
		return nil, err
	}
	if err := requireOwner(actor, p.UserID); err != nil {
		return nil, err
	}
	if err := requireManagerAction(actor, p); err != nil {
		return nil, err
	}

	// In-flight guard: refuse without force.
	inFlight := s.inFlightUnder(ctx, "t.epic_id = $1", e.ID)
	if len(inFlight) > 0 && !force {
		return nil, &GuardError{Code: GuardEpicState, Detail: "claims in flight under the epic — force required (force=true aborts each worker first)"}
	}

	out := &DeleteResult{ItemKind: "epic", ID: e.ID, ShortID: e.ShortID}
	var tasks []*Task // the cascade set (post-commit force aborts read it)
	ew := &eventWriter{svc: s, projectID: p.ID, requestID: requestID, action: "delete_epic", itemKind: "epic", itemID: e.ID, actor: actor}
	_, dup, err := ew.run(ctx, func(tx pgx.Tx) (map[string]any, error) {
		rows, err := tx.Query(ctx, `SELECT `+taskColumns+` FROM tasks t WHERE t.epic_id=$1 ORDER BY t.created_at, t.id`, e.ID)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			t, err := scanTask(rows)
			if err != nil {
				rows.Close()
				return nil, err
			}
			tasks = append(tasks, t)
		}
		rows.Close()
		type orphanedPair struct {
			deleted *Task
			orphans edgeEndpoints
		}
		var externals []orphanedPair
		for _, t := range tasks {
			out.CascadeTasks = append(out.CascadeTasks, t.ShortID)
			// Orphans OUTSIDE the epic (the epic's own rows vanish too —
			// their dependents are inside the blast radius).
			orphans, _, err := edgesAround(ctx, tx, t.ID)
			if err != nil {
				return nil, err
			}
			external := edgeEndpoints{}
			for _, o := range orphans.nodes {
				if o.epicID != e.ID {
					external.nodes = append(external.nodes, o)
					external.ids = append(external.ids, o.shortID)
				}
			}
			if len(external.nodes) > 0 {
				externals = append(externals, orphanedPair{deleted: t, orphans: external})
				out.OrphanedDependents = append(out.OrphanedDependents, external.ids...)
			}
		}
		// Tasks (their edges cascade), then the epic.
		if _, err := tx.Exec(ctx, `DELETE FROM tasks WHERE epic_id=$1`, e.ID); err != nil {
			return nil, err
		}
		if _, err := tx.Exec(ctx, `DELETE FROM epics WHERE id=$1`, e.ID); err != nil {
			return nil, err
		}
		// Per-task snapshot events (content preservation) + external
		// orphan events — AFTER the cascade so the rederives converge.
		for _, t := range tasks {
			if err := insertEventTx(ctx, tx, p.ID,
				engineEventID("delete_task", t.ID, 0), "delete_task", "task", t.ID, actor,
				map[string]any{"snapshot": snapshotOf(t), "id": t.ID, "short_id": t.ShortID, "cascade_of": e.ShortID}); err != nil {
				return nil, err
			}
		}
		for _, ex := range externals {
			if err := writeOrphanEvents(ctx, tx, p.ID, ex.deleted, ex.orphans, actor); err != nil {
				return nil, err
			}
		}
		return map[string]any{"snapshot": snapshotOf(e), "cascade_tasks": out.CascadeTasks,
			"id": e.ID, "short_id": e.ShortID, "orphaned_dependents": out.OrphanedDependents}, nil
	})
	if err != nil {
		return nil, err
	}
	_ = dup
	// Force aborts AFTER commit (r2.2, mirroring DeleteTask's ordering
	// fix): a failed transaction must never cut in-flight workers for
	// tasks that still exist. The rows/leases died with the commit, so
	// the aborts are cleanup, not the authority.
	if force && s.aborter != nil {
		for _, t := range epicInFlight(tasks) {
			_ = s.aborter(ctx, t.id, t.assignee)
		}
	}
	return out, nil
}

// epicInFlight extracts the in-flight (task, assignee) pairs of a
// cascade set for the post-commit force aborts.
func epicInFlight(tasks []*Task) []struct{ id, assignee string } {
	var out []struct{ id, assignee string }
	for _, t := range tasks {
		if t.Stage == StageInProgress && t.AssigneeSession != "" {
			out = append(out, struct{ id, assignee string }{t.ID, t.AssigneeSession})
		}
	}
	return out
}

// edgeEndpoint is one side of an edge around a to-be-deleted task.
type edgeEndpoint struct {
	id, shortID, epicID string
}

// edgeEndpoints carries the endpoints + their rendered short ids.
type edgeEndpoints struct {
	nodes []edgeEndpoint
	ids   []string
}

// edgesAround loads a task's dependents (orphans-to-be) and upstream
// (prerequisites) before the FK cascade removes the edge rows.
func edgesAround(ctx context.Context, tx pgx.Tx, taskID string) (orphans, upstream edgeEndpoints, err error) {
	rows, err := tx.Query(ctx, `
		SELECT dt.id, dt.short_id, dt.epic_id FROM board_dependencies d
		JOIN tasks dt ON dt.id = d.task_id
		WHERE d.depends_on_task_id = $1`, taskID)
	if err != nil {
		return edgeEndpoints{}, edgeEndpoints{}, err
	}
	for rows.Next() {
		var n edgeEndpoint
		if err := rows.Scan(&n.id, &n.shortID, &n.epicID); err != nil {
			rows.Close()
			return edgeEndpoints{}, edgeEndpoints{}, err
		}
		orphans.nodes = append(orphans.nodes, n)
		orphans.ids = append(orphans.ids, n.shortID)
	}
	rows.Close()
	rows, err = tx.Query(ctx, `
		SELECT ut.id, ut.short_id, ut.epic_id FROM board_dependencies d
		JOIN tasks ut ON ut.id = d.depends_on_task_id
		WHERE d.task_id = $1`, taskID)
	if err != nil {
		return edgeEndpoints{}, edgeEndpoints{}, err
	}
	for rows.Next() {
		var n edgeEndpoint
		if err := rows.Scan(&n.id, &n.shortID, &n.epicID); err != nil {
			rows.Close()
			return edgeEndpoints{}, edgeEndpoints{}, err
		}
		upstream.nodes = append(upstream.nodes, n)
	}
	rows.Close()
	return orphans, upstream, nil
}

// reconnectOrphansTx implements the OPTIONAL reconnect flag
// (#1747 6261560142): each orphaned dependent inherits the deleted
// node's direct upstream edges — deduplicated (ON CONFLICT), cycle
// guarded (the DB trigger), self-edges skipped. Rendered as
// "orphan→upstream" short-id pairs. Runs BEFORE the row delete (the
// cycle guard needs the full live graph).
func reconnectOrphansTx(ctx context.Context, tx pgx.Tx, p *Project, orphans, upstream edgeEndpoints) ([]string, error) {
	var pairs []string
	for _, o := range orphans.nodes {
		for _, u := range upstream.nodes {
			if o.id == u.id {
				continue // an orphan cannot depend on itself
			}
			if _, err := tx.Exec(ctx, `
				INSERT INTO board_dependencies (task_id, depends_on_task_id, created_by)
				VALUES ($1, $2, $3) ON CONFLICT DO NOTHING`, o.id, u.id, p.UserID); err != nil {
				return nil, mapDepErr(err, p.ID)
			}
			pairs = append(pairs, o.shortID+"→"+u.shortID)
		}
	}
	return pairs, nil
}

// writeOrphanEvents emits one "unblocked_by_delete of <id>" audit event
// per orphaned dependent (affected-party visibility, warning-only) and
// rederives each orphan's is_blocked — call AFTER the row delete so the
// cascade has taken the stale edges.
func writeOrphanEvents(ctx context.Context, tx pgx.Tx, projectID string, deleted *Task, orphans edgeEndpoints, actor Actor) error {
	for _, o := range orphans.nodes {
		if err := insertEventTx(ctx, tx, projectID,
			"engine:unblocked_by_delete:"+deleted.ID+":"+o.id, "unblocked_by_delete", "task", o.id, actor,
			map[string]any{"of": deleted.ShortID, "orphan": o.shortID}); err != nil {
			return err
		}
		if err := rederiveBlocked(ctx, txQ{tx}, o.id); err != nil {
			return err
		}
	}
	return nil
}

// snapshotOf renders a row's full final state for a delete event's
// payload (content preservation — nothing else versions task bodies).
func snapshotOf(v any) map[string]any {
	b, err := json.Marshal(v)
	if err != nil {
		return map[string]any{}
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		return map[string]any{}
	}
	return m
}
