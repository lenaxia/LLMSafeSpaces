package opencode

import (
	"bufio"
	"encoding/json"
	"os"
	"strings"
	"testing"
)

// Replays a live-captured opencode event stream (production, pinned
// 1.18.10, #1288 fix 2) through the real translator. The captures are
// embedded as fixtures so the test needs no cluster.
func TestReplayCapturedEvents(t *testing.T) {
	for _, fixture := range []string{"testdata/events-text-turn.txt", "testdata/events-tool-turn.txt"} {
		t.Run(fixture, func(t *testing.T) {
			f, err := os.Open(fixture)
			if err != nil {
				t.Skip("fixture missing")
			}
			defer f.Close()
			tr := ABITranslator{}
			sc := bufio.NewScanner(f)
			sc.Buffer(make([]byte, 1024*1024), 1024*1024)
			for sc.Scan() {
				line := sc.Text()
				if !strings.HasPrefix(line, "data: ") {
					continue
				}
				var env struct {
					ID         string          `json:"id"`
					Type       string          `json:"type"`
					Properties json.RawMessage `json:"properties"`
				}
				if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &env); err != nil {
					continue
				}
				raw, _ := json.Marshal(map[string]any{"id": env.ID, "type": env.Type, "properties": env.Properties})
				evt, ok, _ := tr.Parse(raw)
				if !ok || evt == nil {
					continue
				}
				// #1288 fix 2 invariants: every PART_* event carries a
				// non-empty partID and messageID — the frontend keys on
				// partID and partitions per messageID; either empty
				// breaks the live renderer.
				switch evt.Type.String() {
				case "EVENT_TYPE_PART_START", "EVENT_TYPE_PART_END":
					if evt.PartId == "" {
						t.Errorf("%s -> %s: partID empty (frontend cannot key)", env.Type, evt.Type.String())
					}
					if evt.MessageId == "" {
						t.Errorf("%s -> %s: messageID empty (frontend cannot partition per message)", env.Type, evt.Type.String())
					}
				case "EVENT_TYPE_PART_DELTA":
					if evt.PartId == "" {
						t.Errorf("%s -> PART_DELTA: partID empty (frontend drops the delta)", env.Type)
					}
				}
				// #1291 r1: a tool END must carry the COMPLETE part — the
				// success frame has no name/input on the wire, and
				// consumers replace-by-key: an END without them wipes the
				// running bubble. The output must contain the result
				// (content[]/structured flattened).
				if env.Type == "session.next.tool.success" && evt.Type.String() == "EVENT_TYPE_PART_END" {
					tp := evt.Part.GetTool()
					if tp == nil {
						t.Fatalf("tool.success -> PART_END carries no ToolPart")
					}
					if tp.Name == "" {
						t.Errorf("tool.success END: name empty — wipes the bubble (memo not recalled)")
					}
					if len(tp.Input) == 0 {
						t.Errorf("tool.success END: input empty — wipes the bubble (memo not recalled)")
					}
					if len(tp.Output) == 0 {
						t.Errorf("tool.success END: output empty — result never renders live (content[]/structured not decoded)")
					}
				}
				t.Logf("%-42s -> %-28s partID=%.12s msgID=%.12s", env.Type, evt.Type.String(), evt.PartId, evt.MessageId)
			}
		})
	}
}

// The failure path: a tool.error/failure frame must also carry the
// complete part (memo-recalled name/input) and surface the error text.
func TestTranslateNextTool_FailureCarriesCompletePart(t *testing.T) {
	tr := ABITranslator{}
	start := `{"sessionID":"ses_1","assistantMessageID":"msg_1","callID":"call_1","tool":"bash","input":{"command":"false"}}`
	evtStart, ok, err := tr.Parse([]byte(`{"id":"e1","type":"session.next.tool.called","properties":` + start + `}`))
	if err != nil || !ok || evtStart.Part.GetTool() == nil {
		t.Fatalf("called frame: ok=%v err=%v", ok, err)
	}
	fail := `{"sessionID":"ses_1","assistantMessageID":"msg_1","callID":"call_1","error":{"type":"unknown","message":"boom"}}`
	evtFail, ok, err := tr.Parse([]byte(`{"id":"e2","type":"session.next.tool.failure","properties":` + fail + `}`))
	if err != nil || !ok {
		t.Fatalf("failure frame: ok=%v err=%v", ok, err)
	}
	tp := evtFail.Part.GetTool()
	if tp == nil {
		t.Fatal("failure END carries no ToolPart")
	}
	if tp.Name != "bash" {
		t.Errorf("failure END name=%q, want bash (memo recall)", tp.Name)
	}
	if len(tp.Input) == 0 {
		t.Error("failure END input empty — wipes the bubble")
	}
	if len(tp.Output) == 0 || !strings.Contains(string(tp.Output), "boom") {
		t.Errorf("failure END output=%q, want the error text", tp.Output)
	}
	if tp.State.GetStatus().String() != "TOOL_STATUS_ERROR" {
		t.Errorf("failure END status=%s, want ERROR", tp.State.GetStatus())
	}
}
