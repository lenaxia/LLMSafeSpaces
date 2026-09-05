package sessionstate

import (
	"bufio"
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/lenaxia/llmsafespaces/pkg/agent/opencode"
)

// E2E (#1291 r5): mock opencode SSE server serving the PRODUCTION capture
// frames → the authority's real Ingest seam → the delivered surface
// (State snapshot + the Stream frames a subscriber would receive). This
// is the live-render pipeline end to end: wire bytes → parse → project →
// consumer-visible state, no history reload.
func TestAuthorityE2E_SSEStreamToDeliveredSurface(t *testing.T) {
	fixture := "../../../pkg/agent/opencode/testdata/events-tool-turn.txt"
	f, err := os.Open(fixture)
	if err != nil {
		t.Fatalf("committed fixture must exist: %v", err)
	}
	defer f.Close()
	var frames []string
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1024*1024), 1024*1024)
	for sc.Scan() {
		if strings.HasPrefix(sc.Text(), "data: ") {
			frames = append(frames, strings.TrimPrefix(sc.Text(), "data: "))
		}
	}
	if len(frames) == 0 {
		t.Fatal("fixture produced no frames")
	}

	auth, err := New(Config{Parser: &opencode.ABITranslator{}, Passwords: []string{"pw"}, PlatformDir: t.TempDir()})
	if err != nil {
		t.Fatalf("authority construction: %v", err)
	}

	// The delivered surface: subscribe BEFORE streaming (the same
	// ordering production uses).
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stream, unsub, err := auth.Stream(ctx)
	if err != nil {
		t.Fatalf("stream: %v", err)
	}
	defer unsub()

	// Feed the frames through the real ingest seam — in production the
	// tracker's onRawEvent hands the authority exactly these raw SSE
	// payload bytes (main.go:208); the stream subscription above stands
	// in for the API's contract-events consumer.
	go func() {
		for _, frame := range frames {
			auth.Ingest([]byte(frame))
		}
	}()

	// Collect delivered frames until the turn ends (step.ended) or a bound.
	deadline := time.After(10 * time.Second)
	var delivered int
	var toolCompletedSeen bool
	for {
		select {
		case frame := <-stream:
			delivered++
			seqd := frame.GetEvent()
			if seqd == nil {
				continue
			}
			evt := seqd.GetEvent()
			if evt == nil {
				continue
			}
			if evt.Part != nil {
				if tp := evt.Part.GetTool(); tp != nil {
					if evt.Type.String() == "EVENT_TYPE_PART_END" && tp.GetState().GetStatus().String() == "TOOL_STATUS_COMPLETED" {
						if tp.GetName() == "" || len(tp.GetInput()) == 0 || len(tp.GetOutput()) == 0 {
							t.Errorf("delivered tool END incomplete: name=%q input=%d output=%d", tp.GetName(), len(tp.GetInput()), len(tp.GetOutput()))
						} else {
							toolCompletedSeen = true
						}
					}
				}
			}
			if evt.Type.String() == "EVENT_TYPE_MESSAGE_END" && delivered > 4 {
				// the captured turn's terminal step.ended
				goto done
			}
		case <-deadline:
			goto done
		}
	}
done:
	if delivered == 0 {
		t.Fatal("no frames reached the delivered stream surface")
	}
	if !toolCompletedSeen {
		t.Fatal("the completed tool part never reached the delivered surface — a live consumer would spin it forever")
	}
	// The snapshot agrees: same invariants at State().
	state := auth.State()
	for _, sess := range state.Sessions {
		for _, part := range sess.InFlightParts {
			if tp := part.GetTool(); tp != nil && tp.GetState().GetStatus().String() == "TOOL_STATUS_COMPLETED" {
				if tp.GetName() == "" || len(tp.GetOutput()) == 0 {
					t.Errorf("snapshot tool part %s incomplete after delivery", part.GetId())
				}
			}
		}
	}
}
