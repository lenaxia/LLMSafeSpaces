# Worklog: design 0060 implementation PR 2 — supervisor upload_apply + the destination scrub

**Date:** 2026-09-21
**Session:** feat/upload-apply-supervisor — §9 PR 2 of the approved design 0060: the supervisor-side apply engine and the uid-1000 destination scrub
**Status:** Complete (this PR)

---

## Objective

Implement design 0060 §3.2/§4.4/§9.2: the `upload_apply` control-socket method (closed-param validation, write-time statfs gate, bounded-window copy + size/sha verification + same-fs rename, the additive ack fields), and the uid-1000 destination scrub closing the pre-existing sidecar-mode gap (nothing reclaimed `/workspace/uploads/*.tmp` — the single-container boot-scrub call site is unreachable from the workspace container; the sidecar's own call is an RO no-op).

---

## Work Completed

- `cmd/workspace-agentd/upload_apply.go` (new): `uploadApplyEngine` —
  - **Param validation** (§3.2's closed set): uuid-regex on upload_id/staged_name (and their equality), positive-integer size, 64-hex sha256, target through `sanitizeUploadFilename` (traversal/slash → `target_rejected`). Malformed shape → `bad_request` (A.3's wire class); unknown KEYS ignored (A.1 — the decoder drops them, pinned).
  - **Write-time gate** (§4.4, TOCTOU-authoritative): `statfs(/workspace).avail ≥ size + margin` (margin default 64 MiB, `UPLOAD_DEST_MARGIN`); the staged object is untouched on rejection.
  - **Copy + verify + rename** (§3.4): 256 KiB window, incremental sha256, `.tmp` → same-fs rename; verification gates the rename (nothing partial ever visible); the `.tmp` is removed on every failure arm.
  - **The ack** (§3.2): `{applied, path, size}` + the A.1-additive `dest_avail_after` and `margin_consumed` — both computed supervisor-side (it owns the margin parameter); the margin flag is the post-rename observation, never a gate.
  - **Serialization** (§8 item 3's simplest choice): a mutex — independent uuid targets, bounded fd pressure, honest §6.6 characterization.
- `control_socket.go`: the `upload_apply` dispatch arm + the engine field (nil answers `internal` — a wiring bug, never a silent no-op).
- `supervise_opencode.go`: the engine wired at server construction; the **destination scrub** at supervisor boot (ttl=0) + the TTL sweeper on the shared `UPLOAD_STAGING_TTL` clock (one knob, both surfaces).

## Branch note (rebase)

PR 2 was developed stacked on PR 1 and rebased onto main after #1515's squash-merge (305d7aad) — the branch carries only PR-2 changes.

---

## Key Decisions

1. The margin flag is mathematically unreachable single-threaded (size+margin ≤ avail implies post-write avail ≥ margin) — it exists for the TOCTOU interleave, and the TEST shapes it exactly: the rename seam stands in for a concurrent writer consuming the volume post-gate (the design's §6.4 interleave, deterministic).
2. The supervisor uses the same staging-root default + env override as the sidecar (the tmpfs is shared; defaults align by construction — relocation must set the env on both containers, noted).
3. `bad_request` only for malformed params; every semantic failure rides the §3.2 closed enum (worker 2's forwarding consumes the enum 1:1).

### Assumptions stated and validated (Rule 7)

- The workspace container sees the same `/sandbox-runtime` tmpfs (validated live in the design lane: RW mount in this pod's workspace container).
- `supervise-opencode` is the workspace container's PID 1 in sidecar mode (validated: the control-socket server it constructs is the one the sidecar's refresh_files flow reaches).

---

## Blockers

None. PR 4 (e2e un-skip) follows once PR 3 lands.

---

## Tests Run

- `go test -run 'TestUploadApply|TestDestinationScrub' ./cmd/workspace-agentd/` — 9 tests green.
- Mutations, each witnessed red then restored green: pre-copy gate removed (dest_disk_full pin); verification disabled (checksum + size pins, 2 fails); scrub glob widened to non-.tmp (final-objects-never-scrubbed pin).
- Full `./cmd/workspace-agentd/` — ok (277s); golangci-lint 0 issues.

---

## Next Steps

- PR 4 after worker 2's #1516 lands: the nightly's sidecar upload rows flip from assert-clean-fail to assert-delivery (E2/E10/E11 un-skip).

---

## Files Modified

- `cmd/workspace-agentd/upload_apply.go` — new: the engine + destination scrub
- `cmd/workspace-agentd/upload_apply_test.go` — new: 9 tests + mutation-verified pins
- `cmd/workspace-agentd/control_socket.go` — the dispatch arm + engine field
- `cmd/workspace-agentd/supervise_opencode.go` — engine wiring + the boot/TTL destination scrub
- `cmd/workspace-agentd/upload_staging.go` — statfsT moved to production (both files share the alias)
- `cmd/workspace-agentd/upload_staging_test.go` — the alias's test-side duplicate removed
- `worklogs/NNNN_2026-09-21_upload-apply-supervisor.md` — this worklog


---

## Review round 1 (4 findings incl. 1 critical + 2 high, all fixed)

- **CRITICAL — gate before MkdirAll**: statfs(uploadsDir) ran while the dir didn't exist (nothing creates it in a sidecar pod) → ENOENT → avail −1 → PERMANENT dest_disk_full on every fresh workspace (the reviewer reproduced it; my design-lane validation missed that the dir is single-container-lazy). Fixed: MkdirAll first; the gate stats the FILESYSTEM (the dir's parent — the mount root, which always exists). Pinned by TestUploadApply_FreshWorkspaceGateOrdering.
- **HIGH — .tmp-named user finals deleted by the sweeper**: SanitizeFilename permits .tmp names, so `<uuid>-backup.tmp` is a legitimate final indistinguishable from a crashed temp by extension. Fixed with a STRUCTURAL marker: temps are `staging-<uuid>-<name>.tmp` — finals always begin with the upload uuid (uuid regex), so a final can never match the `staging-*.tmp` class. Pinned (the uuid-prefixed .tmp final survives boot AND TTL scrubs).
- **HIGH — the server's blanket 10s deadline truncated 10-60s applies** (EOF → misclassified transport → mid-copy unlink): upload_apply now re-arms the connection deadline to its own bound (+ack slack) from the shared UPLOAD_APPLY_TIMEOUT_MS knob.
- **HIGH — cancellation ignored / wedged copy poisons the method**: Apply takes ctx (checked per window; the single blocked-syscall residual documented) + the lock is TryLock — concurrent applies REJECT with the §3.2 busy enum (bounded queueing, the 429 semantics) instead of queueing past their deadlines. The dead enum member is live; pinned.
- LOW: one engine instance shared by sweeper + server (the second constructor removed); the conn ctx now bounds every method.
- Integration coverage (Rule 0): TestUploadApplySocketRoundTrip — the real server (dispatch/deadlines/error shaping) + the real PR-1 client + real staged object + real destination, the full hop in-process, success and closed-enum legs.
