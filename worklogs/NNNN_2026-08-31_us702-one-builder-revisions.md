# Worklog: US-70.2 — one builder + two-tier revisions + conditional pull (#1183)

**Date:** 2026-08-31
**Session:** Epic 70 US-70.2 (design 0057 R1; absorbs design 0052 Phase 2 + the #1165 manifest into the revision model). Also: merged US-70.0 (#1187) per the operator runbook Phase 0.1 and rebased this branch over it.
**Status:** Complete — code, unit/integration/exec tests, pin tests green; cluster rows (`local/us-70-revisions-e2e.sh`) wired to nightly+pool, first execution in CI; PG integration-tagged tests run in CI (no PG server in this workspace).

---

## Objective

Issue #1183: collapse the three batch builders into one (`BuildWorkspaceBatch`), introduce two-tier revisions (`manifest_rev = (seq, manifestHash)` + `batchHash`), enforce the W12 entry contract + version-counter discipline, add the conditional pull endpoint (304/ETag, zero-decrypt 304 path), anchor revisions pod-side (`spawned_rev`/`files_rev` = `seq:manifestHash:contentHash`), and close the stale-replica downgrade race with an apply-guard — mixed-fleet tolerant in both directions (W15).

---

## Work Completed

### Server core

- **Migration 000029** (+ byte-identical helm/migrations copies): value-version columns on `user_secrets`, `provider_credentials`, `mcp_servers` (create=1, every value/config-affecting UPDATE bumps; re-encryption does NOT bump — values unchanged, documented at the UPDATE) and `workspace_secret_revisions(workspace_id text PK, seq, manifest_hash, updated_at)` — **text PK**: workspace delivery identity is the CR name, not a UUID (the epic's `secretautopush` uuid error-loop is the live counterevidence).
- **`pkg/secrets/batch.go`**: `BatchEntry` (W12 contract), `Batch{Entries, Revision}`, `BatchRevision{Seq, ManifestHash, BatchHash}`, `ManifestEntry` (value-less tier), `CanonicalBatch` (sorted entries; Go map-key sorting = canonical), `BatchHash` (entries only — no revision recursion), `ManifestHash` (owner line + sorted decrypt-free lines; metadata canonicalized by parse-remarshal), `LegacyBatchJSON` (byte-compatible push body — ordering parity with the deleted `buildSecretsJSON` pinned by test).
- **One builder** `SecretService.BuildWorkspaceBatch(ctx, ownerUserID, workspaceID) (*Batch, *BuildDegrade, error)`: session identity absent from construction (`TestBuildWorkspaceBatch_NeedsNoSessionIdentity` pins it with the session cache evicted); user entries via `GetDEKServerSide`; per-entry decrypt failures audit-and-degrade (loud, never silent); manifest tier built from rows pre-decrypt — a decrypt failure does not reshape the manifest (the revision describes the intended set; the failure is a reason code until the row heals). **Deleted**: `InjectSecrets`, `InjectSessionlessSecrets`, `InjectSecretsForPodBootstrap`, the three injector interfaces, `loadLLMCredentials`/`loadServerKEKCredentials`/`loadNonLLMSecrets`/`loadMCPServers`/`buildSecretsJSON` (−705 lines). All call sites migrated: pod-bootstrap handler, `agentpush.Push` (session-auth plumbing removed — `WithAuth`/`AuthFromContext` deleted with their last consumer), secrets-handler push/reload, MCP push adapters, `secretautopush` (DEK priming dropped — the builder is session-independent; `GetDEKForUser` survives for the heal path only).
- **`EnsureRevision`** (revision_store.go): DB-as-single-writer seq mint — CAS `UPDATE ... WHERE manifest_hash <> $h RETURNING seq`; loser takes the INSERT-ON-CONFLICT-DO-NOTHING → SELECT arm (same hash ⇒ winner's seq); distinct-hash race ⇒ bounded 3-attempt retry ⇒ `ErrRevisionConvergeFailed`, never a fabricated seq. PG race test (8 concurrent racers, distinct hashes, no dup seqs) — integration-tagged.
- **`ManifestFor`**: decrypt-free manifest seam (the same `loadWorkspaceRows` the builder uses, pre-decrypt) for the 304 decision.

### Conditional pull + pod side

- `POST /internal/v1/pod-bootstrap`: request gains `contractVersion`/`clientManifestHash`; v2 + hash match ⇒ **304** + `ETag "<seq>:<manifestHash>"` (no decrypt — proven by `bombRootProvider`/`bombKeyStore` detonator tests over the real service; the 304 never mints seqs); changed ⇒ 200 envelope; legacy request ⇒ today's exact response (byte-shape-pinned). ETag format == pod-side anchor prefix (cross-pinned).
- bootstrap subcommand: v2 request; 304 ⇒ keep file; failed pull with a prior envelope ⇒ keep last-good (never-block extended); legacy body still parsed (old API + new pod).
- sidecar boot: `sidecarBootSecretsAlreadyRan` skip-guard **deleted** — every boot attempts the (cheap, conditional) pull; failures keep last-good.
- materializer: `ParseBatchFile`/`LoadBatchFile` total over envelope/legacy/empty; merge semantics — a cache overlay that changes the effective set drops the revision (legacy semantics return), an identical overlay keeps it (order-preserving merge makes the comparison deterministic); rev anchors written (`secrets-env.rev` = `{rev, appliedSeq}` + additive `rev` in the staged manifest); apply-guard — pulled `seq <= appliedSeq` skips (stale/equal never re-materialize), legacy/push invalidates the marker; anchor lifetime == state lifetime (same emptyDir).
- spawn seams: served `rev` = `seq:manifestHash:contentHash` when anchored, bare contentHash otherwise; supervisor's `spawned_rev`/`files_rev` carry it (I4: content hash always self-computed at the point of consumption).

### E2E + harness (on the #1187 harness)

- `local/us-70-revisions-e2e.sh`: REV-0 contract probe (loud skip-all on pre-contract fleets — nightly stays green on mixed state); REV-1 two-replica identical-revision (scale API→2, per-pod port-forwards, identical revision + canonicalized entries); REV-2 304/ETag + bind → stale-hash 200 with strictly-greater seq + new-hash 304; REV-3 anchored `spawnedRev` (seq prefix equals REV-2's final 200 — pod-delete forces the revisioned re-pull, because a push-converged state intentionally drops the anchor); REV-4 pull path contains the just-bound entry. Wired into e2e-nightly (18084) + the pool (18087, **before** fault arming — pin-tested).
- Pin tests `local/us70_revisions_script_test.go` (bash -n, 304/ETag/monotonic/two-replica/skip pins, both-workflow lockstep + ordering).

### Adjacent

- Merged US-70.0 (#1187 → main) per the runbook; rebased; the local post-rewrite hook's renumber of the US-70.0 worklog (NNNN→0879; the bot's pass after #1187 renamed only the US-69.9 worklog) rides this branch.
- `api/internal/server/router_fault_injection_test.go` fixture migrated to the builder (found by validation — the #1187 fixture post-dated Part 1's branch point).
- Stale comments corrected (pod_bootstrap_e2e_test, reload_credentials_e2e_test, org_credentials_test, secretautopush timeout doc).

---

## Assumptions (Rule 7 — stated and validated)

| # | Assumption | Validation |
|---|---|---|
| A-1 | Batch metadata must be `json.RawMessage` (spec said `map[string]string`) | MCP metadata carries native JSON (args array, numeric timeoutMs); `pkg/agentd/secrets` contract-shape seam tests assert the array; `map[string]string` would break `LegacyBatchJSON` byte-compat |
| A-2 | `workspace_secret_revisions.workspace_id` is TEXT (CR names) | `secretautopush`'s live `invalid input syntax for type uuid` loop on `user_secret_bindings.workspace_id uuid` vs CR names; delivery identity is the name |
| A-3 | Re-encryption must NOT bump version | Values unchanged under re-wrap; the manifest describes values, not ciphertext — documented at the UPDATE; priority flips still move the hash (winner's SecretID changes) |
| A-4 | Per-entry `BatchHash` metadata key order is stable | PG jsonb emits canonical key order; the only metadata-producing path marshals a Go map (sorted keys) — pinned in `CanonicalBatch` doc + shuffle-invariant property tests |
| A-5 | 304 only ever reaches v2 clients (never an old API) | A prior envelope (hence a clientManifestHash) exists only after a v2 200; old APIs ignore unknown request fields and answer legacy — `ParseBatchFile` totals over both shapes |
| A-6 | Mid-life workspaceConfig/adminPrompt changes may ride a fresh 200 on 304 | opencode reads config at startup only; prompt changes already require restart (bootstrap-time merge, #483) |
| A-7 | Value rotation must bump `version` to move the manifest | Lived in the e2e fixture: the manifest covers id/version/type/name/metadata, not values — by design (version counters ARE the change signal); discipline enforced by store tests |
| A-8 | `Metadata` floats: `canonicalMetadata` parses to `interface{}` (float64) | No metadata field carries ints > 2^53 today (`var_name`, `args`, `timeoutMs`); noted as a constraint at the function; `UseNumber` if metadata ever grows big ints |

## Key Decisions

1. **Manifest tier describes rows pre-decrypt** — decrypt failure is a degrade reason, not a batch-shape change; the cheap conditional path and the builder share `loadWorkspaceRows`, so the two tiers can never drift (one query path, zero parity risk).
2. **DB as the single seq writer** — replicas never mint; CAS + read-arm + bounded retry; the returned `(seq, manifestHash)` always describes the batch served with it.
3. **Anchor-drop on push overlay** — the legacy push path deliberately returns a workspace to legacy rev semantics (no anchor, no guard) rather than fabricating a revision for a merged set; US-70.3's notify-pull + US-70.5's push demolition complete the flip.
4. **Mixed-fleet tolerance in both directions** — legacy clients byte-identical (pinned), new client parses both shapes, push body stays legacy: emergency partial rollback degrades to legacy semantics instead of stranding pods. (Runbook Phase 6 note: agentd reaches pods via the controller delivery digest — the API release and the digest bump are the two coordinated knobs.)
5. **Sidecar boot skip-guard replaced by conditional re-pull** — a 304 is cheaper than the guard's staleness window; unreachable API keeps last-good (never-block).

## Adversarial review (Rule 11)

One validator round over the full diff. Real findings fixed: **F1** the #1187 router fixture still implemented the deleted builder (package didn't compile — a rebase-integration miss); **F2** this worklog; **F3** four stale comments describing the deleted builders as live; **F5** unpinned metadata-canonicalization assumption (now documented on `CanonicalBatch`). Investigated and disproven with evidence: manifest instability under ordering, revision recursion in BatchHash, concurrent same-hash double-mint, first-insert race, 304-mints/decrypts, ABA hash-revert stale-304, order-only merge spurious anchor-drop, anchor-outliving-state, old-API/new-client breakage, legacy byte-shape drift, ETag/anchor format mismatch, unbumpped re-wrap hole, migration copy drift, session-identity leakage, autopush scope honesty (uuid loop root-caused to the retained bindings check — `service.go` NOTE; demolition is US-70.5).

## Blockers

None in-repo. First executions of the cluster rows (nightly + pool) and the PG integration-tagged tests run in CI. The runbook's Phase 6 coordination note (API release + `controller.agentdDelivery.image` digest bump together) is recorded in the PR description.

---

## Tests Run

- `go build ./...` — ok
- `go test -timeout 300s -race ./pkg/secrets/ ./api/internal/handlers/` — ok
- `go test -timeout 120s -race ./api/internal/server/` — ok (after F1)
- `go test -timeout 900s ./cmd/workspace-agentd/ ./pkg/agentd/... ./controller/...` — ok
- `go test -timeout 60s ./local/` — ok; `go test ./api/internal/app/ ./api/internal/services/...` — ok
- golangci-lint (touched pkgs, new-findings gate) — clean after F1; gofmt/goimports/vet — clean
- Not runnable here: PG `integration`-tagged tests (incl. the 8-racer EnsureRevision test), cluster e2e rows — both wired to CI

---

## Next Steps

1. PR review/iterate → merge; watch the first nightly run of `us-70-revisions-e2e.sh` (expect the loud-skip on mixed state until both halves deploy).
2. US-70.3 (#1184 W8 merges first — shared `pkg/secrets/secret_service.go`; small rebase expected): notify-pull + reconcile loop consuming the seq-anchored `spawned_rev`/`files_rev` + the stored revision row; `secrets_resync`; revocation = absence.
3. US-70.4 re-wrap reconciler: when healing an unwrappable row, bump `version` so the manifest moves and pods converge (A-7 corollary).
4. US-70.5 demolition: `GetDEKForUser` walk, rehydrate, reload cache, `secretautopush` (+ its uuid-loop bindings check), push path — the builder flip's remaining legacy seams.

---

## Files Modified

- `api/migrations/000029_secret_delivery_revisions.{up,down}.sql` (+ `helm/migrations/` copies)
- `pkg/secrets/`: batch.go (new), batch_test.go (new), builder_test.go (new), revision_store.go (new), manifest_for_test.go (new), revision_integration_test.go (new), injection.go (rewritten), injection_test.go, types.go, credential_store.go, mcp_store.go, pg_secret_store.go, pg_credential_store.go, store.go, errors.go, secret_service.go (doc), key_service.go (doc), plus migrated fixtures
- `api/internal/handlers/`: pod_bootstrap.go, pod_bootstrap_conditional_test.go (new), pod_bootstrap_test.go, pod_bootstrap_e2e_test.go, secrets.go, org_credentials_test.go
- `api/internal/server/`: router_fault_injection_test.go (fixture migration), router_openapi_contract_test.go
- `api/internal/services/`: agentpush/agentpush.go, secretautopush/service.go (+tests)
- `api/internal/app/`: app.go, secrets_adapters.go (+ wiring tests)
- `cmd/workspace-agentd/`: bootstrap.go, sidecar_boot.go, secrets.go, rev_anchor.go (new), spawn_env_pull.go, spawn_files_pull.go, supervise_opencode.go (+ new/updated tests)
- `pkg/agentd/secrets/`: batch_file.go (new), staging.go, secrets.go (+ tests)
- `local/us-70-revisions-e2e.sh` (new), `local/us70_revisions_script_test.go` (new), `.github/workflows/e2e-nightly.yml`, `.github/workflows/us-70-delivery-pool.yml`
- `README-LLM.md` (volume row + "Secret batch revisions (US-70.2)"), `design/stories/epic-70-secret-delivery-v2/README.md` (story status), worklogs/ (US-70.0 renumber riding this branch)
