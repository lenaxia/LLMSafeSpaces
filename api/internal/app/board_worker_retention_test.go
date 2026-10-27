// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package app

// board_worker_retention_test.go — #1966 owner-ruling pins: the
// Workers folder carries a hard cap (default 50); beyond it the OLDEST
// worker sessions are hard-deleted, oldest-first — with the two safety
// disciplines the ruling demands:
//   - worker-only eviction: the deleter refuses anything that does not
//     carry a worker signal (origin 'worker' or a parent) — a
//     misclassification reaching the candidate list can NEVER delete a
//     user session, and each candidate is re-verified with a fresh
//     single-row read immediately before the delete;
//   - a grace floor: sessions updated less than an hour ago (mid-lease
//     workers) are never deleted.
//
// Born RED — the retainer did not exist.

import (
	"context"
	"testing"
	"time"

	wf "github.com/lenaxia/llmsafespaces/pkg/workflows"
)

type retentionRow = wf.WorkerSessionRow

type stubRetentionLister struct {
	rows   []retentionRow
	verify map[string]bool // session id -> IsWorkerSession verdict
}

func (s *stubRetentionLister) ListWorkerSessions(ctx context.Context, workspaceID string) ([]retentionRow, error) {
	return s.rows, nil
}

func (s *stubRetentionLister) IsWorkerSession(ctx context.Context, workspaceID, sessionID string) (bool, error) {
	if v, ok := s.verify[sessionID]; ok {
		return v, nil
	}
	return true, nil
}

func ago(d time.Duration) time.Time { return time.Now().Add(-d) }

func row(id string, originWorker, parented bool, lastActivity, updated time.Duration) retentionRow {
	return retentionRow{
		SessionID: id, OriginWorker: originWorker, Parented: parented,
		LastActivity: ago(lastActivity), UpdatedAt: ago(updated),
	}
}

// verifiedAll builds the default verify map (everything a real worker).
func verifiedAll(rows []retentionRow) map[string]bool {
	m := map[string]bool{}
	for _, r := range rows {
		m[r.SessionID] = true
	}
	return m
}

func newTestRetainer(lister workerRetentionLister, deleted *[]string, failIds map[string]bool) *boardWorkerRetainer {
	deleter := func(ctx context.Context, workspaceID, podIP, sessionID string) bool {
		if failIds != nil && failIds[sessionID] {
			return false
		}
		*deleted = append(*deleted, sessionID)
		return true
	}
	return newBoardWorkerRetainer(lister, deleter, 3, 3600)
}

// Pin 1 (the ruling's cap enforcement): the create that pushes past the
// cap deletes the OLDEST worker — and only enough to return to the cap.
func TestBoardWorkerRetention_OverCapCreatePrunesOldestFirst(t *testing.T) {
	rows := []retentionRow{
		row("w-oldest", true, false, 48*time.Hour, 47*time.Hour),
		row("w-mid", false, true, 24*time.Hour, 23*time.Hour),
		row("w-fresh", true, false, 2*time.Hour, 2*time.Hour),
		row("w-just-created", true, false, 1*time.Minute, 30*time.Second),
	}
	lister := &stubRetentionLister{rows: rows, verify: verifiedAll(rows)}
	var deleted []string
	r := newTestRetainer(lister, &deleted, nil)

	r.prune(context.Background(), "ws-1", "10.42.0.7", "w-just-created")

	// 4 rows, cap 3 → exactly one delete: the oldest worker.
	if len(deleted) != 1 || deleted[0] != "w-oldest" {
		t.Fatalf("expected exactly the oldest worker deleted, got %v", deleted)
	}
}

// Pin 2 (THE failure mode): a user session that reaches the candidate
// list through misclassification is REFUSED — the deleter never touches
// it, even when it is the oldest row.
func TestBoardWorkerRetention_MisclassifiedUserSessionIsRefused(t *testing.T) {
	rows := []retentionRow{
		// A user session sneaked into the listing: neither signal set.
		row("USER-session", false, false, 96*time.Hour, 95*time.Hour),
		row("w-old", true, false, 48*time.Hour, 47*time.Hour),
		row("w-mid", true, false, 24*time.Hour, 23*time.Hour),
		row("w-just-created", true, false, 1*time.Minute, 30*time.Second),
	}
	lister := &stubRetentionLister{rows: rows, verify: verifiedAll(rows)}
	var deleted []string
	r := newTestRetainer(lister, &deleted, nil)

	r.prune(context.Background(), "ws-1", "10.42.0.7", "w-just-created")

	for _, id := range deleted {
		if id == "USER-session" {
			t.Fatal("MISCLASSIFICATION FAILURE: the deleter deleted a user session")
		}
	}
	// The over-cap condition is still resolved with a REAL worker.
	if len(deleted) != 1 || deleted[0] != "w-old" {
		t.Fatalf("expected the oldest true worker deleted instead, got %v", deleted)
	}
}

// Pin 2b (the re-verification leg): a listed worker whose fresh
// single-row read no longer says worker (the mark flipped or vanished
// between listing and delete) is refused.
func TestBoardWorkerRetention_ReverifyRefusesStaleClassification(t *testing.T) {
	rows := []retentionRow{
		row("w-flipped", true, false, 48*time.Hour, 47*time.Hour),
		row("w-mid", true, false, 24*time.Hour, 23*time.Hour),
		row("w-just-created", true, false, 1*time.Minute, 30*time.Second),
	}
	lister := &stubRetentionLister{
		rows:   rows,
		verify: map[string]bool{"w-flipped": false, "w-mid": true, "w-just-created": true},
	}
	var deleted []string
	r := newTestRetainer(lister, &deleted, nil)

	r.prune(context.Background(), "ws-1", "10.42.0.7", "w-just-created")

	if len(deleted) != 0 {
		t.Fatalf("re-verification must refuse the flipped row; deleted %v", deleted)
	}
}

// Pin 3 (the grace floor): mid-lease workers (updated <1h ago) are
// never deleted — even when they are the oldest and the cap is
// exceeded. Bounded: the prune never chases past the floor.
func TestBoardWorkerRetention_GraceFloorProtectsFreshWorkers(t *testing.T) {
	rows := []retentionRow{
		row("w-stale", true, false, 72*time.Hour, 71*time.Hour),
		row("w-lease-1", false, true, 70*time.Hour, 10*time.Minute),
		row("w-lease-2", true, false, 69*time.Hour, 5*time.Minute),
		row("w-just-created", true, false, 1*time.Minute, 30*time.Second),
	}
	lister := &stubRetentionLister{rows: rows, verify: verifiedAll(rows)}
	var deleted []string
	r := newTestRetainer(lister, &deleted, nil)

	r.prune(context.Background(), "ws-1", "10.42.0.7", "w-just-created")

	// Only the stale row is evictable; the two mid-lease workers stay
	// even though the set is still over cap after the single delete.
	if len(deleted) != 1 || deleted[0] != "w-stale" {
		t.Fatalf("grace floor violated or wrong row: deleted %v", deleted)
	}
}

// The just-created session is never an eviction candidate — explicit
// keep guard on top of the grace floor (belt and braces).
func TestBoardWorkerRetention_NeverDeletesJustCreatedSession(t *testing.T) {
	rows := []retentionRow{
		row("w-just-created", true, false, 72*time.Hour, 71*time.Hour), // oldest AND the keep id
		row("w-a", true, false, 48*time.Hour, 47*time.Hour),
		row("w-b", true, false, 24*time.Hour, 23*time.Hour),
		row("w-c", true, false, 12*time.Hour, 11*time.Hour),
	}
	lister := &stubRetentionLister{rows: rows, verify: verifiedAll(rows)}
	var deleted []string
	r := newTestRetainer(lister, &deleted, nil)

	r.prune(context.Background(), "ws-1", "10.42.0.7", "w-just-created")

	for _, id := range deleted {
		if id == "w-just-created" {
			t.Fatal("the just-created worker was deleted")
		}
	}
	if len(deleted) != 1 || deleted[0] != "w-a" {
		t.Fatalf("expected next-oldest eviction, got %v", deleted)
	}
}

// Review r2 pin: archived workers are the user's deliberate retirement
// — they neither evict nor block the prune (a live worker goes instead).
func TestBoardWorkerRetention_ArchivedWorkerIsShielded(t *testing.T) {
	rows := []retentionRow{
		{SessionID: "w-archived", OriginWorker: true, LastActivity: ago(96 * time.Hour), UpdatedAt: ago(95 * time.Hour), Archived: true},
		row("w-live", true, false, 48*time.Hour, 47*time.Hour),
		row("w-mid", true, false, 24*time.Hour, 23*time.Hour),
		row("w-just-created", true, false, 1*time.Minute, 30*time.Second),
	}
	lister := &stubRetentionLister{rows: rows, verify: verifiedAll(rows)}
	var deleted []string
	r := newTestRetainer(lister, &deleted, nil)

	r.prune(context.Background(), "ws-1", "10.42.0.7", "w-just-created")

	for _, id := range deleted {
		if id == "w-archived" {
			t.Fatal("an ARCHIVED worker was hard-deleted — retirement outranks retention")
		}
	}
	if len(deleted) != 1 || deleted[0] != "w-live" {
		t.Fatalf("expected the oldest LIVE worker instead, got %v", deleted)
	}
}

// At or under the cap: no deletes at all (the common case).
func TestBoardWorkerRetention_AtOrUnderCapIsNoop(t *testing.T) {
	rows := []retentionRow{
		row("w-a", true, false, 48*time.Hour, 47*time.Hour),
		row("w-b", true, false, 24*time.Hour, 23*time.Hour),
	}
	lister := &stubRetentionLister{rows: rows, verify: verifiedAll(rows)}
	var deleted []string
	r := newTestRetainer(lister, &deleted, nil)

	r.prune(context.Background(), "ws-1", "10.42.0.7", "")

	if len(deleted) != 0 {
		t.Fatalf("under-cap prune deleted rows: %v", deleted)
	}
}

// A failed hard delete (pod leg refused) does not abort the prune —
// the next candidate still gets its chance; the failed one is simply
// not counted.
func TestBoardWorkerRetention_DeleteFailureContinuesToNextCandidate(t *testing.T) {
	rows := []retentionRow{
		row("w-fail", true, false, 72*time.Hour, 71*time.Hour),
		row("w-next", true, false, 48*time.Hour, 47*time.Hour),
		row("w-c", true, false, 24*time.Hour, 23*time.Hour),
		row("w-just-created", true, false, 1*time.Minute, 30*time.Second),
	}
	lister := &stubRetentionLister{rows: rows, verify: verifiedAll(rows)}
	var deleted []string
	r := newTestRetainer(lister, &deleted, map[string]bool{"w-fail": true})

	r.prune(context.Background(), "ws-1", "10.42.0.7", "w-just-created")

	if len(deleted) != 1 || deleted[0] != "w-next" {
		t.Fatalf("expected continuation past the failed delete, got %v", deleted)
	}
}

// Listing failure is logged and swallowed — retention never fails the
// spawn path (best-effort discipline).
func TestBoardWorkerRetention_ListingFailureIsSwallowed(t *testing.T) {
	lister := &failingRetentionLister{}
	var deleted []string
	r := newTestRetainer(lister, &deleted, nil)

	r.prune(context.Background(), "ws-1", "10.42.0.7", "keep") // must not panic or block
	if len(deleted) != 0 {
		t.Fatalf("listing failure must not delete anything, got %v", deleted)
	}
}

// Cap/grace defaults: non-positive config falls back to the ruling's
// 50 / 1h.
func TestBoardWorkerRetention_Defaults(t *testing.T) {
	r := newBoardWorkerRetainer(&stubRetentionLister{}, func(ctx context.Context, ws, pod, id string) bool { return true }, 0, 0)
	if r.cap != 50 {
		t.Fatalf("default cap must be 50, got %d", r.cap)
	}
	if r.grace != time.Hour {
		t.Fatalf("default grace must be 1h, got %s", r.grace)
	}
}

type failingRetentionLister struct{}

func (*failingRetentionLister) ListWorkerSessions(ctx context.Context, workspaceID string) ([]retentionRow, error) {
	return nil, context.DeadlineExceeded
}
func (*failingRetentionLister) IsWorkerSession(ctx context.Context, workspaceID, sessionID string) (bool, error) {
	return false, context.DeadlineExceeded
}
