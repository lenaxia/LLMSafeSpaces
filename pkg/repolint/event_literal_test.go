// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package repolint

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/lenaxia/llmsafespaces/pkg/agent/opencode/wire"
)

func writeGoFile(t *testing.T, dir, rel, content string) {
	t.Helper()
	p := filepath.Join(dir, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestEventLiteralCheck_FlagsNewMatchOutsideSeam(t *testing.T) {
	dir := t.TempDir()
	writeGoFile(t, dir, "api/internal/handlers/newthing.go", `package handlers

func f(eventType string) bool {
	return eventType == "session.next.step.ended"
}
`)
	rep, err := EventLiteralCheck(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !rep.HasNew() {
		t.Fatalf("new string-match on an agent event name must be flagged; got %+v", rep.Violations)
	}
	if rep.Violations[0].Literal != "session.next.step.ended" {
		t.Fatalf("wrong literal: %+v", rep.Violations[0])
	}
}

func TestEventLiteralCheck_AllowedPrefixesExempt(t *testing.T) {
	dir := t.TempDir()
	writeGoFile(t, dir, "pkg/agent/opencode/wire/wire.go", `package wire

func f(t string) bool { return t == "session.updated" }
`)
	writeGoFile(t, dir, "cmd/workspace-agentd/tracker.go", `package main

func f(t string) bool { return t == "session.status" }
`)
	rep, err := EventLiteralCheck(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Violations) != 0 {
		t.Fatalf("seam + agentd must be exempt; got %+v", rep.Violations)
	}
}

func TestEventLiteralCheck_KnownLeakTolerated(t *testing.T) {
	// The shipped map is empty since US-69.13 (the last leaks migrated
	// behind the adapter seam); the tolerance MECHANISM stays — seed an
	// entry the way a future known leak would, and verify it is reported
	// as tolerated (leaked=true), not new.
	restore := eventLiteralKnownLeaks
	eventLiteralKnownLeaks = map[string]string{
		"api/internal/handlers/proxy_events.go": "test-seeded leak (mechanism coverage)",
	}
	defer func() { eventLiteralKnownLeaks = restore }()

	dir := t.TempDir()
	writeGoFile(t, dir, "api/internal/handlers/proxy_events.go", `package handlers

func f(eventType string) bool { return eventType == "session.updated" }
`)
	rep, err := EventLiteralCheck(dir)
	if err != nil {
		t.Fatal(err)
	}
	if rep.HasNew() {
		t.Fatalf("known leak file must be tolerated (leaked=true); got %+v", rep.Violations)
	}
	if len(rep.Violations) != 1 || !rep.Violations[0].IsLeaked {
		t.Fatalf("leak must still be REPORTED as tolerated; got %+v", rep.Violations)
	}
}

func TestEventLiteralCheck_EmissionNotFlagged(t *testing.T) {
	dir := t.TempDir()
	writeGoFile(t, dir, "api/internal/handlers/emitter.go", `package handlers

func emit() Event { return Event{Type: "session.status"} }
`)
	rep, err := EventLiteralCheck(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Violations) != 0 {
		t.Fatalf("struct-literal emission is the platform's own event name — not a match; got %+v", rep.Violations)
	}
}

func TestEventLiteralCheck_CaseAndNeqContexts(t *testing.T) {
	dir := t.TempDir()
	writeGoFile(t, dir, "api/internal/foo/bar.go", `package foo

func f(t string) bool {
	switch t {
	case "message.part.updated":
		return true
	}
	return t != "step-finish"
}
`)
	rep, err := EventLiteralCheck(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Violations) != 2 {
		t.Fatalf("case and != contexts must both flag; got %+v", rep.Violations)
	}
}

func TestEventLiteralCheck_RealRepo_OnlyKnownLeaks(t *testing.T) {
	// The repository itself must produce only tolerated leaks — a new
	// string-match anywhere outside the seam fails this test (and CI).
	rep, err := EventLiteralCheck(repoRoot(t))
	if err != nil {
		t.Fatal(err)
	}
	for _, v := range rep.Violations {
		if !v.IsLeaked {
			t.Fatalf("NEW agent-event string match outside the seam: %s:%d (%s) — %s\nMove it behind pkg/agent/opencode (see design/0049) or add a dated knownLeaks entry with an issue pointer.", v.File, v.Line, v.Literal, v.Excerpt)
		}
	}
}

func TestEventLiteralCheck_CommaCaseMapKeyAndContains(t *testing.T) {
	dir := t.TempDir()
	writeGoFile(t, dir, "api/x/y.go", `package x

import "strings"

var m = map[string]int{}

func f(t, s string) int {
	switch t {
	case "session.updated", "other":
	}
	if strings.Contains(s, "step-finish") {
	}
	return m["message.part.delta"]
}
`)
	rep, err := EventLiteralCheck(dir)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, v := range rep.Violations {
		got[v.Literal] = true
	}
	for _, want := range []string{"session.updated", "step-finish", "message.part.delta"} {
		if !got[want] {
			t.Errorf("literal %q must be flagged (comma-case/map-key/Contains contexts); got %+v", want, rep.Violations)
		}
	}
}

// TestEventLiteralsSyncedWithWireTaxonomy enforces the runbook invariant
// (REFRESH.md): the wire taxonomy and the lint's literal table gain new
// types together. Every EVENT-type literal the lint flags must be known
// to wire.IsKnownEventType — otherwise a fixture refresh updates one
// list and not the other, and events the seam accepts get lint-flagged
// (or worse, vice versa). Part-type literals (step-start/step-finish)
// live inside message.part.updated payloads, not in the event taxonomy,
// and are exempt.
func TestEventLiteralsSyncedWithWireTaxonomy(t *testing.T) {
	exempt := map[string]bool{"step-start": true, "step-finish": true}
	for _, lit := range agentEventLiterals {
		if exempt[lit] {
			continue
		}
		if !wire.IsKnownEventType(lit) {
			t.Errorf("lint literal %q is not in the wire taxonomy — extend both together (REFRESH.md upgrade runbook; wire.knownEventTypes)", lit)
		}
	}
}
