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

// SpecPath is the client-facing OpenAPI spec this rule lints.
const SpecPath = "sdks/openapi.yaml"

// Violation kinds for SpecCouplingMarkerCheck.
const (
	// SpecViolationMarkerKey: an x-opencode-proxy extension key — the
	// documented admission that an operation/schema drifts with the
	// agent. All were deleted by #1304; reintroduction fails here.
	SpecViolationMarkerKey = "x-opencode-proxy key"
	// SpecViolationCouplingPhrase: a description admitting client-facing
	// schemas track the agent ("tracks upstream", "from opencode",
	// "opencode session object").
	SpecViolationCouplingPhrase = "coupling-phrase description"
)

// specCouplingPhrases are the description phrases that admit the spec
// tracks the agent's shapes. Matched case-insensitively on word
// boundaries inside description values (block-scalar continuation
// lines included) EXCEPT under the top-level info: block — the spec's
// platform-overview description legitimately mentions opencode, and
// that one anchored site is allowlisted, nothing else (#1305).
var specCouplingPhrases = []string{
	`tracks upstream`,
	`from opencode`,
	`opencode session object`,
}

// specMarkerKeyRe matches an x-opencode-proxy KEY (optionally quoted)
// anywhere in the spec — the info: allowlist does not cover it.
var specMarkerKeyRe = regexp.MustCompile(`^\s*"?x-opencode-proxy"?\s*:`)

// specDescRe matches a description mapping key and its inline value.
var specDescRe = regexp.MustCompile(`^(\s*)description:\s*(.*)$`)

// specCouplingKnownLeaks tolerates existing marker/phrase lines until
// their cleanup lands. Keyed by the trimmed violating LINE (stable
// across line-number churn); each entry must carry a reason and issue
// pointer (validateKnownLeaksEntries). Empty at birth (#1305) — #1304
// deleted every marker and truth-upped every description.
var specCouplingKnownLeaks = map[string]string{}

// SpecCouplingViolation is one coupling marker found in the spec.
type SpecCouplingViolation struct {
	Line     int
	Kind     string // SpecViolationMarkerKey | SpecViolationCouplingPhrase
	Match    string // the matched phrase, or the key for marker lines
	Excerpt  string // trimmed violating line (the known-leaks key)
	IsLeaked bool   // true when the trimmed line is a tolerated known leak
}

// SpecCouplingReport aggregates the check's findings.
type SpecCouplingReport struct {
	Violations []SpecCouplingViolation
}

// HasNew returns true when any violation is not a tolerated leak.
func (r SpecCouplingReport) HasNew() bool {
	for _, v := range r.Violations {
		if !v.IsLeaked {
			return true
		}
	}
	return false
}

// SpecCouplingMarkerCheck lints sdks/openapi.yaml for spec-to-agent
// coupling markers: any x-opencode-proxy key, and any description
// matching the coupling phrases. The top-level info: block's
// description is the single anchored allowlist — it may mention
// opencode (platform overview); every other description is held to the
// phrase bans. A missing spec is an error, never a silent pass.
// Line-based by design: markers and phrases are line-local, and
// description block scalars are tracked by indentation relative to
// their description: key (blank lines inside a scalar are neutral).
func SpecCouplingMarkerCheck(root string) (SpecCouplingReport, error) {
	path := filepath.Join(root, filepath.FromSlash(SpecPath))
	data, err := os.ReadFile(path) //nolint:gosec // G122: repo-rooted lint scan
	if err != nil {
		return SpecCouplingReport{}, fmt.Errorf("spec coupling marker: %w", err)
	}

	phraseRes := make([]*regexp.Regexp, len(specCouplingPhrases))
	for i, p := range specCouplingPhrases {
		phraseRes[i] = regexp.MustCompile(`(?i)\b` + p + `\b`)
	}

	var report SpecCouplingReport
	inInfo := false
	descIndent := -1 // indentation of the open description: key; -1 = none
	for i, line := range strings.Split(string(data), "\n") {
		trimmedLine := strings.TrimSpace(line)
		if trimmedLine == "" {
			continue // blank lines close nothing and carry no content
		}
		indent := len(line) - len(strings.TrimLeft(line, " \t"))

		if indent == 0 {
			inInfo = trimmedLine == "info:"
			descIndent = -1 // any non-blank top-level line closes open blocks
		}

		// Continuation of a description block scalar.
		if descIndent >= 0 && indent > descIndent {
			if !inInfo {
				checkPhrases(&report, phraseRes, i+1, trimmedLine)
			}
			continue
		}
		descIndent = -1

		if m := specDescRe.FindStringSubmatch(line); m != nil {
			value := strings.TrimSpace(m[2])
			if strings.HasPrefix(value, "|") || strings.HasPrefix(value, ">") {
				// Block scalar: content starts on the following, more-
				// indented lines.
				descIndent = indent
			} else if !inInfo {
				checkPhrases(&report, phraseRes, i+1, trimmedLine)
			}
			continue
		}

		if specMarkerKeyRe.MatchString(line) {
			report.Violations = append(report.Violations, SpecCouplingViolation{
				Line: i + 1, Kind: SpecViolationMarkerKey, Match: "x-opencode-proxy",
				Excerpt: trimmedLine, IsLeaked: isSpecKnownLeak(trimmedLine),
			})
		}
	}
	sort.Slice(report.Violations, func(i, j int) bool {
		return report.Violations[i].Line < report.Violations[j].Line
	})
	return report, nil
}

// checkPhrases records a violation for every coupling phrase present in
// a description value.
func checkPhrases(report *SpecCouplingReport, phraseRes []*regexp.Regexp, line int, trimmedLine string) {
	for i, re := range phraseRes {
		if !re.MatchString(trimmedLine) {
			continue
		}
		report.Violations = append(report.Violations, SpecCouplingViolation{
			Line: line, Kind: SpecViolationCouplingPhrase, Match: specCouplingPhrases[i],
			Excerpt: trimmedLine, IsLeaked: isSpecKnownLeak(trimmedLine),
		})
	}
}

func isSpecKnownLeak(trimmedLine string) bool {
	_, ok := specCouplingKnownLeaks[trimmedLine]
	return ok
}
