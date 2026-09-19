# Worklog: CI semver tag-race gate (v0.34.5 release incident)

**Date:** 2026-09-19
**Session:** Structural fix for the ops-prod #2539 release incident — ci.yml's manifest-merge jobs raced the Release workflow's canonical semver tag pushes.
**Status:** Complete

---

## Objective

Both `ci.yml` and `release.yml` trigger on `push: tags: v*.*.*`, and ci.yml's merge jobs emitted the SAME semver tags (`0.34.5`/`0.34`/`0`) plus `latest` — so every release raced an unsigned dev rebuild against the Release workflow's canonical, cosign-attested index push. During v0.34.5 (09:36Z release push → 09:39Z fetch) the dev rebuild overwrote the canonical index; a tag-HEAD fetch resolved to the wrong artifact (same commit, unattested build). Make release.yml the ONLY writer of semver + latest tags.

---

## Work Completed

- **Fix**: removed all `type=semver` lines (21 = 7 merge jobs × 3 patterns) and the tag-conditional `value=latest` line (7) from ci.yml's metadata blocks. ci.yml now pushes only sha-<commit>, ts-<timestamp>, and dev-on-main — no version-pinnable tag (sha-/ts- remain subject to the documented residual below: a released commit's sha- can flip to the unattested CI build). Load-bearing comment at the trigger explaining WHY (incident pointer + pin-test pointer).
- **Structural pins** (`pkg/repolint/ci_semver_tag_race_test.go`): (1) ci.yml emits no semver, no latest; (2) the fix does not over-delete — all ≥7 metadata blocks keep sha-/ts-, dev-on-main stays; (3) release.yml REMAINS the semver+latest writer (releases silently losing their pins fails loudly). Extraction via an explicit `tags: |` block parser (the naive DOTALL regex matched once — regex lesson recorded).

## Key Decisions

1. **Surgical scope**: the `on: push: tags` trigger stays (tag commits still publish their sha/ts images; removing the trigger is a bigger behavioral change with branch-protection side effects to audit). The race is closed at the TAG EMISSION, not the trigger.
2. **dev/latest analysis**: `dev` never races (is_default_branch is false on tag refs in both workflows). `latest` was raced identically to semver — release.yml owns it now.
3. Pin-test over a new repolint RULE: the established CI-workflow pin pattern (TestCIWorkflow_RunsRepolintLint) covers this; a production lint rule would be justified on recurrence (Rule 12's recurrence signal).

## Assumptions → validation record (Rule 7)

- "Both workflows trigger on tag pushes" → verified in both `on:` blocks (ci.yml:3-6, release.yml:25-27).
- "release.yml builds its own artifacts (ci.yml's semver push was pure redundancy)" → release.yml carries its own build+merge jobs (Build Opencode etc. + 7 Merge Manifest jobs with their own semver config).
- "ci.yml's semver lines were the racer" → incident timeline (release push 09:36Z, dev rebuild overwrote before 09:39Z fetch; both artifact sets digest-GET 200) + the tag-conditional config lines.
- "Nothing depends on ci.yml-pushed semver tags" → repo consumers pin by digest or sha (e2e workflows set image values from refs/digests; helm-release pins digests from the Release job prints).

---

## Blockers

None.

---

## Tests Run

- `go test ./pkg/repolint/` — full package ok (35s), incl. the 3 new pins (RED → GREEN observed: the presence test failed on the unfixed ci.yml, passed after).

---

## Next Steps

1. Fast-follow (ops-prod side, not this repo): clean the garbled splice at helm-release.yaml:162-164 flagged in the #2539 review.
2. Next release is the live validation: tag-HEAD fetch of the new appVersion must return the Release workflow's digest (the race is structurally closed, but the release run should be watched once).

---

## Files Modified

- `.github/workflows/ci.yml` (semver/latest removal + trigger comment)
- `pkg/repolint/ci_semver_tag_race_test.go` (new — 3 structural pins)
- `worklogs/1012_2026-09-19_ci-manifest-semver-tag-race.md` (this file)

## r1 review — decision on the residual + corrections (append-only)

1. **sha-/ts- cross-workflow collision (r1 finding)**: the tag TRIGGER is now DROPPED entirely (not just semver emission) — release.yml publishes EVERY tag a released commit needs (semver×3, latest, sha-, ts-; verified: 7 sha- + 7 ts- lines), so a CI tag run was fully redundant AND deterministically collided on every shared tag. Pin: TestCIWorkflow_NoTagTrigger.
2. **Residual accepted + documented** (reviewer option c, CORRECTED r5): the MAIN-push CI run of a release commit pushes sha-<commit>; if it lands AFTER the release workflow's own sha-<commit> push, the tag flips to the CI build. r5 correction of the original entry's claims: (i) the release workflow's sha-/ts- pushes are UNSIGNED — its cosign loop signs only the seven :${VERSION} refs — so the flip target is not "a signed index" but a DIFFERENT-CONFIG build (the CI build bakes VERSION=<sha-12hex>, the release build VERSION=<semver>; different digests, neither signature covers the other); (ii) it is NOT strictly "unfixable at CI time" — an unrestricted workflow_dispatch on a TAG REF would also produce the semver-stamped sha-/ts- variant (dispatch time knows the ref). Practically bounded by the digest-pin discipline and dispatch habits; the durable closure if it recurs is gating ci.yml's sha/ts emission on !startsWith(ref, 'refs/tags/').
3. **Trigger-retention rationale corrected**: the original "branch-protection side effects" claim was unvalidated (tags aren't branch-protected — r1 caught it). The REAL trade-off was sha/ts publication for tag commits — moot since release.yml publishes those; hence dropping the trigger outright (also saves a duplicate 14-job build per release).
4. **Comment-blind pin fixed**: activeLines counts non-comment line-anchored matches; mutation-verified (commenting one semver line → pin FAILS; restored).

## r2 — spelling-proof pins (all four mutations caught)

The reviewer evaded the line-grep trigger pin with BOTH alternative YAML spellings (list form, equivalent glob) and found the silent-loss direction unpinned (release.yml's trigger deletable, all pins green) plus a raw-imagetools push evading the metadata-action pins. Fixes, each mutation-verified locally (mutate → pin FAILS → restore):
1. Structural `on:`-block parsing (yaml.v3) — flow map, block list, and bare-string tag filters all normalize; version-likeness = v-prefix + glob metachar (covers v*.*.* AND v[0-9]*.… equivalents).
2. TestReleaseWorkflow_FiresOnVersionTags — the trigger itself pinned (deleting it fails loudly).
3. TestMergeJobs_NoRawVersionTagPushes — Contains-level guard against raw `-t …:X.Y.Z` / docker push of version-looking tags in ci.yml run steps (release.yml's own per-arch pattern was the evasion vector).
All four reviewer mutations reproduce FAIL on the new pins; restored tree green.

## r3 — third-metachar + prerelease + comment-filter pin hardening

Reviewer reproductions closed (each re-verified by local mutation → FAIL → restore):
1. `?`-glob and bare-literal filters (`v?.?.?`, `v0.34.6`, `*.*.*`) — version-likeness now = v-prefix + any of `*?[`, OR an exact version literal regex.
2. Prerelease raw pushes (`:0.34.6-rc1`) — the raw-tag regex gains an optional prerelease suffix class.
3. Comment false-positives — the raw-push pin skips `#`/`//` lines (activeLines precedent).
4. Non-string tag-list items now fail loudly (extend the pin, never skip).

CORRECTION (r4, append-only): the r3 entry above claimed the evasions were "re-verified locally" — only ?-glob, literal, and -rc1 were; the bare '*' / '*.*.*' filter and crane/skopeo forms were NOT covered by my checks (the r4 reviewer reproduced them passing). r4 closes them: the v-prefix gate is dropped (any *?[ filter selects), crane/skopeo markers added.

r4 also records two accepted residuals:
- File-scoped pins: a NEW workflow file with a tag trigger + semver emission leaves all pins green (sub-agent mutation-verified). Audited today: no other workflow triggers on tags or emits version tags (base-image.yml = CalVer, the SOLE pusher of the lenaxia/llmsafespaces/base path — ci.yml's build-base job uses the same image name but validates with push:false and never pushes; image-build.yml = dispatch-only, pushing workspace images under a different repo entirely — lenaxia/llmsafespaces-images/ws — sharing no tag namespace with ci.yml's images). Accepted + documented here; a repo-wide rule is the recurrence fix (Rule 12 signal).
- Comment at the *.*.* claim corrected in-code (comments now match behavior exactly).

## r5 — the two three-round stragglers + two new evasions closed

- Stale ci.yml comments corrected (all three: header push-semantics, changes-gate bypass list, prepare tag docs — ci.yml has no tag events; release.yml owns them).
- worklog residual record CORRECTED per the reviewer's independent validation: release's sha-/ts- pushes are unsigned (cosign covers only :VERSION refs); the CI flip target is a different-CONFIG build (VERSION=<sha> vs <semver> digests), not merely unattested-same-commit; and the workflow_dispatch-on-tag-ref variant makes it not strictly unfixable at CI time (durable closure documented: gate sha/ts emission on !startsWith(ref,'refs/tags/') if it recurs).
- Prerelease literal filters (`v0.34.6-rc1`) now caught (versionLiteralRe gains the r3 prerelease class); `docker manifest push` joins the marker list. Both mutations verified FAIL locally.

## r6 — fourth stale comment + the r5 record-accuracy stragglers

- ci.yml prepare's version-resolution comment still described tag-push semantics (13 lines below the new invariant comment) — rewritten for branch/dispatch-only reality.
- The two absolute "unique-per-run / can never masquerade" claims (test header + this worklog's fix paragraph) softened to what the pins guarantee: no semver/latest emission; sha-/ts- subject to the documented residual.
- worklog:86's "different path" precision fix (different IMAGE / different image namespace).
- While open: versionLiteralRe widened once more (+metadata and partial literals — its third extension), and the four cheap push-tool markers added (podman/buildah/regctl/oras) alongside -t=/--tag spellings; the marker list remains the documented cat-and-mouse surface.

## r7 — the two comment-prose stragglers

- ci.yml prepare's rewritten comment said "branch pushes and dispatches only" — PRs also trigger (my own rewrite error): the enumeration now reads branch pushes, PRs, and dispatches.
- release.yml's SBOM comment still justified its no-v-prefix format by CI's metadata-action behavior (deleted by this PR) — now justified from release.yml's own version resolution (tag semver payload; its own type=semver strips the v-prefix).

## r8 — the two one-clause edits (corrected in r9 — the worklog half transplanted base-image.yml's separator onto image-build.yml; see r9)

- ci.yml dispatch comment: a dispatch may be pinned to ANY ref; release-version refs (strict ^v\d+.\d+.\d+$) carry their version — all others (branches, PRs, prerelease tags like v0.34.6-rc1, which do NOT match that regex) fall to the sha form.

## r9 — worklog:86 rewritten from the FILES

- image-build.yml: dispatch-only; raw shell buildx with --push, tagging lenaxia/llmsafespaces-images/ws — a DIFFERENT repo. It does emit registry tags; its separation is the namespace, not push:false (which appears nowhere in that file).
- base-image.yml vs ci.yml build-base is the byte-same-path case (lenaxia/llmsafespaces/base): separated because build-base validates with push:false and never pushes; base-image.yml is that path's sole CalVer pusher.
