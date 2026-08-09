// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package session

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// forbiddenIdentifiers are opencode-specific tokens that must never appear in
// the platform-owned contract (Design 0049 §4.1 rule 1: "No agent identifier
// leaks"). opencode's ses_/msg_ ID prefixes and the "opencode" name itself
// are the high-signal tokens; "patch" is intentionally NOT forbidden because
// it is the unified-diff term (authoritative per §4.1 rule 4), not an
// opencode identifier.
var forbiddenIdentifiers = []string{
	"opencode",
	"ses_",
	"msg_",
	"verbose",
}

func TestWireOutputContainsNoAgentIdentifiers(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Millisecond)
	representative := []any{
		Session{ID: "s1", WorkspaceID: "ws", Status: StatusBusy, Model: &ModelRef{ID: "m"}},
		UserMessage("m1", "hi", now),
		AssistantMessage("m2", []Part{{Type: PartText, Text: "x"}}, now),
		Part{Type: PartTool, Tool: &ToolPart{Name: "bash", State: ToolState{Status: ToolStatusRunning}}},
		Part{Type: PartFileChange, FileChange: &FileDiff{Path: "a", Status: ChangeModified, Patch: "@@ -1 +1 @@"}},
		Event{Type: EventError, Timestamp: now, Status: StatusBusy, Error: &Error{Message: "boom"}},
		InputRequest{ID: "i", Kind: InputQuestion, Question: "q"},
		SendOpts{Admission: AdmissionSteer},
	}
	for _, v := range representative {
		out, err := json.Marshal(v)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		lower := strings.ToLower(string(out))
		for _, tok := range forbiddenIdentifiers {
			if strings.Contains(lower, tok) {
				t.Fatalf("wire output contains forbidden agent identifier %q: %s", tok, out)
			}
		}
	}
}

func TestPackageSourceContainsNoAgentIdentifiers(t *testing.T) {
	// Scan non-test source files for agent-specific identifiers. Comments and
	// identifiers in this contract package must stay agent-neutral; the seam
	// is pkg/agent/opencode/, not here.
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatalf("glob: %v", err)
	}
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		body, err := os.ReadFile(f)
		if err != nil {
			t.Fatalf("read %s: %v", f, err)
		}
		lower := strings.ToLower(string(body))
		for _, tok := range forbiddenIdentifiers {
			if strings.Contains(lower, tok) {
				t.Fatalf("%s contains forbidden agent identifier %q", f, tok)
			}
		}
	}
}
