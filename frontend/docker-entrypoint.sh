#!/bin/sh
# Inject runtime environment variables into env.json at container start
cat > /usr/share/nginx/html/env.json <<EOF
{
  "apiBaseUrl": "${API_BASE_URL:-/api/v1}"
}
EOF
