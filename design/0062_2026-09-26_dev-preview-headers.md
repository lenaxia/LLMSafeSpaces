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

`spec.networkAccess.devPreview` stays the boolean toggle — it is load-bearing (the API's 503 gate reads it off the typed CRD, `workspace.Spec.NetworkAccess.DevPreview`, dev_preview.go:194-197; the REST DTO mirrors it as `DevPreviewEnabled` at workspace_service.go:556-558; #1581 projects it into the pod; `feature_status` reports it). Headers nest as a **sibling list**, not inside the bool:

```go
// pkg/apis/llmsafespaces/v1/workspace_types.go
type WorkspaceNetworkAccess struct {
    // ... existing Egress/Ingress ...
    DevPreview bool `json:"devPreview,omitempty"` // unchanged — the toggle

    // DevPreviewHeaders configures header delivery for the dev-preview
    // proxy hop (design 0062). Inject entries are resolved server-side
    // from PLATFORM-MINTED Secrets (§5.0 — user-supplied references do
    // not exist); forward entries allowlist incoming edge headers.
    // Values NEVER appear here — only minted Secret references.
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
    Name string `json:"name"` // canonical header name

    // SecretKeyRef points at the PLATFORM-MINTED Secret (§5.0). It is
    // written by the settings service, never by the client: the PUT
    // carries the VALUE transiently (write-only), the service stores
    // it in a labeled, owner-referenced Secret, and the spec carries
    // only the minted reference. corev1.SecretKeySelector has NO
    // namespace field (a LocalObjectReference) — resolution is
    // namespace-locked to the API's workspace namespace by the
    // resolver itself (§5.1), and the resolve-time ownership check
    // (§5.0) makes a forged reference inert.
    SecretKeyRef corev1.SecretKeySelector `json:"secretKeyRef"` // minted name + key
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
{ "inject": [{"name": "X-Service-Key", "value": "<write-only>"}],
  "forward": ["X-Forwarded-User"] }
```

The request's `value` field is **write-only**: the service mints/updates the platform-owned Secret (§5.0) and the response, DTO, spec, and logs carry only `{name, secretKeyRef}` — values are never echoed (the platform's standard credential-handling posture: transient over the authenticated API, at rest only in Secrets).

- **Authz:** identical to the toggle — `AuthMiddleware` + `WorkspaceAccessMiddleware` (ownership) on `idGroup`; the service method re-checks ownership (`SetDevPreview` precedent, api/internal/services/workspace/workspace_service.go:1906; the endpoint at api/internal/server/router.go:1313). No new role surface.
- **Validation** (§6 for the full rules): names canonicalized + reserved-denylisted (the inject set for inject, the forward subset for forward); a `secretKeyRef` field in the REQUEST is schema-rejected outright (the mint model — §5.0; the spec's field is service-written only); caps enforced; duplicates rejected.
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

**Where:** `HandleDevPreview`'s director — the `Rewrite` func spanning dev_preview.go:248-307 (through the P0-2 WebSocket re-establishment block at :296-306). Ordering, pinned exactly: G34's allowlist copy → the tunnel's own transport headers (`Authorization` Basic, `X-Forwarded-For`, `X-Forwarded-Host/Proto) → **forward** (the allowlisted extension) → **inject** → the P0-2 WS re-establishment LAST. Injection is after the transport headers (configuration can never be shadowed by caller input; `Sec-WebSocket-*` is on the inject denylist so it cannot clobber a handshake) but BEFORE the WS block, so the WS descriptors remain the last writer for upgraded connections — non-WS requests are unaffected by the block and injection is effectively last-writer for them:

```go
// (existing) r.Out.Header = http.Header{}; copyRequestHeaders(...); Set("Authorization", basic); Set("X-Forwarded-For", ...)
// (existing) X-Forwarded-Host/Proto ...

// 0062 §4: forward — G34's fixed set plus the workspace's allowlist.
copyForwardedHeaders(r.In.Header, r.Out.Header, cfg.ForwardNames)  // same shape as copyRequestHeaders with a per-workspace extension

// 0062 §4/F3: injection consumes PRE-RESOLVED values. ReverseProxy's
// Rewrite has no error return — resolution happens in the HANDLER,
// after every gate, before proxy.ServeHTTP; its failure is §5.2's
// JSON 502 emitted there. The director only Sets:
for _, h := range resolvedInject {   // []{Name, Value} — handler-resolved
    r.Out.Header.Set(h.Name, h.Value)
}
```

**The mechanics (r2/F3, pinned):** `Rewrite func(*ProxyRequest)` cannot return errors, so §5.2's loud 502 is emitted at HANDLER level — the resolution loop runs after all gates and before `proxy.ServeHTTP`, writes `resolvedInject`, and on any failure writes the 502 and returns without proxying. The director (still inside `Rewrite`) only applies the resolved values. The ordering invariant is unchanged and now testable: **Secret reads happen strictly after every 400/503/429 gate, and no Secret is read for a request that will not be proxied.**

**Both topology modes funnel here.** Path mode (`/dev-preview/:port/*`) is this handler directly. Origin mode's bootstrap hop (`/dev-preview-bootstrap/:port`) is a redirect carrying a one-time signed token in the query string (preview_origin.go:612-614) — **no injection there** (nothing is proxied to the service); the subsequent per-workspace-origin request lands on this same handler. One injection point covers both modes; nothing mode-specific exists in the header design.

**The agentd hop is untouched.** It already forwards non-`Authorization` headers to the service; injected and forwarded headers ride the authenticated tunnel like the existing allowlisted three. The pod sees header VALUES in proxied requests — unavoidable (the service must receive them) and explicitly accepted by the framing: the rule the design enforces is that values never live in **spec, env, or API responses** — the pod's process tree still cannot *read* them, only the previewed service receives them per-request. (The terminal-probe caveat: a malicious previewed service exfiltrates its own injected headers — that service is the developer's own code, in their own workspace, receiving their own configured credentials; the D-class adversary boundary is unchanged.)

**Gate ordering (the 503 interaction), the REAL sequence** (r2 correction — the handler's actual order at dev_preview.go:140-212): port parse + denylist **400** → phase/PodIP **503** → per-space flag **503** → conn-cap **429** → *(r2/F3: handler-level header resolution — §5.2's 502 on failure)* → proxying. Header resolution happens only for a request that already passed every gate: a disabled space never touches a Secret; a misconfigured header entry never masks the enable/disable semantics; the kill-switch (config-level, `devPreviewConfigFromSettings`) precedes the handler entirely.

---

## 5. Secret lifecycle

### 5.0 Who may be referenced — the mint model (the r1 security ruling)

The design's first cut allowed **user-supplied `secretKeyRef`** — any Secret in the namespace. The design review's critical finding (r1) is recorded here because it is the load-bearing constraint: tenancy is a **single shared namespace** housing the platform credentials (`master-secret` — the KEK root of trust, `jwt-secret`, `postgres/redis-passwords`, `internal-token`) and every tenant's `workspace-pw-<name>`; the API already holds unscoped `secrets:[get,...]` there; so secretKeyRef-by-name is a **silent arbitrary-Secret-read primitive** — one PUT (`inject: [{name: "X-Cred", secretKeyRef: {name: "jwt-secret", key:"..."}}]`) would deliver the platform's signing key to the caller's own preview and browser. RBAC cannot repair it (`resourceNames` cannot express per-workspace prefixes — the chart documents this itself).

**The mint model closes it structurally:**

- **No user-supplied references exist.** The PUT carries the header VALUE (write-only, §3); the service writes it to a Secret IT mints:
  - name: `dev-preview-hdr-<UUID>-<sha8(headerName)>` where **UUID is the Workspace's `ObjectMeta.Name` — the 36-char CRD identity, NOT the human display name** (which is non-unique, mutable via rename, unvalidated for length/charset, and would open a same-name collision channel between workspaces). 36 + 17 + 8 fits the 253-char Secret-name budget; the UUID's `[0-9a-f-]` charset is label-safe.
  - labels: `llmsafespaces.dev/dev-preview-header: "true"`, `llmsafespaces.dev/workspace: <UUID>`;
  - `ownerReferences` → the Workspace object (k8s-native GC on workspace deletion).
  - sha8 truncation collisions between two headers of the SAME workspace are detected at PUT time (name clash on distinct header names → 422 suggesting the operator cap catch it first; astronomically unlikely, but the check is one line).
- **Resolve-time enforcement (defense-in-depth):** before reading, the resolver verifies BOTH the naming convention AND the `llmsafespaces.dev/workspace` label equal THIS workspace's UUID — a forged spec (raw kubectl apply, DB edit) referencing any platform or foreign Secret fails the check and takes §5.2's loud 502, the value never read. The primitive is unreachable even with spec write access.
- **Platform ownership:** these Secrets are platform-managed end-to-end (created, rotated, deleted by the settings service); §5.3's earlier "owner-managed" wording is superseded — the owner manages the CONFIGURATION; the platform manages the ARTIFACTS.

### 5.1 Resolution

- **Per-request `Get`** on the workspace namespace (the `relay_handoff.go:59` precedent — the API's clientset reads Secrets there). **No cache in v1**: at `maxConnsPerWorkspace=50` the apiserver load is bounded and rotation becomes **instant** (the next request sees the new value) — the simplest correct semantics win.
- **RBAC:** the API's existing grants suffice — `secrets: [get, list, watch, delete]` + unscoped `create` (helm/templates/rbac.yaml:364-367). The chart deliberately grants `update`/`patch` ONLY on three named Secrets (`:389-395`) and documents that `resourceNames` cannot express per-workspace prefixes — so **rotation is DELETE + RECREATE** (both verbs held broadly), never in-place update. Under the mint model (§5.0) the reach of the grant is also irrelevant to header injection: only service-minted, label-checked Secrets are ever resolved.

### 5.2 Failure semantics (loud, per the house convention)

A configured entry whose Secret/key is missing or unreadable is **misconfiguration, not degradation**: the request fails **502** with a reason body naming the entry and the Secret — `{"error":"dev-preview header configuration unavailable","reason":"header X-Service-Key: secret dev-preview-hdr-3f1c9a2e-<...>/key not found (mint-model check: not a platform-minted Secret for this workspace)"}` — and a WARN log line. Skipping the entry would produce a confusing broken preview indistinguishable from a service bug (#1580's whole lesson); the toggle-level gates stay 503-first so the two failure classes never alias.

### 5.3 Rotation + deletion (the mint model's semantics)

- **Rotation:** PUT the config again with the new value — the service **deletes and recreates** the deterministic-named Secret (§5.1's RBAC pin: the API holds no `update` grant, and the chart documents why none is coming); the next request resolves the new object (no cache). The delete+recreate window is a per-request `Get` gap of milliseconds — a preview request inside it takes §5.2's 502, self-healing on retry.
- **Deletion:** removing an entry from the PUT DELETES the minted Secret (the platform owns the artifact); workspace deletion garbage-collects all of them via `ownerReferences` (§5.0) — no finalizer work. An entry whose Secret is nonetheless missing (manual deletion) takes §5.2's 502 naming it.
- **Out-of-band platform Secrets are structurally unreachable** — there is no flow that mints a reference to anything not created by this service, and the resolve-time check backstops forged specs (§5.0).

---

## 6. Validation rules (webhook + service layer)

| rule | where | failure |
|---|---|---|
| header name canonical (`textproto.CanonicalMIMEHeaderKey`), printable, ≤ 128 chars | webhook + PUT | 422 |
| **inject reserved denylist**: `Authorization, Proxy-Authorization, Cookie, Set-Cookie, Host, Connection, Upgrade, X-Forwarded-For, X-Forwarded-Host, X-Forwarded-Proto, Sec-WebSocket-*` (all variants), + all hop-by-hop | webhook + PUT | 422 |
| **forward reserved denylist** (a STRICT SUBSET — identity headers are forward's whole point): `Authorization, Cookie, Host, Proxy-Authorization, Connection, Upgrade, X-Forwarded-For, X-Forwarded-Host, X-Forwarded-Proto, Sec-WebSocket-*`, + hop-by-hop. **`X-Forwarded-User` is ALLOWED for forward** (§6.1) and denied for inject | webhook + PUT | 422 |
| inject `secretKeyRef` (the SPEC field) well-formed and mint-shaped (§5.0) — written only by the service; the resolver enforces mint-name + workspace-label at runtime (forged specs → 502, §5.0) | webhook (form) + resolve (ownership) | 422 / 502 |
| no duplicate header names within inject; no name in both inject and forward | webhook + PUT | 422 |
| entry counts ≤ instance caps | PUT (service-clamped like the settings knobs) + webhook MaxItems | 422 |
| `devPreview: false` + headers present → VALID but inert (the gates run first, §4) | — | — |

**Namespace reconciliation:** `corev1.SecretKeySelector` embeds `LocalObjectReference` — there IS no namespace field to validate. The namespace is locked by the RESOLVER (it reads only the workspace namespace, §5.1) and the mint model (§5.0) makes the question moot for references. The two denylists exist because injection and forward have different trust boundaries: inject runs late and server-side (spoofing a transport/credential header would overwrite the tunnel's own — §4), while forward's names describe what the EDGE already authenticated (§6.1).

### 6.1 The forward trust boundary

Forwarded headers come from the request as the API receives it — i.e., from the platform edge (the same source the API already trusts for the JWT cookie). Allowing `X-Forwarded-User` through forward is therefore sound **iff** the edge actually strips client-supplied copies (the edge-auth deployment's property; the API cannot verify it per-request). The design's stance: **forward may name `X-Forwarded-User`** (the primary use case — §6's forward denylist is the strict subset excluding it) while inject may not (an injected identity header would be a platform-forged identity); the doc records the edge-trust assumption explicitly, and the operator doc note for deployers without a sanitizing edge is: don't forward identity headers. No per-request verification machinery (§9).

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
- **Failure semantics** — missing minted Secret/key → 502 with the naming body; the kill-switch and disabled-flag gates still return 503 **before** any Secret read (ordering pin).
- **The mint-model security pins (r1's critical finding, closed)** — the PUT never accepts a `secretKeyRef` field (schema-level reject); a FORGED spec (raw CRD edit) referencing `jwt-secret`/`master-secret`/`workspace-pw-<other>` fails the resolve-time mint-name + workspace-label check → 502, value never read; the minted Secret carries ownerReferences→Workspace and both labels; workspace deletion GCs them; PUT-without-echo (the response never contains `value`).
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
| **user-supplied `secretKeyRef` (the r0 shape)** | **REJECTED by the r1 security review — the critical finding**: in the shared namespace with the API's unscoped Secret reads, it is a one-PUT arbitrary-Secret-read primitive (jwt-secret/master-KEK/other tenants' passwords delivered to the caller's own preview); the mint model (§5.0) replaces it — RBAC cannot express the needed per-workspace scoping (`resourceNames` limitation, the chart's own docs) |
| routing header values through the encrypted-secrets service (reviewer option 3) | viable but heavier than v1 needs — plain labeled+owner-referenced Secrets with the resolve-time check are structurally sufficient; the service remains the right home if header values ever need KEK encryption at rest (recorded, not closed) |
| per-port header scoping (`ports: []int` matcher) | **deferred, not rejected** — v1 is workspace-scoped; the additive path (a matcher field later) is clean, and no use case in the issue needs it (multi-service previews are the future that would) |
| CRD condition for missing Secrets | the 502 + WARN already name the Secret; a condition is operator-polish for later |
| per-request edge-trust verification for forwarded identity headers | unverifiable at the API; the edge-trust assumption is documented (§6.1) and the operator doc carries the warning |
| secretKeyRef existence REJECTED-hard at PUT | obsolete under the mint model (the service mints the Secret in the same PUT — there is no creation-order coupling to tolerate) |

## 10. Rollout

1. CRD fields + webhook validation (additive; chart CRD refresh).
2. Instance settings keys + `DevPreviewConfig` extension.
3. The PUT/DTO/service layer + validation table.
4. The director: forward allowlist extension, inject + §5.2 failure path, gate-ordering pins.
5. `feature_status` entry (§7, corrected basis).
6. The e2e arm (§8).
7. Docs: the operator note (§6.1's edge-trust caveat), the workspace-settings UI copy (names + refs, never values).

Each step lands green independently; nothing is feature-flagged (the config's absence IS the flag).
