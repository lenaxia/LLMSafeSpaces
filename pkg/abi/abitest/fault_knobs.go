// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package abitest

import (
	"context"
	"net/http"
	"strings"
	"time"
)

// fault_knobs.go — the epic-71 / 2b fault-matrix controls (#1312 change
// item 1, remaining legs): the reference server documents its failure
// modes in executable form. A reference implementation that only behaves
// correctly tests nothing about robustness. Every knob follows the
// established idioms: mutex-guarded, composable with the others, state
// inspectable for assertions, one-shot where determinism demands it.
// Test scaffolding only — production code must not import this package.

// DeliveryCall is one observed Deliver invocation (leg 7): the replay
// sequence — duplicates and out-of-order attempts included — is the
// assertion surface for S2 (turn uniqueness) and S9 (ack exactly-once).
type DeliveryCall struct {
	EntryID string
	Attempt uint32
}

// CorruptMode selects the wire-drift shape (leg 10, the #1308 class):
// schema-drifted bytes riding a valid HTTP 200 — the layer that defeats
// hand-adjacent parsers while passing transport checks.
type CorruptMode int

const (
	CorruptModeUnset CorruptMode = iota
	// CorruptInvalidJSON truncates the JSON body mid-object.
	CorruptInvalidJSON
	// CorruptTrailingGarbage appends non-JSON bytes after valid JSON.
	CorruptTrailingGarbage
	// CorruptEmptyBody returns a 200 with zero bytes.
	CorruptEmptyBody
	// CorruptHTMLErrorPage returns a 200 carrying an HTML error page —
	// the classic reverse-proxy shape.
	CorruptHTMLErrorPage
)

func (m CorruptMode) String() string {
	switch m {
	case CorruptInvalidJSON:
		return "invalid_json"
	case CorruptTrailingGarbage:
		return "trailing_garbage"
	case CorruptEmptyBody:
		return "empty_body"
	case CorruptHTMLErrorPage:
		return "html_error_page"
	}
	return "unset"
}

// corruption is the armed one-shot leg-10 state.
type corruption struct {
	procedureSuffix string
	mode            CorruptMode
}

type faultKnobs struct {
	recordDeliveries bool
	deliverCalls     []DeliveryCall
	deliverDelay     time.Duration
	nextCorruption   corruption
}

func (f *faultKnobs) record(call DeliveryCall) {
	if !f.recordDeliveries {
		return
	}
	f.deliverCalls = append(f.deliverCalls, call)
}

// RecordDeliverCalls arms leg-7 call recording: every Deliver invocation
// that PASSES validation (and the file-part gate) is appended verbatim —
// duplicates and out-of-order attempts are the signal; rejected
// invocations never reach the record point. DeliverCalls reads the
// sequence. Recording relies on the caller holding s.mu (single call
// site: the Deliver handler).
func (s *Server) RecordDeliverCalls() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.knobs.recordDeliveries = true
}

// DeliverCalls returns the recorded call sequence (nil until armed).
func (s *Server) DeliverCalls() []DeliveryCall {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]DeliveryCall(nil), s.knobs.deliverCalls...)
}

// DelayDeliverAck arms fault leg 8 (harness slowness at boundary): the
// Deliver handler stalls for d before answering — the slow-turn shape
// the delivery poll window (3.5min) and admission ctx (3min) must
// survive. The stall honors ctx cancellation. Zero disarms.
func (s *Server) DelayDeliverAck(d time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.knobs.deliverDelay = d
}

// DeliverAckDelay reports the armed stall (knob-state inspection).
func (s *Server) DeliverAckDelay() time.Duration {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.knobs.deliverDelay
}

// CorruptNextResponse arms fault leg 10 (one-shot): the NEXT request
// whose procedure path ends with procedureSuffix is answered with
// schema-corrupted bytes per mode — an HTTP 200 carrying the drift
// shape, exactly the class that defeated the #1308 parser. Empty
// suffix matches nothing.
func (s *Server) CorruptNextResponse(procedureSuffix string, mode CorruptMode) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.knobs.nextCorruption = corruption{procedureSuffix: procedureSuffix, mode: mode}
}

// CorruptNextResponseArmed reports whether a corruption is stored. An
// empty-suffix arming reports armed while matching nothing — the guard
// in takeCorruption is the load-bearing check that keeps it inert (its
// removal would hijack every procedure; pinned by
// TestCorruptNextResponse_EmptySuffixMatchesNothing).
func (s *Server) CorruptNextResponseArmed() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.knobs.nextCorruption.mode != CorruptModeUnset
}

// takeCorruption consumes the armed corruption if the path matches.
func (s *Server) takeCorruption(path string) (corruption, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	c := s.knobs.nextCorruption
	if c.mode == CorruptModeUnset || c.procedureSuffix == "" || !strings.HasSuffix(path, c.procedureSuffix) {
		return corruption{}, false
	}
	s.knobs.nextCorruption = corruption{}
	return c, true
}

// stallDeliverAck sleeps the armed leg-8 delay, honoring ctx.
func (s *Server) stallDeliverAck(ctx context.Context) {
	s.mu.Lock()
	d := s.knobs.deliverDelay
	s.mu.Unlock()
	if d <= 0 {
		return
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
	case <-timer.C:
	}
}

// corruptBodies per mode — 200s carrying drift bytes.
func corruptBody(mode CorruptMode) string {
	switch mode {
	case CorruptInvalidJSON:
		return `{"sessionId":"s1"`
	case CorruptTrailingGarbage:
		return `{"sessionId":"s1"}trailing-garbage`
	case CorruptEmptyBody:
		return ""
	case CorruptHTMLErrorPage:
		return `<html><body><h1>502 Bad Gateway</h1></body></html>`
	}
	return ""
}

// faultMiddleware wraps the ABI handler with the leg-10 corruption
// seam: an armed, matching request never reaches the typed handler —
// the wire itself is the fault surface.
func faultMiddleware(s *Server, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if c, ok := s.takeCorruption(r.URL.Path); ok {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(corruptBody(c.mode)))
			return
		}
		next.ServeHTTP(w, r)
	})
}
