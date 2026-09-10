// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package handlers

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/go-redis/redis/v8"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/watch"
	k8sfake "k8s.io/client-go/kubernetes/fake"

	"github.com/lenaxia/llmsafespaces/api/internal/services/outbox"
	k8smocks "github.com/lenaxia/llmsafespaces/mocks/kubernetes"
	abiv1 "github.com/lenaxia/llmsafespaces/pkg/abi/v1"
	v1 "github.com/lenaxia/llmsafespaces/pkg/apis/llmsafespaces/v1"
)

// --- 0b (#1316) handler-side wiring -----------------------------------------
//
// The outbox package owns the sweeper decision table; this file proves
// the API-side halves: the LedgerProbe adapter (not_found → no-row),
// constant parity with the frozen ABI enum, the 2026-09-10 replay
// (completion without re-POST, S9), and the full Start() → phase-change
// wiring.

// newSweeperEnv builds a ProxyHandler whose agentdEndpoint resolves to
// the given stub (password via the fake clientset secret, port via the
// override), with the outbox service on miniredis.
func newSweeperEnv(t *testing.T, wsName string, stubURL string) (*ProxyHandler, *outbox.Service, *redis.Client) {
	t.Helper()

	k8sMock := k8smocks.NewMockKubernetesClient()
	llmMock := k8smocks.NewMockLLMSafespacesV1Interface()
	wsMock := k8smocks.NewMockWorkspaceInterface()
	k8sMock.On("LlmsafespacesV1").Return(llmMock, nil)
	llmMock.On("Workspaces", "default").Return(wsMock)
	fakeClientset := k8sfake.NewSimpleClientset()
	k8sMock.On("Clientset").Return(fakeClientset)

	podHost := strings.TrimPrefix(stubURL, "http://")
	port := 0
	if idx := strings.LastIndex(podHost, ":"); idx >= 0 {
		port, _ = strconv.Atoi(podHost[idx+1:])
		podHost = podHost[:idx]
	}
	ws := makeWorkspaceCRDWithStatus(wsName, podHost, string(v1.WorkspacePhaseActive), wsName)
	ws.Spec.Owner.UserID = "user-1"
	list := &v1.WorkspaceList{}
	list.Items = []v1.Workspace{*ws}
	wsMock.On("List", mock.Anything, mock.Anything).Return(list, nil).Maybe()
	wsMock.On("Watch", mock.Anything, mock.Anything).Return(watch.NewFake(), nil).Maybe()
	wsMock.On("Get", mock.Anything, wsName, mock.Anything).Return(ws, nil).Maybe()
	wsMock.On("Patch", mock.Anything, wsName, mock.Anything, mock.Anything, mock.Anything).Return(ws, nil).Maybe()

	_, err := fakeClientset.CoreV1().Secrets("default").Create(context.Background(),
		makePasswordSecret(wsName, "pw"), metav1.CreateOptions{})
	require.NoError(t, err)

	handler, err := NewProxyHandler(k8sMock, &testLogger{}, "default", &http.Client{}, nil)
	require.NoError(t, err)
	if port > 0 {
		handler.agentdPortOverride = port
	}

	mr, err := miniredis.Run()
	require.NoError(t, err)
	t.Cleanup(mr.Close)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	svc := outbox.New(rdb)
	handler.SetOutboxForTest(svc)
	return handler, svc, rdb
}

// seedParked pushes a status:error entry with the incident's shape:
// attempts exhausted on honest timeouts.
func seedParked(t *testing.T, svc *outbox.Service, ws, ses, id string, attempts int, lastErr string) {
	t.Helper()
	e := outbox.Entry{ID: id, ClientMessageID: "cmid-" + id, UserID: "u1",
		Text: "proceed with the split and improvement", AcceptedAt: time.Now().UTC(),
		Status: outbox.StatusError, Attempts: attempts, LastError: lastErr}
	raw, err := json.Marshal(e)
	require.NoError(t, err)
	require.NoError(t, svc.SeedEntryForTest(context.Background(), ws, ses, string(raw)))
}

func queueOf(t *testing.T, rdb *redis.Client, ws, ses string) []outbox.Entry {
	t.Helper()
	vals, err := rdb.LRange(context.Background(), "outboxq:"+ws+":"+ses, 0, -1).Result()
	require.NoError(t, err)
	out := make([]outbox.Entry, 0, len(vals))
	for _, v := range vals {
		var e outbox.Entry
		require.NoError(t, json.Unmarshal([]byte(v), &e))
		out = append(out, e)
	}
	return out
}

// TestLedgerStateConstants_PinnedToABIEnum: the outbox package's local
// copies ARE the frozen ABI enum names and the terminus's wire constants
// — any drift fails here instead of silently mis-dispositioning rows.
func TestLedgerStateConstants_PinnedToABIEnum(t *testing.T) {
	assert.Equal(t, abiv1.LedgerState_LEDGER_STATE_LEDGERED.String(), outbox.LedgerStateLedgered)
	assert.Equal(t, abiv1.LedgerState_LEDGER_STATE_ADMITTED.String(), outbox.LedgerStateAdmitted)
	assert.Equal(t, abiv1.LedgerState_LEDGER_STATE_PROMOTED.String(), outbox.LedgerStatePromoted)
	assert.Equal(t, abiv1.LedgerState_LEDGER_STATE_TURN_ENDED.String(), outbox.LedgerStateTurnEnded)
	assert.Equal(t, abiv1.LedgerState_LEDGER_STATE_STALLED.String(), outbox.LedgerStateStalled)
	assert.Equal(t, abiv1.LedgerState_LEDGER_STATE_FAILED.String(), outbox.LedgerStateFailed)

	assert.Equal(t, ledgerStateLedgered, outbox.LedgerStateLedgered)
	assert.Equal(t, ledgerStateAdmitted, outbox.LedgerStateAdmitted)
	assert.Equal(t, ledgerStatePromoted, outbox.LedgerStatePromoted)
	assert.Equal(t, ledgerStateTurnEnded, outbox.LedgerStateTurnEnded)
	assert.Equal(t, ledgerStateStalled, outbox.LedgerStateStalled)
	assert.Equal(t, ledgerStateFailed, outbox.LedgerStateFailed)
}

// TestOutboxLedgerProbe_Contract: the adapter maps the wire to the
// LedgerProbe contract — a row's state verbatim, not_found to ("", nil)
// (absence, not failure), transport failures as errors.
func TestOutboxLedgerProbe_Contract(t *testing.T) {
	stub := newLedgerStub(t, 0)
	stub.muxx.Lock()
	stub.rows[rowKey("e-live", 3)] = "admitted"
	stub.muxx.Unlock()
	handler, _, _ := newSweeperEnv(t, "ws-probe", stub.server.URL)

	state, err := handler.outboxLedgerProbe(context.Background(), "ws-probe", "ses", "e-live", 3)
	require.NoError(t, err)
	assert.Equal(t, ledgerStateAdmitted, state)

	state, err = handler.outboxLedgerProbe(context.Background(), "ws-probe", "ses", "e-absent", 7)
	require.NoError(t, err)
	assert.Empty(t, state, "not_found maps to no-row, not an error")

	handlerUnreachable, _, _ := newSweeperEnv(t, "ws-probe2", "http://127.0.0.1:1")
	_, err = handlerUnreachable.outboxLedgerProbe(context.Background(), "ws-probe2", "ses", "e-x", 1)
	require.Error(t, err, "transport failure surfaces as indeterminate")
}

// TestSweeperReplay_2026_09_10 is the #1316 integration replay: entries
// parked as error on honest `context deadline exceeded` timeouts while
// the ledger held them admitted — the sweeper completes them by
// prior-attempt lookup ONLY (the Deliver endpoint is never hit: S9, no
// re-POST), failed rows stay parked, and other workspaces are untouched.
func TestSweeperReplay_2026_09_10(t *testing.T) {
	stub := newLedgerStub(t, 1<<30) // admissions never drive: rows stay as seeded
	stub.muxx.Lock()
	stub.rows[rowKey("ob-adm", 5)] = "admitted"
	stub.rows[rowKey("ob-fail", 5)] = "failed"
	stub.muxx.Unlock()
	handler, svc, rdb := newSweeperEnv(t, "ws-replay", stub.server.URL)
	svc.SetLedgerProbe(handler.outboxLedgerProbe)
	var delivered atomic.Int32
	var deliveredIDs []string
	svc.SetOnDelivered(func(ws, ses string, e outbox.Entry) {
		delivered.Add(1)
		deliveredIDs = append(deliveredIDs, e.ID)
	})

	seedParked(t, svc, "ws-replay", "ses-1", "ob-adm", 5, "context deadline exceeded")
	seedParked(t, svc, "ws-replay", "ses-1", "ob-fail", 5, "context deadline exceeded")
	seedParked(t, svc, "ws-other", "ses-1", "ob-elsewhere", 5, "context deadline exceeded")

	n, err := svc.SweepWorkspaceParkedErrors(context.Background(), "ws-replay")
	require.NoError(t, err)
	assert.Equal(t, 1, n, "exactly the ledger-admitted entry recovers")

	remaining := queueOf(t, rdb, "ws-replay", "ses-1")
	require.Len(t, remaining, 1, "failed-and-exhausted stays parked (terminal; retry/dismiss is the user path)")
	assert.Equal(t, "ob-fail", remaining[0].ID)
	assert.Equal(t, outbox.StatusError, remaining[0].Status)
	other := queueOf(t, rdb, "ws-other", "ses-1")
	require.Len(t, other, 1, "other workspace untouched")
	assert.Equal(t, outbox.StatusError, other[0].Status)
	assert.Equal(t, int32(1), delivered.Load(), "one completion event (queue.update sent rides it)")
	assert.ElementsMatch(t, []string{"ob-adm"}, deliveredIDs)
	assert.Equal(t, int64(0), stub.deliverHits.Load(), "S9: recovery never re-POSTs an admitted row")
}

// TestSweeperE2E_PhaseChangeWiring: the full production wiring —
// Start() wires the probe iff the terminus regime is on, and the
// Active phase transition (a resume) triggers the workspace sweep that
// clears the stranded admission — the #1308 manual-recovery box,
// automated.
func TestSweeperE2E_PhaseChangeWiring(t *testing.T) {
	stub := newLedgerStub(t, 1<<30)
	stub.muxx.Lock()
	stub.rows[rowKey("ob-e2e", 5)] = "admitted"
	stub.muxx.Unlock()
	handler, svc, rdb := newSweeperEnv(t, "ws-swp70b", stub.server.URL)
	handler.SetAgentdTerminus(true)
	var delivered atomic.Int32
	svc.SetOnDelivered(func(ws, ses string, e outbox.Entry) { delivered.Add(1) })

	seedParked(t, svc, "ws-swp70b", "ses-1", "ob-e2e", 5, "context deadline exceeded")
	handler.SetPriorPhaseForTest("ws-swp70b", "Suspended") // make the Active event a real transition

	require.NoError(t, handler.Start())
	t.Cleanup(func() { _ = handler.Stop() })

	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if len(queueOf(t, rdb, "ws-swp70b", "ses-1")) == 0 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	assert.Empty(t, queueOf(t, rdb, "ws-swp70b", "ses-1"),
		"Start-seeded Active transition swept the stranded admission")
	assert.Equal(t, int32(1), delivered.Load())
	assert.Equal(t, int64(0), stub.deliverHits.Load(), "recovery is lookup-only")
}
