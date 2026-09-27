# 0062 — Dev-preview header configuration: inject + forward, values never in the pod

**Status:** Proposed (2026-09-26) — design stage; implementation follows owner approval per the 0060/0061 convention.
**Date:** 2026-09-26
**Issues:** #1583 (this design's charter) · builds on #1580/#1581 (the dev-preview flag projection + `feature_status` inventory) · future candidate for #1582's consent flow.
**Binding rulings:** the owner's approved framing (issue #1583, quoted below) — two header modes, server-side injection, values only in Secrets, owner-scoped changes, per-request/agent-supplied values explicitly OUT. This design does not re-litigate them; the rejected alternatives live in §9.

---

## 1. The problem, in one page

The dev preview proxies browser requests to an in-workspace dev server through two hops:

```
browser → API (HandleDevPreview, api/internal/handlers/dev_preview.go)
        → agentd :4097 (devPreviewHandler, cmd/workspace-agentd/dev_preview.go)
        → localhost:<port> (the service)
```

Today **no path exists for any header the service expects to reach it**:

- The API hop strips everything: G34's allowlist (`proxy_helpers.go:25` — `forwardedRequestHeaders = {Content-Type, Accept, X-Request-ID}`) exists precisely so the caller's `Cookie` (JWT), `Origin`, `Referer`, and `Authorization` never reach untrusted in-pod code. Correct — and it also drops everything else.
- The agentd hop forwards whatever the API sent, deleting only the tunnel's own Basic `Authorization` (`cmd/workspace-agentd/dev_preview.go:87`).

So a previewed service that requires an identity header (forward-auth setups: `X-Forwarded-User` from the platform edge) or a service API key has **no way to receive it** — the preview loads broken and the developer cannot tell configuration-absence from service-bug. This is #1580's silent-no-op shape, one layer down: the preview answers, the service rejects.

The design adds two header modes at the **API boundary** (the component that terminates the authenticated tunnel and already holds K8s Secret read access):

- **inject** — platform-resolved `secretKeyRef` entries: the API resolves the Secret and adds the header when forwarding. Values live only in K8s Secrets; the spec carries references; **never** literal values; **never** projected into the pod (the credential-posture rule — the agent must not read them).
- **forward** — an allowlist of incoming header NAMES passed through to the service (e.g. the edge's `X-Forwarded-User`), extending G34's fixed allowlist with owner-scoped config.

**Explicitly OUT (binding):** per-request or agent-supplied header values at runtime. The preview request path is agent-reachable (the MCP tool mints the URL); accepting agent-supplied values would hand the sandbox a credential-injection path into its own preview egress. The only writers are owner-scoped workspace settings.

---

## 2. The CRD shape + migration

### 2.1 v1 schema

`spec.networkAccess.devPreview` stays the boolean toggle — it is load-bearing (the API's 503 gate reads it via `ws.DevPreviewEnabled`; #1581 projects it into the pod; `feature_status` reports it). Headers nest as a **sibling list**, not inside the bool:

```go
// pkg/apis/llmsafespaces/v1/workspace_types.go
type WorkspaceNetworkAccess struct {
    // ... existing Egress/Ingress ...
    DevPreview bool `json:"devPreview,omitempty"` // unchanged — the toggle

    // DevPreviewHeaders configures header delivery for the dev-preview
    // proxy hop (design 0062). Inject entries are resolved server-side
    // from Secrets; forward entries allowlist incoming edge headers.
    // Values NEVER appear here — only Secret references.
    DevPreviewHeaders *WorkspaceDevPreviewHeaders `json:"devPreviewHeaders,omitempty"`
}

type WorkspaceDevPreviewHeaders struct {
    // Inject: the platform resolves each Secret and adds the header
    // on the API→agentd hop. Max entries: the instance cap
    // (devPreview.maxHeaderEntries, default 10).
    Inject []WorkspaceDevPreviewHeaderInject `json:"inject,omitempty"`

    // Forward: incoming header names passed through to the service
    // (G34's fixed allowlist plus these). Max names: the instance cap.
    Forward []string `json:"forward,omitempty"`
}

type WorkspaceDevPreviewHeaderInject struct {
    Name string `json:"name"`           // canonical header name
    // SecretKeyRef: the Secret must live in the workspace's namespace.
    SecretKeyRef corev1.SecretKeySelector `json:"secretKeyRef"` // name + key
}
```

### 2.2 Why sibling-not-nested (the migration answer)

Keeping the bool flat means **zero migration**: existing specs are already valid (`devPreview: true` with no `devPreviewHeaders` = today's behavior, byte-for-byte). The webhook validation (§6) governs the new field additively. Had we gone `devPreview: {enabled, headers}` the bool→object change would have required a conversion webhook across every stored spec — cost with no benefit, since the toggle's readers (#1581's projection, the API gate, the UI) all keep working untouched.

### 2.3 Storage/versioning

Plain v1 additive fields; no conversion. `+kubebuilder:validation:MaxItems` mirrors the instance cap at the schema level (defense against DB-written-past-API configs, the settings-registry clamping precedent).

---

## 3. The settings/API surface + authz

### 3.1 The write path (owner-scoped, like the toggle)

Extend the existing endpoint family — `PUT /api/v1/workspaces/:id/dev-preview` (router.go:1313, `SetDevPreview`) grows a sibling:

```
PUT /api/v1/workspaces/:id/dev-preview/headers
{ "inject": [{"name": "X-Service-Key", "secretKeyRef": {"name": "my-svc-key", "key": "key"}}],
  "forward": ["X-Forwarded-User"] }
```

- **Authz:** identical to the toggle — `AuthMiddleware` + `WorkspaceAccessMiddleware` (ownership) on `idGroup`; the service method re-checks ownership (`SetDevPreview` precedent, workspace_service.go:1903). No new role surface.
- **Validation** (§6 for the full rules): names canonicalized + reserved-denylisted; `secretKeyRef` namespace forced to the workspace's own; caps enforced; duplicates rejected.
- **DTO:** `DevPreviewHeaders` on the Workspace DTO mirrors `DevPreviewEnabled` (workspace_service.go:557 reads the CRD; the DTO is the API's honest mirror). Names and refs are public configuration; **Secret values never appear in any DTO**.
- SDK/OpenAPI follow the DTO mechanically (the Epic-66 endpoint family precedent).

### 3.2 The instance settings (operator knobs)

Two registry keys (the `pkg/settings/registry.go` devPreview block):

| key | default | role |
|---|---|---|
| `devPreview.maxHeaderEntries` | 10 | cap on inject entries per workspace |
| `devPreview.maxForwardedNames` | 10 | cap on forward names per workspace |

Read via `devPreviewConfigFromSettings` (dev_preview_wiring.go) into `DevPreviewConfig` — the established fail-to-typed-default pattern on read error.

### 3.3 The consent-flow future (#1582)

The write path being owner-scoped API today makes it a **natural first candidate** for #1582's agent-proposes/user-consents flow: the agent can already *read* the config surface via `feature_status` (§7); when #1582 lands, the PUT becomes the API-executes arm with zero new CRD surface. No consent machinery is built here — recorded so the shape doesn't foreclose it (the PUT takes the same body the consent prompt would render).

---

## 4. The injection point + the two topology modes

**Where:** `HandleDevPreview`'s director (dev_preview.go:250-275), after G34's allowlist copy and after the proxy sets its own transport headers (`Authorization` Basic, `X-Forwarded-For`, `X-Forwarded-Host/Proto`) — injection is LAST, so configuration can never be shadowed by caller input and caller input can never be confused with injection:

```go
// (existing) r.Out.Header = http.Header{}; copyRequestHeaders(...); Set("Authorization", basic); Set("X-Forwarded-For", ...)
// (existing) X-Forwarded-Host/Proto ...

// 0062 §4: forward — G34's fixed set plus the workspace's allowlist.
copyForwardedHeaders(r.In.Header, r.Out.Header, cfg.ForwardNames)  // same shape as copyRequestHeaders with a per-workspace extension

// 0062 §4: inject — server-side resolution, ordered, last-writer.
for _, entry := range cfg.InjectEntries {
    v, err := resolveHeaderSecret(ctx, entry)   // §5
    if err != nil { /* §5's loud path */ }
    r.Out.Header.Set(entry.Name, v)
}
```

**Both topology modes funnel here.** Path mode (`/dev-preview/:port/*`) is this handler directly. Origin mode's bootstrap hop (`/dev-preview-bootstrap/:port`) is a redirect+cookie step — **no injection there** (nothing is proxied to the service); the subsequent per-workspace-origin request lands on this same handler. One injection point covers both modes; nothing mode-specific exists in the header design.

**The agentd hop is untouched.** It already forwards non-`Authorization` headers to the service; injected and forwarded headers ride the authenticated tunnel like the existing allowlisted three. The pod sees header VALUES in proxied requests — unavoidable (the service must receive them) and explicitly accepted by the framing: the rule the design enforces is that values never live in **spec, env, or API responses** — the pod's process tree still cannot *read* them, only the previewed service receives them per-request. (The terminal-probe caveat: a malicious previewed service exfiltrates its own injected headers — that service is the developer's own code, in their own workspace, receiving their own configured credentials; the D-class adversary boundary is unchanged.)

**Gate ordering (the 503 interaction):** the existing gates run FIRST and unchanged — kill-switch (503), per-space disabled (503 via the CRD flag; the agentd tool additionally fails loud pre-URL per #1581), port denylist, conn caps. Header config resolution happens only for a request that already passed every gate: a disabled space never touches a Secret; a misconfigured header entry never masks the enable/disable semantics.

---

## 5. Secret lifecycle

### 5.1 Resolution

- **Per-request `Get`** on the workspace namespace (the `relay_handoff.go:59` precedent — the API's clientset reads Secrets there). **No cache in v1**: at `maxConnsPerWorkspace=50` the apiserver load is bounded and rotation becomes **instant** (the next request sees the new value) — the simplest correct semantics win.
- **RBAC:** the API's existing Secrets read in its namespace (app.go:1242 / relay-handoff usage); no new grants.

### 5.2 Failure semantics (loud, per the house convention)

A configured entry whose Secret/key is missing or unreadable is **misconfiguration, not degradation**: the request fails **502** with a reason body naming the entry and the Secret — `{"error":"dev-preview header configuration unavailable","reason":"header X-Service-Key: secret my-svc-key/key not found"}` — and a WARN log line. Skipping the entry would produce a confusing broken preview indistinguishable from a service bug (#1580's whole lesson); the toggle-level gates stay 503-first so the two failure classes never alias.

### 5.3 Rotation + deletion

- **Rotation:** write the new Secret value; the next request picks it up (no cache, §5.1). No signal needed.
- **Deletion:** the Secret's absence turns every preview request into the §5.2 502 naming it. The owner removes the entry (PUT) to restore. Optionally surfaced later via a CRD condition — deferred (§9): the 502 + log already name the Secret.
- **Ownership:** the Secret is owner-managed (like the workspace password Secret); the platform never writes or garbage-collects it. `secretKeyRef` without a matching Secret is caught at write time opportunistically (a read-only existence check in the PUT handler; a TOCTOU window remains by design — §5.2 is the runtime backstop) — warn-not-reject at PUT time if unreadable, since Secret and workspace config may be created in either order.

---

## 6. Validation rules (webhook + service layer)

| rule | where | failure |
|---|---|---|
| header name canonical (`textproto.CanonicalMIMEHeaderKey`), printable, ≤ 128 chars | webhook + PUT | 422 |
| **reserved denylist**: `Authorization, Cookie, Host, Proxy-Authorization, Connection, Upgrade, X-Forwarded-For, X-Forwarded-Host, X-Forwarded-Proto, X-Forwarded-User*` (*see below), + all hop-by-hop | webhook + PUT | 422 |
| inject `secretKeyRef.name/key` well-formed; namespace forced to the workspace's (cross-namespace refs rejected) | webhook + PUT | 422 |
| no duplicate header names within inject; no name in both inject and forward | webhook + PUT | 422 |
| entry counts ≤ instance caps | PUT (service-clamped like the settings knobs) + webhook MaxItems | 422 |
| `devPreview: false` + headers present → VALID but inert (the gates run first, §4) | — | — |

The reserved denylist exists because injection runs last (§4): a configured `Authorization` or `X-Forwarded-For` would overwrite the tunnel's own transport headers — either breaking the tunnel or laundering a spoofed transport descriptor past G34. `X-Forwarded-User` is special: it is in the **forward** denylist's trust boundary, not inject's — see §6.1.

### 6.1 The forward trust boundary

Forwarded headers come from the request as the API receives it — i.e., from the platform edge (the same source the API already trusts for the JWT cookie). Allowing `X-Forwarded-User` through forward is therefore sound **iff** the edge actually strips client-supplied copies (the edge-auth deployment's property; the API cannot verify it per-request). The design's stance: **forward may name `X-Forwarded-User`** (the primary use case) **and the reserved denylist for forward excludes only the transport/credential set the proxy itself owns** (`Authorization, Cookie, Host, hop-by-hop, X-Forwarded-For/Host/Proto`); the doc records the edge-trust assumption explicitly, and the operator doc note for deployers without a sanitizing edge is: don't forward identity headers. No per-request verification machinery (§9).

---

## 7. feature_status + the pod boundary invariant

`feature_status` (#1581) grows one entry — closing the loop between owner config and agent inspection. **The basis question first:** the tool runs in-pod and the pod has no API credentials (the D3 posture), so the DTO is unreachable — whatever the entry reports must project through the controller's env surface, exactly like `WORKSPACE_DEV_PREVIEW_ENABLED` (#1581). What is SAFE to project: counts, and counts only. A count pair (`2 inject entries, 1 forwarded name`) is configuration metadata — it carries no header names (which the agent has no need for), no Secret references, and self-evidently no values. The invariant this design actually enforces is **values never in spec-responses-or-pod; names and refs never in the pod; counts are fine**:

```json
{"feature": "dev_preview_headers", "active": <bool>, "source": "space",
 "source_detail": "spec.networkAccess.devPreviewHeaders via WORKSPACE_DEV_PREVIEW_HEADERS=N/M (counts only; names/refs/values never project)",
 "controllable": false}
```

The controller writes `WORKSPACE_DEV_PREVIEW_HEADERS=N/M` beside the existing boolean projection (#1581's pattern, the same both-containers rule); `active` = N+M > 0; the truthful-basis discipline (#1581 r1/r2's lesson) holds — the entry reports only what the env authentically carries.

---

## 8. Test strategy (the implementation PR's contract)

- **Webhook/service validation table** — every §6 rule, both layers.
- **Director unit tests** — httptest through `HandleDevPreview` with a fake Secret getter: injected headers present on the agentd-bound request (ordered, last-writer); forwarded names pass and non-allowlisted stay stripped; reserved names never overwrite the tunnel's own; G34's original three unaffected.
- **Failure semantics** — missing Secret/key → 502 with the naming body; the kill-switch and disabled-flag gates still return 503 **before** any Secret read (ordering pin).
- **Migration/compat** — a spec with `devPreview: true` and no headers renders byte-identical proxy behavior; round-trip through the PUT with empty body clears config.
- **The pod-boundary pin** — the ONLY new pod projection is the count pair `WORKSPACE_DEV_PREVIEW_HEADERS=N/M` (§7); a source-scan pin asserts no header NAME, no Secret name/key, and no value string ever reaches pod env (the #1581 projection inventory stays the complete list).
- **E2E arm** — the dev-preview tunnel e2e seeds a header-demanding fixture service (rejects requests without `X-Service-Key`, echoes `X-Forwarded-User`); inject+forward configured → the preview renders; Secret deleted → the 502 with the named Secret (§5.2's loud path, full-stack).

## 9. Rejected alternatives (with reasons)

| alternative | why rejected |
|---|---|
| `devPreview: {enabled, headers}` structured bool | forces a stored-spec conversion webhook across every workspace for zero reader benefit (§2.2) |
| injection in agentd (in-pod) | the pod must never hold Secret read access — the credential-posture rule; the API boundary already has it (§1) |
| per-request/agent-supplied values | binding OUT ruling: agent-reachable path + credential injection = sandbox egress laundering (§1) |
| skip-on-missing-Secret + warning header | silent-degradation class (#1580's lesson); the preview would break confusingly (§5.2) |
| TTL cache on Secret reads | rotation semantics get a window; the no-cache load is already bounded by maxConns (§5.1) |
| per-port header scoping (`ports: []int` matcher) | **deferred, not rejected** — v1 is workspace-scoped; the additive path (a matcher field later) is clean, and no use case in the issue needs it (multi-service previews are the future that would) |
| CRD condition for missing Secrets | the 502 + WARN already name the Secret; a condition is operator-polish for later |
| per-request edge-trust verification for forwarded identity headers | unverifiable at the API; the edge-trust assumption is documented (§6.1) and the operator doc carries the warning |
| secretKeyRef existence REJECTED-hard at PUT | creation-order coupling (Secret may legitimately land after the config); warn-not-reject (§5.3) |

## 10. Rollout

1. CRD fields + webhook validation (additive; chart CRD refresh).
2. Instance settings keys + `DevPreviewConfig` extension.
3. The PUT/DTO/service layer + validation table.
4. The director: forward allowlist extension, inject + §5.2 failure path, gate-ordering pins.
5. `feature_status` entry (§7, corrected basis).
6. The e2e arm (§8).
7. Docs: the operator note (§6.1's edge-trust caveat), the workspace-settings UI copy (names + refs, never values).

Each step lands green independently; nothing is feature-flagged (the config's absence IS the flag).
