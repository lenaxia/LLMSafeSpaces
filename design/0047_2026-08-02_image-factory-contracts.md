# Image Factory — contracts, types, and test plan

**Companion to:** `design/0046_2026-08-01_image-factory.md` (the design).
**Purpose:** pin the exact Go types, DB row shapes, interface seams, and
test coverage before any code is written. The design says *what* and *why*;
this doc says *exactly what the code looks like* so stories don't diverge.

Conventions matched to the existing codebase:
- AGPL-3.0 header (`// Copyright (C) 2026 Michael Kao` + SPDX line).
- Handlers in `api/internal/handlers/`, Gin (`*gin.Context`).
- DB access via `api/internal/services/database/Service` (stdlib `*sql.DB`
  over pgx; one `Service`, per-domain store methods on it).
- Migrations in `api/migrations/NNNNNN_*.up.sql` + `.down.sql`, next free
  number is `000013`.
- Tests TDD, `-race`, table-driven, happy + unhappy + edge.

## Package layout

```
api/internal/imagefactory/        NEW — pure logic (no DB, no HTTP)
    types.go                      domain types (Config, Extension, Base, ...)
    selection.go                  selection → hash, decode helpers
    resolved_values.go            resolved_values JSONB shape + validate
    dockerfile.go                 resolved_values + base → deterministic Dockerfile
    dockerfile_test.go
    resolved_values_test.go
    selection_test.go

api/internal/services/database/
    imagefactory.go               NEW — store methods on *database.Service
    imagefactory_test.go

api/internal/handlers/
    imagefactory_catalog.go       NEW — GET /v1/image-factory/catalog
    imagefactory_configs.go       NEW — POST/GET /v1/image-factory/configs[-/{hash}]
    imagefactory_builds.go        NEW — GET /v1/image-factory/builds/{id}
    imagefactory_callback.go      NEW — POST internal callback (token-authed)
    imagefactory_admin.go         NEW — /v1/admin/image-factory/* (split per resource)
    imagefactory_*_test.go        per-handler

api/internal/interfaces/
    imagefactory.go               NEW — ImageBuilderDispatcher, StatusResolver
                                       interfaces (for handler DI + mocking)

api/migrations/
    000013_image_factory.up.sql   NEW — the 6 tables + indexes
    000013_image_factory.down.sql NEW — DROP in reverse dep order

pkg/imagefactory/                 NEW — re-export of pure logic for cmd/ reuse
    (wraps api/internal/imagefactory so the GH Actions renderer can be
     invoked from a small CLI if we ever want to render locally)
```

Note: pure logic lives under `api/internal/` because nothing outside the API
consumes it today (no second binary). If the GH Actions workflow ever needs
to render a Dockerfile without round-tripping through the API, we promote
to `pkg/` then — not before (containment-before-abstraction, Rule 12).

## Domain types (`api/internal/imagefactory/types.go`)

```go
package imagefactory

// ExtensionType enumerates the catalog extension kinds. "run" and "env"
// are deliberately absent — they were the injection vectors (design #5).
type ExtensionType string

const (
    ExtensionTypeApt  ExtensionType = "apt"
    ExtensionTypeMise ExtensionType = "mise"
    ExtensionTypeFile ExtensionType = "file"
)

// Extension is a catalog row. Immutable-once-published (design #7): the
// build-relevant fields (Type, Value, FileSpec, SupportedBases) do not
// change after creation. Only Retired, ReviewRequested, Description mutate.
type Extension struct {
    ID              string        `json:"id"`
    Type            ExtensionType `json:"type"`
    Value           string        `json:"value"`
    FileSpec        *FileSpec     `json:"fileSpec,omitempty"`     // present iff Type==file
    SupportedBases  []string      `json:"supportedBases"`         // base *names*
    Retired         bool          `json:"retired"`
    ReviewRequested bool          `json:"reviewRequested"`
    Description     string        `json:"description,omitempty"`
}

// FileSpec is the target for a type=file extension. Path must be absolute
// (no traversal); Mode defaults to "0644" when empty.
type FileSpec struct {
    Path string `json:"path"`
    Mode string `json:"mode,omitempty"` // octal string "0755"
}

// Base is a (name, version) row of an operator-approved base image.
// Composite-keyed: old versions persist (design #8).
type Base struct {
    Name      string `json:"name"`
    Version   string `json:"version"`
    Image     string `json:"image"`
    Tag       string `json:"tag,omitempty"`
    Digest    string `json:"digest,omitempty"`    // wins over Tag
    IsDefault bool   `json:"isDefault"`
}

// Ref returns the pullable reference. Digest wins over tag.
func (b Base) Ref() string

// PlatformConfig is the single-row platform-level factory config.
type PlatformConfig struct {
    Architectures []string `json:"architectures"` // e.g. ["linux/amd64","linux/arm64"]
}

// ResolvedValue is one entry in the resolved_values JSONB. It is the
// cached projection of an Extension's build fields, frozen at config-save
// time. Shape is pinned here so the DB story and the Dockerfile-render
// story cannot diverge (design resolved_values shape spec).
type ResolvedValue struct {
    Type     ExtensionType `json:"type"`
    Value    string        `json:"value"`
    FileSpec *FileSpec     `json:"fileSpec,omitempty"` // present iff Type==file
}

// ResolvedValues maps extension ID → frozen resolved value. This is the
// exact JSONB shape stored on image_factory_configs.resolved_values and
// image_factory_builds.resolved_values.
type ResolvedValues map[string]ResolvedValue

// Selection is the user's pick: an unordered set of extension IDs. Hashing
// sorts, so input order does not affect the result.
type Selection []string

// ConfigScope enumerates the three friendly-name scopes (design #25).
type ConfigScope string

const (
    ScopeMember   ConfigScope = "member"
    ScopeOrg      ConfigScope = "org"
    ScopePlatform ConfigScope = "platform"
)

// ConfigStatus is the config lifecycle pill (design #20).
type ConfigStatus string

const (
    StatusBuilding ConfigStatus = "building"
    StatusReady    ConfigStatus = "ready"
    StatusRejected ConfigStatus = "rejected"
)

// Config is a saved user/org/platform config.
type Config struct {
    ID             string         `json:"id"`
    Hash           string         `json:"hash"`           // s-<sha256[:16]>
    Name           string         `json:"name"`           // friendly name
    Selection      Selection      `json:"selection"`      // sorted extension IDs
    ResolvedValues ResolvedValues `json:"resolvedValues"` // frozen projection
    BaseName       string         `json:"baseName"`
    BaseVersion    string         `json:"baseVersion"`
    Scope          ConfigScope    `json:"scope"`
    OwnerID        *string        `json:"ownerId,omitempty"` // member_id; nil for platform
    OrgID          *string        `json:"orgId,omitempty"`   // org_id; nil for member/platform
    Status         ConfigStatus   `json:"status"`
}

// KnownFailure is a row of image_factory_known_failures — the blocklist.
type KnownFailure struct {
    SelectionHash  string    `json:"selectionHash"`
    Selection      Selection `json:"selection"`
    BaseName       string    `json:"baseName"`
    Explanation    string    `json:"explanation,omitempty"`
    FailureReason  string    `json:"-"`               // never serialized to non-admins
    DetectedAt     time.Time `json:"detectedAt"`
    Retriable      bool      `json:"retriable"`
}

// Build is one row of image_factory_builds. One row per API dispatch —
// transient retry happens inside the GH Actions workflow (design #12), so
// the API sees exactly one dispatch + one final result.
type Build struct {
    ID             string         `json:"id"`
    ConfigID       string         `json:"configId"`
    Hash           string         `json:"hash"`
    BaseName       string         `json:"baseName"`
    BaseVersion    string         `json:"baseVersion"`
    ResolvedValues ResolvedValues `json:"resolvedValues"`
    Architectures  []string       `json:"architectures"`
    ImageRef       string         `json:"imageRef,omitempty"`  // set on success
    Digest         string         `json:"digest,omitempty"`    // set on success
    Status         BuildStatus    `json:"status"`
    GHRunID        *int64         `json:"ghRunId,omitempty"`
    FailureReason  string         `json:"-"`                   // never serialized to non-admins
    Explanation    string         `json:"explanation,omitempty"`
    TriggeredBy    *string        `json:"triggeredBy,omitempty"`
    StartedAt      time.Time      `json:"startedAt"`
    FinishedAt     *time.Time     `json:"finishedAt,omitempty"`
}

type BuildStatus string

const (
    BuildDispatched BuildStatus = "dispatched"
    BuildSucceeded  BuildStatus = "succeeded"
    BuildFailed     BuildStatus = "failed"
)
```

## Pure functions (`selection.go`, `resolved_values.go`, `dockerfile.go`)

```go
// HashSelection computes the content-addressed schematic hash over the
// sorted selection IDs + base name. Pure, deterministic, no I/O.
// Returns "s-" + 16 hex chars of SHA-256. Design #1, #2.
func HashSelection(sel Selection, baseName string) (string, error)

// ValidateSelection checks that every ID is non-empty and matches the
// extension ID charset [a-z0-9._-]+. Does NOT check existence in the
// catalog — that's a store concern.
func ValidateSelection(sel Selection) error

// ResolveSelection joins a Selection against a set of Extensions and
// returns the frozen ResolvedValues projection. Errors if any ID is
// missing, retired, or unsupported on baseName.
func ResolveSelection(sel Selection, exts map[string]Extension, baseName string) (ResolvedValues, error)

// ValidateResolved checks the resolved shape: every entry has a known
// type, a non-empty value, and (for file) an absolute non-traversal path
// + valid octal mode.
func ValidateResolved(rv ResolvedValues) error

// RenderDockerfile renders the deterministic Dockerfile from frozen
// resolved values + a base ref. Pure function of (ResolvedValues, Base).
// Identical inputs → identical Dockerfile byte-for-byte.
func RenderDockerfile(rv ResolvedValues, base Base) (string, error)
```

## DB store interface (`api/internal/services/database/imagefactory.go`)

Methods added to `*database.Service`. Each takes `ctx` and returns error;
rows are the domain types above. This is the seam handlers depend on and
tests fake.

```go
// Catalog
GetPlatformConfig(ctx) (PlatformConfig, error)
SetPlatformConfig(ctx, PlatformConfig) error

ListBases(ctx) ([]Base, error)
GetBase(ctx, name, version string) (Base, error)
UpsertBase(ctx, Base) error                // admin
DeleteBase(ctx, name, version string) error // admin (retire)

ListExtensions(ctx, includeRetired bool) ([]Extension, error)
GetExtension(ctx, id string) (Extension, error)
PublishExtension(ctx, Extension) error      // admin; immutable build fields after this
RetireExtension(ctx, id string) error        // admin
SetExtensionReviewRequested(ctx, id string, v bool) error

// Known failures
ListKnownFailures(ctx) ([]KnownFailure, error)
GetKnownFailure(ctx, selectionHash, baseName string) (KnownFailure, error)
RecordKnownFailure(ctx, KnownFailure) error         // upsert
SetKnownFailureRetriable(ctx, selectionHash, baseName string, retriable bool) error
DeleteKnownFailure(ctx, selectionHash, baseName string) error
ListRejectedConfigsForFailure(ctx, selectionHash, baseName string) ([]Config, error) // for un-block→rebuild

// Configs
CreateConfig(ctx, Config) error
GetConfig(ctx, id string) (Config, error)
GetConfigByHash(ctx, hash string, scope ConfigScope, ownerID, orgID *string) (Config, error)
ListConfigs(ctx, scope ConfigScope, ownerID, orgID *string) ([]Config, error)
SetConfigStatus(ctx, id string, status ConfigStatus) error

// Builds — including coalescing + dispatch ordering
GetBuild(ctx, id string) (Build, error)
GetInFlightOrSuccessfulBuild(ctx, hash, baseVersion string) (*Build, error)  // coalescing probe
GetBuildByGHRunID(ctx, ghRunID int64) (Build, error)                          // callback lookup
CreateBuild(ctx, Build) error
MarkBuildSucceeded(ctx, id, imageRef, digest string) error
MarkBuildFailed(ctx, id, failureReason, explanation string) error
LinkConfigToBuild(ctx, configID, buildID string) error                        // coalescing link
```

## Builder dispatcher + status resolver interfaces

```go
// BuildDispatcher fires a GitHub Actions workflow_dispatch. Implemented
// by a real GH Actions client in production and a fake in tests.
type BuildDispatcher interface {
    // Dispatch returns the GH Actions run ID. It MUST be called BEFORE
    // the config row commits (design #17 — rollback on failure).
    Dispatch(ctx, DispatchRequest) (ghRunID int64, err error)
}

type DispatchRequest struct {
    BuildID        string
    CallbackToken  string         // generated by caller, stored on build row
    Hash           string
    BaseName       string
    BaseVersion    string
    BaseImageRef   string         // base.Ref(), resolved by caller
    Architectures  []string
    ResolvedValues ResolvedValues
}

// StatusResolver queries the GH Actions API for a run's status. Used by
// the on-read derivation path (design #21). Result cached ~30s by caller.
type StatusResolver interface {
    // Resolve returns (completed, success, logTail, err). When completed
    // is false, the build is still in flight. logTail is populated only
    // on completion+failure (for the failure seam).
    Resolve(ctx, ghRunID int64) (completed bool, success bool, logTail string, err error)
}

// FailureExplainer is the platform-LLM seam (design #22). One call, on the
// real failure. May return empty explanation on LLM-down (degradation).
type FailureExplainer interface {
    Explain(ctx, logTail string, rv ResolvedValues) (explanation string, attributedExtensionID string, err error)
}
```

## Handler contracts (Gin)

All under `/v1/image-factory/*` (user) and `/v1/admin/image-factory/*`
(platform owner) and `/v1/admin/orgs/{org}/image-factory/*` (org owner).
Auth via the existing middleware (extracts memberID, orgID, role).

| Method+Path | Handler | Body / Query | Returns | Auth |
|---|---|---|---|---|
| GET | `/v1/image-factory/catalog` | — | `{architectures, bases[], extensions[] (non-retired), knownFailures[]}` | any authed |
| POST | `/v1/image-factory/configs` | `{name, selection[], baseName, baseVersion}` | `Config` (201) | member/solo; org-member iff policy=assemble |
| GET | `/v1/image-factory/configs` | `?scope=` | `Config[]` with derived status pills | any authed (filtered to visible scopes) |
| GET | `/v1/image-factory/configs/{hash}` | — | `Config` + decoded `resolvedValues` + derived build status | owner of the config |
| GET | `/v1/image-factory/builds/{id}` | — | `Build` (derived status from GH on read) | owner of the build's config |
| POST | `/internal/image-factory/builds/{id}/callback` | `{status, digest?, failureReason?}` + `Authorization: Bearer <token>` | 204 | callback_token (ConstantTimeCompare) |
| GET/POST/PUT/DELETE | `/v1/admin/image-factory/{bases,extensions,known-failures,configs,builds,platform-config}` | resource-specific | resource-specific | platform owner |
| GET/PUT | `/v1/admin/orgs/{org}/image-factory/policy` | — / `{policy}` | policy | org owner or platform owner |
| GET/POST | `/v1/admin/orgs/{org}/image-factory/configs` | — / `{name, selection[], baseName, baseVersion}` | `Config[]` / `Config` | org owner or platform owner |

**`POST /configs` exact sequence** (load-bearing, design #15/#16/#17):

1. Bind + validate body (name charset, selection non-empty + valid IDs, base exists).
2. Load extensions; `ResolveSelection` → `ResolvedValues`; `ValidateResolved`.
3. `HashSelection` → hash.
4. Check `known_failures` for `(hash, baseName)` with `retriable=false` → 422 if blocked.
5. `GetInFlightOrSuccessfulBuild(hash, baseVersion)`:
   - If succeeded exists → create config row at `status=ready`, link to build, return 201. No dispatch.
   - If in-flight exists → create config row at `status=building`, link to build, return 201. No dispatch.
6. No existing build → generate `callback_token` (32 bytes hex); `Dispatch` to GH Actions:
   - On error → return 503, **do not commit config row** (design #17).
7. On dispatch success → `CreateBuild` (status=dispatched, gh_run_id, callback_token) and `CreateConfig` (status=building) in one tx.
8. Return 201 with the config.

## Test plan

TDD per repo Rule 0. Coverage target: every pure function and every handler
path has happy + unhappy + edge tests; the dispatch/coalescing sequence has
an integration test exercising the real ordering.

### Pure logic (`api/internal/imagefactory/`)

**`selection_test.go` — HashSelection / ValidateSelection**
- Happy: empty-selection-rejected; single ID; many IDs; order-independence
  (two perms of the same set hash equal); base name in preimage (different
  base → different hash); stable across runs.
- Unhappy: empty ID; invalid charset (`foo bar`, `foo;rm`); duplicate IDs
  dedup to same hash; nil selection.
- Edge: very large selection (100 IDs); unicode in IDs rejected; base name
  with whitespace rejected.

**`resolved_values_test.go` — ResolveSelection / ValidateResolved**
- Happy: resolves against a stub extension map; frozen projection matches
  inputs; file_spec carried when type=file.
- Unhappy: ID missing from map; extension retired → error; extension
  unsupported on baseName → error; value empty → invalid; file path
  relative → invalid; file path traversal (`/etc/../x`) → invalid; file
  mode non-octal → invalid.
- Edge: duplicate IDs in selection collapse; selection resolving to empty
  resolved_values rejected (no-op config).

**`dockerfile_test.go` — RenderDockerfile**
- Happy: deterministic (two calls same inputs byte-equal); order-
  independent (map iteration randomized — Go); apt block emits only when
  apt extensions present; mise block emits `mise install --system` + reshim;
  file extensions base64-encoded with chmod; base ref uses digest when
  present, else tag.
- Unhappy: invalid resolved_values rejected; unknown extension type rejected.
- Edge: empty resolved_values → FROM + ENTRYPOINT only; opencode entrypoint
  preserved at the end; USER sandbox + WORKDIR /workspace present.

### DB store (`api/internal/services/database/imagefactory_test.go`)

Uses the existing migration-safety harness (`postgres:16` container, schema
round-trip). Per repo convention, an integration test, not a mock.

- CRUD round-trip per table; partial-unique-index enforcement (platform
  scope allows one "ml-stack"; member scope allows same name for different
  ownerID; org scope same).
- Coalescing probe: `GetInFlightOrSuccessfulBuild` returns the in-flight
  row when one exists, the succeeded row when one exists, nil otherwise.
- `ListRejectedConfigsForFailure` returns exactly the rejected configs
  whose (hash, baseName) match — the un-block→rebuild target set.

### Handler tests (`api/internal/handlers/imagefactory_*_test.go`)

Table-driven, fake store + fake dispatcher/resolver/explainer injected.

**catalog** — happy (200, returns non-retired only); rejects unauthed.

**configs POST** — the load-bearing sequence:
- Happy novel → dispatch called once, config+build committed, 201, status=building.
- Happy with existing succeeded build → **dispatch NOT called**, config at ready, 201.
- Happy with existing in-flight build → **dispatch NOT called**, config at building, 201 (coalescing).
- Dispatch failure → 503, **no config row committed** (dispatch-before-commit).
- `(hash, baseName)` in known_failures retriable=false → 422, no dispatch.
- Org-member under `published_only` policy → 403.
- Org-member under `assemble` → 201.
- Invalid body (bad selection, unknown base, retired extension) → 422.

**configs GET** — happy returns visible scopes (member sees own + org +
platform); filter by scope query param; rejected configs include
explanation.

**builds GET** — happy derives status from GH resolver on read; when GH
says completed+success → transitions build to succeeded + config to ready
idempotently; when GH says completed+failure → transitions to failed +
config to rejected + writes known_failure + calls explainer once.

**callback POST** — happy (valid token → transitions); wrong token → 403
+ no state change; replay (second POST with same token) → idempotent 204;
payload missing fields → 400.

### Integration / e2e (one test, `tests/`)

A single happy-path e2e once the unit + handler layers are green:
POST config → fake-dispatcher records dispatch → callback POST with
success → GET config shows ready → GET builds/{id} shows succeeded. This
is the round-trip proof; everything else is unit/handler.

### Adversarial review gate (repo Rule 11)

Before any story is marked done: the structured adversarial review. For
this feature the specific things to probe:
- **Coalescing races.** Two concurrent POST /configs for the same novel
  hash — do both dispatch, or does one coalesce? (Tx + unique index on
  `(hash, base_version)` for in-flight builds, or post-hoc dedup.)
- **Callback replay/forge.** Token scope is per-build; verify a token
  from build A cannot mutate build B.
- **Status derivation idempotency.** Concurrent reads of a finished build
  must not double-transition or double-write the known_failure.
- **Dispatch-before-commit rollback.** Verify no row survives a simulated
  dispatch failure (mock dispatcher returns error mid-tx).

## Story decomposition (build order)

Vertical-slice-first; failure-path ships alongside the workflow, not after.

1. **S1 — migrations + pure logic.** `000013` migration; `imagefactory/`
   pure package (types, selection, resolved_values, dockerfile) + full
   unit tests. No HTTP, no DB yet. Proves the hash/render determinism.
2. **S2 — DB store.** `database/imagefactory.go` + integration tests
   against `postgres:16`. Coalescing probe + partial unique indexes.
3. **S3 — catalog + configs GET.** Read-only endpoints; the consumer UX
   can render. Fake dispatcher stub returns "dispatched" without GH.
4. **S4 — configs POST + coalescing + dispatch-before-commit.** The
   load-bearing handler. Fake dispatcher; full unhappy-path coverage.
5. **S5 — callback + status derivation.** Callback auth token; on-read GH
   resolution; the success/failure transition + known_failure write.
6. **S6 — failure explainer seam.** LLM integration with degradation.
7. **S7 — admin portal endpoints.** Catalog/known-failures/policy CRUD.
8. **S8 — GH Actions workflow + retry-inside-workflow.** The actual
   `image-build.yml`; transient retry step; callback POST.
9. **S9 — e2e + adversarial review gate.** The round-trip proof + the
   four adversarial probes above.
10. **S10 — frontend** (settings page + launch picker pills). Last; API
    is stable by then.

S1–S2 are independently mergeable. S3–S5 are the API vertical slice. S8
can land in parallel with S5–S7. S10 is last.
