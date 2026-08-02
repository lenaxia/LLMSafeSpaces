// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package repolint

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// GinSetModeReport lists *_test.go files that call gin.SetMode from a
// test body that may run in parallel.
//
// gin.SetMode writes a package-global variable in the gin library. When a
// test calls it inside a function invoked from a t.Parallel() test body,
// concurrent goroutines write the same global and the race detector trips
// under `go test -race`. The safe pattern is to set the mode exactly once
// from a package-level `init()` or `TestMain`, which complete before any
// parallel test goroutine is released by the Go test runner.
//
// Originating incident: 2026-08-02 — Image Factory S4+S5 (PR #619) merged
// with a data race on gin's mode global; main CI was red for hours and it
// blocked the v0.7.1 release gate. See worklog 0663.
type GinSetModeReport struct {
	// Violations lists the relative paths of offending files, sorted.
	Violations []string
}

// OK reports whether no violations were found.
func (r GinSetModeReport) OK() bool {
	return len(r.Violations) == 0
}

// String returns a human-readable description.
func (r GinSetModeReport) String() string {
	if r.OK() {
		return "(ok)"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "  %d file(s) call gin.SetMode from a parallel test body:\n", len(r.Violations))
	for _, f := range r.Violations {
		fmt.Fprintf(&b, "    - %s\n", f)
	}
	b.WriteString("  Fix: move gin.SetMode(gin.TestMode) into a package-level\n" +
		"  func init() { ... } or TestMain. Set it once; never from a\n" +
		"  t.Parallel() test body or a helper it invokes.\n")
	return b.String()
}

// GinSetModeCheck scans every *_test.go file under root and flags files
// that call gin.SetMode from code that can execute in a parallel test
// goroutine. Files that only call it inside init() or TestMain pass.
//
// root must be the repository root (the scan uses filepath.WalkDir, so
// subdirectories are included automatically).
func GinSetModeCheck(root string) (GinSetModeReport, error) {
	var violations []string

	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		// Only lint Go test files.
		if filepath.Ext(path) != ".go" || !strings.HasSuffix(path, "_test.go") {
			return nil
		}
		// Skip vendored or generated code.
		if isExcludedPath(root, path) {
			return nil
		}

		viol, err := ginSetModeViolation(path)
		if err != nil {
			// Parse errors on test files are real problems, but they
			// are surfaced by `go test`/`go vet` themselves; failing
			// the lint on a syntax error would be noise. Skip.
			return nil
		}
		if viol {
			rel, rerr := filepath.Rel(root, path)
			if rerr != nil {
				rel = path
			}
			violations = append(violations, rel)
		}
		return nil
	})
	if err != nil {
		return GinSetModeReport{}, err
	}

	sort.Strings(violations)
	return GinSetModeReport{Violations: violations}, nil
}

// isExcludedPath reports whether path is under a vendored/generated
// directory that the check should not lint.
func isExcludedPath(root, path string) bool {
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return false
	}
	for _, part := range strings.Split(rel, string(filepath.Separator)) {
		switch part {
		case "vendor", ".git", "node_modules", "third_party":
			return true
		}
	}
	return false
}

// ginSetModeViolation parses the test file at path and reports whether it
// contains a gin.SetMode call that is NOT inside an init() or TestMain.
func ginSetModeViolation(path string) (bool, error) {
	src, err := os.ReadFile(path)
	if err != nil {
		return false, err
	}
	// Fast path: no gin.SetMode at all, or no t.Parallel — a file without
	// either cannot exhibit this race (serial-only calls are safe).
	if !strings.Contains(string(src), "gin.SetMode") {
		return false, nil
	}
	if !strings.Contains(string(src), "t.Parallel") {
		return false, nil
	}

	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, path, src, parser.AllErrors)
	if err != nil {
		return false, err
	}

	// Map: function name → is it safe (init/TestMain)?
	unsafe := false
	ast.Inspect(f, func(n ast.Node) bool {
		if unsafe {
			return false
		}
		fn, ok := n.(*ast.FuncDecl)
		if !ok {
			return true
		}
		name := fn.Name.Name
		safeFn := name == "init" || name == "TestMain"
		if safeFn {
			return true
		}
		// Walk this function's body looking for gin.SetMode.
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			if sel.Sel.Name == "SetMode" {
				unsafe = true
				return false
			}
			return true
		})
		return !unsafe
	})
	return unsafe, nil
}
