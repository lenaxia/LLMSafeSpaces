FROM node:22-bookworm-slim AS build
WORKDIR /app
COPY package.json package-lock.json ./
RUN npm ci
COPY . .
RUN npm run build

# nginxinc/nginx-unprivileged runs as UID 101 (nginx) and listens on
# 8080 by default. This avoids Trivy DS-0002 (running as root) and
# matches the security posture of the controller pods. The chart
# deployment templates already expose port 8080 internally; the
# Service maps 80 -> 8080.
#
# We chown /usr/share/nginx/html to the nginx user so the runtime
# entrypoint (40-envsubst-env-json.sh) can write env.json into the
# document root. Without the chown, the entrypoint fails with
# "Permission denied" because root copied the dist files in but
# nginx (UID 101) needs to overwrite env.json on every container
# start.
FROM nginxinc/nginx-unprivileged:1.27-alpine
USER root
# Patch system libraries (openssl, libpng, libxml2, musl, nghttp2, zlib) to
# their fixed versions. The nginx-unprivileged:1.27-alpine base lags
# Alpine's security advisories; an explicit apk upgrade keeps the frontend
# image clean for the release Trivy gate (control-plane images must have
# zero HIGH/CRITICAL findings, unlike the warn-only base runtime image).
RUN apk upgrade --no-cache
COPY --from=build --chown=nginx:nginx /app/dist /usr/share/nginx/html
COPY nginx.conf /etc/nginx/conf.d/default.conf
COPY docker-entrypoint.sh /docker-entrypoint.d/40-envsubst-env-json.sh
RUN chmod +x /docker-entrypoint.d/40-envsubst-env-json.sh \
    && chown -R nginx:nginx /usr/share/nginx/html
USER nginx
EXPOSE 8080
