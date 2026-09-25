# Worklog: M3 — the posture gate workflow (design 0061 §5)

**Date:** 2026-09-23 (r1–r27 fixes: 2026-09-24/25)
**Session:** Design 0061 implementation, M3 lane (the gate itself): `.github/workflows/posture-gate.yml` + `local/posture_gate_workflow_test.go` — seven structural pin tests + the two golden inventories — + the README-LLM contribution rule (the M3 AC's fourth clause). Merge sequenced per the #1548 recorded order.
**Status:** r27 fixes pushed (the sweep complete); awaiting re-review.

---

## Objective

One CI job that cold-installs the chart under its OWN mandated default posture on kind and asserts the posture is livable: the four §5 assertions in order (all-Ready, the armed line, zero forbidden, the commit-stamp envelope), on every PR touching the posture surface (§11 ruling-3 breadth: `helm/**` + `controller/**` + `api/**`).

## Work Completed

- **`.github/workflows/posture-gate.yml`** — the e2e-nightly bootstrap reused verbatim (kind v0.32.0, helm, the `lss-e2e-registry` digest-pin registry, the 2-node `kind-cluster-nightly.yaml` topology, the stamped image-build chain, cert-manager, the credentials Secret, test Postgres/Redis), plus the router image built from tree under a local ref (the drill-shape precedent — kind cannot pull the ghcr default; the override is environmental image plumbing, not posture).
- **The install** carries ONLY environmental overrides (image repos/tags/pullPolicy, delivery pins, `mcp.enabled=false` (issue #28 — no image exists), test DB/Redis, logging verbosity). Every posture lever — `rbac.scope`, `relayOnlyKeyDelivery.enabled`, `agentdSidecar.enabled`, `allowRelayRouterEgress` — reaches helm UNTOUCHED at its shipped default. If the shipped defaults cannot go all-Ready, the gate is red. That is the point.
- **The four assertions, in order, as separate unconditional steps** (r1: each hardened — see the round record).
- **`local/posture_gate_workflow_test.go`** — seven structural pin tests plus the two golden inventories, mutation-checked every round since r0 (see the round records). [r21: this line said "six structural pins" for three consecutive rounds while two round records claimed it fixed — the Work Completed bullet, not the Session header, was the surviving copy.]
- **README-LLM** — the contribution rule subsection (the M3 AC's "contribution rule lands in README-LLM" clause): a PR flipping any multi-component default must include posture-gate evidence covering the new posture.

## r1 round record (the review EXECUTED the gate on a live kind cluster)

The review's live run validated the bootstrap + install wiring end-to-end and the mechanics of assertions 2/3/4 against real controller logs — and caught real defects, all fixed this round:

1. **Assertion 1 used nonexistent kubectl syntax** (`rollout status deployment --all` → `error: unknown flag: --all`, verified live twice). Fixed to the verified idiom (`kubectl wait --for=condition=available deployment --all -n <ns>`), and pin (b) now enshrines the VALID literal.
2. **`helm --wait` returned rc=0 mid-crashloop** (a transient Available window; `RESTARTS 3 (36s ago)` observed with install success). Fixed: assertion 1 gained a stability window — Available re-asserted after a 45s settle, and restart snapshots before/after must be IDENTICAL (a crashlooping pod cannot stay quiet 45s; a healthy cold install that restarted during first-boot arming records no NEW restarts). The install step comment now states that `--wait`'s rc is not the verdict; the assertions carry it.
3. **Pin gaps (four mutations the r0 pins did not catch — reviewer-verified):** (a) assertion 4's comparison could be gutted while the FAIL-echo retained the literal → pin (d) now requires the comparison shape `!= '${{ github.sha }}'`; (b) the llm-relay halves of assertions 1/3 could be deleted → their literals now name BOTH namespaces; (c) silent-disarm mutations (job-level `if:`, `continue-on-error`, `pull_request.types`/`branches` filters) → pin (f) bans continue-on-error on every step and `if:` on the job, pin (a) requires the pull_request block to carry exactly the `paths` key; (d) `2>/dev/null` swallowed per-pod log-fetch failures and `--previous` was absent → a fetch failure is now itself a red verdict ("a pod whose logs cannot be read cannot be cleared") and prior crashed containers are grepped too.
4. **The honest dependency record** (the "Blockers: None" correction — see Blockers below).

**r1 mutation self-checks (my own, post-fix):** deleting a single llm-relay wait line from assertion 1 leaves the pin green — because the stability re-check still carries the both-namespace scope (verified: fully gutting BOTH llm-relay waits fails pin (b)); removing `--previous` fails; gutting the assertion-4 comparison fails; adding `continue-on-error: true` fails.

## r2 round record (review validated the design sound; five findings, all fixed)

r2 confirmed every r1 fix live-verified (the wait idiom against kubectl's own docs, the stability window against the chart's hook-delete policies — no legitimate pod-churn source false-positives the restart snapshot, the render under the gate's exact flags, 7/7 mutation catches). Findings and fixes:

1. **Pin (c) was bypassable (the r0 skeptical-reviewer's values-file note — my r1 miss):** a `--values posture-override.yaml` (or `--set-json`) smuggled whole posture overrides past the `--set` ban list, and `--set controller.watchNamespaces=<ns>` would silence the exact #1555 crashloop class the gate exists to catch. Fixed: `--values`, ` -f `, `--set-json`, and `watchNamespaces=` all banned on the install run block.
2. **`set -euo pipefail` was unpinned:** deleting it from an assertion block neuters every check with zero literal drift (failed kubectl waits continue; a stuck-not-ready deployment prints OK). Fixed: pin (f) requires it as the FIRST statement of each assertion block.
3. **Assert 3's pod-LIST fetch was invisible to `set -e`** (process-substitution feed): a namespace whose pods cannot be enumerated was silently cleared. Fixed: `PODS=$(kubectl …)` under `set -e`, loop fed via herestring; the failure-checked-fetch shape is pinned.
4. **Assert 4's `-z` diagnostic was dead code** (pipefail killed the step before the branch). Fixed: failure-checked log fetch to a file, extraction with `|| true` so the unstamped-artifact diagnostic actually fires. Assert 2 gained the same failure-checked fetch pattern for consistency.
5. **The dependency record mislabeled M2/M4** (r1's own correction was itself wrong): #1557 is the **M4** PR; **no M2 PR exists yet** (open-PR scan: M1 #1553, M3 #1556, M4 #1557). Corrected here and in the PR body.
6. (Minor) README-LLM.md:300's structure block said `charts/` — corrected to `helm/` while the file was open (the zero-pre-existing-errors rule).

## r3 round record (three one-liners on a validated foundation)

r2's verdict: design sound, every mechanical fix since r0 verified, 5/5 new mutation classes caught — remaining: (1) the PR body still carried the M2/M4 mislabel while this worklog claimed the correction had landed there (my r2 `sed` on the body had silently missed the bold-marked line — the replace-verification lesson, again; fixed with an asserted Python replace, grep-verified in the live body); (2) the ` -f ` ban missed the pflag `-f=<file>` form — `-f=` banned too; (3) the `set -euo pipefail` pin checked presence, not persistence — a later `set +e` countermanded it under the pinned prefix — `set +e` and `set +o pipefail` are now banned in assertion blocks. Plus the two offered polish items: the README-LLM tree comment-column alignment, and `permissions: contents: read` on the workflow (the r3 security hardening note — the job needs nothing more of the token).

## r4 round record (completeness closes + the live-scan discipline)

r3's closes were incomplete within their own classes (both mutation-demonstrated by the review): the exact-spelling `set +e` ban missed `set +o errexit`/`set +o nounset`/double-space variants — now the whole countermand family is banned against WHITESPACE-NORMALIZED run text; the literal ` -f `/`-f=` bans missed the tab-delimited `-f<TAB>file` form (tab is IFS whitespace — a live smuggle) — replaced by the regex `(^|\s)-f[\s=]` over the install run block. Plus r4's optional finding adopted: Assert 1's `kubectl wait` lines must be bare (`|| true` appends now red). And the record-accuracy lesson landed for good: r3's push asserted `#1555 (OPEN)` and `M4 (#1557, open)` AFTER both had flipped (merged 01:50, closed-unmerged 02:05 vs push 02:08) — the r4 refresh above is a LIVE scan at write time, and the worklog now states scan time.

## r5 round record (the structural close — the spelling war ends)

r5's twelve live one-liner escapes (shell/helm-proven by the review and its skeptical sub-agent, each cross-validated) demonstrated the whack-a-mole failure mode: five rounds of exact-spelling closes, each falsified within one round. The required close was structural, and is now in:

1. **The `--set` ALLOWLIST** (pin c's core): every `--set` key parsed out of the install (continuations joined, quotes stripped, key = value-before-first-`=`) must be on the environmental allowlist; every allowlist entry must be used (dead entries are drift); the parse must find the full set (≥20 keys — a silently empty parse is a vacuous pass). Quote-split (`--set rbac.scope"=cluster"` → key `rbac.scope"`), variable indirection (`--set "${KEY}=cluster"` → key `${KEY}`), and any future lever fail in ONE check regardless of spelling.
2. **The values channels banned outright**: `-f` in ANY spelling (regex `(^|\s)-f` — attached, delimited, tab, continuation), `--values`, `--set-json`, `--set-string`, `--reuse-values`, and `--post-renderer` (a whole-manifest rewrite channel, helm-proven live).
3. **Shape-pinned wait lines**: each Assert-1 `kubectl wait` line must START the line bare (no `!`/`if`/assignment prefix) and carry no shell operator (`||`, `&&`, `;`, backtick) and no continuation.
4. **The countermand family, structurally**: normalization now joins backslash-newline continuations (the `set +o\`+newline+`errexit` form), and the bans are `set +o` as a PREFIX (all long forms), bare `+o errexit`/`+o pipefail`/`+o nounset` (the mixed `set -e +o pipefail` form), `set +e`, and `trap ` (a `trap 'exit 0' EXIT` is a complete neuter that is no set-spelling at all).
5. **The residual threat model, stated** (pin-file header): the pins deter ACCIDENTAL DRIFT on maintainer PRs — not adversarial shell evasion (eval, function overrides, PATH shims, chart rewrites are out of scope; the reviewed diff is the control for those). The pin comments no longer claim class-completeness beyond this.

All twelve r5 escapes re-verified red by my own mutations after the close (S1–S6 install-channel; A1–A6 assertion-disarm). The worklog's stale duplicate merge-call line (r5's minor finding) is deleted.

## r6 round record (four root causes closed + one refuted with executed evidence)

r6's mutation gauntlet (41 mutations, review + skeptical sub-agent cross-validated) found ten more green escapes converging on four root causes. Disposition:

1. **Root cause B — the comma multi-set channel (REAL, closed):** helm accepts `--set a=1,b=2`; a key-before-first-`=` parse is blind to the tail — `--set "mcp.enabled=false,freeModelsRefresher.enabled=false"` passed every pin. `extractSetKeys` now splits each value on commas and validates EVERY segment's key against the allowlist (mutation re-verified red).
2. **Root cause C — the shape pin's entry condition (REAL, closed):** a double-space `kubectl   wait … || true` skipped the `Contains(line, "kubectl wait")` guard entirely — the guard, not the ban, was the hole. Lines are whitespace-collapsed before matching (re-verified red).
3. **Root cause D — unpinned payloads (REAL, closed):** `sleep 45` → `sleep 1` (the window's duration is the crashloop catch's teeth) and the `forbidden` grep pattern were pinned nowhere. Now pinned — the grep on BOTH files (a Contains-anywhere pin passed while only the current-container grep was swapped; the partial swap neuters half the detector — re-verified red both partial and full).
4. **`exit 0` (REAL, closed):** strictly larger blast radius than the trap ban's class — it neuters the restart-diff too, re-opening the #1546 Defect-1 path (crashlooping router, zero forbidden, armed controller) through every remaining assertion. Banned in all assertion blocks; failure paths use `exit 1` only (false-positive-free).
5. **The env-carrier channel (REAL, closed):** a step-level `env:` block feeding `${GATE_EXTRA}` into the helm line evaded every run-text ban. The install step's env block is now required EMPTY (parsed via the step's `env` key).
6. **Root cause A — the backslash-newline join (CLOSED BY THE BELT VIEW; the extra-indent form is fail-closed — this corrects r6's own "REFUTED" claim, which r7's review counter-demonstrated):** a continuation line at the BLOCK-MINIMUM indent is valid YAML, and after YAML strips the block indentation bash joins `se\`+newline+`t +e` with NOTHING — `set +e` executes (errexit verifiably off). The extra-indent form leaves the indent as separator whitespace (argv splits, `unknown flag` / `command not found` → fail-closed red — my executed test at `/tmp/opencode/jointest/t.sh` demonstrated this form correctly). The `banViews` empty-join view models bash's join exactly, so the block-minimum form goes red under the pins; the extra-indent form is pin-GREEN but fail-closed at RUNTIME (argv splits — `unknown flag` / `command not found` → red gate). The class is closed — by the belt view for the live form, by fail-closure for the inert form — not refuted. The r6 record's "REFUTED for this context" claim was wrong and is corrected here.

## r7 round record (the committed-mutation incident — owned, reverted, pinned)

r7's review found the r6 head BROKEN: my root-cause-A debug mutation (`--valu\`+newline+`es=/tmp/posture-override.yaml` on the install head line) was left IN the committed workflow. Root cause of the incident, on the record: my r6 mutation gauntlet's baseline (`/tmp/opencode/pg6b.yml`) was captured AFTER the debug mutation was applied — every subsequent "restore" faithfully re-applied the broken state, six rounds of pins stayed green over it (the tokens never form a banned shape), and `bash -n` passes it (syntactically valid). The review executed it end-to-end: `helm: unknown flag: --valu` — the gate was dead on every tree. This is the exact "final-tree state, not the syntax check, is the instrument" lesson.

Fixes this round:
1. **The revert** — the head line restored; a full audit (diff r5→r6 head + stray-text grep) confirmed the single stray mutation was the only one.
2. **The regression pin** — the install's FIRST command line must be exactly `helm upgrade --install llmsafespaces helm \` (the head-of-line shape pin; the append class defeated every ban view, the allowlist parse, and the `-f` regex at once).
3. **Continuations banned in Assert 1** (the sub-agent's split escape: `kubectl \`+newline+`wait … || true` never enters the shape guard).
4. **Job-level env required empty** (the step-env close pinned only the step; the job-level carrier smuggled a values file to a live render).
5. **Inversion pins** — Assert 3's detector greps pinned in the positive `if grep` shape (a `!`-inversion kept every literal while inverting green/red), and its fetch guard pinned as `if ! kubectl` (negation removal = silent pass on fetch failure).
6. **The record corrections** — the false REFUTED claim fixed (above); the dependency record refreshed from a live scan: **M1 (#1553) MERGED 03:27Z; M2 now exists — #1559, OPEN; the defect fix #1558 OPEN; M4 (#1557) merged**. The gate's remaining red-drivers: #1558 and (per the recorded order) M2.

All five r7 mutation classes (head-append, split-wait, job-env, bang-inversion, fetch-guard-denial) re-verified RED by my own mutations; the pristine baseline is md5-verified after the gauntlet. Mutation-hygiene rule adopted in practice: the baseline is checksummed before every gauntlet and verified after it.

## r8 round record (eight adjacent-shape escapes closed)

r8's review confirmed the r7 closes real (29 mutations, 23 RED spot-checks; the head-of-line pin re-caught the exact incident replay; the revert verified by empty diff and a live rc=0 render) — and found eight more GREEN escapes, all small test-side closes, three of them one line short of classes r7 itself declared:

1. **Assert 2's detector line pinned whole** (`if ! grep -F '…' /tmp/gate-armed.log`) — polarity AND target: dropping the `!` inverted green/red; swapping the file to the empty-on-healthy stderr capture vacated the check.
2. **Assert 3's loop FEED pinned** (`done <<< "$PODS"`) — without it the pinned fetch is dead code (read consumes empty stdin, both namespaces silently cleared); the process-substitution form stays banned by absence.
3. **The WORKFLOW-level env allowlisted** (exactly {CLUSTER_NAME, IMAGE_TAG, NS}) — the carrier channel was closed at step and job levels but open one tier up; the review live-proved a `GATE_EXTRA` watchNamespaces smuggle rendering into the deployed controller.
4. **Assert 1's AFTER-block pinned** (`AFTER_NS=$(restarts_snapshot`) — deleting the comparison block left a 45s delay with every literal green (the gut-the-comparison class r1 closed for Assert 4).
5. **`break`/`continue` banned** in assertion blocks — `exit 1` → `break` in the detector branch fires the grep, abandons the loop, prints OK.
6. **The install's operator surface banned** (`&&`, `;`, `|`, backtick, `$(`) — the nightly's shape is ONE operator-free command; a tail append (`--wait … && kubectl set env …`) opened a post-install channel no allowlist saw.
7. **The head-line pin compares RAW** — TrimSpace defeated it for the escaped-space spelling (`… \ ` — helm gets a positional arg, install fails on every tree, all pins green; live-proven rc=1).
8. **The worklog overclaim corrected** — "BOTH forms go red under the pins" was wrong: the extra-indent form is pin-green and fail-closed at runtime; the record now says exactly that.

All eight re-verified RED by my own mutations (F5's first attempt hit the registry step's exit 1 — outside the pinned scope by design; the redo targeted the assertion block). Pristine baseline checksummed before and verified after the gauntlet.

## r9 round record (the uniform close: exact-line pins)

r9's review verified all eight r8 closes by mutation — and found the sibling-spelling level: a `&& false` suffix on a verdict condition, a `!=`→`==` polarity flip, a newline-separated second command after the wait line, a `PODS=""` blanket between the pinned pieces, a deleted 60s re-check pair, and an assertion-step env block — every one GREEN, several live-demonstrated. The uniform close the review prescribed, now in:

1. **Every verdict-bearing line is pinned as an EXACT trimmed line** (no sibling spellings exist): the four waits, the stability comparison, Assert 2's fetch+detector, Assert 3's fetch+both greps+loop feed, Assert 4's fetch+both verdict branches.
2. **The install tail shape-pinned**: the block's last command line must be exactly `--wait --timeout 10m`, and every non-comment line before it must end with a continuation backslash — the install is one command, no newline-separated seconds (a live-demonstrated post-install RBAC-patch channel closed).
3. **Exactly one `PODS=` assignment** in Assert 3 (a blanket reassignment relocated the r2 silent-skip one line below every pin).
4. **Exactly four `kubectl wait` lines** in Assert 1 (the 60s settle-window re-assertion is part of the verdict, deletable around the 300s literals).
5. **No step may carry an env block** (the carrier channel was closed at workflow/job/install levels; assertion-step env could re-scope the checks).

All seven r9 classes re-verified RED by my own mutations; pristine baseline checksummed before and verified after. [CORRECTED r14: the r9-era claim that exact lines "have no siblings" was true only line-locally — sibling CHANNELS (tail appends, rebinds, injections between lines) kept being found through r13; the statement inventory is what made it hold. Kept for the record with this annotation.]

## r10 round record (a verdict is a block, not a line)

r10's review (63 mutations, review + skeptical sub-agent) found the block-below-the-line surface: the verdict INPUTS (r9 dropped the r8 snapshot anchor when moving the comparison to an exact line — an `AFTER_NS="$BEFORE_NS"` alias silently neutered the restart-diff), the verdict CARRIERS (deleting a FAIL branch's `exit 1` turned it into echo-and-continue, five for five), and the fetch-to-detect gap (`--previous=false` satisfied the Contains literal; the one unpinned fetch line accepted `&& false`). The sub-agent added NS-rebinding, a stray `read -r _` swallowing every other pod, and a second capture-file truncate. All closed:

1. **The four snapshot assignments exact-pinned** (the verdict's inputs, by name).
2. **`exit 1` floors per assertion** (1/2/3/3) — the conditions were r9's close; the carriers are r10's. A floor, not an equality: additional fail-closed branches are legitimate drift, fewer is a neuter.
3. **The prev-fetch guard exact-pinned** (`--previous=false` and the `&& false` suffix both red now).
4. **NS rebinding banned in assertion blocks** (`(?m)^\s*NS=` — the env ban closed the parsed carrier; assignment rebinding was the same class one level down).
5. **Exactly one `read -r` consumer, and each capture file written exactly once** in Assert 3 (the stray-reader and truncate rewires).

All ten r10 classes re-verified RED by my own mutations; pristine baseline checksummed before and verified after. [CORRECTED r14: the "named drift shapes" list was incomplete one tier down — r11 found the producer/extraction inputs, r12 the variable-rebind and injection spellings, r13 the export/declare forms. Each round's "All closed:" claim is HISTORICAL — accurate for its round's named classes, falsified by the next round's siblings. The statement inventory (r13) is the close that ended the pattern.]

## r11 round record (the inputs one tier deeper, the comment-padding neuter, and the injection channel)

r11's review (~68 mutations, review + skeptical sub-agent) found the verdict inputs BELOW r10's pins: the snapshot PRODUCER (a constant producer — jsonpath typo evaluating empty, or an appended `| head -n 0` — makes BEFORE and AFTER equal by construction; the comparison can never fire), Assert 4's extraction (gutted to a no-op grep + a pinned-sha assignment — the comparison unfireable with every literal green), the exit-1 floor counting `# exit 1` comments, capture-file injection on Assert 2/4's files (an `echo <expected> > file` between fetch and detector is always-green), and two rebind spellings (`GATE_NS=` in the loop — both iterations scan llm-relay, the release namespace never checked; `export NS=`). All closed: [CORRECTED r15: "All closed" is HISTORICAL — r12's own record documents that five of its eleven escapes were inside classes this round declared closed (the write-count single spelling, the HasPrefix count scope, the loose FAIL-context, the POD variable, the duplicate producer).]

1. **The producer's jsonpath line exact-pinned** and **exactly-one counts on all four snapshot assignments** (a duplicate AFTER the window re-snapshots past the sleep).
2. **Assert 4's extraction first line exact-pinned** (the comparison's input, r10-RC-A precedent — Assert 1's inputs were pinned, Assert 4's left open).
3. **The exit-1 floor counts non-comment lines only.**
4. **Write-count pins extended to `/tmp/gate-armed.log` and `/tmp/gate-controller.log`** (exactly one write each).
5. **`GATE_NS=` and `export NS=` banned** alongside the r10 `NS=` rebind ban.

All nine r11 mutation REPLAYS re-verified RED — nine replays of the review's five named findings, not nine distinct closes [CORRECTED r15: the count was mine, from the J-series gauntlet; the section lists five closes] (J1a's first sed misfired on the pipe character — the python redo verified applied-and-caught); pristine baseline checksummed before and after.

## r12 round record (the assertion-block spelling war ends the same way the install's did)

r12's review (~68 mutations again) found eleven escapes, five inside classes r11 declared closed — the precise pattern: verdicts are BLOCKS, and any line inserted between pinned lines escapes. Closes:

1. **Semantic assignment counts** for the verdict variables: `BEFORE_NS`/`BEFORE_RELAY`/`AFTER_NS`/`AFTER_RELAY` each assigned exactly ONCE, any spelling (the exact-string count missed an interior-space sibling; the semantic count is immune), and `RUNNING_COMMIT` exactly once (a rebind below the pinned extraction unfired both provenance verdicts — the variable-rebind class completed for every verdict variable: PODS r9, snapshots r10/r11, RUNNING_COMMIT now). [CORRECTED r14: "completed" was premature — r13 found the export/declare spellings; the semantic Contains counts closed them.]
2. **The extraction TAIL exact-pinned** (`|| true` → `|| echo '<sha>'` converted the unstamped-artifact red into a pass).
3. **Write tools banned in assertion blocks** (`tee`/`cp`/`dd`/`mv`/`sed` at line start or after a pipe/semicolon) — `echo … | tee file` evaded the `>` write-count close.
4. **`exit 1 &` banned** (backgrounded exits abandon the verdict) and **every exit-1 carrier must sit in a FAIL branch** (a FAIL echo within three lines above) — the dead-branch floor laundering closed [CORRECTED r14: r13 RC-C falsified this round's form — the `if false` ban and the comment-proof FAIL-context are the actual close].
5. **`sleep 45` exact-pinned** (`sleep 45 &` returned immediately with the Contains literal intact — runtime-proven 0.0s elapsed).
6. **The rebind ban completed**: `export`/`declare -x` forms of NS and GATE_NS rebinding banned alongside the plain assignments.
7. **`--set-file` and `--set-literal` banned** on the install (`--set-file` is a REAL lever — freeModelsRefresher drives the RBAC render; the -f regex cannot see inside the flag).

All eight r12 mutation families re-verified RED (K2's exact-string count missed the interior-space sibling — caught by my own gauntlet, fixed to the semantic count, re-verified); pristine baseline checksummed before and after.

## r13 round record (the statement inventory — the assertion blocks' allowlist moment)

r13's review found seven more escapes, three inside r12's own declared classes, and named the three-round-old lesson outright: the r5 allowlist ended the install channel's war because it is SEMANTIC; the assertion blocks had no analogue. They do now:

**`TestPostureGate_StatementInventory`** — the four assertion blocks are pinned as COMPLETE golden inventories: every non-comment line, in sequence. Any insertion, deletion, or sibling-spelling edit fails in one check regardless of spelling (the thirteen rounds' every escape class — injections, rebinding spellings, dead-branch padding, duplicate producers, backgrounded sleeps — is an inventory diff). Legitimate changes to a block update its golden deliberately; the diff shows exactly what changed.

Semantic backstops kept as belt (and because they document the WHY): Contains-scoped exactly-once assignment counts for the snapshot variables (export/declare forms included — HasPrefix had missed them); regex-scoped write counts for all four capture files (any redirect spacing — `>file` and tab forms escaped the single-spelling match); exactly-one producer definition; the POD loop-variable rebind ban; the `if false` dead-branch ban; and the FAIL-context tightened to a real `echo "FAIL…` line (a comment containing FAIL satisfied the loose form).

All seven r13 classes re-verified RED (each caught twice — inventory and backstop); pristine baseline checksummed before and after. The write-tool ban's comment no longer overclaims: its prefix class misses `&& tee` (fail-closed payload per the sub-agent's analysis); the INVENTORY is the close for that surface.

## r14 round record (the third spelling of the install operator channel, and the record pass delivered in place)

r14's review (47 mutations) delivered the verdict the thirteen rounds were building toward: **the first round with zero assertion-block escapes** — every r13 replay and all fourteen prior classes RED. One escape remained (install-channel): the process-substitution spelling `< <(kubectl patch clusterrole …)` inside the continuation chain executed mid-install and evaded the r8 operator ban (r9 closed the newline twin). Closed: `<(`, `>(`, and `&` added to the install's banned operators (mutation re-verified red).

The record pass, delivered IN PLACE this time (the r13 narration-without-correction lapse): the falsified claims in the r9–r12 sections are annotated where they stand ("[CORRECTED r14: …]" — history kept, the falsehood no longer uncorrected; r15 completed the pass: the r11 section's two claims, missed by the r14 pass, are now annotated too); the "latest scan: r7" label that survived five rounds of flags is replaced by a live r14 scan; and the material state change is recorded: **#1558 (the defect fix) MERGED 16:21:22Z** — the gate's primary red-driver has landed, M2 (#1559) is the one remaining recorded-order predecessor, and the merge framing now reads "the gate could now run green for the first time" rather than "the gate is red by design." The PR body's sequencing section carries the same refresh.

## r15 round record (the view inconsistency, closed with the mechanism the file already had)

r15's finding 1 was an inconsistency, not a new mechanism: the operator ban ran over RAW install text while the lever bans and the -f regex in the SAME test ran over `banViews` — whose empty-join view exists precisely because bash removes backslash-newline with no space. The composed spellings (`<\`+NL+`(`, `>\`+NL+`(`, `$\`+NL+`(`) executed live substitutions mid-install with all pins green (not silent-green — helm fails arg validation — but the r8-finding-7 pin-green-dead-install class, the fourth escape round on this channel). Closed: the operator ban now runs over every `banViews` view; the double-backslash-newline spelling (`\\`+NL — an escaped backslash that ends the command) joins the ban per the adjudicated finding; and `extractSetKeys` parses BOTH join views (the symmetric close for the split-`--set` parse — posture-neutral per the adjudication, but symmetry is cheap). Findings 2+3: the r11 section's two missed annotations are delivered, and the r14 record's completeness claim now states the pass it actually made.

## r16 round record (the chain-integrity pin, the fourth view, the CR ban)

r16's milestone verdict: **the first round in sixteen with no false-green escape constructible** — every pin-green mutation both passes found fails the gate at runtime. The remaining findings were sibling spellings of the dead-install class (pin-green, runtime fail-closed): the blank-line/comment-line/CR continuation amputations and the extra-indent composed operator. Closed:

1. **CHAIN INTEGRITY** — every RAW line strictly between the install's head and tail must end with a backslash, with mid-chain comments banned outright (a comment amputates regardless of its trailing backslash — bash ends comments at the newline).
2. **The fourth view** — the operator ban gains the EMPTY-JOIN-then-whitespace-stripped view (in that order: strip whitespace first and the backslash survives between the operator characters — my own first attempt had it backwards and its gauntlet caught it); the extra-indent composed spelling forms there.
3. **The CR ban on the RAW file** — YAML normalizes CRLF inside the parsed scalar, so a `\`+CR+newline continuation is invisible post-parse; any CR byte in the workflow is corruption.
4. **The `defaults:` ban** (the optional close, adopted): a workflow-level defaults.run.shell re-scopes every step silently.

All five r16 classes re-verified RED (blank line, mid-chain comment, CR corruption, extra-indent composed operator, defaults block); pristine baseline checksummed before and after.

## r17 round record (the trailing comment, the shell tier, the GITHUB_ENV channel, the -f fourth view, the trigger/permissions pins)

r17 confirmed the second consecutive no-false-green round; the findings were siblings and tiers of r16's own closes, all runtime fail-closed or verdict-neutral. Closed:

1. **No `#` anywhere in a chain line** — the trailing mid-line comment (`--set … # keep \`) amputated the command with the backslash inside the comment (helm received argv without --wait; argv-dump verified); the prefix-only comment ban caught only full-line comments.
2. **The shell tier closed**: job-level `defaults:` and step-level `shell:` banned (r16 banned the workflow tier; the r9 tier-inconsistency class).
3. **The `$GITHUB_ENV` channel pinned**: exactly the four delivery-pin writes, exact-pinned lines (any other runner-env write is drift — the r8 env-exact-map class one tier down); the install's `-n $NS --create-namespace` line exact-pinned; and the `$IMAGE_TAG` references in the install are now QUOTED (the word-split amplifier the review argv-proved).
4. **The `-f` regex gained the fourth view** (empty-join-then-stripped — the sibling operator ban's r16 order).
5. **The top-level trigger set exact** ({workflow_dispatch, pull_request} — an added push trigger gated unconfirmed surface) and **the permissions block exact** (contents: read — r3's least-privilege note finally pinned).
6. The Blockers scan label refreshed (live scan r17: M2 #1559 the sole remaining recorded-order predecessor; #1558/M1/M4 merged, unchanged from r14's content).

All eight r17 classes re-verified RED; pristine baseline checksummed before and after.

## r18 round record (the #1566 handoff: the gate's first true execution caught the gate itself)

The gate ran for real on w2's #1566 (its api/** changes trigger the paths) — and its FIRST-EVER execution of Assert 1 caught a defect in the gate: the restart-snapshot jsonpath used `{/end}` (invalid — the terminator is `{end}`), so the stability step could never run. My authoring error from r0, pinned verbatim by r13's golden; sixteen rounds of mutation war never executed the line (the reviews' live runs stopped at r0's earlier syntax failure). w2 fixed it on their branch (8e1a85bd + the pin unfreeze 5f214d8c); both ADOPTED here — Assert 1 passed for the first time ever (all-Ready + 45s stability, live on kind).

**The remaining red — Assert 2 — probed with the run's own evidence (run 36053803876):**
- **Suspect (a) REFUTED**: the failure dump shows controller RESTARTS=0 — there IS no previous container; the armed line was never emitted by any container (restart-laundering cannot be the story THIS run).
- **Suspect (b) sharpened by reading the tree**: SetupRelayStaging has NO silent nil path for enabled=true (every path emits the armed line or returns an error → the refusing-to-start exit); the call at main.go:353 is unconditional; registerRelayFlags() precedes flag.Parse() (main.go:220-221); SetLogger precedes the call (231 < 353 — no delegating-sink discard). A healthy, 0-restart, no-line pod with the flag ON THE SPEC is impossible on this tree — it is the wrong-bits residue §5 r1 names (stamp≠behavior, the 0.34.7 class) until another explanation lands.

**The Assert 2 fix (an M3-layer correctness fix — the gate was asserting the wrong thing):**
1. **The deployment TRUTH first**: the pod spec's args must contain `--relay-only-key-delivery=true` — absent → the render lied (chart/values defect, named); present + healthy + no line + no exit-85 → wiring drift, NAMED as such. The gate now discriminates the two defect classes instead of conflating them.
2. **Both containers consulted**: the armed line is a boot-time emission; a post-arming restart (which the gate's own stability window LEGITIMATELY tolerates) moves it to the previous container — Assert 2 now greps current AND previous (Assert 3's --previous rule, one tier up). The r18 mutation checks: flag-check deletion, previous-consult deletion, and the `{end}` regression all RED (each caught twice — inventory and backstop).

The stability window's first live contact: PASSED (45s, no new restarts, on a genuinely all-Ready cold install). w2 rebases #1566 onto this head; both PRs' gate runs should then agree.

## r19 round record (the vacuous view removed, the convention pinned, the count de-spelled, the record uncontradicted)

r19's review live-verified the r18 story (the `{/end}` parse error proven, the run's step list confirmed: Assert 1 SUCCESS for the first time) and found four findings:

1. **The r17 `-f` "fourth view" was VACUOUS** — whitespace-stripping destroys the `(^|\s)` boundary, so the regex could never fire on that view; the inert extra-indent split was pin-green. Removed the vacuous view; the honest close for the inert class is the outright ban on `-\` line endings; view 3 remains what catches the LIVE block-minimum splits (the comment and this record now say exactly that).
2. **The quoted tag values exact-pinned** — the r17 quoting was convention-only; unquoting was pins-green.
3. **The GITHUB_ENV count regex-scoped** — a single-`>` truncate write evaded the exact-string count; the channel is the pinned surface, not one spelling of it.
4. **The record uncontradicted** — the three stale lines (stability-window "awaits first contact" in Tests Run/Blockers/Next Steps) corrected to the passed r18 contact; the inner scan labels refreshed (r19: M1, M2, M4, #1558 ALL MERGED — the recorded order is clear for the gate; #1566 rebases onto this head); "six structural pins" → seven.

All three r19 mutation classes re-verified RED; pristine baseline checksummed before and after.

## r20 round record (the channel count, the install golden, the duplicate-key ban)

The fourth consecutive round with no verdict-flipping escape constructible; the findings were the r19 closes' own siblings. Closed:

1. **The GITHUB_ENV CHANNEL pinned**: exactly four `GITHUB_ENV` occurrences in the whole file, any spelling — unquoted, `tee -a`, indirection (`ENVF="$GITHUB_ENV"`), single-`>` — a fifth mention of the channel is drift by definition (the r19 regex count was spelling-scoped against its own stated principle).
2. **The install block joins the golden treatment** (the r13 mechanism, prescribed for this block since r13): every non-comment line pinned in sequence — the mid-chain `--valu\`+NL+`es=` split class closes structurally, not by spelling; the head/tail/chain/operator/allowlist pins remain as documented backstops.
3. **`(?m)-\s*\\$`** — the inert-split ban covers dash-with-whitespace (`- \` escaped the r19 dash-adjacent form).
4. **Duplicate `--set` keys banned** — each key exactly once per join view; a duplicate within a view is helm's last-wins override (parse deduped per view; the cross-view count is exactly two per key).

All six r20 mutation classes re-verified RED (unquoted/tee/indirect env writes, dash-space split, mid-chain valu split, duplicate key); pristine baseline checksummed before and after. The record: the stale Next-Steps scan line and the "six structural pins" header fixed in place; the PR body's sequencing section refreshed (all recorded-order predecessors merged).

## r21 round record (the vacuous dedup fixed, the continuation-pair rule, the record's own fix-claims audited)

r21's review proved the r20 duplicate-key count check VACUOUS — `extractSetKeys` deduped per view before appending, so the count was structurally always 2 (proven three ways, including the live mutation: a duplicate line left the containing test GREEN; only the golden caught it). Fixed: the extractor appends RAW per view (no dedup) and the count check fires in its own test — U1/U2 re-verified: a duplicate `--set` line AND a comma-level duplicate now fail `TestPostureGate_InstallShippedPosture` directly. The pin comment and this record now describe the mechanism that exists.

The assertion goldens' comment/blank skip left one pin-green fail-closed insertion point — Assert 4's continuation pair (a comment or blank after the `RUNNING_COMMIT=… \` line joins onto the command and dead-gates the block; bash syntax error). Closed: within assertion blocks, the line after ANY backslash-terminated line must be neither blank nor comment (the install block's r16/r17 chain-integrity rule, extended). U3/U4 re-verified RED.

The record's own fix-claims audited: the "six structural pins" Work Completed bullet survived two round records claiming it fixed (the Session header was edited, the bullet was not) — fixed now with the annotation; the Blockers label refreshed to r21 and the stale "though the recorded order names M2 first" parenthetical removed (M2 merged).

The fifth consecutive round with no verdict-flipping escape constructible; the two findings were a vacuous mechanism (the class the thread keeps catching in its own closes) and the one remaining golden-skip insertion point.

## r22 round record (the escaped-space raw check, the labels harmonized)

r22's review delivered a first: **every claim in the r21 round record audited true** — and one finding remained: the escaped-space spelling on Assert 4's continuation head (`… log \ ` — a trailing space after the backslash) passed every TrimSpace comparison and dead-gated the step on every tree (bash syntax error; the r8 install-head class, never extended to the assertion blocks). Closed (at r22 for space+tab — [CORRECTED r23: the claim said "whitespace" while the mechanism banned two spellings; NBSP, the paste-artifact member, stayed green until r23's rune-level close]): no RAW assertion line may carry backslash-then-whitespace. The three scan labels harmonized to r22 (Blockers preamble, item 2, Next Steps); the PR body's label refreshed with them.

## r23 round record (the whitespace class closed at rune level, file-wide)

r23's review found the r22 close two-spellings-wide of its own claim: `\`+NBSP (U+00A0 — the paste-artifact whitespace) passed every pin and dead-gated the step (`bash -n` rc=2), and the sub-agent extended the class past continuations entirely (NBSP after `fi`, NBSP on the install tail — same dead-gate, same green). Closed twice over:

1. **The raw-file ban extended from CR to ANY non-ASCII `unicode.IsSpace` rune** — NBSP, U+1680, U+2000–200A, U+202F, U+205F, U+3000 are corruption in this artifact, anywhere in the file (this is the one-edit close for the whole class, continuation-bound or not).
2. **The assertion-line check made rune-level** (`TrimRightFunc(unicode.IsSpace)` — fires for any trailing whitespace after backslash-terminated content; the r22 space+tab spellings were two members of it).

W1/W2/W3 re-verified RED (NBSP after the continuation backslash, after `fi`, on the install tail). The r22 record's overclaim annotated in place (the claim-vs-mechanism gap, one spelling wide — the same recurring class as r18's vacuous view and r20's vacuous count). The PR body's inner label refreshed.

## r24 round record (the label harmonization — the r5→r6 lapse pattern, named and closed)

r24's sole finding: the r23 commit edited the worklog carrying the scan labels without refreshing them (all three still read r22), and the body's header read "r22 refresh" above an inner "Live scan at r23" — the same one-section self-contradiction r20/r21/r22 each counted. Fixed: all labels harmonized to a fresh r24 live scan (content unchanged: M1/M2/M4/#1558 all MERGED; #1566 OPEN; no new recorded-order predecessors among {1562, 1564, 1567}). The pattern itself — editing the record without re-scanning it — is the r5→r6 lapse recurrence; the discipline going forward is that any commit touching the worklog refreshes the labels in the same edit.

## r25 round record (the milestone the record awaited had already happened)

r24's review brought the news the record was waiting for: **the gate has gone fully green, twelve times** (00:30Z–04:59Z 2026-09-25 on #1566's branch — SEVEN dispatched, FIVE pull_request-triggered [r26: the r25 record said "nine+ dispatched" — the phrasing inherited from the r24 verdict's own words, which counted both event types; corrected here for both]; all four assertions SUCCESS, runs of each type step-verified via the Actions API; that branch's four assertion run blocks are BYTE-IDENTICAL to this PR's, its additive peel being a helm version pin, an RBAC-binding assertion step, and a failure-dump step). The r18 Assert 2 lines are live-validated; the stability window has passed many times; the provenance assertion has passed live. The record refresh: Tests Run, Next Steps, and the body's merge-call paragraph now characterize the green runs accurately — and the r24 commit had edited the very section carrying the stale "await their first live cluster contact" line (the lapse the r24 record itself names, recurring one round later — named again here, honestly). The verdict's own words: one record pass and this is approvable on the current mechanism.

## r26 round record (the attribution made precise, the residue swept)

The r25 verdict verified the milestone real — TWELVE full-green runs (not nine+), and only SEVEN of them dispatched (five were pull_request-triggered; the "nine+ dispatched" phrasing was inherited from the r24 verdict's own words and adopted in good faith). The record states the attribution precisely in the four locations the r25 verdict named [r27: the totality claim "everywhere it appears" was one instance wide — Next Steps 3 still said "nine+" until this round; the claim is now scoped to the audited locations]. The two true-but-superseded residue lines in Blockers are swept with their supersession noted; the body's labels harmonized; the body's own surviving "should now be reachable" framing — the worklog twin of which was swept in r26 — swept in r27 with the same supersession note.

## Key Decisions

1. **Unconditional assertions.** The nightly's cancel-guard arming protects EVIDENCE lanes from unrelated row failures; here the install is the thing under test — a failed `helm --wait` already fails the job, and conditioning the assertions would only manufacture skip-paths around red gates.
2. **The stability window rather than zero-restarts-absolute**: a healthy cold install may legitimately restart the controller during first-boot arming waits (the 30s startup guard vs. router keypair availability); the invariant is NO NEW RESTARTS inside the window, which the crashloop class (sub-30s exit cycles) cannot satisfy.
3. **The provenance channel is the running binary's own startup line** (`pkg/version.CommitSHA`, the same ldflags channel the release stamps) — not a registry label lookup through the node's containerd, which would re-derive the same attestation with more moving parts on the kind path.
4. **Router from tree, local ref** (the drill-shape precedent): deterministic on PR runs, no external image dependency, and the router binary is this tree's code — the gate exercising it is coverage, not drift.
5. **`mcp.enabled=false` is environmental, not posture**: the mcp image does not exist (issue #28); a cold install would ImagePullBackOff on infrastructure that cannot ship. Recorded in the install comment and in pin (c)'s allowed list.

## Blockers

**The dependency record, live-scanned at r27 (2026-09-25 — each round re-scans at write time; two earlier rounds asserted states that had flipped before their pushes, the lesson that made the scans explicit):**

1. **The shipped-posture cache-scoping defect** — namespace scope + `watchNamespaces` unset → cluster-wide informer → forbidden → CrashLoop (56 denial lines in the r0 live run). **The fix (#1558, "namespace-scope cache-scoping derivation") MERGED 2026-09-24T16:21:22Z** — the gate's primary red-driver landed [r26: the superseded "should now be reachable" framing swept — it happened, twelve times, see Tests Run]. The r0 red was the gate working: first contact detected a real shipped-posture defect, retroactively validating design 0061.
2. **The #1548 recorded order**: M1 → M2 → M4 → gate → e2e. Live scan at r27: **M1 (#1553), M2 (#1559), M4 (#1557), and the defect fix #1558 ALL MERGED** — the recorded order is clear for the gate; #1566 (the e2e story) rebases onto this head.

The merge call (all recorded-order predecessors merged; the gate has run fully green twelve times) is the orchestrator's.

## Tests Run

- `go test ./local/ -run TestPostureGate -count=1` — 7/7 PASS incl. both golden inventories (r27 shape).
- Mutation checks across rounds (r1 mine; r2–r4 the reviews', each re-verified by me after closing): llm-relay gutting / `--previous` removal / assertion-4 comparison gutting / `continue-on-error` / job `if:` / `types:` filter / posture `--set` injection / `--values` / `--set-json` / `watchNamespaces=` / `set -euo pipefail` deletion / process-substitution reversion / `-f=` / `set +e` / `set +o errexit` / tab-form `-f` / `|| true` on a wait line — all caught.
- `bash -n` on every run block — clean (re-verified after each round's edits).
- `go test ./local/ -count=1` — full package green. `go vet ./local/` clean; gofmt/goimports clean.
- Live-cluster execution: r0's review run (their evidence, cited above); the r1 stability-window mechanics are NOW live-validated (Assert 1 passed, run 36053803876 — r18); and the r18 Assert 2 deployment-truth/prev-consult lines are LIVE-VALIDATED TOO [r25/r26: TWELVE full-green gate runs 00:30Z–04:59Z 2026-09-25 on #1566's branch — seven dispatched, five pull_request-triggered — all four assertions SUCCESS, runs of each type step-verified via the Actions API by the r24/r25 reviews; that branch's assertion run blocks are byte-identical to this PR's, its additive peel being a helm pin, an RBAC-binding assertion step, and a failure-dump step]. The gate's first full green has happened, repeatedly.

## Next Steps

1. Re-review (r27 verdict pending — the bar stands: approvable on the current mechanism once the record is precise).
2. The orchestrator sequences the merge — live scan at r27: M1, M2, M4, and the defect fix #1558 ALL MERGED; the recorded order is clear. The gate's first dispatched green run HAS HAPPENED (twelve full-green runs — seven dispatched, five pull_request-triggered — on #1566's byte-identical assertion code) — the loop is closed; what remains is the merge call itself.
3. The stability window has passed live MANY times (r18's first contact; then the twelve full-green runs of 2026-09-25 — seven dispatched, five pull_request-triggered; the gate's complete four-assertion green, demonstrated. The window's only in-window failure, 36094038738, died in the build stage — infra, not an assertion). No churn false-positives observed; the hook Jobs delete on success.

## Files Modified

- `.github/workflows/posture-gate.yml` (new)
- `local/posture_gate_workflow_test.go` (new)
- `README-LLM.md` (the contribution-rule subsection; `charts/` → `helm/` structure correction)
- `worklogs/NNNN_2026-09-23_m3-posture-gate.md` (this worklog)


## r27 round record (the last two instances — and the scripting incident, owned)

The r26 verdict found the sweep two instances short: Next Steps 3 still said "nine+" (falsifying the r26 record's "everywhere it appears" totality claim — now scoped to the audited locations, with the miss annotated), and the body carried the same "should now be reachable" framing whose worklog twin r26 swept. Both fixed; the in-window build-stage infra failure (36094038738) acknowledged.

**The incident, on the record**: the first r27 commit (0f65a3b5) DESTROYED this worklog — a scripting bug in the round-record append (an anchor slice taken from a failed find) exploded a replace into a 25MB file; the broken commit landed and pushed. This file is rebuilt from 35b336cb in the immediately following commit, edits re-applied one-at-a-time with asserted matches and a length guard. The lesson is the thread's oldest: verify the replace — and now, verify the SIZE of what you write.
