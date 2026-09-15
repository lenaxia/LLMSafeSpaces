# Worklog: Design 0058 — relay-only key delivery (issue #820)

**Date:** 2026-09-15
**Session:** Design-only elaboration of the owner's 2026-08-27 decision record on #820 into design doc 0058 + Epic 72 story folder; exposure surfaces re-verified on main; PR opened for AI review (holds — design PRs never auto-merge).
**Status:** Complete (design stage; implementation tracked in Epic 72 stories)

---

## Objective

Turn the fixed decision record (D1 relay-through default, D2 per-request resolve / no long-lived plaintext cache, D3 agentd non-decrypt-capable) into the authoritative design document and epic story folder, consistent with current architecture, landed via an approved design PR. No implementation.

---

## Work Completed

### Exposure surfaces re-verified on main @ 17a2f288 (Rule 7 — designs carry assumptions too)

The Aug-14 verification comment's four-surface list is stale in one place: US-70.5 (#1224, 2026-09-01) demolished `/sandbox-runtime/last-reload-secrets.json` (survives only as a repolint deleted-symbol guard, `pkg/repolint/deleted_symbols.go:48`). Corrected inventory (design 0058 §1.2): S1 `agent-config.json` (`pkg/agent/opencode/format.go:71`, writer `configwriter.go:652`, mode 0640 — agent-readable both modes); S2 `auth.json` (boot merge `cmd/workspace-agentd/secrets.go:601-612` chmod 0660 at `secrets.go:1495-1498`; reload PUT `/auth` `pkg/agent/opencode/client.go:191,261,276`); S3 the batch file — single-container: `/sandbox-cfg/secrets.json` (`bootstrap.go:86,173`, uid 1000 per `pod_builder.go:605`) AND `/sandbox-runtime/rt/secrets.json` (US-70.3 B1 relocation `pod_builder.go:331-343`, resync write `resync_secrets.go:177`) — both uid-1000-readable in the default single-container mode (`agentdSidecar.enabled: false`, `helm/values.yaml:376-377`); sidecar mode: uid-2000-written 0600 (`agentd_sidecar.go:99,189`), NOT agent-readable. Non-surfaces verified: `secrets-env` never carries llm-provider material (`pkg/agentd/secrets/secrets.go:653-654,703`).

### Design decisions made beyond the owner's three (flagged in PR body)

1. **Stage 0 EXCLUDED** — 0400 is a no-op in single-container mode (same uid) and breaks opencode's read in sidecar mode (0640+shared-gid is load-bearing); deny rules don't gate arbitrary code. §5 of the design.
2. **Epic 68 → 72** — the decision record's draft folder collides with shipped `epic-68-chat-file-attachments`; renumber note in the epic README (the 67→68 renumber precedent).
3. **Token minted by the router** (`POST /internal/v1/tokens`), controller obtains and stages it — single signing-key holder; renewal rides US-70.2/70.3 manifest-tier conditional pull (no new delivery path, K6).
4. **Batch-builder rewrite is API-side** (one-builder invariant, US-70.2); agentd needs zero seam changes — its story (US-72.4) is liveness/degrade-codes/sweep hooks.
5. **Separate BYO router Deployment in `llm-relay`**, not the Epic 42 fleet router and not fleet VMs (customer keys stay inside the operator trust boundary); leans separate-Deployment, settled in US-72.2's worklog.
6. **Full provider coverage assumed** (not the withdrawn draft's `baseURL != ""` restriction) — surfaced as open item Q1 for the owner.

### Integration claims validated

`AgentConfigWriter` seam passes provider bytes verbatim (`pkg/agent/agentconfig.go:85-147`); `upstreamAuth` router-side injection precedent (`cmd/relay-router/proxy.go:40-56`, `helm` `controller.inferenceRelay.upstreamAuth.keySecret`); per-binding `ModelAllowlist` already exists and is enforced batch-side (`pkg/secrets/credential_store.go:50`, `injection.go:531+`); KMS provider precedents (`kms_aws_provider.go`, `kms_gcp_provider.go`, `RootKeyProvider` at `root_key.go:35`); #1078 = deploy-drain grace (cap-not-delay) + in-flight surfacing, pattern applied to the 2-replica router; `shouldSkipRelay` (`relay_injector.go:98-131`) is free-tier-Zen-only, orthogonal to BYO relay-through; NetworkPolicy carve-out pattern (`relay-router-networkpolicy.yaml` namespaceSelector); condition-types precedent (`workspace_types.go:264-294`, `SecretsDelivery` at `:439`).

### Review iteration 1 (AI reviewer, CHANGES_REQUESTED → fixed)

Five findings, all validated as real (Rule 11 Phase 2), fixed in commit 2:
1. Router log/persistence posture unspecified → §4.7 persistence invariant added (metadata-only; bodies never logged/sampled/buffered; `pkg/redact` gate) + epic K7 + `router_logs_metadata_only` test.
2. HPKE under-specified → §4.2 pinned: candidate lib `cloudflare/circl` (no stdlib HPKE; verified no circl/hpke in `go.mod`), RBAC-authenticated key distribution (router-SA-writable Secrets, not TOFU), reconcile-driven rotation, US-72.1 dependency-review requirement; §10.3 no-KMS-prod trust statement (same class as US-50.1 KEK mount).
3. 2026-09-15 triage question unengaged → §4.1 coverage paragraph (all owner types; per-credential refusal rejected per §9) + epic problem statement.
4. Route-path contradiction design vs epic → unified to `/w/<workspaceID>/<providerSlug>/v1` (design §4.5 authoritative; verified the `/v1` terminal shape matches `@ai-sdk/openai-compatible` induction, `format.go:74-83`).
5. False `[epic-72]` label claim → rephrased as "to be applied at epic filing".
Carried forward per reviewer's test note: `revocation_secret_delete_401` gains a deletion→401 window bound (informer propagation) + `exp` clock-skew test; §4.4 wording updated (revocation bounded by watch propagation, not immediate).

### Review iteration 2 (AI reviewer, CHANGES_REQUESTED → fixed)

Round-1 fixes verified addressed; four new findings (three introduced/stale from the fix commit, one robustness minor), all validated real, fixed in commit 3: (1) §4.2↔§4.3 RBAC contradiction — router-SA keypair writes vs get/list/watch-only posture → name-scoped write carve-out for exactly `llm-relay-hpke-key`/`llm-relay-hpke-pub`, stated in §4.2, §4.3, US-72.2; (2) stale `POST /v1/…` diagram label → `/w/<ws>/<slug>/v1/…`; (3) dangling §9 citation for the per-credential-refusal rejection → §9 row added; (4) hop-6 revocation cross-ref §3 → §4.4. Robustness minor: HPKE rotation availability window → prior keypair retained until re-seal confirms (fail-open availability, fail-closed trust), §4.2. Ride-along citation nits: `routerHopHeaders` `proxy.go:66-75` (was 59-68), `applyModelAllowlist` `injection.go:525+` (was 531+), epic-69 story range `#1135–#1148` with `#1134` as the tracking issue.

### Review iteration 3 (AI reviewer, CHANGES_REQUESTED → fixed)

One blocking + three minors, all validated real, fixed in commit 4: (1) the round-2 "availability window" sentence was unimplementable under the doc's own pins — controller "confirming resolve" needs decrypt (forbidden), retaining a "prior keypair Secret" collides with the two-name carve-out, and the trigger deleted what the window retained → §4.2 rotation rewritten: controller-driven rotate request, same-name in-place update (carve-out intact), prior private key retained in ROUTER MEMORY with dual-key resolve (envelopes carry plaintext `keyID`), completion confirmed by envelope-keyID metadata alone, then retirement; delete-regenerate reclassified as disaster recovery (honest fail-closed window, `CredentialStale`-surfaced). (2) worklog self-contradiction `injection.go:531+` → 525+. (3) §4.3 "them" antecedent → "all other `llm-relay` Secrets (the staged envelopes, `llm-relay-kek`)". (4) worklog K1–K6 → K1–K7. Reviewer's second pass self-discarded two false alarms (`format.go:152`, A10 `secrets.go:575-581` — both verified correct as cited).

### Review iteration 4 (AI reviewer, CHANGES_REQUESTED → fixed)

Two blocking + three minors, all validated real, fixed in commit 5: (1) §4.2 HPKE machinery was single-replica while §4.3 pins ×2 — first-boot keypair race, replica-local rotation/retirement, DR inheriting the race → rewritten replica-symmetric: create-or-adopt first boot (API-server create is the serializer; fingerprint assert), Secret-watch key loading on every replica, dual-key resolve window **bounded by the watch-propagation bound** (same class as K5; the "no availability window" claim corrected to bounded-by-watch), time-bounded uniform prior-key retention (default 10m, no cross-replica ack protocol), DR = create-or-adopt regeneration with honest fail-closed window. (2) Machinery had no owner/API/tests → `POST /internal/v1/keys/rotate` allocated in §4.4; US-72.2 gains scope + ACs + five named tests (`firstboot_two_replica_adopt`, `rotation_dual_key_window_bounded`, `prior_key_retention_expiry`, `dr_keypair_loss_failclosed_recovery`, `envelope_keyid_aad_bound`); §7 gains the row; US-72.3's rotate leg specified (trigger, re-seal, keyID-metadata confirmation); US-72.1 owns the envelope `keyID`-as-AAD format. Minors: envelope `keyID` now specified as GCM AAD-bound (§4.2 envelope bullet); controller `get` on `llm-relay-hpke-pub` granted (§4.3 + US-72.2 RBAC); this ledger reordered chronologically (1→2→3→4).

### Review iteration 5 (AI reviewer, CHANGES_REQUESTED → six minors, zero blockers → fixed)

Six one-clause amendments, all validated real, fixed in commit 6: (1) rotation availability claim falsified by a replica restart mid-re-seal (prior key is memory-only) → failure mode stated explicitly in §4.2 + US-72.2 AC; (2) DR "corrupted" path had no mechanism → fingerprint-assert failure triggers in-place regeneration via the existing update grant (no delete needed); (3) retention-vs-re-seal inequality unpinned → violation behavior stated (CredentialStale, never silent) + `reseal_completes_before_retention_expiry` AC/test; (4) torn two-Secret update could pass completion vacuously → private-then-pub ordering pinned + controller reads pub only after rotate returns + `rotation_torn_update_unconfirmable` test; (5) controller envelope read-back ungranted → confirmation rephrased to sole-writer write-acks (no read-back, no RBAC change); (6) self-referential §4.2 cite reworded. Reviewer's second pass self-discarded five below-bar/false-alarm candidates (retire endpoint, US-72.1 vs 72.2 test home, K-invariant naming, §10.3 phrasing, story-table summary granularity).

### Review iteration 6 (AI reviewer, CHANGES_REQUESTED → one minor → fixed)

Single finding, validated real, fixed in commit 7: the corrupted-DR recovery trigger was stated only for first-boot create-or-adopt, leaving a corrupted keypair Secret in a never-restarting fleet without a bounded recovery path → §4.2: the fingerprint assert re-runs on EVERY watch-delivered key load (corruption detected without a restart); §7 + US-72.2 test descriptions extended with the no-restart pin. Reviewer's second pass self-discarded seven candidates (including the lead-clause "contradiction", informer-vs-get transport choice, crash-between-updates — all derivable from stated machinery or implementation-grade).

### Review iteration 7 (AI reviewer, CHANGES_REQUESTED → one blocking → fixed)

Single blocking finding, validated real: the round-6 watch-time fingerprint assert had an unpinned data source — the natural cross-Secret reading (derive pub from private, compare against `llm-relay-hpke-pub`) fires the DR overwrite path spuriously during every routine rotation's deliberate private-then-pub torn window, racing the rotator's pub write and enabling a vacuous controller confirmation. Fixed in commit 8 by pinning: (1) generation-tagged self-contained payloads (`llm-relay-hpke-key` = {privateKey, publicKey, generation}; `-pub` = {publicKey, generation}); (2) the assert derives pub from private and compares against the CO-LOCATED copy + generation monotonicity — never cross-Secret (pub consumed solely by the controller; resolve uses the private key only, so the torn window cannot read as corruption to a peer); (3) torn-vs-corruption distinguished by generation at controller SEAL TIME (rotate returns the generation; pub generation mismatch → rejected, never sealed against); (4) shape-valid wrong-pub residual stated honestly (CredentialStale on first resolve; repair = re-run rotation). New tests: `rotation_assert_quiesced_in_torn_window`, `pub_sealtime_generation_validated` (§7 + US-72.2). Ride-along cite fix: `lkms:v1:` literal at `root_key.go:19-32`.

### Files

- `design/0058_2026-09-15_relay-only-key-delivery.md` — the design doc (house style per 0055/0057: status header, depends/composes, numbered sections, threat model, assumptions table, rejected alternatives, open items)
- `design/stories/epic-72-relay-only-key-delivery/README.md` — epic folder (renumber note, story map US-72.0–.6 with scope/AC/test-plan/sizing/deps, cross-cutting invariants K1–K7)
- this worklog

---

## Key Decisions

See "Design decisions made beyond the owner's three" above. All six are elaborations within the decision record's authority, surfaced in the PR body; #1 and #6 are explicitly flagged as questions for the owner (delegated Stage 0 disposition; first-party provider coverage).

---

## Blockers

None.

---

## Tests Run

None — design-only change (no code paths touched; `make lint`/repolint run via pre-commit hook on commit). Exposure-surface verification was read-only source inspection at main @ 17a2f288.

---

## Next Steps

1. AI review loop on the design PR until APPROVE (holds; never auto-merges).
2. On approval + owner `/merge`: file Epic 72 story issues (US-72.0 first — #910 re-arm), then implementation per the story map.
3. Owner answers open items Q1–Q3 (design 0058 §10) before US-72.3 lands defaults.

---

## Files Modified

- `design/0058_2026-09-15_relay-only-key-delivery.md` (new)
- `design/stories/epic-72-relay-only-key-delivery/README.md` (new)
- `worklogs/NNNN_2026-09-15_relay-only-key-delivery-design.md` (new, sentinel)
