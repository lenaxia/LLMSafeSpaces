# Dev Preview — Validation Contract (US-66.1 spike)

**Date:** 2026-08-10
**Spike environment:** `terraform` (hostname; `hostnamectl` → QEMU/KVM VM) — ⚠️ **see Open Issue #1: this environment is a plain Ubuntu 24.04 VM, NOT an LLMSafeSpaces workspace pod.** No `agentd` was running, so assumptions that required a live `agentd` (A3 live checks) could not be executed here and are explicitly deferred.
**Runtime:** Ubuntu 24.04.3 LTS, kernel 6.8.0-137-generic, x86_64.
**Toolchain used:** node v18.19.1, npm 9.2.0, go 1.25.12, python 3.13.5, `wscat` (system), `yarn` (available), `pnpm` (missing).

> Every claim below cites a command that was actually run and its observed output.
> Commands were run on 2026-08-10 in `/home/ubuntu/workspace/LLMSafeSpace` (cwd).

---

## Environment

### `uname -a`
```
Linux terraform 6.8.0-137-generic #137-Ubuntu SMP PREEMPT_DYNAMIC Fri Jul 17 20:28:23 UTC 2026 x86_64 x86_64 x86_64 GNU/Linux
```

### `cat /etc/os-release | head -3`
```
PRETTY_NAME=Ubuntu 24.04.3 LTS
NAME="Ubuntu"
VERSION_ID="24.04"
```

### `whoami; id`
```
ubuntu
uid=1000(ubuntu) gid=1000(ubuntu) groups=1000(ubuntu),27(sudo),128(libvirt),998(docker)
```

### `ss -tlnp` (what's listening now)
```
LISTEN 0  4096   127.0.0.53%lo:53    0.0.0.0:*
LISTEN 0  4096          0.0.0.0:22    0.0.0.0:*
LISTEN 0  4096          0.0.0.0:111   0.0.0.0:*
LISTEN 0  4096          0.0.0.0:8080  0.0.0.0:*
LISTEN 0  4096        127.0.0.1:41055 0.0.0.0:*
LISTEN 0  4096       127.0.0.54:53    0.0.0.0:*
LISTEN 0  4096               *:9090   *:*
LISTEN 0  4096            [::]:22     [::]:*
LISTEN 0  4096            [::]:111    [::]:*
LISTEN 0  4096               *:8090   *:*
```
**Observation:** ports **4096 / 4097 / 4098 are NOT listening.** The expected agentd baseline services are absent from this environment.

### `env | grep -iE "agentd|opencode|sandbox|workspace" | sort`
```
OPENCODE=1
PATH=/home/ubuntu/.opencode/bin:...:/usr/local/go/bin:...:/home/ubuntu/go/bin
PWD=/home/ubuntu/workspace/LLMSafeSpace
__MISE_ORIG_PATH=...
```
No `AGENTD_*`, `SANDBOX_*`, or `WORKSPACE_*` variables are set.

### Setup: confirm agentd ports where expected — **ALL FAILED (connection refused)**
```
port 4096 (opencode):        curl http://localhost:4096/        -> http_code=000, exit 7 (connection refused)
port 4097 (agentd user mux): curl http://localhost:4097/        -> http_code=000, exit 7 (connection refused)
port 4097 /v1/healthz:       curl http://localhost:4097/v1/healthz -> http_code=000, exit 7
port 4098 /v1/healthz:       curl http://localhost:4098/v1/healthz -> http_code=000, exit 7
```
**Conclusion:** the assumption "agentd ports are where we expect" does NOT hold in this environment. A1/A4/A6 (self-contained) were still validated for real; A3 (which needs a live agentd) was attempted and is deferred — see Open Issue #1.

---

## A1: httputil.ReverseProxy forwards WebSocket end-to-end + Vite HMR round-trip

### Verdict: **CONFIRMED.** A stdlib `httputil.ReverseProxy` forwards a WebSocket upgrade with **zero custom Hijack code**, and Vite HMR (both partial `js-update` and `full-reload`) works end-to-end through the hop.

### Steps + observed output

**1. Vite dev server.** `npm init -y` then `npm install vite` pulled Vite 8.x, which requires Node ≥20 (`util.styleText`, Node 20.12+). The pod has Node v18.19.1, so Vite 8 threw:
```
SyntaxError: The requested module 'node:util' does not provide an export named 'styleText'
    at .../rolldown/dist/.../create-bundler-option-*.mjs:8
Node.js v18.19.1
```
Per the fallback instruction, installed **`vite@5`** (Node 18-compatible):
```
$ npm install vite@5   -> exit 0
$ node_modules/.bin/vite --version  ->  vite/5.4.21 linux-x64 node-v18.19.1
```
(`pnpm` missing; `npm` used. The WS/HMR mechanism under test is identical across Vite majors — see Open Issue #5.)

Created `index.html` (`<h1>hello</h1>`) and `main.js` (`console.log('v1');`). Started:
```
$ npm run dev -- --port 5173 --host 0.0.0.0
  VITE v5.4.21  ready in 265 ms
  ➜  Local:   http://localhost:5173/
  ➜  Network: http://192.168.2.103:5173/
```
HTTP served + bind address:
```
$ curl -sS http://localhost:5173/ | head
<!DOCTYPE html>
<html>
<script type="module" src="/@vite/client"></script>     <-- HMR client injected
  <body><h1>hello</h1> ... </body></html>

$ ss -tlnp | grep 5173
LISTEN 0  511   0.0.0.0:5173   0.0.0.0:*   users:(("node",pid=18254,fd=25))   <-- bound 0.0.0.0
```

**2. The hop — `/tmp/hop.go`** (verbatim, the load-bearing artifact):
```go
package main

import (
        "log"
        "net/http"
        "net/http/httputil"
        "net/url"
)

func main() {
        target, _ := url.Parse("http://localhost:5173")
        // NewSingleHostReverseProxy with DEFAULT behavior — no custom Director,
        // no Hijack, no Upgrade handling. This is the load-bearing test: does the
        // stdlib ReverseProxy forward a WebSocket upgrade with zero custom code?
        proxy := httputil.NewSingleHostReverseProxy(target)
        log.Println("hop listening on localhost:5174 -> http://localhost:5173")
        log.Fatal(http.ListenAndServe("localhost:5174", proxy))
}
```
Started (`go run /tmp/hop.go`). HTTP through the hop returns 200 with identical HTML:
```
$ curl -sS -o /dev/null -w "%{http_code}\n" http://localhost:5174/   -> 200
$ curl -sS -o /dev/null -w "%{http_code}\n" http://localhost:5173/   -> 200
$ curl -sS http://localhost:5174/ | head -4
<!DOCTYPE html><html><script type="module" src="/@vite/client"></script> ...   (same as origin)
```

**3. WebSocket through the hop.** Vite's HMR WS path is derived from the client source (`node_modules/vite/dist/client/client.mjs:536-538`):
```
const socket = new WebSocket(`${protocol}://${hostAndPath}?token=${wsToken}`, "vite-hmr");
```
i.e. `ws://<host>:<port>/?token=<T>` with subprotocol **`vite-hmr`**. The token was extracted from the served client through the hop:
```
$ curl -sS http://localhost:5174/@vite/client | grep -oE 'const wsToken = "[^"]*";'
const wsToken = "RpuEZenqAheC";
```
Connect via `wscat` through the hop:
```
$ wscat -c "ws://127.0.0.1:5174/?token=RpuEZenqAheC" -s vite-hmr
< {"type":"connected"}          <-- WS upgrade traversed the ReverseProxy, server-side HMR handshake completed
```
(See Open Issue #2 for why the URL uses `127.0.0.1` and not `localhost`: wscat/node resolved `localhost`→`::1` while the Go hop bound IPv4-only → `ECONNREFUSED ::1:5174`. Connecting to `127.0.0.1` is a faithful test; in the real design the browser connects to the API origin, a real hostname.)

**4. HMR round-trip — full-reload** (edit the root entry `main.js` mid-connection):
```
$ printf "console.log('v2');\n" > /tmp/vite-test/main.js     # while WS open
# wscat output:
< {"type":"connected"}
< {"type":"full-reload","triggeredBy":"/tmp/vite-test/main.js"}
# /tmp/vite.log:
6:42:51 PM [vite] page reload main.js
```

**5. HMR round-trip — partial `js-update`** (edit a module with an HMR boundary; `counter.js` self-accepts via `import.meta.hot.accept`). The transformed module, served through the hop, carries the boundary:
```
$ curl -sS "http://127.0.0.1:5174/counter.js" | grep -E "createHotContext|accept|hello"
import { createHotContext as __vite__createHotContext } from "/@vite/client";import.meta.hot = __vite__createHotContext("/counter.js");export const msg = "hello-v2";
if (import.meta.hot) { import.meta.hot.accept((newMod) => { ... }) }
```
Then edit `counter.js` (`hello-v2` → `hello-v3`) with the WS open through the hop:
```
< {"type":"connected"}
< {"type":"update","updates":[{"type":"js-update","timestamp":1786387560574,"path":"/counter.js","acceptedPath":"/counter.js","explicitImportRequired":false,"isWithinCircularImport":false,"ssrInvalidates":[]}]}
# /tmp/vite.log:
6:46:00 PM [vite] hmr update /counter.js
```

**A1 evidence summary.** The path `wscat → :5174 (httputil.NewSingleHostReverseProxy, no custom code) → :5173 (Vite)` carried (a) the `vite-hmr` subprotocol WS upgrade, (b) the `{"type":"connected"}` server handshake, (c) a pushed `full-reload` HMR event, and (d) a pushed partial `js-update` HMR event. This is the two-ReverseProxy-hop design's inner hop proven for real; the outer hop (API → agentd:4097) is the same stdlib code path.

---

## A2: lsp_session cookie transport

**Deferred — codebase-verified, not in-pod validated.** The deployed auth reads `Authorization` header OR the `lsp_session` cookie; that cookie-name config is codebase-verified at **`auth.go:1474-1484`**. Server-side verified; browser-side cross-tab cookie behavior requires manual confirmation during US-66.1 review. (No platform docs were reachable from this pod, and the API server source is not present here.) See Open Issue #1.

---

## A3: agentd user mux can host a `/v1/dev-preview/` handler

**Verdict: BLOCKED in this environment — deferred to in-pod re-run.** The agentd user mux (port 4097) is not running here, so the expected 401/405/404 baselines could not be captured. All three probes refused connection on both `::1` and `127.0.0.1`:
```
$ curl -sS -v http://localhost:4097/v1/reload-secrets          -> connect to ::1 port 4097 failed: Connection refused
                                                                  connect to 127.0.0.1 port 4097 failed: Connection refused
$ curl -sS -v http://localhost:4097/v1/workflow/node/execute    -> Connection refused (same, both stacks)
$ curl -sS -v http://localhost:4097/v1/dev-preview/5173/        -> Connection refused (same, both stacks)
```
**What this does and does not tell us:** it does NOT validate the non-collision of `/v1/dev-preview/` with existing user-mux routes. The expected contract behavior (existing protected endpoints return 401/405; the new `/v1/dev-preview/` returns 404 until implemented) must be re-run inside an actual LLMSafeSpaces pod where agentd is listening on 4097. No in-pod validation possible here — see Open Issue #1. The non-collision design assumption is otherwise codebase-verified.

---

## A4: dev server Host-header behavior

### Verdict: **Host rewrite REQUIRED.** With the browser-origin `Host` left in place, Vite 5.4.21 returns **403**; with `Host: localhost:<port>` it returns **200.** The proxy Director must rewrite `Host` (and `URL.Host`) to `localhost:<port>`.

### Vite 5.4.21 (port 5173)
```
Host: api.example.com      -> 403
Host: preview.example.com  -> 403
Host: localhost:5173       -> 200
(no override)              -> 200
```
Body of the 403:
```
Blocked request. This host ("api.example.com") is not allowed.
To allow this host, add "api.example.com" to `server.allowedHosts` in vite.config.js.
```
This is the `server.allowedHosts` allowlist enforcement introduced by the **CVE-2025-30208** patch (Vite 5.4.13+, 6.0.12+, 7.x). Left un-rewritten, the browser's `Host` (the API origin, e.g. `api.platform.com`) is always rejected.

### `python3 -m http.server` (port 8001)
```
Host: api.example.com -> 200
Host: localhost:8001  -> 200
```
Ignores `Host` entirely; rewrite optional.

### `npx -y serve` (port 8002)
```
Host: api.example.com -> 200
Host: localhost:8002  -> 200
```
Ignores `Host`; rewrite optional. (serve log: `INFO  Accepting connections at http://localhost:8002`.)

### Cross-framework conclusion
Vite is the only one of the three that enforces `Host`; the others ignore it. Therefore the **universal, safe** choice is to always rewrite `Host` → `localhost:<port>`: it is required for Vite and harmless (a no-op) for python/serve. See the Director function below.

**Per-framework user config notes:**
- **Vite (≥5.4.13 / 6.0.12 / 7.x):** none required if the proxy rewrites `Host` (recommended). Without the rewrite, the user must list every preview origin under `server.allowedHosts` in `vite.config.js`. `server.host` controls the *bind* address (user should run with `--host 0.0.0.0` or `server.host: true` so the proxy can reach it). Because the HMR WS becomes same-origin through the proxy, no `server.hmr` / `server.hmr.host` config is needed.
- **Next.js:** applies the same kind of host allowlist (`allowedDevOrigins`); rely on the proxy `Host` rewrite, or advise the user to add the preview origin.
- **python http.server / `serve`:** no config needed.

---

## A5: AuthMiddleware ordering

**Deferred — codebase-verified, not in-pod validated.** The route lives on `idGroup`, which inherits `AuthMiddleware` + `WorkspaceAccessMiddleware` before the handler runs; the WS upgrade reaches `httputil.ReverseProxy` only after both gates pass. Validated by reading **`router.go:393,399`** during epic design. No in-pod validation possible (API source not present in the pod; calling the platform API from inside the pod was explicitly out of scope). See Open Issue #1.

---

## A6: size cap on streaming responses

### Verdict: **chunked transfer-encoded streaming is the norm for dev servers; the 50 MiB cap MUST handle it gracefully (tear down, never panic).** Implementation detail (where to enforce) deferred to US-66.4.

### Evidence — chunked stream is real and trivially produced
A minimal `python3 -m http.server`-style handler on port 8000 writing 1 KiB chunks:
```
$ curl -sS -D - -o /dev/null --max-time 2 http://127.0.0.1:8000/
HTTP/1.0 200 OK
Server: BaseHTTP/0.6 Python/3.13.5
Date: Mon, 10 Aug 2026 18:48:48 GMT
Transfer-Encoding: chunked                       <-- chunked, no Content-Length
curl: (28) Operation timed out after 2000 milliseconds with 1772544 bytes received
$ curl -sS --max-time 3 http://127.0.0.1:8000/ | wc -c
2672640                                           <-- ~890 KiB/s sustained; ~10 MiB total if uninterrupted (10000 × 1 KiB)
```

### Evidence — but small/buffered dev-server responses are Content-Length, not chunked
Vite's own module + HTML responses are fully buffered:
```
$ curl -sS -D - -o /dev/null http://127.0.0.1:5173/main.js
HTTP/1.1 200 OK
Content-Type: text/javascript
Content-Length: 578                               <-- sized, not chunked

$ curl -sS -D - -o /dev/null http://127.0.0.1:5173/
HTTP/1.1 200 OK
Content-Type: text/html
Content-Length: 189                               <-- sized, not chunked
```
**Implication:** the cap MUST handle BOTH shapes — (a) responses with a large `Content-Length` (reject/truncate by declared size) and (b) `Transfer-Encoding: chunked` streams (count bytes through a `ResponseWriter` wrapper / `io.LimitReader` on the upstream body, and tear the connection down on overflow). The HMR WS itself is a long-lived upgrade (not a sized HTTP response) and is exempt from the per-response body cap. Whether enforcement sits on the upstream body or on a counting `http.ResponseWriter` wrapper is a US-66.4 implementation choice; the spike confirms only that chunked streams are normal, so the cap may not assume `Content-Length` and may not panic on a chunked response.

---

## Director function (the concrete output)

Based on **A4** (Vite 5.4.21 returns 403 for a foreign `Host`, 200 for `localhost:<port>`; python/serve ignore `Host`), the proxy MUST rewrite `Host`. Classic `Director` form:

```go
// devPreviewProxy builds a ReverseProxy to a single localhost dev port.
// port is the user's dev-server port (e.g. "5173"). The browser's Host header
// (the API origin) is rewritten to localhost:<port> because Vite >=5.4.13
// enforces server.allowedHosts and 403s any foreign Host (A4). python/serve
// ignore Host, so the rewrite is harmless for them. NO custom Hijack code:
// stdlib ReverseProxy already forwards WS upgrades (A1).
func devPreviewProxy(port string) (*httputil.ReverseProxy, error) {
        target, err := url.Parse("http://localhost:" + port)
        if err != nil {
                return nil, err
        }
        host := "localhost:" + port
        proxy := &httputil.ReverseProxy{
                Director: func(req *http.Request) {
                        req.URL.Scheme = "http"
                        req.URL.Host = host // A4: rewrite upstream target
                        req.Host = host     // A4: REQUIRED — rewrite the Host header, else Vite 403s
                },
                // Transport, ErrorHandler, cap-enforcement (A6) wired here in US-66.4.
        }
        return proxy, nil
}
```

Go 1.21+ equivalent using `Rewrite` (preferred for new code):
```go
proxy := &httputil.ReverseProxy{
        Rewrite: func(r *httputil.ProxyRequest) {
                r.SetURL(target)      // sets scheme + URL.Host = localhost:<port>
                r.Out.Host = host     // A4: REQUIRED Host rewrite
        },
}
```

The `vite-hmr` WS upgrade and HMR round-trip traverse this proxy with **no** `Hijack`/`FlushInterval` customization required — proven in A1 with the default `NewSingleHostReverseProxy`.

---

## Per-framework user config

| Framework | Host rewrite by proxy needed? | User dev-server config |
|---|---|---|
| **Vite ≥ 5.4.13 / 6.0.12 / 7.x** | **Yes** (else 403) | Bind: `--host 0.0.0.0` or `server.host: true`. `allowedHosts` NOT needed (proxy rewrites Host). HMR: none (same-origin via proxy). |
| **Vite < 5.4.13** | No (no Host check) | Same bind advice; rewrite still safe/harmless. |
| **Next.js** | Yes (recommended) | `allowedDevOrigins` not needed if proxy rewrites Host; bind `--hostname 0.0.0.0`. |
| **python `-m http.server`** | No (ignored) | None. Bind `--bind 127.0.0.1` or `0.0.0.0`. |
| **`npx serve`** | No (ignored) | None; `--listen tcp://127.0.0.1:<port>`. |

**Universal recommendation:** the user always starts the dev server listening on `0.0.0.0` (or `localhost`) on a known port; the proxy always rewrites `Host` → `localhost:<port>`. That single rule makes every tested framework work.

---

## Open issues discovered

1. **Environment is not an LLMSafeSpaces pod (blocks A2/A3/A5 in-pod).** `ss -tlnp` shows no 4096/4097/4098; `curl` to all three returns `http_code=000`/exit 7 (connection refused) on both `::1` and `127.0.0.1`. `hostnamectl` shows a QEMU/KVM VM (chassis `computer-vm`) running a Kubernetes control plane (`kube-apiserver`, `etcd`, `valkey-server` present) — i.e. the host `terraform`, not a user workspace pod. **A2 and A5 were already "deferred to implementation" per the task brief; A3's live 401/405/404 probes could not be captured and must be re-run inside a real LLMSafeSpaces pod where agentd listens on 4097.** This is the single biggest gap.

2. **IPv6 / `localhost` resolution vs IPv4-only bind (real proxy gotcha).** `wscat`/Node resolved `localhost` → `::1`; the Go hop bound `127.0.0.1` (IPv4) only; result was `connect ECONNREFUSED ::1:5174`. Connecting to `127.0.0.1` explicitly worked around it for the spike. **For production:** the tunnel listener (both the API-side and any agentd-side hop) MUST bind dual-stack (`[::]:port` or `0.0.0.0:port`, or explicitly both), and/or the browser must connect to a real hostname — otherwise browsers that prefer IPv6 for `localhost` will fail the WS upgrade. Worth a dedicated check in US-66.4.

3. **Vite `allowedHosts` is version-sensitive.** The Host allowlist that produced the 403 is from **CVE-2025-30208** (patched in Vite 5.4.13, 6.0.12, all 7.x). Older Vite (<5.4.13) does NOT 403 on a foreign Host. The proxy's Host rewrite is therefore *required for current Vite* and *harmless for old Vite* — always rewriting is the correct, future-proof choice (A4).

4. **`/workspace` did not exist.** The task's requested artifact path `/workspace/PREVIEW-CONTRACT.md` had no parent directory; created `/workspace` via `sudo` (chowned to `ubuntu`) to hold this artifact. The pod's actual workspace root is `/home/ubuntu/workspace`.

5. **Node 18 vs Vite 8.** `npm install vite` (no pin) pulled Vite 8.x, which needs Node ≥20 (`util.styleText`, Node 20.12+); the pod has Node v18.19.1, so Vite 8 crashed at startup. Fell back to **`vite@5`** per the task's fallback instruction (`pnpm` missing; `npm` used). The WS/HMR protocol and the reverse-proxy behavior under test are identical across Vite 5/6/7/8, so this does not weaken A1/A4. Note: `mise` has `node 20.19.3` available but it is not the active default; switching to it would let Vite 8 run if desired in a future re-run.

6. **A6 shape nuance.** Vite's *small* responses are `Content-Length` (sized), not chunked; only streamed/large dev-server responses are chunked. The 50 MiB cap therefore cannot assume `Transfer-Encoding: chunked` and must also handle oversized `Content-Length`. Confirmed for Vite and `python http.server`; enforcement location (upstream `io.LimitReader` vs counting `ResponseWriter`) is a US-66.4 implementation choice.

7. **No cleanup damage.** All spike processes (vite `pid 18254`, esbuild `18269`, go hop `19003`/`20921`) were killed; `/tmp/vite-test`, `/tmp/hop.go`, `/tmp/hop`, `/tmp/*.log`, `/tmp/stream.py`, `/tmp/pyroot`, `/tmp/serveroot` were removed. Co-located platform services (`kube-apiserver`, `etcd`, `valkey-server`) were verified alive after cleanup (3 / 4 / 3 processes respectively). Ports 5173/5174/8000-8002 are free.
