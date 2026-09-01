// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package metrics

import (
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
)

// TestRecordSecretNotify_IncrementsCounter pins the US-70.3 notify
// outcome vocabulary: success, rate_limited, no_pod, failed.
func TestRecordSecretNotify_IncrementsCounter(t *testing.T) {
	SecretNotifyCounter().Reset()

	RecordSecretNotify("success")
	RecordSecretNotify("rate_limited")
	RecordSecretNotify("rate_limited")
	RecordSecretNotify("no_pod")
	RecordSecretNotify("failed")
	RecordSecretNotify("")

	assert.Equal(t, 1.0, testutil.ToFloat64(SecretNotifyCounter().WithLabelValues("success")))
	assert.Equal(t, 2.0, testutil.ToFloat64(SecretNotifyCounter().WithLabelValues("rate_limited")))
	assert.Equal(t, 1.0, testutil.ToFloat64(SecretNotifyCounter().WithLabelValues("no_pod")))
	assert.Equal(t, 1.0, testutil.ToFloat64(SecretNotifyCounter().WithLabelValues("failed")))
	assert.Equal(t, 1.0, testutil.ToFloat64(SecretNotifyCounter().WithLabelValues("unknown")),
		"empty outcome must be counted as unknown, never dropped")
}

// TestSetSecretsDeliveryConverged_TogglesGauge pins the per-workspace
// convergence gauge: 1 converged, 0 divergent, latest write wins.
func TestSetSecretsDeliveryConverged_TogglesGauge(t *testing.T) {
	SecretsDeliveryConvergedGauge().Reset()

	SetSecretsDeliveryConverged("ws-a", true)
	SetSecretsDeliveryConverged("ws-b", false)
	SetSecretsDeliveryConverged("ws-a", false)

	assert.Equal(t, 0.0, testutil.ToFloat64(SecretsDeliveryConvergedGauge().WithLabelValues("ws-a")))
	assert.Equal(t, 0.0, testutil.ToFloat64(SecretsDeliveryConvergedGauge().WithLabelValues("ws-b")))
}

func TestRecordSecretsReconcilePass_IncrementsCounter(t *testing.T) {
	SecretsReconcilePassesCounter().Reset()

	RecordSecretsReconcilePass("success")
	RecordSecretsReconcilePass("error")

	assert.Equal(t, 1.0, testutil.ToFloat64(SecretsReconcilePassesCounter().WithLabelValues("success")))
	assert.Equal(t, 1.0, testutil.ToFloat64(SecretsReconcilePassesCounter().WithLabelValues("error")))
}

func TestRecordSecretsDeliveryDivergent_IncrementsCounter(t *testing.T) {
	SecretsDeliveryDivergentCounter().Reset()

	RecordSecretsDeliveryDivergent("missing_rev")
	RecordSecretsDeliveryDivergent("stale_seq")
	RecordSecretsDeliveryDivergent("legacy_format")
	RecordSecretsDeliveryDivergent("notify_failed")
	RecordSecretsDeliveryDivergent("")

	assert.Equal(t, 1.0, testutil.ToFloat64(SecretsDeliveryDivergentCounter().WithLabelValues("missing_rev")))
	assert.Equal(t, 1.0, testutil.ToFloat64(SecretsDeliveryDivergentCounter().WithLabelValues("stale_seq")))
	assert.Equal(t, 1.0, testutil.ToFloat64(SecretsDeliveryDivergentCounter().WithLabelValues("legacy_format")))
	assert.Equal(t, 1.0, testutil.ToFloat64(SecretsDeliveryDivergentCounter().WithLabelValues("notify_failed")))
	assert.Equal(t, 1.0, testutil.ToFloat64(SecretsDeliveryDivergentCounter().WithLabelValues("unknown")))
}

func TestSetSecretsReconcilePassSuccess_TimestampAdvances(t *testing.T) {
	before := time.Now().Unix()
	SetSecretsReconcilePassSuccess()
	got := testutil.ToFloat64(secretsReconcileLastPassSuccess)
	assert.GreaterOrEqual(t, int64(got), before)
}
