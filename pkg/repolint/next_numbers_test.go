// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package repolint

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// NextNumbers pins: the soft layer's answer must skip numbers claimed
// by open PRs — the exact knowledge whose absence produced migration
// 41x2 (both PRs hand-numbered 000041 from the same main view) and the
// 1173/1175 worklog pairs.
func TestNextNumbers_SkipsOpenPRClaims(t *testing.T) {
	root := t.TempDir()
	wl := filepath.Join(root, "worklogs")
	mig := filepath.Join(root, "api", "migrations")
	if err := mkdirAll(wl, mig); err != nil {
		t.Fatal(err)
	}
	for i := 1178; i <= 1180; i++ {
		mustWrite(t, filepath.Join(wl, pad4(i)+"_2026-10-22_mainline.md"), "")
	}
	mustWrite(t, filepath.Join(mig, "000042_board_config.up.sql"), "")
	mustWrite(t, filepath.Join(mig, "000042_board_config.down.sql"), "")
	mustWrite(t, filepath.Join(mig, "000043_board_phrase_ids.up.sql"), "")
	mustWrite(t, filepath.Join(mig, "000043_board_phrase_ids.down.sql"), "")

	// No git repo, no claims: next numbers are max+1 per sequence.
	restoreClaimsSeam(t, func(string) *OpenClaims { return nil })
	rep := NextNumbers(root)
	if rep.WorklogNext != 1181 || rep.MigrationNext != 44 {
		t.Fatalf("unclaimed: want worklog 1181 / migration 44, got %d / %d", rep.WorklogNext, rep.MigrationNext)
	}

	// A sibling open PR claims worklog 1181 AND migration 44 — the
	// numbers our lane would otherwise take. Next must skip both.
	restoreClaimsSeam(t, func(string) *OpenClaims {
		return &OpenClaims{
			Worklogs:   map[int][]int{1181: {1841}},
			Migrations: map[int][]int{44: {1841}},
		}
	})
	rep = NextNumbers(root)
	if rep.WorklogNext != 1182 {
		t.Fatalf("worklog next must skip sibling-claimed 1181, got %d", rep.WorklogNext)
	}
	if rep.MigrationNext != 45 {
		t.Fatalf("migration next must skip sibling-claimed 44, got %d", rep.MigrationNext)
	}
	if !strings.Contains(rep.String(), "1181 (PR 1841)") {
		t.Errorf("report should name the claiming PRs, got:\n%s", rep.String())
	}
}

func mkdirAll(dirs ...string) error {
	for _, d := range dirs {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return err
		}
	}
	return nil
}
