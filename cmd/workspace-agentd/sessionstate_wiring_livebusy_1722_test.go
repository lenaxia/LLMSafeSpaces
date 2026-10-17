// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

// sessionstate_wiring_livebusy_1722_test.go — #1727 r1 F4: the store
// reader's #1722 settle-gate fill is a correctness contract, not an
// implementation detail — pin it:
//
//   - a registry that CANNOT be read marks every seed potentially-live
//     (LiveBusy=true) — the conservative direction. Inverting the
//     default would silently break "never mis-fires mid-turn": a
//     transient registry outage would read as "no live turns" and the
//     reconcile could settle live turns.
//   - a successful read maps per-session: busy verdicts floor their
//     seed, everything else (idle/absent/unknown shapes) does not.

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// liveBusyRegistryServer serves the store-reader surface with a
// scriptable /session/status (failMode toggles a 500).
type liveBusyRegistryServer struct {
	srv       *httptest.Server
	registry  atomic.Value // string body
	failReg   atomic.Bool
	regCalls  atomic.Int64
	sessionID string
}

func newLiveBusyRegistryServer(t *testing.T, sessionID, registryBody string) *liveBusyRegistryServer {
	t.Helper()
	h := &liveBusyRegistryServer{}
	h.registry.Store(registryBody)
	h.sessionID = sessionID
	h.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/session":
			_, _ = w.Write([]byte(`[{"id":"` + sessionID + `","title":"t","status":"idle"}]`))
		case "/session/status":
			h.regCalls.Add(1)
			if h.failReg.Load() {
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
			w.WriteHeader(http.StatusOK)
			body, _ := h.registry.Load().(string)
			_, _ = w.Write([]byte(body))
		default:
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`[]`))
		}
	}))
	t.Cleanup(h.srv.Close)
	orig := agentAddrAtomic.Load()
	t.Cleanup(func() { agentAddrAtomic.Store(orig) })
	agentAddrAtomic.Store(h.srv.URL)
	return h
}

func TestSessionStates_LiveBusyRegistryFailureIsConservative(t *testing.T) {
	h := newLiveBusyRegistryServer(t, "ses_1", `{}`)
	h.failReg.Store(true)

	r := opencodeStoreReader{client: &OpenCodeClient{password: "pw", client: h.srv.Client()}}
	seeds, err := r.SessionStates(context.Background())
	require.NoError(t, err, "a registry outage must not fail the store read (the settle retries next pass)")
	require.Len(t, seeds, 1)
	assert.True(t, seeds["ses_1"].LiveBusy,
		"an unreadable registry marks every session potentially-live — the conservative direction; reading it as 'no live turns' would let the reconcile settle LIVE turns mid-run")
	assert.EqualValues(t, 1, h.regCalls.Load(), "the registry IS consulted (the failure is the signal, not a skipped call)")
}

func TestSessionStates_LiveBusyPerSessionMapping(t *testing.T) {
	h := newLiveBusyRegistryServer(t, "ses_1", `{"ses_1":{"type":"busy"},"ses_other":{"type":"idle"}}`)

	r := opencodeStoreReader{client: &OpenCodeClient{password: "pw", client: h.srv.Client()}}
	seeds, err := r.SessionStates(context.Background())
	require.NoError(t, err)
	require.Len(t, seeds, 1)
	assert.True(t, seeds["ses_1"].LiveBusy, "a busy registry verdict floors the seed (the live-turn hold)")
}

func TestSessionStates_LiveBusyAbsentOrNonBusyIsFalse(t *testing.T) {
	// The session is ABSENT from an otherwise healthy registry — the
	// post-abort shape (wire-proven: the registry clears at the abort).
	h := newLiveBusyRegistryServer(t, "ses_1", `{}`)

	r := opencodeStoreReader{client: &OpenCodeClient{password: "pw", client: h.srv.Client()}}
	seeds, err := r.SessionStates(context.Background())
	require.NoError(t, err)
	assert.False(t, seeds["ses_1"].LiveBusy,
		"absent from a healthy registry = no live turn (the settle signal's registry half is satisfied)")
}
