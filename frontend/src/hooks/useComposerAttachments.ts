import { useCallback, useEffect, useRef, useState } from "react";
import { ApiClientError } from "../api/client";
import { uploadsApi } from "../api/uploads";

// Mirrors the backend per-send file cap (pkg/session/attachments maxFiles).
export const MAX_ATTACHMENTS = 10;

export interface PendingAttachment {
  /** Client-side identity. A fresh uuid per upload attempt — retry never reuses a stale id (U1.6.18). */
  id: string;
  /** Original file name as picked (display only; the server sanitizes its own copy). */
  name: string;
  /** Byte size of the picked file (display only). */
  size: number;
  status: "uploading" | "attached" | "error";
  /** Server path once the upload settled (status === "attached"). */
  path?: string;
  error?: string;
}

/**
 * Workspace-scoped composer attachment state (Epic 68 US-68.5, D17/U1.6.17).
 *
 * Chips persist across session switches inside a workspace and clear on
 * workspace switch. Send is gated on `uploading` (D17): the caller disables
 * send while any chip is mid-upload; failed chips do NOT block send — the
 * user's explicit choice — and are excluded from `attachedFiles`. The picked
 * File objects live in a ref map (never render state) so retry can re-upload
 * the original bytes under a fresh uuid.
 */
export function useComposerAttachments(workspaceId: string | undefined) {
  const [chips, setChips] = useState<PendingAttachment[]>([]);
  const [capViolation, setCapViolation] = useState(false);
  const prevWorkspaceRef = useRef(workspaceId);
  const filesRef = useRef(new Map<string, File>());

  useEffect(() => {
    if (prevWorkspaceRef.current !== workspaceId) {
      prevWorkspaceRef.current = workspaceId;
      filesRef.current.clear();
      setChips([]);
      setCapViolation(false);
    }
  }, [workspaceId]);

  const uploadChip = useCallback(
    (id: string) => {
      if (!workspaceId) return;
      const file = filesRef.current.get(id);
      if (!file) return;
      uploadsApi
        .upload(workspaceId, file)
        .then((res) => {
          setChips((prev) =>
            prev.map((c) => (c.id === id ? { ...c, status: "attached", path: res.path, error: undefined } : c)),
          );
        })
        .catch((err: unknown) => {
          setChips((prev) =>
            prev.map((c) =>
              c.id === id
                ? { ...c, status: "error", error: uploadErrorMessage(err) }
                : c,
            ),
          );
        });
    },
    [workspaceId],
  );

  const addFiles = useCallback(
    (files: File[]) => {
      if (files.length === 0 || !workspaceId) return;
      const room = MAX_ATTACHMENTS - chips.length;
      if (room <= 0) {
        setCapViolation(true);
        return;
      }
      const accepted = files.slice(0, room);
      if (accepted.length < files.length) {
        setCapViolation(true);
      }
      const newChips: PendingAttachment[] = accepted.map((file) => ({
        id: crypto.randomUUID(),
        name: file.name,
        size: file.size,
        status: "uploading",
      }));
      newChips.forEach((chip, i) => filesRef.current.set(chip.id, accepted[i]!));
      // Defensive clamp: two picker events inside one render batch both
      // read the same closure chips.length — the slice keeps the visual
      // cap invariant even then.
      setChips((prev) => [...prev, ...newChips].slice(0, MAX_ATTACHMENTS));
      newChips.forEach((chip) => uploadChip(chip.id));
    },
    [workspaceId, chips.length, uploadChip],
  );

  const remove = useCallback((id: string) => {
    filesRef.current.delete(id);
    setChips((prev) => prev.filter((c) => c.id !== id));
  }, []);

  const retry = useCallback(
    (id: string) => {
      const file = filesRef.current.get(id);
      if (!file) return;
      filesRef.current.delete(id);
      const replacement: PendingAttachment = {
        id: crypto.randomUUID(),
        name: file.name,
        size: file.size,
        status: "uploading",
      };
      filesRef.current.set(replacement.id, file);
      setChips((prev) => prev.map((c) => (c.id === id ? replacement : c)));
      uploadChip(replacement.id);
    },
    [uploadChip],
  );

  const clearAttached = useCallback(() => {
    setChips((prev) => {
      prev.filter((c) => c.status === "attached").forEach((c) => filesRef.current.delete(c.id));
      return prev.filter((c) => c.status !== "attached");
    });
  }, []);

  const dismissCapViolation = useCallback(() => setCapViolation(false), []);

  const uploading = chips.some((c) => c.status === "uploading");
  const attachedFiles = chips.filter((c) => c.status === "attached" && c.path).map((c) => c.path!);

  return {
    chips,
    uploading,
    attachedFiles,
    capViolation,
    addFiles,
    remove,
    retry,
    clearAttached,
    dismissCapViolation,
  };
}

// Upload-failure chip text: the API's message, with the workspace phase
// appended when the 409 body carries one (Epic 68 D5 — the user sees WHY
// the workspace is not accepting uploads, e.g. "(phase: Suspended)").
function uploadErrorMessage(err: unknown): string {
  if (err instanceof ApiClientError) {
    const phase = err.body?.phase;
    if (phase) return `${err.message} (phase: ${phase})`;
    return err.message;
  }
  return err instanceof Error ? err.message : "Upload failed";
}
