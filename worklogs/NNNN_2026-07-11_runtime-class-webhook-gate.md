# Worklog: runtimeClass webhook gate — admin-gate the gVisor opt-out

**Date:** 2026-07-11
**Session:** Close the S51.2 deferral on `spec.runtimeClass` enforcement. Fifth of the network hardening sweep targeted at v0.3.0.
**Status:** Complete

---

## Objective

The Workspace CRD's `spec.runtimeClass` field (Epic 51 S51.1) lets a workspace opt out of the default RuntimeClass (typically gVisor in prod). The design (`design/stories/epic-51-tenant-isolation/README.md:92`) explicitly states "Opt-out is admin-gated, not tenant-selectable, to prevent weakening the default." But the CRD field comment (`pkg/apis/llmsafespaces/v1/workspace_types.go:158-161`) documented this as a deferred S51.2 follow-up:

> Enforcement: webhook validation to prevent tenants from setting this field via direct kubectl is deferred to S51.2. Today the API's CreateWorkspaceRequest does not expose the field (mitigating the API path), but direct kubectl users can set it.

Goal: close that gap. Any user with workspace create/update RBAC who could `kubectl apply` a Workspace with `spec.runtimeClass: "runc"` could escape gVisor, defeating the kernel-level isolation layer for their own pods.

---

## Work Completed

### TDD: failing tests first (`controller/internal/webhooks/workspace_runtimeclass_test.go`)

- `TestWorkspaceWebhook_RuntimeClassRejectsByDefault` — primary case.
- `TestWorkspaceWebhook_RuntimeClassAllowedWithAdminAnnotation` — operator path.
- `TestWorkspaceWebhook_RuntimeClassNilAlwaysAllowed` — secure default.
- `TestWorkspaceWebhook_RuntimeClassAnnotationWrongValue` — value must be literal "true".
- `TestWorkspaceWebhook_RuntimeClassUpdateRejectedWithoutAnnotation` — UPDATE path.
- `TestWorkspaceWebhook_RuntimeClassUpdateAllowedWithAnnotation` — UPDATE with annotation.

Verified red before implementing.

### Implementation (`controller/internal/webhooks/workspace_webhook.go`)

- New constant `allowRuntimeClassOverrideAnnotation = "llmsafespaces.dev/allow-runtime-class-override"` with a doc block explaining the admin-gating scheme.
- New helper `adminAllowsRuntimeClassOverride(annotations) bool` — strict `== "true"` check (no YAML-truthy ambiguity).
- New validation block in `Handle()` (between existing checks 6 and the old 7, renumbered): if `spec.runtimeClass` is non-nil and non-empty AND the object lacks the annotation → `admission.Denied(...)` with an actionable message naming the annotation.

### Documentation updates

- `pkg/apis/llmsafespaces/v1/workspace_types.go:151-166` — replaced the "deferred to S51.2" CRD comment with the actual enforcement scheme (annotation name + which RBAC tier applies it).
- `charts/llmsafespaces/values.yaml:905-912` — expanded the `gvisor:` block comment to document the annotation and which RBAC tier can apply it.

---

## Key Decisions

1. **Annotation-gating, not RBAC-rule gating.** An annotation on the object is the simplest mechanism that fits the existing admission webhook pattern (the workspace webhook already runs on every CREATE/UPDATE). Alternatives: (a) a separate CRD like `RuntimeClassException` requiring admin RBAC to create — heavier and introduces a new object type; (b) OPA/Kyverno policy — adds a policy engine dependency. The annotation is the right-sized mechanism.
2. **Strict `"true"` value, not YAML-truthy.** Avoids the classic footgun where `"yes"`, `"on"`, `"1"` would all be misread as true. Matches the Turnstile config guard's value semantics.
3. **The controller path is unaffected.** The controller reads `workspace.Spec.RuntimeClass` to set the pod's `RuntimeClassName` directly (pod_builder.go:248-251) via its own ServiceAccount — it does not go through the workspace webhook. The webhook only governs the workspace CRD object itself.
4. **Threat model scope.** The admin-gating works for the threat model in scope (tenant API users without cluster RBAC). A tenant who somehow has cluster-admin RBAC is by definition an operator, which is exactly the tier the annotation scheme blesses. Out-of-scope: cluster-admin compromise.

---

## Assumptions stated and validated (Rule 7)

1. *Tenant API users never see kubectl.* Validated by reading the API auth flow — JWT auth → API → CRD writes via the API server's ServiceAccount, not the user's identity. Tenant API users have no k8s RBAC at all.
2. *The controller sets RuntimeClassName on pods, not workspaces.* Validated by reading `pod_builder.go:248-251` — the controller's pod creation uses its own ServiceAccount, not the workspace webhook path. The webhook only validates Workspace CRD writes.
3. *No existing test sets `spec.runtimeClass`.* Validated by `grep -rn "RuntimeClass" controller/internal/webhooks/` — no existing webhook test touches the field.
4. *The annotation value `"true"` is the only acceptable value.* Documented in `adminAllowsRuntimeClassOverride`'s strict equality; pinned by `TestWorkspaceWebhook_RuntimeClassAnnotationWrongValue`.

---

## Blockers

None.

---

## Tests Run

```
go test -timeout 60s -run "TestWorkspaceWebhook_RuntimeClass" ./controller/internal/webhooks/
→ ok  0.060s

go test -timeout 120s -short ./controller/internal/webhooks/...
→ ok  0.120s   (no regression in existing webhook tests)

go build ./...   → clean
gofmt / goimports  → clean
```

---

## Next Steps

1. Open this PR for review.
2. After approval + merge: JWT iss/aud, doc reconciliation, v0.3.0 release.

---

## Files Modified

- `controller/internal/webhooks/workspace_webhook.go` (new constant, new helper, new validation block, renumbered existing comments)
- `controller/internal/webhooks/workspace_runtimeclass_test.go` (new file — TDD test battery)
- `pkg/apis/llmsafespaces/v1/workspace_types.go` (CRD comment: replaced "deferred" with actual enforcement scheme)
- `charts/llmsafespaces/values.yaml` (gvisor block: documented the annotation + RBAC tier)
- `worklogs/NNNN_2026-07-11_runtime-class-webhook-gate.md` (this entry)

---

## Update — reviewer-found bug fix (CHANGES_REQUESTED on initial PR)

The AI reviewer on PR #518 found a critical bypass in the initial
implementation. My check was:

    if ws.Spec.RuntimeClass != nil && strings.TrimSpace(*ws.Spec.RuntimeClass) != "" {

The `strings.TrimSpace(...) != ""` guard exempted empty string from the
annotation check. But per `pod_builder.go:247` ("Empty string = runc")
and the existing `TestS51_1_PerWorkspaceOptOutEmpty`, an empty-string
value explicitly clears RuntimeClassName — falling through to the
kubelet default (runc). So a kubectl user could escape gVisor by
setting `spec.runtimeClass: ""` instead of `"runc"`, defeating the
entire purpose of the webhook.

### Fix

Removed the `TrimSpace != ""` guard. The check now fires for any
non-nil `Spec.RuntimeClass`, regardless of value. Error message calls
out the empty-string case explicitly so a kubectl user hitting the
guard understands why their empty value was rejected.

### New regression tests

- `TestWorkspaceWebhook_RuntimeClassEmptyStringRejectedWithoutAnnotation`
- `TestWorkspaceWebhook_RuntimeClassEmptyStringAllowedWithAnnotation`

### Documentation updates

- CRD comment (`workspace_types.go:151-173`): added a NOTE explaining
  the empty-string vs nil asymmetry and that both require the annotation.
- Chart `gvisor:` block: same clarification.

### Adversarial self-review (this round)

- *Is there any other value a kubectl user could set to escape gVisor?*
  No. The only ways to influence RuntimeClassName from spec are:
    (a) Set spec.runtimeClass to a non-empty value → caught.
    (b) Set spec.runtimeClass to empty string → now caught.
    (c) Omit spec.runtimeClass (nil) → falls through to
        DefaultRuntimeClass, which is operator-set. Secure default.
- *Could a kubectl user set a value that points to a different
  RuntimeClass like "kata-containers"?* Yes — but kata is also a
  sandbox runtime, and the operator controls what RuntimeClasses
  exist in the cluster. Out of scope for this gate.

### Tests Run

    go test -timeout 60s -run "TestWorkspaceWebhook_RuntimeClass" ./controller/internal/webhooks/
    → ok  0.038s
    go test -timeout 120s -short ./controller/internal/webhooks/...
    → ok  0.167s

