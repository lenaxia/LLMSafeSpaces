// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package sessionstate

import (
	"testing"

	abiv1 "github.com/lenaxia/llmsafespaces/pkg/abi/v1"
)

// TestReplayRingSkipsSeqlessFrame pins the append-time belt (review r2,
// optional): a frame that consumes no seq must never be ringed — the
// ring's contiguity premise (one seq per retained frame) is what makes
// any lowWater ≤ since ≤ seq cursor exact, and a seq-less head would
// underflow lowWater to ^uint64(0). Unreachable through the authority
// today (both fanoutLocked call sites publish seq'd frames); this pin
// future-proofs the guard against a third frame kind.
func TestReplayRingSkipsSeqlessFrame(t *testing.T) {
	r := replayRing{cap: 8, budget: 1 << 20}
	r.append(&abiv1.StreamFrame{Frame: &abiv1.StreamFrame_Snapshot{Snapshot: &abiv1.SnapshotFrame{}}})
	if len(r.frames) != 0 || r.bytes != 0 {
		t.Fatalf("a seq-less frame must not be ringed: %d frames, %d bytes", len(r.frames), r.bytes)
	}
	if low, high := r.lowWater(7), uint64(7); low != high {
		t.Fatalf("empty ring must stay trivially servable (low == cur), got low=%d cur=%d", low, high)
	}
	// A seq'd frame afterwards rings normally.
	r.append(&abiv1.StreamFrame{Frame: &abiv1.StreamFrame_Event{Event: &abiv1.SequencedEvent{Seq: 1}}})
	if len(r.frames) != 1 {
		t.Fatalf("seq'd frames must ring normally after a skipped seq-less frame, have %d", len(r.frames))
	}
	if low := r.lowWater(7); low != 0 {
		t.Fatalf("lowWater after retaining seq 1 = %d, want 0", low)
	}
}
