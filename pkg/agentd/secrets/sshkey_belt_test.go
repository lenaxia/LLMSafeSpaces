// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package secrets

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"golang.org/x/crypto/ssh"
)

// #1793: the delivered key carried ONE TRAILING SPACE on EVERY line
// (markers included — verified live: 70-char base64 rows reading as
// 71 cols, BEGIN/END lines suffixed too). Stock OpenSSH fails with the
// misleading "error in libcrypto"; the key material itself is intact.
// The writer must deliver PARSEABLE bytes or the repair, never the
// corruption.

// stagedKeyPath is where this package's bytes land: the staging tree's
// ssh entry (publish swaps the tree in atomically; the uid-1000
// supervisor delivers staged->targets verbatim, so the STAGED bytes are
// the contract this package owns).
const stagedKeyPath = "/sandbox-runtime/staged-secret-files/ssh/id_ed25519_github"

// corruptTrailingSpaces reproduces the live corruption byte-shape: one
// trailing space appended to every line, markers included (the helper
// first normalizes the input's own line endings to plain \n).
// corruptTrailingSpacesAndCRLF (review r1) stacks BOTH corruption
// families from the issue thread: CRLF endings AND the trailing space.
func corruptTrailingSpaces(clean string) string {
	lines := strings.Split(strings.ReplaceAll(clean, "\r\n", "\n"), "\n")
	for i, l := range lines {
		if l != "" {
			lines[i] = l + " "
		}
	}
	return strings.Join(lines, "\n")
}

// corruptTrailingSpacesAndCRLF: CRLF endings AND one trailing space per
// line (both corruption families from the issue thread at once).
func corruptTrailingSpacesAndCRLF(clean string) string {
	lines := strings.Split(strings.ReplaceAll(clean, "\r\n", "\n"), "\n")
	for i, l := range lines {
		if l != "" {
			lines[i] = l + " "
		}
	}
	return strings.ReplaceAll(strings.Join(lines, "\n"), "\n", "\r\n")
}

// genTestKey builds a real unencrypted OpenSSH ed25519 private key at
// runtime (no committed fixtures).
func genTestKey(t *testing.T) string {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	block, err := ssh.MarshalPrivateKey(priv, "generated@llmsafespaces-test")
	require.NoError(t, err)
	return string(pem.EncodeToMemory(block))
}

// PIN (writer emits parseable bytes): an ssh-key secret whose stored
// plaintext carries the #1793 corruption (trailing space per line,
// markers included) must be delivered REPAIRED — the staged file
// parses with stock OpenSSH semantics (x/crypto parse as the
// in-process equivalent) and carries no trailing whitespace.
func TestSSHKey_StagedBytesParseable_DespiteCorruptPlaintext(t *testing.T) {
	clean := genTestKey(t)
	m, fs := newFixture(t)

	res, err := m.Materialize([]Secret{{
		Type:      "ssh-key",
		Name:      "github",
		Metadata:  map[string]string{"key_type": "ed25519", "host": "github.com"},
		Plaintext: corruptTrailingSpaces(clean),
	}})
	require.NoError(t, err)
	require.Equal(t, OutcomeMaterialized, res.Results[0].Outcome)

	got := string(fs.contents[stagedKeyPath])
	for _, line := range strings.Split(got, "\n") {
		if line == "" {
			continue
		}
		require.NotEqual(t, " ", line[len(line)-1:], "no line may carry trailing whitespace: %q", line)
	}
	_, err = ssh.ParsePrivateKey([]byte(got))
	require.NoError(t, err, "the staged key must load (the live symptom was 'error in libcrypto')")
}

// PIN (repair regression): the repaired bytes are the CLEAN key —
// strict per-line trim reproduces the original PEM exactly (the
// corruption was formatting-only; the wrap is preserved).
func TestSSHKey_RepairReproducesCleanKey(t *testing.T) {
	clean := genTestKey(t)
	m, fs := newFixture(t)

	_, err := m.Materialize([]Secret{{
		Type:      "ssh-key",
		Name:      "github",
		Metadata:  map[string]string{"key_type": "ed25519", "host": "github.com"},
		Plaintext: corruptTrailingSpaces(clean),
	}})
	require.NoError(t, err)
	require.Equal(t, clean, string(fs.contents[stagedKeyPath]),
		"the repair must reproduce the original clean PEM byte-for-byte")
}

// PIN (never worse): a plaintext that is garbage EVEN after
// normalization is staged verbatim — the belt repairs formatting, it
// does not invent keys or reject the secret outright.
func TestSSHKey_UnparseableStagedVerbatim(t *testing.T) {
	m, fs := newFixture(t)
	garbage := "-----BEGIN OPENSSH PRIVATE KEY-----\nnot-a-key at all!! \n-----END OPENSSH PRIVATE KEY-----\n"

	_, err := m.Materialize([]Secret{{
		Type:      "ssh-key",
		Name:      "github",
		Metadata:  map[string]string{"key_type": "ed25519", "host": "github.com"},
		Plaintext: garbage,
	}})
	require.NoError(t, err)
	require.Equal(t, garbage, string(fs.contents[stagedKeyPath]),
		"unparseable-even-after-normalize plaintext must be delivered verbatim (never worse)")
}

// PIN (review r2 — the tolerance BOUNDARY): a PKCS#8-ENCRYPTED key
// ("ENCRYPTED PRIVATE KEY" envelope) parses as UNSUPPORTED in
// x/crypto — an untyped error, not PassphraseMissingError — so a
// corrupt one stages VERBATIM under the never-worse rule. This pins
// the documented boundary: the encrypted-key repair covers
// OpenSSH-format encrypted keys, not every encrypted PEM envelope.
func TestSSHKey_PKCS8EncryptedCorruptStagedVerbatim(t *testing.T) {
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	der, err := x509.MarshalPKCS8PrivateKey(priv)
	require.NoError(t, err)
	// The belt cannot distinguish really-encrypted from envelope-typed
	// bytes (it never decrypts); the pin needs exactly the failure mode
	// x/crypto source-verifies for this envelope: an unsupported-type
	// error, not PassphraseMissingError. pem.EncryptPEMBlock is gone
	// from this toolchain, so the fixture wraps real PKCS#8 DER in the
	// ENCRYPTED envelope — same parse path, same non-tolerated error.
	clean := string(pem.EncodeToMemory(&pem.Block{Type: "ENCRYPTED PRIVATE KEY", Bytes: der}))
	m, fs := newFixture(t)

	_, err = m.Materialize([]Secret{{
		Type:      "ssh-key",
		Name:      "github",
		Metadata:  map[string]string{"key_type": "ed25519", "host": "github.com"},
		Plaintext: corruptTrailingSpaces(clean),
	}})
	require.NoError(t, err)
	require.Equal(t, corruptTrailingSpaces(clean), string(fs.contents[stagedKeyPath]),
		"PKCS#8-encrypted corrupt keys stage VERBATIM (x/crypto: unsupported, not PassphraseMissingError) — the documented boundary")
}

// PIN (clean passthrough): a valid key is staged byte-identical — the
// belt must not gratuitously rewrite healthy keys.
func TestSSHKey_CleanPassthrough(t *testing.T) {
	clean := genTestKey(t)
	m, fs := newFixture(t)

	_, err := m.Materialize([]Secret{{
		Type:      "ssh-key",
		Name:      "github",
		Metadata:  map[string]string{"key_type": "ed25519", "host": "github.com"},
		Plaintext: clean,
	}})
	require.NoError(t, err)
	require.Equal(t, clean, string(fs.contents[stagedKeyPath]))
}

// PIN (review r1 — the encrypted blind spot): a passphrase-PROTECTED
// key never parses in-process (x/crypto reports PassphraseMissingError
// — validity, not corruption), so a corrupt one must still be repaired:
// the staged bytes equal the CLEAN encrypted PEM and still report
// passphrase-protected (proof the blob was repaired, not mangled).
func TestSSHKey_EncryptedCorruptKeyRepaired(t *testing.T) {
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	block, err := ssh.MarshalPrivateKeyWithPassphrase(priv, "generated@llmsafespaces-test", []byte("passphrase-1793"))
	require.NoError(t, err)
	clean := string(pem.EncodeToMemory(block))
	m, fs := newFixture(t)

	_, err = m.Materialize([]Secret{{
		Type:      "ssh-key",
		Name:      "github",
		Metadata:  map[string]string{"key_type": "ed25519", "host": "github.com"},
		Plaintext: corruptTrailingSpaces(clean),
	}})
	require.NoError(t, err)
	staged := string(fs.contents[stagedKeyPath])
	require.Equal(t, clean, staged, "a corrupt ENCRYPTED key must be repaired to the clean PEM byte-for-byte")
	_, err = ssh.ParsePrivateKey([]byte(staged))
	var missing *ssh.PassphraseMissingError
	require.ErrorAs(t, err, &missing, "the repaired key must still be passphrase-protected (repaired, not mangled)")
}

// PIN (review r1 — combined fixture): CRLF endings AND the trailing
// space per line at once — both corruption families from the issue
// thread repaired to the clean PEM.
func TestSSHKey_CorruptCRLFAndTrailingSpacesRepaired(t *testing.T) {
	clean := genTestKey(t)
	m, fs := newFixture(t)

	_, err := m.Materialize([]Secret{{
		Type:      "ssh-key",
		Name:      "github",
		Metadata:  map[string]string{"key_type": "ed25519", "host": "github.com"},
		Plaintext: corruptTrailingSpacesAndCRLF(clean),
	}})
	require.NoError(t, err)
	require.Equal(t, clean, string(fs.contents[stagedKeyPath]),
		"CRLF + trailing-space corruption must repair to the clean PEM")
}
