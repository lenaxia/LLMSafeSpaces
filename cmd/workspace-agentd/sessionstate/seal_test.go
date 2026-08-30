// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package sessionstate_test

import (
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestModuleSealDependencies enforces the design-0055 placement seal:
// sessionstate is a module-sealed subsystem — it must not import agentd
// supervision internals (package main or any cmd/... package), and the only
// platform packages it may touch are the ABI schema, shared constants, and
// the opencode-agnostic logging/types seams. The supervision side imports
// sessionstate through its exported API only (verified by construction:
// package main can be imported by nothing).
func TestModuleSealDependencies(t *testing.T) {
	const root = "."
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	checked := 0
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") {
			continue
		}
		path := filepath.Join(root, e.Name())
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		checked++
		fset := token.NewFileSet()
		f, err := parser.ParseFile(fset, path, nil, parser.ImportsOnly)
		if err != nil {
			t.Fatalf("parse %s: %v", path, err)
		}
		for _, imp := range f.Imports {
			p := strings.Trim(imp.Path.Value, `"`)
			switch {
			case p == "github.com/lenaxia/llmsafespaces/cmd/workspace-agentd/sessionstate":
			case strings.HasPrefix(p, "github.com/lenaxia/llmsafespaces/cmd/"):
				t.Errorf("%s imports supervision package %q — module seal violated", path, p)
			case p == "github.com/lenaxia/llmsafespaces/pkg/agentd":
				// Shared constants package: allowed (port numbers, paths).
			default:
				// stdlib + third-party (connect, zap, protobuf) + the ABI
				// schema are allowed; anything else under llmsafespaces
				// must be reviewed here.
				if strings.HasPrefix(p, "github.com/lenaxia/llmsafespaces/") &&
					p != "github.com/lenaxia/llmsafespaces/pkg/abi/v1" &&
					p != "github.com/lenaxia/llmsafespaces/pkg/abi/v1/abiconnect" {
					t.Errorf("%s imports %q — outside the module's allowed seam (ABI schema + shared constants); extend this test only with design review", path, p)
				}
			}
		}
	}
	if checked == 0 {
		t.Fatal("no sessionstate sources found to seal-check")
	}
}
