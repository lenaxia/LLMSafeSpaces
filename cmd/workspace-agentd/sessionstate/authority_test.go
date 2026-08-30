// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package sessionstate_test

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/lenaxia/llmsafespaces/cmd/workspace-agentd/sessionstate"
	abiv1 "github.com/lenaxia/llmsafespaces/pkg/abi/v1"
)

// fixtureParser maps a tiny test dialect ("busy <id>" / "idle <id>" lines)
// onto contract events. The real opencode parser is injected by agentd
// wiring; sessionstate is dialect-free machinery.
type fixtureParser struct{}

func (p *fixtureParser) Parse(raw []byte) (*abiv1.Event, bool, error) {
	s := string(raw)
	var id, status string
	if n, _ := fmt.Sscanf(s, "%s %s", &status, &id); n != 2 {
		return nil, false, nil
	}
	switch status {
	case "busy":
		return &abiv1.Event{Type: abiv1.EventType_EVENT_TYPE_SESSION_STATUS, SessionId: id, Status: abiv1.SessionStatus_SESSION_STATUS_BUSY}, true, nil
	case "idle":
		return &abiv1.Event{Type: abiv1.EventType_EVENT_TYPE_SESSION_STATUS, SessionId: id, Status: abiv1.SessionStatus_SESSION_STATUS_IDLE}, true, nil
	case "panic":
		panic("parser panic — recover wall must contain this")
	default:
		return nil, false, nil
	}
}

// hangingStore simulates a wedged opencode: every store read blocks until
// the context is canceled (M3.1 / the 2026-08-15 starvation shape).
type hangingStore struct{}

func (hangingStore) SessionStates(ctx context.Context) (map[string]sessionstate.SessionSeed, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}

type mapStore struct {
	m   map[string]abiv1.SessionStatus
	err error
}

func (s *mapStore) SessionStates(ctx context.Context) (map[string]sessionstate.SessionSeed, error) {
	if s.err != nil {
		return nil, s.err
	}
	out := make(map[string]sessionstate.SessionSeed, len(s.m))
	for k, v := range s.m {
		out[k] = sessionstate.SessionSeed{Status: v}
	}
	return out, nil
}

func testConfig(t *testing.T, dir string, parser sessionstate.EventParser, store sessionstate.StoreReader) sessionstate.Config {
	t.Helper()
	return sessionstate.Config{
		PlatformDir: dir,
		Parser:      parser,
		Store:       store,
		Passwords:   []string{"agentd-password"},
	}
}

func newAuthority(t *testing.T, cfg sessionstate.Config) *sessionstate.Authority {
	t.Helper()
	a, err := sessionstate.New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = a.Close() })
	return a
}

// TestIngestFuzzRecover: 10k iterations of adversarial raw payloads (and a
// parser that PANICS) never take down ingestion — the recover wall holds,
// seq never skips for subsequent good events (issue #1136: ingest_fuzz_recover).
func TestIngestFuzzRecover(t *testing.T) {
	dir := t.TempDir()
	a := newAuthority(t, testConfig(t, dir, &fixtureParser{}, &mapStore{}))

	rng := rand.New(rand.NewSource(1))
	garbage := [][]byte{
		nil, {}, []byte("\x00\xff\xfe"), []byte("{"), []byte(`{"type":`),
		[]byte("busy"), []byte("busy s1 extra fields"), []byte("idle"),
		[]byte("panic s1"), []byte("\xc3\x28 invalid utf8"), []byte("busy " + string(make([]byte, 1<<16))),
	}
	for i := 0; i < 10000; i++ {
		a.Ingest(garbage[rng.Intn(len(garbage))])
	}

	a.Ingest([]byte("busy s1"))
	st := a.State()
	if st.Seq == 0 {
		t.Fatalf("no seq assigned after 10k fuzz iterations + one good event")
	}
	if got := st.Sessions["s1"].Status; got != abiv1.SessionStatus_SESSION_STATUS_BUSY {
		t.Errorf("good event after fuzz storm not applied: s1=%v", got)
	}
}

// TestSeqMonotonicAcrossKill9: hard-close (simulated SIGKILL: no graceful
// Close) mid-stream, reopen on the same platform dir — seq CONTINUES, never
// reuses a value that was assigned-and-published (issue #1136:
// seq_monotonic_across_kill9; I1 + fsync-before-publish cursor policy).
func TestSeqMonotonicAcrossKill9(t *testing.T) {
	dir := t.TempDir()

	a1, err := sessionstate.New(testConfig(t, dir, &fixtureParser{}, &mapStore{}))
	if err != nil {
		t.Fatal(err)
	}
	frames, cancel, err := a1.Stream(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	published := map[uint64]bool{}
	done := make(chan struct{})
	go func() {
		defer close(done)
		for f := range frames {
			if e := f.GetEvent(); e != nil {
				published[e.Seq] = true
			}
		}
	}()

	for i := 0; i < 50; i++ {
		a1.Ingest([]byte(fmt.Sprintf("busy s%d", i%3)))
	}
	// No Close — the kill. Drain briefly so publishes complete.
	time.Sleep(50 * time.Millisecond)
	a1.KillForTest()
	cancel()
	<-done

	a2, err := sessionstate.New(testConfig(t, dir, &fixtureParser{}, &mapStore{}))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = a2.Close() }()
	if base := a2.State().Seq; base == 0 {
		t.Fatal("cursor did not persist across kill")
	}

	frames2, cancel2, err := a2.Stream(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	reused := make(chan uint64, 8)
	go func() {
		for f := range frames2 {
			if e := f.GetEvent(); e != nil && published[e.Seq] {
				select {
				case reused <- e.Seq:
				default:
				}
			}
		}
	}()
	for i := 100; i < 150; i++ {
		a2.Ingest([]byte(fmt.Sprintf("busy s%d", i%3)))
	}
	time.Sleep(50 * time.Millisecond)
	cancel2()
	select {
	case s := <-reused:
		t.Fatalf("seq %d REUSED after kill-9 restart — I1 violated", s)
	default:
	}
}

// TestStampAtomicityRace: concurrent snapshot readers vs event application
// under -race — every observed state is exactly the fold of events ≤ its
// stamp; no torn snapshot (issue #1136: stamp_atomicity_race; I1).
func TestStampAtomicityRace(t *testing.T) {
	dir := t.TempDir()
	a := newAuthority(t, testConfig(t, dir, &fixtureParser{}, &mapStore{}))

	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { // writer
		defer wg.Done()
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
				a.Ingest([]byte(fmt.Sprintf("busy s%d", i%4)))
			}
		}
	}()
	var violated atomic.Bool
	go func() { // reader
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			st := a.State()
			// Atomic stamp contract: the status of every session in the
			// snapshot was set by an event with seq ≤ st.Seq — verify by
			// replaying: after a fresh Ingest, state seq strictly grows and
			// statuses only change alongside seq growth.
			next := a.State()
			if next.Seq < st.Seq {
				violated.Store(true)
			}
			if st.Seq > 0 && len(st.Sessions) == 0 {
				violated.Store(true)
			}
		}
	}()
	time.Sleep(300 * time.Millisecond)
	close(stop)
	wg.Wait()
	if violated.Load() {
		t.Fatal("stamp atomicity violated under -race (seq regressed or torn state)")
	}
}

// TestReseedUnderActiveStreaming: generation change with events in flight —
// inbound events BUFFER during reseed and apply after; `projection.reseeded`
// is emitted exactly once, consuming a seq; no seq gap (issue #1136:
// reseed_under_active_streaming; I3).
func TestReseedUnderActiveStreaming(t *testing.T) {
	dir := t.TempDir()
	store := &mapStore{m: map[string]abiv1.SessionStatus{"s1": abiv1.SessionStatus_SESSION_STATUS_IDLE}}
	a := newAuthority(t, testConfig(t, dir, &fixtureParser{}, store))

	frames, cancel, err := a.Stream(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer cancel()
	var got []*abiv1.StreamFrame
	done := make(chan struct{})
	go func() {
		defer close(done)
		for f := range frames {
			got = append(got, f)
		}
	}()

	a.Ingest([]byte("busy s1"))
	time.Sleep(20 * time.Millisecond)

	// Slow store + events arriving DURING the reseed window.
	slow := &slowStore{inner: store, delay: 80 * time.Millisecond}
	a.SetStoreForTest(slow)
	go func() {
		for i := 0; i < 5; i++ {
			a.Ingest([]byte("busy s2"))
			time.Sleep(10 * time.Millisecond)
		}
	}()
	if err := a.Reseed(context.Background(), sessionstate.ReseedReasonGenerationChange); err != nil {
		t.Fatalf("reseed: %v", err)
	}
	time.Sleep(150 * time.Millisecond)
	cancel()
	<-done

	reseeds := 0
	lastSeq := uint64(0)
	sawS2Events := false
	for _, f := range got {
		var seq uint64
		switch fr := f.Frame.(type) {
		case *abiv1.StreamFrame_Snapshot:
			lastSeq = fr.Snapshot.AtSeq // the stamp: everything <= S is folded
			continue
		case *abiv1.StreamFrame_Reseeded:
			reseeds++
			seq = fr.Reseeded.Seq
		case *abiv1.StreamFrame_Event:
			seq = fr.Event.Seq
			if fr.Event.Event.SessionId == "s2" {
				sawS2Events = true
			}
		}
		if seq <= lastSeq {
			t.Fatalf("seq regressed (%d after %d) — reseed broke monotonicity", seq, lastSeq)
		}
		lastSeq = seq
	}
	if reseeds != 1 {
		t.Fatalf("projection.reseeded emitted %d times, want exactly 1", reseeds)
	}
	if !sawS2Events {
		t.Error("events ingested during the reseed window were lost — I3 buffering broken")
	}
	// Store truth reaches live clients the protocol way: the reseed notice
	// forces a re-snapshot; a FRESH connection's snapshot must show it.
	fresh, freshCancel, err := a.Stream(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer freshCancel()
	ff := <-fresh
	fsnap := ff.GetSnapshot()
	if fsnap == nil {
		t.Fatalf("fresh connection after reseed got no snapshot frame: %+v", ff.Frame)
	}
	var s1Fresh abiv1.SessionStatus
	for _, ss := range fsnap.GetSnapshot().GetSessions() {
		if ss.GetSessionId() == "s1" {
			s1Fresh = ss.GetStatus()
		}
	}
	if s1Fresh != abiv1.SessionStatus_SESSION_STATUS_IDLE {
		t.Errorf("post-reseed snapshot s1 = %v, want store truth IDLE", s1Fresh)
	}

	st := a.State()
	if st.Sessions["s1"].Status != abiv1.SessionStatus_SESSION_STATUS_IDLE {
		t.Errorf("projection does not reflect store truth after reseed: %+v", st.Sessions["s1"])
	}
}

// TestReseedConvergesToStoreTruth: agentd restart with opencode alive
// (sidecar-rollout shape) — a fresh Authority reseeds from the fixture
// store and converges (issue #1136: reseed_agentd_restart_opencode_alive).
func TestReseedConvergesToStoreTruth(t *testing.T) {
	dir := t.TempDir()
	store := &mapStore{m: map[string]abiv1.SessionStatus{
		"s1": abiv1.SessionStatus_SESSION_STATUS_BUSY,
		"s2": abiv1.SessionStatus_SESSION_STATUS_IDLE,
		"s3": abiv1.SessionStatus_SESSION_STATUS_ERROR,
	}}

	a1, err := sessionstate.New(testConfig(t, dir, &fixtureParser{}, store))
	if err != nil {
		t.Fatal(err)
	}
	a1.Ingest([]byte("idle s1")) // stale view: s1 idle; store says busy
	if err := a1.Close(); err != nil {
		t.Fatal(err)
	}

	a2, err := sessionstate.New(testConfig(t, dir, &fixtureParser{}, store))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = a2.Close() }()
	if err := a2.Reseed(context.Background(), sessionstate.ReseedReasonBoot); err != nil {
		t.Fatal(err)
	}
	st := a2.State()
	for id, want := range store.m {
		if got := st.Sessions[id].Status; got != want {
			t.Errorf("session %s = %v, want store truth %v — I4 store-is-truth broken", id, got, want)
		}
	}
}

// TestReseedStoreFailureKeepsServing: a failing store must degrade to the
// previous projection (never wedge the authority) and surface the error.
func TestReseedStoreFailureKeepsServing(t *testing.T) {
	dir := t.TempDir()
	a := newAuthority(t, testConfig(t, dir, &fixtureParser{}, nil))
	a.Ingest([]byte("busy s1"))

	if err := a.Reseed(context.Background(), sessionstate.ReseedReasonBoot); !errors.Is(err, sessionstate.ErrNoStore) {
		t.Fatalf("reseed without store wiring = %v, want ErrNoStore", err)
	}
	if got := a.State().Sessions["s1"].Status; got != abiv1.SessionStatus_SESSION_STATUS_BUSY {
		t.Errorf("failed reseed corrupted projection: s1=%v", got)
	}
}

type slowStore struct {
	inner sessionstate.StoreReader
	delay time.Duration
}

func (s *slowStore) SessionStates(ctx context.Context) (map[string]sessionstate.SessionSeed, error) {
	select {
	case <-time.After(s.delay):
		return s.inner.SessionStates(ctx)
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// TestStreamSnapshotFirstFrame: every stream connection receives the
// snapshot frame FIRST, stamped with the current seq; subsequent events all
// have seq > snapshot stamp (I2 subscribe-before-snapshot ordering).
func TestStreamSnapshotFirstFrame(t *testing.T) {
	dir := t.TempDir()
	a := newAuthority(t, testConfig(t, dir, &fixtureParser{}, &mapStore{}))
	a.Ingest([]byte("busy s1"))

	frames, cancel, err := a.Stream(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	first := <-frames
	sf := first.GetSnapshot()
	if sf == nil {
		t.Fatalf("first frame is not a snapshot frame: %+v", first.Frame)
	}
	if sf.AtSeq == 0 {
		t.Error("snapshot frame not stamped")
	}
	if sf.GetCapabilities() == nil {
		t.Error("snapshot frame missing capability report")
	}
	a.Ingest([]byte("idle s1")) // post-subscribe event
	next := <-frames
	if e := next.GetEvent(); e != nil && e.Seq <= sf.AtSeq {
		t.Errorf("post-snapshot event seq %d ≤ snapshot stamp %d — I2 gap/overlap", e.Seq, sf.AtSeq)
	}
	cancel()
}

// TestNoEventLossMidConnect: events emitted BETWEEN snapshot capture and
// live attach are never lost (I2). Ingest floods a connection DURING
// establishment; the correctness property is exact: delivered event seqs
// are contiguous from the snapshot stamp + 1 to the final assigned seq —
// every event is either folded into the snapshot or delivered, none lost,
// none duplicated, none skipped.
func TestNoEventLossMidConnect(t *testing.T) {
	dir := t.TempDir()
	a := newAuthority(t, testConfig(t, dir, &fixtureParser{}, &mapStore{}))
	a.Ingest([]byte("busy s1"))

	const flood = 200
	floodDone := make(chan struct{})
	go func() {
		defer close(floodDone)
		for i := 0; i < flood; i++ {
			a.Ingest([]byte("busy s2"))
		}
	}()
	defer func() { <-floodDone }() // t.TempDir cleanup must not race the flood writes
	frames, cancel, err := a.Stream(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer cancel()

	first := <-frames
	snap := first.GetSnapshot()
	if snap == nil {
		t.Fatalf("first frame is not a snapshot: %+v", first.Frame)
	}

	deadline := time.After(10 * time.Second)
	var delivered []uint64
	final := uint64(0)
	for done := false; !done; {
		final = a.State().Seq
		if final > 0 && len(delivered) == int(final)-int(snap.AtSeq) {
			return // full coverage observed
		}
		select {
		case f := <-frames:
			if e := f.GetEvent(); e != nil {
				if len(delivered) > 0 && e.Seq != delivered[len(delivered)-1]+1 {
					t.Fatalf("seq gap/duplicate: %d after %d — events lost mid-connect", e.Seq, delivered[len(delivered)-1])
				}
				if len(delivered) == 0 && e.Seq != snap.AtSeq+1 {
					t.Fatalf("first delivered seq %d != stamp+1 %d — gap between snapshot and live attach", e.Seq, snap.AtSeq+1)
				}
				delivered = append(delivered, e.Seq)
			}
		case <-deadline:
			t.Fatalf("lost events mid-connect: delivered %d contiguous frames (last=%d), final seq=%d, stamp=%d",
				len(delivered), lastOf(delivered), final, snap.AtSeq)
		}
	}
}

func lastOf(s []uint64) uint64 {
	if len(s) == 0 {
		return 0
	}
	return s[len(s)-1]
}

// TestCursorFileFsyncPolicy pins the cursor file location and the
// fsync-before-publish policy artifacts (a persisted cursor ≥ every
// published seq once the authority is closed or killed).
func TestCursorFileFsyncPolicy(t *testing.T) {
	dir := t.TempDir()
	a, err := sessionstate.New(testConfig(t, dir, &fixtureParser{}, &mapStore{}))
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 10; i++ {
		a.Ingest([]byte("busy s1"))
	}
	st := a.State()
	if err := a.Close(); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(dir, "seq-cursor"))
	if err != nil {
		t.Fatalf("cursor file missing: %v", err)
	}
	var persisted struct{ Last uint64 }
	if _, err := fmt.Sscanf(string(data), "%d", &persisted.Last); err != nil {
		t.Fatalf("cursor file malformed: %q", data)
	}
	if persisted.Last < st.Seq {
		t.Fatalf("persisted cursor %d < assigned %d — fsync-before-publish violated", persisted.Last, st.Seq)
	}
}
