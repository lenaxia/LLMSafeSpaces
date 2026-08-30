// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package sessionstate_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/lenaxia/llmsafespaces/cmd/workspace-agentd/sessionstate"
	abiv1 "github.com/lenaxia/llmsafespaces/pkg/abi/v1"
	abiconnect "github.com/lenaxia/llmsafespaces/pkg/abi/v1/abiconnect"
)

func testServer(t *testing.T, cfgMut func(*sessionstate.Config)) (*sessionstate.Authority, *httptest.Server) {
	t.Helper()
	cfg := sessionstate.Config{
		PlatformDir: t.TempDir(),
		Parser:      &fixtureParser{},
		Store:       &mapStore{},
		Passwords:   []string{"agentd-password"},
	}
	if cfgMut != nil {
		cfgMut(&cfg)
	}
	a, err := sessionstate.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = a.Close() })
	_, h := a.Handler()
	ts := httptest.NewServer(h)
	t.Cleanup(ts.Close)
	return a, ts
}

// authedClient returns a connect client whose transport injects the given
// Basic password; password "" means no Authorization header at all.
func authedClient(url, password string) abiconnect.HarnessABIServiceClient {
	return abiconnect.NewHarnessABIServiceClient(&http.Client{Transport: authTransport{password}}, url)
}

type authTransport struct{ password string }

func (t authTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	if t.password != "" {
		r.SetBasicAuth("opencode", t.password)
	}
	return http.DefaultTransport.RoundTrip(r)
}

func asConnectError(err error) *connect.Error {
	if ce, ok := err.(*connect.Error); ok {
		return ce
	}
	return nil
}

// TestAuthEveryRoute401: EVERY ABI op — including the Events stream —
// rejects unauthenticated and wrong-password requests; zero unauthenticated
// routes exist (issue #1136: auth_every_route_401; I8).
func TestAuthEveryRoute401(t *testing.T) {
	_, ts := testServer(t, nil)
	base := ts.URL

	cases := []struct {
		name string
		call func(c abiconnect.HarnessABIServiceClient) error
	}{
		{"Events", func(c abiconnect.HarnessABIServiceClient) error {
			s, err := c.Events(context.Background(), connect.NewRequest(&abiv1.EventsRequest{}))
			if err != nil {
				return err
			}
			defer func() { _ = s.Close() }()
			if !s.Receive() {
				return s.Err()
			}
			return nil
		}},
		{"GetSnapshot", func(c abiconnect.HarnessABIServiceClient) error {
			_, err := c.GetSnapshot(context.Background(), connect.NewRequest(&abiv1.GetSnapshotRequest{SessionId: "s1"}))
			return err
		}},
		{"Deliver", func(c abiconnect.HarnessABIServiceClient) error {
			_, err := c.Deliver(context.Background(), connect.NewRequest(&abiv1.DeliveryRequest{SessionId: "s1", EntryId: "e1", Attempt: 1,
				Parts: []*abiv1.DeliveryPart{{Part: &abiv1.DeliveryPart_Text{Text: "x"}}}}))
			return err
		}},
		{"GetDeliveryStatus", func(c abiconnect.HarnessABIServiceClient) error {
			_, err := c.GetDeliveryStatus(context.Background(), connect.NewRequest(&abiv1.GetDeliveryStatusRequest{SessionId: "s1", EntryId: "e1", Attempt: 1}))
			return err
		}},
		{"Act", func(c abiconnect.HarnessABIServiceClient) error {
			_, err := c.Act(context.Background(), connect.NewRequest(&abiv1.ActionRequest{SessionId: "s1",
				Action: &abiv1.ActionRequest_Interrupt{Interrupt: &abiv1.InterruptAction{}}}))
			return err
		}},
	}

	for _, tc := range cases {
		for _, auth := range []string{"none", "wrong"} {
			t.Run(tc.name+"/"+auth, func(t *testing.T) {
				pw := ""
				if auth == "wrong" {
					pw = "not-the-password"
				}
				err := tc.call(authedClient(base, pw))
				ce := asConnectError(err)
				if err == nil || ce == nil {
					t.Fatalf("%s succeeded (or non-connect error %v) without valid credentials — I8 violated", tc.name, err)
				}
				if ce.Code() != connect.CodeUnauthenticated {
					t.Errorf("code = %v, want Unauthenticated (HTTP 401)", ce.Code())
				}
			})
		}
	}

	// With the right password, no route returns Unauthenticated (they may
	// be Unimplemented at this story — but never 401).
	t.Run("valid_password_never_401", func(t *testing.T) {
		c := authedClient(base, "agentd-password")
		for _, tc := range cases {
			if err := tc.call(c); err != nil {
				if ce := asConnectError(err); ce != nil && ce.Code() == connect.CodeUnauthenticated {
					t.Errorf("%s: 401 with VALID password", tc.name)
				}
			}
		}
	})
}

// TestRateLimitPerSession: authenticated bursts on Deliver exceed the
// per-session bucket → ResourceExhausted (HTTP 429 on the connect
// protocol), while a second session is unaffected (issue #1136 AC).
func TestRateLimitPerSession(t *testing.T) {
	_, ts := testServer(t, func(c *sessionstate.Config) {
		c.RateLimit.Burst = 3
		c.RateLimit.RefillPerSec = 0
	})
	c := authedClient(ts.URL, "agentd-password")

	deliver := func(session string) error {
		_, err := c.Deliver(context.Background(), connect.NewRequest(&abiv1.DeliveryRequest{
			SessionId: session, EntryId: "e", Attempt: 1,
			Parts: []*abiv1.DeliveryPart{{Part: &abiv1.DeliveryPart_Text{Text: "x"}}},
		}))
		return err
	}

	for i := 0; i < 3; i++ {
		if err := deliver("s1"); err != nil {
			if ce := asConnectError(err); ce != nil && ce.Code() == connect.CodeResourceExhausted {
				t.Fatalf("session s1 exhausted on call %d — burst not honored", i+1)
			}
		}
	}
	err := deliver("s1")
	ce := asConnectError(err)
	if err == nil || ce == nil || ce.Code() != connect.CodeResourceExhausted {
		t.Fatalf("burst code = %v (%v), want ResourceExhausted (429)", ce, err)
	}
	if err := deliver("s2"); err != nil {
		if ce := asConnectError(err); ce != nil && ce.Code() == connect.CodeResourceExhausted {
			t.Error("per-SESSION limit leaked across sessions")
		}
	}
}

// TestWedgedOpencodeHotPaths: with the store blackholed (every read hangs),
// the events stream and state reads still serve within budget — M3.1: a
// wedged opencode never blocks a snapshot/stream (2026-08-15 class).
func TestWedgedOpencodeHotPaths(t *testing.T) {
	a, ts := testServer(t, func(c *sessionstate.Config) {
		c.Store = hangingStore{}
	})
	a.Ingest([]byte("busy s1"))

	c := authedClient(ts.URL, "agentd-password")

	const budget = 2 * time.Second
	done := make(chan error, 2)
	go func() {
		s, err := c.Events(context.Background(), connect.NewRequest(&abiv1.EventsRequest{}))
		if err != nil {
			done <- err
			return
		}
		defer func() { _ = s.Close() }()
		if !s.Receive() {
			done <- s.Err()
			return
		}
		if s.Msg().GetSnapshot() == nil {
			done <- errString("first frame not snapshot")
			return
		}
		done <- nil
	}()
	go func() {
		if st := a.State(); st.Seq == 0 {
			done <- errString("state read empty")
			return
		}
		done <- nil
	}()

	for i := 0; i < 2; i++ {
		select {
		case err := <-done:
			if err != nil {
				t.Fatalf("hot path failed while opencode wedged: %v", err)
			}
		case <-time.After(budget):
			t.Fatalf("hot path exceeded %v budget with wedged opencode — M3.1 violated", budget)
		}
	}

	// A reseed against the wedged store must also not wedge the stream:
	// bounded context; events still flow afterwards.
	rctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	_ = a.Reseed(rctx, sessionstate.ReseedReasonGenerationChange)
	a.Ingest([]byte("idle s1"))
	s2, err := c.Events(context.Background(), connect.NewRequest(&abiv1.EventsRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s2.Close() }()
	if !s2.Receive() {
		t.Fatalf("stream did not serve after wedged-store reseed: %v", s2.Err())
	}
	snap := s2.Msg().GetSnapshot()
	if snap == nil {
		t.Fatalf("first frame after wedged reseed is not a snapshot: %+v", s2.Msg().Frame)
	}
	var s1 abiv1.SessionStatus
	for _, ss := range snap.GetSnapshot().GetSessions() {
		if ss.GetSessionId() == "s1" {
			s1 = ss.GetStatus()
		}
	}
	if s1 != abiv1.SessionStatus_SESSION_STATUS_IDLE {
		t.Errorf("post-ingest snapshot s1 = %v, want IDLE — events did not flow after a failed (wedged-store) reseed", s1)
	}
}

// TestEventsStreamReseedNoticeOnWire proves the reseed notice reaches wire
// subscribers and carries a seq (mandatory re-snapshot signal, I3).
func TestEventsStreamReseedNoticeOnWire(t *testing.T) {
	a, ts := testServer(t, nil)
	c := authedClient(ts.URL, "agentd-password")

	s, err := c.Events(context.Background(), connect.NewRequest(&abiv1.EventsRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	if !s.Receive() { // snapshot frame
		t.Fatal(s.Err())
	}

	reseeded := make(chan uint64, 1)
	go func() {
		for s.Receive() {
			if r := s.Msg().GetReseeded(); r != nil {
				reseeded <- r.Seq
				return
			}
		}
	}()
	if err := a.Reseed(context.Background(), sessionstate.ReseedReasonGenerationChange); err != nil {
		t.Fatal(err)
	}
	select {
	case seq := <-reseeded:
		if seq == 0 {
			t.Error("reseed notice carried no seq")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("projection.reseeded never reached the wire subscriber")
	}
}

type errString string

func (e errString) Error() string { return string(e) }
