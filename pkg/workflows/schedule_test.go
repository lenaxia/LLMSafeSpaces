// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package workflows

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/lenaxia/llmsafespaces/pkg/types"
)

func TestValidateCronSourceConfig(t *testing.T) {
	cases := []struct {
		name    string
		cfg     string
		wantErr string
	}{
		{"valid utc", `{"expr":"*/15 * * * *","tz":"UTC"}`, ""},
		{"valid no tz", `{"expr":"0 2 * * *"}`, ""},
		{"valid iana tz", `{"expr":"0 9 * * *","tz":"America/Los_Angeles"}`, ""},
		{"empty expr", `{"tz":"UTC"}`, "cron source requires 'expr'"},
		{"garbage expr", `{"expr":"not-a-cron"}`, "invalid cron expr"},
		{"six fields rejected", `{"expr":"0 0 0 * * *"}`, "invalid cron expr"},
		{"bad tz", `{"expr":"0 * * * *","tz":"Mars/Olympus"}`, "invalid tz"},
		{"non-json", `not json`, "invalid cron source config"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ValidateCronSourceConfig(json.RawMessage(tc.cfg))
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("expected valid, got %v", err)
				}
				return
			}
			if err == nil {
				t.Fatal("expected error, got nil")
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("error %q does not contain %q", err.Error(), tc.wantErr)
			}
		})
	}
}

func TestNextCronFire_TimezoneMath(t *testing.T) {
	// 09:00 America/Los_Angeles on 2026-09-17 is 16:00 UTC (PDT, UTC-7).
	now := time.Date(2026, 9, 16, 20, 45, 0, 0, time.UTC)
	cfg := &types.CronSourceConfig{Expr: "0 9 * * *", TZ: "America/Los_Angeles"}
	next, err := NextCronFire(cfg, now)
	if err != nil {
		t.Fatal(err)
	}
	want := time.Date(2026, 9, 17, 16, 0, 0, 0, time.UTC)
	if !next.Equal(want) {
		t.Fatalf("got %s, want %s", next, want)
	}
}

func TestNextCronFire_StrictlyAfterNow(t *testing.T) {
	now := time.Date(2026, 9, 17, 2, 0, 0, 0, time.UTC)
	cfg := &types.CronSourceConfig{Expr: "0 2 * * *", TZ: "UTC"}
	next, err := NextCronFire(cfg, now)
	if err != nil {
		t.Fatal(err)
	}
	if !next.After(now) {
		t.Fatalf("next fire %s must be strictly after now %s (no double-fire on the boundary)", next, now)
	}
	want := time.Date(2026, 9, 18, 2, 0, 0, 0, time.UTC)
	if !next.Equal(want) {
		t.Fatalf("got %s, want %s", next, want)
	}
}

func TestNextCronFireFromConfig(t *testing.T) {
	_, next, err := NextCronFireFromConfig(json.RawMessage(`{"expr":"0 3 1 * *","tz":"UTC"}`), time.Date(2026, 9, 17, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	if want := time.Date(2026, 10, 1, 3, 0, 0, 0, time.UTC); !next.Equal(want) {
		t.Fatalf("got %s, want %s", next, want)
	}

	if _, _, err := NextCronFireFromConfig(json.RawMessage(`{"expr":"x"}`), time.Now()); err == nil {
		t.Fatal("invalid config must error")
	}
}
