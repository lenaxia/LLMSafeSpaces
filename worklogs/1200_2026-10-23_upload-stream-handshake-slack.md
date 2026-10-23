# 1842 — TestUpload_Streams_WithoutFullBuffering race-suite flake: zero-slack handshake vs multipart boundary holdback

Date: 2026-10-23 · Lane: lane-1842 · Branch: fix/1842-upload-stream-flake · Issue: #1842

## Summary

`TestUpload_Streams_WithoutFullBuffering` failed intermittently on main's race
suite with the producer's 10s gate: `streaming violation: agentd received
nothing while producer was still writing — API buffered the body` (confirmed
verbatim in run 39711266972; ~10.07s signature). Root cause is a TEST-DESIGN
flaw (zero-slack handshake), not a product race. Fixed test-side by giving the
handshake 512 KiB of slack, keeping the anti-buffering teeth, and pinning the
slack invariant with a deterministic holdback-injection subtest. No production
code changed; no sleeps/retries/skips added.

## Root cause (proven, not presumed)

The test's producer writes exactly 1 MiB of pre-gate part data (chunk1+chunk2),
then refuses to write the tail until the fake agentd confirms it has READ ≥ 1 MiB
— the signal threshold equaled the total pre-gate byte count: **zero slack**.

But `mime/multipart`'s partReader legitimately withholds a bounded tail of the
part stream mid-stream: `scanUntilBoundary` (Go 1.26.x src/mime/multipart/
multipart.go) holds back any window tail that prefix-matches `"\r\n--<boundary>"`
until more input disambiguates it or EOF arrives. When the part's final pre-gate
byte is `\r` (P ≈ 1/256 per run for random content; rarer longer prefix matches
add ~nothing), that byte is withheld, agentd stalls at 1 MiB − 1 bytes, the
signal never fires, and the producer's 10s timer mislabels it "API buffered the
body". Identical across go1.26.6 and go1.26.10 (stdlib diff verified: multipart/
transfer/chunked identical; only unrelated CONNECT/ReadForm changes).

Proof chain:
1. Micro-experiment (standalone): a part stream pausing right after `\r` →
   Part.Read returns total−1 and stalls while the stream is open. CONFIRMED.
2. In-harness deterministic repro: forcing chunk2's last byte to `\r` with the
   OLD threshold reds the test 10.1s with the exact CI signature. CONFIRMED.
3. Source read: every other pipeline layer releases eagerly (transport chunked
   path uses FlushAfterChunkWriter — per-chunk flush, no residue; bufs drain on
   demand), so the multipart holdback is the organic trigger.

False lead from the prior triage lane (transport chunked-writeLoop bufio
residue) is disproven by (3) — the residue theory predicted failure only when
bufio never flushes mid-stream; it does, after every chunk.

## Fix (test-only)

`api/internal/handlers/uploads_test.go`:

- `streamSignalThreshold = 512*1024` — HALF the 1 MiB pre-gate data. Any
  legitimate holdback (≤ ~26 B multipart + buffer residue) is absorbed; a
  fully-buffering API still hands agentd nothing pre-gate and still deadlocks
  the handshake (teeth preserved).
- The deterministic `\r` tail on chunk2 is kept PERMANENTLY — the organic
  flake condition now fires every run and is asserted to be tolerated.
- New `TestUpload_Streams_ToleratesHopHoldback` (U1.2.3b): `uploadCaptureTransport`
  gains `holdbackBytes`; `holdbackReader` withholds a rolling 64 KiB tail until
  EOF on the agentd hop — an exaggerated (~2500×) deterministic stand-in for
  any legitimate mid-stream holdback. Greens with slack; reds deterministically
  under the old zero-slack threshold (mutation-verified).
- Failure message now names the threshold instead of "received nothing".

## Evidence

- Before (main, old test): 55 local iterations (`-race -count=1`, incl.
  GOMAXPROCS=1 ×15 and GOMAXPROCS=2 ×10) all green — organic rate too low to
  hit locally (~1/256), consistent with 1 confirmed red among ~7 main CI runs.
  With the deterministic `\r` tail: RED 10.1s, exact CI signature, 100%
  reproducible.
- After (fix): `-race -count=3` both tests green (0.03–0.05s each); full
  package `-race -count=1` green (249s); 20-iteration loop green;
  golangci-lint 0 issues; gofmt clean.
- Mutation-verify: threshold reverted to `cap1MiB` (zero slack) → BOTH tests
  red deterministically (10.04s each) → restored. The subtest is not vacuous.

## Assumptions / residuals

- Production streaming is correct and unchanged; the withheld-tail behavior is
  stdlib-correct (a real client never refuses to write pending data).
- The 43%-of-main-runs impression from the issue over-counts: 2 of the 3 cited
  reds failed a different job (Origin plugin freeze pin), not this test. 1
  confirmed red of this test among ~7 runs matches the 1/256 organic rate
  poorly (1 sample; can't refine further from available data).
- Mutation-verify suggestion for the orchestrator to pin: flip
  `streamSignalThreshold` back to `cap1MiB` on the merged tree — both streaming
  tests must go red within one `-count=1` run (deterministic, no loop needed).
