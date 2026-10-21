# Worklog: #1793 — provisioned SSH key corrupt (trailing space per line): writer belt + save-side normalization

**Date:** 2026-10-20
**Session:** continuation worker lane (orchestrator dispatch with firsthand diagnosis, issuecomment 6303850750). Worktree `wt-1793`, branch `fix/1793-ssh-key-corruption` off main @ 26e67673.
**Status:** Complete

---

## Objective

Fix the corrupt provisioned SSH key (`error in libcrypto`, all SSH git ops dead in fresh workspaces): make the delivery writer emit parseable bytes (validate + strict-alphabet repair) and normalize on the save path so the corruption class cannot enter storage again.

## Evidence (verified live on this pod)

`/sandbox-runtime/rt/ssh/id_ed25519_github` delivers with ONE TRAILING SPACE ON EVERY LINE — PEM markers included (`awk` per-line dump: `-----BEGIN OPENSSH PRIVATE KEY----- ` etc.; 70-char base64 rows read as 71 cols, last row 64+1 — matching the issue's 71/71/71/71/65 shape). The orchestrator's byte-level reconstruction (strip EOL spaces, re-join) yields a WORKING key: corruption is formatting-only.

## Root-cause tracing (why the belt, not a generator fix)

- The frontend generator (`SecretsTab.tsx` generateSSHKeypair) wraps at 70 with plain `\n` joins — byte-clean in git history back to its introduction (7459b65a). It never emitted trailing spaces.
- The API→agentd batch is JSON (byte-exact); `staging.add` writes verbatim; the store round-trips exactly.
- Conclusion: the STORED value entered corrupt (paste/transport reflow — the class, not a specific line of code). Two durable defenses: (a) the delivery writer validates+repairs whatever is stored; (b) the save path normalizes ssh-key values so pastes cannot reintroduce it. Both shipped.

## Work Completed

### (a) RED-first pins (first commit)

`pkg/agentd/secrets/sshkey_belt_test.go` — four pins, run measured vs main @ 26e67673: 2 FAIL (`TestSSHKey_StagedBytesParseable_DespiteCorruptPlaintext`, `TestSSHKey_RepairReproducesCleanKey`), 2 PASS (`TestSSHKey_UnparseableStagedVerbatim`, `TestSSHKey_CleanPassthrough` — today's verbatim behavior already satisfies never-worse/passthrough). Keys are generated at runtime (`ed25519.GenerateKey` + `ssh.MarshalPrivateKey`) — no committed fixtures; the corrupt fixture reproduces the live shape (trailing space per line, markers included).

### (b) The delivery belt (`pkg/agentd/secrets/sshkey.go` + the applySSHKey seam, secrets.go:745-752)

`normalizeOpenSSHPrivateKey`: structurally-clean (no edge whitespace, no CRLF) AND parse-valid → staged byte-identical; otherwise one repair (per-line TrimSpace + CRLF normalize) staged only if it parses; garbage-after-repair staged VERBATIM (never worse). **Structural detection, not parser detection**: Go's x/crypto TOLERATES the trailing-space class (verified empirically — the corrupt fixture parses clean in Go while stock OpenSSH 9.2p1 fails with `error in libcrypto`), so validity-only under-detects. Validation = `ssh.ParsePrivateKey`, the in-process equivalent of the suggested `ssh-keygen -y` (no subprocess at boot).

### (c) Save-side normalization (frontend)

`frontend/src/lib/sshPrivateKey.ts` (`normalizeSSHPrivateKeyForSave`: CRLF-normalize + per-line trim) wired into BOTH submit paths in SecretsTab (create + update) for `type === "ssh-key"`; the created-value display shows the normalized bytes. Vitest pins in `sshPrivateKey.test.ts` (CI-verified — lane convention: no local node_modules). Deliberately SURGICAL diff on SecretsTab.tsx (13 insertions): a whole-file prettier pass was reverted — the file is not format-swept and 500+ lines of churn would drown the review.

## Tests Run

- `go test ./pkg/agentd/secrets/ -count=1` — full package green (all four belt pins + the 64 pre-existing).
- Frontend: vitest/tsc via CI (no local node_modules, per lane convention).
- golangci --new-from-merge-base + repolint/gofmt/goimports via pre-commit.

## Files Modified

`pkg/agentd/secrets/{sshkey.go(new),secrets.go,sshkey_belt_test.go(new)}`, `frontend/src/lib/sshPrivateKey.{ts,test.ts}(new)`, `frontend/src/components/settings/SecretsTab.tsx`, `CHANGELOG.md`, this worklog.

## Review r1 remediation (commit after 181a764b)

Verifier-executed findings, all remediated:

1. **The encrypted-key blind spot** (the reviewer reproduced it empirically with MarshalPrivateKeyWithPassphrase): a passphrase-protected key NEVER parses in-process (PassphraseMissingError), so a corrupt encrypted key fell to the verbatim branch and stayed corrupt — contradicting the CHANGELOG's blanket claim. Fixed: `parsesOrPassphraseProtected` treats PassphraseMissingError as validity in BOTH belt branches; per-line trim cannot alter key material. Pinned: TestSSHKey_EncryptedCorruptKeyRepaired (staged == clean encrypted PEM byte-for-byte, AND the repaired key still reports passphrase-protected — repaired, not mangled). CHANGELOG corrected.
2. **Orphaned/factually-wrong test comment** (claimed the fixture adds CRLF; the helper strips it) — removed; a NEW corruptTrailingSpacesAndCRLF helper + TestSSHKey_CorruptCRLFAndTrailingSpacesRepaired pin the COMBINED corruption family (missing-test case 2).
3. **Real-wiring happy path** (missing-test case 3): TestSSHKeyBelt_CorruptKeyDeliveredClean (cmd/workspace-agentd) — corrupt fixture through Materialize → loadStagedFiles → fileDelivery.apply (the boot matrix's real staged→delivered wiring), asserting the DELIVERED ~/.ssh file equals the clean PEM and parses. This mechanically pins the no-bypass claim the PR had audited by hand.
4. **SecretsTab component-level wiring pin** (missing-test case 4, marked minor by the review): DECLINED in-round with rationale — no SecretsTab.test.tsx harness exists (every sibling tab has one; building it is its own surface), the pure function is pinned, and the delivery belt backstops a silently-dropped guard. Offered as a follow-up.

Frontend/gitleaks CI history: first-commit base64-shaped dummies rewritten to low-entropy bodies (fbe904cf) + .gitleaks.toml allowlist entry per the repo's documented fixture precedent (181a764b).

## Review r2 remediation

r2 (796e8452) verified all three r1 blockers fixed by execution (bypass-scratch red/green on all repair-dependent pins). Two remaining findings, both remediated:

1. **False decline premise — owned**: the r1 worklog claimed "no SecretsTab.test.tsx harness exists". FALSE — the harness predates the PR (310 lines, mocked secretsApi + openCreateForm/selectSecretType helpers); my earlier directory listing truncated it away and I declined on incomplete evidence. The component pin is now SHIPPED in the harness (ssh-key create with a trailing-space-corrupt value → secretsApi.create called with the normalized bytes).
2. **CHANGELOG overstatement — scoped + pinned**: "encrypted keys included" was wrong for PKCS#8 `ENCRYPTED PRIVATE KEY` envelopes (x/crypto returns an untyped unsupported-type error there, not PassphraseMissingError). The claim now says OpenSSH-format passphrase-protected keys; the boundary is pinned by TestSSHKey_PKCS8EncryptedCorruptStagedVerbatim (corrupt PKCS#8 envelope → staged verbatim, never-worse).

## Review r3 — THE INCIDENT and the reconstruction

The r3 review revealed the true scale of the 718a0855 damage my r2
`git add -A` had staged: not two worklog files but a ~99-file inversion
of main's post-fork delta (the whole #1747 board-manager feature incl.
applied migration 000039 and its 000036 index reversion, the
#1794/#1756 shortid entropy fix, an ai-workflows pin downgrade
v0.2.20→v0.2.18) — all deleting SILENTLY at merge (merge-tree verified
by the reviewer; only 2 conflicts masked it). Mechanism of the
working-tree reversion remains unidentified (the ff-merge to 796e8452
printed 'Already up to date.' while the tree held pre-merge content —
the status-before-add discipline would have caught it; it is now
mandatory for me).

REMEDY (per the reviewer's prescription and the #1806 precedent): the
branch is RECONSTRUCTED from main @ 2dcf61f5 (v0.40.0) as two commits —
the red-first pins (d2bcdc31) and this implementation commit — with the
delta verified to be EXACTLY the #1793 file set. The r3 update-path
finding is also fixed: the harness mock gains `update` and BOTH submit
paths are pinned at component level. Local gates green (belt pins,
secrets + agentd suites, golangci --new-from-merge-base 0 issues).
Force-push with lease to replace the poisoned history; full disclosure
on the PR thread.
