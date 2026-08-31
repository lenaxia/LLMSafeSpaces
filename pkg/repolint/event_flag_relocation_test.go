// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package repolint

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestSupervisorExportsEventSystemFlag pins the design 0053 S3 relocation
// of OPENCODE_EXPERIMENTAL_EVENT_SYSTEM: the entrypoint is deleted and the
// export lives in the supervisor's spawn seam (opencodeChildEnv —
// containment per #942: opencode env-var names are runtime knowledge
// behind the agent seam). The flag is load-bearing for context usage
// (worklog 0263: without it opencode never emits the event stream the API
// tracker consumes). If this export is removed, pods boot event-blind and
// #739-class silence returns — THIS file is the guardian.
func TestSupervisorExportsEventSystemFlag(t *testing.T) {
	root := repoRoot(t)
	p := filepath.Join(root, "cmd", "workspace-agentd", "managed_process.go")
	data, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("read supervisor spawn seam: %v", err)
	}
	if !strings.Contains(string(data), `"OPENCODE_EXPERIMENTAL_EVENT_SYSTEM", "true"`) {
		t.Fatalf("opencodeChildEnv must set OPENCODE_EXPERIMENTAL_EVENT_SYSTEM=true — " +
			"it is load-bearing for the event stream (worklog 0263); relocated from the " +
			"deleted entrypoint in design 0053 S3. See pkg/agent/opencode/testdata/REFRESH.md.")
	}
}
