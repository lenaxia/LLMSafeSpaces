// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package repolint

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

// fluxFixture builds a minimal repo tree with one chart CRD and (unless
// omitted) its mirror + kustomization. Returns the root.
func fluxFixture(t *testing.T, mirror bool, kustomization string) string {
	t.Helper()
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, FluxCRDChartDir), 0o755); err != nil {
		t.Fatal(err)
	}
	crd := "apiVersion: apiextensions.k8s.io/v1\nkind: CustomResourceDefinition\nmetadata:\n  name: workspaces.llmsafespaces.dev\n"
	if err := os.WriteFile(filepath.Join(root, FluxCRDChartDir, "workspace.yaml"), []byte(crd), 0o644); err != nil {
		t.Fatal(err)
	}
	if mirror {
		if err := os.MkdirAll(filepath.Join(root, FluxCRDMirrorDir), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, FluxCRDMirrorDir, "workspace.yaml"), []byte(crd), 0o644); err != nil {
			t.Fatal(err)
		}
		if kustomization != "" {
			if err := os.WriteFile(filepath.Join(root, FluxCRDMirrorDir, "kustomization.yaml"), []byte(kustomization), 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}
	return root
}

const fluxOKKustomization = "apiVersion: kustomize.config.k8s.io/v1beta1\nkind: Kustomization\nresources:\n  - workspace.yaml\n"

func TestFluxCRDDrift_NoDrift_ReturnsOK(t *testing.T) {
	root := fluxFixture(t, true, fluxOKKustomization)
	rep, err := FluxCRDDriftCheck(root)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !rep.OK() {
		t.Fatalf("expected OK, got failures:\n%s", rep.String())
	}
}

// The born-red pin for the guard itself: a chart CRD schema change
// without the mirror bump FAILS — the exact PR shape that wedged
// production on 2026-11-17 (issue #2092).
func TestFluxCRDDrift_UnbumpedChartChange_Fails(t *testing.T) {
	root := fluxFixture(t, true, fluxOKKustomization)
	// The PR edits the chart CRD (schema change) and does NOT touch the
	// mirror.
	path := filepath.Join(root, FluxCRDChartDir, "workspace.yaml")
	if err := os.WriteFile(path, []byte("apiVersion: apiextensions.k8s.io/v1\nkind: CustomResourceDefinition\nmetadata:\n  name: workspaces.llmsafespaces.dev\n# schema v2\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	rep, err := FluxCRDDriftCheck(root)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if rep.OK() {
		t.Fatal("expected drift failure for an unbumped chart CRD change")
	}
	if len(rep.Drifted) != 1 || rep.Drifted[0] != "workspace.yaml" {
		t.Fatalf("expected workspace.yaml in Drifted, got %v", rep.Drifted)
	}
}

func TestFluxCRDDrift_MissingMirror_Fails(t *testing.T) {
	root := fluxFixture(t, false, "")
	rep, err := FluxCRDDriftCheck(root)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if rep.OK() {
		t.Fatal("expected failure for a CRD with no mirror")
	}
	if len(rep.MissingInMirror) != 1 || rep.MissingInMirror[0] != "workspace.yaml" {
		t.Fatalf("expected workspace.yaml in MissingInMirror, got %v", rep.MissingInMirror)
	}
}

// An unlisted mirror file is an unapplied CRD — the same wedge one level
// up: Flux reconciles only what kustomization references.
func TestFluxCRDDrift_UnlistedInKustomization_Fails(t *testing.T) {
	root := fluxFixture(t, true, "apiVersion: kustomize.config.k8s.io/v1beta1\nkind: Kustomization\nresources: []\n")
	rep, err := FluxCRDDriftCheck(root)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if rep.OK() {
		t.Fatal("expected failure for an unlisted mirror CRD")
	}
	if len(rep.UnlistedInKustomization) != 1 || rep.UnlistedInKustomization[0] != "workspace.yaml" {
		t.Fatalf("expected workspace.yaml in UnlistedInKustomization, got %v", rep.UnlistedInKustomization)
	}
}

// A stale kustomization entry breaks Flux reconciliation of the whole
// path — it must fail here, not in production.
func TestFluxCRDDrift_StaleKustomizationEntry_Fails(t *testing.T) {
	root := fluxFixture(t, true, "apiVersion: kustomize.config.k8s.io/v1beta1\nkind: Kustomization\nresources:\n  - workspace.yaml\n  - deleted-crd.yaml\n")
	rep, err := FluxCRDDriftCheck(root)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if rep.OK() {
		t.Fatal("expected failure for a stale kustomization entry")
	}
	if len(rep.StaleKustomizationEntries) != 1 || rep.StaleKustomizationEntries[0] != "deleted-crd.yaml" {
		t.Fatalf("expected deleted-crd.yaml in StaleKustomizationEntries, got %v", rep.StaleKustomizationEntries)
	}
}

func TestFluxCRDDrift_MissingKustomization_Fails(t *testing.T) {
	root := fluxFixture(t, true, "")
	rep, err := FluxCRDDriftCheck(root)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if rep.OK() {
		t.Fatal("expected failure for a missing kustomization")
	}
	if !rep.KustomizationMissing {
		t.Fatal("expected KustomizationMissing")
	}
}

// SyncFluxCRDs is the healer: after it runs, the check passes (the fix
// side and the gate side cannot drift apart).
func TestSyncFluxCRDs_ResolvesDrift(t *testing.T) {
	root := fluxFixture(t, false, "")
	if err := SyncFluxCRDs(root); err != nil {
		t.Fatalf("sync: %v", err)
	}
	rep, err := FluxCRDDriftCheck(root)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !rep.OK() {
		t.Fatalf("expected OK after sync, got:\n%s", rep.String())
	}
	// The regenerated kustomization carries the discipline header.
	b, err := os.ReadFile(filepath.Join(root, FluxCRDMirrorDir, "kustomization.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(b, []byte("release-train CRD guard")) {
		t.Fatalf("regenerated kustomization lost the discipline header:\n%s", b)
	}
}

// SyncFluxCRDs must also remove a mirror file whose chart CRD was
// deleted (a stale mirror keeps applying a dead CRD via Flux).
func TestSyncFluxCRDs_RemovesStaleMirrorFiles(t *testing.T) {
	root := fluxFixture(t, true, "apiVersion: kustomize.config.k8s.io/v1beta1\nkind: Kustomization\nresources:\n  - workspace.yaml\n  - old.yaml\n")
	stale := filepath.Join(root, FluxCRDMirrorDir, "old.yaml")
	if err := os.WriteFile(stale, []byte("stale"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := SyncFluxCRDs(root); err != nil {
		t.Fatalf("sync: %v", err)
	}
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Fatal("expected stale mirror file removed")
	}
	rep, err := FluxCRDDriftCheck(root)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !rep.OK() {
		t.Fatalf("expected OK after sync, got:\n%s", rep.String())
	}
}

// TestLive_FluxCRD_NoDrift is the live-tree pin: the committed Flux
// path mirror matches the chart CRDs exactly. This is the check CI runs
// on every PR (the release-train CRD guard, issue #2092 ruling 1).
func TestLive_FluxCRD_NoDrift(t *testing.T) {
	root, err := filepath.Abs("..")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "go.mod")); err != nil {
		t.Skip("not running from the repo tree")
	}
	rep, err := FluxCRDDriftCheck(root)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !rep.OK() {
		t.Fatalf("Flux CRD path drift:\n%s", rep.String())
	}
}

// TestLive_FluxCRD_MirrorCoversAllChartCRDs — the guard covers ALL
// platform CRDs: every yaml under helm/crds/ is mirrored (the coverage
// half of the check; a future CRD added without a mirror fails here).
func TestLive_FluxCRD_MirrorCoversAllChartCRDs(t *testing.T) {
	root, err := filepath.Abs("..")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "go.mod")); err != nil {
		t.Skip("not running from the repo tree")
	}
	rep, err := FluxCRDDriftCheck(root)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := []string{"inferencerelay.yaml", "runtimeenvironment.yaml", "workspace.yaml"}
	if len(rep.ChartCRDs) != len(want) {
		t.Fatalf("expected %d chart CRDs, got %v — update this pin AND check the guard still covers the new CRD", len(want), rep.ChartCRDs)
	}
	for i, n := range want {
		if rep.ChartCRDs[i] != n {
			t.Fatalf("chart CRD set changed: got %v want %v — update this pin", rep.ChartCRDs, want)
		}
	}
}
