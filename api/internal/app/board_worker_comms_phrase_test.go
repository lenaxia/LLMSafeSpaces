// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package app

// board_worker_comms_phrase_test.go — the #1959c phrase-resolution WIRING
// pins (issue #1959 root cause 2, comment 6425743005).
//
// The #1963 wiring pins (board_worker_comms_test.go) ran against a stub
// opencode that ACCEPTED any session id on V1 — so the phrase-resolution
// gap was invisible to them: the engine's assignee/implementer_session
// store the platform PHRASE (durable identity, #1850/#1816; live evidence
// on 0.44.3: the re-dispatch POSTed /session/florid-teapot-verifies/message),
// while opencode's own routes key on RAW ses_ ids — a phrase id VERBATIM
// is a 404, the 10s fast-fail returns it, and pkg/board deterministically
// falls back to a fresh spawn (the token drain).
//
// These pins run against a REAL-SHAPE stub (stubRealOpencode):
//
//   - POST /session/:id/message and /session/:id/abort answer 404 for
//     every id outside the stub's live session set — phrase ids are
//     NEVER accepted by opencode routes (they are platform-layer
//     identity, #1816; opencode has never heard of them);
//   - GET /session lists the live sessions in the 1.18.x bare-array
//     shape — with NO phrase field on the wire (the honest production
//     finding: the opencode session list cannot carry the phrase→raw
//     binding, so the SEAM's resolution — the platform's own phrase
//     registry — is the only path to green).
//
// Born RED against the verbatim-delivery wiring (the pins compile against
// main; every resolution pin fails exactly the way the live 0.44.3 probe
// did: the phrase POST 404s and the fresh-worker fallback fires).

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// stubRealOpencode is the REAL-SEMANTICS opencode backend (#1959c): the
// live-session set is RAW-keyed — unknown ids 404, phrase ids are never
// members (phrases are platform-layer identity opencode has never seen).
// This is the stub shape the accept-anything #1963 stub should have had:
// it is the only shape under which a verbatim-phrase delivery REDS.
type stubRealOpencode struct {
	*httptest.Server
	mu sync.Mutex

	live        map[string]bool // raw ses_ ids opencode knows (phrases NEVER enter this set)
	postedTo    []string        // session ids that received POST /session/:id/message, in order
	abortedTo   []string        // session ids that received POST /session/:id/abort, in order
	listServed  int             // GET /session count (the list endpoint exists; it carries no phrases)
	v1Texts     []string
	messageReqs int
}

func newStubRealOpencode(t *testing.T, live ...string) *stubRealOpencode {
	t.Helper()
	s := &stubRealOpencode{live: map[string]bool{}}
	for _, id := range live {
		s.live[id] = true
	}
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Path
		switch {
		case r.Method == http.MethodPost && isV1MessagePath(path):
			id := v1SessionID(path, "/message")
			var body struct {
				Parts []struct {
					Type string `json:"type"`
					Text string `json:"text"`
				} `json:"parts"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			var texts []string
			for _, p := range body.Parts {
				if p.Type == "text" {
					texts = append(texts, p.Text)
				}
			}
			s.mu.Lock()
			known := s.live[id]
			s.messageReqs++
			if known {
				s.postedTo = append(s.postedTo, id)
				s.v1Texts = append(s.v1Texts, strings.Join(texts, "\n"))
			}
			s.mu.Unlock()
			s.answerSessionRoute(w, known)
		case r.Method == http.MethodPost && strings.HasSuffix(path, "/abort"):
			id := v1SessionID(path, "/abort")
			s.mu.Lock()
			known := s.live[id]
			if known {
				s.abortedTo = append(s.abortedTo, id)
			}
			s.mu.Unlock()
			s.answerSessionRoute(w, known)
		case r.Method == http.MethodGet && path == "/session":
			// The 1.18.x list shape: a bare array of session objects,
			// keyed by RAW ids — no phrase field exists on this wire
			// (the phrase→raw binding is platform-layer, #1816).
			s.mu.Lock()
			s.listServed++
			ids := make([]string, 0, len(s.live))
			for id := range s.live {
				ids = append(ids, id)
			}
			s.mu.Unlock()
			var b strings.Builder
			b.WriteString("[")
			for i, id := range ids {
				if i > 0 {
					b.WriteString(",")
				}
				b.WriteString(fmt.Sprintf(`{"id":%q,"title":"session %s"}`, id, id))
			}
			b.WriteString("]")
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(b.String()))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(s.Close)
	return s
}

// answerSessionRoute renders opencode's raw-keyed route verdict: a known
// RAW id gets the V1 success wire; anything else — an unknown id, or a
// PHRASE id (never a member of the live set) — gets the 404 that fires
// the seam's fast-fail and pkg/board's fresh-worker fallback.
func (s *stubRealOpencode) answerSessionRoute(w http.ResponseWriter, known bool) {
	w.Header().Set("Content-Type", "application/json")
	if !known {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":"session not found"}`))
		return
	}
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(`{"info":{"role":"assistant","id":"msg_v1_reply","time":{"created":1786400000000}},"parts":[{"type":"text","text":"V1 reply"}]}`))
}

// v1SessionID extracts the :id segment of /session/:id/<suffix>.
func v1SessionID(path, suffix string) string {
	trimmed := strings.TrimSuffix(path, suffix)
	return strings.TrimPrefix(trimmed, "/session/")
}

func (s *stubRealOpencode) snapshot() (posted, aborted, texts []string, messageReqs int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.postedTo...),
		append([]string(nil), s.abortedTo...),
		append([]string(nil), s.v1Texts...),
		s.messageReqs
}

// stubPhraseRegistry is the stand-in for the platform's phrase registry
// (session_index — the same layer the SessionRefResolution middleware and
// the pod replica read). The seam's resolution consults exactly this
// surface; the stub opencode's list endpoint deliberately carries none of
// it, so resolution can ONLY come from the seam.
type stubPhraseRegistry struct {
	mu        sync.Mutex
	byPhrase  map[string]string // lowercased phrase → raw harness id
	lookups   []string
	dbFailure error
}

func (r *stubPhraseRegistry) lookup(_ context.Context, _, phrase string) (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.lookups = append(r.lookups, phrase)
	if r.dbFailure != nil {
		return "", r.dbFailure
	}
	return r.byPhrase[phrase], nil // "" = no session holds the phrase
}

func (r *stubPhraseRegistry) recorded() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.lookups...)
}

const (
	implPhrase  = "florid-teapot-verifies" // the live probe's original implementer
	implRaw     = "ses_raw_implementer"    // the raw id opencode actually keys on
	deadPhrase  = "reaped-lantern-hushes"  // a phrase no live session holds
	phraseShort = "tsk-phrase-pin"
)

// TestBoardWorkerComms_PhraseResolution_RedispatchPostsToRawID is THE
// #1959c pin: a re-dispatch addressed by the stored PHRASE must resolve
// to the RAW harness id and POST there — the original implementer's
// EXISTING session receives the turn (the owner ruling's bounce-target
// semantics), not a fresh spawn. Born RED exactly as the live 0.44.3
// probe failed: the phrase id posted VERBATIM meets the stub's raw-keyed
// 404 and the seam returns the fresh-worker fallback error.
func TestBoardWorkerComms_PhraseResolution_RedispatchPostsToRawID(t *testing.T) {
	stub := newStubRealOpencode(t, implRaw)
	adapter := newBoardCommsAdapter(t, stub.Server)
	scope := &stubScope{}
	registry := &stubPhraseRegistry{byPhrase: map[string]string{implPhrase: implRaw}}

	redispatch := newBoardRedispatcher(scope.resolve, newBoardResolvingSender(adapter, registry.lookup))

	err := redispatch(context.Background(), implPhrase, redispatchText, phraseShort)
	require.NoError(t, err,
		"a live phrase-addressed implementer must receive the re-dispatch — an error here is the phrase posted verbatim (the 404 → fresh-worker fallback, #1959 root cause 2)")

	posted, _, texts, messageReqs := stub.snapshot()
	require.Len(t, posted, 1, "the V1 POST must be attempted exactly once")
	assert.Equal(t, implRaw, posted[0],
		"the stub must receive the RAW ses_ id — opencode routes are raw-keyed; a phrase id verbatim is a 404")
	assert.Equal(t, 1, messageReqs)
	require.Len(t, texts, 1)
	assert.Contains(t, texts[0], "BOARD BOUNCE RE-DISPATCH")
	assert.Contains(t, texts[0], "tok-fresh")
	assert.Equal(t, []string{implPhrase}, registry.recorded(),
		"the seam must consult the phrase registry exactly once, with the stored phrase")
	assert.Equal(t, []string{phraseShort}, scope.recorded())
}

// TestBoardWorkerComms_PhraseResolution_UnknownPhrase_FastFailKeepsFallback
// pins the liveness contract under resolution: a phrase NO live session
// holds (reaped/archived/never-turned original) must surface as a
// synchronous seam error within the fast-fail window — pkg/board's
// fresh-implementer fallback keys on it — and NO V1 post may be
// attempted against the phrase (the resolution failure precedes delivery).
func TestBoardWorkerComms_PhraseResolution_UnknownPhrase_FastFailKeepsFallback(t *testing.T) {
	stub := newStubRealOpencode(t, implRaw)
	adapter := newBoardCommsAdapter(t, stub.Server)
	registry := &stubPhraseRegistry{byPhrase: map[string]string{implPhrase: implRaw}}

	redispatch := newBoardRedispatcher((&stubScope{}).resolve, newBoardResolvingSender(adapter, registry.lookup))

	start := time.Now()
	err := redispatch(context.Background(), deadPhrase, redispatchText, phraseShort)
	elapsed := time.Since(start)
	require.Error(t, err, "an unknown/dead phrase must return error so pkg/board spawns a fresh worker")
	assert.Contains(t, err.Error(), deadPhrase,
		"the error must name the unresolvable phrase (the liveness answer's provenance)")
	assert.Less(t, elapsed, boardV1FastFail,
		"the resolution verdict must arrive within the fast-fail window")

	posted, _, _, messageReqs := stub.snapshot()
	assert.Empty(t, posted, "no V1 post may fire for an unresolvable phrase")
	assert.Equal(t, 0, messageReqs, "delivery must not be attempted when resolution fails")
}

// TestBoardWorkerComms_PhraseResolution_RegistryFailure_IsFallbackError
// pins the degraded-lookup posture: a transient registry (DB) failure is
// a synchronous seam error (never a silent passthrough that would POST
// the phrase verbatim), within the window.
func TestBoardWorkerComms_PhraseResolution_RegistryFailure_IsFallbackError(t *testing.T) {
	stub := newStubRealOpencode(t, implRaw)
	adapter := newBoardCommsAdapter(t, stub.Server)
	registry := &stubPhraseRegistry{
		byPhrase:  map[string]string{implPhrase: implRaw},
		dbFailure: fmt.Errorf("session_index unavailable"),
	}

	redispatch := newBoardRedispatcher((&stubScope{}).resolve, newBoardResolvingSender(adapter, registry.lookup))

	err := redispatch(context.Background(), implPhrase, redispatchText, phraseShort)
	require.Error(t, err, "a registry failure must surface as the seam's error, not a verbatim-phrase POST")
	assert.Contains(t, err.Error(), "session_index unavailable")
	_, _, _, messageReqs := stub.snapshot()
	assert.Equal(t, 0, messageReqs, "no delivery may fire when the registry errors")
}

// TestBoardWorkerComms_PhraseResolution_DirectivesResolveToo pins the
// same-disease arm (halt/resume messenger): directives addressed by
// phrase must land on the RAW id over V1 — a halt that 404s is a
// pause-must-pause violation (#1747).
func TestBoardWorkerComms_PhraseResolution_DirectivesResolveToo(t *testing.T) {
	stub := newStubRealOpencode(t, implRaw)
	adapter := newBoardCommsAdapter(t, stub.Server)
	registry := &stubPhraseRegistry{byPhrase: map[string]string{implPhrase: implRaw}}

	messenger := newBoardMessenger((&stubScope{}).resolve, newBoardResolvingSender(adapter, registry.lookup))

	require.NoError(t, messenger(context.Background(), implPhrase, "halt", phraseShort))
	require.Eventually(t, func() bool {
		posted, _, _, _ := stub.snapshot()
		return len(posted) >= 1
	}, 5*time.Second, 20*time.Millisecond, "the halt directive must be delivered")

	require.NoError(t, messenger(context.Background(), implPhrase, "resume", phraseShort))
	require.Eventually(t, func() bool {
		posted, _, _, _ := stub.snapshot()
		return len(posted) >= 2
	}, 5*time.Second, 20*time.Millisecond, "the resume directive must be delivered")

	posted, _, texts, _ := stub.snapshot()
	require.Len(t, posted, 2, "both directives must land on the LIVE raw session")
	assert.Equal(t, []string{implRaw, implRaw}, posted,
		"every directive must be posted to the RAW id — the phrase never reaches the route")
	require.Len(t, texts, 2)
	assert.Contains(t, texts[0], "BOARD PAUSE DIRECTIVE")
	assert.Contains(t, texts[1], "BOARD RESUME DIRECTIVE")
}

// TestBoardWorkerComms_PhraseResolution_AborterResolvesToo pins the
// interrupt leg's resolution (the aborter rides the same raw-keyed V1
// route): an abort addressed by the stored phrase must hit the RAW id —
// a verbatim-phrase abort 404s and the in-flight claim's interrupt
// silently never lands.
func TestBoardWorkerComms_PhraseResolution_AborterResolvesToo(t *testing.T) {
	stub := newStubRealOpencode(t, implRaw)
	adapter := newBoardCommsAdapter(t, stub.Server)
	registry := &stubPhraseRegistry{byPhrase: map[string]string{implPhrase: implRaw}}

	aborter := newBoardAborter((&stubScope{}).resolve, newBoardResolvingSender(adapter, registry.lookup))

	require.NoError(t, aborter(context.Background(), phraseShort, implPhrase))
	_, aborted, _, _ := stub.snapshot()
	require.Len(t, aborted, 1, "the abort must land on the live raw session")
	assert.Equal(t, implRaw, aborted[0], "the abort must hit the RAW id, never the phrase")
}

// TestBoardWorkerComms_PhraseResolution_RawRefsStillDeliver pins the
// regression guard: canonical RAW refs (the #1922 stored-rows-canonical
// posture) pass through untouched — resolution must never make a raw-
// addressed session unaddressable (the #1663 lesson), and the phrase
// registry is not consulted for raw-shaped refs.
func TestBoardWorkerComms_PhraseResolution_RawRefsStillDeliver(t *testing.T) {
	stub := newStubRealOpencode(t, implRaw)
	adapter := newBoardCommsAdapter(t, stub.Server)
	registry := &stubPhraseRegistry{}

	redispatch := newBoardRedispatcher((&stubScope{}).resolve, newBoardResolvingSender(adapter, registry.lookup))

	err := redispatch(context.Background(), implRaw, redispatchText, phraseShort)
	require.NoError(t, err, "a raw-addressed live session must deliver exactly as before")
	posted, _, _, _ := stub.snapshot()
	require.Len(t, posted, 1)
	assert.Equal(t, implRaw, posted[0])
	assert.Empty(t, registry.recorded(),
		"raw-shaped refs must not consult the phrase registry (passthrough is free)")
}
