// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package metrics

import (
	"os"
	"testing"
	"time"

	"github.com/lenaxia/llmsafespaces/api/internal/logger"
	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
	"github.com/stretchr/testify/assert"
)

var metricsService *Service

func TestMain(m *testing.M) {
	log, _ := logger.New(true, "debug", "console")
	metricsService = New(log)
	os.Exit(m.Run())
}

func TestNew_AllCountersInitialised(t *testing.T) {
	assert.NotNil(t, metricsService.requestCounter)
	assert.NotNil(t, metricsService.requestDuration)
	assert.NotNil(t, metricsService.responseSize)
	assert.NotNil(t, metricsService.activeConnections)
	assert.NotNil(t, metricsService.workspacesCreated)
	assert.NotNil(t, metricsService.workspacesTerminated)
	assert.NotNil(t, metricsService.errorsTotal)
	assert.NotNil(t, metricsService.resourceUsage)
}

func TestStart(t *testing.T) {
	assert.NoError(t, metricsService.Start())
}

func TestStop(t *testing.T) {
	assert.NoError(t, metricsService.Stop())
}

func TestRecordRequest(t *testing.T) {
	metricsService.RecordRequest("GET", "/api/v1/workspaces", 200, 100*time.Millisecond, 1024)

	metric, err := metricsService.requestCounter.GetMetricWithLabelValues("GET", "/api/v1/workspaces", "200")
	assert.NoError(t, err)
	assert.Equal(t, 1.0, counterValue(metric))

	obs, err := metricsService.requestDuration.GetMetricWithLabelValues("GET", "/api/v1/workspaces")
	assert.NoError(t, err)
	assert.InDelta(t, 0.1, histogramSum(obs.(prometheus.Histogram)), 0.01)

	obs, err = metricsService.responseSize.GetMetricWithLabelValues("GET", "/api/v1/workspaces")
	assert.NoError(t, err)
	assert.InDelta(t, 1024.0, histogramSum(obs.(prometheus.Histogram)), 0.1)
}

func TestRecordError(t *testing.T) {
	metricsService.RecordError("api_error", "/api/v1/workspaces", "404")

	metric, err := metricsService.errorsTotal.GetMetricWithLabelValues("api_error", "/api/v1/workspaces", "404")
	assert.NoError(t, err)
	assert.Equal(t, 1.0, counterValue(metric))
}

func TestRecordResourceUsage(t *testing.T) {
	metricsService.RecordResourceUsage("sandbox-123", 0.5, 1024*1024*1024)

	cpu, err := metricsService.resourceUsage.GetMetricWithLabelValues("sandbox-123", "cpu")
	assert.NoError(t, err)
	assert.Equal(t, 0.5, gaugeValue(cpu))

	mem, err := metricsService.resourceUsage.GetMetricWithLabelValues("sandbox-123", "memory")
	assert.NoError(t, err)
	assert.Equal(t, float64(1024*1024*1024), gaugeValue(mem))
}

func TestIncrementDecrementActiveConnections(t *testing.T) {
	metricsService.activeConnections.Reset()
	metricsService.IncrementActiveConnections("websocket", "user-99")
	metricsService.IncrementActiveConnections("websocket", "user-99")
	metricsService.DecrementActiveConnections("websocket", "user-99")

	metric, err := metricsService.activeConnections.GetMetricWithLabelValues("websocket", "user-99")
	assert.NoError(t, err)
	assert.Equal(t, 1.0, gaugeValue(metric))
}

func TestRecordRequest_DifferentStatuses(t *testing.T) {
	for _, status := range []int{200, 400, 500} {
		metricsService.RecordRequest("POST", "/api/v1/workspaces", status, time.Millisecond, 0)
	}
	for _, status := range []string{"200", "400", "500"} {
		m, err := metricsService.requestCounter.GetMetricWithLabelValues("POST", "/api/v1/workspaces", status)
		assert.NoError(t, err)
		assert.GreaterOrEqual(t, counterValue(m), 1.0)
	}
}

// helpers

func counterValue(m prometheus.Metric) float64 {
	var d dto.Metric
	if err := m.Write(&d); err != nil {
		return 0
	}
	return d.Counter.GetValue()
}

func gaugeValue(m prometheus.Metric) float64 {
	var d dto.Metric
	if err := m.Write(&d); err != nil {
		return 0
	}
	return d.Gauge.GetValue()
}

func histogramSum(h prometheus.Histogram) float64 {
	var d dto.Metric
	if err := h.Write(&d); err != nil {
		return 0
	}
	return d.Histogram.GetSampleSum()
}

func TestRecordAuthFailure_IncrementsCounter(t *testing.T) {
	pre := getCounterValue(authFailuresTotal.With(prometheus.Labels{"reason": "invalid_token"}))
	RecordAuthFailure("invalid_token")
	post := getCounterValue(authFailuresTotal.With(prometheus.Labels{"reason": "invalid_token"}))
	assert.Equal(t, pre+1, post)
}

func TestRecordSessionCompleted_ObservesHistogram(t *testing.T) {
	// Histogram observe shouldn't panic and sum should increase.
	// workspace_id label removed for cardinality reasons; histogram is unlabelled.
	pre := getHistogramSum(sessionDurationSeconds.With(prometheus.Labels{}))
	metricsService.RecordSessionCompleted("ws-test", 42.5)
	post := getHistogramSum(sessionDurationSeconds.With(prometheus.Labels{}))
	assert.Greater(t, post, pre, "session duration histogram sum must increase")
}

func getHistogramSum(h prometheus.Observer) float64 {
	// Observer doesn't expose Write directly; use type assertion.
	if hv, ok := h.(prometheus.Histogram); ok {
		var d dto.Metric
		if err := hv.Write(&d); err != nil {
			return 0
		}
		return d.Histogram.GetSampleSum()
	}
	return 0
}

// getCounterValue reads the current value of a prometheus counter.
func getCounterValue(c prometheus.Counter) float64 {
	var d dto.Metric
	if err := c.Write(&d); err != nil {
		return 0
	}
	return d.Counter.GetValue()
}

func TestRecordUploadRequest_IncrementsCounter(t *testing.T) {
	pre := getCounterValue(uploadsTotal.With(prometheus.Labels{"reason": "cap"}))
	RecordUploadRequest("cap")
	post := getCounterValue(uploadsTotal.With(prometheus.Labels{"reason": "cap"}))
	assert.Equal(t, pre+1, post)
}

func TestRecordUploadRequest_EmptyReasonCountsUnknown(t *testing.T) {
	pre := getCounterValue(uploadsTotal.With(prometheus.Labels{"reason": "unknown"}))
	RecordUploadRequest("")
	post := getCounterValue(uploadsTotal.With(prometheus.Labels{"reason": "unknown"}))
	assert.Equal(t, pre+1, post)
}
