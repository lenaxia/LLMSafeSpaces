// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package chart_test

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// #1332 — the controller's --api-public-url flag (Helm value
// controller.apiPublicURL) wires the PUBLIC API origin into workspace
// pods as LLMSAFESPACE_API_PUBLIC_URL. Optional by design: when unset
// and preview origins are enabled the controller derives
// https://api.<baseDomain>. These tests pin the render contract both
// ways — absent by default, exact value when set.

// TestControllerArgs_ApiPublicURL_AbsentByDefault: the flag renders only
// when the operator sets it (empty derivation happens in the controller
// binary, not the chart — the chart never guesses an origin).
func TestControllerArgs_ApiPublicURL_AbsentByDefault(t *testing.T) {
	docs := helmTemplate(t, "")
	args := findControllerArgs(t, docs)
	for _, a := range args {
		require.NotContains(t, a, "--api-public-url",
			"flag must not render without controller.apiPublicURL, got %q", a)
	}
}

// TestControllerArgs_ApiPublicURL_RendersWhenSet: the exact operator
// value reaches the controller args verbatim.
func TestControllerArgs_ApiPublicURL_RendersWhenSet(t *testing.T) {
	docs := helmTemplate(t, "controller:\n  apiPublicURL: \"https://api.example.com\"\n")
	args := findControllerArgs(t, docs)
	var found string
	for _, a := range args {
		if strings.HasPrefix(a, "--api-public-url=") {
			found = a
			break
		}
	}
	require.Equal(t, "--api-public-url=https://api.example.com", found,
		"controller.apiPublicURL must render verbatim as --api-public-url")
}
