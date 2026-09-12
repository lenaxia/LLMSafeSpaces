// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package inbox

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/go-redis/redis/v8"
)

func newTestService(t *testing.T) (*Service, *miniredis.Miniredis) {
	t.Helper()
	mr, err := miniredis.Run()
	if err != nil {
		t.Fatalf("miniredis: %v", err)
	}
	t.Cleanup(mr.Close)
	return New(redis.NewClient(&redis.Options{Addr: mr.Addr()})), mr
}

func questionRecord(id string) Record {
	return Record{
		ID:        id,
		SessionID: "ses_a",
		Kind:      KindQuestion,
		Status:    StatusPending,
		Question:  "Deploy now?",
		Header:    "Deploy",
		Options: []Option{
			{Label: "Yes", Description: "ship"},
			{Label: "No", Description: "hold"},
		},
		RecordedAt: time.Now().UTC().Truncate(time.Millisecond),
	}
}

func TestRecord_HappyPath(t *testing.T) {
	svc, _ := newTestService(t)
	ctx := context.Background()

	if err := svc.Record(ctx, "ws1", questionRecord("que_1")); err != nil {
		t.Fatalf("record: %v", err)
	}
	got, err := svc.List(ctx, "ws1", "ses_a")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(got) != 1 || got[0].ID != "que_1" {
		t.Fatalf("want 1 pending que_1, got %+v", got)
	}
	if got[0].Question != "Deploy now?" || len(got[0].Options) != 2 || got[0].Options[0].Label != "Yes" {
		t.Fatalf("record fidelity lost: %+v", got[0])
	}
	if got[0].Status != StatusPending {
		t.Fatalf("status: want pending, got %s", got[0].Status)
	}
}

func TestRecord_SeparateSessionsDoNotLeak(t *testing.T) {
	svc, _ := newTestService(t)
	ctx := context.Background()
	_ = svc.Record(ctx, "ws1", questionRecord("que_1"))
	rec2 := questionRecord("que_2")
	rec2.SessionID = "ses_b"
	_ = svc.Record(ctx, "ws1", rec2)

	a, _ := svc.List(ctx, "ws1", "ses_a")
	b, _ := svc.List(ctx, "ws1", "ses_b")
	if len(a) != 1 || a[0].ID != "que_1" {
		t.Fatalf("ses_a: %+v", a)
	}
	if len(b) != 1 || b[0].ID != "que_2" {
		t.Fatalf("ses_b: %+v", b)
	}
}

func TestRecord_UpsertPendingRefreshes(t *testing.T) {
	svc, _ := newTestService(t)
	ctx := context.Background()

	rec := questionRecord("que_1")
	if err := svc.Record(ctx, "ws1", rec); err != nil {
		t.Fatalf("record 1: %v", err)
	}
	rec.Question = "Deploy now, really?"
	rec.RecordedAt = rec.RecordedAt.Add(time.Second)
	if err := svc.Record(ctx, "ws1", rec); err != nil {
		t.Fatalf("record 2: %v", err)
	}
	got, _ := svc.List(ctx, "ws1", "ses_a")
	if len(got) != 1 {
		t.Fatalf("want 1 record after upsert, got %d", len(got))
	}
	if got[0].Question != "Deploy now, really?" {
		t.Fatalf("upsert did not refresh: %+v", got[0])
	}
}

func TestRecord_RefreshPreservesOriginalTimestamp(t *testing.T) {
	// Snapshot flights re-record every live ask; a refresh must not
	// reset the eviction clock (r1 finding 5: long-lived live asks would
	// become eviction-immune, degrading FIFO to
	// least-recently-snapshotted). The FIRST-recorded stamp wins.
	svc, _ := newTestService(t)
	ctx := context.Background()
	base := time.Now().UTC().Truncate(time.Millisecond).Add(-time.Hour)

	a := questionRecord("que_a")
	a.RecordedAt = base
	_ = svc.Record(ctx, "ws1", a)
	b := questionRecord("que_b")
	b.RecordedAt = base.Add(time.Minute)
	_ = svc.Record(ctx, "ws1", b)

	// Refresh A much later — its ORIGINAL stamp must survive.
	aLate := questionRecord("que_a")
	aLate.RecordedAt = base.Add(2 * time.Hour)
	if err := svc.Record(ctx, "ws1", aLate); err != nil {
		t.Fatalf("refresh: %v", err)
	}
	got, _ := svc.List(ctx, "ws1", "ses_a")
	if len(got) != 2 {
		t.Fatalf("want 2 records, got %d", len(got))
	}
	if got[0].ID != "que_a" || got[0].RecordedAt.After(base.Add(time.Second)) {
		t.Fatalf("refresh reset the stamp: %+v", got[0])
	}
}

func TestRecord_TerminalImmutable(t *testing.T) {
	svc, _ := newTestService(t)
	ctx := context.Background()
	_ = svc.Record(ctx, "ws1", questionRecord("que_1"))

	if err := svc.Resolve(ctx, "ws1", "ses_a", "que_1", StatusAnswered); err != nil {
		t.Fatalf("resolve: %v", err)
	}

	rec := questionRecord("que_1")
	rec.Question = "zombie"
	if err := svc.Record(ctx, "ws1", rec); err != nil {
		t.Fatalf("record over terminal: %v", err)
	}

	got, _ := svc.List(ctx, "ws1", "ses_a")
	if len(got) != 0 {
		t.Fatalf("terminal record re-presented or mutated: %+v", got)
	}
}

func TestResolve_HappyPaths(t *testing.T) {
	svc, _ := newTestService(t)
	ctx := context.Background()
	_ = svc.Record(ctx, "ws1", questionRecord("que_1"))
	_ = svc.Record(ctx, "ws1", questionRecord("que_2"))

	if err := svc.Resolve(ctx, "ws1", "ses_a", "que_1", StatusAnswered); err != nil {
		t.Fatalf("resolve answered: %v", err)
	}
	if err := svc.Resolve(ctx, "ws1", "ses_a", "que_2", StatusDismissed); err != nil {
		t.Fatalf("resolve dismissed: %v", err)
	}
	got, _ := svc.List(ctx, "ws1", "ses_a")
	if len(got) != 0 {
		t.Fatalf("want empty pending list, got %+v", got)
	}
}

func TestResolve_UnknownRecordIsNotError(t *testing.T) {
	svc, _ := newTestService(t)
	ctx := context.Background()
	if err := svc.Resolve(ctx, "ws1", "ses_a", "que_nope", StatusAnswered); err != nil {
		t.Fatalf("resolve of unknown ask must be a no-op success (droppable events), got %v", err)
	}
}

func TestResolve_AlreadyTerminalIdempotent(t *testing.T) {
	svc, _ := newTestService(t)
	ctx := context.Background()
	_ = svc.Record(ctx, "ws1", questionRecord("que_1"))
	if err := svc.Resolve(ctx, "ws1", "ses_a", "que_1", StatusAnswered); err != nil {
		t.Fatalf("first resolve: %v", err)
	}
	if err := svc.Resolve(ctx, "ws1", "ses_a", "que_1", StatusDismissed); err != nil {
		t.Fatalf("second resolve must not error: %v", err)
	}
}

func TestResolve_InvalidStatusRejected(t *testing.T) {
	svc, _ := newTestService(t)
	ctx := context.Background()
	_ = svc.Record(ctx, "ws1", questionRecord("que_1"))
	if err := svc.Resolve(ctx, "ws1", "ses_a", "que_1", "bogus"); err == nil {
		t.Fatal("invalid status must be rejected")
	}
	if err := svc.Resolve(ctx, "ws1", "ses_a", "que_1", StatusPending); err == nil {
		t.Fatal("resolving to pending must be rejected (terminal statuses only)")
	}
}

func TestRecord_CapEvictsOldestPending(t *testing.T) {
	svc, _ := newTestService(t)
	ctx := context.Background()
	base := time.Now().UTC().Truncate(time.Millisecond)
	for i := 0; i < Cap; i++ {
		rec := questionRecord("que_" + string(rune('a'+i)))
		rec.RecordedAt = base.Add(time.Duration(i) * time.Second)
		if err := svc.Record(ctx, "ws1", rec); err != nil {
			t.Fatalf("record %d: %v", i, err)
		}
	}
	// One more — the oldest (que_a) must be evicted to make room.
	rec := questionRecord("que_z")
	rec.RecordedAt = base.Add(time.Duration(Cap) * time.Second)
	if err := svc.Record(ctx, "ws1", rec); err != nil {
		t.Fatalf("record over cap: %v", err)
	}
	got, _ := svc.List(ctx, "ws1", "ses_a")
	if len(got) != Cap {
		t.Fatalf("want %d records after eviction, got %d", Cap, len(got))
	}
	for _, r := range got {
		if r.ID == "que_a" {
			t.Fatal("oldest pending record was not evicted")
		}
	}
	if got[0].RecordedAt.After(got[len(got)-1].RecordedAt) {
		t.Fatal("list must be ordered oldest-first by RecordedAt")
	}
}

func TestRecord_CapCountsPendingOnly(t *testing.T) {
	svc, _ := newTestService(t)
	ctx := context.Background()
	base := time.Now().UTC().Truncate(time.Millisecond)
	for i := 0; i < Cap; i++ {
		rec := questionRecord("que_" + string(rune('a'+i)))
		rec.RecordedAt = base.Add(time.Duration(i) * time.Second)
		_ = svc.Record(ctx, "ws1", rec)
	}
	// Answer the oldest; a fresh record must NOT evict anything (pending < cap).
	if err := svc.Resolve(ctx, "ws1", "ses_a", "que_a", StatusAnswered); err != nil {
		t.Fatalf("resolve: %v", err)
	}
	rec := questionRecord("que_z")
	rec.RecordedAt = base.Add(time.Duration(Cap+1) * time.Second)
	if err := svc.Record(ctx, "ws1", rec); err != nil {
		t.Fatalf("record with terminal headroom: %v", err)
	}
	got, _ := svc.List(ctx, "ws1", "ses_a")
	if len(got) != Cap {
		t.Fatalf("want %d pending, got %d", Cap, len(got))
	}
	found := 0
	for _, r := range got {
		if r.ID == "que_b" || r.ID == "que_z" {
			found++
		}
	}
	if found != 2 {
		t.Fatal("cap must count pending records only; que_b evicted wrongly")
	}
}

func TestList_WorkspaceSessionScoping(t *testing.T) {
	svc, _ := newTestService(t)
	ctx := context.Background()
	_ = svc.Record(ctx, "ws1", questionRecord("que_1"))
	rec := questionRecord("que_2")
	rec.SessionID = "ses_b"
	_ = svc.Record(ctx, "ws1", rec)
	rec3 := questionRecord("que_3")
	_ = svc.Record(ctx, "ws2", rec3)

	w1a, _ := svc.List(ctx, "ws1", "ses_a")
	if len(w1a) != 1 || w1a[0].ID != "que_1" {
		t.Fatalf("ws1/ses_a: %+v", w1a)
	}
	empty, err := svc.List(ctx, "nope", "ses_a")
	if err != nil || len(empty) != 0 {
		t.Fatalf("unknown workspace: %+v err=%v", empty, err)
	}
}

func TestRecord_TTLSetsExpiry(t *testing.T) {
	svc, mr := newTestService(t)
	ctx := context.Background()
	if err := svc.Record(ctx, "ws1", questionRecord("que_1")); err != nil {
		t.Fatalf("record: %v", err)
	}
	ttl := mr.TTL(key("ws1", "ses_a"))
	if ttl <= 0 || ttl > TTL {
		t.Fatalf("want TTL in (0,%s], got %s", TTL, ttl)
	}
}

func TestRecord_ReconnectSurvival(t *testing.T) {
	svc, mr := newTestService(t)
	ctx := context.Background()
	_ = svc.Record(ctx, "ws1", questionRecord("que_1"))

	// A Redis reconnect (client re-dial) must not lose records — the store
	// is stateless over the client (same construction as the outbox).
	svc2 := New(redis.NewClient(&redis.Options{Addr: mr.Addr()}))
	got, err := svc2.List(ctx, "ws1", "ses_a")
	if err != nil || len(got) != 1 {
		t.Fatalf("after reconnect: %+v err=%v", got, err)
	}
}

func TestNew_NilClientReturnsNil(t *testing.T) {
	if New(nil) != nil {
		t.Fatal("New(nil) must return nil (outbox convention)")
	}
}

func TestRecord_ContextCancellationPropagates(t *testing.T) {
	svc, _ := newTestService(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := svc.Record(ctx, "ws1", questionRecord("que_1")); !errors.Is(err, context.Canceled) {
		t.Fatalf("want context.Canceled, got %v", err)
	}
	if _, err := svc.List(ctx, "ws1", "ses_a"); !errors.Is(err, context.Canceled) {
		t.Fatalf("list: want context.Canceled, got %v", err)
	}
	if err := svc.Resolve(ctx, "ws1", "ses_a", "que_1", StatusAnswered); !errors.Is(err, context.Canceled) {
		t.Fatalf("resolve: want context.Canceled, got %v", err)
	}
}

func TestList_OrderedOldestFirst(t *testing.T) {
	svc, _ := newTestService(t)
	ctx := context.Background()
	base := time.Now().UTC().Truncate(time.Millisecond)
	for i, id := range []string{"que_c", "que_a", "que_b"} {
		rec := questionRecord(id)
		rec.RecordedAt = base.Add(time.Duration(i) * time.Second)
		_ = svc.Record(ctx, "ws1", rec)
	}
	got, _ := svc.List(ctx, "ws1", "ses_a")
	if got[0].ID != "que_c" || got[1].ID != "que_a" || got[2].ID != "que_b" {
		t.Fatalf("insertion/recorded order must hold: %+v", got)
	}
}

func TestListWorkspace_SpansSessionsOldestFirst(t *testing.T) {
	svc, _ := newTestService(t)
	ctx := context.Background()
	base := time.Now().UTC().Truncate(time.Millisecond)
	a := questionRecord("que_1")
	a.RecordedAt = base
	b := questionRecord("que_2")
	b.SessionID = "ses_b"
	b.RecordedAt = base.Add(time.Second)
	c := questionRecord("que_3")
	c.SessionID = "ses_c"
	c.RecordedAt = base.Add(2 * time.Second)
	_ = svc.Record(ctx, "ws1", a)
	_ = svc.Record(ctx, "ws1", b)
	_ = svc.Record(ctx, "ws1", c)

	got, err := svc.ListWorkspace(ctx, "ws1")
	if err != nil {
		t.Fatalf("list workspace: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("want 3 records across sessions, got %d", len(got))
	}
	if got[0].ID != "que_1" || got[1].ID != "que_2" || got[2].ID != "que_3" {
		t.Fatalf("workspace list must be globally oldest-first: %+v", got)
	}
}

func TestListWorkspace_FiltersTerminalAndScopesWorkspace(t *testing.T) {
	svc, _ := newTestService(t)
	ctx := context.Background()
	_ = svc.Record(ctx, "ws1", questionRecord("que_1"))
	_ = svc.Record(ctx, "ws2", questionRecord("que_9"))
	_ = svc.Resolve(ctx, "ws1", "ses_a", "que_1", StatusAnswered)

	got, err := svc.ListWorkspace(ctx, "ws1")
	if err != nil {
		t.Fatalf("list workspace: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("terminal records must not appear in workspace list: %+v", got)
	}
	other, _ := svc.ListWorkspace(ctx, "ws2")
	if len(other) != 1 || other[0].ID != "que_9" {
		t.Fatalf("workspace scoping broken: %+v", other)
	}
}

func TestLookupPending_FoundAcrossSessions(t *testing.T) {
	svc, _ := newTestService(t)
	ctx := context.Background()
	rec := questionRecord("que_1")
	rec.SessionID = "ses_deep"
	_ = svc.Record(ctx, "ws1", rec)

	got, ok, err := svc.LookupPending(ctx, "ws1", "que_1")
	if err != nil || !ok {
		t.Fatalf("lookup: ok=%v err=%v", ok, err)
	}
	if got.SessionID != "ses_deep" || got.Question != "Deploy now?" {
		t.Fatalf("lookup fidelity: %+v", got)
	}
}

func TestLookupPending_TerminalAndMissing(t *testing.T) {
	svc, _ := newTestService(t)
	ctx := context.Background()
	_ = svc.Record(ctx, "ws1", questionRecord("que_1"))
	_ = svc.Resolve(ctx, "ws1", "ses_a", "que_1", StatusDismissed)

	if _, ok, err := svc.LookupPending(ctx, "ws1", "que_1"); ok || err != nil {
		t.Fatalf("terminal record must not be pending: ok=%v err=%v", ok, err)
	}
	if _, ok, err := svc.LookupPending(ctx, "ws1", "que_none"); ok || err != nil {
		t.Fatalf("missing record: ok=%v err=%v", ok, err)
	}
}
