package workflows

import (
	"testing"
)

func TestRecordRunStarted(t *testing.T) {
	// Just verify it doesn't panic.
	RecordRunStarted()
	RecordRunStarted()
	// The gauge should be 2 now, but we can't easily read it back.
	// This test exists to prevent the function from being removed.
}

func TestRecordRunFinished(t *testing.T) {
	RecordRunFinished("succeeded", "", "user", 1.5)
	RecordRunFinished("failed", "node_failed", "org", 2.3)
}

func TestRecordNodeDuration(t *testing.T) {
	RecordNodeDuration("script", 0.5)
	RecordNodeDuration("agent", 3.2)
}

func TestRecordTriggerFire(t *testing.T) {
	RecordTriggerFire("cron", "fired")
	RecordTriggerFire("webhook", "delivered")
}

func TestRecordWebhookDelivery(t *testing.T) {
	RecordWebhookDelivery("hook-1", "fired")
}

func TestRecordSchedulerTick(t *testing.T) {
	RecordSchedulerTick(0.01)
}
