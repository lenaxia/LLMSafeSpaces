// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package app

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"strings"
	"sync"

	"golang.org/x/crypto/bcrypt"

	"github.com/lenaxia/llmsafespaces/api/internal/config"
	"github.com/lenaxia/llmsafespaces/api/internal/interfaces"
	"github.com/lenaxia/llmsafespaces/api/internal/logger"
	"github.com/lenaxia/llmsafespaces/api/internal/services/agentpush"
	"github.com/lenaxia/llmsafespaces/api/internal/services/metrics"
	v1 "github.com/lenaxia/llmsafespaces/pkg/apis/llmsafespaces/v1"
	pkginterfaces "github.com/lenaxia/llmsafespaces/pkg/interfaces"
	"github.com/lenaxia/llmsafespaces/pkg/kubernetes"
	"github.com/lenaxia/llmsafespaces/pkg/secrets"
	"github.com/lenaxia/llmsafespaces/pkg/types"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// dbKeyStoreAdapter is an in-memory KeyStore used by app-level wiring
// tests so they don't need a Postgres instance. It is NOT used in
// production: app.New refuses to start if pgxpool initialisation fails.
//
// Concurrency: guarded by a mutex so future tests running with
// t.Parallel() do not race; correctness is otherwise irrelevant
// because every call within a single goroutine reads/writes the same
// map atomically under the lock.
type dbKeyStoreAdapter struct {
	mu      sync.Mutex
	memKeys map[string]*secrets.UserKeyRecord
}

func (a *dbKeyStoreAdapter) GetUserKey(_ context.Context, userID string) (*secrets.UserKeyRecord, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.memKeys == nil {
		return nil, nil
	}
	r, ok := a.memKeys[userID]
	if !ok {
		return nil, nil
	}
	cp := *r
	return &cp, nil
}

func (a *dbKeyStoreAdapter) CreateUserKey(_ context.Context, record *secrets.UserKeyRecord) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.memKeys == nil {
		a.memKeys = make(map[string]*secrets.UserKeyRecord)
	}
	cp := *record
	a.memKeys[record.UserID] = &cp
	return nil
}

func (a *dbKeyStoreAdapter) UpdateWrappedDEK(_ context.Context, userID string, wrappedDEK []byte, salt []byte, keyVersion int) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.memKeys == nil {
		return nil
	}
	r, ok := a.memKeys[userID]
	if !ok {
		return nil
	}
	r.WrappedDEK = wrappedDEK
	r.Salt = salt
	r.KeyVersion = keyVersion
	return nil
}

func (a *dbKeyStoreAdapter) UpdateWrappedDEKRecovery(_ context.Context, userID string, wrappedDEKRecovery []byte, recoverySalt []byte) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.memKeys == nil {
		return nil
	}
	r, ok := a.memKeys[userID]
	if !ok {
		return nil
	}
	r.WrappedDEKRecovery = wrappedDEKRecovery
	r.RecoverySalt = recoverySalt
	return nil
}

// dbSecretStoreAdapter is an in-memory SecretStore used by app-level
// wiring tests. NOT used in production: app.New refuses to start if
// pgxpool initialisation fails, which would otherwise be the only
// caller of this adapter.
//
// Concurrency: guarded by a mutex so future tests running with
// t.Parallel() do not race. The audit slice is bounded so a long test
// run does not grow without bound.
type dbSecretStoreAdapter struct {
	mu       sync.Mutex
	secrets  map[string]*secrets.UserSecret
	bindings map[string][]string
	audit    []*secrets.AuditEntry
}

// maxAdapterAuditEntries caps the in-memory audit slice. Production
// uses pg-backed audit storage so this only affects test-suite memory
// footprint; without the cap a long test run accumulates audit entries
// without bound.
const maxAdapterAuditEntries = 4096

// init lazy-initializes maps. Caller must already hold a.mu.
func (a *dbSecretStoreAdapter) init() {
	if a.secrets == nil {
		a.secrets = make(map[string]*secrets.UserSecret)
		a.bindings = make(map[string][]string)
	}
}

func (a *dbSecretStoreAdapter) CreateSecret(_ context.Context, secret *secrets.UserSecret) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.init()
	for _, s := range a.secrets {
		if s.UserID == secret.UserID && s.Name == secret.Name {
			return &duplicateErr{secret.Name}
		}
	}
	if secret.ID == "" {
		secret.ID = generateID()
	}
	cp := *secret
	a.secrets[secret.ID] = &cp
	return nil
}

func (a *dbSecretStoreAdapter) GetSecret(_ context.Context, userID, secretID string) (*secrets.UserSecret, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.init()
	s, ok := a.secrets[secretID]
	if !ok || s.UserID != userID {
		return nil, nil
	}
	cp := *s
	return &cp, nil
}

func (a *dbSecretStoreAdapter) GetSecretByName(_ context.Context, userID, name string) (*secrets.UserSecret, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.init()
	for _, s := range a.secrets {
		if s.UserID == userID && s.Name == name {
			cp := *s
			return &cp, nil
		}
	}
	return nil, nil
}

func (a *dbSecretStoreAdapter) ListSecrets(_ context.Context, userID string) ([]*secrets.UserSecret, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.init()
	var result []*secrets.UserSecret
	for _, s := range a.secrets {
		if s.UserID == userID {
			cp := *s
			result = append(result, &cp)
		}
	}
	return result, nil
}

func (a *dbSecretStoreAdapter) ListGlobalDefaultSecrets(_ context.Context, userID string) ([]*secrets.UserSecret, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.init()
	var result []*secrets.UserSecret
	for _, s := range a.secrets {
		if s.UserID == userID && s.GlobalDefault {
			cp := *s
			result = append(result, &cp)
		}
	}
	return result, nil
}

func (a *dbSecretStoreAdapter) UpdateSecret(_ context.Context, secret *secrets.UserSecret) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.init()
	if _, ok := a.secrets[secret.ID]; !ok {
		return &notFoundErr{secret.ID}
	}
	cp := *secret
	a.secrets[secret.ID] = &cp
	return nil
}

func (a *dbSecretStoreAdapter) ReEncryptUserSecrets(ctx context.Context, userID string, newKeyVersion int, transform func([]byte) ([]byte, error), commit func(context.Context) error) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.init()
	updates := make(map[string][]byte)
	for id, s := range a.secrets {
		if s.UserID != userID {
			continue
		}
		newCT, err := transform(s.Ciphertext)
		if err != nil {
			return err
		}
		updates[id] = newCT
	}
	if commit != nil {
		// Drop the lock so commit's downstream callbacks can re-enter
		// the adapter without deadlocking. The mutation phase below
		// re-acquires and re-validates each id (a concurrent
		// DeleteSecret during the unlocked window could have removed
		// rows from a.secrets — without re-validation we'd nil-deref
		// on the next line).
		a.mu.Unlock()
		err := commit(ctx)
		a.mu.Lock()
		if err != nil {
			return err
		}
	}
	for id, newCT := range updates {
		s, ok := a.secrets[id]
		if !ok || s == nil {
			// Concurrent DeleteSecret removed this row during the
			// commit-callback window. Skip silently — the secret no
			// longer exists, so nothing to re-encrypt.
			continue
		}
		s.Ciphertext = newCT
		s.KeyVersion = newKeyVersion
	}
	return nil
}

func (a *dbSecretStoreAdapter) DeleteSecret(_ context.Context, userID, secretID string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.init()
	s, ok := a.secrets[secretID]
	if !ok || s.UserID != userID {
		return &notFoundErr{secretID}
	}
	delete(a.secrets, secretID)
	for wsID, sids := range a.bindings {
		var filtered []string
		for _, sid := range sids {
			if sid != secretID {
				filtered = append(filtered, sid)
			}
		}
		a.bindings[wsID] = filtered
	}
	return nil
}

func (a *dbSecretStoreAdapter) SetBindings(_ context.Context, workspaceID string, secretIDs []string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.init()
	a.bindings[workspaceID] = secretIDs
	return nil
}

func (a *dbSecretStoreAdapter) AddBindings(_ context.Context, workspaceID string, secretIDs []string) error {
	if len(secretIDs) == 0 {
		return nil
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	a.init()
	existing := a.bindings[workspaceID]
	seen := make(map[string]struct{}, len(existing)+len(secretIDs))
	for _, id := range existing {
		seen[id] = struct{}{}
	}
	for _, id := range secretIDs {
		if _, dup := seen[id]; dup {
			continue
		}
		seen[id] = struct{}{}
		existing = append(existing, id)
	}
	a.bindings[workspaceID] = existing
	return nil
}

func (a *dbSecretStoreAdapter) GetBindings(_ context.Context, workspaceID string) ([]*secrets.UserSecret, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.init()
	sids := a.bindings[workspaceID]
	var result []*secrets.UserSecret
	for _, sid := range sids {
		if s, ok := a.secrets[sid]; ok {
			cp := *s
			result = append(result, &cp)
		}
	}
	return result, nil
}

func (a *dbSecretStoreAdapter) GetBindingsForSecret(_ context.Context, secretID string) ([]string, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.init()
	var ws []string
	for wsID, sids := range a.bindings {
		for _, sid := range sids {
			if sid == secretID {
				ws = append(ws, wsID)
			}
		}
	}
	return ws, nil
}

func (a *dbSecretStoreAdapter) LogAudit(_ context.Context, entry *secrets.AuditEntry) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.audit = append(a.audit, entry)
	// Bound the slice — drop oldest when we exceed the cap. The cap
	// is large enough for any realistic test run but small enough to
	// keep memory bounded in long-running suites.
	if len(a.audit) > maxAdapterAuditEntries {
		drop := len(a.audit) - maxAdapterAuditEntries
		a.audit = a.audit[drop:]
	}
	return nil
}

func (a *dbSecretStoreAdapter) QueryAudit(_ context.Context, userID string, _ secrets.AuditQuery) ([]*secrets.AuditEntry, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	var result []*secrets.AuditEntry
	for _, e := range a.audit {
		if e.UserID == userID {
			result = append(result, e)
		}
	}
	return result, nil
}

// workspaceOwnerVerifierAdapter and its local OrgMembershipChecker interface
// were removed in design 0041 Story 2. WorkspaceAccessMiddleware is now the
// single ownership gate for every /:id workspace route (including bindings,
// env, and reload-secrets), and the user provider-credential bind/unbind
// routes — which live outside /:id — are wired directly against
// workspace.Service.ResolveWorkspace + CheckOwnership in app.New. The old
// adapter lacked the D5 creator-membership re-check that CheckOwnership
// carries, so consolidating onto the canonical path also closes that gap.

type duplicateErr struct{ name string }

func (e *duplicateErr) Error() string { return "duplicate secret: " + e.name }
func (e *duplicateErr) Unwrap() error { return secrets.ErrDuplicateSecret }

type notFoundErr struct{ id string }

func (e *notFoundErr) Error() string { return "not found: " + e.id }
func (e *notFoundErr) Unwrap() error { return secrets.ErrSecretNotFound }

func generateID() string {
	b := make([]byte, 16)
	// crypto/rand.Read is documented to never fail on Linux/macOS in
	// practice, but if entropy is somehow unavailable we'd produce
	// id collisions. Panic rather than silently degrading.
	if _, err := rand.Read(b); err != nil {
		panic(fmt.Sprintf("generateID: crypto/rand.Read failed: %v", err))
	}
	return fmt.Sprintf("%x", b)
}

type apiKeyStoreAdapter struct {
	db interfaces.DatabaseService
}

func (a *apiKeyStoreAdapter) ListAPIKeysWithDecrypt(ctx context.Context, userID string) ([]*secrets.APIKeyRecord, error) {
	keys, err := a.db.ListAPIKeysWithDecrypt(ctx, userID)
	if err != nil {
		return nil, err
	}
	var records []*secrets.APIKeyRecord
	for _, k := range keys {
		records = append(records, &secrets.APIKeyRecord{
			ID:            k.ID,
			WrappedDEK:    k.WrappedDEK,
			KekSalt:       k.KekSalt,
			KeyCiphertext: k.KeyCiphertext,
			DecryptAccess: k.DecryptAccess,
		})
	}
	return records, nil
}

func (a *apiKeyStoreAdapter) UpdateAPIKeyDEK(ctx context.Context, keyID string, wrappedDEK, kekSalt []byte, synced bool) error {
	return a.db.UpdateAPIKeyDEK(ctx, keyID, wrappedDEK, kekSalt, synced)
}

// bcryptPasswordUpdater implements handlers.PasswordHashUpdater using the DatabaseService.
type bcryptPasswordUpdater struct {
	db interfaces.DatabaseService
}

func (u *bcryptPasswordUpdater) UpdatePasswordHash(ctx context.Context, userID string, newPassword []byte) error {
	hash, err := bcrypt.GenerateFromPassword(newPassword, 12)
	if err != nil {
		return err
	}
	hashStr := string(hash)
	return u.db.UpdateUser(ctx, userID, types.UserUpdates{PasswordHash: &hashStr})
}

// newPurposeProvider constructs a per-purpose RootKeyProvider from the master
// KEK via deriveServerKey (US-50.2). Each purpose string ("provider-credentials",
// "org-credentials") produces a cryptographically independent key via HKDF. Used
// at boot to wire handlers and services with provider-bound encrypt/decrypt.
// Returns nil when deriveServerKey yields no key (master secret not configured).
func newPurposeProvider(purpose string) secrets.RootKeyProvider {
	key := deriveServerKey(purpose)
	if key == nil {
		return nil
	}
	p, err := secrets.NewStaticKeyProvider(key)
	if err != nil {
		return nil
	}
	return p
}

func newRootKeyProvider(cfg *config.Config, log *logger.Logger) secrets.RootKeyProvider {
	provider := cfg.Security.RootKeyProvider
	switch provider {
	case "sealed":
		if cfg.Security.SealedKeyPath == "" || cfg.Security.PassphrasePath == "" {
			log.Error("sealed root key provider requires both sealedKeyPath and passphrasePath", nil)
			return nil
		}
		p, err := secrets.NewSealedKeyProvider(cfg.Security.SealedKeyPath, cfg.Security.PassphrasePath)
		if err != nil {
			log.Error("failed to initialize sealed root key provider", err)
			return nil
		}
		return p
	case "static", "":
		mk := dekMasterKey()
		if mk == nil {
			return nil
		}
		p, err := secrets.NewStaticKeyProvider(mk)
		if err != nil {
			log.Error("failed to initialize static root key provider", err)
			return nil
		}
		if !cfg.Security.SkipMasterKeyWarning {
			log.Warn("using static root key provider — intended for development only; use the sealed provider in production (see pkg/secrets/README.md)")
		}
		return p
	default:
		log.Error("unknown root key provider", nil, "provider", provider)
		return nil
	}
}

// Env vars holding the master KEK. The file-path var (US-50.1) is preferred so
// the KEK is never exposed via /proc/1/environ; the value vars are retained for
// one release as a deprecated fallback for non-Helm deployments.
const (
	masterSecretFileEnv   = "LLMSAFESPACES_MASTER_SECRET_FILE"
	masterSecretValueEnv  = "LLMSAFESPACES_MASTER_SECRET"
	masterSecretLegacyEnv = "LLMSAFESPACES_DEK_MASTER_KEY"
)

// dekMasterKey derives the DEK cache encryption key from the master secret.
// Uses HKDF with purpose-specific context so each derived key is independent.
func dekMasterKey() []byte {
	return deriveServerKey("dek-cache")
}

// decodeMasterRaw turns a raw secret string into key material bytes. A valid
// even-length hex string is decoded to bytes; anything else is used as raw
// bytes (Helm randAlphaNum, base64, etc.). Returns nil only for an empty
// string. It does NOT enforce a minimum length so that callers (the file
// loader + validateMasterSecret) can distinguish "present but too short"
// from "missing" for precise diagnostics. Side-effect-free.
func decodeMasterRaw(raw string) []byte {
	if raw == "" {
		return nil
	}
	if decoded, err := hex.DecodeString(raw); err == nil {
		return decoded // valid hex path
	}
	return []byte(raw) // raw bytes path
}

// decodeMasterMaterial turns a raw secret string into key material suitable
// for HKDF/AES-256-GCM: decodeMasterRaw plus the 32-byte minimum. Returns nil
// if empty or shorter than 32 bytes. Side-effect-free.
func decodeMasterMaterial(raw string) []byte {
	m := decodeMasterRaw(raw)
	if len(m) < 32 { // AES-256-GCM requires 32 bytes minimum
		return nil
	}
	return m
}

// loadMasterSecretMaterials reads the master KEK from the file mount(s)
// referenced by LLMSAFESPACES_MASTER_SECRET_FILE (US-50.1). The value is a
// colon-separated list of paths so a rotation window (US-50.4/US-50.5) can
// mount old + new key files at once; the entries are returned in file order
// with the LAST file treated as the highest (active) version.
//
// Each file holds a single value — the key material as either hex (>=64 chars)
// or raw bytes (>=32 bytes), whitespace-trimmed. Files that are missing or
// unreadable are skipped. A file that is PRESENT but decodes to fewer than 32
// bytes (including empty/whitespace-only content) is INCLUDED as a short or
// nil entry so validateMasterSecret can report the precise diagnostic and the
// system fails closed — never silently continuing with an earlier key during a
// rotation window where the active file was mis-mounted.
//
// Returns nil when the env var is unset. Side-effect-free (no logging):
// validateMasterSecret in app.go is responsible for startup diagnostics.
func loadMasterSecretMaterials() [][]byte {
	pathList := os.Getenv(masterSecretFileEnv)
	if pathList == "" {
		return nil
	}
	var out [][]byte
	for _, p := range strings.Split(pathList, ":") {
		if p == "" {
			continue
		}
		data, err := os.ReadFile(p) //nolint:gosec // G703: path is operator-configured (Helm-mounted Secret volume via LLMSAFESPACES_MASTER_SECRET_FILE), not user input
		if err != nil {
			continue // missing/unreadable file: skip (validateMasterSecret reports the empty result)
		}
		// Always append the decoded material, even when nil (empty file): presence
		// must be preserved so an empty/short active file fails closed at validation
		// rather than being silently dropped (which would fall back to an earlier key).
		out = append(out, decodeMasterRaw(strings.TrimSpace(string(data))))
	}
	return out
}

// activeMasterSecret returns the master KEK material to derive purpose keys
// from. The file mount (US-50.1) is preferred; the active key is the highest
// version (last file). If no file material is available it falls back to the
// legacy value env vars (LLMSAFESPACES_MASTER_SECRET then LLMSAFESPACES_DEK_MASTER_KEY)
// for one release. Returns nil when no usable source is configured.
// Side-effect-free.
func activeMasterSecret() []byte {
	if materials := loadMasterSecretMaterials(); len(materials) > 0 {
		active := materials[len(materials)-1]
		if len(active) >= 32 {
			return active
		}
		// Active file material is too short — a misconfiguration validateMasterSecret
		// reports at boot; return nil here so callers fail closed rather than derive
		// from a weak key.
		return nil
	}
	if raw := os.Getenv(masterSecretValueEnv); raw != "" {
		return decodeMasterMaterial(raw)
	}
	if raw := os.Getenv(masterSecretLegacyEnv); raw != "" {
		return decodeMasterMaterial(raw)
	}
	return nil
}

// deriveServerKey derives a 32-byte purpose-scoped key from the master KEK
// using HKDF-SHA256. Each purpose string produces an independent key.
//
// Source preference (US-50.1): file mount (LLMSAFESPACES_MASTER_SECRET_FILE)
// first, then the legacy value env vars as a one-release fallback.
//
// Returns nil if no usable source is configured or the material decodes below
// the 32-byte AES-256-GCM minimum.
//
// This function is intentionally side-effect-free (no logging). It is passed
// by reference as secrets.AdminKeyDeriver; callers that need diagnostics must
// inspect the sources independently (see validateMasterSecret in app.go).
func deriveServerKey(purpose string) []byte {
	master := activeMasterSecret()
	if master == nil {
		return nil
	}
	key, err := secrets.DeriveKEKFromKey(master, []byte("llmsafespaces-server"), purpose)
	if err != nil {
		return nil
	}
	return key
}

// k8sWorkspaceGetterAdapter adapts the K8s client to the handlers.WorkspaceGetter interface.
type k8sWorkspaceGetterAdapter struct {
	client    *kubernetes.Client
	namespace string
}

func (a *k8sWorkspaceGetterAdapter) GetWorkspace(ctx context.Context, id string) (*v1.Workspace, error) {
	v1Client, err := a.client.LlmsafespacesV1()
	if err != nil {
		return nil, fmt.Errorf("initialize LLMSafespacesV1 client: %w", err)
	}
	return v1Client.Workspaces(a.namespace).Get(ctx, id, metav1.GetOptions{})
}

// workspaceCRDGetter is the minimal interface needed by secretsPodIPResolver.
// Defined here (rather than reusing handlers.WorkspaceGetter) to keep the
// dependency direction one-way: app depends on handlers, not the other way.
type workspaceCRDGetter interface {
	GetWorkspace(ctx context.Context, id string) (*v1.Workspace, error)
}

// dbOwnerLookup is the minimal interface needed to verify workspace ownership
// for the secrets reload path. We require the database lookup (rather than
// trusting the CRD's spec.owner) because the API treats PostgreSQL as the
// authority for ownership at the API layer.
type dbOwnerLookup interface {
	GetWorkspace(ctx context.Context, workspaceID string) (*types.WorkspaceMetadata, error)
}

// secretsPodIPResolver resolves the pod IP for a workspace owned by a given
// user. Returns ("", nil) if the workspace is not owned by the caller, is
// not Active, has no PodIP yet, or the apiserver/DB is transiently
// unavailable. The handler treats every empty result as errNoRunningPod
// (409 Conflict) so the response shape is uniform across "you don't own
// this workspace" / "doesn't exist" / "DB is having a bad day" — this is
// deliberate: we do not want to leak workspace existence cross-user via
// status-code differences.
//
// Transient-failure errors (DB or apiserver blips) are still observable
// to operators because the resolver logs them at Warn before returning
// empty. Without that log a Postgres outage would produce silent 409s
// across the fleet with no signal in the API logs (Finding 2 in worklog
// 0094 follow-up audit).
//
// This adapter exists because handlers.SecretsHandler.SetPodIPResolver was
// never called from app.New — see Bug 1 in worklog 0085. Without it the
// reload-secrets endpoint returned 503 unconditionally and SetBindings'
// auto-push silently failed.
type secretsPodIPResolver struct {
	crd    workspaceCRDGetter
	db     dbOwnerLookup
	logger pkginterfaces.LoggerInterface
}

func newSecretsPodIPResolver(crd workspaceCRDGetter, db dbOwnerLookup, logger pkginterfaces.LoggerInterface) *secretsPodIPResolver {
	return &secretsPodIPResolver{crd: crd, db: db, logger: logger}
}

func (r *secretsPodIPResolver) GetWorkspacePodIP(ctx context.Context, userID, workspaceID string) (string, error) {
	if userID == "" || workspaceID == "" {
		return "", nil
	}

	// Ownership check: if the middleware has already validated ownership for
	// this workspace (all HTTP routes on idGroup), trust its decision — it
	// includes the D5 creator-membership and D6 org-admin checks that the
	// legacy meta.UserID comparison below lacks. For non-HTTP callers (none
	// today, but defensively), fall through to the DB-based check.
	if cm, ok := types.WorkspaceMetaFromCtx(ctx); !ok || cm == nil || cm.ID != workspaceID {
		if r.db != nil {
			meta, err := r.db.GetWorkspace(ctx, workspaceID)
			if err != nil {
				if r.logger != nil {
					r.logger.Warn("secretsPodIPResolver: DB lookup failed; downgrading to no-running-pod",
						"workspaceID", workspaceID, "error", err.Error())
				}
				return "", nil
			}
			if meta == nil || meta.UserID != userID {
				return "", nil
			}
		}
	}

	ws, err := r.crd.GetWorkspace(ctx, workspaceID)
	if err != nil {
		// Workspace CR missing or apiserver error — caller treats as
		// "no running pod"; do not surface raw K8s errors upstream.
		// Logged at Debug because CR-not-found is the common case for
		// freshly-created or terminating workspaces.
		if r.logger != nil {
			r.logger.Debug("secretsPodIPResolver: CRD lookup failed",
				"workspaceID", workspaceID, "error", err.Error())
		}
		return "", nil
	}
	if ws == nil || ws.Status.Phase != v1.WorkspacePhaseActive {
		return "", nil
	}
	return ws.Status.PodIP, nil
}

// credentialSeeder is the narrow interface for free-tier credential seeding.
type credentialSeeder interface {
	UpsertFreeTierCredential(ctx context.Context, ciphertext []byte) error
	BackfillFreeTierBindings(ctx context.Context) (int64, error)
}

// ensureFreeTierCredential upserts the platform free-tier opencode credential
// at API startup and backfills bindings for existing workspaces. Idempotent.
func ensureFreeTierCredential(ctx context.Context, seeder credentialSeeder, provider secrets.RootKeyProvider, logger pkginterfaces.LoggerInterface) error {
	if provider == nil {
		return fmt.Errorf("admin RootKeyProvider not configured; skipping free-tier credential seed")
	}
	// Epic 55 plaintext shape: kind + slug instead of provider. The slug
	// MUST match the value the DAL inserts into provider_credentials.slug
	// (see UpsertFreeTierCredential's INSERT in pkg/secrets/pg_credential_store.go)
	// so the column-level identity and the encrypted-blob identity agree.
	// LLMProviderData.Validate() rejects the credential at materialize time
	// otherwise, with `kind is required` — see TestEnsureFreeTierCredential_PlaintextHasKindAndSlug.
	plaintext := []byte(`{"kind":"opencode","slug":"opencode-free-tier","apiKey":"public"}`)
	ciphertext, err := provider.Encrypt(ctx, plaintext)
	if err != nil {
		return fmt.Errorf("encrypt free-tier key: %w", err)
	}
	if err := seeder.UpsertFreeTierCredential(ctx, ciphertext); err != nil {
		return fmt.Errorf("upsert free-tier credential: %w", err)
	}
	// Backfill existing workspaces that lack the free-tier binding.
	backfilled, err := seeder.BackfillFreeTierBindings(ctx)
	if err != nil {
		logger.Warn("free-tier backfill failed (non-fatal)", "error", err.Error())
	} else if backfilled > 0 {
		logger.Info("free-tier backfill complete", "workspacesBackfilled", backfilled)
	}
	return nil
}

// wsAgentPusherAdapter adapts *agentpush.Service to the narrow
// workspace.SecretPusher interface. This is the dependency-inversion
// seam between the workspace service (which declares the interface it
// needs) and the concrete pusher (which lives in the agentpush package,
// unaware of any consumer). Without this adapter the workspace package
// would have to import agentpush directly, creating a wider dependency
// than the SOLID DIP allows.
type wsAgentPusherAdapter struct {
	pusher *agentpush.Service
}

// Push satisfies workspace.SecretPusher by delegating and dropping the
// Result (the workspace-side auto-push flow doesn't inspect it — the
// count is recorded via the metric hook + structured logs).
func (a *wsAgentPusherAdapter) Push(ctx context.Context, userID, workspaceID string) error {
	_, err := a.pusher.Push(ctx, userID, workspaceID)
	return err
}

// recordAutoPushOutcome is the process-wide metrics-emitter used by both
// the workspace service (pod-recreation path) and the agentpush service
// (all push flows). Package-level func so it can be passed as a plain
// callback without a Service handle.
//
// If the metrics registration is disabled or misconfigured, the counter
// is a no-op — recordAutoPushOutcome silently succeeds. Do not add
// error-return here; callers won't handle it and the metric is best-
// effort observability, not a correctness gate.
func recordAutoPushOutcome(outcome string) {
	metrics.RecordSecretAutoPush(outcome)
}
