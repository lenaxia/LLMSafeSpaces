// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package repolint_test

// #1469 follow-up incident (2026-09-19, live pod): /opencode/plugins
// shipped mode 644 — NO execute bit — so uid 1000 (opencode's runtime
// uid) could not traverse the directory and the origin plugin's file://
// import failed silently (agentd refused origin injection; every
// send_message degraded to self-declared mode). Root cause: BuildKit
// applies COPY --chmod to IMPLICITLY CREATED PARENT DIRECTORIES as well
// as the copied entries — the 644 was meant for the .js file, but it
// also landed on the auto-created /plugins. The plugin is DATA (never
// executed directly), so 755 on it is harmless; the directory's
// traversal bit is load-bearing.

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

func TestOpencodeOverlay_PluginsCopyIsTraversable(t *testing.T) {
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			break
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("repo root not found")
		}
		dir = parent
	}
	data, err := os.ReadFile(filepath.Join(dir, "runtimes", "opencode", "Dockerfile"))
	if err != nil {
		t.Fatal(err)
	}
	src := string(data)

	// Every COPY that creates /plugins must carry 755: 644/66x strips
	// the traversal bit from the implicitly created directory.
	re := regexp.MustCompile(`(?m)^COPY[^\n]*plugins[^\n]*$`)
	found := false
	for _, line := range strings.Split(src, "\n") {
		if !re.MatchString(line) {
			continue
		}
		found = true
		if strings.Contains(line, "--chmod=644") || strings.Contains(line, "--chmod=666") || strings.Contains(line, "--chmod=640") {
			t.Errorf("Dockerfile COPY into /plugins uses a non-traversable chmod: %q — BuildKit applies --chmod to implicitly created parent dirs too; /plugins must carry the x bit or uid 1000 cannot traverse and the plugin import fails silently (live incident 2026-09-19)", strings.TrimSpace(line))
		}
	}
	if !found {
		t.Fatal("no COPY line referencing plugins found — the origin plugin delivery moved; update this pin")
	}
}
