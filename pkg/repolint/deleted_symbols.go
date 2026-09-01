// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package repolint

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// DeletedSymbolsReport lists resurrected references to symbols deleted
// by the US-70.5 secret-delivery demolition (issue #1209).
type DeletedSymbolsReport struct {
	Violations []string
}

func (r DeletedSymbolsReport) OK() bool { return len(r.Violations) == 0 }

func (r DeletedSymbolsReport) String() string {
	if r.OK() {
		return "ok: no references to US-70.5-deleted symbols"
	}
	var b strings.Builder
	b.WriteString("references to US-70.5-deleted symbols present (demolition gate, issue #1209; the replacements are BuildWorkspaceBatch/GetCachedDEKForUser/applySecretsBatch/resync-secrets):\n")
	for _, v := range r.Violations {
		fmt.Fprintf(&b, "  - %s\n", v)
	}
	b.WriteString("historical worklogs/ and design/ are exempt. If a symbol is genuinely needed again, remove it from deletedSymbols in deleted_symbols.go in the same change.\n")
	return b.String()
}

// deletedSymbols are the exact identifiers and artifacts the demolition
// removed. Anchored to specific names: the literal string
// "reload-secrets" legitimately survives for the API-side manual-resync
// route and the resync naming, so only the deleted pod-side identifiers
// are pinned.
var deletedSymbols = []string{
	"InjectSecrets",
	"InjectSessionlessSecrets",
	"InjectSecretsForPodBootstrap",
	"rehydrateDEKFromJWTSession",
	"GetDEKForUser",
	"secretautopush",
	"UserCredsPresent",
	"last-reload-secrets",
	"writeReloadSecretsCache",
	"loadReloadSecretsCache",
	"reloadSecretsHandler",
	"pushInitialSpawnEnv",
}

// deletedSymbolsExemptDirs are historical-record trees: worklog and
// design prose name deleted symbols by design and never compile.
var deletedSymbolsExemptDirs = []string{
	"worklogs",
	"design",
}

// deletedSymbolsExemptSuffixes are the pin list itself — this check
// and its test necessarily name every deleted symbol exactly once, in
// the one audited place that defines the gate. Suffix-matched so the
// check works from any walk root.
var deletedSymbolsExemptSuffixes = []string{
	"repolint/deleted_symbols.go",
	"repolint/deleted_symbols_test.go",
}

// DeletedSymbolsCheck fails when any non-historical .go file references
// a deleted symbol. This is the mechanical guard against the
// stale-branch-merge resurrection mode (the c9c68684 precedent the
// ForbiddenPaths check was built for) applied to identifiers instead of
// paths.
func DeletedSymbolsCheck(root string) (DeletedSymbolsReport, error) {
	var violations []string
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			name := d.Name()
			if name == ".git" || name == "vendor" || name == "node_modules" {
				return filepath.SkipDir
			}
			for _, ex := range deletedSymbolsExemptDirs {
				if name == ex {
					return filepath.SkipDir
				}
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") {
			return nil
		}
		data, rErr := os.ReadFile(path) //nolint:gosec // G122: repo-rooted lint scan (same trust domain as event_literal.go's read)
		if rErr != nil {
			return rErr
		}
		rel, rErr := filepath.Rel(root, path)
		if rErr != nil {
			return rErr
		}
		rel = filepath.ToSlash(rel)
		for _, ex := range deletedSymbolsExemptSuffixes {
			if strings.HasSuffix(rel, ex) {
				return nil
			}
		}
		for _, sym := range deletedSymbols {
			if strings.Contains(string(data), sym) {
				violations = append(violations, fmt.Sprintf("%s: %s", rel, sym))
			}
		}
		return nil
	})
	if err != nil {
		return DeletedSymbolsReport{}, fmt.Errorf("walk %s: %w", root, err)
	}
	return DeletedSymbolsReport{Violations: violations}, nil
}
