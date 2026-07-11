// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package repolint

// ClusterDriftCheck — detect schema drift between the chart's CRD YAML
// (the source of truth for what fields the binary expects to be able to
// write) and the CRD that is actually deployed on a live Kubernetes
// cluster (the source of truth for what fields the apiserver will
// accept on the wire).
//
// Why this exists: CRDDriftCheck (crd_drift.go) catches Go↔chart-yaml
// drift at commit time. It does NOT catch chart-yaml↔cluster drift —
// the case where the chart is correct but the cluster's CRD is older.
// This is the failure mode behind worklog 0465: a workspace resume
// returned HTTP 200 but the controller never observed a transition
// because the deployed CRD was missing spec.suspend, so the apiserver
// silently pruned the field on every Update.
//
// Helm's `crds/` directory is install-only by design; helm upgrade does
// not reconcile CRDs. The Makefile's `helm-deploy` target applies them
// explicitly (`kubectl apply -f charts/.../crds/`) but operators who
// run `helm upgrade` directly bypass that step. This check makes the
// drift visible the next time anyone runs repolint with -cluster-drift.
//
// Limitations: same name-set semantics as CRDDriftCheck — compares the
// keys present at a chosen path in the chart YAML against the keys
// present at the same path in the deployed CRD. Type/constraint drift
// is out of scope.

import (
	"context"
	"fmt"
	"sort"
	"strings"

	apiextv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apiextclient "k8s.io/apiextensions-apiserver/pkg/client/clientset/clientset"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/clientcmd"
)

// ClusterDriftBinding declares one (chart-yaml ↔ deployed-CRD) drift
// check. The CRDName is the metadata.name of the deployed CRD on the
// cluster; the CRDFile and CRDPath address the same schema location
// in the chart YAML. The check terminates at a node whose
// `properties:` map is what gets compared, identical semantics to
// CRDBinding.CRDPath.
type ClusterDriftBinding struct {
	// CRDName is the metadata.name of the deployed CustomResourceDefinition
	// (e.g. "workspaces.llmsafespaces.dev").
	CRDName string
	// CRDFile is the path to the chart's CRD YAML, relative to repo root.
	CRDFile string
	// CRDPath walks from the document root to the schema node whose
	// `properties:` keys are compared. Same shape as CRDBinding.CRDPath.
	// Typically ["spec","versions","0","schema","openAPIV3Schema","properties","spec"].
	CRDPath []string
	// IgnoreClusterProperties lists keys present on the deployed CRD
	// but intentionally absent from the chart (rare; e.g. a field
	// deprecated server-side that callers no longer set).
	IgnoreClusterProperties []string
	// IgnoreChartProperties lists keys present in the chart but
	// intentionally not yet rolled out to the cluster (rare; only
	// useful during a multi-step migration where the chart leads).
	IgnoreChartProperties []string
}

// ClusterDriftReport is the result of one ClusterDriftCheck run.
type ClusterDriftReport struct {
	Binding ClusterDriftBinding
	// ChartMissingInCluster lists keys declared in the chart YAML but
	// absent from the deployed CRD. These are fields the binary will
	// try to write but the apiserver will silently drop. This is the
	// worklog 0465 incident symptom and the primary thing this check
	// is here to surface.
	ChartMissingInCluster []string
	// ClusterMissingInChart lists keys declared on the deployed CRD
	// but absent from the chart. Indicates a stale CRD on the cluster
	// from a previous chart version that has since been pruned.
	ClusterMissingInChart []string
}

// OK reports whether the binding is drift-free.
func (r ClusterDriftReport) OK() bool {
	return len(r.ChartMissingInCluster) == 0 && len(r.ClusterMissingInChart) == 0
}

// String returns a human-readable, unified-style diff.
func (r ClusterDriftReport) String() string {
	if r.OK() {
		return "(ok)"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "  binding: cluster:%s @ %s  vs  %s\n",
		r.Binding.CRDName, strings.Join(r.Binding.CRDPath, "."), r.Binding.CRDFile)
	if len(r.ChartMissingInCluster) > 0 {
		fmt.Fprintf(&b, "  Chart has but cluster missing (%d) — apiserver will silently prune writes:\n",
			len(r.ChartMissingInCluster))
		for _, f := range r.ChartMissingInCluster {
			fmt.Fprintf(&b, "    + %s\n", f)
		}
	}
	if len(r.ClusterMissingInChart) > 0 {
		fmt.Fprintf(&b, "  Cluster has but chart missing (%d) — stale CRD on cluster:\n",
			len(r.ClusterMissingInChart))
		for _, f := range r.ClusterMissingInChart {
			fmt.Fprintf(&b, "    + %s\n", f)
		}
	}
	fmt.Fprintf(&b, "  remediation: kubectl apply -f %s\n", r.Binding.CRDFile)
	return b.String()
}

// CRDFetcher abstracts the apiserver call so the diff logic is unit-
// testable without a live cluster. Production wiring uses
// NewKubeCRDFetcher; tests pass a stub.
type CRDFetcher interface {
	GetCRD(ctx context.Context, name string) (*apiextv1.CustomResourceDefinition, error)
}

type kubeCRDFetcher struct {
	c apiextclient.Interface
}

func (k *kubeCRDFetcher) GetCRD(ctx context.Context, name string) (*apiextv1.CustomResourceDefinition, error) {
	return k.c.ApiextensionsV1().CustomResourceDefinitions().Get(ctx, name, metav1.GetOptions{})
}

// NewKubeCRDFetcher loads kubeconfig (KUBECONFIG env, then default
// merge rules) and constructs a CRDFetcher backed by the live
// apiextensions API. Returns an error if no current-context can be
// resolved — the operator should set KUBECONFIG or run inside a pod.
func NewKubeCRDFetcher() (CRDFetcher, error) {
	loadingRules := clientcmd.NewDefaultClientConfigLoadingRules()
	cfg, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(
		loadingRules, &clientcmd.ConfigOverrides{},
	).ClientConfig()
	if err != nil {
		return nil, fmt.Errorf("load kubeconfig: %w", err)
	}
	c, err := apiextclient.NewForConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("build apiextensions client: %w", err)
	}
	return &kubeCRDFetcher{c: c}, nil
}

// ClusterDriftCheck fetches the deployed CRD and compares its
// properties at CRDPath against the chart YAML's properties at the
// same path.
//
// Returns an error only on unrecoverable input failure: chart YAML
// missing/malformed, deployed CRD not found, or path not resolvable
// in either side. Drift itself is non-fatal and surfaces via
// ClusterDriftReport.OK()==false.
func ClusterDriftCheck(ctx context.Context, root string, b ClusterDriftBinding, f CRDFetcher) (ClusterDriftReport, error) {
	rep := ClusterDriftReport{Binding: b}

	chartProps, err := extractCRDProperties(joinPath(root, b.CRDFile), b.CRDPath)
	if err != nil {
		return rep, fmt.Errorf("parse chart CRD: %w", err)
	}

	deployed, err := f.GetCRD(ctx, b.CRDName)
	if err != nil {
		return rep, fmt.Errorf("fetch cluster CRD %s: %w", b.CRDName, err)
	}
	clusterProps, err := extractDeployedCRDProperties(deployed, b.CRDPath)
	if err != nil {
		return rep, fmt.Errorf("walk cluster CRD %s: %w", b.CRDName, err)
	}

	ignoreCluster := toSet(b.IgnoreClusterProperties)
	ignoreChart := toSet(b.IgnoreChartProperties)

	for name := range chartProps {
		if _, ok := clusterProps[name]; ok {
			continue
		}
		if _, ok := ignoreChart[name]; ok {
			continue
		}
		rep.ChartMissingInCluster = append(rep.ChartMissingInCluster, name)
	}
	for name := range clusterProps {
		if _, ok := chartProps[name]; ok {
			continue
		}
		if _, ok := ignoreCluster[name]; ok {
			continue
		}
		rep.ClusterMissingInChart = append(rep.ClusterMissingInChart, name)
	}
	sort.Strings(rep.ChartMissingInCluster)
	sort.Strings(rep.ClusterMissingInChart)
	return rep, nil
}

// extractDeployedCRDProperties walks the deployed CRD's openAPIV3Schema
// along crdPath and returns the keys of the .properties map at the
// terminal schema node — matching the contract of extractCRDProperties
// (which walks the YAML and then implicitly steps into `properties`).
//
// crdPath uses the same convention as the YAML walker:
//
//	["spec","versions","0","schema","openAPIV3Schema","properties","spec"]
//
// where each step after openAPIV3Schema is either:
//   - "properties" — descend into the .Properties map; the next path
//     element must be a property name to descend into;
//   - "items" — descend into the array element schema (.Items.Schema).
//
// The path must terminate at a schema node (one whose .Properties map
// is what we ultimately want to return). The walker then returns the
// keys of that node's .Properties map.
func extractDeployedCRDProperties(crd *apiextv1.CustomResourceDefinition, crdPath []string) (map[string]struct{}, error) {
	if len(crdPath) < 5 {
		return nil, fmt.Errorf("path too short, expected at least 5 elements (spec.versions.N.schema.openAPIV3Schema...): %v", crdPath)
	}
	// First five elements are fixed for every CRD path we use:
	//   0: "spec"
	//   1: "versions"
	//   2: numeric index into spec.versions
	//   3: "schema"
	//   4: "openAPIV3Schema"
	if crdPath[0] != "spec" || crdPath[1] != "versions" || crdPath[3] != "schema" || crdPath[4] != "openAPIV3Schema" {
		return nil, fmt.Errorf("unsupported path prefix; expected spec.versions.N.schema.openAPIV3Schema, got %v", crdPath[:5])
	}
	var idx int
	if _, err := fmt.Sscanf(crdPath[2], "%d", &idx); err != nil {
		return nil, fmt.Errorf("expected integer version index at path[2], got %q", crdPath[2])
	}
	if idx < 0 || idx >= len(crd.Spec.Versions) {
		return nil, fmt.Errorf("version index %d out of range [0,%d)", idx, len(crd.Spec.Versions))
	}
	v := crd.Spec.Versions[idx]
	if v.Schema == nil || v.Schema.OpenAPIV3Schema == nil {
		return nil, fmt.Errorf("version %s has no openAPIV3Schema", v.Name)
	}

	// Walk from openAPIV3Schema along the rest of crdPath. After the
	// fixed prefix [spec,versions,N,schema,openAPIV3Schema], remaining
	// elements come in pairs ("properties","<name>") with optional
	// "items" steps. The loop pairs them up; an unpaired terminal
	// "properties" step is allowed and yields that map's keys.
	cur := v.Schema.OpenAPIV3Schema
	for i := 5; i < len(crdPath); i++ {
		key := crdPath[i]
		switch key {
		case "properties":
			if cur.Properties == nil {
				return nil, fmt.Errorf("at path %q: node has no properties map",
					strings.Join(crdPath[:i+1], "."))
			}
			// terminal "properties" — return the keys.
			if i == len(crdPath)-1 {
				return propsKeys(cur.Properties), nil
			}
			// otherwise the next element must be a property name to descend into.
			// nextIdx (i+1) is provably in-bounds because the early-return above
			// handled the i==len-1 case, but gosec can't see that.
			nextIdx := i + 1
			//nolint:gosec // G602: nextIdx<len(crdPath) by the early-return guard above
			fieldName := crdPath[nextIdx]
			next, ok := cur.Properties[fieldName]
			if !ok {
				return nil, fmt.Errorf("at path %q: property %q not found",
					strings.Join(crdPath[:nextIdx+1], "."), fieldName)
			}
			cur = &next
			i = nextIdx // skip the field name we just consumed
		case "items":
			if cur.Items == nil || cur.Items.Schema == nil {
				return nil, fmt.Errorf("at path %q: node has no items.Schema",
					strings.Join(crdPath[:i+1], "."))
			}
			cur = cur.Items.Schema
		default:
			return nil, fmt.Errorf("at path %q: unsupported step %q (only `properties` and `items` are supported after openAPIV3Schema)",
				strings.Join(crdPath[:i+1], "."), key)
		}
	}
	// Path terminated at a schema node (e.g. ["...","properties","spec"]).
	// Return the keys of that node's .Properties map — matches
	// extractCRDProperties's "step into `properties` after path walk"
	// contract.
	if cur.Properties == nil {
		return nil, fmt.Errorf("terminal node at %s has no `properties` map",
			strings.Join(crdPath, "."))
	}
	return propsKeys(cur.Properties), nil
}

// propsKeys returns the key set of a CRD properties map.
func propsKeys(p map[string]apiextv1.JSONSchemaProps) map[string]struct{} {
	out := make(map[string]struct{}, len(p))
	for k := range p {
		out[k] = struct{}{}
	}
	return out
}

// joinPath joins a repo root with a relative path. Wrapper kept
// separate so callers don't import filepath here just to compose
// paths (the rest of the package imports filepath).
func joinPath(root, rel string) string {
	if root == "" {
		return rel
	}
	if strings.HasSuffix(root, "/") {
		return root + rel
	}
	return root + "/" + rel
}

// LiveClusterBindings returns the (chart YAML ↔ deployed CRD) pairs
// that the cluster-drift check evaluates by default. These mirror
// LiveBindings() but are addressed by the deployed CRD's metadata.name
// rather than by Go struct.
//
// Adding a binding here surfaces it in `repolint -cluster-drift`.
func LiveClusterBindings() []ClusterDriftBinding {
	return []ClusterDriftBinding{
		{
			CRDName: "workspaces.llmsafespaces.dev",
			CRDFile: "helm/crds/workspace.yaml",
			CRDPath: []string{"spec", "versions", "0", "schema", "openAPIV3Schema", "properties", "spec"},
		},
		{
			CRDName: "workspaces.llmsafespaces.dev",
			CRDFile: "helm/crds/workspace.yaml",
			CRDPath: []string{"spec", "versions", "0", "schema", "openAPIV3Schema", "properties", "status"},
		},
		{
			CRDName: "runtimeenvironments.llmsafespaces.dev",
			CRDFile: "helm/crds/runtimeenvironment.yaml",
			CRDPath: []string{"spec", "versions", "0", "schema", "openAPIV3Schema", "properties", "spec"},
		},
		{
			CRDName: "runtimeenvironments.llmsafespaces.dev",
			CRDFile: "helm/crds/runtimeenvironment.yaml",
			CRDPath: []string{"spec", "versions", "0", "schema", "openAPIV3Schema", "properties", "status"},
		},
		{
			CRDName: "inferencerelays.llmsafespaces.dev",
			CRDFile: "helm/crds/inferencerelay.yaml",
			CRDPath: []string{"spec", "versions", "0", "schema", "openAPIV3Schema", "properties", "spec"},
		},
		{
			CRDName: "inferencerelays.llmsafespaces.dev",
			CRDFile: "helm/crds/inferencerelay.yaml",
			CRDPath: []string{"spec", "versions", "0", "schema", "openAPIV3Schema", "properties", "status"},
		},
	}
}
