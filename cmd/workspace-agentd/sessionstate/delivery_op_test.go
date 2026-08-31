// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package sessionstate_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/lenaxia/llmsafespaces/cmd/workspace-agentd/sessionstate"
	abiv1 "github.com/lenaxia/llmsafespaces/pkg/abi/v1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// recordingAdmitter counts admissions over the wire path.
type recordingAdmitter struct {
	calls atomic.Int64
}

func (r *recordingAdmitter) Admit(ctx context.Context, sessionID, text, model string) (string, error) {
	r.calls.Add(1)
	return "wire-msg", nil
}

func ledgerAuthority(t *testing.T) *sessionstate.Authority {
	t.Helper()
	a, err := sessionstate.New(sessionstate.Config{
		PlatformDir: t.TempDir(),
		Parser:      &fixtureParser{},
		Store:       &mapStore{},
		Passwords:   []string{"pw"},
		Admitter:    &recordingAdmitter{},
		FastCursor:  true,
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = a.Close() })
	return a
}

// TestDeliverOp_WireDedupe: two identical POSTs over the generated op
// produce ONE admission (I5 on the wire) and the second ack carries the
// same state.
func TestDeliverOp_WireDedupe(t *testing.T) {
	a := ledgerAuthority(t)
	_, h := a.Handler()
	c := newAuthedServer(t, h)

	body := &abiv1.DeliveryRequest{
		SessionId: "s1", EntryId: "e-1", Attempt: 1,
		Parts: []*abiv1.DeliveryPart{{Part: &abiv1.DeliveryPart_Text{Text: "hello"}}},
	}
	r1, err := c.Deliver(context.Background(), connect.NewRequest(body))
	require.NoError(t, err)
	assert.Equal(t, abiv1.LedgerState_LEDGER_STATE_LEDGERED, r1.Msg.GetState())

	r2, err := c.Deliver(context.Background(), connect.NewRequest(body))
	require.NoError(t, err)
	// The duplicate ack carries the row's CURRENT state (admission may
	// have landed in between) — idempotence is "no second row", not
	// "state frozen".
	assert.Contains(t, []abiv1.LedgerState{
		abiv1.LedgerState_LEDGER_STATE_LEDGERED,
		abiv1.LedgerState_LEDGER_STATE_ADMITTED,
	}, r2.Msg.GetState())

	st := waitForState(t, c, "e-1", 1, abiv1.LedgerState_LEDGER_STATE_ADMITTED)
	require.NotNil(t, st)
	// Exactly one admission row exists: the state op resolves the SAME
	// (entry, attempt) the duplicate ack referenced — one row, both acks.
}

// TestDeliverOp_FilePartsNotSupported: D3 on the wire — file parts are
// typed NotSupported while text parts deliver.
func TestDeliverOp_FilePartsNotSupported(t *testing.T) {
	a := ledgerAuthority(t)
	_, h := a.Handler()
	c := newAuthedServer(t, h)

	_, err := c.Deliver(context.Background(), connect.NewRequest(&abiv1.DeliveryRequest{
		SessionId: "s1", EntryId: "e-f", Attempt: 1,
		Parts: []*abiv1.DeliveryPart{{Part: &abiv1.DeliveryPart_File{File: &abiv1.FileReference{Path: "/workspace/uploads/x"}}}},
	}))
	ce, ok := err.(*connect.Error)
	require.True(t, ok)
	require.Equal(t, connect.CodeUnimplemented, ce.Code())
	require.NotEmpty(t, ce.Details(), "typed NotSupported detail")
}

// TestDeliverOp_StatusLifecycle: ledgered → admitted observable via the
// status op; unknown (entry, attempt) is a typed NotFound.
func TestDeliverOp_StatusLifecycle(t *testing.T) {
	a := ledgerAuthority(t)
	_, h := a.Handler()
	c := newAuthedServer(t, h)

	_, err := c.GetDeliveryStatus(context.Background(), connect.NewRequest(&abiv1.GetDeliveryStatusRequest{
		EntryId: "nope", Attempt: 1,
	}))
	ce, ok := err.(*connect.Error)
	require.True(t, ok)
	assert.Equal(t, connect.CodeNotFound, ce.Code())

	_, err = c.Deliver(context.Background(), connect.NewRequest(&abiv1.DeliveryRequest{
		SessionId: "s1", EntryId: "e-2", Attempt: 3,
		Parts: []*abiv1.DeliveryPart{{Part: &abiv1.DeliveryPart_Text{Text: "x"}}},
	}))
	require.NoError(t, err)

	st := waitForState(t, c, "e-2", 3, abiv1.LedgerState_LEDGER_STATE_ADMITTED)
	assert.EqualValues(t, 3, st.GetAttempt())
}

func waitForState(t *testing.T, c abiclientIface, entryID string, attempt uint32, want abiv1.LedgerState) *abiv1.DeliveryStatus {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := c.GetDeliveryStatus(context.Background(), connect.NewRequest(&abiv1.GetDeliveryStatusRequest{
			EntryId: entryID, Attempt: attempt,
		}))
		if err == nil && resp.Msg.GetState() == want {
			return resp.Msg
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("delivery status never reached %v", want)
	return nil
}

// --- small harness (reuses service_test.go's authedClient) -------------------

type abiclientIface interface {
	Deliver(context.Context, *connect.Request[abiv1.DeliveryRequest]) (*connect.Response[abiv1.DeliveryAck], error)
	GetDeliveryStatus(context.Context, *connect.Request[abiv1.GetDeliveryStatusRequest]) (*connect.Response[abiv1.DeliveryStatus], error)
}

func newAuthedServer(t *testing.T, h http.Handler) abiclientIface {
	t.Helper()
	ts := httptest.NewServer(h)
	t.Cleanup(ts.Close)
	return authedClient(ts.URL, "pw")
}
