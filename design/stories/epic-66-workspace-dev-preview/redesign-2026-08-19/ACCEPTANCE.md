# Dev Preview Redesign — Acceptance Runbook

**Version:** 2026-08-19 · companion to DESIGN.md / TICKETS.md / THREAT-MODEL.md
**Purpose:** per-phase verification with exact commands and expected outputs.
No context from the discovery session is required to use this.

## 0. Conventions

- `<WS>` = workspace UUID (example: `42ae0489-8d54-42a3-af62-163e50da84e8`)
- Old (path-based) URL: `https://api.safespaces.dev/api/v1/workspaces/<WS>/dev-preview/<PORT>/...`
- New (per-origin) URL: `https://<WS>-preview.safespaces.dev/<PORT>/...`
- Browser checks are canonical for JS-behavior tests (auth automatic);
  curl for headers/bodies. To script curl checks you need the session cookie:
  DevTools → Application → Cookies → `lsp_session` value, then

  ```bash
  export PV="Cookie: lsp_session=<VALUE>"
  curl -s --compressed -H "$PV" <URL>
  ```

  Never commit/paste that value into tickets.
- All test pages live in `harness/ (repo: design/stories/epic-66-workspace-dev-preview/redesign-2026-08-19/harness/)` and are
  **self-reporting**: load → read PASS/FAIL lines.

## 1. Harness setup (test server)

Run inside the workspace (survives shell restarts; NOT container
reschedules — rerun after any pod restart):

```bash
setsid nohup python3 harness/ (repo: design/stories/epic-66-workspace-dev-preview/redesign-2026-08-19/harness/)serve_stress.py \
  >> harness/ (repo: design/stories/epic-66-workspace-dev-preview/redesign-2026-08-19/harness/)server.log 2>&1 < /dev/null &
```

Local sanity (inside the pod):

```bash
curl -s -o /dev/null -w "%{http_code}\n" http://127.0.0.1:5173/        # 200
python3 harness/ (repo: design/stories/epic-66-workspace-dev-preview/redesign-2026-08-19/harness/)ws_client_test.py                  # LOCAL WS TEST: PASS
```

Also start a plain server WITHOUT cache headers (needed for check A1):

```bash
cd harness/ && setsid nohup python3 -m http.server 5174 \
  >> server5174.log 2>&1 < /dev/null &
```

| Page | Verifies |
|---|---|
| `csp-test.html` | inline `<script>` / external file / inline handler |
| `ws-test.html` | WebSocket upgrade + echo through the tunnel |
| `tier3-test.html` | inline `<style>`, `style=` attr, CSSOM, eval/new Function, JSON data block, CDN script, same/cross-origin fetch |
| `stress.html` | SSE streaming (phase 1), 2 MB body (phase 2), request waves + rate behavior (phases 3–5, click-gated) |

---

## 2. Phase 0 — fixes on current URLs

### A1 · P0-1 no-store on HTML (edge-injected)

Must use the plain server on :5174 — `serve_stress.py` already sends
`no-store` itself, which would mask whether the EDGE adds it.

```bash
curl -sI --compressed -H "$PV" \
  https://api.safespaces.dev/api/v1/workspaces/<WS>/dev-preview/5174/stress.html \
  | grep -i cache-control
```

- **PASS:** `cache-control: no-store` (or stronger) on `text/html`.
- Also check a non-HTML asset on :5173 (`/stress.css`): app-set caching
  behavior must be preserved (no forced no-store on CSS/JS).
- **FAIL:** header absent on HTML, or no-store forced on hashed assets.

### A2 · P0-2 WebSocket forwarding

1. Load old URL `.../dev-preview/5173/ws-test.html` in browser.
   - **PASS:** `PASS — server echoed the message back through the tunnel`
2. In-pod confirmation:

   ```bash
   grep "WS OPEN" harness/ (repo: design/stories/epic-66-workspace-dev-preview/redesign-2026-08-19/harness/)server.log | tail -1   # entry must exist for the browser test
   grep '"GET /ws' harness/ (repo: design/stories/epic-66-workspace-dev-preview/redesign-2026-08-19/harness/)server.log | tail -1  # must NOT be the latest WS-related line
   ```

3. CSP cleanup: `curl -sI -H "$PV" <old URL>` → the `content-security-policy`
   header must **no longer contain bare `wss:`** (should read
   `connect-src 'self'` or equivalent).

### A3 · P0-3 cookie hardening

Browser DevTools → Application → Cookies after the release:
- During dual-accept: both `lsp_session` and `__Host-lsp_session` present.
- After retirement: only `__Host-*`; HttpOnly ✓ Secure ✓; no Domain
  attribute (the `__Host-` prefix enforces this); SameSite=Lax.
- Regression: platform login/logout and preview loads still work.

### A4 · P0-4 Cloudflare rewriters off (byte identity)

```bash
curl -s --compressed -H "$PV" <old URL>/dev-preview/5173/stress.html -o /tmp/t.html
curl -s http://127.0.0.1:5173/stress.html -o /tmp/l.html
diff /tmp/t.html /tmp/l.html && echo BYTE-IDENTICAL
```

- **PASS:** `BYTE-IDENTICAL`, and `grep -c beacon.min.js /tmp/t.html` → `0`.
- Headers may differ; the BODY must not.

**Phase 0 exit:** A1–A4 all PASS; `ws-test.html` PASS; regression table rows
1–3 behave per DESIGN §8.

---

## 3. Phase 1 — per-workspace origin (flagged)

### B1 · P1-1 host routing

```bash
curl -s -o /dev/null -w "%{http_code}\n" -H "$PV" https://<WS>-preview.safespaces.dev/5173/
# PASS: 200 (with preview-session auth per P1-2; during rollout flag-gated)
curl -s -o /dev/null -w "%{http_code}\n" https://<WS>-preview.safespaces.dev/api/v1/me
# PASS: 404 — API routes must not exist on preview hosts (test T5)
curl -s -o /dev/null -w "%{http_code}\n" https://garbage-preview.safespaces.dev/
# PASS: 421/default-backend — unknown hosts route nowhere (test N1)
```

### B2 · P1-2 signed bootstrap + session cookie

- `dev_preview_url` returns URL with `?t=<token>`; first load 302s to clean
  path and sets `__Host-pv` (HttpOnly, Secure, no Domain).
- **One-time (T4):** copy the `?t=` URL, load twice → second redemption
  must 401. Reload without token → 200 (cookie session works).
- Suspend/resume workspace → cookie invalidated.

### B3 · P1-3 tooling + B4 · P1-4 port policy

- Tool-layer blocklist: ask the agent for `dev_preview_url(port=4097)` →
  **tool itself refuses** (error before any URL is minted).
- Proxy indistinguishability (T3 hardening): through a valid session,

  ```bash
  curl -s -w "\n%{http_code}\n" -H "$PV" https://<WS>-preview.safespaces.dev/4097/
  curl -s -w "\n%{http_code}\n" -H "$PV" https://<WS>-preview.safespaces.dev/9999/
  ```

  Blocked (4097) and dead (9999) must return **identical status + body**.
  Denial reasons appear in server logs only.
- Listener probing: stop the app server, call `dev_preview_url(port=5173)`
  → response names no listener and suggests live ports (or 502 with an
  agent-readable body when proxied).

### B5 · P1-5 rate limiting

- Burst via `stress.html` phases 3–4 → now expect **429s** with
  `Retry-After` when the documented budget is exceeded (the frozen-counter
  behavior from 2026-08-19 must be gone), and recovery timing consistent
  with the documented window (T7 in DESIGN §8).

**Phase 1 exit:** B1–B5 PASS; old URLs still functional (overlap).

---

## 4. Phase 2 — CSP relaxation (new origin only)

Hard precondition: **A4 merged and verified** (THREAT-MODEL T8).

- Load `csp-test.html` and `tier3-test.html` on the NEW origin:
  - **PASS expected:** inline script, inline handler, inline `<style>`,
    `style=` attr, eval, new Function → all flip to PASS.
  - **Must remain:** JSON data block PASS, CSSOM PASS, same-origin fetch
    PASS, cross-origin fetch FAIL (CORS now, not CSP), CDN script FAIL.
- Same pages on the OLD origin during overlap: still FAIL (strict policy
  deliberately unchanged there).
- Header check: new origin's `content-security-policy` matches DESIGN §5.4
  (note: no bare `wss:`; `connect-src 'self'`).

---

## 5. Phase 3 — soak (2 weeks)

- Daily: load `ws-test.html` (PASS) and `stress.html` phase 1–2 (SSE
  streamed, 2 MB intact) on the new origin.
- Watch: 429 frequency under real dev usage (tune P1-5 budget with data),
  WS stability over >5 min idle, cookie behavior across suspend/resume.
- Weekly: rerun full regression table (DESIGN §8) and record results next
  to it.

## 6. Phase 4 — cutover

```bash
curl -s -o /dev/null -w "%{http_code}\n" -H "$PV" \
  https://api.safespaces.dev/api/v1/workspaces/<WS>/dev-preview/5173/
# PASS: 410 Gone (test T6) — after the announced deprecation window
```

- New-origin regression table fully green; old routes removed from the API
  host; final run of §8 table archived with the release notes.

---

## 7. Platform-side checks (not automatable from the workspace)

| Check | Answers | When |
|---|---|---|
| Code review: credential injection into agentd | Q2 | P0-2 review |
| Dashboard origin decision | Q3 | before P2 |
| `secrets.json` blast radius + pod hardening | T11 | independent track |
| Rate telemetry informs final budget | P1-5 | during soak |
