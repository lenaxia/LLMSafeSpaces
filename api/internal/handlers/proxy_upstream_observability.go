// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package handlers

import (
	"strconv"
	"strings"

	"github.com/lenaxia/llmsafespaces/api/internal/services/metrics"
	pkginterfaces "github.com/lenaxia/llmsafespaces/pkg/interfaces"
)

// upstream5xxBodyPreviewCap bounds how much of an upstream error body
// we include in the observability log line. opencode's error envelopes
// are typically <200 bytes ({"name":"UnknownError","data":{"ref":"err_*"}}),
// so 512 is generous. A larger cap would inflate log volume with no
// diagnostic gain — the ref alone lets an operator find opencode's full
// stack trace via `kubectl logs <ws> -c workspace`.
const upstream5xxBodyPreviewCap = 512

// recordUpstream5xx is the single observability chokepoint for opencode
// 5xx responses at the API proxy layer. Emits BOTH a Warn log line
// (with body preview + workspaceID/path/status) AND increments the
// api_upstream_5xx_total counter labeled by workspaceID/path/status.
// See LLMSafeSpaces#488 for the incident this exists to make debuggable.
//
// body may be nil (streaming proxy path) — the preview field is set to
// "" in that case; caller comment should note the streaming caveat.
//
// The `path` argument SHOULD be the raw opencode-side path with the
// original session ID intact (e.g. `/session/ses_abc/message`) — this
// helper sanitizes it internally via sanitizePathForMetric for the
// metric label (session IDs collapse to `:id`) while preserving the
// raw path in the log line so operators can grep for a specific
// session's failure history. The two dimensions are intentionally
// asymmetric: metric labels must be bounded-cardinality; log lines
// benefit from full detail.
func recordUpstream5xx(logger pkginterfaces.LoggerInterface, workspaceID, path string, status int, body []byte) {
	preview := ""
	if len(body) > 0 {
		if len(body) > upstream5xxBodyPreviewCap {
			preview = string(body[:upstream5xxBodyPreviewCap]) + "..."
		} else {
			preview = string(body)
		}
	}

	if logger != nil {
		logger.Warn("Upstream 5xx from workspace pod",
			"workspaceID", workspaceID,
			"path", path,
			"upstreamStatus", status,
			"bodyPreview", preview,
		)
	}

	metrics.RecordUpstream5xx(
		workspaceID,
		sanitizePathForMetric(path),
		strconv.Itoa(status),
	)
}

// sanitizePathForMetric bounds the cardinality of the `path` label on
// the api_upstream_5xx_total counter by collapsing session-ID-shaped
// path segments to `:id`. Prometheus counters with unbounded
// cardinality (raw session IDs) would exhaust memory.
//
// Currently handles: /session/{id}/message, /session/{id}. Any other
// path passes through verbatim.
func sanitizePathForMetric(path string) string {
	segments := strings.Split(path, "/")
	for i, seg := range segments {
		if strings.HasPrefix(seg, "ses_") {
			segments[i] = ":id"
		}
	}
	return strings.Join(segments, "/")
}
