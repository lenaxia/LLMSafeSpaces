// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package handlers

// board_guard_codes_parity_test.go — pins the frontend's golden error-
// code fixture (frontend/src/api/boardErrorCodes.json) against a source
// scan of pkg/board's GuardError/DependencyError constructions (#1702 r3:
// the r2 mirror-parity test pinned the mirror against itself, so it could
// not detect that the live "validation" code was missing — this closes
// the drift class: neither the Go side nor the frontend mirror can drift
// alone).
//
// The scan is the board_parity_test.go source-scan discipline: every
// GuardError{...} construction site in NON-TEST pkg/board sources has its
// Code expression resolved (bare const identifiers through the board
// package's exported constants; string literals verbatim), the
// never-returned GuardDependency const drops out by construction (it has
// no construction site), and DependencyError's literal codes join the
// set. The derived set must equal the fixture exactly.

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"testing"

	"github.com/lenaxia/llmsafespaces/pkg/board"
)

// guardConstValues maps pkg/board's Guard* constant IDENTIFIERS (as they
// appear in construction sites) to their exported values. A new constant
// used in a construction but missing here fails the lookup — the test
// reddens until this map (and the fixture) grow.
var guardConstValues = map[string]string{
	"GuardStage":            board.GuardStage,
	"GuardDone":             board.GuardDone,
	"GuardClaimToken":       board.GuardClaimToken,
	"GuardManagerSurface":   board.GuardManagerSurface,
	"GuardWorkspaceBinding": board.GuardWorkspaceBinding,
	"GuardEpicState":        board.GuardEpicState,
	"GuardDependency":       board.GuardDependency,
	"GuardProjectState":     board.GuardProjectState,
	"GuardAuthz":            board.GuardAuthz,
}

// codeExpr matches the Code field of a GuardError{...} or
// DependencyError{...} composite literal on one line.
var codeExpr = regexp.MustCompile(`(?:GuardError|DependencyError)\{[^}]*?Code:\s*([A-Za-z0-9_]+|"[^"]*")`)

func boardSourceFiles(t *testing.T) []string {
	t.Helper()
	matches, err := filepath.Glob(filepath.Join("..", "..", "..", "pkg", "board", "*.go"))
	if err != nil {
		t.Fatalf("glob pkg/board: %v", err)
	}
	var out []string
	for _, m := range matches {
		if filepath.Ext(m) == ".go" && len(m) > 8 && m[len(m)-8:] != "_test.go" {
			out = append(out, m)
		}
	}
	if len(out) == 0 {
		t.Fatal("no pkg/board sources found — scan is broken, not the fixture")
	}
	return out
}

// scanEmittedBoardErrorCodes derives the set of error codes pkg/board can
// place on GuardError/DependencyError constructions (non-test sources).
func scanEmittedBoardErrorCodes(t *testing.T) map[string]bool {
	t.Helper()
	emitted := map[string]bool{}
	for _, f := range boardSourceFiles(t) {
		src, err := os.ReadFile(f)
		if err != nil {
			t.Fatalf("read %s: %v", f, err)
		}
		for _, m := range codeExpr.FindAllStringSubmatch(string(src), -1) {
			expr := m[1]
			if expr[0] == '"' {
				emitted[expr[1:len(expr)-1]] = true
				continue
			}
			val, ok := guardConstValues[expr]
			if !ok {
				t.Fatalf("GuardError construction in %s uses unknown constant %q — extend guardConstValues and the fixture", f, expr)
			}
			emitted[val] = true
		}
	}
	return emitted
}

func TestBoardGuardCodesParity_WithFrontendGolden(t *testing.T) {
	emitted := scanEmittedBoardErrorCodes(t)

	fixturePath := filepath.Join("..", "..", "..", "frontend", "src", "api", "boardErrorCodes.json")
	raw, err := os.ReadFile(fixturePath)
	if err != nil {
		t.Fatalf("read frontend golden fixture: %v", err)
	}
	var golden []string
	if err := json.Unmarshal(raw, &golden); err != nil {
		t.Fatalf("parse frontend golden fixture: %v", err)
	}

	goldenSet := map[string]bool{}
	for _, c := range golden {
		if goldenSet[c] {
			t.Errorf("fixture lists %q twice", c)
		}
		goldenSet[c] = true
	}

	for c := range goldenSet {
		if !emitted[c] {
			t.Errorf("fixture code %q is NOT emitted by pkg/board (dead key) — remove it or the scan is stale", c)
		}
	}
	for c := range emitted {
		if !goldenSet[c] {
			t.Errorf("pkg/board emits code %q but the frontend fixture misses it (frontend/src/api/boardErrorCodes.json + BOARD_GUARD_MESSAGES hint)", c)
		}
	}

	if len(golden) == 0 {
		t.Fatal("empty fixture — the parity contract cannot be empty")
	}
	sort.Strings(golden)
	t.Logf("board error-code contract (%d codes): %v", len(golden), golden)
}
