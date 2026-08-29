package opencode

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

// #1119 follow-up 2: the verify oracle runs on a dedicated bounded client
// with fresh connections, so one wedged agent connection costs a single
// bounded pass — never the shared pool, never the promotion-await budget.

func TestVerifyClient_Config(t *testing.T) {
	hc := newVerifyHTTPClient()
	tr, ok := hc.Transport.(*http.Transport)
	require.True(t, ok, "verify client must use a plain transport")
	assert.True(t, tr.DisableKeepAlives, "verify must not pool connections (wedge isolation)")
	assert.Equal(t, verifyRequestTimeout, hc.Timeout, "verify must carry a hard per-request bound")
}

func TestVerifyClient_CopyNotMutation(t *testing.T) {
	shared := NewClient("http://base", "pw", zap.NewNop())
	before := shared.httpClient
	vc := shared.withVerifyTransport(newVerifyHTTPClient())
	assert.NotSame(t, shared, vc, "withVerifyTransport returns a copy")
	assert.Same(t, before, shared.httpClient, "the resolved (shared) client must keep its pooled transport")
	assert.NotSame(t, before, vc.httpClient, "the verify copy carries the dedicated transport")
}

// TestVerifyDelivery_SurvivesWedgedConnection reproduces the first-live-
// traffic incident shape (2026-08-29): the agent wedges the FIRST request
// (hangs until the client bound) and serves the second normally. With
// keep-alive-free verify, pass 2 opens a fresh connection and confirms
// delivery — the stuck-verifying-forever outcome is impossible.
func TestVerifyDelivery_SurvivesWedgedConnection(t *testing.T) {
	origTimeout := verifyRequestTimeout
	verifyRequestTimeout = 200 * time.Millisecond
	t.Cleanup(func() { verifyRequestTimeout = origTimeout })

	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&hits, 1) == 1 {
			// Wedge: hold the first connection open until the client
			// bound fires (request ctx cancels), then drop it.
			<-r.Context().Done()
			return
		}
		created := time.Now().UTC().Add(-1 * time.Second)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `[{"info":{"role":"user","id":"msg_1","time":{"created":%d}},"parts":[{"type":"text","text":"hello"}]}]`,
			created.UnixMilli())
	}))
	t.Cleanup(srv.Close)

	a := newTestAdapter(t, srv)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// Pass 1: wedged connection — bounded error, not a hang past the budget.
	_, _, err := a.VerifyDelivery(ctx, "", "ws-1", "ses_1", "hello", time.Now().UTC().Add(-time.Minute))
	require.Error(t, err, "the wedged first connection must fail (bounded), not hang")

	// Pass 2: fresh connection (DisableKeepAlives) — delivered.
	delivered, definitive, err := a.VerifyDelivery(ctx, "", "ws-1", "ses_1", "hello", time.Now().UTC().Add(-time.Minute))
	require.NoError(t, err)
	assert.True(t, delivered, "fresh connection confirms the persisted text")
	assert.True(t, definitive)
	assert.GreaterOrEqual(t, atomic.LoadInt32(&hits), int32(2))
}

// TestVerifyDelivery_V2WedgedConnection is the same property through the
// V2-store branch (a.v2Store=true): single-fetch oracle on the dedicated
// client.
func TestVerifyDelivery_V2WedgedConnection(t *testing.T) {
	origTimeout := verifyRequestTimeout
	verifyRequestTimeout = 200 * time.Millisecond
	t.Cleanup(func() { verifyRequestTimeout = origTimeout })

	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings_HasSuffixV2Messages(r) {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		if atomic.AddInt32(&hits, 1) == 1 {
			<-r.Context().Done()
			return
		}
		created := time.Now().UTC().Add(-1 * time.Second)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"data": []map[string]any{{
				"id": "msg_1", "type": "user", "text": "hello",
				"time": map[string]int64{"created": created.UnixMilli()},
			}},
		})
	}))
	t.Cleanup(srv.Close)

	a := newTestAdapter(t, srv)
	a.v2Store = true

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	_, _, err := a.VerifyDelivery(ctx, "", "ws-1", "ses_1", "hello", time.Now().UTC().Add(-time.Minute))
	require.Error(t, err)

	delivered, definitive, err := a.VerifyDelivery(ctx, "", "ws-1", "ses_1", "hello", time.Now().UTC().Add(-time.Minute))
	require.NoError(t, err)
	assert.True(t, delivered)
	assert.True(t, definitive)
}

func strings_HasSuffixV2Messages(r *http.Request) bool {
	return r.Method == http.MethodGet &&
		len(r.URL.Path) > len("/api/session/") &&
		r.URL.Path[len("/api/session/"):] != "" &&
		hasSuffix(r.URL.Path, "/message")
}

func hasSuffix(s, suf string) bool {
	return len(s) >= len(suf) && s[len(s)-len(suf):] == suf
}
