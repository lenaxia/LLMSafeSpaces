# NNNN — Dev-preview WS-unreachable-502: root-causing the v0.44.9 release flake

Task `tsk-teal-pelican-leaves` (release-blocking flake, run 41001929062,
`Test (-short, with coverage)` job, 04:26). Single-test failure:

```
httputil: ReverseProxy read error during body copy: response exceeds size cap (32768 > 500)
--- FAIL: TestDevPreviewHandler_WSUnreachablePort_502
    dev_preview_test.go:916: expected 502 from ErrorHandler, got resp=&{404 Not Found ...}
```

## Classification (criterion 1): TEST-ONLY nondeterminism — no prod bug

The incident hypothesis was "the two error routes race (which one claims the
response)". That race does not exist. In `net/http/httputil` the error routes
are strictly sequenced by control flow, never concurrent:

- transport `RoundTrip` error → `getErrorHandler()` is called BEFORE any
  response byte exists (reverseproxy.go, `ServeHTTP`: `res, err :=
  transport.RoundTrip(...); if err != nil { p.getErrorHandler()(...) }`) —
  our ErrorHandler's 502 (or 413 on the "size cap" string) is the only
  possible outcome;
- a body-copy error (`cappedReader` mid-stream) can only fire AFTER
  `rw.WriteHeader(res.StatusCode)` has already committed the status line —
  it logs (`copyBuffer`'s `httputil: ReverseProxy read error during body
  copy: ...`) and aborts; it cannot choose a status;
- no path through ReverseProxy can synthesize a **404**. A 404 reaches the
  WS client only as a faithfully proxied response from a live upstream.

### The `32768 > 500` line was a red herring from a sibling test

32768 is not a 32 KB error page — it is `copyBuffer`'s 32 KiB copy buffer
(`make([]byte, 32*1024)` in reverseproxy.go): the first `Read` on the 100 KB
chunked stream lands the counter at exactly 32768. And max=500 exists only in
`TestDevPreviewHandler_ResponseSizeCap_ChunkedStream`'s handler
(`MaxResponseBytes: 500`); the WS test's handler uses 50 MiB. Verified by
running just the two tests — the exact log line is emitted by the PASSING
chunked test:

```
=== RUN   TestDevPreviewHandler_ResponseSizeCap_ChunkedStream
2026/10/30 06:44:04 httputil: ReverseProxy read error during body copy: response exceeds size cap (32768 > 500)
--- PASS: TestDevPreviewHandler_ResponseSizeCap_ChunkedStream (0.01s)
```

Std-lib `log.Printf` (ReverseProxy's default ErrorLog) bypasses `go test`
attribution, so in the raw CI log it surfaced adjacent to the WS test's FAIL
block. Log-adjacency, not causality.

### The actual race: the freed ephemeral port was stolen mid-test

The test closed an `httptest` server and dialed the freed port, assuming
"nothing listens there now". That assumption is false on a busy netns:

- CI runs `go test -race -short ./...` — package binaries run in PARALLEL,
  and every package's tests open/close `httptest.Server` listeners in the
  same network namespace, churning the ephemeral range continuously;
- the test's own `ts := httptest.NewServer(...)` is created immediately
  after `backend.Close()` — a prime candidate to re-bind the just-freed port
  itself;
- measured on an idle box: 2/2000 freed ports re-bound by one of the next
  two `NewServer` calls (~0.1%; worse under CI port pressure).

Whoever re-binds first, the proxy's dial SUCCEEDS, forwards
`GET /v1/dev-preview/5173/ws`, and the foreign server — any router without
that route — answers 404, which ReverseProxy faithfully copies back to the
gorilla dialer. **Reproduced end-to-end** with a scratch test that
force-binds a bare `gin.New()` on the freed port before the dial:

```
zz_flake_repro_test.go:64: expected 502 from ErrorHandler, got resp=&{404 Not Found 404 HTTP/1.1 1 1 map[Content-Length:[18] Content-Type:[text/plain] ...
```

— byte-for-byte the CI signature (18-byte body = gin's `404 page not found`).
The repro also explains the 20/20-local + CI-only split: a quiet dev box
rarely re-binds the port in the microsecond window; a loaded release runner
does.

## The fix (test-only): own the port — a listener that RSTs

`TestDevPreviewHandler_WSUnreachablePort_502` now binds its own
`net.Listen("tcp", "127.0.0.1:0")` and holds it for the test's whole
lifetime; a goroutine accepts and immediately slams every connection closed
(never reads, never responds). The upstream failure is then synchronous and
unambiguous:

- the port can never be stolen — it is never free;
- no interleaving can produce a response: whether the slam lands before or
  after the proxy's write, the request fails on a real socket (RST or EOF),
  `RoundTrip` returns the error before any response byte exists, and the
  ErrorHandler produces the 502 under test;
- the real transport dial/write/read path stays exercised (no stub
  RoundTripper).

The task hint's other option — "a listener that never accepts" — was
rejected deliberately: the kernel completes the TCP handshake from the
accept backlog, so the dial SUCCEEDS and the request hangs until timeout;
there is no prompt transport error and no prompt 502. (Backlog-full SYN
drop is equally timeout-shaped.) "A port that RSTs" is the variant that
fails fast and deterministically. Documented in the test comment.

Assertion strength unchanged: still `resp == nil || resp.StatusCode !=
http.StatusBadGateway` → fatal — 502 exactly, not merely non-404.

Residual, explicitly out of scope: `TestDevPreviewHandler_DevServerUnreachable_502`
dials fixed port 59999 — same class in theory (any process binding 59999),
but the test opens no window itself (fixed port inside the default ephemeral
range; random-bind collision only), it has never flaked, and touching it is
scope creep beyond the incident. Noted here for the record.

## Evidence (criterion 2)

- `go test ./api/internal/handlers/ -run 'TestDevPreviewHandler' -count=50` —
  ok (25 tests × 50 = 1250 runs, 1.6s)
- `go test ./api/internal/handlers/ -race -run 'TestDevPreviewHandler'
  -count=20` — ok (25 × 20 = 500 runs, 1.7s)
- full package in the CI shape, `go test ./api/internal/handlers/ -race
  -short` — ok (100.9s)
- `gofmt -l api/internal/handlers/` — clean

No prod code changed → no CHANGELOG entry (per task constraints).
