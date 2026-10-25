package app

import (
	"fmt"
	"regexp"
	"strings"
	"testing"

	"github.com/lenaxia/llmsafespaces/pkg/board"
)

// TestBoardRunID_EpisodeUnique (#1957): two dispatches of the SAME task
// must never share a RunID — agentd's workflow-agent mux drops duplicate
// (workflow, run) keys, which starved re-claimed tasks of their charter
// (the live stillborn-spawn class).
func TestBoardRunID_EpisodeUnique(t *testing.T) {
	n := 0
	mk := func(stage string) *board.Claim {
		n++
		return &board.Claim{TaskID: "3eec1981-b775-4934-bd52-90e9baca03da", Stage: stage,
			ClaimToken: fmt.Sprintf("tok-%d", n)}
	}
	seen := map[string]bool{}
	for i := 0; i < 2; i++ {
		for _, stage := range []string{"", board.StageInProgress, board.StageInReview} {
			id := boardRunID(mk(stage))
			if seen[id] {
				t.Fatalf("RunID reuse across episodes: %s", id)
			}
			seen[id] = true
			if !strings.HasPrefix(id, "3eec1981-b775-4934-bd52-90e9baca03da-") {
				t.Fatalf("RunID lost its task prefix: %s", id)
			}
		}
	}
	// worker arms ("" and in_progress) share the "w" tag family; review arms tag their stage.
	if !regexp.MustCompile(`^3eec1981-b775-4934-bd52-90e9baca03da-w-tok-`).MatchString(boardRunID(mk(""))) {
		t.Fatalf("worker RunID shape unexpected: %s", boardRunID(mk("")))
	}
	if !strings.Contains(boardRunID(mk(board.StageInReview)), "-in_review-") {
		t.Fatalf("review RunID shape unexpected: %s", boardRunID(mk(board.StageInReview)))
	}
}
