// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package handlers_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestNoSessionStateDerivationRemains (US-69.11 AC): the API holds no
// session-state derivation from the retired SSE dialect — no tracker
// imports, no dialect emitters, no v2 shadow/pending bookkeeping, no
// unknown-taxonomy classifier wiring. Display state is the pod's
// projection (contract streams); billing and the Epic 28 bridge hang off
// the busy-gated usage stream.
func TestNoSessionStateDerivationRemains(t *testing.T) {
	banned := []string{
		"services/sse\"",
		"services/shadowconsumer",
		"publishClientEvents",
		"emitNormalizedInputEvent",
		"persistContextFromEvent",
		"MeteringFromEvent",
		"RecordAgentEvent",
		"SetTokenSeenStore",
		"V2QueueShadow",
		"v2PendingTracker",
		"wakeStrandedV2Sessions",
		"SubscribeDrain",
	}

	root := "."
	if _, err := os.Stat("../.."); err == nil {
		// Running from api/internal/handlers — walk the api tree.
		root = "../.."
	}
	var offenders []string
	_ = filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return nil
		}
		text := string(data)
		for _, b := range banned {
			if strings.Contains(text, b) {
				offenders = append(offenders, path+": "+b)
			}
		}
		return nil
	})
	if len(offenders) > 0 {
		t.Fatalf("retired session-state derivation remains:\n%s", strings.Join(offenders, "\n"))
	}
}
