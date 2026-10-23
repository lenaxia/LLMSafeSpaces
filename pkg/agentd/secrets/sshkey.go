// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package secrets

import (
	"errors"
	"strings"

	"golang.org/x/crypto/ssh"
)

// sshkey.go — the #1793 belt at the delivery writer.
//
// The live incident: a stored ssh-key plaintext carried ONE trailing
// space on EVERY line (PEM markers included — 70-char base64 rows
// reading as 71 columns). The key MATERIAL was intact, but stock
// OpenSSH's base64 decoder tolerates the whitespace and the byte
// stream then misparses downstream — surfacing as the misleading
// "error in libcrypto" and killing every SSH git operation in fresh
// workspaces. The corruption class is formatting-only (editor paste,
// transport reflow — the frontend generator itself is clean, verified
// by git history), so the writer validates and repairs FORMAT, never
// key material.
//
// Belt semantics (pinned in sshkey_belt_test.go):
//   - a plaintext that is structurally clean (no line carries edge
//     whitespace, no CRLF) AND parses — or is passphrase-protected,
//     which x/crypto reports as PassphraseMissingError, not failure —
//     is staged byte-identical (no gratuitous rewrite of healthy keys);
//   - anything else gets ONE repair attempt — strict per-line trim of
//     leading/trailing whitespace, CRLF normalization — and the
//     repaired bytes are staged only if they, too, parse or are
//     passphrase-protected. Structural detection, not parser
//     detection: Go's x/crypto TOLERATES the trailing-space class that
//     makes stock OpenSSH fail with "error in libcrypto" (its base64
//     decoder ignores the space; OpenSSH's misparses downstream), so
//     validity alone under-detects. The passphrase tolerance matters
//     for the same reason on the other side: an ENCRYPTED key never
//     parses in-process, so without it a corrupt encrypted key would
//     fall to the verbatim branch and stay corrupt. Per-line
//     whitespace trim cannot alter key material — the base64 stream
//     (or the encrypted blob it encodes) is byte-identical once the
//     padding is gone (pinned: repaired == clean, and the repaired
//     key still reports passphrase-protected, not mangled). The
//     encrypted tolerance covers OPENSSH-format encrypted keys (the
//     platform's format for ssh-key secrets); other encrypted PEM
//     envelopes — PKCS#8 "ENCRYPTED PRIVATE KEY" — parse as
//     unsupported in x/crypto (an untyped error) and stage VERBATIM
//     under the never-worse rule (pinned; review r2);
//   - a plaintext that is garbage even after normalization is staged
//     VERBATIM: the belt repairs formatting, it never invents keys or
//     rejects the secret (never worse than the pre-belt behavior).
//
// Validation uses x/crypto's ssh.ParsePrivateKey — the in-process
// equivalent of the suggested `ssh-keygen -y` check (no subprocess at
// boot, same OpenSSH wire format).
func normalizeOpenSSHPrivateKey(plaintext string) (staged string, repaired bool) {
	if structurallyCleanPEMLines(plaintext) && parsesOrPassphraseProtected([]byte(plaintext)) {
		return plaintext, false
	}
	lines := strings.Split(strings.ReplaceAll(plaintext, "\r\n", "\n"), "\n")
	for i, l := range lines {
		lines[i] = strings.TrimSpace(l)
	}
	repairedText := strings.Join(lines, "\n")
	if parsesOrPassphraseProtected([]byte(repairedText)) {
		return repairedText, true
	}
	// Unparseable even after the format repair: deliver verbatim —
	// the belt never makes delivery worse than the stored secret.
	return plaintext, false
}

// parsesOrPassphraseProtected reports whether the bytes are a private
// key this process can use — or an ENCRYPTED one it cannot (review r1:
// ParsePrivateKey fails passphrase-protected keys with
// PassphraseMissingError, which is validity, not corruption; treating
// it as failure left corrupt encrypted keys unrepaired).
func parsesOrPassphraseProtected(pemBytes []byte) bool {
	_, err := ssh.ParsePrivateKey(pemBytes)
	if err == nil {
		return true
	}
	var missing *ssh.PassphraseMissingError
	return errors.As(err, &missing)
}

// structurallyCleanPEMLines reports whether every line is free of
// leading/trailing whitespace and the text carries no CRLF — the byte
// shape stock OpenSSH round-trips. The #1793 corruption (one trailing
// space per line, markers included) fails this by construction.
func structurallyCleanPEMLines(text string) bool {
	if strings.Contains(text, "\r") {
		return false
	}
	for _, l := range strings.Split(text, "\n") {
		if l != strings.TrimSpace(l) {
			return false
		}
	}
	return true
}
