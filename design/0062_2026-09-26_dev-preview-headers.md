# 0062 — Dev-preview header configuration: a plain agentd MCP tool, literal values, injected at the agentd hop

**Status:** Proposed (2026-09-26; radically simplified 2026-09-28 by owner ruling) — design stage; implementation follows owner approval per the 0060/0061 convention.
**Date:** 2026-09-26 (r30 simplification: 2026-09-28)
**Issues:** #1583 (this design's charter) · builds on #1580/#1581 (the dev-preview flag projection + `feature_status` inventory).
**Binding rulings:** the owner's simplification ruling (2026-09-28): dev-preview headers become a plain agentd MCP tool — set/clear/list of header name+value, **literal values only**, stored plainly in agentd, injected at the preview forwarding hop. This revision DELETES the prior design's mint model, Secrets storage, write-only ceremony, owner-scoping, consent-flow integration, and the inject/forward mode split (§9 records what was cut and why). The prior review rounds' findings (r1's secretKeyRef exposure among them) are history: the entire API/CRD/Secret surface this ruling removes no longer exists to attack.

---

## 1. The problem, in half a page

The dev preview proxies browser requests through two hops (browser → API `HandleDevPreview` → agentd `:4097` `devPreviewHandler` → `localhost:<port>`). Both hops strip or forward only: the API hop's G34 allowlist keeps `{Content-Type, Accept, X-Request-ID}` (so the caller's credentials never reach untrusted in-pod code — correct, untouched), and the agentd hop forwards what the API sent minus the tunnel's own Basic `Authorization` (`cmd/workspace-agentd/dev_preview.go:87-89`). A previewed service that expects a header (a service API key, a test identity) has no way to receive one: the preview loads broken.

The fix is now the simplest thing that works: **the agent sets headers directly, as literals, through an agentd MCP tool; agentd injects them at its own forwarding hop.** The values never traverse the API, never enter any spec, CRD, Secret, or log outside the pod — the API boundary and G34 are untouched, and no platform surface changes at all.

## 2. The tool

One MCP tool on agentd's existing surface (the `feature_status` precedent from #1581):

```
dev_preview_headers { action: "set"   , name: "X-Service-Key", value: "abc123" }
dev_preview_headers { action: "clear" , name: "X-Service-Key" }        // or name:"*" for all
dev_preview_headers { action: "list"  }                                // → [{name, value}] (values are agent-owned literals; echoing them is fine)
```

Validation (tool-call time; a bad call is an MCP error — there is no later failure path):

| rule | failure |
|---|---|
| name canonical (`textproto.CanonicalMIMEHeaderKey`), printable, ≤128 chars | MCP error |
| reserved denylist: `Authorization` (the tunnel's own), `Cookie`, `Set-Cookie`, `Host`, `Connection`, `Upgrade`, hop-by-hop, `Sec-WebSocket-*` (all variants) | MCP error |
| value is a literal string, ≤4 KiB, valid header bytes | MCP error |
| ≤20 entries per workspace | MCP error |

**Literal-values-only is the entire security guard, and it is structural:** the tool has no reference resolution of any kind — there is no field that names a Secret, so no platform Secret (`jwt-secret`, `master-secret`, tenant passwords) is reachable by construction. The prior design needed a mint model to make Secret references safe; this design has no references to make safe.

## 3. Storage + injection

**Storage — plain in agentd:** a JSON file (`{name: value, …}`) in agentd's state directory (the `sessionstate` package's persistence precedent), mode 0600, written atomically on every mutation. It survives agentd restarts; pod deletion deletes it with everything else. Nothing is stored anywhere else: no CRD field, no API object, no Secret.

**Injection — the agentd hop, exactly:** in `devPreviewHandler`'s `Rewrite` (`dev_preview.go:82-97`), after the existing tunnel-credential strip:

```go
// Strip the agentd Basic auth credential — the dev server
// has no use for it and shouldn't see it.
r.Out.Header.Del("Authorization")                    // existing, :87-89

// 0062 §3: the workspace's configured preview headers — agent-set
// literals, last-writer before the localhost hop.
for name, value := range h.devPreviewHeaders() {     // in-memory map, loaded from the JSON at boot + on mutation
    r.Out.Header.Set(name, value)
}
```

Configured headers are last-writer over the API-allowlisted three and the forwarded `X-Forwarded-*` (the reserved denylist keeps them off the tunnel's own headers and the WS handshake machinery). Both topology modes (path and per-origin) funnel through this one handler — one injection point covers everything.

**Failure semantics: none.** There is no resolution step, no remote read, no cache — a stored literal IS the injected value. Misconfiguration is impossible by construction; a header entry cannot be "unresolvable." The 502-with-uniform-body machinery of the prior design deleted in full.

## 4. Threat model (the owner's argument)

The previewed terminal service is **the agent's own code** — the agent wrote it, runs it, and can already set arbitrary env vars, argv, and files for it; an identity or API-key header handed to that service is not sensitive beyond what the agent already holds. The dev preview's egress is the pod's own localhost. On this surface, header configuration is agent-owned tooling, not platform credential handling — so it is stored plainly, set plainly, and injected plainly, and the only structural guard that matters is the one that keeps it that way: **literals only, no Secret referencing** (§2).

What the design does NOT weaken: the API hop still terminates the authenticated tunnel and strips to G34's allowlist — header VALUES never traverse the API at all (they originate in the pod), the caller's credentials still never reach in-pod code, and the tunnel's Basic auth is still stripped before the localhost hop. A malicious previewed service could exfiltrate its own configured headers — to the agent that configured them; that boundary is unchanged from the prior design's D-class analysis.

## 5. `feature_status`

One entry, trivial now that tool and state share a process: `{"feature": "dev_preview_headers", "active": N>0, "source_detail": "N entries (agent-set literals in agentd state)", "controllable": true}` — no env projection needed (the #1581 basis question dissolves when the state is local).

## 6. Test strategy

- **Tool validation table** — every §2 rule (canonicalization, denylist, size caps, `*` clear).
- **Injection unit tests** — httptest through `devPreviewHandler`: configured headers present on the localhost-bound request, last-writer over allowlisted/forwarded; reserved names never overwrite the tunnel's or WS machinery; `Authorization` still stripped.
- **Persistence** — set → restart agentd → still injected; clear removes; `*` clears all; file mode 0600.
- **Literal-only pin** — the tool schema has NO field other than a literal `value` string (source-scan: no Secret reference, no env expansion, no file indirection).
- **E2E** — fixture service rejects requests without `X-Service-Key`; agent sets it via the tool → preview renders; clear → service rejects again.

## 7. Rollout

One PR in `cmd/workspace-agentd`: the tool, the JSON state, the Rewrite lines, the `feature_status` entry, the tests. No CRD, chart, API, SDK, or settings changes — the config's absence in old pods is simply "tool not found," which is correct.

## 8. Future note (one line)

Forward-mode (allowlisting edge headers like `X-Forwarded-User` at the API boundary) remains a possible future addition at the API hop; nothing here forecloses or builds it.

## 9. What this revision deleted (the sunk-cost ledger)

| deleted | why it existed | why it's gone |
|---|---|---|
| the mint model (§5.0: service-minted labeled Secrets, resolve-time checks) | made user-supplied `secretKeyRef` safe against the shared-namespace arbitrary-read | there are no references at all — literals only, excluded by construction |
| Secrets storage + delete-recreate rotation + no-cache resolution | kept values out of spec/pod per the credential-posture rule | the values are agent-owned and live only in the pod; no platform object holds them |
| write-only PUT ceremony + owner-scoped API endpoint | the writer was the owner via the API | the writer is the agent, in-pod, via MCP |
| uniform-502 failure semantics + gate-ordering pins | misconfiguration (unresolvable Secret) had to fail loud without an existence oracle | misconfiguration is impossible — no resolution step exists |
| consent-flow integration (#1582) + `feature_status` env projection | the config was owner-scope platform state the agent could only see via projected counts | the state is agent-owned and local; counts (and the tool itself) are one process away |
| inject/forward mode split + per-mode denylists + edge-trust analysis | two trust boundaries: platform-resolved values vs edge-authenticated names | one trust boundary: the agent's own literals (forward: §8's one-liner) |
| CRD fields, webhook validation, instance settings keys, DTO/SDK surface | platform configuration surface | zero platform surface remains |
