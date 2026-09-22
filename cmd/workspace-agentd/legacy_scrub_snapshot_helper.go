// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import "github.com/lenaxia/llmsafespaces/pkg/agentd"

// legacyScrubSnapshotFor adapts the tracker to the healthz snapshot
// convention (nil-tracker → nil-snapshot → omitted field).
func legacyScrubSnapshotFor(t *legacyScrubTracker) func() *agentd.LegacyScrubHealth {
	return func() *agentd.LegacyScrubHealth { return t.snapshot() }
}
