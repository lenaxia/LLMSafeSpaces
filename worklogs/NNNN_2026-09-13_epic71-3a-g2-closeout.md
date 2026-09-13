# Worklog NNNN — epic-71 / 3a closure: the G2 live-model leg (#1313 close-out)

**Date:** 2026-09-13
**Session:** Close out #1313 after PR #1355 (squash-merged 0acdd84b): execute the one recorded deferral — the G2 live-model continuation leg — and close the issue.

## Objective

#1313's acceptance bar included "the model continues without re-ask (assert the assistant's next turn references the answer, not a re-ask)". Every in-repo harness runs the scripted mock; the design doc recorded this leg as deferred to a live-model context. Execute it, record the evidence in-tree, close the issue.

## Post-merge evidence recap

- Main pool run 34729939896 (head 4be47a5e — includes the merge): **success**; the epic71/3a walk-away row ALL GREEN (W1 L7, W2 late answer + dedupe + Q&A payload, W3 dismiss, W4 suspend/resume, W5 rollover convergence + dismiss-of-unknown 404). The two previously-documented main-red rows were resolved by their owning streams in the same window (F1 via #1356; AC-1b's copy-contract work landed earlier).
- PR checks, main CI, repolint: green. Worklog renumbered (0926).

## G2 execution (live model, pinned opencode 1.18.15)

Setup: this workspace's own harness (127.0.0.1:4096, real model), throwaway session — the 0a-probe precedent (issue #1314 comment 5629429701). Single ask turn ("ask me exactly one question: tabs vs spaces, two labeled options"); the ask rose in ~5s; killed via reject (the deterministic platform death); the late answer sent VERBATIM in the composeQA framing through the V1 message path (the surface the outbox's admission drives).

Observed chain:

1. Ask: `que_…D1Nn` — "Which indentation should the new config file use?" (Tabs | Spaces).
2. Death state in transcript: `tool: question | status: error | error: The user dismissed this question`.
3. Late answer (user message, composeQA verbatim): `Answering your earlier question — "Which indentation should the new config file use?": "Spaces". Continue with this answer in mind.`
4. Model's next turn (~25s): "Noted — the new config file will use four-space indentation. … tell me which file to create and I'll write it with 4-space indents."

**Verdict:** references the answer ✓ (expands "Spaces" to "four spaces" via the option description); no re-ask ✓ (no question tool call); zero new pending asks ✓; proceeds to act ✓. #1313's core experience assertion holds on the live path.

Probe-hygiene note: the first attempt double-sent the ask request (a client-side `000` whose POST the harness had already accepted) — root-caused from the duplicate transcript row, session deleted, rerun clean with a single send. Recorded here because the same failure mode can mislead any future live probe: a `000` from curl does NOT mean the harness did not accept the message.

## Evidence artifacts

- `pkg/agent/opencode/testdata/ask_terminal_states_1_18_15.json` — new `g2_live_model_continuation` block (verbatim exchange + verdict; provenance updated to note the live-model leg).
- `design/stories/epic-71-input-delivery-robustness/US-71.3a-unanswered-question-inbox.md` — §G2 rewritten from "recorded deferral" to "EXECUTED" with the exchange.

## Tests run

- Docs/fixture-only change: `make -C sdks validate` n/a; JSON validity of the fixture checked; no Go/TS surface touched.

## Next steps

- Close #1313 with the acceptance-criteria table + evidence links; flip the epic reserved comment (3a complete incl. G2).
- Remaining epic-71 streams: 4a (#1302, gated on the #828 batch-3 train) and 4b (cleanup tail).
