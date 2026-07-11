# LLMSafeSpaces Core Packages

Shared Go packages used by both the API service and the controller. All packages target Go 1.25+ and follow Kubernetes operator patterns where applicable.

## Package Index

| Package | Purpose |
|---------|---------|
| `apis/llmsafespaces/v1` | CRD Go types (kubebuilder-annotated) for Workspace, RuntimeEnvironment, InferenceRelay |
| `agentd` | Types for the workspace-agentd sidecar HTTP API (healthz, readyz, statusz) |
| `config` | Kubernetes client configuration structs |
| `credentials` | Credential set entity types + encryption service (AES-256-GCM, key rotation) |
| `crds` | CRD YAML definitions (Workspace, RuntimeEnvironment) |
| `http` | HTTP utilities (BodyCaptureWriter for response inspection) |
| `interfaces` | Core interface contracts (KubernetesClient, LoggerInterface) |
| `kubernetes` | Kubernetes client with leader election, informers, and typed CRD access |
| `logger` | Zap-based structured logger implementing LoggerInterface |
| `mcp` | MCP (Model Context Protocol) server and client |
| `redact` | 16-rule regex pipeline for scrubbing secrets from agent stdout |
| `secrets` | encrypted secret store — key wrapping, encryption, audit, workspace bindings |
| `settings` | Declarative settings schema + instance/user settings services with typed accessors |
| `types` | API DTOs (request/response types, domain errors, context keys) |
| `utilities` | Hashing, string masking, Kubernetes label helpers |

---

## Kubernetes Client (`kubernetes`)

Manages Kubernetes API interactions for the API service.

```go
client, err := kubernetes.New(cfg, logger)
client.Start()
defer client.Stop()

wsClient := client.LlmsafespacesV1().Workspaces("namespace")
ws, err := wsClient.Get("my-workspace", metav1.GetOptions{})
```

Key features: in-cluster or kubeconfig auth, connection pooling (QPS 100, Burst 200), leader election via Lease locks, SharedInformerFactory (30m resync), typed REST client for `llmsafespaces.dev/v1`.

---

## CRD Types (`apis/llmsafespaces/v1`)

Three CRDs in the `llmsafespaces.dev/v1` API group:

| Kind | Scope | Short | Purpose |
|------|-------|-------|---------|
| `Workspace` | Namespaced | `ws` | PVC-backed persistent environment + pod lifecycle |
| `RuntimeEnvironment` | Cluster | `rte` | Mapping from runtime name → container image |
| `InferenceRelay` | Cluster | `irelay` | Managed fleet of relay VMs proxying free-tier inference (opt-in, Epic 42) |

Nine Workspace phases: `Pending → Creating → Active → Suspending → Suspended → Resuming → Active`, with `Terminating → Terminated` and `Failed` terminal exits.

---

## Secrets (`secrets`)

encrypted at rest secret store. Per-user DEK derived from password via HKDF-SHA256.

Components: `SecretService` (CRUD + bindings + audit), `KeyService` (DEK wrapping/caching/rotation), `crypto.go` (AES-256-GCM), `SecretProvider` interface (Postgres impl), `RedisCache` (session DEK cache).

Secret types: `llm-provider`, `ssh-key`, `git-credential`, `secret-file`, `env-secret`

---

## Settings (`settings`)

Declarative tiered configuration. Single Go schema drives validation, seeding, API, and frontend forms.

- Tier 2 (Instance): admin-mutable, `instance_settings` table, singleflight-cached (60s TTL)
- Tier 3 (User): per-user, `user_settings` table

Typed accessors: `GetBool(ctx, key)`, `GetInt(ctx, key)`, `GetString(ctx, key)`

---

## Credentials (`credentials`)

Admin-managed credential sets with AES-256-GCM encryption and versioned key rotation.

---

## Interfaces (`interfaces`)

```go
type KubernetesClient interface {
    Start() error
    Stop()
    Clientset() kubernetes.Interface
    DynamicClient() dynamic.Interface
    RESTConfig() *rest.Config
    InformerFactory() informers.SharedInformerFactory
    LlmsafespacesV1() LLMSafespacesV1Interface
}

type LoggerInterface interface {
    Debug(msg string, keysAndValues ...interface{})
    Info(msg string, keysAndValues ...interface{})
    Warn(msg string, keysAndValues ...interface{})
    Error(msg string, err error, keysAndValues ...interface{})
    Fatal(msg string, err error, keysAndValues ...interface{})
    With(keysAndValues ...interface{}) LoggerInterface
    Sync() error
}
```

---

## Testing

All packages include unit tests. Mocks in `/mocks` (kubernetes, types, logger).

```bash
go test -timeout 90s -race ./pkg/...
```
