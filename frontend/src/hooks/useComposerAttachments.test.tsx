import { describe, expect, it, vi, beforeEach } from "vitest";
import { renderHook, act, waitFor } from "@testing-library/react";
import { useComposerAttachments, MAX_ATTACHMENTS } from "./useComposerAttachments";
import { uploadsApi } from "../api/uploads";

vi.mock("../api/uploads", () => ({
  uploadsApi: { upload: vi.fn() },
}));

const uploadMock = vi.mocked(uploadsApi.upload);

function fileOf(name: string, bytes = 10): File {
  return new File(["x".repeat(bytes)], name, { type: "application/octet-stream" });
}

function okPath(name: string): string {
  return `/workspace/uploads/11111111-2222-3333-4444-555555555555-${name}`;
}

describe("useComposerAttachments", () => {
  beforeEach(() => {
    uploadMock.mockReset();
    uploadMock.mockResolvedValue({ path: okPath("f.txt"), name: "f.txt", size: 1 });
  });

  it("uploads a picked file and marks the chip attached with the server path", async () => {
    uploadMock.mockResolvedValue({ path: okPath("notes.txt"), name: "notes.txt", size: 3 });
    const { result } = renderHook(() => useComposerAttachments("ws-1"));

    act(() => result.current.addFiles([fileOf("notes.txt")]));

    expect(result.current.chips).toHaveLength(1);
    expect(result.current.chips[0]).toMatchObject({ name: "notes.txt", size: 10, status: "uploading" });
    expect(result.current.uploading).toBe(true);

    await waitFor(() => expect(result.current.chips[0]!.status).toBe("attached"));
    expect(result.current.chips[0]!.path).toBe(okPath("notes.txt"));
    expect(result.current.uploading).toBe(false);
    expect(uploadMock).toHaveBeenCalledWith("ws-1", expect.any(File));
  });

  it("marks the chip error on upload failure and keeps it removable", async () => {
    uploadMock.mockRejectedValue(new Error("507"));
    const { result } = renderHook(() => useComposerAttachments("ws-1"));

    act(() => result.current.addFiles([fileOf("big.bin")]));
    await waitFor(() => expect(result.current.chips[0]!.status).toBe("error"));

    expect(result.current.chips[0]!.error).toBeTruthy();
    expect(result.current.uploading).toBe(false);

    act(() => result.current.remove(result.current.chips[0]!.id));
    expect(result.current.chips).toHaveLength(0);
  });

  it("surfaces the workspace phase in the chip error on a 409 (Epic 68 D5/E4)", async () => {
    const { ApiClientError } = await import("../api/client");
    uploadMock.mockRejectedValue(
      new ApiClientError(409, { error: "workspace not active", phase: "Suspended" }),
    );
    const { result } = renderHook(() => useComposerAttachments("ws-1"));

    act(() => result.current.addFiles([fileOf("f.txt")]));
    await waitFor(() => expect(result.current.chips[0]!.status).toBe("error"));

    expect(result.current.chips[0]!.error).toBe("workspace not active (phase: Suspended)");
  });

  it("retry performs a NEW upload with a fresh id and no stale path reuse", async () => {
    let calls = 0;
    uploadMock.mockImplementation(async () => {
      calls++;
      if (calls === 1) throw new Error("boom");
      return { path: okPath("retry.txt"), name: "retry.txt", size: 1 };
    });
    const { result } = renderHook(() => useComposerAttachments("ws-1"));
    act(() => result.current.addFiles([fileOf("retry.txt")]));
    await waitFor(() => expect(result.current.chips[0]!.status).toBe("error"));
    const failedId = result.current.chips[0]!.id;

    act(() => result.current.retry(failedId));
    await waitFor(() => expect(result.current.chips[0]!.status).toBe("attached"));

    expect(uploadMock).toHaveBeenCalledTimes(2);
    expect(result.current.chips[0]!.id).not.toBe(failedId);
    expect(result.current.chips[0]!.path).toBe(okPath("retry.txt"));
  });

  it("blocks the 11th file client-side (cap mirror, U1.6.6)", () => {
    uploadMock.mockResolvedValue({ path: okPath("f"), name: "f", size: 1 });
    const { result } = renderHook(() => useComposerAttachments("ws-1"));

    const ten = Array.from({ length: MAX_ATTACHMENTS }, (_, i) => fileOf(`f${i}.txt`));
    act(() => result.current.addFiles(ten));
    expect(result.current.chips).toHaveLength(MAX_ATTACHMENTS);

    act(() => result.current.addFiles([fileOf("eleven.txt")]));
    expect(result.current.chips).toHaveLength(MAX_ATTACHMENTS);
    expect(result.current.chips.some((c) => c.name === "eleven.txt")).toBe(false);
    expect(result.current.capViolation).toBe(true);
  });

  it("enforces the cap across a multi-select batch (5 picked, 7 existing → 3 admitted)", () => {
    uploadMock.mockResolvedValue({ path: okPath("f"), name: "f", size: 1 });
    const { result } = renderHook(() => useComposerAttachments("ws-1"));
    act(() => result.current.addFiles(Array.from({ length: 7 }, (_, i) => fileOf(`e${i}.txt`))));
    act(() => result.current.addFiles(Array.from({ length: 5 }, (_, i) => fileOf(`n${i}.txt`))));

    expect(result.current.chips).toHaveLength(MAX_ATTACHMENTS);
    expect(result.current.capViolation).toBe(true);
  });

  it("multi-select of 5 creates 5 uploading chips and 5 uploads", () => {
    uploadMock.mockResolvedValue({ path: okPath("f"), name: "f", size: 1 });
    const { result } = renderHook(() => useComposerAttachments("ws-1"));
    act(() => result.current.addFiles(Array.from({ length: 5 }, (_, i) => fileOf(`m${i}.txt`))));

    expect(result.current.chips).toHaveLength(5);
    expect(uploadMock).toHaveBeenCalledTimes(5);
  });

  it("same file attached twice → two chips, two uploads (no client dedup)", () => {
    uploadMock.mockResolvedValue({ path: okPath("dup.txt"), name: "dup.txt", size: 2 });
    const { result } = renderHook(() => useComposerAttachments("ws-1"));
    const f = fileOf("dup.txt");
    act(() => result.current.addFiles([f]));
    act(() => result.current.addFiles([f]));

    expect(result.current.chips).toHaveLength(2);
    expect(uploadMock).toHaveBeenCalledTimes(2);
  });

  it("clears chips on workspace switch (workspace-scoped state)", () => {
    uploadMock.mockResolvedValue({ path: okPath("a.txt"), name: "a.txt", size: 1 });
    const { result, rerender } = renderHook(({ ws }) => useComposerAttachments(ws), {
      initialProps: { ws: "ws-1" as string | undefined },
    });
    act(() => result.current.addFiles([fileOf("a.txt")]));
    expect(result.current.chips).toHaveLength(1);

    rerender({ ws: "ws-2" });
    expect(result.current.chips).toHaveLength(0);
  });

  it("keeps chips when the workspace id stays the same (session switch persists)", () => {
    uploadMock.mockResolvedValue({ path: okPath("a.txt"), name: "a.txt", size: 1 });
    const { result, rerender } = renderHook(({ ws }) => useComposerAttachments(ws), {
      initialProps: { ws: "ws-1" },
    });
    act(() => result.current.addFiles([fileOf("a.txt")]));
    rerender({ ws: "ws-1" });
    expect(result.current.chips).toHaveLength(1);
  });

  it("clearAttached removes only attached chips; error chips stay for the user's choice", async () => {
    uploadMock.mockImplementation(async (_ws: string, file: File) => {
      if (file.name === "bad.txt") throw new Error("nope");
      return { path: okPath("good.txt"), name: "good.txt", size: 1 };
    });
    const { result } = renderHook(() => useComposerAttachments("ws-1"));
    act(() => result.current.addFiles([fileOf("good.txt"), fileOf("bad.txt")]));
    await waitFor(() => expect(result.current.chips.some((c) => c.status === "error")).toBe(true));

    act(() => result.current.clearAttached());

    expect(result.current.chips).toHaveLength(1);
    expect(result.current.chips[0]!.status).toBe("error");
  });

  it("attachedFiles lists settled paths only (uploading and error excluded)", async () => {
    let release: (v: { path: string; name: string; size: number }) => void = () => {};
    uploadMock.mockImplementation(
      (_ws: string, file: File) =>
        new Promise((resolve) => {
          if (file.name === "slow.txt") release = resolve;
          else resolve({ path: okPath(file.name), name: file.name, size: 1 });
        }),
    );
    const { result } = renderHook(() => useComposerAttachments("ws-1"));
    act(() => result.current.addFiles([fileOf("fast.txt"), fileOf("slow.txt")]));

    await waitFor(() => expect(result.current.chips.find((c) => c.name === "fast.txt")!.status).toBe("attached"));
    expect(result.current.attachedFiles).toEqual([okPath("fast.txt")]);

    act(() => release({ path: okPath("slow.txt"), name: "slow.txt", size: 1 }));
    await waitFor(() =>
      expect(result.current.attachedFiles).toEqual([okPath("fast.txt"), okPath("slow.txt")]),
    );
  });

  it("a failed upload does not block sending (uploading=false once settled)", async () => {
    uploadMock.mockRejectedValue(new Error("err"));
    const { result } = renderHook(() => useComposerAttachments("ws-1"));
    act(() => result.current.addFiles([fileOf("x.txt")]));
    await waitFor(() => expect(result.current.uploading).toBe(false));
    expect(result.current.chips[0]!.status).toBe("error");
  });

  it("dismisses a cap-violation notice", () => {
    const { result } = renderHook(() => useComposerAttachments("ws-1"));
    act(() => result.current.addFiles(Array.from({ length: 11 }, (_, i) => fileOf(`f${i}.txt`))));
    expect(result.current.capViolation).toBe(true);
    act(() => result.current.dismissCapViolation());
    expect(result.current.capViolation).toBe(false);
  });
});
