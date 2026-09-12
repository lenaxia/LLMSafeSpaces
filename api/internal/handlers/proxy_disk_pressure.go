// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package handlers

import (
	"github.com/lenaxia/llmsafespaces/pkg/agent/systemnotices"
)

// Disk-pressure injection (feature: LLM disk-space nudges) — the single
// injection point is the Adapter decorator (pkg/agent/systemnotices.Wrap,
// #944), which covers every entrypoint (HTTP chat, MCP, SDK). The
// handler-level body-rewrite that served the legacy raw-proxy path was
// deleted with that path (#828 batch 1); these aliases keep the
// handler-adjacent tests speaking the systemnotices vocabulary, and all
// level/threshold/wording logic stays in the single source so nothing
// can drift.
//
// When a workspace's persistent disk (/workspace PVC) crosses 90% usage,
// a notice is prepended to every LLM-bound chat request so the agent
// nudges the user to free up space. At 95% the notice escalates: the
// agent is told it may remove ONLY easily-replaceable files (build
// artifacts, caches) and that logs are the last resort because they
// cannot be reproduced once deleted.
//
// The disk ratio is taken from the Workspace CRD status
// (DiskUsedBytes / DiskTotalBytes), which the controller mirrors from
// agentd /v1/statusz on its deep-status poll (~60s) — the same data the
// frontend's DiskUsageBar renders as a %.

// diskPressureLevel mirrors the platform-wide level type. All level,
// threshold, ratio, and wording logic lives in
// pkg/agent/systemnotices — the single source shared with the Adapter
// decorator (#944).
type diskPressureLevel = systemnotices.Level

const (
	diskPressureNone     = systemnotices.LevelNone
	diskPressureWarning  = systemnotices.LevelWarning  // >= 90% full
	diskPressureCritical = systemnotices.LevelCritical // >= 95% full
)

// diskPressureRatio computes the fraction of the workspace disk that is
// used (0 when total is unknown — fail-safe).
func diskPressureRatio(usedBytes, totalBytes int64) float64 {
	return systemnotices.Ratio(usedBytes, totalBytes)
}

// diskPressureLevelForRatio maps a usage ratio to a pressure level.
func diskPressureLevelForRatio(ratio float64) diskPressureLevel {
	return systemnotices.LevelForRatio(ratio)
}

// diskPressureNotice builds the instruction text injected into the
// request. Delegates to systemnotices.Notice — the single wording
// source (byte-for-byte the original text; the tier tests below pin
// it through this delegation).
func diskPressureNotice(level diskPressureLevel, ratio float64) string {
	return systemnotices.Notice(level, ratio)
}
