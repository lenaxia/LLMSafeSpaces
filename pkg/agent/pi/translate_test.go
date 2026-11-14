// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package pi

// translate_test.go — the rich doc-derived session's translation pins:
// tool fusion (call + result → one ToolPart), error results, orphan
// results, compaction/model-switch/custom/shell surfaces, and the
// unknown-role tolerance.

import (
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/lenaxia/llmsafespaces/pkg/session"
)

func richTranscript(t *testing.T) []session.Message {
	t.Helper()
	f, err := os.Open("testdata/rich_session.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	sum, err := scanSessionFile(f)
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	return sum.transcriptMessages()
}

func TestTranslate_RichSession_TypesAndOrder(t *testing.T) {
	msgs := richTranscript(t)
	wantTypes := []session.MessageType{
		session.MessageModelSwitch, // m1000001
		session.MessageUser,        // e1000003
		session.MessageAssistant,   // e1000004 (fused tools)
		session.MessageAssistant,   // e1000007
		session.MessageShell,       // e1000008
		session.MessageCompaction,  // c1000009
		session.MessageModelSwitch, // m1000010
		session.MessageSystem,      // e1000012 (custom, display)
		session.MessageUser,        // e1000013
		session.MessageAssistant,   // e1000014 (stopReason error)
	}
	if len(msgs) != len(wantTypes) {
		t.Fatalf("transcript length %d != %d: %+v", len(msgs), len(wantTypes), msgs)
	}
	for i, want := range wantTypes {
		if msgs[i].Type != want {
			t.Fatalf("msg[%d] type %q != %q", i, msgs[i].Type, want)
		}
	}
}

func TestTranslate_ToolFusion(t *testing.T) {
	msgs := richTranscript(t)
	asst := msgs[2] // e1000004
	if len(asst.Parts) != 3 {
		t.Fatalf("parts: %+v", asst.Parts)
	}
	// thinking → reasoning part
	if asst.Parts[0].Type != session.PartReasoning || asst.Parts[0].Reasoning != "listing first" {
		t.Fatalf("reasoning part: %+v", asst.Parts[0])
	}
	// bash call + successful result → completed with output
	bash := asst.Parts[1].Tool
	if bash == nil || bash.CallID != "call_bash1" || bash.Name != "bash" || bash.State.Status != session.ToolStatusCompleted {
		t.Fatalf("bash tool: %+v", bash)
	}
	var in map[string]any
	if err := json.Unmarshal(bash.Input, &in); err != nil || in["command"] != "ls" {
		t.Fatalf("bash input: %s (%v)", bash.Input, err)
	}
	var out []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(bash.Output, &out); err != nil || len(out) != 1 || out[0].Text != "main.go" {
		t.Fatalf("bash output: %s (%v)", bash.Output, err)
	}
	// edit call + error result → error status carrying the result
	edit := asst.Parts[2].Tool
	if edit == nil || edit.State.Status != session.ToolStatusError {
		t.Fatalf("edit tool: %+v", edit)
	}
	// Cost from recorded usage (reasoning separate, inside output).
	if asst.Cost == nil || asst.Cost.InputTokens != 500 || asst.Cost.CacheReadTokens != 100 || asst.Cost.ReasoningTokens != 5 || asst.Cost.CostUSD != 0.012 {
		t.Fatalf("cost: %+v", asst.Cost)
	}
}

func TestTranslate_ErrorAssistantAndOrphanResult(t *testing.T) {
	msgs := richTranscript(t)
	last := msgs[len(msgs)-1] // e1000014 stopReason=error
	if last.Error == nil || last.Error.Code != "provider_error" || last.Error.Message != "provider 500" {
		t.Fatalf("error message: %+v", last.Error)
	}
	// The orphan toolResult (call_orphan, no matching call) fuses into
	// nothing — no phantom message.
	for _, m := range msgs {
		for _, p := range m.Parts {
			if p.Tool != nil && p.Tool.CallID == "call_orphan" {
				t.Fatal("orphan toolResult leaked a part")
			}
		}
	}
}

func TestTranslate_ShellCompactionModelSwitchCustom(t *testing.T) {
	msgs := richTranscript(t)
	shell := msgs[4]
	if shell.Command != "make test" || shell.ExitCode == nil || *shell.ExitCode != 0 {
		t.Fatalf("shell: %+v", shell)
	}
	comp := msgs[5]
	if !strings.Contains(comp.Text, "make test passed") {
		t.Fatalf("compaction summary: %q", comp.Text)
	}
	sw := msgs[6]
	if sw.ToModel == nil || sw.ToModel.ID != "fixture-model-2" || sw.ToModel.Provider != "fixture" {
		t.Fatalf("model switch: %+v", sw.ToModel)
	}
	sys := msgs[7]
	if sys.Text != "extension note" {
		t.Fatalf("custom display message: %q", sys.Text)
	}
}

func TestTranslate_UnknownRoleTolerated(t *testing.T) {
	e := &piEntry{ID: "x1", Type: "message", Message: &piAgentMessage{Role: "from_the_future", Content: []byte(`"hi"`)}}
	if _, ok := translateEntry(e, e.Message, nil); ok {
		t.Fatal("unknown role must not translate")
	}
}
