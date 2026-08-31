// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package handlers

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/lenaxia/llmsafespaces/api/internal/services/outbox"
	abiv1 "github.com/lenaxia/llmsafespaces/pkg/abi/v1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// --- US-69.8: the outbox terminus switch (design 0055 M2/M4 + D1-B) --------
//
// Delivery = POST/poll against the agentd ledger; the text-scan oracle is
// bypassed entirely on this path (its disposition: deleted on this terminus
// — the ledger IS the oracle). I10: outbox completion ⟺ ledger `admitted`.

func rowKey(entryID string, attempt uint32) string {
	return entryID + "|" + fmt.Sprint(attempt)
}

func decodeJSONBody(r *http.Request, v any) error {
	return json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(v)
}

// writeJSONBody emits the Connect-protocol JSON success envelope the
// generated clients decode: {"message": {...}}.
func writeJSONBody(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"message": v})
}

// writeJSONErr emits the Connect error envelope: {"error": {code,message}}.
func writeJSONErr(w http.ResponseWriter, code, message string) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]string{"code": code, "message": message}})
}

// ledgerStub stands in for the pod's ABI surface: a real connect handler
// backed by an in-process ledger+driver with a scriptable admitter.
type ledgerStub struct {
	server *httptest.Server
	admit  *scriptAdmitter
	muxx   sync.Mutex
	rows   map[string]string
}

type scriptAdmitter struct {
	mu    sync.Mutex
	calls int
	failN int // first failN admissions error
}

func (s *scriptAdmitter) Admit(ctx context.Context, sessionID, text, model string) (string, error) {
	s.mu.Lock()
	s.calls++
	n := s.calls
	failN := s.failN
	s.mu.Unlock()
	if n <= failN {
		return "", errString("admission refused")
	}
	return "stub-msg", nil
}

type errString string

func (e errString) Error() string { return string(e) }

func newLedgerStub(t *testing.T, failN int) *ledgerStub {
	t.Helper()
	admit := &scriptAdmitter{failN: failN}
	stub := &ledgerStub{admit: admit, rows: map[string]string{}}
	stub.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Minimal Connect-protocol JSON transport for the two ops.
		if r.Header.Get("Authorization") == "" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		switch r.URL.Path {
		case "/llmsafespaces.abi.v1.HarnessABIService/Deliver":
			var req struct {
				SessionId string          `json:"sessionId"`
				EntryId   string          `json:"entryId"`
				Attempt   uint32          `json:"attempt"`
				Parts     []*simplePart   `json:"parts"`
				Model     *abiv1.ModelRef `json:"model"`
			}
			_ = decodeJSONBody(r, &req)
			stub.muxx.Lock()
			key := rowKey(req.EntryId, req.Attempt)
			state, seen := stub.rows[key]
			if !seen {
				stub.rows[key] = "ledgered"
				state = "ledgered"
				go stub.driveAdmission(req.EntryId, req.Attempt)
			}
			stub.muxx.Unlock()
			writeJSONBody(w, map[string]any{
				"entryId": req.EntryId, "attempt": req.Attempt,
				"state": stubStates[state],
			})
		case "/llmsafespaces.abi.v1.HarnessABIService/GetDeliveryStatus":
			var req struct {
				EntryId string `json:"entryId"`
				Attempt uint32 `json:"attempt"`
			}
			_ = decodeJSONBody(r, &req)
			stub.muxx.Lock()
			state, ok := stub.rows[rowKey(req.EntryId, req.Attempt)]
			stub.muxx.Unlock()
			if !ok {
				writeJSONErr(w, "not_found", "no ledger row for (entry, attempt)")
				return
			}
			writeJSONBody(w, map[string]any{
				"entryId": req.EntryId, "attempt": req.Attempt,
				"state": stubStates[state],
			})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(stub.server.Close)
	return stub
}

type simplePart struct {
	Text string `json:"text"`
}

// stubStates maps the stub's internal row states to the frozen ABI enum
// names (the wire form) — shared by both handlers so the Deliver ack and
// the status poll can never disagree.
var stubStates = map[string]string{
	"ledgered":   ledgerStateLedgered,
	"admitted":   ledgerStateAdmitted,
	"failed":     ledgerStateFailed,
	"promoted":   ledgerStatePromoted,
	"turn-ended": ledgerStateTurnEnded,
	"stalled":    ledgerStateStalled,
}

// rowState returns the wire state for a stub row ("" if unknown — itself
// the unknown-state shape the guard test asserts never completes).
func (s *ledgerStub) rowState(key string) string {
	s.muxx.Lock()
	defer s.muxx.Unlock()
	return s.rows[key]
}

// driveAdmission simulates the agentd-side async admission driver: N
// failures then admitted, with a small delay so the inline poll exercises
// its window.
//
//nolint:contextcheck // stub: no caller context exists (simulates the agentd-side goroutine).
func (s *ledgerStub) driveAdmission(entryID string, attempt uint32) {
	time.Sleep(30 * time.Millisecond)
	if _, err := s.admit.Admit(context.TODO(), "s1", "", ""); err != nil { //nolint:contextcheck // stub goroutine
		s.muxx.Lock()
		s.rows[rowKey(entryID, attempt)] = "failed"
		s.muxx.Unlock()
		return
	}
	s.muxx.Lock()
	s.rows[rowKey(entryID, attempt)] = "admitted"
	s.muxx.Unlock()
}

// TestAgentdDeliver_InlineFirstAdmission (D1-B + I10): the terminus POSTs
// (entryID, attempt), polls to `admitted` within the inline window, and
// returns success — completion ⟺ admitted, no text-scan oracle.
func TestAgentdDeliver_InlineFirstAdmission(t *testing.T) {
	stub := newLedgerStub(t, 0)
	d := &agentdDeliverer{
		baseURL: stub.server.URL,
		client:  &http.Client{},
		resolve: func(ctx context.Context, workspaceID, sessionID string) (string, string, error) {
			return stub.server.URL, "pw", nil
		},
		inlineWindow: 2 * time.Second,
		pollEvery:    10 * time.Millisecond,
	}
	err := d.deliver(context.Background(), "ws1", "s1", outbox.Entry{ID: "e-1", Text: "hello"})
	require.NoError(t, err)
	stub.muxx.Lock()
	require.Equal(t, "admitted", stub.rows[rowKey("e-1", 1)])
	stub.muxx.Unlock()
	stub.admit.mu.Lock()
	calls := stub.admit.calls
	stub.admit.mu.Unlock()
	require.Equal(t, 1, calls, "exactly one admission")
}

// TestAgentdDeliver_RetryChecksPriorAttemptFirst (I10 + I6): on an outbox
// retry the deliverer resolves the PRIOR attempt's ledger state BEFORE
// re-POSTing — admitted/promoted/turn-ended/stalled complete without a
// second POST; only a failed prior attempt re-arms at attempt+1.
func TestAgentdDeliver_RetryChecksPriorAttemptFirst(t *testing.T) {
	stub := newLedgerStub(t, 0)
	stub.rows[rowKey("e-1", 1)] = "admitted" // prior attempt already admitted
	d := &agentdDeliverer{
		baseURL: stub.server.URL,
		client:  &http.Client{},
		resolve: func(ctx context.Context, workspaceID, sessionID string) (string, string, error) {
			return stub.server.URL, "pw", nil
		},
		inlineWindow: 2 * time.Second,
		pollEvery:    10 * time.Millisecond,
	}
	// Attempts=1 means the prior POST happened (attempt 1) — the deliverer
	// must check attempt 1 and complete without re-POSTing.
	err := d.deliver(context.Background(), "ws1", "s1", outbox.Entry{ID: "e-1", Text: "hello", Attempts: 1})
	require.NoError(t, err, "admitted prior attempt completes — no second turn (I6)")
	stub.muxx.Lock()
	_, hasSecond := stub.rows[rowKey("e-1", 2)]
	stub.muxx.Unlock()
	assert.False(t, hasSecond, "no attempt-2 row: admitted is never re-admitted")
}

// TestAgentdDeliver_TimeoutIsLedgeredPoll: inline window expiry with the
// row still ledgered is NOT an error completion — the entry stays
// delivering (agentd's own retry loop owns admission); the deliverer
// returns a retryable error so the outbox re-enters and re-polls.
func TestAgentdDeliver_TimeoutIsLedgeredPoll(t *testing.T) {
	stub := newLedgerStub(t, 1<<30) // admissions always fail: row stays ledgered
	stub.muxx.Lock()
	stub.rows[rowKey("e-1", 1)] = "ledgered" // pre-seeded: prior attempt stuck
	stub.muxx.Unlock()
	d := &agentdDeliverer{
		baseURL: stub.server.URL,
		client:  &http.Client{},
		resolve: func(ctx context.Context, workspaceID, sessionID string) (string, string, error) {
			return stub.server.URL, "pw", nil
		},
		inlineWindow: 100 * time.Millisecond,
		pollEvery:    20 * time.Millisecond,
	}
	err := d.deliver(context.Background(), "ws1", "s1", outbox.Entry{ID: "e-1", Text: "hello"})
	require.Error(t, err, "not yet admitted: stays delivering, outbox retries")
	assert.False(t, isAmbiguous(err), "ledgered is NOT ambiguous — the ledger is the truth source")
}

// TestAgentdDeliver_FailedAttemptReArms (M2 table): a terminally failed
// attempt re-arms at attempt+1 (a NEW ledger row) and completes when that
// admits.
func TestAgentdDeliver_FailedAttemptReArms(t *testing.T) {
	stub := newLedgerStub(t, 0)
	stub.muxx.Lock()
	stub.rows[rowKey("e-1", 1)] = "failed"
	stub.muxx.Unlock()
	d := &agentdDeliverer{
		baseURL: stub.server.URL,
		client:  &http.Client{},
		resolve: func(ctx context.Context, workspaceID, sessionID string) (string, string, error) {
			return stub.server.URL, "pw", nil
		},
		inlineWindow: 2 * time.Second,
		pollEvery:    10 * time.Millisecond,
	}
	err := d.deliver(context.Background(), "ws1", "s1", outbox.Entry{ID: "e-1", Text: "hello", Attempts: 1})
	require.NoError(t, err)
	stub.muxx.Lock()
	assert.Equal(t, "admitted", stub.rows[rowKey("e-1", 2)], "attempt+1 re-armed and admitted")
	stub.muxx.Unlock()
}

// TestFlagMatrix_IllegalComboRejected (M4 + D4): AGENTD_STATE_AUTHORITY on
// with OPENCODE_V2_DELIVERY off is rejected at wiring time — a dual
// delivery regime is not maintained.
func TestFlagMatrix_IllegalComboRejected(t *testing.T) {
	err := ValidateDeliveryFlags(true, false)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "AGENTD_STATE_AUTHORITY")

	require.NoError(t, ValidateDeliveryFlags(true, true))
	require.NoError(t, ValidateDeliveryFlags(false, true))
	require.NoError(t, ValidateDeliveryFlags(false, false))
}

// TestStateMapping_Guard is the issue's `state_mapping_guard` (I10): the
// guard itself is tested, not the convention — (1) the consumer-side
// constants ARE the frozen ABI enum names, so schema drift breaks loudly
// here (D5 freeze makes the seven states exhaustive); (2) the I10 table
// completes on exactly the admission-implying states and NEVER on an
// unknown/future state; (3) the retry path consults that same table: a
// prior attempt observed at `promoted` (the inline window can span
// promotion — promoted strictly implies admission per the M2 table)
// completes without manufacturing an attempt-2 turn.
func TestStateMapping_Guard(t *testing.T) {
	// (1) wire-constant binding: any rename/reorder in the frozen enum
	// fails here instead of silently strand- or double-completing rows.
	assert.Equal(t, abiv1.LedgerState_LEDGER_STATE_UNSPECIFIED.String(), "LEDGER_STATE_UNSPECIFIED")
	assert.Equal(t, abiv1.LedgerState_LEDGER_STATE_LEDGERED.String(), ledgerStateLedgered)
	assert.Equal(t, abiv1.LedgerState_LEDGER_STATE_ADMITTED.String(), ledgerStateAdmitted)
	assert.Equal(t, abiv1.LedgerState_LEDGER_STATE_PROMOTED.String(), ledgerStatePromoted)
	assert.Equal(t, abiv1.LedgerState_LEDGER_STATE_TURN_ENDED.String(), ledgerStateTurnEnded)
	assert.Equal(t, abiv1.LedgerState_LEDGER_STATE_STALLED.String(), ledgerStateStalled)
	assert.Equal(t, abiv1.LedgerState_LEDGER_STATE_FAILED.String(), ledgerStateFailed)

	// (2) the I10 table, exhaustively: admitted is the terminal 0052
	// semantic; promoted/turn-ended/stalled strictly imply it; ledgered,
	// failed, unspecified, and anything unknown never complete.
	for state, want := range map[string]bool{
		ledgerStateAdmitted:        true,
		ledgerStatePromoted:        true,
		ledgerStateTurnEnded:       true,
		ledgerStateStalled:         true,
		ledgerStateLedgered:        false,
		ledgerStateFailed:          false,
		"LEDGER_STATE_UNSPECIFIED": false,
		"LEDGER_STATE_ANNULLED":    false, // a hypothetical future state: opt-in only
		"":                         false,
	} {
		got, gotState := completionFor(state)
		assert.Equal(t, want, got, "I10 mapping for %q", state)
		assert.Equal(t, state, gotState, "mapping never rewrites the state")
	}

	// (3) through the terminus: promoted-on-retry completes without a
	// second POST (no attempt-2 row) — the mapping is the one the
	// deliverer actually consults, and it cannot invent an alternate one.
	stub := newLedgerStub(t, 0)
	stub.muxx.Lock()
	stub.rows[rowKey("e-1", 1)] = "promoted" // prior attempt: promoted
	stub.muxx.Unlock()
	d := &agentdDeliverer{
		baseURL: stub.server.URL,
		client:  &http.Client{},
		resolve: func(ctx context.Context, workspaceID, sessionID string) (string, string, error) {
			return stub.server.URL, "pw", nil
		},
		inlineWindow: 2 * time.Second,
		pollEvery:    10 * time.Millisecond,
	}
	err := d.deliver(context.Background(), "ws1", "s1", outbox.Entry{ID: "e-1", Text: "hello", Attempts: 1})
	require.NoError(t, err, "promoted implies admitted — completes (no live-lock across promotion)")
	assert.Equal(t, "promoted", stub.rowState(rowKey("e-1", 1)), "prior row untouched")
	assert.Empty(t, stub.rowState(rowKey("e-1", 2)), "no attempt-2 row: the mapping table is the single completion authority")
}
