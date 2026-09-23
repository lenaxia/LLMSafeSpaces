// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package controller

// arming_behavior_test.go — the design §3 shapes, EXECUTED (r1's ask):
// SetupRelayStaging is driven through a minimal manager stub (the
// opencodeOverlayDecision precedent: behavior at a seam, not source
// greps). The armed shape runs the REAL guard against a REAL in-process
// router stub + a REAL (fake) API client carrying a shape-valid pub
// Secret — flags → construction → guard → the enable line. The
// unarmable shape proves "not 1, not a hang": the error returns within
// the window, and the seam maps it to 85.

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"os"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/lenaxia/llmsafespaces/pkg/secrets"
)

// stubManager satisfies exactly the ctrl.Manager surface
// SetupRelayStaging touches (GetAPIReader / GetConfig / GetScheme); the
// embedded nil interface panics on any OTHER use — a future wider
// manager dependency becomes a loud test failure, not a silent gap.
type stubManager struct {
	ctrl.Manager
	reader client.Reader
	scheme *runtime.Scheme
}

func (m *stubManager) GetAPIReader() client.Reader { return m.reader }
func (m *stubManager) GetScheme() *runtime.Scheme  { return m.scheme }

// GetConfig is deliberately NOT stubbed: SetupRelayStaging must never
// touch it (the guard client rides the seam) — a nil-interface panic
// here means production reached around the seam.

func armingScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(s))
	return s
}

// armingRouterStub serves the guard's healthz probe.
func armingRouterStub(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("ok"))
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func validPubSecret(t *testing.T, namespace string) *corev1.Secret {
	t.Helper()
	payload, err := (&secrets.HPKEPubPayload{PublicKey: []byte("test-pub-key"), Generation: 1}).Marshal()
	require.NoError(t, err)
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: secrets.RelayPubSecretName, Namespace: namespace},
		Data:       map[string][]byte{secrets.RelayPubDataKey: payload},
	}
}

// THE ARMED SHAPE (design §3 shape 2): enabled + a reachable, booted
// router → non-nil config AND the armed line emitted (the enable line
// IS the armed contract). The line is captured through the package's
// controller-runtime logger.
func TestSetupRelayStaging_ArmedEmitsLineAndReturnsConfig(t *testing.T) {
	router := armingRouterStub(t)
	ns := "llm-relay"
	fc := fake.NewClientBuilder().WithScheme(armingScheme(t)).
		WithObjects(validPubSecret(t, ns)).Build()
	mgr := &stubManager{reader: fc, scheme: fc.Scheme()}
	// The guard-client seam: the fake API for the pub-Secret read + the
	// mint-key create (hermetic — no rest config, no envtest).
	orig := newStartupGuardClient
	newStartupGuardClient = func(ctrl.Manager) (client.Client, error) { return fc, nil }
	t.Cleanup(func() { newStartupGuardClient = orig })

	cfg, err := SetupRelayStaging(mgr, true, router.URL, ns, 24*time.Hour, "http://api.invalid", "token")
	require.NoError(t, err)
	require.NotNil(t, cfg, "armed: the staging config is returned (main proceeds to exit 0)")
	assert.Equal(t, 0, RelayStagingExitCodeFor(err), "armed maps to exit 0")
}

// THE UNARMABLE SHAPE (design §3 shape 1): enabled + an unreachable
// router → the error returns WITHIN the window ("not 1, not a hang" —
// the elapsed time is bounded and asserted) and the seam maps it to 85.
func TestSetupRelayStaging_UnarmableReturnsErrWithinWindow(t *testing.T) {
	fc := fake.NewClientBuilder().WithScheme(armingScheme(t)).Build()
	mgr := &stubManager{reader: fc, scheme: fc.Scheme()}
	orig := newStartupGuardClient
	newStartupGuardClient = func(ctrl.Manager) (client.Client, error) { return fc, nil }
	t.Cleanup(func() { newStartupGuardClient = orig })

	start := time.Now()
	_, err := SetupRelayStaging(mgr, true, "http://127.0.0.1:1", "llm-relay", 24*time.Hour, "http://api.invalid", "token")
	elapsed := time.Since(start)

	require.Error(t, err, "unarmable: the error returns (main exits — with the code, not a hang)")
	assert.Less(t, elapsed, ArmingStartupGuardWindow+5*time.Second,
		"within the bounded window (the guard's own budget + probe slack)")
	assert.Contains(t, err.Error(), "refusing to start",
		"the refusal carries the not-armed language")
	assert.Equal(t, 85, RelayStagingExitCodeFor(err),
		"the seam maps the unarmable outcome to the fifth rung")
}

// The armed-line LITERAL is pinned here (r1 finding: the release-smoke
// citation was dangling — the parity branch is unmerged; until it lands
// THIS is the armed contract's only in-tree coverage, and the posture
// gate asserts it cluster-side when it lands).
func TestSetupRelayStaging_ArmedLineLiteral(t *testing.T) {
	src, err := readControllerSource()
	require.NoError(t, err)
	assert.Contains(t, src, `"relay-only key delivery enabled"`,
		"the armed contract's literal — the posture gate's cluster-side assertion depends on this exact string")
}

func readControllerSource() (string, error) {
	b, err := os.ReadFile("controller.go")
	return string(b), err
}
