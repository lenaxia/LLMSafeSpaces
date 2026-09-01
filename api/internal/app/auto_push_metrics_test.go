// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"

	"github.com/lenaxia/llmsafespaces/api/internal/services/agentpush"
	"github.com/lenaxia/llmsafespaces/api/internal/services/metrics"
)

// stubResolverAdapter satisfies agentpush.PodIPResolver by returning a
// fixed IP (or empty for the no_pod case).
type stubResolverAdapter struct{ ip string }

func (s *stubResolverAdapter) GetWorkspacePodIP(_ context.Context, _, _ string) (string, error) {
	return s.ip, nil
}

// stubPasswordProviderAdapter satisfies agentpush.PasswordProvider —
// agentd enforces Basic auth on the user mux (#848), so Notify needs one.
type stubPasswordProviderAdapter struct{}

func (stubPasswordProviderAdapter) WorkspacePassword(_ context.Context, _ string) (string, error) {
	return "pw", nil
}

// newLoopbackClient returns an *http.Client whose transport short-
// circuits every request with the given status + body. Lets us drive
// agentpush.Service.Notify without a real HTTP server.
func newLoopbackClient(status int, body string) *http.Client {
	return &http.Client{Transport: roundTripFunc(func(_ *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: status,
			Body:       io.NopCloser(strings.NewReader(body)),
			Header:     make(http.Header),
		}, nil
	})}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// TestClassifyNotifyOutcome maps agentpush.Notify errors to metric
// labels. This mapping is the sole source of truth for
// api_secret_auto_push_total outcomes — a bug here would silently
// misclassify production incidents.
func TestClassifyNotifyOutcome(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want string
	}{
		{"nil error is success", nil, "success"},
		{"no running pod", agentpush.ErrNoRunningPod, "no_pod"},
		{
			"wrapped no-running-pod",
			fmt.Errorf("post-agentd: %w", agentpush.ErrNoRunningPod),
			"no_pod",
		},
		{
			"notify failure (agentd 502)",
			errors.New("agent resync failed: pull_failed"),
			"notify_failed",
		},
		{
			"notify failure (transport error)",
			errors.New("failed to reach workspace agent: connection refused"),
			"notify_failed",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := classifyPushOutcome(tc.err)
			assert.Equal(t, tc.want, got)
		})
	}
}

// TestWsAgentPusherAdapter_EmitsMetricOnEveryNotify proves that every
// call to the adapter increments api_secret_auto_push_total exactly
// once with the correct outcome label. The metric is process-global
// (Prometheus counters can't be scoped to a test), so we reset it and
// read deltas.
//
// This is the load-bearing regression test for the bot-caught bug: the
// old wiring put the outcome hook on the shared pusher, causing
// SetBindings pushes to also increment the counter. Now the metric is
// emitted ONLY from this adapter (which is only invoked from the
// pod-recreation auto-push path).
func TestWsAgentPusherAdapter_EmitsMetricOnEveryNotify(t *testing.T) {
	counter := metrics.SecretAutoPushCounter()
	counter.Reset()

	// Success case: metric should increment {outcome="success"} by 1.
	adapter := &wsAgentPusherAdapter{
		pusher: agentpush.New(
			agentpush.WithPodIPResolver(&stubResolverAdapter{ip: "10.0.0.5"}),
			agentpush.WithPasswordProvider(stubPasswordProviderAdapter{}),
			agentpush.WithHTTPClient(newLoopbackClient(200, `{"status":"applied","appliedRev":"2:a:b"}`)),
		),
	}
	err := adapter.Push(context.Background(), "user", "ws")
	assert.NoError(t, err)
	got := testutil.ToFloat64(counter.WithLabelValues("success"))
	assert.Equal(t, 1.0, got, "success outcome must have incremented once")

	// no_pod case (empty pod IP).
	counter.Reset()
	adapter = &wsAgentPusherAdapter{
		pusher: agentpush.New(
			agentpush.WithPodIPResolver(&stubResolverAdapter{ip: ""}),
		),
	}
	err = adapter.Push(context.Background(), "user", "ws")
	assert.ErrorIs(t, err, agentpush.ErrNoRunningPod)
	assert.Equal(t, 1.0, testutil.ToFloat64(counter.WithLabelValues("no_pod")))
	assert.Equal(t, 0.0, testutil.ToFloat64(counter.WithLabelValues("success")),
		"no_pod outcome must NOT increment success")
}

// TestSharedNotifier_DoesNotEmitAutoPushMetric proves that when the
// shared *agentpush.Service is used directly (i.e. from SecretsHandler's
// SetBindings/ReloadSecrets/revoke paths, NOT via the
// wsAgentPusherAdapter), api_secret_auto_push_total is NOT incremented.
// This locks in the metric's documented meaning ("pod-identity
// transitions on workspace status polls") against future accidental
// re-wiring.
func TestSharedNotifier_DoesNotEmitAutoPushMetric(t *testing.T) {
	counter := metrics.SecretAutoPushCounter()
	counter.Reset()

	shared := agentpush.New(
		agentpush.WithPodIPResolver(&stubResolverAdapter{ip: "10.0.0.5"}),
		agentpush.WithPasswordProvider(stubPasswordProviderAdapter{}),
		agentpush.WithHTTPClient(newLoopbackClient(200, `{"status":"applied","appliedRev":"2:a:b"}`)),
	)
	_, err := shared.Notify(context.Background(), "user", "ws")
	assert.NoError(t, err)

	for _, outcome := range []string{"success", "no_pod", "notify_failed"} {
		assert.Equal(t, 0.0, testutil.ToFloat64(counter.WithLabelValues(outcome)),
			"shared notifier must NOT increment %s outcome", outcome)
	}
}
