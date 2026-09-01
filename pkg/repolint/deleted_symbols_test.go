// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package repolint

import (
	"os"
	"path/filepath"
	"testing"
)

// TestDeletedSymbolsCheck_CleanTree: no .go file references a deleted
// symbol → OK.
func TestDeletedSymbolsCheck_CleanTree(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "pkg", "x"), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "pkg", "x", "a.go"), []byte("package x\n// GetDEKServerSide is fine\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	rep, err := DeletedSymbolsCheck(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !rep.OK() {
		t.Fatalf("expected clean tree, got:\n%s", rep.String())
	}
}

// TestDeletedSymbolsCheck_DetectsResurrection: a .go file naming a
// deleted symbol is reported; worklogs/ and design/ are exempt.
func TestDeletedSymbolsCheck_DetectsResurrection(t *testing.T) {
	dir := t.TempDir()
	for _, sub := range []string{"api", "worklogs", "design"} {
		if err := os.MkdirAll(filepath.Join(dir, sub), 0o750); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "api", "a.go"), []byte("package api\n// reloadSecretsHandler came back\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "worklogs", "w.go"), []byte("package worklogs\n// GetDEKForUser history\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "design", "d.go"), []byte("package design\n// secretautopush history\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	rep, err := DeletedSymbolsCheck(dir)
	if err != nil {
		t.Fatal(err)
	}
	if rep.OK() {
		t.Fatal("expected the api/ resurrection to be reported")
	}
	if len(rep.Violations) != 1 || rep.Violations[0] != filepath.Join("api", "a.go")+": reloadSecretsHandler" {
		t.Fatalf("expected exactly the api violation, got %v", rep.Violations)
	}
}

// TestDeletedSymbolsCheck_RepoTree is the live pin: the repository
// itself must stay grep-clean (the #1209 demolition gate).
func TestDeletedSymbolsCheck_RepoTree(t *testing.T) {
	root, err := filepath.Abs("..")
	if err != nil {
		t.Fatal(err)
	}
	rep, err := DeletedSymbolsCheck(root)
	if err != nil {
		t.Fatal(err)
	}
	if !rep.OK() {
		t.Fatalf("US-70.5 demolition gate violated:\n%s", rep.String())
	}
}
