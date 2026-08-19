// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package workspace

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// #887 D5.1: bearer consumers of the agentd admin mux (:4098) must try
// the DISTINCT admin token first and fall back to the workspace password
// on 401 — the fleet is mixed while legacy pods (env-delivered token ==
// password) coexist with file-delivery pods (distinct token).

func TestStatuszWithBearers_FirstTokenWins(t *testing.T) {
	var got atomic.Value
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got.Store(r.Header.Get("Authorization"))
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	resp, err := statuszWithBearers(context.Background(), deepStatusHTTPClient, srv.URL, []string{"token-a", "token-b"})
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	_ = resp.Body.Close()
	assert.Equal(t, "Bearer token-a", got.Load())
}

func TestStatuszWithBearers_FallsBackOn401(t *testing.T) {
	var attempts atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer legacy-pw" {
			attempts.Add(1)
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	resp, err := statuszWithBearers(context.Background(), deepStatusHTTPClient, srv.URL, []string{"distinct-admin", "legacy-pw"})
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	_ = resp.Body.Close()
	assert.Equal(t, int32(1), attempts.Load(), "exactly the distinct token is rejected before the password fallback succeeds")
}

func TestStatuszWithBearers_AllRejected(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()

	resp, err := statuszWithBearers(context.Background(), deepStatusHTTPClient, srv.URL, []string{"a", "b"})
	require.Error(t, err, "all candidates rejected must surface an error, not a response")
	require.Nil(t, resp)
}

func TestStatuszWithBearers_TransportError(t *testing.T) {
	client := &http.Client{Timeout: 200 * time.Millisecond}
	_, err := statuszWithBearers(context.Background(), client, "http://127.0.0.1:1:2/x", []string{"a"})
	require.Error(t, err)
}
