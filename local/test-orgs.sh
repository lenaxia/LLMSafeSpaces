#!/usr/bin/env bash
set -euo pipefail

# Org-core CRUD e2e (#1088) — exercises the documented wire contracts for
# /api/v1/orgs* against a LIVE API server, following local/test-auth.sh
# conventions. Org creation is platform-admin gated and billing needs
# Stripe, so the regular-user-reachable surface is exactly the guard and
# error contracts — which are the contracts SDK consumers depend on.
# Deeper org CRUD (create/update/delete happy paths) is covered by the
# router wire gate (api/internal/server/router_orgs_wire_test.go) with
# the real handler + guard middleware, and by orgs.hurl against the spec.

BASE_URL="${1:-http://localhost:8080}"
PASS=0
FAIL=0

ok()   { ((PASS++)); printf "\033[32m  PASS\033[0m %s\n" "$1"; }
fail() { ((FAIL++)); printf "\033[31m  FAIL\033[0m %s — %s\n" "$1" "$2"; }

assert_status() {
  local label="$1" expected="$2" actual="$3"
  if [ "$expected" -eq "$actual" ]; then
    ok "$label"
  else
    fail "$label" "expected $expected, got $actual"
  fi
}

assert_contains() {
  local label="$1" haystack="$2" needle="$3"
  if echo "$haystack" | grep -q "$needle"; then
    ok "$label"
  else
    fail "$label" "response does not contain '$needle'"
  fi
}

echo "=== LLMSafeSpaces Org-Core E2E Tests ==="
echo "Target: $BASE_URL"
echo ""

# --- 0. Register + login (fixture) ---
REG_RESP=$(curl -s -w "\n%{http_code}" -X POST "$BASE_URL/api/v1/auth/register" \
  -H "Content-Type: application/json" \
  -d '{"username":"e2eorgs","email":"e2e-orgs@example.com","password":"securepassword123"}')
REG_STATUS=$(echo "$REG_RESP" | tail -1)
if [ "$REG_STATUS" != "201" ]; then
  # already registered from a previous run — log in instead
  LOGIN_RESP=$(curl -s -w "\n%{http_code}" -X POST "$BASE_URL/api/v1/auth/login" \
    -H "Content-Type: application/json" \
    -d '{"email":"e2e-orgs@example.com","password":"securepassword123"}')
  TOKEN=$(echo "$LOGIN_RESP" | head -n -1 | grep -o '"token":"[^"]*"' | cut -d'"' -f4)
else
  TOKEN=$(echo "$REG_RESP" | head -n -1 | grep -o '"token":"[^"]*"' | cut -d'"' -f4)
fi
if [ -z "$TOKEN" ]; then
  echo "FATAL: could not obtain a token" >&2
  exit 1
fi
ok "fixture: authenticated"

# --- 1. GET /orgs (unauthenticated → 401) ---
RESP=$(curl -s -o /dev/null -w "%{http_code}" "$BASE_URL/api/v1/orgs")
assert_status "orgs list: unauthenticated → 401" 401 "$RESP"

# --- 2. GET /orgs (authenticated → 200, bare array) ---
RESP=$(curl -s -w "\n%{http_code}" -H "Authorization: Bearer $TOKEN" "$BASE_URL/api/v1/orgs")
BODY=$(echo "$RESP" | head -n -1)
STATUS=$(echo "$RESP" | tail -1)
assert_status "orgs list: authenticated → 200" 200 "$STATUS"
if echo "$BODY" | grep -q '^\['; then
  ok "orgs list: bare JSON array (documented wrapper)"
else
  fail "orgs list: bare JSON array" "body starts with: $(echo "$BODY" | head -c 40)"
fi

# --- 3. GET /orgs/{id} non-member → 403 ---
RESP=$(curl -s -w "\n%{http_code}" -H "Authorization: Bearer $TOKEN" "$BASE_URL/api/v1/orgs/00000000-0000-0000-0000-000000000041")
BODY=$(echo "$RESP" | head -n -1)
STATUS=$(echo "$RESP" | tail -1)
assert_status "org get: non-member → 403" 403 "$STATUS"
assert_contains "org get: error body" "$BODY" '"error"'

# --- 4. GET /orgs/{id}/members non-member → 403 ---
RESP=$(curl -s -o /dev/null -w "%{http_code}" -H "Authorization: Bearer $TOKEN" "$BASE_URL/api/v1/orgs/00000000-0000-0000-0000-000000000041/members")
assert_status "members list: non-member → 403" 403 "$RESP"

# --- 5. GET /orgs/{id}/workspaces non-member → 403 ---
RESP=$(curl -s -o /dev/null -w "%{http_code}" -H "Authorization: Bearer $TOKEN" "$BASE_URL/api/v1/orgs/00000000-0000-0000-0000-000000000041/workspaces")
assert_status "org workspaces: non-member → 403" 403 "$RESP"

# --- 6. PUT /orgs/{id} non-member (admin guard) → 403 ---
RESP=$(curl -s -o /dev/null -w "%{http_code}" -X PUT -H "Authorization: Bearer $TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"name":"Hijacked"}' "$BASE_URL/api/v1/orgs/00000000-0000-0000-0000-000000000041")
assert_status "org update: non-member → 403" 403 "$RESP"

# --- 7. DELETE /orgs/{id} non-member (admin guard) → 403 ---
RESP=$(curl -s -o /dev/null -w "%{http_code}" -X DELETE -H "Authorization: Bearer $TOKEN" \
  "$BASE_URL/api/v1/orgs/00000000-0000-0000-0000-000000000041")
assert_status "org delete: non-member → 403" 403 "$RESP"

# --- 8. POST /orgs (regular user → 403, create is platform-admin gated) ---
RESP=$(curl -s -w "\n%{http_code}" -X POST -H "Authorization: Bearer $TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"name":"Rogue Org","slug":"rogue-org","ownerEmail":"e2e-orgs@example.com"}' \
  "$BASE_URL/api/v1/orgs")
BODY=$(echo "$RESP" | head -n -1)
STATUS=$(echo "$RESP" | tail -1)
assert_status "org create: regular user → 403" 403 "$STATUS"
assert_contains "org create: error body" "$BODY" '"error"'

# --- 9. POST /orgs/{id}/members/{userID}/verify non-member (admin guard) → 403 ---
RESP=$(curl -s -o /dev/null -w "%{http_code}" -X POST -H "Authorization: Bearer $TOKEN" \
  "$BASE_URL/api/v1/orgs/00000000-0000-0000-0000-000000000041/members/00000000-0000-0000-0000-000000000042/verify")
assert_status "member verify: non-member → 403" 403 "$RESP"

# --- 10. POST billing/checkout (billing unconfigured → 503) ---
RESP=$(curl -s -w "\n%{http_code}" -X POST -H "Authorization: Bearer $TOKEN" \
  -H "Content-Type: application/json" -d '{"planId":"team"}' \
  "$BASE_URL/api/v1/orgs/00000000-0000-0000-0000-000000000041/billing/checkout")
# Guard order: non-member 403 fires before billing 503 on this route for
# non-members; an org ADMIN of an org without Stripe would see 503. The
# router wire gate covers the 503; here we assert the guard held.
STATUS=$(echo "$RESP" | tail -1)
if [ "$STATUS" = "403" ] || [ "$STATUS" = "503" ]; then
  ok "billing checkout: guard-or-unconfigured contract (got $STATUS)"
else
  fail "billing checkout" "expected 403 or 503, got $STATUS"
fi

echo ""
echo "=== Results: $PASS passed, $FAIL failed ==="
[ "$FAIL" -eq 0 ]
