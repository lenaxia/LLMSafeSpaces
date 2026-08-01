// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build integration

package secrets

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/suite"
)

// MCPStoreIntegrationSuite exercises the MCP server store against a real PostgreSQL.
// Gated by the "integration" build tag; skipped when TEST_DATABASE_URL is unreachable.
type MCPStoreIntegrationSuite struct {
	suite.Suite
	pool *pgxpool.Pool
}

func TestMCPStoreIntegrationSuite(t *testing.T) {
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		dsn = "postgres://postgres:testpass@localhost:5433/llmsafespaces_test?sslmode=disable"
	}
	pool, err := pgxpool.New(context.Background(), dsn)
	if err != nil {
		t.Skipf("Skipping PG integration test: %v", err)
	}
	if err := pool.Ping(context.Background()); err != nil {
		t.Skipf("Skipping PG integration test: %v", err)
	}
	suite.Run(t, &MCPStoreIntegrationSuite{pool: pool})
}

func (s *MCPStoreIntegrationSuite) SetupTest() {
	_, err := s.pool.Exec(context.Background(),
		"DELETE FROM mcp_server_bindings; DELETE FROM mcp_server_auto_apply; DELETE FROM mcp_servers")
	s.Require().NoError(err)
}

// newUUID returns a fresh UUID string for test row IDs. The mcp_servers.id column
// is type uuid, so string IDs must be valid UUIDs (Postgres rejects "test-foo").
func newUUID() string { return uuid.New().String() }

// TestCreateGetMCPServer_AdminScope round-trips an admin-scope MCP server.
func (s *MCPStoreIntegrationSuite) TestCreateGetMCPServer_AdminScope() {
	store := &PgSecretStore{pool: s.pool}
	ctx := context.Background()

	id := newUUID()
	row := &MCPServerRow{
		ID: id, OwnerType: "admin", OwnerID: "_platform",
		Name: "wiki-server", Transport: "http", URL: "https://wiki.example.com/mcp",
		Ciphertext: []byte("encrypted-blob"), KeyVersion: 1, Enabled: true,
		CreatedAt: time.Now(), UpdatedAt: time.Now(),
	}
	s.Require().NoError(store.CreateMCPServer(ctx, row))

	got, err := store.GetMCPServer(ctx, "admin", "_platform", id)
	s.Require().NoError(err)
	s.Require().NotNil(got)
	s.Equal("wiki-server", got.Name)
	s.Equal("http", got.Transport)
	s.Equal("https://wiki.example.com/mcp", got.URL)
	s.True(got.Enabled)
}

// TestListMCPServers_ByOwnerScope returns only servers owned by the given (type, id).
func (s *MCPStoreIntegrationSuite) TestListMCPServers_CrossScopeIsolation() {
	store := &PgSecretStore{pool: s.pool}
	ctx := context.Background()

	for _, r := range []*MCPServerRow{
		{ID: newUUID(), OwnerType: "admin", OwnerID: "_platform", Name: "admin-srv", Transport: "http", URL: "https://a.com", Ciphertext: []byte("x"), Enabled: true, CreatedAt: time.Now(), UpdatedAt: time.Now()},
		{ID: newUUID(), OwnerType: "org", OwnerID: "11111111-1111-1111-1111-111111111111", Name: "org-srv", Transport: "sse", URL: "https://b.com", Ciphertext: []byte("x"), Enabled: true, CreatedAt: time.Now(), UpdatedAt: time.Now()},
	} {
		s.Require().NoError(store.CreateMCPServer(ctx, r))
	}

	adminServers, err := store.ListMCPServers(ctx, "admin", "_platform")
	s.Require().NoError(err)
	s.Len(adminServers, 1)

	orgServers, err := store.ListMCPServers(ctx, "org", "11111111-1111-1111-1111-111111111111")
	s.Require().NoError(err)
	s.Len(orgServers, 1)
}

// TestUpdateMCPServer_PartialUpdate preserves ciphertext when not provided.
func (s *MCPStoreIntegrationSuite) TestUpdateMCPServer_PreservesCiphertext() {
	store := &PgSecretStore{pool: s.pool}
	ctx := context.Background()

	id := newUUID()
	original := &MCPServerRow{
		ID: id, OwnerType: "admin", OwnerID: "_platform",
		Name: "orig", Transport: "http", URL: "https://orig.com",
		Ciphertext: []byte("original-secret"), KeyVersion: 1, Enabled: true,
		CreatedAt: time.Now(), UpdatedAt: time.Now(),
	}
	s.Require().NoError(store.CreateMCPServer(ctx, original))

	update := &MCPServerRow{
		Name:       "renamed",
		Enabled:    false,
		Ciphertext: nil,
	}
	s.Require().NoError(store.UpdateMCPServer(ctx, "admin", "_platform", id, update))

	got, err := store.GetMCPServer(ctx, "admin", "_platform", id)
	s.Require().NoError(err)
	s.Equal("renamed", got.Name)
	s.False(got.Enabled)
	s.Equal([]byte("original-secret"), got.Ciphertext)
}

// helper to create a workspace row for FK satisfaction. Also creates the
// referenced user row if it doesn't exist (workspaces.user_id FKs to users.id).
func (s *MCPStoreIntegrationSuite) createTestWorkspace(ctx context.Context, wsID, userID string) {
	// Use a unique email per user to avoid users_email_key collisions
	// when multiple tests create users in the same DB session.
	_, err := s.pool.Exec(ctx, `
		INSERT INTO users (id, username, email, password_hash, role, status, active, created_at, updated_at)
		VALUES ($1, $2, $3, 'x', 'user', 'active', true, now(), now())
		ON CONFLICT (id) DO NOTHING
	`, userID, "test-user-"+userID[:8], "test-"+userID[:8]+"@test.com")
	s.Require().NoError(err)
	_, err = s.pool.Exec(ctx, `
		INSERT INTO workspaces (id, user_id, name, created_at, updated_at)
		VALUES ($1, $2, 'test-ws', now(), now())
		ON CONFLICT (id) DO NOTHING
	`, wsID, userID)
	s.Require().NoError(err)
}

// TestDeleteMCPServer_CascadesBindings deletes a server and verifies bindings are removed.
func (s *MCPStoreIntegrationSuite) TestDeleteMCPServer_CascadesBindings() {
	store := &PgSecretStore{pool: s.pool}
	ctx := context.Background()

	wsID := newUUID()
	userID := "22222222-2222-2222-2222-222222222222"
	s.createTestWorkspace(ctx, wsID, userID)
	defer func() {
		_, _ = s.pool.Exec(ctx, "DELETE FROM workspaces WHERE id = $1; DELETE FROM users WHERE id = $2", wsID, userID)
	}()

	id := newUUID()
	row := &MCPServerRow{
		ID: id, OwnerType: "admin", OwnerID: "_platform",
		Name: "to-delete", Transport: "http", URL: "https://x.com",
		Ciphertext: []byte("x"), Enabled: true, CreatedAt: time.Now(), UpdatedAt: time.Now(),
	}
	s.Require().NoError(store.CreateMCPServer(ctx, row))
	s.Require().NoError(store.BindMCPServerToWorkspace(ctx, id, wsID))

	servers, err := store.GetWorkspaceMCPServers(ctx, wsID)
	s.Require().NoError(err)
	s.Len(servers, 1)

	s.Require().NoError(store.DeleteMCPServer(ctx, "admin", "_platform", id))

	servers, err = store.GetWorkspaceMCPServers(ctx, wsID)
	s.Require().NoError(err)
	s.Empty(servers)
}

// TestSeedWorkspaceMCPServers applies auto-apply rules at workspace create time.
func (s *MCPStoreIntegrationSuite) TestSeedWorkspaceMCPServers_AutoApplyAll() {
	store := &PgSecretStore{pool: s.pool}
	ctx := context.Background()

	id := newUUID()
	srv := &MCPServerRow{
		ID: id, OwnerType: "admin", OwnerID: "_platform",
		Name: "platform-wiki", Transport: "http", URL: "https://wiki.com",
		Ciphertext: []byte("x"), Enabled: true, CreatedAt: time.Now(), UpdatedAt: time.Now(),
	}
	s.Require().NoError(store.CreateMCPServer(ctx, srv))
	s.Require().NoError(store.CreateMCPServerAutoApply(ctx, id, "all", nil))

	wsID := newUUID()
	userID := "33333333-3333-3333-3333-333333333333"
	s.createTestWorkspace(ctx, wsID, userID)
	defer func() {
		_, _ = s.pool.Exec(ctx, "DELETE FROM workspaces WHERE id = $1; DELETE FROM users WHERE id = $2", wsID, userID)
	}()

	s.Require().NoError(store.SeedWorkspaceMCPServers(ctx, wsID, userID, nil))

	servers, err := store.GetWorkspaceMCPServers(ctx, wsID)
	s.Require().NoError(err)
	s.Len(servers, 1)
	s.Equal("platform-wiki", servers[0].Name)
}

// TestCountMCPServersByOwner returns the server count for quota enforcement.
func (s *MCPStoreIntegrationSuite) TestCountMCPServersByOwner() {
	store := &PgSecretStore{pool: s.pool}
	ctx := context.Background()
	userID := "44444444-4444-4444-4444-444444444444"

	for i := 0; i < 3; i++ {
		s.Require().NoError(store.CreateMCPServer(ctx, &MCPServerRow{
			ID:         newUUID(),
			OwnerType:  "user",
			OwnerID:    userID,
			Name:       fmt.Sprintf("srv-%d", i),
			Transport:  "http",
			URL:        fmt.Sprintf("https://%d.com", i),
			Ciphertext: []byte("x"),
			Enabled:    true,
			CreatedAt:  time.Now(),
			UpdatedAt:  time.Now(),
		}))
	}

	count, err := store.CountMCPServersByOwner(ctx, "user", userID)
	s.Require().NoError(err)
	s.Equal(3, count)
}

// TestGetWorkspaceMCPServers_SkipsDisabled verifies disabled servers are omitted.
func (s *MCPStoreIntegrationSuite) TestGetWorkspaceMCPServers_SkipsDisabled() {
	store := &PgSecretStore{pool: s.pool}
	ctx := context.Background()

	wsID := newUUID()
	userID := "55555555-5555-5555-5555-555555555555"
	s.createTestWorkspace(ctx, wsID, userID)
	defer func() {
		_, _ = s.pool.Exec(ctx, "DELETE FROM workspaces WHERE id = $1; DELETE FROM users WHERE id = $2", wsID, userID)
	}()

	for _, srv := range []*MCPServerRow{
		{ID: newUUID(), OwnerType: "admin", OwnerID: "_platform", Name: "enabled-srv", Transport: "http", URL: "https://on.com", Ciphertext: []byte("x"), Enabled: true, CreatedAt: time.Now(), UpdatedAt: time.Now()},
		{ID: newUUID(), OwnerType: "admin", OwnerID: "_platform", Name: "disabled-srv", Transport: "http", URL: "https://off.com", Ciphertext: []byte("x"), Enabled: false, CreatedAt: time.Now(), UpdatedAt: time.Now()},
	} {
		s.Require().NoError(store.CreateMCPServer(ctx, srv))
		s.Require().NoError(store.BindMCPServerToWorkspace(ctx, srv.ID, wsID))
	}

	servers, err := store.GetWorkspaceMCPServers(ctx, wsID)
	s.Require().NoError(err)
	s.Len(servers, 1)
	s.Equal("enabled-srv", servers[0].Name)
}
