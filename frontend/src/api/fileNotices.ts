// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

// fileNotices.ts — decoding of file-part Custom payloads (issue #1307)
// into user-visible notice text. Shared by the history transform and the
// live SSE path so both surfaces render the same honest record: image
// bytes never ride the platform wire, and a downgraded (omitted) image
// names WHY it is gone and how to recover the session.

interface FilePartPayload {
  type?: string;
  mime?: string;
  filename?: string;
  omitted?: boolean;
  notice?: string;
}

// decodeFilePartData accepts the Custom data in any of its wire forms —
// a parsed object (REST history), a JSON string, or raw JSON bytes
// (protobuf custom part on SSE) — and returns the payload when it is a
// file part. The bytes check is realm-safe (Object.prototype.toString,
// not instanceof — jsdom/protobuf runtimes can hand over cross-realm
// Uint8Arrays where instanceof is false).
function isUint8ArrayBytes(value: unknown): value is Uint8Array {
  return Object.prototype.toString.call(value) === "[object Uint8Array]";
}

export function decodeFilePartData(data: unknown): FilePartPayload | null {
  let value: unknown = data;
  if (isUint8ArrayBytes(value)) {
    try {
      value = JSON.parse(new TextDecoder().decode(value));
    } catch {
      return null;
    }
  }
  if (typeof value === "string") {
    try {
      value = JSON.parse(value);
    } catch {
      return null;
    }
  }
  if (value && typeof value === "object" && (value as FilePartPayload).type === "file") {
    return value as FilePartPayload;
  }
  return null;
}

// fileNoticeText renders the display line for a file-part payload: the
// full omission notice when the image was downgraded (text-only model),
// else a neutral attachment record. Null when the payload is not a file
// part.
export function fileNoticeText(data: unknown): string | null {
  const fp = decodeFilePartData(data);
  if (!fp) return null;
  if (fp.omitted) {
    const label = fp.filename ? `${fp.filename} (${fp.mime ?? "image"})` : fp.mime ?? "image";
    return `${fp.notice ?? "[image omitted]"} — ${label}`;
  }
  const label = fp.filename ? `${fp.filename} (${fp.mime ?? "file"})` : fp.mime ?? "file";
  return `[file attached: ${label}]`;
}
