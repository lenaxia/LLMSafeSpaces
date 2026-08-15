// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package app

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/lenaxia/llmsafespaces/api/internal/config"
	"github.com/lenaxia/llmsafespaces/api/internal/handlers"
	"github.com/lenaxia/llmsafespaces/api/internal/logger"
	"github.com/lenaxia/llmsafespaces/api/internal/services/auth"
	"github.com/lenaxia/llmsafespaces/pkg/types"
)

// These tests close the E2E wiring gaps identified after the worklog-0372
// remediation (PRs #265/#266/#267/#272). Each exercises a workflow through the
// LIVE request path — real auth.Service, real PlatformAdminHandler, real
// AuthMiddleware, real TCP server — not just the handler or service in
// isolation. Per README-LLM.md "E2E Wiring Verification": "It compiles" or
// "unit tests pass" is NOT sufficient; the actual wiring must be demonstrated.

// recordingOrgStore is a minimal platformAdminOrgStore that performs a REAL
// status update against an in-memory user map (so the suspend actually flips
// the user's status, which AuthMiddleware reads via GetUser). This is what
// *PgOrgStore.SuspendUserGuardedByLastAdmin does in production — the mock
// faithfully reproduces the atomic "check + update" contract.
type recordingOrgStore struct {
	mu                sync.Mutex
	suspendCalls      []struct{ UserID, Force string }
	lastAdminConflict *types.LastAdminOrg
	users             map[string]*recordingUser // shared with the DB mock
}

type recordingUser struct {
	ID     string
	Role   string
	Status types.UserStatus
	Active bool
}

func (s *recordingOrgStore) UpdateOrgStatus(context.Context, string, *types.OrgStatus, *types.OrgSubscriptionStatus, *types.OrgPlan) error {
	return nil
}
func (s *recordingOrgStore) LogAuditEvent(context.Context, string, string, string, string, *string, map[string]any) error {
	return nil
}
func (s *recordingOrgStore) ListAllOrgs(context.Context, int, int, *string) ([]types.OrgSummary, *types.PaginationMetadata, error) {
	return nil, nil, nil
}

// SuspendUserGuardedByLastAdmin mirrors the real PgOrgStore method: check (no
// conflict configured → proceed), then flip the user's status + active in the
// shared user map so AuthMiddleware's GetUser observes the suspension.
func (s *recordingOrgStore) SuspendUserGuardedByLastAdmin(_ context.Context, userID string, force bool) (*types.LastAdminOrg, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.suspendCalls = append(s.suspendCalls, struct{ UserID, Force string }{userID, fmt.Sprintf("%v", force)})
	if !force && s.lastAdminConflict != nil {
		return s.lastAdminConflict, nil
	}
	if u, ok := s.users[userID]; ok {
		u.Status = types.UserStatusSuspended
		u.Active = false
	}
	return nil, nil
}

// recordingUserStore is the platformAdminUserStore surface (SetUserStatus +
// ListAllUsers). SetUserStatus also flips the shared user map so unsuspend
// works end-to-end.
type recordingUserStore struct {
	mu    sync.Mutex
	users map[string]*recordingUser
}

func (s *recordingUserStore) SetUserStatus(_ context.Context, userID string, status types.UserStatus) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if u, ok := s.users[userID]; ok {
		u.Status = status
		u.Active = status == types.UserStatusActive
	}
	return nil
}
func (s *recordingUserStore) ListAllUsers(context.Context, int, int, *string) ([]types.UserListEntry, *types.PaginationMetadata, error) {
	return nil, nil, nil
}

// recordingCache is a minimal CacheService that actually stores the
// user_suspended:<userID> marker so the marker round-trip is observable.
// Only the methods AuthMiddleware + MarkUserSuspended touch are implemented
// meaningfully; the rest satisfy the interface with no-ops.
type recordingCache struct {
	mu        sync.Mutex
	markers   map[string]string
	markerSet []string
}

func newRecordingCache() *recordingCache { return &recordingCache{markers: map[string]string{}} }

func (c *recordingCache) Get(_ context.Context, key string) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if v, ok := c.markers[key]; ok {
		return v, nil
	}
	return "", nil
}
func (c *recordingCache) Set(_ context.Context, key, value string, _ time.Duration) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.markers[key] = value
	c.markerSet = append(c.markerSet, key)
	return nil
}
func (c *recordingCache) Delete(_ context.Context, key string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.markers, key)
	return nil
}

func (c *recordingCache) DeleteByPrefix(_ context.Context, _ string) error { return nil }

// The remaining CacheService methods are not exercised by the suspend path;
// no-op implementations satisfy the interface.
func (c *recordingCache) SetNX(context.Context, string, string, time.Duration) (bool, error) {
	return false, nil
}
func (c *recordingCache) Incr(context.Context, string, time.Duration) (int64, error) { return 1, nil }
func (c *recordingCache) GetObject(context.Context, string, interface{}) error       { return nil }
func (c *recordingCache) SetObject(context.Context, string, interface{}, time.Duration) error {
	return nil
}
func (c *recordingCache) GetSession(context.Context, string) (*types.CachedSession, error) {
	return nil, nil
}
func (c *recordingCache) SetSession(context.Context, string, types.CachedSession, time.Duration) error {
	return nil
}
func (c *recordingCache) DeleteSession(context.Context, string) error { return nil }
func (c *recordingCache) Ping(context.Context) error                  { return nil }
func (c *recordingCache) Start() error                                { return nil }
func (c *recordingCache) Stop() error                                 { return nil }

// recordingDB is the DatabaseService surface AuthMiddleware's GetUser needs.
// It reads from the same shared user map as the org/user stores so a suspend
// is immediately visible.
type recordingDB struct {
	mu    sync.Mutex
	users map[string]*recordingUser
}

func (d *recordingDB) GetUser(_ context.Context, userID string) (*types.User, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	u, ok := d.users[userID]
	if !ok {
		return nil, nil
	}
	return &types.User{ID: u.ID, Role: u.Role, Status: u.Status, Active: u.Active}, nil
}

// The remaining DatabaseService methods are not exercised by the suspend path.
func (d *recordingDB) GetUserByEmail(context.Context, string) (*types.User, error)   { return nil, nil }
func (d *recordingDB) CreateUser(context.Context, *types.User) error                 { return nil }
func (d *recordingDB) UpdateUser(context.Context, string, types.UserUpdates) error   { return nil }
func (d *recordingDB) DeleteUser(context.Context, string) error                      { return nil }
func (d *recordingDB) CountUsers(context.Context) (int, error)                       { return 0, nil }
func (d *recordingDB) SetUserStatus(context.Context, string, types.UserStatus) error { return nil }
func (d *recordingDB) GetUserByAPIKey(context.Context, string) (*types.User, error)  { return nil, nil }
func (d *recordingDB) CreateAPIKey(context.Context, *types.APIKey) error             { return nil }
func (d *recordingDB) ListAPIKeys(context.Context, string) ([]*types.APIKey, error)  { return nil, nil }
func (d *recordingDB) GetAPIKey(context.Context, string, string) (*types.APIKey, error) {
	return nil, nil
}
func (d *recordingDB) DeleteAPIKey(context.Context, string, string) error { return nil }
func (d *recordingDB) GetAPIKeyRecordByHash(context.Context, string) (*types.APIKey, error) {
	return nil, nil
}
func (d *recordingDB) UpdateAPIKeyDEK(context.Context, string, []byte, []byte, bool) error {
	return nil
}
func (d *recordingDB) ListAPIKeysWithDecrypt(context.Context, string) ([]*types.APIKey, error) {
	return nil, nil
}
func (d *recordingDB) GetWorkspace(context.Context, string) (*types.WorkspaceMetadata, error) {
	return nil, nil
}
func (d *recordingDB) CreateWorkspace(context.Context, *types.WorkspaceMetadata) error { return nil }
func (d *recordingDB) UpdateWorkspace(context.Context, string, types.WorkspaceUpdates) error {
	return nil
}
func (d *recordingDB) DeleteWorkspace(context.Context, string) error { return nil }
func (d *recordingDB) ListWorkspaces(context.Context, string, int, int) ([]*types.WorkspaceMetadata, *types.PaginationMetadata, error) {
	return nil, nil, nil
}
func (d *recordingDB) CountWorkspacesByUserAndOrg(context.Context, string, string) (int, error) {
	return 0, nil
}
func (d *recordingDB) CountActiveWorkspacesByUserAndOrg(context.Context, string, string) (int, error) {
	return 0, nil
}
func (d *recordingDB) SyncWorkspaceVersionInfo(context.Context, string, string, string) {}
func (d *recordingDB) MarkWorkspaceDeleted(context.Context, string)                     {}
func (d *recordingDB) CheckPermission(context.Context, string, string, string, string) (bool, error) {
	return false, nil
}
func (d *recordingDB) CheckResourceOwnership(context.Context, string, string, string) (bool, error) {
	return false, nil
}
func (d *recordingDB) ListSessionIndex(context.Context, string) ([]types.SessionListItem, error) {
	return nil, nil
}
func (d *recordingDB) DeleteSessionIndex(context.Context, string) error { return nil }
func (d *recordingDB) DeleteSessionTree(context.Context, string, string) error {
	return nil
}
func (d *recordingDB) UpsertSessionMessage(context.Context, string, string, time.Time) error {
	return nil
}
func (d *recordingDB) UpsertSessionTitle(context.Context, string, string, string) error { return nil }
func (d *recordingDB) UpsertSessionParent(context.Context, string, string, string) error {
	return nil
}
func (d *recordingDB) UpsertSessionContextUsed(_ context.Context, _, _ string, _ int64) error {
	return nil
}
func (d *recordingDB) UpdateSessionLastSeen(context.Context, string, string) error { return nil }
func (d *recordingDB) ListAllWorkspaceOwners(context.Context) (map[string]string, error) {
	return nil, nil
}
func (d *recordingDB) Ping(context.Context) error { return nil }
func (d *recordingDB) Start() error               { return nil }
func (d *recordingDB) Stop() error                { return nil }

// noopPolicyLogger satisfies handlers.policyLogger for the PlatformAdminHandler.
type noopPolicyLogger struct{}

func (noopPolicyLogger) Warn(string, ...any) {}

// e2eSuspendEnv wires the real auth.Service + real PlatformAdminHandler +
// real AuthMiddleware through a live gin router, sharing a single in-memory
// user map across the DB mock + org store + user store so a suspend is visible
// to every layer. This is the same shape as app.go's wiring (the handler is
// constructed with svc, svc as the revoker — the real constructor signature).
func e2eSuspendEnv(t *testing.T) (*gin.Engine, *auth.Service, *recordingOrgStore, *recordingCache, map[string]*recordingUser) {
	t.Helper()
	gin.SetMode(gin.TestMode)

	users := map[string]*recordingUser{
		"admin-1": {ID: "admin-1", Role: "admin", Status: types.UserStatusActive, Active: true},
		"victim":  {ID: "victim", Role: "user", Status: types.UserStatusActive, Active: true},
	}

	cfg := &config.Config{}
	cfg.Auth.JWTSecret = "e2e-suspend-secret"
	cfg.Auth.TokenDuration = time.Hour
	cfg.Auth.APIKeyPrefix = "lsp_"
	log, _ := logger.New(false, "error", "json")

	cache := newRecordingCache()
	db := &recordingDB{users: users}
	authSvc, err := auth.New(cfg, log, db, cache)
	if err != nil {
		t.Fatalf("auth.New: %v", err)
	}

	orgStore := &recordingOrgStore{users: users}
	userStore := &recordingUserStore{users: users}
	// Construct with the SAME signature app.go uses: revoker = authSvc (so the
	// F4 marker round-trip is real, not stubbed).
	platformH := handlers.NewPlatformAdminHandler(orgStore, userStore, authSvc, authSvc, noopPolicyLogger{})

	router := gin.New()
	// Admin routes: behind AuthMiddleware (a real admin token is needed; we do
	// not gate on role here — the role check is AdminGuard's job, exercised
	// elsewhere; this test proves the suspend→deny wiring, not RBAC).
	adminGrp := router.Group("/api/v1/admin", authSvc.AuthMiddleware())
	{
		adminGrp.POST("/users/:id/suspend", platformH.SuspendUser)
		adminGrp.POST("/users/:id/unsuspend", platformH.UnsuspendUser)
	}
	// A protected route the victim tries to reach.
	router.GET("/api/v1/secure", authSvc.AuthMiddleware(), func(c *gin.Context) {
		c.JSON(200, gin.H{"ok": true})
	})
	return router, authSvc, orgStore, cache, users
}

// TestE2E_AdminSuspendUser_VictimTokenRejected is the canonical F4 wiring
// proof: an admin POSTs /admin/users/:id/suspend through the live router, the
// handler writes the revocation marker + flips the DB status, and the victim's
// PREVIOUSLY-ISSUED token (still cryptographically valid) is rejected on the
// next request with 401 "account suspended". This is the full path:
// HTTP POST → router → AuthMiddleware → PlatformAdminHandler.SuspendUser →
// SuspendUserGuardedByLastAdmin (status flip) → MarkUserSuspended (Redis marker)
// → victim's next HTTP GET → AuthMiddleware → marker hit → 401.
func TestE2E_AdminSuspendUser_VictimTokenRejected(t *testing.T) {
	router, authSvc, orgStore, cache, _ := e2eSuspendEnv(t)

	// Issue the victim's token BEFORE the suspend (it stays cryptographically
	// valid; the marker is what must deny it).
	victimToken, err := authSvc.GenerateToken("victim")
	if err != nil {
		t.Fatalf("GenerateToken(victim): %v", err)
	}
	adminToken, err := authSvc.GenerateToken("admin-1")
	if err != nil {
		t.Fatalf("GenerateToken(admin): %v", err)
	}

	// Start a real TCP server so this is a true network round-trip.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	server := &http.Server{Handler: router}
	go server.Serve(ln)
	defer server.Close()
	base := fmt.Sprintf("http://%s", ln.Addr().String())
	c := &http.Client{Timeout: 5 * time.Second}

	// Phase 1: victim can reach the protected route before suspend.
	resp := getE2E(t, c, base+"/api/v1/secure", victimToken)
	assertStatusE2E(t, resp, 200, "victim before suspend")

	// Phase 2: admin suspends the victim via the live router.
	resp = postE2E(t, c, base+"/api/v1/admin/users/victim/suspend", "", adminToken)
	assertStatusE2E(t, resp, 200, "admin suspend")

	// Phase 3: the org store's atomic suspend ran (status flipped in shared map).
	if len(orgStore.suspendCalls) != 1 || orgStore.suspendCalls[0].UserID != "victim" {
		t.Fatalf("expected 1 atomic suspend call for victim, got %+v", orgStore.suspendCalls)
	}

	// Phase 4: the F4 revocation marker was written (wiring: handler → authSvc).
	// The cache also holds token-validation entries (ValidateToken caches the
	// userID under token:<hash>), so filter for the suspend marker specifically.
	var sawSuspendMarker bool
	for _, k := range cache.markerSet {
		if k == "user_suspended:victim" {
			sawSuspendMarker = true
		}
	}
	if !sawSuspendMarker {
		t.Fatalf("expected the revocation marker 'user_suspended:victim' via MarkUserSuspended, got %v", cache.markerSet)
	}

	// Phase 5: victim's SAME token is now rejected with 401 (the marker + DB
	// status agree). This is the load-bearing assertion — pre-fix the token
	// stayed valid until natural expiry.
	resp = getE2E(t, c, base+"/api/v1/secure", victimToken)
	assertStatusE2E(t, resp, 401, "victim after suspend")
	body, _ := io.ReadAll(resp.Body)
	if !bytes.Contains(body, []byte("suspended")) {
		t.Errorf("expected 'suspended' in 401 body, got %s", string(body))
	}
}

// TestE2E_AdminUnsuspendUser_VictimTokenAcceptedAgain proves the unsuspend path
// clears the marker so the user's existing tokens work again — the full
// round-trip, not just the clear in isolation.
func TestE2E_AdminUnsuspendUser_VictimTokenAcceptedAgain(t *testing.T) {
	router, authSvc, _, _, _ := e2eSuspendEnv(t)

	victimToken, err := authSvc.GenerateToken("victim")
	if err != nil {
		t.Fatalf("GenerateToken(victim): %v", err)
	}
	adminToken, err := authSvc.GenerateToken("admin-1")
	if err != nil {
		t.Fatalf("GenerateToken(admin): %v", err)
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	server := &http.Server{Handler: router}
	go server.Serve(ln)
	defer server.Close()
	base := fmt.Sprintf("http://%s", ln.Addr().String())
	c := &http.Client{Timeout: 5 * time.Second}

	// Suspend then unsuspend through the live router.
	resp := postE2E(t, c, base+"/api/v1/admin/users/victim/suspend", "", adminToken)
	assertStatusE2E(t, resp, 200, "suspend")
	resp = postE2E(t, c, base+"/api/v1/admin/users/victim/unsuspend", "", adminToken)
	assertStatusE2E(t, resp, 200, "unsuspend")

	// Victim's token works again — the marker was cleared AND the DB status
	// flipped back to active by the atomic path.
	resp = getE2E(t, c, base+"/api/v1/secure", victimToken)
	assertStatusE2E(t, resp, 200, "victim after unsuspend")
}

// --- HTTP helpers (local to this file to avoid import cycles) ---

func getE2E(t *testing.T, c *http.Client, url, token string) *http.Response {
	t.Helper()
	req, _ := http.NewRequest("GET", url, nil)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := c.Do(req)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	return resp
}

func postE2E(t *testing.T, c *http.Client, url, body, token string) *http.Response {
	t.Helper()
	req, _ := http.NewRequest("POST", url, bytes.NewBufferString(body))
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := c.Do(req)
	if err != nil {
		t.Fatalf("POST %s: %v", url, err)
	}
	return resp
}

func assertStatusE2E(t *testing.T, resp *http.Response, want int, label string) {
	t.Helper()
	if resp.StatusCode != want {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("%s: expected %d, got %d: %s", label, want, resp.StatusCode, string(b))
	}
}
