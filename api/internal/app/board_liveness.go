// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package app

// board_liveness.go — the seamless lease's opencode adapter (design
// reports/specs/seamless-lease-design.md; owner rulings 2026-10-27/28):
// the production AssigneeLivenessSource + NudgeSender at the app
// integration point, read-only over sessionstate.
//
// The engine (pkg/board) stays harness-agnostic: it asks four POOR
// questions per assignee — moved? exists? anything after the sentinel?
// busy? — and this file is the only place that knows how opencode
// answers them. Three evidence legs, deliberately composed:
//
//   - The 0069 frame stream (FINE-grained): the usagestream singleton
//     holds one subscription per active pod, and EVERY applied frame —
//     PART_START tool-part events included, which no other leg sees —
//     stamps the in-process activity index. THIS is the leg that makes
//     "a worker mid-command longer than the lease window" observable:
//     a tool-call START is movement evidence the moment it happens, not
//     when the command completes.
//   - session_index (DURABLE): message_count/updated_at deltas survive
//     stream drops and serve cross-replica; message boundaries only.
//   - the usagestream gate's derived-busy truth (2026-10-29 busy-gate):
//     the consult-maintained #1574 answer for "is the turn still
//     running" — mid-tool-call silence is ALIVE, never a wedge.
//
// Both legs are SAME-SOURCE deltas (this process's previous answer) or
// SAME-ROW counters (the index's own message_count then vs now) — no
// cross-process timestamp is ever compared. The ordinal wedge query is
// answered in-frame-order: any frame received after the sentinel's
// send-mark is at the sentinel's transcript position or later (the
// stream is seq-ordered and live; a replayed old frame can only err
// FALSE-ALIVE — the safe direction, never a wrong reclaim of a live
// worker). The DB leg backs it when no stream was held: message_count
// above its at-send snapshot means something landed after the nudge.
//
// Honesty mapping (liveness.go's contract):
//
//   - row present, deleted_at NULL    → Observed, Exists
//   - row present, deleted_at NOT NULL → Observed, dead-evidence (the
//     soft delete is a deliberate write — provably gone)
//   - row absent                       → Observed=false (the index is
//     event-fed and can lag; an unindexed session is NO-evidence,
//     never death — the engine falls to the honest wall-clock path)
//   - query failure                    → all refs no-evidence + a loud
//     warn (per-sweep degradation IS the Nil contract; a cheap batched
//     SELECT needs no circuit-breaker to converge next sweep)

import (
	"context"
	"encoding/json"
	"errors"
	"expvar"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	abiv1 "github.com/lenaxia/llmsafespaces/pkg/abi/v1"
	"github.com/lenaxia/llmsafespaces/pkg/board"
	"github.com/lenaxia/llmsafespaces/pkg/shortid"
)

// boardObserveBudget bounds one ObserveAssignees call (the seam's ~2s
// hygiene: the reconciler sweeps on a cadence and stalling here stalls
// reclaims globally).
const boardObserveBudget = 2 * time.Second

// boardLivenessDegraded counts ObserveAssignees calls that degraded to
// no-evidence (the design's loud-gauge requirement: a fleet-wide
// regression to wall-clock semantics must be VISIBLE — expvar, exported
// on /debug/vars as board_liveness_degraded).
var boardLivenessDegraded expvar.Int

func init() { expvar.Publish("board_liveness_degraded", &boardLivenessDegraded) }

// nudgeMarkTTL bounds how long a sentinel's send-mark stays answerable:
// lease windows are 45m; a mark older than 2h can anchor no live episode
// and is evictable state (the hub's maps are process-local and otherwise
// unbounded).
const nudgeMarkTTL = 2 * time.Hour

// boardLivenessMaxEntries caps the frame/cursor maps (small entries, but
// never-growing over process lifetime); over the cap the entries not
// touched by the current sweep are pruned oldest-first.
const boardLivenessMaxEntries = 8192

// boardLivenessHub is the shared state of the two adapter halves: the
// frame-activity index (fed by the usagestream observer), the nudge
// send-marks (fed by the NudgeSender), and the per-ref observation
// cursors (the same-source delta baselines). One hub per process; the
// Service and the Reconciler MUST share it so a nudge sent by either is
// ordinal-anchored for both.
type boardLivenessHub struct {
	pool *pgxpool.Pool

	mu      sync.Mutex
	counter uint64 // monotonic frame stamp; also the send-mark clock
	// frames stamps the last frame-arrival stamp per key (the RAW
	// harness id AND the wire phrase both get stamped — #1817: frames
	// carry the phrase on emission, the raw id internally).
	frames map[string]uint64
	// nudges holds the send-mark per nudge id: the frame stamp at send
	// and the session_index message_count snapshot at send.
	nudges map[string]boardNudgeMark
	// cursors holds the previous observation per assignee ref.
	cursors map[string]boardLiveCursor

	// busyFn is the derived-busy leg (the 2026-10-29 busy-gate): the
	// usagestream consumer's consult-maintained #1574 truth, read per
	// sweep for the wedge verdict's candidates. nil (unwired) is INERT
	// — Busy stays false and the ladder runs today's semantics. Read
	// under mu (set once at wiring, before the first sweep).
	busyFn boardBusySource
}

// boardBusySource answers "is this session mid-turn (derived busy)?" —
// the consumer's gate map; ok=false is unknown (no gate, no answer),
// never busy.
type boardBusySource func(workspaceID, harnessSessionID string) (busy, ok bool)

// SetBusySource wires the derived-busy leg (app.go, once, before the
// first sweep; late-bound like the frame observer so construction order
// stays free).
func (h *boardLivenessHub) SetBusySource(fn boardBusySource) {
	h.mu.Lock()
	h.busyFn = fn
	h.mu.Unlock()
}

// boardNudgeMark anchors one sentinel's ordinal position.
type boardNudgeMark struct {
	frameStamp uint64
	dbCount    int64     // message_count at send; -1 = the row was unreadable
	markedAt   time.Time // eviction clock (nudgeMarkTTL)
}

// boardLiveCursor is the same-source delta baseline for one ref.
type boardLiveCursor struct {
	frameStamp uint64
	dbCount    int64 // message_count at the previous observation; -1 absent
}

func newBoardLivenessHub(pool *pgxpool.Pool) *boardLivenessHub {
	return &boardLivenessHub{
		pool:    pool,
		frames:  map[string]uint64{},
		nudges:  map[string]boardNudgeMark{},
		cursors: map[string]boardLiveCursor{},
	}
}

// ObserveFrame is the usagestream OnFrame observer: one stamp per
// applied frame, under both identity keys the frame carries. Cheap by
// contract (a map write under one mutex); it must never block or panic
// the fold goroutine.
func (h *boardLivenessHub) ObserveFrame(_ string, evt *abiv1.Event) {
	if evt == nil {
		return
	}
	h.mu.Lock()
	h.counter++
	stamp := h.counter
	for _, key := range [...]string{evt.GetInternalSessionId(), evt.GetSessionId()} {
		if key != "" {
			h.frames[key] = stamp
		}
	}
	h.mu.Unlock()
}

// pruneLocked evicts expired nudge send-marks (nudgeMarkTTL) and, over
// boardLivenessMaxEntries, prunes the oldest frame/cursor entries — the
// hub's maps are process-local and otherwise unbounded (the review's
// slow-leak finding). Caller holds h.mu.
func (h *boardLivenessHub) pruneLocked() {
	if len(h.nudges) > 0 {
		for id, m := range h.nudges {
			if time.Since(m.markedAt) > nudgeMarkTTL {
				delete(h.nudges, id)
			}
		}
	}
	if len(h.frames) <= boardLivenessMaxEntries && len(h.cursors) <= boardLivenessMaxEntries {
		return
	}
	// Over the cap: drop the quarter of each map with the smallest
	// stamps (the least-recently-answered refs). A dropped frame stamp
	// can only cost one gratuitous renewal (the safe direction); a
	// dropped cursor re-baselines on the next observation.
	type kv struct {
		k string
		v uint64
	}
	pruneUint := func(m map[string]uint64) {
		if len(m) <= boardLivenessMaxEntries {
			return
		}
		entries := make([]kv, 0, len(m))
		for k, v := range m {
			entries = append(entries, kv{k, v})
		}
		sort.Slice(entries, func(i, j int) bool { return entries[i].v < entries[j].v })
		for _, e := range entries[:len(entries)/4] {
			delete(m, e.k)
		}
	}
	pruneUint(h.frames)
	if len(h.cursors) > boardLivenessMaxEntries {
		entries := make([]kv, 0, len(h.cursors))
		for k, c := range h.cursors {
			entries = append(entries, kv{k, c.frameStamp})
		}
		sort.Slice(entries, func(i, j int) bool { return entries[i].v < entries[j].v })
		for _, e := range entries[:len(entries)/4] {
			delete(h.cursors, e.k)
		}
	}
}

// frameStampLocked-ish helpers keep the observe path under one lock
// hold (see ObserveAssignees).

// markNudge records a sentinel's send-mark: the current frame stamp and
// the assignee's session_index message_count snapshot. Called BEFORE
// delivery — a mark that precedes its sentinel can only err false-alive.
func (h *boardLivenessHub) markNudge(ctx context.Context, nudgeID, ref string) {
	h.mu.Lock()
	h.counter++
	mark := boardNudgeMark{frameStamp: h.counter, dbCount: -1, markedAt: time.Now()}
	h.mu.Unlock()
	if h.pool != nil {
		var count int64
		err := h.pool.QueryRow(ctx,
			`SELECT message_count FROM session_index WHERE session_id=$1`, ref).Scan(&count)
		if err == nil {
			mark.dbCount = count
		} else if !errors.Is(err, pgx.ErrNoRows) {
			// The unreadable snapshot still anchors the frame leg; the
			// DB leg of the ordinal answer sits this episode out.
			slog.WarnContext(ctx, "board: lease nudge could not snapshot message_count (frame leg only)",
				"session", ref, "err", err.Error())
		}
	}
	h.mu.Lock()
	h.nudges[nudgeID] = mark
	h.mu.Unlock()
}

// sessionIndexRow is one resolved assignee's durable evidence.
type sessionIndexRow struct {
	ref          string // the engine's ref (as passed)
	nudgeID      string // the engine's live nudge anchor ("" = skip)
	rawID        string // session_index.session_id
	phrase       string // session_index.short_id ("" when unset)
	workspaceID  string // session_index.workspace_id (the busy leg's gate scope)
	messageCount int64
	deleted      bool
	present      bool
}

// resolveRefs batch-resolves the sweep's refs against session_index.
// RAW refs (the spawner's ses_-form mint) resolve by session_id — a
// session id is a globally unique platform key (the boardTurnEvidence
// assumption); PHRASE-form refs resolve by short_id, where a
// cross-workspace phrase collision merges at most two sessions'
// evidence (bounded: the answer feeds a boolean; the wrong direction is
// a wasted nudge, never a wrong reclaim of a provably-moving worker).
func (h *boardLivenessHub) resolveRefs(ctx context.Context, refs []board.AssigneeRef) ([]sessionIndexRow, error) {
	if h == nil || h.pool == nil {
		return nil, errors.New("board: liveness hub has no pool")
	}
	var raw, phrases []string
	for _, r := range refs {
		if shortid.IsShortIDForm(r.SessionID) {
			phrases = append(phrases, shortid.Normalize(r.SessionID))
		} else {
			raw = append(raw, r.SessionID)
		}
	}
	byRaw := map[string]sessionIndexRow{}
	byPhrase := map[string]sessionIndexRow{}
	scan := func(rows pgx.Rows, refKey func(sessionIndexRow) string, into map[string]sessionIndexRow) error {
		defer rows.Close()
		for rows.Next() {
			var r sessionIndexRow
			var deleted *time.Time
			if err := rows.Scan(&r.rawID, &r.workspaceID, &r.phrase, &r.messageCount, &deleted); err != nil {
				return err
			}
			r.present = true
			r.deleted = deleted != nil
			into[refKey(r)] = r
		}
		return rows.Err()
	}
	if len(raw) > 0 {
		rows, err := h.pool.Query(ctx,
			`SELECT session_id, workspace_id, coalesce(short_id,''), message_count, deleted_at
			 FROM session_index WHERE session_id = ANY($1)`, raw)
		if err != nil {
			return nil, err
		}
		if err := scan(rows, func(r sessionIndexRow) string { return r.rawID }, byRaw); err != nil {
			return nil, err
		}
	}
	if len(phrases) > 0 {
		rows, err := h.pool.Query(ctx,
			`SELECT session_id, workspace_id, coalesce(short_id,''), message_count, deleted_at
			 FROM session_index WHERE lower(short_id) = ANY($1)`, phrases)
		if err != nil {
			return nil, err
		}
		if err := scan(rows, func(r sessionIndexRow) string { return r.phrase }, byPhrase); err != nil {
			return nil, err
		}
	}
	out := make([]sessionIndexRow, 0, len(refs))
	for _, ref := range refs {
		row, ok := byRaw[ref.SessionID]
		if !ok && shortid.IsShortIDForm(ref.SessionID) {
			row, ok = byPhrase[shortid.Normalize(ref.SessionID)]
		}
		if !ok {
			out = append(out, sessionIndexRow{ref: ref.SessionID, nudgeID: ref.NudgeID})
			continue
		}
		row.ref = ref.SessionID
		row.nudgeID = ref.NudgeID
		out = append(out, row)
	}
	return out, nil
}

// ObserveAssignees implements board.AssigneeLivenessSource: one batched
// session_index read + the in-process frame index, composed into the
// three facts per ref. Failure of the DB leg answers EVERY ref
// no-evidence (never death) and logs loud — the per-sweep degradation
// is exactly the Nil contract, and the next sweep retries one cheap
// query (no circuit-breaker state to strand).
func (h *boardLivenessHub) ObserveAssignees(sweepCtx context.Context, sess board.SessionObservations) board.SessionObservationResults {
	ctx, cancel := context.WithTimeout(sweepCtx, boardObserveBudget)
	defer cancel()
	// The degraded-sweep gauge (the design's "loud" requirement): an
	// expvar counter, exported on /debug/vars — a silently dead source
	// regresses the fleet to wall-clock semantics; this makes the
	// regression observable.
	boardLivenessDegraded.Add(0) // publish the var on first use

	out := make(board.SessionObservationResults, len(sess.Refs))
	if len(sess.Refs) == 0 {
		return out
	}
	rows, err := h.resolveRefs(ctx, sess.Refs)
	if err != nil {
		boardLivenessDegraded.Add(1)
		slog.WarnContext(ctx, "board: assignee liveness degraded this sweep (no-evidence; wall-clock semantics)",
			"refs", len(sess.Refs), "err", err.Error(), "degraded_total", boardLivenessDegraded.String())
		for _, ref := range sess.Refs {
			out[ref.SessionID] = board.AssigneeObservation{Observed: false}
		}
		return out
	}

	h.mu.Lock()
	defer h.mu.Unlock()
	h.pruneLocked()
	for _, row := range rows {
		ref := row.ref
		if !row.present {
			// Unindexed: no-evidence (the index lags; absence is not
			// death). The cursor stays untouched — a later indexing
			// starts a clean baseline.
			out[ref] = board.AssigneeObservation{Observed: false}
			continue
		}
		obs := board.AssigneeObservation{Observed: true, Exists: !row.deleted}

		// The frame stamp: whichever identity key moved latest.
		stamp := h.frames[row.rawID]
		if p := h.frames[row.phrase]; p > stamp {
			stamp = p
		}
		cur := h.cursors[ref]
		if _, hadCursor := h.cursors[ref]; !hadCursor {
			// First observation of this ref: the DB leg establishes its
			// baseline NOW (a message_count days old is not this
			// episode's movement — it asserts itself from the next
			// sweep on), while the frame leg deliberately baselines at
			// zero: any stamp this process ever saw for the session is
			// post-spawn activity, and one possibly-gratuitous renewal
			// for a long-quiet session is the safe direction (the busy
			// worker — the live incident — renews on first sight
			// instead of eating a nudge; a corpse loses one window to
			// detection, never the reverse).
			cur.dbCount = row.messageCount
		}
		if !row.deleted &&
			(stamp > cur.frameStamp || row.messageCount > cur.dbCount) {
			obs.MovedSinceLastSweep = true
		}
		// The ordinal wedge arm: only the engine knows the live nudge
		// id; answer positionally when we hold its send-mark. NO mark
		// (another replica sent the sentinel; a restart; an eviction)
		// is NOT silence — NudgeMarkKnown stays false and the engine
		// re-nudges instead of wedging on an unmeasured answer.
		if row.nudgeID != "" {
			if mark, ok := h.nudges[row.nudgeID]; ok {
				obs.NudgeMarkKnown = true
				after := stamp > mark.frameStamp
				if !after && mark.dbCount >= 0 && row.messageCount > mark.dbCount {
					after = true // the DB leg: something landed after the send
				}
				obs.EventsAfterNudge = after
			}
		}
		// The busy-gate leg (2026-10-29 live-fire): the consumer's
		// derived-busy truth says the turn is still RUNNING — the
		// worker is inside a tool call (however silent) and the engine
		// must not wedge it. Honored only when this process has SEEN a
		// frame for the session (stamp>0): the event that flips a
		// session busy (a PART_START consult) also stamps this index,
		// so busy-with-no-frame-ever is an answer about a session this
		// replica never observed — distrust it (the safe direction is
		// today's semantics, never a wrong verdict). Residual
		// staleness (a stream dropped mid-turn, gate map holding a
		// stale-true) is bounded by the gate's own lifecycle: reconnect
		// re-seeds derived from the authority snapshot, and a dropped
		// gate takes the map with it.
		if !row.deleted && h.busyFn != nil && stamp > 0 {
			if busy, known := h.busyFn(row.workspaceID, row.rawID); known && busy {
				obs.Busy = true
			}
		}
		h.cursors[ref] = boardLiveCursor{frameStamp: stamp, dbCount: row.messageCount}
		out[ref] = obs
	}
	return out
}

// leaseNudgeSentinel is the additive agentmessage-v1 payload the
// sentinel carries (the grammar's unknown-key forward-compatibility
// makes this a legal v1 extension; parsers that don't know the keys
// read the message as plain text — exactly the prose fallback the
// design wants for grammar-ignorant workers).
type leaseNudgeSentinel struct {
	Mode    string `json:"mode"`
	TaskID  string `json:"taskId"`
	NudgeID string `json:"nudgeId"`
	Expects string `json:"expects"`
}

// leaseNudgeText composes the sentinel message: the v1 marker line
// carrying the ordinal anchor, then the surgical prose — the FULL
// nudge contract (item 6 amendment, owner ruling 2026-10-28: respond
// via board_heartbeat FIRST, then continue / advance / declare
// failure; NEVER a restart).
func leaseNudgeText(shortID string, p board.NudgePayload) string {
	payload, _ := json.Marshal(leaseNudgeSentinel{
		Mode:    "lease-nudge",
		TaskID:  shortID,
		NudgeID: p.NudgeID,
		Expects: "respond|advance|fail",
	})
	var b strings.Builder
	b.WriteString("<!-- lsp:agent-message-v1 ")
	b.Write(payload)
	b.WriteString(" -->\n")
	fmt.Fprintf(&b, "BOARD LEASE NUDGE: your claim on task %s has gone quiet. "+
		"First RESPOND (call board_heartbeat once, with your claimToken) to confirm you are active — "+
		"respond before running any long command so the wedge clock clears. "+
		"Then: continue working if there is more work to do; otherwise move your task to its next stage "+
		"(board_move_stage with your claimToken), or declare it failed with a reason if you cannot complete it. "+
		"Do NOT restart — assess your current state.", shortID)
	return b.String()
}

// newBoardNudger builds the production NudgeSender: resolve the task's
// scope, mark the sentinel's ordinal anchor (BEFORE delivery — a mark
// racing its sentinel can only err false-alive), and deliver over the
// V1 route with the bounce seam's fast-fail window so a DEAD session's
// 4xx returns synchronously (the engine falls to the wall-clock arm
// this sweep instead of waiting out a nudge no one will ever see).
func newBoardNudger(scope boardScopeResolver, sender boardV1Sender, hub *boardLivenessHub) board.NudgeSender {
	return func(ctx context.Context, sessionID string, p board.NudgePayload) error {
		userID, workspaceID, err := scope(ctx, p.TaskID)
		if err != nil {
			return fmt.Errorf("board: lease nudge scope: %w", err)
		}
		hub.markNudge(ctx, p.NudgeID, sessionID)
		return deliverBoardTextV1(ctx, sender, "lease nudge", userID, workspaceID, sessionID,
			leaseNudgeText(p.TaskShortID, p), boardV1FastFail)
	}
}
