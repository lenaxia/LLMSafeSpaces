// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package migrations_test

// migration_set_unit_test.go — the duplicate-version class pin (PR #2071
// review round 1): two migrations sharing a version number (a branch cut
// before main took its number, merged without renumbering) is INVISIBLE
// to CI — CI runs the PR branch, which lacks main's file — and dead on
// arrival on the merged tree: golang-migrate's iofs source refuses
// duplicate versions ("duplicate migration file"), breaking every
// migration consumer (the PG test harness, both populated-migration
// tests, the migrate CLI). This pin reddens at PR time: the embedded FS
// must open as a migration source with NO error, which is exactly what
// duplicates would break. Unit-speed, no DB.

import (
	"testing"

	"github.com/golang-migrate/migrate/v4/source/iofs"

	"github.com/lenaxia/llmsafespaces/api/migrations"
)

// TestMigrationSetNoDuplicateVersions — the embedded migration set must
// open as a golang-migrate source: duplicate version numbers (the
// 000055 collision class from #2071 review round 1) refuse here first,
// at PR time, instead of at first boot on the merged tree. The iofs
// source's Migrations registry refuses duplicate versions at Open —
// this pin rides exactly that refusal; First() additionally proves the
// set is non-empty and ordered.
func TestMigrationSetNoDuplicateVersions(t *testing.T) {
	src, err := iofs.New(migrations.FS, ".")
	if err != nil {
		t.Fatalf("the embedded migration set has a duplicate/garbled version (the #2071 review round 1 class — renumber to the next free version and mirror to helm/migrations): %v", err)
	}
	defer func() { _ = src.Close() }()
	first, err := src.First()
	if err != nil {
		t.Fatalf("the embedded migration set has no first version: %v", err)
	}
	if first != 1 {
		t.Fatalf("first migration version = %d, want 1", first)
	}
}
