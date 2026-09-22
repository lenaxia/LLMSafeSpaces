// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package workspace

// upload_staging_env.go — design 0060 §9 PR 2.5: the deploy plumbing
// for the upload-staging knobs. Nothing below changes behavior when
// every field is zero — no env lands and agentd's compiled defaults
// stand (48 MiB budget / 24 MiB floor / 4 concurrent / 15 m TTL /
// 60 s apply timeout). Set fields land verbatim on BOTH containers
// that run staging/apply code (the sidecar container: admission +
// staging; the workspace container: the upload_apply engine + the
// destination scrub — one knob set, one behavior).

import (
	"fmt"
	"strconv"
	"strings"

	corev1 "k8s.io/api/core/v1"
)

// UploadStagingConfig carries the helm → controller → pod env knobs.
// Zero = unset = agentd defaults.
type UploadStagingConfig struct {
	BudgetBytes         int64
	CredentialFloorByte int64
	MaxConcurrent       int
	TTLMilliseconds     int64
	ApplyTimeoutMilli   int64
	// credentialFloorSet marks an EXPLICIT floor=0 (disable the
	// reserve) — a meaningful setting agentd honors (n ≥ 0) that must
	// not be conflated with unset (agentd's 24 MiB default).
	credentialFloorSet bool
}

// FlagString renders the --upload-staging flag payload (k=v, comma-
// separated; empty when fully unset — the flag can be omitted).
func (c UploadStagingConfig) FlagString() string {
	var parts []string
	if c.BudgetBytes > 0 {
		parts = append(parts, "budget="+strconv.FormatInt(c.BudgetBytes, 10))
	}
	if c.credentialFloorSet {
		parts = append(parts, "floor="+strconv.FormatInt(c.CredentialFloorByte, 10))
	}
	if c.MaxConcurrent > 0 {
		parts = append(parts, "concurrent="+strconv.Itoa(c.MaxConcurrent))
	}
	if c.TTLMilliseconds > 0 {
		parts = append(parts, "ttlMs="+strconv.FormatInt(c.TTLMilliseconds, 10))
	}
	if c.ApplyTimeoutMilli > 0 {
		parts = append(parts, "applyTimeoutMs="+strconv.FormatInt(c.ApplyTimeoutMilli, 10))
	}
	return strings.Join(parts, ",")
}

// ParseUploadStagingFlag is FlagString's inverse (the controller's
// flag entry point). Unknown keys are REJECTED loudly — a typo'd helm
// knob must not silently default (Rule: explicit over implicit).
func ParseUploadStagingFlag(raw string) (UploadStagingConfig, error) {
	var cfg UploadStagingConfig
	if strings.TrimSpace(raw) == "" {
		return cfg, nil
	}
	seen := map[string]bool{}
	for _, part := range strings.Split(raw, ",") {
		k, v, ok := strings.Cut(part, "=")
		if !ok {
			return cfg, fmt.Errorf("upload staging knob %q: want key=value", part)
		}
		if seen[k] {
			return cfg, fmt.Errorf("upload staging knob %q: duplicate key (last-win is the silent-default class this flag rejects)", k)
		}
		seen[k] = true
		switch k {
		case "budget":
			n, err := strconv.ParseInt(v, 10, 64)
			if err != nil || n <= 0 {
				return cfg, fmt.Errorf("upload staging budget %q: want a positive integer", v)
			}
			cfg.BudgetBytes = n
		case "floor":
			n, err := strconv.ParseInt(v, 10, 64)
			if err != nil || n < 0 {
				return cfg, fmt.Errorf("upload staging floor %q: want a non-negative integer", v)
			}
			cfg.CredentialFloorByte = n
			cfg.credentialFloorSet = true
		case "concurrent":
			n, err := strconv.Atoi(v)
			if err != nil || n <= 0 {
				return cfg, fmt.Errorf("upload staging concurrent %q: want a positive integer", v)
			}
			cfg.MaxConcurrent = n
		case "ttlMs":
			n, err := strconv.ParseInt(v, 10, 64)
			if err != nil || n <= 0 {
				return cfg, fmt.Errorf("upload staging ttlMs %q: want a positive integer", v)
			}
			cfg.TTLMilliseconds = n
		case "applyTimeoutMs":
			n, err := strconv.ParseInt(v, 10, 64)
			if err != nil || n <= 0 {
				return cfg, fmt.Errorf("upload staging applyTimeoutMs %q: want a positive integer", v)
			}
			cfg.ApplyTimeoutMilli = n
		default:
			return cfg, fmt.Errorf("upload staging knob %q: unknown key (budget, floor, concurrent, ttlMs, applyTimeoutMs)", k)
		}
	}
	// Cross-validation (the r1 robustness finding): the TTL sweeper must
	// not be able to reclaim an in-flight apply's temp — ttl must
	// strictly exceed the supervisor's apply bound (applyTimeout + its
	// 5s slack). The bound uses the EFFECTIVE apply timeout: an unset
	// applyTimeoutMs falls back to agentd's compiled 60s default (the
	// r2 hole — single-knob configs raced the guard).
	effectiveApply := cfg.ApplyTimeoutMilli
	if effectiveApply <= 0 {
		effectiveApply = 60000 // agentd's defaultApplyTimeout (upload_staging.go)
	}
	effectiveTTL := cfg.TTLMilliseconds
	if effectiveTTL <= 0 {
		effectiveTTL = 900000 // agentd's defaultStagingTTL 15m (upload_staging.go)
	}
	if effectiveTTL <= effectiveApply+5000 {
		return cfg, fmt.Errorf("upload staging ttl %dms must exceed the effective apply bound %d+5000=%dms (the sweeper must never reclaim an in-flight apply temp; unset knobs use agentd's 15m TTL / 60s apply defaults)",
			effectiveTTL, effectiveApply, effectiveApply+5000)
	}
	return cfg, nil
}

// EnvVars lands the SET fields as agentd-consumed env — the exact
// names cmd/workspace-agentd parses (upload_staging.go /
// upload_apply.go). Unset fields emit nothing: agentd defaults.
func (c UploadStagingConfig) EnvVars() []corev1.EnvVar {
	var env []corev1.EnvVar
	if c.BudgetBytes > 0 {
		env = append(env, corev1.EnvVar{Name: "UPLOAD_STAGING_BUDGET", Value: strconv.FormatInt(c.BudgetBytes, 10)})
	}
	if c.credentialFloorSet {
		env = append(env, corev1.EnvVar{Name: "UPLOAD_STAGING_CREDENTIAL_FLOOR", Value: strconv.FormatInt(c.CredentialFloorByte, 10)})
	}
	if c.MaxConcurrent > 0 {
		env = append(env, corev1.EnvVar{Name: "UPLOAD_STAGING_MAX_CONCURRENT", Value: strconv.Itoa(c.MaxConcurrent)})
	}
	if c.TTLMilliseconds > 0 {
		env = append(env, corev1.EnvVar{Name: "UPLOAD_STAGING_TTL_MS", Value: strconv.FormatInt(c.TTLMilliseconds, 10)})
	}
	if c.ApplyTimeoutMilli > 0 {
		env = append(env, corev1.EnvVar{Name: "UPLOAD_APPLY_TIMEOUT_MS", Value: strconv.FormatInt(c.ApplyTimeoutMilli, 10)})
	}
	return env
}
