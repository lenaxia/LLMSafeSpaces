// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

// withShortIDStore swaps the process-wide replica for a fresh
// temp-file-backed one. The emission warm worker is a process-wide
// singleton too: reset it so no prior test's worker/backoff bleeds in,
// and stop it before the fake platform server is torn down.
func withShortIDStore(t *testing.T) *sessionShortIDStore {
	t.Helper()
	resetShortIDWarmForTest(t)
	old := sessionShortIDsAtomic.Load().(*sessionShortIDStore)
	s := newSessionShortIDStore(t.TempDir() + "/session-shortids.json")
	sessionShortIDsAtomic.Store(s)
	t.Cleanup(func() { sessionShortIDsAtomic.Store(old) })
	t.Cleanup(stopShortIDWarmForTest)
	// Production starts the worker at boot; tests start it explicitly so
	// the miss path has no lazy/Background fallback.
	ensureShortIDWarmWorker(t.Context(), nil)
	return s
}

// resetShortIDWarmForTest stops any prior worker and zeroes the warm
// queue/backoff state so each test observes a cold singleton.
func resetShortIDWarmForTest(t *testing.T) {
	t.Helper()
	stopShortIDWarmForTest()
	warmWorkerOnce = sync.Once{}
	warmWake = make(chan struct{}, 1)
	warmMu.Lock()
	warmPending = map[string]struct{}{}
	warmBackoff = map[string]shortIDWarmBackoff{}
	warmMu.Unlock()
}

// stopShortIDWarmForTest cancels the singleton worker and joins it.
func stopShortIDWarmForTest() {
	if warmWorkerCancel != nil {
		warmWorkerCancel()
	}
	warmWorkerWg.Wait()
	warmWorkerCancel = nil
}

func TestShortIDStore_RoundTripPersists(t *testing.T) {
	path := t.TempDir() + "/session-shortids.json"
	s := newSessionShortIDStore(path)
	s.put("amber-falcon-glides", "ses_live")

	// Reload from disk: the mapping survives (atomic-rename write).
	s2 := newSessionShortIDStore(path)
	if id, ok := s2.resolveShortID("Amber-Falcon-Glides"); !ok || id != "ses_live" {
		t.Fatalf("case-insensitive reload broken: %q,%v", id, ok)
	}
	if s2.shortIDFor("ses_live") != "amber-falcon-glides" {
		t.Fatalf("reverse lookup broken: %q", s2.shortIDFor("ses_live"))
	}
}

func TestShortIDStore_RejectsNonShortIDForms(t *testing.T) {
	s := withShortIDStore(t)
	s.put("not-a-short-id", "ses_x")                 // not the 3-word phrase shape
	s.put("oc-ab12cd34", "ses_y")                    // #1745 hash form: never re-learned as canonical
	s.put("ses_ed9183a46ffeRiNw2PddpL5dAA", "ses_z") // full id
	s.put("swift-heron", "ses_w")                    // partial phrase (2 words)
	if s.size() != 0 {
		t.Fatalf("invalid forms must not enter the replica: size %d", s.size())
	}
}

func TestShortIDStore_UnparseableLoadsEmpty(t *testing.T) {
	path := t.TempDir() + "/session-shortids.json"
	if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	s := newSessionShortIDStore(path)
	if s.size() != 0 {
		t.Fatalf("unparseable file must load empty, got %d", s.size())
	}
}

func TestShortIDStore_PutAllReplaces(t *testing.T) {
	s := withShortIDStore(t)
	s.put("linen-orchard-chirps", "ses_old")
	s.putAll(map[string]string{"jade-harvest-murmurs": "ses_new"})
	if _, ok := s.resolveShortID("linen-orchard-chirps"); ok {
		t.Fatal("putAll must replace, not merge")
	}
	if id, ok := s.resolveShortID("jade-harvest-murmurs"); !ok || id != "ses_new" {
		t.Fatal("putAll entry missing")
	}
}

// --- resolution ------------------------------------------------------------

// fakeRefAgent serves a minimal GET /session inventory with slugs.
func fakeRefAgent(t *testing.T, sessions ...map[string]any) {
	t.Helper()
	withAgentServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/session" {
			_ = json.NewEncoder(w).Encode(sessions)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
}

func TestResolveSessionRef_LegacyPassthroughZeroExtraRoundTrips(t *testing.T) {
	withShortIDStore(t)
	// No seam call may happen: a legacy id must resolve byte-identical
	// with ZERO added round trips.
	withAgentServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("legacy-id resolution must not touch the seam: %s", r.URL.Path)
	}))
	got, err := resolveSessionRef(context.Background(), "pw", "ses_ed9183a46ffeRiNw2PddpL5dAA")
	if err != nil || got != "ses_ed9183a46ffeRiNw2PddpL5dAA" {
		t.Fatalf("legacy passthrough broken: %q,%v", got, err)
	}
	// The #1663 truncated id passes through byte-identical too.
	got, err = resolveSessionRef(context.Background(), "pw", "ses_ed9183a46ffeRiNw2PddpL5dA")
	if err != nil || got != "ses_ed9183a46ffeRiNw2PddpL5dA" {
		t.Fatalf("truncated id must pass through unchanged: %q,%v", got, err)
	}
}

func TestResolveSessionRef_ShortIDFromReplica(t *testing.T) {
	s := withShortIDStore(t)
	s.put("amber-falcon-glides", "ses_live")
	withAgentServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("replica hit must not touch the seam: %s", r.URL.Path)
	}))
	got, err := resolveSessionRef(context.Background(), "pw", "Amber-Falcon-Glides")
	if err != nil || got != "ses_live" {
		t.Fatalf("short-id resolution broken: %q,%v", got, err)
	}
}

func TestResolveSessionRef_ShortIDMissOnMissPull(t *testing.T) {
	withShortIDStore(t)
	// Fake platform registry answering the on-miss pull.
	platform := newPlatServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/internal/v1/session-registry" {
			_ = json.NewEncoder(w).Encode(map[string]any{
				"sessions": []map[string]string{{"sessionID": "ses_pulled", "shortID": "opal-meadow-drifts"}},
			})
			return
		}
		w.WriteHeader(http.StatusNotFound)
	})
	setupPodSessionEnv(t, platform)

	withAgentServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("id-shaped miss must not touch the seam: %s", r.URL.Path)
	}))
	// The platform registry answers the on-miss pull synchronously —
	// the FIRST resolve already resolves to the canonical id.
	got, err := resolveSessionRef(context.Background(), "pw", "opal-meadow-drifts")
	if err != nil || got != "ses_pulled" {
		t.Fatalf("on-miss pull resolution broken: %q,%v", got, err)
	}
	// And the replica learned it: the SECOND resolve is replica-only.
	got, err = resolveSessionRef(context.Background(), "pw", "opal-meadow-drifts")
	if err != nil || got != "ses_pulled" {
		t.Fatalf("post-pull resolution broken: %q,%v", got, err)
	}
}

// TestEmissionTranslator_WarmsOnMiss (#1817 piece 3, r4 finding 1): a session
// created through the UI (not the agentd-MCP round trip) is unknown to the
// replica, so the emission translator returns "" for it (heal window). The
// fix schedules an ASYNC replica warm on that miss, so a SUBSEQUENT emission
// for the same session resolves to the phrase. Without the warm, the
// replica never learns UI-created sessions and their contract-stream frames
// stay harness-keyed forever (the headline symptom). This pin:
//   - first call for an unknown harness id returns "" (heal window — never worse),
//   - the async pull populates the replica from the platform registry,
//   - a second call for the same id returns the phrase.
func TestEmissionTranslator_WarmsOnMiss(t *testing.T) {
	s := withShortIDStore(t)
	platform := newPlatServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/internal/v1/session-registry" {
			_ = json.NewEncoder(w).Encode(map[string]any{
				"sessions": []map[string]string{{"sessionID": "ses_uicreated", "shortID": "ultra-violet-drift"}},
			})
			return
		}
		w.WriteHeader(http.StatusNotFound)
	})
	setupPodSessionEnv(t, platform)

	const harnessID = "ses_uicreated"
	const phraseID = "ultra-violet-drift"

	// Isolate the shared TTL rate limiter — a prior test in the package may
	// have set shortIDLastPull, which would suppress this test's on-miss
	// warm (the rate limit is the very mechanism the fix relies on).
	shortIDPullMu.Lock()
	shortIDLastPull = time.Time{}
	shortIDPullMu.Unlock()

	// First emission for a UI-created (replica-unknown) session: heal window.
	if got := emissionIDTranslator(harnessID); got != "" {
		t.Fatalf("first emission for an unknown session must return \"\" (heal window), got %q", got)
	}
	// The async warm must have populated the replica from the registry.
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if s.shortIDFor(harnessID) == phraseID {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if got := s.shortIDFor(harnessID); got != phraseID {
		t.Fatalf("the on-miss async warm did not populate the replica: shortIDFor(%q)=%q, want %q (a UI-created session would emit harness-keyed frames forever)", harnessID, got, phraseID)
	}
	// A subsequent emission for the same session now carries the phrase.
	if got := emissionIDTranslator(harnessID); got != phraseID {
		t.Fatalf("post-warm emission: got %q, want the phrase %q", got, phraseID)
	}
}

// TestEmissionTranslator_WarmNotLockedByPriorPull (#1817 piece 3, r5
// finding 1): a UI-created session B created within the 30s TTL window of a
// prior session A's warm must STILL warm. The r4 design gated the warm on
// the shared fixed 30s TTL (shortIDLastPull), so B's warm would be
// suppressed by A's pull and B's whole first turn streamed harness-keyed —
// #1817's dead-streaming symptom on the common path. The r5 fix gates on an
// in-flight singleflight instead: B's warm proceeds once A's completes.
//
// This pin models the production ordering exactly: the platform registry
// gains session B's row AFTER A's warm has already pulled (the create
// flow registers B in the API's session_index before B's first emission
// reaches agentd). A single re-pull (allowed because no warm is in flight)
// must learn B.
func TestEmissionTranslator_WarmNotLockedByPriorPull(t *testing.T) {
	s := withShortIDStore(t)
	var mu sync.Mutex
	reg := map[string]string{} // the platform registry the pull reads
	platform := newPlatServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/internal/v1/session-registry" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		mu.Lock()
		sessions := make([]map[string]string, 0, len(reg))
		for sid, short := range reg {
			sessions = append(sessions, map[string]string{"sessionID": sid, "shortID": short})
		}
		mu.Unlock()
		_ = json.NewEncoder(w).Encode(map[string]any{"sessions": sessions})
	})
	setupPodSessionEnv(t, platform)

	// Session A exists in the registry from the start.
	mu.Lock()
	reg["ses_A"] = "alpha-amber-arrow"
	mu.Unlock()

	// A's first emission warms the replica (learns A).
	if got := emissionIDTranslator("ses_A"); got != "" {
		t.Fatalf("A's first emission must be the heal window (\"\"), got %q", got)
	}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if s.shortIDFor("ses_A") == "alpha-amber-arrow" {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if got := s.shortIDFor("ses_A"); got != "alpha-amber-arrow" {
		t.Fatalf("A's warm did not populate the replica: %q", got)
	}

	// Session B is created in the registry AFTER A's warm (the create
	// flow's ordering) — within A's 30s TTL window, so the OLD fixed-TTL
	// design would suppress B's warm.
	mu.Lock()
	reg["ses_B"] = "bravo-bright-bloom"
	mu.Unlock()

	// B's first emission (still the heal window) must schedule a FRESH warm
	// (no warm is in flight now — A's completed) and learn B.
	if got := emissionIDTranslator("ses_B"); got != "" {
		t.Fatalf("B's first emission must be the heal window (\"\"), got %q", got)
	}
	deadline = time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if s.shortIDFor("ses_B") == "bravo-bright-bloom" {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if got := s.shortIDFor("ses_B"); got != "bravo-bright-bloom" {
		t.Fatalf("B's warm was suppressed by A's pull (the r4 30s-TTL defect): shortIDFor(ses_B)=%q, want %q — B's first turn would stream harness-keyed", got, "bravo-bright-bloom")
	}
	// A subsequent B emission carries the phrase.
	if got := emissionIDTranslator("ses_B"); got != "bravo-bright-bloom" {
		t.Fatalf("B post-warm emission: got %q, want the phrase", got)
	}
}

// TestEmissionTranslator_NoGoroutinePerEmission (#1817 r7, replacing the
// vacuous r6 pin): the r6 test counted worker STARTS, which stays 1 even if
// a per-emission `go` is reintroduced. This pin measures the thing that
// actually leaked: LIVE goroutines. The registry pull is blocked inside
// httptest, then N=50 distinct unresolved ids are emitted. A single worker
// parks in that one pull; a per-emission goroutine that performs a pull
// leaves one blocked goroutine PER emission. With the barrier held we assert
// the goroutine delta stays a handful. Mutation: reintroducing a
// per-emission `go` (either the literal `go warmSessionShortIDsOnMiss(id)`
// wrapper or a per-emission pull) spikes the peak to ~50 and the pin fails;
// the single worker passes.
func TestEmissionTranslator_NoGoroutinePerEmission(t *testing.T) {
	withShortIDStore(t)
	pullStarted := make(chan struct{})
	releasePull := make(chan struct{})
	var startedOnce sync.Once
	platform := newPlatServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/internal/v1/session-registry" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		startedOnce.Do(func() { close(pullStarted) })
		<-releasePull
		_ = json.NewEncoder(w).Encode(map[string]any{"sessions": []map[string]string{}})
	})
	setupPodSessionEnv(t, platform)

	baseline := runtime.NumGoroutine()
	// Sample the PEAK live count while the emissions run: a per-emission
	// `go` can be short-lived (an enqueue) or blocked (a pull); the peak
	// catches both, the blocked-pull barrier makes the pull variant stick.
	var peak atomic.Int32
	peak.Store(int32(baseline))
	stopSampler := make(chan struct{})
	samplerDone := make(chan struct{})
	go func() {
		defer close(samplerDone)
		for {
			n := int32(runtime.NumGoroutine())
			for {
				old := peak.Load()
				if n <= old || peak.CompareAndSwap(old, n) {
					break
				}
			}
			select {
			case <-stopSampler:
				return
			default:
				runtime.Gosched()
			}
		}
	}()
	for i := 0; i < 50; i++ {
		if got := emissionIDTranslator(fmt.Sprintf("ses_unresolved_%d", i)); got != "" {
			t.Fatalf("unknown session must stay in the heal window, got %q", got)
		}
	}
	// Barrier: the one worker pull is now blocked inside httptest. Any
	// per-emission goroutine that also pulls is blocked here too.
	select {
	case <-pullStarted:
	case <-time.After(3 * time.Second):
		t.Fatal("the warm pull never started")
	}
	// Let all 50 emissions' goroutines (if any) reach the blocked pull.
	time.Sleep(200 * time.Millisecond)
	close(stopSampler)
	<-samplerDone
	got := int(peak.Load())
	if cur := runtime.NumGoroutine(); cur > got {
		got = cur
	}
	close(releasePull)

	// A single worker + its HTTP transport helpers stay well under this.
	// 50 per-emission goroutines (the regression) blow through it.
	const slack = 8
	if got > baseline+slack {
		t.Fatalf("goroutines peaked at %d from a baseline of %d while one warm pull was blocked (max +%d): a per-emission goroutine is present", got, baseline, slack)
	}
}

// TestEmissionTranslator_GlobalPullRateBound (#1817 r7 finding 5): per-id
// backoff bounds a REPEATED id, but N DISTINCT newly-seen ids are each
// immediately due, so without a global floor the worker pulls ~once per new
// id. The floor (shortIDWarmMinPullInterval) batches them. 25 distinct ids
// arriving one per 10ms must yield at most 2 pulls inside the first floor
// window; the same drive against a zero floor yields ~25.
func TestEmissionTranslator_GlobalPullRateBound(t *testing.T) {
	withShortIDStore(t)
	var pulls int32
	platform := newPlatServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/internal/v1/session-registry" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		atomic.AddInt32(&pulls, 1)
		_ = json.NewEncoder(w).Encode(map[string]any{"sessions": []map[string]string{}})
	})
	setupPodSessionEnv(t, platform)

	for i := 0; i < 25; i++ {
		if got := emissionIDTranslator(fmt.Sprintf("ses_distinct_%d", i)); got != "" {
			t.Fatalf("unknown session must stay in the heal window, got %q", got)
		}
		time.Sleep(10 * time.Millisecond)
	}
	// Well inside the global floor window (1s), after the first pull and
	// the burst of distinct ids. Without the floor this is ~25 pulls.
	time.Sleep(100 * time.Millisecond)
	got := atomic.LoadInt32(&pulls)
	if got < 1 {
		t.Fatal("no warm pull ran")
	}
	if got > 2 {
		t.Fatalf("pulls = %d for 25 distinct ids inside the floor window, want <=2: distinct ids are not globally throttled", got)
	}
}

// TestEmissionTranslator_ThrottlesUnresolvedPulls (#1817 r6 finding 3):
// removing the fixed TTL left the emission warm unthrottled — the in-flight
// flag clears on return, so an id the registry never resolves pulls once per
// emitted frame. The worker's per-id exponential backoff (1s → 30s) bounds
// it. 100 frames for a never-resolving id must produce at most 2 pulls inside
// the first backoff window (head: ~one pull per frame).
func TestEmissionTranslator_ThrottlesUnresolvedPulls(t *testing.T) {
	withShortIDStore(t)
	var mu sync.Mutex
	pulls := 0
	platform := newPlatServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/internal/v1/session-registry" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		mu.Lock()
		pulls++
		mu.Unlock()
		_ = json.NewEncoder(w).Encode(map[string]any{"sessions": []map[string]string{}})
	})
	setupPodSessionEnv(t, platform)

	// Spread the frames like a live text stream: on head each frame lands
	// after the previous (fast) pull completed, so the in-flight CAS no
	// longer coalesces them.
	for i := 0; i < 100; i++ {
		emissionIDTranslator("ses_never_resolves")
		time.Sleep(5 * time.Millisecond)
	}
	time.Sleep(100 * time.Millisecond)
	mu.Lock()
	got := pulls
	mu.Unlock()
	if got < 1 {
		t.Fatalf("no warm pull ran: the miss must schedule a bounded pull")
	}
	if got > 2 {
		t.Fatalf("pulls = %d, want <=2: the emission warm is unthrottled (one pull per frame)", got)
	}
}

// TestEmissionTranslator_OverlappingWarmQueued (#1817 r6 finding 4): an
// overlapping warm used to be DROPPED (the in-flight CAS returned false and
// the id was forgotten), so session B emitting while A's pull was in flight
// stayed harness-keyed. The worker queues B and re-pulls after A completes.
// The fake registry returns only A on the first (blocked) pull and A+B on the
// second, proving B's warm was queued rather than satisfied by A's payload.
func TestEmissionTranslator_OverlappingWarmQueued(t *testing.T) {
	s := withShortIDStore(t)
	var mu sync.Mutex
	pulls := 0
	firstStarted := make(chan struct{})
	releaseFirst := make(chan struct{})
	var startedOnce sync.Once
	platform := newPlatServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/internal/v1/session-registry" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		mu.Lock()
		pulls++
		n := pulls
		mu.Unlock()
		if n == 1 {
			startedOnce.Do(func() { close(firstStarted) })
			<-releaseFirst
			_ = json.NewEncoder(w).Encode(map[string]any{"sessions": []map[string]string{
				{"sessionID": "ses_A", "shortID": "alpha-amber-arrow"},
			}})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"sessions": []map[string]string{
			{"sessionID": "ses_A", "shortID": "alpha-amber-arrow"},
			{"sessionID": "ses_B", "shortID": "bravo-bright-bloom"},
		}})
	})
	setupPodSessionEnv(t, platform)

	if got := emissionIDTranslator("ses_A"); got != "" {
		t.Fatalf("A's first emission must be the heal window, got %q", got)
	}
	select {
	case <-firstStarted:
	case <-time.After(3 * time.Second):
		t.Fatal("A's warm pull never started")
	}
	// B emits WHILE A's pull is in flight: it must be queued.
	if got := emissionIDTranslator("ses_B"); got != "" {
		t.Fatalf("B's first emission must be the heal window, got %q", got)
	}
	close(releaseFirst)

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if s.shortIDFor("ses_B") == "bravo-bright-bloom" {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if got := s.shortIDFor("ses_B"); got != "bravo-bright-bloom" {
		t.Fatalf("B's overlapping warm was dropped: shortIDFor(ses_B)=%q, want bravo-bright-bloom", got)
	}
	mu.Lock()
	gotPulls := pulls
	mu.Unlock()
	if gotPulls < 2 {
		t.Fatalf("pulls = %d, want >=2: B must trigger a follow-up pull after A completes", gotPulls)
	}
}

// newPlatServer stands up a fake platform internal API.
func newPlatServer(t *testing.T, fn http.HandlerFunc) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(fn)
	t.Cleanup(srv.Close)
	return srv
}

func TestResolveSessionRef_AliasTitleAndSlug(t *testing.T) {
	withShortIDStore(t)
	fakeRefAgent(t,
		map[string]any{"id": "ses_a", "title": "Worker: #1691 short ids", "slug": "sunny-wolf", "time": map[string]int64{"updated": 1}},
		map[string]any{"id": "ses_b", "title": "Board lane", "slug": "quiet-tiger", "time": map[string]int64{"updated": 2}},
	)
	for _, ref := range []string{"Worker: #1691 short ids", "sunny-wolf", "Sunny-Wolf"} {
		got, err := resolveSessionRef(context.Background(), "pw", ref)
		if err != nil || got != "ses_a" {
			t.Errorf("alias %q resolved to %q,%v; want ses_a", ref, got, err)
		}
	}
}

func TestResolveSessionRef_AliasCollisionRefuses(t *testing.T) {
	withShortIDStore(t)
	// The fleet's live case: two sessions share 'mighty-harbor'.
	fakeRefAgent(t,
		map[string]any{"id": "ses_new", "title": "Explore", "slug": "mighty-harbor"},
		map[string]any{"id": "ses_old", "title": "Architecture Q", "slug": "mighty-harbor"},
	)
	_, err := resolveSessionRef(context.Background(), "pw", "mighty-harbor")
	if err == nil {
		t.Fatal("ambiguous alias must refuse")
	}
	msg := err.Error()
	if !strings.Contains(msg, "ambiguous") || !strings.Contains(msg, "ses_new") || !strings.Contains(msg, "ses_old") {
		t.Fatalf("ambiguity must name both candidates: %q", msg)
	}
}

func TestResolveSessionRef_AliasMissPassesThroughForSeamVerdict(t *testing.T) {
	withShortIDStore(t)
	fakeRefAgent(t,
		map[string]any{"id": "ses_a", "title": "Swift Heron — follow-up", "slug": "swift-heron"},
	)
	// Charset-valid miss (the seam could still be asked): passthrough.
	got, err := resolveSessionRef(context.Background(), "pw", "swift-heron-follow-ups")
	if err != nil || got != "swift-heron-follow-ups" {
		t.Fatalf("charset-valid alias miss must pass through byte-identical: %q,%v", got, err)
	}
	// A never-seam-valid ref (spaces) fails HERE with the clause —
	// the seam's charset guard would only mangle the diagnosis.
	_, err = resolveSessionRef(context.Background(), "pw", "swift heron follow-ups")
	if err == nil || !strings.Contains(err.Error(), "did you mean") {
		t.Fatalf("space-bearing miss must carry the clause: %v", err)
	}
}

// The suggestion half of the miss: the TOOL-level 404 clause carries
// the near title (suggestions, not silence — at the layer that owns
// the verdict).
func TestSessionRead_AliasMissSuggestsNotSilence(t *testing.T) {
	withShortIDStore(t)
	f := newFakeAgent()
	withAgentServer(t, f.handler(t))
	f.newSession("Swift Heron — follow-up")

	_, err := mcpSessionRead(context.Background(), mcpTestPassword, "swift heron follow-ups", 5)
	if err == nil {
		t.Fatal("unknown alias must fail at the seam")
	}
	if !strings.Contains(err.Error(), "did you mean") || !strings.Contains(err.Error(), "Swift Heron") {
		t.Fatalf("the miss must name the near session: %q", err.Error())
	}
}

func TestResolveSessionRef_ListFailureDegradesToPassthrough(t *testing.T) {
	withShortIDStore(t)
	// 5xx from the seam: resolution must degrade, not fail the tool.
	withAgentServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	got, err := resolveSessionRef(context.Background(), "pw", "some-title")
	if err != nil || got != "some-title" {
		t.Fatalf("transport failure must degrade to passthrough: %q,%v", got, err)
	}
}

// --- create_session registers with the platform ----------------------------

func TestCreateSession_ReturnsShortID(t *testing.T) {
	withShortIDStore(t)
	f := newFakeAgent()
	withAgentServer(t, f.handler(t))

	platform := newPlatServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/internal/v1/session-register" {
			_ = json.NewEncoder(w).Encode(map[string]string{"shortID": "amber-falcon-glides"})
			return
		}
		w.WriteHeader(http.StatusNotFound)
	})
	setupPodSessionEnv(t, platform)

	out, err := mcpCreateSession(context.Background(), mcpTestPassword, "do the thing", "t")
	if err != nil {
		t.Fatalf("mcpCreateSession: %v", err)
	}
	// #1756 §1: session_id IS the phrase; short_id no longer emits.
	if !strings.Contains(out, `"session_id":"amber-falcon-glides"`) {
		t.Fatalf("create_session result's session_id must be the registered phrase: %s", out)
	}
	if strings.Contains(out, "short_id") {
		t.Fatalf("the retired short_id field must not emit: %s", out)
	}
	// The replica learned it — future resolution costs no round trip.
	if id, ok := currentSessionShortIDs().resolveShortID("amber-falcon-glides"); !ok || id == "" {
		t.Fatal("replica must learn the created mapping")
	}
}

func TestCreateSession_PlatformTimeoutDegradesToHarnessID(t *testing.T) {
	withShortIDStore(t)
	f := newFakeAgent()
	withAgentServer(t, f.handler(t))

	platform := newPlatServer(t, func(w http.ResponseWriter, r *http.Request) {
		// Never answer before the client's 5s register timeout; the
		// belt-and-suspenders timer keeps the fake reappable even if
		// the cancel signal is lost in transport teardown.
		select {
		case <-r.Context().Done():
		case <-time.After(6 * time.Second):
		}
	})
	setupPodSessionEnv(t, platform)

	out, err := mcpCreateSession(context.Background(), mcpTestPassword, "hello", "")
	if err != nil {
		t.Fatalf("mcpCreateSession (degraded): %v", err)
	}
	if strings.Contains(out, "short_id") {
		t.Fatalf("degraded result must omit short_id: %s", out)
	}
}

// --- session_list joins short ids ------------------------------------------

func TestSessionList_EmitsPhraseAsTheID(t *testing.T) {
	store := withShortIDStore(t)
	store.put("amber-falcon-glides", "ses_a")
	fakeRefAgent(t,
		map[string]any{"id": "ses_a", "title": "Known"},
		map[string]any{"id": "ses_b", "title": "Unknown"},
	)
	out, err := mcpSessionList(context.Background(), mcpTestPassword, true)
	if err != nil {
		t.Fatal(err)
	}
	// #1756 §1: the row's id IS the phrase; the harness id never
	// emits for a mapped row; unmapped rows keep it with the marker.
	if !strings.Contains(out, `"id":"amber-falcon-glides"`) {
		t.Fatalf("session_list row id must be the phrase: %s", out)
	}
	if strings.Contains(out, `"id":"ses_a"`) || strings.Contains(out, `"shortId"`) {
		t.Fatalf("harness ids and the retired shortId field must not emit for mapped rows: %s", out)
	}
	if !strings.Contains(out, `"id":"ses_b"`) || !strings.Contains(out, `"id_deprecated":true`) {
		t.Fatalf("unmapped row keeps the harness id with the deprecation marker: %s", out)
	}
}

func TestSessionList_EmptyReplicaPassesRawBytesThrough(t *testing.T) {
	withShortIDStore(t)
	raw := `[{"id":"ses_a","title":"Verbatim"}]`
	withAgentServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(raw))
	}))
	out, err := mcpSessionList(context.Background(), mcpTestPassword, true)
	if err != nil {
		t.Fatal(err)
	}
	if out != raw {
		t.Fatalf("empty replica must pass raw bytes through unchanged:\n got %s\nwant %s", out, raw)
	}
}

// --- per-tool wiring pins (the orchestrator's acceptance list) --------------

func TestSessionRead_AcceptsAlias(t *testing.T) {
	withShortIDStore(t)
	f := newFakeAgent()
	withAgentServer(t, f.handler(t))
	f.newSession("Alias Readable")

	if _, err := mcpSessionRead(context.Background(), mcpTestPassword, "alias readable", 5); err != nil {
		t.Fatalf("session_read must accept a title alias: %v", err)
	}
}

func TestSendMessage_AcceptsShortIDAndAlias(t *testing.T) {
	store := withShortIDStore(t)
	f := newFakeAgent()
	withAgentServer(t, f.handler(t))
	target := f.newSession("Alias Sendable")
	origin := f.newSession("Origin Session")
	store.put("amber-falcon-glides", target)

	if _, err := mcpSendMessage(context.Background(), mcpTestPassword, "amber-falcon-glides", "ping", "", origin); err != nil {
		t.Fatalf("send_message must accept a short id: %v", err)
	}
	f.awaitMsgDone(t, target, 1)
	if len(f.sentFor(target)) == 0 {
		t.Fatal("the short-id-addressed message must deliver to the canonical session")
	}

	if _, err := mcpSendMessage(context.Background(), mcpTestPassword, "alias sendable", "pong", "", "origin session"); err != nil {
		t.Fatalf("send_message must accept aliases for target and origin: %v", err)
	}
	f.awaitMsgDone(t, target, 2)
}

func TestSessionMetadata_AcceptsSlugAlias(t *testing.T) {
	withShortIDStore(t)
	f := newFakeAgent()
	withAgentServer(t, f.handler(t))
	id := f.newSession("Board lane")
	f.mu.Lock()
	f.slugs[id] = "quiet-tiger"
	f.mu.Unlock()

	out, err := mcpSessionMetadata(context.Background(), mcpTestPassword, "quiet-tiger", "", false, true)
	if err != nil {
		t.Fatalf("session_metadata must accept a slug alias: %v", err)
	}
	if !strings.Contains(out, id) {
		t.Fatalf("metadata must describe the resolved session: %s", out)
	}
}

func TestSessionMetadata_SlugCollisionRefuses(t *testing.T) {
	withShortIDStore(t)
	fakeRefAgent(t,
		map[string]any{"id": "ses_a", "title": "A", "slug": "twin-star"},
		map[string]any{"id": "ses_b", "title": "B", "slug": "twin-star"},
	)
	_, err := mcpSessionMetadata(context.Background(), mcpTestPassword, "twin-star", "", false, true)
	if err == nil || !strings.Contains(err.Error(), "ambiguous") {
		t.Fatalf("slug collision must refuse with ambiguity: %v", err)
	}
}

func TestAbortSession_AcceptsShortID(t *testing.T) {
	store := withShortIDStore(t)
	f := newFakeAgent()
	withAgentServer(t, f.handler(t))
	id := f.newSession("Busy")
	f.busySet[id] = true
	store.put("amber-falcon-glides", id)

	if _, err := mcpAbortSession(context.Background(), mcpTestPassword, "Amber-Falcon-Glides"); err != nil {
		t.Fatalf("abort_session must accept a case-insensitive short id: %v", err)
	}
	assert.Equal(t, []string{id}, f.aborted)
}

func TestArchiveSession_AcceptsAlias(t *testing.T) {
	withShortIDStore(t)
	f := newFakeAgent()
	withAgentServer(t, f.handler(t))
	f.newSession("Archivable")

	var archivedBody string
	platform := newPlatServer(t, func(w http.ResponseWriter, r *http.Request) {
		buf := make([]byte, 512)
		n, _ := r.Body.Read(buf)
		archivedBody = string(buf[:n])
		w.WriteHeader(http.StatusNoContent)
	})
	setupPodSessionEnv(t, platform)

	if _, err := mcpSessionArchive(context.Background(), mcpTestPassword, "archivable", true); err != nil {
		t.Fatalf("session_archive must accept an alias: %v", err)
	}
	if !strings.Contains(archivedBody, `"sessionID":"ses_`) {
		t.Fatalf("platform must receive the canonical harness id: %q", archivedBody)
	}
}

// --- warm worker lifecycle + bounds (#1817 r9 findings 1-2) ----------------

// TestShortIDWarm_ShutdownJoinsWorker pins the JOIN, not the goroutine
// start: the lifecycle WaitGroup must be released only when the worker LOOP
// exits. The registry pull is held in flight and the replica write lock is
// held, so the worker cannot finish after cancellation. lifecycle.Wait must
// stay blocked until both are released; moving Done ahead of
// shortIDWarmWorker makes it return immediately and this pin fails.
func TestShortIDWarm_ShutdownJoinsWorker(t *testing.T) {
	resetShortIDWarmForTest(t)
	s := newSessionShortIDStore(t.TempDir() + "/session-shortids.json")
	old := sessionShortIDsAtomic.Load().(*sessionShortIDStore)
	sessionShortIDsAtomic.Store(s)
	t.Cleanup(func() { sessionShortIDsAtomic.Store(old) })
	t.Cleanup(stopShortIDWarmForTest)

	pullStarted := make(chan struct{})
	releasePull := make(chan struct{})
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(releasePull) }) }
	var startedOnce sync.Once
	platform := newPlatServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/internal/v1/session-registry" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		startedOnce.Do(func() { close(pullStarted) })
		<-releasePull
		_ = json.NewEncoder(w).Encode(map[string]any{"sessions": []map[string]string{}})
	})
	setupPodSessionEnv(t, platform)

	ctx, cancel := context.WithCancel(context.Background())
	var lifecycle sync.WaitGroup
	ensureShortIDWarmWorker(ctx, &lifecycle)

	s.mu.Lock()
	unlocked := false
	t.Cleanup(func() {
		if !unlocked {
			s.mu.Unlock()
		}
		release()
		cancel()
	})

	warmSessionShortIDsOnMiss("ses_join_probe")
	select {
	case <-pullStarted:
	case <-time.After(3 * time.Second):
		t.Fatal("the warm pull never started")
	}

	cancel()

	joined := make(chan struct{})
	go func() {
		lifecycle.Wait()
		close(joined)
	}()

	// The worker is still in flight (blocked in the pull handler, then on
	// the replica lock). A Done-before-loop releases lifecycle here.
	select {
	case <-joined:
		t.Fatal("lifecycle.Wait returned while the warm worker was still in flight: shutdown does not join it")
	case <-time.After(200 * time.Millisecond):
	}

	s.mu.Unlock()
	unlocked = true
	release()

	select {
	case <-joined:
	case <-time.After(3 * time.Second):
		t.Fatal("lifecycle.Wait did not return after the worker exited")
	}

	exited := make(chan struct{})
	go func() {
		warmWorkerWg.Wait()
		close(exited)
	}()
	select {
	case <-exited:
	case <-time.After(3 * time.Second):
		t.Fatal("the warm worker did not exit")
	}
}

// TestShortIDWarm_PendingCap pins the hard pending-set ceiling: many
// distinct unresolved ids cannot grow warmPending without bound, and an
// emission after an overflow reset is still enqueued.
func TestShortIDWarm_PendingCap(t *testing.T) {
	resetShortIDWarmForTest(t)
	total := shortIDWarmMaxPending * 3
	for i := 0; i < total; i++ {
		warmSessionShortIDsOnMiss(fmt.Sprintf("ses_pending_cap_%d", i))
	}
	warmMu.Lock()
	got := len(warmPending)
	warmMu.Unlock()
	if got > shortIDWarmMaxPending {
		t.Fatalf("warmPending = %d, want <= %d: the pending set is unbounded", got, shortIDWarmMaxPending)
	}
	if got == 0 {
		t.Fatal("warmPending is empty after a burst; the overflow reset dropped the newest emission")
	}

	warmSessionShortIDsOnMiss("ses_pending_cap_after_reset")
	warmMu.Lock()
	_, ok := warmPending["ses_pending_cap_after_reset"]
	warmMu.Unlock()
	if !ok {
		t.Fatal("an emission after an overflow reset must be enqueued again")
	}
}

// TestShortIDWarm_Prune pins the single-pass prune: expired entries leave
// both maps while live ones survive, and the backoff ceiling evicts the
// oldest surplus in one call.
func TestShortIDWarm_Prune(t *testing.T) {
	resetShortIDWarmForTest(t)
	now := time.Now()
	expired := now.Add(-shortIDWarmEntryTTL - time.Minute)
	live := now.Add(-time.Minute)

	warmMu.Lock()
	warmBackoff = map[string]shortIDWarmBackoff{
		"ses_old_a": {lastSeen: expired},
		"ses_old_b": {lastSeen: expired},
		"ses_live":  {lastSeen: live},
	}
	warmPending = map[string]struct{}{
		"ses_old_a": {},
		"ses_old_b": {},
		"ses_live":  {},
	}
	shortIDWarmPruneLocked(now)
	_, oldA := warmBackoff["ses_old_a"]
	_, oldB := warmBackoff["ses_old_b"]
	_, liveKept := warmBackoff["ses_live"]
	_, pendOldA := warmPending["ses_old_a"]
	_, pendOldB := warmPending["ses_old_b"]
	_, pendLive := warmPending["ses_live"]
	warmMu.Unlock()

	if oldA || oldB || pendOldA || pendOldB {
		t.Fatal("expired entries must be pruned from both warmBackoff and warmPending")
	}
	if !liveKept || !pendLive {
		t.Fatal("the live entry must survive the prune")
	}

	// Ceiling: keep exactly shortIDWarmMaxEntries, evicting the oldest.
	warmMu.Lock()
	warmBackoff = make(map[string]shortIDWarmBackoff, shortIDWarmMaxEntries+16)
	warmPending = make(map[string]struct{}, shortIDWarmMaxEntries+16)
	for i := 0; i < shortIDWarmMaxEntries+16; i++ {
		id := fmt.Sprintf("ses_ceiling_%05d", i)
		warmBackoff[id] = shortIDWarmBackoff{lastSeen: now.Add(time.Duration(i) * time.Second)}
		warmPending[id] = struct{}{}
	}
	shortIDWarmPruneLocked(now)
	n := len(warmBackoff)
	nPending := len(warmPending)
	_, oldest := warmBackoff["ses_ceiling_00000"]
	_, newest := warmBackoff[fmt.Sprintf("ses_ceiling_%05d", shortIDWarmMaxEntries+15)]
	warmMu.Unlock()

	if n != shortIDWarmMaxEntries || nPending != shortIDWarmMaxEntries {
		t.Fatalf("after ceiling prune: warmBackoff=%d warmPending=%d, want both %d", n, nPending, shortIDWarmMaxEntries)
	}
	if oldest {
		t.Fatal("the oldest surplus entry must be evicted")
	}
	if !newest {
		t.Fatal("the newest entry must survive the ceiling prune")
	}
}
