// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package workspace

import (
	"context"
	"fmt"
	"sync"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"

	v1 "github.com/lenaxia/llmsafespaces/pkg/apis/llmsafespaces/v1"
)

// crd_capability.go — the CREATE-TIME capability probe (issue #2092
// ruling 4): on a cluster whose Workspace CRD predates 0.44.15 (no
// spec.harness), a workspace created WITHOUT an explicit harness is
// born-unbound and can NEVER bind — the apiserver prunes every
// spec.harness write, so it parks in Creating forever with no pod and
// no convergence (the #2051 silent-degradation lesson: fail loud at
// creation instead of accepting an object that can never work).
//
// The probe is EMPIRICAL, not schema-reflective: a server-side DRY-RUN
// create of a minimal Workspace carrying spec.harness=opencode runs the
// full apiserver pipeline (webhook admission, structural pruning) and
// returns the object as it would be persisted — nothing is stored. A
// returned object that still carries spec.harness means the cluster CRD
// has the field; a pruned one means it does not. This needs no CRD-read
// RBAC (the create verb the API already holds covers DryRun) and cannot
// disagree with the real pruning behavior the bind path verified (.16).
//
// Caching discipline (charter: "startup + on skew detection, not
// per-request cluster calls"): the verdict is cached for
// capabilityCacheTTL (60s — bounds post-heal staleness to a minute
// while capping probe traffic at one call per window regardless of
// create QPS), and INVALIDATED the moment a bind fails to persist (the
// freshest skew evidence there is). Inconclusive probes (any error)
// never gate creation — absence of evidence is not evidence (#1685
// posture); the bind-verify remains the runtime authority.

// crdCapability is the probe's verdict for one spec field.
type crdCapability int

const (
	// crdCapabilityUnknown — the probe errored; creation must NOT be
	// gated on it (fail-open: the bind-verify still catches a real skew
	// loudly at bind time).
	crdCapabilityUnknown crdCapability = iota
	// crdCapabilitySupported — the dry-run round-trip kept the field.
	crdCapabilitySupported
	// crdCapabilitySkewed — the dry-run round-trip pruned the field:
	// the cluster CRD predates the bind-once schema.
	crdCapabilitySkewed
)

// capabilityCacheTTL bounds how long a conclusive verdict is trusted.
// 60s: a post-CRD-apply heal unblocks creates within a minute, while a
// hostile caller cannot make the API probe per-request. Var for tests.
var capabilityCacheTTL = 60 * time.Second

// harnessCapabilityProbe is the probe body seam (dynamic-client
// dry-run create by default; tests inject a stub). Returns the verdict.
type harnessCapabilityProbe func(ctx context.Context) crdCapability

// capabilityCache memoizes one field's verdict with a TTL + explicit
// invalidation. Zero value is ready (nil probe = always unknown).
type capabilityCache struct {
	mu        sync.Mutex
	probe     harnessCapabilityProbe
	verdict   crdCapability
	checkedAt time.Time
}

// newCapabilityCache wraps a probe body.
func newCapabilityCache(probe harnessCapabilityProbe) *capabilityCache {
	return &capabilityCache{probe: probe}
}

// get returns the cached verdict, probing (at most once per TTL) when
// stale or invalidated. A nil/absent probe yields unknown without
// gating anything.
func (c *capabilityCache) get(ctx context.Context) crdCapability {
	if c == nil || c.probe == nil {
		return crdCapabilityUnknown
	}
	c.mu.Lock()
	fresh := c.verdict != crdCapabilityUnknown && time.Since(c.checkedAt) < capabilityCacheTTL
	if fresh {
		v := c.verdict
		c.mu.Unlock()
		return v
	}
	c.mu.Unlock()

	v := c.probe(ctx)

	c.mu.Lock()
	c.verdict = v
	c.checkedAt = time.Now()
	c.mu.Unlock()
	return v
}

// invalidate drops the cached verdict — called on live skew evidence
// (a bind that failed to persist) so the very next create re-probes
// instead of trusting a stale "supported".
func (c *capabilityCache) invalidate() {
	if c == nil {
		return
	}
	c.mu.Lock()
	c.verdict = crdCapabilityUnknown
	c.mu.Unlock()
}

// dynamicHarnessCapabilityProbe is the production probe body: dry-run
// create via the dynamic client (the typed WorkspaceInterface has no
// options parameter; the dynamic client reaches the same endpoint with
// CreateOptions.DryRun). The minimal object passes the CRD's required
// fields and names a RuntimeEnvironment (no '/' → no registry
// allow-list leg in the webhook), so a healthy cluster admits it and a
// skewed one prunes spec.harness.
func (s *Service) dynamicHarnessCapabilityProbe(ctx context.Context) crdCapability {
	if s.k8sClient == nil {
		return crdCapabilityUnknown
	}
	dyn := s.k8sClient.DynamicClient()
	if dyn == nil {
		return crdCapabilityUnknown
	}
	probe := &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": v1.SchemeGroupVersion.String(),
			"kind":       "Workspace",
			"metadata": map[string]interface{}{
				"generateName": "crd-capability-probe-",
			},
			"spec": map[string]interface{}{
				"owner":   map[string]interface{}{"userID": "crd-capability-probe"},
				"runtime": "python-3.11",
				"storage": map[string]interface{}{"size": "1Gi"},
				"harness": string(v1.HarnessOpencode),
			},
		},
	}
	gvr := schema.GroupVersionResource{
		Group:    v1.SchemeGroupVersion.Group,
		Version:  v1.SchemeGroupVersion.Version,
		Resource: "workspaces",
	}
	created, err := dyn.Resource(gvr).Namespace(s.config.Namespace).Create(
		ctx, probe, metav1.CreateOptions{DryRun: []string{metav1.DryRunAll}})
	if err != nil {
		// Inconclusive: RBAC, webhook refusal, apiserver hiccup — never
		// a skew verdict (the bind-verify stays the authority).
		s.logger.Warn("harness capability probe inconclusive (dry-run create failed); create-time gate inactive this window",
			"error", err.Error())
		return crdCapabilityUnknown
	}
	harness, found, _ := unstructured.NestedString(created.Object, "spec", "harness")
	if !found || harness == "" {
		return crdCapabilitySkewed
	}
	return crdCapabilitySupported
}

// errCRDMissingHarnessCapability is the fail-loud create-time refusal.
func errCRDMissingHarnessCapability() error {
	return fmt.Errorf(
		"the cluster's Workspace CRD predates bind-once (no spec.harness): a workspace created without an explicit harness can never bind on this cluster " +
			"(the apiserver prunes the bind write and it parks in Creating forever) — pass an explicit harness or upgrade the CRD " +
			"(kubectl apply -f helm/crds/workspace.yaml, or bump the Flux crds path; see docs/operator/upgrading.md, CRD skew recovery)")
}

// ensureHarnessCapabilityCache lazily arms the service's probe cache
// (constructor-shaped nil-safety: unwired/test services never panic).
func (s *Service) ensureHarnessCapabilityCache() {
	if s.harnessCapability != nil {
		return
	}
	s.harnessCapability = newCapabilityCache(s.dynamicHarnessCapabilityProbe)
}
