# Worklog NNNN — Image Factory S7: Admin Portal Endpoints

**Date:** 2026-08-02
**Scope:** S7 — platform-owner admin endpoints for catalog management.

## Summary

Implemented admin CRUD endpoints for the image factory catalog: bases,
extensions, known-failures, and platform config (architectures). All
behind AuthMiddleware + AdminGuard.

## What was built

- `ImageFactoryAdminHandler` (separate from consumer handler — ISP)
- `imageFactoryAdminStore` interface (ISP: different method subset)
- 14 endpoints across platform-config, bases, extensions, known-failures
- ExtensionType validation (apt/mise/file only)
- FileSpec validation at publish time (reuses pure-logic validator)
- Real `middleware.AdminGuard()` exercised in tests

## Tests (15)

Platform-config get/set, bases CRUD + 400/500, extensions publish/retire +
invalid-type/file-without-spec, known-failures list/toggle/clear + 500,
AdminGuard 404, store-error 500. All -race clean.
