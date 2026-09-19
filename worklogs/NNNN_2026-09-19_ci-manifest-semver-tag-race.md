# Worklog: CI semver tag-race gate (v0.34.5 release incident)

**Date:** 2026-09-19
**Session:** Structural fix for the ops-prod #2539 release incident — ci.yml's manifest-merge jobs raced the Release workflow's canonical semver tag pushes.
**Status:** Complete

---

## Objective

Both `ci.yml` and `release.yml` trigger on `push: tags: v*.*.*`, and ci.yml's merge jobs emitted the SAME semver tags (`0.34.5`/`0.34`/`0`) plus `latest` — so every release raced an unsigned dev rebuild against the Release workflow's canonical, cosign-attested index push. During v0.34.5 (09:36Z release push → 09:39Z fetch) the dev rebuild overwrote the canonical index; a tag-HEAD fetch resolved to the wrong artifact (same commit, unattested build). Make release.yml the ONLY writer of semver + latest tags.

---

## Work Completed

- **Fix**: removed all `type=semver` lines (21 = 7 merge jobs × 3 patterns) and the tag-conditional `value=latest` line (7) from ci.yml's metadata blocks. ci.yml now pushes only sha-<commit>, ts-<timestamp>, and dev-on-main — unique-per-run tags that can never masquerade as a release pin. Load-bearing comment at the trigger explaining WHY (incident pointer + pin-test pointer).
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
- `worklogs/NNNN_2026-09-19_ci-manifest-semver-tag-race.md` (this file)

## r1 review — decision on the residual + corrections (append-only)

1. **sha-/ts- cross-workflow collision (r1 finding)**: the tag TRIGGER is now DROPPED entirely (not just semver emission) — release.yml publishes EVERY tag a released commit needs (semver×3, latest, sha-, ts-; verified: 7 sha- + 7 ts- lines), so a CI tag run was fully redundant AND deterministically collided on every shared tag. Pin: TestCIWorkflow_NoTagTrigger.
2. **Residual accepted + documented** (reviewer option c): the MAIN-push CI run of a release commit still pushes sha-<commit> unsigned; if that run lands AFTER the release workflow's signed sha-<commit> push (queue timing), the tag flips to the unattested same-commit build. Attestation-only delta (same commit), practically unreachable under the digest-pin discipline (#2539's fetch flow is digest-GET), and unfixable at CI time (CI cannot know a commit will be released). Recorded here per the review's demand; revisit only if a consumer pins sha- tags of released commits.
3. **Trigger-retention rationale corrected**: the original "branch-protection side effects" claim was unvalidated (tags aren't branch-protected — r1 caught it). The REAL trade-off was sha/ts publication for tag commits — moot since release.yml publishes those; hence dropping the trigger outright (also saves a duplicate 14-job build per release).
4. **Comment-blind pin fixed**: activeLines counts non-comment line-anchored matches; mutation-verified (commenting one semver line → pin FAILS; restored).
