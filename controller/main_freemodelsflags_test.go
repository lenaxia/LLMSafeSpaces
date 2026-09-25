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

// TestRegisterFreeModelsFlags_ParsesIntoReturnedStruct is the
// registerFreeModelsFlags twin of the #1548 root-cause pin
// (main_relayflags_test.go): the SAME orphaning class — value return
// with flag.BoolVar/DurationVar bound into the local — shipped here
// for months while the relay pair's fix landed one lane over (the r2
// finding on #1566: --enable-free-models-refresher=false parsed into
// a dead struct, the refresher ran anyway, and the chart's withheld
// configmap-write grant turned every refresh into a forbidden write —
// #1546 Defect 3's exact failure mode, re-armed).
//
// The pin parses a FRESH FlagSet against the RETURNED struct: if the
// registration ever re-orphaned the parse targets, enabled stays true
// and interval stays 6h here exactly as it did in production.
func TestRegisterFreeModelsFlags_ParsesIntoReturnedStruct(t *testing.T) {
	fs := flag.NewFlagSet("probe", flag.ContinueOnError)
	fm := registerFreeModelsFlagsInto(fs)
	require.NoError(t, fs.Parse([]string{
		"--enable-free-models-refresher=false",
		"--free-models-refresh-interval=1h",
	}))

	assert.False(t, fm.enabled,
		"the parsed disable MUST land in the struct the caller reads — the value-copy orphaning re-arms #1546 Defect 3 (the refresher runs against a withheld grant)")
	assert.Equal(t, time.Hour, fm.interval,
		"the parsed interval MUST land in the struct the caller reads")
}

// The defaults on the RETURNED struct are load-bearing (main's
// flag-true path consumes them; the orphaning zeroed them too).
func TestRegisterFreeModelsFlags_DefaultsLandInReturnedStruct(t *testing.T) {
	fs := flag.NewFlagSet("probe-defaults", flag.ContinueOnError)
	fm := registerFreeModelsFlagsInto(fs)
	require.NoError(t, fs.Parse(nil))

	assert.True(t, fm.enabled)
	assert.Equal(t, 6*time.Hour, fm.interval)
}
