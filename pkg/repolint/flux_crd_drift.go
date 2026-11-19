// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package repolint

// FluxCRDDriftCheck — the release-train CRD guard (issue #2092, ruling 1).
//
// helm NEVER upgrades the crds/ directory on `helm upgrade` (helm/README.md
// "CRD upgrades"), so on a Flux-managed cluster the ONLY thing that moves a
// CRD schema is the ops repo's `cluster-llmsafespaces-crds` kustomization.
// On 2026-11-17 that path was still serving the pre-0.44.15 Workspace CRD
// while the chart train had shipped spec.harness — every born-unbound
// workspace created since wedged in Creating forever (the apiserver pruned
// the bind write; the 0.44.16 skew fix made it loud but nothing converged,
// nothing alerted, and no gate tied the schema change to the CRD apply).
//
// This check makes the coupling repo-local and born-red: every chart CRD
// under helm/crds/ must have a byte-identical copy in the tracked Flux path
// mirror deploy/flux/crds/ — the copy the release checklist carries into
// the ops repo's kustomization. A schema change without the mirror bump
// fails `make repolint` (pre-commit + CI) in the SAME PR that made it,
// which is exactly the train gate: schema change = CRD apply, together.
//
// It also checks the kustomization itself: every mirrored CRD file must be
// listed in deploy/flux/crds/kustomization.yaml resources (an unlisted file
// is an unapplied CRD — the same wedge, one level up), and the
// kustomization must not reference files that do not exist (a stale entry
// breaks Flux reconciliation of the whole path).
//
// Companion checks (do not duplicate here): CRDDriftCheck keeps the Go
// structs ↔ chart CRD schema aligned; ClusterDriftCheck (opt-in
// -cluster-drift) compares the chart YAMLs against the deployed CRDs after
// a deploy. This check sits BEFORE both in the release lane — it is the
// only one that runs on every PR and needs neither a Go struct nor a
// reachable cluster.

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// FluxCRDPaths are the canonical locations the guard compares.
const (
	FluxCRDChartDir  = "helm/crds"
	FluxCRDMirrorDir = "deploy/flux/crds"
)

// FluxCRDDriftReport is the result of one FluxCRDDriftCheck run.
type FluxCRDDriftReport struct {
	// ChartCRDs lists the CRD files found under helm/crds/ (basenames).
	ChartCRDs []string
	// MissingInMirror lists chart CRDs with no copy in deploy/flux/crds/.
	MissingInMirror []string
	// Drifted lists chart CRDs whose mirror copy is not byte-identical.
	Drifted []string
	// UnlistedInKustomization lists mirrored CRDs absent from the
	// kustomization's resources (an unapplied CRD).
	UnlistedInKustomization []string
	// KustomizationMissing is true when the mirror kustomization.yaml
	// itself does not exist (the whole guard surface is gone).
	KustomizationMissing bool
	// StaleKustomizationEntries lists kustomization resources that do
	// not exist as files in the mirror (breaks Flux reconciliation).
	StaleKustomizationEntries []string
}

// OK reports whether the Flux CRD path is drift-free.
func (r FluxCRDDriftReport) OK() bool {
	return len(r.MissingInMirror) == 0 &&
		len(r.Drifted) == 0 &&
		len(r.UnlistedInKustomization) == 0 &&
		len(r.StaleKustomizationEntries) == 0 &&
		!r.KustomizationMissing
}

// String renders the failure list with the operator fix.
func (r FluxCRDDriftReport) String() string {
	if r.OK() {
		return "(ok)"
	}
	var b strings.Builder
	for _, f := range r.MissingInMirror {
		fmt.Fprintf(&b, "  - %s: no mirror in %s/\n", f, FluxCRDMirrorDir)
	}
	for _, f := range r.Drifted {
		fmt.Fprintf(&b, "  - %s: mirror in %s/ is NOT byte-identical to %s/\n", f, FluxCRDMirrorDir, FluxCRDChartDir)
	}
	for _, f := range r.UnlistedInKustomization {
		fmt.Fprintf(&b, "  - %s: mirrored but NOT listed in %s/kustomization.yaml (Flux would never apply it)\n", f, FluxCRDMirrorDir)
	}
	for _, f := range r.StaleKustomizationEntries {
		fmt.Fprintf(&b, "  - kustomization references %q which has no file in %s/\n", f, FluxCRDMirrorDir)
	}
	if r.KustomizationMissing {
		fmt.Fprintf(&b, "  - %s/kustomization.yaml is missing\n", FluxCRDMirrorDir)
	}
	fmt.Fprintf(&b, "  A CRD schema change must bump the Flux CRD path in the SAME PR:\n"+
		"    make flux-sync-crds   (or: repolint -fix-flux-crds)\n"+
		"  then commit the result. See docs/release-bump-checklist.md (CRD apply\n"+
		"  train gate) and issue #2092 for the incident this guard closes.\n")
	return b.String()
}

// FluxCRDDriftCheck compares every chart CRD under <root>/helm/crds/ against
// the tracked Flux path mirror <root>/deploy/flux/crds/ and the mirror's
// kustomization. Drift surfaces via FluxCRDDriftReport.OK()==false; only
// structural errors (unreadable directory) return an error.
func FluxCRDDriftCheck(root string) (FluxCRDDriftReport, error) {
	rep := FluxCRDDriftReport{}

	chartDir := filepath.Join(root, FluxCRDChartDir)
	entries, err := os.ReadDir(chartDir)
	if err != nil {
		return rep, fmt.Errorf("read %s: %w", chartDir, err)
	}
	var chartCRDs []string
	for _, e := range entries {
		if e.IsDir() || filepath.Ext(e.Name()) != ".yaml" {
			continue
		}
		chartCRDs = append(chartCRDs, e.Name())
	}
	sort.Strings(chartCRDs)
	rep.ChartCRDs = chartCRDs

	mirrorDir := filepath.Join(root, FluxCRDMirrorDir)
	kustomization := filepath.Join(mirrorDir, "kustomization.yaml")
	kustBytes, kustErr := os.ReadFile(kustomization)

	for _, name := range chartCRDs {
		chartBytes, err := os.ReadFile(filepath.Join(chartDir, name))
		if err != nil {
			return rep, fmt.Errorf("read %s: %w", name, err)
		}
		mirrorBytes, err := os.ReadFile(filepath.Join(mirrorDir, name))
		if os.IsNotExist(err) {
			rep.MissingInMirror = append(rep.MissingInMirror, name)
			continue
		}
		if err != nil {
			return rep, fmt.Errorf("read mirror %s: %w", name, err)
		}
		if !bytes.Equal(chartBytes, mirrorBytes) {
			rep.Drifted = append(rep.Drifted, name)
		}
	}

	if kustErr != nil {
		if os.IsNotExist(kustErr) {
			rep.KustomizationMissing = true
			// Without the kustomization every mirrored file is
			// structurally unlisted; report that too so the fix list
			// is complete in one pass.
			for _, name := range chartCRDs {
				if !sliceContains(rep.MissingInMirror, name) {
					rep.UnlistedInKustomization = append(rep.UnlistedInKustomization, name)
				}
			}
			return rep, nil
		}
		return rep, fmt.Errorf("read %s: %w", kustomization, kustErr)
	}

	listed, err := fluxKustomizationResources(kustBytes)
	if err != nil {
		return rep, fmt.Errorf("parse %s: %w", kustomization, err)
	}
	listedSet := toSet(listed)

	for _, name := range chartCRDs {
		// Only mirrored files can be listed meaningfully; a missing
		// mirror is already reported above.
		if sliceContains(rep.MissingInMirror, name) {
			continue
		}
		if _, ok := listedSet[name]; !ok {
			rep.UnlistedInKustomization = append(rep.UnlistedInKustomization, name)
		}
	}

	// Stale entries: kustomization references that resolve to no file in
	// the mirror dir (a renamed/removed CRD whose entry survived).
	mirrorFiles := map[string]bool{}
	me, err := os.ReadDir(mirrorDir)
	if err != nil {
		return rep, fmt.Errorf("read %s: %w", mirrorDir, err)
	}
	for _, e := range me {
		if !e.IsDir() {
			mirrorFiles[e.Name()] = true
		}
	}
	for _, ref := range listed {
		if !mirrorFiles[filepath.Base(ref)] {
			rep.StaleKustomizationEntries = append(rep.StaleKustomizationEntries, ref)
		}
	}
	sort.Strings(rep.StaleKustomizationEntries)

	return rep, nil
}

// fluxKustomizationResources extracts the resources list from a
// kustomization.yaml (basename entries; sub-path entries are tolerated and
// compared by basename — the mirror is flat by contract).
func fluxKustomizationResources(src []byte) ([]string, error) {
	var doc struct {
		Resources []string `yaml:"resources"`
	}
	if err := yaml.Unmarshal(src, &doc); err != nil {
		return nil, err
	}
	return doc.Resources, nil
}

func sliceContains(ss []string, s string) bool {
	for _, v := range ss {
		if v == s {
			return true
		}
	}
	return false
}

// SyncFluxCRDs performs the fix side of the guard: copies every
// helm/crds/*.yaml into deploy/flux/crds/ (removing stale mirror files the
// chart no longer has) and rewrites kustomization.yaml's resources list to
// exactly the mirrored set. Pure-Go `cp` in the chart-sync-migrations
// shape; the comment header of the existing kustomization is regenerated
// from FluxCRDKustomizationHeader so the discipline text cannot drift.
func SyncFluxCRDs(root string) error {
	chartDir := filepath.Join(root, FluxCRDChartDir)
	mirrorDir := filepath.Join(root, FluxCRDMirrorDir)
	if err := os.MkdirAll(mirrorDir, 0o750); err != nil {
		return fmt.Errorf("create %s: %w", mirrorDir, err)
	}

	entries, err := os.ReadDir(chartDir)
	if err != nil {
		return fmt.Errorf("read %s: %w", chartDir, err)
	}
	var chartCRDs []string
	for _, e := range entries {
		if !e.IsDir() && filepath.Ext(e.Name()) == ".yaml" {
			chartCRDs = append(chartCRDs, e.Name())
		}
	}
	sort.Strings(chartCRDs)
	chartSet := toSet(chartCRDs)

	// Remove obsolete mirror files (a CRD deleted from the chart must not
	// keep applying from the Flux path).
	me, err := os.ReadDir(mirrorDir)
	if err != nil {
		return fmt.Errorf("read %s: %w", mirrorDir, err)
	}
	for _, e := range me {
		if e.IsDir() || filepath.Ext(e.Name()) != ".yaml" || e.Name() == "kustomization.yaml" {
			continue
		}
		if _, ok := chartSet[e.Name()]; !ok {
			if err := os.Remove(filepath.Join(mirrorDir, e.Name())); err != nil {
				return fmt.Errorf("remove stale %s: %w", e.Name(), err)
			}
		}
	}

	for _, name := range chartCRDs {
		if err := copyFileTo(filepath.Join(chartDir, name), filepath.Join(mirrorDir, name)); err != nil {
			return fmt.Errorf("copy %s: %w", name, err)
		}
	}

	var b strings.Builder
	b.WriteString(FluxCRDKustomizationHeader)
	b.WriteString("apiVersion: kustomize.config.k8s.io/v1beta1\nkind: Kustomization\nresources:\n")
	for _, name := range chartCRDs {
		fmt.Fprintf(&b, "  - %s\n", name)
	}
	return os.WriteFile(filepath.Join(mirrorDir, "kustomization.yaml"), []byte(b.String()), 0o644)
}

// FluxCRDKustomizationHeader is the discipline text written atop the
// regenerated kustomization — single-sourced so the hand-written original
// and any sync output cannot diverge.
const FluxCRDKustomizationHeader = `# The Flux CRD path mirror (issue #2092, release-train CRD guard).
#
# Production runs Flux-managed: CRDs ride the ops repo's kustomization
# ` + "`cluster-llmsafespaces-crds`" + ` (./kubernetes/apps/llmsafespaces/
# llmsafespaces/crds). ` + "`helm upgrade`" + ` NEVER upgrades crds/ (helm/README.md
# "CRD upgrades"), so that kustomization is the ONLY thing that moves a
# CRD schema on the cluster — and on 2026-11-17 it was the gap: the chart
# train moved to 0.44.15/.16/.17 carrying the spec.harness schema change
# while the Flux path still served the pre-.15 CRD. Every born-unbound
# workspace created since wedged in Creating forever (the apiserver pruned
# the bind write; nothing converged, nothing alerted).
#
# This directory is this repo's TRACKED COPY of what that Flux path must
# reference. repolint (` + "`make repolint`" + `, pre-commit + CI) fails the PR when
# any helm/crds/*.yaml is not byte-identical to its mirror here — a CRD
# schema change MUST bump this path in the SAME PR, and the release
# checklist (docs/release-bump-checklist.md, CRD apply train gate) carries
# the copy into the ops repo. Schema change = train gate on CRD apply.
#
# Sync with: make flux-sync-crds   (or: repolint -fix-flux-crds)
`

func copyFileTo(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }()
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer func() { _ = out.Close() }()
	if _, err := io.Copy(out, in); err != nil {
		return err
	}
	return out.Sync()
}
