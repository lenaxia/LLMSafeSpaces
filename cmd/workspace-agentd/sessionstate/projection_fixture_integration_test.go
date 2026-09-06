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
			var maxParts int
			var toolCompleteSeen bool
			var busySeen bool
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
				// Sample DURING the turn: the fold's in-flight parts exist
				// only until the terminal step (finish:"stop" → IDLE clears
				// them by design — idle means reconcile-from-history).
				state := auth.State()
				n := 0
				for _, sess := range state.Sessions {
					n += len(sess.InFlightParts)
					if sess.Busy {
						busySeen = true
					}
					for _, part := range sess.InFlightParts {
						if part.GetId() == "" {
							t.Error("snapshot part with empty ID — the fold cannot key it")
						}
						if tp := part.GetTool(); tp != nil {
							// The r1 wipe class: name is known from the first
							// frame (input.started precedes called); INPUT is
							// only guaranteed once the call is complete
							// (input.started carries no input on the wire).
							if tp.GetName() == "" {
								t.Errorf("projected tool part %s lost its name (the wipe bug)", part.GetId())
							}
							if st := tp.GetState().GetStatus(); st == abiToolCompleted {
								if len(tp.GetInput()) == 0 {
									t.Errorf("projected tool part %s completed without input (the wipe bug)", part.GetId())
								}
								toolCompleteSeen = true
								if len(tp.GetOutput()) == 0 {
									t.Errorf("projected tool part %s completed with no output (content[]/structured undecoded)", part.GetId())
								}
							}
						}
					}
				}
				if n > maxParts {
					maxParts = n
				}
			}
			if maxParts == 0 {
				t.Fatal("no parts reached the projected snapshot during the turn")
			}
			if strings.Contains(fixture, "tool") && !toolCompleteSeen {
				t.Fatal("tool fixture: the completed tool part was never observed in the fold")
			}
			// #1292a: the terminal shape — the fixture's turn ENDED, so the
			// session must be IDLE with an empty fold (busy-stuck is the
			// user-facing bug this pins).
			final := auth.State()
			for sid, sess := range final.Sessions {
				if sess.Busy {
					t.Errorf("session %s stuck BUSY after the captured turn ended — the #1292a bug", sid)
				}
				if len(sess.InFlightParts) != 0 {
					t.Errorf("session %s: idle fold still carries %d in-flight parts", sid, len(sess.InFlightParts))
				}
			}
			if !busySeen {
				t.Fatal("the captured turn never showed BUSY — the fixture is not exercising the busy→idle cycle")
			}
		})
	}
}

const abiToolCompleted = abiv1.ToolStatus_TOOL_STATUS_COMPLETED

// Unhappy path: a malformed frame mid-stream must not corrupt the fold —
// the well-formed frames around it still project.
func TestAuthorityProjection_MalformedFrameMidStream(t *testing.T) {
	auth, err := New(Config{Parser: &opencode.ABITranslator{}, Passwords: []string{"pw"}, PlatformDir: t.TempDir()})
	if err != nil {
		t.Fatalf("authority construction: %v", err)
	}
	good := `{"id":"e1","type":"session.next.text.started","properties":{"sessionID":"ses_1","assistantMessageID":"msg_1","textID":"txt_1","text":"hello"}}`
	auth.Ingest([]byte(good))
	// #1291 r5: properties-shape drift on a CLAIMING frame — the
	// (nil, true, err) parser contract. Must be counted and NEVER fatal
	// (applyLocked(nil) was a live SIGSEGV on this exact input).
	auth.Ingest([]byte(`{"id":"e2","type":"session.next.text.started","properties":[1,2,3]}`))
	auth.Ingest([]byte(`{"id":"e2b","type":"session.next.text.delta","properties":{"not":"json"`)) // malformed
	auth.Ingest([]byte(`{"id":"e3","type":"totally.unknown.event","properties":{"sessionID":"ses_1"}}`))
	state := auth.State()
	sess := state.Sessions["ses_1"]
	if sess == nil || len(sess.InFlightParts) == 0 {
		t.Fatal("the well-formed part must survive the malformed and unknown frames")
	}
	if sess.InFlightParts[0].GetId() != "txt_1" {
		t.Errorf("surviving part ID = %q, want txt_1", sess.InFlightParts[0].GetId())
	}
	if got := auth.ParserFailuresForTest(); got < 1 {
		t.Errorf("parser failures = %d, want >= 1 (the shape-drift frame must be counted)", got)
	}
}
