// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package handlers

// pod_bootstrap_conditional_test.go — the conditional-pull 304 path
// proven against a REAL *secrets.SecretService whose every decrypt
// dependency (DEK key store, admin/org root providers) fails the test
// the moment it is touched. If the 304 decision ever costs a decrypt,
// these panicking seams make the test go red.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lenaxia/llmsafespaces/pkg/secrets"
	"github.com/lenaxia/llmsafespaces/pkg/types"
)

// bombRootProvider detonates the test if the conditional path decrypts.
type bombRootProvider struct{ t *testing.T }

func (b *bombRootProvider) Encrypt(context.Context, []byte) ([]byte, error) {
	b.t.Fatalf("the 304 decision must never encrypt through a root provider")
	return nil, nil
}

func (b *bombRootProvider) Decrypt(context.Context, []byte) ([]byte, error) {
	b.t.Fatalf("the 304 decision must never decrypt through a root provider")
	return nil, nil
}

// bombKeyStore detonates the test if the conditional path touches the
// DEK tier.
type bombKeyStore struct{ t *testing.T }

func (b *bombKeyStore) GetUserKey(context.Context, string) (*secrets.UserKeyRecord, error) {
	b.t.Fatalf("the 304 decision must never touch user_keys")
	return nil, nil
}
func (b *bombKeyStore) CreateUserKey(context.Context, *secrets.UserKeyRecord) error {
	b.t.Fatalf("the 304 decision must never touch user_keys")
	return nil
}
func (b *bombKeyStore) UpdateWrappedDEK(context.Context, string, []byte, []byte, int) error {
	b.t.Fatalf("the 304 decision must never touch user_keys")
	return nil
}

type bombDEKCache struct{ t *testing.T }

func (b *bombDEKCache) CacheDEK(context.Context, string, []byte, time.Duration) error {
	b.t.Fatalf("the 304 decision must never touch the DEK cache")
	return nil
}
func (b *bombDEKCache) GetDEK(context.Context, string) ([]byte, error) {
	b.t.Fatalf("the 304 decision must never touch the DEK cache")
	return nil, nil
}
func (b *bombDEKCache) EvictDEK(context.Context, string) error {
	b.t.Fatalf("the 304 decision must never touch the DEK cache")
	return nil
}

// conditionalRevStore is a stateful RevisionStore: the row the ETag seq
// is read from.
type conditionalRevStore struct {
	seq  int64
	hash string
	ok   bool
}

func (s *conditionalRevStore) CurrentRevision(context.Context, string) (int64, string, bool, error) {
	return s.seq, s.hash, s.ok, nil
}
func (s *conditionalRevStore) EnsureRevision(_ context.Context, _, manifestHash string) (int64, error) {
	if s.ok && s.hash == manifestHash {
		return s.seq, nil
	}
	s.seq++
	s.hash = manifestHash
	s.ok = true
	return s.seq, nil
}

// conditionalStore pairs the e2e bindings store (from
// pod_bootstrap_e2e_test.go) with a stateful revision row. The row is a
// named field (not an embed): e2eSecretStore also declares the revision
// methods, and a same-depth double promotion would silently drop
// conditionalStore from the RevisionStore method set. The bindings are
// mutex-guarded: the test mutates them between pulls while httptest
// request goroutines from the previous pull may still be unwinding.
type conditionalStore struct {
	e2eSecretStore
	mu  sync.Mutex
	rev *conditionalRevStore
}

func (s *conditionalStore) GetWorkspaceCredentials(ctx context.Context, workspaceID string) ([]secrets.CredentialBinding, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.e2eSecretStore.GetWorkspaceCredentials(ctx, workspaceID)
}

func (s *conditionalStore) setBindings(bindings []secrets.CredentialBinding) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.bindings = bindings
}

func (s *conditionalStore) CurrentRevision(ctx context.Context, workspaceID string) (int64, string, bool, error) {
	return s.rev.CurrentRevision(ctx, workspaceID)
}

func (s *conditionalStore) EnsureRevision(ctx context.Context, workspaceID, manifestHash string) (int64, error) {
	return s.rev.EnsureRevision(ctx, workspaceID, manifestHash)
}

// TestPodBootstrap_304WithRealService_ZeroDecrypts: end-to-end over the
// real SecretService — a v2 client whose manifest hash matches gets a
// 304 stamped from the stored revision row while every decrypt
// dependency is rigged to fail the test on contact.
func TestPodBootstrap_304WithRealService_ZeroDecrypts(t *testing.T) {
	store := &conditionalStore{e2eSecretStore: e2eSecretStore{}, rev: &conditionalRevStore{}}
	keySvc := secrets.NewKeyService(&bombKeyStore{t: t}, &bombDEKCache{t: t})
	svc := secrets.NewSecretService(keySvc, store)
	svc.SetAdminProvider(&bombRootProvider{t: t})
	svc.SetOrgProvider(&bombRootProvider{t: t})

	ctx := context.Background()
	hash, err := svc.ManifestFor(ctx, "user-e2e", "ws-e2e")
	require.NoError(t, err)

	// Seed the revision row exactly as a prior 200 pull would have left it.
	_, err = store.rev.EnsureRevision(ctx, "ws-e2e", hash)
	require.NoError(t, err)

	gin.SetMode(gin.TestMode)
	r := gin.New()
	h := NewPodBootstrapHandler(
		&staticTokenReviewer{username: "system:serviceaccount:llmsafespace:workspace-ws-e2e"},
		svc,
		&wsMetaLookup{ws: &types.WorkspaceMetadata{ID: "ws-e2e", UserID: "user-e2e"}},
		nil,
		testBootstrapNamespace,
	)
	r.POST("/internal/v1/pod-bootstrap", h.Bootstrap)

	w := doBootstrap(t, r, "token",
		`{"workspaceID":"ws-e2e","contractVersion":2,"clientManifestHash":"`+hash+`"}`)

	require.Equal(t, http.StatusNotModified, w.Code)
	assert.Empty(t, w.Body.String())
	assert.Equal(t, `"1:`+hash+`"`, w.Header().Get("ETag"))
}

// TestPodBootstrap_v2EnvelopeWithRealService_RoundTrip: the same real
// service on the changed-hash path serves an envelope whose revision
// matches the manifest tier — the client's next conditional pull with
// that manifestHash must then 304.
func TestPodBootstrap_v2EnvelopeWithRealService_RoundTrip(t *testing.T) {
	store := &conditionalStore{e2eSecretStore: e2eSecretStore{}, rev: &conditionalRevStore{}}
	keySvc := secrets.NewKeyService(&bombKeyStore{t: t}, &bombDEKCache{t: t})
	svc := secrets.NewSecretService(keySvc, store)
	svc.SetAdminProvider(&bombRootProvider{t: t})
	svc.SetOrgProvider(&bombRootProvider{t: t})

	// No user-DEK rows are bound, so the build completes without touching
	// the bombed decrypt seams... it cannot: an admin binding decrypts
	// through the admin provider. Give the store NO bindings so the batch
	// is the quiet-empty envelope.
	store.bindings = nil

	gin.SetMode(gin.TestMode)
	r := gin.New()
	h := NewPodBootstrapHandler(
		&staticTokenReviewer{username: "system:serviceaccount:llmsafespace:workspace-ws-e2e"},
		svc,
		&wsMetaLookup{ws: &types.WorkspaceMetadata{ID: "ws-e2e", UserID: "user-e2e"}},
		nil,
		testBootstrapNamespace,
	)
	r.POST("/internal/v1/pod-bootstrap", h.Bootstrap)

	first := doBootstrap(t, r, "token", `{"workspaceID":"ws-e2e","contractVersion":2}`)
	require.Equal(t, http.StatusOK, first.Code)

	var resp bootstrapAPIResponse
	require.NoError(t, json.Unmarshal(first.Body.Bytes(), &resp))
	var envelope struct {
		Entries  []json.RawMessage     `json:"entries"`
		Revision secrets.BatchRevision `json:"revision"`
	}
	require.NoError(t, json.Unmarshal(resp.Secrets, &envelope))
	assert.Empty(t, envelope.Entries)
	require.NotEmpty(t, envelope.Revision.ManifestHash)

	second := doBootstrap(t, r, "token",
		`{"workspaceID":"ws-e2e","contractVersion":2,"clientManifestHash":"`+envelope.Revision.ManifestHash+`"}`)
	require.Equal(t, http.StatusNotModified, second.Code)
	assert.Equal(t, `"`+itoa64(envelope.Revision.Seq)+`:`+envelope.Revision.ManifestHash+`"`,
		second.Header().Get("ETag"))
}

func itoa64(v int64) string {
	return fmt.Sprintf("%d", v)
}

// TestE2E_ConditionalBootstrap_304ThenRotate_200: the full stack — real
// SecretService + handler over HTTP, the REAL workspace-agentd bootstrap
// and materialize subprocesses. First pull lands the envelope; the
// unchanged conditional re-pull 304s and keeps the file byte-identical;
// a credential rotation (version bump) flips the re-pull back to 200
// with a fresh revision; materialize writes the rev anchor.
func TestE2E_ConditionalBootstrap_304ThenRotate_200(t *testing.T) {
	bin := buildAgentd(t)
	dir := t.TempDir()

	adminProv, err := secrets.NewStaticKeyProvider(deterministicKey(0x01))
	require.NoError(t, err)
	orgProv, err := secrets.NewStaticKeyProvider(deterministicKey(0x02))
	require.NoError(t, err)

	binding := e2eProviderBinding{ownerType: "org", ownerID: "org-1", provider: "anthropic", apiKey: "sk-org"}
	firstRow := encryptE2EBinding(t, binding)
	firstRow.ID = "cred-e2e-org"
	firstRow.Version = 1
	store := &conditionalStore{
		e2eSecretStore: e2eSecretStore{bindings: []secrets.CredentialBinding{firstRow}},
		rev:            &conditionalRevStore{},
	}
	keySvc := newE2EKeyService(t, deterministicKey(0x03))
	svc := secrets.NewSecretService(keySvc, store)
	svc.SetAdminProvider(adminProv)
	svc.SetOrgProvider(orgProv)

	gin.SetMode(gin.TestMode)
	router := gin.New()
	const testNS = "llmsafespace-e2e"
	const wsID = "ws-e2e"
	h := NewPodBootstrapHandler(
		&staticTokenReviewer{username: "system:serviceaccount:" + testNS + ":workspace-" + wsID},
		svc,
		&wsMetaLookup{ws: &types.WorkspaceMetadata{ID: wsID, UserID: "user-e2e"}},
		nil,
		testNS,
	)
	router.POST("/internal/v1/pod-bootstrap", h.Bootstrap)
	apiSrv := httptest.NewServer(router)
	t.Cleanup(apiSrv.Close)

	secretsOut := filepath.Join(dir, "secrets.json")
	tokenFile := filepath.Join(dir, "token")
	require.NoError(t, os.WriteFile(tokenFile, []byte("e2e-token"), 0o600))

	runBoot := func() (int, string) {
		code, _, stderr := runAgentd(t, bin,
			[]string{"bootstrap", "--workspace-id", wsID, "--api-url", apiSrv.URL,
				"--token-file", tokenFile, "--out", secretsOut},
			nil)
		return code, stderr
	}

	// First pull: 200 envelope with revision seq 1.
	code, stderr := runBoot()
	require.Equal(t, 0, code, "first bootstrap; stderr=%s", stderr)
	firstBytes, err := os.ReadFile(secretsOut)
	require.NoError(t, err)
	var first struct {
		Entries  []json.RawMessage     `json:"entries"`
		Revision secrets.BatchRevision `json:"revision"`
	}
	require.NoError(t, json.Unmarshal(firstBytes, &first))
	require.NotEmpty(t, first.Entries)
	require.EqualValues(t, 1, first.Revision.Seq)

	// Unchanged conditional re-pull: 304 keeps the file byte-identical.
	code, stderr = runBoot()
	require.Equal(t, 0, code, "conditional bootstrap; stderr=%s", stderr)
	require.Contains(t, stderr, "304", "the skip must be logged; stderr=%s", stderr)
	keptBytes, err := os.ReadFile(secretsOut)
	require.NoError(t, err)
	require.Equal(t, string(firstBytes), string(keptBytes),
		"a 304 re-pull must keep the envelope byte-identical")

	// Materialize the envelope; the rev anchor lands next to secrets-env.
	envPath := filepath.Join(dir, "env")
	mCode, _, mStderr := runAgentd(t, bin,
		[]string{"materialize", "--from", secretsOut},
		map[string]string{
			"LLMSAFESPACES_AGENT_CONFIG_PATH": filepath.Join(dir, "agent-config.json"),
			"LLMSAFESPACES_SECRETS_BASE_DIR":  filepath.Join(dir, "secrets"),
			"LLMSAFESPACES_SSH_DIR":           filepath.Join(dir, ".ssh"),
			"LLMSAFESPACES_SECRETS_ENV_PATH":  envPath,
			"LLMSAFESPACES_GIT_CREDS_PATH":    filepath.Join(dir, ".git-credentials"),
			"LLMSAFESPACES_RELOAD_CACHE_PATH": filepath.Join(dir, "last-reload-secrets.json"),
			"HOME":                            dir,
		})
	require.Equal(t, 0, mCode, "materialize; stderr=%s", mStderr)

	anchorBytes, err := os.ReadFile(envPath + ".rev")
	require.NoError(t, err, "the rev anchor must land next to secrets-env")
	var anchor struct {
		Rev     string `json:"rev"`
		Applied int64  `json:"appliedSeq"`
	}
	require.NoError(t, json.Unmarshal(anchorBytes, &anchor))
	assert.Equal(t, "1:"+first.Revision.ManifestHash, anchor.Rev)
	assert.EqualValues(t, 1, anchor.Applied)

	// Rotate the credential (value change ⇒ row version bump, exactly
	// what the store's update path does): the conditional re-pull gets a
	// 200 with a NEW envelope at the next seq.
	rotated := binding
	rotated.apiKey = "sk-rotated"
	rotatedRow := encryptE2EBinding(t, rotated)
	rotatedRow.ID = "cred-e2e-org"
	rotatedRow.Version = 2
	store.setBindings([]secrets.CredentialBinding{rotatedRow})
	code, stderr = runBoot()
	require.Equal(t, 0, code, "post-rotation bootstrap; stderr=%s", stderr)
	require.NotContains(t, stderr, "304", "a changed manifest must not 304")
	rotatedBytes, err := os.ReadFile(secretsOut)
	require.NoError(t, err)
	var second struct {
		Entries  []json.RawMessage     `json:"entries"`
		Revision secrets.BatchRevision `json:"revision"`
	}
	require.NoError(t, json.Unmarshal(rotatedBytes, &second))
	assert.EqualValues(t, 2, second.Revision.Seq, "the changed manifest mints the next seq")
	assert.NotEqual(t, first.Revision.ManifestHash, second.Revision.ManifestHash)
	assert.Contains(t, string(rotatedBytes), "sk-rotated")
}
