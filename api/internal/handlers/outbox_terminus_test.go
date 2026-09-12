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
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lenaxia/llmsafespaces/api/internal/services/outbox"
	"github.com/lenaxia/llmsafespaces/pkg/abi/abitest"
	abiv1 "github.com/lenaxia/llmsafespaces/pkg/abi/v1"
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

// writeJSONBody emits the Connect-protocol unary JSON success body: the
// response message itself, bare (connect-go's unary codec never wraps it
// in {"message": ...} — that is the streaming frame shape). Mirrors what
// abiconnect.NewHarnessABIServiceHandler emits; pinned against the real
// handler in TestAgentdDeliver_RealConnectHandler.
func writeJSONBody(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

// connectHTTPStatus maps connect codes to the HTTP statuses the real
// connect-go unary handler uses (connectrpc.com/docs/protocol#http-errors).
func connectHTTPStatus(code string) int {
	switch code {
	case "invalid_argument", "out_of_range":
		return http.StatusBadRequest
	case "unauthenticated":
		return http.StatusUnauthorized
	case "permission_denied":
		return http.StatusForbidden
	case "not_found":
		return http.StatusNotFound
	case "resource_exhausted":
		return http.StatusTooManyRequests
	case "unimplemented":
		return http.StatusNotImplemented
	default:
		return http.StatusInternalServerError
	}
}

// writeJSONErr emits the Connect unary error shape: HTTP status + bare
// {"code","message"} body.
func writeJSONErr(w http.ResponseWriter, code, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(connectHTTPStatus(code))
	_ = json.NewEncoder(w).Encode(map[string]string{"code": code, "message": message})
}

// ledgerStub stands in for the pod's ABI surface: a real connect handler
// backed by an in-process ledger+driver with a scriptable admitter.
type ledgerStub struct {
	server *httptest.Server
	admit  *scriptAdmitter
	muxx   sync.Mutex
	rows   map[string]string
	// deliverHits counts POSTs to the Deliver endpoint — the #1316
	// sweeper/guard tests assert re-POST never happens (S9).
	deliverHits atomic.Int64
	// statusHits counts GetDeliveryStatus polls (0b e2e diagnostics).
	statusHits atomic.Int64
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
			writeJSONErr(w, "unauthenticated", "missing basic credential")
			return
		}
		switch r.URL.Path {
		case "/llmsafespaces.abi.v1.HarnessABIService/Deliver":
			stub.deliverHits.Add(1)
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
			stub.statusHits.Add(1)
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

// TestAgentdDeliver_PriorLedgeredTimeoutIsPriorPending (#1316 review
// defect 2): the prior-attempt poll timeout drove NO new attempt — it
// must surface as outbox.PriorAttemptPending so the outbox neither
// mints an attempt number nor parks (a phantom attempt parked the entry
// against a row the sweeper could never find).
func TestAgentdDeliver_PriorLedgeredTimeoutIsPriorPending(t *testing.T) {
	stub := newLedgerStub(t, 1<<30)
	stub.muxx.Lock()
	stub.rows[rowKey("e-1", 2)] = "ledgered" // prior attempt (Attempts=2) LEDGERED
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
	err := d.deliver(context.Background(), "ws1", "s1", outbox.Entry{ID: "e-1", Text: "hello", Attempts: 2})
	require.Error(t, err)
	var pp *outbox.PriorAttemptPendingError
	require.ErrorAs(t, err, &pp, "prior-row poll timeout is PriorAttemptPending — no attempt minted")
	assert.Equal(t, int64(0), stub.deliverHits.Load(), "the prior branch never re-POSTs")
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

// --- Wire-shape pin: the REAL generated connect handler --------------------
//
// The stub above mimics the transport, but the terminus once shipped a
// {"message": ...} envelope parser that connect-go's unary codec never
// emits — every real Deliver ack failed as "empty message envelope" while
// the ledger (and the transcript) held a successful admission, stranding
// the outbox rows as error pills (production probe, 2026-09-10: 200
// {"entryId","attempt","state"}; 404 {"code":"not_found",...}). These
// tests drive the deliverer against the actual generated handler
// (abiconnect.NewHarnessABIServiceHandler over abitest.Server) so shape
// drift between the stub and the wire can never re-emerge.

func newRealABIStub(t *testing.T) *abitest.Server {
	t.Helper()
	return abitest.New()
}

// TestAgentdDeliver_RealConnectHandler: POST + poll against the real
// generated handler — the Deliver ack parses (no "empty message
// envelope"), and a row advanced to ADMITTED completes the terminus
// through the real status wire shape.
func TestAgentdDeliver_RealConnectHandler(t *testing.T) {
	abi := newRealABIStub(t)
	srv := httptest.NewServer(abi.Handler())
	t.Cleanup(srv.Close)

	// Admit the (entry, attempt) the moment the ledger row exists: swap
	// the deliverer's poll cadence for the server-side state advance.
	d := &agentdDeliverer{
		baseURL: srv.URL,
		client:  &http.Client{},
		resolve: func(ctx context.Context, workspaceID, sessionID string) (string, string, error) {
			return srv.URL, "pw", nil
		},
		inlineWindow: 2 * time.Second,
		pollEvery:    10 * time.Millisecond,
	}
	go func() {
		// The handler LEDGERs on Deliver; advance it shortly after.
		time.Sleep(20 * time.Millisecond)
		abi.SetDeliveryState("e-real-1", 1, abiv1.LedgerState_LEDGER_STATE_ADMITTED)
	}()
	err := d.deliver(context.Background(), "ws1", "sess-ref", outbox.Entry{ID: "e-real-1", Text: "hello"})
	require.NoError(t, err, "real unary ack must parse: LEDGERED poll -> ADMITTED completes")
}

// TestAgentdDeliver_RealConnectHandlerLedgeredTimesOut: with the row never
// advancing past LEDGERED, the terminus surfaces the retryable window
// timeout — parsed from the REAL wire shapes, never "empty message
// envelope".
func TestAgentdDeliver_RealConnectHandlerLedgeredTimesOut(t *testing.T) {
	abi := newRealABIStub(t)
	srv := httptest.NewServer(abi.Handler())
	t.Cleanup(srv.Close)

	d := &agentdDeliverer{
		baseURL: srv.URL,
		client:  &http.Client{},
		resolve: func(ctx context.Context, workspaceID, sessionID string) (string, string, error) {
			return srv.URL, "pw", nil
		},
		inlineWindow: 100 * time.Millisecond,
		pollEvery:    10 * time.Millisecond,
	}
	err := d.deliver(context.Background(), "ws1", "sess-ref", outbox.Entry{ID: "e-real-2", Text: "hello"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "ledgered but not admitted")
	assert.NotContains(t, err.Error(), "empty message envelope")
	_, retryable := err.(*retryableError)
	assert.True(t, retryable, "window timeout stays retryable")
}

// TestLedgerLookup_RealConnectHandlerNotFound: the real handler's 404 +
// bare {"code":"not_found"} body surfaces as a parseable error (the
// deliverer's retry path treats it as fall-through-to-re-POST).
func TestLedgerLookup_RealConnectHandlerNotFound(t *testing.T) {
	abi := newRealABIStub(t)
	srv := httptest.NewServer(abi.Handler())
	t.Cleanup(srv.Close)

	_, err := ledgerLookup(context.Background(), &http.Client{}, srv.URL, "pw", "ob_missing", 7)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not_found")
	assert.NotContains(t, err.Error(), "empty message envelope")
}

// TestAgentdDeliver_WireDriftCorruption (epic-71 / 2b, #1312 change
// item 4 — leg 10 at the terminus parse site): corrupted 200s (the four
// abitest modes) must surface as delivery ERRORS the ladder can retry —
// never a silent success and never a misparse into a phantom state.
// The #1308 class, pinned per mode via the leg-10 knob.
func TestAgentdDeliver_WireDriftCorruption(t *testing.T) {
	for _, mode := range []abitest.CorruptMode{
		abitest.CorruptInvalidJSON,
		abitest.CorruptTrailingGarbage,
		abitest.CorruptEmptyBody,
		abitest.CorruptHTMLErrorPage,
	} {
		t.Run(mode.String(), func(t *testing.T) {
			abi := newRealABIStub(t)
			abi.CorruptNextResponse("Deliver", mode)
			srv := httptest.NewServer(abi.Handler())
			t.Cleanup(srv.Close)

			d := &agentdDeliverer{
				baseURL: srv.URL,
				client:  &http.Client{},
				resolve: func(ctx context.Context, workspaceID, sessionID string) (string, string, error) {
					return srv.URL, "pw", nil
				},
				inlineWindow: 50 * time.Millisecond,
				pollEvery:    10 * time.Millisecond,
			}
			err := d.deliver(context.Background(), "ws1", "sess-ref", outbox.Entry{ID: "e-drift-1", Text: "hello"})
			require.Error(t, err, "a corrupted Deliver ack must fail the delivery, not complete it")
			// DISCRIMINATION (review r1): the failure must be the POST
			// parse, not the ledgered-window timeout a lenient parser
			// degrades into — positive: a json syntax failure; negative:
			// never the "agentd owns admission" sentinel. Under the
			// swallowed-Unmarshal mutation the error IS the window
			// sentinel and this pin fires.
			var se *json.SyntaxError
			require.ErrorAs(t, err, &se, "the corrupted 200 must fail AT THE PARSE (json.SyntaxError)")
			assert.NotContains(t, err.Error(), "agentd owns admission", "not the ledgered-window timeout shape")
		})
	}
}

// TestTerminus_StatusWireDrift_FailsOpenToKeyedRePOST (leg 10 at the
// third hand-adjacent procedure — GetDeliveryStatus, r1 finding 2): a
// corrupted status-200 on the retry path's prior-attempt lookup must
// degrade to a FRESH POST at attempt+1 (the fail-open direction, safe
// only because the harness write is keyed) — never a phantom completion
// and never a swallowed state. The re-POST is observable via the leg-7
// call recorder.
func TestTerminus_StatusWireDrift_FailsOpenToKeyedRePOST(t *testing.T) {
	for _, mode := range []abitest.CorruptMode{
		abitest.CorruptInvalidJSON,
		abitest.CorruptTrailingGarbage,
		abitest.CorruptEmptyBody,
		abitest.CorruptHTMLErrorPage,
	} {
		t.Run(mode.String(), func(t *testing.T) {
			abi := newRealABIStub(t)
			abi.RecordDeliverCalls()
			abi.SetDeliveryState("e-drift-2", 1, abiv1.LedgerState_LEDGER_STATE_LEDGERED) // the prior attempt
			abi.CorruptNextResponse("GetDeliveryStatus", mode)                            // the retry path's lookup
			srv := httptest.NewServer(abi.Handler())
			t.Cleanup(srv.Close)

			d := &agentdDeliverer{
				baseURL: srv.URL,
				client:  &http.Client{},
				resolve: func(ctx context.Context, workspaceID, sessionID string) (string, string, error) {
					return srv.URL, "pw", nil
				},
				inlineWindow: 150 * time.Millisecond,
				pollEvery:    10 * time.Millisecond,
			}
			// Advance the re-POSTed attempt 2 to ADMITTED shortly after
			// it lands (the one-shot corruption is consumed by the
			// lookup; the fresh POST's poll path reads clean status).
			go func() {
				time.Sleep(30 * time.Millisecond)
				abi.SetDeliveryState("e-drift-2", 2, abiv1.LedgerState_LEDGER_STATE_ADMITTED)
			}()
			err := d.deliver(context.Background(), "ws1", "sess-ref", outbox.Entry{ID: "e-drift-2", Text: "hello", Attempts: 1})
			require.NoError(t, err, "the fail-open re-POST completes the delivery")

			calls := abi.DeliverCalls()
			reposted := false
			for _, c := range calls {
				if c.EntryID == "e-drift-2" && c.Attempt == 2 {
					reposted = true
				}
			}
			assert.True(t, reposted, "the corrupted prior-attempt lookup degraded to a KEYED re-POST at attempt 2 (observable, not assumed)")
		})
	}
}
