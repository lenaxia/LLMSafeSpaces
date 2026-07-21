#!/usr/bin/env bash
# CORS expose-headers synthetic monitor.
#
# Catches the class of bug where an ingress-controller middleware at the
# edge overrides the app's Access-Control-Expose-Headers — silently
# stripping X-Next-Cursor (and X-RateLimit-*) from the browser's view.
# The app's own tests (api/internal/middleware/security_exposed_headers_test.go)
# cannot catch this because they only cover the app; the override happens
# between the app and the browser.
#
# Exit codes:
#   0 — all expected headers present in Access-Control-Expose-Headers.
#   1 — one or more expected headers missing (FAIL).
#   2 — infrastructure error (couldn't reach the API, non-HTTP response).
#
# Usage:
#   ./hack/monitor-cors-expose-headers.sh [API_BASE_URL]
#
# Defaults to https://api.safespaces.dev. Override for other deployments:
#   ./hack/monitor-cors-expose-headers.sh https://api.example.com
#
# Scheduling options:
#   - UptimeKuma / Pingdom: push monitor, 5-min interval, alert on non-zero exit.
#   - cron: */5 * * * * /path/to/monitor-cors-expose-headers.sh >> /var/log/cors-monitor.log 2>&1
#   - GitHub Actions: scheduled workflow, see .github/workflows/monitor-cors.yml
#                  (if added — not included here to avoid burning Actions
#                   minutes on every fork's default schedule).
#
# The monitor hits any unauthenticated endpoint (the security middleware
# sets CORS headers pre-auth, so a 401/404 still carries the expose-list).
# No credentials needed — safe to run from any location.
set -euo pipefail

API_BASE_URL="${1:-https://api.safespaces.dev}"
ORIGIN="https://chat.safespaces.dev"

# Endpoint that exists in the router (doesn't have to return 200; the
# security middleware runs pre-routing and sets CORS headers regardless).
# /api/v1/auth/config is public-facing and stable. If it's ever removed,
# any route under /api/v1/ will do — the CORS headers are set globally.
ENDPOINT="/api/v1/auth/config"

# Headers that the app emits (DefaultSecurityConfig().ExposedHeaders,
# api/internal/middleware/security.go:64). If any of these are missing
# from the wire response's Access-Control-Expose-Headers, the edge is
# overriding the app's value and the corresponding frontend feature
# breaks silently.
EXPECTED_HEADERS=(
  "X-Request-Id"
  "X-RateLimit-Limit"
  "X-RateLimit-Remaining"
  "X-RateLimit-Reset"
  "X-Next-Cursor"
)

echo "[$(date -u +%Y-%m-%dT%H:%M:%SZ)] Checking CORS expose-headers on ${API_BASE_URL}${ENDPOINT}"

# Fetch headers only (-I = HEAD). The -sS flags suppress progress but
# show errors. -H "Origin: ..." triggers the app's CORS path so the
# Access-Control-* headers are emitted. --max-time 10 prevents hangs.
RESPONSE=$(curl -sSI --max-time 10 \
  -H "Origin: ${ORIGIN}" \
  "${API_BASE_URL}${ENDPOINT}" 2>&1) || {
    echo "FAIL: could not reach ${API_BASE_URL}${ENDPOINT}"
    echo "  error: ${RESPONSE}"
    exit 2
}

# Extract the Access-Control-Expose-Headers value (case-insensitive).
# Use grep -i instead of awk's IGNORECASE (not portable to busybox awk
# / mawk). grep -i finds the line regardless of casing; cut extracts
# the value after the colon. tr -d removes the leading space.
#
# `|| true` because grep exits non-zero when no match is found — that's
# a valid case (the header is absent) and we handle it below. Without
# the `|| true`, `set -e` would terminate the script here.
EXPOSE=$(printf '%s\n' "${RESPONSE}" | grep -i '^access-control-expose-headers:' | cut -d: -f2- | sed 's/^[[:space:]]*//' || true)

if [ -z "${EXPOSE}" ]; then
  echo "FAIL: Access-Control-Expose-Headers header is absent from response."
  echo "  Either no CORS middleware is active, or the edge is stripping it."
  echo "  Response headers:"
  printf '%s\n' "${RESPONSE}" | sed 's/^/    /'
  exit 1
fi

# Normalize for substring checks: collapse whitespace, strip casing.
EXPOSE_LOWER=$(printf '%s' "${EXPOSE}" | tr '[:upper:]' '[:lower:]' | tr -d ' ')

FAIL=0
for expected in "${EXPECTED_HEADERS[@]}"; do
  # Header names are case-insensitive per RFC 7230; check lowercase.
  expected_lower=$(printf '%s' "${expected}" | tr '[:upper:]' '[:lower:]')
  if [[ "${EXPOSE_LOWER}" == *"${expected_lower}"* ]]; then
    echo "OK   ${expected}"
  else
    echo "FAIL ${expected} — missing from Access-Control-Expose-Headers"
    FAIL=1
  fi
done

echo "  expose-headers value: ${EXPOSE}"

if [ "${FAIL}" -ne 0 ]; then
  echo ""
  echo "FAIL: one or more expected headers are missing. The edge middleware"
  echo "is likely overriding the app's Access-Control-Expose-Headers value."
  echo "Check the Traefik Middleware CR bound to this ingress — its"
  echo "accessControlExposeHeaders list must mirror the app's full set."
  echo "See docs/operator/networking.md#cors-at-the-edge."
  exit 1
fi

echo "OK: all expected headers present."
exit 0
