// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package repolint

import (
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"testing"

	"github.com/lenaxia/llmsafespaces/pkg/settings"
)

// TestCanary_SchemaVersion_TwinParity guards against the drift class seen in
// PR #869: the settings SchemaVersion bumped to 11 updated the Go canary's
// expectedSchemaVersion but the Python and TypeScript twins kept 10 — and
// because the Python canary section had never executed in CI (bootstrap bug,
// fixed in #861), the drift surfaced only as an intermittent canary failure
// weeks later. Each twin must equal the authority constant
// (settings.SchemaVersion), so a bump that updates none — or only some — of
// the twins fails here in blocking CI (worklog 0596 precedent).
func TestCanary_SchemaVersion_TwinParity(t *testing.T) {
	root := repoRoot(t)

	twins := map[string]string{
		"go": filepath.Join(root, "sdks", "canary", "go", "scenarios", "s-user-settings", "main.go"),
		"py": filepath.Join(root, "sdks", "canary", "python", "scenarios", "s_user_settings.py"),
		"ts": filepath.Join(root, "sdks", "canary", "typescript", "scenarios", "s-user-settings.ts"),
	}

	patterns := map[string]*regexp.Regexp{
		"go": regexp.MustCompile(`expectedSchemaVersion\s*=\s*(\d+)`),
		"py": regexp.MustCompile(`EXPECTED_SCHEMA_VERSION\s*=\s*(\d+)`),
		"ts": regexp.MustCompile(`EXPECTED_SCHEMA_VERSION\s*=\s*(\d+)`),
	}

	authority := strconv.Itoa(settings.SchemaVersion)
	for name, path := range twins {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("reading %s twin %s: %v", name, path, err)
		}
		m := patterns[name].FindSubmatch(data)
		if m == nil {
			t.Fatalf("%s twin %s: expected-schema-version pattern not found", name, path)
		}
		if string(m[1]) != authority {
			t.Errorf("canary twin drift: %s expects schema version %s, authority settings.SchemaVersion is %s (update sdks/canary/{go,python,typescript} s-user-settings together with pkg/settings/schema.go)", name, m[1], authority)
		}
	}
}
