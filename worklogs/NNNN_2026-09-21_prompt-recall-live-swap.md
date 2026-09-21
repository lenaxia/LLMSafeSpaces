# Worklog: prompt-recall live swap + symbol @→# (Refs #1496, #1499)

**Date:** 2026-09-21
**Session:** Swap the composer's mocked prompt-library for the merged live backend (6ee54512) + owner-decided recall-symbol switch; branch `fix/prompt-recall-live-swap`
**Status:** Complete

---

## Objective

(a) verify the merged envelope against the actual handler, (b) re-seam the vitest suite to the api-client boundary, (c) adapter contract pin + empty-list degradation, (d) Playwright unchanged unless deviation — plus the scope addition: recall symbol `@`→`#` (people-tagging convention), one PR.

---

## Work Completed

- **(a) Envelope verification:** handler → `{"prompts":[…]}` (named envelope, updatedAt DESC) ↔ adapter `api.get("/me/prompts").then(r => r.prompts ?? [])` — exact match; client prefix `/api/v1` confirmed. No deviation → Playwright's network mocks kept the named-envelope shape (only the trigger symbol changed).
- **(b) Vitest re-seam:** both composer suites mock `../../api/client` (not the adapter module) — the real adapter's unwrap runs in every row; the slash suite preserves `ApiClientError` via `importOriginal` (its compact row constructs the real class).
- **(c) Contract pin:** `promptLibrary.test.ts` — `/me/prompts` path, named-envelope unwrap, missing-key → `[]`, transport-error propagation (the hook's `query.data ?? []` degrades above). Playwright already pins the 500-never-opens row.
- **(d) Symbol switch:** `atToken`→`promptToken` (`findPromptToken`/`expandPromptToken`, `AtToken`→`PromptToken`); Composer state/testid renames (`prompt-recall-popup`); all suites flipped. The deliberate #-collision pins:
  - **Markdown headings (my call, rationale in-code):** bare `#` at line start never opens (a heading's first keystroke); `#word` at line start does (explicit intent); bare mid-sentence `#` still opens.
  - **Code-ish:** `C#`/`a#b` suppressed by the word-boundary rule (pinned); `#include`-shape opens — same class as the old `@`-mid-text, Esc exists.
  - **Recursion guard retargeted:** content ending in a `#token` leaves a live token at the new caret; suppression keyed-on-text pinned in vitest + the jsdom fast-loop row + Playwright.
  - Escape dismissal keys on the ANCHOR INDEX — the re-arm fixture uses a different anchor ("x #dep" and "y #rev" share anchor 2; row documents it).
  - PromptsTab copy: "recall them in the composer with #".

---

## Key Decisions

- **Heading suppression scope:** line-start-only, bare-only. Narrow rule kills the heading false positive at its source without breaking `#deploy` at line start or bare mid-sentence `#`. `#include`-shape opening accepted (filter-then-Esc) — symmetric with the old `@` behavior for emails-in-text.
- **Re-seam at api-client, not fetch:** the adapter's `.then` unwrap is the seam under pin; one level below fetch would re-implement the client in every test.

## Blockers

None.

## Tests Run

- vitest: 27 files / 435 tests green (re-seamed suites, promptToken 15-row collision matrix, promptLibrary pin 3, PromptsTab 7)
- `npx tsc --noEmit` clean
- `npx playwright test tests/e2e/composer-slash-at.spec.ts` — 8/8 (live dev server)

## Next Steps

- Review loop to APPROVED; orchestrator merges.

## Files Modified

- `frontend/src/lib/promptToken.ts` + `promptToken.test.ts` (renamed from atToken*; symbol + heading rule)
- `frontend/src/components/chat/Composer.tsx`
- `frontend/src/components/chat/Composer.atRecall.test.tsx`, `Composer.slash.test.tsx`
- `frontend/src/api/promptLibrary.test.ts` (new)
- `frontend/tests/e2e/composer-slash-at.spec.ts`
- `frontend/src/components/settings/PromptsTab.tsx`
