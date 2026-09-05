package sessionstate

import (
	"bufio"
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/lenaxia/llmsafespaces/pkg/agent/opencode"
)

// Integration (#1291 r4): production-captured opencode event streams →
// the authority's REAL Ingest seam (raw bytes, the parser path) → the
// projected SNAPSHOT read back via State() — asserting what consumers
// actually see, not the translated events on the way in.
func TestAuthorityProjection_FixtureReplayIntegration(t *testing.T) {
	for _, fixture := range []string{
		"../../../pkg/agent/opencode/testdata/events-text-turn.txt",
		"../../../pkg/agent/opencode/testdata/events-tool-turn.txt",
	} {
		t.Run(fixture, func(t *testing.T) {
			f, err := os.Open(fixture)
			if err != nil {
				t.Fatalf("committed fixture must exist: %v", err)
			}
			defer f.Close()
			auth, err := New(Config{Parser: &opencode.ABITranslator{}, Passwords: []string{"pw"}, PlatformDir: t.TempDir()})
			if err != nil {
				t.Fatalf("authority construction: %v", err)
			}
			sc := bufio.NewScanner(f)
			sc.Buffer(make([]byte, 1024*1024), 1024*1024)
			for sc.Scan() {
				line := sc.Text()
				if !strings.HasPrefix(line, "data: ") {
					continue
				}
				// Feed the RAW frame through the authority's real ingest
				// seam — the same bytes production parses.
				var envelope struct {
					ID         string          `json:"id"`
					Type       string          `json:"type"`
					Properties json.RawMessage `json:"properties"`
				}
				if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &envelope); err != nil {
					continue
				}
				raw, _ := json.Marshal(map[string]any{"id": envelope.ID, "type": envelope.Type, "properties": envelope.Properties})
				auth.Ingest(raw)
			}
			// SNAPSHOT-level assertions: what the frontend's fold renders.
			state := auth.State()
			totalParts := 0
			toolParts := 0
			for _, sess := range state.Sessions {
				for _, part := range sess.InFlightParts {
					totalParts++
					if part.GetId() == "" {
						t.Error("snapshot part with empty ID — the fold cannot key it")
					}
					if tp := part.GetTool(); tp != nil {
						toolParts++
						// The r1 wipe class: a completed tool part must
						// retain name AND input in the projected snapshot.
						if tp.GetName() == "" {
							t.Errorf("projected tool part %s lost its name (the wipe bug)", part.GetId())
						}
						if len(tp.GetInput()) == 0 {
							t.Errorf("projected tool part %s lost its input (the wipe bug)", part.GetId())
						}
					}
				}
			}
			if totalParts == 0 {
				t.Fatal("no parts reached the projected snapshot")
			}
			if strings.Contains(fixture, "tool") && toolParts == 0 {
				t.Fatal("tool fixture produced no projected tool parts")
			}
		})
	}
}

// Unhappy path: a malformed frame mid-stream must not corrupt the fold —
// the well-formed frames around it still project.
func TestAuthorityProjection_MalformedFrameMidStream(t *testing.T) {
	auth, err := New(Config{Parser: &opencode.ABITranslator{}, Passwords: []string{"pw"}, PlatformDir: t.TempDir()})
	if err != nil {
		t.Fatalf("authority construction: %v", err)
	}
	good := `{"id":"e1","type":"session.next.text.started","properties":{"sessionID":"ses_1","assistantMessageID":"msg_1","textID":"txt_1","text":"hello"}}`
	auth.Ingest([]byte(good))
	auth.Ingest([]byte(`{"id":"e2","type":"session.next.text.delta","properties":{"not":"json"`)) // malformed
	auth.Ingest([]byte(`{"id":"e3","type":"totally.unknown.event","properties":{"sessionID":"ses_1"}}`))
	state := auth.State()
	sess := state.Sessions["ses_1"]
	if sess == nil || len(sess.InFlightParts) == 0 {
		t.Fatal("the well-formed part must survive the malformed and unknown frames")
	}
	if sess.InFlightParts[0].GetId() != "txt_1" {
		t.Errorf("surviving part ID = %q, want txt_1", sess.InFlightParts[0].GetId())
	}
}
