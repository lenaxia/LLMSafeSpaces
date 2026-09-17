# Worklog: #1416 — public CA roots in the agentd image

**Date:** 2026-09-17
**Session:** Workflow http nodes failed every TLS target: the digest-pinned agentd image is FROM scratch and ships no trust store. One COPY line plus the contract pins.
**Status:** Complete

---

## Objective
Make TLS egress from the agentd sidecar verify against real roots without disturbing the delivery image's contracts (binary-only image volume, per-arch sha256 pins, nothing executable, ~25MB pull budget).

## Work Completed
- Dockerfile: COPY the bookworm builder's ca-certificates.crt to /etc/ssl/certs/ca-certificates.crt (Go's first default system-roots probe path, root_linux.go certFiles[0]).
- Verified additive-only: in-cluster API traffic is plain HTTP (APIServiceURL http://…svc); nothing repo-wide sets SSL_CERT_FILE; the main container's own TLS paths use its own rootfs bundle. The self-verify hashes /proc/self/exe only — unaffected.
- Contract pins in pkg/repolint (dockerfile_ca_bundle_test.go): bundle COPY present at the exact path, bundle never chmod'd executable, binary stays --chmod=755; pins fail loud on unreadable Dockerfile.
- Docs corrected where they claimed binary-only contents or a ~25MB image: operator delivery doc + the Dockerfile's own sizing comment.
- Deferred live verification (http node GET https://…/health) tracked in #1427.

## Key Decisions
- Copy from the $BUILDPLATFORM builder (bundle is arch-independent data).
- No clear-path invention for inputSchema-style semantics — n/a here; single-purpose data file.

## Blockers
None.

## Tests Run
- repolint pins (3) green; full pkg/repolint suite green; image builds in CI.

## Next Steps
Merge with the #1410-#1419 batch; live http-node leg in the comprehensive test (#1427).

## Files Modified
- cmd/workspace-agentd/Dockerfile (+doc comment)
- pkg/repolint/dockerfile_ca_bundle_test.go
- docs/operator/agentd-delivery.md
