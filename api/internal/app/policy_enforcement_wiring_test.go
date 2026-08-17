// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package app

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lenaxia/llmsafespaces/api/internal/handlers"
	"github.com/lenaxia/llmsafespaces/api/internal/services/policy"
	k8smocks "github.com/lenaxia/llmsafespaces/mocks/kubernetes"
	pkginterfaces "github.com/lenaxia/llmsafespaces/pkg/interfaces"
)

// TestWirePolicyEnforcement_ReachesProxyHandler is the wiring pin the #912
// round-2 review demanded: SetModelPolicyChecker previously had zero
// production call sites, so the per-prompt org-policy gate was fail-open
// dead code in every deployed replica while its handler tests passed.
// This test fails if the app-level wiring drops the proxy leg again.
func TestWirePolicyEnforcement_ReachesProxyHandler(t *testing.T) {
	policySvc := policy.New(nil, nil) // store+cache nil is a supported degraded mode

	proxyHandler, err := handlers.NewProxyHandler(k8smocks.NewMockKubernetesClient(), nopLogger{}, "default", nil, nil)
	require.NoError(t, err)
	modelsHandler := handlers.NewModelsHandler(nil)

	// Pre-wiring: enforcement off.
	assert.False(t, proxyHandler.HasModelPolicyChecker(),
		"sanity: fresh handler must be unwired")

	wirePolicyEnforcement(policySvc, modelsHandler, proxyHandler)

	assert.True(t, proxyHandler.HasModelPolicyChecker(),
		"wiring must reach the PROXY handler — dropping this line reverts the prompt-path enforcement to fail-open dead code")
}

// TestWirePolicyEnforcement_NilPolicy_Noop: org policies disabled must not
// wire anything (fail-open by configuration, not by accident).
func TestWirePolicyEnforcement_NilPolicy_Noop(t *testing.T) {
	proxyHandler, err := handlers.NewProxyHandler(k8smocks.NewMockKubernetesClient(), nopLogger{}, "default", nil, nil)
	require.NoError(t, err)

	wirePolicyEnforcement(nil, nil, proxyHandler)

	assert.False(t, proxyHandler.HasModelPolicyChecker())
}

// nopLogger satisfies pkginterfaces.LoggerInterface minimally — NewProxyHandler
// requires a non-nil logger. Tests here never trigger a log call.
type nopLogger struct{}

func (nopLogger) Debug(string, ...interface{})                        {}
func (nopLogger) Info(string, ...interface{})                         {}
func (nopLogger) Warn(string, ...interface{})                         {}
func (nopLogger) Error(string, error, ...interface{})                 {}
func (nopLogger) Fatal(string, error, ...interface{})                 {}
func (l nopLogger) With(...interface{}) pkginterfaces.LoggerInterface { return l }
func (nopLogger) Sync() error                                         { return nil }
