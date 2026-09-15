// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package repolint

import (
	"strings"
	"testing"
)

func TestAgentIDPrefixCheck_FlagsMatchOutsideSeam(t *testing.T) {
	dir := t.TempDir()
	writeGoFile(t, dir, "api/internal/handlers/x.go", `package handlers

import "strings"

func dispatch(id string) string {
	if strings.HasPrefix(id, "que_") {
		return "question"
	}
	return ""
}
`)
	rep, err := AgentIDPrefixCheck(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !rep.HasNew() {
		t.Fatalf("que_ prefix match in a handler must be flagged; got %+v", rep.Violations)
	}
	if rep.Violations[0].Prefix != "que_" {
		t.Fatalf("wrong prefix: %+v", rep.Violations[0])
	}
}

func TestAgentIDPrefixCheck_AllowedPathsExempt(t *testing.T) {
	dir := t.TempDir()
	body := `package opencode

import "strings"

func dispatch(id string) string {
	if strings.HasPrefix(id, "que_") {
		return "question"
	}
	return ""
}
`
	writeGoFile(t, dir, "pkg/agent/opencode/y.go", body)
	writeGoFile(t, dir, "cmd/workspace-agentd/actor.go", `package main

import "strings"

func dispatch(id string) bool {
	return strings.HasPrefix(id, "per_") || strings.HasPrefix(id, "ses_")
}
`)
	writeGoFile(t, dir, "pkg/repolint/selftable.go", `package repolint

var table = []string{"msg_"}
`)
	// The sanctioned MCP dispatch fast-path (#1302 4a-2 r3): per-file
	// allowlist entry, not a known leak.
	writeGoFile(t, dir, "pkg/mcp/server.go", `package mcp

import "strings"

func dispatch(id string) string {
	switch {
	case strings.HasPrefix(id, "que_"):
		return "q"
	case strings.HasPrefix(id, "per_"):
		return "p"
	}
	return ""
}
`)
	// Golden fixtures are non-Go payloads; a stray .go there is still
	// inside the seam prefix and exempt.
	writeGoFile(t, dir, "pkg/agent/opencode/testdata/note.go", "package testdata\n")

	rep, err := AgentIDPrefixCheck(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Violations) != 0 {
		t.Fatalf("seam + agentd + repolint + sanctioned mcp dispatch must be exempt; got %+v", rep.Violations)
	}
}

func TestAgentIDPrefixCheck_SiblingDirStillFlagged(t *testing.T) {
	// The allowlist entry is the FILE pkg/mcp/server.go, not the
	// directory: a new file in pkg/mcp/ carrying a prefix match must
	// still fail (no silent widening).
	dir := t.TempDir()
	writeGoFile(t, dir, "pkg/mcp/other.go", `package mcp

import "strings"

func f(id string) bool { return strings.HasPrefix(id, "ses_") }
`)
	rep, err := AgentIDPrefixCheck(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !rep.HasNew() {
		t.Fatalf("pkg/mcp/other.go is not the sanctioned dispatch file; got %+v", rep.Violations)
	}
}

func TestAgentIDPrefixCheck_AnchoredNoFalsePositives(t *testing.T) {
	dir := t.TempDir()
	writeGoFile(t, dir, "api/internal/services/billing/billing.go", `package billing

import "strings"

const label = "question_answered"

var quotas = map[string]int{"per_user": 5, "per_node": 2}

func f(s string) bool {
	return strings.HasPrefix(s, "question_") || strings.Contains(s, "last_msg_id")
}

func eq(id string) string {
	if id == "sessionstate" {
		return "user_session"
	}
	return ""
}
`)
	rep, err := AgentIDPrefixCheck(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Violations) != 0 {
		t.Fatalf("question_/per_user/mid-literal substrings are not agent ID prefixes (anchor at literal start); got %+v", rep.Violations)
	}
}

func TestAgentIDPrefixCheck_CommentsNotFlagged(t *testing.T) {
	dir := t.TempDir()
	writeGoFile(t, dir, "api/internal/handlers/doc.go", `package handlers

// The agent mints que_/per_/ses_/msg_ IDs behind the seam.
// dispatch: que_ means question, per_ means permission, ses_ session, msg_ message.
`)
	rep, err := AgentIDPrefixCheck(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Violations) != 0 {
		t.Fatalf("prose comments are out of scope (as event_literal); got %+v", rep.Violations)
	}
}

func TestAgentIDPrefixCheck_MatchContexts(t *testing.T) {
	dir := t.TempDir()
	writeGoFile(t, dir, "api/x/contexts.go", `package x

import "strings"

var m = map[string]int{}

func f(id, s string, kind string) (int, string) {
	if id == "ses_abc123" {
		kind = "session"
	}
	if "msg_08cb3ab7" != s {
		s = ""
	}
	switch kind {
	case "que_norecord":
		return 1, ""
	}
	n := m["per_42"]
	if strings.Contains(s, "ses_f73747f8") {
		n++
	}
	minted := "msg_" + id
	if strings.HasPrefix(s, "que_") {
		minted += "?"
	}
	return n, minted
}
`)
	rep, err := AgentIDPrefixCheck(dir)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, v := range rep.Violations {
		got[v.Prefix] = true
	}
	for _, want := range []string{"que_", "per_", "ses_", "msg_"} {
		if !got[want] {
			t.Errorf("prefix %q must be flagged (==/!=/case/map-key/Contains/concat/HasPrefix contexts); got %+v", want, rep.Violations)
		}
	}
	if len(rep.Violations) < 7 {
		t.Errorf("each context must produce its own violation; got %d: %+v", len(rep.Violations), rep.Violations)
	}
}

func TestAgentIDPrefixCheck_EmissionNotFlagged(t *testing.T) {
	// Precedent: event_literal's emission exemption. Argument-position
	// full-ID literals (e.g. the SDK canary's negative probe
	// "ses_nonexistent00000000000000") are not matching contexts.
	dir := t.TempDir()
	writeGoFile(t, dir, "sdks/canary/go/scenarios/probe/main.go", `package main

func probe(c Client) error {
	return c.Send("ses_nonexistent00000000000000", "ping")
}
`)
	rep, err := AgentIDPrefixCheck(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Violations) != 0 {
		t.Fatalf("argument-position literals are not matches (emission exemption); got %+v", rep.Violations)
	}
}

func TestAgentIDPrefixCheck_KnownLeakTolerated(t *testing.T) {
	// The shipped map is empty at birth (#1305); the MECHANISM stays.
	// Seed an entry the way a future known leak would and verify it is
	// reported as tolerated (IsLeaked=true), not new.
	restore := agentIDPrefixKnownLeaks
	agentIDPrefixKnownLeaks = map[string]string{
		"api/internal/handlers/legacy.go": "legacy dispatch pending seam migration (#9999)",
	}
	defer func() { agentIDPrefixKnownLeaks = restore }()

	dir := t.TempDir()
	writeGoFile(t, dir, "api/internal/handlers/legacy.go", `package handlers

import "strings"

func f(id string) bool { return strings.HasPrefix(id, "que_") }
`)
	rep, err := AgentIDPrefixCheck(dir)
	if err != nil {
		t.Fatal(err)
	}
	if rep.HasNew() {
		t.Fatalf("known leak file must be tolerated; got %+v", rep.Violations)
	}
	if len(rep.Violations) != 1 || !rep.Violations[0].IsLeaked {
		t.Fatalf("leak must still be REPORTED as tolerated; got %+v", rep.Violations)
	}
}

func TestAgentIDPrefixCheck_TestFilesExcluded(t *testing.T) {
	dir := t.TempDir()
	writeGoFile(t, dir, "api/internal/handlers/x_test.go", `package handlers

import "strings"

func TestX(t *testing.T) {
	_ = strings.HasPrefix("que_1", "que_")
}
`)
	rep, err := AgentIDPrefixCheck(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Violations) != 0 {
		t.Fatalf("_test.go files are out of scope (production code shape, as event_literal); got %+v", rep.Violations)
	}
}

func TestAgentIDPrefixCheck_RealRepo_BirthStateClean(t *testing.T) {
	// #1305's birth-state assertion: zero violations on the real tree.
	// The input-surface prefixes live behind the seams (#1302/#1371);
	// pkg/mcp/server.go's dispatch fast-path is allowlisted per-file.
	rep, err := AgentIDPrefixCheck(repoRoot(t))
	if err != nil {
		t.Fatal(err)
	}
	for _, v := range rep.Violations {
		if !v.IsLeaked {
			t.Fatalf("NEW agent ID-prefix match outside the seam: %s:%d (%s) — %s\nMove it behind pkg/agent/opencode (see design/0049) or add a dated knownLeaks entry with an issue pointer.", v.File, v.Line, v.Prefix, v.Excerpt)
		}
	}
	if len(rep.Violations) != 0 {
		t.Fatalf("rule ships with zero known-leaks entries at birth (#1305); got %+v", rep.Violations)
	}
}

func TestAgentIDPrefixKnownLeaks_MetaValidation(t *testing.T) {
	// Shipped map must be empty at birth.
	if len(agentIDPrefixKnownLeaks) != 0 {
		t.Fatalf("agentIDPrefixKnownLeaks must be empty at birth (#1305); got %+v", agentIDPrefixKnownLeaks)
	}
	// Every entry must carry a reason and an issue pointer — the
	// event_literal/agent_import knownLeaks contract.
	bad := map[string]string{
		"api/x.go": "",
	}
	if err := validateKnownLeaksEntries("agentIDPrefixKnownLeaks", bad); err == nil {
		t.Fatal("entry without reason must fail meta-validation")
	}
	bad = map[string]string{
		"api/x.go": "no issue pointer",
	}
	if err := validateKnownLeaksEntries("agentIDPrefixKnownLeaks", bad); err == nil {
		t.Fatal("entry without issue pointer must fail meta-validation")
	}
	good := map[string]string{
		"api/x.go": "legacy dispatch pending seam migration (#9999)",
	}
	if err := validateKnownLeaksEntries("agentIDPrefixKnownLeaks", good); err != nil {
		t.Fatalf("entry with reason + issue pointer must pass: %v", err)
	}
	// The shipped (empty) map must validate clean.
	if err := validateKnownLeaksEntries("agentIDPrefixKnownLeaks", agentIDPrefixKnownLeaks); err != nil {
		t.Fatalf("shipped map must validate: %v", err)
	}
}

func TestAgentIDPrefixCheck_ConstAndRegexLaundering(t *testing.T) {
	// #1305 scopes the rule to "string literals / regex patterns":
	// declaring a prefix constant or compiling a prefix regex outside
	// the seam is dispatch knowledge, whatever the indirection.
	dir := t.TempDir()
	writeGoFile(t, dir, "api/internal/handlers/laundry.go", `package handlers

import (
	"regexp"
	"strings"
)

const quePrefix = "que_"

var perPrefix = "per_"

var sesRe = regexp.MustCompile("^ses_")

var idRe = regexp.MustCompile(`+"`"+`^(que|msg)_[a-z]+$`+"`"+`)

func f(id string) bool {
	return strings.HasPrefix(id, quePrefix) || sesRe.MatchString(id) || idRe.MatchString(id)
}
`)
	rep, err := AgentIDPrefixCheck(dir)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]int{}
	for _, v := range rep.Violations {
		got[v.Prefix]++
	}
	for _, want := range []string{"que_", "per_", "ses_", "msg_"} {
		if got[want] == 0 {
			t.Errorf("laundered %q (const/var declaration or regex pattern) must be flagged; got %+v", want, rep.Violations)
		}
	}
}

func TestAgentIDPrefixCheck_TrailingAndBlockCommentsNotFlagged(t *testing.T) {
	dir := t.TempDir()
	writeGoFile(t, dir, "api/internal/handlers/prose.go", `package handlers

func f(i int) int {
	// legacy: if id == "ses_abc" { ... } — removed with the seam migration
	i := 0 // trailing prose: old code did id == "ses_abc" here
	/*
	 * block prose mentioning que_ and per_ dispatch
	 */
	return i
}
`)
	rep, err := AgentIDPrefixCheck(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Violations) != 0 {
		t.Fatalf("full-line, trailing, and block prose comments are out of scope; got %+v", rep.Violations)
	}
}

func TestAgentIDPrefixCheck_ReportOrdering(t *testing.T) {
	dir := t.TempDir()
	writeGoFile(t, dir, "api/x/b.go", `package x

import "strings"

func f(id string) bool { return strings.HasPrefix(id, "msg_") }
`)
	writeGoFile(t, dir, "api/x/a.go", `package x

import "strings"

func f(id string) bool { return strings.HasPrefix(id, "ses_") }
`)
	rep, err := AgentIDPrefixCheck(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Violations) != 2 {
		t.Fatalf("expected 2 violations; got %+v", rep.Violations)
	}
	if rep.Violations[0].File != "api/x/a.go" || rep.Violations[1].File != "api/x/b.go" {
		t.Fatalf("violations must be sorted by file then line; got %+v", rep.Violations)
	}
	if !strings.Contains(rep.Violations[0].File, "a.go") || rep.Violations[0].Line == 0 {
		t.Fatalf("violations must carry file:line; got %+v", rep.Violations[0])
	}
}
