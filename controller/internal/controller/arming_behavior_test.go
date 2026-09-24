// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package controller

// arming_behavior_test.go — the design §3 shapes, EXECUTED (r1's ask):
// SetupRelayStaging driven through a minimal manager stub + a
// guard-client DI seam, hermetically (no envtest, no cluster).

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"

	"github.com/lenaxia/llmsafespaces/pkg/secrets"
)

// stubManager satisfies exactly the ctrl.Manager surface
// SetupRelayStaging touches (GetAPIReader / GetScheme); the embedded
// nil interface panics on any OTHER use — a future wider manager
// dependency becomes a loud test failure, not a silent gap. GetConfig
// is deliberately NOT stubbed: the guard client rides the DI seam, and
// a nil-interface panic there means production reached around it.
type stubManager struct {
	ctrl.Manager
	reader client.Reader
	scheme *runtime.Scheme
}

func (m *stubManager) GetAPIReader() client.Reader { return m.reader }
func (m *stubManager) GetScheme() *runtime.Scheme  { return m.scheme }

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

// withGuardClient swaps the guard-client DI seam to the fake client.
func withGuardClient(t *testing.T, fc client.Client) {
	t.Helper()
	orig := newStartupGuardClient
	newStartupGuardClient = func(ctrl.Manager) (client.Client, error) { return fc, nil }
	t.Cleanup(func() { newStartupGuardClient = orig })
}

// THE ARMED SHAPE (design §3 shape 2): enabled + a reachable, booted
// router → non-nil config, exit-0 through the seam, AND the armed line
// EMITTED — captured at a real sink. [r3 correction: the r2 round
// claimed this capture "impossible" on a fabricated empirical record —
// one flawed probe (a hand-rolled logr sink) recorded as an
// impossibility proof; the reviewer reproduced the capture twice with
// the controller-runtime zap adapter. This test is THEIR approach.]
func TestSetupRelayStaging_ArmedReturnsConfigAndEmitsLine(t *testing.T) {
	var buf bytes.Buffer
	logf.SetLogger(zap.New(zap.WriteTo(&buf)))

	router := armingRouterStub(t)
	ns := "llm-relay"
	fc := fake.NewClientBuilder().WithScheme(armingScheme(t)).
		WithObjects(validPubSecret(t, ns)).Build()
	mgr := &stubManager{reader: fc, scheme: fc.Scheme()}
	withGuardClient(t, fc)

	cfg, err := SetupRelayStaging(mgr, true, router.URL, ns, 24*time.Hour, "http://api.invalid", "token")
	require.NoError(t, err)
	require.NotNil(t, cfg, "armed: the staging config is returned (main proceeds to exit 0)")
	assert.Equal(t, 0, RelayStagingExitCodeFor(err), "armed maps to exit 0")
	assert.Contains(t, buf.String(), `"msg":"relay-only key delivery enabled"`,
		"the armed line IS emitted at runtime (the armed contract, captured at the sink)")
}

// THE UNARMABLE SHAPE (design §3 shape 1): enabled + an unreachable
// router → a PROMPT error (the probe is single-shot, 5s client
// timeout) and the seam maps it to 85. The literal <35s is a
// HANG-GUARD only (r4: the dead port returns in ~ms, so this bound
// passes regardless of the window's value — it proves "not a hang",
// NOT the window's budget; the window's 30s value is enforced by the
// source pin in local/m1_arming_source_test.go).
func TestSetupRelayStaging_UnarmableReturnsErrWithinWindow(t *testing.T) {
	fc := fake.NewClientBuilder().WithScheme(armingScheme(t)).Build()
	mgr := &stubManager{reader: fc, scheme: fc.Scheme()}
	withGuardClient(t, fc)

	start := time.Now()
	_, err := SetupRelayStaging(mgr, true, "http://127.0.0.1:1", "llm-relay", 24*time.Hour, "http://api.invalid", "token")
	elapsed := time.Since(start)

	require.Error(t, err, "unarmable: the error returns (main exits — with the code, not a hang)")
	assert.Less(t, elapsed, 35*time.Second,
		"within the design's bounded 30s window + probe slack (the guard's context budget)")
	assert.Contains(t, err.Error(), "refusing to start",
		"the refusal carries the not-armed language")
	assert.Equal(t, 85, RelayStagingExitCodeFor(err),
		"the seam maps the unarmable outcome to the fifth rung")
}

// The armed-line STRUCTURAL pin (r2 finding 1: the literal must live
// INSIDE SetupRelayStaging's body, AFTER the guard's success — deletion
// AND relocation both fail; a bare file-level grep catches only
// deletion). The parity branch's release-smoke markers do not exist on
// main; until they or the posture gate land, THIS is the armed
// contract's only in-tree coverage.
func TestSetupRelayStaging_ArmedLineLivesInArmedPath(t *testing.T) {
	src, err := os.ReadFile("controller.go")
	require.NoError(t, err)
	fnStart := strings.Index(string(src), "func SetupRelayStaging(")
	require.GreaterOrEqual(t, fnStart, 0, "SetupRelayStaging not found")
	fnEnd := strings.Index(string(src)[fnStart:], "\nfunc ")
	body := string(src)[fnStart : fnStart+fnEnd]
	assert.Contains(t, body, `"relay-only key delivery enabled"`,
		"the armed line lives in SetupRelayStaging's body")
	guardIdx := strings.Index(body, "ValidateRelayStagingStartup")
	lineIdx := strings.Index(body, `"relay-only key delivery enabled"`)
	if guardIdx >= 0 && lineIdx >= 0 {
		assert.Greater(t, lineIdx, guardIdx,
			"the armed line is emitted only AFTER the guard succeeds (the armed conjunction's order)")
	}
}
