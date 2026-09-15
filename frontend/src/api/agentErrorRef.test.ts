import { describe, it, expect } from "vitest";
import { extractAgentErrorRef, extractAgentErrorMessage } from "./agentErrorRef";

// LLMSafeSpaces#490: the chat page's message-history query silently
// renders empty on 5xx. Part of the diagnostic banner design is
// surfacing the agent error reference (err_XXXXXXXX) so operators can
// jump directly from the browser DevTools/UI to the workspace pod's
// agent log. This helper reads the ref from the error bodies the API
// authors — see the JSDoc on extractAgentErrorRef for details.
//
// #1303: the API authors every error body on the message routes since
// #828 + the question/permission REST migration (#1302) retired the
// raw agent passthrough. The legacy nested envelope
// `{name, data: {ref, message}}` is dead — these rows pin that the
// extractors return undefined for it instead of resurrecting the
// agent-shape knowledge in the UI seam.
describe("extractAgentErrorRef", () => {
  it("returns the ref from the top-level allowlisted shape (API-promoted)", () => {
    // EnrichChatErrorBody promotes allowlisted agent error fields
    // (including `ref`) to the top level of the API's error body.
    const body = {
      _tag: "SomeError",
      message: "boom",
      ref: "err_abcdef12",
      sessionID: "ses_xxx",
    };
    expect(extractAgentErrorRef(body)).toBe("err_abcdef12");
  });

  it("returns undefined for the legacy nested envelope (dead shape, #1303)", () => {
    // Pre-#828 the GET history path passed the raw agent envelope
    // through verbatim (#486 hit `{"name":"UnknownError","data":{...}}`).
    // That passthrough is deleted; the nested fallback went with it
    // (#1303). A nested-only ref must NOT be extracted.
    const body = {
      name: "UnknownError",
      data: {
        message: "Unexpected server error. Check server logs for details.",
        ref: "err_b8d02ae9",
      },
    };
    expect(extractAgentErrorRef(body)).toBeUndefined();
  });

  it("reads the top-level ref even when a legacy data key is present", () => {
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

describe("extractAgentErrorMessage", () => {
  it("returns the message from the top-level allowlisted shape (API-promoted)", () => {
    const body = {
      _tag: "SomeError",
      message: "provider rate-limited",
      ref: "err_abcdef12",
    };
    expect(extractAgentErrorMessage(body)).toBe("provider rate-limited");
  });

  it("returns undefined for the legacy nested envelope (dead shape, #1303)", () => {
    const body = {
      name: "UnknownError",
      data: { message: "Unexpected server error.", ref: "err_b8d02ae9" },
    };
    expect(extractAgentErrorMessage(body)).toBeUndefined();
  });

  it("returns undefined for the API's own { error } bodies", () => {
    // e.g. GET history 502: {"error":"failed to fetch history"}
    // (proxy_handlers.go). Callers fall back to `body.error` — this
    // extractor must not manufacture a message that is not there.
    expect(extractAgentErrorMessage({ error: "failed to fetch history" })).toBeUndefined();
  });

  it("returns undefined for non-object bodies and never throws", () => {
    expect(extractAgentErrorMessage(null)).toBeUndefined();
    expect(extractAgentErrorMessage(undefined)).toBeUndefined();
    expect(extractAgentErrorMessage("boom")).toBeUndefined();
    expect(extractAgentErrorMessage(42)).toBeUndefined();
    expect(extractAgentErrorMessage([])).toBeUndefined();
  });

  it("returns undefined when message is an empty string or non-string", () => {
    expect(extractAgentErrorMessage({ message: "" })).toBeUndefined();
    expect(extractAgentErrorMessage({ message: 42 })).toBeUndefined();
    expect(extractAgentErrorMessage({ data: { message: "nested" } })).toBeUndefined();
  });
});
