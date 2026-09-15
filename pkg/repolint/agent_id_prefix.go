// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package repolint

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// agentIDPrefixes are the agent's ID lexicon (design/0049 discipline
// rule 2): question, permission, session, and message identifiers all
// carry one of these prefixes. Matching on them outside the seam is
// agent knowledge — the exact coupling this rule bans.
var agentIDPrefixes = []string{"que_", "per_", "ses_", "msg_"}

// agentIDPrefixAllowedPaths are the paths permitted to match on the
// agent's ID prefixes:
//   - the opencode seam itself (incl. its dialect dispatch and the
//     golden fixtures under testdata/, listed explicitly per #1305);
//   - the in-pod agentd (the actor's seam-local helpers
//     isQuestionID/isPermissionID and the ledger's msg_ minting);
//   - this rule's own literal table.
//
// pkg/mcp/server.go is a FILE entry (not a directory): the MCP
// run_resolve tool's prefix dispatch fast-path was sanctioned at the
// #1302 4a-2 review chain (PR #1363) — "dispatch, never validation" (see
// pkg/agent/opencode/dialect.go's header comment). The entry is
// per-file so any OTHER file under pkg/mcp/ introducing a prefix match
// still fails. Conflict flagged in worklogs/NNNN_2026-09-15_repolint-
// prefix-and-marker-rules.md and the #1305 PR description; retire it
// when the dispatch moves behind the dialect seam.
var agentIDPrefixAllowedPaths = []string{
	"pkg/agent/opencode/",
	"pkg/agent/opencode/testdata/",
	"cmd/workspace-agentd/",
	"pkg/repolint/",
	"pkg/mcp/server.go",
}

// agentIDPrefixKnownLeaks tolerates existing prefix matches outside the
// allowlist until their seam migration lands. Each entry must carry a
// reason and issue pointer (validateKnownLeaksEntries enforces the
// shape); the rule fails on any NEW match. Empty at birth (#1305) —
// the input surface's prefix literals were confined to the seams by
// #1302 (PR #1363), and pkg/mcp/server.go's dispatch is allowlisted
// above, not leaked here.
var agentIDPrefixKnownLeaks = map[string]string{}

// AgentIDPrefixViolation is one instance of an agent ID-prefix match
// found outside the allowed paths.
type AgentIDPrefixViolation struct {
	File     string
	Line     int
	Prefix   string
	Excerpt  string
	IsLeaked bool // true when the file is a tolerated known leak
}

// AgentIDPrefixReport aggregates the check's findings.
type AgentIDPrefixReport struct {
	Violations []AgentIDPrefixViolation
}

// HasNew returns true when any violation is not a tolerated leak.
func (r AgentIDPrefixReport) HasNew() bool {
	for _, v := range r.Violations {
		if !v.IsLeaked {
			return true
		}
	}
	return false
}

// AgentIDPrefixCheck scans every non-test .go file outside the allowed
// paths for matching, minting, or laundering contexts on the agent's
// ID prefixes: equality, case clauses, map-key lookups, the
// strings.Contains/HasPrefix/TrimPrefix family, concatenation minting
// ("msg_" + x), declaration/assignment of prefix literals (const
// quePrefix = "que_"), and regex-pattern dispatch
// (regexp.MustCompile("^que_") / ("^(que|per)_")). The literal must
// START with the prefix (anchored immediately after the opening quote)
// — "question_" is not "que_", and "per_user" as a mid-literal
// substring is not a permission ID. Prose (full-line, trailing, and
// block comments) and argument-position literals (emission, per
// event_literal's precedent) are out of scope; the boundary protected
// is production dispatch shape, so _test.go files are excluded as in
// event_literal.
func AgentIDPrefixCheck(root string) (AgentIDPrefixReport, error) {
	var report AgentIDPrefixReport
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
		if anyPrefix(norm, agentIDPrefixAllowedPaths) {
			return nil
		}
		data, rerr := os.ReadFile(path) //nolint:gosec // G122: repo-rooted lint scan (same trust domain as agent_import.go's importsOf)
		if rerr != nil {
			return nil
		}
		leaked := false
		if _, ok := agentIDPrefixKnownLeaks[norm]; ok {
			leaked = true
		}
		for i, line := range strings.Split(string(data), "\n") {
			trimmed := strings.TrimSpace(line)
			if isCommentLine(trimmed) {
				continue
			}
			code := stripTrailingComment(line)
			trimmedCode := strings.TrimSpace(code)
			seen := map[string]bool{}
			for _, p := range agentIDPrefixIndex {
				if seen[p.prefix] || !p.match.MatchString(code) {
					continue
				}
				seen[p.prefix] = true
				report.Violations = append(report.Violations, AgentIDPrefixViolation{
					File:     norm,
					Line:     i + 1,
					Prefix:   p.prefix,
					Excerpt:  truncateFor(trimmedCode, 100),
					IsLeaked: leaked,
				})
			}
		}
		return nil
	})
	if err != nil {
		return AgentIDPrefixReport{}, err
	}
	sort.Slice(report.Violations, func(i, j int) bool {
		if report.Violations[i].File != report.Violations[j].File {
			return report.Violations[i].File < report.Violations[j].File
		}
		return report.Violations[i].Line < report.Violations[j].Line
	})
	return report, nil
}

// idPrefixPattern binds one prefix to its match-context regexes.
type idPrefixPattern struct {
	prefix string
	match  *regexp.Regexp
}

// agentIDPrefixIndex is the per-prefix match-context table. All
// contexts anchor the prefix immediately after an opening quote ("
// or `) so the literal must START with the prefix — the `question_` ≠
// `que_` anchoring #1305 requires. Declaration and regex-pattern
// laundering (const x = "que_"; regexp.MustCompile("^que_")) is in
// scope per #1305's "string literals / regex patterns"; the bare
// alternation forms additionally match `que` inside a regex group
// (`(que|per)_`), where no underscore follows the prefix.
var agentIDPrefixIndex = buildAgentIDPrefixIndex()

func buildAgentIDPrefixIndex() []idPrefixPattern {
	idx := make([]idPrefixPattern, 0, len(agentIDPrefixes))
	for _, lit := range agentIDPrefixes {
		// q is the quote-anchored prefix: an opening quote immediately
		// followed by the prefix, and (where the literal continues)
		// non-quote content until the closing quote. bare is the prefix
		// without its trailing underscore, for regex-alternation groups.
		q := `["` + "`" + `]` + regexp.QuoteMeta(lit)
		bare := regexp.QuoteMeta(strings.TrimSuffix(lit, "_"))
		idx = append(idx, idPrefixPattern{
			prefix: lit,
			match: regexp.MustCompile(
				// equality / inequality: == "que_…" / "que_…" !=
				`(?:==|!=)\s*` + q +
					`|` + q + `[^"` + "`" + `]*"` + `\s*(?:==|!=)` +
					// case clauses (incl. comma lists): case "que_…":
					`|case\s+(?:[a-zA-Z0-9_."]+,\s*)*` + q + `[^"` + "`" + `]*"` + `\s*[:,]` +
					// map-key lookups: m["que_…"]
					`|\[\s*` + q + `[^"` + "`" + `]*"` + `\s*\]` +
					// match family needles: strings.Contains(x, "que_…")
					`|(?:Contains|HasPrefix|HasSuffix|TrimPrefix|TrimSuffix)\([^,)]+\s*,\s*` + q +
					// concatenation minting: "msg_" + x
					`|` + q + `[^"` + "`" + `]*"` + `\s*\+` +
					// declaration/assignment minting: const quePrefix = "que_"
					// (laundering the literal through an identifier is
					// still dispatch knowledge)
					`|=\s*` + q +
					// assignment laundering of a regex alternation:
					// const pat = "^(que|per)_"
					`|=\s*["` + "`" + `][^"` + "`" + `]*[(|:]` + bare + `[|)]` +
					// regex-pattern dispatch: regexp.MustCompile("^que_…")
					`|(?:MustCompile|Compile)\(\s*["` + "`" + `](?:\^|\(|\|)?` + regexp.QuoteMeta(lit) +
					// regex alternation groups: (que|per)_ / (?:que|msg)_
					`|(?:MustCompile|Compile)\([^)]*[(|:]` + bare + `[|)]`,
			),
		})
	}
	return idx
}

// isCommentLine reports whether a trimmed line is prose only: a
// full-line //, block-comment opener /*, or block-comment continuation
// starting with *.
func isCommentLine(trimmed string) bool {
	return strings.HasPrefix(trimmed, "//") ||
		strings.HasPrefix(trimmed, "/*") ||
		strings.HasPrefix(trimmed, "*")
}

// stripTrailingComment cuts a line at the earliest // or /* so trailing
// prose ("i := 0 // old code did id == \"ses_abc\"") is not matched.
// The cut is textual: a // inside a string literal (URLs) also cuts,
// which can only suppress matches on the remainder (a rare miss,
// never a false positive).
func stripTrailingComment(line string) string {
	if i := strings.Index(line, "//"); i >= 0 {
		line = line[:i]
	}
	if i := strings.Index(line, "/*"); i >= 0 {
		line = line[:i]
	}
	return line
}

// validateKnownLeaksEntries enforces the knownLeaks contract shared by
// the literal rules: every entry must carry a non-empty reason that
// includes an issue pointer (#NNNN). Entries without one are the moral
// equivalent of `// TODO: fix later` and are rejected at test time.
func validateKnownLeaksEntries(name string, m map[string]string) error {
	issueRe := regexp.MustCompile(`#\d+`)
	for k, v := range m {
		if strings.TrimSpace(v) == "" {
			return fmt.Errorf("%s[%q]: entry must carry a reason", name, k)
		}
		if !issueRe.MatchString(v) {
			return fmt.Errorf("%s[%q]: reason must include an issue pointer (#NNNN), got %q", name, k, v)
		}
	}
	return nil
}
