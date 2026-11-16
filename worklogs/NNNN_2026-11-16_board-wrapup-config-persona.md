# Worklog: wrapUpReviewer config slot + seeded wrap-up reviewer role (epic wrap-up review)

**Date:** 2026-11-16
**Session:** wrapUpReviewer board config slot implementation (tsk-candid-almanac-blooms, wrapup-config-persona)
**Status:** Complete

---

## Objective

Land the wrap-up reviewer's CONFIG half (low-level design §6 / high-level D8): the `wrapUpReviewer` board-config key on user + project tiers, the seeded `wrapup-reviewer` platform role carrying the judgment-half charter, and the `wrapUpReviewerForProject` resolution seam the wrap-up engine story consumes. Story-independent from the engine's 000056 (either order lands green).

---

## Work Completed

### Migration — `000055_board_wrapup_config_key` (NEW, api + helm mirror)

Both key CHECK constraints grown with `'wrapUpReviewer'` (the 000049 vocabulary-growth pattern): defaults → 5 keys, overrides → 4 (defaultWorkerSlots stays user-only). No row-shape change; a stored config without the key resolves to the code default at merge time. Roundtrip + idempotency verified (`make migration-roundtrip migration-idempotent`).

### The slot — `pkg/board/config.go`

`ConfigKeyWrapUpReviewer`; `BoardConfig.WrapUpReviewer string` (always on the wire, empty = default); `BoardConfigOverride.WrapUpReviewer *string` (nil inherits, MergeBoardConfig like ReviewColumn); `DefaultBoardConfig()` seeds `DefaultWrapUpReviewerPersona.RoleID`; `ValidateBoardConfigKey` decodes the PersonaConfig role-ref shapes (bare string canonical, `{roleId}` decodes, legacy shapes/null/empty/oversize reject); `validatePersonaRoleRefs` collects the key beside stagePersonas — unresolved ref refuses the WHOLE write on BOTH tiers (per-key error naming; role-blind deployment refuses; directory outage = 500-class). `buildBoardConfigView` renders the slot via `BoardConfigKeys()` with ConfigSource* annotations. NO set-ness flag (no opt-out semantics to preserve); `personaEligibleStages` untouched (not a stage persona).

### The seeded role + resolution — `pkg/board/persona.go`, `api/internal/app/board_role_directory.go`, `app.go`

`DefaultWrapUpReviewerRoleSlug = "wrapup-reviewer"` + `DefaultWrapUpReviewerRoleSystemPrompt` (the §6 judgment charter VERBATIM: holistic aggregate-diff read; coherence/integration-seams/dead-code/never-wired; evidence+wiring as lenses; reviewer owns judgment; procedure not theirs to change; gaps as real deduplicated stories; never merge/push) + `DefaultWrapUpReviewerPersona`. `seedBuiltinWrapUpReviewerRole` beside `seedBuiltinReviewerRole` (platform-scope slug check; idempotent; org namesake never suppresses), wired in app.go's startup block (best-effort, next boot heals). `Service.wrapUpReviewerForProject`: effectiveConfigForProject → WrapUpReviewer → default persona → applyPersonaRole (slug→id canonicalization; unresolvable DEGRADES to nil + warn — never a role-less spawn).

### Surfaces

Agentd `board_get_config`/`board_set_config` descriptions name the key; the set_config validate-file schema gains `wrapUpReviewer` (oneOf personaRoleRef/persona — the DTO-parity test drives it) with the trim-aware semantic persona-role-ref check on both shapes. CHANGELOG [Unreleased].

### The pins (born-red first — all three packages failed before the implementation)

- `pkg/board/wrapup_config_unit_test.go` (NEW): default slot seeds the slug; key validation (all shapes); merge inherit/override; decode+rows canonical bare-string round-trip; the exact-5-key set pin (no epic-level key vocabulary exists); `validatePersonaRoleRefs` belt (unresolved names the key, both-keys write refuses on either stale ref, role-blind refuses, outage is not a 400); `buildBoardConfigView` sources at both tiers; the prompt pin (charter clauses verbatim + the engine-owns-procedure pin: NO board_record_wrapup_verdict/gapRefs/wrapup_ack/consecutive in the prose).
- `pkg/board/wrapup_config_integration_test.go` (NEW, PG): set-time refusal on BOTH tiers leaves the stored map untouched; slug + id writes land (stored bare string; the grown CHECKs admit the key — proven by the writes); role-blind refuses both tiers; user→project inheritance + REPLACE-discipline un-set; view sources at both tiers.
- `pkg/board/wrapup_reviewer_internal_test.go` (NEW, PG, in-package): wrapUpReviewerForProject — default resolves + canonicalizes to the role id; explicit wins; project tier wins; unresolvable degrades to nil AND heals when the vocabulary returns; stored drift degrades to nil.
- `api/internal/app/board_wrapup_reviewer_seed_test.go` (NEW): exact slug/name/platform scope; the seeded system prompt IS the charter verbatim (parsed from written jsonb); idempotent across restarts; org namesake non-suppression; store errors propagate; selectable via `AgentRolesForUser`.
- Grown pins: `TestBoardConfigKnownKeys` (5 keys), the REST wire-shape expectations (handlers + config_test — the effective row always carries the slot), agentd set_config DTO-parity + role-ref shape subtests.

---

## Key Decisions

- **Unresolved-ref refusal covers the whole write, per key named in the error** (D8 identically to stagePersonas): `validatePersonaRoleRefs` restructured to keyed refs; the stagePersonas message stays byte-compatible where pinned.
- **The prompt carries NO engine procedure** (D8/D9): judgment-half prose only; the verdict verb/gap schema/cap are engine-enforced — pinned by banning the engine vocabulary from the constant.
- **`""` is not a writable value**: the trim-aware non-empty rule applies (empty means default on the EFFECTIVE row only; un-setting is key absence). Stored empty/drift fails closed at read (StorageDriftError) and resolution degrades to nil.
- **Helper lives in persona.go** (not a new wrapup.go): the engine story creates wrapup.go; the resolution seam is persona-shaped and sits beside `stagePersona`.

---

## Blockers

None. NOTE: `TestSupervisePiSubprocess_GracefulShutdown`/`RepeatSignalExitsAtOnce` fail identically on pristine origin/main in this sandbox (subprocess supervision timeouts — environmental, not this diff); CI runs them in a proper environment.

---

## Tests Run

- `go test ./pkg/board/ ./api/internal/app/ ./cmd/workspace-agentd/` — ok (born-red → green)
- `TEST_DATABASE_URL=… go test -tags integration -race ./pkg/board/` — ok (13s, full suite)
- `TEST_DATABASE_URL=… go test -tags integration ./api/internal/app/` — ok
- `make migration-roundtrip migration-idempotent` — ok
- `make fmt-check imports-check vet repolint` — ok; pinned golangci-lint: 0 new issues (one pre-existing v2.15.0 errcheck hit on main's pi_supervisor.go; CI pins v2.13.1)

---

## Next Steps

The wrapup-engine story (000056, the seam/mint) consumes `wrapUpReviewerForProject` at its three call sites (mint, reconciler re-claim, spawnFor).
