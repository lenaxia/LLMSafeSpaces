# Dev Preview — Viewing In-Workspace Web Apps

**Epic 66** — Authenticated HTTP/WS tunnel from your browser to a dev server (Vite, Next, webpack-dev-server, Playwright, ad-hoc HTTP server) running inside your workspace.

---

## How it works

When Dev Preview is enabled on a workspace, you can open a URL like:

```
https://<port>-<workspace-id>-preview.<platform-domain>/
```

in your browser and see your dev server's output, **including hot-module replacement (HMR)** — file changes in your workspace update the browser without a manual reload.

### New URL Format (Epic 67)

**Port-in-subdomain format:** `https://5173-<workspace-id>-preview.<platform-domain>/`

The port is now in the hostname, not the path. This fixes a critical issue where apps that emit root-absolute URLs (like `303 Location: /login`) would break — they now resolve correctly relative to the preview origin instead of losing the port.

**Legacy format (still supported):** `https://<platform-origin>/api/v1/workspaces/<workspace-id>/dev-preview/5173/`

The legacy format continues to work for backward compatibility, but new requests use the port-in-subdomain format.

The preview URL is **authenticated to you only** — it uses the same login session as the rest of the platform. No one else can view your preview (not other tenants, not unauthenticated viewers). Do not confuse this with a public shareable URL — there is no such feature in v1.

## Enabling Dev Preview

1. Open your workspace.
2. Click the **Settings** (gear) icon.
3. Under **Dev Preview**, check **Enable dev preview tunnel**.
4. Click **Save**.

Alternatively, PATCH the workspace CRD directly:

```bash
kubectl patch workspace <name> --type=merge \
  -p '{"spec":{"networkAccess":{"devPreview":true}}}'
```

## Opening the preview

With Dev Preview enabled and your workspace **Active**:

1. Start a dev server in your workspace terminal, e.g.:
    ```bash
    npm run dev -- --host 0.0.0.0 --port 5173
    ```
2. In the Settings drawer, enter the port (default 5173) and click **Open preview**. A new browser tab opens at the preview URL.
3. Edit a source file. The browser updates automatically via HMR.

### Manual URL construction

**New format:**
```
https://5173-<workspace-id>-preview.<platform-domain>/
```

**Legacy format:**
```
https://<platform-origin>/api/v1/workspaces/<workspace-id>/dev-preview/5173/
```

## Per-framework configuration

The platform rewrites the `Host` header to `localhost:<port>` on every request, so most dev servers work with no additional configuration.

| Framework | Bind | Extra config |
|---|---|---|
| **Vite ≥ 5.4.13 / 6.0.12 / 7.x** | `--host 0.0.0.0` or `server.host: true` | None — the proxy's Host rewrite covers CVE-2025-30208's `server.allowedHosts` enforcement |
| **Vite < 5.4.13** | same | None (older Vite does not enforce Host) |
| **Next.js** | `--hostname 0.0.0.0` | None — Host rewrite covers `allowedDevOrigins` |
| **python `-m http.server`** | `--bind 0.0.0.0` or `127.0.0.1` | None |
| **`npx serve`** | default | None |

**Universal rule:** always start your dev server on `0.0.0.0` (or `localhost`) on a known port; the proxy handles the rest.

## Limitations

- **Suspend/Resume:** if your workspace is suspended (idle or manual), the preview stops working. After resume, restart your dev server and refresh the preview tab. Long-lived WebSocket connections (HMR) do not survive pod restarts.
- **Response size cap:** responses are capped at 50 MiB by default (operator-configurable). Streaming responses (large media) are torn down mid-stream when the cap is exceeded.
- **Connection cap:** 50 concurrent connections per workspace (operator-configurable), separate from agent-session limits.
- **Ports 4096, 4097, 4098 are blocked.** These are the platform's internal agentd/opencode ports.
- **Privileged ports (<1024) are blocked.** Use ports ≥1024 for your dev server.
- **WebSocket support:** HMR (Vite, Next, webpack) works. The proxy forwards WS upgrades natively.
- **Cookie scope:** Each distinct port (e.g., 5173 vs 3000) on the same workspace requires its own preview session bootstrap. This is because preview origins are distinct browser origins; cross-port `fetch()` calls require CORS configuration in your app.

## Sharing your app with others

Dev Preview URLs are **not shareable** — they require your authenticated session. If you need to share a running app with a teammate or the public, run a tunnel provider inside your workspace:

```bash
# Cloudflare (free, no account for quick tunnels):
cloudflared tunnel --url http://localhost:5173

# ngrok:
ngrok http 5173
```

Both produce a public HTTPS URL that you can share. The workspace egress NetworkPolicy already allows outbound HTTPS to public internet, so no platform configuration is needed. **The platform does not provide public shareable URLs in v1.**

## Troubleshooting

| Symptom | Cause | Fix |
|---|---|---|
| `503 workspace not ready` | Workspace is suspended or not yet Active | Activate the workspace and wait for `Active` phase |
| `503 dev preview not enabled` | The workspace has `spec.networkAccess.devPreview: false` | Enable it in Settings or via `kubectl patch` |
| `503 dev preview is disabled on this instance` | The operator has set `devPreview.enabled=false` globally | Contact your platform operator |
| `400 port denied` | You requested port 4096/4097/4098 or a privileged port | Use a port ≥1024 that isn't in the denylist |
| `413 response exceeded size cap` | The response body exceeded the cap (default 50 MiB) | Reduce the response size or contact the operator to raise the cap |
| `429 connection limit reached` | More than 50 concurrent connections from your browser | Close excess tabs; the limit resets as connections close |
| `502 dev server unreachable` | No process is listening on the requested port | Start your dev server and confirm it's listening |
| HMR works but page loads with wrong origin | Dev server is checking `Host` against an allowlist | Platform already rewrites Host to `localhost:<port>`; if your framework overrides this, remove the override |
| Login redirects break app | App emits root-absolute URLs that lose the port | Use the new port-in-subdomain format — the port is in the hostname, so redirects preserve it automatically |

## IPv6 / `localhost` note

Browsers resolve `localhost` to `::1` (IPv6) first. The platform's API ingress must serve both A and AAAA records (or bind dual-stack). If your browser reports `ECONNREFUSED ::1`, contact your operator — the ingress binding needs to support IPv6. The in-pod agentd listener is already dual-stack-safe (`0.0.0.0`).

## URL format details (Epic 67)

### Host disambiguation

The platform distinguishes between two preview host formats:

- **Legacy:** `<uuid>-preview.<baseDomain>` (e.g., `a1b2c3d4-preview.safespaces.dev`)
- **New:** `<port>-<uuid>-preview.<baseDomain>` (e.g., `5173-a1b2c3d4-preview.safespaces.dev`)

These formats are mathematically disjoint — no host can match both patterns simultaneously. This prevents ambiguity with digit-leading UUIDs (e.g., `1044f4f2-...`), ensuring port parsing is always correct.

### Cookie behavior

Each port-host gets its own `__Host-pv` cookie because preview origins are distinct browser origins. This means:
- Switching ports requires re-authentication (one-click bootstrap)
- Cross-port `fetch()` calls require CORS configuration in your app
- Sessions are scoped to the workspace, not the port

### Path behavior

- **Legacy format:** The entire path `/5173/app/path` is stripped to `/app/path` (port from path)
- **New format:** The entire path `/app/path` goes to the app verbatim (port from host)