# Epic 66 Field Findings & Preview Redesign (2026-08-19)

**Origin:** black-box testing of the *deployed* dev-preview tunnel from
inside a live workspace (the environment the epic targets), followed by
threat modeling with live probes. No platform source was read; every claim
cites a dated, reproducible observation.

**Read order:**

| Doc | What it is |
|---|---|
| `REGRESSION.md` | Verified behavior matrix of the deployed tunnel — CSP (verbatim policy), WS-upgrade stripping, stale caching, unenforced rate headers, SSE pass-through, transfer baselines. The ground truth the rest builds on. |
| `DESIGN.md` | Proposed redesign: per-workspace origins (`<ws>-preview.safespaces.dev`, single-label — rides Universal SSL, no ACM), signed-bootstrap auth, relaxed CSP, port policy. Four-phase migration. |
| `THREAT-MODEL.md` | 17 threats, prompt-injection-first framing. 5 user-side tests resolved 2026-08-19 (T2 cookie audit, T7 auth+ownership, T3 proxy blocklist, T11 credential exposure). Top open: T1 (same-origin credentialed API calls from preview JS) — fixed by the redesign, not patchable in place. |
| `TICKETS.md` | 12 phased items with verification hooks. Phase 0 (no-store, WS forwarding, cookie hygiene, CF rewriters off) is independently shippable. |
| `ACCEPTANCE.md` | Per-phase runbook: exact commands + expected outputs. Self-service exit criteria per phase. |
| `harness/` | The self-reporting test pages (`csp-test`, `ws-test`, `tier3-test`, `stress`) + servers (`serve_stress.py`: no-store static + WS echo + SSE drip + byte generator; `ws_client_test.py`). These double as the acceptance harness per ACCEPTANCE.md. |

**Notable deltas vs. the shipped implementation** (relevant to the epic's
own design intent, README "Scope": *WS support is mandatory for HMR*):

1. WebSocket upgrades are stripped by the deployed chain — HMR is broken in
   production today (evidence: `ws-test` + server log, REGRESSION.md).
2. The strict CSP that shipped blocks inline scripts/styles/eval —
   Next.js App Router and CDN-based prototyping cannot run through the
   tunnel.
3. HTML caching through the chain served stale pages across app changes.
4. The `x-ratelimit-*` headers are not enforced on the proxy path
   (127-request burst → zero 429s, counter frozen).

All four are addressed by Phase 0 + Phase 1 of the redesign; none require
the origin migration except the CSP relaxation's safety margin.
