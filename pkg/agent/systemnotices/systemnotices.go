// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

// Package systemnotices injects platform-level system notices into
// agent-bound messages at the Adapter seam (#944).
//
// The disk-pressure nudge was originally implemented inside the proxy's
// raw-transport rewrite, which chat traffic abandoned twice (US-63.7
// moved chat onto the Adapter, #755 moved Send off the V2 queue) — each
// migration silently orphaned the feature. A must-apply-to-every-message
// concern belongs at the ONE seam every entrypoint (HTTP chat, MCP, SDK,
// future triggers) shares: pkg/agent.Adapter. Wrap() decorates that
// seam at its single wiring point (app.go).
package systemnotices

import (
	"context"
	"fmt"
	"math"
	"os"
	"strconv"

	"github.com/lenaxia/llmsafespaces/pkg/agent"
	"github.com/lenaxia/llmsafespaces/pkg/session"
)

// Level is a disk-pressure tier.
type Level string

const (
	LevelNone     Level = "none"
	LevelWarning  Level = "warning"  // >= 90% full
	LevelCritical Level = "critical" // >= 95% full
)

// Default disk-usage ratios at which the notice is injected.
const (
	defaultWarningThreshold  = 0.90
	defaultCriticalThreshold = 0.95
)

// warningThreshold / criticalThreshold are the usage ratios (used/total)
// at which the notice is injected, overridable via environment
// (DISK_WARNING_THRESHOLD / DISK_CRITICAL_THRESHOLD). Values outside
// (0,1) or a collapsed/inverted tier pair (warning >= critical) fall
// back to the defaults so the warning tier is never silently made
// unreachable. Single source of truth: the legacy raw-proxy injector
// (api/internal/handlers/proxy_disk_pressure.go) reads these via
// Thresholds() so the two paths cannot drift.
var (
	warningThreshold  = envFloatOr("DISK_WARNING_THRESHOLD", defaultWarningThreshold)
	criticalThreshold = envFloatOr("DISK_CRITICAL_THRESHOLD", defaultCriticalThreshold)
)

func init() {
	warningThreshold, criticalThreshold = normalizeThresholds(
		warningThreshold, criticalThreshold, defaultWarningThreshold, defaultCriticalThreshold)
}

// Thresholds returns the active (warning, critical) ratios.
func Thresholds() (float64, float64) {
	return warningThreshold, criticalThreshold
}

func normalizeThresholds(warning, critical, defW, defC float64) (float64, float64) {
	if warning <= 0 || warning >= 1 || critical <= 0 || critical >= 1 || warning >= critical {
		return defW, defC
	}
	return warning, critical
}

func envFloatOr(key string, def float64) float64 {
	if v := os.Getenv(key); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil && f > 0 && f < 1 {
			return f
		}
	}
	return def
}

// Ratio computes the used fraction of the workspace disk. Returns 0
// when total is unknown/non-positive so a not-yet-scraped workspace
// never trips the warning (fail-safe).
func Ratio(usedBytes, totalBytes int64) float64 {
	if totalBytes <= 0 {
		return 0
	}
	return float64(usedBytes) / float64(totalBytes)
}

// LevelForRatio maps a usage ratio to a pressure level.
func LevelForRatio(ratio float64) Level {
	switch {
	case ratio >= criticalThreshold:
		return LevelCritical
	case ratio >= warningThreshold:
		return LevelWarning
	default:
		return LevelNone
	}
}

// pctString floors (never rounds) the displayed percentage so it cannot
// cross a level boundary upward: at ratio 0.949999 the level is warning
// (< 0.95) and must display "94%", not "95%".
func pctString(ratio float64) string {
	return fmt.Sprintf("%.0f%%", math.Floor(ratio*100))
}

// Notice builds the system-notice text injected ahead of the user's
// message. The notice is addressed to the agent (it tells the model
// what to do) but reads naturally if it surfaces in the transcript.
//
//   - Warning (>=90%): nudge the user; no deletion authority.
//   - Critical (>=95%): stronger; may delete ONLY easily-replaceable
//     files (build artifacts, caches); logs are the last resort because
//     they cannot be reproduced; user approval required.
//
// Wording is byte-for-byte the original proxy_disk_pressure text — this
// function is its new single source.
func Notice(level Level, ratio float64) string {
	pct := pctString(ratio)
	switch level {
	case LevelCritical:
		return "System notice: your workspace disk is at " + pct +
			" capacity — critically low. Tell the user their disk is nearly full and offer to help clean it up. " +
			"If the user agrees, delete ONLY files that are easily replaceable: build artifacts and true temporary " +
			"files such as caches. Logs are the last resort — they cannot be reproduced once deleted — so prefer " +
			"build artifacts and caches first, and only consider logs if nothing else frees enough space. " +
			"Never delete anything without the user's explicit approval."
	case LevelWarning:
		return "System notice: your workspace disk is at " + pct +
			" capacity. Gently let the user know their disk is getting full and suggest they free up some space " +
			"(for example by removing old or large files, build artifacts, or caches). Do not delete anything yourself."
	default:
		return ""
	}
}

// WorkspaceDiskUsage sources a workspace's disk usage. Implemented at
// the wiring layer over the Workspace CRD status (the same data the
// controller mirrors from agentd /v1/statusz and the frontend renders)
// — no new telemetry.
type WorkspaceDiskUsage interface {
	DiskUsage(ctx context.Context, workspaceID string) (usedBytes, totalBytes int64, err error)
}

// Wrap returns an agent.Adapter that prepends the disk-pressure system
// notice to every Send/SendAsync text at or above the warning
// threshold. All other methods delegate untouched. A nil usage source
// disables injection (the decorator becomes a pass-through).
func Wrap(a agent.Adapter, usage WorkspaceDiskUsage) agent.Adapter {
	return &noticingAdapter{Adapter: a, usage: usage}
}

type noticingAdapter struct {
	agent.Adapter
	usage WorkspaceDiskUsage
}

var _ agent.Adapter = (*noticingAdapter)(nil)

func (n *noticingAdapter) Send(ctx context.Context, userID, workspaceID, sessionID, text string, opts session.SendOpts) (*session.Message, error) {
	return n.Adapter.Send(ctx, userID, workspaceID, sessionID, n.withNotice(ctx, workspaceID, text), opts)
}

func (n *noticingAdapter) SendAsync(ctx context.Context, userID, workspaceID, sessionID, text string, opts session.SendOpts) (string, error) {
	return n.Adapter.SendAsync(ctx, userID, workspaceID, sessionID, n.withNotice(ctx, workspaceID, text), opts)
}

// withNotice prepends the notice when the workspace is at/above the
// warning threshold. Fail-open by construction: a usage read error, an
// unknown total, a below-threshold ratio, or a nil usage source all
// return the text unchanged.
func (n *noticingAdapter) withNotice(ctx context.Context, workspaceID, text string) string {
	if n.usage == nil {
		return text
	}
	used, total, err := n.usage.DiskUsage(ctx, workspaceID)
	if err != nil || total <= 0 {
		return text
	}
	level := LevelForRatio(Ratio(used, total))
	notice := Notice(level, Ratio(used, total))
	if notice == "" {
		return text
	}
	return notice + "\n\n" + text
}
