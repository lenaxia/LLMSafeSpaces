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

// TestSweeperE2E_PhaseChangeTransitionSweeps: the Active phase
// transition itself (a resume) drives the workspace's parked-error
// sweep — proven WITHOUT Start()/Run so the periodic pass cannot
// confound the result (review finding: the previous e2e passed via
// Run's first tick while the transition path was dead behind the
// adapter gate). Also pins the gate semantics: terminus on, adapter
// absent — the transition sweep must still fire.
func TestSweeperE2E_PhaseChangeTransitionSweeps(t *testing.T) {
	stub := newLedgerStub(t, 1<<30)
	stub.muxx.Lock()
	stub.rows[rowKey("ob-tr", 5)] = "admitted"
	stub.muxx.Unlock()
	handler, svc, rdb := newSweeperEnv(t, "ws-swp70tr", stub.server.URL)
	handler.SetAgentdTerminus(true)
	// No Start() here (its Run loop would confound the periodic path);
	// wire the probe exactly as Start does — the Start wiring has its
	// own test below.
	svc.SetLedgerProbe(handler.outboxLedgerProbe)
	assert.Nil(t, handler.adapter, "gate pin: terminus without adapter must still sweep")
	var delivered atomic.Int32
	svc.SetOnDelivered(func(ws, ses string, e outbox.Entry) { delivered.Add(1) })

	seedParked(t, svc, "ws-swp70tr", "ses-1", "ob-tr", 5, "context deadline exceeded")
	handler.SetPriorPhaseForTest("ws-swp70tr", "Suspended") // real transition into Active
	handler.onPhaseChange(makeWorkspaceCRDWithStatus("ws-swp70tr", "127.0.0.1",
		string(v1.WorkspacePhaseActive), "ws-swp70tr"))

	waitFor(t, func() bool { return len(queueOf(t, rdb, "ws-swp70tr", "ses-1")) == 0 })
	assert.Empty(t, queueOf(t, rdb, "ws-swp70tr", "ses-1"), "the TRANSITION swept the stranded admission")
	assert.Equal(t, int32(1), delivered.Load())
	assert.Equal(t, int64(0), stub.deliverHits.Load(), "recovery is lookup-only")
}

// TestSweeperE2E_NoTransitionNoSweep: the control for the above — an
// activity-driven Active→Active update (no transition) must not sweep.
func TestSweeperE2E_NoTransitionNoSweep(t *testing.T) {
	stub := newLedgerStub(t, 1<<30)
	stub.muxx.Lock()
	stub.rows[rowKey("ob-nt", 5)] = "admitted"
	stub.muxx.Unlock()
	handler, svc, rdb := newSweeperEnv(t, "ws-swp70nt", stub.server.URL)
	handler.SetAgentdTerminus(true)

	seedParked(t, svc, "ws-swp70nt", "ses-1", "ob-nt", 5, "context deadline exceeded")
	handler.SetPriorPhaseForTest("ws-swp70nt", "Active") // no transition
	handler.onPhaseChange(makeWorkspaceCRDWithStatus("ws-swp70nt", "127.0.0.1",
		string(v1.WorkspacePhaseActive), "ws-swp70nt"))

	time.Sleep(300 * time.Millisecond) // the sweep is async — give a false positive room to fire
	assert.Len(t, queueOf(t, rdb, "ws-swp70nt", "ses-1"), 1, "no transition, no sweep")
	assert.Equal(t, int64(0), stub.statusHits.Load())
}

// TestSweeperE2E_StartWiresProbeAndRunSweeps: the Start() wiring —
// terminus on wires the probe (the Run loop's periodic pass recovers a
// parked entry with no phase change at all), and off wires nothing.
func TestSweeperE2E_StartWiresProbeAndRunSweeps(t *testing.T) {
	stub := newLedgerStub(t, 1<<30)
	stub.muxx.Lock()
	stub.rows[rowKey("ob-run", 5)] = "admitted"
	stub.muxx.Unlock()
	handler, svc, rdb := newSweeperEnv(t, "ws-swp70run", stub.server.URL)
	handler.SetAgentdTerminus(true)

	seedParked(t, svc, "ws-swp70run", "ses-1", "ob-run", 5, "context deadline exceeded")
	handler.SetPriorPhaseForTest("ws-swp70run", "Active") // no transition: only the periodic pass can recover

	require.NoError(t, handler.Start())
	t.Cleanup(func() { _ = handler.Stop() })

	waitFor(t, func() bool { return len(queueOf(t, rdb, "ws-swp70run", "ses-1")) == 0 })
	assert.Empty(t, queueOf(t, rdb, "ws-swp70run", "ses-1"),
		"Start wired the probe — the Run periodic sweep recovered the entry with no phase change")
	assert.Equal(t, int64(0), stub.deliverHits.Load(), "recovery is lookup-only (S9)")
}

// waitFor polls cond until true or fails the test (bounded 10s).
func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// TestSweeperFaultLeg_RolloverAdmitsViaPlusOne (#1316 leg 6 shape): an
// API rollover mid-delivery leaves the entry parked unverifiable while
// the ledger's in-flight row (attempt+1) went admitted — the sweeper's
// +1 probe completes it with zero Deliver POSTs.
func TestSweeperFaultLeg_RolloverAdmitsViaPlusOne(t *testing.T) {
	stub := newLedgerStub(t, 1<<30)
	stub.muxx.Lock()
	stub.rows[rowKey("ob-leg6", 3)] = "admitted" // the ambiguous in-flight row
	stub.muxx.Unlock()
	handler, svc, rdb := newSweeperEnv(t, "ws-leg6", stub.server.URL)
	handler.SetAgentdTerminus(true)
	svc.SetLedgerProbe(handler.outboxLedgerProbe)

	seedParked(t, svc, "ws-leg6", "ses-1", "ob-leg6", 2, "delivery unverifiable: agent unreachable")
	n, err := svc.SweepWorkspaceParkedErrors(context.Background(), "ws-leg6")
	require.NoError(t, err)
	assert.Equal(t, 1, n, "the admitted in-flight row completes even the unverifiable park")
	assert.Empty(t, queueOf(t, rdb, "ws-leg6", "ses-1"))
	assert.Equal(t, int64(0), stub.deliverHits.Load(), "lookup-only recovery (S9)")
}

// TestSweeperE2E_SeedTransitionArmedBeforeWatcher (#1316 review r2
// finding 4): the probe is wired BEFORE the watcher starts, so the
// Start() seed's own Active transition can sweep — pinned by shrinking
// the periodic interval to nothing-relevant (an hour), leaving the
// seed transition as the only possible recovery path.
func TestSweeperE2E_SeedTransitionArmedBeforeWatcher(t *testing.T) {
	stub := newLedgerStub(t, 1<<30)
	stub.muxx.Lock()
	stub.rows[rowKey("ob-seed", 5)] = "admitted"
	stub.muxx.Unlock()
	handler, svc, rdb := newSweeperEnv(t, "ws-swp70seed", stub.server.URL)
	handler.SetAgentdTerminus(true)

	old := outbox.ParkedSweepInterval
	outbox.ParkedSweepInterval = time.Hour // kill the periodic path
	t.Cleanup(func() { outbox.ParkedSweepInterval = old })

	seedParked(t, svc, "ws-swp70seed", "ses-1", "ob-seed", 5, "context deadline exceeded")
	handler.SetPriorPhaseForTest("ws-swp70seed", "Suspended") // the seed IS a transition

	require.NoError(t, handler.Start())
	t.Cleanup(func() { _ = handler.Stop() })

	waitFor(t, func() bool { return len(queueOf(t, rdb, "ws-swp70seed", "ses-1")) == 0 })
	assert.Empty(t, queueOf(t, rdb, "ws-swp70seed", "ses-1"),
		"the seed-window transition found the probe armed (wire-before-watch)")
	assert.Equal(t, int64(0), stub.deliverHits.Load())
}
