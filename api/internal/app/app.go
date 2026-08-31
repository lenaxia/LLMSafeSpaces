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
	"github.com/lenaxia/llmsafespaces/api/internal/imagefactory"
	apiinterfaces "github.com/lenaxia/llmsafespaces/api/internal/interfaces"
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
	"github.com/lenaxia/llmsafespaces/api/internal/services/outbox"
	"github.com/lenaxia/llmsafespaces/api/internal/services/passkey"
	"github.com/lenaxia/llmsafespaces/api/internal/services/policy"
	"github.com/lenaxia/llmsafespaces/api/internal/services/prompt"
	"github.com/lenaxia/llmsafespaces/api/internal/services/role"
	"github.com/lenaxia/llmsafespaces/api/internal/services/secretautopush"
	"github.com/lenaxia/llmsafespaces/api/internal/services/sessionalerts"
	"github.com/lenaxia/llmsafespaces/api/internal/services/sessionindex"
	"github.com/lenaxia/llmsafespaces/api/internal/services/sse"
	"github.com/lenaxia/llmsafespaces/api/internal/services/sso"
	"github.com/lenaxia/llmsafespaces/api/internal/services/workspace"
	"github.com/lenaxia/llmsafespaces/api/internal/services/wsstate"
	apiwf "github.com/lenaxia/llmsafespaces/api/internal/workflows"
	pkgagent "github.com/lenaxia/llmsafespaces/pkg/agent"
	agentoc "github.com/lenaxia/llmsafespaces/pkg/agent/opencode"
	"github.com/lenaxia/llmsafespaces/pkg/agent/systemnotices"
	"github.com/lenaxia/llmsafespaces/pkg/agentd"
	"github.com/lenaxia/llmsafespaces/pkg/billing"
	emailpkg "github.com/lenaxia/llmsafespaces/pkg/email"
	"github.com/lenaxia/llmsafespaces/pkg/kubernetes"
	"github.com/lenaxia/llmsafespaces/pkg/secrets"
	"github.com/lenaxia/llmsafespaces/pkg/settings"
	"github.com/lenaxia/llmsafespaces/pkg/types"
	"github.com/lenaxia/llmsafespaces/pkg/workflows"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
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
	sessionAlertsSvc   *sessionalerts.Service
	instanceSettings   *settings.InstanceService
	userSettings       *settings.UserService
	asyncAudit         *secrets.AsyncAuditLogger // nil if pgxpool path not used
	secretsPool        *pgxpool.Pool             // pgx pool for secrets store; closed on shutdown
	dekCacheClient     *redis.Client             // redis client for DEK cache; closed on shutdown
	healthChecker      *health.Checker           // periodic dependency probe; nil only in degraded test setups
	pendingOrgCleaner  *handlers.PendingOrgCleaner
	jwtSessionJanitor  *secrets.JWTSessionJanitor // Epic 56: prunes expired jwt_sessions rows
	wfReconciler       *apiwf.Reconciler          // Epic 64: workflow run executor
	wfScheduler        *apiwf.Scheduler           // Epic 64: cron trigger scheduler
	invitationsHandler *handlers.InvitationsHandler
	emailService       *emailsvc.Service
	emailHandler       *handlers.EmailHandler
	emailVerifyHandler *handlers.EmailVerifyHandler
	shutdownCh         chan struct{}
	ctx                context.Context
	cancel             context.CancelFunc
}

// addTurnstileToCSP extends a Content-Security-Policy directive string
// with the Cloudflare Turnstile origin (challenges.cloudflare.com) in
// script-src and frame-src. Idempotent: if the origin is already
// present in a directive, the input is left unchanged.
//
// The Turnstile widget loads api.js from challenges.cloudflare.com and
// renders its challenge in an iframe on the same origin. Without both
// directives, the browser blocks the widget entirely; script-src fires
// onerror on the script tag, frame-src blocks the iframe. Users see a
// permanently-disabled submit button and no way to complete
// registration.
//
// If script-src or frame-src is absent, this function adds a new
// directive. When frame-src is absent it falls back to default-src, so
// we explicitly add frame-src rather than relying on that fallback.
//
// PR #501 review round 4 (2026-07-04) — the chart-side CSP annotation
// in frontend-ingress.yaml has an equivalent transformation for the
// nginx-ingress path. This function covers the API-level SecurityConfig
// used by the gin security middleware.
func addTurnstileToCSP(csp string) string {
	const origin = "https://challenges.cloudflare.com"
	// Split on ";", trim each directive, and extend/insert as needed.
	parts := strings.Split(csp, ";")
	var haveScript, haveFrame bool
	for i, p := range parts {
		trimmed := strings.TrimSpace(p)
		switch {
		case strings.HasPrefix(trimmed, "script-src"):
			haveScript = true
			if !strings.Contains(trimmed, origin) {
				parts[i] = " " + trimmed + " " + origin
			}
		case strings.HasPrefix(trimmed, "frame-src"):
			haveFrame = true
			if !strings.Contains(trimmed, origin) {
				parts[i] = " " + trimmed + " " + origin
			}
		}
	}
	if !haveScript {
		parts = append(parts, " script-src 'self' "+origin)
	}
	if !haveFrame {
		parts = append(parts, " frame-src 'self' "+origin)
	}
	return strings.Join(parts, ";")
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

	// US-65.6-followup: register the agent runtime explicitly instead of
	// relying on init() side-effects.
	agentoc.Register()

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

	// US-65.4: construct the Agent Adapter and wire it into ProxyHandler.
	// Batch-1 handlers (session_parents, session_index, proxy_permissions,
	// proxy_input's emitPendingInputRequests) check h.adapter != nil and
	// use the Adapter path. Remaining handlers use the legacy dialect path.
	// US-65.4 batches 2+ will migrate the client-facing proxy handlers.
	//
	// The resolvers returned by ProxyHandler are generic Go types
	// (func + interface) to avoid importing pkg/agent/opencode from
	// api/internal/handlers/. agentoc.PasswordResolver is a func type
	// (explicit conversion); agentoc.PodIPResolver is an interface that
	// handlers.WorkspacePodIPResolver satisfies structurally (same
	// method signature, no explicit cast needed — Go structural typing).
	//
	// V2 delivery (design 0052, OPENCODE_V2_DELIVERY=1): routes outbox
	// delivery through the V2 admit-and-return prompt endpoint and
	// history reads/verification through the V2 store. The flag pairs
	// the adapter option with proxyHandler.SetV2Delivery below — the
	// two MUST move together (delivery and verification must agree on
	// the store). Off by default; requires opencode ≥ 1.18.15 (the
	// V2 queue-drain fixes — the runtime pin's floor).
	v2Delivery := strings.EqualFold(os.Getenv("OPENCODE_V2_DELIVERY"), "1") || strings.EqualFold(os.Getenv("OPENCODE_V2_DELIVERY"), "true")
	var agentAdapter *agentoc.Adapter
	if v2Delivery {
		agentAdapter = agentoc.NewAdapter(
			agentoc.PasswordResolver(proxyHandler.AdapterPasswordResolver()),
			proxyHandler.AdapterPodIPResolver(),
			log.ZapLogger(),
			agentoc.WithV2Store(true),
		)
	} else {
		agentAdapter = agentoc.NewAdapter(
			agentoc.PasswordResolver(proxyHandler.AdapterPasswordResolver()),
			proxyHandler.AdapterPodIPResolver(),
			log.ZapLogger(),
		)
	}

	// #944: system-notice injection at the Adapter seam — the ONE point
	// every entrypoint (HTTP chat, MCP, SDK) shares. The disk-pressure
	// nudge was twice orphaned by path migrations when it lived in the
	// proxy transport; wrapping here covers all of them forever.
	proxyHandler.SetAdapter(systemnotices.Wrap(agentAdapter, &crdDiskUsage{
		k8s:       k8sClient,
		namespace: cfg.Kubernetes.Namespace,
	}))

	// Wire the V2 client concrete factory (US-65.6: removes opencode import
	// from proxy_v2.go; the factory is the only allowed opencode import site).
	proxyHandler.SetV2ClientConcreteFactory(func(baseURL, password string) (pkgagent.V2SessionClient, error) {
		return agentoc.NewClient(baseURL, password, log.ZapLogger()), nil
	})

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

	// D6 (#998) finding 4: persist hung-session escalations so alerts
	// survive SSE disconnects and remain queryable (GET /workspaces/:id/alerts).
	sessionAlertsSvc := sessionalerts.New(svc.Database, log)
	proxyHandler.SetSessionAlerts(sessionAlertsSvc)

	if cacheSvc, ok := svc.Cache.(*cache.Service); ok {
		proxyHandler.SetV2QueueShadow(handlers.NewV2QueueShadow(cacheSvc.GetClient()))
		if v2Delivery {
			proxyHandler.SetV2Delivery(true)
		}
		proxyHandler.SetV2PendingTracker(handlers.NewV2PendingTracker(cacheSvc.GetClient()))

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

		// #759: persist per-session cumulative usage dedup state on the
		// shared cache so an API pod restart never re-bills a session's
		// cumulative input tokens.
		proxyHandler.SetTokenSeenStore(sse.NewRedisTokenSeenStore(
			cacheSvc.GetClient(),
			sse.DefaultTokenSeenTTL,
		))

		// D3 (design 0050 §D3, #907): the durable-prompt outbox — same
		// Valkey instance (AOF-persisted); accepts survive client
		// disconnects and API restarts.
		proxyHandler.SetOutbox(outbox.New(cacheSvc.GetClient()))
		if v2Delivery {
			// Design 0052: admission-scale delivery windows. The V1
			// sync send blocks turn-to-completion (hence 10-minute
			// bounds); the V2 admit-and-return POST completes in
			// milliseconds, so the bounds shrink to admission scale —
			// but the delivery attempt ALSO carries the #1119
			// promotion-await loop, whose window must fit INSIDE the
			// budget with margin for the admission POST, the final
			// verify poll, and bookkeeping. First live traffic
			// (2026-08-29) showed the hazard of budget == window: the
			// detached ctx expires mid-loop and surfaces as
			// "ambiguous: context deadline exceeded" instead of the
			// promotion-window ambiguity. Set once here, before
			// Start() spawns the worker.
			outbox.DeliveryTimeout = handlers.V2PromotionAwaitBudget() + 20*time.Second
			outbox.LockTTL = 2 * time.Minute
		}
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

	// When workspace.defaultStorageClass is set in the Helm values, pin the
	// `workspace.defaultStorageClass` instance setting to that value so the
	// operator's chart-declared choice can't be silently overridden via the
	// admin UI. Empty (the chart default) leaves the setting admin-mutable
	// and it falls through to the DB-backed value (schema default "" =
	// cluster-default StorageClass).
	if cfg.Workspace.DefaultStorageClass != "" {
		instanceSettings.SetHelmOverrides(map[string]any{
			"workspace.defaultStorageClass": cfg.Workspace.DefaultStorageClass,
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
	var unlockDEKHandler *handlers.UnlockDEKHandler
	var adminProvCredHandler *handlers.AdminProviderCredentialsHandler
	var userProvCredHandler *handlers.UserProviderCredentialsHandler
	var imageFactoryHandler *handlers.ImageFactoryHandler
	var imageFactoryAdminHandler *handlers.ImageFactoryAdminHandler
	var adminMcpHandler *handlers.MCPServersHandler
	var orgMcpHandler *handlers.MCPServersHandler
	var userMcpHandler *handlers.MCPServersHandler
	// Epic 64: workflow + trigger handlers
	var userWorkflowsHandler *handlers.WorkflowsHandler
	var orgWorkflowsHandler *handlers.WorkflowsHandler
	var userTriggersHandler *handlers.TriggersHandler
	var orgTriggersHandler *handlers.TriggersHandler
	var webhookReceiverHandler *handlers.WebhookReceiverHandler
	var wfReconciler *apiwf.Reconciler
	var wfScheduler *apiwf.Scheduler
	var engineLogger *apiwf.AppEngineLogger
	// mcpPushAdapter is assigned after agentPusher is constructed; used by
	// all three MCP handler scopes for live reload after bind.
	var mcpPushAdapter func(ctx context.Context, userID, workspaceID string) error
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
	var passkeyHandler *handlers.PasskeyHandler
	var asyncAudit *secrets.AsyncAuditLogger // populated when secrets are enabled; drained on Shutdown
	var secretsPool *pgxpool.Pool            // closed on Shutdown
	var dekCacheClient *redis.Client         // closed on Shutdown
	{
		// US-50.2: construct per-purpose RootKeyProviders before the earliest
		// consumer (the Redis DEK cache below). Each purpose yields an
		// independent HKDF-derived key; the provider wraps it for the
		// Encrypt/Decrypt interface.
		providerCredsProv := newPurposeProvider(cfg, log, "provider-credentials")
		orgCredsProv := newPurposeProvider(cfg, log, "org-credentials")

		// US-57.1 D7: fail-closed guard. When RootKeyProvider is explicitly
		// "aws-kms" and any per-purpose provider construction failed (nil),
		// refuse to boot BEFORE AuditedProvider wrapping (which always
		// returns non-nil, making post-wrap nil checks dead code caught by
		// nilness/staticcheck).
		if cfg.Security.RootKeyProvider == "aws-kms" || cfg.Security.RootKeyProvider == "gcp-kms" {
			if providerCredsProv == nil {
				cancel()
				return nil, errors.New("KMS root key provider enabled but providerCredentials provider failed to initialize — refusing to boot")
			}
			if orgCredsProv == nil {
				cancel()
				return nil, errors.New("KMS root key provider enabled but orgCredentials provider failed to initialize — refusing to boot")
			}
		}

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

		// Epic 53: MCP server handlers. Admin/org use the same RootKeyProvider
		// as their credential counterparts (D3 — reuse existing crypto purposes).
		// User-scope uses the session DEK (zero-knowledge, D13), mirroring the
		// user provider-credential handler.
		adminMcpHandler = handlers.NewAdminMCPServersHandler(pgStore, providerCredsProv)
		// pgOrgStore is not yet initialized here (it's created in the org
		// init block below). Pass nil now; SetOrgChecker is called after
		// pgOrgStore is available. Without this deferred wiring, UserCreate
		// panics on h.orgChecker.GetUserOrgID (nil pointer).
		userMcpHandler = handlers.NewUserMCPServersHandler(pgStore, nil, keyService, secrets.NewPgKeyStore(secretsPool))

		// Wire governance + operational deps shared across all MCP scopes.
		for _, mh := range []*handlers.MCPServersHandler{adminMcpHandler, userMcpHandler} {
			mh.SetSettings(instanceSettings)
			mh.SetLogger(log)
		}
		// Audit + settings wiring deferred to after pgOrgStore init for
		// BOTH admin and user handlers (pgOrgStore is nil here — created
		// in the org init block below). The old calls at lines 483-484
		// were nil-wired and silently dropped all audit events.

		// Seed the free-tier opencode credential (Epic 30 US-30.4).
		if err := ensureFreeTierCredential(context.Background(), pgStore, providerCredsProv, log); err != nil {
			log.Warn("free-tier credential seeding skipped", "error", err.Error())
		}

		// Epic 64: Workflow + trigger handlers. The workflows.Store wraps the
		// secrets pgxpool (same connection pool, different table set).
		wfStore := workflows.NewStore(secretsPool)
		userWorkflowsHandler = handlers.NewUserWorkflowsHandler(wfStore, instanceSettings)
		orgWorkflowsHandler = handlers.NewOrgWorkflowsHandler(wfStore, instanceSettings)
		userTriggersHandler = handlers.NewUserTriggersHandler(wfStore, instanceSettings, providerCredsProv)
		orgTriggersHandler = handlers.NewOrgTriggersHandler(wfStore, instanceSettings, providerCredsProv)
		webhookReceiverHandler = handlers.NewWebhookReceiverHandler(wfStore, providerCredsProv, 1<<20)
		webhookReceiverHandler.SetRateChecker(svc.GetRateLimiter(), 10, 20)

		// Epic 64: Construct the workflow engine (reconciler + scheduler).
		// Started as background goroutines in Start() — same pattern as
		// jwtSessionJanitor. The engine runs in the API server because it
		// needs PostgreSQL access (FOR UPDATE SKIP LOCKED), K8s API (workspace
		// activation), and HTTP to workspace pods (agentd dispatch).
		engineLogger = &apiwf.AppEngineLogger{
			LogFn: func(msg string, kv ...any) { log.Info(msg, kv...) },
			ErrFn: func(err error, msg string, kv ...any) { log.Error(msg, err, kv...) },
		}
		wfReconciler = &apiwf.Reconciler{
			Store:        wfStore,
			AgentdClient: newWorkflowAgentdExecutor(proxyHandler),
			Activator: &apiwf.K8sWorkspaceActivator{
				K8sClient: k8sClient,
				Namespace: cfg.Kubernetes.Namespace,
			},
			WorkspaceCreator: &appWorkspaceCreator{
				wsSvc:   svc.Workspace.(*workspace.Service),
				wfStore: wfStore,
			},
			Logger: engineLogger,
		}
		wfScheduler = &apiwf.Scheduler{
			Store: wfStore,
			Activator: &apiwf.K8sWorkspaceActivator{
				K8sClient: k8sClient,
				Namespace: cfg.Kubernetes.Namespace,
			},
			AgentdClient:     newWorkflowAgentdExecutor(proxyHandler),
			Logger:           engineLogger,
			PasswordProvider: proxyHandler,
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
			agentpush.WithPasswordProvider(proxyHandler),
			agentpush.WithModelCache(sharedModelCache),
			agentpush.WithLogger(log),
		)
		secretsHandler.SetAgentPusher(agentPusher)

		// Epic 53: wire the shared agent pusher into the MCP handlers so
		// bound MCP servers reach running pods via live reload-secrets push.
		mcpPushAdapter = func(ctx context.Context, userID, workspaceID string) error {
			_, err := agentPusher.Push(ctx, userID, workspaceID)
			return err
		}
		if adminMcpHandler != nil {
			adminMcpHandler.SetSecretPusher(mcpPushAdapter)
		}
		if userMcpHandler != nil {
			userMcpHandler.SetSecretPusher(mcpPushAdapter)
		}
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
		// Epic 56: soft-unlock handler — same KeyService backing
		// UnlockDEKWithSigningKey for rewriting the durable jwt_sessions
		// row when a Valkey miss + missing/stale durable row needs the
		// user to re-enter their password.
		unlockDEKHandler = handlers.NewUnlockDEKHandler(keyService)

		rkp := newRootKeyProvider(cfg, log)
		// US-57.1 D7: fail-closed guard for the apiKeyProv path.
		if cfg.Security.RootKeyProvider == "aws-kms" && rkp == nil {
			cancel()
			return nil, errors.New("KMS root key provider enabled but masterKek provider failed to initialize — refusing to boot")
		}
		// US-50.7: apiKeyProv uses the "master-kek" purpose string (not
		// "dek-cache") so a Redis compromise cannot help unwrap Postgres
		// API-key ciphertexts. The multi-key provider (US-50.4) also holds the
		// old "dek-cache" key so existing rows still decrypt. New encrypts use
		// "master-kek" (version 2, active); the rotation CLI (US-50.5) re-wraps
		// legacy rows. When rkp is a sealed provider (production) it wraps the
		// raw root key — no purpose string applies, so rkp is used as-is.
		//
		// US-57.1 D7: when rkp is a CompositeProvider (KMS-backed), skip
		// the multi-version upgrade entirely.
		apiKeyProv := rkp
		if _, isComposite := apiKeyProv.(*secrets.CompositeProvider); isComposite {
			// KMS-backed composite — no multi-version upgrade needed.
		} else if apiKeyProv == nil {
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
		imageFactoryHandler = handlers.NewImageFactoryHandler(dbSvc, pgOrgStore)
		imageFactoryAdminHandler = handlers.NewImageFactoryAdminHandler(dbSvc)
		imageFactoryHandler.SetLogger(log)

		// Seed the catalog from the embedded YAML (design/0046 #9).
		if err := imagefactory.SeedCatalog(context.Background(), dbSvc); err != nil {
			log.Warn("image factory catalog seed failed", "error", err.Error())
		}

		// Design 0053 D5/S4: platform-train base sync removed — the base
		// is content-versioned on its own cadence (design 0053 §4.4).
		// Catalog rows advance by operator-reviewed seed updates, not
		// release-train auto-bumping.

		imageRepo := cfg.ImageFactory.ImageRepo
		if imageRepo == "" {
			imageRepo = "ghcr.io/lenaxia/llmsafespaces-images/ws"
		}
		callbackURL := cfg.ImageFactory.CallbackURL
		if callbackURL == "" {
			callbackURL = "/internal/image-factory"
		}
		imageFactoryHandler.SetBuildStore(dbSvc, imageRepo, callbackURL)

		// Wire the GH Actions dispatcher via GitHub App (enables image builds).
		if cfg.ImageFactory.GHDispatcher.AppID != "" && cfg.ImageFactory.GHDispatcher.PrivateKey != "" {
			imageFactoryHandler.SetDispatcher(
				handlers.NewGHActionsDispatcher(
					cfg.ImageFactory.GHDispatcher.AppID,
					cfg.ImageFactory.GHDispatcher.PrivateKey,
					cfg.ImageFactory.GHDispatcher.Owner,
					cfg.ImageFactory.GHDispatcher.Repo,
					cfg.ImageFactory.GHDispatcher.WorkflowID,
					cfg.ImageFactory.GHDispatcher.Ref,
				))
		}

		if cfg.ImageFactory.LLMExplainer.BaseURL != "" {
			imageFactoryHandler.SetFailureExplainer(
				handlers.NewLLMExplainer(handlers.LLMExplainerConfig{
					BaseURL: cfg.ImageFactory.LLMExplainer.BaseURL,
					Model:   cfg.ImageFactory.LLMExplainer.Model,
					APIKey:  cfg.ImageFactory.LLMExplainer.APIKey,
				}))
			imageFactoryHandler.SetExtensionReviewer(dbSvc)
		}
		orgsHandler = handlers.NewOrgsHandler(pgOrgStore, svc.GetAuth())
		orgCredsHandler = handlers.NewOrgCredentialsHandler(pgStore, pgStore, orgCredsProv, svc.GetAuth())
		orgMcpHandler = handlers.NewOrgMCPServersHandler(pgStore, orgCredsProv, pgOrgStore)
		orgMcpHandler.SetSettings(instanceSettings)
		orgMcpHandler.SetLogger(log)
		orgMcpHandler.SetAudit(pgOrgStore)
		orgMcpHandler.SetSecretPusher(mcpPushAdapter)

		// Deferred wiring: now that pgOrgStore is available, install it
		// on the user AND admin MCP handlers (were nil at construction time).
		if userMcpHandler != nil {
			userMcpHandler.SetOrgChecker(pgOrgStore)
			userMcpHandler.SetAudit(pgOrgStore)
		}
		if adminMcpHandler != nil {
			adminMcpHandler.SetAudit(pgOrgStore)
		}

		// Epic 64: deferred audit wiring for workflow + trigger handlers.
		if userWorkflowsHandler != nil {
			userWorkflowsHandler.SetAudit(pgOrgStore)
		}
		if orgWorkflowsHandler != nil {
			orgWorkflowsHandler.SetAudit(pgOrgStore)
		}
		if userTriggersHandler != nil {
			userTriggersHandler.SetAudit(pgOrgStore)
		}
		if orgTriggersHandler != nil {
			orgTriggersHandler.SetAudit(pgOrgStore)
		}

		// US-43.10: OIDC SSO. The service reuses the auth service as the JWT
		// issuer (GenerateToken) and the server KEK (RootKeyProvider) to encrypt
		// the IdP client secret (D17-S4). A dedicated state-signing key is
		// derived from the master secret so PKCE cookies are unforgeable.
		if authSvc, ok := svc.Auth.(*auth.Service); ok {
			stateKey := deriveServerKey("oidc-state-cookie")
			if stateKey != nil {
				ssoSvc, ssoErr := sso.New(pgOrgStore, dbSvc, sso.ServiceConfig{
					TokenIssuer:         authSvc,
					KeyManager:          authSvc,
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

		// Epic 59: WebAuthn passkey registration + login. Constructed only when
		// RPID + RPOrigins are configured; nil handler = routes not registered.
		if cfg.Passkey.RPID != "" && len(cfg.Passkey.RPOrigins) > 0 {
			pkStore := passkey.NewPgStore(secretsPool)
			var pkSessionStore passkey.SessionStore
			if cacheSvc, ok := svc.Cache.(*cache.Service); ok {
				pkSessionStore = passkey.NewCacheSessionStore(cacheSvc.GetClient())
			}
			pkSvc, pkErr := passkey.New(passkey.ServiceConfig{
				RPID:      cfg.Passkey.RPID,
				RPName:    cfg.Passkey.RPName,
				RPOrigins: cfg.Passkey.RPOrigins,
				Store:     pkStore,
				Users:     dbSvc,
				Sessions:  pkSessionStore,
				Logger:    log,
			})
			if pkErr != nil {
				log.Error("failed to construct passkey service", pkErr)
			} else {
				if authSvc, ok := svc.Auth.(*auth.Service); ok {
					passkeyHandler = handlers.NewPasskeyHandler(pkSvc, authSvc, dbSvc, cfg.Auth.TokenDuration, cfg.Auth.CookieName, cfg.OrgSubdomainRouting.CookieDomain)
				}
			}
		}

		wsSvc, wsSvcOk := svc.Workspace.(*workspace.Service)
		if wsSvcOk {
			wsSvc.SetCredentialProvisioner(pgStore)
			wsSvc.SetSecretAutoProvisioner(secretService)
			wsSvc.SetOrgStore(pgOrgStore)
			// Image-factory launch integration (design/0046). Wrap dbSvc
			// so database.ErrNotFound is translated to the workspace
			// package's ErrConfigNotLaunchable sentinel — keeps the
			// workspace service decoupled from the database package.
			wsSvc.SetImageFactoryStore(&launchableConfigAdapter{store: dbSvc})
			// Default-image hierarchy: user preference → org policy → platform.
			// The org tier reads from policyChecker (already wired via
			// SetPolicyChecker) — the defaultRuntime key is a standard
			// org_policies entry (migration 000015).
			wsSvc.SetUserSettings(userSettings)
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
		// Wire the instance settings reader so the bootstrap response carries
		// workspace.allowedExternalDirectories (default ["/tmp/*"]) — agentd
		// materializes it into /sandbox-runtime/allowed-dirs.json and the
		// AgentConfigWriter injects mode.permissions.external_directory
		// allow-rules so agents stop prompting for /tmp/* on every session.
		podBootstrapHandler.SetSettingsReader(instanceSettings)
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

	// When Turnstile is enabled, the CSP must allow Cloudflare's
	// challenges.cloudflare.com origin in script-src (widget script)
	// and frame-src (challenge iframe). Without this, the browser
	// blocks the widget entirely and the register submit button
	// stays permanently disabled — a hard registration lockout.
	// PR #501 review round 4 flagged this at the API layer to match
	// the chart-side fix in the frontend Ingress CSP annotation.
	if cfg.Turnstile.Enabled {
		securityCfg.ContentSecurityPolicy = addTurnstileToCSP(securityCfg.ContentSecurityPolicy)
	}

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

	// Create terminal handler (Epic 14 — WebSocket terminal proxy).
	//
	// G35: same-origin-only by default, with an operator-controlled
	// allowlist for cross-origin deployments. Configured via Helm value
	// `terminal.allowedOrigins`; empty → fail-closed to same-origin.
	terminalHandler := handlers.NewTerminalHandler(
		svc.Cache,
		&k8sWorkspaceGetterAdapter{client: k8sClient, namespace: cfg.Kubernetes.Namespace},
		cfg.Kubernetes.Namespace,
		log,
		cfg.Terminal.AllowedOrigins,
	)

	// Epic 66: Dev Preview handler (authenticated HTTP/WS tunnel to dev servers).
	// The ProxyHandler implements WorkspacePasswordProvider, so the dev-preview
	// handler shares the existing password cache via that interface (OQ3 in the
	// epic — resolved: inject ProxyHandler as the password provider).
	var devPreviewHandler *handlers.DevPreviewHandler
	if proxyHandler != nil {
		devPreviewHandler = handlers.NewDevPreviewHandler(
			&k8sWorkspaceGetterAdapter{client: k8sClient, namespace: cfg.Kubernetes.Namespace},
			proxyHandler,
			cfg.Kubernetes.Namespace,
			log,
			devPreviewConfigFromSettings(ctx, instanceSettings, log),
		)
	}

	// Epic 66 Phase 1: per-workspace preview origins. Wraps the SAME
	// devPreviewHandler (shared proxy machinery, gates, caps). Nil unless
	// config enables it AND the required secret is present — the config
	// loader already fails boot on enabled-without-secret; the nil-check
	// here is defense-in-depth (a nil handler means the engine middleware
	// is never registered: fail-closed, preview hosts 404 at the ingress).
	var previewOriginHandler *handlers.PreviewOriginHandler
	if devPreviewHandler != nil && cfg.PreviewOrigin.Enabled && cfg.PreviewOrigin.TokenSecret != "" {
		previewOriginHandler = handlers.NewPreviewOriginHandler(
			devPreviewHandler,
			handlers.PreviewOriginConfig{
				Enabled:        cfg.PreviewOrigin.Enabled,
				BaseDomain:     cfg.PreviewOrigin.BaseDomain,
				TokenSecret:    []byte(cfg.PreviewOrigin.TokenSecret),
				FrameAncestors: cfg.PreviewOrigin.FrameAncestors,
			},
			svc.GetCache(),
			log,
		)
	}

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
		// US-65.6: wire the status checker factory so agent_reload.go
		// doesn't import pkg/agent/opencode.
		agentReloadHandler.SetStatusCheckerFactory(func(podIP, password string) handlers.SessionStatusChecker {
			return agentoc.NewClient("http://"+podIP, password, log.ZapLogger())
		})
		bulkReloadHandler.SetStatusCheckerFactory(func(podIP, password string) handlers.SessionStatusChecker {
			return agentoc.NewClient("http://"+podIP, password, log.ZapLogger())
		})
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
				modelsHandler.SetRelayChecker(buildRelayChecker(ipResolver, func(ctx context.Context, wsID string) ([]string, error) {
					pw, pwErr := pwGetter.WorkspacePassword(ctx, wsID)
					if pwErr != nil {
						return nil, pwErr
					}
					candidates := []string{}
					secretName := fmt.Sprintf("workspace-pw-%s", wsID)
					if secret, sErr := k8sClient.Clientset().CoreV1().Secrets(cfg.Kubernetes.Namespace).Get(ctx, secretName, metav1.GetOptions{}); sErr == nil {
						if tok := strings.TrimSpace(string(secret.Data["admin-token"])); tok != "" {
							candidates = append(candidates, tok)
						}
					}
					if pw != "" {
						candidates = append(candidates, pw)
					}
					return candidates, nil
				}))
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
		if passkeyHandler != nil {
			passkeyHandler.SetEmailVerifier(verifier)
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

	// US-43.8 + PR #912: org-policy enforcement wiring (ListModels filter,
	// SetModel gate, per-prompt override gate). Extracted so the wiring is
	// unit-testable (see policy_enforcement_wiring_test.go).
	wirePolicyEnforcement(policySvc, modelsHandler, proxyHandler)

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
		// #475: inject the logger so scrapeRouterMetrics' three error paths
		// (request build, HTTP transport, response read) and non-2xx responses
		// surface as Warn lines instead of silently-zero dashboard counters.
		relayAdminHandler.SetLogger(log)
	} else {
		log.Warn("failed to construct LlmsafespacesV1 client, relay admin routes will not be available", "error", err.Error())
	}

	// PlatformInfoHandler reads deployed Deployment image tags for the admin
	// "Versions" tab. Uses the same clientset as RelayAdminHandler and the
	// instance settings for the base-runtime default image.
	platformInfoHandler := handlers.NewPlatformInfoHandler(
		k8sClient.Clientset(),
		cfg.Kubernetes.Namespace,
		instanceSettings,
	)
	platformInfoHandler.SetLogger(log)

	router := server.NewRouter(svc, log, proxyHandler, server.RouterConfig{
		Debug:                           cfg.Logging.Development,
		LoggingConfig:                   server.DefaultRouterConfig().LoggingConfig,
		RateLimitConfig:                 rateLimitCfg,
		PerRouteRateLimitConfig:         server.DefaultRouterConfig().PerRouteRateLimitConfig,
		SecurityConfig:                  securityCfg,
		TracingConfig:                   server.DefaultRouterConfig().TracingConfig,
		SettingsHandler:                 settingsHandler,
		InstanceSettings:                instanceSettings,
		AdminProviderCredentialsHandler: adminProvCredHandler,
		UserProviderCredentialsHandler:  userProvCredHandler,
		ImageFactoryHandler:             imageFactoryHandler,
		ImageFactoryAdminHandler:        imageFactoryAdminHandler,
		AdminMCPServersHandler:          adminMcpHandler,
		OrgMCPServersHandler:            orgMcpHandler,
		UserMCPServersHandler:           userMcpHandler,
		SecretsHandler:                  secretsHandler,
		ModelsHandler:                   modelsHandler,
		WorkspaceEnvHandler:             workspaceEnvHandler,
		UnlockDEKHandler:                unlockDEKHandler,
		OrgsHandler:                     orgsHandler,
		OrgCredentialsHandler:           orgCredsHandler,
		TerminalHandler:                 terminalHandler,
		DevPreviewHandler:               devPreviewHandler,
		PreviewOriginHandler:            previewOriginHandler,
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
		PlatformInfoHandler:             platformInfoHandler,
		AdminSessionHandler:             adminSessionHandler,
		PlatformAdminHandler:            platformAdminHandler,
		InternalOrgStatusHandler:        internalOrgStatusHandler,
		PodBootstrapHandler:             podBootstrapHandler,
		SSOHandler:                      ssoHandler,
		LoginDiscoveryHandler:           loginDiscoveryHandler,
		PasskeyHandler:                  passkeyHandler,
		PasskeyDefaultSignup:            cfg.Passkey.DefaultSignup,
		UserWorkflowsHandler:            userWorkflowsHandler,
		OrgWorkflowsHandler:             orgWorkflowsHandler,
		UserTriggersHandler:             userTriggersHandler,
		OrgTriggersHandler:              orgTriggersHandler,
		WebhookReceiverHandler:          webhookReceiverHandler,
		CookieName:                      cfg.Auth.CookieName,
		CookieDomain:                    cfg.OrgSubdomainRouting.CookieDomain,
		Turnstile: server.TurnstileRouterConfig{
			Enabled:   cfg.Turnstile.Enabled,
			SecretKey: cfg.Turnstile.SecretKey,
			VerifyURL: cfg.Turnstile.VerifyURL,
		},
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
		sessionAlertsSvc:   sessionAlertsSvc,
		instanceSettings:   instanceSettings,
		userSettings:       userSettings,
		asyncAudit:         asyncAudit,
		secretsPool:        secretsPool,
		pendingOrgCleaner:  pendingOrgCleaner,
		jwtSessionJanitor:  jwtSessionJanitor,
		wfReconciler:       wfReconciler,
		wfScheduler:        wfScheduler,
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

	// Epic 64: Start the workflow engine (reconciler + scheduler) as background
	// goroutines. These run in the API server, not the controller — the API has
	// the pgxpool, K8s client, and HTTP connectivity to workspace pods. FOR
	// UPDATE SKIP LOCKED provides multi-replica safety without leader election.
	if a.wfReconciler != nil {
		go func() {
			if err := a.wfReconciler.Start(a.ctx); err != nil {
				a.logger.Error("workflow reconciler stopped", err)
			}
		}()
		go func() {
			if err := a.wfScheduler.Start(a.ctx); err != nil {
				a.logger.Error("workflow scheduler stopped", err)
			}
		}()
		a.logger.Info("workflow engine started (reconciler + scheduler)")
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
			// #739 Gap 2: per-type agent-event counters + unknown-type
			// warns (classifier wired at tracker construction).
			tracker.SetEventMetrics(metricsSvc)
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

	if err := a.sessionAlertsSvc.Start(); err != nil {
		_ = a.sessionAlertsSvc.Stop()
		_ = a.sessionIndexSvc.Stop()
		_ = a.proxyHandler.Stop()
		a.k8sClient.Stop()
		_ = a.services.Stop()
		return fmt.Errorf("failed to start session alerts: %w", err)
	}

	a.logger.Info("Starting HTTP server", "address", a.server.Addr)

	if err := a.server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		return fmt.Errorf("server error: %w", err)
	}

	return nil
}

// crdDiskUsage sources workspace disk usage for the system-notice
// decorator (#944) from the Workspace CRD status — the same numbers the
// controller mirrors from agentd /v1/statusz and the frontend renders.
// No new telemetry; read failures fail open inside the decorator.
type crdDiskUsage struct {
	k8s       *kubernetes.Client
	namespace string
}

var _ systemnotices.WorkspaceDiskUsage = (*crdDiskUsage)(nil)

func (c *crdDiskUsage) DiskUsage(ctx context.Context, workspaceID string) (int64, int64, error) {
	v1Client, err := c.k8s.LlmsafespacesV1()
	if err != nil {
		return 0, 0, fmt.Errorf("disk usage: llmsafespaces client: %w", err)
	}
	ws, err := v1Client.Workspaces(c.namespace).Get(ctx, workspaceID, metav1.GetOptions{})
	if err != nil {
		return 0, 0, fmt.Errorf("disk usage: get workspace %s: %w", workspaceID, err)
	}
	return ws.Status.DiskUsedBytes, ws.Status.DiskTotalBytes, nil
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

	if err := a.sessionAlertsSvc.Stop(); err != nil {
		a.logger.Error("Session alerts shutdown error", err)
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
// resolves podIP + bearer candidates internally, keeping the ModelsHandler
// free of pod/auth concerns (US-29.5 design). Candidates: distinct admin
// token first, workspace password fallback (#887 D5.1 mixed fleet).
func buildRelayChecker(
	ipResolver handlers.PodIPResolver,
	bearerGetter func(context.Context, string) ([]string, error),
) handlers.RelayStateChecker {
	return newRelayChecker(&http.Client{Timeout: 5 * time.Second}, agentd.AgentdAdminPort, ipResolver, bearerGetter)
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
	bearerGetter func(context.Context, string) ([]string, error),
) handlers.RelayStateChecker {
	return func(ctx context.Context, userID, workspaceID string) bool {
		podIP, err := ipResolver.GetWorkspacePodIP(ctx, userID, workspaceID)
		if err != nil || podIP == "" {
			return false
		}
		bearers, err := bearerGetter(ctx, workspaceID)
		if err != nil || len(bearers) == 0 {
			return false
		}
		url := fmt.Sprintf("http://%s:%d/v1/readyz", podIP, port)
		resp, err := handlers.GetWithBearers(ctx, client, url, bearers)
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

// launchableConfigAdapter wraps a database.ImageFactoryStore so that
// database.ErrNotFound is translated to workspace.ErrConfigNotLaunchable.
// This keeps the workspace service decoupled from the database package —
// it only knows the workspace package's sentinel, not the DB layer's.
type launchableConfigAdapter struct {
	store database.ImageFactoryStore
}

func (a *launchableConfigAdapter) GetLaunchableConfigByHash(ctx context.Context, hash string, scope imagefactory.ConfigScope, ownerID, orgID *string) (imagefactory.Config, string, error) {
	cfg, ref, err := a.store.GetLaunchableConfigByHash(ctx, hash, scope, ownerID, orgID)
	if errors.Is(err, database.ErrNotFound) {
		return imagefactory.Config{}, "", workspace.ErrConfigNotLaunchable
	}
	return cfg, ref, err
}

// appWorkspaceCreator implements apiwf.WorkspaceCreator — provisions a new
// workspace for the workflow owner when on_missing_workspace='create' and
// the target workspace is gone. Pins the new workspace as the workflow's
// target_workspace_id so subsequent runs reuse it.
type appWorkspaceCreator struct {
	wsSvc   *workspace.Service
	wfStore *workflows.Store
}

// newWorkflowAgentdExecutor builds the agentd node-dispatch client used by
// both the workflow reconciler and the scheduler. Extracted from New so
// the wiring — especially the PasswordProvider required for authenticated
// dispatch (#762) — is testable without booting PostgreSQL/Redis.
func newWorkflowAgentdExecutor(pw apiinterfaces.WorkspacePasswordProvider) *apiwf.HTTPAgentExecutor {
	return &apiwf.HTTPAgentExecutor{Port: 4097, PasswordProvider: pw}
}

func (c *appWorkspaceCreator) CreateWorkspace(ctx context.Context, workflowID, ownerType, ownerID string) (string, error) {
	wfRow, err := c.wfStore.GetWorkflow(ctx, ownerType, ownerID, workflowID)
	if err != nil {
		return "", fmt.Errorf("lookup workflow for workspace creation: %w", err)
	}

	req := types.CreateWorkspaceRequest{
		Name:    "wf-" + wfRow.Slug,
		Runtime: "python",
	}
	if ownerType == types.WorkflowOwnerOrg {
		orgID := ownerID
		req.OrgID = &orgID
	}

	ws, err := c.wsSvc.CreateWorkspace(ctx, ownerID, req)
	if err != nil {
		return "", fmt.Errorf("create workspace for workflow %s: %w", workflowID, err)
	}

	wsID := ws.ID
	_, err = c.wfStore.UpdateWorkflow(ctx, ownerType, ownerID, workflowID, &workflows.WorkflowUpdate{
		TargetWorkspaceID: &wsID,
	})
	if err != nil {
		return "", fmt.Errorf("pin workspace %s on workflow %s: %w", wsID, workflowID, err)
	}
	return wsID, nil
}
