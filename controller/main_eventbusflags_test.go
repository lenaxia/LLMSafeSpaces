// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

// main_eventbusflags_test.go — the design-0072 rollout pins for the
// controller's flag surface: registration must land in the RETURNED
// struct (the #1548 relay-flags orphaning posture) and boot validation
// must refuse the two malformed arms while admitting the authless one.

import (
	"flag"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestRegisterEventbusFlags_ParsesIntoReturnedStruct is the #1548
// posture pin for the eventbus wrapper: if registration ever re-orphans
// the parse targets (value return, field copy, shadowed struct), the
// parsed values stay here exactly as they did in the relay incident —
// the controller would run healthy with the bus silently dormant.
func TestRegisterEventbusFlags_ParsesIntoReturnedStruct(t *testing.T) {
	fs := flag.NewFlagSet("probe", flag.ContinueOnError)
	regs := registerEventbusFlagsInto(fs)
	require.NoError(t, fs.Parse([]string{
		"--workspace-eventbus-addr=llmsafespaces-valkey.llmsafespaces.svc.cluster.local:6379",
		"--workspace-eventbus-password-secret=llmsafespaces-credentials:redis-password",
	}))

	assert.Equal(t, "llmsafespaces-valkey.llmsafespaces.svc.cluster.local:6379", regs.addr,
		"the parsed address MUST land in the struct the caller reads")
	assert.Equal(t, "llmsafespaces-credentials:redis-password", regs.passwordSecret)
}

// TestRegisterEventbusFlags_DefaultsKeepBusDormant: the defaults are
// load-bearing — unset means no bus env in any pod, the pre-0072
// posture, byte-identically.
func TestRegisterEventbusFlags_DefaultsKeepBusDormant(t *testing.T) {
	fs := flag.NewFlagSet("probe-defaults", flag.ContinueOnError)
	regs := registerEventbusFlagsInto(fs)
	require.NoError(t, fs.Parse(nil))

	assert.Empty(t, regs.addr)
	assert.Empty(t, regs.passwordSecret)
}

// TestParseEventbusWiring pins every boot-validation arm: the two
// refusal shapes (a malformed NAME:KEY, a secret with no address) and
// the two legal shapes (unconfigured = dormant; address without secret
// = authless Redis, admitted).
func TestParseEventbusWiring(t *testing.T) {
	cases := []struct {
		name      string
		addr      string
		secretRef string
		wantName  string
		wantKey   string
		wantErr   bool
	}{
		{name: "unconfigured is the dormant default", addr: "", secretRef: "", wantName: "", wantKey: ""},
		{name: "addr without secret is legal (authless Redis)", addr: "some-redis:6379", secretRef: "", wantName: "", wantKey: ""},
		{name: "IPv6 literal carries no @ and is legal", addr: "[::1]:6379", secretRef: "", wantName: "", wantKey: ""},
		{name: "addr plus NAME:KEY splits", addr: "some-redis:6379", secretRef: "llmsafespaces-credentials:redis-password", wantName: "llmsafespaces-credentials", wantKey: "redis-password"},
		{name: "secret without addr refuses", addr: "", secretRef: "llmsafespaces-credentials:redis-password", wantErr: true},
		{name: "secret missing the KEY refuses", addr: "some-redis:6379", secretRef: "llmsafespaces-credentials:", wantErr: true},
		{name: "secret missing the NAME refuses", addr: "some-redis:6379", secretRef: ":redis-password", wantErr: true},
		{name: "secret with no colon at all refuses", addr: "some-redis:6379", secretRef: "llmsafespaces-credentials", wantErr: true},
		{name: "credentialed redis URL refuses (the password must ride the secret, never the addr)", addr: "redis://:sekrit@some-redis:6379", secretRef: "", wantErr: true},
		{name: "credentialed URL with secret also set still refuses (addr is checked first)", addr: "redis://agent:pw@some-redis:6379", secretRef: "creds:redis-password", wantErr: true},
		{name: "bare user:pass@host form refuses", addr: "agent:pw@some-redis:6379", secretRef: "", wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			name, key, err := parseEventbusWiring(tc.addr, tc.secretRef)
			if tc.wantErr {
				require.Error(t, err, "this misconfiguration must refuse startup, not limp armed")
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.wantName, name)
			assert.Equal(t, tc.wantKey, key)
		})
	}
}
