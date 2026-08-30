// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package repolint

import (
	"fmt"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// AgentImportForbiddenPath is the agent implementation package that must
// stay behind the adapter seam. Platform code imports pkg/agent (interface)
// and pkg/session (contract types); only the construction/wiring layer and
// the in-pod agentd binary may touch the implementation. See Epic 65
// (design/0049 §4.6) and Rule 12 (Containment Before Abstraction).
const AgentImportForbiddenPath = "github.com/lenaxia/llmsafespaces/pkg/agent/opencode"

// agentSelfPrefix is the on-disk prefix of the seam itself; files inside
// it may import their own subpackages.
const agentSelfPrefix = "pkg/agent/opencode/"

// agentImportAllowedPrefixes lists the only directory prefixes permitted to
// import AgentImportForbiddenPath. These are the construction/wiring layer
// (api/internal/app builds the adapter; controller and workspace_service
// register the runtime) and the in-pod agentd binary (the config writer
// US-65.1 contained). Everything else must import pkg/agent + pkg/session.
var agentImportAllowedPrefixes = []string{
	"api/internal/app/",
	"cmd/workspace-agentd/",
	"cmd/controller/",
	"controller/cmd/", // alternate layout
	"controller/main.go",
}

// agentImportKnownLeaks records current violations of the boundary rule as
// explicit, dated tech debt with the story that retires each one. The rule
// fails on any NEW leak; entries here are tolerated only until their retiring
// story lands. Adding to this list without a worklog + story citation is the
// moral equivalent of `// TODO: fix later` and is rejected at review.
//
// Retirement plan:
//   - US-65.3 (opencode Adapter) introduces pkg/agent.Adapter and stops
//     callers needing the concrete *opencode.Client type or constructor.
//   - US-65.4 (proxy migration) rewrites the proxy handlers against Adapter.
//   - US-65.6-followup (sentinel centralization + boot wiring) moves the
//     ErrNoRunningPod canonical definition to pkg/agent (breaking the import
//     cycle that today forces the re-export to live in opencode/) and
//     replaces the init() Register() calls with explicit registration at
//     app.New / controller main boot.
//   - Once those land, this list must be empty; the rule then enforces the
//     design's full allow-set with no exceptions.
var agentImportKnownLeaks = map[string]string{
	// Epic 69 S1 shadow comparator (#1139): the reference fold deliberately
	// consumes the DIALECT via the wire subpackage — the comparator's value
	// is two independent derivations of one stream. Disposable with the
	// comparator at S1/S3 exit (design 0055); retire when the tracker
	// retirement deletes the package (#1145).
	"api/internal/services/shadowconsumer/comparator.go": "S1 shadow comparator consumes the dialect by design; retired with the comparator at Epic 69 S1/S3 exit (#1139, #1145)",
}

// KnownLeaks returns a defensive copy of the current agentImportKnownLeaks
// map. The success message in cmd/repolint uses len() on this to surface how
// much tech debt the boundary is tolerating, so the number is visible while
// it is non-zero and the rule visibly tightens when the last leak retires.
func KnownLeaks() map[string]string {
	out := make(map[string]string, len(agentImportKnownLeaks))
	for k, v := range agentImportKnownLeaks {
		out[k] = v
	}
	return out
}

// AgentImportReport lists files that import the agent implementation package
// from outside the allowed construction/wiring layer.
type AgentImportReport struct {
	Violations []AgentImportViolation
}

// AgentImportViolation describes one boundary break.
type AgentImportViolation struct {
	File   string // repository-relative path
	Reason string // why it failed (not in allow-set, or not in knownLeaks)
}

// OK reports whether no violations were found.
func (r AgentImportReport) OK() bool { return len(r.Violations) == 0 }

// String returns a human-readable description.
func (r AgentImportReport) String() string {
	if r.OK() {
		return "(ok)"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "  %d file(s) import %s from outside the allowed seam:\n",
		len(r.Violations), AgentImportForbiddenPath)
	for _, v := range r.Violations {
		fmt.Fprintf(&b, "    - %s (%s)\n", v.File, v.Reason)
	}
	b.WriteString("  Platform code must import pkg/agent (interface) and\n" +
		"  pkg/session (contract types). Only api/internal/app/ (construction)\n" +
		"  and cmd/workspace-agentd/ (config writer) may import the opencode\n" +
		"  implementation. See design/0049 §4.6 and Epic 65 US-65.6.\n")
	return b.String()
}

// AgentImportCheck scans every non-test .go file under root and flags any
// that import AgentImportForbiddenPath from outside agentImportAllowedPrefixes
// unless listed in agentImportKnownLeaks.
//
// Test files (_test.go) are excluded: tests of the opencode package itself
// and tests that exercise the concrete client against a fake server are
// legitimate. The boundary the rule protects is production code shape.
//
// root must be the repository root.
func AgentImportCheck(root string) (AgentImportReport, error) {
	var violations []AgentImportViolation

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
		if isExcludedPath(root, path) {
			return nil
		}
		rel, rerr := filepath.Rel(root, path)
		if rerr != nil {
			rel = path
		}
		norm := filepath.ToSlash(rel)
		// The seam itself may import its own subpackages.
		if strings.HasPrefix(norm, agentSelfPrefix) {
			return nil
		}
		imports, err := importsOf(path)
		if err != nil {
			return nil
		}
		// Prefix-match, not exact: subpackages of the implementation
		// (e.g. pkg/agent/opencode/wire) are the same boundary — an
		// exact match here let platform code import the wire subpackage
		// untouched (found in #947 review).
		if !importsForbiddenAgentPath(imports) {
			return nil
		}
		if anyPrefix(norm, agentImportAllowedPrefixes) {
			return nil
		}
		if reason, ok := agentImportKnownLeaks[norm]; ok {
			_ = reason
			return nil
		}
		violations = append(violations, AgentImportViolation{
			File:   norm,
			Reason: "new leak — not in allow-set, not in knownLeaks; see Epic 65 US-65.6",
		})
		return nil
	})
	if err != nil {
		return AgentImportReport{}, err
	}
	sort.Slice(violations, func(i, j int) bool {
		return violations[i].File < violations[j].File
	})
	return AgentImportReport{Violations: violations}, nil
}

// importsForbiddenAgentPath reports whether any import is the forbidden
// agent implementation package or a subpackage of it.
func importsForbiddenAgentPath(imports []string) bool {
	for _, imp := range imports {
		if imp == AgentImportForbiddenPath || strings.HasPrefix(imp, AgentImportForbiddenPath+"/") {
			return true
		}
	}
	return false
}

// importsOf parses path's import declarations. Returns the union of every
// import path declared in the file (singletons + groups, with or without
// aliases). Parse errors are returned; the caller treats them as "skip".
func importsOf(path string) ([]string, error) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, path, nil, parser.ImportsOnly)
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(f.Imports))
	for _, imp := range f.Imports {
		s := strings.Trim(imp.Path.Value, `"`)
		out = append(out, s)
	}
	return out, nil
}

func anyPrefix(s string, prefixes []string) bool {
	for _, p := range prefixes {
		if strings.HasPrefix(s, p) {
			return true
		}
	}
	return false
}
