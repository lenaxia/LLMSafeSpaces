# Epic 12: Usage Metering & Billing

**Status:** Planning (updated 2026-06-12)
**Created:** 2026-05-28
**Depends On:** Epic 6 (Collapse Sandbox into Workspace), Epic 30 (Unified Credential Model — complete, `owner_type`/`owner_id` pattern in production)
**Priority:** High

**Motivation:** LLMSafeSpace needs per-tenant usage tracking for billing, cost attribution, quota enforcement, and abuse detection. The platform must meter compute time, LLM tokens, storage, and API calls — attributable to individual users today, rollable up to organizations in the future (Epic 11).

---

## Current State (as of 2026-06-12)

**What exists:**

| Area | Status | Detail |
|------|--------|--------|
| Fleet-level inference Prometheus metrics | Done | `llmsafespace_inference_{requests,input_tokens,output_tokens,cost_dollars}_total{model_id,provider_id,tier}` in `api/internal/services/metrics/metrics.go`. Fed by `SSETracker.onInference` callback. `workspace_id` intentionally omitted — see cardinality note in `metrics.go:233-237`. |
| Compute time (Prometheus only) | Done | `llmsafespace_workspace_active_seconds_total{workspace_id,user_id,runtime,security_level}` + `llmsafespace_user_active_seconds_total{user_id,runtime,security_level}` in `controller/internal/metrics/metrics.go`. Accumulated every 15s in `phase_active.go:152`. Uses constant 15s, not wall-clock elapsed. Not persisted to DB. |
| Storage (Prometheus only) | Done | `llmsafespace_workspace_storage_bytes`, `llmsafespace_workspace_disk_used_bytes` gauges in controller. PVC allocated size, not actual filesystem usage. Updated every 15s. Not persisted to DB. |
| Phase transitions (Prometheus only) | Done | `llmsafespace_workspace_phase_transitions_total{from_phase,to_phase}` counter in both controller (`reconciler.go:165`) and API (`metrics.go:227`). No duration histogram. |
| Resume/create duration histograms | Done | `llmsafespace_workspace_resume_duration_seconds{resume_type}`, `llmsafespace_workspace_create_duration_seconds{has_packages,has_init_script}` in controller. |
| Session duration histogram | Done | `llmsafespace_session_duration_seconds` in API metrics. |
| Auth failure counter | Done | `llmsafespace_auth_failures_total{reason}` in API metrics. Success path not tracked. |
| Request ID generation | Done | UUID v4 in `request_id.go`, set in Gin context as `"request_id"`, set in response header `X-Request-ID`. `TracingMiddleware` (already wired) performs the same function. |
| Structured JSON logging | Done | `logging.go` now reads `request_id` from Gin context (fixed) and includes `user_id` in response log when set by auth middleware. |
| Billing Grafana dashboard | Done | `charts/llmsafespace/dashboards/billing.json` — 26 panels. 3 panels reference unregistered metrics (see phantom metrics section). |
| Operational Grafana dashboard | Done | `charts/llmsafespace/dashboards/operational.json` — 32 panels. |
| Canary suite | Done | Fission-based serverless framework at `sdks/canary/`. 41 Go scenarios, multi-language (Go/TypeScript/Python), two-tier schedule (1-min shallow, 5-10-min deep). Alerting via Fission HTTP 500 detection. |
| Workspace-count quota | Partial | `router.go` returns 429 when `MAX_WORKSPACES_PER_USER` env var exceeded. Env-var-only; no general usage quota system. |
| Async audit write pattern | Done | `pkg/secrets/pg_secret_store.go:491-632` — `AsyncAuditLogger` with buffered channel, non-blocking send, drain-on-stop, pass-through interface wrapping. Reference implementation for US-12.1. |
| userBroker workspace→owner map | Done | `ProxyHandler` maintains an in-memory `workspaceID → userID` map via `userBroker.RecordWorkspaceOwner()`, populated from CRD watch events at `proxy.go:971`. Available at SSE callback time. |

**What does NOT exist:**

| Area | Gap |
|------|-----|
| Per-user/per-workspace DB metering | No `usage_events` table. No MeteringService. Prometheus counters are operational-grade (lossy on restart, unbounded cardinality at scale), not billing-grade. |
| LLM proxy latency histogram | `llmsafespace_llm_request_duration_seconds` not registered. `doProxy` does not time the round-trip. |
| LLM error classification counter | `llmsafespace_llm_errors_total` not registered. No per-provider/model error counting. |
| Proxy operational counters | `llmsafespace_proxy_rejections_total`, `llmsafespace_proxy_retries_total` not registered. |
| Compute time DB persistence | No `compute_seconds` DB events. Controller restart resets Prometheus counters — billing gap. |
| Reconciliation cron | No mechanism to detect/fill compute time gaps. |
| API call metering middleware | No per-user call counting. |
| Usage API endpoints | No `/api/v1/usage`, `/api/v1/usage/quota`, or admin routes. |
| Billing provider interface | No `pkg/billing/`. No export pipeline. No `billing_accounts` table. |
| Billing webhook | No `/api/v1/webhooks/billing`. No `users.plan_id` column. |
| Generalized audit log | `secret_audit_log` is secrets-specific. No cross-domain audit trail. |
| DLQ infrastructure | No dead-letter queue system. |
| Dependency health metrics | No DB/Redis pool gauges or query latency histograms. Auth success path untracked. |
| Storage metering cron | No DB-persisted `storage_bytes` events. |

**Phantom dashboard metrics (referenced in billing.json, not registered in Go):**

| Metric | Panels | Note |
|--------|--------|------|
| `llmsafespace_user_llm_calls_total` | 12, 16 | No registration anywhere |
| `llmsafespace_workspace_llm_request_duration_seconds` | 22 | Dashboard: "Empty until events-gateway Epic 33" |
| `llmsafespace_workspace_proxy_bytes_total` | 23 | Dashboard: "Empty until events-gateway Epic 33" |

**Stale comment fixed:** `api/internal/services/metrics/metrics.go:238` now references the metering service instead of the phantom `RecordBillingEvent`.

---

## Design Principles

1. **Two-tier observability.** Prometheus is operational: fleet-level, real-time, lossy on restart, cardinality-bounded. PostgreSQL is billing-grade: per-user, durable, auditable, reconcilable. Both tiers are written from the same instrumentation points. Neither substitutes for the other.
2. **Owner-aware from day one.** Every usage event records `owner_id + owner_type`. Reuses the `owner_type`/`owner_id` convention from Epic 30's `provider_credentials` (complete, PR #39). With Epic 11 organizations now complete, billing rolls up without schema migration.
3. **Actor ≠ Owner.** The `actor_id` (who performed the action) is always a user, even within an org-owned workspace. Enables per-member cost breakdown.
4. **Metering never blocks requests.** Usage recording is async (buffered channel → batch write). A metering failure must never degrade the user experience.
5. **At-least-once with idempotency.** Events may be delivered more than once (retries, reconciliation). `idempotency_key` with `ON CONFLICT DO NOTHING` prevents double-counting.
6. **Billing provider owns pricing, plans, and invoices.** We emit raw usage events. The billing provider handles rate cards, plan management, invoicing, and payment.
7. **Reconcilable.** A cron detects and fills gaps caused by process restarts or queue saturation. Every correction is auditable via `source='reconciliation'`.
8. **Cardinality discipline.** The Prometheus billing metrics in the controller (`workspace_active_seconds_total`) carry `workspace_id` labels, which accumulate unboundedly as workspaces are created over time. At scale this becomes a cardinality problem. For billing-grade per-workspace queries, the DB is the authoritative path. Prometheus counters are operational-only.

### Build vs. Buy

| Concern | Decision | Rationale |
|---------|----------|-----------|
| Usage event storage | Build | Source of truth for disputes, reconciliation, quota. Must survive provider migration. |
| Async buffered writes | Build | ~80 lines of Go. Follow `AsyncAuditLogger` in `pkg/secrets/pg_secret_store.go:491-632`. |
| Idempotent dedup | Build | One SQL clause: `ON CONFLICT DO NOTHING`. |
| Dead-letter queue | Build | A Postgres table. Simpler than a message broker. |
| Quota enforcement | Build | In-path real-time. Cannot round-trip to an external provider per request. |
| Compute time reconciliation | Build | K8s events have 1h retention — not billing-grade. No provider offers this. |
| Pricing / rate cards | Buy | Billing provider's core job. |
| Plan/subscription management | Buy | Billing provider handles upgrades, downgrades, proration. |
| Invoice generation | Buy | Billing provider handles entirely. |
| Credits / refunds | Buy | Emit event to provider; they handle accounting. |
| Audit log | Generalize | Extend existing `secret_audit_log` table pattern to a shared `audit_log` with a `domain` discriminator. |

---

## Architecture

### LLM Token Metering Data Flow

Token data is not available synchronously in the proxy response. It arrives asynchronously via SSE from the opencode agent. The `SSETracker` is the correct and only metering point for tokens.

```
opencode agent (workspace pod :4096)
  emits: session.updated SSE event { tokens.input, tokens.output, cost, model }
         │
         ▼
SSETracker.handleSessionUpdated()  [session_tracker.go:401]
  delta-tracks tokens per workspace:session
  fires: onInference(workspaceID, modelID, providerID, inputTokens, outputDelta, costDelta)
         │
         ├─→ metricsSvc.RecordInference(...)      // fleet Prometheus (exists)
         └─→ meteringSvc.RecordTokens(...)        // per-user DB (NEW, US-12.2)
              resolves ownerID, isCanary via proxyHandler.GetWorkspaceInfo(workspaceID)
              [uses userBroker in-memory map — no K8s round-trip]

SEPARATELY: proxyToWorkspace [proxy.go, post-doProxy success]
  fires: meteringSvc.RecordRequest(...)           // llm_request count (NEW, US-12.2)
  captures: workspaceID, sessionID, userID (from c.GetString("userID")), latency, status
```

**Why `userBroker`, not a CRD lookup:** The `onInference` callback fires in a background SSETracker goroutine with no request context and no Kubernetes client. `ProxyHandler` already maintains `userBroker`, an in-memory map of `workspaceID → ownerUserID`, populated from the CRD watcher's phase-change events (`proxy.go:971`). This is the correct lookup path: O(1), no I/O, always consistent with current ownership.

### Compute Time Metering Data Flow

The controller has no database connection and will not be given one. The controller's domain is Kubernetes state; the API service owns the database. Compute time metering flows through the API service's reconciliation cron, not from the controller directly.

```
Controller reconciler — phase_active.go (every 15s requeue):
  accumulateActiveSeconds(workspace, requeueActive)    // Prometheus counter (exists)
  [no DB write — controller has no DB connection]

API service — onPhaseChange callback [proxy.go:960]:
  fires on every CRD phase change (already wired)
  emits workspace_lifecycle_event to DB

API service — compute time reconciliation cron (every 5 min, US-12.4):
  enumerates Active workspaces from CRD watcher in-memory cache
  computes elapsed seconds since last recorded compute event in DB
  emits catch-up compute_seconds events for any gap > 45s
  source = 'reconciliation' for all events from this path
```

**Why not add DB to the controller:** Adding a PostgreSQL client to the controller binary increases its operational footprint, adds a new failure mode (DB unavailable → controller fails to reconcile), and violates the separation of concerns between the Kubernetes operator and the application-level services. The controller's sole responsibility is Kubernetes state reconciliation. The reconciliation cron in the API service achieves the same correctness guarantee (at-least-once, idempotent) without coupling the controller to the database tier.

### Async Buffered Writer Data Flow

```
Instrumentation point
  meteringSvc.Record(event)   [non-blocking, no return value]
        │
        ▼
  buffered channel (capacity 4096)
  if full: log warning, increment llmsafespace_metering_events_dropped_total, return

        │ drained by background goroutine
        ▼
  batch accumulator (up to 100 events OR 1s elapsed, whichever comes first)
        │
        ▼
  INSERT INTO usage_events ... ON CONFLICT (idempotency_key) DO NOTHING
        │
   success: increment llmsafespace_metering_events_recorded_total
   failure: INSERT INTO usage_events_dlq (payload, error_message)
            if DLQ insert also fails: log ERROR, increment dropped counter
            [reconciliation cron is the safety net for compute events]

DLQ reaper goroutine (every 60s):
  SELECT id, payload, retry_count FROM usage_events_dlq
    WHERE resolved_at IS NULL AND retry_count < 5
    ORDER BY last_retried_at ASC NULLS FIRST LIMIT 50
  for each entry:
    attempt INSERT INTO usage_events ... ON CONFLICT DO NOTHING
    on success: UPDATE usage_events_dlq SET resolved_at=NOW(), resolution='reprocessed'
    on failure: UPDATE usage_events_dlq SET retry_count=retry_count+1, last_retried_at=NOW()
  entries reaching retry_count=5: mark resolution='dead', increment llmsafespace_metering_dlq_dead_total
```

**DLQ reaper does direct DB inserts — not re-enqueue via Record().** Re-enqueueing via `Record()` would lose retry count tracking (the channel is anonymous — the reaper cannot know if the batch writer succeeded or failed for a specific DLQ entry). The reaper is the only component with DLQ entry IDs, so it owns the retry lifecycle.

### Quota Enforcement Data Flow

```
Quota check runs BEFORE proxying, on the LLM request path.
It is SYNCHRONOUS — CheckQuota() blocks until it gets a Redis response.
This is intentional: a fast Redis round-trip (<2ms p99) is acceptable
on the proxy path; a stale quota counter is not acceptable for billing limits.

proxyToWorkspace:
  owner = resolveBillingOwner(userID)
  allowed, remaining, err = meteringSvc.CheckQuota(ctx, owner, "llm_request")
  if !allowed: return 429

After successful proxy:
  meteringSvc.IncrementQuotaCounter(ctx, owner, "llm_request")  // Redis INCR, sync
  meteringSvc.Record(usageEvent)  // async DB write

CheckQuota() and IncrementQuotaCounter() operate on Redis directly.
Record() writes to the DB async. These are separate concerns.
Record() must never call IncrementQuotaCounter internally — Record() is fire-and-forget.
```

### Billing Export Data Flow

```
Export goroutine (every 5 min, runs in API service):
  SELECT ... FROM usage_events WHERE id > last_exported_id ORDER BY id LIMIT 500
  map each event → UsageExportEvent{ExternalCustomerID, ...}
    (requires JOIN or lookup against billing_accounts)
  call BillingProvider.ReportUsage(batch)
  on success: UPDATE billing_export_cursor SET last_exported_id=max(batch.id)
  on failure: log error, do not advance cursor, retry on next tick
```

### Owner Resolution

```go
// resolveBillingOwner returns the billing owner for a workspace operation.
// Today: always returns the user who owns the workspace.
// Epic 11: will check workspace.Spec.Owner.OrgID first.
//
// For the proxy path: userID is c.GetString("userID") — never c.MustGet.
// For the SSETracker onInference path: ownerID is from
// proxyHandler.GetWorkspaceInfo(workspaceID).ownerID (userBroker map, zero I/O).
func resolveBillingOwner(userID string) BillingOwner {
    return BillingOwner{ID: userID, Type: OwnerTypeUser}
}
```

Uses the same `owner_type`/`owner_id` convention as `provider_credentials` from Epic 30.

---

## Migration Numbering

Current highest migration: `000023`. Next available: `000024`.

Epic 12 introduces multiple migrations. Each must be added to **both** `api/migrations/` and `charts/llmsafespace/migrations/` with both `.up.sql` and `.down.sql` files. Repolint enforces byte-for-byte equality between the two directories.

Suggested sequence:

| Number | File | Creates |
|--------|------|---------|
| 000024 | `000024_metering_tables` | `usage_events`, `usage_events_dlq` |
| 000025 | `000025_workspace_lifecycle_events` | `workspace_lifecycle_events` |
| 000026 | `000026_usage_limits` | `usage_limits`, `users.plan_id` |
| 000027 | `000027_billing_accounts` | `billing_accounts`, `billing_export_cursor` |
| 000028 | `000028_audit_log` | `audit_log` |

---

## Data Model

### usage_events

Core metering table. Append-only. Source of truth for billing, disputes, and quota.

```sql
CREATE TABLE usage_events (
    id              BIGSERIAL PRIMARY KEY,
    idempotency_key TEXT UNIQUE,

    -- Who is billed
    owner_id        TEXT NOT NULL,
    owner_type      TEXT NOT NULL DEFAULT 'user' CHECK (owner_type IN ('user', 'org')),

    -- Who acted (always a user)
    actor_id        TEXT NOT NULL,

    -- What resource
    workspace_id    TEXT,   -- NULL for api_call events not tied to a workspace

    -- Classification
    event_type      TEXT NOT NULL
        CHECK (event_type IN ('compute_seconds','llm_request','llm_tokens','storage_bytes','api_call')),
    event_subtype   TEXT,   -- input_tokens | output_tokens | active | read | write

    -- Quantity
    quantity        BIGINT NOT NULL CHECK (quantity >= 0),

    -- Pricing context (immutable at write time)
    resource_tier   TEXT,   -- small | medium | large | gpu (from workspace Spec)
    region          TEXT,   -- future multi-region support

    -- Dimensional detail
    metadata        JSONB NOT NULL DEFAULT '{}',

    -- Dispute resolution context
    request_context JSONB,  -- {ip, api_key_id, session_id, request_id}

    -- Provenance
    source          TEXT NOT NULL DEFAULT 'api'
        CHECK (source IN ('api','controller','cron','reconciliation')),

    -- Timing
    event_time      TIMESTAMPTZ NOT NULL,
    recorded_at     TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    period          DATE NOT NULL DEFAULT CURRENT_DATE
);

CREATE INDEX idx_usage_owner_period  ON usage_events(owner_id, owner_type, period);
CREATE INDEX idx_usage_actor_period  ON usage_events(actor_id, period);
CREATE INDEX idx_usage_workspace     ON usage_events(workspace_id, period)
    WHERE workspace_id IS NOT NULL;
CREATE INDEX idx_usage_type_period   ON usage_events(event_type, period);
CREATE INDEX idx_usage_idempotency   ON usage_events(idempotency_key)
    WHERE idempotency_key IS NOT NULL;
```

### workspace_lifecycle_events

Immutable phase transition history. Supports billing dispute resolution and compute time gap detection.

```sql
CREATE TABLE workspace_lifecycle_events (
    id              BIGSERIAL PRIMARY KEY,
    workspace_id    TEXT NOT NULL,
    owner_id        TEXT NOT NULL,
    owner_type      TEXT NOT NULL DEFAULT 'user',
    from_phase      TEXT,           -- NULL for initial creation
    to_phase        TEXT NOT NULL,
    resource_tier   TEXT,
    event_time      TIMESTAMPTZ NOT NULL,
    recorded_at     TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE INDEX idx_wle_workspace ON workspace_lifecycle_events(workspace_id, event_time);
CREATE INDEX idx_wle_owner     ON workspace_lifecycle_events(owner_id, owner_type, event_time);
```

### usage_limits

Quota configuration per owner. Admin-managed, Redis-cached for enforcement.

```sql
CREATE TABLE usage_limits (
    id           BIGSERIAL PRIMARY KEY,
    owner_id     TEXT NOT NULL,
    owner_type   TEXT NOT NULL DEFAULT 'user',
    event_type   TEXT NOT NULL,
    period_type  TEXT NOT NULL DEFAULT 'monthly' CHECK (period_type IN ('daily','monthly','lifetime')),
    max_quantity BIGINT NOT NULL CHECK (max_quantity > 0),
    created_at   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE(owner_id, owner_type, event_type, period_type)
);
```

Note: `scope`/`scope_id` per-actor limits are deferred to Epic 11 (org member breakdown). The table supports it via a future column addition without breaking existing rows.

### billing_accounts

Maps our owners to external billing provider customers.

```sql
CREATE TABLE billing_accounts (
    id                       BIGSERIAL PRIMARY KEY,
    owner_id                 TEXT NOT NULL,
    owner_type               TEXT NOT NULL DEFAULT 'user',
    provider                 TEXT NOT NULL,   -- 'stripe' | 'lago' | 'noop'
    external_customer_id     TEXT NOT NULL,
    external_subscription_id TEXT,
    status                   TEXT NOT NULL DEFAULT 'active',
    created_at               TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at               TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE(owner_id, owner_type, provider)
);
```

### billing_export_cursor

Incremental sync high-water mark. One row per provider.

```sql
CREATE TABLE billing_export_cursor (
    provider            TEXT PRIMARY KEY,
    last_exported_id    BIGINT NOT NULL DEFAULT 0,
    last_exported_at    TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
```

### usage_events_dlq

Dead-letter queue for failed batch writes. Managed exclusively by the DLQ reaper goroutine.

```sql
CREATE TABLE usage_events_dlq (
    id              BIGSERIAL PRIMARY KEY,
    payload         JSONB NOT NULL,
    error_message   TEXT NOT NULL,
    retry_count     INTEGER NOT NULL DEFAULT 0 CHECK (retry_count >= 0),
    first_failed_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    last_retried_at TIMESTAMPTZ,
    resolved_at     TIMESTAMPTZ,
    resolution      TEXT CHECK (resolution IN ('reprocessed','discarded','dead'))
);
CREATE INDEX idx_dlq_unresolved ON usage_events_dlq(last_retried_at ASC NULLS FIRST)
    WHERE resolved_at IS NULL;
```

### users.plan_id

```sql
ALTER TABLE users ADD COLUMN plan_id TEXT NOT NULL DEFAULT 'free';
```

### audit_log

Generalizes `secret_audit_log` to all admin-initiated changes. New code writes to `audit_log`; the existing `secret_audit_log` table is retained for backwards read compatibility until migrated.

```sql
CREATE TABLE audit_log (
    id          BIGSERIAL PRIMARY KEY,
    actor_id    TEXT NOT NULL,
    domain      TEXT NOT NULL CHECK (domain IN ('billing','secrets','admin')),
    action      TEXT NOT NULL,
    target_id   TEXT,
    metadata    JSONB NOT NULL DEFAULT '{}',
    created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE INDEX idx_audit_domain ON audit_log(domain, created_at DESC);
CREATE INDEX idx_audit_actor  ON audit_log(actor_id, created_at DESC);
```

---

## Go Interfaces

Interface Segregation: each consumer depends only on the methods it needs. The concrete `metering.Service` satisfies all.

```go
// EventRecorder is the fire-and-forget write path.
// Implemented by metering.Service. Used by: SSETracker callback, proxy handler,
// API call metering middleware, storage metering cron.
// Never blocks. No return value. Failures handled internally by the batch writer.
type EventRecorder interface {
    Record(event UsageEvent)
}

// QuotaChecker is the synchronous in-path enforcement query.
// Implemented by metering.Service. Used by: proxy handler before forwarding.
// Blocks until Redis responds. Must complete in <5ms p99.
type QuotaChecker interface {
    CheckQuota(ctx context.Context, owner BillingOwner, eventType string) (allowed bool, remaining int64, err error)
    IncrementQuotaCounter(ctx context.Context, owner BillingOwner, eventType string) error
}

// UsageReader is the read path for user-facing and admin API endpoints.
// Implemented by metering.Service.
type UsageReader interface {
    GetUsage(ctx context.Context, owner BillingOwner, from, to time.Time) (*UsageReport, error)
    GetUsageByWorkspace(ctx context.Context, owner BillingOwner, workspaceID string, from, to time.Time) (*UsageReport, error)
    GetQuotaStatus(ctx context.Context, owner BillingOwner) ([]QuotaStatus, error)
}

// BillingExporter is the export pipeline.
// Implemented by metering.Service.
type BillingExporter interface {
    ExportUsage(ctx context.Context) (exported int, err error)
}
```

### Core Types

```go
// OwnerType mirrors the provider_credentials convention from Epic 30.
type OwnerType string
const (
    OwnerTypeUser OwnerType = "user"
    OwnerTypeOrg  OwnerType = "org"
)

type BillingOwner struct {
    ID   string
    Type OwnerType
}

type UsageEvent struct {
    IdempotencyKey string
    Owner          BillingOwner
    ActorID        string        // always a userID
    WorkspaceID    string        // empty for non-workspace events
    EventType      string        // compute_seconds | llm_request | llm_tokens | storage_bytes | api_call
    EventSubtype   string        // input_tokens | output_tokens | active | read | write
    Quantity       int64
    ResourceTier   string        // from workspace Spec.SecurityLevel or similar
    Region         string        // reserved for multi-region
    Metadata       map[string]any  // JSONB; matches secret_audit_log metadata pattern
    RequestContext map[string]any  // {ip, api_key_id, session_id, request_id}
    Source         string        // api | controller | cron | reconciliation
    EventTime      time.Time
}

type UsageReport struct {
    OwnerID     string
    OwnerType   OwnerType
    PeriodFrom  time.Time
    PeriodTo    time.Time
    Totals      map[string]int64             // event_type → total
    ByWorkspace map[string]map[string]int64  // workspace_id → event_type → total
    ByDay       map[string]map[string]int64  // date string → event_type → total
}

type QuotaStatus struct {
    EventType  string
    PeriodType string
    Limit      int64
    Used       int64
    Remaining  int64
    ResetsAt   time.Time
}
```

### BillingProvider Interface

```go
// pkg/billing/provider.go

// BillingProvider abstracts the external billing system.
// Ships as NoopBillingProvider; implement when provider is chosen.
type BillingProvider interface {
    ReportUsage(ctx context.Context, events []UsageExportEvent) (reportedIDs []int64, err error)
    CreateCustomer(ctx context.Context, owner BillingOwner, email string) (externalID string, err error)
    SuspendCustomer(ctx context.Context, externalID string) error
}

type UsageExportEvent struct {
    ExternalCustomerID string
    EventType          string
    Quantity           int64
    Timestamp          time.Time
    IdempotencyKey     string
    Properties         map[string]string
}

type NoopBillingProvider struct{}
// All methods return nil errors and empty results.
```

---

## Event Types & Metering Points

| event_type | event_subtype | unit | source | Trigger | Code location |
|-----------|---------------|------|--------|---------|---------------|
| `compute_seconds` | `active` | seconds | reconciliation | API reconciliation cron detects Active workspace gap | API service cron (US-12.4) |
| `llm_request` | `message` | 1 | api | After successful `doProxy()` in `proxyToWorkspace` | `proxy.go` post-doProxy success block |
| `llm_tokens` | `input` | token count | api | `onInference` callback from `SSETracker.handleSessionUpdated` | `session_tracker.go:446` |
| `llm_tokens` | `output` | token count | api | `onInference` callback (delta-tracked) | `session_tracker.go:446` |
| `storage_bytes` | `pvc` | bytes (allocated) | cron | Daily: emit PVC allocated size per workspace | API service daily cron (US-12.11) |
| `api_call` | `read` | 1 | api | Batched: every authenticated GET returning 2xx | API metering middleware (US-12.5) |
| `api_call` | `write` | 1 | api | Batched: every authenticated POST/PUT/DELETE 2xx | API metering middleware (US-12.5) |

**Compute time note:** The controller accumulates `compute_seconds` Prometheus counters via a constant 15s addition per reconcile. This is a deliberate approximation — it systematically undercounts during queue saturation. For billing-grade compute time, the reconciliation cron (US-12.4) is the authoritative path: it computes elapsed time using `workspace_lifecycle_events` timestamps and the last recorded `compute_seconds` event, using actual wall-clock time rather than the constant 15s.

---

## API Surface

### User Endpoints

| Endpoint | Method | Auth | Purpose |
|----------|--------|------|---------|
| `/api/v1/usage` | GET | JWT/API Key | Own usage summary (`?from=&to=`, defaults to current billing period) |
| `/api/v1/usage/workspaces/:id` | GET | JWT/API Key | Per-workspace usage (ownership enforced) |
| `/api/v1/usage/quota` | GET | JWT/API Key | Current quota status: limits, used, remaining, resets_at per event_type |

### Admin Endpoints

| Endpoint | Method | Auth | Purpose |
|----------|--------|------|---------|
| `/api/v1/admin/usage/:ownerId` | GET | JWT (admin) | Any owner's usage |
| `/api/v1/admin/limits/:ownerId` | PUT | JWT (admin) | Set/update quota limits |
| `/api/v1/admin/limits/:ownerId` | GET | JWT (admin) | Get quota limits |
| `/api/v1/admin/billing/status` | GET | JWT (admin) | Export cursor positions, DLQ size |
| `/api/v1/admin/billing/dlq` | GET | JWT (admin) | View unresolved DLQ entries |
| `/api/v1/admin/billing/dlq/:id/retry` | POST | JWT (admin) | Trigger immediate retry of one DLQ entry |
| `/api/v1/admin/billing/dlq/:id/discard` | POST | JWT (admin) | Mark DLQ entry discarded + audit log |

### Webhook Endpoint

| Endpoint | Method | Auth | Purpose |
|----------|--------|------|---------|
| `/api/v1/webhooks/billing` | POST | Provider HMAC signature | Plan changes, payment events → update `users.plan_id` + audit log |

---

## Failure Modes & Recovery

| Failure | Detection | Recovery |
|---------|-----------|----------|
| Metering buffer full (Postgres down, high write rate) | `llmsafespace_metering_events_dropped_total` rising | Compute gaps filled by reconciliation cron; token events are lossy (acceptable) |
| API crash mid-session | Reconciliation cron detects compute gap | Catch-up event with `source='reconciliation'` |
| Double-counted event on retry | `idempotency_key` UNIQUE constraint | `ON CONFLICT DO NOTHING` — safe to retry indefinitely |
| Postgres down (batch write fails) | `llmsafespace_metering_events_failed_total` rising | Batch written to DLQ; DLQ reaper retries on recovery |
| DLQ reaper fails (Postgres down) | `llmsafespace_metering_dlq_size` rising | DLQ retries resume when Postgres recovers |
| DLQ entry permanently failing (bad data) | retry_count reaches 5 | Marked `resolution='dead'`; `llmsafespace_metering_dlq_dead_total` fires alert |
| Wrong token count (extraction bug) | User dispute vs provider dashboard | Admin issues credit via billing provider |
| Controller restart (Prometheus counter reset) | Reconciliation cron detects gap (no recent `compute_seconds` events) | Catch-up events fill the gap |
| Billing export fails | `llmsafespace_metering_export_lag_seconds` rising | Retry from cursor on next tick (export is idempotent) |

### Decision: refunds are provider-side, the local ledger is append-only (#767)

The `usage_events` constraints — `CHECK (quantity >= 0)` and the
`event_type` enum — are **deliberate**, not emergent. They encode the
Build/Buy split above ("Credits / refunds | Buy"): `usage_events` is an
append-only raw-consumption ledger for disputes, reconciliation, and
quota; reversals and credits are accounting and belong to the billing
provider. Stripe's metered `usagerecord` accepts negative quantities,
so the reversal primitive exists on the provider side once a non-noop
provider is live (the default `NoopBillingProvider` charges nobody and
cannot refund).

Consequences of this decision:

- A turn that fails after partial token consumption (pod OOM, agent
  crash mid-stream) IS billed for the tokens the provider consumed.
  The platform was itself charged for them; the local ledger records
  consumption, not satisfaction.
- An admin correcting a wrong charge issues the credit in the billing
  provider's dashboard (the "Wrong token count" row above). The local
  ledger keeps the original event; disputes are resolved against it,
  not edited through it.

**Deferred until billing goes live** (tracked in #767): the
turn-failure → negative-quantity `ReportUsage` trigger — correlating
"tokens billed" with "response never delivered" requires turn-boundary
tracking that does not exist yet, and it is only meaningful with a real
provider attached. If product later decides pod-crash failures should
be refunded by the platform absorbing the cost, that is a NEW decision
overriding this one — it would need schema changes (both CHECKs) and a
net-vs-gross distinction in every `SUM(quantity)` consumer.

---

## User Stories

### US-12.1: Metering Service Foundation

**Goal:** Implement the core buffered writer, DLQ reaper, and DB schema.

**Reference implementation:** `AsyncAuditLogger` in `pkg/secrets/pg_secret_store.go:491-632`. The metering service is structurally similar: buffered channel, drain goroutine, non-blocking send, pass-through wrapping.

**Migration files (both required per repolint):**
- `api/migrations/000024_metering_tables.up.sql` + `.down.sql`
- `charts/llmsafespace/migrations/000024_metering_tables.up.sql` + `.down.sql`
- Creates: `usage_events`, `usage_events_dlq`

**New package:** `api/internal/services/metering/`

**Service wiring (follow Services struct pattern in `api/internal/services/services.go`):**
1. Add `MeteringService` interface to `api/internal/interfaces/interfaces.go` exposing `Start()`, `Stop()`, and the `EventRecorder`/`QuotaChecker`/`UsageReader`/`BillingExporter` methods.
2. Add `Metering interfaces.MeteringService` field to `Services` struct and `GetMetering() interfaces.MeteringService` getter — mirroring the pattern at `services.go:20-53`.
3. Update the `interfaces.Services` interface at `interfaces.go:155-162` to add `GetMetering()`.
4. In `services.New()`: instantiate `metering.New(db, cache, log)` and assign.
5. In `services.Start()`: call `s.Metering.Start()` **after** `s.Database.Start()` and `s.Cache.Start()` (both must be running first; metering needs DB for batch writes and Redis for quota counters).
6. In `services.Stop()`: call `s.Metering.Stop()` **before** `s.Database.Stop()` — the stop must drain the channel buffer and flush any pending batch to the DB before the DB connection closes.

**`metering.Service` implementation:**
- Channel capacity: 4096 (matches `AsyncAuditLogger`'s default)
- Batch writer: flush at 100 events or 1s, whichever comes first; single multi-row INSERT with `ON CONFLICT (idempotency_key) DO NOTHING`
- On batch INSERT failure: write batch to `usage_events_dlq` as JSONB; if DLQ write also fails: log ERROR + increment `llmsafespace_metering_events_dropped_total`
- DLQ reaper: goroutine sleeping 60s between runs; SELECT unresolved entries (retry_count < 5), attempt direct INSERT into `usage_events`, update DLQ row on each outcome; dead after 5 retries

**Canary exclusion — call site responsibility:**
The `Record()` method does not check canary status; it has no workspace CRD reference. Canary exclusion is enforced at each call site:
- `proxy.go` post-doProxy: `workspace` variable is in scope — check `workspace.Labels["llmsafespace.dev/canary"] == "true"` before calling `Record()`.
- `onInference` callback in `app.go:457`: canary status is not available from the `workspaceID` string alone. Extend `ProxyHandler.GetWorkspaceOwner()` to a broader `GetWorkspaceInfo(workspaceID string) (ownerID string, isCanary bool)` that reads both owner and canary flag from the `userBroker`. The `userBroker.RecordWorkspaceOwner()` call at `proxy.go:971` (which fires on every phase change) already has the full workspace CRD in scope — extend it to also record `isCanary`. The `UserEventBroker` (in `event_broker_user.go`) needs a corresponding `IsCanary(workspaceID string) bool` method and an internal map.

**Prometheus metrics (all `llmsafespace_metering_` prefix, registered in metering package):**
- `llmsafespace_metering_events_recorded_total{event_type,source}` — Counter
- `llmsafespace_metering_events_failed_total{event_type,error_type}` — Counter
- `llmsafespace_metering_events_dropped_total` — Counter (buffer full or DLQ also failed)
- `llmsafespace_metering_batch_write_duration_seconds` — Histogram
- `llmsafespace_metering_dlq_size` — Gauge (sampled by DLQ reaper after each run)
- `llmsafespace_metering_dlq_dead_total` — Counter

**Acceptance Criteria:**
- `Record()` returns immediately; never blocks regardless of channel state
- Events reach DB within 2s of `Record()` under normal load
- Duplicate `idempotency_key` deduplicated silently by `ON CONFLICT`
- DB failure path: batch → DLQ table; DLQ reaper retries and succeeds on DB recovery
- DLQ reaper failure path: retry_count=5 → marked `dead`, `llmsafespace_metering_dlq_dead_total` incremented
- Canary workspace events not persisted (enforced at call sites, not inside `Record()`)
- Integration test: Record 1000 events with unique keys → all in DB within 5s
- Integration test: Simulate DB failure → verify DLQ entries created → restore DB → verify reaper drains DLQ

---

### US-12.2: LLM Request & Token Metering

**Goal:** Wire per-user DB metering events from the existing `onInference` callback and proxy success path. Add LLM operational Prometheus metrics.

**Prerequisites in proxy.go:**
Add `GetWorkspaceInfo(workspaceID string) (ownerID string, isCanary bool)` to `ProxyHandler` as a thin wrapper:
```go
func (h *ProxyHandler) GetWorkspaceInfo(workspaceID string) (ownerID string, isCanary bool) {
    if h.userBroker == nil {
        return "", false
    }
    return h.userBroker.WorkspaceOwner(workspaceID), h.userBroker.IsCanary(workspaceID)
}
```
`userBroker.IsCanary()` reads from a new internal `canary map[string]bool` populated alongside `recordOwner` in `RecordWorkspaceOwner()`. The `onPhaseChange` call site at `proxy.go:971` already has the full `*v1.Workspace` CRD — extend the call to also pass the canary label value.

**Token metering** — extend the `onInference` closure in `app.go:457`:
```go
tracker.SetOnInference(func(workspaceID, modelID, providerID string, inputTokens, outputTokens int64, costDollars float64) {
    metricsSvc.RecordInference(modelID, providerID, inputTokens, outputTokens, costDollars)
    ownerID, isCanary := proxyHandler.GetWorkspaceInfo(workspaceID)
    if isCanary || ownerID == "" {
        return  // skip canary or unknown-owner events
    }
    owner := metering.BillingOwner{ID: ownerID, Type: metering.OwnerTypeUser}
    meteringSvc.Record(metering.UsageEvent{
        Owner: owner, ActorID: ownerID, WorkspaceID: workspaceID,
        EventType: "llm_tokens", EventSubtype: "input",
        Quantity: inputTokens, Source: "api", EventTime: time.Now(),
        Metadata: map[string]any{"model_id": modelID, "provider_id": providerID},
    })
    if outputTokens > 0 {
        meteringSvc.Record(/* output event, same pattern */)
    }
})
```
`meteringSvc` is obtained via `services.GetMetering().(metering.EventRecorder)` (type assertion, same pattern as `metricsSvc` at `app.go:456`).

**Request metering** — add to `proxyToWorkspace` (`proxy.go`) post-`doProxy()` success block:
- Use `c.GetString("userID")` (not `MustGet` — `MustGet` panics; while this path is always auth-guarded, defensive code is preferred). If empty, skip metering and log a warning.
- Skip if `workspace.Labels["llmsafespace.dev/canary"] == "true"` (workspace is in scope at this point).
- Emit `llm_request/message` event with request context `{ip, request_id, session_id}`.

**LLM proxy Prometheus metrics** — instrument `doProxy()` and `proxyToWorkspace()`:

`doProxy()` currently returns `nil` in two cases that are not transport errors but indicate upstream problems:
- HTTP 401 from upstream (`proxy.go:642-653`): converts to 502 and returns `nil`. Caller sees `proxyErr == nil` but `c.Writer.Status() == 502`. To emit `upstream_auth` error count, check `c.Writer.Status() == http.StatusBadGateway` after `doProxy()` returns `nil`.
- Non-2xx upstream (any status ≥ 300): forwarded to client, `doProxy()` returns `nil`. To emit `upstream_error`, check `c.Writer.Status() >= 300` after `doProxy()` returns `nil`.

Instrument `proxyToWorkspace` immediately after the `doProxy()` call:

```go
start := time.Now()
proxyErr := h.doProxy(c, podIP, targetPath, password, bodyBytes, stripPatch)
elapsed := time.Since(start)
status := c.Writer.Status()

// Always record latency
recordLLMRequestDuration(elapsed, status)

// Classify and count errors
if proxyErr != nil {
    if isConnectionError(proxyErr) {
        llmErrorsTotal.WithLabelValues("connection_refused").Inc()
    } else if isTimeoutError(proxyErr) {
        llmErrorsTotal.WithLabelValues("timeout").Inc()
    } else {
        llmErrorsTotal.WithLabelValues("other").Inc()
    }
} else if status == http.StatusBadGateway {
    llmErrorsTotal.WithLabelValues("upstream_auth").Inc()
} else if status >= 300 && status != http.StatusSwitchingProtocols {
    llmErrorsTotal.WithLabelValues("upstream_error").Inc()
}
```

New Prometheus metrics (in `api/internal/services/metrics/metrics.go`):
- `llmsafespace_llm_request_duration_seconds{status_class}` — Histogram, buckets `[0.1, 0.5, 1, 2, 5, 10, 30, 60]`. `status_class` = `"2xx"`, `"3xx"`, `"4xx"`, `"5xx"`, `"error"` (transport error). Avoids high-cardinality individual status codes.
- `llmsafespace_llm_errors_total{error_type}` — Counter; error_type = `timeout | connection_refused | upstream_auth | upstream_error | other`
- `llmsafespace_proxy_rejections_total{reason}` — Counter; reason = `connection_limit | session_limit | workspace_not_active | workspace_no_ip`
- `llmsafespace_proxy_retries_total` — Counter

**Acceptance Criteria:**
- Every `onInference` callback with non-zero output tokens produces `llm_tokens/input` + `llm_tokens/output` DB events for non-canary workspaces
- Every successful `SendMessage` / `SendPromptAsync` for non-canary workspace produces one `llm_request` DB event
- `GetWorkspaceInfo` returning empty `ownerID` → event dropped with warning, no crash
- Metering failure does not affect proxy response or SSETracker processing
- `llmsafespace_llm_request_duration_seconds` recorded for every `doProxy()` call
- `llmsafespace_llm_errors_total` incremented on connection errors, timeouts, 401 upstream, non-2xx upstream
- Integration test: send message → verify `llm_request` + `llm_tokens` events in DB with correct metadata

---

### US-12.3: Lifecycle Event Recording

**Goal:** Persist workspace phase transitions as an auditable lifecycle record.

**Migration:** `workspace_lifecycle_events` table (migration `000025`).

**Architecture decision — reuse existing plumbing:**

The controller has no DB connection and will not be given one. The correct approach reuses the existing CRD watcher: the API service's `onPhaseChange` callback at `proxy.go:960` already fires for every phase change with the full workspace CRD object in scope. It already has `workspace.Spec.Owner.UserID`, old phase, and new phase — all fields needed for `workspace_lifecycle_events`.

Extend `onPhaseChange` to call `meteringSvc.Record()` with a lifecycle event. `meteringSvc` must be injected into `ProxyHandler` (add a field, wire it in `app.go` after `services.Start()`).

**Deduplication:** The CRD watcher may receive the same phase-change event multiple times (at-least-once delivery). Use an idempotency key `lifecycle:{workspace_id}:{from_phase}:{to_phase}:{event_time_unix}` with `ON CONFLICT DO NOTHING`.

**Scope:**
- Migration: `workspace_lifecycle_events` table
- Extend `onPhaseChange` in `proxy.go:960` to call `meteringSvc.Record()` with the lifecycle event
- Add `meteringSvc` field to `ProxyHandler`; wire in `app.go` after `services.Start()`

**Acceptance Criteria:**
- Every phase transition produces one `workspace_lifecycle_events` row
- Duplicate delivery of the same CRD change does not produce duplicate rows
- Integration test: trigger phase transitions (pending→creating→active→suspending→suspended) → verify 5 lifecycle rows exist with correct phases

---

### US-12.4: Compute Time Reconciliation

**Goal:** Authoritative compute time DB metering. The reconciliation cron is the primary path for `compute_seconds` events — not the controller.

**Scope:**
- Reconciliation goroutine in the API service (every 5 min via `time.Ticker`)
- Enumerate Active workspaces using the CRD watcher's in-memory phase map — call `proxyHandler.GetAllKnownPhases()` (from `crd_watcher.go`) rather than a live K8s LIST. This avoids a K8s API server round-trip every 5 minutes and leverages already-cached state. The watcher is kept current by Watch events.
- Workspace `user_id` resolution: query `SELECT id, user_id FROM workspaces WHERE deleted_at IS NULL` from PostgreSQL. This is a **new DB method** (`database.Service.ListAllWorkspaceOwners()`) — the existing `ListWorkspaces` is per-user only and cannot list all users' workspaces.
- Per workspace in Active phase:
  1. Join in-memory active set with DB ownership map
  2. Query `workspace_lifecycle_events` for the most recent transition INTO Active (`to_phase = 'Active'`)
  3. Query `usage_events` for the last `compute_seconds` event for this workspace
  4. Compute uncovered interval: `[max(last_compute_event_time, entered_active_time), now)`
  5. If gap > 45s: emit `compute_seconds/active` events covering the gap in 15s buckets
     - Idempotency key per bucket: `compute:{workspace_id}:{unix_ts_floor_15s}`
     - `source = 'reconciliation'`, `quantity = 15`
     - Final bucket uses the actual partial remainder

**New DB method required (does not currently exist):**
```go
// ListAllWorkspaceOwners returns (workspaceID, userID) pairs for all non-deleted workspaces.
// Used by the metering reconciliation cron — not user-scoped.
func (s *Service) ListAllWorkspaceOwners(ctx context.Context) (map[string]string, error)
```
Add to `database.Service` and the `DatabaseService` interface in `api/internal/interfaces/interfaces.go`.

**New Prometheus metrics:**
- `llmsafespace_metering_reconciliation_catchup_total` — Counter; events emitted by this cron

**Acceptance Criteria:**
- Simulated 3-min controller downtime → reconciliation fills the entire gap on next run
- No duplicates: idempotency keys match what real heartbeats would have produced
- Integration test: create workspace → advance mock clock 5 min → run reconciliation → verify compute events cover the period

---

### US-12.5: API Call Metering Middleware

**Goal:** Count authenticated API calls per user for billing.

**Scope:**
- New middleware `api/internal/middleware/metering.go`
- In-memory aggregation per `(userID, event_subtype)` pair with 10s flush window
- Flush: emit one `api_call` event per non-zero `(userID, subtype)` bucket
- Subtype: `read` for GET; `write` for POST/PUT/DELETE/PATCH
- Skip: unauthenticated requests (no `userID` in context), `/health`, `/livez`, `/readyz`, `/metrics`
- Depends on `EventRecorder` interface only
- Register after auth middleware (must run after `userID` is in context)

**Acceptance Criteria:**
- Authenticated requests produce `api_call` DB events
- Unauthenticated requests produce no events
- Under load (1000 req/s): middleware adds <1ms p99 latency (in-memory accumulation)
- Integration test: 50 authenticated GET calls within one flush window → single `api_call/read` event with quantity ≈ 50

---

### US-12.6: Quota Enforcement

**Goal:** Block requests when usage exceeds configured limits.

**Scope:**
- Migration: `usage_limits` table, `users.plan_id` column
- `CheckQuota()` and `IncrementQuotaCounter()` on `metering.Service`

**Redis key design:**
- Key format: `quota:{owner_id}:{event_type}:{period_key}`
- Monthly period key: `YYYY-MM` (e.g. `2026-06`). TTL = `time.Until(firstDayOfNextMonth - 1 nanosecond)` using UTC midnight. Example Go: `time.Date(now.Year(), now.Month()+1, 1, 0, 0, 0, 0, time.UTC).Sub(now)`.
- Daily period key: `YYYY-MM-DD`. TTL = `time.Until(next UTC midnight)`.
- `IncrementQuotaCounter` uses Redis `INCR` then checks if the key was just created (returned 1) → sets `EXPIREAT`.

**Limit lookup precedence (no limit = unlimited):**
1. Row in `usage_limits` for `(owner_id, owner_type, event_type, period_type)` → use `max_quantity`
2. `instance_settings` key `quota.default.{event_type}.{period_type}` → use as default
3. Key absent from `instance_settings` → **unlimited** (no 429, no enforcement)

**Sentinel for "unlimited":** Absence means unlimited. `usage_limits.max_quantity > 0` constraint is correct.

**New `instance_settings` keys** — add to `pkg/settings/schema.go` and increment `SchemaVersion` to 3:
```go
{Key: "quota.default.llm_request.monthly",    Value: json.RawMessage(`0`), Tier: TierInstance}
{Key: "quota.default.compute_seconds.monthly", Value: json.RawMessage(`0`), Tier: TierInstance}
{Key: "quota.default.storage_bytes.monthly",   Value: json.RawMessage(`0`), Tier: TierInstance}
{Key: "quota.default.api_call.daily",          Value: json.RawMessage(`0`), Tier: TierInstance}
```
A value of `0` means "unlimited" at the `instance_settings` layer. The metering service treats `max_quantity <= 0` from instance_settings as "no limit."

**Quota check in `proxyToWorkspace`** (before forwarding, after auth):
```go
if allowed, remaining, err := meteringSvc.CheckQuota(ctx, owner, "llm_request"); !allowed {
    c.Header("X-Quota-Limit", strconv.FormatInt(limit, 10))
    c.Header("X-Quota-Remaining", "0")
    c.Header("X-Quota-Resets-At", resetsAt.Format(time.RFC3339))
    c.JSON(http.StatusTooManyRequests, gin.H{"error": "quota exceeded", "event_type": "llm_request"})
    return
}
```
After successful proxy: `meteringSvc.IncrementQuotaCounter(ctx, owner, "llm_request")`.

**Prometheus counter:** `llmsafespace_metering_quota_exceeded_total{event_type}`

**Acceptance Criteria:**
- User with daily limit of 10 `llm_request`: 11th request returns 429 with correct headers
- No limit configured → unlimited (existing behavior preserved, no 429)
- Quota check adds <5ms p99 (single Redis INCR)
- Integration test: configure limit → exhaust → verify 429 → reset period key → verify allowed

---

### US-12.7: Usage API Endpoints

**Goal:** Expose usage data to users and admins.

**Scope:**
- New handler file `api/internal/handlers/usage.go`
- Routes registered in `api/internal/server/router.go`
- User endpoints: `GET /api/v1/usage`, `GET /api/v1/usage/workspaces/:id`, `GET /api/v1/usage/quota`
  - Ownership enforced: users see only their own data
  - `from`/`to` query params; default = current billing period
  - Cursor pagination for large date ranges
- Admin endpoints: `GET /api/v1/admin/usage/:ownerId`, `PUT/GET /api/v1/admin/limits/:ownerId`
  - Admin guard middleware (already exists: `middleware/admin_guard.go`)

**Acceptance Criteria:**
- User cannot query another user's usage (403)
- Admin can query any user's usage
- Quota endpoint returns real-time remaining count (from Redis counter + DB limit)
- Integration test: record events → query API → verify totals match DB aggregates

---

### US-12.8: Billing Provider Integration & Export

**Goal:** Implement the export pipeline and provider abstraction.

**Scope:**
- New package `pkg/billing/provider.go` — `BillingProvider` interface + `NoopBillingProvider`
- Migrations: `billing_accounts`, `billing_export_cursor`, `users.plan_id`
- Export goroutine (every 5 min): paginated SELECT > cursor → `ReportUsage()` → advance cursor on success only
- Prometheus gauge: `llmsafespace_metering_export_lag_seconds{provider}`

**Missing `billing_accounts` row during export:**
When a `usage_events` row references an `owner_id` with no corresponding `billing_accounts` row, the export goroutine **skips that event batch for the missing owner** and logs a warning with the owner_id. It does NOT stall the cursor for all users. `NoopBillingProvider` always has a billing account for every user (`CreateCustomer` inserts a row with `external_customer_id = owner_id`).

**Billing account creation on user registration:**
`AuthService.Register()` should not acquire a `BillingProvider` dependency. Use a **post-registration hook** at the router layer: after `authSvc.Register()` succeeds in the register route handler, call `billingProvider.CreateCustomer(ctx, owner, email)` in a fire-and-forget goroutine. Failure is logged but does not fail registration. The `BillingProvider` is accessed from the metering service.

**Acceptance Criteria:**
- `NoopBillingProvider`: export runs, cursor advances, no external calls
- Cursor advances only on successful `ReportUsage()` call
- Missing billing_account: events for that owner are skipped (not stuck), warning logged
- Integration test: record events → run export → verify cursor advanced to max(event.id)

---

### US-12.9: Billing Webhook & Plan Sync

**Goal:** Receive plan changes from billing provider; update local state.

**Scope:**
- `POST /api/v1/webhooks/billing` — no auth middleware; validates provider-specific HMAC signature
- On plan change event: `UPDATE users SET plan_id=$1 WHERE id=$2` + `audit_log` entry
- On payment failure: optional workspace suspension (controlled by `instance_settings[billing.suspend_on_payment_failure]`)
- Audit log entry for every webhook-triggered change

**Acceptance Criteria:**
- Invalid HMAC → 401 (no state mutation)
- Valid plan-change event → `users.plan_id` updated + audit log entry
- Integration test: POST signed webhook → verify plan_id updated + audit log row created

---

### US-12.10: Admin DLQ & Audit Operations

**Goal:** Admin visibility into DLQ and generalized audit trail.

**Scope:**
- Migration: `audit_log` table
- DLQ admin endpoints: list (`GET /api/v1/admin/billing/dlq`), retry (`POST .../retry`), discard (`POST .../discard`)
  - Discard writes `audit_log` entry with `domain='billing', action='dlq_discarded'`
  - Retry triggers immediate attempt against `usage_events` (not re-enqueue via Record())
- Export status endpoint (`GET /api/v1/admin/billing/status`): cursor positions per provider, DLQ counts
- New async audit writer for `audit_log` — same channel+batch pattern as `AsyncAuditLogger`

**Migration note:** New billing/admin audit writes go to `audit_log`. The existing `secret_audit_log` writes remain unchanged. A separate migration (not in this epic) will backfill.

**Acceptance Criteria:**
- DLQ entries visible via admin API
- Discard creates audit log entry; entry marked discarded in DLQ table
- Manual retry succeeds when DB is healthy
- Integration test: create DLQ entry → retry via API → verify resolved + audit log entry

---

### US-12.11: Storage Metering

**Goal:** Daily billing-grade record of allocated storage per workspace.

**New DB method required (does not currently exist):**
`database.Service.ListAllWorkspacesForBilling()` — the existing `ListWorkspaces` is scoped to a single user. Add a new admin-level method:
```go
type WorkspaceBillingRecord struct {
    ID          string
    UserID      string
    StorageSize string  // e.g. "15Gi"
}

func (s *Service) ListAllWorkspacesForBilling(ctx context.Context) ([]WorkspaceBillingRecord, error)
// SQL: SELECT id, user_id, storage_size FROM workspaces WHERE deleted_at IS NULL
```
Add to `database.Service` and the `DatabaseService` interface.

**Canary exclusion:** The `workspaces` PostgreSQL table has no `is_canary` column and no label storage. Skip workspaces owned by users whose `username` starts with `canary` (canary accounts are created with usernames `canary1`, `canary2` by `sdks/canary/go/cmd/seed-accounts/main.go`). Requires a JOIN with `users` on `user_id`. A cleaner `is_canary BOOLEAN` column can be added in a future migration.

**Scope:**
- Daily goroutine in the API service (fires once at midnight UTC via `time.Ticker`)
- Calls `database.Service.ListAllWorkspacesForBilling()` — no K8s LIST needed
- `storage_size` from PostgreSQL is the PVC allocated size (consistent with CRD `Spec.Storage.Size`)
- Skips workspaces where owning user's username starts with `canary`
- Emits `storage_bytes/pvc` event per workspace, idempotency key `storage:{workspace_id}:{YYYY-MM-DD}`
- Note: quantity = allocated PVC bytes (provisioning cost), not filesystem usage

**Acceptance Criteria:**
- One `storage_bytes/pvc` event per non-deleted, non-canary workspace per day
- Idempotency: running cron twice in one day produces no duplicates
- Integration test: N workspaces → run cron → verify N events in DB

---

### US-12.12: Dependency & Service Health Metrics

**Goal:** Instrument Postgres, Redis, auth, and proxy with operational Prometheus metrics.

**Scope — Database** (instrument `api/internal/services/database/database.go`):
- `llmsafespace_db_query_duration_seconds{operation}` — Histogram; operation = `select|insert|update|delete`. Wrap the pool's query/exec methods.
- `llmsafespace_db_errors_total{operation,error_type}` — Counter; error_type = `timeout|connection|deadlock|other`
- `llmsafespace_db_pool_active_connections` — Gauge (from `pgxpool.Stat().AcquiredConns()`)
- `llmsafespace_db_pool_idle_connections` — Gauge (from `pgxpool.Stat().IdleConns()`)
- `llmsafespace_db_pool_max_connections` — Gauge (from `pgxpool.Stat().MaxConns()`)

**Scope — Redis** (instrument `api/internal/services/cache/cache.go`):
- `llmsafespace_redis_command_duration_seconds{command}` — Histogram
- `llmsafespace_redis_errors_total{command}` — Counter

**Scope — Auth** (instrument `api/internal/middleware/auth.go`):
- `llmsafespace_auth_attempts_total{method,result}` — Counter; method=`jwt|apikey`, result=`success|invalid|expired|revoked|locked`. Extends existing `llmsafespace_auth_failures_total` to also count successes.
- `llmsafespace_auth_lockouts_total` — Counter

**Scope — Proxy operational** (instrument `api/internal/handlers/proxy.go`):
- `llmsafespace_proxy_rejections_total{reason}` — Counter
- `llmsafespace_proxy_retries_total` — Counter
Both already specified in US-12.2; add them here if not already implemented.

**Scope — Dependency health** (extend `api/internal/server/router.go` `/readyz` handler):
- `llmsafespace_dependency_up{dependency}` — Gauge (1=healthy, 0=unhealthy; dependency=`postgres|redis`)
- Set to 0 at startup; updated on each `/readyz` call. `/readyz` already pings both dependencies.

**Scope — Service startup** (instrument `api/internal/app/app.go`):
- `llmsafespace_service_startup_duration_seconds{service}` — Histogram; `service` = each named service in startup order

**NOT in scope:**
- Per-customer labels on HTTP error metrics (`owner_id` on 4xx/5xx) — high cardinality (unbounded user IDs) for marginal operational value. Request-ID correlation via logs is the correct solution for per-customer error investigation.

**Acceptance Criteria:**
- DB pool exhaustion visible in metrics before requests fail
- Redis errors visible within the scrape interval
- Auth brute force attempts visible via lockout counter
- `/readyz` failure flips `llmsafespace_dependency_up` gauge to 0
- Integration test: simulate DB unavailability → verify `db_errors_total` increments + `dependency_up{postgres}` → 0

---

### US-12.13: Synthetic Canary — DONE

**Status: Complete.** No implementation work required for the canary framework itself.

The canary system is fully implemented as a Fission-based serverless framework at `sdks/canary/`. 41 Go scenarios covering auth, CRUD, workspace lifecycle, sessions, credentials, SSE, terminal, quotas, and ownership isolation. Multi-language (Go, TypeScript, Python). Shallow scenarios run every 1 min; deep scenarios every 5-10 min. Alerting fires on HTTP 500 from any scenario function.

**Remaining work tied to billing (tracked in US-12.1):**
Canary workspaces must carry `llmsafespace.dev/canary: "true"` so the metering service skips them.

Canary scenarios create workspaces using `CreateWorkspaceRequest`, which already has a `Labels map[string]string` field (`pkg/types/types.go:429`) that `buildWorkspaceCRD()` merges into the CRD's `ObjectMeta.Labels` (`workspace_service.go:617-623`). Each canary scenario that creates a workspace must include `Labels: map[string]string{"llmsafespace.dev/canary": "true"}` in the request. No API or service changes needed.

---

### US-12.14: Structured Request Logging — DONE

**Status: Complete.** Applied in this session.

**What was fixed:**
- `logging.go` now reads `c.GetString("request_id")` from the Gin context (set by `TracingMiddleware`, which runs before `LoggingMiddleware` in the router chain at `router.go:125` vs `127`). The independent 8-char random ID generation and `generateRequestID()` function have been removed.
- `logging.go` now includes `user_id` in `logResponse` fields, read via `c.GetString("userID")`. Present only for authenticated requests; omitted for unauthenticated.
- Three regression tests added to `logging_test.go`: request_id context propagation, user_id in response log, user_id absent when unauthenticated.

**What remains as-is:**
- `request_id.go` implements `RequestIDMiddleware()` but it is not wired in the router — `TracingMiddleware` already performs the same function. Not a duplicate.
- `tracing.go` OTel integration is commented out — deferred.

---

## Dependency Graph

```
US-12.1 (Metering Foundation)
    │
    ├──→ US-12.2 (LLM Metering)          ─┐
    ├──→ US-12.3 (Lifecycle Events)       │ parallel after US-12.1
    ├──→ US-12.4 (Compute Reconciliation) ─┤ (US-12.4 depends on US-12.3 schema)
    ├──→ US-12.5 (API Call Metering)      │
    ├──→ US-12.11 (Storage Metering)      ─┘
    │
    ├──→ US-12.6 (Quota Enforcement)
    │        └──→ US-12.7 (Usage API)
    │
    └──→ US-12.8 (Billing Export)
             └──→ US-12.9 (Webhook/Plan Sync)

US-12.10 (Admin DLQ & Audit)  requires: US-12.1
US-12.12 (Dependency Health)  independent — no prerequisites
US-12.13 (Canary)             DONE — label pending US-12.1
US-12.14 (Logging Fix)        DONE
```

**Critical path:** US-12.1 → US-12.3 + US-12.4 → US-12.6 → US-12.7

**First PR:** US-12.1 alone. It is large enough (new package, 2 migrations, services wiring) to merit its own review cycle.

**Fully independent (start anytime):** US-12.12, US-12.14

**Deferred until provider chosen:** US-12.8 real implementation (Noop ships first), US-12.9

---

## Prometheus Metrics — New

All new metrics use the `llmsafespace_` prefix. Metering-system metrics additionally use `metering_` as a sub-namespace.

### Metering System Health (from metering.Service)

| Metric | Type | Labels | Purpose |
|--------|------|--------|---------|
| `llmsafespace_metering_events_recorded_total` | Counter | `event_type`, `source` | Events written to DB |
| `llmsafespace_metering_events_failed_total` | Counter | `event_type`, `error_type` | Batch writes that failed (went to DLQ) |
| `llmsafespace_metering_events_dropped_total` | Counter | — | Events lost (buffer full or DLQ also failed) |
| `llmsafespace_metering_batch_write_duration_seconds` | Histogram | — | DB batch flush latency |
| `llmsafespace_metering_dlq_size` | Gauge | — | Unresolved DLQ entries |
| `llmsafespace_metering_dlq_dead_total` | Counter | — | DLQ entries that exhausted retries |
| `llmsafespace_metering_reconciliation_catchup_total` | Counter | — | Compute gap-fill events emitted |
| `llmsafespace_metering_export_lag_seconds` | Gauge | `provider` | Seconds since last successful export |
| `llmsafespace_metering_quota_exceeded_total` | Counter | `event_type` | Quota enforcement triggers |

### LLM Proxy Operational (new, in api/internal/services/metrics)

| Metric | Type | Labels | Purpose |
|--------|------|--------|---------|
| `llmsafespace_llm_request_duration_seconds` | Histogram | `status_class` | `doProxy()` wall-clock latency |
| `llmsafespace_llm_errors_total` | Counter | `error_type` | Proxy-level LLM failures |
| `llmsafespace_proxy_rejections_total` | Counter | `reason` | Requests blocked before proxying |
| `llmsafespace_proxy_retries_total` | Counter | — | Stale pod IP retries |

### Dependency Health (new, in api/internal/services/metrics)

| Metric | Type | Labels | Purpose |
|--------|------|--------|---------|
| `llmsafespace_db_query_duration_seconds` | Histogram | `operation` | DB query latency |
| `llmsafespace_db_errors_total` | Counter | `operation`, `error_type` | DB failures |
| `llmsafespace_db_pool_active_connections` | Gauge | — | In-use pool connections |
| `llmsafespace_db_pool_idle_connections` | Gauge | — | Idle pool connections |
| `llmsafespace_db_pool_max_connections` | Gauge | — | Pool capacity |
| `llmsafespace_redis_command_duration_seconds` | Histogram | `command` | Redis latency |
| `llmsafespace_redis_errors_total` | Counter | `command` | Redis failures |
| `llmsafespace_auth_attempts_total` | Counter | `method`, `result` | All auth attempts (success + failure) |
| `llmsafespace_auth_lockouts_total` | Counter | — | Brute-force lockouts |
| `llmsafespace_dependency_up` | Gauge | `dependency` | 1=healthy, 0=unhealthy |
| `llmsafespace_service_startup_duration_seconds` | Histogram | `service` | Per-service boot time |
| `llmsafespace_workspaces_suspended_total` | Gauge | — | Currently Suspended workspaces |

### Already Registered (do not recreate)

| Metric | File |
|--------|------|
| `llmsafespace_inference_requests_total` | `api/internal/services/metrics/metrics.go` |
| `llmsafespace_inference_input_tokens_total` | same |
| `llmsafespace_inference_output_tokens_total` | same |
| `llmsafespace_inference_cost_dollars_total` | same |
| `llmsafespace_model_selections_total` | same |
| `llmsafespace_workspace_phase_transitions_total` | same + `controller/internal/workspace/reconciler.go` |
| `llmsafespace_auth_failures_total` | same |
| `llmsafespace_session_duration_seconds` | same |
| `llmsafespace_agent_reload_total` | same |
| `llmsafespace_agent_reload_duration_ms` | same |
| `llmsafespace_workspace_active_seconds_total` | `controller/internal/metrics/metrics.go` |
| `llmsafespace_user_active_seconds_total` | same |
| `llmsafespace_workspace_storage_bytes` | same |
| `llmsafespace_workspace_disk_used_bytes` | same |
| `llmsafespace_workspace_create_duration_seconds` | same |
| `llmsafespace_workspace_resume_duration_seconds` | same |
| `llmsafespace_workspaces_running` | same |

### Phantom Dashboard Metrics — Resolution

| Phantom Metric | Resolution |
|---------------|------------|
| `llmsafespace_user_llm_calls_total` | Remove from billing dashboard; data exists at fleet level in `llmsafespace_inference_requests_total`. Per-user breakdown requires DB query (US-12.7). |
| `llmsafespace_workspace_llm_request_duration_seconds` | Retained as "pending Epic 33". Do not implement now. |
| `llmsafespace_workspace_proxy_bytes_total` | Retained as "pending Epic 33". Do not implement now. |

---

## Non-Requirements (Explicitly Out of Scope)

- **Billing provider SDK** (Stripe/Lago) — `NoopBillingProvider` ships; implement when provider chosen
- **Invoice generation** — billing provider's job
- **Payment collection / dunning** — billing provider's job
- **Tax calculation** — billing provider handles (region column enables it when needed)
- **Pricing rate cards in our DB** — billing provider owns pricing
- **Plan/subscription management UI** — billing provider dashboard or future frontend work
- **Per-request cost estimation** — nice-to-have, not MVP
- **Multi-currency** — USD only; schema is forward-compatible
- **Org-level billing endpoints** — schema supports it; endpoints deferred to Epic 11
- **Real-time usage push** — polling via `/api/v1/usage` is sufficient
- **Actual filesystem usage metering** — `storage_bytes` records PVC allocated capacity only
- **Model/provider labels on proxy latency histogram** — not available at proxy time; deferred to Epic 33
- **TTFB histogram** — our proxy latency is not user-perceived TTFB
- **Per-customer HTTP error labels** — high cardinality; request_id correlation via logs is correct

---

## Security Considerations

1. **Usage data is PII-adjacent.** Access restricted to: own data (user), all data (admin). Ownership enforced at handler level, not only at DB query level.
2. **Admin actions audited.** Every DLQ operation, limit override, and discard logged to `audit_log` with actor, target, and timestamp.
3. **No secrets in metering data.** `request_context` stores `api_key_id` (a non-secret identifier), not the key itself. `metadata` stores provider name, not credentials.
4. **DLQ contains user IDs and workspace IDs.** DLQ API access restricted to admin role only.
5. **Webhook signature verification.** Provider webhooks validated via HMAC before any state mutation.
6. **Export to billing provider.** Only `owner_id` and usage quantities sent. No workspace content, no credentials.

---

## Organization Extension Point (Epic 11 — Complete)

Epic 11 (Organizations) is complete (PR #137). The extension point is ready to activate:

**What changes:**
- `resolveBillingOwner()`: check `workspace.Spec.Owner.OrgID` → return `{ID: orgID, Type: OwnerTypeOrg}`
- Org admin usage endpoints become active
- `usage_limits` table gains org-level rows (no schema change — `owner_type='org'` rows already valid)
- Export maps org → single billing account

**What does NOT change:**
- `usage_events` schema — already has `owner_type`
- All Go interfaces — consumers depend on `BillingOwner`, not the resolution logic
- `BillingProvider` interface
- Reconciliation, DLQ, quota enforcement logic
