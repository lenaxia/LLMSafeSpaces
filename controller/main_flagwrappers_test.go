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

// TestRegisterRelayFlags_WrapperBindsTheCommandLineItReturns is the
// caller-seam pin of the #1548 orphaning class. The Into-seam twins
// guard registerRelayFlagsInto against a value-return regression, but
// main captures `relayFlags := registerRelayFlags()` BEFORE flag.Parse
// parses flag.CommandLine — if the wrapper ever stopped registering
// into flag.CommandLine (its own FlagSet, a copied struct), Parse
// would write a dead set while the caller reads zeros: the exact
// production split-brain, invisible to every unit pin that builds a
// fresh FlagSet. Only the live TTL probe/gate would catch it today.
//
// The pin swaps in a fresh flag.CommandLine (saved/restored), walks
// the REAL wrapper, and proves the parse targets and the returned
// struct are the same memory the process-boot Parse writes.
func TestRegisterRelayFlags_WrapperBindsTheCommandLineItReturns(t *testing.T) {
	saved := flag.CommandLine
	t.Cleanup(func() { flag.CommandLine = saved })
	flag.CommandLine = flag.NewFlagSet("wrapper-probe", flag.ContinueOnError)

	regs := registerRelayFlags()

	for _, name := range []string{
		"relay-only-key-delivery", "llm-relay-router-url",
		"llm-relay-namespace", "relay-token-ttl",
	} {
		assert.NotNil(t, flag.CommandLine.Lookup(name),
			"the wrapper must register %s into flag.CommandLine — main's flag.Parse() reads that set", name)
	}
	require.NoError(t, flag.CommandLine.Parse([]string{
		"--relay-only-key-delivery=true",
		"--llm-relay-router-url=http://llm-relay-router.llm-relay.svc.cluster.local",
		"--llm-relay-namespace=llm-relay",
		"--relay-token-ttl=12h",
	}))
	assert.True(t, regs.enabled,
		"the wrapper's RETURNED struct must be what flag.CommandLine's Parse writes — the caller-seam orphaning is #1548's class")
	assert.Equal(t, "http://llm-relay-router.llm-relay.svc.cluster.local", regs.routerURL)
	assert.Equal(t, "llm-relay", regs.namespace)
	assert.Equal(t, 12*time.Hour, regs.tokenTTL)
}

// TestRegisterFreeModelsFlags_WrapperBindsTheCommandLineItReturns is
// the free-models twin of the caller-seam pin: registerFreeModelsFlags
// must register into flag.CommandLine and return the struct those
// bindings write, or --enable-free-models-refresher=false parses into
// a dead set while main reads the default-true (#1546 Defect 3
// re-armed at the wrapper seam).
func TestRegisterFreeModelsFlags_WrapperBindsTheCommandLineItReturns(t *testing.T) {
	saved := flag.CommandLine
	t.Cleanup(func() { flag.CommandLine = saved })
	flag.CommandLine = flag.NewFlagSet("wrapper-probe-fm", flag.ContinueOnError)

	regs := registerFreeModelsFlags()

	for _, name := range []string{"enable-free-models-refresher", "free-models-refresh-interval"} {
		assert.NotNil(t, flag.CommandLine.Lookup(name),
			"the wrapper must register %s into flag.CommandLine — main's flag.Parse() reads that set", name)
	}
	require.NoError(t, flag.CommandLine.Parse([]string{
		"--enable-free-models-refresher=false",
		"--free-models-refresh-interval=1h",
	}))
	assert.False(t, regs.enabled,
		"the wrapper's RETURNED struct must be what flag.CommandLine's Parse writes — else the disable flag is ignored (#1546 Defect 3)")
	assert.Equal(t, time.Hour, regs.interval)
}
