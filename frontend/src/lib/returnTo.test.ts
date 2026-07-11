import { describe, expect, it } from "vitest";
import { sanitiseReturnTo } from "./returnTo";

describe("sanitiseReturnTo", () => {
  it("returns empty string for empty input", () => {
    expect(sanitiseReturnTo("")).toBe("");
  });

  it("returns empty string for paths that don't start with /", () => {
    expect(sanitiseReturnTo("chat")).toBe("");
    expect(sanitiseReturnTo("//evil.com")).toBe("");
    expect(sanitiseReturnTo("\\\\evil.com")).toBe("");
  });

  describe("valid same-app paths", () => {
    it("accepts /chat", () => {
      expect(sanitiseReturnTo("/chat")).toBe("/chat");
    });

    it("accepts /invitations/abc123", () => {
      expect(sanitiseReturnTo("/invitations/abc123")).toBe(
        "/invitations/abc123",
      );
    });

    it("accepts /settings/preferences", () => {
      expect(sanitiseReturnTo("/settings/preferences")).toBe(
        "/settings/preferences",
      );
    });

    it("accepts path with query string", () => {
      expect(sanitiseReturnTo("/chat?tab=settings")).toBe("/chat?tab=settings");
    });

    it("accepts /", () => {
      expect(sanitiseReturnTo("/")).toBe("/");
    });

    it("accepts path with fragment", () => {
      expect(sanitiseReturnTo("/chat#section")).toBe("/chat#section");
    });
  });

  describe("protocol-relative attack vectors", () => {
    it("rejects //evil.com", () => {
      expect(sanitiseReturnTo("//evil.com")).toBe("");
    });

    it("rejects //evil.com/path", () => {
      expect(sanitiseReturnTo("//evil.com/path")).toBe("");
    });
  });

  describe("UNC path attack vectors", () => {
    it("rejects \\\\evil", () => {
      expect(sanitiseReturnTo("\\\\evil")).toBe("");
    });

    it("rejects \\\\evil\\share", () => {
      expect(sanitiseReturnTo("\\\\evil\\share")).toBe("");
    });
  });

  describe("absolute URL attack vectors", () => {
    it("rejects https://evil.com", () => {
      expect(sanitiseReturnTo("https://evil.com")).toBe("");
    });

    it("rejects http://evil.com/path", () => {
      expect(sanitiseReturnTo("http://evil.com/path")).toBe("");
    });

    it("rejects //user:pass@evil.com", () => {
      expect(sanitiseReturnTo("//user:pass@evil.com")).toBe("");
    });
  });

  describe("userinfo and host injection", () => {
    it("rejects path with userinfo via URL parser", () => {
      expect(sanitiseReturnTo("//user@localhost")).toBe("");
    });
  });

  describe("URL-parser normalisation mismatch", () => {
    it("rejects path where URL parser strips leading backslash", () => {
      // WHATWG URL parser treats /\\evil.com as http://evil.com/
      // Our guard catches this because pathname != raw.split("?")[0].
      expect(sanitiseReturnTo("/\\evil.com")).toBe("");
    });
  });

  describe("encoded payloads", () => {
    it("accepts %2F-encoded slashes in same-app paths (browser doesn't treat as redirect)", () => {
      // %2F decodes to /, but the URL parser keeps it encoded in pathname.
      // The resulting path is /evil.com on the same origin — not a redirect.
      expect(sanitiseReturnTo("/%2F%2Fevil.com")).toBe("/%2F%2Fevil.com");
    });

    it("rejects backslash-encoded protocol-relative", () => {
      expect(sanitiseReturnTo("/\\%2Fevil.com")).toBe("");
    });
  });

  describe("CRLF and special characters", () => {
    it("rejects path with CRLF injection", () => {
      expect(sanitiseReturnTo("/chat\r\nLocation: https://evil.com")).toBe("");
    });

    it("rejects path with newline", () => {
      expect(sanitiseReturnTo("/chat\nHost: evil.com")).toBe("");
    });
  });

  describe("long and unusual inputs", () => {
    it("handles very long paths", () => {
      const long = "/" + "a".repeat(2000);
      expect(sanitiseReturnTo(long)).toBe(long);
    });

    it("returns empty for whitespace-only", () => {
      expect(sanitiseReturnTo("   ")).toBe("");
    });
  });
});
