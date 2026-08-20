// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

// Package secrets materializes user-supplied secrets onto the sandbox pod
// filesystem with strict validation and TOCTOU-safe permissions.
//
// This package is the single source of truth for secret materialization.
// Both code paths use it:
//
//   - Boot-time: `workspace-agentd materialize --from /sandbox-cfg/secrets.json`
//     invoked from the runtime entrypoint script before opencode starts.
//   - Reload: the agentd HTTP handler `/v1/reload-secrets` calls Materialize
//     directly with the request body.
//
// Before this package existed, materialization was duplicated across a bash
// script (entrypoint-common.sh) and an inline Go function inside
// cmd/workspace-agentd/main.go, both of which suffered from Epic 17 G2:
// shell-quoted interpolation that broke on a single quote in the value.
// They also lacked input validation, used non-atomic chmod-after-write
// (G20), and used naive string-contains checks for path traversal.
//
// The Materialize function:
//
//   - Validates every field of every Secret against an allowlist (var names,
//     key types, hostnames, protocols, mount-path scopes).
//   - Writes files using os.OpenFile with O_CREATE|O_EXCL and mode 0600 so
//     permissions are atomic with creation — no window where the file is
//     readable with default umask.
//   - Encodes the env-file value using shellquote.Bash (single-quoted with
//     embedded single quotes escaped) so a malicious PLAINTEXT cannot
//     break out of the bash `source` consumer at entrypoint-opencode.sh
//     and at agentd buildEnv().
//   - Resolves mount paths via filepath.Clean + strict prefix containment
//     against the secrets base directory.
//   - Returns a typed *MaterializeResult that carries per-secret outcomes
//     (Materialized / Skipped / Failed) along with a redacted reason. The
//     caller decides whether to surface this to the operator (via pod
//     status) or to logs.
//
// Threat-model invariants this package enforces:
//
//	T1 No interpretation of secret values by the shell.
//	T2 No file ever exists on disk with mode > 0600 for credential material.
//	   (Design-0051 exception, agent-config.json ONLY: 0640 — the file is
//	   written by the uid-2000 sidecar and read by uid-1000 opencode, so
//	   the pod's shared gid 1000 must carry the read bit. Design 0051 §D1
//	   rules this file uid-1000-readable BY NECESSITY (opencode is its
//	   reader; the embedded MCP Basic header is a documented residual) —
//	   0640 grants exactly that reader set, nothing wider.)
//	T3 No path written outside SecretsBasePath, $HOME/.ssh, or AgentConfigPath.
//	T4 No env-file line that does not round-trip cleanly through `source`.
//	T5 An invalid secret skips that secret only; the rest still materialize.
//
// See `secrets_test.go` for the regression corpus, including:
//   - Single-quote, dollar-sign, backtick, newline injection in PLAINTEXT.
//   - Path traversal via "..", URL-encoded "..", absolute paths, symlinks.
//   - Hostname injection via "  IdentityFile /etc/shadow".
//   - Var-name injection via reserved bash names and embedded "=" / ";".
package secrets

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/lenaxia/llmsafespaces/pkg/agentd"
	sec "github.com/lenaxia/llmsafespaces/pkg/secrets"
	"github.com/lenaxia/llmsafespaces/pkg/validation"
)

// Secret is the materialization-time representation of a credential.
// Metadata is intentionally kept as a typed map to avoid leaking arbitrary
// JSON shape into the materializer; unknown keys are ignored.
//
// WIRE COMPATIBILITY (2026-08-15 incident): the injection pipeline writes
// mcp-server metadata per MATERIALIZE-CONTRACT.md with native JSON types
// ("args": [...], "timeoutMs": 5000), while this struct historically
// required map[string]string — one bound MCP server crash-looped every
// workspace boot ("cannot unmarshal array into ... Secret.metadata").
// UnmarshalJSON accepts BOTH shapes: string values pass through;
// numbers/booleans/arrays/objects are carried JSON-encoded as strings
// (exactly what the mcp staging branch's json.Unmarshal(argsStr) expects).
type Secret struct {
	Type      string            `json:"type"`
	Name      string            `json:"name"`
	Metadata  map[string]string `json:"metadata"`
	Plaintext string            `json:"plaintext"`

	// MetadataInvalid records why the metadata could not be normalized
	// (metadata that is not a JSON object). Materialize reports it
	// per-entry as a Skipped result instead of aborting the whole batch.
	//
	// Wire persistence: round-tripped through the reserved
	// "metadata_invalid" JSON key (MarshalJSON emits it; UnmarshalJSON
	// reads it) so the reload-secrets cache replays the skip verdict
	// identically — without it the cached entry marshals with
	// "metadata":null, the flag is lost, and a restart materializes the
	// rejected secret with defaults (T5 violation; review N1 on #871).
	MetadataInvalid string `json:"-"`
}

// MarshalJSON emits the struct fields plus the reserved "metadata_invalid"
// verdict key when set, so cache round-trips preserve the skip decision.
// The key is namespaced ("metadata_invalid") so it cannot collide with a
// legitimate metadata member key; UnmarshalJSON reads the same tag.
func (s Secret) MarshalJSON() ([]byte, error) {
	type wireSecret struct {
		Type      string            `json:"type"`
		Name      string            `json:"name"`
		Metadata  map[string]string `json:"metadata"`
		Plaintext string            `json:"plaintext"`
	}
	w := wireSecret{Type: s.Type, Name: s.Name, Metadata: s.Metadata, Plaintext: s.Plaintext}
	if s.MetadataInvalid != "" {
		return json.Marshal(struct {
			wireSecret
			MetadataInvalid string `json:"metadata_invalid"`
		}{w, s.MetadataInvalid})
	}
	return json.Marshal(w)
}

// UnmarshalJSON implements the dual-shape metadata contract documented on
// Secret. It never fails on metadata content — malformed metadata sets
// MetadataInvalid so the entry can be skipped downstream with an accurate
// reason rather than killing the file parse for every other secret. The
// reserved metadata_invalid key (emitted by MarshalJSON) restores a
// persisted verdict so a replayed cache entry skips identically to its
// first-boot parse.
func (s *Secret) UnmarshalJSON(data []byte) error {
	type wireSecret struct {
		Type      string          `json:"type"`
		Name      string          `json:"name"`
		Metadata  json.RawMessage `json:"metadata"`
		Plaintext string          `json:"plaintext"`
	}
	var w wireSecret
	if err := json.Unmarshal(data, &w); err != nil {
		return err
	}
	s.Type = w.Type
	s.Name = w.Name
	s.Plaintext = w.Plaintext

	// Persisted verdict (reserved wire key): restore before metadata
	// analysis so replay skips identically.
	var verdict struct {
		MetadataInvalid string `json:"metadata_invalid"`
	}
	_ = json.Unmarshal(data, &verdict)
	s.MetadataInvalid = verdict.MetadataInvalid

	if len(w.Metadata) == 0 {
		return nil
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(w.Metadata, &raw); err != nil {
		s.MetadataInvalid = "metadata is not a JSON object: " + err.Error()
		return nil
	}
	s.Metadata = make(map[string]string, len(raw))
	for k, v := range raw {
		var str string
		if err := json.Unmarshal(v, &str); err == nil {
			s.Metadata[k] = str
			continue
		}
		// Non-string member: carry the raw JSON verbatim.
		s.Metadata[k] = string(v)
	}
	return nil
}

// Outcome describes what happened to a single secret.
type Outcome string

const (
	OutcomeMaterialized Outcome = "materialized"
	OutcomeSkipped      Outcome = "skipped"
	OutcomeFailed       Outcome = "failed"
)

// SecretResult is the per-secret outcome reported by Materialize.
// Reason is human-readable but MUST NOT include the secret's plaintext.
type SecretResult struct {
	Type    string  `json:"type"`
	Name    string  `json:"name"`
	Outcome Outcome `json:"outcome"`
	Reason  string  `json:"reason,omitempty"`
}

// MaterializeResult aggregates outcomes for a Materialize call.
// The aggregate Error is nil when every secret was Materialized; otherwise
// it is a sentinel that callers can wrap. Per-secret reasons live on
// Results so callers can render structured status.
type MaterializeResult struct {
	Results []SecretResult `json:"results"`
}

// Counts returns (materialized, skipped, failed).
func (r *MaterializeResult) Counts() (int, int, int) {
	var m, s, f int
	for _, x := range r.Results {
		switch x.Outcome {
		case OutcomeMaterialized:
			m++
		case OutcomeSkipped:
			s++
		case OutcomeFailed:
			f++
		}
	}
	return m, s, f
}

// HasFailures returns true if any secret produced an OutcomeFailed.
// OutcomeSkipped does not count: skipping a malformed secret is a
// successful security decision, not a failure.
func (r *MaterializeResult) HasFailures() bool {
	for _, x := range r.Results {
		if x.Outcome == OutcomeFailed {
			return true
		}
	}
	return false
}

// ErrPartialFailure is returned by Materialize when at least one secret
// reached OutcomeFailed. Callers should still consider partially-applied
// state — files for already-materialized secrets remain on disk.
//
// This sentinel stays a plain error (not *apierrors.APIError) because it
// lives in pkg/ which cannot import api/internal/ (Go internal-package
// visibility). It is consumed only by cmd/workspace-agentd (the agent
// daemon), never by an HTTP handler, so HTTP status mapping is not needed.
var ErrPartialFailure = errors.New("secret materialization had partial failures")

// Filesystem is the minimal interface Materialize needs. Tests inject a
// fake; production uses RealFS which delegates to os.*.
type Filesystem interface {
	RemoveAll(path string) error
	MkdirAll(path string, perm os.FileMode) error
	OpenForCreate(path string, flag int, perm os.FileMode) (io.WriteCloser, error)
	Remove(path string) error
}

// realFS is the os-backed filesystem.
type realFS struct{}

// RealFS returns the production Filesystem.
func RealFS() Filesystem { return realFS{} }

func (realFS) RemoveAll(path string) error                  { return os.RemoveAll(path) }
func (realFS) MkdirAll(path string, perm os.FileMode) error { return os.MkdirAll(path, perm) }
func (realFS) Remove(path string) error                     { return os.Remove(path) }
func (realFS) OpenForCreate(path string, flag int, perm os.FileMode) (io.WriteCloser, error) {
	return os.OpenFile(path, flag, perm)
}

// Paths configures filesystem destinations. Defaults match agentd
// constants; tests override.
type Paths struct {
	Home            string // user home (e.g. /home/sandbox)
	SecretsBaseDir  string // secret-file root (/sandbox-runtime/rt/secrets)
	SSHDir          string // SSH config directory (/sandbox-runtime/rt/ssh)
	AgentConfigPath string // opencode config (/sandbox-runtime/agent-config.json)
	SecretsEnvPath  string // env-file (/sandbox-runtime/secrets-env)
	GitCredsPath    string // git-credentials file (/sandbox-runtime/rt/git-credentials)
}

// DefaultPaths returns production paths derived from the agentd package
// constants and the given home dir.
//
// US-35.7: SSH/git/secrets paths point to /sandbox-runtime/rt/* (tmpfs) to
// match loadMaterializeConfig() in cmd/workspace-agentd. The $HOME-relative
// PVC paths are symlinks (created by init container) pointing here.
func DefaultPaths(home string) Paths {
	if home == "" {
		home = "/home/sandbox"
	}
	return Paths{
		Home:            home,
		SecretsBaseDir:  agentd.SecretsBasePath,
		SSHDir:          "/sandbox-runtime/rt/ssh",
		AgentConfigPath: agentd.AgentConfigPath,
		SecretsEnvPath:  agentd.SecretsEnvPath,
		GitCredsPath:    "/sandbox-runtime/rt/git-credentials",
	}
}

// Validators ----------------------------------------------------------------

// hostnameRE matches a permissive but safe hostname. RFC 1123 plus a length
// cap. We do NOT accept IP literals here — operators who need IP-based hosts
// can configure them via DNS.
var hostnameRE = regexp.MustCompile(`^[A-Za-z0-9]([A-Za-z0-9-]{0,62}[A-Za-z0-9])?(\.[A-Za-z0-9]([A-Za-z0-9-]{0,62}[A-Za-z0-9])?)*$`)

var allowedKeyTypes = map[string]struct{}{
	"rsa":     {},
	"ed25519": {},
	"ecdsa":   {},
	"dsa":     {},
}

var allowedProtocols = map[string]struct{}{
	"https": {},
	"http":  {},
}

func validateVarName(s string) error {
	// G37: delegate to the shared validator so the API layer and the
	// in-pod materializer enforce identical rules — POSIX regex, length
	// cap, AND the dangerous-names blocklist (LD_PRELOAD, PATH,
	// PYTHONPATH, etc.). The API handler rejects these up front; this is
	// defense-in-depth for any path that bypasses the API (direct DB
	// write, future bug).
	return validation.ValidateEnvVarName(s)
}

func validateName(s string) error {
	return validation.ValidateSecretName(s)
}

// sanitizeEnvSuffix converts a secret name into a valid env var suffix by
// replacing non-alphanumeric characters with underscores and uppercasing.
func sanitizeEnvSuffix(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range strings.ToUpper(s) {
		if (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
		} else {
			b.WriteByte('_')
		}
	}
	return b.String()
}

func validateHostname(s string) error {
	if len(s) == 0 || len(s) > 253 {
		return fmt.Errorf("hostname length %d out of range", len(s))
	}
	if !hostnameRE.MatchString(s) {
		return fmt.Errorf("hostname %q is not a valid RFC 1123 name", s)
	}
	return nil
}

func validateKeyType(s string) error {
	if _, ok := allowedKeyTypes[s]; !ok {
		return fmt.Errorf("key_type %q not in {rsa,ed25519,ecdsa,dsa}", s)
	}
	return nil
}

func validateProtocol(s string) error {
	if _, ok := allowedProtocols[s]; !ok {
		return fmt.Errorf("protocol %q not in {http,https}", s)
	}
	return nil
}

// resolveMountPath resolves a user-supplied mount_path against the secrets
// base directory using filepath.Clean and a strict prefix check.
//
// Behavior:
//   - If mountPath is absolute and already under base, accept as-is.
//   - If mountPath is absolute but outside base, reject.
//   - If mountPath is relative, join under base.
//   - After Clean, the resolved path must still be under base.
//
// This protects against ".." traversal, symlink-style absolute path
// injection, and historical naive prefix-strip bugs in the bash version.
func resolveMountPath(base, mountPath string) (string, error) {
	if mountPath == "" {
		return "", errors.New("mount_path is empty")
	}
	cleanBase := filepath.Clean(base)
	var candidate string
	if filepath.IsAbs(mountPath) {
		candidate = filepath.Clean(mountPath)
	} else {
		candidate = filepath.Clean(filepath.Join(cleanBase, mountPath))
	}
	rel, err := filepath.Rel(cleanBase, candidate)
	if err != nil {
		return "", fmt.Errorf("resolving mount_path: %w", err)
	}
	if rel == "." || strings.HasPrefix(rel, "..") {
		return "", fmt.Errorf("mount_path %q escapes secrets base directory", mountPath)
	}
	return candidate, nil
}

// shellSingleQuote quotes a string for safe inclusion in a single-quoted
// bash literal. The transform is:
//
//	'   →  '\''
//	foo →  'foo'
//
// This is the exact algorithm used by bash's `printf %q` for strings
// containing only printable characters; for our purpose (round-trip through
// `source` consuming the file) this is sufficient and explicit.
//
// The function does NOT pre-validate the input. Callers MUST have already
// validated the variable name; the value side accepts any byte sequence.
func shellSingleQuote(value string) string {
	var b strings.Builder
	b.Grow(len(value) + 2)
	b.WriteByte('\'')
	for i := 0; i < len(value); i++ {
		if value[i] == '\'' {
			b.WriteString(`'\''`)
		} else {
			b.WriteByte(value[i])
		}
	}
	b.WriteByte('\'')
	return b.String()
}

// FormatEnvLine produces an `export VAR='value'` line suitable for
// `source` by bash. The single-quote escaping (`'\”`) is the canonical
// safe form for embedding arbitrary text in a single-quoted shell string.
//
// Consumers MUST read the resulting file via `bash source` (or an
// equivalent that implements bash's quoting rules), NOT via line-based
// regex parsing — values may contain literal newlines inside the quoted
// region, and a naive split-on-newline parser will mangle them.
//
// In this codebase, the consumer is buildEnvFrom() in cmd/workspace-agentd
// which delegates to a bash subprocess so the source-of-truth parser is
// bash itself.
func FormatEnvLine(varName, value string) string {
	return "export " + varName + "=" + shellSingleQuote(value) + "\n"
}

// LLMProviderFormatter is a callback that renders staged LLM provider
// data into the agent-specific config format. Each agent type (opencode,
// Claude Code, Codex) provides its own implementation.
type LLMProviderFormatter func(providers []sec.LLMProviderData) ([]byte, error)

// Materializer holds dependencies for materialization. Construct with
// NewMaterializer or pass a Materializer{} with field defaults filled in
// by the caller.
type Materializer struct {
	FS               Filesystem
	Paths            Paths
	stagedProviders  []sec.LLMProviderData
	stagedMCPServers []StagedMCPServer
}

// NewMaterializer returns a Materializer using the production filesystem
// and paths derived from $HOME.
func NewMaterializer() *Materializer {
	return &Materializer{
		FS:    RealFS(),
		Paths: DefaultPaths(os.Getenv("HOME")),
	}
}

// Materialize processes secrets and returns per-secret outcomes.
// The function performs a full reset of the secrets base directory, SSH
// directory, env file, agent config, and git credentials before applying
// the new set, matching the existing reload semantics.
func (m *Materializer) Materialize(secrets []Secret) (*MaterializeResult, error) {
	if m.FS == nil {
		m.FS = RealFS()
	}
	if m.Paths.Home == "" {
		m.Paths = DefaultPaths(os.Getenv("HOME"))
	}

	// Full replace of the materialized state.
	if err := m.reset(); err != nil {
		return nil, fmt.Errorf("reset: %w", err)
	}

	result := &MaterializeResult{Results: make([]SecretResult, 0, len(secrets))}
	for _, s := range secrets {
		result.Results = append(result.Results, m.applyOne(s))
	}
	if result.HasFailures() {
		return result, ErrPartialFailure
	}
	return result, nil
}

func (m *Materializer) reset() error {
	m.stagedProviders = nil
	m.stagedMCPServers = nil

	if err := m.FS.RemoveAll(m.Paths.SecretsBaseDir); err != nil && !os.IsNotExist(err) {
		return err
	}
	if err := m.FS.MkdirAll(m.Paths.SecretsBaseDir, 0o700); err != nil {
		return err
	}
	if err := m.FS.RemoveAll(m.Paths.SSHDir); err != nil && !os.IsNotExist(err) {
		return err
	}
	if err := m.FS.MkdirAll(m.Paths.SSHDir, 0o700); err != nil {
		return err
	}
	// These three are best-effort; absence is fine.
	_ = m.FS.Remove(m.Paths.GitCredsPath)
	_ = m.FS.Remove(m.Paths.AgentConfigPath)
	_ = m.FS.Remove(m.Paths.SecretsEnvPath)
	return nil
}

// applyOne dispatches by Type and returns a SecretResult. Errors are
// captured into Reason; the function never returns an error.
func (m *Materializer) applyOne(s Secret) SecretResult {
	r := SecretResult{Type: s.Type, Name: s.Name}

	if s.MetadataInvalid != "" {
		// Malformed input maps to Skipped — pod boot is not blocked by a
		// single malformed secret (invariant T5; same doctrine as
		// validateName below). The Reason carries the parse failure for
		// the reload status and result log; OutcomeFailed is reserved
		// for materializer-side errors the platform owns. Consequently
		// the reload handler surfaces this entry as a per-entry skipped
		// reason (200), not a 500 — a deliberate choice: malformed
		// input is the client's, boot tolerance is the platform's.
		r.Outcome = OutcomeSkipped
		r.Reason = s.MetadataInvalid
		return r
	}

	if err := validateName(s.Name); err != nil {
		// api-key and llm-provider do not require a meaningful name.
		if s.Type != "api-key" && s.Type != "llm-provider" {
			r.Outcome = OutcomeSkipped
			r.Reason = err.Error()
			return r
		}
	}

	var err error
	switch s.Type {
	case "api-key":
		err = m.applyAPIKey(s)
	case "llm-provider":
		err = m.applyLLMProvider(s)
	case "ssh-key":
		err = m.applySSHKey(s)
	case "git-credential":
		err = m.applyGitCredential(s)
	case "secret-file":
		err = m.applySecretFile(s)
	case "env-secret":
		err = m.applyEnvSecret(s)
	case "mcp-server":
		err = m.applyMCPServer(s)
	default:
		r.Outcome = OutcomeSkipped
		r.Reason = fmt.Sprintf("unknown secret type %q", s.Type)
		return r
	}

	if err != nil {
		// Distinguish validation failures (Skipped) from filesystem failures
		// (Failed). A validation error means the input was malformed; a
		// filesystem error means the system couldn't apply a valid input.
		var ve *validationError
		if errors.As(err, &ve) {
			r.Outcome = OutcomeSkipped
			r.Reason = ve.Error()
			return r
		}
		r.Outcome = OutcomeFailed
		r.Reason = err.Error()
		return r
	}
	r.Outcome = OutcomeMaterialized
	return r
}

// validationError marks errors that come from input validation rather than
// I/O. Materialize maps these to Skipped instead of Failed so the pod boot
// is not blocked by a single malformed secret.
type validationError struct{ msg string }

func (e *validationError) Error() string { return e.msg }
func newValidationError(format string, args ...interface{}) error {
	return &validationError{msg: fmt.Sprintf(format, args...)}
}

// --- per-type appliers ----------------------------------------------------

func (m *Materializer) applyAPIKey(s Secret) error {
	// api-key secrets are written as environment variables, not to AgentConfigPath.
	// AgentConfigPath is exclusively managed by FlushProviders (llm-provider type).
	if s.Plaintext == "" {
		return newValidationError("api-key plaintext is empty")
	}
	varName := "API_KEY"
	if s.Name != "" {
		varName = "API_KEY_" + sanitizeEnvSuffix(s.Name)
	}
	line := FormatEnvLine(varName, s.Plaintext)
	return appendFile(m.FS, m.Paths.SecretsEnvPath, []byte(line), 0o600)
}

func (m *Materializer) applySSHKey(s Secret) error {
	keyType := s.Metadata["key_type"]
	if keyType == "" {
		keyType = "ed25519"
	}
	if err := validateKeyType(keyType); err != nil {
		return newValidationError("%s", err.Error())
	}
	host := s.Metadata["host"]
	if host == "" {
		host = "github.com"
	}
	if err := validateHostname(host); err != nil {
		return newValidationError("%s", err.Error())
	}
	if s.Plaintext == "" {
		return newValidationError("ssh-key plaintext is empty")
	}

	keyPath := filepath.Join(m.Paths.SSHDir, "id_"+keyType+"_"+s.Name)
	if err := atomicWrite(m.FS, keyPath, []byte(s.Plaintext), 0o600); err != nil {
		return err
	}

	// Append a config block. host and keyPath are validated; nothing the
	// caller controls can introduce a newline that would inject another
	// directive.
	configPath := filepath.Join(m.Paths.SSHDir, "config")
	block := "Host " + host + "\n    IdentityFile " + keyPath + "\n    StrictHostKeyChecking accept-new\n"
	return appendFile(m.FS, configPath, []byte(block), 0o600)
}

func (m *Materializer) applyGitCredential(s Secret) error {
	host := s.Metadata["host"]
	if host == "" {
		host = "github.com"
	}
	if err := validateHostname(host); err != nil {
		return newValidationError("%s", err.Error())
	}
	protocol := s.Metadata["protocol"]
	if protocol == "" {
		protocol = "https"
	}
	if err := validateProtocol(protocol); err != nil {
		return newValidationError("%s", err.Error())
	}
	if s.Plaintext == "" {
		return newValidationError("git-credential token is empty")
	}
	// Reject tokens that contain URL-reserved characters that would alter
	// the resulting URL's authority. Most OAuth tokens are alphanumeric +
	// underscore/dash; this is a conservative allowlist.
	for i := 0; i < len(s.Plaintext); i++ {
		c := s.Plaintext[i]
		switch {
		case c >= 'A' && c <= 'Z',
			c >= 'a' && c <= 'z',
			c >= '0' && c <= '9',
			c == '-', c == '_', c == '.', c == '~':
			continue
		default:
			return newValidationError("git-credential token contains a non-URL-safe character at offset %d", i)
		}
	}
	line := protocol + "://oauth2:" + s.Plaintext + "@" + host + "\n"
	return appendFile(m.FS, m.Paths.GitCredsPath, []byte(line), 0o600)
}

func (m *Materializer) applySecretFile(s Secret) error {
	mountPath := s.Metadata["mount_path"]
	resolved, err := resolveMountPath(m.Paths.SecretsBaseDir, mountPath)
	if err != nil {
		return newValidationError("%s", err.Error())
	}
	if err := m.FS.MkdirAll(filepath.Dir(resolved), 0o700); err != nil {
		return err
	}
	return atomicWrite(m.FS, resolved, []byte(s.Plaintext), 0o600)
}

func (m *Materializer) applyEnvSecret(s Secret) error {
	varName := s.Metadata["var_name"]
	if err := validateVarName(varName); err != nil {
		return newValidationError("%s", err.Error())
	}
	line := FormatEnvLine(varName, s.Plaintext)
	return appendFile(m.FS, m.Paths.SecretsEnvPath, []byte(line), 0o600)
}

func (m *Materializer) applyLLMProvider(s Secret) error {
	if s.Plaintext == "" {
		return newValidationError("llm-provider plaintext is empty")
	}
	var data sec.LLMProviderData
	if err := json.Unmarshal([]byte(s.Plaintext), &data); err != nil {
		return newValidationError("llm-provider plaintext: %v", err)
	}
	if err := data.Validate(); err != nil {
		return newValidationError("%s", err.Error())
	}
	m.stagedProviders = append(m.stagedProviders, data)
	return nil
}

// applyMCPServer stages an MCP server config for later rendering into the
// opencode mcp section. The plaintext carries the JSON-encoded env+headers
// payload (MCPServerSecretPayload); the metadata carries transport/url/
// command/args. The staged entry is consumed by FormatMCPServers →
// AgentConfigWriter.SetMCPServers.
func (m *Materializer) applyMCPServer(s Secret) error {
	transport := s.Metadata["transport"]
	if transport == "" {
		return newValidationError("mcp-server metadata missing transport")
	}

	var payload struct {
		Env     map[string]string `json:"env"`
		Headers map[string]string `json:"headers"`
	}
	if s.Plaintext != "" {
		if err := json.Unmarshal([]byte(s.Plaintext), &payload); err != nil {
			return newValidationError("mcp-server plaintext: %v", err)
		}
	}

	var args []string
	if argsStr := s.Metadata["args"]; argsStr != "" {
		_ = json.Unmarshal([]byte(argsStr), &args) //nolint:errcheck
	}
	timeoutMs := 0
	if ts := s.Metadata["timeoutMs"]; ts != "" {
		if n, err := strconv.Atoi(ts); err == nil {
			timeoutMs = n
		}
	}

	staged := StagedMCPServer{
		Name:      s.Name,
		Transport: transport,
		URL:       s.Metadata["url"],
		Command:   s.Metadata["command"],
		Args:      args,
		TimeoutMs: timeoutMs,
		Env:       payload.Env,
		Headers:   payload.Headers,
	}
	if staged.Args == nil {
		staged.Args = []string{}
	}
	m.stagedMCPServers = append(m.stagedMCPServers, staged)
	return nil
}

// StagedProviders returns the LLM provider data accumulated during
// Materialize. Returns nil if no llm-provider secrets were in the batch.
// This allows callers to use the structured data for direct API injection
// (e.g., PUT /auth/:providerID) instead of or in addition to file-based
// config rendering via FlushProviders.
func (m *Materializer) StagedProviders() []sec.LLMProviderData {
	return m.stagedProviders
}

// StagedMCPServer is one materialized MCP server awaiting config rendering.
type StagedMCPServer struct {
	Name      string
	Transport string
	URL       string
	Command   string
	Args      []string
	TimeoutMs int
	Env       map[string]string
	Headers   map[string]string
}

// StagedMCPServers returns the MCP servers accumulated during Materialize.
func (m *Materializer) StagedMCPServers() []StagedMCPServer {
	return m.stagedMCPServers
}

// RenderOpencodeMCPServerEntry renders one MCP server into the opencode config
// shape per MATERIALIZE-CONTRACT.md. Shared between the AgentConfigWriter
// (cmd/workspace-agentd/agent_config_writer.go) and the boot-time
// applyMCPServersToConfig (cmd/workspace-agentd/secrets.go) so both paths
// produce identical output.
func RenderOpencodeMCPServerEntry(srv StagedMCPServer) map[string]any {
	isRemote := srv.Transport == "http" || srv.Transport == "sse"
	entry := map[string]any{"enabled": true}
	if isRemote {
		entry["type"] = "remote"
		entry["url"] = srv.URL
		if len(srv.Headers) > 0 {
			entry["headers"] = srv.Headers
		}
	} else {
		entry["type"] = "local"
		entry["command"] = append([]string{srv.Command}, srv.Args...)
		if len(srv.Env) > 0 {
			entry["environment"] = srv.Env
		}
	}
	if srv.TimeoutMs > 0 {
		entry["timeout"] = srv.TimeoutMs
	}
	return entry
}

// EnrichProviders applies fn to the staged provider slice, replacing it with
// the result. Callers use this to inject additional fields (e.g. a live model
// list fetched from the provider's /models endpoint) after Materialize and
// before FlushProviders. fn must not be nil.
func (m *Materializer) EnrichProviders(fn func([]sec.LLMProviderData) []sec.LLMProviderData) {
	if fn == nil {
		return
	}
	m.stagedProviders = fn(m.stagedProviders)
}

// FormatProviders calls the formatter with all staged LLM provider data and
// returns the formatted bytes WITHOUT writing to disk. Callers that use an
// external config writer (e.g. AgentConfigWriter in cmd/workspace-agentd)
// call this instead of FlushProviders so the writer is the sole disk writer.
//
// Returns (nil, nil) when formatter is nil or no providers are staged,
// matching FlushProviders' no-op semantics. This lets callers unconditionally
// call FormatProviders → writer.SetProviders without branching.
func (m *Materializer) FormatProviders(formatter LLMProviderFormatter) ([]byte, error) {
	if formatter == nil || len(m.stagedProviders) == 0 {
		return nil, nil
	}
	cfg, err := formatter(m.stagedProviders)
	if err != nil {
		return nil, fmt.Errorf("llm-provider formatter: %w", err)
	}
	return cfg, nil
}

// FlushProviders calls FormatProviders and writes the result to
// AgentConfigPath. Used by callers that do NOT have an AgentConfigWriter
// (e.g. the materialize subcommand, which runs as a separate process
// before agentd starts). Callers inside the agentd process should use
// FormatProviders + AgentConfigWriter.Rebuild instead.
//
// When formatter is nil, FlushProviders is a no-op (no agent config is
// written). This allows callers to conditionally skip agent-specific
// rendering.
// AgentConfigWriteMode is the mode for the materialized agent-config.json
// (design 0051 §D1): 0640 — written across the uid split (uid-1000 init at
// boot, uid-2000 sidecar on reload), read by uid-1000 opencode via the
// pod's shared gid 1000. The T2 exception; see the header comment.
const AgentConfigWriteMode os.FileMode = 0o640

func (m *Materializer) FlushProviders(formatter LLMProviderFormatter) error {
	cfg, err := m.FormatProviders(formatter)
	if err != nil {
		return err
	}
	if cfg == nil {
		return nil
	}
	return atomicWrite(m.FS, m.Paths.AgentConfigPath, cfg, AgentConfigWriteMode)
}

// --- file-write helpers ---------------------------------------------------

// atomicWrite writes the file with perm set in the open syscall, closing
// the TOCTOU window where chmod-after-write would leave the file readable
// to a same-UID watcher (Epic 17 G20).
//
// O_TRUNC is required because reset() does best-effort cleanup but a stale
// file may remain (e.g. on a partial materialization). We do NOT use
// O_EXCL because reset() may have raced with another writer; clobbering is
// the correct semantics for a full-replace materialization.
func atomicWrite(fs Filesystem, path string, data []byte, perm os.FileMode) error {
	f, err := fs.OpenForCreate(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, perm)
	if err != nil {
		return fmt.Errorf("open %s: %w", path, err)
	}
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		return fmt.Errorf("write %s: %w", path, err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("close %s: %w", path, err)
	}
	return nil
}

func appendFile(fs Filesystem, path string, data []byte, perm os.FileMode) error {
	f, err := fs.OpenForCreate(path, os.O_WRONLY|os.O_CREATE|os.O_APPEND, perm)
	if err != nil {
		return fmt.Errorf("open %s: %w", path, err)
	}
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		return fmt.Errorf("write %s: %w", path, err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("close %s: %w", path, err)
	}
	return nil
}

// --- entrypoint helper ----------------------------------------------------

// LoadSecretsFile reads and parses a secrets.json file.
func LoadSecretsFile(path string) ([]Secret, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", path, err)
	}
	var secrets []Secret
	if err := json.Unmarshal(data, &secrets); err != nil {
		return nil, fmt.Errorf("parsing %s: %w", path, err)
	}
	return secrets, nil
}
