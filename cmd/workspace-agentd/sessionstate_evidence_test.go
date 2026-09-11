// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lenaxia/llmsafespaces/cmd/workspace-agentd/sessionstate"
	abiv1 "github.com/lenaxia/llmsafespaces/pkg/abi/v1"
)

// #1311 store evidence: opencodeStoreReader.MessagePresence pages the V1
// message list; absence is proven only by cursor exhaustion, never by a
// page-budget overrun or transport error.

func messagePresenceStub(t *testing.T, pages [][]string, failAfter int) *httptest.Server {
	t.Helper()
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if failAfter > 0 && calls >= failAfter {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		page := 0
		if before := r.URL.Query().Get("before"); before != "" {
			page, _ = strconv.Atoi(before)
		}
		if page >= len(pages) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`[]`))
			return
		}
		body := "["
		for i, id := range pages[page] {
			if i > 0 {
				body += ","
			}
			body += fmt.Sprintf(`{"info":{"id":%q}}`, id)
		}
		body += "]"
		if page+1 < len(pages) {
			w.Header().Set("X-Next-Cursor", strconv.Itoa(page+1))
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestOpencodeStoreReader_MessagePresence(t *testing.T) {
	tests := []struct {
		name      string
		pages     [][]string
		failAfter int
		ids       []string
		want      map[string]bool
		wantErr   bool
	}{
		{
			name:  "present on first page",
			pages: [][]string{{"msg_a", "msg_b"}},
			ids:   []string{"msg_a"},
			want:  map[string]bool{"msg_a": true},
		},
		{
			name:  "present on later page via cursor",
			pages: [][]string{{"msg_1"}, {"msg_2"}, {"msg_target"}},
			ids:   []string{"msg_target"},
			want:  map[string]bool{"msg_target": true},
		},
		{
			name:  "absence proven when cursor exhausts",
			pages: [][]string{{"msg_1"}, {"msg_2"}},
			ids:   []string{"msg_gone"},
			want:  map[string]bool{"msg_gone": false},
		},
		{
			name:  "mixed ids",
			pages: [][]string{{"msg_1"}, {"msg_2"}},
			ids:   []string{"msg_1", "msg_gone"},
			want:  map[string]bool{"msg_1": true, "msg_gone": false},
		},
		{
			name:      "transport failure is no evidence, never absent",
			failAfter: 1,
			ids:       []string{"msg_a"},
			wantErr:   true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := messagePresenceStub(t, tt.pages, tt.failAfter)
			orig := agentAddrAtomic.Load()
			defer agentAddrAtomic.Store(orig)
			agentAddrAtomic.Store(srv.URL)
			r := opencodeStoreReader{client: &OpenCodeClient{password: "pw", client: &http.Client{Timeout: 5 * time.Second}}}

			got, err := r.MessagePresence(context.Background(), "s1", tt.ids)
			if tt.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}

// TestOpencodeStoreReader_MessagePresenceBudgetIsNoEvidence: exhausting
// the page budget returns an error (absence unproven) — the sweep must
// never treat a too-deep message as absent.
func TestOpencodeStoreReader_MessagePresenceBudgetIsNoEvidence(t *testing.T) {
	pages := make([][]string, messageEvidencePageBudget+5)
	for i := range pages {
		pages[i] = []string{fmt.Sprintf("msg_%d", i)}
	}
	srv := messagePresenceStub(t, pages, 0)
	orig := agentAddrAtomic.Load()
	defer agentAddrAtomic.Store(orig)
	agentAddrAtomic.Store(srv.URL)
	r := opencodeStoreReader{client: &OpenCodeClient{password: "pw", client: &http.Client{Timeout: 5 * time.Second}}}

	_, err := r.MessagePresence(context.Background(), "s1", []string{"msg_never"})
	require.Error(t, err, "page-budget overrun must be an error, not a false")
}

// wiringEvidenceStore adapts the in-memory evidence shape to the
// sessionstate.StoreReader seam for watchdog tests.
type wiringEvidenceStore struct {
	states map[string]sessionstate.SessionSeed
	msgs   map[string]map[string]bool
}

func (s wiringEvidenceStore) SessionStates(ctx context.Context) (map[string]sessionstate.SessionSeed, error) {
	out := make(map[string]sessionstate.SessionSeed, len(s.states))
	for k, v := range s.states {
		out[k] = v
	}
	return out, nil
}

func (s wiringEvidenceStore) MessagePresence(ctx context.Context, sessionID string, messageIDs []string) (map[string]bool, error) {
	present := map[string]bool{}
	for _, id := range messageIDs {
		present[id] = s.msgs[sessionID][id]
	}
	return present, nil
}

// TestSessionStateWatchdog_ReconcilesLedger (#1311): the production
// watchdog LOOP — not a direct Reconcile call — converges a stranded
// admitted row via store evidence and carries the outcome to the
// reconciled counter.
func TestSessionStateWatchdog_ReconcilesLedger(t *testing.T) {
	store := wiringEvidenceStore{
		states: map[string]sessionstate.SessionSeed{
			"s1": {Status: abiv1.SessionStatus_SESSION_STATUS_IDLE},
		},
		msgs: map[string]map[string]bool{"s1": {"msg-1": true}},
	}
	a, err := sessionstate.New(sessionstate.Config{
		PlatformDir: t.TempDir(),
		Parser:      noopParser{},
		Store:       store,
		Passwords:   []string{"pw"},
		Admitter:    instantAdmitter{},
		FastCursor:  true,
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = a.Close() })

	// Land an admitted row through the real wire op (the #1311 incident
	// shape: admitted, promotion event never arrives).
	_, h := a.Handler()
	ts := httptest.NewServer(h)
	t.Cleanup(ts.Close)
	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/llmsafespaces.abi.v1.HarnessABIService/Deliver",
		strings.NewReader(`{"sessionId":"s1","entryId":"e-1","attempt":1,"parts":[{"text":"hi"}]}`))
	req.SetBasicAuth("opencode", "pw")
	req.Header.Set("Content-Type", "application/json")
	res, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	require.Equal(t, 200, res.StatusCode)
	_ = res.Body.Close()
	require.Eventually(t, func() bool {
		m := a.Metrics()
		return m.LedgerDepths != nil && m.LedgerDepths["admitted"] == 1
	}, 3*time.Second, 10*time.Millisecond, "row admitted")

	ctx, cancel := context.WithCancel(context.Background())
	go runSessionStateWatchdog(ctx, "ws-1311", a, 5*time.Millisecond)
	t.Cleanup(cancel)

	require.Eventually(t, func() bool {
		return testutil.ToFloat64(sessionStateMetrics.reconciled.WithLabelValues("promoted")) >= 1
	}, 3*time.Second, 10*time.Millisecond, "the loop sweeps the stranded row and counts the outcome")
	m := a.Metrics()
	assert.Equal(t, int64(1), m.ReconcilePromoted, "Metrics() carries the cumulative sweep outcome")
}

// TestRecordSessionStateMetrics_ExportsReseedSweepOutcomes (r2-2): a
// reseed-embedded boot-heal sweep's outcomes reach the Prometheus series —
// the delta bridge in recordSessionStateMetrics is the single export path,
// so sweeps that never pass through the watchdog's own Reconcile return
// are counted identically.
func TestRecordSessionStateMetrics_ExportsReseedSweepOutcomes(t *testing.T) {
	store := wiringEvidenceStore{
		states: map[string]sessionstate.SessionSeed{
			"s1": {Status: abiv1.SessionStatus_SESSION_STATUS_IDLE},
		},
		msgs: map[string]map[string]bool{"s1": {"msg-1": true}},
	}
	a, err := sessionstate.New(sessionstate.Config{
		PlatformDir: t.TempDir(),
		Parser:      noopParser{},
		Store:       store,
		Passwords:   []string{"pw"},
		Admitter:    instantAdmitter{},
		FastCursor:  true,
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = a.Close() })

	// Land an admitted row through the real wire op, then let it strand
	// (no events arrive — the harness-dead shape).
	_, h := a.Handler()
	ts := httptest.NewServer(h)
	t.Cleanup(ts.Close)
	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/llmsafespaces.abi.v1.HarnessABIService/Deliver",
		strings.NewReader(`{"sessionId":"s1","entryId":"e-1","attempt":1,"parts":[{"text":"hi"}]}`))
	req.SetBasicAuth("opencode", "pw")
	req.Header.Set("Content-Type", "application/json")
	res, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	require.Equal(t, 200, res.StatusCode)
	_ = res.Body.Close()
	require.Eventually(t, func() bool {
		m := a.Metrics()
		return m.LedgerDepths != nil && m.LedgerDepths["admitted"] == 1
	}, 3*time.Second, 10*time.Millisecond, "row admitted")

	// Baseline scrape (prom counters are process-global across tests —
	// assert the DELTA), then the BOOT RESEED (the sweep runs inside it).
	recordSessionStateMetrics("ws-r22", a)
	before := testutil.ToFloat64(sessionStateMetrics.reconciled.WithLabelValues("promoted"))
	require.NoError(t, a.Reseed(context.Background(), sessionstate.ReseedReasonBoot))

	m := a.Metrics()
	require.Equal(t, int64(1), m.ReconcilePromoted, "boot-embedded sweep recorded in cumulative counters")
	recordSessionStateMetrics("ws-r22", a)
	assert.Equal(t, before+1.0, testutil.ToFloat64(sessionStateMetrics.reconciled.WithLabelValues("promoted")),
		"the reseed-embedded outcome reaches the Prometheus series via the delta bridge")
}
