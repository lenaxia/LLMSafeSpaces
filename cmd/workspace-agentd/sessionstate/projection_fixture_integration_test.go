package sessionstate

import (
	"bufio"
	"encoding/json"
	"os"
	"strings"
	"testing"

	abiv1 "github.com/lenaxia/llmsafespaces/pkg/abi/v1"
	"github.com/lenaxia/llmsafespaces/pkg/agent/opencode"
)

// Integration (#1291 r2): production-captured opencode event streams →
// the REAL translator → the REAL authority projection, end to end. Pins
// the live-render invariants the frontend depends on: events carry
// keyed part IDs and message attribution, and tool parts survive
// completion with their name intact.
func TestAuthorityProjection_FixtureReplayIntegration(t *testing.T) {
	for _, fixture := range []string{
		"../../pkg/agent/opencode/testdata/events-text-turn.txt",
		"../../pkg/agent/opencode/testdata/events-tool-turn.txt",
	} {
		t.Run(fixture, func(t *testing.T) {
			f, err := os.Open(fixture)
			if err != nil {
				t.Skip("fixture missing")
			}
			defer f.Close()
			tr := &opencode.ABITranslator{}
			auth, err := New(Config{})
			if err != nil {
				t.Fatalf("authority construction: %v", err)
			}
			toolNames := map[string]string{}
			sc := bufio.NewScanner(f)
			sc.Buffer(make([]byte, 1024*1024), 1024*1024)
			partEvents := 0
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
				auth.IngestForTest(evt)
				switch evt.Type {
				case abiv1.EventType_EVENT_TYPE_PART_START, abiv1.EventType_EVENT_TYPE_PART_END:
					partEvents++
					if evt.PartId == "" || evt.MessageId == "" {
						t.Errorf("%s -> %s: partID/messageID must both be non-empty", env.Type, evt.Type)
					}
					if tp := evt.Part.GetTool(); tp != nil && tp.GetName() != "" {
						toolNames[evt.PartId] = tp.GetName()
					}
					if evt.Type == abiv1.EventType_EVENT_TYPE_PART_END {
						if tp := evt.Part.GetTool(); tp != nil {
							if name, seen := toolNames[evt.PartId]; seen && tp.GetName() == "" {
								t.Errorf("tool END for %s lost its name (%s was set at START) — the wipe bug", evt.PartId, name)
							}
						}
					}
				case abiv1.EventType_EVENT_TYPE_PART_DELTA:
					if evt.PartId == "" {
						t.Errorf("%s: PART_DELTA with empty partID is dropped by the frontend", env.Type)
					}
				}
			}
			if partEvents == 0 {
				t.Fatal("no part events reached the projection")
			}
		})
	}
}
