import { describe, it, expect } from "vitest";
import { readFileSync } from "node:fs";
import { dirname, join } from "node:path";
import { fileURLToPath } from "node:url";

/**
 * #1303 grep-gate: the legacy nested-error-envelope extraction is dead
 * and must not come back. The API authors every error body on the
 * message routes (#828 deleted the raw passthrough; #1302 finished the
 * REST migration), so no client code may parse a nested `data.*`
 * envelope, revive the old helper name, or re-document the dead
 * passthrough as live behavior. The repolint-side lexicon guard is #1305's
 * lane; this pins the frontend seam until then.
 *
 * The gate reads the real files and fails if one is missing — a renamed
 * or moved file cannot pass silently.
 */
const here = dirname(fileURLToPath(import.meta.url));

const guardedFiles = [
  "../../api/agentErrorRef.ts",
  "../../api/types.ts",
  "../../hooks/useSessionTitle.ts",
] as const;

// Patterns whose presence means the nested-envelope coupling returned.
const forbiddenPatterns: Array<{ pattern: RegExp; why: string }> = [
  {
    pattern: /record\.data|body\.data/,
    why: "nested data.* envelope extraction",
  },
  {
    pattern: /extractOpencodeField/,
    why: "the deleted nested-or-flat extractor name",
  },
  {
    pattern: /OpencodeErrorEnvelope/,
    why: "the deleted raw-envelope fixture type",
  },
  {
    pattern: /proxied through|passed through verbatim/,
    why: "documenting the deleted raw passthrough as live",
  },
];

describe("#1303 grep-gate: nested error-envelope extraction stays dead", () => {
  for (const rel of guardedFiles) {
    it(`${rel} contains none of the forbidden patterns`, () => {
      const source = readFileSync(join(here, rel), "utf-8");
      for (const { pattern, why } of forbiddenPatterns) {
        expect(source, `${rel} must not contain ${why}`).not.toMatch(pattern);
      }
    });
  }

  it("guards the real files (no vacuous pass after a move/rename)", () => {
    // If this row fails, update guardedFiles — do not delete the gate.
    for (const rel of guardedFiles) {
      expect(() => readFileSync(join(here, rel), "utf-8")).not.toThrow();
    }
  });
});
