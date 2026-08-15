import { describe, it, expect } from "vitest";
import { extractAgentErrorRef } from "./agentErrorRef";

// LLMSafeSpaces#490: the chat page's message-history query silently
// renders empty on 5xx. Part of the diagnostic banner design is
// surfacing opencode's err_XXXXXXXX ref so operators can jump directly
// from the browser DevTools/UI to the workspace pod's opencode log.
// This helper reads the ref from either of the two shapes it appears
// in — see the JSDoc on extractAgentErrorRef for details.
describe("extractAgentErrorRef", () => {
  it("returns the ref from the flat allowlisted shape (POST /prompt path)", () => {
    // Backend's EnrichChatErrorBody promotes `ref` to top level.
    const body = {
      _tag: "SomeError",
      message: "boom",
      ref: "err_abcdef12",
      sessionID: "ses_xxx",
    };
    expect(extractAgentErrorRef(body)).toBe("err_abcdef12");
  });

  it("returns the ref from the nested opencode envelope (GET history path)", () => {
    // Raw opencode error passed through by the API. #486 hit this
    // exact shape: `{"name":"UnknownError","data":{"ref":"err_xxx"}}`.
    const body = {
      name: "UnknownError",
      data: {
        message: "Unexpected server error. Check server logs for details.",
        ref: "err_b8d02ae9",
      },
    };
    expect(extractAgentErrorRef(body)).toBe("err_b8d02ae9");
  });

  it("prefers the top-level ref when both are present", () => {
    // Guards against accidentally reading the nested one when the API
    // has already promoted the ref to the top.
    const body = {
      ref: "err_top",
      data: { ref: "err_nested" },
    };
    expect(extractAgentErrorRef(body)).toBe("err_top");
  });

  it("returns undefined when no ref is present", () => {
    expect(extractAgentErrorRef({ error: "unauthorized" })).toBeUndefined();
    expect(extractAgentErrorRef({ data: { message: "nope" } })).toBeUndefined();
  });

  it("returns undefined for non-object bodies", () => {
    expect(extractAgentErrorRef(null)).toBeUndefined();
    expect(extractAgentErrorRef(undefined)).toBeUndefined();
    expect(extractAgentErrorRef("some string")).toBeUndefined();
    expect(extractAgentErrorRef(42)).toBeUndefined();
    expect(extractAgentErrorRef([])).toBeUndefined();
  });

  it("returns undefined when ref is an empty string or non-string", () => {
    // Empty-string ref is treated as absent — it's not a useful ID.
    expect(extractAgentErrorRef({ ref: "" })).toBeUndefined();
    expect(extractAgentErrorRef({ ref: 42 })).toBeUndefined();
    expect(extractAgentErrorRef({ ref: null })).toBeUndefined();
    expect(extractAgentErrorRef({ data: { ref: "" } })).toBeUndefined();
  });
});
