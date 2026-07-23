// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package server

// OpenAPI ↔ router contract test.
//
// Compares the routes declared in sdks/openapi.yaml against the routes
// the gin router registers. Catches:
//
//   - handler exists, OpenAPI doesn't document it (clients have no
//     idea the endpoint is there)
//   - OpenAPI documents an endpoint, no handler exists (clients call,
//     get 404)
//   - method drift (spec says POST, handler is PUT)
//
// Why a route-only check rather than full schema diff: schema-level
// contract testing requires either oapi-codegen (and committing
// generated code) or a runtime checker like prism. Both are larger
// scope than this gate is meant to deliver. Route presence is the
// 80%-value catch.
//
// Allowlists at the bottom of the file document each known mismatch
// with rationale. New mismatches MUST land with a justification, not
// just a list addition.

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/mock"
	"gopkg.in/yaml.v3"

	"github.com/lenaxia/llmsafespaces/api/internal/handlers"
	"github.com/lenaxia/llmsafespaces/api/internal/interfaces"
	apilogger "github.com/lenaxia/llmsafespaces/api/internal/logger"
	imocks "github.com/lenaxia/llmsafespaces/api/internal/mocks"
)

// route is a (method, path) pair. Paths use Gin's :param syntax;
// OpenAPI's {param} syntax is normalized into this form before
// comparison.
type route struct {
	method string
	path   string
}

func (r route) String() string {
	return fmt.Sprintf("%-7s %s", r.method, r.path)
}

// TestOpenAPIRouterContract loads sdks/openapi.yaml, registers the
// router with mock services, and asserts that every spec-declared
// (method, path) is present in the router and vice versa. The
// allowlists at the bottom document permitted mismatches.
func TestOpenAPIRouterContract(t *testing.T) {
	specRoutes, err := loadOpenAPIRoutes()
	if err != nil {
		t.Fatalf("load openapi: %v", err)
	}
	if len(specRoutes) == 0 {
		t.Fatal("no routes found in openapi.yaml — likely a parse failure")
	}

	router := newContractFixture(t)
	implRoutes := loadRouterRoutes(router)

	specSet := make(map[route]bool, len(specRoutes))
	for _, r := range specRoutes {
		specSet[r] = true
	}
	implSet := make(map[route]bool, len(implRoutes))
	for _, r := range implRoutes {
		implSet[r] = true
	}

	// Spec routes that have no implementation. Documented endpoints
	// that don't actually exist will surface as 404s in production.
	var specOnly []route
	for r := range specSet {
		if !implSet[r] && !specOnlyAllowlist[r] {
			specOnly = append(specOnly, r)
		}
	}
	sort.Slice(specOnly, func(i, j int) bool { return specOnly[i].String() < specOnly[j].String() })

	// Implementation routes that aren't documented. Undocumented
	// endpoints can't be safely consumed by SDK users.
	var implOnly []route
	for r := range implSet {
		if !specSet[r] && !implOnlyAllowlist[r] {
			implOnly = append(implOnly, r)
		}
	}
	sort.Slice(implOnly, func(i, j int) bool { return implOnly[i].String() < implOnly[j].String() })

	if len(specOnly) > 0 {
		t.Errorf("OpenAPI declares %d route(s) with no matching handler:", len(specOnly))
		for _, r := range specOnly {
			t.Errorf("  %s", r)
		}
		t.Errorf("Either implement the handler or add the route to specOnlyAllowlist with rationale.")
	}

	if len(implOnly) > 0 {
		t.Errorf("Router has %d route(s) not documented in OpenAPI:", len(implOnly))
		for _, r := range implOnly {
			t.Errorf("  %s", r)
		}
		t.Errorf("Either document in sdks/openapi.yaml or add to implOnlyAllowlist with rationale.")
	}
}

// loadOpenAPIRoutes parses sdks/openapi.yaml and returns one route
// entry per (path, method). The OpenAPI servers entry prepends
// /api/v1; we use the absolute path the router actually registers.
func loadOpenAPIRoutes() ([]route, error) {
	// Walk up to repo root; tests run from the package dir.
	repoRoot, err := findRepoRoot()
	if err != nil {
		return nil, err
	}
	specPath := filepath.Join(repoRoot, "sdks", "openapi.yaml")
	data, err := os.ReadFile(specPath)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", specPath, err)
	}

	// We only need the .paths object; everything else is irrelevant
	// for route-presence checking. A targeted struct keeps the YAML
	// parse fast and skips schema validation surprises.
	var spec struct {
		Servers []struct {
			URL string `yaml:"url"`
		} `yaml:"servers"`
		Paths map[string]map[string]any `yaml:"paths"`
	}
	if err := yaml.Unmarshal(data, &spec); err != nil {
		return nil, fmt.Errorf("parse openapi: %w", err)
	}

	// servers[0].url is e.g. "/api/v1". Prepend to each path so we
	// compare against the router's absolute paths.
	var prefix string
	if len(spec.Servers) > 0 {
		prefix = strings.TrimRight(spec.Servers[0].URL, "/")
	}

	validMethods := map[string]bool{
		"get": true, "post": true, "put": true, "delete": true,
		"patch": true, "head": true, "options": true,
	}

	var out []route
	for path, methods := range spec.Paths {
		// Health endpoints are documented at root scope (no /api/v1
		// prefix) per worklog 0066. Everything else gets the prefix.
		var fullPath string
		switch path {
		case "/livez", "/readyz", "/healthz", "/health":
			fullPath = path
		default:
			fullPath = prefix + path
		}

		for m := range methods {
			lm := strings.ToLower(m)
			if !validMethods[lm] {
				// "parameters" / "summary" / "description" siblings
				// of method maps; skip silently.
				continue
			}
			out = append(out, route{
				method: strings.ToUpper(lm),
				path:   normalizeOpenAPIPath(fullPath),
			})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].String() < out[j].String() })
	return out, nil
}

// normalizeOpenAPIPath rewrites OpenAPI {param} to Gin :param so the
// comparison is apples-to-apples.
func normalizeOpenAPIPath(p string) string {
	// Quick replace: {x} -> :x. Multiple params handled with strings.Builder.
	var b strings.Builder
	b.Grow(len(p))
	i := 0
	for i < len(p) {
		c := p[i]
		if c == '{' {
			end := strings.IndexByte(p[i:], '}')
			if end < 0 {
				// Unbalanced; pass through as-is.
				b.WriteString(p[i:])
				return b.String()
			}
			name := p[i+1 : i+end]
			b.WriteByte(':')
			b.WriteString(name)
			i += end + 1
			continue
		}
		b.WriteByte(c)
		i++
	}
	return b.String()
}

// loadRouterRoutes walks gin's registered routes.
func loadRouterRoutes(r *gin.Engine) []route {
	var out []route
	for _, ri := range r.Routes() {
		out = append(out, route{
			method: ri.Method,
			path:   ri.Path,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].String() < out[j].String() })
	return out
}

// findRepoRoot walks up looking for go.mod. Tests can run from
// arbitrary working directories under different go test invocations.
func findRepoRoot() (string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", fmt.Errorf("no go.mod found from working directory")
		}
		dir = parent
	}
}

// -----------------------------------------------------------------------------
// Allowlists
//
// Each entry MUST have a comment explaining why the mismatch is OK.
// A real production gap should NOT land here — fix it instead.
// -----------------------------------------------------------------------------

// specOnlyAllowlist: routes documented in OpenAPI but not registered in
// the test fixture. These are feature-gated routes (registered via
// `if cfg.XxxHandler != nil` in the router) that the test fixture does
// not wire because the handlers have complex dependencies. They exist
// in production deployments with the handlers configured.
var specOnlyAllowlist = map[route]bool{
	// Feature-gated routes not wired in the test fixture.
	// Each is registered behind `if cfg.XxxHandler != nil` in router.go
	// and exists in production. The test fixture (newContractFixture)
	// does not construct these handlers.
	{method: "GET", path: "/api/v1/admin/agent-roles"}:                                          true,
	{method: "POST", path: "/api/v1/admin/agent-roles"}:                                         true,
	{method: "GET", path: "/api/v1/admin/agent-roles/:id"}:                                      true,
	{method: "PUT", path: "/api/v1/admin/agent-roles/:id"}:                                      true,
	{method: "DELETE", path: "/api/v1/admin/agent-roles/:id"}:                                   true,
	{method: "GET", path: "/api/v1/admin/prompt"}:                                               true,
	{method: "PUT", path: "/api/v1/admin/prompt"}:                                               true,
	{method: "GET", path: "/api/v1/admin/provider-credentials"}:                                 true,
	{method: "POST", path: "/api/v1/admin/provider-credentials"}:                                true,
	{method: "GET", path: "/api/v1/admin/provider-credentials/:id"}:                             true,
	{method: "PUT", path: "/api/v1/admin/provider-credentials/:id"}:                             true,
	{method: "DELETE", path: "/api/v1/admin/provider-credentials/:id"}:                          true,
	{method: "GET", path: "/api/v1/admin/provider-credentials/:id/models"}:                      true,
	{method: "POST", path: "/api/v1/admin/provider-credentials/:id/auto-apply"}:                 true,
	{method: "GET", path: "/api/v1/admin/provider-credentials/:id/auto-apply"}:                  true,
	{method: "DELETE", path: "/api/v1/admin/provider-credentials/:id/auto-apply/:targetType/:targetId"}: true,
	{method: "GET", path: "/api/v1/provider-credentials"}:                                       true,
	{method: "POST", path: "/api/v1/provider-credentials"}:                                      true,
	{method: "GET", path: "/api/v1/provider-credentials/:id"}:                                   true,
	{method: "DELETE", path: "/api/v1/provider-credentials/:id"}:                                true,
	{method: "GET", path: "/api/v1/provider-credentials/:id/models"}:                            true,
	{method: "GET", path: "/api/v1/provider-credentials/:id/bindings"}:                          true,
	{method: "POST", path: "/api/v1/provider-credentials/:id/bind/:workspaceId"}:                true,
	{method: "DELETE", path: "/api/v1/provider-credentials/:id/bind/:workspaceId"}:              true,
	{method: "GET", path: "/api/v1/usage"}:                                                      true,
	{method: "GET", path: "/api/v1/usage/workspaces/:id"}:                                       true,
	{method: "GET", path: "/api/v1/usage/quota"}:                                                true,
	{method: "POST", path: "/api/v1/workspaces/:id/agent/reload"}:                               true,
	{method: "GET", path: "/api/v1/workspaces/:id/agent-role"}:                                  true,
	{method: "PUT", path: "/api/v1/workspaces/:id/agent-role"}:                                  true,
	{method: "DELETE", path: "/api/v1/workspaces/:id/agent-role"}:                               true,
	{method: "GET", path: "/api/v1/workspaces/:id/effective-agent-role"}:                        true,
	{method: "GET", path: "/api/v1/workspaces/:id/prompt"}:                                      true,
	{method: "PUT", path: "/api/v1/workspaces/:id/prompt"}:                                      true,
	{method: "POST", path: "/api/v1/auth/lookup"}:                                               true,
	{method: "POST", path: "/api/v1/auth/password-reset/request"}:                               true,
	{method: "POST", path: "/api/v1/auth/password-reset/confirm"}:                               true,
	{method: "POST", path: "/api/v1/auth/verify-email"}:                                         true,
	{method: "POST", path: "/api/v1/auth/verify-email/resend"}:                                  true,
	{method: "POST", path: "/api/v1/auth/unlock-dek"}:                                           true,
}

// implOnlyAllowlist: routes registered by the router but not in
// OpenAPI. Permitted for legitimately-internal routes that aren't
// part of the public API contract.
var implOnlyAllowlist = map[route]bool{
	// /metrics is a Prometheus scrape endpoint exposed for the
	// in-cluster monitoring stack. Not part of the public API.
	{method: "GET", path: "/metrics"}: true,

	// /health (no prefix) — legacy alias for /livez. Documented in
	// OpenAPI under the root path. The router also serves an
	// unprefixed /health for backward compat with existing dashboards
	// that hardcode it. Documented variant is /api/v1/health under
	// the prefix; the unprefixed one is implementation-only.
	{method: "GET", path: "/health"}: true,

	// Epic 28: User-scoped SSE event stream. Long-lived connection,
	// not a typical REST endpoint. Not documented in OpenAPI.
	{method: "GET", path: "/api/v1/events"}: true,
}

// -----------------------------------------------------------------------------
// Mock fixture: a minimum viable services bundle so NewRouter can wire
// the route table. We don't exercise handlers — just need them
// registered.
// -----------------------------------------------------------------------------

type contractMockServices struct {
	auth interfaces.AuthService
	met  interfaces.MetricsService
	db   interfaces.DatabaseService
	ca   interfaces.CacheService
	ws   interfaces.WorkspaceService
	rl   interfaces.RateLimiterService
}

func (s *contractMockServices) GetAuth() interfaces.AuthService           { return s.auth }
func (s *contractMockServices) GetDatabase() interfaces.DatabaseService   { return s.db }
func (s *contractMockServices) GetCache() interfaces.CacheService         { return s.ca }
func (s *contractMockServices) GetMetrics() interfaces.MetricsService     { return s.met }
func (s *contractMockServices) GetWorkspace() interfaces.WorkspaceService { return s.ws }
func (s *contractMockServices) GetRateLimiter() interfaces.RateLimiterService {
	return s.rl
}
func (s *contractMockServices) GetMetering() interfaces.MeteringService { return nil }

func newContractFixture(t *testing.T) *gin.Engine {
	t.Helper()
	gin.SetMode(gin.TestMode)
	log, err := apilogger.New(false, "error", "json")
	if err != nil {
		t.Fatalf("logger: %v", err)
	}

	auth := &imocks.MockAuthMiddlewareService{}
	met := &imocks.MockMetricsService{}
	met.On("RecordRequest", mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything).Maybe()
	auth.On("AuthMiddleware").Return(gin.HandlerFunc(func(c *gin.Context) { c.Next() }))
	auth.On("GetUserID", mock.Anything).Return("")

	svc := &contractMockServices{auth: auth, met: met}
	// Provide non-nil handler stubs so the conditional-registration
	// guards in NewRouter wire every route. We never invoke the
	// handlers; the test asserts route presence only.
	cfg := RouterConfig{
		Debug:               false,
		SettingsHandler:     &handlers.SettingsHandler{},
		SecretsHandler:      &handlers.SecretsHandler{},
		ModelsHandler:       &handlers.ModelsHandler{},
		WorkspaceEnvHandler: &handlers.WorkspaceEnvHandler{},
		RotateKeyHandler:    &handlers.RotateKeyHandler{},
		TerminalHandler:     &handlers.TerminalHandler{},
	}
	// proxyHandler also has a conditional wiring guard (sessions,
	// events, message, prompt, abort routes). Pass a zero-value stub
	// for the same route-presence reason.
	proxyStub := &handlers.ProxyHandler{}
	return NewRouter(svc, log, proxyStub, cfg)
}
