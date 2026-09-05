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

// E2E happy path (#1291): the PRODUCTION capture frames fed through the
// authority's real Ingest seam (the bytes the tracker's onRawEvent hands
// it in production) → the delivered surface (Stream frames + State
// snapshot a live consumer sees). No mock server, no history reload: wire
// bytes → parse → project → consumer-visible state.
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
	fed := make(chan struct{})
	go func() {
		defer close(fed)
		for _, frame := range frames {
			auth.Ingest([]byte(frame))
		}
	}()
	defer func() { <-fed }() // join before cleanup: the feed must not race TempDir removal

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
			if toolCompletedSeen {
				// Exit on the invariant, not a frame-order guess: the first
				// MESSAGE_END in the capture is the mid-turn step boundary
				// ("finish":"tool-calls") — step 2 exists after it.
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

// E2E unhappy paths (#1291 r6): tool-failure mid-turn through the real
// wiring to the delivered surface; malformed frame mid-stream with a
// LIVE subscriber (delivery continues); restart-mid-turn (fresh
// translator, late success) leaves the bubble running, not wiped.
func TestAuthorityE2E_UnhappyPaths(t *testing.T) {
	t.Run("tool failure mid-turn", func(t *testing.T) {
		auth, err := New(Config{Parser: &opencode.ABITranslator{}, Passwords: []string{"pw"}, PlatformDir: t.TempDir()})
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		stream, unsub, _ := auth.Stream(ctx)
		defer unsub()
		frames := []string{
			`{"id":"e1","type":"session.next.tool.called","properties":{"sessionID":"ses_F","assistantMessageID":"msg_F","callID":"call_F","tool":"bash","input":{"command":"false"}}}`,
			`{"id":"e2","type":"session.next.tool.failure","properties":{"sessionID":"ses_F","assistantMessageID":"msg_F","callID":"call_F","error":{"type":"exec","message":"exit 1: boom"}}}`,
		}
		for _, fr := range frames {
			auth.Ingest([]byte(fr))
		}
		var sawFailedEnd bool
		deadline := time.After(5 * time.Second)
		for !sawFailedEnd {
			select {
			case frame := <-stream:
				seqd := frame.GetEvent()
				if seqd == nil {
					continue
				}
				evt := seqd.GetEvent()
				if evt == nil {
					continue
				}
				if evt.Type.String() == "EVENT_TYPE_PART_END" {
					if tp := evt.Part.GetTool(); tp != nil {
						if tp.GetState().GetStatus().String() != "TOOL_STATUS_ERROR" {
							t.Errorf("failure END status = %s, want ERROR", tp.GetState().GetStatus())
						}
						if tp.GetName() != "bash" || len(tp.GetInput()) == 0 {
							t.Errorf("failure END lost name/input: name=%q", tp.GetName())
						}
						if !strings.Contains(string(tp.GetOutput()), "boom") {
							t.Errorf("failure END output = %q, want the error text", tp.GetOutput())
						}
						sawFailedEnd = true
					}
				}
			case <-deadline:
				t.Fatal("the failure tool END never reached the delivered surface")
			}
		}
	})

	t.Run("malformed frame mid-stream with live subscriber", func(t *testing.T) {
		auth, err := New(Config{Parser: &opencode.ABITranslator{}, Passwords: []string{"pw"}, PlatformDir: t.TempDir()})
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		stream, unsub, _ := auth.Stream(ctx)
		defer unsub()
		good1 := `{"id":"g1","type":"session.next.text.started","properties":{"sessionID":"ses_M","assistantMessageID":"msg_M","textID":"txt_M","text":"part one"}}`
		auth.Ingest([]byte(good1))
		auth.Ingest([]byte(`{"id":"b1","type":"session.next.text.delta","properties":{"not":"json"`))
		auth.Ingest([]byte(`{"id":"b2","type":"session.next.text.started","properties":[1,2,3]}`))
		good2 := `{"id":"g2","type":"session.next.text.delta","properties":{"sessionID":"ses_M","assistantMessageID":"msg_M","textID":"txt_M","delta":" continues"}}`
		auth.Ingest([]byte(good2))
		// Delivery must CONTINUE past the malformed frames: the delta for
		// the surviving part arrives on the stream.
		var deltaDelivered bool
		deadline := time.After(5 * time.Second)
		for !deltaDelivered {
			select {
			case frame := <-stream:
				seqd := frame.GetEvent()
				if seqd == nil {
					continue
				}
				evt := seqd.GetEvent()
				if evt == nil {
					continue
				}
				if evt.Type.String() == "EVENT_TYPE_PART_DELTA" && evt.PartId == "txt_M" {
					deltaDelivered = true
				}
			case <-deadline:
				t.Fatal("delivery stopped at the malformed frames — the live stream did not continue")
			}
		}
	})

	t.Run("restart mid-turn leaves the bubble running", func(t *testing.T) {
		// The original translator dies mid-turn (fresh instance simulates
		// the restart); the late success is a memo-miss on the new
		// instance and must be DROPPED — the delivered part stays RUNNING,
		// never wiped.
		old := &opencode.ABITranslator{}
		start := `{"id":"e1","type":"session.next.tool.called","properties":{"sessionID":"ses_R","assistantMessageID":"msg_R","callID":"call_R","tool":"bash","input":{"command":"x"}}}`
		if _, ok, err := old.Parse([]byte(start)); err != nil || !ok {
			t.Fatalf("called: ok=%v err=%v", ok, err)
		}
		auth, err := New(Config{Parser: old, Passwords: []string{"pw"}, PlatformDir: t.TempDir()})
		if err != nil {
			t.Fatal(err)
		}
		auth.Ingest([]byte(start))
		// Restart: the authority's parser swaps to the fresh instance.
		fresh := &opencode.ABITranslator{}
		auth.SetParserForTest(fresh)
		late := `{"id":"e2","type":"session.next.tool.success","properties":{"sessionID":"ses_R","assistantMessageID":"msg_R","callID":"call_R","content":[{"type":"text","text":"late"}],"structured":{"exit":0}}}`
		auth.Ingest([]byte(late))
		// The projected snapshot keeps the part RUNNING with name+input.
		state := auth.State()
		sess := state.Sessions["ses_R"]
		if sess == nil || len(sess.InFlightParts) == 0 {
			t.Fatal("the running tool part vanished on the restart window (the wipe)")
		}
		tp := sess.InFlightParts[0].GetTool()
		if tp == nil {
			t.Fatal("expected a tool part")
		}
		if tp.GetState().GetStatus().String() != "TOOL_STATUS_RUNNING" {
			t.Errorf("post-restart status = %s, want RUNNING (memo-miss END dropped, bubble preserved)", tp.GetState().GetStatus())
		}
		if tp.GetName() != "bash" {
			t.Errorf("post-restart name = %q, want bash", tp.GetName())
		}
	})
}
