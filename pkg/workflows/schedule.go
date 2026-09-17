// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package workflows

// Shared cron-schedule logic for triggers (#1410, #1411): the write path
// (trigger create/update handlers) and the read path (engine scheduler)
// must agree on what a valid cron source config is and when the next
// fire occurs. Before this file, the handler never validated exprs and
// initialized next_fire_at to "now" (immediate fire), while the engine
// silently retried unparseable exprs hourly.

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/robfig/cron/v3"

	"github.com/lenaxia/llmsafespaces/pkg/types"
)

// cronParser matches the five-field syntax the engine has always used
// (Minute Hour Dom Month Dow). A shared parser instance is safe for
// concurrent use.
var cronParser = cron.NewParser(cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow)

// ValidateCronSourceConfig decodes and validates a cron trigger's
// source_config JSON: expr must parse under the five-field syntax and
// tz (when set) must be a loadable IANA location. It returns the typed
// config so callers don't decode twice.
func ValidateCronSourceConfig(sourceConfig json.RawMessage) (*types.CronSourceConfig, error) {
	var cfg types.CronSourceConfig
	if len(sourceConfig) > 0 {
		if err := json.Unmarshal(sourceConfig, &cfg); err != nil {
			return nil, fmt.Errorf("invalid cron source config: %w", err)
		}
	}
	if cfg.Expr == "" {
		return nil, fmt.Errorf("cron source requires 'expr'")
	}
	if _, err := cronParser.Parse(cfg.Expr); err != nil {
		return nil, fmt.Errorf("invalid cron expr %q: %w", cfg.Expr, err)
	}
	if cfg.TZ != "" {
		if _, err := time.LoadLocation(cfg.TZ); err != nil {
			return nil, fmt.Errorf("invalid tz %q: %w", cfg.TZ, err)
		}
	}
	return &cfg, nil
}

// NextCronFire returns the first fire time strictly after now for a
// validated cron config. The config is re-validated here so the engine
// keeps its defensive behavior for rows written before validation
// existed (callers translate the error into a fallback).
func NextCronFire(cfg *types.CronSourceConfig, now time.Time) (time.Time, error) {
	if cfg == nil {
		return time.Time{}, fmt.Errorf("nil cron config")
	}
	sched, err := cronParser.Parse(cfg.Expr)
	if err != nil {
		return time.Time{}, fmt.Errorf("invalid cron expr %q: %w", cfg.Expr, err)
	}
	loc := time.UTC
	if cfg.TZ != "" {
		parsed, err := time.LoadLocation(cfg.TZ)
		if err != nil {
			return time.Time{}, fmt.Errorf("invalid tz %q: %w", cfg.TZ, err)
		}
		loc = parsed
	}
	return sched.Next(now.In(loc)).UTC(), nil
}

// NextCronFireFromConfig validates source_config and computes the next
// fire after now in one step — the write-path convenience.
func NextCronFireFromConfig(sourceConfig json.RawMessage, now time.Time) (*types.CronSourceConfig, time.Time, error) {
	cfg, err := ValidateCronSourceConfig(sourceConfig)
	if err != nil {
		return nil, time.Time{}, err
	}
	next, err := NextCronFire(cfg, now)
	if err != nil {
		return nil, time.Time{}, err
	}
	return cfg, next, nil
}
