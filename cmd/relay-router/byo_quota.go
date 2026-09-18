// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"sync"
	"time"
)

// byoWorkspaceQuota bounds the exfil-through-relay residual (design 0058
// §3/§4.7 rule 6): per-workspace request-rate and byte-rate counters over
// a sliding window, with caps and 429 rejection. Byte counters cover BOTH
// directions — the residual is primarily request-direction content
// (prompts), response bytes complete the picture. Counters are in-memory
// per replica — the bound is per-replica, which the two-replica topology
// multiplies by 2; documented, deployment-tunable.
type byoWorkspaceQuota struct {
	window      time.Duration
	maxRequests int64
	maxBytes    int64
	clock       func() time.Time
	mu          sync.Mutex
	workspaces  map[string]*workspaceCounters
}

type workspaceCounters struct {
	buckets []quotaBucket
}

type quotaBucket struct {
	start    time.Time
	requests int64
	bytes    int64
}

func newByoWorkspaceQuota(window time.Duration, maxRequests, maxBytes int64) *byoWorkspaceQuota {
	return &byoWorkspaceQuota{
		window:      window,
		maxRequests: maxRequests,
		maxBytes:    maxBytes,
		clock:       time.Now,
		workspaces:  map[string]*workspaceCounters{},
	}
}

func (q *byoWorkspaceQuota) countersLocked(workspaceID string) *workspaceCounters {
	c, ok := q.workspaces[workspaceID]
	if !ok {
		c = &workspaceCounters{}
		q.workspaces[workspaceID] = c
	}
	return c
}

// Admit atomically checks the request budget and, when the request fits,
// counts it immediately. Admission-time counting is the correctness
// invariant: a request counts against the window from the moment it is
// accepted, NOT when its response completes — otherwise a client that
// observes a completed response can race the previous handler's
// post-stream bookkeeping and slip an over-budget request through
// (pinned by TestQuotaAdmissionCountsInFlight after a CI flake).
// Denied requests never consume quota.
func (q *byoWorkspaceQuota) Admit(workspaceID string) bool {
	if q.maxRequests <= 0 {
		return true
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	c := q.countersLocked(workspaceID)
	q.pruneLocked(c)
	var requests int64
	for _, b := range c.buckets {
		requests += b.requests
	}
	if requests >= q.maxRequests {
		return false
	}
	q.addLocked(c, 1, 0)
	return true
}

// addLocked records requests and/or bytes into the current (or new)
// minute bucket. Callers hold q.mu.
func (q *byoWorkspaceQuota) addLocked(c *workspaceCounters, requests, bytes int64) {
	now := q.clock()
	var b *quotaBucket
	if len(c.buckets) > 0 {
		last := &c.buckets[len(c.buckets)-1]
		if now.Sub(last.start) < time.Minute {
			b = last
		}
	}
	if b == nil {
		c.buckets = append(c.buckets, quotaBucket{start: now})
		b = &c.buckets[len(c.buckets)-1]
	}
	b.requests += requests
	b.bytes += bytes
}

// RecordBytes counts bytes only (both directions; requests are counted
// once at admission via Admit).
func (q *byoWorkspaceQuota) RecordBytes(workspaceID string, bytes int64) {
	q.mu.Lock()
	defer q.mu.Unlock()
	c := q.countersLocked(workspaceID)
	q.pruneLocked(c)
	q.addLocked(c, 0, bytes)
}

// BytesLeft reports the remaining byte budget in the window (cap check
// happens after the response completes — overage is visible in metrics —
// but requests already over budget are refused up front).
func (q *byoWorkspaceQuota) BytesLeft(workspaceID string) int64 {
	if q.maxBytes <= 0 {
		return 1 << 62
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	c := q.countersLocked(workspaceID)
	q.pruneLocked(c)
	var used int64
	for _, b := range c.buckets {
		used += b.bytes
	}
	if used >= q.maxBytes {
		return 0
	}
	return q.maxBytes - used
}

func (q *byoWorkspaceQuota) pruneLocked(c *workspaceCounters) {
	cutoff := q.clock().Add(-q.window)
	live := c.buckets[:0]
	for _, b := range c.buckets {
		if b.start.After(cutoff) {
			live = append(live, b)
		}
	}
	c.buckets = live
}

// Workspaces snapshots the tracked workspace IDs (metrics/tests).
func (q *byoWorkspaceQuota) Workspaces() []string {
	q.mu.Lock()
	defer q.mu.Unlock()
	ids := make([]string, 0, len(q.workspaces))
	for id := range q.workspaces {
		ids = append(ids, id)
	}
	return ids
}
