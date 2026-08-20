// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package repolint

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestEntrypointExportsEventSystemFlag pins the #942 relocation of
// OPENCODE_EXPERIMENTAL_EVENT_SYSTEM from the controller's pod builder to
// the runtime entrypoint. The flag is load-bearing for context usage
// (worklog 0263 live-cluster experiment: without it opencode never emits
// the event stream the API tracker consumes). If this export is removed,
// pods boot event-blind and #739-class silence returns — with the
// controller-side pin removed, THIS file is the guardian.
func TestEntrypointExportsEventSystemFlag(t *testing.T) {
	root := repoRoot(t)
	p := filepath.Join(root, "runtimes", "base", "tools", "entrypoints", "entrypoint-opencode.sh")
	data, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("read entrypoint: %v", err)
	}
	if !strings.Contains(string(data), "OPENCODE_EXPERIMENTAL_EVENT_SYSTEM=true") {
		t.Fatalf("entrypoint must export OPENCODE_EXPERIMENTAL_EVENT_SYSTEM=true — " +
			"it is load-bearing for the event stream (worklog 0263); relocated from the " +
			"controller in #942. See pkg/agent/opencode/testdata/REFRESH.md.")
	}
}
