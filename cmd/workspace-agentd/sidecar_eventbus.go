// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

// sidecar_eventbus.go — the design-0071/0072 wiring for the SIDECAR
// topology (the #1332 both-modes convention): the sidecar's muxes serve
// the MCP subscription surface (subscribe_session et al.), so this
// process must own the notifier's delivery engine and — when
// AGENTD_EVENTBUS_ADDR arms a bus — its consumer groups, exactly as the
// single-container boot path does (main.go). Without this, the env the
// controller injects into the sidecar container would land in the one
// process that never reads it, and the bus could not go live in sidecar
// mode regardless of the flags.

import (
	"context"
	"sync"

	"go.uber.org/zap"

	"github.com/lenaxia/llmsafespaces/cmd/workspace-agentd/sessionstate"
	"github.com/lenaxia/llmsafespaces/pkg/eventbus"
)

// wireSidecarNotifierBus mirrors main()'s notifier + eventbus wiring:
// eventbusFromEnv (nil bus when unset/unreachable/misconfigured → the
// v1 point-to-point delivery layer, the default and the fallback),
// wireSessionNotifier (swaps the fully-wired engine into the MCP
// surface the sidecar serves), runEventbusConsumers (the notifier +
// lease consumer groups), startSessionNotifierLoops (the settle fold's
// debounce/registry loops). Extracted so the topology parity is pinned
// (sidecar_eventbus_test.go) rather than grep-verified.
func wireSidecarNotifierBus(ctx context.Context, wg *sync.WaitGroup, authority *sessionstate.Authority, getenv func(string) string, password string, logger *zap.Logger) (*sessionNotifier, *eventbus.RedisBus) {
	if logger == nil {
		logger = zap.NewNop()
	}
	bus := eventbusFromEnv(getenv, logger) //nolint:contextcheck // eventbusFromEnv owns its 5s boot-probe budget (context.Background is deliberate: a boot-time reachability check, not request-scoped work to cancel)
	notifier := wireSessionNotifier(sessionNotifierWiring{
		Authority: authority,
		Password:  password,
		Bus:       bus,
		Logger:    logger,
	})
	runEventbusConsumers(ctx, wg, bus, notifier, password, logger)
	startSessionNotifierLoops(ctx, wg, notifier)
	return notifier, bus
}
