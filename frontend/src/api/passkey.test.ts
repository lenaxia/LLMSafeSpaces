import { describe, expect, it, vi } from "vitest";
import { passkeyApi } from "./passkey";

vi.mock("./client", () => ({
  api: {
    post: vi.fn(),
    get: vi.fn(),
  },
}));

import { api } from "./client";

describe("passkeyApi", () => {
  it("registerBegin posts to /auth/passkey/register/begin with {email, name}", () => {
    vi.mocked(api.post).mockResolvedValueOnce({ options: {}, sessionToken: "tok" });
    passkeyApi.registerBegin("a@b.com", "Alice");
    expect(api.post).toHaveBeenCalledWith("/auth/passkey/register/begin", { email: "a@b.com", name: "Alice" });
  });

  it("registerFinish posts with sessionToken, email, response, name", () => {
    vi.mocked(api.post).mockResolvedValueOnce({ token: "t" });
    passkeyApi.registerFinish("tok", "a@b.com", { id: "x" }, "Alice");
    expect(api.post).toHaveBeenCalledWith("/auth/passkey/register/finish", {
      sessionToken: "tok", email: "a@b.com", response: { id: "x" }, name: "Alice",
    });
  });

  it("loginBegin posts to /auth/passkey/login/begin with {email}", () => {
    vi.mocked(api.post).mockResolvedValueOnce({ options: {}, sessionToken: "tok" });
    passkeyApi.loginBegin("a@b.com");
    expect(api.post).toHaveBeenCalledWith("/auth/passkey/login/begin", { email: "a@b.com" });
  });

  it("loginFinish posts with sessionToken, email, response", () => {
    vi.mocked(api.post).mockResolvedValueOnce({ token: "t" });
    passkeyApi.loginFinish("tok", "a@b.com", { id: "x" });
    expect(api.post).toHaveBeenCalledWith("/auth/passkey/login/finish", {
      sessionToken: "tok", email: "a@b.com", response: { id: "x" },
    });
  });

  it("recover posts to /auth/passkey/recover with {email, code}", () => {
    vi.mocked(api.post).mockResolvedValueOnce({ token: "t", mustEnrollPasskey: true });
    passkeyApi.recover("a@b.com", "CODE");
    expect(api.post).toHaveBeenCalledWith("/auth/passkey/recover", { email: "a@b.com", code: "CODE" });
  });
});
