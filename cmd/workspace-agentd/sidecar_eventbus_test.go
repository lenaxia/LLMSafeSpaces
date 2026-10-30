// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

// sidecar_eventbus_test.go — pins the sidecar topology's design-0072
// parity (review round 1, finding 2b): wireSidecarNotifierBus must run
// the SAME wiring the single-container boot path runs — a wired bus
// when the env arms one, a nil bus (v1 point-to-point) when it does
// not — and the notifier must always come back non-nil (the sidecar's
// muxes serve the MCP subscription surface).

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/alicebob/miniredis/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

func TestWireSidecarNotifierBus_ArmsBusFromEnv(t *testing.T) {
	mr, err := miniredis.Run()
	require.NoError(t, err)
	defer mr.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var wg sync.WaitGroup
	getenv := func(k string) string {
		switch k {
		case "AGENTD_EVENTBUS_ADDR":
			return mr.Addr()
		case "WORKSPACE_ID":
			return "ws-sidecar-pin"
		}
		return ""
	}
	notifier, bus := wireSidecarNotifierBus(ctx, &wg, nil, getenv, "pw", zap.NewNop())
	require.NotNil(t, notifier, "the sidecar's MCP subscription surface must always carry a wired notifier")
	require.NotNil(t, bus, "a reachable AGENTD_EVENTBUS_ADDR must arm the bus in the sidecar topology too")
	cancel()
	wg.Wait()
}

func TestWireSidecarNotifierBus_NilBusWhenUnarmed(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var wg sync.WaitGroup
	notifier, bus := wireSidecarNotifierBus(ctx, &wg, nil, func(string) string { return "" }, "pw", zap.NewNop())
	require.NotNil(t, notifier)
	assert.Nil(t, bus, "no AGENTD_EVENTBUS_ADDR: nil bus — the v1 point-to-point leg (the default and the fallback)")
	cancel()
	wg.Wait()
}

func TestEventbusPasswordBothSetWarnsAndURLWins(t *testing.T) {
	// The both-set config smell must be LOUD (review round 1,
	// Robustness): a credentialed AGENTD_EVENTBUS_ADDR URL keeps its
	// own credentials and the password env loses — deterministically,
	// and now with a warn log telling the operator to drop one.
	mr, err := miniredis.Run()
	require.NoError(t, err)
	defer mr.Close()
	mr.RequireAuth("urlpw")

	core, logs := observer.New(zap.WarnLevel)
	getenv := func(k string) string {
		switch k {
		case "AGENTD_EVENTBUS_ADDR":
			return "redis://:urlpw@" + mr.Addr()
		case "AGENTD_EVENTBUS_PASSWORD":
			return "envpw"
		case "WORKSPACE_ID":
			return "ws-both-set"
		}
		return ""
	}
	bus := eventbusFromEnv(getenv, zap.New(core))
	require.NotNil(t, bus, "the URL's own credentials must wire the bus (the URL wins)")

	warned := false
	for _, e := range logs.TakeAll() {
		if e.Level == zap.WarnLevel && strings.Contains(e.Message, "AGENTD_EVENTBUS_PASSWORD set alongside") {
			warned = true
		}
	}
	assert.True(t, warned, "both-set must warn — the URL's credentials win, loudly, not silently")
}
