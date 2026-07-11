# Worklog: NetworkPolicy CGNAT drift — chart/controller parity

**Date:** 2026-07-11
**Session:** Close the chart-side / controller-side drift in the default blocked-egress CIDR list. Fourth of the network hardening sweep targeted at v0.3.0.
**Status:** Complete

---

## Objective

The chart's `networkPolicy.blockedEgressCIDRs` default (`charts/llmsafespaces/values.yaml:720-724`) listed only 4 CIDRs:

```
- 10.0.0.0/8
- 172.16.0.0/12
- 192.168.0.0/16
- 169.254.0.0/16
```

The controller-side equivalent (`controller/internal/workspace/network_policy.go:95-102`, `privateOrInternalCIDRs`) lists 7:

```
- 10.0.0.0/8
- 172.16.0.0/12
- 192.168.0.0/16
- 169.254.0.0/16
- 127.0.0.0/8
- 224.0.0.0/4
- 100.64.0.0/10   ← CGNAT
```

The chart-side list is the default egress `except:` applied to ALL workspace pods. The controller-side list is applied at `spec.networkAccess.egress` validation. Both serve overlapping-but-different purposes; their CIDR sets drifted when Epic 17 G16 added CGNAT (and loopback/multicast) to the controller side.

The security-relevant gap is **`100.64.0.0/10` (CGNAT)**. Managed Kubernetes offerings — AKS default VNet, some EKS configs, k3s with default flannel — use `100.64/10` as the pod CIDR. Without it in the chart-side `except:` list, workspace pods on such clusters can reach internal pods/services in the CGNAT range, defeating the cross-tenant isolation the policy is supposed to enforce.

Goal: bring the chart-side list to parity with the controller-side list, and pin the parity with a test.

---

## Work Completed

### TDD: failing tests first (`charts/llmsafespaces/networkpolicy_cgnat_test.go`)

- `TestG16_DefaultRender_BlockedEgressIncludesCGNAT` — primary fix. Asserts the rendered workspace-egress NetworkPolicy's `ipBlock.except:` list includes `100.64.0.0/10`.
- `TestG16_DefaultRender_BlockedEgressIncludesAllControllerSideCIDRs` — pins parity for the security-relevant subset (`10/8, 172.16/12, 192.168/16, 169.254/16, 100.64/10`).
- Helper `blockedEgressCIDRs` extracts the `except:` list from the rendered policy.

Verified red before implementing.

### Fix (`charts/llmsafespaces/values.yaml`)

Added the 3 missing CIDRs with per-CIDR comments explaining the rationale and a back-reference to `controller/internal/workspace/network_policy.go:privateOrInternalCIDRs` so future drift is mechanically preventable:

```yaml
blockedEgressCIDRs:
  # This list mirrors `privateOrInternalCIDRs` in
  # controller/internal/workspace/network_policy.go. Keep both in sync;
  # the chart-test TestG16_DefaultRender_BlockedEgressIncludesAllControllerSideCIDRs
  # pins parity for the security-relevant subset.
  - 10.0.0.0/8        # RFC1918 — in-cluster service ranges, internal admin endpoints
  - 172.16.0.0/12     # RFC1918
  - 192.168.0.0/16    # RFC1918
  - 169.254.0.0/16    # link-local + cloud metadata (169.254.169.254)
  - 100.64.0.0/10     # CGNAT — used by managed Kubernetes pod CIDRs (AKS, some EKS, k3s default)
  - 127.0.0.0/8       # loopback
  - 224.0.0.0/4       # multicast
```

---

## Key Decisions

1. **Add all 3 missing CIDRs, not just CGNAT.** Loopback and multicast are security-irrelevant in practice (no realistic egress target), but full parity is cheap, makes the two lists identical (easier to reason about), and the test pins the parity going forward. Half-parity would have left the door open to the same drift reoccurring on the other 2.
2. **Document the cross-reference in the chart comment.** Future maintainers adding a CIDR to one list need to know about the other. The back-reference + the parity test close the loop.
3. **Out of scope: the `except:` overrides-`allowed:` semantics.** A pre-existing limitation means operators who genuinely need to reach e.g. a CGNAT LLM gateway from a workspace can't do it by adding `100.64.0.0/10` to `allowedEgressCIDRs` — the `except` blocks it. The documented escape hatch (expose via a public FQDN behind ingress) is the supported path. This PR doesn't change that; it only extends the deny list.

---

## Assumptions stated and validated (Rule 7)

1. *The chart-side list is the default deny set for ALL workspace pods.* Validated by reading `workspace-network-policy.yaml:174-185` — the `except:` block applies the `blockedEgressCIDRs` list to every `allowedEgressCIDRs` ipBlock entry.
2. *The two lists are intended to mirror each other.* Validated by comparing the controller comment (`network_policy.go:83-91`) with the chart comment — same rationale ("in-cluster service ranges", "cloud metadata", "CGNAT").
3. *Adding CIDRs to the deny list is security-direction-correct and can only make egress more restrictive.* Validated by K8s NetworkPolicy semantics — `except` is a hard block.
4. *Operators who need an exception today have the same options they had before.* Validated by reading the controller-side comment ("expose via a public FQDN behind ingress, OR add the specific CIDR to the chart's allowedEgressCIDRs"). Pre-existing limitation, unchanged by this PR.

---

## Blockers

None.

---

## Tests Run

```
go test -timeout 120s -run "TestG16_DefaultRender_BlockedEgress" ./charts/llmsafespaces/...
→ ok  0.328s

go test -timeout 180s ./charts/llmsafespaces/...
→ ok  19.242s   (no regressions in existing chart tests)

gofmt -l charts/llmsafespaces/networkpolicy_cgnat_test.go   → clean
goimports -l charts/llmsafespaces/networkpolicy_cgnat_test.go  → clean
```

---

## Next Steps

1. Open this PR for review.
2. After approval + merge: runtimeClass webhook gate, JWT iss/aud, doc reconciliation, v0.3.0 release.

---

## Files Modified

- `charts/llmsafespaces/values.yaml` (added 3 missing CIDRs + cross-reference comment)
- `charts/llmsafespaces/networkpolicy_cgnat_test.go` (new file — TDD parity test)
- `worklogs/0608_2026-07-11_networkpolicy-cgnat-drift.md` (this entry)
