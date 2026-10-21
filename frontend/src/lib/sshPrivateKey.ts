// #1793 — SSH private-key save normalization.
//
// The live incident: a stored ssh-key carried ONE trailing space on
// EVERY line (PEM markers included). Stock OpenSSH 9.2p1 fails the file
// with "error in libcrypto" (its base64 decoder tolerates the space,
// the byte stream then misparses downstream), killing every SSH git
// operation provisioned from that secret. The generator in
// SecretsTab emits clean bytes — the corruption class is paste/transport
// reflow — so the SAVE path normalizes: any ssh-key value is
// per-line-trimmed and CRLF-normalized before it is submitted, the same
// repair the delivery belt (pkg/agentd/secrets/sshkey.go) applies as
// defense in depth.
export function normalizeSSHPrivateKeyForSave(value: string): string {
  return value
    .replace(/\r\n/g, "\n")
    .split("\n")
    .map((line) => line.replace(/[ \t]+$/g, "").replace(/^[ \t]+/g, ""))
    .join("\n");
}
