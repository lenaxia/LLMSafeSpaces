import { describe, expect, it, vi, beforeEach } from "vitest";
import { uploadsApi } from "./uploads";
import { ApiClientError } from "./client";
import { getEnv } from "../env";

vi.mock("../env", () => ({
  getEnv: vi.fn(() => ({ apiBaseUrl: "https://api.test/api/v1" })),
}));

function mockFetch(status: number, body: unknown) {
  return vi.fn().mockResolvedValue(
    new Response(JSON.stringify(body), {
      status,
      headers: { "Content-Type": "application/json" },
    }),
  );
}

describe("uploadsApi.upload", () => {
  beforeEach(() => {
    vi.restoreAllMocks();
  });

  it("POSTs multipart FormData with field name 'file' to the workspace uploads route", async () => {
    const fetchMock = mockFetch(201, { path: "/workspace/uploads/u-notes.txt", name: "notes.txt", size: 3 });
    vi.stubGlobal("fetch", fetchMock);
    const file = new File(["abc"], "notes.txt", { type: "text/plain" });

    const res = await uploadsApi.upload("ws-1", file);

    expect(res).toEqual({ path: "/workspace/uploads/u-notes.txt", name: "notes.txt", size: 3 });
    expect(fetchMock).toHaveBeenCalledTimes(1);
    const [url, init] = fetchMock.mock.calls[0] as [string, RequestInit];
    expect(url).toBe("https://api.test/api/v1/workspaces/ws-1/uploads");
    expect(init.method).toBe("POST");
    expect(init.credentials).toBe("include");
    expect(init.body).toBeInstanceOf(FormData);
    expect((init.body as FormData).get("file")).toBe(file);
    const headers = init.headers as Record<string, string>;
    expect(headers["Content-Type"]).toBeUndefined();
  });

  it("throws ApiClientError with the server body on a phase-gate 409", async () => {
    vi.stubGlobal("fetch", mockFetch(409, { error: "workspace is not Active", phase: "Suspended" }));
    const file = new File(["x"], "x.txt");

    await expect(uploadsApi.upload("ws-1", file)).rejects.toMatchObject({
      name: "ApiClientError",
      status: 409,
      body: { error: "workspace is not Active", phase: "Suspended" },
    });
  });

  it("throws ApiClientError on cap 413 and disk 507", async () => {
    vi.stubGlobal("fetch", mockFetch(413, { error: "upload exceeds size cap" }));
    await expect(uploadsApi.upload("ws-1", new File(["x"], "x.txt"))).rejects.toBeInstanceOf(ApiClientError);

    vi.stubGlobal("fetch", mockFetch(507, { error: "insufficient storage" }));
    await expect(uploadsApi.upload("ws-1", new File(["x"], "x.txt"))).rejects.toMatchObject({
      status: 507,
    });
  });

  it("does not set a JSON Content-Type (browser must set the multipart boundary)", async () => {
    const fetchMock = mockFetch(201, { path: "/p", name: "n", size: 0 });
    vi.stubGlobal("fetch", fetchMock);
    await uploadsApi.upload("ws-1", new File(["x"], "x.txt"));
    const headers = (fetchMock.mock.calls[0] as [string, RequestInit])[1].headers as Record<string, string>;
    expect(Object.keys(headers).some((k) => k.toLowerCase() === "content-type")).toBe(false);
  });

  it("uses the env API base URL", async () => {
    const fetchMock = mockFetch(201, { path: "/p", name: "n", size: 0 });
    vi.stubGlobal("fetch", fetchMock);
    await uploadsApi.upload("ws-9", new File(["x"], "x.txt"));
    expect((fetchMock.mock.calls[0] as [string])[0]).toContain("/api/v1/workspaces/ws-9/uploads");
    expect(getEnv).toHaveBeenCalled();
  });
});
