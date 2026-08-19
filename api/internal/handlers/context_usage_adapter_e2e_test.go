// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package handlers

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"

	opencode "github.com/lenaxia/llmsafespaces/pkg/agent/opencode"
)

// Real-adapter integration for the context-usage seam (PR #938 review):
// drives REAL captured 1.18.10 wire events through the full handler path
// (onRawEvent → persistContextFromEvent → real Adapter → wire → session
// index), pinning the seam contract that the stub-based wiring tests only
// assume. Wire-shape authenticity: the payloads below are byte-shapes from
// the golden fixture (testdata/sse_events_1_18_10.jsonl, captured from a
// live 1.18.10 event store) and the epic-63 live SSE capture.

func newRealAdapterEnv(t *testing.T) (*ProxyHandler, *contextUsedSessionIndex, *observer.ObservedLogs) {
	t.Helper()
	// The adapter needs no backend for event translation (no HTTP is made
	// on this path), but the env wiring requires one for construction.
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(backend.Close)

	env := newE2EEnv(t, backend) // wires the REAL opencode adapter into env.handler

	// Swap the adapter for one with an observed logger so drift warns are
	// assertable (newE2EEnv constructs with a nil logger → nop).
	core, logs := observer.New(zap.WarnLevel)
	env.handler.SetAdapter(opencode.NewAdapter(
		env.handler.AdapterPasswordResolver(),
		env.handler.AdapterPodIPResolver(),
		zap.New(core),
		opencode.WithAdapterHTTPClient(backend.Client()),
		opencode.WithAdapterPort(extractPort(t, backend.URL)),
	))

	si := newContextUsedSessionIndex()
	env.handler.SetSessionIndex(si)
	return env.handler, si, logs
}

func TestE2E_RealAdapter_StepFinishEvent_PersistsOccupancy(t *testing.T) {
	h, si, _ := newRealAdapterEnv(t)

	// Byte-shape from the golden fixture: suffixed type, step-finish part.
	h.onRawEvent("ws-1", "message.part.updated.1", `{"id":"evt1","type":"message.part.updated.1","properties":{"sessionID":"ses_new","part":{"id":"prt1","type":"step-finish","reason":"tool-calls","cost":0,"tokens":{"total":3950,"input":2310,"output":285,"reasoning":75,"cache":{"read":1280,"write":0}}}}}`)

	v, ok := si.get("ws-1", "ses_new")
	require.True(t, ok, "real adapter must decode the 1.18.10 part-update shape")
	assert.Equal(t, int64(2310+1280+0), v, "occupancy = input + cache.read + cache.write")
}

func TestE2E_RealAdapter_UnsuffixedTypeAndLegacyShape_AlsoPersist(t *testing.T) {
	h, si, _ := newRealAdapterEnv(t)

	// Unsuffixed type name (epic-63 live SSE capture shape) with the same part body.
	h.onRawEvent("ws-1", "message.part.updated", `{"id":"evt2","type":"message.part.updated","properties":{"sessionID":"ses_u","part":{"type":"step-finish","tokens":{"input":100,"output":10,"reasoning":0,"cache":{"read":40,"write":10}}}}}`)
	// Legacy standalone event (pre-1.18 mixed-fleet rollout).
	h.onRawEvent("ws-1", "session.next.step.ended", `{"type":"session.next.step.ended","properties":{"sessionID":"ses_old","tokens":{"input":800,"output":400,"reasoning":100,"cache":{"read":200,"write":50}}}}`)

	vU, okU := si.get("ws-1", "ses_u")
	require.True(t, okU)
	assert.Equal(t, int64(150), vU, "unsuffixed type must decode: 100+40+10")

	vOld, okOld := si.get("ws-1", "ses_old")
	require.True(t, okOld)
	assert.Equal(t, int64(1050), vOld, "legacy shape must decode: 800+200+50")
}

func TestE2E_RealAdapter_DriftPath_NoPersistenceAndWarns(t *testing.T) {
	h, si, logs := newRealAdapterEnv(t)

	// Usage-typed event whose payload lacks tokens: real adapter must
	// report no usage AND warn (drift), and the handler must skip persistence.
	h.onRawEvent("ws-1", "message.part.updated.1", `{"id":"evt3","type":"message.part.updated.1","properties":{"sessionID":"ses_drift","part":{"type":"step-finish","reason":"stop"}}}`)

	_, ok := si.get("ws-1", "ses_drift")
	assert.False(t, ok, "drift payload must not persist")

	warned := false
	for _, e := range logs.All() {
		if e.Message == "opencode usage event claims tokens but fails to decode — wire drift?" {
			warned = true
			break
		}
	}
	assert.True(t, warned, "the real adapter must warn on usage-typed-but-undecodable events")
}

func TestE2E_RealAdapter_TextPartsAndDeltas_NeverPersist(t *testing.T) {
	h, si, logs := newRealAdapterEnv(t)

	// High-frequency non-usage traffic through the real adapter: no
	// persistence, and no drift warns (these are normal events, not drift).
	h.onRawEvent("ws-1", "message.part.updated.1", `{"id":"evt4","type":"message.part.updated.1","properties":{"sessionID":"ses_x","part":{"type":"text","text":"streaming"}}}`)
	h.onRawEvent("ws-1", "message.part.delta", `{"id":"evt5","type":"message.part.delta","properties":{"sessionID":"ses_x","delta":"he"}}`)
	h.onRawEvent("ws-1", "session.updated.1", `{"id":"evt6","type":"session.updated.1","properties":{"sessionID":"ses_x"}}`)

	assert.Empty(t, si.contextUsed, "non-usage events must never persist")
	assert.Empty(t, logs.All(), "non-usage events are normal traffic — no drift warns")
}

// TestNewSSETracker_MeteringDecoderWiredAtConstruction pins the production
// wiring: once an adapter is set, the constructed tracker carries the
// metering decoder — behaviorally (a session.updated event fires
// inference). Deleting the construction-site wiring silently zeroes all
// billing while every other suite stays green (the exact failure class
// app.go's SetOnInference comment documents).
func TestNewSSETracker_MeteringDecoderWiredAtConstruction(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(backend.Close)
	env := newE2EEnv(t, backend) // real adapter wired into env.handler

	tracker := env.handler.newSSETracker()
	var fired bool
	tracker.SetOnInference(func(_, _, _ string, _, outputTokens int64, _ float64) {
		fired = outputTokens == 500
	})

	tracker.ProcessEvent("ws-wiring", `{"id":"evt_t","type":"session.updated","properties":{"sessionID":"ses_w","info":{"id":"ses_w","cost":0.01,"tokens":{"input":1000,"output":500},"model":{"id":"gpt-4o","providerID":"openai"}}}}`)

	assert.True(t, fired, "construction-site decoder wiring must make billing inference work end-to-end")
}
