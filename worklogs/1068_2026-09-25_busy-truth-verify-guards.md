# NNNN 2026-09-25 — PR1: the #1573 verify guards + the #1574 busy definition

Lane: fix/1573-1574-busy-truth (w2). Split approved by the orchestrator:
PR1 = incident guards + busy-from-data (this worklog); PR2 = #1576 layers
1+2 + #1573 asks 2/3; PR3 = layer 3; PR4 = layer 4.

## The incident (#1573 Part 1) — what actually ran

The failing process was NOT current code. Current `selfVerifyDecision`
with unset pins yields "no sha256 pin for arch" — never
`expected=sha256("")`. The observed `expected=e3b0c442…` (sha256 of the
empty string) means the failing artifact hashed the pin VALUE — a shape
that never existed in `supervise_selfverify.go` (born #1021, tweaked
#1027). Conclusion: the executed artifact was the STALE BAKED
pre-#1021 bash-entrypoint-era binary (the 80683260 class; the incident
pod ran an old factory base with `/usr/local/bin/workspace-agentd`
still present and on PATH — current bases are S3-stripped). Its old
verify failed → its old failure path group-SIGKILLed the process group
(exit 137, PID 1 died, two live turns halted).

## The two guards (make the class structurally dead)

1. **Config ≠ tamper.** `AgentdVerificationConfigError` class: empty /
   malformed pin, unknown arch = operator signal — loud (distinct exit
   83 + termination log + controller condition/event/metric), never the
   tamper verdict, never a process-group signal. Pin-present mismatch
   stays fail-closed exit 81 (contract unchanged).
2. **Baked self-denial.** `AgentdBakedRefused` class: when
   `AGENTD_IMAGE_VOLUME=1` and the running exe resolves outside the
   overlay mount (`/agentd/…`), the binary refuses to supervise/serve
   (exit 84) — the baked path can never be the recovery fallback. Any
   binary carrying this code is immune; pre-code artifacts (the
   incident's) are covered by base-strip + controller detection.

## #1574 — busy from data, one definition, both views

`busy := streaming || inFlightParts > 0 || pendingToolExecutions > 0 ||
queueDepth > 0`, with the permission/question-wait carve-out (NOT busy
— pills are their own surface; that boundary gets an explicit pin per
the orchestrator's watch-item). One definition in the projection, fed
to the sessionStatusTracker, components surfaced in the snapshot so the
UI renders WHY.

## Round log

- (opening) Read the terrain: self-verify history, projection busy
  override (events-only, `projection.go` — the override considers none
  of InFlightParts/PendingInputs/QueueDepth), tracker, controller
  detection (81/82 only). Tests first.
