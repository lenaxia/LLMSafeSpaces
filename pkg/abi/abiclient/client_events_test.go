// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package abiclient_test

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/lenaxia/llmsafespaces/pkg/abi/abiclient"
	abiv1 "github.com/lenaxia/llmsafespaces/pkg/abi/v1"
)

// TestStreamAppliedEvents: the applied-events option delivers every event
// the fold accepted (post discard rule), in stream order, with its seq —
// and nothing for discarded duplicates (US-69.11's usage consumer depends
// on exactly-once-per-seq event delivery).
func TestStreamAppliedEvents(t *testing.T) {
	a, ts, _ := newSurface(t, nil, nil)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	type seen struct {
		seq uint64
		typ abiv1.EventType
	}
	got := make(chan seen, 16)
	seeded := make(chan struct{}, 1)
	go func() {
		_ = abiclient.New(&http.Client{Transport: authedTransport{pw: "pw"}}, ts.URL).Stream(ctx, func(*abiclient.SessionState) {
			select {
			case seeded <- struct{}{}:
			default:
			}
		}, abiclient.WithAppliedEvents(func(evt *abiv1.Event, seq uint64) {
			got <- seen{seq: seq, typ: evt.GetType()}
		}))
	}()

	// Ingest ONLY after the snapshot landed, so the events arrive live
	// (pre-connect events would fold into the snapshot, not the stream).
	select {
	case <-seeded:
	case <-time.After(30 * time.Second):
		t.Fatal("stream never delivered its snapshot")
	}

	a.IngestForTest(statusEvt("s0", abiv1.SessionStatus_SESSION_STATUS_BUSY))
	a.IngestForTest(inputEvt("q1", "s0"))

	var out []seen
	deadline := time.Now().Add(30 * time.Second)
	for len(out) < 2 && time.Now().Before(deadline) {
		select {
		case s := <-got:
			out = append(out, s)
		case <-time.After(100 * time.Millisecond):
		}
	}
	require.Len(t, out, 2, "expected 2 applied events")
	require.Equal(t, abiv1.EventType_EVENT_TYPE_SESSION_STATUS, out[0].typ)
	require.Equal(t, abiv1.EventType_EVENT_TYPE_INPUT_REQUEST, out[1].typ)
	require.Greater(t, out[1].seq, out[0].seq)

	// Every ingestion is a distinct seq on this surface; re-ingesting the
	// same logical status still delivers exactly one applied event per
	// seq (the reconnect-overlap discard path is pinned by the property
	// fuzz suite).
	a.IngestForTest(statusEvt("s0", abiv1.SessionStatus_SESSION_STATUS_BUSY))
	select {
	case s := <-got:
		require.Equal(t, uint64(3), s.seq, "seqs are assigned monotonically")
	case <-time.After(30 * time.Second):
		t.Fatal("third event never delivered as applied event")
	}
	select {
	case s := <-got:
		t.Fatalf("unexpected extra event: %+v", s)
	case <-time.After(300 * time.Millisecond):
	}
}
