// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package handlers

import (
	"context"
	"errors"
	"net/http"
	"sort"
	"sync"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/lenaxia/llmsafespaces/api/internal/interfaces"
	"github.com/lenaxia/llmsafespaces/api/internal/services/sessionindex"
	agent "github.com/lenaxia/llmsafespaces/pkg/agent"
	"github.com/lenaxia/llmsafespaces/pkg/types"
)

// sessionGone messages are the #1340 typed gone-state body, house
// pattern (code + human message, mirroring the 422
// text_only_model_image_history surface). The code discriminator drives
// the frontend's gone-state rendering; the message is for humans and
// MUST be true for the guard outcome: a reaped row is gone from the
// list; a history-bearing row the guard kept is still listed pending
// operator review (r2 finding — one message for both was a user-visible
// untruth in the kept case).
const (
	sessionGoneReaped    = "This session no longer exists on the agent — it may have been deleted. It has been removed from your session list."
	sessionGoneKept      = "This session no longer exists on the agent — it may have been deleted. Its history entry remains in your session list pending review."
	sessionGoneUncertain = "This session no longer exists on the agent — it may have been deleted. It may take a short while to disappear from your session list."
)

func writeSessionGoneBody(c *gin.Context, message string) {
	writeSessionGoneBodyWithSuggestions(c, message, nil)
}

// writeSessionGoneBodyWithSuggestions is the #1667-generalized gone
// body (design 0066 §4.6): the typed 410 keeps its code + message
// contract (the frontend discriminator) and gains the structured
// did-you-mean payload additively — a truncated or mistyped id that
// matches nothing at the agent now names its near neighbors instead
// of answering silence. Suggestions are omitted (not empty) when
// nothing scores: the payload is information, not a shape.
func writeSessionGoneBodyWithSuggestions(c *gin.Context, message string, suggestions []any) {
	body := gin.H{
		"code":  "session_gone",
		"error": message,
	}
	if len(suggestions) > 0 {
		body["suggestions"] = suggestions
	}
	c.JSON(http.StatusGone, body)
}

// SessionOriginLister feeds the #1901 carrier reap-guard bypass: the
// workspace's CARRIER-class origin marks — session ids the platform
// itself deleted (session_origins.kind='carrier'), the ONLY class the
// bypass admits. Satisfied in production by an adapter over the
// workflows store's ListCarrierOriginMarks; preserved sessions' marks
// (kind='session') are invisible through this seam BY CONTRACT, so the
// blanket-bypass defect (a harness store reset reaping a preserved
// session's history row — #1901 item b) is unreachable at the handler.
type SessionOriginLister interface {
	ListCarrierOriginMarks(ctx context.Context, workspaceID string) (map[string]bool, error)
}

// SetSessionOriginLister wires the #1901 carrier-mark lookup. Nil (or
// never called) disables the bypass — the reap guard keeps its
// DefaultReapGuard-only semantics.
func (h *ProxyHandler) SetSessionOriginLister(l SessionOriginLister) {
	h.sessionOriginLister = l
}

// carrierOriginMarks returns the workspace's carrier-class marks for
// the reap guard. Fail-safe: a lookup error degrades to NO marks (the
// history-bearing row stays kept_for_operator — uncertainty never
// widens a reap).
func (h *ProxyHandler) carrierOriginMarks(ctx context.Context, workspaceID string) map[string]bool {
	if h.sessionOriginLister == nil {
		return nil
	}
	marks, err := h.sessionOriginLister.ListCarrierOriginMarks(ctx, workspaceID)
	if err != nil {
		h.logger.Warn("session reconcile: carrier marks lookup failed; carrier bypass disabled for this pass",
			"workspaceID", workspaceID, "error", err.Error())
		return nil
	}
	return marks
}

// reapSessionIfGone classifies an adapter error from a session-scoped
// read: when the agent's definitive verdict is not-found
// (agent.ErrSessionNotFound), the session_index row is a stale ghost
// (#1340). The TYPED 410 always answers (the session is unreadable
// either way); the row itself is reaped only when the #1340 triage
// safety guard admits it (no message history — nothing of value, or a
// #1882 origin-marked platform carrier: its deletion was platform
// policy, not a harness-store reset). A history-bearing unmarked row
// the harness lost stays for operator attention (a vanished session
// WITH history may indicate a harness store reset). Any other error
// (transport, 5xx, timeouts) returns false so the caller keeps its
// generic path: an unreachable pod is not evidence of deletion.
func (h *ProxyHandler) reapSessionIfGone(c *gin.Context, workspaceID, sessionID string, err error, route string) bool {
	if !errors.Is(err, agent.ErrSessionNotFound) {
		return false
	}
	// Design 0066 §4.6 (the #1667 generalization to the API surface):
	// the agent's definitive not-found verdict is exactly the 404-class
	// outcome suggestions attach to — never to authz/transport/5xx.
	suggestions := h.sessionSuggestions(c, workspaceID, sessionID)
	if h.sessionIndex != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		rows, listErr := h.sessionIndex.ListByWorkspace(ctx, workspaceID)
		if listErr != nil {
			h.logger.Warn("session_gone: index lookup failed; row left for the convergence pass",
				"route", route, "workspaceID", workspaceID, "sessionID", sessionID, "error", listErr.Error())
		}
		marks := h.carrierOriginMarks(ctx, workspaceID)
		for _, row := range rows {
			// The middleware canonicalizes to the harness id; index
			// rows carry the phrase in ID post-#1756 — compare on
			// InternalID or minted rows never match and every
			// 404-class session reports an "uncertain" reap verdict.
			if row.InternalID != sessionID {
				continue
			}
			if sessionindex.DefaultReapGuard(row) || marks[sessionID] {
				if delErr := h.sessionIndex.DeleteSession(ctx, workspaceID, sessionID); delErr != nil {
					h.logger.Warn("session_gone: index row reap failed",
						"route", route, "workspaceID", workspaceID, "sessionID", sessionID, "error", delErr.Error())
					writeSessionGoneBody(c, sessionGoneUncertain)
				} else {
					if marks[sessionID] {
						sessionindex.ReconcileOutcome("reaped_carrier")
					}
					writeSessionGoneBodyWithSuggestions(c, sessionGoneReaped, suggestions)
				}
			} else {
				h.logger.Warn("session_gone: history-bearing row kept for operator review",
					"route", route, "workspaceID", workspaceID, "sessionID", sessionID,
					"messageCount", row.MessageCount)
				writeSessionGoneBodyWithSuggestions(c, sessionGoneKept, suggestions)
			}
			cancel()
			return true
		}
		cancel()
	}
	// No confirmed reap (nil index, lookup error, or row not indexed):
	// never claim removal — the convergence pass will converge it.
	writeSessionGoneBodyWithSuggestions(c, sessionGoneUncertain, suggestions)
	return true
}

// reconcileCadence bounds how often a workspace runs the #1340
// convergence pass: the sidebar refreshes often, but one harness
// session-list round-trip per workspace per 30s is the S5b/L10 budget.
// The claim is CROSS-REPLICA (wsstate SETNX-style): one replica runs
// each window, so the counter read-modify-write below has a single
// writer — the "N counts checks, not per-replica repetitions" invariant
// is enforced, not approximated.
const reconcileCadence = 30 * time.Second

// reconcileMissThreshold is the consecutive-absent check count before a
// ghost row is deleted (issue #1340: N=2 survives one mid-restart
// snapshot miss).
const reconcileMissThreshold = 2

// ReconcileSessionIndex is the #1340 convergence pass, piggybacked on
// the sidebar's session list (the path that already overlays harness
// ground truth). Fire-and-forget: cadence-claimed per workspace
// (cross-replica), diffs the index rows against the agent's
// authoritative session list, and deletes guard-admitted rows the agent
// has reported absent for N consecutive checks. The miss counters live
// in wsstate (cross-replica with decay).
func (h *ProxyHandler) ReconcileSessionIndex(ctx context.Context, workspaceID string) {
	if h.sessionIndex == nil {
		return
	}
	if !h.state().ClaimReconcileTurn(ctx, workspaceID, reconcileCadence) {
		return
	}
	go h.runSessionIndexReconciliation(workspaceID) //nolint:gosec,contextcheck // G118: intentional fire-and-forget detach (BackfillSessionParents precedent)
}

// runSessionIndexReconciliation deliberately uses a detached
// context.Background() bounded by a 15s timeout: the pass benefits
// future requests, not this one, and must survive client disconnect.
func (h *ProxyHandler) runSessionIndexReconciliation(workspaceID string) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	rows, err := h.sessionIndex.ListByWorkspace(ctx, workspaceID)
	if err != nil {
		h.logger.Debug("session reconcile: index list failed", "workspaceID", workspaceID, "error", err.Error())
		return
	}
	if len(rows) == 0 {
		return
	}

	prev := h.state().GetReconcileMisses(ctx, workspaceID)
	// The harness list is walked ONCE and shared by the reap plan and
	// the count rebuild (#1481) — both consumers need the same
	// present-set; a second ListSessions per pass would double the
	// harness load for zero information.
	var presentSet map[string]bool
	lister := func() (map[string]bool, error) {
		if presentSet != nil {
			return presentSet, nil
		}
		sessions, err := h.adapter.ListSessions(ctx, "", workspaceID)
		if err != nil {
			return nil, err
		}
		present := make(map[string]bool, len(sessions))
		for _, s := range sessions {
			present[s.ID] = true
		}
		presentSet = present
		return present, nil
	}

	// #1901 carrier bypass (discriminated): CARRIER-class origin marks
	// (session_origins.kind='carrier' — platform-deleted ephemeral
	// routine carriers) are admitted past the history exception even
	// with message_count > 0 — the harness-store-reset rationale does
	// not apply to sessions the platform itself created and deleted.
	// Preserved sessions' marks (kind='session') never reach this
	// guard: their absence from the harness keeps the
	// kept_for_operator review (a store reset is exactly that
	// scenario for them — #1901 item b). Every admitted row still
	// faces the #1850 by-ID probe below.
	marks := h.carrierOriginMarks(ctx, workspaceID)
	reap := func(row types.SessionListItem) bool {
		return sessionindex.DefaultReapGuard(row) || marks[row.InternalID]
	}
	remove, keptForOperator, next := sessionindex.PlanReconciliation(rows, lister, prev, reconcileMissThreshold, reap)
	// keptForOperator rows keep a threshold-level counter so every
	// subsequent pass re-detects them — the operator signal fires
	// consistently, never every-other-pass (r2 finding).
	for _, id := range keptForOperator {
		next[id] = reconcileMissThreshold
	}
	h.state().SetReconcileMisses(ctx, workspaceID, next)
	for _, id := range remove {
		// #1850: the harness LIST is not a liveness authority — sessions
		// created through the harness/Act path (exactly the
		// create_session spawn class) can be live and readable by ID
		// while the list omits them (#1452). A fresh spawn's row sits at
		// message_count==0 for its whole (long) first turn, so two
		// list-miss passes reaped LIVE sessions — the first
		// turn-boundary event then re-registered the row and the
		// registration seam minted a NEW phrase (the old phrase dead:
		// sends to it failed, the session re-emitted flagged). Confirm
		// gone by ID before deleting — the SessionExists precedent: the
		// by-ID probe is immune to the list-visibility gap. ONLY the
		// probe's definitive not-found verdict reaps; every other
		// outcome (alive, transport/5xx error, or a sessionless 2xx
		// answer) keeps the row — uncertainty never deletes.
		alive, perr := h.probeSessionAliveByID(ctx, workspaceID, id)
		gone := perr != nil && errors.Is(perr, agent.ErrSessionNotFound)
		switch {
		case perr == nil && alive:
			// Live, list-invisible: presence. Keep the row (its phrase
			// is the session's addressable identity — #1756 §1), reset
			// the miss counter so the next absence starts a fresh count.
			sessionindex.ReconcileOutcome("reap_vetoed_live")
			h.logger.Info("session reconcile: reap vetoed — session live by ID, absent from the harness list (#1452 class)",
				"workspaceID", workspaceID, "sessionID", id)
			delete(next, id)
			h.state().SetReconcileMisses(ctx, workspaceID, next)
			continue
		case gone:
			// Definitive not-found verdict — fall through to the delete.
		default:
			// Indeterminate probe (transport/5xx error, or a sessionless
			// 2xx answer — reachable: ParseSessionWire answers (session,
			// nil) with an empty ID for empty 2xx bodies): defer to the
			// next pass, counter pinned at threshold so the probe
			// retries (the harness-error-never-reaps rule, applied to
			// the confirmation itself).
			sessionindex.ReconcileOutcome("reap_deferred_probe")
			h.logger.Warn("session reconcile: reap deferred — by-ID probe indeterminate",
				"workspaceID", workspaceID, "sessionID", id, "error", perr)
			next[id] = reconcileMissThreshold
			h.state().SetReconcileMisses(ctx, workspaceID, next)
			continue
		}
		if err := h.sessionIndex.DeleteSession(ctx, workspaceID, id); err != nil {
			sessionindex.ReconcileOutcome("delete_failed")
			// Pin the counter at threshold: a failed delete retries on
			// the NEXT pass, not two passes later (the plan moved the
			// row to remove without persisting its miss state).
			next[id] = reconcileMissThreshold
			h.state().SetReconcileMisses(ctx, workspaceID, next)
			h.logger.Warn("session reconcile: ghost delete failed", "workspaceID", workspaceID, "sessionID", id, "error", err.Error())
			continue
		}
		sessionindex.ReconcileOutcome("reaped")
		if marks[id] {
			sessionindex.ReconcileOutcome("reaped_carrier")
		}
		h.logger.Info("session reconcile: reaped ghost index row",
			"workspaceID", workspaceID, "sessionID", id)
	}
	// #1850 truthfulness split (review r1's robustness nit): the kept
	// population is TWO populations with different meanings —
	// history-bearing rows the guard held for operator review, and
	// fresh count-zero rows sheltered by the spawn-window grace (NOT
	// history-bearing; count 0). One "history-bearing" metric+log for
	// both conflated them. The row lookup re-derives which is which;
	// a kept id missing from the lookup keeps the operator-review
	// classification (never worse than the old single bucket).
	byInternal := make(map[string]types.SessionListItem, len(rows))
	for _, row := range rows {
		byInternal[row.InternalID] = row
	}
	for _, id := range keptForOperator {
		if row, ok := byInternal[id]; ok && row.MessageCount == 0 {
			sessionindex.ReconcileOutcome("kept_spawn_window")
			h.logger.Warn("session reconcile: spawn-window grace kept fresh row (not a ghost until the grace lapses)",
				"workspaceID", workspaceID, "sessionID", id)
			continue
		}
		// #1340 triage guard: history-bearing rows the harness reports
		// gone are NOT auto-deleted — metric + Warn for operator review
		// (a vanished session with history may indicate a harness store
		// reset; the read path still answers the typed gone-state).
		sessionindex.ReconcileOutcome("kept_for_operator")
		h.logger.Warn("session reconcile: history-bearing row absent at harness, kept for operator review",
			"workspaceID", workspaceID, "sessionID", id)
	}

	h.backfillShortIDs(ctx, workspaceID, rows)
	h.rebuildMessageCounts(ctx, workspaceID, rows, presentSet)
}

// probeSessionAliveByID is the #1850 reap confirmation: the by-ID
// existence probe (the send_message SessionExists precedent — immune
// to the #1452 harness list-visibility gap). A found session reports
// alive; an adapter not-found verdict reports !alive with the typed
// error the caller matches as the ONE definitive gone-verdict; every
// other outcome — transport/5xx errors, or a sessionless 2xx answer
// (nil session / empty ID with nil error, which ParseSessionWire
// produces for empty 2xx bodies) — is indeterminate and the caller
// DEFERS (uncertainty never deletes).
func (h *ProxyHandler) probeSessionAliveByID(ctx context.Context, workspaceID, sessionID string) (bool, error) {
	s, err := h.adapter.GetSession(ctx, "", workspaceID, sessionID)
	if err != nil {
		return false, err
	}
	if s == nil || s.ID == "" {
		return false, nil
	}
	return true, nil
}

// shortIDBackfillBudget bounds the short-id mint pass (design 0066
// §4.2): steady state is zero NULL rows; the budget only paces the
// one-time rollout catch-up so a single reconcile pass never turns
// into a workspace-wide write storm.
const shortIDBackfillBudget = 25

// backfillShortIDs is the design 0066 convergence sweep's short-id
// half: rows whose short_id is still NULL (predating the rollout, or a
// registration path that raced/failed) are registered — mint-and-
// persist exactly once, idempotently; the LRU makes the steady state
// a pure row scan with no writes.
func (h *ProxyHandler) backfillShortIDs(ctx context.Context, workspaceID string, rows []types.SessionListItem) {
	if h.sessionIndex == nil {
		return
	}
	minted := 0
	for _, row := range rows {
		if row.ShortID != "" {
			continue
		}
		if minted >= shortIDBackfillBudget {
			break
		}
		// #1882 r1: the rows are a PRE-PASS snapshot — a session whose
		// confirmed delete propagated mid-pass (tombstone set, row
		// removed) must not be re-registered: Register is a
		// row-creating mint, so the dead carrier would be re-born.
		//nolint:contextcheck // the tombstone read is store-local; the sweep's detached ctx never reaches it (isSessionDeleted precedent)
		if h.isSessionDeleted(workspaceID, row.InternalID) {
			continue
		}
		if h.sessionIndex.Register(ctx, workspaceID, row.InternalID, "opencode") != "" {
			minted++
		}
	}
	if minted > 0 {
		sessionindex.ReconcileOutcome("short_id_backfilled")
		h.logger.Info("session reconcile: backfilled short ids",
			"workspaceID", workspaceID, "count", minted)
	}
}

// reconcileCountBudget bounds the count-rebuild walks per pass (#1481).
// Measured on live pods (the #1452 lane's probe): a full 500-message
// page costs 240–490ms over localhost, and typical sessions fit ONE
// page (the pinned session list carries no count field — the walk is
// the cheapest source). At K=3 the steady-state pass adds ≤3 single-GET
// walks (typical); every present row converges within
// ceil(N/3)×30s (≤17min at a 100-row workspace, <4min typically; the
// #1886 uncapped listing scales N with the workspace's own rows).
// The 15s pass timeout bounds the pathological multi-page tail.
const reconcileCountBudget = 3

// countPassIndex is the rebuild rotation's window index: wall-clock
// buckets of the reconcile cadence (stateless — every replica computes
// the same bucket for a given instant, and the cross-replica cadence
// claim means ONE replica runs each window's pass, so consecutive
// claimed windows advance the rotation by exactly one step). Tests pin
// it via countPassIndexOverride instead of sleeping past boundaries.
func (h *ProxyHandler) countPassIndex(now time.Time) int64 {
	if h.countPassIndexOverride != nil {
		return *h.countPassIndexOverride
	}
	return now.Unix() / int64(reconcileCadence.Seconds())
}

// rebuildMessageCounts repairs drifted message_count values for PRESENT
// sessions from the harness ground truth (#1481, the #754 fold-in): the
// incremental RecordMessage path double-counts duplicate SSE events,
// and without a rebuild the drift persists forever. Oldest-touched rows
// first (steady-state fairness); drift-only writes; walk failures skip
// the row — a vanished session is the miss-machinery's business, never
// a count error. The incremental path stays as the between-passes
// approximation.
func (h *ProxyHandler) rebuildMessageCounts(ctx context.Context, workspaceID string, rows []types.SessionListItem, present map[string]bool) {
	if present == nil || len(rows) == 0 {
		// #1880 direction 1: a pass-level skip must be loud. present ==
		// nil means the harness LIST failed (or the pass ran without a
		// list at all) — the entire rebuild silently returned for the
		// duration, which is one of the three suspected non-convergence
		// classes in the issue. One outcome + one Warn per pass.
		if present == nil && len(rows) > 0 {
			sessionindex.ReconcileOutcome("count_skip_pass_no_list")
			h.logger.Warn("session reconcile: count rebuild skipped — no harness list this pass",
				"workspaceID", workspaceID)
		}
		return
	}
	candidates := make([]types.SessionListItem, 0, len(rows))
	for _, row := range rows {
		if present[row.InternalID] {
			candidates = append(candidates, row)
			continue
		}
		// #1880 direction 1: the present-miss must be loud — but ONLY
		// for drift-suspect rows. A count-zero row the harness LIST
		// omits is exactly the #1452 list-invisibility class (live
		// spawn-created sessions the #1865 reap veto protects from
		// deletion): the rebuild can NEVER converge it here, one line
		// per skipped row makes that attributable from logs/metrics.
		// History-bearing absent rows are the ghost/operator machinery's
		// population (kept_for_operator + miss counters) — not this
		// signal, or every real ghost would pollute it.
		if row.MessageCount == 0 {
			sessionindex.ReconcileOutcome("count_skip_not_present")
			h.logger.Warn("session reconcile: count rebuild cannot reach list-invisible count-zero row (the #1452 gap; the listing serve-through owns its convergence)",
				"workspaceID", workspaceID, "sessionID", row.InternalID)
		}
	}
	if len(candidates) == 0 {
		return
	}
	// #1880 direction 1 (r2): the pass-ran denominator — this pass's
	// count leg executed with a live present-set. The issue's third
	// skip class ("the pass not being scheduled for this workspace at
	// all") was invisible: sidebar traffic with no count_pass_ran
	// names it from metrics alone.
	sessionindex.ReconcileOutcome("count_pass_ran")
	sort.SliceStable(candidates, func(i, j int) bool {
		a, b := candidates[i].LastMessageAt, candidates[j].LastMessageAt
		if a == nil {
			return b != nil
		}
		if b == nil {
			return false
		}
		return a.Before(*b)
	})
	if len(candidates) > reconcileCountBudget {
		// #1880 r2, thread Finding 2 (rebuild stationarity): the sort
		// key is last_message_at, but the count upsert never writes
		// that column — repaired rows STAY in the (stable, NULLs-first)
		// group, so the budget re-walked the same first-K candidates
		// every pass forever and rows K+1..N never walked (7 of the
		// issue's 10 evidence rows). Rotate the window instead: start
		// at (passIndex × K) mod N, the pass index a wall-clock bucket
		// of the reconcile cadence — stateless, and consistent under
		// the cadence claim (one replica runs each window's pass, so
		// every claimant computes the same window for its instant).
		// Union of P consecutive windows covers P×K consecutive
		// candidates: full coverage within ceil(N/K) passes. Workspaces
		// at or under the budget keep the deterministic oldest-first
		// walk (start 0).
		n := len(candidates)
		start := (int(h.countPassIndex(time.Now())) * reconcileCountBudget) % n
		rotated := make([]types.SessionListItem, 0, reconcileCountBudget+(n-start))
		rotated = append(rotated, candidates[start:]...)
		rotated = append(rotated, candidates[:start]...)
		candidates = rotated[:reconcileCountBudget]
	}
	for _, row := range candidates {
		count, err := h.adapter.CountMessages(ctx, "", workspaceID, row.InternalID)
		if err != nil {
			sessionindex.ReconcileOutcome("count_walk_error")
			// #1880 direction 1: Warn, not Debug — a silently skipped
			// row is indistinguishable from a converged one in prod
			// logs, which is how the drift went unnoticed for hours.
			h.logger.Warn("session reconcile: count walk failed; row skipped",
				"workspaceID", workspaceID, "sessionID", row.ID, "error", err.Error())
			continue
		}
		// #1882 r1 — the reconcile re-birth race: the count walk is the
		// validated window (240–490ms/page of network between the list
		// snapshot and this write). A session whose confirmed delete
		// propagated DURING the walk (tombstone set, row removed) must
		// not be re-written: Register + RebuildMessageCount are both
		// row-creating mints, so the dead carrier would be re-born with
		// message_count>0 — a DefaultReapGuard-refused ghost forever.
		//nolint:contextcheck // the tombstone read is store-local; the sweep's detached ctx never reaches it (isSessionDeleted precedent)
		if h.isSessionDeleted(workspaceID, row.InternalID) {
			continue
		}
		if count == row.MessageCount {
			sessionindex.ReconcileOutcome("count_unchanged")
			continue
		}
		// Design 0066 §4.2: the count rebuild is a row-creating upsert
		// shape — register through the seam first (review r1 finding 1;
		// the loop's own budget bounds the mint cost).
		h.sessionIndex.Register(ctx, workspaceID, row.InternalID, "opencode")
		// InternalID here too — RebuildMessageCount is a row-creating
		// upsert; passing the phrase (row.ID) mints a ghost row keyed
		// by the phrase while the real row never converges.
		if err := h.sessionIndex.RebuildMessageCount(ctx, workspaceID, row.InternalID, count); err != nil {
			h.logger.Warn("session reconcile: count rebuild write failed", "workspaceID", workspaceID, "sessionID", row.InternalID, "error", err.Error())
			continue
		}
		sessionindex.ReconcileOutcome("count_rebuilt")
	}
}

// listingCountBudget bounds the #1880 listing serve-through walks per
// request — the drift class converges across listing polls (the sidebar
// refreshes ~30s), so K=3 mirrors reconcileCountBudget's measured cost
// bound (≤3 single-GET walks, ~1.5s worst case, ONLY while drift rows
// exist). "Converged" is exact only for the proxy-traversed class; the
// never-traversed signature (last_message_at NULL) deliberately stays
// hot after a serve-through persist, so those rows re-walk on each
// cooldown expiry — bounded staleness by design, not a steady state of
// zero walks.
const listingCountBudget = 3

// countServeCooldown backs off a drift row whose last walk already
// matched the served count: a legitimately-empty session (walked 0,
// index 0) must not eat the walk budget of live drift rows on every
// listing poll. The cooldown bounds staleness for the never-traversed
// class (their count is served up to this interval old); history-
// bearing rows are never walked at all, and a row whose walk CHANGED
// the count re-arms on its own new tracker entry.
const countServeCooldown = 5 * time.Minute

// countServeCap bounds the walk-tracker maps (drift rows per process).
// Far above any sane workspace's drift cardinality; a workspace set
// this pathological resets the rotation state rather than growing.
const countServeCap = 8192

// countServeTracker is the in-replica walk state behind
// ServeHarnessMessageCounts: the last walk time and walked count per
// (workspace/session) drift row. Rotation fairness (never-walked
// first, then oldest — the rebuild pass's oldest-touched discipline on
// the read path) and the unchanged-row cooldown both read it. Deliberately
// NOT durable: any replica converges its own drift set, and a restart
// just re-warms.
type countServeTracker struct {
	mu     sync.Mutex
	lastAt map[string]time.Time
	lastCt map[string]int
}

func (t *countServeTracker) lookup(key string) (time.Time, int, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	at, ok := t.lastAt[key]
	if !ok {
		return time.Time{}, 0, false
	}
	return at, t.lastCt[key], true
}

func (t *countServeTracker) record(key string, count int, at time.Time) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.lastAt == nil {
		t.lastAt = map[string]time.Time{}
		t.lastCt = map[string]int{}
	}
	if len(t.lastAt) >= countServeCap {
		t.lastAt = map[string]time.Time{}
		t.lastCt = map[string]int{}
	}
	t.lastAt[key] = at
	t.lastCt[key] = count
}

// countServeState lazily initializes the serve-through tracker (the
// resolvers lazy-init precedent: struct-literal constructions in tests
// must not nil-panic on the first listing).
func (h *ProxyHandler) countServeState() *countServeTracker {
	h.countServeMu.Lock()
	defer h.countServeMu.Unlock()
	if h.countServe == nil {
		h.countServe = &countServeTracker{}
	}
	return h.countServe
}

// ServeHarnessMessageCounts is the #1880 direction-3 serve-through: the
// listing read path makes the HARNESS count authoritative for the
// drift class — rows whose message stream never traversed the API
// proxy (board/dispatch-spawned sessions: pod-local prompt delivery,
// no SSE subscriber) sit at message_count 0 forever because neither
// the incremental RecordMessage path nor the present-only reconcile
// rebuild can see them (#1452 list-visibility). For eligible rows
// (sessionindex.CountServeEligible) this walks the harness count by
// ID — the #1865 reap-veto's by-ID probe precedent, immune to the
// list gap — serves it on the caller's slice IN PLACE, and persists
// it through the rebuild seam (self-healing, the backfillContextUsed
// precedent). Budgeted (listingCountBudget walks/request),
// oldest-touched first across passes; unchanged rows back off for
// countServeCooldown. A failed walk serves the index value (never
// worse than the pre-fix wire) and counts the outcome loudly.
func (h *ProxyHandler) ServeHarnessMessageCounts(ctx context.Context, workspaceID string, sessions []types.SessionListItem) {
	// h.adapter is ctor-required (#828 — never nil-checked). The
	// sessionIndex seam is optional wiring; nil disables the overlay.
	if h.sessionIndex == nil || len(sessions) == 0 {
		return
	}
	now := time.Now()

	type candidate struct {
		idx      int
		walkedAt time.Time
		walked   bool
	}
	candidates := make([]candidate, 0, len(sessions))
	for i := range sessions {
		if !sessionindex.CountServeEligible(sessions[i]) {
			continue
		}
		key := workspaceID + "/" + sessions[i].InternalID
		at, walkedCt, walked := h.countServeState().lookup(key)
		if walked && walkedCt == sessions[i].MessageCount && now.Sub(at) < countServeCooldown {
			sessionindex.ReconcileOutcome("listing_count_cooldown")
			continue
		}
		candidates = append(candidates, candidate{idx: i, walkedAt: at, walked: walked})
	}
	if len(candidates) == 0 {
		return
	}
	// Rotation fairness: never-walked rows first, then oldest walk —
	// without this the same three rows would consume every pass's
	// budget and the tail would never converge.
	sort.SliceStable(candidates, func(a, b int) bool {
		if !candidates[a].walked {
			return candidates[b].walked
		}
		if !candidates[b].walked {
			return false
		}
		return candidates[a].walkedAt.Before(candidates[b].walkedAt)
	})
	if len(candidates) > listingCountBudget {
		candidates = candidates[:listingCountBudget]
	}
	for _, cand := range candidates {
		row := &sessions[cand.idx]
		key := workspaceID + "/" + row.InternalID
		persisted := row.MessageCount
		count, err := h.adapter.CountMessages(ctx, "", workspaceID, row.InternalID)
		if err != nil {
			// NO tracker entry on error (review r2 defect 1): the
			// error-path count is not a walked truth. Recording it
			// anyway poisoned the cooldown tracker on count-0 rows
			// (lastCt 0 == index 0 → the next listing cooldown-skipped
			// the retry for five minutes, mislabeled as
			// listing_count_cooldown). A failed walk re-arms.
			sessionindex.ReconcileOutcome("listing_count_walk_error")
			h.logger.Warn("session listing: harness count walk failed; serving the index value",
				"workspaceID", workspaceID, "sessionID", row.InternalID, "error", err.Error())
			continue
		}
		h.countServeState().record(key, count, time.Now())
		// Serve-through: the response carries harness truth.
		row.MessageCount = count
		if count == persisted {
			sessionindex.ReconcileOutcome("listing_count_unchanged")
			continue
		}
		// #1882 r4 F1: the walk above IS the race window — a carrier's
		// confirmed delete (tombstone + row removal, the propagation
		// tail) may have landed mid-walk. This writer's Register +
		// RebuildMessageCount is a row-CREATING mint at count>0 (the
		// only such writer post-r1), and a re-minted carrier row is a
		// permanent kept_for_operator ghost the reap guard refuses
		// (count>0). The tombstone consult — the same one-line seam
		// the r1 fix put before the two reconcile sweeps' registers.
		//nolint:contextcheck // the tombstone read is store-local; the listing's ctx never reaches it (isSessionDeleted precedent)
		if h.isSessionDeleted(workspaceID, row.InternalID) {
			sessionindex.ReconcileOutcome("listing_count_skip_tombstoned")
			continue
		}
		// Row-creating upsert shape — the registration seam first
		// (design 0066 §4.2; keyed by the harness id, never the
		// phrase — the #1756/#1881 discipline).
		h.sessionIndex.Register(ctx, workspaceID, row.InternalID, "opencode")
		if err := h.sessionIndex.RebuildMessageCount(ctx, workspaceID, row.InternalID, count); err != nil {
			sessionindex.ReconcileOutcome("listing_count_persist_error")
			h.logger.Warn("session listing: count persist failed (truth still served)",
				"workspaceID", workspaceID, "sessionID", row.InternalID, "error", err.Error())
			continue
		}
		sessionindex.ReconcileOutcome("listing_count_served")
	}
}

func (h *ProxyHandler) SetSessionIndex(si interfaces.SessionIndexService) {
	h.sessionIndex = si
}

// SetSessionRefs wires design 0066's resolution/suggestion surface (the
// database service). Nil (or never called) disables the layer.
func (h *ProxyHandler) SetSessionRefs(r sessionindex.RefResolver) {
	h.sessionRefs = r
}

// sessionSuggestions computes the did-you-mean payload for a failed
// session ref on this workspace — best-effort, bounded (§4.6), nil on
// any degradation. Used by the 404-class verdict paths only.
func (h *ProxyHandler) sessionSuggestions(c *gin.Context, workspaceID, ref string) []any {
	if h.sessionRefs == nil {
		return nil
	}
	userID, _ := extractAuth(c)
	out := sessionindex.SuggestionsFor(c.Request.Context(), h.sessionRefs, userID, workspaceID, ref)
	anyOut := make([]any, 0, len(out))
	for _, s := range out {
		anyOut = append(anyOut, s)
	}
	return anyOut
}

func (h *ProxyHandler) fetchAndPersistTitle(workspaceID, sessionID string) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	s, err := h.adapter.GetSession(ctx, "", workspaceID, sessionID)
	if err != nil || s == nil {
		return
	}
	h.persistSessionMeta(ctx, workspaceID, sessionID, s.Title, s.ParentID)
}

// persistSessionMeta writes the title and parentID to the session index
// from the Adapter's typed session.Session.
func (h *ProxyHandler) persistSessionMeta(ctx context.Context, workspaceID, sessionID, title, parentID string) {
	// Design 0066 registration seam: title/parent upserts are row-
	// creating shapes — register first (idempotent, LRU-guarded).
	if h.sessionIndex != nil {
		h.sessionIndex.Register(ctx, workspaceID, sessionID, "")
	}
	if title != "" {
		if err := h.sessionIndex.UpsertTitle(ctx, workspaceID, sessionID, title); err != nil {
			h.logger.Error("Failed to persist session title", err, "workspaceID", workspaceID, "sessionID", sessionID)
		}
	}
	if parentID != "" {
		if err := h.sessionIndex.UpsertParent(ctx, workspaceID, sessionID, parentID); err != nil {
			h.logger.Error("Failed to persist session parent", err, "workspaceID", workspaceID, "sessionID", sessionID)
		}
	}
}

func (h *ProxyHandler) BackfillSessionParents(ctx context.Context, workspaceID string) {
	if h.sessionIndex == nil {
		return
	}
	if h.state().GetParentBackfilled(ctx, workspaceID) {
		return
	}
	h.state().SetParentBackfilled(ctx, workspaceID)

	// Fire-and-forget: the backfill benefits future requests, not this one, so
	// it must survive client disconnect. runParentBackfill uses a detached
	// context.Background() bounded by a 15s timeout (not the request ctx).
	go h.runParentBackfill(workspaceID) //nolint:gosec,contextcheck // G118: intentional fire-and-forget detach
}

// runParentBackfill deliberately uses a detached context.Background() (bounded
// by a 15s timeout): the backfill must survive client disconnect (the request
// ctx is canceled once the response is flushed), which would abort the
// parent_session_id persistence. It takes no ctx so contextcheck does not
// expect propagation from the (request-scoped) caller.
func (h *ProxyHandler) runParentBackfill(workspaceID string) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	sessions, err := h.adapter.ListSessions(ctx, "", workspaceID)
	if err != nil {
		h.logger.Debug("Backfill: adapter ListSessions failed", "workspaceID", workspaceID, "error", err)
		h.state().DeleteParentBackfilled(ctx, workspaceID)
		return
	}
	written := 0
	for _, s := range sessions {
		if s.ID == "" || s.ParentID == "" {
			continue
		}
		// Design 0066 registration seam (parent-mirror backfill path).
		h.sessionIndex.Register(ctx, workspaceID, s.ID, "")
		if err := h.sessionIndex.UpsertParent(ctx, workspaceID, s.ID, s.ParentID); err != nil {
			h.logger.Debug("Backfill: upsert parent failed", "workspaceID", workspaceID, "sessionID", s.ID, "error", err)
			continue
		}
		written++
	}
	if written > 0 {
		h.logger.Info("Backfilled session parents", "workspaceID", workspaceID, "count", written)
	}
}
