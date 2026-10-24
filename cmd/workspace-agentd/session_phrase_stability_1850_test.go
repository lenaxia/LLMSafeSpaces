// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

// #1850 red-first pins — session phrase stability across the
// create_session spawn path. The reported fleet failures (3/3 on
// 2026-10-22): a worker spawned via create_session is issued a NEW
// 3-word phrase at its first turn boundary (old phrase dead — a
// send_message to it 500s; the session re-emits flagged
// id_deprecated). Two independent defects fire exactly there:
//
//  1. the agentd replica's registry-pull apply REPLACES the whole map
//     (putAll): a pull that started before the spawn's register
//     round-trip committed returns without the fresh row and wipes the
//     just-put phrase→harness mapping — the warm worker's 1s pull
//     cadence makes this the spawn race, not a rarity;
//  2. the API's reconcile pass reaps the fresh spawn's session_index
//     row while its (long) first turn is still generating: the row has
//     message_count==0 and the harness list is not a liveness
//     authority for Act-path sessions (#1452 — live but list-invisible);
//     the first turn-boundary event then re-registers the session and
//     the registration seam MINTS A NEW PHRASE. (Pinned on the API
//     side: proxy_session_reconcile_test.go.)

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// --- unit: the replica apply is a merge, never a wipe -----------------------

// TestSessionShortIDs_PullMustNotWipeRacedPut pins defect 1 in
// isolation: a registry pull that does not list a mapping the store
// already holds (the pull's snapshot predates the register commit)
// must not remove it. Pre-fix, putAll replaced the whole map — the
// raced put died with the pull and the spawned session degraded to
// harness-id emission (id_deprecated) until some later pull happened
// to re-learn it.
func TestSessionShortIDs_PullMustNotWipeRacedPut(t *testing.T) {
	s := newSessionShortIDStore(t.TempDir() + "/session-shortids.json")

	// create_session's register round trip learns the fresh mapping.
	s.put("amber-falcon-glides", "ses_spawn")

	// A pull whose snapshot predates the register (in-flight at spawn)
	// returns only the pre-existing rows.
	s.putAll(map[string]string{
		"brave-otter-slides": "ses_old",
	})

	if _, ok := s.resolveShortID("amber-falcon-glides"); !ok {
		t.Fatal("stale pull wiped the raced put: the spawn phrase no longer resolves (#1850 re-phrase class)")
	}
	if _, ok := s.resolveShortID("brave-otter-slides"); !ok {
		t.Fatal("the pull must still install its own entries")
	}
	if got := s.shortIDFor("ses_spawn"); got != "amber-falcon-glides" {
		t.Fatalf("reverse mapping lost: shortIDFor(ses_spawn)=%q", got)
	}

	// A LATER pull that carries a CHANGED entry for a known phrase
	// still wins (upsert, not first-writer-wins) — the registry is the
	// authority for what it lists.
	s.putAll(map[string]string{
		"brave-otter-slides": "ses_old2",
	})
	if id, _ := s.resolveShortID("brave-otter-slides"); id != "ses_old2" {
		t.Fatalf("pull upsert must overwrite: brave-otter-slides -> %q, want ses_old2", id)
	}
}

// --- e2e: the spawn phrase survives the first turn boundary -----------------

// spawnPhrasePlatform fakes the platform's internal API for the spawn
// lane: POST /internal/v1/session-register mints one phrase per
// session (recorded), GET /internal/v1/session-registry serves a
// registry snapshot the test can flip (the stale-pull race).
type spawnPhrasePlatform struct {
	mu        sync.Mutex
	minted    map[string]string // harness id -> phrase
	registry  map[string]string // phrase -> harness id (what the pull serves); nil = serve minted verbatim
	registerN int
}

func newSpawnPhrasePlatform() *spawnPhrasePlatform {
	return &spawnPhrasePlatform{minted: map[string]string{}}
}

func (p *spawnPhrasePlatform) handler(t *testing.T) http.HandlerFunc {
	t.Helper()
	return func(w http.ResponseWriter, r *http.Request) {
		p.mu.Lock()
		defer p.mu.Unlock()
		switch r.URL.Path {
		case "/internal/v1/session-register":
			p.registerN++
			var req struct {
				WorkspaceID string `json:"workspaceID"`
				SessionID   string `json:"sessionID"`
			}
			require.NoError(t, json.NewDecoder(r.Body).Decode(&req))
			phrase := p.minted[req.SessionID]
			if phrase == "" {
				// Deterministic, shape-correct mint for the test.
				phrase = strings.ToLower("amber-falcon-glides")
				if len(p.minted) == 1 {
					phrase = "brave-otter-slides"
				}
				p.minted[req.SessionID] = phrase
			}
			_ = json.NewEncoder(w).Encode(map[string]string{"shortID": phrase})
		case "/internal/v1/session-registry":
			entries := p.registry
			if entries == nil {
				entries = map[string]string{}
				for h, ph := range p.minted {
					entries[ph] = h
				}
			}
			out := make([]map[string]string, 0, len(entries))
			for ph, h := range entries {
				out = append(out, map[string]string{"sessionID": h, "shortID": ph})
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"sessions": out})
		case "/internal/v1/session-archived":
			// session_list's archived-set consult (freshness-gated;
			// empty set = nothing filtered).
			_ = json.NewEncoder(w).Encode(map[string]any{"archived": []string{}})
		default:
			t.Errorf("spawnPhrasePlatform: unexpected %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}
}

func (p *spawnPhrasePlatform) phraseFor(harnessID string) string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.minted[harnessID]
}

func (p *spawnPhrasePlatform) setRegistry(entries map[string]string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.registry = entries
}

// TestCreateSession_PhraseStableAcrossFirstTurnBoundary drives the
// REAL create_session path against the fake seam + fake platform and
// pins the #1850 contract end to end: the phrase returned at spawn is
// still the session's emitted id after (a) the first turn's prompt
// delivery completes and (b) a registry pull that predates the spawn's
// registration lands — and a send addressed to the ORIGINAL phrase
// still resolves and delivers (never a 500).
func TestCreateSession_PhraseStableAcrossFirstTurnBoundary(t *testing.T) {
	store := withShortIDStore(t)
	f := newFakeAgent()
	withAgentServer(t, f.handler(t))

	plat := newSpawnPhrasePlatform()
	platform := newPlatServer(t, plat.handler(t))
	setupPodSessionEnv(t, platform)

	out, err := mcpCreateSession(context.Background(), mcpTestPassword, "do the work", "worker-1")
	require.NoError(t, err)

	var created struct {
		SessionID string `json:"session_id"`
	}
	require.NoError(t, json.Unmarshal([]byte(out), &created))
	phrase := created.SessionID
	require.True(t, isLowercase3WordPhrase(phrase), "create_session must return the 3-word phrase, got %q", phrase)

	// The harness id the platform registered (the spawned session).
	var harnessID string
	for id := range f.titles {
		if id != "" && plat.phraseFor(id) == phrase {
			harnessID = id
		}
	}
	require.NotEmpty(t, harnessID, "the spawn phrase must map to the created harness session")
	assert.Equal(t, phrase, plat.phraseFor(harnessID))

	// (a) First turn boundary: the background prompt delivery lands.
	f.awaitMsgDone(t, harnessID, 1)

	// Emission still the phrase — no deprecation marker.
	assertSessionListShowsPhrase(t, phrase, harnessID)

	// (b) The stale-pull race: a pull whose snapshot predates the
	// registration (the warm worker's in-flight pull at spawn) returns
	// WITHOUT the fresh row.
	plat.setRegistry(map[string]string{})
	require.True(t, pullSessionShortIDs(context.Background()), "the stale pull must succeed (it is a valid snapshot)")

	// THE #1850 assertion: the spawn phrase survives the stale pull.
	assertSessionListShowsPhrase(t, phrase, harnessID)
	if _, ok := store.resolveShortID(phrase); !ok {
		t.Fatalf("stale pull wiped the spawn phrase %q — session_list degrades to the harness id + id_deprecated", phrase)
	}

	// A later converged pull re-affirms the mapping.
	plat.setRegistry(nil)
	require.True(t, pullSessionShortIDs(context.Background()))
	assertSessionListShowsPhrase(t, phrase, harnessID)

	// Send to the ORIGINAL phrase: resolves and delivers (the #1820
	// delivery boundary; a re-phrased session is what made this 500).
	caller := f.newSession("caller")
	sendOut, serr := mcpSendMessage(context.Background(), mcpTestPassword, phrase, "status?", caller)
	require.NoError(t, serr, "send_message to the ORIGINAL spawn phrase must not fail")
	assert.Contains(t, sendOut, "delivering", "the phrase-addressed send must deliver: %s", sendOut)
	f.awaitMsgDone(t, harnessID, 2)
}

// assertSessionListShowsPhrase drives the real session_list emission
// and requires the spawned session's row to carry the phrase as its id
// with NO id_deprecated marker (the degraded emission is exactly the
// #1850 observable: raw harness id + id_deprecated: true).
func assertSessionListShowsPhrase(t *testing.T, phrase, harnessID string) {
	t.Helper()
	out, err := mcpSessionList(context.Background(), mcpTestPassword, false)
	if err != nil {
		t.Fatalf("session_list: %v", err)
	}
	var rows []map[string]any
	require.NoError(t, json.Unmarshal([]byte(out), &rows))
	for _, row := range rows {
		if id, _ := row["id"].(string); id == phrase || id == harnessID {
			assert.Equal(t, phrase, id, "the spawned session must emit its ORIGINAL phrase as the id (row: %v)", row)
			assert.NotContains(t, row, "id_deprecated", "a phrase-known row must never carry the deprecation marker (row: %v)", row)
			return
		}
	}
	t.Fatalf("session_list does not show the spawned session (%s/%s): %s", phrase, harnessID, out)
}

// TestResolveSessionRef_SpawnPhraseSurvivesCompactionAndRename pins
// the acceptance bar's second leg on the resolution tier: after the
// compaction (summarize) and rename paths touch the session, the
// ORIGINAL phrase still resolves to the same harness id.
func TestResolveSessionRef_SpawnPhraseSurvivesCompactionAndRename(t *testing.T) {
	store := withShortIDStore(t)
	f := newFakeAgent()
	withAgentServer(t, f.handler(t))
	harnessID := f.newSession("worker-2")
	const phrase = "calm-beacon-drifts"
	store.put(phrase, harnessID)

	ctx := context.Background()

	// Rename (title change — the alias layer moves; the id must not).
	_, rerr := mcpRenameSession(ctx, mcpTestPassword, harnessID, "worker-2 renamed")
	require.NoError(t, rerr)
	got, err := resolveSessionRef(ctx, mcpTestPassword, phrase)
	require.NoError(t, err)
	assert.Equal(t, harnessID, got, "the phrase must survive a rename")

	// Compaction (the summarize route — the repro noted one victim was
	// compacted at the boundary; the mapping is untouched).
	cout, cerr := mcpCompact(ctx, mcpTestPassword, harnessID, "")
	require.NoError(t, cerr)
	assert.Contains(t, cout, "compact", "compact result shape: %s", cout)
	got, err = resolveSessionRef(ctx, mcpTestPassword, phrase)
	require.NoError(t, err)
	assert.Equal(t, harnessID, got, "the phrase must survive compaction")
}

// isLowercase3WordPhrase mirrors the #1756 canonical shape check
// (readable pins; the classifier itself is pkg/shortid's business).
func isLowercase3WordPhrase(s string) bool {
	parts := strings.Split(s, "-")
	if len(parts) != 3 {
		return false
	}
	for _, p := range parts {
		if p == "" || p != strings.ToLower(p) {
			return false
		}
		for _, c := range p {
			if c < 'a' || c > 'z' {
				return false
			}
		}
	}
	return true
}
