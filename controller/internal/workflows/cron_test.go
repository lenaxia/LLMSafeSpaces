package workflows

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/lenaxia/llmsafespaces/pkg/types"
	wf "github.com/lenaxia/llmsafespaces/pkg/workflows"
)

func TestComputeNextFire_EveryNMinutes(t *testing.T) {
	trigger := &wf.TriggerRow{
		SourceConfig: json.RawMessage(`{"expr":"*/15 * * * *","tz":"UTC"}`),
	}
	now := time.Date(2026, 8, 7, 12, 0, 0, 0, time.UTC)
	next := computeNextFire(trigger, now)
	expected := now.Add(15 * time.Minute)
	if !next.Equal(expected) {
		t.Errorf("*/15 minutes: expected %v, got %v", expected, next)
	}
}

func TestComputeNextFire_Every5Minutes(t *testing.T) {
	trigger := &wf.TriggerRow{
		SourceConfig: json.RawMessage(`{"expr":"*/5 * * * *"}`),
	}
	now := time.Date(2026, 8, 7, 12, 0, 0, 0, time.UTC)
	next := computeNextFire(trigger, now)
	expected := now.Add(5 * time.Minute)
	if !next.Equal(expected) {
		t.Errorf("*/5 minutes: expected %v, got %v", expected, next)
	}
}

func TestComputeNextFire_Hourly(t *testing.T) {
	trigger := &wf.TriggerRow{
		SourceConfig: json.RawMessage(`{"expr":"0 * * * *"}`),
	}
	now := time.Date(2026, 8, 7, 12, 30, 0, 0, time.UTC)
	next := computeNextFire(trigger, now)
	expected := now.Add(time.Hour)
	if !next.Equal(expected) {
		t.Errorf("hourly: expected %v, got %v", expected, next)
	}
}

func TestComputeNextFire_DailyAtHour(t *testing.T) {
	trigger := &wf.TriggerRow{
		SourceConfig: json.RawMessage(`{"expr":"0 9 * * *"}`),
	}
	now := time.Date(2026, 8, 7, 12, 0, 0, 0, time.UTC)
	next := computeNextFire(trigger, now)
	// Should be tomorrow at 9:00
	expected := time.Date(2026, 8, 8, 9, 0, 0, 0, time.UTC)
	if !next.Equal(expected) {
		t.Errorf("daily 9am: expected %v, got %v", expected, next)
	}
}

func TestComputeNextFire_DailyToday(t *testing.T) {
	trigger := &wf.TriggerRow{
		SourceConfig: json.RawMessage(`{"expr":"0 14 * * *"}`),
	}
	now := time.Date(2026, 8, 7, 12, 0, 0, 0, time.UTC)
	next := computeNextFire(trigger, now)
	// 14:00 hasn't passed yet today → today
	expected := time.Date(2026, 8, 7, 14, 0, 0, 0, time.UTC)
	if !next.Equal(expected) {
		t.Errorf("daily 2pm today: expected %v, got %v", expected, next)
	}
}

func TestComputeNextFire_Weekdays(t *testing.T) {
	trigger := &wf.TriggerRow{
		SourceConfig: json.RawMessage(`{"expr":"0 9 * * 1-5"}`),
	}
	// Friday Aug 7 2026 at noon → next should be Monday Aug 10 at 9am
	now := time.Date(2026, 8, 7, 12, 0, 0, 0, time.UTC)
	next := computeNextFire(trigger, now)

	// First check it's at 9am
	if next.Hour() != 9 || next.Minute() != 0 {
		t.Errorf("weekday cron: expected 9:00, got %02d:%02d", next.Hour(), next.Minute())
	}
	// Then check it's a weekday
	if next.Weekday() == time.Saturday || next.Weekday() == time.Sunday {
		t.Errorf("weekday cron: expected a weekday, got %v", next.Weekday())
	}
}

func TestComputeNextFire_EmptyExpr(t *testing.T) {
	trigger := &wf.TriggerRow{
		SourceConfig: json.RawMessage(`{"expr":""}`),
	}
	now := time.Date(2026, 8, 7, 12, 0, 0, 0, time.UTC)
	next := computeNextFire(trigger, now)
	expected := now.Add(time.Hour)
	if !next.Equal(expected) {
		t.Errorf("empty expr: expected %v, got %v", expected, next)
	}
}

func TestComputeNextFire_MalformedExpr(t *testing.T) {
	trigger := &wf.TriggerRow{
		SourceConfig: json.RawMessage(`{"expr":"not a cron"}`),
	}
	now := time.Date(2026, 8, 7, 12, 0, 0, 0, time.UTC)
	next := computeNextFire(trigger, now)
	// Should fall through to default (1 hour)
	expected := now.Add(time.Hour)
	if !next.Equal(expected) {
		t.Errorf("malformed expr: expected %v, got %v", expected, next)
	}
}

func TestComputeNextFire_TooFewFields(t *testing.T) {
	trigger := &wf.TriggerRow{
		SourceConfig: json.RawMessage(`{"expr":"0 9"}`),
	}
	now := time.Date(2026, 8, 7, 12, 0, 0, 0, time.UTC)
	next := computeNextFire(trigger, now)
	expected := now.Add(time.Hour)
	if !next.Equal(expected) {
		t.Errorf("too few fields: expected %v, got %v", expected, next)
	}
}

// Ensure types import is used
var _ = types.TriggerSourceCron

func TestComputeNextFire_TimezoneSupport(t *testing.T) {
	// A trigger configured for 9am America/New_York should fire at 9am ET,
	// which is 13:00 UTC during EDT (UTC-4) or 14:00 UTC during EST (UTC-5).
	// We test with a fixed summer date when EDT is active (UTC-4).
	trigger := &wf.TriggerRow{
		SourceConfig: json.RawMessage(`{"expr":"0 9 * * *","tz":"America/New_York"}`),
	}
	// Noon UTC on Aug 7 2026 → 8am EDT. Next 9am ET is in 1 hour → 13:00 UTC.
	now := time.Date(2026, 8, 7, 12, 0, 0, 0, time.UTC)
	next := computeNextFire(trigger, now)

	// Should be 13:00 UTC (9am EDT = UTC-4).
	if next.Hour() != 13 {
		t.Errorf("expected 13:00 UTC (9am EDT), got %02d:%02d UTC", next.Hour(), next.Minute())
	}
	if next.Minute() != 0 {
		t.Errorf("expected :00 minutes, got :%02d", next.Minute())
	}
}

func TestComputeNextFire_TimezoneInvalid(t *testing.T) {
	// Invalid timezone should fall back to UTC.
	trigger := &wf.TriggerRow{
		SourceConfig: json.RawMessage(`{"expr":"0 14 * * *","tz":"Mars/Olympus"}`),
	}
	now := time.Date(2026, 8, 7, 12, 0, 0, 0, time.UTC)
	next := computeNextFire(trigger, now)

	// With UTC fallback: 14:00 hasn't passed → today at 14:00 UTC.
	expected := time.Date(2026, 8, 7, 14, 0, 0, 0, time.UTC)
	if !next.Equal(expected) {
		t.Errorf("invalid TZ fallback to UTC: expected %v, got %v", expected, next)
	}
}
