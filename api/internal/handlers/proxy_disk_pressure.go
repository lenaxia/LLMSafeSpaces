// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package handlers

import (
	"encoding/json"
	"strings"

	"github.com/lenaxia/llmsafespaces/pkg/agent/systemnotices"
)

// Disk-pressure injection (feature: LLM disk-space nudges) — LEGACY
// RAW-PROXY PATH. The primary injection point since #944 is the
// Adapter decorator (pkg/agent/systemnotices.Wrap), which covers every
// entrypoint (HTTP chat, MCP, SDK); this body-rewrite remains for the
// raw-proxy clients still hitting proxyToWorkspace and delegates all
// level/threshold/wording logic to the same single source, so the two
// paths cannot drift.
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

// diskPressureLevel mirrors the platform-wide level type. All level,
// threshold, ratio, and wording logic lives in
// pkg/agent/systemnotices — the single source shared with the Adapter
// decorator (#944) so this legacy raw-proxy injector cannot drift.
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
// harmless: JSON objects are unordered per RFC 8259 §4, so any
// spec-compliant parser — including opencode — reads the body
// identically regardless of key order.
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

	noticePart, _ := json.Marshal(struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}{Type: "text", Text: diskPressureNotice(level, ratio)})
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
// prompt that reaches the LLM. Only /message remains post-US-63.7 (V2
// prompts go through enqueueV2 → client.PromptV2, not proxyToWorkspace).
func isLLMPromptPath(targetPath string) bool {
	return strings.HasSuffix(targetPath, "/message")
}
