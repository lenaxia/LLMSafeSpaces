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
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

// withShortIDStore swaps the process-wide replica for a fresh
// temp-file-backed one.
func withShortIDStore(t *testing.T) *sessionShortIDStore {
	t.Helper()
	old := sessionShortIDsAtomic.Load().(*sessionShortIDStore)
	s := newSessionShortIDStore(t.TempDir() + "/session-shortids.json")
	sessionShortIDsAtomic.Store(s)
	t.Cleanup(func() { sessionShortIDsAtomic.Store(old) })
	return s
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

// TestEmissionTranslator_WarmDedupsToSinglePull (#1817 piece 3, r5 finding
// 2): many unresolved emissions while the replica is cold must NOT spawn an
// unbounded number of concurrent warm pulls (the r4 draft forked a goroutine
// per emission frame). The singleflight flag coalesces them to one in-flight
// pull. This pin makes the platform pull BLOCK so the in-flight window is
// observable, fires many emissions, and asserts only ONE pull ran.
func TestEmissionTranslator_WarmDedupsToSinglePull(t *testing.T) {
	s := withShortIDStore(t)
	_ = s
	var mu sync.Mutex
	pulls := 0
	var releaseOnce sync.Once
	release := make(chan struct{})
	blockPull := true
	platform := newPlatServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/internal/v1/session-registry" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		mu.Lock()
		pulls++
		block := blockPull
		mu.Unlock()
		if block {
			<-release // block the pull so the in-flight window is observable
		}
		releaseOnce.Do(func() { close(release) })
		_ = json.NewEncoder(w).Encode(map[string]any{"sessions": []map[string]string{}})
	})
	setupPodSessionEnv(t, platform)

	// Fire many emissions for distinct unresolved sessions. The first spawns
	// the (blocked) pull; the rest must coalesce onto it (no second pull).
	for i := 0; i < 20; i++ {
		emissionIDTranslator(fmt.Sprintf("ses_many_%d", i))
	}
	// Give the coalesced warm time to attempt (and block on) the pull.
	time.Sleep(300 * time.Millisecond)
	mu.Lock()
	gotPulls := pulls
	// Release the blocked pull and stop future ones from blocking.
	blockPull = false
	mu.Unlock()
	releaseOnce.Do(func() { close(release) })
	// One warm goroutine is allowed a little slack, but it must NOT be 20.
	if gotPulls > 2 {
		t.Fatalf("expected the singleflight to coalesce the warm to ~1 pull, got %d (the r4 goroutine-per-emission defect)", gotPulls)
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
