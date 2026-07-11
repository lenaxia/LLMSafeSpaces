// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package secrets

// Tests for Epic 17 G2 / G20 remediation.
//
// These tests validate that the Materializer correctly:
//
//   1. Resists shell injection via single quotes, dollar signs, backticks,
//      newlines, and other metacharacters in plaintext values (G2).
//   2. Creates files with mode 0600 atomically, with no TOCTOU window
//      between creation and chmod (G20).
//   3. Rejects path traversal attempts in mount_path with both ".." and
//      absolute-path injection.
//   4. Rejects malformed var_name, name, host, key_type, and protocol with
//      a Skipped (not Failed) outcome so a single bad secret doesn't
//      block pod boot.
//   5. Round-trips arbitrary byte sequences through FormatEnvLine /
//      ParseEnvLine, so the bash `source` consumer gets the exact value
//      the user supplied.

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	sec "github.com/lenaxia/llmsafespaces/pkg/secrets"
	"github.com/stretchr/testify/require"
)

// requireBash skips the test if /bin/bash is unavailable. The bash-execution
// tests are the ground truth for G2: the actual consumer of the env file is
// a bash `source` directive, so we exercise it directly.
func requireBash(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available; skipping shell-injection regression")
	}
}

// requireBashSourceProducesValue writes envFileContent to a temp file,
// sources it from a bash subprocess, and asserts:
//
//  1. The subprocess exits with status 0 (no syntax error, no failed
//     command from injection).
//  2. The variable VAR_NAME is set to the exact wantValue (byte-for-byte).
//  3. No unexpected variables leak (a successful injection often sets
//     extra env vars; we sample-check for HIJACK).
//  4. Stderr is empty (a partially-executed injection often produces
//     "command not found" or similar diagnostics).
//
// This is the only test that catches the class of bug where pure Go
// round-trip succeeds but the actual bash parser interprets the line
// differently.
func requireBashSourceProducesValue(t *testing.T, envFileContent, varName, wantValue string) {
	t.Helper()

	dir := t.TempDir()
	envPath := filepath.Join(dir, "secrets-env")
	require.NoError(t, os.WriteFile(envPath, []byte(envFileContent), 0o600))

	// Build the script. The envPath is interpolated as a bash-single-
	// quoted string so $() / backtick expansion can't fire on the path
	// itself (some test temp-dir names contain payloads like $(whoami)).
	const sep = "\x1f"
	bashSafePath := "'" + strings.ReplaceAll(envPath, "'", `'\''`) + "'"
	script := fmt.Sprintf(
		`set -e
source %s
printf '%%s%s' "${%s-__UNSET__}"
printf '%%s%s' "${HIJACK-__UNSET__}"
`, bashSafePath, sep, varName, sep)

	cmd := exec.Command("bash", "-c", script)
	cmd.Env = []string{"PATH=" + os.Getenv("PATH")}
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()

	require.NoError(t, err,
		"bash source failed (likely a shell-injection escape): stderr=%q stdout=%q file=%q",
		stderr.String(), stdout.String(), envFileContent)
	require.Empty(t, stderr.String(),
		"bash produced stderr output (likely a partially-executed injection): %q file=%q",
		stderr.String(), envFileContent)

	parts := strings.Split(stdout.String(), sep)
	require.Len(t, parts, 3, "expected 3 sep-delimited parts, got %d: %q", len(parts), stdout.String())
	gotValue := parts[0]
	gotHijack := parts[1]

	require.Equal(t, wantValue, gotValue,
		"bash $%s did not match expected value\nfile=%q", varName, envFileContent)
	require.Equal(t, "__UNSET__", gotHijack,
		"injection set HIJACK=%q (expected unset); file=%q", gotHijack, envFileContent)
}

// fakeFile records writes for assertion. It also captures the open mode so
// tests can verify mode was atomic with creation rather than chmod-ed
// after the fact.
type fakeFile struct {
	path string
	mode os.FileMode
	flag int
	buf  []byte
	fs   *fakeFS
}

func (f *fakeFile) Write(p []byte) (int, error) {
	f.buf = append(f.buf, p...)
	return len(p), nil
}

func (f *fakeFile) Close() error {
	f.fs.mu.Lock()
	defer f.fs.mu.Unlock()
	// Append-mode writes accumulate.
	if f.flag&os.O_APPEND != 0 {
		existing := f.fs.contents[f.path]
		f.fs.contents[f.path] = append(existing, f.buf...)
	} else {
		f.fs.contents[f.path] = append([]byte(nil), f.buf...)
	}
	f.fs.modes[f.path] = f.mode
	return nil
}

// fakeFS captures all filesystem operations.
type fakeFS struct {
	mu       sync.Mutex
	contents map[string][]byte
	modes    map[string]os.FileMode
	dirs     map[string]os.FileMode
	removed  []string
}

func newFakeFS() *fakeFS {
	return &fakeFS{
		contents: map[string][]byte{},
		modes:    map[string]os.FileMode{},
		dirs:     map[string]os.FileMode{},
	}
}

func (f *fakeFS) RemoveAll(path string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for k := range f.contents {
		if strings.HasPrefix(k, path) {
			delete(f.contents, k)
			delete(f.modes, k)
		}
	}
	for k := range f.dirs {
		if strings.HasPrefix(k, path) {
			delete(f.dirs, k)
		}
	}
	f.removed = append(f.removed, path)
	return nil
}

func (f *fakeFS) MkdirAll(path string, perm os.FileMode) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.dirs[path] = perm
	return nil
}

func (f *fakeFS) Remove(path string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.contents[path]; !ok {
		return &fs.PathError{Op: "remove", Path: path, Err: fs.ErrNotExist}
	}
	delete(f.contents, path)
	delete(f.modes, path)
	return nil
}

func (f *fakeFS) OpenForCreate(path string, flag int, perm os.FileMode) (io.WriteCloser, error) {
	return &fakeFile{path: path, mode: perm, flag: flag, fs: f}, nil
}

// helpers ------------------------------------------------------------------

func newFixture(t *testing.T) (*Materializer, *fakeFS) {
	t.Helper()
	fs := newFakeFS()
	paths := Paths{
		Home:            "/home/sandbox",
		SecretsBaseDir:  "/sandbox-runtime/rt/secrets",
		SSHDir:          "/sandbox-runtime/rt/ssh",
		AgentConfigPath: "/sandbox-runtime/agent-config.json",
		SecretsEnvPath:  "/sandbox-runtime/secrets-env",
		GitCredsPath:    "/sandbox-runtime/rt/git-credentials",
	}
	return &Materializer{FS: fs, Paths: paths}, fs
}

// G2 regression suite ------------------------------------------------------

// TestG2_EnvSecretShellInjection_PlaintextWithSingleQuote is the headline
// regression for G2. Pre-fix, the bash line produced was:
//
//	export VAR='val'; whoami; '
//
// which executed `whoami` when sourced. The fix uses shellSingleQuote so
// the resulting line round-trips through `source`.
//
// CRITICAL: This test MUST shell out to bash to verify the file is safe to
// `source`. A pure Go round-trip test (FormatEnvLine → ParseEnvLine) is
// NOT sufficient: a buggy FormatEnvLine combined with a buggy ParseEnvLine
// can pass round-trip checks while still being shell-exploitable. The
// bash subprocess is the ground truth because that's the actual consumer.
func TestG2_EnvSecretShellInjection_PlaintextWithSingleQuote(t *testing.T) {
	requireBash(t)

	m, fs := newFixture(t)
	payload := `'; whoami; '`
	res, err := m.Materialize([]Secret{{
		Type:      "env-secret",
		Name:      "evil",
		Metadata:  map[string]string{"var_name": "MY_TOKEN"},
		Plaintext: payload,
	}})
	require.NoError(t, err)
	require.Len(t, res.Results, 1)
	require.Equal(t, OutcomeMaterialized, res.Results[0].Outcome,
		"materialization must succeed; injection is neutralized, not skipped")

	envFile := string(fs.contents["/sandbox-runtime/secrets-env"])
	requireBashSourceProducesValue(t, envFile, "MY_TOKEN", payload)
}

// TestG2_EnvSecretShellInjection_Corpus exhaustively covers shell-meaningful
// payloads. Each must be neutralized AND, when the produced env-file is
// sourced by bash, MY_TOKEN must equal the original payload exactly. We
// shell out to bash because the bash sourcer is the actual consumer; pure
// Go round-trip tests miss attacks that exploit shell parsing differences.
func TestG2_EnvSecretShellInjection_Corpus(t *testing.T) {
	requireBash(t)

	corpus := []struct {
		name    string
		payload string
	}{
		{"single-quote", `'`},
		{"single-quote-rce", `'; rm -rf /; '`},
		{"dollar-paren", `$(whoami)`},
		{"backtick", "`whoami`"},
		{"dollar-brace", `${PATH}`},
		{"newline-injection", "val\nexport HIJACK=1"},
		{"newline-and-semicolon", "val\n; touch /tmp/pwn"},
		{"escaped-quote-attempt", `\'; echo pwn; \'`},
		{"empty", ""},
		{"only-quotes", `''`},
		{"unicode", "💀\nval"},
		{"long-value", strings.Repeat("a", 4096)},
		{"crlf", "val\r\nHIJACK=1"},
		// NUL bytes can't appear in shell variables (POSIX); excluded.
	}
	for _, tc := range corpus {
		t.Run(tc.name, func(t *testing.T) {
			m, fs := newFixture(t)
			res, err := m.Materialize([]Secret{{
				Type:      "env-secret",
				Name:      "case",
				Metadata:  map[string]string{"var_name": "VAR"},
				Plaintext: tc.payload,
			}})
			require.NoError(t, err)
			require.Equal(t, OutcomeMaterialized, res.Results[0].Outcome,
				"payload %q must materialize cleanly", tc.payload)

			envFile := string(fs.contents["/sandbox-runtime/secrets-env"])
			requireBashSourceProducesValue(t, envFile, "VAR", tc.payload)
		})
	}
}

// TestG2_EnvSecret_InvalidVarName_Skipped ensures malformed var names are
// rejected with Skipped outcome rather than blindly written.
//
// Includes G37 dangerous-name blocklist entries (LD_PRELOAD, PATH,
// PYTHONPATH, etc.) so the defense-in-depth delegation from
// validateVarName to validation.ValidateEnvVarName is exercised at the
// materialize layer — without this the agentd side only proves the
// delegation wiring, not the actual blocklist enforcement.
func TestG2_EnvSecret_InvalidVarName_Skipped(t *testing.T) {
	cases := []string{
		"",
		"123FOO",                 // leading digit
		"FOO=BAR",                // embedded =
		"FOO BAR",                // space
		"FOO;HIJACK",             // semicolon
		"FOO\nBAR",               // newline
		"$(whoami)",              // shell substitution
		strings.Repeat("X", 257), // overlong
		// G37 dangerous-name blocklist — agentd must reject at
		// materialize time even if the API layer was bypassed.
		"LD_PRELOAD",
		"LD_LIBRARY_PATH",
		"PATH",
		"PYTHONPATH",
		"NODE_OPTIONS",
		"BASH_ENV",
		"DYLD_INSERT_LIBRARIES",
		"ld_preload", // case-insensitive
	}
	for _, name := range cases {
		t.Run(fmt.Sprintf("name=%q", name), func(t *testing.T) {
			m, fs := newFixture(t)
			res, err := m.Materialize([]Secret{{
				Type:      "env-secret",
				Name:      "bad",
				Metadata:  map[string]string{"var_name": name},
				Plaintext: "value",
			}})
			require.NoError(t, err, "invalid var_name must skip, not fail the batch")
			require.Equal(t, OutcomeSkipped, res.Results[0].Outcome)
			require.Empty(t, fs.contents["/sandbox-runtime/secrets-env"],
				"no env-file content must be written for skipped secret")
		})
	}
}

// TestG2_SSHKey_NameInjection ensures that NAME cannot escape the SSH dir.
// Pre-fix: NAME was concatenated into KEY_PATH unchecked, so a name like
// "../etc/cron.d/evil" would write into /etc/.
func TestG2_SSHKey_NameInjection(t *testing.T) {
	m, fs := newFixture(t)
	res, err := m.Materialize([]Secret{{
		Type:      "ssh-key",
		Name:      "../../etc/cron.d/evil",
		Metadata:  map[string]string{"key_type": "ed25519", "host": "github.com"},
		Plaintext: "key-bytes",
	}})
	require.NoError(t, err)
	require.Equal(t, OutcomeSkipped, res.Results[0].Outcome,
		"path-traversing names must be rejected")
	for path := range fs.contents {
		require.False(t, strings.Contains(path, "/etc/"),
			"no file should be written outside /home/sandbox/.ssh; got %q", path)
	}
}

// TestG2_SSHKey_HostInjection ensures crafted hostnames cannot inject SSH
// config directives. A host like "github.com\n    User root" would, before
// validation, append a User directive to ssh/config.
func TestG2_SSHKey_HostInjection(t *testing.T) {
	cases := []string{
		"github.com\n    User root",
		"github.com IdentityFile /etc/shadow",
		"github.com\tHostName attacker.example",
		"-oProxyCommand=evil",
	}
	for _, host := range cases {
		t.Run(host, func(t *testing.T) {
			m, fs := newFixture(t)
			res, err := m.Materialize([]Secret{{
				Type:      "ssh-key",
				Name:      "key",
				Metadata:  map[string]string{"key_type": "ed25519", "host": host},
				Plaintext: "key-bytes",
			}})
			require.NoError(t, err)
			require.Equal(t, OutcomeSkipped, res.Results[0].Outcome,
				"host %q must be rejected", host)
			require.Empty(t, fs.contents["/sandbox-runtime/rt/ssh/config"],
				"no ssh config must be written for rejected host")
		})
	}
}

// TestG2_SSHKey_KeyTypeAllowlist ensures key_type is restricted.
func TestG2_SSHKey_KeyTypeAllowlist(t *testing.T) {
	cases := []struct {
		keyType string
		want    Outcome
	}{
		{"ed25519", OutcomeMaterialized},
		{"rsa", OutcomeMaterialized},
		{"ecdsa", OutcomeMaterialized},
		{"dsa", OutcomeMaterialized},
		{"../foo", OutcomeSkipped},
		{"id;rm -rf /", OutcomeSkipped},
		{"", OutcomeMaterialized}, // empty -> default ed25519
	}
	for _, tc := range cases {
		t.Run(tc.keyType, func(t *testing.T) {
			m, _ := newFixture(t)
			res, err := m.Materialize([]Secret{{
				Type:      "ssh-key",
				Name:      "key",
				Metadata:  map[string]string{"key_type": tc.keyType, "host": "github.com"},
				Plaintext: "key-bytes",
			}})
			require.NoError(t, err)
			require.Equal(t, tc.want, res.Results[0].Outcome,
				"key_type %q outcome", tc.keyType)
		})
	}
}

// TestG2_GitCredential_TokenSanity ensures malformed tokens that would
// alter the URL authority are rejected.
func TestG2_GitCredential_TokenSanity(t *testing.T) {
	cases := []struct {
		token string
		want  Outcome
	}{
		{"ghp_abcdefghij1234567890", OutcomeMaterialized},
		{"normal-token_with.allowed~chars", OutcomeMaterialized},
		{"@injected.com", OutcomeSkipped},
		{"token@evil/path", OutcomeSkipped},
		{"token#fragment", OutcomeSkipped},
		{"token?param=1", OutcomeSkipped},
		{"token with space", OutcomeSkipped},
		{"", OutcomeSkipped},
	}
	for _, tc := range cases {
		t.Run(tc.token, func(t *testing.T) {
			m, _ := newFixture(t)
			res, err := m.Materialize([]Secret{{
				Type:      "git-credential",
				Name:      "cred",
				Metadata:  map[string]string{"host": "github.com", "protocol": "https"},
				Plaintext: tc.token,
			}})
			require.NoError(t, err)
			require.Equal(t, tc.want, res.Results[0].Outcome,
				"token %q outcome", tc.token)
		})
	}
}

// TestG2_GitCredential_ProtocolAllowlist ensures the URL scheme cannot be
// arbitrary (e.g. file://).
func TestG2_GitCredential_ProtocolAllowlist(t *testing.T) {
	cases := []struct {
		proto string
		want  Outcome
	}{
		{"https", OutcomeMaterialized},
		{"http", OutcomeMaterialized},
		{"file", OutcomeSkipped},
		{"ftp", OutcomeSkipped},
		{"javascript", OutcomeSkipped},
		{"", OutcomeMaterialized}, // empty -> default https
	}
	for _, tc := range cases {
		t.Run(tc.proto, func(t *testing.T) {
			m, _ := newFixture(t)
			res, err := m.Materialize([]Secret{{
				Type:      "git-credential",
				Name:      "cred",
				Metadata:  map[string]string{"host": "github.com", "protocol": tc.proto},
				Plaintext: "abc123",
			}})
			require.NoError(t, err)
			require.Equal(t, tc.want, res.Results[0].Outcome,
				"protocol %q outcome", tc.proto)
		})
	}
}

// TestG2_SecretFile_PathTraversal exhaustively tests path traversal attempts.
func TestG2_SecretFile_PathTraversal(t *testing.T) {
	cases := []struct {
		mountPath string
		want      Outcome
	}{
		{"foo.txt", OutcomeMaterialized},                                 // relative, under base
		{"sub/foo.txt", OutcomeMaterialized},                             // nested
		{"/sandbox-runtime/rt/secrets/foo.txt", OutcomeMaterialized},     // absolute under base
		{"../../etc/passwd", OutcomeSkipped},                             // dot-dot
		{"/etc/passwd", OutcomeSkipped},                                  // absolute outside base
		{"/sandbox-runtime/rt/secrets/../../etc/passwd", OutcomeSkipped}, // mixed
		{"foo/../../../etc/passwd", OutcomeSkipped},                      // relative dot-dot
		{"./..//etc/passwd", OutcomeSkipped},                             // normalised dot-dot
		{"", OutcomeSkipped},                                             // empty
	}
	for _, tc := range cases {
		t.Run(tc.mountPath, func(t *testing.T) {
			m, fs := newFixture(t)
			res, err := m.Materialize([]Secret{{
				Type:      "secret-file",
				Name:      "f",
				Metadata:  map[string]string{"mount_path": tc.mountPath},
				Plaintext: "data",
			}})
			require.NoError(t, err)
			require.Equal(t, tc.want, res.Results[0].Outcome,
				"mount_path %q outcome", tc.mountPath)
			for path := range fs.contents {
				require.True(t, strings.HasPrefix(path, "/sandbox-runtime/rt/secrets/") ||
					path == "/sandbox-runtime/agent-config.json",
					"no file outside secrets base; got %q", path)
			}
		})
	}
}

// G20 regression suite -----------------------------------------------------

// TestG20_AllFilesCreatedWithMode0600 ensures every credential file gets
// mode 0600 atomically with creation, not via chmod-after-write.
func TestG20_AllFilesCreatedWithMode0600(t *testing.T) {
	m, fs := newFixture(t)
	_, err := m.Materialize([]Secret{
		{Type: "api-key", Name: "p", Plaintext: `{"key":"val"}`},
		{Type: "ssh-key", Name: "k", Metadata: map[string]string{"key_type": "ed25519", "host": "github.com"}, Plaintext: "kbytes"},
		{Type: "git-credential", Name: "c", Metadata: map[string]string{"host": "github.com", "protocol": "https"}, Plaintext: "abc"},
		{Type: "secret-file", Name: "s", Metadata: map[string]string{"mount_path": "f.txt"}, Plaintext: "data"},
		{Type: "env-secret", Name: "e", Metadata: map[string]string{"var_name": "VAR"}, Plaintext: "v"},
	})
	require.NoError(t, err)

	for path, mode := range fs.modes {
		require.Equal(t, os.FileMode(0o600), mode,
			"file %q must be created with mode 0600 (G20)", path)
	}
}

// Round-trip and parse tests ------------------------------------------------

// TestFormatEnvLine_BashSourceRoundTrip verifies the produced line round-
// trips through bash `source` for every payload in the corpus. We test
// FormatEnvLine in isolation (not via Materialize) so the failure surface
// is small if the quoting changes.
func TestFormatEnvLine_BashSourceRoundTrip(t *testing.T) {
	requireBash(t)
	cases := []struct{ name, value string }{
		{"FOO", "bar"},
		{"FOO", `bar with spaces`},
		{"FOO", `'single-quoted'`},
		{"FOO", "newline\nvalue"},
		{"FOO", `$(whoami)`},
		{"FOO", `\backslash\`},
		{"FOO", ``},
		{"FOO", "tab\tvalue"},
	}
	for _, tc := range cases {
		t.Run(tc.name+"="+tc.value, func(t *testing.T) {
			line := FormatEnvLine(tc.name, tc.value)
			requireBashSourceProducesValue(t, line, tc.name, tc.value)
		})
	}
}

// Multiple secrets -----------------------------------------------------------

// TestMaterialize_MixedBatch_OneBadDoesNotBlockOthers covers the threat
// model invariant T5: an invalid secret only skips itself.
func TestMaterialize_MixedBatch_OneBadDoesNotBlockOthers(t *testing.T) {
	m, fs := newFixture(t)
	res, err := m.Materialize([]Secret{
		{Type: "env-secret", Name: "good", Metadata: map[string]string{"var_name": "GOOD"}, Plaintext: "1"},
		{Type: "env-secret", Name: "bad", Metadata: map[string]string{"var_name": "1BAD"}, Plaintext: "2"},
		{Type: "env-secret", Name: "good2", Metadata: map[string]string{"var_name": "GOOD2"}, Plaintext: "3"},
	})
	require.NoError(t, err, "skipped secrets must not produce a batch error")
	require.Equal(t, OutcomeMaterialized, res.Results[0].Outcome)
	require.Equal(t, OutcomeSkipped, res.Results[1].Outcome)
	require.Equal(t, OutcomeMaterialized, res.Results[2].Outcome)

	envFile := string(fs.contents["/sandbox-runtime/secrets-env"])
	require.Contains(t, envFile, "export GOOD=")
	require.Contains(t, envFile, "export GOOD2=")
	require.NotContains(t, envFile, "1BAD",
		"skipped secret must not appear in env file")
}

// TestMaterialize_EmptyInput is a smoke test.
func TestMaterialize_EmptyInput(t *testing.T) {
	m, _ := newFixture(t)
	res, err := m.Materialize(nil)
	require.NoError(t, err)
	require.Empty(t, res.Results)
}

// TestMaterialize_UnknownType skips with reason.
func TestMaterialize_UnknownType(t *testing.T) {
	m, _ := newFixture(t)
	res, err := m.Materialize([]Secret{{Type: "novel", Name: "x"}})
	require.NoError(t, err)
	require.Equal(t, OutcomeSkipped, res.Results[0].Outcome)
	require.Contains(t, res.Results[0].Reason, "unknown secret type")
}

// resolveMountPath direct tests -------------------------------------------

func TestResolveMountPath(t *testing.T) {
	base := "/sandbox-runtime/rt/secrets"
	cases := []struct {
		input string
		ok    bool
		want  string
	}{
		{"foo.txt", true, "/sandbox-runtime/rt/secrets/foo.txt"},
		{"sub/dir/file", true, "/sandbox-runtime/rt/secrets/sub/dir/file"},
		{"/sandbox-runtime/rt/secrets/abs", true, "/sandbox-runtime/rt/secrets/abs"},
		{"../../etc/passwd", false, ""},
		{"/etc/passwd", false, ""},
		{"", false, ""},
		{".", false, ""},
		{"./", false, ""},
	}
	for _, tc := range cases {
		t.Run(tc.input, func(t *testing.T) {
			got, err := resolveMountPath(base, tc.input)
			if !tc.ok {
				require.Error(t, err, "input %q must be rejected", tc.input)
				return
			}
			require.NoError(t, err)
			require.Equal(t, tc.want, got)
		})
	}
}

// LoadSecretsFile -----------------------------------------------------------

func TestLoadSecretsFile_RoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "secrets.json")
	require.NoError(t, os.WriteFile(path, []byte(`[{"type":"env-secret","name":"x","metadata":{"var_name":"X"},"plaintext":"v"}]`), 0o600))

	got, err := LoadSecretsFile(path)
	require.NoError(t, err)
	require.Len(t, got, 1)
	require.Equal(t, "env-secret", got[0].Type)
	require.Equal(t, "x", got[0].Name)
	require.Equal(t, "X", got[0].Metadata["var_name"])
	require.Equal(t, "v", got[0].Plaintext)
}

func TestLoadSecretsFile_MissingFile(t *testing.T) {
	_, err := LoadSecretsFile("/nonexistent/path")
	require.Error(t, err)
}

func TestLoadSecretsFile_BadJSON(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "bad.json")
	require.NoError(t, os.WriteFile(path, []byte(`not json`), 0o600))

	_, err := LoadSecretsFile(path)
	require.Error(t, err)
}

// Integration with real filesystem (just to confirm OpenForCreate semantics).

func TestRealFS_OpenForCreate_EnforcesMode(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "f")
	w, err := RealFS().OpenForCreate(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	require.NoError(t, err)
	_, _ = w.Write([]byte("x"))
	require.NoError(t, w.Close())

	st, err := os.Stat(path)
	require.NoError(t, err)
	// On unix, umask is applied to the open mode. Some test envs set umask 022;
	// in that case the resulting mode is 0o600 & ~0o022 == 0o600. Either way
	// world-readable bits must be off.
	require.Zero(t, st.Mode().Perm()&0o077,
		"file must not have group or other permission bits; got %v", st.Mode())
}

// LLM Provider tests --------------------------------------------------------

// TestLLMProvider_Valid materializes a well-formed llm-provider secret.
func TestLLMProvider_Valid(t *testing.T) {
	m, fs := newFixture(t)
	res, err := m.Materialize([]Secret{{
		Type:      "llm-provider",
		Name:      "anthropic",
		Plaintext: `{"kind":"anthropic","slug":"anthropic","apiKey":"sk-ant-123","default":"anthropic/claude-sonnet-4-5"}`,
	}})
	require.NoError(t, err)
	require.Len(t, res.Results, 1)
	require.Equal(t, OutcomeMaterialized, res.Results[0].Outcome)

	// Provider should be staged, not yet written to AgentConfigPath
	require.Empty(t, fs.contents["/sandbox-runtime/agent-config.json"],
		"provider must not be written until FlushProviders is called")
	require.Len(t, m.stagedProviders, 1)
	require.Equal(t, "anthropic", m.stagedProviders[0].Slug)
}

// TestLLMProvider_Valid_Minimal materializes a minimal llm-provider (just provider + apiKey).
func TestLLMProvider_Valid_Minimal(t *testing.T) {
	m, fs := newFixture(t)
	res, err := m.Materialize([]Secret{{
		Type:      "llm-provider",
		Name:      "openai",
		Plaintext: `{"kind":"openai","slug":"openai","apiKey":"sk-..."}`,
	}})
	require.NoError(t, err)
	require.Equal(t, OutcomeMaterialized, res.Results[0].Outcome)
	require.Len(t, m.stagedProviders, 1)
	require.Empty(t, fs.contents["/sandbox-runtime/agent-config.json"])
}

// TestLLMProvider_EmptyPlaintext is rejected.
func TestLLMProvider_EmptyPlaintext(t *testing.T) {
	m, _ := newFixture(t)
	res, err := m.Materialize([]Secret{{
		Type:      "llm-provider",
		Name:      "x",
		Plaintext: "",
	}})
	require.NoError(t, err)
	require.Equal(t, OutcomeSkipped, res.Results[0].Outcome)
	require.Contains(t, res.Results[0].Reason, "empty")
}

// TestLLMProvider_MissingProviderField is rejected.
func TestLLMProvider_MissingProviderField(t *testing.T) {
	m, _ := newFixture(t)
	res, err := m.Materialize([]Secret{{
		Type:      "llm-provider",
		Name:      "x",
		Plaintext: `{"apiKey":"sk-..."}`,
	}})
	require.NoError(t, err)
	require.Equal(t, OutcomeSkipped, res.Results[0].Outcome)
	require.Contains(t, res.Results[0].Reason, "provider")
}

// TestLLMProvider_MissingAPIKey is rejected.
func TestLLMProvider_MissingAPIKey(t *testing.T) {
	m, _ := newFixture(t)
	res, err := m.Materialize([]Secret{{
		Type:      "llm-provider",
		Name:      "x",
		Plaintext: `{"kind":"anthropic","slug":"anthropic"}`,
	}})
	require.NoError(t, err)
	require.Equal(t, OutcomeSkipped, res.Results[0].Outcome)
	require.Contains(t, res.Results[0].Reason, "apiKey")
}

// TestLLMProvider_InvalidJSON is rejected.
func TestLLMProvider_InvalidJSON(t *testing.T) {
	m, _ := newFixture(t)
	res, err := m.Materialize([]Secret{{
		Type:      "llm-provider",
		Name:      "x",
		Plaintext: "not json",
	}})
	require.NoError(t, err)
	require.Equal(t, OutcomeSkipped, res.Results[0].Outcome)
	require.Contains(t, res.Results[0].Reason, "invalid")
}

// TestLLMProvider_BadNameIsAllowed ensures llm-provider follows the same
// relaxed name policy as api-key (meaningful names are not required).
func TestLLMProvider_BadNameIsAllowed(t *testing.T) {
	m, _ := newFixture(t)
	res, err := m.Materialize([]Secret{{
		Type:      "llm-provider",
		Name:      "", // empty name would fail validateName
		Plaintext: `{"kind":"anthropic","slug":"anthropic","apiKey":"sk-..."}`,
	}})
	require.NoError(t, err)
	require.Equal(t, OutcomeMaterialized, res.Results[0].Outcome,
		"llm-provider with empty name should be allowed like api-key")
}

// TestLLMProvider_MultipleProviders_AllStaged verifies multiple llm-provider
// secrets are all collected in stagedProviders.
func TestLLMProvider_MultipleProviders_AllStaged(t *testing.T) {
	m, _ := newFixture(t)
	res, err := m.Materialize([]Secret{
		{Type: "llm-provider", Name: "anthropic", Plaintext: `{"kind":"anthropic","slug":"anthropic","apiKey":"sk-ant-123"}`},
		{Type: "llm-provider", Name: "openai", Plaintext: `{"kind":"openai","slug":"openai","apiKey":"sk-openai-456"}`},
	})
	require.NoError(t, err)
	require.Len(t, res.Results, 2)
	require.Equal(t, OutcomeMaterialized, res.Results[0].Outcome)
	require.Equal(t, OutcomeMaterialized, res.Results[1].Outcome)
	require.Len(t, m.stagedProviders, 2)
}

// TestLLMProvider_MixedWithOtherTypes verifies llm-provider secrets coexist
// with other secret types and don't interfere.
func TestLLMProvider_MixedWithOtherTypes(t *testing.T) {
	m, fs := newFixture(t)
	res, err := m.Materialize([]Secret{
		{Type: "llm-provider", Name: "p1", Plaintext: `{"kind":"anthropic","slug":"anthropic","apiKey":"sk-1"}`},
		{Type: "env-secret", Name: "e1", Metadata: map[string]string{"var_name": "VAR"}, Plaintext: "val"},
		{Type: "llm-provider", Name: "p2", Plaintext: `{"kind":"openai","slug":"openai","apiKey":"sk-2"}`},
	})
	require.NoError(t, err)
	require.Len(t, res.Results, 3)
	require.Equal(t, OutcomeMaterialized, res.Results[0].Outcome)
	require.Equal(t, OutcomeMaterialized, res.Results[1].Outcome)
	require.Equal(t, OutcomeMaterialized, res.Results[2].Outcome)
	require.Len(t, m.stagedProviders, 2)

	// Env file should still be written
	require.Contains(t, string(fs.contents["/sandbox-runtime/secrets-env"]), "export VAR=")
}

// TestLLMProvider_FlushProviders_CallsFormatter writes staged providers
// through the formatter and writes result to AgentConfigPath.
func TestLLMProvider_FlushProviders_CallsFormatter(t *testing.T) {
	m, fs := newFixture(t)
	_, err := m.Materialize([]Secret{{
		Type:      "llm-provider",
		Name:      "anthropic",
		Plaintext: `{"kind":"anthropic","slug":"anthropic","apiKey":"sk-ant-123","default":"anthropic/claude-sonnet-4-5"}`,
	}})
	require.NoError(t, err)
	require.Len(t, m.stagedProviders, 1)

	formatter := LLMProviderFormatter(func(providers []sec.LLMProviderData) ([]byte, error) {
		if len(providers) != 1 {
			t.Fatalf("expected 1 provider, got %d", len(providers))
		}
		if providers[0].Slug != "anthropic" {
			t.Errorf("expected provider 'anthropic', got %q", providers[0].Slug)
		}
		return []byte(`{"formatted":true}`), nil
	})

	require.NoError(t, m.FlushProviders(formatter))
	require.Equal(t, `{"formatted":true}`, string(fs.contents["/sandbox-runtime/agent-config.json"]))
}

// TestLLMProvider_FlushProviders_NoStagedProviders is a no-op.
func TestLLMProvider_FlushProviders_NoStagedProviders(t *testing.T) {
	m, fs := newFixture(t)
	_, err := m.Materialize([]Secret{
		{Type: "env-secret", Name: "e", Metadata: map[string]string{"var_name": "VAR"}, Plaintext: "v"},
	})
	require.NoError(t, err)
	require.Empty(t, m.stagedProviders)

	formatter := LLMProviderFormatter(func(providers []sec.LLMProviderData) ([]byte, error) {
		t.Fatal("formatter should not be called with no staged providers")
		return nil, nil
	})

	require.NoError(t, m.FlushProviders(formatter))
	require.Empty(t, fs.contents["/sandbox-runtime/agent-config.json"],
		"no agent config should be written when no providers staged")
}

// TestLLMProvider_FlushProviders_NilFormatter is a no-op.
func TestLLMProvider_FlushProviders_NilFormatter(t *testing.T) {
	m, fs := newFixture(t)
	_, err := m.Materialize([]Secret{{
		Type:      "llm-provider",
		Name:      "anthropic",
		Plaintext: `{"kind":"anthropic","slug":"anthropic","apiKey":"sk-..."}`,
	}})
	require.NoError(t, err)
	require.Len(t, m.stagedProviders, 1)

	require.NoError(t, m.FlushProviders(nil))
	require.Empty(t, fs.contents["/sandbox-runtime/agent-config.json"],
		"no agent config should be written when formatter is nil")
}

// TestLLMProvider_FlushProviders_FormatterError is propagated.
func TestLLMProvider_FlushProviders_FormatterError(t *testing.T) {
	m, _ := newFixture(t)
	_, err := m.Materialize([]Secret{{
		Type:      "llm-provider",
		Name:      "anthropic",
		Plaintext: `{"kind":"anthropic","slug":"anthropic","apiKey":"sk-..."}`,
	}})
	require.NoError(t, err)

	formatter := LLMProviderFormatter(func(providers []sec.LLMProviderData) ([]byte, error) {
		return nil, errors.New("simulated formatter error")
	})

	err = m.FlushProviders(formatter)
	require.Error(t, err)
	require.Contains(t, err.Error(), "simulated formatter error")
}

// TestG20_LLMProvider_Mode0600 ensures flushed provider config is written
// with mode 0600.
func TestG20_LLMProvider_Mode0600(t *testing.T) {
	m, fs := newFixture(t)
	_, err := m.Materialize([]Secret{{
		Type:      "llm-provider",
		Name:      "anthropic",
		Plaintext: `{"kind":"anthropic","slug":"anthropic","apiKey":"sk-..."}`,
	}})
	require.NoError(t, err)

	formatter := LLMProviderFormatter(func(providers []sec.LLMProviderData) ([]byte, error) {
		return []byte(`{"config":true}`), nil
	})
	require.NoError(t, m.FlushProviders(formatter))

	mode, ok := fs.modes["/sandbox-runtime/agent-config.json"]
	require.True(t, ok, "mode should be recorded for agent-config.json")
	require.Equal(t, os.FileMode(0o600), mode,
		"agent config must be written with mode 0600 (G20)")
}

// HasFailures / Counts ------------------------------------------------------

func TestMaterializeResult_Counts(t *testing.T) {
	r := &MaterializeResult{Results: []SecretResult{
		{Outcome: OutcomeMaterialized},
		{Outcome: OutcomeSkipped},
		{Outcome: OutcomeSkipped},
		{Outcome: OutcomeFailed},
	}}
	m, s, f := r.Counts()
	require.Equal(t, 1, m)
	require.Equal(t, 2, s)
	require.Equal(t, 1, f)
	require.True(t, r.HasFailures())
}

// ErrPartialFailure -------------------------------------------------------

func TestMaterialize_PartialFailure_ReturnsSentinel(t *testing.T) {
	failing := &errFS{newFakeFS()}
	m := &Materializer{
		FS: failing,
		Paths: Paths{
			Home:            "/home/sandbox",
			SecretsBaseDir:  "/sandbox-runtime/rt/secrets",
			SSHDir:          "/sandbox-runtime/rt/ssh",
			AgentConfigPath: "/sandbox-runtime/agent-config.json",
			SecretsEnvPath:  "/sandbox-runtime/secrets-env",
			GitCredsPath:    "/sandbox-runtime/rt/git-credentials",
		},
	}
	_, err := m.Materialize([]Secret{
		{Type: "env-secret", Name: "x", Metadata: map[string]string{"var_name": "X"}, Plaintext: "v"},
	})
	require.ErrorIs(t, err, ErrPartialFailure)
}

// errFS wraps fakeFS but fails OpenForCreate.
type errFS struct{ *fakeFS }

func (e *errFS) OpenForCreate(string, int, os.FileMode) (io.WriteCloser, error) {
	return nil, errors.New("simulated open failure")
}

// TestStagedProviders_Accessor verifies the public StagedProviders() method
// returns the same data that the internal stagedProviders field holds.
func TestStagedProviders_Accessor(t *testing.T) {
	m, _ := newFixture(t)

	// Before materialize — nil
	require.Nil(t, m.StagedProviders())

	_, err := m.Materialize([]Secret{
		{Type: "llm-provider", Name: "anthropic", Plaintext: `{"kind":"anthropic","slug":"anthropic","apiKey":"sk-ant-123","baseURL":"https://custom.endpoint"}`},
		{Type: "llm-provider", Name: "openai", Plaintext: `{"kind":"openai","slug":"openai","apiKey":"sk-oai-456"}`},
	})
	require.NoError(t, err)

	staged := m.StagedProviders()
	require.Len(t, staged, 2)
	require.Equal(t, "anthropic", staged[0].Slug)
	require.Equal(t, "sk-ant-123", staged[0].APIKey)
	require.Equal(t, "https://custom.endpoint", staged[0].BaseURL)
	require.Equal(t, "openai", staged[1].Slug)
	require.Equal(t, "sk-oai-456", staged[1].APIKey)
}

// TestStagedProviders_EmptyAfterNoLLMProviders verifies StagedProviders
// returns nil when batch has no llm-provider secrets.
func TestStagedProviders_EmptyAfterNoLLMProviders(t *testing.T) {
	m, _ := newFixture(t)
	_, err := m.Materialize([]Secret{
		{Type: "env-secret", Name: "x", Metadata: map[string]string{"var_name": "X"}, Plaintext: "v"},
	})
	require.NoError(t, err)
	require.Nil(t, m.StagedProviders())
}
