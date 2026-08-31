// US-69.10 acceptance gates (design 0055 S3 test plan):
//   old_dialect_dead_code — the chat render path no longer consumes the
//     old session-state event dialect (session.status / session.event /
//     part.delta / part.end / agent.question / agent.permission), and no
//     timestamp-based stitching remains anywhere in the frontend.
//   generated_types_only — contract wire types come from the generated
//     ABI schema (frontend/src/abi); no hand-written contract wire types.
//
// The SessionActivityProvider's USER-stream consumption of session.status
// / agent.* is the documented exception: cross-workspace events are
// API-owned (Epic 28) and stay until US-69.11 retires the emitters.

import { describe, it, expect } from "vitest";
import { readdirSync, readFileSync, statSync } from "node:fs";
import { join } from "node:path";

const SRC = join(__dirname, "..");

function walk(dir: string): string[] {
  const out: string[] = [];
  for (const entry of readdirSync(dir)) {
    const full = join(dir, entry);
    if (statSync(full).isDirectory()) {
      if (entry === "abi" || entry === "node_modules" || entry === "tests") continue;
      out.push(...walk(full));
    } else if (/\.(ts|tsx)$/.test(entry)) {
      out.push(full);
    }
  }
  return out;
}

// Files whose old-dialect consumption is intentionally retained (user
// stream / provider surface, retired by US-69.11).
const PROVIDER_EXCEPTIONS = new Set([
  join(SRC, "providers", "SessionActivityProvider.tsx"),
  join(SRC, "hooks", "useUserEventStream.ts"),
]);

function sourceFiles(): string[] {
  return walk(SRC);
}

describe("old_dialect_dead_code", () => {
  it("the chat render path consumes no session-state event dialect", () => {
    const offenders: string[] = [];
    for (const file of sourceFiles()) {
      if (PROVIDER_EXCEPTIONS.has(file)) continue;
      if (file.includes(".test.")) continue;
      // api/types.ts holds wire-shape DEFINITIONS for events the user
      // stream still delivers (agent.* — provider-owned until US-69.11);
      // definitions are not consumption.
      if (file.endsWith(join("src", "api", "types.ts"))) continue;
      const text = readFileSync(file, "utf8");
      // Consumption patterns: handler comparisons / type discriminants.
      const patterns = [
        /["']session\.event["']/,
        /["']part\.delta["']/,
        /["']part\.end["']/,
        /["']part\.start["']/,
        /["']agent\.question(\.resolved)?["']/,
        /["']agent\.permission(\.resolved)?["']/,
      ];
      for (const re of patterns) {
        if (re.test(text)) offenders.push(`${file}: ${re.source}`);
      }
    }
    expect(offenders, `old session-state dialect consumption remains:\n${offenders.join("\n")}`).toEqual([]);
  });

  it("ChatPage consumes session.status from neither stream", () => {
    const chat = readFileSync(join(SRC, "pages", "ChatPage.tsx"), "utf8");
    expect(chat).not.toMatch(/["']session\.status["']/);
  });

  it("no timestamp-based stitching remains", () => {
    const offenders: string[] = [];
    for (const file of sourceFiles()) {
      if (file.includes(".test.")) continue;
      const text = readFileSync(file, "utf8");
      // The I12 rule: transcript order is backend order; the message
      // history stitch must never SORT by createdAt. (Timestamps used
      // for display/recency elsewhere are not stitching.)
      let sortIdx = text.indexOf(".sort(");
      while (sortIdx !== -1) {
        const window = text.slice(sortIdx, sortIdx + 400);
        if (/createdAt|getTime\(\)/.test(window)) {
          offenders.push(file);
          break;
        }
        sortIdx = text.indexOf(".sort(", sortIdx + 1);
      }
    }
    expect(offenders, `timestamp-based transcript stitching remains in:\n${offenders.join("\n")}`).toEqual([]);
  });

  it("no hand-written contract wire types remain", () => {
    const types = readFileSync(join(SRC, "api", "types.ts"), "utf8");
    for (const name of ["SessionContractEvent", "ContractEvent", "ContractSession", "ContractPart"]) {
      expect(types, `api/types.ts still defines ${name}`).not.toMatch(
        new RegExp(`(interface|type) ${name}\\b`),
      );
    }
  });
});

describe("generated_types_only", () => {
  it("contract wire types are imported from the generated ABI only", () => {
    const offenders: string[] = [];
    for (const file of sourceFiles()) {
      if (file.includes(".test.")) continue;
      if (file.includes(join("src", "abi"))) continue;
      const text = readFileSync(file, "utf8");
      // Any import of ABI-generated types must come from ../abi (or
      // deeper); hand-written shapes (SessionContractEvent etc.) must not
      // be imported at all.
      if (/(SessionContractEvent|ContractPart\b)/.test(text)) offenders.push(file);
    }
    expect(offenders, `hand-written contract wire types referenced in:\n${offenders.join("\n")}`).toEqual([]);
  });

  it("the ABI fold consumes generated StreamFrame types, not hand-rolled shapes", () => {
    const fold = readFileSync(join(SRC, "session", "fold.ts"), "utf8");
    expect(fold).toMatch(/from "\.\.\/abi\/llmsafespaces\/abi\/v1\//);
    expect(fold).not.toMatch(/interface (StreamFrame|SequencedEvent|SnapshotFrame)\b/);
  });
});
