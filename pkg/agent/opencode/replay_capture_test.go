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
				t.Logf("%-42s -> %-28s partID=%.12s msgID=%.12s", env.Type, evt.Type.String(), evt.PartId, evt.MessageId)
			}
		})
	}
}
