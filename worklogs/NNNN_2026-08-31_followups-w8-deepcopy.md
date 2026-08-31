# Worklog: session follow-ups — W8 bind-time size validation + `make deepcopy` controller-gen wiring

**Date:** 2026-08-31
**Session:** Close the two follow-ups flagged after #1171 (the stories go to other sessions): the W8 file-class size ceilings and the deepcopy Makefile fix from worklog 0870.
**Status:** Complete.

## W8 — bind-time size validation + `size_exceeded`

The gap (epic #1158 W8): file-class values stage onto the pod tmpfs and serve under the `/v1/spawn-files` response cap, but nothing rejected an oversized value early — the only bound was the supervisor's 8MiB reader, whose failure mode is a late `spawn_files_bad_response` loop at spawn time.

Three enforcement points, one shared constant:

1. **`pkg/agentd.StagedFilesMaxBytes` (8MiB)** — single source for the wire cap and the staging ceilings; the handler's `LimitReader` now uses it.
2. **Bind time (`pkg/secrets.validateValue`, create + update)**: plaintext > `MaxSecretValueBytes` (2MiB) → 400 (`ErrInvalidMetadata`, reason `size_exceeded`). The user-facing gate.
3. **Materializer (defense-in-depth, bypass paths)**: per-entry > budget → `OutcomeFailed` with reason `size_exceeded` (T5: the rest of the batch still stages — the `apply_failed:size_exceeded` vocabulary); whole-batch total > budget → loud error BEFORE publish, so the previous complete staging generation survives (the endpoint never serves a body the reader cannot pull whole).

Tests: `TestValidateValue_SizeCap` (at-cap legal, over-cap 400 for every class); `TestR2B_SizeExceeded_PerEntry` (skips only the oversized entry); `TestR2B_SizeExceeded_WholeBatch` (loud refusal, last-good generation intact).

## `make deepcopy` — controller-gen wiring

The 0870 finding, now root-caused and fixed: `hack/tools.go` pinned only `k8s.io/code-generator` (marker-based deepcopy-gen), but `pkg/apis` carries kubebuilder `object:generate` annotations and its committed `zz_generated.deepcopy.go` is controller-gen output — `gen_helpers` grepped for `+k8s:deepcopy-gen=` markers the package never had and silently generated nothing.

- `hack/tools.go` pins `sigs.k8s.io/controller-tools/cmd/controller-gen` (v0.17.3, matching controller-runtime v0.20.3 / k8s 0.32.x; apiextensions-apiserver bumped 0.32.1→0.32.2 in tow).
- `hack/update-deepcopy.sh` rewritten to `controller-gen object:headerFile=hack/boilerplate.deepcopy.go.txt paths=./pkg/apis/...`; the boilerplate is the repo's two-line SPDX header (the full AGPL block in boilerplate.go.txt was not the committed file's style).
- Regen proof the no-op was real: **62 → 66 funcs** — `AgentSessionStatus` and `PodSecurityContext` had NO deepcopy methods at all (nothing referenced them yet, so it compiled); everything else unchanged (pure reorder). Second regen is idempotent. All controller/pkg/apis suites green.

## Tests run

`pkg/secrets`, `pkg/agentd/...`, `pkg/apis/...`, `cmd/workspace-agentd` full (incl. exec), `api -short` — green. golangci-lint 0 issues, repolint pass, gofmt/goimports clean.

## Files

- `pkg/agentd/types.go` (StagedFilesMaxBytes), `cmd/workspace-agentd/spawn_files_pull.go` (shared const)
- `pkg/secrets/secret_service.go` (MaxSecretValueBytes + validateValue), `secret_size_test.go` (new)
- `pkg/agentd/secrets/staging.go` (per-entry + total), `secrets.go` (whole-batch guard), `cross_uid_test.go` (two W8 tests)
- `hack/tools.go`, `hack/update-deepcopy.sh`, `hack/boilerplate.deepcopy.go.txt` (new), `Makefile` (target comment), `pkg/apis/.../zz_generated.deepcopy.go` (regenerated)

## Addendum (2026-08-31, review round): integration + exec coverage

- `api/internal/handlers/secrets_size_integration_test.go`: HTTP-surface
  pins — create over-cap → 400 + `size_exceeded` (secret-file), at-cap →
  201 boundary, update over-cap → 400 + `size_exceeded`, value-type sweep
  (env-secret / git-credential / api-key).
- `cmd/workspace-agentd/reload_size_test.go`: the materializer-bypass
  rows against the REAL reload handler + materializer — mixed batch
  (T5-tolerant: 500 at the HTTP surface, `"reloaded":1` in the body,
  subset manifest, oversized entry never stages), all-failed batch (500 +
  `size_exceeded` + honest `[]` manifest), whole-batch overage (500 +
  `size_exceeded`, last-good manifest INTACT — the refusal runs
  pre-publish). Adversarial note: an initial "refusal must not publish"
  redesign was evaluated and REJECTED — the T5 tolerance and full-replace
  semantics are the author's intended contract (unit tests pin them); the
  observed `[]` on all-failed passes is full-replace honesty, with the
  #443 cache preserving the batch for the next boot's loud replay. One
  observation left for follow-up: a failed batch IS persisted to the
  reload cache (replays loudly; harmless but burns tmpfs bytes for
  oversized entries — the API cap is the real gate).

## Addendum 2 (review round 2): complete spawn-workflow rows + flake fix

- `cmd/workspace-agentd/spawn_files_size_exec_test.go`: near-cap
  (API-cap-sized) secret-file delivers byte-complete at spawn with the
  mode contract; a refused whole-batch re-stage never retracts the
  published last-good generation and the NEXT spawn still delivers it
  (never-block-spawn holds) — the complete bind -> stage -> pull ->
  deliver workflow under both W8 outcomes.
- Cherry-picked the supervisor environ-read de-flake (liveChildEnviron)
  from #1194 — the same load-induced flake (TestSupervisorSubprocess_
  SpawnPull_RevocationIsAbsence) was failing this branch's full-suite CI.
