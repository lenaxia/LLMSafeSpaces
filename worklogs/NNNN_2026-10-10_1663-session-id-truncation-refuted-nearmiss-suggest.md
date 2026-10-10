# Worklog: #1663 — session-ID "truncation": refuted-by-evidence, near-miss did-you-mean shipped

Session: w22 (ses_ed8a10cceffeKXcf8mSy139yXZ), branch fix/1663-session-id-truncation from main @ f9a74a19.

## Verdict first

**The alleged platform-side one-character truncation does not exist.** Every platform
surface carried the affected session's id byte-identically; the truncated form existed
only in the caller's own emitted tool-call arguments. The issue's three evidence lines
are refuted by persisted bytes. What IS ours to fix: the platform answered the caller's
near-miss id with a bare "not found" — turning a recoverable single-char typo into an
hour-long "zombie session" diagnosis. Shipped: a suggest-only did-you-mean clause on
every id-taking agentd tool's not-found error, plus a standing e2e guard that the
create path never diverges response-vs-store ids at the boundary lengths.

## Evidence chain (all read-only against the live pod; opencode.db never written)

Affected session: ses_ed9183a46ffeRiNw2PddpL5dDA ("Worker: #1648 mobile viewport fix").

1. **Store**: `SELECT id FROM session WHERE id LIKE '%ed9183a46%'` → full 30-char id,
   length 30. ALL 61 store ids are length 30 — the affected one is not anomalous.
2. **opencode raw list** (live GET /session, basic auth): carries the FULL id for the
   session (verified via python json scan; 61 sessions).
3. **opencode by-ID GET**: full id → 200; truncated 29-char id → 404. Self-consistent.
4. **Persisted event store**: `SELECT DISTINCT aggregate_id FROM event WHERE
   aggregate_id LIKE '%ed9183a46%'` → the FULL id (61 aggregates).
5. **The create_session tool result as PERSISTED in the orchestrator's own transcript**
   (parts fetched read-only from GET /session/ses_f499ee9e6ffe52BJ8jxc2TEQQJ/message):
   `[2026-10-10T17:41:25.479Z] llmsafespaces_create_session → {"status":"created",
   "session_id":"ses_ed9183a46ffeRiNw2PddpL5dDA","prompt_status":"delivering_in_background"}`
   — the FULL id. All 24 create_session results in the transcript: len 30.
6. **Every failed call in the transcript carries a 29-char id in TWO variants**:
   ...L5dA (penultimate 'D' dropped) and ...L5DA (final 'A' dropped) — across
   send_message, session_metadata, session_read, timestamps 19:18Z–19:48Z. A
   fixed-width clip cannot produce two different variants; model re-emission can.
   Every send with the FULL id (pre-19:18Z): `completed`. Zero failures with the full
   id; zero successes with any 29-char id.
7. **All fromSession sentinels ever delivered to the orchestrator** (including w21's
   four): len 30. Not one 29-char sentinel exists in the entire transcript.

Refuted claims, mapped: "create response truncated" → (5); "fromSession truncated" →
(7); "session_list/index truncated" → (2)+(6); "true form 404s against the index" →
(6) (all full-id sends completed). The orchestrator's own reasoning trace shows it
flip-flopping between ...L5dA and ...L5DA while transcribing — the failure signature
of near-identical high-entropy strings re-typed from context, the known #1530
emission-slip class (already documented in the send_message tool description).

## Why the platform still owns a fix

When the caller's model degrades an id, the platform's answer is the only feedback
loop. A bare `session X not found in this workspace (IDs from session_list)` — against
a workspace that CONTAINS a session one edit away — hides the recovery. The fleet lost
an hour; heartbeat nudges and steering silently no-op'd because every retry repeated
the caller's corrupted id.

## The fix (ours, at the failure layer)

`cmd/workspace-agentd/session_suggest.go`:

- `suggestSessionID(requested, ids)` — bounded Levenshtein (≤2 edits, early exit),
  deterministic: unique closest candidate → "did you mean <id>? (your id differs by
  N character[s]; re-copy the exact id from session_list / session_metadata)";
  a tie → names all tied candidates, picks none. SUGGEST-ONLY: no auto-correct,
  no re-addressing, nothing ever delivered on a guess (destructive verbs refuse
  exactly as before).
- `sessionSuggestClause` — one bounded GET /session on the failure path;
  best-effort (list failure → silent "" — never masks or delays the real error).
- `isSessionNotFoundErr` — recognizes the seam's two 404 shapes (`status 404:`
  from statusError/automation, `returned 404` from Abort). Suggestions ride ONLY
  on not-found; transport/5xx keep their original text.

Wired into every id-taking tool's not-found branch: send_message (target AND origin),
session_read, session_metadata (explicit scope; ids already in hand), abort_session,
session_archive, delete_session. mcpSessionArchive gains the `password` param
(dispatch already had it; mcpDeleteSession's pattern). Error PREFIXES unchanged
(Contains-pins stay green).

## Pins

- **Red-first** (agentd, `session_suggest_test.go`): compile-red on the undefined
  helpers captured pre-implementation; post-fix 14/14 green — both observed live
  single-char drop variants of the incident id, boundary lengths 29/30/31 both
  directions (drop/insert/substitute), distance-2 in / distance-3 out, exact-id and
  empty-inventory silence, ambiguity (no single guess), determinism, list-failure
  silence, and one tool-level pin per wired surface (send/read/metadata/abort/archive).
- **Standing guard** (`pkg/agent/opencode/session_id_fidelity_1663_test.go`): loop 45
  creations through the real SessionCreate path against a store-faithful fake minting
  ids at lengths 29/30/31 — response-vs-store byte equality per round, list + by-ID
  surfaces serve the same bytes, and a dropped-char id does NOT resolve. This pin
  passes against current code BY DESIGN (there is no truncation to make it red); it
  exists to catch any future real clip at the exact suspected boundary. Verified green
  locally by name (2/2).

## Honest disclosure

- Local full-suite run of cmd/workspace-agentd was disk-blocked (pod pvc at 99%;
  standing go clean performed; two stale scratch logs in the SHARED /tmp/opencode
  removed — test-job.log 253M, race3550.log 145M — sibling worktrees untouched).
  My new pins in both packages ran green individually (verified by name); the
  sed signature-updates to mcp_session_archive_test.go compiled in the earlier
  14/14 run; full-suite verification rides CI.
- /home/sandbox/.cache/go-build cleaned twice (shared cache, rebuilt on demand).
- Live pod queries were ALL read-only (GET endpoints, sqlite mode=ro). The zombie
  session itself was never touched.

## Upstream note

Filed for the PR body per the #1577 pattern (draft at /workspace/upstream-opencode-issue.md
— broader caller-identity surface): the incident class is caller-side id
re-emission; opencode-side surfaces were byte-exact throughout. No opencode change
requested for THIS issue; the platform-side tolerance is the fix.
