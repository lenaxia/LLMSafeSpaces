// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package repolint

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// ForbiddenPathsReport lists resurrected legacy paths.
type ForbiddenPathsReport struct {
	Violations []string
}

func (r ForbiddenPathsReport) OK() bool { return len(r.Violations) == 0 }

func (r ForbiddenPathsReport) String() string {
	if r.OK() {
		return "ok: no forbidden legacy paths"
	}
	var b strings.Builder
	b.WriteString("forbidden legacy paths present (deleted by Epic 7 / US-7.8 and #854; see also c9c68684 for how a stale-branch merge resurrected them once already):\n")
	for _, v := range r.Violations {
		fmt.Fprintf(&b, "  - %s\n", v)
	}
	b.WriteString("the per-language runtime images were never built by any pipeline (only runtimes/base is); tenant toolchains ship via mise in the base image. If a per-language image is genuinely needed again, remove the path from ForbiddenPaths in forbidden_paths.go in the same change.\n")
	return b.String()
}

// forbiddenPaths are repository paths whose presence indicates a
// resurrection of the deleted legacy per-language runtime images
// (#854 investigation, 2026-08-14: deleted by 1166b86d, resurrected
// accidentally by c9c68684 hours later, deleted again; nothing builds
// them, so their pins rot invisibly — Go 1.20.5 sat EOL in runtimes/go
// for months).
var forbiddenPaths = []string{
	"runtimes/go",
	"runtimes/nodejs",
	"runtimes/python",
	"runtimes/tests",
}

// ForbiddenPathsCheck fails when any legacy runtime path exists. This is
// the mechanical guard against the c9c68684 failure mode: a stale branch
// re-adding "new file mode" copies of deleted trees in a merge that
// reviewers read as an unrelated fix.
func ForbiddenPathsCheck(root string) (ForbiddenPathsReport, error) {
	var violations []string
	for _, p := range forbiddenPaths {
		full := filepath.Join(root, p)
		if _, err := os.Lstat(full); err == nil {
			violations = append(violations, p)
		} else if !errorsIsNotExist(err) {
			return ForbiddenPathsReport{}, fmt.Errorf("stat %s: %w", full, err)
		}
	}
	return ForbiddenPathsReport{Violations: violations}, nil
}

func errorsIsNotExist(err error) bool { return os.IsNotExist(err) }
