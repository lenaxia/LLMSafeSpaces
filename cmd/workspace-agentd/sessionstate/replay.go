// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package sessionstate

import (
	"google.golang.org/protobuf/proto"

	abiv1 "github.com/lenaxia/llmsafespaces/pkg/abi/v1"
)

// The bounded replay ring (design 0069, amending design 0055 M1's
// no-replay ruling): every frame the authority publishes — harness
// events, reseed notices, orphan-sweep aborts, platform-local action
// events; anything that consumes a seq and reaches fanoutLocked — is
// ALSO appended here, evict-oldest under two independent budgets:
// a frame-count cap and a byte budget. A reconnecting Events consumer
// that presents its last applied seq (EventsRequest.since) is served
// the retained frames with seq > since and then goes live, gapless by
// construction (StreamSince). Consumers whose cursor fell below the
// low-water mark get today's snapshot-first behavior — the stamped
// snapshot (I2) remains the authority for since soundness: any
// since >= low-water is served exactly because the ring holds every
// published frame after it.
//
// Memory: the ring shares the already-published frame pointers (the
// same immutability discipline fanout relies on — the projection
// clones everything it retains, so frames are never mutated after
// publish). Worst case is the byte budget plus one oversized frame;
// the defaults bound that at 16 MiB + 1 frame per authority.
const (
	// defaultReplayFrames is the frame-count cap. 1024 frames covers a
	// full turn's event traffic with margin (a busy turn emits tens to
	// low hundreds of frames); beyond it, evict-oldest.
	defaultReplayFrames = 1024
	// defaultReplayBytes is the byte budget, the bound that actually
	// protects memory when frames are large. 16 MiB mirrors the SSE
	// line-buffer precedent (session_tracker.go: a 100K-file patch
	// event is ~10 MB — the ring must absorb bursts of large frames
	// without becoming the pod's biggest allocation).
	defaultReplayBytes = 16 << 20
)

// replayRing is the bounded retained-frame window. All methods assume
// the authority lock is held (appends happen inside fanoutLocked; reads
// happen under the same hold as subscriber registration, which is what
// makes resume gapless by construction).
type replayRing struct {
	frames []*abiv1.StreamFrame
	bytes  int
	cap    int // frame-count cap (Config.ReplayRingFrames; default applied in New)
	budget int // byte budget (Config.ReplayRingBytes; default applied in New)
}

// append retains one published frame, then evicts oldest until both
// budgets hold. The newest frame is ALWAYS retained even when it alone
// exceeds the byte budget: a ring that dropped it would publish a seq
// no future cursor can ever be served from, for a one-frame saving —
// and lowWater() would otherwise advance past a seq that was never
// evicted for cause. The empty-slot nil keeps evicted frames
// collectable while the backing array is reused.
func (r *replayRing) append(frame *abiv1.StreamFrame) {
	size := proto.Size(frame)
	r.frames = append(r.frames, frame)
	r.bytes += size
	for len(r.frames) > 1 && (len(r.frames) > r.cap || r.bytes > r.budget) {
		r.bytes -= proto.Size(r.frames[0])
		r.frames[0] = nil
		r.frames = r.frames[1:]
	}
}

// frameSeq extracts the consuming seq of any published frame kind
// (events and reseed notices both carry one; snapshot frames are never
// ringed).
func frameSeq(f *abiv1.StreamFrame) uint64 {
	if e := f.GetEvent(); e != nil {
		return e.GetSeq()
	}
	if n := f.GetReseeded(); n != nil {
		return n.GetSeq()
	}
	return 0
}

// lowWater is the smallest since the ring can serve exactly: with
// frames retained, any since >= oldest-1 replays from the oldest frame
// (seqs are contiguous — one seq per published frame); with an empty
// ring (boot, post-eviction of everything, or after a process restart)
// the trivially servable floor is the authority's current seq, since
// there is nothing before it left to miss. cur is the authority's seq
// under the same lock hold.
func (r *replayRing) lowWater(cur uint64) uint64 {
	if len(r.frames) == 0 {
		return cur
	}
	return frameSeq(r.frames[0]) - 1
}

// after returns the retained frames with seq > since, in publish
// order. Caller holds the lock; the returned slice is freshly built so
// concurrent appends after the lock releases cannot mutate it (the
// frame pointers themselves are shared-but-immutable).
func (r *replayRing) after(since uint64) []*abiv1.StreamFrame {
	out := make([]*abiv1.StreamFrame, 0, len(r.frames))
	for _, f := range r.frames {
		if frameSeq(f) > since {
			out = append(out, f)
		}
	}
	return out
}
