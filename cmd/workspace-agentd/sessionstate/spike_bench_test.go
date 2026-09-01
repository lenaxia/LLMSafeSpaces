// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package sessionstate

import (
	"context"
	"fmt"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	abiv1 "github.com/lenaxia/llmsafespaces/pkg/abi/v1"
)

// S1 spike harnesses (US-69.6, design 0055 open items). These benchmarks
// produce the numbers the recorded decisions cite; run with:
//
//	go test -bench . -benchtime 50x -run xxx ./cmd/workspace-agentd/sessionstate/
//
// Cluster-bound numbers (gVisor/Longhorn/EFS fsync, pod-level p95) run the
// same harnesses on the staged pool — findings land in design 0055 §Open
// items with both environments' data.

// seedStoreN builds a store of n sessions, each with the given pending
// inputs — the pod-wide projection shape at scale.
type seedStoreN struct {
	seeds map[string]SessionSeed
}

func (s seedStoreN) SessionStates(ctx context.Context) (map[string]SessionSeed, error) {
	out := make(map[string]SessionSeed, len(s.seeds))
	for k, v := range s.seeds {
		out[k] = v
	}
	return out, nil
}

func benchAuthority(tb testing.TB, sessions int, partsPer int, pendingPer int) *Authority {
	tb.Helper()
	seeds := make(map[string]SessionSeed, sessions)
	for i := 0; i < sessions; i++ {
		seed := SessionSeed{Status: abiv1.SessionStatus_SESSION_STATUS_BUSY}
		for p := 0; p < pendingPer; p++ {
			seed.PendingInputs = append(seed.PendingInputs, &abiv1.InputRequest{
				Id: fmt.Sprintf("q-%d-%d", i, p), SessionId: fmt.Sprintf("s-%d", i),
				Kind: abiv1.InputKind_INPUT_KIND_QUESTION, Question: "Proceed?",
			})
		}
		seeds[fmt.Sprintf("s-%d", i)] = seed
	}
	dir := tb.TempDir()
	a, err := New(Config{
		PlatformDir: dir,
		Parser:      nopParser{},
		Store:       seedStoreN{seeds: seeds},
		Passwords:   []string{"pw"},
		FastCursor:  true,
	})
	if err != nil {
		tb.Fatal(err)
	}
	tb.Cleanup(func() { _ = a.Close() })
	if err := a.Reseed(context.Background(), ReseedReasonBoot); err != nil {
		tb.Fatal(err)
	}
	for i := 0; i < sessions; i++ {
		for p := 0; p < partsPer; p++ {
			a.IngestForTest(&abiv1.Event{
				Type: abiv1.EventType_EVENT_TYPE_PART_START, SessionId: fmt.Sprintf("s-%d", i),
				PartId: fmt.Sprintf("p-%d-%d", i, p),
				Part: &abiv1.Part{Id: fmt.Sprintf("p-%d-%d", i, p), Type: abiv1.PartType_PART_TYPE_TEXT,
					Payload: &abiv1.Part_Text{Text: "in-flight partial output for the snapshot size spike"}},
			})
		}
	}
	return a
}

type nopParser struct{}

func (nopParser) Parse(raw []byte) (*abiv1.Event, bool, error) { return nil, false, nil }

// BenchmarkSnapshotSize100/500 (US-69.6 spike 4): pod-wide snapshot frame
// size and latency at 100/500 sessions with in-flight parts and pending
// inputs. Decision input: cap-or-paginate.
func benchmarkSnapshotAt(b *testing.B, sessions int) {
	a := benchAuthority(b, sessions, 4, 2)
	var lastBytes int
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		frames, cancel, err := a.Stream(context.Background())
		if err != nil {
			b.Fatal(err)
		}
		f := <-frames
		snap := f.GetSnapshot()
		lastBytes = proto.Size(snap)
		cancel()
	}
	b.ReportMetric(float64(lastBytes), "frame-bytes")
}

func BenchmarkSnapshotSize100(b *testing.B) { benchmarkSnapshotAt(b, 100) }
func BenchmarkSnapshotSize500(b *testing.B) { benchmarkSnapshotAt(b, 500) }

// BenchmarkResumeBudget (US-69.6 spike 3): authority reopen (durable cursor
// load) + boot reseed against a large store — the in-process share of the
// ~22s resume budget. The V2 store full-list cost on a ~20k-message session
// is measured on the pool with the same harness shape (StoreSessionStates
// replaced by the real reader over a populated opencode store).
func BenchmarkResumeBudget(b *testing.B) {
	seeds := make(map[string]SessionSeed, 50)
	for i := 0; i < 50; i++ {
		seeds[fmt.Sprintf("s-%d", i)] = SessionSeed{Status: abiv1.SessionStatus_SESSION_STATUS_IDLE}
	}
	dir := b.TempDir()
	// Simulate the pre-suspend cursor position (a few hundred events).
	warm, err := New(Config{PlatformDir: dir, Parser: nopParser{}, Store: seedStoreN{seeds: seeds}, Passwords: []string{"pw"}, FastCursor: true})
	if err != nil {
		b.Fatal(err)
	}
	for i := 0; i < 500; i++ {
		warm.IngestForTest(&abiv1.Event{Type: abiv1.EventType_EVENT_TYPE_SESSION_STATUS,
			SessionId: fmt.Sprintf("s-%d", i%50), Status: abiv1.SessionStatus_SESSION_STATUS_BUSY})
	}
	if err := warm.Close(); err != nil {
		b.Fatal(err)
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		a, err := New(Config{PlatformDir: dir, Parser: nopParser{}, Store: seedStoreN{seeds: seeds}, Passwords: []string{"pw"}, FastCursor: true})
		if err != nil {
			b.Fatal(err)
		}
		if err := a.Reseed(context.Background(), ReseedReasonBoot); err != nil {
			b.Fatal(err)
		}
		_ = a.Close()
	}
}

// BenchmarkCursorFsyncLatency (US-69.6 spike 2): ledger-grade fsync cost of
// the durable cursor on THIS filesystem — the baseline against which the
// gVisor/Longhorn/EFS pool numbers are compared; group-commit decision input.
func BenchmarkCursorFsyncLatency(b *testing.B) {
	dir := b.TempDir()
	c, err := openSeqCursor(dir, false)
	if err != nil {
		b.Fatal(err)
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := c.persist(uint64(i + 1)); err != nil {
			b.Fatal(err)
		}
	}
}

// TestSpikeNumbers records the headline numbers as a test (CI keeps them
// observable; the design doc cites pool runs for the cluster-bound ones).
func TestSpikeNumbers(t *testing.T) {
	if testing.Short() {
		t.Skip("short mode")
	}
	t.Run("snapshot_frame_bytes_100", func(t *testing.T) {
		a := benchAuthority(t, 100, 4, 2)
		frames, cancel, err := a.Stream(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		defer cancel()
		f := <-frames
		snap := f.GetSnapshot()
		bytes := proto.Size(snap)
		t.Logf("100-session pod snapshot frame: %d bytes (%.1f KiB)", bytes, float64(bytes)/1024)
		if bytes > 4<<20 {
			t.Errorf("snapshot frame exceeds the 4MiB comfort ceiling: %d bytes — cap/paginate needed", bytes)
		}
	})
	t.Run("fsync_baseline_local", func(t *testing.T) {
		c, err := openSeqCursor(t.TempDir(), false)
		if err != nil {
			t.Fatal(err)
		}
		start := time.Now()
		n := 50
		for i := 0; i < n; i++ {
			if err := c.persist(uint64(i + 1)); err != nil {
				t.Fatal(err)
			}
		}
		perOp := time.Since(start) / time.Duration(n)
		t.Logf("local-fs cursor fsync: %v/op avg over %d", perOp, n)
		if perOp > 10*time.Millisecond {
			t.Logf("NOTE: fsync >10ms/op — group-commit candidacy; pool numbers decide")
		}
	})
}

// BenchmarkResumeBudget_LongSessions (US-69.13 resume_budget evidence):
// the S4 AC's long-session shape — 20k store entries across 50 sessions,
// reseed + cursor replay on boot. Recorded numbers live in the US-69.13
// worklog; the p95 assertion is the pool's (the ~22s e2e budget includes
// pod scheduling + PVC attach that only a cluster measures).
func BenchmarkResumeBudget_LongSessions(b *testing.B) {
	seeds := make(map[string]SessionSeed, 50)
	for i := 0; i < 50; i++ {
		seeds[fmt.Sprintf("s-%d", i)] = SessionSeed{Status: abiv1.SessionStatus_SESSION_STATUS_IDLE}
	}
	dir := b.TempDir()
	warm, err := New(Config{PlatformDir: dir, Parser: nopParser{}, Store: seedStoreN{seeds: seeds}, Passwords: []string{"pw"}, FastCursor: true})
	if err != nil {
		b.Fatal(err)
	}
	// 20k events: the long-session projection cost (store full-list is
	// the upstream pool item; this pins OUR side of the budget).
	for i := 0; i < 20000; i++ {
		warm.IngestForTest(&abiv1.Event{Type: abiv1.EventType_EVENT_TYPE_SESSION_STATUS,
			SessionId: fmt.Sprintf("s-%d", i%50), Status: abiv1.SessionStatus_SESSION_STATUS_BUSY})
	}
	if err := warm.Close(); err != nil {
		b.Fatal(err)
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		a, err := New(Config{PlatformDir: dir, Parser: nopParser{}, Store: seedStoreN{seeds: seeds}, Passwords: []string{"pw"}, FastCursor: true})
		if err != nil {
			b.Fatal(err)
		}
		if err := a.Reseed(context.Background(), ReseedReasonBoot); err != nil {
			b.Fatal(err)
		}
		_ = a.Close()
	}
}
