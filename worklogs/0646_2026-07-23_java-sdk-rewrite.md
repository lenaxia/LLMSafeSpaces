# Worklog 0646: Epic 62 US-62.5 — Java SDK Typed-Facade Rewrite

**Date:** 2026-07-23
**Epic:** 62 — SDK Refresh, API-Surface Parity & Publish
**PR:** #586
**Story:** US-62.5

## Context

The Java SDK was a 133-line generic HTTP wrapper with no typed facade, no typed exceptions (all errors collapsed to `LLMSafeSpacesException`), no auth-retry, and no 404/409/429 differentiation. This worklog covers the rewrite into a typed facade matching the other three SDKs.

## What was done

### Typed facade (`LLMSafeSpacesClient.java`)
- Builder-constructed facade with 9 service groups (workspaces, sessions, auth, secrets, terminal, account, userSettings, providerCredentials, adminProviderCredentials)
- Single-retry guard for 401 auto-relogin (prevents stack overflow on persistent 401)
- Handles 207 MultiStatus credential wrapper (unwraps `credential` field)
- Handles correct `list_bindings` wrapper shape (`workspaceIds` extraction)

### Exception hierarchy
- Unchecked (extends `RuntimeException`) — modern Java SDK convention
- `LLMSafeSpacesException` → `AuthException` (401/403), `NotFoundException` (404), `ConflictException` (409), `RateLimitException` (429)
- Status-code mapping via `mapException()` switch expression

### Models
- `Workspace`, `EnsureSessionResponse`, `MessageResponse` (with `extractContent`), `ProviderCredential`

### Tests (9 total)
- `workspacesCreate` — typed workspace creation
- `notFound` — NotFoundException on 404
- `sendMessage` — content extraction from parts array
- `delete` — 204 void response
- `notFound_unchecked` — RuntimeException, no checked-exception boilerplate
- `conflict` — ConflictException on 409
- `exceptionsAreUnchecked` — compiles without throws clause
- `sendMessage_multiPart` — multi-part extraction
- `secretsReveal` — password parameter correctly passed

## Breaking change note

The old `LLMSafeSpacesException extends Exception` (checked) is now `extends RuntimeException` (unchecked). Callers with `catch (LLMSafeSpacesException e)` continue to work. Callers with `throws LLMSafeSpacesException` method signatures now have a redundant but harmless throws declaration. No source-compatible migration needed — the change is source-backward-compatible (catching an unchecked exception is legal Java).

## Assumptions validated

- Java 17's `java.net.http.HttpClient` is sufficient (sync requests, timeouts)
- Gson is sufficient (no need for Jackson/Moshi)
- JDK's `HttpServer` is sufficient for unit tests (no WireMock dependency)
