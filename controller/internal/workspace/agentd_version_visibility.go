// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package workspace

// agentd_version_visibility.go — RC-A (2026-10-30 incident): fleet
// currency for workspace-resident agentds.
//
// The incident: the pod ran agentd v0.44.7 (commit 6ce1859f, built
// Oct 29 02:08) while the #1986 spawn fix had shipped in v0.44.9 ~15
// hours earlier — the fix existed but never reached the workspace, and
// discovering the skew required manually curling the pod's admin
// healthz (localhost:4098/v1/healthz), which already carried
// commit_sha/build_time/uptime. Workspace pods do not roll with fleet
// releases (#1746 owner ruling: the user owns when their sessions are
// interrupted) — so an N-1 fleet is BY DESIGN, but an INVISIBLE N-1
// fleet is not.
//
// This file closes the visibility gap on the controller's existing
// 15s liveness poll of that same healthz endpoint:
//
//   - Visibility: the running agentd's self-reported build identity
//     (version / commit_sha / build_time) is mirrored onto the
//     Workspace CRD status and through the API's status read — no
//     shell access needed to answer "what is this pod running?".
//   - Skew detection: the reported version is compared against
//     FleetVersion — the release the controller itself was deployed
//     with (the chart appVersion; both binaries are stamped by the
//     same release pipeline). Older ⇒ the AgentdVersionStale
//     condition, a Warning event exactly-once per (agentd, fleet)
//     version pair, the llmsafespaces_workspace_agentd_version_skew
//     gauge (1 while stale), and the episode counter. The roll
//     decision itself stays with the #1746 updatePolicy — see
//     docs/operator/agentd-delivery.md ("Version visibility & skew
//     detection") for the runbook.

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/lenaxia/llmsafespaces/controller/internal/metrics"
	"github.com/lenaxia/llmsafespaces/pkg/agentd"
	v1 "github.com/lenaxia/llmsafespaces/pkg/apis/llmsafespaces/v1"
)

// agentdVersionStaleReportedAnnotation persists the last-reported skew
// stamp so the exactly-once event survives controller restarts (the
// same pattern as mirrorLegacyScrub / mirrorAgentSupervisor).
const agentdVersionStaleReportedAnnotation = "llmsafespaces.dev/agentd-version-stale-reported"

// mirrorAgentdVersion (RC-A) mirrors the running agentd's self-reported
// build identity from /v1/healthz onto the CRD status and evaluates
// version skew against FleetVersion. Runs on ANY decoded healthz
// response — build identity is a property of the binary that answered,
// not of its health — mirroring mirrorAgentSupervisor's placement.
//
// Mixed-fleet policy (W15): empty response fields (an older runtime
// omitting commit_sha/build_time) keep the previously mirrored value;
// the unreachable/undecodable paths in health.go clear all three.
//
// Skew verdicts:
//
//   - agentd < fleet  → AgentdVersionStale=True, gauge 1, one Warning
//     event + counter tick per (agentd, fleet) version pair.
//   - agentd >= fleet → AgentdVersionStale=False/Current, gauge 0.
//   - not comparable (an "unknown"/dev build on either side) →
//     AgentdVersionStale=False/Unknown, gauge UNTOUCHED — the last
//     comparable verdict stays latched instead of flapping green on a
//     weird response.
//
// Never gates health, liveness, or restarts (the roll decision is the
// #1746 updatePolicy's).
func (r *WorkspaceReconciler) mirrorAgentdVersion(ctx context.Context, ws *v1.Workspace, rep *agentd.HealthzResponse) {
	if rep == nil {
		return
	}
	if rep.Version != "" {
		ws.Status.AgentdVersion = rep.Version
	}
	if rep.CommitSHA != "" {
		ws.Status.AgentdCommit = rep.CommitSHA
	}
	if rep.BuildTime != "" {
		ws.Status.AgentdBuildTime = rep.BuildTime
	}

	agentdVersion := ws.Status.AgentdVersion
	cmp, ok := compareVersions(agentdVersion, r.FleetVersion)
	if !ok {
		r.setCondition(ws, v1.WorkspaceConditionAgentdVersionStale, "False", v1.ReasonAgentdVersionUnknown,
			fmt.Sprintf("agentd version not comparable with the fleet release (agentd %q, fleet %q) — unstamped build?",
				agentdVersion, r.FleetVersion))
		return
	}
	if cmp >= 0 {
		metrics.WorkspaceAgentdVersionSkew.WithLabelValues(ws.Name).Set(0)
		r.setCondition(ws, v1.WorkspaceConditionAgentdVersionStale, "False", v1.ReasonAgentdVersionCurrent,
			fmt.Sprintf("agentd %s is current against the fleet release %s", agentdVersion, r.FleetVersion))
		return
	}

	// Stale: the incident's silent skew, now loud.
	metrics.WorkspaceAgentdVersionSkew.WithLabelValues(ws.Name).Set(1)
	r.setCondition(ws, v1.WorkspaceConditionAgentdVersionStale, "True", v1.ReasonAgentdVersionStale,
		fmt.Sprintf("agentd %s (commit %s, built %s) is older than the fleet release %s — the fix train has not reached this pod; roll runbook: docs/operator/agentd-delivery.md",
			agentdVersion, ws.Status.AgentdCommit, ws.Status.AgentdBuildTime, r.FleetVersion))

	stamp := agentdVersion + "|" + r.FleetVersion
	if ws.Annotations[agentdVersionStaleReportedAnnotation] == stamp {
		return // already reported this exact skew pair (in-memory fast path)
	}
	logger := log.FromContext(ctx)
	logger.Info("agentd version skew detected (RC-A): workspace agentd older than the fleet release",
		"workspace", ws.Name, "agentdVersion", agentdVersion,
		"agentdCommit", ws.Status.AgentdCommit, "fleetVersion", r.FleetVersion)
	metrics.WorkspaceAgentdVersionSkewDetectedTotal.WithLabelValues(ws.Name).Inc()

	// Persist the stamp via a FULL object Update on a FRESH fetch — the
	// health-check's Status().Update writes only the status subresource
	// and drops metadata (same reason as mirrorLegacyScrub).
	var fresh v1.Workspace
	if err := r.Get(ctx, types.NamespacedName{Name: ws.Name, Namespace: ws.Namespace}, &fresh); err != nil {
		return // a later pass retries (the in-memory stamp stays unset)
	}
	if fresh.Annotations == nil {
		fresh.Annotations = map[string]string{}
	}
	fresh.Annotations[agentdVersionStaleReportedAnnotation] = stamp
	if err := r.Update(ctx, &fresh); err != nil {
		return // a later pass retries honestly; never silently dropped
	}
	ws.ResourceVersion = fresh.ResourceVersion // keep the caller's later Status().Update conflict-free
	if ws.Annotations == nil {
		ws.Annotations = map[string]string{}
	}
	ws.Annotations[agentdVersionStaleReportedAnnotation] = stamp
	if r.Recorder != nil {
		r.Recorder.Eventf(ws, "Warning", "AgentdVersionStale",
			"workspace agentd %s (commit %s, built %s) is older than the fleet release %s — platform fixes have not reached this pod; see docs/operator/agentd-delivery.md for the roll runbook",
			agentdVersion, ws.Status.AgentdCommit, ws.Status.AgentdBuildTime, r.FleetVersion)
	}
}

// compareVersions orders two release version strings semver-ishly:
// numeric dot segments compared left-to-right ("0.44.7" < "0.44.11"),
// tolerating a leading "v" and ignoring prerelease/build-metadata
// suffixes ("0.44.11-rc.1", "0.44.7+g6ce1859f"). Returns (cmp, true)
// with cmp <0 / 0 / >0, or (0, false) when either side is not a
// numeric dot version ("unknown", "dev") — incomparable is a verdict,
// not an error: the caller reports it instead of guessing.
func compareVersions(a, b string) (int, bool) {
	na, ok := parseNumericVersion(a)
	if !ok {
		return 0, false
	}
	nb, ok := parseNumericVersion(b)
	if !ok {
		return 0, false
	}
	n := len(na)
	if len(nb) > n {
		n = len(nb)
	}
	for i := 0; i < n; i++ {
		va, vb := 0, 0
		if i < len(na) {
			va = na[i]
		}
		if i < len(nb) {
			vb = nb[i]
		}
		if va != vb {
			if va < vb {
				return -1, true
			}
			return 1, true
		}
	}
	return 0, true
}

// parseNumericVersion extracts the numeric dot segments of a version
// string ("v0.44.7+gabc" → [0 44 7]). Every segment must be a plain
// non-negative integer; empty or non-numeric segments (including the
// "unknown" placeholder) make the whole version incomparable.
func parseNumericVersion(v string) ([]int, bool) {
	v = strings.TrimPrefix(v, "v")
	if i := strings.IndexAny(v, "+-"); i >= 0 {
		v = v[:i]
	}
	if v == "" {
		return nil, false
	}
	parts := strings.Split(v, ".")
	nums := make([]int, 0, len(parts))
	for _, p := range parts {
		if p == "" {
			return nil, false
		}
		n, err := strconv.Atoi(p)
		if err != nil {
			return nil, false
		}
		nums = append(nums, n)
	}
	return nums, true
}
