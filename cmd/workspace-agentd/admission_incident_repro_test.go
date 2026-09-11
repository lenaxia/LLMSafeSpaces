// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

// admission_incident_repro_test.go — the #1315 incident repro (epic-71 /
// 0a): the ses_f73747f8 signature, miniature and deterministic. A
// synchronous V1 turn that hangs past the admitter ctx must yield
// exactly ONE harness user message for the entry's lifetime — the store
// evidence short-circuit plus the keyed upsert close the re-admission
// loop that appended 16 transcript copies (~180s apart, attempts never
// incremented) on 2026-09-10.
//
// Fidelity notes (Rule 7, validated):
//   - The harness fake reproduces the G1-probed opencode 1.18.15
//     semantics: it validates the msg-prefixed messageID, records it as
//     the user message's store ID, and never responds until released
//     (the hung external_directory ask turn).
//   - The StoreReader reads the SAME recorded store — production's
//     MessagePresence pages the harness transcript; here the coupling is
//     direct.
//   - AdmitterTimeout shrinks the 3-minute production ctx so the ladder
//     exhausts in test time; everything else (driver, ladder, evidence
//     check, ledger) is the production path.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"

	"github.com/lenaxia/llmsafespaces/cmd/workspace-agentd/sessionstate"
	abiv1 "github.com/lenaxia/llmsafespaces/pkg/abi/v1"
	abiconnect "github.com/lenaxia/llmsafespaces/pkg/abi/v1/abiconnect"
)

// hungHarness is the incident's harness: message POSTs are accepted and
// appended to the transcript, the turn never completes. Implements
// sessionstate.StoreReader over the same transcript (evidence).
type hungHarness struct {
	mu      sync.Mutex
	posts   []harnessPost // the transcript appends — one per accepted POST
	release chan struct{}
	srv     *httptest.Server
}

type harnessPost struct {
	sessionID  string
	messageID  string
	badIDShape bool
}

func newHungHarness(t *testing.T) *hungHarness {
	t.Helper()
	h := &hungHarness{release: make(chan struct{})}
	h.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == fmt.Sprintf("/session/%s/message", "ses_incident") {
			var body struct {
				MessageID string `json:"messageID"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			h.mu.Lock()
			h.posts = append(h.posts, harnessPost{
				sessionID:  "ses_incident",
				messageID:  body.MessageID,
				badIDShape: len(body.MessageID) < 4 || body.MessageID[:4] != "msg_",
			})
			h.mu.Unlock()
			// The hung turn: never respond. The client ctx aborts — the
			// incident's aborted-POST shape (the message IS in the
			// transcript).
			<-h.release
		}
	}))
	t.Cleanup(func() {
		close(h.release) // unblock any hanging handler on shutdown
		h.srv.Close()
	})
	return h
}

func (h *hungHarness) url() string { return h.srv.URL }

func (h *hungHarness) transcriptCopies() []harnessPost {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]harnessPost, len(h.posts))
	copy(out, h.posts)
	return out
}

func (h *hungHarness) SessionStates(ctx context.Context) (map[string]sessionstate.SessionSeed, error) {
	return nil, nil
}

func (h *hungHarness) PendingInputs(ctx context.Context) (map[string][]*abiv1.InputRequest, error) {
	return map[string][]*abiv1.InputRequest{}, nil
}

func (h *hungHarness) MessagePresence(ctx context.Context, sessionID string, messageIDs []string) (map[string]bool, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	present := map[string]bool{}
	for _, id := range messageIDs {
		for _, p := range h.posts {
			if p.sessionID == sessionID && p.messageID == id {
				present[id] = true
				break
			}
		}
	}
	return present, nil
}

// incidentAuthedClient mirrors sessionstate_test's authedClient (the ABI
// is Basic-authed; package main cannot import the external test helper).
func incidentAuthedClient(url, password string) abiconnect.HarnessABIServiceClient {
	tr := &incidentTransport{password: password}
	return abiconnect.NewHarnessABIServiceClient(&http.Client{Transport: tr}, url)
}

type incidentTransport struct{ password string }

func (t *incidentTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	if t.password != "" {
		r.SetBasicAuth("opencode", t.password)
	}
	return http.DefaultTransport.RoundTrip(r)
}

// TestAdmissionIncidentRepro_HungTurnExactlyOneTranscriptMessage: the
// 16-copy signature cannot recur — through the full production path
// (wire Deliver → ledger → driver ladder → real opencodeAdmitter POST →
// hung harness), the entry's transcript cardinality is exactly one and
// the row resolves ADMITTED-by-evidence despite the turn never
// completing. Pre-fix behavior (the incident): one transcript append per
// ladder attempt, then per re-arm — the mechanism-level reds are pinned
// in sessionstate.TestDeliver_EvidenceShortCircuitsReAdmission.
// noEventsParser is the minimal EventParser for the repro: no event
// stream is connected in this test, so every parse is a non-event.
type noEventsParser struct{}

func (noEventsParser) Parse(raw []byte) (*abiv1.Event, bool, error) { return nil, false, nil }

func TestAdmissionIncidentRepro_HungTurnExactlyOneTranscriptMessage(t *testing.T) {
	harness := newHungHarness(t)
	orig := agentAddrAtomic.Load()
	defer agentAddrAtomic.Store(orig)
	agentAddrAtomic.Store(harness.url())

	a, err := sessionstate.New(sessionstate.Config{
		PlatformDir:     t.TempDir(),
		Parser:          noEventsParser{},
		Passwords:       []string{"pw"},
		Admitter:        opencodeAdmitter{password: "harness-pw"},
		Store:           harness,
		AdmitterTimeout: 150 * time.Millisecond, // the hung-turn ctx, shrunk
		FastCursor:      true,
	})
	requireNoError(t, err)
	t.Cleanup(func() { _ = a.Close() })
	_, h := a.Handler()
	ts := httptest.NewServer(h)
	t.Cleanup(ts.Close)
	c := incidentAuthedClient(ts.URL, "pw")

	start := time.Now()
	_, err = c.Deliver(context.Background(), connect.NewRequest(&abiv1.DeliveryRequest{
		SessionId: "ses_incident",
		EntryId:   "ob_1789066979866284129_d691d25d",
		Attempt:   1,
		Parts:     []*abiv1.DeliveryPart{{Part: &abiv1.DeliveryPart_Text{Text: "proceed with the split and improvement"}}},
	}))
	if err != nil {
		t.Fatalf("deliver (the ack): %v", err)
	}

	// The ladder: 5 attempts × 150ms hung ctx + sub-second backoffs. The
	// first POST lands; every subsequent attempt must short-circuit on
	// evidence. Wait past the full ladder budget.
	deadline := time.Now().Add(10 * time.Second)
	for {
		copies := harness.transcriptCopies()
		if len(copies) > 1 {
			t.Fatalf("INCIDENT RECURRED: %d transcript copies for one entry (the 16-copy class)", len(copies))
		}
		if len(copies) == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("first POST never landed in %s", time.Since(start))
		}
		time.Sleep(50 * time.Millisecond)
	}
	// Give any would-be second attempt (ladder/backoff) time to fire and
	// be caught — the evidence short-circuit must hold across the whole
	// ladder window.
	time.Sleep(1500 * time.Millisecond)
	copies := harness.transcriptCopies()
	if len(copies) != 1 {
		t.Fatalf("transcript cardinality = %d, want exactly 1 for the entry's lifetime (S2)", len(copies))
	}
	if copies[0].badIDShape {
		t.Fatalf("harness write carried a non-msg-prefixed key: %q", copies[0].messageID)
	}
	wantKey := "msg_ob_1789066979866284129_d691d25d"
	if copies[0].messageID != wantKey {
		t.Fatalf("harness write key = %q, want the entry-derived %q", copies[0].messageID, wantKey)
	}

	// The ledger must have resolved ADMITTED-by-evidence (the turn never
	// completed — evidence is the only admission signal).
	statusDeadline := time.Now().Add(10 * time.Second)
	for {
		resp, err := c.GetDeliveryStatus(context.Background(), connect.NewRequest(&abiv1.GetDeliveryStatusRequest{
			EntryId: "ob_1789066979866284129_d691d25d", Attempt: 1,
		}))
		if err == nil && resp.Msg.GetState() == abiv1.LedgerState_LEDGER_STATE_ADMITTED {
			break
		}
		if time.Now().After(statusDeadline) {
			st := "nil"
			if resp != nil && resp.Msg != nil {
				st = resp.Msg.GetState().String()
			}
			t.Fatalf("row never resolved ADMITTED-by-evidence (state=%s, err=%v) — the ledger must learn the write from the store", st, err)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func requireNoError(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}
