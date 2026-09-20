# Worklog: file-upload investigation — disposition (a), the known Epic 68 D1 sidecar limitation

**Date:** 2026-09-20
**Session:** fix/investigate-file-upload (investigate-first lane; no code change per disposition) — owner-reported "file upload not properly working"
**Status:** Complete (investigation deliverable; disposition reported, owner decision pending)

---

## Objective

Determine whether the owner's report is (a) the known by-design sidecar-mode upload limitation, (b) a new frontend/API bug, or (c) the E4-class CI flake — with file:line evidence; report before fixing.

---

## Work Completed

- **Disposition: (a)** — issue #1497 filed with the full evidence chain. No code change (per the delegation, the mode/config decision is the owner's).
- Verified prod pod mode directly: `/agentd-config` mounted RO tmpfs on a live prod workspace pod = the US-4b sidecar layout (this workspace's own pod).
- Traced the full failure chain: uploads must land on the PVC (`UploadsPath = /workspace/uploads`, pkg/agentd/types.go:66 — Epic 68 D2/D3 persistence contract); the sidecar's /workspace is RO by design; no writable PVC-backed volume exists in the sidecar (the 8MiB config tmpfs and secrets tmpfs are structurally unusable — size caps, no persistence); the write fails → agentd 500 `storage unavailable` (cmd/workspace-agentd/uploads.go:26 documents the caveat) → API 502 `workspace agent upload failed` (api/internal/handlers/uploads.go:244) → composer chip-error + toast + notice. Deterministic on EVERY upload on EVERY sidecar workspace — matching the report.
- Ruled out (b): every API failure class (409/507/413/415/502) surfaces visibly in the composer; no silent-failure shape exists; recent main CI Frontend jobs green.
- Ruled out (c): the E4 Playwright row is a mocked 409-surfacing test (frontend/tests/e2e/attachments.spec.ts:281), not the real upload path; not reproducing in the last 6 main CI runs; flake shape (intermittent) mismatches the report (persistent).

---

## Key Decisions

1. **STOP after evidence per the delegation** — (a) means no code fix; the near-term (single-container fleet for upload-bearing workspaces) vs proper-fix (Epic 68 D1 control-socket write-op) decision is the owner's.
2. **No live replay of the owner's request** (no owner credentials) — stated as a Rule-7 caveat in the issue, with the one-command pod-spec confirmation for the owner's specific workspace.

### Assumptions stated and validated (Rule 7)

- Prod runs sidecar mode — validated on a live prod pod (mount topology) + the orchestrator's values statement; chart default is false (helm/values.yaml:391), prod values enable it.
- The E4 flake is not currently active — validated against the last 6 main CI runs' Frontend jobs.

---

## Blockers

None (owner decision pending on the resolution path).

---

## Tests Run

None — investigation-only lane; no code changed. Evidence: code-path reads (file:line in the issue), live pod mount inspection, CI history review.

---

## Next Steps

- Orchestrator takes the mode/config decision to the owner. If the control-socket write-op is funded, it is a design-doc-first lane (Appendix-A socket semantics — same class as #1455's B1 analysis).

---

## Files Modified

- `worklogs/NNNN_2026-09-20_file-upload-investigation.md` — this worklog (no repo code changes; issue #1497 carries the deliverable)
