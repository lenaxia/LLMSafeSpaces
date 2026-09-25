# Worklog: #1530 — send_message arg-mutation investigation: race falsified, emission root cause, recovery + seam refusal

**Date:** 2026-09-24
**Session:** Orchestrator-delegated investigation of the send_message arg-mutation race hypothesis (13 delivery misfires), evolved into the emission-duplication recovery + duplicate-key seam refusal
**Status:** Complete

---

## Objective

Adjudicate the orchestrator's hypothesis for #1530 — the origin plugin's in-place arg mutation racing the MCP serializer, duplicating id fields when calls carry both origin and target params — with a probe; fix at the plugin if confirmed, deliver the honest negative otherwise. Root-cause the 13 live misfire instances (the orchestrator's 11 + this worker's 2, both emitted while reporting the #1525 fix).

---

## Work Completed

### Investigation and falsification (r1)
- Read the pinned upstream 1.18.15 dispatch chain (plugin/index.ts trigger, session/tools.ts:106, MCP client path): hooks run sequentially+awaited over one shared args reference — aliasing is real, concurrent serialization is not.
- `scripts/1530-arg-mutation-probe.mjs` (new): harness-exact replica, four legs, 11,501 dual-param wire bodies — sequential, concurrent, aliased-shared-args interleaved at await points, and literal mutation-during-stringify via hostile Proxy. Zero corrupt bodies: V8's synchronous `JSON.stringify` cannot emit duplicated keys or unbalanced quotes from any object state. Race FALSIFIED structurally.
- Root cause proven from the bytes: every corrupt value contains ESCAPED quotes — escapes exist only in JSON source text, so the corruption predates serialization. MODEL EMISSION (duplicating the tail of a similar prior call visible in context). `scripts/1530-corrupt-args-liveprobe.py` (new, era-aware): live raw-wire characterization against agentd.

### The fix (r1)
- `splitDuplicatedArgFragment` + recovery in `mcpSendMessage` (mcp_tools.go): exact-shape detection, leading-id recovery, delivery, composed omit-when-clean warning (extends the #1525 self-send field); embedded origin never trusted (mismatch surfaced as cosmetic; resolved origin wins).
- Four red-first integration rows in mcp_tools_test.go; the initial red reproduced the production error byte-for-byte.

### Round-1 review findings (all addressed)
- Duplicate-key silent-collapse exposure (issue comment thread; neither shipped nor ruled out): SHIPPED — `pkg/utilities/json_duplicate_keys.go` scanner (token-walk recursive descent; Go's decoder collapses duplicate keys silently, last-wins) + tools/call seam refusal (-32602, names the duplicated paths) in mcpHandler; unit tables + handler rows.
- Rejection-half coverage of the predicate: SHIPPED — `TestSplitDuplicatedArgFragment_RejectionHalf` (11 rows: malformed ids, empty target, trailing garbage, multi-mark) + `AcceptanceHalf`.
- Missing e2e level: SHIPPED — `TestOriginE2E_EmissionDuplicationRecovery` (happy: recovery + delivery + warning through real opencode + real plugin + real agentd) and `...RecoveredTargetNonexistent` (unhappy: error names the RECOVERED id, fragment absent). Both verified against the live pinned binary (15s each).
- Real-binary wire-integrity probe restored: `TestOriginPlugin_DualParamWireIntegrity` in the pkg harness (raw-body capture in the stub MCP; injected-overwrite semantics + each identity key exactly once + scanner clean on raw bytes); rides CI's existing `-run TestOriginPlugin` selector. Replica probe wired into the origin-plugin-pin CI job (node step).
- Stale liveprobe narrative: rewritten era-aware (post-#1530 recovers and names the recovered id; pre-#1530 hard-fails on the raw fragment; either era passes with its label) and renamed `.sh`→`.py` (python shebang vs scripts/ bash convention).
- Comment truth (Rule 4): the dispatch comment's "never schema-advertised, never model-supplied by construction" corrected — models DO supply copies (the key is visible in rendered prior calls).
- Description tension: the send_message description no longer carries the literal fragment bytes (removing a second imitation-priming channel); the recovery notice is now generic.
- Worklog restructured onto the mandated template.

### Round-2 review findings (all addressed)
- **Escapable refusal (verified fail-open)**: the gate's `err == nil && len(dups) > 0` skipped refusal when the scanner errored, while the seam's streaming Decode accepts trailing garbage — a duplicate-key body with a garbage tail dispatched with keys silently collapsed (the exact misdelivery the gate exists to prevent). FIXED: the tools/call gate fails CLOSED — scanner error → -32700 (unscannable body), dups → -32602. The scanner's doc comment (which asserted the opposite of the seam's behavior) rewritten to state the contract.
- **Missing pin rows**: `TestMCPHandler_ToolsCallTrailingGarbageDuplicateKeyRefused` (the escape, both legs: dupes+garbage and garbage-only) and the escape-shadowed scanner row (`{"\u0073ession_id":1,"session_id":2}` → `$.session_id`, plus an escape-resolved-distinct clean row).
- **Style**: dead `_ = ctx` removed from the unhappy e2e row.
- Issue closure verified FULLY ADDRESSED this round (legs 1–3 + all three comment-thread artifacts on-branch, CI-enforced).

### Round-3 review findings (all addressed)
- **Unbounded raw-body buffering (validated DoS edge)**: the r1 raw read (`io.ReadAll(r.Body)`) had no limit and ran for every method (pre-PR the handler streamed). FIXED: `http.MaxBytesReader(w, r.Body, 16<<20)` (the package's own convention — client.go 16MiB, sessionstate 4MiB), over-limit mapped to -32700 with an explicit message; pinned by `TestMCPHandler_ToolsCallBodyLimitRefused`.
- **Id-grammar divergence**: `sesIDPattern` forbade `-` while the adapter seam's own guard (`loopback.go sessionIDPattern`, #1364) admits hyphens — a hyphenated leading id silently fell back to the lossy pre-fix path. FIXED: the pattern mirrors the seam grammar (`ses_` + `[A-Za-z0-9_-]` under the seam's 1-128 TOTAL-length cap → `{1,124}`); the "deliberately permissive" doc claim replaced with the parity rationale. Pins: hyphenated recovery, the 128-cap boundary (124 recovers, 125 refuses).
- Issue closure FULLY ADDRESSED both rounds; both red-first claims mutation-verified by the reviewer at r3's SHA.

### Migration to fix/1530-send-message-arg-recovery (post-#1562 closure)
- #1562 closed uneaten-verdict (GitHub event-routing anomaly — every carrier reusing its SHAs failed); lane migrated to a fresh branch off main with new SHAs per owner decision.
- Squash-merge of 61d3e09b onto main: ONE conflict in mcp_server.go, reconciled with #1564's merged #1561 boundary — BOTH behaviors coexist: the body is read ONCE bounded by maxMCPBodyBytes (1MiB, 413 on trip, diagnostics verbatim), decodeOneDocument keeps its strict one-document contract on the in-memory copy, and the tools/call duplicate-key scan sees the RAW bytes beneath the decoder's collapse. The fail-closed scanner branch stays as parser-divergence defense (unreachable while decodeOneDocument stays strict — comment says so). My 16MiB layer was superseded BY the socialized 1MiB cap; TestMCPHandler_ToolsCallBodyLimitRefused adapted to 413/"byte cap".
- Verified BOTH families together: #1564's TrailingDataRejected/MidStringGarbage/LiteralIssue1561Repro/TrailingNewline/BodyCap413/BodyCapExactDocPlusTrailingByte413/MetaKeyAllowed/AdditiveTolerance AND all #1530 rows — green.
- Full-suite note (disclosed): 2 UNCAPTURED full-package failures in runs 1/3 (cold-cache window, ~300s runs; failure output lost to tail-only logging — my gap); 8 consecutive greens after, including 2 verbose runs. If CI reproduces, the suspect set is the package's known timing-sensitive family, not a deterministic defect in this lane's rows (every targeted row green across all runs).

### #1572 round-1 findings (all addressed)
- **[Blocking] Quadratic scanner (validated ~170x CPU amplification)**: `path()` built the full ancestor string for EVERY key — measured 4.72s single-thread on a valid 829KB tools/call body vs ~28ms decode. FIXED: lazy path construction (duplicates only, O(dups × depth)); the adversarial shape now scans in ~50ms. Pinned by `TestFindDuplicateKeys_NoQuadraticBlowup` (the reviewer's deep+wide shape, 1.5s bound — two orders below quadratic) + `DeepDuplicateStillFound` (laziness costs no detection).
- **Uncovered composed-warning path**: `EmissionDuplicationGuard_ComposedWithSelfSend` — recovered target == origin (the orchestrator's original misfire shape): BOTH warning parts joined in the single field, delivery still lands.
- **Stale doc ref**: liveprobe `.sh` → `.py` in the test-file comment.
- **Overstated parity**: sesIDPattern comment now says prefix-restricted SUBSET (the seam has no ses_ requirement) with the lossy-fallback framing.

### #1572 round-2 findings (all addressed)
- **[Blocking] The perf pin did not discriminate**: the r1 absolute 1.5s bound PASSED against the known-bad eager implementation on CI-class hardware (~1.0s eager on that ~438KB shape). REWRITTEN: the shape scaled toward the cap (8000-deep + 60K-key fan, 727,793 bytes = 710.7 KiB, under 1MiB and the 10000-depth limit) AND ratio-pinned (scan ≤ 20× the stdlib decode of the SAME body — hardware self-normalizing) with the absolute belt retained. MUTATION-VERIFIED RED before push: the restored eager build fails at 8.29s scan (5.5x past the belt; ~14x past the ratio); lazy green at ~0.13s.
- **Comment accuracy in the test**: sizes and measurements now state this shape's actuals (727,793 bytes; eager 8.3s / lazy 0.13s from the mutation run), not the r0 reviewer's larger shape.

### #1572 round-3 findings (all addressed)
- **[Blocking] Duplicate-bearing amplification survived the lazy fix (reviewer measured twice)**: a 619KB body (8000-deep + 45K dup pairs) made the uncapped scan render 89,999 full-depth paths (7.63s CPU / 17.78GB alloc) and the seam join build a 3.93GB refusal string. FIXED by construction: the scanner caps the REPORT (first maxReportedDupPaths=16 paths, each truncated at maxDupPathLength=4096 with an explicit marker) and returns the TOTAL dup count; the seam message is paths + "…and K more" — the join cannot explode. -32602 semantics unchanged (refuse loud stays).
- **Dup-shape complexity pin added**: TestFindDuplicateKeys_DuplicateBearingBounded (reviewer's shape class, 45K distinct keys each duplicated once at depth, under the 1MiB cap): total counted, list capped, message kilobyte-scale, scan ≤ 20× the stdlib decode + absolute belt. MUTATION-VERIFIED RED on the uncapped build (ReportCapped fails instantly: 40 paths vs 16 — the cap semantics; captured under the pod's load-55 resource pressure, which precluded the multi-second timing-leg red — the instant cap-leg red is the same sabotage reason); shipped build green.
- **Size figures corrected**: 727,793 bytes (710.7 KiB) everywhere; the "~620KB" claims gone.
- Signature change: FindDuplicateKeys now returns (paths, total, error) — callers updated.

### #1572 round-4 findings (all addressed)
- **Untested composition branch**: TestMCPHandler_ToolsCallManyDuplicatesBoundedMessage — 21 copies → 20 dups at the SEAM: -32602, the production "…and 4 more" suffix, message < 4KB, nothing dispatches. (The scanner-level test replicated the composition test-side; this row pins the handler line itself.)
- **Size-claim accuracy (round three of the class)**: the dup-bearing pin's comment now states the builder's byte-exact actuals — 1,046,683 bytes = 1022.1 KiB, 1,893 bytes under the 1MiB cap, 45,000 dups — with the reviewer's ~619KB/89,999-path numbers attributed to THEIR demonstration shape, not this builder.
- **Rider**: the scanner's cap comment no longer overstates — stored REPORT paths are truncated to maxDupPathLength; the transient path() build is O(depth), bounded by the body cap.

### Byproduct — filed separately
- #1561: agentd's HTTP layer salvages invalid JSON bodies, silently dropping trailing keys (probe first-draft finding). Orchestrator ruling: own issue, not this lane.

---

## Key Decisions

- **Recovery over refusal for the fragment shape** (message intent is mechanically recoverable; the #1525 not-a-refusal philosophy) **but refusal (-32602) for the duplicate-key shape** (intent is NOT recoverable — which copy was meant is a guess; loud refusal beats silent last-wins misbinding). Both are model-emission shapes; they get opposite treatments because recoverability differs.
- **Embedded origin never trusted** — cosmetic echo only; attribution always from the resolved origin (injected > declared). Pinned by test.
- **Scanner in pkg/utilities, refusal at the tools/call seam only** — the exposure surface is tool arguments; blanket refusal would touch notifications/initialize traffic for no benefit.
- **Era-aware liveprobe** rather than post-fix-only expectations: deployed pods run pre-fix agentd until the next train; a probe that fails fleet-wide pre-deploy helps nobody.
- Probe A ships as a CI regression despite the negative result: it converts "the race is impossible today" into "the race stays impossible through future plugin rewrites."

## Blockers

None.

---

## Tests Run

- `go test ./pkg/utilities/ -run FindDuplicateKeys` — ok (table + malformed-errors).
- `go test ./cmd/workspace-agentd/ -run 'DuplicateKey|SplitDuplicatedArgFragment|EmissionDuplication|SelfSendGuard'` — ok.
- `OPENCODE_BINARY=/opencode/usr/local/bin/opencode go test -tags=integration -run TestOriginPlugin_DualParamWireIntegrity ./pkg/agent/opencode/` — ok (22s, real binary).
- `OPENCODE_BINARY=... go test -tags=integration -run 'TestOriginE2E_EmissionDuplication' ./cmd/workspace-agentd/` — ok (both rows, real binary, 30s).
- `node scripts/1530-arg-mutation-probe.mjs 10000` — RACE NOT REPRODUCED, exit 0.
- `./scripts/1530-corrupt-args-liveprobe.py` — 4/4 pass (era-aware, pre-fix labels on this pod).
- Full `go test ./cmd/workspace-agentd/` — run before push (see PR CI).
- `go vet`, `gofmt`, golangci-lint (pre-commit) — clean.

---

## Next Steps

- Iterate PR #1562 to APPROVED (round 2+).
- Post-merge: the recovery + seam refusal deploy with the next train; the era-aware probe flips to post-fix labels fleet-wide.
- #1561 (HTTP salvage) is a queued lane candidate — not this lane.

---

## Files Modified

- `cmd/workspace-agentd/mcp_tools.go` — recovery guard + splitDuplicatedArgFragment + composed warning
- `cmd/workspace-agentd/mcp_tools_test.go` — recovery rows, rejection/acceptance tables, seam refusal rows
- `cmd/workspace-agentd/mcp_server.go` — tools/call duplicate-key refusal (-32602), comment-truth fix, description rewrite
- `pkg/utilities/json_duplicate_keys.go` (+ `_test.go`) — NEW: duplicate-key scanner
- `pkg/agent/opencode/origin_plugin_integration_test.go` — raw-body capture, dual-param emission, TestOriginPlugin_DualParamWireIntegrity
- `cmd/workspace-agentd/origin_plugin_e2e_integration_test.go` — two #1530 e2e rows
- `scripts/1530-arg-mutation-probe.mjs` — NEW: falsification harness (CI-wired)
- `scripts/1530-corrupt-args-liveprobe.py` — NEW (renamed from .sh): era-aware live probe
- `.github/workflows/ci.yml` — origin-plugin-pin: probe step
- `worklogs/NNNN_2026-09-24_1530-arg-mutation-investigation.md` — this worklog
