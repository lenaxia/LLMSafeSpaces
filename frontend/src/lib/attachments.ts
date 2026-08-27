// TypeScript port of the v1 attachment-manifest parser
// (pkg/session/attachments/attachments.go, Epic 68 D7/D11). The manifest
// format is a stable API contract locked by the golden fixtures in the Go
// package's testdata/ — this port is verified against those same fixtures.
//
// The frontend only ever PARSES (history render strips the trailing block
// and renders chips); composition is server-side at send acceptance
// (D7 compose-once, D11 never-mutate-client-side).
//
// Format v1: one line per file after the user text separated by one blank
// line, each line:
//
//   [llmsafespaces:attachment path="<path>" name="<name>"]
//
// Attribute values backslash-escape quotes (") and backslashes (\). The
// parser consumes ONLY a trailing block of such lines; interior lines stay
// text (forged-line defense, U1.6.10). Unknown versions/attributes are
// plain text (forward compatibility, U1.3.19).

export interface Attachment {
  path: string;
  name: string;
}

export interface ParsedAttachments {
  text: string;
  attachments: Attachment[] | null;
}

const attachmentLinePattern =
  /^\[llmsafespaces:attachment path="((?:\\\\|\\"|[^"\\])*)" name="((?:\\\\|\\"|[^"\\])*)"\]$/;

export function parseAttachments(text: string): ParsedAttachments {
  const core = trimTrailingNewlines(text);
  if (core === "") {
    return { text, attachments: null };
  }
  const lines = core.split("\n");
  const start = trailingBlockStart(lines);
  if (start === lines.length) {
    return { text, attachments: null };
  }
  let end = start;
  if (end > 0 && lines[end - 1] === "") {
    end--;
  }
  const attachments: Attachment[] = [];
  for (const line of lines.slice(start)) {
    const m = attachmentLinePattern.exec(line);
    if (!m) continue;
    attachments.push({ path: unescapeAttribute(m[1]!), name: unescapeAttribute(m[2]!) });
  }
  return { text: lines.slice(0, end).join("\n"), attachments };
}

function trailingBlockStart(lines: string[]): number {
  let i = lines.length;
  while (i > 0 && attachmentLinePattern.test(lines[i - 1]!)) {
    i--;
  }
  return i;
}

function trimTrailingNewlines(text: string): string {
  let end = text.length;
  while (end > 0 && text[end - 1] === "\n") {
    end--;
  }
  return text.slice(0, end);
}

function unescapeAttribute(v: string): string {
  return v.replace(/\\(["\\])/g, "$1");
}
