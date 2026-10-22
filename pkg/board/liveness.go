// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package board

// liveness.go — platform-derived claim liveness (#1847 spec items 1-4).
//
// PRIMARY liveness is what the PLATFORM observes about the claim's
// session, never what a worker DECLARES: an open turn is alive (it
// holds the lease across arbitrarily long silent tool calls — the
// exact case heartbeats cannot cover), and recent observed activity
// with a STARTED session is alive within the lease grace. Worker-
// declared timestamps retire as the liveness input (the C1 incident
// class dies by construction); the heartbeat tool remains only as an
// optional manual override with server-clock stamps (PR1's clamp).
//
// The observability source is the in-workspace agentd, which pushes
// per-session samples over the pod SA-token seam (the resource-pressure
// reporter precedent) into board_session_liveness; RC1 classifies every
// lapsed claim against that projection before reclaiming:
//
//	alive   → lease re-armed (attempts untouched — nothing was lost);
//	dead+started
//	        → ONE pre-reclaim appeal ping at the session's turn
//	          boundary, an appeal window on the row, reclaim if silent;
//	none    (no row / never started — the stillborn spawn) → straight
//	          reclaim: a stillborn spawn is never renewed.
//
// Server-clock authority holds on this path too: any future-stamped
// sample field clamps to the classifying server's now — a garbage or
// future timestamp cannot make a session look infinitely fresh.

import "time"

// LivenessSample is one session's platform-observed liveness (the
// board_session_liveness row shape). Nil pointer = no row (the session
// never pushed — stillborn spawn or agentd N-1).
type LivenessSample struct {
	Busy    bool
	Started bool
	// LastActivity is the session's observed event watermark as pushed.
	LastActivity time.Time
	// PushedAt is when the sample landed (server-stamped at upsert).
	PushedAt time.Time
}

// LivenessVerdict is RC1's classification of one lapsed claim.
type LivenessVerdict string

const (
	// LivenessNone: no liveness row — the session never pushed. The
	// stillborn-spawn class: never renewed, no appeal (nothing can
	// answer it).
	LivenessNone LivenessVerdict = "none"
	// LivenessAlive: platform liveness holds the claim — busy (open
	// turn, pushes fresh) or started with activity inside the grace.
	LivenessAlive LivenessVerdict = "alive"
	// LivenessDead: a row exists, the session started, but neither arm
	// holds — the appeal class (one ping, then reclaim).
	LivenessDead LivenessVerdict = "dead"
)

// Lapse reasons (tasks.lapse_reason vocabulary; the escalation-reason
// input, spec item 6).
const (
	LapseNoAssignee        = "no_assignee"         // the spawn never stamped an identity
	LapseSessionUnknown    = "session_unknown"     // no liveness row — stillborn spawn
	LapseSilentBeyondGrace = "silent_beyond_grace" // started, idle, activity older than the grace
	LapseStaleLiveness     = "stale_liveness"      // pushes stopped (pod down) — the busy latch expired
	LapseAppealIgnored     = "appeal_ignored"      // pinged at the turn boundary, silent through the window
)

// ClassifyLiveness decides one lapsed claim's verdict from the
// session's sample. grace bounds the activity arm (spec item 1:
// ~2x the lease window); staleBound bounds how long a latched busy
// stays authoritative without a fresh push (a pod that stops pushing
// cannot hold leases forever — the wedged-turn release). A sample's
// activity stamp clamps to its OWN push anchor — information cannot be
// newer than when it was pushed — so a garbage/future stamp is exactly
// as fresh as the row's honest push time, never more.
//
// stub: #1847 red-first — behavior lands with the RC1 rework.
func ClassifyLiveness(now time.Time, sample *LivenessSample, grace, staleBound time.Duration) LivenessVerdict {
	_ = now
	_ = sample
	_ = grace
	_ = staleBound
	return LivenessNone
}
