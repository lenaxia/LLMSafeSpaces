#!/bin/sh
# Inject runtime environment variables into env.json at container start.
#
# JSON is hand-formatted rather than using `jq` (not in the base image).
# Every value MUST be JSON-string-safe — envsubst-style ${VAR} passthrough
# lets shell metacharacters through. Escape any user-controlled values here
# if you ever add them; API_BASE_URL / TURNSTILE_SITE_KEY are operator-set.
cat > /usr/share/nginx/html/env.json <<EOF
{
  "apiBaseUrl": "${API_BASE_URL:-/api/v1}",
  "turnstileSiteKey": "${TURNSTILE_SITE_KEY:-}"
}
EOF
