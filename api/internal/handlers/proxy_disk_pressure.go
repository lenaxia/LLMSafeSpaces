// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package handlers

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"strconv"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Disk-pressure injection (feature: LLM disk-space nudges).
//
// When a workspace's persistent disk (/workspace PVC) crosses 90% usage,
// the proxy prepends a notice part to every LLM-bound chat request so the
// agent nudges the user to free up space. At 95% the notice escalates:
// the agent is told it may remove ONLY easily-replaceable files (build
// artifacts, caches) and that logs are the last resort because they
// cannot be reproduced once deleted.
//
// The disk ratio is taken from the Workspace CRD status
// (DiskUsedBytes / DiskTotalBytes), which the controller mirrors from
// agentd /v1/statusz on its deep-status poll (~60s). This is the same
// data the frontend's DiskUsageBar renders as a %, so no new telemetry
// is introduced — the injection is purely a proxy-side rewrite of the
// request body before it reaches opencode.
//
// Design decisions:
//   - Fail-open: any read/parse error passes the request through unchanged.
//   - The notice is injected as a leading text part (opencode has no
//     "system" part type). It rides the request but is not persisted to
//     session history by the API; opencode stores what it receives.
//   - Thresholds are package-level vars, overridable via env
//     (DISK_WARNING_THRESHOLD / DISK_CRITICAL_THRESHOLD), mirroring the
//     memory-pressure monitor's env-override pattern.

type diskPressureLevel string

const (
	diskPressureNone     diskPressureLevel = "none"
	diskPressureWarning  diskPressureLevel = "warning"  // >= 90% full
	diskPressureCritical diskPressureLevel = "critical" // >= 95% full
)

// Default disk-usage ratios at which the proxy injects the notice.
// 90% per the user requirement; 95% escalates.
const (
	defaultDiskWarningThreshold  = 0.90
	defaultDiskCriticalThreshold = 0.95
)

// diskWarningThreshold / diskCriticalThreshold are the disk usage ratios
// (used/total) at which the proxy injects the notice. Overridable via env
// (DISK_WARNING_THRESHOLD / DISK_CRITICAL_THRESHOLD). If the overrides
// invert or collapse the tiers (warning >= critical), both fall back to
// the defaults so the warning tier is never silently made unreachable.
var (
	diskWarningThreshold  = envFloatOr("DISK_WARNING_THRESHOLD", defaultDiskWarningThreshold)
	diskCriticalThreshold = envFloatOr("DISK_CRITICAL_THRESHOLD", defaultDiskCriticalThreshold)
)

func init() {
	w, c := normalizeDiskThresholds(diskWarningThreshold, diskCriticalThreshold,
		defaultDiskWarningThreshold, defaultDiskCriticalThreshold)
	diskWarningThreshold = w
	diskCriticalThreshold = c
}

// normalizeDiskThresholds enforces warning < critical. When the configured
// values invert or collapse the tiers (warning >= critical), both fall
// back to the supplied defaults so the warning tier stays reachable. Each
// value is also expected to be in (0,1); out-of-range values (which
// envFloatOr already rejects, but defensive here for direct callers) also
// trigger the fallback.
func normalizeDiskThresholds(warning, critical, defW, defC float64) (float64, float64) {
	if warning <= 0 || warning >= 1 || critical <= 0 || critical >= 1 || warning >= critical {
		return defW, defC
	}
	return warning, critical
}

// envFloatOr reads a float env var, falling back to def on unset/parse
// failure. Thresholds must be within (0, 1); anything else is ignored so
// a bad value cannot disable or invert the feature.
func envFloatOr(key string, def float64) float64 {
	if v := os.Getenv(key); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil && f > 0 && f < 1 {
			return f
		}
	}
	return def
}

// diskPressureRatio computes the fraction of the workspace disk that is
// used. Returns 0 when total is unknown/non-positive so a not-yet-scraped
// workspace never trips the warning (fail-safe).
func diskPressureRatio(usedBytes, totalBytes int64) float64 {
	if totalBytes <= 0 {
		return 0
	}
	return float64(usedBytes) / float64(totalBytes)
}

// diskPressureLevelForRatio maps a usage ratio to a pressure level.
func diskPressureLevelForRatio(ratio float64) diskPressureLevel {
	switch {
	case ratio >= diskCriticalThreshold:
		return diskPressureCritical
	case ratio >= diskWarningThreshold:
		return diskPressureWarning
	default:
		return diskPressureNone
	}
}

// diskPressureNotice builds the instruction text injected into the
// request. The notice is addressed to the agent (it tells the model what
// to do) but reads naturally if it surfaces in the transcript.
//
//   - Warning (>=90%): nudge the user; no deletion authority.
//   - Critical (>=95%): stronger; may delete ONLY easily-replaceable
//     files (build artifacts, caches); logs are the last resort because
//     they cannot be reproduced; user approval required.
func diskPressureNotice(level diskPressureLevel, ratio float64) string {
	// Floor (not Round) so the displayed % never crosses a level
	// boundary upward: at ratio 0.949999 the level is warning (< 0.95)
	// and must display "94%", not "95%" (which would read as critical).
	pct := fmt.Sprintf("%.0f%%", math.Floor(ratio*100))
	switch level {
	case diskPressureCritical:
		return "System notice: your workspace disk is at " + pct +
			" capacity — critically low. Tell the user their disk is nearly full and offer to help clean it up. " +
			"If the user agrees, delete ONLY files that are easily replaceable: build artifacts and true temporary " +
			"files such as caches. Logs are the last resort — they cannot be reproduced once deleted — so prefer " +
			"build artifacts and caches first, and only consider logs if nothing else frees enough space. " +
			"Never delete anything without the user's explicit approval."
	case diskPressureWarning:
		return "System notice: your workspace disk is at " + pct +
			" capacity. Gently let the user know their disk is getting full and suggest they free up some space " +
			"(for example by removing old or large files, build artifacts, or caches). Do not delete anything yourself."
	default:
		return ""
	}
}

// injectDiskPressureNotice rewrites an opencode message body
// ({parts:[...], messageID?, ...}) by prepending a text part carrying the
// disk-pressure notice when the usage ratio is at/above the warning
// threshold. Only `parts` is rewritten; ALL other top-level fields
// (messageID, model, mode, ...) are preserved so the proxy never needs
// to enumerate opencode's full message schema. This matters because the
// frontend sends a `model` field on /prompt when the user picked a
// specific model — dropping it would silently reroute the message to
// opencode's default model.
//
// Field values round-trip unchanged; top-level KEY ORDER may change
// (encoding/json marshals maps with sorted keys). This is semantically
// harmless — JSON objects are unordered per RFC 8259 §4, and opencode
// parses the body with standard order-independent unmarshal (verified:
// pkg/agent/opencode uses json.Decode; the proxy's own
// extractPromptText uses json.Unmarshal into a struct).
//
// Fail-open: empty or malformed bodies pass through unchanged, as does
// any ratio below the warning threshold.
func injectDiskPressureNotice(body []byte, ratio float64) []byte {
	level := diskPressureLevelForRatio(ratio)
	if level == diskPressureNone || len(body) == 0 {
		return body
	}

	// Decode into a raw map so unknown sibling fields round-trip
	// untouched. map[string]json.RawMessage is the correct tool here:
	// the proxy is genuinely passing through arbitrary fields it does
	// not own (Rule 12 — Containment: the proxy must not enumerate
	// opencode's schema).
	var msg map[string]json.RawMessage
	if err := json.Unmarshal(body, &msg); err != nil {
		return body
	}

	// Parse the existing parts (if any) to prepend the notice.
	var existing []json.RawMessage
	if rawParts, ok := msg["parts"]; ok {
		if err := json.Unmarshal(rawParts, &existing); err != nil {
			return body
		}
	}

	noticePart, _ := json.Marshal(promptPart{Type: "text", Text: diskPressureNotice(level, ratio)})
	parts := make([]json.RawMessage, 0, len(existing)+1)
	parts = append(parts, noticePart)
	parts = append(parts, existing...)

	partsRaw, err := json.Marshal(parts)
	if err != nil {
		return body
	}
	msg["parts"] = partsRaw

	out, err := json.Marshal(msg)
	if err != nil {
		return body
	}
	return out
}

// isLLMPromptPath reports whether a proxy target path carries a user
// prompt that reaches the LLM (message send + prompt_async). Other
// paths (question/permission replies, session CRUD, abort, etc.) do not
// warrant a disk-pressure notice.
func isLLMPromptPath(targetPath string) bool {
	return strings.HasSuffix(targetPath, "/message") || strings.HasSuffix(targetPath, "/prompt_async")
}

// workspaceDiskRatio returns the workspace's disk usage ratio
// (used/total) from the Workspace CRD status — the same fields the
// controller mirrors from agentd /v1/statusz on its deep-status poll
// (~60s) and that the frontend renders as a disk-usage %. Returns 0 on
// any read error (fail-open: no injection).
func (h *ProxyHandler) workspaceDiskRatio(ctx context.Context, workspaceID string) float64 {
	if h.k8sClient == nil {
		return 0
	}
	v1Client, err := h.k8sClient.LlmsafespacesV1()
	if err != nil {
		return 0
	}
	ws, err := v1Client.Workspaces(h.namespace).Get(ctx, workspaceID, metav1.GetOptions{})
	if err != nil {
		return 0
	}
	return diskPressureRatio(ws.Status.DiskUsedBytes, ws.Status.DiskTotalBytes)
}
