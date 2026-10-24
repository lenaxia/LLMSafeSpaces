// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package board

// phrase_read.go — #1922: read-time session-phrase translation at the
// Service's PUBLIC-read edge. The stored rows stay canonical forever:
// scanTask keeps copying t.assignee_session verbatim (the bounce
// aborter, the reconciler, the stillborn guard, the halt/resume
// messenger, and the spawn stamp all consume the canonical harness id),
// and board_events.actor stays byte-identical (append-only evidence).
// Translation happens ONLY on the in-memory copies a public read (or a
// write echo, or the SSE projection) hands to a handler — covering the
// REST surface and the manager MCP pass-through in one seam.
//
// Degraded posture follows the #1817/#1821 SSE precedent: the phrase
// when the index knows it; the raw harness id + deprecation flag while
// the design-0066 registration seam has not yet observed the session
// (never worse). No additive internal_assignee_session — no consumer
// demonstrated correlation need (seam ruling, item 2).

import (
	"context"
	"encoding/json"
	"log/slog"

	"github.com/jackc/pgx/v5"

	"github.com/lenaxia/llmsafespaces/pkg/shortid"
)

// PhraseQueryer is the minimal consult surface the bounded lookup needs
// (satisfied by the board pool, pgx pools/txs generally).
type PhraseQueryer interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

// LookupSessionPhrases resolves raw harness session ids to their minted
// #1756 phrases within ONE workspace — the ruling's bounded lookup
// (session_index keyed on the project's target workspace; no
// cross-workspace fan-out). Sessions the index does not know are simply
// absent from the map; the CALLER owns the degraded posture (raw +
// flag). A consult failure logs and returns nil — the read degrades,
// never errors (the translation is a presentation concern).
func LookupSessionPhrases(ctx context.Context, q PhraseQueryer, workspaceID string, sessions []string) map[string]string {
	if workspaceID == "" || len(sessions) == 0 {
		return nil
	}
	rows, err := q.Query(ctx, `
		SELECT session_id, short_id FROM session_index
		WHERE workspace_id=$1 AND short_id IS NOT NULL AND session_id = ANY($2)`,
		workspaceID, sessions)
	if err != nil {
		slog.WarnContext(ctx, "board: phrase lookup failed — degrading to raw+flagged",
			"workspaceID", workspaceID, "err", err.Error())
		return nil
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var sid, phrase string
		if err := rows.Scan(&sid, &phrase); err == nil && phrase != "" {
			out[sid] = phrase
		}
	}
	return out
}

// TranslateActorSessionPhrase rewrites one actor JSON blob's
// session_id to its phrase form at READ time (board_events.actor is
// append-only — the stored row is never rewritten). The degraded shape
// matches the SSE precedent's markDeprecatedSessionID (#1821) exactly:
// the raw id passes through carrying session_id_deprecated="true".
// Blobs without a session_id (user/system actors on session-less
// surfaces) and already-translated phrases return byte-identical (the
// #1817 piece-3 double-translation guard).
func TranslateActorSessionPhrase(raw []byte, phrases map[string]string) []byte {
	if len(raw) == 0 {
		return raw
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil || m == nil {
		return raw
	}
	sid, _ := m["session_id"].(string)
	if sid == "" || shortid.IsShortIDForm(sid) {
		return raw
	}
	if phrase, ok := phrases[sid]; ok && phrase != "" {
		m["session_id"] = phrase
	} else {
		m["session_id_deprecated"] = "true"
	}
	out, err := json.Marshal(m)
	if err != nil {
		return raw
	}
	return out
}

// translateTaskReads rewrites the assignee_session fields of the given
// in-memory task copies to the phrase form (warm) or flags them
// deprecated (cold). One project→workspace query for the batch, then
// one bounded session_index consult per DISTINCT target workspace.
// Canonical store rows are never touched — only these response copies.
func (s *Service) translateTaskReads(ctx context.Context, tasks ...*Task) {
	byProject := map[string][]*Task{}
	for _, t := range tasks {
		if t == nil || t.AssigneeSession == "" || shortid.IsShortIDForm(t.AssigneeSession) {
			continue
		}
		byProject[t.ProjectID] = append(byProject[t.ProjectID], t)
	}
	if len(byProject) == 0 {
		return
	}
	projectIDs := make([]string, 0, len(byProject))
	for pid := range byProject {
		projectIDs = append(projectIDs, pid)
	}
	rows, err := s.q().Query(ctx,
		`SELECT id::text, coalesce(target_workspace_id::text,'') FROM projects WHERE id = ANY($1::uuid[])`,
		projectIDs)
	if err != nil {
		slog.WarnContext(ctx, "board: read-edge workspace resolve failed — degrading to raw+flagged", "err", err.Error())
		flagUntranslated(byProject)
		return
	}
	wsByProject := map[string]string{}
	for rows.Next() {
		var pid, ws string
		if err := rows.Scan(&pid, &ws); err == nil {
			wsByProject[pid] = ws
		}
	}
	rows.Close()

	// Group the distinct raw ids per workspace, then one bounded consult
	// per workspace.
	pending := map[string]map[string]bool{}
	for pid, ts := range byProject {
		ws := wsByProject[pid]
		if ws == "" {
			continue
		}
		if pending[ws] == nil {
			pending[ws] = map[string]bool{}
		}
		for _, t := range ts {
			pending[ws][t.AssigneeSession] = true
		}
	}
	phrases := map[string]string{}
	for ws, ids := range pending {
		sessions := make([]string, 0, len(ids))
		for sid := range ids {
			sessions = append(sessions, sid)
		}
		for sid, phrase := range LookupSessionPhrases(ctx, s.q(), ws, sessions) {
			phrases[sid] = phrase
		}
	}

	for _, ts := range byProject {
		for _, t := range ts {
			if phrase, ok := phrases[t.AssigneeSession]; ok && phrase != "" {
				t.AssigneeSession = phrase
				t.AssigneeSessionDeprecated = false
			} else {
				// Cold window (or consult failure): raw passes through,
				// flagged — the SSE precedent's never-worse degrade.
				t.AssigneeSessionDeprecated = true
			}
		}
	}
}

// flagUntranslated marks every pending task's assignee raw+flagged (the
// workspace-resolve failure path — the value IS the raw form).
func flagUntranslated(byProject map[string][]*Task) {
	for _, ts := range byProject {
		for _, t := range ts {
			t.AssigneeSessionDeprecated = true
		}
	}
}

// translateHealthLeases rewrites HealthLease response copies the same
// way; wsFor resolves each lease's project to its target workspace
// (the user-scoped health passes its map, the workspace-scoped health
// a constant closure).
func (s *Service) translateHealthLeases(ctx context.Context, leases []HealthLease, wsFor func(projectID string) string) {
	sessionsByWS := map[string]map[string]bool{}
	for i := range leases {
		if leases[i].AssigneeSession == "" || shortid.IsShortIDForm(leases[i].AssigneeSession) {
			continue
		}
		ws := wsFor(leases[i].ProjectID)
		if ws == "" {
			leases[i].AssigneeSessionDeprecated = true
			continue
		}
		if sessionsByWS[ws] == nil {
			sessionsByWS[ws] = map[string]bool{}
		}
		sessionsByWS[ws][leases[i].AssigneeSession] = true
	}
	phrases := map[string]string{}
	for ws, ids := range sessionsByWS {
		keys := make([]string, 0, len(ids))
		for sid := range ids {
			keys = append(keys, sid)
		}
		for sid, phrase := range LookupSessionPhrases(ctx, s.q(), ws, keys) {
			phrases[sid] = phrase
		}
	}
	for i := range leases {
		if leases[i].AssigneeSession == "" || shortid.IsShortIDForm(leases[i].AssigneeSession) {
			continue
		}
		if phrase, ok := phrases[leases[i].AssigneeSession]; ok && phrase != "" {
			leases[i].AssigneeSession = phrase
			leases[i].AssigneeSessionDeprecated = false
		} else {
			leases[i].AssigneeSessionDeprecated = true
		}
	}
}

// translateEventActors translates the actor blobs of an event batch at
// read time (TaskEvents): the task's project resolves the workspace
// (bounded — one project read + one consult), each actor's session_id
// becomes the phrase or the flagged raw form. The STORED rows are never
// rewritten.
func (s *Service) translateEventActors(ctx context.Context, projectID string, events []Event) {
	sessions := map[string]bool{}
	for _, ev := range events {
		if len(ev.Actor) == 0 {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal(ev.Actor, &m); err != nil || m == nil {
			continue
		}
		if sid, _ := m["session_id"].(string); sid != "" && !shortid.IsShortIDForm(sid) {
			sessions[sid] = true
		}
	}
	if len(sessions) == 0 {
		return
	}
	var ws string
	if err := s.q().QueryRow(ctx,
		`SELECT coalesce(target_workspace_id::text,'') FROM projects WHERE id=$1::uuid`, projectID).
		Scan(&ws); err != nil || ws == "" {
		slog.WarnContext(ctx, "board: events workspace resolve failed — actors degrade raw+flagged", "err", err)
	}
	keys := make([]string, 0, len(sessions))
	for sid := range sessions {
		keys = append(keys, sid)
	}
	phrases := LookupSessionPhrases(ctx, s.q(), ws, keys)
	for i := range events {
		events[i].Actor = TranslateActorSessionPhrase(events[i].Actor, phrases)
	}
}
