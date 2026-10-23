import { describe, expect, it } from "vitest";
import { normalizeSSHPrivateKeyForSave } from "./sshPrivateKey";

// #1793 — the corruption class that killed SSH git ops: one trailing
// space per line (markers included). NOTE: bodies are deliberately
// LOW-ENTROPY repeats — this suite pins whitespace normalization, not
// key material, and realistic base64 trips the repo's gitleaks
// private-key heuristic (never a real key).
const BEGIN = "-----BEGIN OPENSSH PRIVATE KEY-----";
const END = "-----END OPENSSH PRIVATE KEY-----";
const body = (n: number) => "A".repeat(n);

describe("normalizeSSHPrivateKeyForSave", () => {
  it("strips the #1793 trailing-space-per-line corruption (markers included)", () => {
    const clean = [BEGIN, body(70), body(70), body(64), END, ""].join("\n");
    const corrupt = clean
      .split("\n")
      .map((l) => (l === "" ? l : l + " "))
      .join("\n");
    expect(normalizeSSHPrivateKeyForSave(corrupt)).toBe(clean);
  });

  it("normalizes CRLF and leading whitespace, preserves inner content exactly", () => {
    const inVal = `${BEGIN}\r\n  ${body(20)}== \r\n${END}\r\n`;
    expect(normalizeSSHPrivateKeyForSave(inVal)).toBe(`${BEGIN}\n${body(20)}==\n${END}\n`);
  });

  it("passes clean values through byte-identical", () => {
    const clean = `${BEGIN}\n${body(28)}==\n${END}\n`;
    expect(normalizeSSHPrivateKeyForSave(clean)).toBe(clean);
  });

  it("leaves empty lines intact (final newline preserved)", () => {
    expect(normalizeSSHPrivateKeyForSave("a \n\nb \n")).toBe("a\n\nb\n");
  });
});
