# Worklog: rbac namespace-scope cache-scoping derivation — the drill-shape crashloop fix (run 35872827066)

**Date:** 2026-09-23
**Session:** Standby assignment from the orchestrator after the monitor's report: the nightly's drill-shape pre-step (its FIRST-EVER execution — every prior nightly died upstream, so the hardened lane from #1542 carried it here for the first time) failed `helm upgrade --wait` with a 10m `context deadline exceeded`. Diagnosis first (no rerun, no push), then GO on the fix PR.
**Status:** Complete

---

## Objective

Make the runbook's own prescribed `rbac.scope=cluster → namespace` migration path actually work: under namespace scope the controller must not run a cluster-wide manager cache against namespaced RBAC.

## Work Completed

### The diagnosis (reported with evidence before any code)

- **Pod status at the deadline:** `llmsafespaces-controller 0/1 CrashLoopBackOff, 3 restarts in 10m` — the sole unready resource; everything else (api ×2, five workspaces, mock-llm, postgres, valkey) healthy. helm `--wait` burned its full 10m on that one deployment.
- **Controller logs (the failure dump):** `Could not wait for Cache to sync` (workspace controller, `kind source: *v1.ServiceAccount`) + `failed waiting for *v1.Secret Informer to sync` → manager exit → crashloop.
- **The causal chain, verified in code:** `values.yaml controller.watchNamespaces: ""` → `controller/main.go` builds a CLUSTER-WIDE cache when the flag is absent ("watching all namespaces") → the drill-shape flip deletes the ClusterRole/Binding (namespace Role covers one namespace) → `SetupWithManager`'s informers (For(Workspace) + Owns(Pod/Secret/ServiceAccount/PVC)) need list/watch across ALL namespaces → Forbidden outside the workspace namespace → informer sync never completes → exit(1) → CrashLoopBackOff.
- **Not relay-specific, not #1537:** the informer RBAC deficit is flag-independent; the relay startup guard PASSED (llm-relay reads ride the direct API reader by design §4.3 — the design's no-informer posture held while the informers died). Latent chart bug: the G5 default posture (namespace scope) itself carries it; nothing ever exercised the namespace-scope RUNTIME until the hardened lane reached the drill-shape step.

### The fix (TDD)

- **RED first:** `helm/rbac_cache_scope_chart_test.go` — the two derivation pins failed against the unfixed chart (no `--watch-namespaces` arg rendered).
- **The derivation** (`controller-deployment.yaml`): `controller.watchNamespaces` set → passes through verbatim (unchanged); unset AND namespace scope → `--watch-namespaces={{ workspaceNamespace helper }}` (the release namespace by default, `api.config.kubernetes.namespace` override honored — the helper's contract); cluster scope → NO arg (byte-identical off-path).
- **Five pins:** the derivation; the custom-namespace override; explicit-wins; cluster-scope-absent (the off-path regression pin, the repo's flag-landing convention); explicit-under-cluster-scope (pre-existing behavior unchanged).
- **Docs:** `values.yaml` comment + the `helm-values.md` reference row + a runbook "Known interactions" entry citing the full crash chain (the migration's first exercise = the nightly's exact signature) and noting the guard/direct-reader distinction.

Two template-assembly defects caught by the red/green cycle itself: Go templates do not chain `else if` on `with` (restructured as `if/else if`), and a both-side-trimmed inline comment glued the emitted arg onto the neighbor line (the tests' arg extraction returned "" — the comment moved above the conditional).

## Key Decisions

1. **Chart-level derivation, not a pre-step flag** (the orchestrator's ruling): the symptom fix would leave every operator's real migration broken; the derivation fixes the default posture (G5) and the runbook path everywhere.
2. **Explicit `watchNamespaces` always wins** — the derivation is a default, never a clobber; cluster scope renders byte-identically (pinned).
3. **The derivation follows the `workspaceNamespace` helper** (not `.Release.Namespace` directly) — one namespace-resolution point, honoring the `api.config.kubernetes.namespace` override.

## Blockers

None. This PR unblocks the #820 evidence checklist (the drill-shape step is the gate to rows 2-4: the drill run, the CredentialsStaged observation, the sweep run).

## Tests Run

- `go test ./helm/ -run 'TestRBAC' -count=1 -v` — 5/5 (grown red-first: the two derivation pins failed pre-fix).
- `go test ./helm/ -count=1` — FULL package green, 30.0s with real helm v3.16.4 (the #1534 lesson: default-posture template changes get checked against every sibling pin, not a scoped run).
- `make repolint` — all checks passed; `golangci-lint run ./helm/...` — 0 issues.

## Next Steps

1. Merge → the orchestrator's schedule-vs-manual decision for the evidence run (monitor r3 armed); the drill-shape step should now converge and the drill + sweep execute for the first time.
2. If the next run still crashloops: the derivation emitted but something else denies informer sync — the failure dump's controller logs will say which Kind; the chart's Role coverage is the next suspect (not expected: the Role grants list/watch on all five informer kinds in the workspace namespace).

## Files Modified

`helm/templates/controller-deployment.yaml` (the derivation + fail guard + comment), `helm/rbac_cache_scope_chart_test.go` (new — 10 pin functions; the honest count, corrected r4 from "8"), `helm/values.yaml` (comment), `docs/reference/helm-values.md` (row), `docs/runbooks/relay-only-flip.md` (Known-interactions entry), this worklog.

## r1 — the watch-all hole, the contradicting bullets, the split-ns caveat (bot review)

1. **`watchNamespaces: "*"` under namespace scope crashlooped identically** (the migration population's own spelling): the template's truthy check passed `*` through verbatim → the controller parses it to a nil namespace map → cluster-wide cache vs namespaced RBAC → the exact run-35872827066 death. Fixed per the remediation ruling: a render-time `fail` (the llm-relay-rbac fail-loud convention in the same file) for `"*"` AND whitespace-only values under namespace scope, the message naming both remedies (explicit namespaces, or keep `rbac.scope=cluster`). Pinned by `TestRBACNamespaceScope_WatchAllFailsRender` (3 value shapes × 4 message assertions) + `TestRBACClusterScope_WatchAllStillValid` (`"*"` stays VALID under cluster scope — the guard must not leak across the scope boundary). MUTATION-VERIFIED: disabling the guard turns the pin 4-red. My first guard's whitespace branch compared the already-trimmed value against empty (unreachable — the pin's whitespace subtests caught it red); fixed by distinguishing raw-vs-trimmed.
2. **The values.yaml bullets contradicted the new paragraph in the same block** (`""` no longer means cluster-wide under the default scope; `"*"` is no longer "equivalent to empty"). Rewritten to the new truth.
3. **The split-namespace caveat (docs-only per the ruling):** when `api.config.kubernetes.namespace` ≠ the release namespace, the derived single-namespace cache leaves the free-models refresher's catalog upsert unreachable. Documented in values.yaml, helm-values.md, and the runbook entry; the watch-both code change is flagged as an owner follow-up in the PR body, NOT built here.

## r2 — the guard mirrored the consumer; the workaround scope-qualified (bot review)

1. **The r1 guard leaked the same-class spellings:** `"*,"`/`"*,ns1"`/`"**"` (embedded `*` — parses to cluster-wide or a bogus `"*`"-named informer) and `"\n"`/NBSP (my `trimAll " \t"` is NOT Go's TrimSpace class) all sailed into the run-35872827066 death. The guard now mirrors the controller's own `parseWatchNamespaces` semantics: fail on `contains "*"` ANYWHERE plus a TrimSpace-equivalent whitespace-only test (`[[:space:]]` + U+0085 + Zs via regexMatch). Pinned by `TestRBACNamespaceScope_WatchAllGuardLeakShapes` (5 shapes); all green shapes stay green (full 8-test set + package). Template-literal lesson encoded in the fix: the regex backslashes must be `\\`-escaped (the template parser consumes `\x`/`\p` as its own escapes — the parse error named the line instantly).
2. **The split-ns workaround was a doc bug of my own making:** "list both namespaces" without scope qualification prescribes the SAME crashloop under namespace scope (the release-ns Role grants only leases/events/configmaps — a release-ns informer is Forbidden). All three sites now scope-qualify: both-namespaces is the remedy ONLY under cluster scope (or wherever RBAC covers both); under namespace scope, accept the refresher caveat.
3. **The refresher mischaracterization corrected (r2 minor finding):** not "per-tick NotFound noise" — the cached read for a namespace outside the cache set ERRORS (not IsNotFound), the upsert never runs, and the catalog NEVER exists (a per-tick "ConfigMap sync failed" error log; `refresher.go:234-236`, not 139-163). All three doc sites reworded.
4. The fail message's explicit-namespaces remedy now names the coverage rule ("namespaces your RBAC actually covers — under namespace scope that is the workspace namespace ONLY, NOT the release namespace").

## r3 — the comma-collapse branch, the stale in-file comment, Zl/Zp, NOTES.txt, the pure-defaults pin

1. **The comma-collapse guard hole (the blocker):** `","`/`" , "`/`",,,"` contain no `*` and are not whitespace-only — the r2 guard passed them verbatim into `parseWatchNamespaces`'s nil branch (`watch_namespaces.go:38-40`, proven by the repo's own `TestParseWatchNamespaces_AllEmptyEntriesReturnsNil`) → cluster-wide → the same death. The guard's empty-collapse test now includes COMMAS: the value fails the render when it is non-empty and matches `^[[:space:],\x{85}\p{Zs}\p{Zl}\p{Zp}]*$`. The comment's "mirrors parseWatchNamespaces" claim is now true of that branch too. Pinned: 3 comma shapes + U+2028/U+2029 (the Zl/Zp TrimSpace edge the r2 class missed).
2. **The stale in-file comment** (`controller-deployment.yaml` relay-URL rationale, "controller watches cluster-wide by default") corrected — the derivation's own file no longer contradicts it.
3. **NOTES.txt scope statement fixed:** "Role-scoped in {{ .Release.Namespace }} only" was wrong under the split-ns override (the workspace Role lives in the override namespace; the release ns holds leases/events/configmaps) — the operator-facing output now names the workspaceNamespace and the split-ns layout, the same family as the r2 docs pass.
4. **The refresher cite corrected:** 232-234 (the SyncConfigMap call + error log + return), not 234-236 (236 is the success log) — values.yaml fixed.
5. **The pure-defaults pin** (`TestRBACDefaultRender_DerivesWatchNamespaces`): no `rbac.scope` key at all — the `| default "namespace"` fallback must flow through the derivation; every prior pin set scope explicitly, so a silent values-default flip would have changed the default render uncaught.

## r4 — five vacuous pins (the r3 landing's own proof was hollow), the missed comment, the counts

1. **Five of thirteen guard subtests were VACUOUS — including every comma-collapse pin the r3 commit existed to land.** Mechanism (the bot's trace, reproduced): raw operator input in `t.Run` names — `value=,`, `value=*,`, … — lands in `t.TempDir()`'s path, and helm's `-f` is a pflag StringSlice that splits on the comma: writeFile succeeds at the full path, helm opens the comma-truncated first element, errors — `helmTemplateErr` returns that HARNESS error and `require.Error` passes with ZERO template evaluation. Mutation-proven by the bot (deleting the collapse branch left the r3 suite green). Fixed: subtest names are index-based (`case%02d`) — raw input never reaches `t.Run` names — AND every fail-shape pin now also asserts `err` contains `CrashLoopBackOff` (the guard's own message — a harness failure cannot satisfy it). Re-mutation-verified: under the well-formed collapse-branch mutation, CommaCollapse goes 6-red (5 subtests + parent) and LeakShapes 3-red (case03 newline + case04 NBSP + parent — the three star-bearing shapes correctly stay green under `contains "*"`). *(r5 correction: this entry first claimed "LeakShapes 6-red" — arithmetically impossible (5 subtests) and measured from a SLOPPY mutation whose unbalanced paren made every subtest fail on a template parse error; the failure messages I did not read said "unexpected right paren" outright. The mutation-verification itself was unverified — recorded from a broken experiment. The r4-added CrashLoopBackOff assertion is precisely what exposes that class: a parse error can never satisfy it.)*
2. **The second stale comment** (`values.yaml` inferenceRelay.workspaceRouterURL rationale, "the controller watches cluster-wide by default") corrected to the scope-conditional truth — the identical falsehood r3 fixed in controller-deployment.yaml, missed in the other file this PR edits.
3. **The counted-claims record corrected:** r3's "Pinned: 3 comma shapes" described three vacuous pins (they pinned nothing until this round); Files Modified now reads 10 pin functions (r3's "8" counted functions, not the added shapes). The suite's honest state at this head: 10 pin functions, every fail-shape now mutation-proven against its guard branch.

## r6 — the stale-posture family in the chart's own docs (three more survivors + a record fix)

1. **`helm/README.md:228`** (values table): "empty = all" → the derivation truth (empty derives the workspace namespace under the default namespace scope; empty = all only under cluster scope; any `*` fails the render under namespace scope).
2. **`helm/README.md:282-284`** (RBAC-scope section): the INVERSE claim ("cluster scope is required when watchNamespaces is empty") corrected — empty DERIVES under namespace scope; cluster-wide watching now requires explicit cluster scope. The redundant "combine with watchNamespaces=<release-ns>" advice replaced by the derivation statement + the crashloop cite. (r7 then tightened the "additional namespaces your RBAC covers" phrasing to the guard's own rule: under namespace scope the chart's RBAC covers the workspace namespace ONLY — additional namespaces need operator-created RoleBindings.)
3. **`docs/reference/cli.md:69`** (flag table): the identical `""`/`"*"` = cluster-wide falsehood — the r6 commit's first push said "not applicable" from a wrong-path grep (docs/cli.md instead of docs/reference/cli.md); the real site was found and fixed one amend later. The r6 commit message as first written therefore claimed "checked, not applicable" while the final diff APPLIES the fix — corrected in the message (r7).
4. **PROCESS (the r6 record):** this worklog entry itself was missing at r6 — my r6 notify said "worklog's r6 note rides in the commit," which was false (nothing rode; the entry did not exist). Recorded here per the counted-claims rule.

## r7 — the multi-tenant prescriptions (the family's most consequential survivors)

1. **`docs/operator/multi-tenant.md` "Combining with watchNamespaces" + "How to deploy per-tenant namespaces":** both prescribed `rbac.scope=namespace` + multi-tenant `watchNamespaces` with no RoleBinding qualification — that config RENDERS but CrashLoops at runtime (the chart's workspace Role binds only in the workspace namespace; the watched tenant namespaces' informers hit Forbidden LIST/WATCH — the run-35872827066 death class). Both sites now carry the load-bearing caveat: the chart creates NO per-tenant RoleBindings; bind the workspace Role in every listed namespace yourself.
2. **`docs/reference/helm-values.md` + `multi-tenant.md` scope tables:** "scoped to the release namespace" → the workspace-namespace geography (the `api.config.kubernetes.namespace` override); the storageclasses ClusterRole misattribution fixed (read-only, ALWAYS created regardless of scope — rbac.yaml:186).
3. The r6 commit message amended to the true cli.md story; the orchestrator's directive file (accidentally swept by an add-all) removed from the tree.
