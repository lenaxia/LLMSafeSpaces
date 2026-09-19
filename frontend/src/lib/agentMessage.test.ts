import { readFileSync } from "node:fs";
import { resolve } from "node:path";
import { describe, expect, it } from "vitest";
import { parseAgentMessage } from "./agentMessage";

// Fixtures are the Go package's golden files, read at runtime (the
// attachments.test.ts precedent): reading from the source of truth
// keeps TS/Go parser drift structurally impossible (#1465, mirroring
// epic-68 D7). Anchored on cwd (vitest runs with cwd=frontend/); falls
// back to a repo-root cwd.
const testdataDir = resolve(
  process.cwd().endsWith("frontend") ? ".." : ".",
  "pkg/session/agentmessage/testdata",
);
const fixture = (name: string): unknown =>
  JSON.parse(readFileSync(resolve(testdataDir, `${name}.json`), "utf-8"));

interface ComposeFixture {
  message: string;
  origin: AgentMessageFixture;
}

interface AgentMessageFixture {
  fromSession: string;
  workspace?: string;
}

interface ParseWant {
  origin: AgentMessageFixture | null;
  found: boolean;
  text: string;
}

function expectParseGolden(inName: string, wantName: string) {
  const input = (fixture(inName) as { text: string }).text;
  const want = fixture(wantName) as ParseWant;
  const got = parseAgentMessage(input);
  expect(got.origin).toEqual(want.origin);
  expect(got.text).toBe(want.text);
}

describe("parseAgentMessage (golden fixtures from pkg/session/agentmessage/testdata)", () => {
  it("parses the basic sentinel and strips the line", () => {
    expectParseGolden("parse_basic.in", "parse_basic.want");
  });

  it("returns plain text unchanged without a sentinel", () => {
    expectParseGolden("parse_no_sentinel.in", "parse_no_sentinel.want");
  });

  it("keeps an interior sentinel line as plain text", () => {
    expectParseGolden("parse_interior_kept.in", "parse_interior_kept.want");
  });

  it("treats unknown/newer versions as plain text", () => {
    expectParseGolden("parse_unknown_version.in", "parse_unknown_version.want");
  });

  it("treats malformed JSON as plain text", () => {
    expectParseGolden("parse_malformed_json.in", "parse_malformed_json.want");
  });

  it("treats a sentinel without fromSession as plain text", () => {
    expectParseGolden("parse_missing_from_session.in", "parse_missing_from_session.want");
  });

  it("tolerates workspace appearing (forward compatibility)", () => {
    expectParseGolden("parse_workspace_tolerated.in", "parse_workspace_tolerated.want");
  });

  it("tolerates unknown additive keys", () => {
    expectParseGolden("parse_unknown_keys_tolerated.in", "parse_unknown_keys_tolerated.want");
  });

  it("parses a sentinel-only text to an empty message", () => {
    expectParseGolden("parse_sentinel_only.in", "parse_sentinel_only.want");
  });

  it("preserves trailing newlines after the message", () => {
    expectParseGolden("parse_trailing_newlines.in", "parse_trailing_newlines.want");
  });

  it("rejects trailing content after the closing --> (greedy terminator)", () => {
    expectParseGolden("parse_trailing_content_rejected.in", "parse_trailing_content_rejected.want");
  });

  it("rejects a lowercase fromsession key (strict exact-key parity with Go)", () => {
    expectParseGolden("parse_lowercase_key_rejected.in", "parse_lowercase_key_rejected.want");
  });

  it("rejects a non-string workspace value (type-strict known keys)", () => {
    expectParseGolden("parse_workspace_bad_type_rejected.in", "parse_workspace_bad_type_rejected.want");
  });

  it("rejects a non-string mode value (type-strict known keys)", () => {
    expectParseGolden("parse_mode_bad_type_rejected.in", "parse_mode_bad_type_rejected.want");
  });

  it("parses the self-declared mode label", () => {
    expectParseGolden("parse_mode_self_declared.in", "parse_mode_self_declared.want");
  });

  it("treats explicit null optional keys as absent (Go json null-into-string parity)", () => {
    expectParseGolden("parse_null_keys_tolerated.in", "parse_null_keys_tolerated.want");
  });
});

describe("parseAgentMessage over Go compose outputs", () => {
  it("round-trips every compose golden: origin parsed, message intact", () => {
    for (const name of [
      "compose_one_line",
      "compose_multiline_and_manifest",
      "compose_empty_message",
      "compose_workspace",
      "compose_strip_existing",
    ]) {
      const tc = fixture(`${name}.in`) as ComposeFixture;
      const composed = fixture(`${name}.want`) as string;
      const got = parseAgentMessage(composed);
      expect(got.origin, name).toEqual(tc.origin);
      // For the strip-existing fixture the input message itself opens
      // with a sentinel — Compose strips it, so the parser's expected
      // text is the input minus its own leading line.
      const expectedText = parseAgentMessage(tc.message).text;
      expect(got.text, name).toBe(expectedText);
    }
  });
});

describe("parseAgentMessage edge cases", () => {
  it("empty text returns no origin and unchanged text", () => {
    expect(parseAgentMessage("")).toEqual({ origin: null, text: "" });
  });

  it("a leading space disqualifies the sentinel", () => {
    const text = ' <!-- lsp:agent-message-v1 {"fromSession":"ses_a"} -->\nhi';
    expect(parseAgentMessage(text)).toEqual({ origin: null, text });
  });

  it("a mid-text re-serialized sentinel survives inside the message", () => {
    const sentinel = '<!-- lsp:agent-message-v1 {"fromSession":"ses_a"} -->';
    const got = parseAgentMessage(`${sentinel}\nbody quotes:\n${sentinel}`);
    expect(got.origin).toEqual({ fromSession: "ses_a" });
    expect(got.text).toBe(`body quotes:\n${sentinel}`);
  });

  it("assistant-side text is never disturbed", () => {
    const text = "<!-- some html comment -->\nregular markdown";
    expect(parseAgentMessage(text)).toEqual({ origin: null, text });
  });
});
