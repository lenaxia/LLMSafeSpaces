// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

// reload_credentials_e2e_test.go exercises the LIVE credential reload path:
//
//	provider_credentials (DB row)
//	  → SecretService.InjectSecrets   (decrypt + dedupe by provider)
//	  → reloadSecretsHandler                        (HTTP POST /v1/reload-secrets)
//	  → Materializer.Materialize → FlushProviders   (stages + writes agent-config.json)
//	  → AgentConfigWriter.rebuild                   (atomic write)
//	  → agent-config.json                           (the file opencode reads)
//
// This is the reload-path twin of pod_bootstrap_e2e_test.go. The API-side
// tests (secrets_integration_test.go, secrets_llmprovider_test.go) wire a real
// SecretService but mock agentd; the agentd-side reload tests hand-craft JSON
// and bypass SecretService entirely. No test previously joined the two halves,
// so an org-provider regression on the reload path (identical class to the
// boot-path bug) would ship undetected.

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	opencode "github.com/lenaxia/llmsafespaces/pkg/agent/opencode"
	"github.com/lenaxia/llmsafespaces/pkg/secrets"
)

// --- minimal stubs for a real SecretService (same shape as the bootstrap e2e
// test in api/internal/handlers, kept local because that's a different package) ---

type reloadE2ECredStore struct{ bindings []secrets.CredentialBinding }

func (s *reloadE2ECredStore) GetWorkspaceCredentials(_ context.Context, _ string) ([]secrets.CredentialBinding, error) {
	return s.bindings, nil
}
func (s *reloadE2ECredStore) UpsertFreeTierCredential(_ context.Context, _ []byte) error {
	return nil
}
func (s *reloadE2ECredStore) SeedWorkspaceCredentials(_ context.Context, _, _ string, _ *string) error {
	return nil
}
func (s *reloadE2ECredStore) BindCredentialToAllUserWorkspaces(_ context.Context, _, _ string) error {
	return nil
}
func (s *reloadE2ECredStore) HasUserProviderCredential(_ context.Context, _, _ string) (bool, error) {
	return false, nil
}
func (s *reloadE2ECredStore) GetWorkspaceMCPServers(_ context.Context, _ string) ([]secrets.MCPServerBindingRow, error) {
	return nil, nil
}

// reloadE2EStore satisfies secrets.SecretStore (only the injection-touched
// methods return real values; the rest panic to surface drift loudly).
type reloadE2EStore struct{ cred *reloadE2ECredStore }

func (s *reloadE2EStore) GetWorkspaceCredentials(ctx context.Context, ws string) ([]secrets.CredentialBinding, error) {
	return s.cred.GetWorkspaceCredentials(ctx, ws)
}
func (s *reloadE2EStore) UpsertFreeTierCredential(ctx context.Context, b []byte) error {
	return s.cred.UpsertFreeTierCredential(ctx, b)
}
func (s *reloadE2EStore) SeedWorkspaceCredentials(ctx context.Context, w, u string, o *string) error {
	return s.cred.SeedWorkspaceCredentials(ctx, w, u, o)
}
func (s *reloadE2EStore) BindCredentialToAllUserWorkspaces(ctx context.Context, c, u string) error {
	return s.cred.BindCredentialToAllUserWorkspaces(ctx, c, u)
}
func (s *reloadE2EStore) HasUserProviderCredential(ctx context.Context, u, p string) (bool, error) {
	return s.cred.HasUserProviderCredential(ctx, u, p)
}
func (s *reloadE2EStore) GetWorkspaceMCPServers(ctx context.Context, ws string) ([]secrets.MCPServerBindingRow, error) {
	return s.cred.GetWorkspaceMCPServers(ctx, ws)
}
func (s *reloadE2EStore) GetBindings(_ context.Context, _ string) ([]*secrets.UserSecret, error) {
	return nil, nil
}
func (s *reloadE2EStore) LogAudit(_ context.Context, _ *secrets.AuditEntry) error { return nil }
func (s *reloadE2EStore) CreateSecret(_ context.Context, _ *secrets.UserSecret) error {
	panic("unexpected CreateSecret")
}
func (s *reloadE2EStore) GetSecret(_ context.Context, _, _ string) (*secrets.UserSecret, error) {
	panic("unexpected GetSecret")
}
func (s *reloadE2EStore) GetSecretByName(_ context.Context, _, _ string) (*secrets.UserSecret, error) {
	panic("unexpected GetSecretByName")
}
func (s *reloadE2EStore) ListSecrets(_ context.Context, _ string) ([]*secrets.UserSecret, error) {
	panic("unexpected ListSecrets")
}
func (s *reloadE2EStore) ListGlobalDefaultSecrets(_ context.Context, _ string) ([]*secrets.UserSecret, error) {
	panic("unexpected ListGlobalDefaultSecrets")
}
func (s *reloadE2EStore) UpdateSecret(_ context.Context, _ *secrets.UserSecret) error {
	panic("unexpected UpdateSecret")
}
func (s *reloadE2EStore) DeleteSecret(_ context.Context, _, _ string) error {
	panic("unexpected DeleteSecret")
}
func (s *reloadE2EStore) ReEncryptUserSecrets(_ context.Context, _ string, _ int,
	_ func([]byte) ([]byte, error), _ func(context.Context) error) error {
	panic("unexpected ReEncryptUserSecrets")
}
func (s *reloadE2EStore) SetBindings(_ context.Context, _ string, _ []string) error {
	panic("unexpected SetBindings")
}
func (s *reloadE2EStore) AddBindings(_ context.Context, _ string, _ []string) error {
	panic("unexpected AddBindings")
}
func (s *reloadE2EStore) GetBindingsForSecret(_ context.Context, _ string) ([]string, error) {
	panic("unexpected GetBindingsForSecret")
}
func (s *reloadE2EStore) QueryAudit(_ context.Context, _ string, _ secrets.AuditQuery) ([]*secrets.AuditEntry, error) {
	panic("unexpected QueryAudit")
}
func (s *reloadE2EStore) CurrentRevision(context.Context, string) (int64, string, bool, error) {
	return 0, "", false, nil
}
func (s *reloadE2EStore) EnsureRevision(context.Context, string, string) (int64, error) {
	return 1, nil
}

// reloadE2EKeyStore holds a single user_keys record so the builder's
// server-side DEK unwrap (GetDEKServerSide) can recover the fixture's
// user DEK — the one-builder replacement for the deleted session-based
// InjectSecrets call shape.
type reloadE2EKeyStore struct{ record *secrets.UserKeyRecord }

func (s *reloadE2EKeyStore) GetUserKey(_ context.Context, _ string) (*secrets.UserKeyRecord, error) {
	if s.record == nil {
		return nil, nil
	}
	cp := *s.record
	return &cp, nil
}
func (s *reloadE2EKeyStore) CreateUserKey(_ context.Context, _ *secrets.UserKeyRecord) error {
	return nil
}
func (s *reloadE2EKeyStore) UpdateWrappedDEK(_ context.Context, _ string, _, _ []byte, _ int) error {
	return nil
}

type reloadE2EDEKCache struct{ dek []byte }

func (c *reloadE2EDEKCache) CacheDEK(_ context.Context, _ string, _ []byte, _ time.Duration) error {
	return nil
}
func (c *reloadE2EDEKCache) GetDEK(_ context.Context, _ string) ([]byte, error) { return c.dek, nil }
func (c *reloadE2EDEKCache) EvictDEK(_ context.Context, _ string) error         { return nil }

// reloadBinding describes one provider credential to seed for the reload test.
type reloadBinding struct {
	ownerType string
	provider  string
	apiKey    string
}

// deterministicKeyReload returns a 32-byte key where every byte == seed.
func deterministicKeyReload(seed byte) []byte {
	k := make([]byte, 32)
	for i := range k {
		k[i] = seed
	}
	return k
}

// buildReloadSecretService constructs a real SecretService seeded with the
// given bindings, each encrypted under a distinct per-ownerType KEK. The
// returned booleans control which RootKeyProviders are wired (flip false to
// simulate the org-provider-not-wired regression).
func buildReloadSecretService(t *testing.T, bindings []reloadBinding, wireAdmin, wireOrg bool) *secrets.SecretService {
	t.Helper()
	userDEK := deterministicKeyReload(0x03)
	masterProv, err := secrets.NewStaticKeyProvider(deterministicKeyReload(0x10))
	require.NoError(t, err)
	wrappedUserDEK, err := masterProv.Encrypt(context.Background(), userDEK)
	require.NoError(t, err)
	keySvc := secrets.NewKeyService(&reloadE2EKeyStore{record: &secrets.UserKeyRecord{
		UserID: "user-e2e", KeyVersion: 1, WrappedDEK: wrappedUserDEK, DEKSource: "server_kek",
	}}, &reloadE2EDEKCache{dek: userDEK})
	keySvc.SetAPIKeyStore(nil, masterProv)

	credBindings := make([]secrets.CredentialBinding, 0, len(bindings))
	for _, b := range bindings {
		var kek []byte
		switch b.ownerType {
		case "admin":
			kek = deterministicKeyReload(0x01)
		case "org":
			kek = deterministicKeyReload(0x02)
		case "user":
			kek = deterministicKeyReload(0x03)
		default:
			t.Fatalf("unknown ownerType %q", b.ownerType)
		}
		plaintext, err := json.Marshal(secrets.LLMProviderData{Kind: b.provider, Slug: b.provider, APIKey: b.apiKey})
		require.NoError(t, err)
		cipher, err := secrets.EncryptSecret(kek, plaintext)
		require.NoError(t, err)
		credBindings = append(credBindings, secrets.CredentialBinding{
			ID: "cred-" + b.ownerType + "-" + b.provider, OwnerType: b.ownerType,
			Kind: b.provider, Slug: b.provider, Ciphertext: cipher, Version: 1, SourceType: "auto",
		})
	}
	store := &reloadE2EStore{cred: &reloadE2ECredStore{bindings: credBindings}}
	svc := secrets.NewSecretService(keySvc, store)
	if wireAdmin {
		p, err := secrets.NewStaticKeyProvider(deterministicKeyReload(0x01))
		require.NoError(t, err)
		svc.SetAdminProvider(p)
	}
	if wireOrg {
		p, err := secrets.NewStaticKeyProvider(deterministicKeyReload(0x02))
		require.NoError(t, err)
		svc.SetOrgProvider(p)
	}
	return svc
}

// runReloadE2E is the shared harness: builds the real SecretService, calls
// the one builder to produce the decrypted secrets JSON (legacy wire
// shape), then POSTs it to the real reloadSecretsHandler and returns the
// materialized agent-config.json path + HTTP status + body.
func runReloadE2E(t *testing.T, bindings []reloadBinding, wireAdmin, wireOrg bool) (agentCfgPath string, httpStatus int, httpBody string) {
	t.Helper()
	dir := t.TempDir()
	agentCfgPath = filepath.Join(dir, "agent-config.json")
	cfg := materializeConfig{
		secretsBaseDir:  filepath.Join(dir, "secrets"),
		sshDir:          filepath.Join(dir, ".ssh"),
		agentConfigPath: agentCfgPath,
		secretsEnvPath:  filepath.Join(dir, "env"),
		gitCredsPath:    filepath.Join(dir, ".git-credentials"),
		home:            dir,
	}

	svc := buildReloadSecretService(t, bindings, wireAdmin, wireOrg)

	// Real decrypt: the seam that broke for org credentials.
	batch, degrade, err := svc.BuildWorkspaceBatch(context.Background(), "user-e2e", "ws-e2e")
	require.NoError(t, err)
	require.Nil(t, degrade, "all fixture credentials must decrypt")
	secretsJSON := secrets.LegacyBatchJSON(*batch)
	require.NotEmpty(t, secretsJSON, "the builder must return non-empty JSON for non-empty bindings")

	// Real reloadSecretsHandler: materialize → enrich → flush → writer rebuild.
	writer := opencode.NewConfigWriter(agentCfgPath)
	deps := reloadSecretsDeps{OpencodePassword: "test-pw", AgentConfigWriter: writer}
	req := httptest.NewRequest(http.MethodPost, "/v1/reload-secrets", bytes.NewReader(secretsJSON))
	req.Header.Set("Authorization", "Basic "+basicAuth("test-pw"))
	rec := httptest.NewRecorder()
	reloadSecretsHandler(cfg, deps)(rec, req)
	return agentCfgPath, rec.Code, rec.Body.String()
}

// readReloadAgentConfig parses the reload-written agent-config.json.
func readReloadAgentConfig(t *testing.T, path string) struct {
	Schema   string                     `json:"$schema"`
	Provider map[string]json.RawMessage `json:"provider"`
} {
	t.Helper()
	raw, err := os.ReadFile(path)
	require.NoError(t, err, "agent-config.json must exist after reload at %s", path)
	var cfg struct {
		Schema   string                     `json:"$schema"`
		Provider map[string]json.RawMessage `json:"provider"`
	}
	require.NoError(t, json.Unmarshal(raw, &cfg), "agent-config.json must be valid JSON; got=%s", raw)
	return cfg
}

// TestE2E_ReloadSecrets_AllOwnerTypesMaterialized is the reload-path twin of
// TestE2E_BootstrapMaterialize_AllOwnerTypesMaterialized. It seeds org + admin
// + user credentials, runs InjectSecrets → reloadSecretsHandler,
// and asserts all three providers appear in agent-config.json. A regression
// where org credentials stop surviving the reload decrypt path fails here.
func TestE2E_ReloadSecrets_AllOwnerTypesMaterialized(t *testing.T) {
	agentCfgPath, status, body := runReloadE2E(t,
		[]reloadBinding{
			{ownerType: "org", provider: "anthropic", apiKey: "sk-org"},
			{ownerType: "admin", provider: "opencode", apiKey: "sk-admin"},
			{ownerType: "user", provider: "openai", apiKey: "sk-user"},
		},
		true, true,
	)
	require.Equal(t, http.StatusOK, status, "reload must succeed; body=%s", body)

	cfg := readReloadAgentConfig(t, agentCfgPath)
	assert.Contains(t, cfg.Provider, "anthropic", "org provider must survive reload decrypt path")
	assert.Contains(t, cfg.Provider, "opencode", "admin provider must survive reload decrypt path")
	assert.Contains(t, cfg.Provider, "openai", "user provider must survive reload decrypt path")

	var anthropicEntry struct {
		Options struct {
			APIKey string `json:"apiKey"`
		} `json:"options"`
	}
	require.NoError(t, json.Unmarshal(cfg.Provider["anthropic"], &anthropicEntry))
	assert.Equal(t, "sk-org", anthropicEntry.Options.APIKey,
		"org apiKey must round-trip through InjectSecrets → reload → agent-config.json")
}

// TestE2E_ReloadSecrets_OrgOnly pins that a sole org-scoped credential
// materializes on the reload path (the exact regression class).
func TestE2E_ReloadSecrets_OrgOnly(t *testing.T) {
	agentCfgPath, status, body := runReloadE2E(t,
		[]reloadBinding{{ownerType: "org", provider: "anthropic", apiKey: "sk-org"}},
		true, true,
	)
	require.Equal(t, http.StatusOK, status, "reload must succeed; body=%s", body)
	cfg := readReloadAgentConfig(t, agentCfgPath)
	assert.Contains(t, cfg.Provider, "anthropic",
		"the sole org provider must survive reload even when no other ownerType is present")
}

// --- Unhappy paths ---

// TestE2E_ReloadSecrets_OrgProviderUnwired_OrgAbsentButReloadSucceeds is the
// direct regression guard. When SetOrgProvider is not called, the org
// credential is skipped at decrypt time (injection.go:176) and the reload
// still succeeds with the remaining providers. This test fails if that skip
// ever becomes a hard error or (worse) the org provider appears via a
// wrong-key fallback.
func TestE2E_ReloadSecrets_OrgProviderUnwired_OrgAbsentButReloadSucceeds(t *testing.T) {
	agentCfgPath, status, body := runReloadE2E(t,
		[]reloadBinding{
			{ownerType: "org", provider: "anthropic", apiKey: "sk-org"},
			{ownerType: "admin", provider: "opencode", apiKey: "sk-admin"},
		},
		true, false, // org provider NOT wired — the regression
	)
	require.Equal(t, http.StatusOK, status,
		"reload must succeed even when org provider unwired (graceful degradation); body=%s", body)
	cfg := readReloadAgentConfig(t, agentCfgPath)
	assert.NotContains(t, cfg.Provider, "anthropic",
		"org provider must NOT appear when SetOrgProvider was not called (would indicate wrong-key fallback)")
	assert.Contains(t, cfg.Provider, "opencode", "admin provider must still materialize")
}

// TestE2E_ReloadSecrets_EmptyBindings_Returns200 verifies the graceful
// degradation contract for the reload path: an empty decrypted batch must
// return 200, not 500. This is the "user has no credentials yet" case.
func TestE2E_ReloadSecrets_EmptyBindings_Returns200(t *testing.T) {
	// The builder on empty bindings returns "[]".
	svc := buildReloadSecretService(t, nil, true, true)
	batch, _, err := svc.BuildWorkspaceBatch(context.Background(), "user-e2e", "ws-e2e")
	require.NoError(t, err)
	secretsJSON := secrets.LegacyBatchJSON(*batch)

	dir := t.TempDir()
	agentCfgPath := filepath.Join(dir, "agent-config.json")
	cfg := materializeConfig{
		secretsBaseDir:  filepath.Join(dir, "secrets"),
		sshDir:          filepath.Join(dir, ".ssh"),
		agentConfigPath: agentCfgPath,
		secretsEnvPath:  filepath.Join(dir, "env"),
		gitCredsPath:    filepath.Join(dir, ".git-credentials"),
		home:            dir,
	}
	writer := opencode.NewConfigWriter(agentCfgPath)
	req := httptest.NewRequest(http.MethodPost, "/v1/reload-secrets", bytes.NewReader(secretsJSON))
	req.Header.Set("Authorization", "Basic "+basicAuth("test-pw"))
	rec := httptest.NewRecorder()
	reloadSecretsHandler(cfg, reloadSecretsDeps{OpencodePassword: "test-pw", AgentConfigWriter: writer})(rec, req)
	assert.Equal(t, http.StatusOK, rec.Code, "empty batch must return 200, not error; body=%s", rec.Body.String())
}

// TestE2E_ReloadSecrets_BadJSON_Returns400 verifies malformed input is
// rejected with 400, not a silent no-op or a 500. (Pins the contract that
// the API push path validates before applying.)
func TestE2E_ReloadSecrets_BadJSON_Returns400(t *testing.T) {
	dir := t.TempDir()
	cfg := materializeConfig{agentConfigPath: filepath.Join(dir, "agent-config.json")}
	req := httptest.NewRequest(http.MethodPost, "/v1/reload-secrets", strings.NewReader("not json"))
	req.Header.Set("Authorization", "Basic "+basicAuth("test-pw"))
	rec := httptest.NewRecorder()
	reloadSecretsHandler(cfg, reloadSecretsDeps{OpencodePassword: "test-pw", AgentConfigWriter: opencode.NewConfigWriter(cfg.agentConfigPath)})(rec, req)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}
