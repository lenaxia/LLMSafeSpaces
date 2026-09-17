// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/lenaxia/llmsafespaces/pkg/redact"
	"github.com/lenaxia/llmsafespaces/pkg/secrets"
	"github.com/lenaxia/llmsafespaces/pkg/version"
)

// Machine-readable rejection codes (K3/§4.7): violations are
// CredentialRejected-class events, never silent.
const (
	byoRejectUnauthorized        = "unauthorized"
	byoRejectCredentialStale     = "credential_stale"
	byoRejectScopeViolation      = "scope_violation"
	byoRejectSanitization        = "sanitization_refused"
	byoRejectQuota               = "quota_exceeded"
	byoRejectUpstreamUnreachable = "upstream_unreachable"
)

const (
	byoDefaultMaxBodyBytes = 10 << 20 // 10 MiB request cap (§4.7 rule 3)
	byoDefaultMaxRespBytes = 50 << 20 // response cap
	byoInternalAuthHeader  = "Authorization"
)

// sanitizeMeta strips control characters from K7 metadata values before
// logging (workspace/slug/keyID originate from client-visible routing
// paths — a newline could otherwise inject forged log lines).
func sanitizeMeta(s string) string {
	return strings.Map(func(r rune) rune {
		if r < 32 || r == 127 {
			return -1
		}
		return r
	}, s)
}

// byoServerConfig carries the deployment-tunable knobs.
type byoServerConfig struct {
	listenAddr    string
	namespace     string
	maxBodyBytes  int64
	maxRespBytes  int64
	quotaWindow   time.Duration
	quotaRequests int64
	quotaBytes    int64
	retention     time.Duration
}

func byoReject(w http.ResponseWriter, code string, status int, detail string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{
		"error":  "relay_rejected",
		"reason": code,
		"detail": detail,
	})
}

// byoMintService is the minter surface the server depends on: token
// mint/verify plus the controller-facing mint-auth key (static in tests,
// Secret-held and cached at runtime).
type byoMintService interface {
	Mint(payload byoTokenPayload) (string, error)
	Verify(token string) (byoTokenPayload, error)
	AuthKey(ctx context.Context) (string, error)
}

// staticMintService serves a fixed signing + auth key (tests).
type staticMintService struct {
	minter  *byoTokenMinter
	authKey string
}

func (s staticMintService) Mint(p byoTokenPayload) (string, error)   { return s.minter.Mint(p) }
func (s staticMintService) Verify(t string) (byoTokenPayload, error) { return s.minter.Verify(t) }
func (s staticMintService) AuthKey(context.Context) (string, error)  { return s.authKey, nil }

// byoServer is the BYO resolve router (design 0058 §4.1 hop 5): verify
// token → resolve envelope from the informer cache (local decrypt, µs) →
// sanitize → inject the real key → forward upstream. Persistence posture
// (K7): metadata-only logging; bodies never logged; body-adjacent
// diagnostics pass the redaction engine.
type byoServer struct {
	cfg      byoServerConfig
	minter   byoMintService
	cache    *byoEnvelopeCache
	resolve  resolveDispatcher
	quota    *byoWorkspaceQuota
	redactor *redact.Redactor
	client   *http.Client
	metrics  *byoMetrics
}

func (s *byoServer) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", s.handleHealthz)
	mux.HandleFunc("/metrics", s.handleMetrics)
	mux.HandleFunc("/internal/v1/tokens", s.requireMintAuth(s.handleMintToken))
	mux.HandleFunc("/internal/v1/keys/rotate", s.requireMintAuth(s.handleRotateKeys))
	mux.HandleFunc("/w/", s.handleWorkspaceTraffic)
	return mux
}

func (s *byoServer) handleHealthz(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("X-Llmsafespaces-Version", version.Version)
	w.WriteHeader(http.StatusOK)
}

func (s *byoServer) handleMetrics(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	s.metrics.writePrometheus(w)
}

// requireMintAuth gates the internal API: only the controller holds the
// mint key (delivered as a Secret; design §4.4).
func (s *byoServer) requireMintAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		key, err := s.minter.AuthKey(r.Context())
		if err != nil {
			byoReject(w, byoRejectUnauthorized, http.StatusServiceUnavailable, "mint key unavailable")
			return
		}
		token, ok := extractBearerToken(r.Header.Get(byoInternalAuthHeader))
		if !ok || subtle.ConstantTimeCompare([]byte(token), []byte(key)) != 1 {
			byoReject(w, byoRejectUnauthorized, http.StatusUnauthorized, "mint auth required")
			s.metrics.recordInternal(r.URL.Path, http.StatusUnauthorized)
			return
		}
		next(w, r)
	}
}

type mintTokenRequest struct {
	WorkspaceID    string   `json:"workspaceID"`
	ProviderSlug   string   `json:"providerSlug"`
	BaseURL        string   `json:"baseURL"`
	ModelAllowlist []string `json:"modelAllowlist"`
	TTLSec         int64    `json:"ttlSeconds"`
	KeyID          string   `json:"keyID"`
}

func (s *byoServer) handleMintToken(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		byoReject(w, byoRejectSanitization, http.StatusMethodNotAllowed, "POST only")
		return
	}
	var req mintTokenRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&req); err != nil {
		byoReject(w, byoRejectSanitization, http.StatusBadRequest, "invalid mint request")
		return
	}
	now := time.Now()
	ttl := time.Duration(req.TTLSec) * time.Second
	if ttl <= 0 || ttl > 7*24*time.Hour {
		byoReject(w, byoRejectSanitization, http.StatusBadRequest, "ttlSeconds out of range")
		return
	}
	token, err := s.minter.Mint(byoTokenPayload{
		WorkspaceID:    req.WorkspaceID,
		ProviderSlug:   req.ProviderSlug,
		BaseURL:        req.BaseURL,
		ModelAllowlist: req.ModelAllowlist,
		IssuedAt:       now.Unix(),
		ExpiresAt:      now.Add(ttl).Unix(),
		KeyID:          req.KeyID,
	})
	if err != nil {
		byoReject(w, byoRejectSanitization, http.StatusBadRequest, "invalid scope")
		s.metrics.recordInternal(r.URL.Path, http.StatusBadRequest)
		return
	}
	s.metrics.recordInternal(r.URL.Path, http.StatusOK)
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{"token": token})
}

func (s *byoServer) handleRotateKeys(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		byoReject(w, byoRejectSanitization, http.StatusMethodNotAllowed, "POST only")
		return
	}
	pub, err := s.resolve.keys.Rotate(r.Context())
	if err != nil {
		log.Printf("byo-router: rotate failed: %v", err) // metadata only
		byoReject(w, byoRejectUpstreamUnreachable, http.StatusInternalServerError, "rotate failed")
		s.metrics.recordInternal(r.URL.Path, http.StatusInternalServerError)
		return
	}
	s.metrics.recordInternal(r.URL.Path, http.StatusOK)
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"keyID":      secrets.HPKEKeyID(pub.Generation),
		"generation": pub.Generation,
		"publicKey":  pub.PublicKey,
	})
}

// parseWorkspacePath splits /w/<workspaceID>/<providerSlug>/v1/<sub...>.
var errBadWorkspacePath = errors.New("expected /w/<workspaceID>/<providerSlug>/v1/<subpath>")

func parseWorkspacePath(path string) (workspaceID, providerSlug, subPath string, err error) {
	rest, ok := strings.CutPrefix(path, "/w/")
	if !ok {
		return "", "", "", errBadWorkspacePath
	}
	parts := strings.SplitN(rest, "/", 3)
	if len(parts) != 3 || parts[0] == "" || parts[1] == "" || !strings.HasPrefix(parts[2], "v1/") {
		return "", "", "", errBadWorkspacePath
	}
	return parts[0], parts[1], strings.TrimPrefix(parts[2], "v1/"), nil
}

// handleWorkspaceTraffic is the full §4.7 pipeline. Logging is
// metadata-only (K7): workspace, slug, keyID, status, latency, bytes,
// rejection reason.
func (s *byoServer) handleWorkspaceTraffic(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	workspaceID, providerSlug, subPath, err := parseWorkspacePath(r.URL.Path)
	if err != nil {
		byoReject(w, byoRejectSanitization, http.StatusNotFound, "bad routing path")
		return
	}

	reject := func(code string, status int, detail string) {
		s.metrics.recordRequest(workspaceID, providerSlug, status)
		log.Printf("byo-router: ws=%s slug=%s status=%d reason=%s latency=%s", //nolint:gosec // sanitized metadata only (K7)
			sanitizeMeta(workspaceID), sanitizeMeta(providerSlug), status, sanitizeMeta(code), time.Since(start))
		byoReject(w, code, status, detail)
	}

	// (1) Method/path allowlist before anything touches a body.
	if err := byoMethodPathAllowed(r.Method, subPath); err != nil {
		reject(byoRejectSanitization, http.StatusNotFound, err.Error())
		return
	}

	// (2) Token: HMAC ∧ staged-Secret-present ∧ not-expired (K3).
	rawToken, ok := extractBearerToken(r.Header.Get(byoInternalAuthHeader))
	if !ok {
		reject(byoRejectUnauthorized, http.StatusUnauthorized, "bearer token required")
		return
	}
	payload, err := s.minter.Verify(rawToken)
	if err != nil {
		reject(byoRejectUnauthorized, http.StatusUnauthorized, "token invalid or expired")
		return
	}
	if payload.WorkspaceID != workspaceID || payload.ProviderSlug != providerSlug {
		reject(byoRejectScopeViolation, http.StatusForbidden, "token scope does not match routing path")
		return
	}

	envelope, cached := s.cache.Envelope(workspaceID, providerSlug)
	if !cached {
		// Revocation (Secret deletion) or not-yet-staged: HMAC-valid but
		// no ciphertext — fail closed with the distinct stale signal.
		reject(byoRejectCredentialStale, http.StatusUnauthorized, "no staged credential")
		return
	}
	if _, envKeyID, ierr := secrets.InspectStagingEnvelope(envelope); ierr == nil && envKeyID != payload.KeyID {
		reject(byoRejectCredentialStale, http.StatusUnauthorized, "token keyID does not match staged envelope")
		return
	}

	// (3) Quota (§4.7 rule 6).
	if !s.quota.Allow(workspaceID) {
		w.Header().Set("Retry-After", "10")
		reject(byoRejectQuota, http.StatusTooManyRequests, "workspace quota exceeded")
		return
	}

	// (4) Read the request body (capped) for the model-allowlist check and
	// the staged-key body redaction.
	var bodyBytes []byte
	if r.Body != nil {
		capped := io.LimitReader(r.Body, s.cfg.maxBodyBytes+1)
		bodyBytes, err = io.ReadAll(capped)
		if err != nil {
			reject(byoRejectSanitization, http.StatusBadRequest, "reading request body")
			return
		}
		if int64(len(bodyBytes)) > s.cfg.maxBodyBytes {
			reject(byoRejectSanitization, http.StatusRequestEntityTooLarge, "request body exceeds cap")
			return
		}
	}
	shape, err := parseByoRequestBody(bodyBytes)
	if err != nil {
		reject(byoRejectSanitization, http.StatusBadRequest, "request body must be JSON")
		return
	}
	if err := byoModelAllowed(shape, payload.ModelAllowlist); err != nil {
		reject(byoRejectScopeViolation, http.StatusForbidden, "model outside token allowlist")
		return
	}

	// (5) Per-request resolve — local decrypt, discard (D2). Registration
	// with the redaction engine happens inside Resolve (US-72.1 seam), so
	// the dynamic staged-key rules below are live for this key.
	providerKey, err := s.resolve.Resolve(r.Context(), envelope)
	if err != nil {
		if errors.Is(err, errUnknownStagingKey) {
			reject(byoRejectCredentialStale, http.StatusUnauthorized, "staged credential unreadable")
			return
		}
		reject(byoRejectCredentialStale, http.StatusUnauthorized, "resolve failed")
		return
	}

	// (6) Body redaction: the precise dynamic staged-key rules — a
	// resolved key can never be echoed back through the relay (§4.9).
	redactedBody := s.redactor.RedactDynamicOnly(string(bodyBytes))

	// (7) Forward: sanitized headers, resolved key injected, destination
	// pinned to the token's baseURL (SSRF-proof by construction, §4.4).
	target := strings.TrimSuffix(payload.BaseURL, "/") + "/" + subPath
	if r.URL.RawQuery != "" {
		target += "?" + r.URL.RawQuery
	}
	upstreamReq, err := http.NewRequestWithContext(r.Context(), r.Method, target, strings.NewReader(redactedBody)) //nolint:gosec // target is the token's baseURL, controller-minted (SSRF-proof pinning, §4.4)
	if err != nil {
		reject(byoRejectUpstreamUnreachable, http.StatusBadGateway, "building upstream request")
		return
	}
	copyByoHeaders(upstreamReq.Header, r.Header)
	upstreamReq.Header.Set(byoInternalAuthHeader, "Bearer "+string(providerKey))
	if len(bodyBytes) > 0 {
		upstreamReq.Header.Set("Content-Type", "application/json")
	}
	upstreamReq.ContentLength = int64(len(redactedBody))

	resp, err := s.client.Do(upstreamReq) //nolint:gosec // target is the token's baseURL (see above)
	if err != nil {
		// K7: log the unwrapped transport error only (proxy.go's
		// logUpstreamError precedent — URLs may carry secrets).
		if ue, ok := err.(*url.Error); ok { //nolint:errorlint // matching the fleet router's precedent
			err = ue.Err
		}
		log.Printf("byo-router: ws=%s slug=%s upstream request failed: %v", //nolint:gosec // sanitized metadata + unwrapped transport error
			sanitizeMeta(workspaceID), sanitizeMeta(providerSlug), err)
		if r.Context().Err() != nil {
			return
		}
		reject(byoRejectUpstreamUnreachable, http.StatusBadGateway, "upstream unreachable")
		return
	}
	defer func() { _ = resp.Body.Close() }()

	// (8) Response: sanitized headers, capped, streamed with the staged-key
	// redaction carry, flushed per chunk for SSE.
	copyByoHeaders(w.Header(), resp.Header)
	w.WriteHeader(resp.StatusCode)

	matcher := newExactValueMatcher(s.redactor)
	stream := newStreamRedactor(matcher)
	var total int64
	flusher, canFlush := w.(http.Flusher)
	buf := make([]byte, 32*1024)
	for {
		n, readErr := resp.Body.Read(buf)
		if n > 0 {
			total += int64(n)
			if total > s.cfg.maxRespBytes {
				// Over cap: stop copying. The stream is truncated at a
				// chunk boundary — visible to the client as a short body.
				log.Printf("byo-router: ws=%s slug=%s response exceeded cap (%d bytes)", //nolint:gosec // sanitized metadata only
					sanitizeMeta(workspaceID), sanitizeMeta(providerSlug), s.cfg.maxRespBytes)
				return
			}
			out := stream.Write(buf[:n])
			if len(out) > 0 {
				_, _ = w.Write(out)
				if canFlush {
					flusher.Flush()
				}
			}
		}
		if readErr != nil {
			break
		}
	}
	if tail := stream.Flush(); len(tail) > 0 {
		_, _ = w.Write(tail)
		if canFlush {
			flusher.Flush()
		}
	}

	s.quota.Record(workspaceID, total)
	s.metrics.recordRequest(workspaceID, providerSlug, resp.StatusCode)
	s.metrics.recordBytes(workspaceID, total)
	log.Printf("byo-router: ws=%s slug=%s keyID=%s status=%d latency=%s bytes=%d", //nolint:gosec // sanitized metadata only (K7)
		sanitizeMeta(workspaceID), sanitizeMeta(providerSlug), sanitizeMeta(payload.KeyID), resp.StatusCode, time.Since(start), total)
}
