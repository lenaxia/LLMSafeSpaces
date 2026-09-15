import { describe, expect, it } from "vitest";
import { decodeFilePartData, fileNoticeText } from "./fileNotices";

// #1307 r2: the decoder's wire forms in isolation. CustomPart.data is
// `bytes` on the ABI wire (Uint8Array on SSE); the REST history path
// delivers a parsed object; defensive string form also pinned.
describe("decodeFilePartData", () => {
  it("decodes the Uint8Array (protobuf bytes) form — the live SSE wire", () => {
    const bytes = new TextEncoder().encode(JSON.stringify({ type: "file", mime: "image/png", filename: "shot.png" }));
    expect(decodeFilePartData(bytes)).toEqual({ type: "file", mime: "image/png", filename: "shot.png" });
  });

  it("decodes the parsed-object form — the REST history wire", () => {
    expect(decodeFilePartData({ type: "file", mime: "image/png" })).toEqual({ type: "file", mime: "image/png" });
  });

  it("decodes the JSON-string form", () => {
    expect(decodeFilePartData(`{"type":"file","mime":"image/jpeg"}`)).toEqual({ type: "file", mime: "image/jpeg" });
  });

  it("returns null for non-file payloads, invalid JSON, and junk", () => {
    expect(decodeFilePartData({ type: "other" })).toBeNull();
    expect(decodeFilePartData(new TextEncoder().encode("not json"))).toBeNull();
    expect(decodeFilePartData(42)).toBeNull();
    expect(decodeFilePartData(undefined)).toBeNull();
  });
});

describe("fileNoticeText", () => {
  it("renders the omission notice with the file label when downgraded", () => {
    const text = fileNoticeText({ type: "file", mime: "image/png", filename: "shot.png", omitted: true, notice: "[image omitted] switch models." });
    expect(text).toContain("[image omitted] switch models.");
    expect(text).toContain("shot.png");
    expect(text).toContain("image/png");
  });

  it("renders a neutral attachment record when not downgraded", () => {
    const text = fileNoticeText({ type: "file", mime: "image/png", filename: "chart.png" });
    expect(text).toBe("[file attached: chart.png (image/png)]");
  });

  it("returns null for non-file payloads", () => {
    expect(fileNoticeText({ type: "other" })).toBeNull();
  });
});
