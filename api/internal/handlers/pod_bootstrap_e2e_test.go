// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package handlers

// pod_bootstrap_e2e_test.go exercises the FULL credential materialization
// path that production runs at pod boot:
//
//	provider_credentials (DB row)
//	  → SecretService.InjectSecretsForPodBootstrap (best-effort user-DEK unwrap;
//	                                                degrades to sessionless
//	                                                on DEK-unavailable)
//	  → PodBootstrapHandler.Bootstrap              (HTTP /internal/v1/pod-bootstrap)
//	  → workspace-agentd bootstrap                 (subprocess: fetch + write secrets.json)
//	  → workspace-agentd materialize               (subprocess: write agent-config.json)
//	  → agent-config.json                          (the file opencode actually reads)
//
// Each prior layer is covered by a unit test in isolation, but no test wired
// them together. That is exactly the gap README-LLM.md §0 (TDD) calls out:
// "integration tests that exercise the real wiring… unit tests alone are not
// sufficient". The org-provider regression (org-level llm providers not
// materializing in new workspaces) shipped because every layer mocked the next.
//
// This test asserts on the materialized agent-config.json (the file opencode
// reads), not on intermediate bytes. Any break in the chain — provider
// resolution, bootstrap→materialize handoff, tmpfs path resolution, or the
// decrypt wiring — produces an agent-config.json missing a provider and fails.

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lenaxia/llmsafespaces/pkg/secrets"
	"github.com/lenaxia/llmsafespaces/pkg/types"
)

// --- minimal stubs that let us construct a REAL *secrets.SecretService ---
//
// SecretService.InjectSecretsForPodBootstrap only touches:
//   - the CredentialStore type assertion (H-3 path)
//   - KeyService.GetDEKForUser (returns ErrDEKUnavailable when jwt_sessions
//     enumerator not wired; degrades to InjectSessionlessSecrets)
//   - SecretStore.GetBindings (non-LLM path — empty in this test)
//   - SecretStore.LogAudit (best-effort)
//
// The remaining SecretStore / KeyStore methods exist only to satisfy the
// interface so NewSecretService accepts the store. They panic if called so
// any drift in SecretService's dependencies surfaces here rather than
// silently returning zero values.

// staticTokenReviewer always returns a valid workspace-<id> SA principal.
// PodBootstrapHandler treats the token as already-authenticated (the real
// TokenReview is a K8s API call we cannot make in a unit test); this test
// verifies everything AFTER authentication.
type staticTokenReviewer struct {
	username string
	err      error
}

func (s *staticTokenReviewer) Review(_ context.Context, _ string) (string, error) {
	return s.username, s.err
}

// wsMetaLookup returns a fixed WorkspaceMetadata for the bootstrap handler's
// GetWorkspace call. DefaultModel is set so the workspace-config.json →
// agent-config.json model handoff is also covered.
type wsMetaLookup struct {
	ws *types.WorkspaceMetadata
}

func (l *wsMetaLookup) GetWorkspace(_ context.Context, _ string) (*types.WorkspaceMetadata, error) {
	return l.ws, nil
}

// e2eSecretStore is a minimal SecretStore + CredentialStore for the e2e
// path. It carries the provider bindings the real SecretService will decrypt.
type e2eSecretStore struct {
	bindings []secrets.CredentialBinding
}

// --- CredentialStore surface (the path the injector methods use) ---

func (s *e2eSecretStore) GetWorkspaceCredentials(_ context.Context, _ string) ([]secrets.CredentialBinding, error) {
	return s.bindings, nil
}
func (s *e2eSecretStore) UpsertFreeTierCredential(_ context.Context, _ []byte) error {
	return nil
}
func (s *e2eSecretStore) SeedWorkspaceCredentials(_ context.Context, _, _ string, _ *string) error {
	return nil
}
func (s *e2eSecretStore) BindCredentialToAllUserWorkspaces(_ context.Context, _, _ string) error {
	return nil
}
func (s *e2eSecretStore) HasUserProviderCredential(_ context.Context, _, _ string) (bool, error) {
	return false, nil
}

// --- SecretStore surface (only the methods touched by the injection path
// return real values; the rest panic to surface drift loudly) ---

func (s *e2eSecretStore) GetBindings(_ context.Context, _ string) ([]*secrets.UserSecret, error) {
	return nil, nil // no non-LLM secrets in this test
}
func (s *e2eSecretStore) LogAudit(_ context.Context, _ *secrets.AuditEntry) error { return nil }
func (s *e2eSecretStore) CreateSecret(_ context.Context, _ *secrets.UserSecret) error {
	panic("unexpected CreateSecret in bootstrap e2e")
}
func (s *e2eSecretStore) GetSecret(_ context.Context, _, _ string) (*secrets.UserSecret, error) {
	panic("unexpected GetSecret in bootstrap e2e")
}
func (s *e2eSecretStore) GetSecretByName(_ context.Context, _, _ string) (*secrets.UserSecret, error) {
	panic("unexpected GetSecretByName in bootstrap e2e")
}
func (s *e2eSecretStore) ListSecrets(_ context.Context, _ string) ([]*secrets.UserSecret, error) {
	panic("unexpected ListSecrets in bootstrap e2e")
}
func (s *e2eSecretStore) ListGlobalDefaultSecrets(_ context.Context, _ string) ([]*secrets.UserSecret, error) {
	panic("unexpected ListGlobalDefaultSecrets in bootstrap e2e")
}
func (s *e2eSecretStore) UpdateSecret(_ context.Context, _ *secrets.UserSecret) error {
	panic("unexpected UpdateSecret in bootstrap e2e")
}
func (s *e2eSecretStore) DeleteSecret(_ context.Context, _, _ string) error {
	panic("unexpected DeleteSecret in bootstrap e2e")
}
func (s *e2eSecretStore) ReEncryptUserSecrets(_ context.Context, _ string, _ int,
	_ func([]byte) ([]byte, error), _ func(context.Context) error) error {
	panic("unexpected ReEncryptUserSecrets in bootstrap e2e")
}
func (s *e2eSecretStore) SetBindings(_ context.Context, _ string, _ []string) error {
	panic("unexpected SetBindings in bootstrap e2e")
}
func (s *e2eSecretStore) AddBindings(_ context.Context, _ string, _ []string) error {
	panic("unexpected AddBindings in bootstrap e2e")
}
func (s *e2eSecretStore) GetBindingsForSecret(_ context.Context, _ string) ([]string, error) {
	panic("unexpected GetBindingsForSecret in bootstrap e2e")
}
func (s *e2eSecretStore) QueryAudit(_ context.Context, _ string, _ secrets.AuditQuery) ([]*secrets.AuditEntry, error) {
	panic("unexpected QueryAudit in bootstrap e2e")
}

// e2eKeyStore is a no-op KeyStore. The injection path never touches user
// key records (admin/org use RootKeyProviders; user uses the DEK cache).
type e2eKeyStore struct{}

func (e2eKeyStore) GetUserKey(_ context.Context, _ string) (*secrets.UserKeyRecord, error) {
	return nil, nil
}
func (e2eKeyStore) CreateUserKey(_ context.Context, _ *secrets.UserKeyRecord) error { return nil }
func (e2eKeyStore) UpdateWrappedDEK(_ context.Context, _ string, _, _ []byte, _ int) error {
	return nil
}
func (e2eKeyStore) UpdateWrappedDEKRecovery(_ context.Context, _ string, _, _ []byte) error {
	return nil
}

// e2eDEKCache serves the user DEK for the test session ID.
type e2eDEKCache struct {
	dek []byte
}

func (c *e2eDEKCache) CacheDEK(_ context.Context, _ string, _ []byte, _ time.Duration) error {
	return nil
}
func (c *e2eDEKCache) GetDEK(_ context.Context, _ string) ([]byte, error) { return c.dek, nil }
func (c *e2eDEKCache) EvictDEK(_ context.Context, _ string) error         { return nil }

// e2eJWTSessionStore is a minimal in-memory JWTSessionStore for exercising
// the design-0045 Change 1 happy path: pod-bootstrap unwraps a jwt_sessions
// row via GetDEKForUser and returns user-DEK secrets alongside server-KEK
// secrets. See TestE2E_BootstrapMaterialize_UserDEKUnwrappable_MaterializesUserProvider.
//
// Only ListActiveJWTSessionsForUser (the read path GetDEKForUser uses) is
// meaningfully implemented; the other methods panic if called so a future
// change that reaches for them surfaces here instead of silently no-op'ing.
type e2eJWTSessionStore struct {
	rows []*secrets.JWTSession
}

func (s *e2eJWTSessionStore) ListActiveJWTSessionsForUser(_ context.Context, userID string, _ int) ([]*secrets.JWTSession, error) {
	out := make([]*secrets.JWTSession, 0)
	for _, r := range s.rows {
		if r.UserID != userID {
			continue
		}
		if !r.ExpiresAt.After(time.Now()) {
			continue
		}
		cp := *r
		out = append(out, &cp)
	}
	return out, nil
}

func (s *e2eJWTSessionStore) GetJWTSession(_ context.Context, _ uuid.UUID) (*secrets.JWTSession, error) {
	panic("e2eJWTSessionStore.GetJWTSession not implemented — only ListActiveJWTSessionsForUser is used by design-0045 path")
}
func (s *e2eJWTSessionStore) WriteJWTSession(_ context.Context, _ *secrets.JWTSession) error {
	panic("e2eJWTSessionStore.WriteJWTSession not implemented")
}
func (s *e2eJWTSessionStore) DeleteJWTSession(_ context.Context, _ uuid.UUID) error {
	panic("e2eJWTSessionStore.DeleteJWTSession not implemented")
}
func (s *e2eJWTSessionStore) DeleteJWTSessionsForUser(_ context.Context, _ string) (int64, error) {
	panic("e2eJWTSessionStore.DeleteJWTSessionsForUser not implemented")
}
func (s *e2eJWTSessionStore) DeleteExpiredJWTSessions(_ context.Context, _ time.Time) (int64, error) {
	panic("e2eJWTSessionStore.DeleteExpiredJWTSessions not implemented")
}

// e2eSigningKeys is a minimal SigningKeyEnumerator wrapping a static list.
// Iteration order matches the slice; return false from fn to stop
// (matching KeyService.tryUnwrapRowWithKnownKeys's contract).
type e2eSigningKeys struct {
	keys [][]byte
}

func (e *e2eSigningKeys) EachSigningKey(fn func(key []byte) bool) {
	for _, k := range e.keys {
		if !fn(k) {
			return
		}
	}
}

// seedJWTSession creates a jwt_sessions row wrapping the given DEK under a
// KEK derived from signingKey || jti (matching KeyService's login-time
// contract at key_service.go:UnlockDEKWithSigningKey). Returns the row so
// tests can assert on jti. Mirrors the pattern in
// pkg/secrets/key_service_get_dek_for_user_test.go's fixture.
func seedJWTSession(t *testing.T, userID string, dek, signingKey []byte, expiresAt time.Time) *secrets.JWTSession {
	t.Helper()
	jti := uuid.New()

	kekSalt, err := secrets.GenerateSalt()
	require.NoError(t, err)

	keyMaterial := make([]byte, 0, len(signingKey)+36)
	keyMaterial = append(keyMaterial, signingKey...)
	keyMaterial = append(keyMaterial, []byte(jti.String())...)
	kek, err := secrets.DeriveKEKFromKey(keyMaterial, kekSalt, secrets.JWTSessionKEKInfo)
	require.NoError(t, err)

	wrapped, err := secrets.EncryptSecret(kek, dek)
	require.NoError(t, err)

	return &secrets.JWTSession{
		JTI:        jti,
		UserID:     userID,
		WrappedDEK: wrapped,
		KEKSalt:    kekSalt,
		CreatedAt:  time.Now().Add(-time.Hour),
		ExpiresAt:  expiresAt,
	}
}

// buildAgentd builds the workspace-agentd binary into a temp dir and returns
// its path. Skipped under -short and on Windows (subprocess test assumes
// POSIX). `go test` runs with the package dir as cwd, so the module root is
// located by walking up to the nearest go.mod (robust against package moves).
func buildAgentd(t *testing.T) string {
	t.Helper()
	if testing.Short() {
		t.Skip("skipping subprocess e2e in -short mode")
	}
	if runtime.GOOS == "windows" {
		t.Skip("subprocess e2e assumes POSIX")
	}
	modRoot := findModuleRoot(t)
	dir := t.TempDir()
	bin := filepath.Join(dir, "workspace-agentd")
	cmd := exec.Command("go", "build", "-o", bin, "./cmd/workspace-agentd")
	cmd.Dir = modRoot
	cmd.Stderr = os.Stderr
	done := make(chan error, 1)
	go func() { done <- cmd.Run() }()
	select {
	case err := <-done:
		require.NoError(t, err, "go build ./cmd/workspace-agentd failed (cwd=%s)", modRoot)
	case <-time.After(120 * time.Second):
		_ = cmd.Process.Kill()
		t.Fatal("go build ./cmd/workspace-agentd timed out after 120s")
	}
	return bin
}

// findModuleRoot walks up from the test cwd until it finds a go.mod. Used so
// the e2e subprocess build resolves ./cmd/workspace-agentd regardless of
// which package directory the test runs from.
func findModuleRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	require.NoError(t, err)
	for i := 0; i < 20; i++ {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	t.Fatal("could not find go.mod walking up from " + dir)
	return ""
}

// runAgentd runs workspace-agentd with a subcommand + env overrides and
// returns (exitCode, stdout, stderr). Mirrors the helper in
// cmd/workspace-agentd/secrets_test.go.
func runAgentd(t *testing.T, bin string, args []string, env map[string]string) (int, string, string) {
	t.Helper()
	cmd := exec.Command(bin, args...)
	cmd.Env = os.Environ()
	for k, v := range env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	runErr := cmd.Run()
	exit := 0
	if exitErr, ok := runErr.(*exec.ExitError); ok {
		exit = exitErr.ExitCode()
	} else if runErr != nil {
		t.Fatalf("workspace-agentd %v subprocess failed: %v\nstdout=%s\nstderr=%s",
			args, runErr, stdout.String(), stderr.String())
	}
	return exit, stdout.String(), stderr.String()
}

// e2eProviderBinding describes one provider credential to seed.
type e2eProviderBinding struct {
	ownerType string // "admin" | "user" | "org"
	ownerID   string
	provider  string
	apiKey    string
}

// deterministicKey returns a 32-byte key where every byte == seed. Used to
// derive distinct per-ownerType KEKs without pulling in crypto/rand.
func deterministicKey(seed byte) []byte {
	k := make([]byte, 32)
	for i := range k {
		k[i] = seed
	}
	return k
}

// encryptE2EBinding encrypts an LLMProviderData blob under the correct KEK
// for the ownerType and returns a CredentialBinding ready for the store.
func encryptE2EBinding(t *testing.T, b e2eProviderBinding) secrets.CredentialBinding {
	t.Helper()
	var kek []byte
	switch b.ownerType {
	case "admin":
		kek = deterministicKey(0x01)
	case "org":
		kek = deterministicKey(0x02)
	case "user":
		kek = deterministicKey(0x03)
	default:
		t.Fatalf("unknown ownerType %q", b.ownerType)
	}
	plaintext, err := json.Marshal(secrets.LLMProviderData{Kind: b.provider, Slug: b.provider, APIKey: b.apiKey})
	require.NoError(t, err)
	cipher, err := secrets.EncryptSecret(kek, plaintext)
	require.NoError(t, err)
	return secrets.CredentialBinding{
		OwnerType: b.ownerType,
		OwnerID:   b.ownerID,
		Kind:      b.provider, Slug: b.provider,
		Ciphertext: cipher,
		SourceType: "auto",
	}
}

// bootstrapE2EConfig controls the harness wiring. The zero value (with
// bindings+defaultModel set) reproduces the production configuration: all
// three ownerType providers wired. Unhappy-path tests flip a field to nil to
// simulate a misconfiguration (e.g. SetOrgProvider forgotten in app.go).
type bootstrapE2EConfig struct {
	bindings     []e2eProviderBinding
	defaultModel string
	// wireAdmin / wireOrg / wireUser flip individual RootKeyProviders off.
	// nil provider => that ownerType's bindings are skipped at decrypt time
	// (matching production's "skip with audit event" behavior at injection.go:176).
	wireAdmin *bool // nil = true (default); set to addr-of-false to disable
	wireOrg   *bool
	// wrongOrgKey, when non-empty, wires a RootKeyProvider built from this key
	// instead of the correct org KEK (0x02). The org binding is encrypted under
	// 0x02, so decrypt FAILS at injection.go:74 — exercising the real wrong-key
	// decrypt-failure path (audit + skip + fall through), distinct from the
	// nil-provider skip path exercised by wireOrg=false.
	wrongOrgKey []byte
	// reviewerErr makes the token reviewer reject the token (simulates a
	// forged/expired SA token). When set, bootstrap must degrade gracefully.
	reviewerErr error
	// workspaceNil makes the lookup return a nil workspace (404 path).
	workspaceNil bool
	// wireJWTSessions, when true, plumbs a JWTSessionStore + SigningKeyEnumerator
	// into the KeyService so InjectSecretsForPodBootstrap can find an unwrappable
	// jwt_sessions row and deliver user-DEK secrets (design 0045 Change 1
	// happy path). Zero value = false → GetDEKForUser returns ErrDEKUnavailable
	// and InjectSecretsForPodBootstrap degrades to sessionless (the historical
	// Epic-35 behavior for offline users).
	wireJWTSessions bool
}

func boolPtr(v bool) *bool { return &v }

// runBootstrapMaterializeE2E stands up the real PodBootstrapHandler backed by
// the real SecretService, runs the real agentd bootstrap + materialize
// subprocesses against it, and returns the path to the materialized
// agent-config.json plus the bootstrap/materialize exit codes + stderr.
//
// Each ownerType gets a distinct KEK (matching production US-50.2), and the
// SecretService is wired with one RootKeyProvider per ownerType so the decrypt
// path is the real one. A misconfiguration (e.g. SetOrgProvider forgotten)
// surfaces as a missing provider in agent-config.json.
func runBootstrapMaterializeE2E(t *testing.T, bindings []e2eProviderBinding, defaultModel string) (agentCfgPath string, bootstrapExit, materializeExit int, bootstrapStderr, materializeStderr string) {
	return runBootstrapMaterializeE2EWith(t, bootstrapE2EConfig{
		bindings:     bindings,
		defaultModel: defaultModel,
	})
}

// runBootstrapMaterializeE2EWith is the full-control harness. Happy paths
// call runBootstrapMaterializeE2E; unhappy paths call this directly.
func runBootstrapMaterializeE2EWith(t *testing.T, cfg bootstrapE2EConfig) (agentCfgPath string, bootstrapExit, materializeExit int, bootstrapStderr, materializeStderr string) {
	t.Helper()
	bin := buildAgentd(t)
	dir := t.TempDir()

	userDEK := deterministicKey(0x03)
	keySvc := secrets.NewKeyService(e2eKeyStore{}, &e2eDEKCache{dek: userDEK})

	// Design 0045 Change 1: when wireJWTSessions is set, seed a valid
	// jwt_sessions row wrapping the same userDEK under a test signing key.
	// GetDEKForUser (called by InjectSecretsForPodBootstrap) will find and
	// unwrap this row, enabling user-DEK bindings to decrypt via the
	// normal (dek, jti) → decryptBinding path. Without this wiring the
	// harness exercises the sessionless-degrade path (user offline).
	if cfg.wireJWTSessions {
		signingKey := deterministicKey(0x04)
		row := seedJWTSession(t, "user-e2e", userDEK, signingKey, time.Now().Add(time.Hour))
		keySvc.SetJWTSessionStore(&e2eJWTSessionStore{rows: []*secrets.JWTSession{row}})
		keySvc.SetSigningKeyEnumerator(&e2eSigningKeys{keys: [][]byte{signingKey}})
	}

	credBindings := make([]secrets.CredentialBinding, 0, len(cfg.bindings))
	for _, b := range cfg.bindings {
		credBindings = append(credBindings, encryptE2EBinding(t, b))
	}
	store := &e2eSecretStore{bindings: credBindings}
	svc := secrets.NewSecretService(keySvc, store)

	// Distinct RootKeyProvider per ownerType — this is the production wiring
	// (app.go:379-380). Forgetting one is exactly the regression we guard.
	if cfg.wireAdmin == nil || *cfg.wireAdmin {
		adminProv, err := secrets.NewStaticKeyProvider(deterministicKey(0x01))
		require.NoError(t, err)
		svc.SetAdminProvider(adminProv)
	}
	if cfg.wireOrg == nil || *cfg.wireOrg {
		orgKey := deterministicKey(0x02)
		if cfg.wrongOrgKey != nil {
			orgKey = cfg.wrongOrgKey // exercise the real decrypt-failure path
		}
		orgProv, err := secrets.NewStaticKeyProvider(orgKey)
		require.NoError(t, err)
		svc.SetOrgProvider(orgProv)
	}

	// Stand up the real PodBootstrapHandler over HTTP.
	gin.SetMode(gin.TestMode)
	router := gin.New()
	const testNS = "llmsafespace-e2e"
	const wsID = "ws-e2e"
	reviewer := &staticTokenReviewer{
		username: "system:serviceaccount:" + testNS + ":workspace-" + wsID,
		err:      cfg.reviewerErr,
	}
	var lookup bootstrapWorkspaceLookup = &wsMetaLookup{ws: &types.WorkspaceMetadata{
		ID:           wsID,
		UserID:       "user-e2e",
		DefaultModel: cfg.defaultModel,
	}}
	if cfg.workspaceNil {
		lookup = &wsMetaLookup{ws: nil}
	}
	h := NewPodBootstrapHandler(reviewer, svc, lookup, nil, testNS)
	router.POST("/internal/v1/pod-bootstrap", h.Bootstrap)
	apiSrv := httptest.NewServer(router)
	t.Cleanup(apiSrv.Close)

	// Run the REAL `workspace-agentd bootstrap` subprocess.
	secretsOut := filepath.Join(dir, "secrets.json")
	tokenFile := filepath.Join(dir, "token")
	require.NoError(t, os.WriteFile(tokenFile, []byte("e2e-token"), 0o600))

	bootstrapExit, _, bootstrapStderr = runAgentd(t, bin,
		[]string{"bootstrap", "--workspace-id", wsID, "--api-url", apiSrv.URL,
			"--token-file", tokenFile, "--out", secretsOut},
		nil,
	)
	require.Equal(t, 0, bootstrapExit, "bootstrap exit non-zero; stderr=%s", bootstrapStderr)
	_, statErr := os.Stat(secretsOut)
	require.NoError(t, statErr, "bootstrap must write secrets.json; stderr=%s", bootstrapStderr)

	// Run the REAL `workspace-agentd materialize` subprocess.
	agentCfgPath = filepath.Join(dir, "agent-config.json")
	materializeExit, _, materializeStderr = runAgentd(t, bin,
		[]string{"materialize", "--from", secretsOut},
		map[string]string{
			"LLMSAFESPACES_AGENT_CONFIG_PATH": agentCfgPath,
			"LLMSAFESPACES_SECRETS_BASE_DIR":  filepath.Join(dir, "secrets"),
			"LLMSAFESPACES_SSH_DIR":           filepath.Join(dir, ".ssh"),
			"LLMSAFESPACES_SECRETS_ENV_PATH":  filepath.Join(dir, "env"),
			"LLMSAFESPACES_GIT_CREDS_PATH":    filepath.Join(dir, ".git-credentials"),
			"LLMSAFESPACES_RELOAD_CACHE_PATH": filepath.Join(dir, "last-reload-secrets.json"),
			"HOME":                            dir,
		},
	)
	return agentCfgPath, bootstrapExit, materializeExit, bootstrapStderr, materializeStderr
}

// readAgentConfig parses the materialized agent-config.json into the subset
// the assertions need. Fails the test if the file is missing or invalid JSON.
func readAgentConfig(t *testing.T, path string) struct {
	Schema   string                     `json:"$schema"`
	Provider map[string]json.RawMessage `json:"provider"`
	Model    string                     `json:"model,omitempty"`
} {
	t.Helper()
	raw, err := os.ReadFile(path)
	require.NoError(t, err, "agent-config.json must exist at %s", path)
	var cfg struct {
		Schema   string                     `json:"$schema"`
		Provider map[string]json.RawMessage `json:"provider"`
		Model    string                     `json:"model,omitempty"`
	}
	require.NoError(t, json.Unmarshal(raw, &cfg), "agent-config.json must be valid JSON; got=%s", raw)
	return cfg
}

// TestE2E_BootstrapMaterialize_AllOwnerTypesMaterialized_UserOffline is the
// central regression guard for the org-provider-not-materializing bug, in
// the user-DEK-unavailable case. It seeds one credential per ownerType
// (org, admin, user), runs the full boot chain WITHOUT wiring a
// JWTSessionStore, and asserts:
//
//   - server-KEK credentials (admin, org) materialize;
//   - user-DEK credentials do NOT (unwrap fails → degrade to sessionless).
//
// This is the pre-design-0045 behavior, still relevant post-fix as the
// degrade path when the workspace owner is offline past the jwt_session TTL.
// The happy-path counterpart is
// TestE2E_BootstrapMaterialize_UserDEKUnwrappable_MaterializesUserProvider.
//
// A break at any seam fails this test:
//   - SecretService not wired with SetOrgProvider → org provider missing
//   - bootstrap subcommand drops secrets.json → all providers missing
//   - materialize writes to wrong path → empty config
//   - decryptFnFor returns nil for a configured provider → that provider missing
//   - User-owned bindings crash the entire prep instead of degrading
//     (regression from the 2026-06-24 bootstrap-path-error incident)
func TestE2E_BootstrapMaterialize_AllOwnerTypesMaterialized_UserOffline(t *testing.T) {
	agentCfgPath, bootstrapExit, materializeExit, bootstrapStderr, materializeStderr :=
		runBootstrapMaterializeE2E(t,
			[]e2eProviderBinding{
				{ownerType: "org", ownerID: "org-1", provider: "anthropic", apiKey: "sk-org"},
				{ownerType: "admin", ownerID: "_platform", provider: "opencode", apiKey: "sk-admin"},
				{ownerType: "user", ownerID: "user-e2e", provider: "openai", apiKey: "sk-user"},
			},
			"anthropic/claude-sonnet-4-5",
		)

	require.Equal(t, 0, bootstrapExit, "bootstrap must succeed; stderr=%s", bootstrapStderr)
	require.Equal(t, 0, materializeExit, "materialize must succeed; stderr=%s", materializeStderr)

	cfg := readAgentConfig(t, agentCfgPath)
	assert.Equal(t, "https://opencode.ai/config.json", cfg.Schema,
		"$schema must be set so opencode treats the file as config")

	// Server-KEK encrypted creds (admin/org) MUST materialize at boot.
	assert.Contains(t, cfg.Provider, "anthropic",
		"org-owned provider must materialize (the org-provider regression)")
	assert.Contains(t, cfg.Provider, "opencode",
		"admin-owned provider must materialize")

	// User-DEK encrypted creds must NOT materialize on the offline-degrade
	// path: no JWTSessionStore is wired, so GetDEKForUser returns
	// ErrDEKUnavailable and InjectSecretsForPodBootstrap falls back to
	// InjectSessionlessSecrets (user-DEK bindings audited-and-skipped).
	// The reload-secrets flow will still deliver them when the user next
	// logs in.
	assert.NotContains(t, cfg.Provider, "openai",
		"on user-offline degrade path, user-owned provider must NOT materialize at boot — delivered later via reload-secrets")

	// Verify the org apiKey round-tripped end-to-end (decrypt → re-marshal).
	var anthropicEntry struct {
		Options struct {
			APIKey string `json:"apiKey"`
		} `json:"options"`
	}
	require.NoError(t, json.Unmarshal(cfg.Provider["anthropic"], &anthropicEntry))
	assert.Equal(t, "sk-org", anthropicEntry.Options.APIKey,
		"org provider apiKey must survive decrypt → bootstrap → materialize")

	// Default model from workspace-config.json must be present (separate
	// handoff that also broke historically).
	assert.Equal(t, "anthropic/claude-sonnet-4-5", cfg.Model,
		"default model must be written as providerID/modelID")
}

// TestE2E_BootstrapMaterialize_UserDEKUnwrappable_MaterializesUserProvider
// is the design-0045 Change 1 happy-path e2e test. It wires a
// JWTSessionStore with an unwrappable row so InjectSecretsForPodBootstrap
// finds the user's DEK via GetDEKForUser and delivers user-DEK secrets
// alongside server-KEK secrets — the entire point of the fix.
//
// This test would FAIL against pre-PR code: the pod-bootstrap handler
// used InjectSessionlessSecrets, which unconditionally skips user-DEK
// bindings. It ALSO fails against the design-0045 code path if any of:
//   - InjectSecretsForPodBootstrap regresses the sessionless fallback into
//     the actual unwrap path (would break the offline test above);
//   - GetDEKForUser stops writing back to Redis under the returned jti;
//   - the bootstrap→materialize handoff drops user-DEK entries from the
//     secrets.json wire format;
//   - materialize refuses to write user-DEK content to agent-config.json
//     even when secrets.json contains it.
//
// The offline-degrade counterpart is
// TestE2E_BootstrapMaterialize_AllOwnerTypesMaterialized_UserOffline.
func TestE2E_BootstrapMaterialize_UserDEKUnwrappable_MaterializesUserProvider(t *testing.T) {
	agentCfgPath, bootstrapExit, materializeExit, bootstrapStderr, materializeStderr :=
		runBootstrapMaterializeE2EWith(t, bootstrapE2EConfig{
			bindings: []e2eProviderBinding{
				{ownerType: "org", ownerID: "org-1", provider: "anthropic", apiKey: "sk-org"},
				{ownerType: "admin", ownerID: "_platform", provider: "opencode", apiKey: "sk-admin"},
				{ownerType: "user", ownerID: "user-e2e", provider: "openai", apiKey: "sk-user-happy"},
			},
			defaultModel:    "anthropic/claude-sonnet-4-5",
			wireJWTSessions: true, // <-- the design 0045 Change 1 happy path
		})

	require.Equal(t, 0, bootstrapExit, "bootstrap must succeed; stderr=%s", bootstrapStderr)
	require.Equal(t, 0, materializeExit, "materialize must succeed; stderr=%s", materializeStderr)

	cfg := readAgentConfig(t, agentCfgPath)

	// Server-KEK creds still materialize (regression guard for both changes).
	assert.Contains(t, cfg.Provider, "anthropic",
		"org-owned provider must materialize on the happy path too")
	assert.Contains(t, cfg.Provider, "opencode",
		"admin-owned provider must materialize on the happy path too")

	// User-DEK cred MUST materialize on the happy path — this is the
	// design 0045 Change 1 fix.
	require.Contains(t, cfg.Provider, "openai",
		"user-owned provider MUST materialize at boot when jwt_sessions unwraps "+
			"(design 0045 Change 1). If this fails, the PR's core behavior change is broken.")

	// The user apiKey plaintext must survive the full round-trip:
	// user-KEK encrypt → DB → GetDEKForUser unwrap → InjectSecrets decrypt
	// → bootstrap wire → materialize → agent-config.json.
	var openaiEntry struct {
		Options struct {
			APIKey string `json:"apiKey"`
		} `json:"options"`
	}
	require.NoError(t, json.Unmarshal(cfg.Provider["openai"], &openaiEntry))
	assert.Equal(t, "sk-user-happy", openaiEntry.Options.APIKey,
		"user provider apiKey must survive the full happy-path round-trip")
}

// TestE2E_BootstrapMaterialize_OrgOnly pins the regression that motivated
// this test: a workspace whose only credential is org-scoped (e.g. an
// org-managed deployment with no personal credentials) must still get exactly
// that provider.
func TestE2E_BootstrapMaterialize_OrgOnly(t *testing.T) {
	agentCfgPath, _, materializeExit, _, materializeStderr :=
		runBootstrapMaterializeE2E(t,
			[]e2eProviderBinding{
				{ownerType: "org", ownerID: "org-1", provider: "anthropic", apiKey: "sk-org"},
			},
			"",
		)
	require.Equal(t, 0, materializeExit, "materialize must succeed; stderr=%s", materializeStderr)

	cfg := readAgentConfig(t, agentCfgPath)
	assert.Contains(t, cfg.Provider, "anthropic",
		"the sole org provider must materialize even when no other ownerType is present")
}

// TestE2E_BootstrapMaterialize_ZeroCredentials_StillBoots pins the
// graceful-degradation contract: a workspace with no credential bindings must
// still exit 0 on both subcommands so the pod boots and the live
// /v1/reload-secrets push path can deliver credentials later.
func TestE2E_BootstrapMaterialize_ZeroCredentials_StillBoots(t *testing.T) {
	_, bootstrapExit, materializeExit, bootstrapStderr, materializeStderr :=
		runBootstrapMaterializeE2E(t, nil, "")
	require.Equal(t, 0, bootstrapExit, "bootstrap must not fail on empty bindings; stderr=%s", bootstrapStderr)
	require.Equal(t, 0, materializeExit, "materialize must not fail on empty secrets.json; stderr=%s", materializeStderr)
}

// --- Unhappy paths ----------------------------------------------------------
//
// The happy-path tests above prove credentials flow when everything is wired.
// The unhappy paths below pin the FAILURE MODES that must degrade gracefully
// (pod still boots) rather than crash-loop or silently surface wrong creds.
// Each mirrors a real production failure: a misconfigured provider, a forged
// token, a wrong KEK. A regression that turns a graceful degradation into a
// hard failure (or vice-versa: turns a hard failure into a silent wrong-cred
// surface) flips one of these tests.

// TestE2E_BootstrapMaterialize_OrgProviderUnwired_OrgDegradesGracefully is the
// direct regression guard for the original bug. When SetOrgProvider is NOT
// called (the misconfiguration that shipped), the org credential is skipped
// with an audit event (injection.go:176) — NOT a hard error. The pod must
// still boot and materialize the remaining providers. This test fails if that
// skip ever turns into a panic, a non-zero exit, or (worse) the org provider
// silently appearing via a fallback key.
func TestE2E_BootstrapMaterialize_OrgProviderUnwired_OrgDegradesGracefully(t *testing.T) {
	agentCfgPath, _, materializeExit, _, materializeStderr :=
		runBootstrapMaterializeE2EWith(t, bootstrapE2EConfig{
			bindings: []e2eProviderBinding{
				{ownerType: "org", ownerID: "org-1", provider: "anthropic", apiKey: "sk-org"},
				{ownerType: "admin", ownerID: "_platform", provider: "opencode", apiKey: "sk-admin"},
			},
			wireOrg: boolPtr(false), // the misconfiguration
		})
	// Pod must still boot (graceful degradation — never a crash-loop).
	require.Equal(t, 0, materializeExit, "materialize must succeed even when org provider unwired; stderr=%s", materializeStderr)

	cfg := readAgentConfig(t, agentCfgPath)
	assert.NotContains(t, cfg.Provider, "anthropic",
		"org provider must NOT materialize when SetOrgProvider was not called (would indicate a wrong-key fallback bug)")
	assert.Contains(t, cfg.Provider, "opencode",
		"admin provider must still materialize — only the unwired ownerType is skipped")
}

// TestE2E_BootstrapMaterialize_TokenRejected_StillBoots pins the contract that
// a rejected SA token (forged, expired, wrong audience) degrades to an empty
// secrets array (bootstrap.go:78-79) rather than failing the pod. The live
// /v1/reload-secrets push path delivers credentials on first activation.
func TestE2E_BootstrapMaterialize_TokenRejected_StillBoots(t *testing.T) {
	agentCfgPath, _, materializeExit, _, _ :=
		runBootstrapMaterializeE2EWith(t, bootstrapE2EConfig{
			bindings: []e2eProviderBinding{
				{ownerType: "admin", ownerID: "_platform", provider: "opencode", apiKey: "sk-admin"},
			},
			reviewerErr: errTokenNotAuthenticated,
		})
	require.Equal(t, 0, materializeExit, "materialize must succeed even when the SA token is rejected")

	// No provider should appear — the bootstrap handler returns 401, agentd
	// writes secrets.json="[]", and materialize produces no providers.
	// (agent-config.json may be absent on the zero-provider path; that's valid.)
	if raw, err := os.ReadFile(agentCfgPath); err == nil {
		var cfg struct {
			Provider map[string]json.RawMessage `json:"provider"`
		}
		if json.Unmarshal(raw, &cfg) == nil {
			assert.Empty(t, cfg.Provider, "no provider must materialize when the bootstrap token was rejected")
		}
	}
}

// TestE2E_BootstrapMaterialize_WorkspaceNotFound_StillBoots pins that a
// missing workspace (lookup returns nil → 404) degrades to empty secrets,
// not a hard failure. This is the resume-after-delete race.
func TestE2E_BootstrapMaterialize_WorkspaceNotFound_StillBoots(t *testing.T) {
	_, _, materializeExit, _, materializeStderr :=
		runBootstrapMaterializeE2EWith(t, bootstrapE2EConfig{
			bindings: []e2eProviderBinding{
				{ownerType: "admin", ownerID: "_platform", provider: "opencode", apiKey: "sk-admin"},
			},
			workspaceNil: true,
		})
	require.Equal(t, 0, materializeExit,
		"materialize must succeed even when the workspace is not found (resume race); stderr=%s", materializeStderr)
}

// TestE2E_BootstrapMaterialize_WrongKEK_SkipsBinding pins that a credential
// encrypted under key A but decrypted with a DIFFERENT key B (a key-rotation
// half-state where old ciphertext lingers) fails decrypt, is audited + skipped
// (injection.go:74-82), and the pod still boots. This exercises the real
// decrypt-failure path — distinct from the nil-provider skip path tested by
// OrgProviderUnwired. The org binding is encrypted under 0x02; the harness
// wires a wrong key (0xFF), so decrypt is ATTEMPTED and FAILS (not skipped).
func TestE2E_BootstrapMaterialize_WrongKEK_SkipsBinding(t *testing.T) {
	agentCfgPath, _, materializeExit, _, materializeStderr :=
		runBootstrapMaterializeE2EWith(t, bootstrapE2EConfig{
			bindings: []e2eProviderBinding{
				{ownerType: "org", ownerID: "org-1", provider: "anthropic", apiKey: "sk-org"},
				{ownerType: "admin", ownerID: "_platform", provider: "opencode", apiKey: "sk-admin"},
			},
			// Wire a WRONG org key — decrypt is attempted and fails (AES-GCM
			// auth tag mismatch), exercising injection.go:74-82. Distinct from
			// wireOrg=false which skips BEFORE decrypt (injection.go:176).
			wrongOrgKey: deterministicKey(0xFF),
		})
	require.Equal(t, 0, materializeExit, "wrong-KEK binding must be skipped, not crash; stderr=%s", materializeStderr)
	cfg := readAgentConfig(t, agentCfgPath)
	assert.NotContains(t, cfg.Provider, "anthropic", "undecryptable binding must not materialize")
	assert.Contains(t, cfg.Provider, "opencode", "decryptable admin binding must still materialize")
}

// TestE2E_BootstrapMaterialize_PartialFailure_DoesNotBlockGoodProviders
// verifies that one undecryptable binding (org, unwired) does not prevent a
// later binding (admin) for a DIFFERENT provider from materializing. This is
// the injection.go "don't set seen — allow fallback" contract.
//
// Originally this test paired the undecryptable org binding with a
// USER binding, but this worklog made user-DEK content non-deliverable
// at boot. Pairing two server-KEK bindings is the correct test of the
// fallback semantics post-Epic 35.
func TestE2E_BootstrapMaterialize_PartialFailure_DoesNotBlockGoodProviders(t *testing.T) {
	agentCfgPath, _, materializeExit, _, materializeStderr :=
		runBootstrapMaterializeE2EWith(t, bootstrapE2EConfig{
			bindings: []e2eProviderBinding{
				{ownerType: "org", ownerID: "org-1", provider: "anthropic", apiKey: "sk-org"},
				{ownerType: "admin", ownerID: "_platform", provider: "opencode", apiKey: "sk-admin"},
			},
			wireOrg: boolPtr(false), // org binding will fail to decrypt
		})
	require.Equal(t, 0, materializeExit, "partial decrypt failure must not fail materialize; stderr=%s", materializeStderr)
	cfg := readAgentConfig(t, agentCfgPath)
	assert.NotContains(t, cfg.Provider, "anthropic", "failed org binding must be absent")
	assert.Contains(t, cfg.Provider, "opencode", "unrelated admin provider must still materialize despite the org failure")
}

// --- Password reset erasure guarantee ---
//
// database.go:790 and password_reset.go:290 both state "no future
// materialization can resurrect them". The boot-path e2e above proves the
// POSITIVE path (providers materialize). These tests prove the NEGATIVE path:
// after a purge (the reset flow's PurgeUserSecrets step), a subsequent boot
// must produce NO resurrected secrets. This is the literal claim, previously
// untested at the materialization layer.

// TestE2E_PasswordReset_PurgeThenBoot_NoResurrect proves that after the
// credential bindings are purged (simulating password_reset.go:294
// PurgeUserSecrets), a pod boot produces NO providers. A bug that caches
// decrypted secrets, or that falls back to stale bindings, would resurrect
// them and fail this test.
func TestE2E_PasswordReset_PurgeThenBoot_NoResurrect(t *testing.T) {
	dir := t.TempDir()
	userDEK := deterministicKey(0x03)
	keySvc := secrets.NewKeyService(e2eKeyStore{}, &e2eDEKCache{dek: userDEK})

	// Seed user + org bindings (the "before reset" state).
	credBindings := []secrets.CredentialBinding{
		encryptE2EBinding(t, e2eProviderBinding{ownerType: "user", ownerID: "user-e2e", provider: "openai", apiKey: "sk-user"}),
		encryptE2EBinding(t, e2eProviderBinding{ownerType: "org", ownerID: "org-1", provider: "anthropic", apiKey: "sk-org"}),
	}
	store := &e2eSecretStore{bindings: credBindings}
	svc := secrets.NewSecretService(keySvc, store)
	adminProv, err := secrets.NewStaticKeyProvider(deterministicKey(0x01))
	require.NoError(t, err)
	orgProv, err := secrets.NewStaticKeyProvider(deterministicKey(0x02))
	require.NoError(t, err)
	svc.SetAdminProvider(adminProv)
	svc.SetOrgProvider(orgProv)

	// Stand up the bootstrap server.
	gin.SetMode(gin.TestMode)
	router := gin.New()
	const testNS = "llmsafespace-reset"
	const wsID = "ws-reset"
	reviewer := &staticTokenReviewer{username: "system:serviceaccount:" + testNS + ":workspace-" + wsID}
	lookup := &wsMetaLookup{ws: &types.WorkspaceMetadata{ID: wsID, UserID: "user-e2e"}}
	h := NewPodBootstrapHandler(reviewer, svc, lookup, nil, testNS)
	router.POST("/internal/v1/pod-bootstrap", h.Bootstrap)
	apiSrv := httptest.NewServer(router)
	t.Cleanup(apiSrv.Close)

	bin := buildAgentd(t)
	runOneBoot := func(label string) map[string]json.RawMessage {
		t.Helper()
		secretsOut := filepath.Join(dir, label+"-secrets.json")
		tokenFile := filepath.Join(dir, "token")
		require.NoError(t, os.WriteFile(tokenFile, []byte("e2e-token"), 0o600))
		exit, _, stderr := runAgentd(t, bin,
			[]string{"bootstrap", "--workspace-id", wsID, "--api-url", apiSrv.URL,
				"--token-file", tokenFile, "--out", secretsOut}, nil)
		require.Equal(t, 0, exit, "bootstrap failed (%s); stderr=%s", label, stderr)

		agentCfg := filepath.Join(dir, label+"-agent-config.json")
		mExit, _, mStderr := runAgentd(t, bin,
			[]string{"materialize", "--from", secretsOut},
			map[string]string{
				"LLMSAFESPACES_AGENT_CONFIG_PATH": agentCfg,
				"LLMSAFESPACES_SECRETS_BASE_DIR":  filepath.Join(dir, label+"-secrets"),
				"LLMSAFESPACES_SSH_DIR":           filepath.Join(dir, label+"-ssh"),
				"LLMSAFESPACES_SECRETS_ENV_PATH":  filepath.Join(dir, label+"-env"),
				"LLMSAFESPACES_GIT_CREDS_PATH":    filepath.Join(dir, label+"-git"),
				"LLMSAFESPACES_RELOAD_CACHE_PATH": filepath.Join(dir, label+"-last-reload-secrets.json"),
				"HOME":                            dir,
			})
		require.Equal(t, 0, mExit, "materialize failed (%s); stderr=%s", label, mStderr)
		raw, rErr := os.ReadFile(agentCfg)
		require.NoError(t, rErr, "(%s) agent-config.json missing", label)
		var cfg struct {
			Provider map[string]json.RawMessage `json:"provider"`
		}
		require.NoError(t, json.Unmarshal(raw, &cfg), "(%s) invalid agent-config.json", label)
		return cfg.Provider
	}

	// Before reset: org provider materializes at boot (server-KEK).
	// User provider does NOT materialize at boot on this test's degrade
	// path: no JWTSessionStore is wired into the KeyService, so
	// GetDEKForUser returns ErrDEKUnavailable and
	// InjectSecretsForPodBootstrap falls back to InjectSessionlessSecrets
	// (user-DEK bindings audited-and-skipped). The design-0045 happy path
	// is exercised separately by
	// TestE2E_BootstrapMaterialize_UserDEKUnwrappable_MaterializesUserProvider.
	// The "no resurrection" property this test guards remains meaningful
	// for the reload path — see TestE2E_PurgedUserCredsDoNotResurrectInReload
	// below.
	before := runOneBoot("before")
	assert.NotContains(t, before, "openai",
		"before reset: user provider must NOT materialize on the offline-degrade path (no jwt_sessions wired)")
	assert.Contains(t, before, "anthropic", "before reset: org provider must be present")

	// RESET: purge the user-owned bindings (PurgeUserSecrets deletes
	// provider_credentials where owner_type='user'). Org bindings belong to
	// the org, not the user, so they survive a user-targeted purge.
	store.bindings = filterOutOwnerType(store.bindings, "user")

	// After reset: the user provider must NOT resurrect. The org provider
	// (not owned by the reset user) correctly survives.
	after := runOneBoot("after")
	assert.NotContains(t, after, "openai",
		"after reset: purged user provider must NOT resurrect (the database.go:790 guarantee)")
	assert.Contains(t, after, "anthropic",
		"after reset: org provider (not owned by the reset user) must survive")
}

// TestE2E_PasswordReset_FullPurgeThenBoot_NoProviders proves the full-purge
// case: when ALL bindings are purged (e.g. org-wide reset), a boot produces
// zero providers and the pod still boots cleanly.
func TestE2E_PasswordReset_FullPurgeThenBoot_NoProviders(t *testing.T) {
	dir := t.TempDir()
	userDEK := deterministicKey(0x03)
	keySvc := secrets.NewKeyService(e2eKeyStore{}, &e2eDEKCache{dek: userDEK})

	store := &e2eSecretStore{bindings: []secrets.CredentialBinding{
		encryptE2EBinding(t, e2eProviderBinding{ownerType: "user", ownerID: "user-e2e", provider: "openai", apiKey: "sk-user"}),
	}}
	svc := secrets.NewSecretService(keySvc, store)
	orgProv, err := secrets.NewStaticKeyProvider(deterministicKey(0x02))
	require.NoError(t, err)
	svc.SetOrgProvider(orgProv)

	gin.SetMode(gin.TestMode)
	router := gin.New()
	const testNS = "llmsafespace-fullpurge"
	const wsID = "ws-fullpurge"
	reviewer := &staticTokenReviewer{username: "system:serviceaccount:" + testNS + ":workspace-" + wsID}
	lookup := &wsMetaLookup{ws: &types.WorkspaceMetadata{ID: wsID, UserID: "user-e2e"}}
	h := NewPodBootstrapHandler(reviewer, svc, lookup, nil, testNS)
	router.POST("/internal/v1/pod-bootstrap", h.Bootstrap)
	apiSrv := httptest.NewServer(router)
	t.Cleanup(apiSrv.Close)

	// RESET: purge everything.
	store.bindings = nil

	bin := buildAgentd(t)
	secretsOut := filepath.Join(dir, "secrets.json")
	tokenFile := filepath.Join(dir, "token")
	require.NoError(t, os.WriteFile(tokenFile, []byte("e2e-token"), 0o600))
	exit, _, stderr := runAgentd(t, bin,
		[]string{"bootstrap", "--workspace-id", wsID, "--api-url", apiSrv.URL,
			"--token-file", tokenFile, "--out", secretsOut}, nil)
	require.Equal(t, 0, exit, "bootstrap must succeed on empty store (graceful); stderr=%s", stderr)

	agentCfg := filepath.Join(dir, "agent-config.json")
	mExit, _, mStderr := runAgentd(t, bin,
		[]string{"materialize", "--from", secretsOut},
		map[string]string{
			"LLMSAFESPACES_AGENT_CONFIG_PATH": agentCfg,
			"LLMSAFESPACES_SECRETS_BASE_DIR":  filepath.Join(dir, "secrets"),
			"LLMSAFESPACES_SSH_DIR":           filepath.Join(dir, "ssh"),
			"LLMSAFESPACES_SECRETS_ENV_PATH":  filepath.Join(dir, "env"),
			"LLMSAFESPACES_GIT_CREDS_PATH":    filepath.Join(dir, "git"),
			"LLMSAFESPACES_RELOAD_CACHE_PATH": filepath.Join(dir, "last-reload-secrets.json"),
			"HOME":                            dir,
		})
	require.Equal(t, 0, mExit, "materialize must succeed on empty secrets; stderr=%s", mStderr)

	// The agent-config.json may be absent on the zero-provider path (valid).
	// If present, it must contain NO providers.
	if raw, err := os.ReadFile(agentCfg); err == nil {
		var cfg struct {
			Provider map[string]json.RawMessage `json:"provider"`
		}
		if json.Unmarshal(raw, &cfg) == nil {
			assert.Empty(t, cfg.Provider, "no provider must resurrect after full purge")
		}
	}
}

// filterOutOwnerType returns bindings with the given ownerType removed
// (simulates PurgeUserSecrets which deletes owner_type='user' rows).
func filterOutOwnerType(bindings []secrets.CredentialBinding, ownerType string) []secrets.CredentialBinding {
	var out []secrets.CredentialBinding
	for _, b := range bindings {
		if b.OwnerType != ownerType {
			out = append(out, b)
		}
	}
	return out
}
