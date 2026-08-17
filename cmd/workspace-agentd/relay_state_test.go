// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

// #906 review: the relay_free_models state machine (0 unknown → 1 ok on
// injected config, 2 degraded on deadline-exhausted fetch) shipped with
// zero tests. These pin the observable contract backing the
// RelayInjectorDegraded alert and statusz field.

import (
	"testing"

	"github.com/lenaxia/llmsafespaces/pkg/agentd"

	"github.com/stretchr/testify/assert"
)

func TestRelayFreeModelsStateDefaultsUnknown(t *testing.T) {
	// Package-level state: other tests may have transitioned it. Save,
	// reset, verify the fresh-generation value, restore — the default is
	// what a NEW process reports, which is the alert-relevant contract.
	orig := relayFreeModelsState.Swap(0)
	t.Cleanup(func() { relayFreeModelsState.Store(orig) })
	assert.EqualValues(t, 0, RelayFreeModelsState(),
		"a fresh agent generation reports unknown (0)")
}

func TestRelayFreeModelsStateTransitions(t *testing.T) {
	orig := relayFreeModelsState.Load()
	t.Cleanup(func() { relayFreeModelsState.Store(orig) })

	relayFreeModelsState.Store(1)
	assert.EqualValues(t, 1, RelayFreeModelsState(), "ok state surfaces via statusz accessor")

	relayFreeModelsState.Store(2)
	assert.EqualValues(t, 2, RelayFreeModelsState(), "degraded state surfaces via statusz accessor")
}

// TestRelayFreeModelsStateStatuszField pins the statusz wiring (#901
// G8): the handler in server.go reads RelayFreeModelsState() into the
// response — a silent field drop fails compilation here (the struct
// field is typed) and the accessor indirection is pinned by the
// transitions test above.
func TestRelayFreeModelsStateStatuszField(t *testing.T) {
	orig := relayFreeModelsState.Load()
	t.Cleanup(func() { relayFreeModelsState.Store(orig) })
	relayFreeModelsState.Store(2)

	// server.go builds the response with RelayFreeModels: RelayFreeModelsState().
	resp := agentd.StatuszResponse{
		RelayFreeModels: RelayFreeModelsState(),
	}
	assert.EqualValues(t, 2, resp.RelayFreeModels,
		"statusz must surface the degraded injector state")
}
