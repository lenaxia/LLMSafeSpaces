// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package abitest

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	abiclient "github.com/lenaxia/llmsafespaces/pkg/abi/abiclient"
	abiv1 "github.com/lenaxia/llmsafespaces/pkg/abi/v1"
)

// The epic-71 / 2b fault-matrix knobs (#1312 change item 1, legs 7/8/10):
// every knob composable with the others, state inspectable for
// assertions, wire behavior pinned against the REAL connect client —
// the #1308 discipline (pins reproduce the production failure shape).

// --- leg 7: duplicate / out-of-order Deliver (S2/S9 assertions) --------------

// TestRecordDeliverCalls_OffByDefault: no recording until armed — the
// reference server stays inert for every existing consumer.
func TestRecordDeliverCalls_OffByDefault(t *testing.T) {
	srv := New()
	ctx := context.Background()
	for i, attempt := range []uint32{1, 1, 2} {
		_, err := srv.Deliver(ctx, connect.NewRequest(&abiv1.DeliveryRequest{
			SessionId: "s1", EntryId: "e-1", Attempt: attempt,
			Parts: []*abiv1.DeliveryPart{{Part: &abiv1.DeliveryPart_Text{Text: "hi"}}},
		}))
		require.NoError(t, err, "delivery %d", i)
	}
	assert.Empty(t, srv.DeliverCalls(), "no recording until RecordDeliverCalls arms it")
}

// TestRecordDeliverCalls_RecordsReplayOrder: the call SEQUENCE is the
// assertion surface for leg 7 — duplicates and out-of-order attempts
// must be observable exactly as the driver replayed them.
func TestRecordDeliverCalls_RecordsReplayOrder(t *testing.T) {
	srv := New()
	srv.RecordDeliverCalls()
	ctx := context.Background()
	deliver := func(entry string, attempt uint32) {
		_, err := srv.Deliver(ctx, connect.NewRequest(&abiv1.DeliveryRequest{
			SessionId: "s1", EntryId: entry, Attempt: attempt,
			Parts: []*abiv1.DeliveryPart{{Part: &abiv1.DeliveryPart_Text{Text: "hi"}}},
		}))
		require.NoError(t, err)
	}
	deliver("e-1", 1)
	deliver("e-1", 1) // duplicate
	deliver("e-1", 2)
	deliver("e-0", 1) // out-of-order across entries

	got := srv.DeliverCalls()
	require.Len(t, got, 4)
	assert.Equal(t, DeliveryCall{EntryID: "e-1", Attempt: 1}, got[0])
	assert.Equal(t, DeliveryCall{EntryID: "e-1", Attempt: 1}, got[1], "duplicates are recorded, not deduped — replay order IS the signal")
	assert.Equal(t, DeliveryCall{EntryID: "e-1", Attempt: 2}, got[2])
	assert.Equal(t, DeliveryCall{EntryID: "e-0", Attempt: 1}, got[3])
}

// --- leg 8: boundary slowness (slow Deliver ack vs the poll window) -----------

// TestDelayDeliverAck_StallsOnlyDeliver: the ack stalls for the armed
// duration (ctx-cancelable), other ops stay instant.
func TestDelayDeliverAck_StallsOnlyDeliver(t *testing.T) {
	srv := New()
	srv.DelayDeliverAck(150 * time.Millisecond)
	assert.Equal(t, 150*time.Millisecond, srv.DeliverAckDelay(), "knob state inspectable")

	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	cl := abiclient.New(http.DefaultClient, ts.URL)
	ctx := context.Background()

	start := time.Now()
	_, err := cl.Act(ctx, &abiv1.ActionRequest{
		SessionId: "s1",
		Action:    &abiv1.ActionRequest_Interrupt{Interrupt: &abiv1.InterruptAction{}},
	})
	require.NoError(t, err)
	assert.Less(t, time.Since(start), 100*time.Millisecond, "Act stays instant")

	start = time.Now()
	_, err = cl.Deliver(ctx, &abiv1.DeliveryRequest{
		SessionId: "s1", EntryId: "e-1", Attempt: 1,
		Parts: []*abiv1.DeliveryPart{{Part: &abiv1.DeliveryPart_Text{Text: "hi"}}},
	})
	require.NoError(t, err)
	elapsed := time.Since(start)
	assert.GreaterOrEqual(t, elapsed, 140*time.Millisecond, "the Deliver ack stalls for the armed window (boundary slowness)")
	assert.Less(t, elapsed, 3*time.Second, "but it completes — a slow boundary, not a dead one")
}

// TestDelayDeliverAck_ZeroDisarms: setting 0 clears the knob (the
// composability contract — knobs are releasable, not sticky).
func TestDelayDeliverAck_ZeroDisarms(t *testing.T) {
	srv := New()
	srv.DelayDeliverAck(time.Minute)
	srv.DelayDeliverAck(0)
	assert.Equal(t, time.Duration(0), srv.DeliverAckDelay())
}

// --- leg 10: response-shape corruption (the #1308 wire-drift class) -----------

// corruptWire drives the real HTTP surface with the corruption armed.
func corruptWire(t *testing.T, srv *Server, procedureSuffix string, mode CorruptMode) (int, string) {
	t.Helper()
	srv.CorruptNextResponse(procedureSuffix, mode)
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)

	req, err := http.NewRequest(http.MethodPost, ts.URL+"/llmsafespaces.abi.v1.HarnessABIService/"+procedureSuffix,
		strings.NewReader(`{"sessionId":"s1"}`))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")
	res, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer func() { _ = res.Body.Close() }()
	body, err := io.ReadAll(res.Body)
	require.NoError(t, err)
	return res.StatusCode, string(body)
}

// TestCorruptNextResponse_OneShotModes: every mode returns HTTP 200 with
// SCHEMA-VALID protocol but corrupted payload bytes — the exact class
// that defeats hand-adjacent parsers while passing transport layers.
// The corruption is ONE-SHOT: the next identical call succeeds.
func TestCorruptNextResponse_OneShotModes(t *testing.T) {
	for _, tc := range []struct {
		mode     CorruptMode
		contains string
	}{
		{CorruptInvalidJSON, `"sessionId":"s1"`},
		{CorruptTrailingGarbage, "trailing-garbage"},
		{CorruptEmptyBody, ""},
		{CorruptHTMLErrorPage, "<html>"},
	} {
		t.Run(tc.mode.String(), func(t *testing.T) {
			srv := New()
			code, body := corruptWire(t, srv, "GetSnapshot", tc.mode)
			assert.Equal(t, http.StatusOK, code, "the corruption rides a 200 — transport sees success")
			assert.Contains(t, body, tc.contains)

			assert.False(t, srv.CorruptNextResponseArmed(), "one-shot consumed")

			// The next call is clean.
			ts := httptest.NewServer(srv.Handler())
			t.Cleanup(ts.Close)
			cl := abiclient.New(http.DefaultClient, ts.URL)
			snap, err := cl.GetSnapshot(context.Background(), "s1")
			require.NoError(t, err, "corruption is one-shot — the next call parses cleanly")
			assert.Equal(t, "s1", snap.GetSessionId())
		})
	}
}

// TestCorruptNextResponse_ProcedureScoped: an armed corruption for one
// procedure never bleeds into others.
func TestCorruptNextResponse_ProcedureScoped(t *testing.T) {
	srv := New()
	srv.CorruptNextResponse("Deliver", CorruptInvalidJSON)
	assert.True(t, srv.CorruptNextResponseArmed())

	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	cl := abiclient.New(http.DefaultClient, ts.URL)

	snap, err := cl.GetSnapshot(context.Background(), "s1")
	require.NoError(t, err, "GetSnapshot is untouched by a Deliver-scoped corruption")
	assert.Equal(t, "s1", snap.GetSessionId())
	assert.True(t, srv.CorruptNextResponseArmed(), "still armed for Deliver")
}

// TestCorruptInvalidJSON_DefeatsTypedParse: the canonical #1308 pin —
// the REAL generated client fails to parse the corrupted response
// (reproducing the production failure class against the reference fix).
func TestCorruptInvalidJSON_DefeatsTypedParse(t *testing.T) {
	srv := New()
	srv.CorruptNextResponse("GetSnapshot", CorruptInvalidJSON)
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	cl := abiclient.New(http.DefaultClient, ts.URL)

	_, err := cl.GetSnapshot(context.Background(), "s1")
	require.Error(t, err, "truncated JSON must defeat the typed parse — this is the pin the #1308 pattern exists for")

	// And the corrupted body is JSON-ish but incomplete.
	var probe map[string]json.RawMessage
	if jsonErr := json.Unmarshal([]byte(`{"sessionId":"s1"`), &probe); err == nil && jsonErr == nil {
		_ = probe // shape sanity only; the truncation is asserted by the wire test above
	}
}

// TestKnobs_Composable: leg 8's stall and leg 7's recording arm together
// without interference — the composability contract every knob must hold.
func TestKnobs_Composable(t *testing.T) {
	srv := New()
	srv.RecordDeliverCalls()
	srv.DelayDeliverAck(50 * time.Millisecond)

	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	cl := abiclient.New(http.DefaultClient, ts.URL)

	start := time.Now()
	_, err := cl.Deliver(context.Background(), &abiv1.DeliveryRequest{
		SessionId: "s1", EntryId: "e-1", Attempt: 1,
		Parts: []*abiv1.DeliveryPart{{Part: &abiv1.DeliveryPart_Text{Text: "hi"}}},
	})
	require.NoError(t, err)
	assert.GreaterOrEqual(t, time.Since(start), 45*time.Millisecond)
	require.Len(t, srv.DeliverCalls(), 1, "the stalled call is still recorded exactly once")
	assert.Equal(t, DeliveryCall{EntryID: "e-1", Attempt: 1}, srv.DeliverCalls()[0])
}
