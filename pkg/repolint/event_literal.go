// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package repolint

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// agentEventLiterals is the opencode event taxonomy used by the
// event-literal rule. Keep in sync with wire.IsKnownEventType's set and
// the golden fixtures (pkg/agent/opencode/testdata/REFRESH.md) — the
// fixture-coverage test in the wire package forces taxonomy extensions
// to land there; extend this list in the same change.
// "session.status" is deliberately NOT listed: the platform's own SSE
// broker emits an event of the same name (apitypes.WorkspaceSSEEvent in
// proxy_events.go), so platform code legitimately matches it against the
// BROKER stream (e.g. pkg/mcp/client.go's idle wait). Lexical
// discrimination between the two streams is impossible; the agent-wire
// matches on it live behind the seam or in agentd (both allowlisted).
var agentEventLiterals = []string{
	"session.updated", "session.created",
	"session.idle", "session.diff", "session.error",
	"message.created", "message.updated",
	"message.part.updated", "message.part.delta",
	"session.next.step.ended", "session.next.step.started", "session.next.step.failed",
	"session.next.prompt.admitted", "session.next.prompted",
	"session.next.text.started", "session.next.text.delta", "session.next.text.ended",
	"step-start", "step-finish",
}

// eventLiteralAllowedPrefixes are the paths permitted to string-match
// agent event names: the opencode seam itself and the in-pod agentd (a
// sanctioned seam consumer — repolint's agent-import allowlist matches).
var eventLiteralAllowedPrefixes = []string{
	"pkg/agent/opencode/",
	"cmd/workspace-agentd/",
	"pkg/repolint/", // this rule's own literal table
}

// eventLiteralKnownLeaks tolerates existing platform-code string matches
// on agent event names until their seam migration lands. Each entry must
// carry a reason and issue pointer; the rule fails on any NEW match.
var eventLiteralKnownLeaks = map[string]string{
	"api/internal/services/sse/tracker.go":  "busy/idle dispatch keyed on session.status; routes via Adapter.Stream when the Epic 65 SSE bridge migrates the tracker (see #938 worklog Next Steps)",
	"api/internal/handlers/proxy_events.go": "title/parent persistence dispatch on session.updated; exact-match verified live-correct (wire unsuffixed), fold into the adapter when #940 touches session surfaces",
	"api/internal/handlers/proxy_v2.go":     "V2 session-queue bridge constants (session.next.prompt.admitted/prompted); Epic 63 surface, migrates with the V2 bridge",
}

// EventLiteralViolation is one instance of agent event-name string
// matching found outside the seam.
type EventLiteralViolation struct {
	File     string
	Line     int
	Literal  string
	Excerpt  string
	IsLeaked bool // true when the file is a tolerated known leak
}

// EventLiteralReport aggregates the check's findings.
type EventLiteralReport struct {
	Violations []EventLiteralViolation
}

// HasNew returns true when any violation is not a tolerated leak.
func (r EventLiteralReport) HasNew() bool {
	for _, v := range r.Violations {
		if !v.IsLeaked {
			return true
		}
	}
	return false
}

// EventLiteralCheck scans every non-test .go file outside the seam for
// string-matching on opencode event names. The agent-import rule cannot
// see string coupling — the exact class the frontend's dead
// session.next.step.ended listener demonstrated (#939). New matches
// fail; known leaks are tolerated with reasons.
func EventLiteralCheck(root string) (EventLiteralReport, error) {
	var report EventLiteralReport
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		if filepath.Ext(path) != ".go" || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		rel, rerr := filepath.Rel(root, path)
		if rerr != nil {
			rel = path
		}
		norm := filepath.ToSlash(rel)
		if anyPrefix(norm, eventLiteralAllowedPrefixes) {
			return nil
		}
		data, rerr := os.ReadFile(path) //nolint:gosec // G122: repo-rooted lint scan (same trust domain as agent_import.go's importsOf)
		if rerr != nil {
			return nil
		}
		leaked := false
		if _, ok := eventLiteralKnownLeaks[norm]; ok {
			leaked = true
		}
		for i, line := range strings.Split(string(data), "\n") {
			trimmed := strings.TrimSpace(line)
			if strings.HasPrefix(trimmed, "//") {
				continue
			}
			for lit, pats := range agentEventLiteralIndex {
				for _, p := range pats {
					if p.MatchString(line) {
						report.Violations = append(report.Violations, EventLiteralViolation{
							File:     norm,
							Line:     i + 1,
							Literal:  agentEventLiterals[lit],
							Excerpt:  truncateFor(trimmed, 100),
							IsLeaked: leaked,
						})
						break
					}
				}
			}
		}
		return nil
	})
	if err != nil {
		return EventLiteralReport{}, err
	}
	sort.Slice(report.Violations, func(i, j int) bool {
		if report.Violations[i].File != report.Violations[j].File {
			return report.Violations[i].File < report.Violations[j].File
		}
		return report.Violations[i].Line < report.Violations[j].Line
	})
	return report, nil
}

func truncateFor(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// agentEventLiteralIndex is the per-literal pattern table (comparison
// contexts only — emission sites are the platform's own event names).
var agentEventLiteralIndex = buildAgentEventLiteralIndex()

func buildAgentEventLiteralIndex() [][]*regexp.Regexp {
	idx := make([][]*regexp.Regexp, len(agentEventLiterals))
	for i, lit := range agentEventLiterals {
		escaped := regexp.QuoteMeta(lit)
		idx[i] = []*regexp.Regexp{
			regexp.MustCompile(`(?:==|!=)\s*"` + escaped + `"`),
			regexp.MustCompile(`"` + escaped + `"\s*(?:==|!=)`),
			// case "lit": and comma-lists: case "lit",  /  case "a", "lit":
			regexp.MustCompile(`case\s+(?:[a-zA-Z0-9_."]+,\s*)*"` + escaped + `"\s*[:,]`),
			// map-key lookups: m["lit"]
			regexp.MustCompile(`\[\s*"` + escaped + `"\s*\]`),
			// substring/prefix guards: strings.Contains(x, "lit") / HasPrefix(x, "lit")
			regexp.MustCompile(`(?:Contains|HasPrefix|HasSuffix|TrimPrefix|TrimSuffix)\([^,)]+\s*,\s*"` + escaped + `"`),
		}
	}
	return idx
}
