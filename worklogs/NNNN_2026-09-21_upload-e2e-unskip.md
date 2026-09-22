# Worklog: design 0060 PR 4 — the docs retirement on #1535's feature-detect gate

**Date:** 2026-09-21 (rebased 2026-09-22)
**Session:** feat/upload-e2e-unskip — §9 PR 4: the documentation retirement for the sidecar upload delivery, layered on #1535's three-way gate (the script and its test pins ride main via #1535; this is the docs-only delta)
**Status:** Complete (this PR; rebased onto post-#1535/#1536 main)

---

## Objective

Retire the pre-0060 documentation claims (sidecar uploads clean-fail) across every surface that still asserts them, accurately describing the feature-detect reality (#1535's gate: 201 → full rows / 502,503 → loud skip / else die) and the design-0060 delivery architecture.

---

## Work Completed

- The nightly's step comment: the both-modes/feature-detect truth (#1535's three-way gate, not the retired clean-fail/skip).
- The single-container workflow: the header rewritten as the complement (direct-write path vs the nightly's sidecar leg), the run-step comment describing the feature-detect, 403 (the code-traced cross-user shape), the D19 E11 contract (one-or-two intact files), and the nightly-edit acknowledgment.
- `cmd/workspace-agentd/uploads.go`: the sidecar caveat replaced with the delivery paragraph (the mechanism pointer).
- `README-LLM.md`: the D1 as-built caveat replaced (the delivery paragraph) + version 1.31.
- The epic-68 README's deviation-1 + line 103 + the rows-coverage claim.
- `docs/api/rest.md`: the upload row's both-modes statement.

---

## Key Decisions

1. #1535's gate is the canonical script surface — this PR does NOT touch the script or its test pins (they ride main). The docs-only delta avoids any conflict with the live-proven detector.
2. The D19 retry contract (ONE-OR-TWO intact files after a mid-upload pod kill — retry = new uuid, both contract-legal) is stated in the workflow header and the script's E11 row. The pre-D19 "exactly one" simplification is retired from the coverage table (the workflow header previously carried it too).

---

## Blockers

None.

---

## Tests Run

- `go test -run 'TestUS68' ./local/` — green (all #1535 gate legs, riding main).
- `go test -run 'TestUS72' ./local/` — green (#1536's relay-OFF pins preserved).
- `bash -n` + `yaml.safe_load` both workflows — clean.
- `go build ./...` — ok.

---

## Next Steps

- The serialization fix (the SR-6 finding — the applyMu over-serialization) follows as its own PR once this lands.

---

## Files Modified

- `.github/workflows/e2e-nightly.yml` — the step comment (the feature-detect truth)
- `.github/workflows/e2e-attachments-single-container.yml` — the header + run-step comment (the complement framing, 403, D19 E11)
- `cmd/workspace-agentd/uploads.go` — the sidecar caveat retired
- `README-LLM.md` — the D1 as-built + version 1.31
- `design/stories/epic-68-chat-file-attachments/README.md` — deviation-1 + line 103 + rows-coverage
- `docs/api/rest.md` — the upload row's both-modes statement
- `worklogs/NNNN_2026-09-21_upload-e2e-unskip.md` — this worklog


---
