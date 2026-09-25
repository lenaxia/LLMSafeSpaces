// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"flag"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestRegisterRelayFlags_ParsesIntoReturnedStruct is the #1548 root-cause
// pin. registerRelayFlags originally returned the struct BY VALUE while
// the flag package bound into the LOCAL's fields — flag.Parse wrote the
// orphaned original and the caller's copy kept zero values forever:
// --relay-only-key-delivery=true parsed into a dead struct, the
// controller ran healthy and never armed (staging unreachable since
// e81a500f), and the split-brain incident was this bug's signature.
// The posture gate's Assert 2 named it ("wiring drift") on #1566's
// discriminator run; the hermetic probe (out-of-range TTL must exit 85)
// reproduced it pre-fix.
//
// The pin parses a FRESH FlagSet against the RETURNED struct: if the
// registration ever re-orphaned the parse targets (value return,
// field copy, shadowed struct), enabled stays false here exactly as it
// did in production.
func TestRegisterRelayFlags_ParsesIntoReturnedStruct(t *testing.T) {
	fs := flag.NewFlagSet("probe", flag.ContinueOnError)
	regs := registerRelayFlagsInto(fs)
	require.NoError(t, fs.Parse([]string{
		"--relay-only-key-delivery=true",
		"--llm-relay-router-url=http://llm-relay-router.llm-relay.svc.cluster.local",
		"--llm-relay-namespace=llm-relay",
		"--relay-token-ttl=12h",
	}))

	assert.True(t, regs.enabled,
		"the parsed flag MUST land in the struct the caller reads — the value-copy orphaning is #1548's root cause")
	assert.Equal(t, "http://llm-relay-router.llm-relay.svc.cluster.local", regs.routerURL)
	assert.Equal(t, "llm-relay", regs.namespace)
	assert.Equal(t, 12*time.Hour, regs.tokenTTL)
}

// The defaults on the RETURNED struct are equally load-bearing (the
// flag-off path's byte-identity depends on them, and the orphaning bug
// also zeroed the namespace/TTL defaults the caller consumed).
func TestRegisterRelayFlags_DefaultsLandInReturnedStruct(t *testing.T) {
	fs := flag.NewFlagSet("probe-defaults", flag.ContinueOnError)
	regs := registerRelayFlagsInto(fs)
	require.NoError(t, fs.Parse(nil))

	assert.False(t, regs.enabled)
	assert.Equal(t, "llm-relay", regs.namespace,
		"the documented default must be the RETURNED struct's value, not just the usage text")
	assert.Equal(t, 24*time.Hour, regs.tokenTTL)
}
