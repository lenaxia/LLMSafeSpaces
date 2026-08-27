// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package attachments

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
)

// TestReadmeManifestSnippetMatchesGoldenFixture is e2e row E12: the
// README-LLM "File Attachments" manifest snippet must byte-match the
// authoritative golden fixture, so documentation and implementation
// cannot drift apart. The snippet is fenced between HTML sentinel
// comments; the ``` fences carry no bytes of the contract.
func TestReadmeManifestSnippetMatchesGoldenFixture(t *testing.T) {
	raw, err := os.ReadFile("../../../README-LLM.md")
	if err != nil {
		t.Fatalf("read README-LLM.md: %v", err)
	}
	const begin = "<!-- begin:attachment-manifest-v1 -->"
	const end = "<!-- end:attachment-manifest-v1 -->"
	beginIdx := strings.Index(string(raw), begin)
	endIdx := strings.Index(string(raw), end)
	if beginIdx < 0 || endIdx < beginIdx {
		t.Fatal("README-LLM.md has no attachment-manifest-v1 snippet block")
	}
	block := string(raw[beginIdx+len(begin) : endIdx])

	lines := strings.Split(block, "\n")
	var content []string
	inFence := false
	for _, line := range lines {
		switch {
		case strings.TrimSpace(line) == "```text":
			inFence = true
		case inFence && strings.TrimSpace(line) == "```":
			inFence = false
		case inFence:
			content = append(content, line)
		}
	}
	snippet := strings.Join(content, "\n") + "\n"
	if snippet == "\n" {
		t.Fatal("snippet block contains no fenced content")
	}

	wantRaw, err := os.ReadFile("testdata/compose_one_file.want.json")
	if err != nil {
		t.Fatalf("read golden fixture: %v", err)
	}
	var want string
	if err := json.Unmarshal(wantRaw, &want); err != nil {
		t.Fatalf("decode golden fixture: %v", err)
	}

	if snippet != want {
		t.Errorf("README-LLM manifest snippet drifted from the golden fixture:\n snippet: %q\n want:    %q", snippet, want)
	}
}
