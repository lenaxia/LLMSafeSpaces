// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package handlers

import (
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lenaxia/llmsafespaces/api/internal/services/metrics"
	v1 "github.com/lenaxia/llmsafespaces/pkg/apis/llmsafespaces/v1"
	pkginterfaces "github.com/lenaxia/llmsafespaces/pkg/interfaces"
)

// LLMSafeSpaces#488: doProxy and doHistoryRequest silently pass through
// upstream 5xx status codes to the client with no server-side observability
// (no log line, no metric). This makes incidents where opencode returns
// 5xx (like #486's ConfigInvalidError) invisible in Grafana/logs and forces
// operators to kubectl-exec + curl inside a workspace pod to diagnose.
//
// These tests are the regression harness for the fix:
//   1. On upstream 5xx, a Warn log fires with workspaceID/path/upstreamStatus/bodyPreview.
//   2. On upstream 5xx, the package-level counter `api_upstream_5xx_total`
//      is incremented with the correct label values.
//   3. Legacy behavior unchanged: 2xx responses do NOT touch the counter
//      and do NOT emit the 5xx-specific log line. 4xx status codes are
//      likewise out of scope for this signal by design — the
//      middleware's api_requests_total{status} already tracks 4xx
//      volume; a separate 4xx counter here would be duplication.
//      Only 2xx is negatively tested below.

// proxyCaptureLogger implements pkginterfaces.LoggerInterface but stores
// every call for later assertion. Renamed from a shorter identifier to
// avoid collision with the emailHandler test's simpler captureLogger.
type proxyCaptureLogger struct {
	mu     sync.Mutex
	warns  []proxyLoggedLine
	errors []proxyLoggedLine
	infos  []proxyLoggedLine
	debugs []proxyLoggedLine
}

type proxyLoggedLine struct {
	msg    string
	err    error
	fields map[string]interface{}
}

func kvToMap(kv []interface{}) map[string]interface{} {
	m := make(map[string]interface{}, len(kv)/2)
	for i := 0; i+1 < len(kv); i += 2 {
		if k, ok := kv[i].(string); ok {
			m[k] = kv[i+1]
		}
	}
	return m
}

func (l *proxyCaptureLogger) Debug(msg string, kv ...interface{}) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.debugs = append(l.debugs, proxyLoggedLine{msg: msg, fields: kvToMap(kv)})
}
func (l *proxyCaptureLogger) Info(msg string, kv ...interface{}) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.infos = append(l.infos, proxyLoggedLine{msg: msg, fields: kvToMap(kv)})
}
func (l *proxyCaptureLogger) Warn(msg string, kv ...interface{}) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.warns = append(l.warns, proxyLoggedLine{msg: msg, fields: kvToMap(kv)})
}
func (l *proxyCaptureLogger) Error(msg string, err error, kv ...interface{}) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.errors = append(l.errors, proxyLoggedLine{msg: msg, err: err, fields: kvToMap(kv)})
}
func (l *proxyCaptureLogger) Fatal(msg string, err error, kv ...interface{}) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.errors = append(l.errors, proxyLoggedLine{msg: "FATAL: " + msg, err: err, fields: kvToMap(kv)})
}
func (l *proxyCaptureLogger) With(kv ...interface{}) pkginterfaces.LoggerInterface { return l }
func (l *proxyCaptureLogger) Sync() error                                          { return nil }

// findWarn returns the first Warn log entry whose msg contains the substring.
func (l *proxyCaptureLogger) findWarn(substr string) *proxyLoggedLine {
	l.mu.Lock()
	defer l.mu.Unlock()
	for i := range l.warns {
		if strings.Contains(l.warns[i].msg, substr) {
			return &l.warns[i]
		}
	}
	return nil
}

// counterValue reads the current value of a CounterVec label combination.
// Returns 0 if the combination has never been observed.
func counterValue(t *testing.T, cv *prometheus.CounterVec, labels ...string) float64 {
	t.Helper()
	m, err := cv.GetMetricWithLabelValues(labels...)
	require.NoError(t, err)
	var out dto.Metric
	require.NoError(t, m.Write(&out))
	if out.Counter == nil || out.Counter.Value == nil {
		return 0
	}
	return *out.Counter.Value
}

// TestGetHistory_Upstream5xx_LogsWarnAndRecordsMetric was deleted: the history arm of the api_upstream_5xx_total counter died with doHistoryRequest (#828 batch 2). Flagged delta: the adapter path logs structured errors but records no Prometheus counter; the counter survives for the still-legacy transport routes (question/permission, and the seam) until #828 final batch. Follow-up filed in the PR body.

// TestGetHistory_Upstream2xx_DoesNotLogWarnOrRecordMetric was deleted: negative row for the deleted doHistoryRequest instrumentation arm.

// TestDoProxy_Upstream5xx_LogsWarnAndRecordsMetric asserts the fix for
// #488 for the streaming proxy code path (doProxy) — used by the
// still-legacy raw-proxy routes (question/permission, and the test
// seams); the send/session/history routes are adapter-served (#828
// batches 1+2) and never hit doProxy.
//
// Uses the /session POST endpoint (a write proxy route) with a mock
// upstream returning 500. The doProxy code path streams the response, so
// bodyPreview is best-effort; the test asserts the log fires with the
// right labeled fields and the counter increments correctly.
func TestDoProxy_Upstream5xx_LogsWarnAndRecordsMetric(t *testing.T) {
	env := newTestEnvWithBackend(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadGateway) // any 5xx
		_, _ = w.Write([]byte(`{"name":"UnknownError","data":{"ref":"err_xyz00001"}}`))
	})
	env.setupWorkspacePodWithT(t, "ws-write-5xx", "10.0.0.3", string(v1.WorkspacePhaseActive), "ws-write-5xx")
	env.setupPasswordWithT(t, "ws-write-5xx", "test-password")
	env.setupWorkspaceWithT(t, "ws-write-5xx", 5)

	cap := &proxyCaptureLogger{}
	env.handler.logger = cap

	upstream5xxTotalReset(t)

	w := env.doRequestWithT(t, "POST",
		"/api/v1/workspaces/ws-write-5xx/legacy-message/ses_1", strings.NewReader(`{}`))

	// Client sees the upstream status (either passed through or wrapped
	// as a 5xx; both are >= 500 and both should have observability).
	require.GreaterOrEqual(t, w.Code, 500)

	line := cap.findWarn("Upstream 5xx")
	require.NotNil(t, line, "expected Warn log line; got warns=%+v", cap.warns)
	assert.Equal(t, "ws-write-5xx", line.fields["workspaceID"])
	assert.Equal(t, 502, line.fields["upstreamStatus"])

	got := counterValue(t, metrics.Upstream5xxCounter(), "ws-write-5xx", "/session/:id/message", "502")
	assert.Equal(t, 1.0, got,
		"upstream_5xx counter must fire for the streaming proxy path too, "+
			"not just the history path")
}

// upstream5xxTotalReset clears the counter for a clean per-test baseline.
// Prometheus counters are process-global; tests must Reset() to isolate.
// The counter must exist by the time tests run — a build-time miss on
// the identifier is the RED signal the fix hasn't landed yet.
func upstream5xxTotalReset(t *testing.T) {
	t.Helper()
	metrics.Upstream5xxCounter().Reset()
}
