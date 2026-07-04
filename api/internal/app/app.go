// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package app

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/gin-gonic/gin"
	"github.com/go-redis/redis/v8"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/lenaxia/llmsafespaces/api/internal/config"
	"github.com/lenaxia/llmsafespaces/api/internal/handlers"
	"github.com/lenaxia/llmsafespaces/api/internal/logger"
	"github.com/lenaxia/llmsafespaces/api/internal/server"
	"github.com/lenaxia/llmsafespaces/api/internal/services"
	"github.com/lenaxia/llmsafespaces/api/internal/services/agentpush"
	"github.com/lenaxia/llmsafespaces/api/internal/services/auth"
	"github.com/lenaxia/llmsafespaces/api/internal/services/cache"
	"github.com/lenaxia/llmsafespaces/api/internal/services/database"
	emailsvc "github.com/lenaxia/llmsafespaces/api/internal/services/email"
	"github.com/lenaxia/llmsafespaces/api/internal/services/health"
	"github.com/lenaxia/llmsafespaces/api/internal/services/metering"
	"github.com/lenaxia/llmsafespaces/api/internal/services/metrics"
	"github.com/lenaxia/llmsafespaces/api/internal/services/msgqueue"
	"github.com/lenaxia/llmsafespaces/api/internal/services/policy"
	"github.com/lenaxia/llmsafespaces/api/internal/services/prompt"
	"github.com/lenaxia/llmsafespaces/api/internal/services/role"
	"github.com/lenaxia/llmsafespaces/api/internal/services/secretautopush"
	"github.com/lenaxia/llmsafespaces/api/internal/services/sessionindex"
	"github.com/lenaxia/llmsafespaces/api/internal/services/sso"
	"github.com/lenaxia/llmsafespaces/api/internal/services/workspace"
	"github.com/lenaxia/llmsafespaces/api/internal/services/wsstate"
	agentoc "github.com/lenaxia/llmsafespaces/pkg/agent/opencode"
	"github.com/lenaxia/llmsafespaces/pkg/agentd"
	"github.com/lenaxia/llmsafespaces/pkg/billing"
	emailpkg "github.com/lenaxia/llmsafespaces/pkg/email"
	"github.com/lenaxia/llmsafespaces/pkg/kubernetes"
	"github.com/lenaxia/llmsafespaces/pkg/secrets"
	"github.com/lenaxia/llmsafespaces/pkg/settings"
	"github.com/lenaxia/llmsafespaces/pkg/types"
)

// Compile-time check that *WorkspaceClient satisfies the caller-shaped
// ModelClient interface (H2-a). If WorkspaceClient.ListModels or
// .PatchConfig signature drifts, this fails at build time instead of at
// the SetAgentClient call site.
var _ handlers.ModelClient = (*agentoc.WorkspaceClient)(nil)

type App struct {
	config             *config.Config
	logger             *logger.Logger
	router             *gin.Engine
	server             *http.Server
	k8sClient          *kubernetes.Client
	services           *services.Services
	proxyHandler       *handlers.ProxyHandler
	agentReloadHandler *handlers.AgentReloadHandler
	bulkReloadHandler  *handlers.BulkReloadHandler
	sessionIndexSvc    *sessionindex.Service
	instanceSettings   *settings.InstanceService
	userSettings       *settings.UserService
	asyncAudit         *secrets.AsyncAuditLogger // nil if pgxpool path not used
	secretsPool        *pgxpool.Pool             // pgx pool for secrets store; closed on shutdown
	dekCacheClient     *redis.Client             // redis client for DEK cache; closed on shutdown
	healthChecker      *health.Checker           // periodic dependency probe; nil only in degraded test setups
	pendingOrgCleaner  *handlers.PendingOrgCleaner
	jwtSessionJanitor  *secrets.JWTSessionJanitor // Epic 56: prunes expired jwt_sessions rows
	invitationsHandler *handlers.InvitationsHandler
	emailService       *emailsvc.Service
	emailHandler       *handlers.EmailHandler
	emailVerifyHandler *handlers.EmailVerifyHandler
	shutdownCh         chan struct{}
	ctx                context.Context
	cancel             context.CancelFunc
}

// newEmailMailer resolves the configured email provider into an
// emailpkg.EmailProvider. Extracted from New to keep New under the funlen
// limit (worklog 0410). SES validation fails fast at boot.
func newEmailMailer(cfg *config.Config) (emailpkg.EmailProvider, error) {
	switch strings.ToLower(cfg.Email.Provider) {
	case "ses":
		if cfg.Email.FromAddress == "" || cfg.Email.BaseURL == "" {
			return nil, fmt.Errorf("email provider 'ses' requires fromAddress and baseUrl to be set")
		}
		awsCfg, err := awsconfig.LoadDefaultConfig(context.Background(),
			awsconfig.WithRegion(cfg.Email.SESRegion))
		if err != nil {
			return nil, fmt.Errorf("init aws config for ses: %w", err)
		}
		return emailpkg.NewSESProvider(awsCfg, cfg.Email.FromAddress), nil
	default:
		return &emailpkg.NoopProvider{}, nil
	}
}

//nolint:funlen,gocyclo // Sequential service initialization; decomposition would require a 20-field return struct with no clarity gain
func New(cfg *config.Config, log *logger.Logger) (*App, error) {
	ctx, cancel := context.WithCancel(context.Background())

	// validateMasterSecret is the very first check — before any infrastructure
	// is constructed. This ensures startup fails fast with a clear error rather
	// than a misleading K8s/DB error, and makes the enforcement unit-testable
	// without a live cluster (see TestApp_New_FailsWithoutMasterSecret).
	if err := validateMasterSecret(log); err != nil {
		cancel()
		return nil, err
	}

	k8sClient, err := kubernetes.New(&cfg.Kubernetes, log)
	if err != nil {
		cancel()
		return nil, fmt.Errorf("failed to create Kubernetes client: %w", err)
	}

	svc, err := services.New(cfg, log, k8sClient)
	if err != nil {
		cancel()
		return nil, fmt.Errorf("failed to initialize services: %w", err)
	}

	proxyHandler, err := handlers.NewProxyHandler(k8sClient, log, cfg.Kubernetes.Namespace, nil, &agentoc.Dialect{})
	if err != nil {
		cancel()
		return nil, fmt.Errorf("failed to create proxy handler: %w", err)
	}
	proxyHandler.SetRequestBufferConfig(cfg.Proxy.RequestBufferSizePerWorkspace, time.Duration(cfg.Proxy.RequestBufferTimeoutSeconds)*time.Second)

	// Resolve subagent (subtask) sessions back to their root user-visible
	// session, so permission/question events from child sessions bubble up
	// to the chat view of the active parent session.
	proxyHandler.EnableSessionParentResolution()

	// Wire session index so sessions are tracked and listable.
	sessionIndexSvc := sessionindex.New(svc.Database, log)
	if wsSvc, ok := svc.Workspace.(*workspace.Service); ok {
		wsSvc.SetSessionIndex(sessionIndexSvc)
	}
	proxyHandler.SetSessionIndex(sessionIndexSvc)

	if cacheSvc, ok := svc.Cache.(*cache.Service); ok {
		queueSvc := msgqueue.NewWithClient(cacheSvc.GetClient())
		proxyHandler.SetMessageQueueService(queueSvc)

		// US-45.2..US-45.8: swap the in-memory state store for a Redis-backed
		// one so multi-replica deployments share all per-workspace state
		// (active sessions, deleted tombstones, password cache, workspace
		// config, prior phase, parent backfill).
		redisStateStore := wsstate.NewRedisStoreWithLogger(
			cacheSvc.GetClient(),
			wsstate.DefaultActiveSessTTL,
			log.With("component", "wsstate"),
		)
		proxyHandler.SetStateStore(redisStateStore)
	} else {
		// M4 (worklog 371): surface the silent fallback to InMemoryStore.
		// Without this warning, a future refactor that wraps the cache
		// service (so the *cache.Service type assertion fails) silently
		// reintroduces multi-replica drift: each replica keeps its own
		// activeSess / deletedSessions / pwCache, and the 2026-06-16
		// stuck-session incident class returns. Single-replica dev/test
		// deployments intentionally hit this path and can ignore the warning.
		log.Warn("Redis cache service unavailable — ProxyHandler is using InMemoryStore. Multi-replica deployments will NOT share per-workspace state (active sessions, tombstones, password cache). This is expected for single-replica dev/test; investigate in production.")
	}

	if svc.Metering != nil {
		proxyHandler.SetMeteringService(svc.Metering)
		if concrete, ok := svc.Metering.(*metering.Service); ok {
			concrete.SetDatabaseService(svc.Database)
			concrete.SetActivePhasesChecker(proxyHandler.GetAllKnownPhases)
		}
	}

	// Initialize settings services (backed by the same DB service).
	dbSvc := svc.Database.(*database.Service)
	instanceSettings := settings.NewInstanceService(dbSvc, log)
	userSettings := settings.NewUserService(dbSvc, log)

	// US-49.2: When email is helm-managed (email block present in config.yaml),
	// mark the email.* instance settings as read-only and pin their values
	// from the helm config. The admin UX will show them disabled with a
	// "Managed by Helm" badge; PUT attempts return 409.
	if cfg.Email.Provider != "" || cfg.Email.FromAddress != "" || cfg.Email.BaseURL != "" {
		instanceSettings.SetHelmOverrides(map[string]any{
			"email.provider":    cfg.Email.Provider,
			"email.sesRegion":   cfg.Email.SESRegion,
			"email.fromAddress": cfg.Email.FromAddress,
			"email.baseUrl":     cfg.Email.BaseURL,
		})
	}

	// Inject instance settings into workspace service for enforcement.
	if wsSvc, ok := svc.Workspace.(*workspace.Service); ok {
		wsSvc.SetInstanceSettings(instanceSettings)
	}

	// Wire version sync: whenever the watcher observes a workspace becoming
	// Active with a new imageTag, persist it to the DB immediately. This
	// replaces the lazy side-effect in GetWorkspaceStatus which only updated
	// the DB when the status endpoint was polled for that specific workspace.
	proxyHandler.SetVersionSyncCallback(func(workspaceID, imageTag, agentVersion string) {
		dbSvc.SyncWorkspaceVersionInfo(context.Background(), workspaceID, imageTag, agentVersion)
	})

	// Create settings handler for API routes.
	settingsHandler := handlers.NewSettingsHandler(instanceSettings, userSettings)

	// Wire secret management (Epic 10).
	var secretsHandler *handlers.SecretsHandler
	var modelsHandler *handlers.ModelsHandler
	var workspaceEnvHandler *handlers.WorkspaceEnvHandler
	var rotateKeyHandler *handlers.RotateKeyHandler
	var unlockDEKHandler *handlers.UnlockDEKHandler
	var adminProvCredHandler *handlers.AdminProviderCredentialsHandler
	var userProvCredHandler *handlers.UserProviderCredentialsHandler
	var orgsHandler *handlers.OrgsHandler
	var orgCredsHandler *handlers.OrgCredentialsHandler
	var pgOrgStore *database.PgOrgStore
	var pendingOrgCleaner *handlers.PendingOrgCleaner
	var invitationsHandler *handlers.InvitationsHandler
	var emailService *emailsvc.Service
	var emailHandler *handlers.EmailHandler
	var emailVerifyHandler *handlers.EmailVerifyHandler
	var passwordResetHandler *handlers.PasswordResetHandler
	var orgCredBinder *secrets.PgSecretStore
	var keyService *secrets.KeyService
	var jwtSessionJanitor *secrets.JWTSessionJanitor // populated when secrets are enabled; goroutine started below
	var policySvc *policy.Service
	var policyHandler *handlers.PolicyHandler
	var promptSvc *prompt.Service
	var promptHandler *handlers.PromptHandler
	var roleSvc *role.Service
	var agentRoleHandler *handlers.AgentRoleHandler
	var auditHandler *handlers.AuditHandler
	var platformAdminHandler *handlers.PlatformAdminHandler
	var internalOrgStatusHandler *handlers.InternalOrgStatusHandler
	var podBootstrapHandler *handlers.PodBootstrapHandler
	var ssoHandler *handlers.SSOHandler
	var loginDiscoveryHandler *handlers.LoginDiscoveryHandler
	var asyncAudit *secrets.AsyncAuditLogger // populated when secrets are enabled; drained on Shutdown
	var secretsPool *pgxpool.Pool            // closed on Shutdown
	var dekCacheClient *redis.Client         // closed on Shutdown
	{
		// US-50.2: construct per-purpose RootKeyProviders before the earliest
		// consumer (the Redis DEK cache below). Each purpose yields an
		// independent HKDF-derived key; the provider wraps it for the
		// Encrypt/Decrypt interface.
		providerCredsProv := newPurposeProvider("provider-credentials")
		orgCredsProv := newPurposeProvider("org-credentials")

		mk := dekMasterKey()
		if mk == nil {
			// Unreachable after validateMasterSecret passed — env var is
			// immutable for the process lifetime. Guards against future
			// refactors that move validateMasterSecret.
			cancel()
			return nil, errors.New("internal: dekMasterKey returned nil after validateMasterSecret passed")
		}
		dekCacheClient = redis.NewClient(&redis.Options{
			Addr:     fmt.Sprintf("%s:%d", cfg.Redis.Host, cfg.Redis.Port),
			Password: cfg.Redis.Password,
			DB:       cfg.Redis.DB,
		})
		// Attach the same metrics hook the primary cache service uses
		// so DEK-cache traffic also feeds the redis duration and error
		// metrics. Without this, traffic that goes exclusively through
		// the DEK cache (key unlock paths) is invisible on the
		// dashboard.
		dekCacheClient.AddHook(cache.NewMetricsHook())
		dekCache := secrets.NewRedisDEKCache(dekCacheClient, mk)

		// Create pgxpool for secret stores (same DB, separate pool for pgx native queries).
		pgxDSN := fmt.Sprintf("host=%s port=%d user=%s password=%s dbname=%s sslmode=%s",
			cfg.Database.Host, cfg.Database.Port, cfg.Database.User,
			cfg.Database.Password, cfg.Database.Database, cfg.Database.SSLMode)
		var pgxErr error
		// Attach the same QueryTracer used by the *sql.DB pool so every
		// query issued by the secrets/keys/credentials pgx-native code
		// also feeds llmsafespaces_db_query_duration_seconds and
		// llmsafespaces_db_errors_total. Without this the secret-store
		// queries are invisible to the operational dashboard.
		var pgxCfg *pgxpool.Config
		pgxCfg, pgxErr = pgxpool.ParseConfig(pgxDSN)
		if pgxErr == nil {
			pgxCfg.ConnConfig.Tracer = database.NewQueryTracer()
			secretsPool, pgxErr = pgxpool.NewWithConfig(context.Background(), pgxCfg)
		}

		var secretService *secrets.SecretService
		var auditStore secrets.SecretStore
		if pgxErr != nil {
			// Refusing to start is the only correct response: the
			// in-memory adapter fallback (dbSecretStoreAdapter,
			// dbKeyStoreAdapter) is racy, unbounded in audit log
			// growth, and loses every secret + key on restart. It
			// existed for dev-environment convenience but in any
			// shape resembling production it is silent data loss
			// disguised as graceful degradation. Tests use the
			// in-memory adapters directly via NewSecretService;
			// production must always have pgxpool.
			cancel()
			return nil, fmt.Errorf("create pgxpool for secrets store: %w (refusing to fall back to in-memory; the in-memory secret/key adapters lose data on restart and are not safe for any environment that handles real user secrets)", pgxErr)
		}
		pgStore := secrets.NewPgSecretStore(secretsPool)
		orgCredBinder = pgStore
		// Wrap the secret store in an async audit logger so audit
		// writes do not block the request goroutine. The wrapper is
		// itself a SecretStore (CRUD methods delegate; LogAudit goes
		// through a 4096-entry buffered channel). Operators see drop
		// counts via Stats() and Warn-level logs.
		asyncAudit = secrets.NewAsyncAuditLogger(pgStore, 4096, log)
		// US-50.12 / G50: wrap each RootKeyProvider with AuditedProvider so
		// every production Decrypt is attributed to secret_audit_log
		// (action "decrypt:<label>", user from context, key version, success).
		// MUST run after asyncAudit is constructed (line above) — placing it
		// earlier makes the wrap dead code. AuditedProvider satisfies
		// VersionedProvider (delegates ActiveVersion to the inner provider) so
		// the key_version column is still stamped correctly at encrypt time.
		// Encrypt is NOT logged — only Decrypt. See pkg/secrets/audited_provider.go.
		providerCredsProv = secrets.NewAuditedProvider(providerCredsProv, asyncAudit, "provider-credentials")
		orgCredsProv = secrets.NewAuditedProvider(orgCredsProv, asyncAudit, "org-credentials")
		keyService = secrets.NewKeyService(secrets.NewPgKeyStore(secretsPool), dekCache)
		keyService.SetLogger(log)
		// Epic 56: wire the durable jwt_sessions store so GetDEK can
		// rehydrate user DEKs after Valkey restart / LRU eviction.
		// Without this, every cache miss surfaces ErrDEKUnavailable
		// regardless of JWT validity — the production bug this epic
		// closes (see design/stories/epic-56-durable-dek-session).
		jwtSessionStore := secrets.NewPgJWTSessionStore(secretsPool)
		keyService.SetJWTSessionStore(jwtSessionStore)
		// Epic 56: prune expired jwt_sessions rows on a 60s cron so the
		// table stays bounded as login traffic accrues. Idempotent and
		// best-effort — see pkg/secrets/jwt_session_janitor.go.
		jwtSessionJanitor = secrets.NewJWTSessionJanitor(jwtSessionStore, 0, log)
		secretService = secrets.NewSecretService(keyService, asyncAudit)
		auditStore = asyncAudit

		// M2-a: shared model cache between SecretsHandler (evicts on bind) and
		// ModelsHandler (reads on ListModels). One cache, two consumers.
		sharedModelCache := handlers.NewInMemoryModelCache()

		secretsHandler = handlers.NewSecretsHandler(secretService)
		secretsHandler.SetModelCache(sharedModelCache)
		// US-29.5: ModelsHandler extracted from SecretsHandler. AgentClient
		// is set later after proxyHandler is constructed (it depends on the
		// runtime password getter). Parser + cache are wired now so the
		// handler is functional for construction-time validation.
		modelsHandler = handlers.NewModelsHandler(nil) // agentClient wired below
		modelsHandler.SetModelCache(sharedModelCache)

		// Wire billing/metering metrics recorder.
		if metricsSvc, ok := svc.GetMetrics().(*metrics.Service); ok {
			modelsHandler.SetMetricsRecorder(metricsSvc)
		}
		// Epic 26: mark relay active when configured.
		if inferenceRelayURL := cfg.Server.InferenceRelayURL; inferenceRelayURL != "" {
			modelsHandler.SetRelayActive(true)
		}
		modelsHandler.SetLogger(log)
		modelsHandler.SetModelStore(dbSvc)
		// US-29.4: WorkspaceEnvHandler owns the env-var endpoints.
		workspaceEnvHandler = handlers.NewWorkspaceEnvHandler(secretService)
		workspaceEnvHandler.SetLogger(log)
		adminProvCredHandler = handlers.NewAdminProviderCredentialsHandler(pgStore, providerCredsProv)
		adminProvCredHandler.SetAutoApplyStore(pgStore)
		userProvCredHandler = handlers.NewUserProviderCredentialsHandler(pgStore, pgStore, keyService, secrets.NewPgKeyStore(secretsPool))
		userProvCredHandler.SetCredentialStateWriter(dbSvc)

		// Seed the free-tier opencode credential (Epic 30 US-30.4).
		if err := ensureFreeTierCredential(context.Background(), pgStore, providerCredsProv, log); err != nil {
			log.Warn("free-tier credential seeding skipped", "error", err.Error())
		}
		// Wire pod-IP resolver so reload-secrets can reach in-pod agentd.
		// Without this the SecretsHandler returns 503 for every reload
		// request and the SetBindings auto-push silently no-ops; see
		// Bug 1 + Bug 2 in worklog 0085.
		secretsPodResolver := newSecretsPodIPResolver(
			&k8sWorkspaceGetterAdapter{client: k8sClient, namespace: cfg.Kubernetes.Namespace},
			dbSvc,
			log,
		)
		secretsHandler.SetPodIPResolver(secretsPodResolver)
		secretsHandler.SetLogger(log)
		secretsHandler.SetCredentialStateWriter(dbSvc)

		// Build the single agentpush.Service and share it between the
		// SecretsHandler (bindings/reload endpoints) and the workspace
		// service (pod-recreation auto-push). Sharing one instance means
		// there's one place to change reload semantics — the SOLID payoff
		// of extracting agentpush from SecretsHandler in worklog 0589.
		//
		// The metrics hook lives on the workspace-side adapter, NOT on
		// the shared pusher: api_secret_auto_push_total is specifically
		// the pod-recreation auto-push counter (per its Help text), and
		// wiring it here would conflate user-initiated SetBindings
		// pushes with automatic pod-recreation pushes — operators
		// couldn't tell "50 users changed bindings" from "50 pods were
		// recreated." See wsAgentPusherAdapter.Push in secrets_adapters.go.
		agentPusher := agentpush.New(
			secretService,
			agentpush.WithPodIPResolver(secretsPodResolver),
			agentpush.WithModelCache(sharedModelCache),
			agentpush.WithLogger(log),
		)
		secretsHandler.SetAgentPusher(agentPusher)
		// worklog 0591: the workspace service is no longer a consumer
		// of the auto-push (that role moved to secretautopush below).
		// SecretsHandler still needs the shared agentPusher for
		// SetBindings/ReloadSecrets user-driven paths, so we wire it
		// above.

		// worklog 0591: watcher-driven auto-push. Uses the shared
		// agentpush.Service + a KeyService.GetDEKForUser retrieval to
		// deliver user-DEK content after a pod recreation (silent or
		// user-initiated), without depending on a live user-request
		// context. Wired into the workspace watcher's per-CRD-event
		// callback via proxyHandler.SetWorkspaceUpdateCallback.
		//
		// Metric emission is handled by the wsAgentPusherAdapter's
		// existing recordAutoPushOutcome call (see adapter.Push in
		// secrets_adapters.go). We intentionally do NOT install a
		// secondary metric hook on secretautopush itself — that would
		// double-count api_secret_auto_push_total every time the
		// watcher-driven push succeeded. The adapter's emission is
		// authoritative.
		//
		// AuthContexter uses agentpush.WithAuth. After GetDEKForUser
		// caches the DEK in Redis under the jti, agentpush.Push's
		// downstream GetDEK(jti, nil) hits the cache and works without
		// a signing key at hand.
		autoPushSvc := secretautopush.New(
			keyService,
			&bindingsCheckerAdapter{store: pgStore},
			&wsAgentPusherAdapter{pusher: agentPusher},
			secretautopush.WithLogger(log),
			secretautopush.WithAuthContexter(agentpushAuthCtxBuilder{}),
		)
		proxyHandler.SetWorkspaceUpdateCallback(autoPushSvc.OnWorkspaceUpdate)
		// Wire password getter so ListModels/SetModel can authenticate
		// to opencode. Uses the same K8s-secret-backed getter as ProxyHandler.
		// Wired after proxyHandler construction (see below).
		// Epic 35: the manifest writer (K8s Secret) has been removed —
		// secretless injection delivers credentials at boot via the
		// bootstrap endpoint. Bind-time delivery is live HTTP push only.
		// Wire the password verifier so RevealSecret enforces a real
		// re-authentication gate. Without this the field is theater
		// (validator finding on RevealSecret in worklog 0094 audit).
		if authSvc, ok := svc.Auth.(*auth.Service); ok {
			secretsHandler.SetPasswordVerifier(authSvc)
		}
		// Workspace-ownership enforcement for the bindings / env / reload-secrets
		// routes lives in WorkspaceAccessMiddleware (design 0041 D1+D5). The
		// SecretService trusts that decision and no longer carries its own
		// verifier — see pkg/secrets/secret_service.go.
		secretService.SetAdminProvider(providerCredsProv)
		secretService.SetOrgProvider(orgCredsProv)
		rotateKeyHandler = handlers.NewRotateKeyHandler(keyService)
		rotateKeyHandler.SetPasswordUpdater(&bcryptPasswordUpdater{db: svc.Database})
		// Epic 56: soft-unlock handler — same KeyService backing
		// UnlockDEKWithSigningKey for rewriting the durable jwt_sessions
		// row when a Valkey miss + missing/stale durable row needs the
		// user to re-enter their password.
		unlockDEKHandler = handlers.NewUnlockDEKHandler(keyService)
		rotateKeyHandler.SetAuditFunc(func(userID, action string) {
			entry := &secrets.AuditEntry{
				UserID:    userID,
				Action:    action,
				Metadata:  []byte(`{}`),
				Timestamp: time.Now(),
			}
			if err := auditStore.LogAudit(context.Background(), entry); err != nil {
				log.Warn("Failed to log audit entry for key rotation", "error", err)
			}
		})

		rkp := newRootKeyProvider(cfg, log)
		// US-50.7: apiKeyProv uses the "master-kek" purpose string (not
		// "dek-cache") so a Redis compromise cannot help unwrap Postgres
		// API-key ciphertexts. The multi-key provider (US-50.4) also holds the
		// old "dek-cache" key so existing rows still decrypt. New encrypts use
		// "master-kek" (version 2, active); the rotation CLI (US-50.5) re-wraps
		// legacy rows. When rkp is a sealed provider (production) it wraps the
		// raw root key — no purpose string applies, so rkp is used as-is.
		apiKeyProv := rkp
		if apiKeyProv == nil {
			masterKEK := deriveServerKey("master-kek")
			dekCacheKey := deriveServerKey("dek-cache")
			if masterKEK != nil && dekCacheKey != nil {
				apiKeyProv, _ = secrets.NewStaticKeyProviderMultiVersion(2, map[int][]byte{
					1: dekCacheKey, // legacy: decrypts existing rows
					2: masterKEK,   // active: encrypts new rows
				})
			}
		} else if sp, ok := apiKeyProv.(*secrets.StaticKeyProvider); ok && sp != nil {
			// rkp is a static provider built from dekMasterKey() (the Helm
			// default path). Upgrade it to a domain-separated multi-key provider
			// so new encrypts use "master-kek" while old rows still decrypt.
			masterKEK := deriveServerKey("master-kek")
			dekCacheKey := deriveServerKey("dek-cache")
			if masterKEK != nil && dekCacheKey != nil {
				apiKeyProv, _ = secrets.NewStaticKeyProviderMultiVersion(2, map[int][]byte{
					1: dekCacheKey,
					2: masterKEK,
				})
			}
		}

		// US-50.12 / G50: wrap the API-key root provider with AuditedProvider
		// so API-key DEK unwraps (auth.go:707) are audited. Placed after the
		// multi-key upgrade so ActiveVersion delegation reports the post-
		// upgrade active version. Same no-key-material contract as above.
		if apiKeyProv != nil && asyncAudit != nil {
			apiKeyProv = secrets.NewAuditedProvider(apiKeyProv, asyncAudit, "api-keys")
		}

		if authSvc, ok := svc.Auth.(*auth.Service); ok {
			authSvc.SetKeyService(keyService)
			authSvc.SetInstanceSettings(instanceSettings)

			if apiKeyProv != nil {
				authSvc.SetRootKeyProvider(apiKeyProv)
			}

			// worklog 0590: expose the API's active JWT signing keys
			// (primary + previous) to KeyService so GetDEKForUser can
			// unwrap a durable jwt_sessions row on behalf of a user in a
			// background context (no request-time matchedSigningKey).
			// This gives the background auto-push path (follow-up PR)
			// the same DEK-access capability every user request already
			// has, without needing to pass session state through the
			// call stack.
			keyService.SetSigningKeyEnumerator(authSvc)
		}

		pgOrgStore = database.NewPgOrgStore(dbSvc.DB)
		orgsHandler = handlers.NewOrgsHandler(pgOrgStore, svc.GetAuth())
		orgCredsHandler = handlers.NewOrgCredentialsHandler(pgStore, pgStore, orgCredsProv, svc.GetAuth())

		// US-43.10: OIDC SSO. The service reuses the auth service as the JWT
		// issuer (GenerateToken) and the server KEK (RootKeyProvider) to encrypt
		// the IdP client secret (D17-S4). A dedicated state-signing key is
		// derived from the master secret so PKCE cookies are unforgeable.
		if authSvc, ok := svc.Auth.(*auth.Service); ok {
			stateKey := deriveServerKey("oidc-state-cookie")
			if stateKey != nil {
				ssoSvc, ssoErr := sso.New(pgOrgStore, dbSvc, sso.ServiceConfig{
					TokenIssuer:         authSvc,
					KeyProvider:         apiKeyProv,
					StateKey:            stateKey,
					TokenTTL:            cfg.Auth.TokenDuration,
					RedirectBaseURL:     cfg.OIDC.RedirectBaseURL,
					FrontendRedirectURL: cfg.OIDC.FrontendRedirectURL,
					StateCookieName:     cfg.OIDC.StateCookieName,
					Logger:              log,
				})
				if ssoErr != nil {
					log.Error("failed to construct sso service", ssoErr)
				} else {
					ssoHandler = handlers.NewSSOHandler(ssoSvc, pgOrgStore, svc.GetAuth(), cfg.Auth.CookieName, cfg.OrgSubdomainRouting.CookieDomain, cfg.OIDC.FrontendRedirectURL, log)
				}
			}
		}

		// US-43.19: platform-admin suspension handlers. orgStore provides
		// UpdateOrgStatus + audit + the atomic last-admin-guarded suspend;
		// dbSvc provides SetUserStatus. svc.GetAuth() wires the F4 token
		// revocation primitive (MarkUserSuspended/ClearUserSuspended). log
		// surfaces best-effort audit-write + revocation-write failures.
		platformAdminHandler = handlers.NewPlatformAdminHandler(pgOrgStore, dbSvc, svc.GetAuth(), svc.GetAuth(), log)
		internalOrgStatusHandler = handlers.NewInternalOrgStatusHandler(pgOrgStore)

		// US-54.1: login discovery handler for POST /api/v1/auth/lookup. Harmless
		// when subdomain routing is disabled (falls back to direct SSO URL).
		loginDiscoveryHandler = handlers.NewLoginDiscoveryHandler(
			svc.Database, pgOrgStore,
			cfg.OrgSubdomainRouting.BaseDomain, log,
		)

		if apiKeyProv != nil {
			keyService.SetAPIKeyStore(&apiKeyStoreAdapter{db: dbSvc}, apiKeyProv)
		}
		wsSvc, wsSvcOk := svc.Workspace.(*workspace.Service)
		if wsSvcOk {
			wsSvc.SetCredentialProvisioner(pgStore)
			wsSvc.SetSecretAutoProvisioner(secretService)
			wsSvc.SetOrgStore(pgOrgStore)
		}
		// Epic 35 US-35.3: pod bootstrap handler. Uses the API's K8s
		// clientset for TokenReview + the SecretService for credential
		// decryption + the DB for workspace lookup + default model.
		// expectedNamespace validates the SA namespace (S1 defense-in-depth).
		//
		// SetLogger is REQUIRED — without it the handler swallows the
		// underlying error on 5xx responses and operators have to read
		// source to diagnose live boot failures (the very gap PR #407
		// closed). Enforced by TestPodBootstrapHandler_LoggerWired.
		podBootstrapHandler = handlers.NewPodBootstrapHandlerFromClientset(
			k8sClient.Clientset(), secretService, dbSvc, nil, cfg.Kubernetes.Namespace,
		)
		podBootstrapHandler.SetLogger(log)
		// User provider-credential bind/unbind routes are NOT under
		// /api/v1/workspaces/:id (they live under /api/v1/provider-credentials/:id/bind/:workspaceId),
		// so WorkspaceAccessMiddleware does not cover them. Wire the
		// canonical ResolveWorkspace + CheckOwnership path so the
		// userProvCred surface shares the exact same authorisation
		// logic as every workspace route — including the D5
		// creator-membership re-check the old adapter lacked. If the
		// workspace service is somehow not the concrete type (defense-
		// in-depth — services.New always constructs *workspace.Service),
		// install a fail-closed checker that rejects every bind rather
		// than silently skipping the ownership check.
		if userProvCredHandler != nil {
			if wsSvcOk {
				userProvCredHandler.SetWorkspaceOwnerChecker(func(ctx context.Context, userID, wsID string) error {
					meta, err := wsSvc.ResolveWorkspace(ctx, wsID)
					if err != nil {
						return err
					}
					return wsSvc.CheckOwnership(ctx, userID, meta)
				})
			} else {
				log.Error("workspace service is not *workspace.Service; user provider-credential bind/unbind will fail-closed", nil)
				userProvCredHandler.SetWorkspaceOwnerChecker(func(_ context.Context, _, _ string) error {
					return fmt.Errorf("ownership verification unavailable: workspace service is misconfigured")
				})
			}
		}
	}

	// In development mode, disable RequireHTTPS so the API works over plain
	securityCfg := server.DefaultRouterConfig().SecurityConfig
	if cfg.Logging.Development {
		securityCfg.Development = true
		securityCfg.RequireHTTPS = false
		securityCfg.AllowHTTPSDowngrade = true
	}
	if len(cfg.Security.AllowedOrigins) > 0 {
		securityCfg.AllowedOrigins = cfg.Security.AllowedOrigins
	}
	securityCfg.AllowCredentials = cfg.Security.AllowCredentials

	rateLimitCfg := server.DefaultRouterConfig().RateLimitConfig
	rateLimitCfg.Enabled = cfg.RateLimiting.Enabled
	if cfg.RateLimiting.DefaultLimit > 0 {
		rateLimitCfg.DefaultLimit = cfg.RateLimiting.DefaultLimit
	}
	if cfg.RateLimiting.DefaultWindow > 0 {
		rateLimitCfg.DefaultWindow = cfg.RateLimiting.DefaultWindow
	}
	if cfg.RateLimiting.BurstSize > 0 {
		rateLimitCfg.BurstSize = cfg.RateLimiting.BurstSize
	}
	if cfg.RateLimiting.Strategy != "" {
		rateLimitCfg.Strategy = cfg.RateLimiting.Strategy
	}

	wsOrigins := server.DefaultRouterConfig().AllowedWebSocketOrigins
	if len(cfg.Security.AllowedOrigins) > 0 && cfg.Security.AllowedOrigins[0] != "*" {
		wsOrigins = cfg.Security.AllowedOrigins
	}

	// Create terminal handler (Epic 14 — WebSocket terminal proxy).
	terminalHandler := handlers.NewTerminalHandler(svc.Cache, &k8sWorkspaceGetterAdapter{client: k8sClient, namespace: cfg.Kubernetes.Namespace}, cfg.Kubernetes.Namespace, log)

	// Epic 27a: Agent reload handler.
	var agentReloadHandler *handlers.AgentReloadHandler
	var bulkReloadHandler *handlers.BulkReloadHandler
	if wsSvc, ok := svc.Workspace.(*workspace.Service); ok {
		agentReloadHandler = handlers.NewAgentReloadHandler(
			wsSvc,
			dbSvc,
			newSecretsPodIPResolver(
				&k8sWorkspaceGetterAdapter{client: k8sClient, namespace: cfg.Kubernetes.Namespace},
				dbSvc,
				log,
			),
			&http.Client{Timeout: 15 * time.Second},
			log,
		)
		bulkReloadHandler = handlers.NewBulkReloadHandler(
			dbSvc,
			wsSvc,
			dbSvc,
			newSecretsPodIPResolver(
				&k8sWorkspaceGetterAdapter{client: k8sClient, namespace: cfg.Kubernetes.Namespace},
				dbSvc,
				log,
			),
			&http.Client{Timeout: 15 * time.Second},
			log,
		)
	}

	// Epic 27b: Drain mode SSETracker wiring is deferred to Run() — the tracker
	// is nil until proxyHandler.Start() runs. Wire password getter + metrics here
	// (these are available at construction time).
	if agentReloadHandler != nil {
		pwGetter := proxyHandler.GetPasswordGetter()
		agentReloadHandler.SetPasswordGetter(pwGetter)
		bulkReloadHandler.SetPasswordGetter(pwGetter)
		// US-29.5: construct ModelsHandler with AgentClient now that
		// the password getter is available.
		if modelsHandler != nil {
			ipResolver := newSecretsPodIPResolver(
				&k8sWorkspaceGetterAdapter{client: k8sClient, namespace: cfg.Kubernetes.Namespace},
				dbSvc, log,
			)
			pwAdapter := func(ctx context.Context, wsID string) (string, error) {
				return pwGetter.WorkspacePassword(ctx, wsID)
			}
			agentClient := agentoc.NewWorkspaceClient(pwAdapter, ipResolver, log.ZapLogger())
			modelsHandler.SetAgentClient(agentClient)
			if relayURL := cfg.Server.InferenceRelayURL; relayURL != "" {
				modelsHandler.SetRelayChecker(buildRelayChecker(ipResolver, pwAdapter))
			}
		}
	}
	// Wire metrics into reload handlers (guarded: handlers are nil when workspace
	// service type assertion fails, e.g. in tests or future refactors).
	if agentReloadHandler != nil {
		if metricsSvc, ok := svc.Metrics.(*metrics.Service); ok {
			agentReloadHandler.SetMetrics(metricsSvc)
			bulkReloadHandler.SetMetrics(metricsSvc)
		}
	}

	usageHandler := handlers.NewUsageHandler(svc.Metering, svc.Database)
	if dbSvc, ok := svc.Database.(*database.Service); ok {
		usageHandler.SetDB(dbSvc.DB)
	}

	// US-44.11: admin-only session recovery (force-abort stuck sessions).
	// Wired with the same *sql.DB handle as the usage handler so the audit
	// log INSERT shares the connection pool; nil DB is handled gracefully.
	var adminSessionHandler *handlers.AdminSessionHandler
	if dbSvc, ok := svc.Database.(*database.Service); ok {
		adminSessionHandler = handlers.NewAdminSessionHandler(proxyHandler, dbSvc.DB, log)
	} else {
		adminSessionHandler = handlers.NewAdminSessionHandler(proxyHandler, nil, log)
	}

	var checkoutProvider billing.CheckoutProvider
	var webhookHandler *handlers.StripeWebhookHandler
	if cfg.Billing.SecretKey != "" {
		sp, err := billing.NewStripeProvider(billing.StripeConfig{
			SecretKey:     cfg.Billing.SecretKey,
			WebhookSecret: cfg.Billing.WebhookSecret,
			PlanPrices:    cfg.Billing.PlanPrices,
			Meters:        cfg.Billing.Meters,
		})
		if err != nil {
			cancel()
			return nil, fmt.Errorf("init stripe provider: %w", err)
		}
		checkoutProvider = sp
		// US-43.17: Wire StripeProvider as usage reporter for metered billing.
		if mSvc, ok := svc.Metering.(*metering.Service); ok {
			mSvc.SetUsageReporter(sp)
		}
		if orgsHandler != nil && cfg.Billing.WebhookSecret != "" && pgOrgStore != nil {
			webhookHandler = handlers.NewStripeWebhookHandler(sp, pgOrgStore, log)
		}
		if orgsHandler != nil {
			orgsHandler.SetBilling(handlers.NewOrgBilling(checkoutProvider),
				cfg.Billing.CheckoutSuccessURL, cfg.Billing.CheckoutCancelURL, cfg.Billing.PortalReturnURL)
		}
	} else if orgsHandler != nil {
		noop := &billing.NoopCheckoutProvider{}
		orgsHandler.SetBilling(handlers.NewOrgBilling(noop),
			cfg.Billing.CheckoutSuccessURL, cfg.Billing.CheckoutCancelURL, cfg.Billing.PortalReturnURL)
	}

	// Pending org cleanup cron: reaps pending_activation orgs whose Stripe
	// checkout was never completed after 7 days. Only runs with a real Stripe
	// provider (needs checkout-session lookup); in dev mode without Stripe the
	// cleanup is a no-op (pending orgs accumulate but are harmless).
	if checkoutProvider != nil && pgOrgStore != nil {
		pendingOrgCleaner = handlers.NewPendingOrgCleaner(
			pgOrgStore, checkoutProvider, log, time.Hour, 7*24*time.Hour)
	}

	// Epic 49: email + password-reset wiring. Extracted into a helper to
	// keep New() under the funlen limit. The helper constructs the email
	// provider, EmailService, EmailHandler, and PasswordResetHandler.
	var emailInitErr error
	emailService, emailHandler, passwordResetHandler, emailInitErr = initEmailStack(cfg, svc, dbSvc, keyService, log)
	if emailInitErr != nil {
		cancel()
		return nil, emailInitErr
	}

	// US-49.6: Email verification. Wire the verifier adapter into auth.Service
	// (so Register sends verification emails) and construct the verify handler
	// (so users can verify + resend). The shared emailTokenStore backs both.
	emailTokenStore := database.NewPgEmailTokenStore(dbSvc.DB)
	verifier := handlers.NewEmailVerifierAdapter(emailTokenStore, emailService, cfg.Email.BaseURL)
	emailVerifyHandler = handlers.NewEmailVerifyHandler(emailTokenStore, svc.Database, emailService, verifier, log)
	if emailService.ProviderName() != "noop" {
		if authSvc, ok := svc.GetAuth().(*auth.Service); ok {
			authSvc.SetEmailVerifier(verifier)
		}
	}

	// Invitations still needs the raw provider + the org store.
	if pgOrgStore != nil {
		mailer, _ := newEmailMailer(cfg)
		invitationsHandler = handlers.NewInvitationsHandler(pgOrgStore, mailer, svc.GetAuth(), cfg.Email.BaseURL, log)
		if orgCredBinder != nil {
			invitationsHandler.SetCredentialBinder(orgCredBinder)
		}
	}

	// US-43.7: Org policy service + handler.
	// Agent Customization: Prompt service + handler.
	if pgOrgStore != nil {
		policySvc = policy.New(pgOrgStore, svc.Cache)
		policyHandler = handlers.NewPolicyHandler(pgOrgStore, policySvc, svc.GetAuth(), log)
		promptSvc = prompt.New(pgOrgStore, svc.Cache)
		promptHandler = handlers.NewPromptHandler(pgOrgStore, promptSvc, svc.GetAuth(), log)
		roleSvc = role.New(pgOrgStore)
		agentRoleHandler = handlers.NewAgentRoleHandler(pgOrgStore, roleSvc, svc.GetAuth(), log)
		if podBootstrapHandler != nil {
			podBootstrapHandler.SetPromptService(promptSvc)
		}
		if wsSvc, ok := svc.Workspace.(*workspace.Service); ok {
			wsSvc.SetPolicyChecker(policySvc)
		}
		// US-43.13: Org audit handler.
		auditHandler = handlers.NewAuditHandler(pgOrgStore)
	}

	// US-43.8: Wire policy checker into secrets handler for model filtering.
	if policySvc != nil && modelsHandler != nil {
		modelsHandler.SetPolicyChecker(policySvc)
	}

	relayRouterSvcURL := os.Getenv("RELAY_ROUTER_SVC_URL")
	if relayRouterSvcURL == "" {
		relayRouterSvcURL = "http://relay-router." + cfg.Kubernetes.Namespace + ".svc.cluster.local:8080"
	}
	routerNamespace := os.Getenv("LLMSAFESPACES_KUBERNETES_PODNAMESPACE")
	if routerNamespace == "" {
		routerNamespace = cfg.Kubernetes.Namespace
	}
	var relayAdminHandler *handlers.RelayAdminHandler
	if llmClient, err := k8sClient.LlmsafespacesV1(); err == nil {
		relayAdminHandler = handlers.NewRelayAdminHandler(
			k8sClient.Clientset(),
			llmClient,
			cfg.Kubernetes.Namespace,
			routerNamespace,
			relayRouterSvcURL,
		)
	} else {
		log.Warn("failed to construct LlmsafespacesV1 client, relay admin routes will not be available", "error", err.Error())
	}

	router := server.NewRouter(svc, log, proxyHandler, server.RouterConfig{
		Debug:                           cfg.Logging.Development,
		LoggingConfig:                   server.DefaultRouterConfig().LoggingConfig,
		RateLimitConfig:                 rateLimitCfg,
		SecurityConfig:                  securityCfg,
		TracingConfig:                   server.DefaultRouterConfig().TracingConfig,
		AllowedWebSocketOrigins:         wsOrigins,
		SettingsHandler:                 settingsHandler,
		InstanceSettings:                instanceSettings,
		AdminProviderCredentialsHandler: adminProvCredHandler,
		UserProviderCredentialsHandler:  userProvCredHandler,
		SecretsHandler:                  secretsHandler,
		ModelsHandler:                   modelsHandler,
		WorkspaceEnvHandler:             workspaceEnvHandler,
		RotateKeyHandler:                rotateKeyHandler,
		UnlockDEKHandler:                unlockDEKHandler,
		OrgsHandler:                     orgsHandler,
		OrgCredentialsHandler:           orgCredsHandler,
		TerminalHandler:                 terminalHandler,
		AgentReloadHandler:              agentReloadHandler,
		BulkReloadHandler:               bulkReloadHandler,
		UsageHandler:                    usageHandler,
		WebhookHandler:                  webhookHandler,
		InvitationsHandler:              invitationsHandler,
		EmailHandler:                    emailHandler,
		EmailVerifyHandler:              emailVerifyHandler,
		PasswordResetHandler:            passwordResetHandler,
		PolicyHandler:                   policyHandler,
		PromptHandler:                   promptHandler,
		AgentRoleHandler:                agentRoleHandler,
		AuditHandler:                    auditHandler,
		RelayAdminHandler:               relayAdminHandler,
		AdminSessionHandler:             adminSessionHandler,
		PlatformAdminHandler:            platformAdminHandler,
		InternalOrgStatusHandler:        internalOrgStatusHandler,
		PodBootstrapHandler:             podBootstrapHandler,
		SSOHandler:                      ssoHandler,
		LoginDiscoveryHandler:           loginDiscoveryHandler,
		CookieName:                      cfg.Auth.CookieName,
		CookieDomain:                    cfg.OrgSubdomainRouting.CookieDomain,
	})

	httpServer := &http.Server{
		Addr:    fmt.Sprintf("%s:%d", cfg.Server.Host, cfg.Server.Port),
		Handler: router,
		// Slowloris hardening: cap header read time. Body read +
		// response write are bounded by per-handler logic; the API has
		// long-lived SSE endpoints so we deliberately do NOT set
		// ReadTimeout/WriteTimeout at the server level.
		ReadHeaderTimeout: 10 * time.Second,
	}

	return &App{
		config:             cfg,
		logger:             log,
		router:             router,
		server:             httpServer,
		k8sClient:          k8sClient,
		services:           svc,
		proxyHandler:       proxyHandler,
		agentReloadHandler: agentReloadHandler,
		bulkReloadHandler:  bulkReloadHandler,
		sessionIndexSvc:    sessionIndexSvc,
		instanceSettings:   instanceSettings,
		userSettings:       userSettings,
		asyncAudit:         asyncAudit,
		secretsPool:        secretsPool,
		pendingOrgCleaner:  pendingOrgCleaner,
		jwtSessionJanitor:  jwtSessionJanitor,
		invitationsHandler: invitationsHandler,
		emailService:       emailService,
		emailHandler:       emailHandler,
		emailVerifyHandler: emailVerifyHandler,
		dekCacheClient:     dekCacheClient,
		shutdownCh:         make(chan struct{}),
		ctx:                ctx,
		cancel:             cancel,
	}, nil
}

func (a *App) Run() error {
	if err := a.services.Start(); err != nil {
		return fmt.Errorf("failed to start services: %w", err)
	}

	// Start the dependency health probe so llmsafespaces_dependency_up
	// and the db-pool gauges have a continuous signal independent of
	// request traffic. Constructed here (not in New) so we have access
	// to the already-initialized services.
	if dbSvc, ok := a.services.Database.(*database.Service); ok {
		deps := map[string]health.Pingable{
			"postgres": dbSvc,
		}
		if cacheSvc, ok := a.services.Cache.(*cache.Service); ok {
			deps["redis"] = cacheSvc
		}
		a.healthChecker = health.NewChecker(a.logger, health.Config{
			Dependencies: deps,
			PoolSource:   dbSvc.DB,
		})
		a.healthChecker.Start(a.ctx)
	}

	// Disabled: self-service org creation removed. Re-enable when billing portal ships.
	// if a.pendingOrgCleaner != nil {
	// 	go a.pendingOrgCleaner.Run(a.ctx)
	// 	a.logger.Info("pending org cleanup cron started", "interval", "1h", "maxAge", "7d")
	// }

	// Epic 56: prune expired jwt_sessions rows on a 60s cron. Started
	// here (after dependencies are healthy) so a transient PG outage at
	// boot doesn't prevent the API from coming up. The janitor's
	// runOnce is internally tolerant of store errors — it retries on
	// the next tick.
	if a.jwtSessionJanitor != nil {
		go a.jwtSessionJanitor.Run(a.ctx)
		a.logger.Info("jwt_sessions janitor started", "interval", secrets.DefaultJWTSessionJanitorInterval.String())
	}

	// Start instance settings (loads cache from DB).
	if err := a.instanceSettings.Start(); err != nil {
		a.logger.Warn("Instance settings failed to start (will use defaults)", "error", err.Error())
		// Non-fatal: settings will fall back to schema defaults.
	}

	// Seed instance settings defaults (idempotent).
	if result, err := settings.Seed(a.ctx, a.services.Database.(*database.Service), a.logger); err != nil {
		a.logger.Warn("Settings seed failed", "error", err.Error())
	} else {
		a.logger.Info("Settings seed complete", "inserted", result.Inserted, "skipped", result.Skipped, "orphaned", len(result.Orphaned))
	}

	if err := a.k8sClient.Start(); err != nil {
		_ = a.services.Stop()
		return fmt.Errorf("failed to start Kubernetes client: %w", err)
	}

	if err := a.proxyHandler.Start(); err != nil {
		a.k8sClient.Stop()
		_ = a.services.Stop()
		return fmt.Errorf("failed to start proxy handler: %w", err)
	}

	// Epic 27a/27b: Wire drain mode dependencies now that proxyHandler.Start()
	// has initialized the SSETracker.
	if a.agentReloadHandler != nil {
		if tracker := a.proxyHandler.GetSSETracker(); tracker != nil {
			a.agentReloadHandler.SetSSETracker(tracker)
			if a.bulkReloadHandler != nil {
				a.bulkReloadHandler.SetSSETracker(tracker)
			}
		}
		// Wire queue clearer and broker so dispose clears pending queue messages.
		if qs := a.proxyHandler.GetMessageQueueService(); qs != nil {
			a.agentReloadHandler.SetQueueClearer(qs)
			if a.bulkReloadHandler != nil {
				a.bulkReloadHandler.SetQueueClearer(qs)
			}
		}
		if b := a.proxyHandler.GetBroker(); b != nil {
			a.agentReloadHandler.SetBrokerPublisher(b)
			if a.bulkReloadHandler != nil {
				a.bulkReloadHandler.SetBrokerPublisher(b)
			}
		}
	}

	// Epic 26 / billing: wire inference callback and session metrics unconditionally.
	// Previously nested inside the agentReloadHandler guard, which meant if the
	// workspace service type assertion failed (or the handler wasn't created),
	// SetOnInference was never called and inference metrics remained permanently zero.
	if tracker := a.proxyHandler.GetSSETracker(); tracker != nil {
		if metricsSvc, ok := a.services.Metrics.(*metrics.Service); ok {
			meteringSvc := a.services.Metering
			ph := a.proxyHandler
			tracker.SetOnInference(func(workspaceID, modelID, providerID string, inputTokens, outputTokens int64, costDollars float64) {
				metricsSvc.RecordInference(modelID, providerID, inputTokens, outputTokens, costDollars)
				if meteringSvc == nil {
					return
				}
				ownerID := ph.GetWorkspaceOwner(workspaceID)
				if ownerID == "" {
					return
				}
				owner := types.BillingOwner{ID: ownerID, Type: types.OwnerTypeUser}
				meteringSvc.Record(types.UsageEvent{
					IdempotencyKey: fmt.Sprintf("tokens:%s:%s:in:%d", workspaceID, modelID, time.Now().UnixNano()),
					Owner:          owner,
					ActorID:        ownerID,
					WorkspaceID:    workspaceID,
					EventType:      "llm_tokens",
					EventSubtype:   "input",
					Quantity:       inputTokens,
					Source:         "api",
					EventTime:      time.Now(),
					Metadata:       map[string]any{"model_id": modelID, "provider_id": providerID},
				})
				if outputTokens > 0 {
					meteringSvc.Record(types.UsageEvent{
						IdempotencyKey: fmt.Sprintf("tokens:%s:%s:out:%d", workspaceID, modelID, time.Now().UnixNano()),
						Owner:          owner,
						ActorID:        ownerID,
						WorkspaceID:    workspaceID,
						EventType:      "llm_tokens",
						EventSubtype:   "output",
						Quantity:       outputTokens,
						Source:         "api",
						EventTime:      time.Now(),
						Metadata:       map[string]any{"model_id": modelID, "provider_id": providerID},
					})
				}
			})
			tracker.SetSessionMetrics(metricsSvc)
		}
	}
	// Epic 27b US-27b.5: Wire agent state checker into proxy for chat error enrichment.
	// dbSvc is referenced via services; use a type assertion to get the concrete type
	// which implements AgentStateChecker (GetLastCredentialChangedAt).
	if dbSvc, ok := a.services.Database.(*database.Service); ok {
		a.proxyHandler.SetAgentStateChecker(dbSvc)
	}

	if err := a.sessionIndexSvc.Start(); err != nil {
		_ = a.proxyHandler.Stop()
		a.k8sClient.Stop()
		_ = a.services.Stop()
		return fmt.Errorf("failed to start session index: %w", err)
	}

	a.logger.Info("Starting HTTP server", "address", a.server.Addr)

	if err := a.server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		return fmt.Errorf("server error: %w", err)
	}

	return nil
}

func (a *App) Shutdown() error {
	a.logger.Info("Shutting down application")

	a.cancel()

	// Stop the dependency probe before the rest of the shutdown so the
	// loop is not still pinging dependencies as their connections are
	// closing. Stop is idempotent and safe even if Run never made it
	// past health-checker construction.
	if a.healthChecker != nil {
		a.healthChecker.Stop()
	}

	ctx, cancel := context.WithTimeout(context.Background(), a.config.Server.ShutdownTimeout)
	defer cancel()

	if err := a.server.Shutdown(ctx); err != nil {
		a.logger.Error("HTTP server shutdown error", err)
	}

	if err := a.proxyHandler.Stop(); err != nil {
		a.logger.Error("Proxy handler shutdown error", err)
	}

	if err := a.sessionIndexSvc.Stop(); err != nil {
		a.logger.Error("Session index shutdown error", err)
	}

	// Drain pending audit entries before tearing down the DB pool so
	// pending writes get a fair chance to land.
	if a.asyncAudit != nil {
		a.asyncAudit.Stop()
		stats := a.asyncAudit.Stats()
		a.logger.Info("Async audit logger drained",
			"written", stats.Written, "dropped", stats.Dropped, "failed", stats.Failed)
	}

	// Close the secrets pgxpool and Redis DEK cache last so any
	// last-millisecond audit write through asyncAudit.run() above
	// could complete. Both are nil-safe; we still nil-check for
	// belt-and-braces against future "secrets disabled" config paths.
	if a.secretsPool != nil {
		a.secretsPool.Close()
	}
	if a.dekCacheClient != nil {
		if err := a.dekCacheClient.Close(); err != nil {
			a.logger.Error("Redis DEK cache close error", err)
		}
	}

	a.k8sClient.Stop()

	if err := a.services.Stop(); err != nil {
		a.logger.Error("Services shutdown error", err)
	}

	a.logger.Info("Application shutdown complete")
	return nil
}

// validateMasterSecret verifies the master KEK is configured and usable.
// Source preference (US-50.1): the file mount (LLMSAFESPACES_MASTER_SECRET_FILE)
// is the modern, /proc-safe delivery; the legacy value env vars
// (LLMSAFESPACES_MASTER_SECRET / LLMSAFESPACES_DEK_MASTER_KEY) are retained for
// one release and log a deprecation Warn when relied upon.
func validateMasterSecret(log *logger.Logger) error {
	// 1) File mount path (preferred). If the path env is set, every referenced
	//    file must exist and decode to >=32 bytes; a configured-but-broken
	//    mount is a startup error, not a silent fallback.
	if fileEnv := os.Getenv(masterSecretFileEnv); fileEnv != "" {
		materials := loadMasterSecretMaterials()
		if len(materials) == 0 {
			return fmt.Errorf(
				"%s is set to %q but no readable key file was found; "+
					"verify the mounted Secret volume; refusing to start without a DEK encryption key",
				masterSecretFileEnv, fileEnv)
		}
		// The active material is the highest version (last file per the US-50.4
		// rotation-window convention); validate its length.
		active := materials[len(materials)-1]
		if len(active) < 32 {
			log.Warn("master KEK file material is too short for AES-256-GCM",
				"decoded_bytes", len(active), "required_bytes", 32, "source", masterSecretFileEnv)
			return fmt.Errorf(
				"master KEK from %s decodes to %d bytes; minimum is 32 (AES-256-GCM key size)",
				masterSecretFileEnv, len(active))
		}
		// File source is healthy. If a legacy value env var is ALSO set, warn: it
		// is unused at runtime (the file wins) but still exposes the KEK value in
		// /proc/1/environ, defeating H1. Operators should remove it. Check BOTH
		// legacy var names so a migration from either is flagged.
		if os.Getenv(masterSecretValueEnv) != "" {
			log.Warn("LLMSAFESPACES_MASTER_SECRET env var is set but ignored because the file mount takes precedence; remove it to avoid exposing the KEK in /proc/1/environ",
				"source", masterSecretFileEnv)
		}
		if os.Getenv(masterSecretLegacyEnv) != "" {
			log.Warn("LLMSAFESPACES_DEK_MASTER_KEY env var is set but ignored because the file mount takes precedence; remove it to avoid exposing the KEK in /proc/1/environ",
				"source", masterSecretFileEnv)
		}
		return nil
	}

	// 2) Legacy value env vars (deprecated). Log a Warn so operators move to
	//    the file mount; only warn when the value is actually present.
	masterRaw := os.Getenv(masterSecretValueEnv)
	if masterRaw == "" {
		masterRaw = os.Getenv(masterSecretLegacyEnv)
	}
	if masterRaw == "" {
		return errors.New(
			"master KEK is required but not configured. Set LLMSAFESPACES_MASTER_SECRET_FILE " +
				"(file mount, preferred) or LLMSAFESPACES_MASTER_SECRET (deprecated env var); " +
				"refusing to start without DEK encryption at rest in Redis. " +
				"Generate one with: openssl rand -hex 32")
	}
	log.Warn("master KEK delivered via env var is deprecated; use the file mount (masterSecret.deliveryMethod defaults to file in the Helm chart). See pkg/secrets/README.md.",
		"source", "env")

	var master []byte
	if decoded, err := hex.DecodeString(masterRaw); err == nil {
		master = decoded
	} else {
		master = []byte(masterRaw)
	}

	if len(master) < 32 {
		log.Warn("LLMSAFESPACES_MASTER_SECRET is set but too short for AES-256-GCM",
			"decoded_bytes", len(master), "required_bytes", 32)
		// masterRaw is intentionally NOT included in the error message or log.
		return fmt.Errorf(
			"LLMSAFESPACES_MASTER_SECRET decodes to %d bytes; minimum is 32 (AES-256-GCM key size). "+
				"Use at least 32 bytes (e.g. 64 hex chars, or 32+ alphanumeric chars)",
			len(master))
	}
	return nil
}

// buildRelayChecker creates a RelayStateChecker that reads the relay
// injection state from the agentd admin port (/v1/readyz). The checker
// resolves podIP + password internally, keeping the ModelsHandler free
// of pod/auth concerns (US-29.5 design).
func buildRelayChecker(
	ipResolver handlers.PodIPResolver,
	pwGetter func(context.Context, string) (string, error),
) handlers.RelayStateChecker {
	return newRelayChecker(&http.Client{Timeout: 5 * time.Second}, agentd.AgentdAdminPort, ipResolver, pwGetter)
}

// readyzReadLimit bounds the /v1/readyz response read. readyz is a tiny
// envelope (a bool plus small fields); 16 KiB is ample and matches the
// precedent set by the statusz decoder in proxy_events.go. Worklog 0372
// (H4): the limit was dropped during the US-29.5 extraction, leaving the
// decoder exposed to an unbounded body.
const readyzReadLimit = 16 * 1024

// newRelayChecker is the testable core of buildRelayChecker. The port and
// http.Client are injected so tests can target an httptest server and
// verify the read limit without binding the real agentd admin port.
func newRelayChecker(
	client *http.Client,
	port int,
	ipResolver handlers.PodIPResolver,
	pwGetter func(context.Context, string) (string, error),
) handlers.RelayStateChecker {
	return func(ctx context.Context, userID, workspaceID string) bool {
		podIP, err := ipResolver.GetWorkspacePodIP(ctx, userID, workspaceID)
		if err != nil || podIP == "" {
			return false
		}
		password, err := pwGetter(ctx, workspaceID)
		if err != nil || password == "" {
			return false
		}
		url := fmt.Sprintf("http://%s:%d/v1/readyz", podIP, port)
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			return false
		}
		req.Header.Set("Authorization", "Bearer "+password)
		resp, err := client.Do(req)
		if err != nil {
			return false
		}
		defer func() { _ = resp.Body.Close() }()
		if resp.StatusCode != http.StatusOK {
			return false
		}
		var readyz struct {
			RelayInjected bool `json:"relay_injected"`
		}
		if json.NewDecoder(io.LimitReader(resp.Body, readyzReadLimit)).Decode(&readyz) != nil {
			return false
		}
		return readyz.RelayInjected
	}
}

// initEmailStack constructs the EmailService, EmailHandler, and
// PasswordResetHandler. Extracted from New() to keep it under the funlen
// limit. Returns an error if the email provider is misconfigured (SES
// requires fromAddress + baseUrl); the caller must propagate it to fail
// fast at boot.
func initEmailStack(
	cfg *config.Config,
	svc *services.Services,
	dbSvc *database.Service,
	keyService *secrets.KeyService,
	log *logger.Logger,
) (*emailsvc.Service, *handlers.EmailHandler, *handlers.PasswordResetHandler, error) {
	mailer, err := newEmailMailer(cfg)
	if err != nil {
		return nil, nil, nil, err
	}
	emailService := emailsvc.NewService(mailer, cfg.Email.BaseURL, cfg.Email.Provider)
	emailHandler := handlers.NewEmailHandler(emailService, svc.GetRateLimiter(), log)

	emailTokenStore := database.NewPgEmailTokenStore(dbSvc.DB)
	var sessionRevoker interface {
		RevokeAllUserSessions(ctx context.Context, userID string) error
	}
	if authSvc, ok := svc.GetAuth().(*auth.Service); ok {
		sessionRevoker = authSvc
	}
	passwordResetHandler := handlers.NewPasswordResetHandler(
		emailTokenStore,
		svc.Database,
		keyService,
		&bcryptPasswordUpdater{db: svc.Database},
		sessionRevoker,
		emailService,
		log,
	)
	// Purge the user's encrypted secret rows on reset (makes the
	// "your saved keys will be deleted" guarantee literal).
	passwordResetHandler.SetSecretPurger(dbSvc)
	// Suspend the user's active workspaces + scrub their ephemeral
	// workspace-secrets-* K8s Secrets so relaunch yields no secrets.
	if wsSvc, ok := svc.GetWorkspace().(*workspace.Service); ok {
		passwordResetHandler.SetWorkspaceNeutralizer(wsSvc)
	}
	return emailService, emailHandler, passwordResetHandler, nil
}
