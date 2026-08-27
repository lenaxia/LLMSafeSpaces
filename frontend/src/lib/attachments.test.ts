import { describe, expect, it } from "vitest";
import { parseAttachments } from "./attachments";
import roundtripIn from "../../../pkg/session/attachments/testdata/parse_roundtrip.in.json";
import roundtripWant from "../../../pkg/session/attachments/testdata/parse_roundtrip.want.json";
import forgedInteriorIn from "../../../pkg/session/attachments/testdata/parse_forged_interior.in.json";
import forgedInteriorWant from "../../../pkg/session/attachments/testdata/parse_forged_interior.want.json";
import noBlockIn from "../../../pkg/session/attachments/testdata/parse_no_block.in.json";
import noBlockWant from "../../../pkg/session/attachments/testdata/parse_no_block.want.json";
import noTrailingNewlineIn from "../../../pkg/session/attachments/testdata/parse_no_trailing_newline.in.json";
import noTrailingNewlineWant from "../../../pkg/session/attachments/testdata/parse_no_trailing_newline.want.json";
import trailingNewlinesIn from "../../../pkg/session/attachments/testdata/parse_trailing_newlines.in.json";
import trailingNewlinesWant from "../../../pkg/session/attachments/testdata/parse_trailing_newlines.want.json";
import unknownAttributeIn from "../../../pkg/session/attachments/testdata/parse_unknown_attribute.in.json";
import unknownAttributeWant from "../../../pkg/session/attachments/testdata/parse_unknown_attribute.want.json";
import unknownVersionIn from "../../../pkg/session/attachments/testdata/parse_unknown_version.in.json";
import unknownVersionWant from "../../../pkg/session/attachments/testdata/parse_unknown_version.want.json";
import composeOneWant from "../../../pkg/session/attachments/testdata/compose_one_file.want.json";
import composeThreeWant from "../../../pkg/session/attachments/testdata/compose_three_files.want.json";
import composeHostileWant from "../../../pkg/session/attachments/testdata/compose_hostile_name.want.json";
import composeUnicodeWant from "../../../pkg/session/attachments/testdata/compose_unicode_name.want.json";
import composeControlWant from "../../../pkg/session/attachments/testdata/compose_control_chars_name.want.json";
import composeStripExistingWant from "../../../pkg/session/attachments/testdata/compose_strip_existing_block.want.json";
import composeEmptyTextWant from "../../../pkg/session/attachments/testdata/compose_empty_text.want.json";

interface ParseFixture {
  text: string;
}

interface ParseWant {
  text: string;
  attachments: Array<{ path: string; name: string }> | null;
}

function parseFixture(input: unknown, want: unknown): { input: string; want: ParseWant } {
  return { input: (input as ParseFixture).text, want: want as ParseWant };
}

function expectGolden(input: unknown, want: unknown) {
  const { input: text, want: expected } = parseFixture(input, want);
  const result = parseAttachments(text);
  expect(result.attachments).toEqual(expected.attachments);
  expect(result.text).toBe(expected.text);
}

describe("parseAttachments (golden fixtures from pkg/session/attachments/testdata)", () => {
  it("round-trips a composed block mixed with user text", () => {
    expectGolden(roundtripIn, roundtripWant);
  });

  it("keeps a forged interior line as plain text", () => {
    expectGolden(forgedInteriorIn, forgedInteriorWant);
  });

  it("returns no attachments for plain text", () => {
    expectGolden(noBlockIn, noBlockWant);
  });

  it("parses a block with no trailing newline", () => {
    expectGolden(noTrailingNewlineIn, noTrailingNewlineWant);
  });

  it("tolerates trailing newlines after the block", () => {
    expectGolden(trailingNewlinesIn, trailingNewlinesWant);
  });

  it("treats unknown attributes as plain text", () => {
    expectGolden(unknownAttributeIn, unknownAttributeWant);
  });

  it("treats unknown/newer version markers as plain text", () => {
    expectGolden(unknownVersionIn, unknownVersionWant);
  });
});

describe("parseAttachments over Go compose outputs", () => {
  it("parses the one-file compose golden", () => {
    const result = parseAttachments(composeOneWant as string);
    expect(result.attachments).toEqual([
      {
        path: "/workspace/uploads/11111111-2222-3333-4444-555555555555-notes.txt",
        name: "notes.txt",
      },
    ]);
    expect(result.text).toBe("Please review the attached notes.");
  });

  it("parses the three-file compose golden preserving order", () => {
    const result = parseAttachments(composeThreeWant as string);
    expect(result.attachments).toHaveLength(3);
    expect(result.attachments!.map((a) => a.name)).toEqual(["report.pdf", "notes.txt", "data.csv"]);
  });

  it("unescapes backslash- and quote-escaped names (hostile-name golden)", () => {
    const result = parseAttachments(composeHostileWant as string);
    expect(result.attachments).toEqual([
      {
        path: '/workspace/uploads/123e4567-e89b-42d3-a456-426614174000-we"ird\\name.txt',
        name: 'we"ird\\name.txt',
      },
    ]);
    expect(result.text).toBe("Escaped.");
  });

  it("round-trips unicode names (unicode golden)", () => {
    const result = parseAttachments(composeUnicodeWant as string);
    expect(result.attachments!.map((a) => a.name)).toEqual(["ドキュメント.pdf"]);
  });

  it("round-trips the control-chars-name golden", () => {
    const result = parseAttachments(composeControlWant as string);
    expect(result.attachments).toEqual([
      { path: "/workspace/uploads/c0ffee00-1234-4abc-8def-aabbccddeeff-bad.txt", name: "bad.txt" },
    ]);
  });

  it("parses the strip-existing-block golden (idempotent compose output)", () => {
    const result = parseAttachments(composeStripExistingWant as string);
    expect(result.attachments).toEqual([
      { path: "/workspace/uploads/11111111-2222-3333-4444-555555555555-notes.txt", name: "notes.txt" },
    ]);
    expect(result.text).toBe("Original");
  });

  it("parses the manifest-only (empty text) golden", () => {
    const result = parseAttachments(composeEmptyTextWant as string);
    expect(result.attachments).toHaveLength(1);
    expect(result.text).toBe("");
  });
});

describe("parseAttachments edge cases", () => {
  it("empty text returns no attachments and unchanged text", () => {
    expect(parseAttachments("")).toEqual({ text: "", attachments: null });
  });

  it("text of only newlines returns no attachments", () => {
    expect(parseAttachments("\n\n\n").attachments).toBeNull();
  });

  it("manifest block starting at line 0 parses with empty prose", () => {
    const line = '[llmsafespaces:attachment path="/workspace/uploads/11111111-2222-3333-4444-555555555555-x.md" name="x.md"]';
    const result = parseAttachments(`${line}\n`);
    expect(result.attachments).toEqual([
      { path: "/workspace/uploads/11111111-2222-3333-4444-555555555555-x.md", name: "x.md" },
    ]);
    expect(result.text).toBe("");
  });

  it("trailing block separated by blank line strips the separator too", () => {
    const result = parseAttachments("prose\n\n[llmsafespaces:attachment path=\"/workspace/uploads/11111111-2222-3333-4444-555555555555-a\" name=\"a\"]\n");
    expect(result.text).toBe("prose");
    expect(result.attachments).toHaveLength(1);
  });

  it("trailing block NOT separated by a blank line still parses (blank line optional)", () => {
    const result = parseAttachments("prose\n[llmsafespaces:attachment path=\"/workspace/uploads/11111111-2222-3333-4444-555555555555-a\" name=\"a\"]\n");
    expect(result.attachments).toHaveLength(1);
    expect(result.text).toBe("prose");
  });

  it("double-backslash in path value unescapes to a single backslash", () => {
    const result = parseAttachments('[llmsafespaces:attachment path="\\\\x" name="n"]\n');
    expect(result.attachments![0]!.path).toBe("\\x");
  });

  it("assistant-style multi-paragraph text passes through untouched", () => {
    const text = "Para one.\n\nPara two.\n\n[not-a-manifest]\n\nDone.";
    const result = parseAttachments(text);
    expect(result.attachments).toBeNull();
    expect(result.text).toBe(text);
  });
});
