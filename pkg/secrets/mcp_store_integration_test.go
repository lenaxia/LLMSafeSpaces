// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build integration

package secrets

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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

	store := &PgSecretStore{pool: pool}
	suite.Run(t, &MCPStoreIntegrationSuite{pool: pool})
	_ = store
}

func (s *MCPStoreIntegrationSuite) SetupTest() {
	// Clean MCP tables before each test.
	_, err := s.pool.Exec(context.Background(),
		"DELETE FROM mcp_server_bindings; DELETE FROM mcp_server_auto_apply; DELETE FROM mcp_servers")
	s.Require().NoError(err)
}

// TestCreateGetMCPServer_AdminScope round-trips an admin-scope MCP server.
func (s *MCPStoreIntegrationSuite) TestCreateGetMCPServer_AdminScope() {
	t := s.T()
	store := &PgSecretStore{pool: s.pool}
	ctx := context.Background()

	row := &MCPServerRow{
		ID: "test-admin-wiki", OwnerType: "admin", OwnerID: "_platform",
		Name: "wiki-server", Transport: "http", URL: "https://wiki.example.com/mcp",
		Ciphertext: []byte("encrypted-blob"), KeyVersion: 1, Enabled: true,
		CreatedAt: time.Now(), UpdatedAt: time.Now(),
	}
	require.NoError(t, store.CreateMCPServer(ctx, row))

	got, err := store.GetMCPServer(ctx, "admin", "_platform", "test-admin-wiki")
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, "wiki-server", got.Name)
	assert.Equal(t, "http", got.Transport)
	assert.Equal(t, "https://wiki.example.com/mcp", got.URL)
	assert.True(t, got.Enabled)
}

// TestListMCPServers_ByOwnerScope returns only servers owned by the given (type, id).
func (s *MCPStoreIntegrationSuite) TestListMCPServers_CrossScopeIsolation() {
	t := s.T()
	store := &PgSecretStore{pool: s.pool}
	ctx := context.Background()

	// Create one admin and one org server.
	for _, r := range []*MCPServerRow{
		{ID: "iso-admin", OwnerType: "admin", OwnerID: "_platform", Name: "admin-srv", Transport: "http", URL: "https://a.com", Ciphertext: []byte("x"), Enabled: true, CreatedAt: time.Now(), UpdatedAt: time.Now()},
		{ID: "iso-org", OwnerType: "org", OwnerID: "org-123", Name: "org-srv", Transport: "sse", URL: "https://b.com", Ciphertext: []byte("x"), Enabled: true, CreatedAt: time.Now(), UpdatedAt: time.Now()},
	} {
		require.NoError(t, store.CreateMCPServer(ctx, r))
	}

	adminServers, err := store.ListMCPServers(ctx, "admin", "_platform")
	require.NoError(t, err)
	require.Len(t, adminServers, 1)
	assert.Equal(t, "admin-srv", adminServers[0].Name)

	orgServers, err := store.ListMCPServers(ctx, "org", "org-123")
	require.NoError(t, err)
	require.Len(t, orgServers, 1)
	assert.Equal(t, "org-srv", orgServers[0].Name)
}

// TestUpdateMCPServer_PartialUpdate preserves ciphertext when not provided.
func (s *MCPStoreIntegrationSuite) TestUpdateMCPServer_PreservesCiphertext() {
	t := s.T()
	store := &PgSecretStore{pool: s.pool}
	ctx := context.Background()

	original := &MCPServerRow{
		ID: "upd-test", OwnerType: "admin", OwnerID: "_platform",
		Name: "orig", Transport: "http", URL: "https://orig.com",
		Ciphertext: []byte("original-secret"), KeyVersion: 1, Enabled: true,
		CreatedAt: time.Now(), UpdatedAt: time.Now(),
	}
	require.NoError(t, store.CreateMCPServer(ctx, original))

	// Update name + enabled, but do NOT touch ciphertext.
	update := &MCPServerRow{
		Name:       "renamed",
		Enabled:    false,
		Ciphertext: nil, // nil = preserve existing
	}
	require.NoError(t, store.UpdateMCPServer(ctx, "admin", "_platform", "upd-test", update))

	got, err := store.GetMCPServer(ctx, "admin", "_platform", "upd-test")
	require.NoError(t, err)
	assert.Equal(t, "renamed", got.Name)
	assert.False(t, got.Enabled)
	assert.Equal(t, []byte("original-secret"), got.Ciphertext, "ciphertext must be preserved")
}

// TestDeleteMCPServer_CascadesBindings deletes a server and verifies bindings are removed.
func (s *MCPStoreIntegrationSuite) TestDeleteMCPServer_CascadesBindings() {
	t := s.T()
	store := &PgSecretStore{pool: s.pool}
	ctx := context.Background()

	// This test requires a workspace row to satisfy the FK. Insert a minimal one.
	wsID := "ws-cascade-test"
	_, _ = s.pool.Exec(ctx, fmt.Sprintf(
		`INSERT INTO workspaces (id, user_id, name, phase, created_at, updated_at)
		 VALUES ('%s', 'user-test', 'test-ws', 'Active', now(), now())
		 ON CONFLICT DO NOTHING`, wsID))
	defer func() { _, _ = s.pool.Exec(ctx, "DELETE FROM workspaces WHERE id = $1", wsID) }()

	row := &MCPServerRow{
		ID: "del-cascade", OwnerType: "admin", OwnerID: "_platform",
		Name: "to-delete", Transport: "http", URL: "https://x.com",
		Ciphertext: []byte("x"), Enabled: true, CreatedAt: time.Now(), UpdatedAt: time.Now(),
	}
	require.NoError(t, store.CreateMCPServer(ctx, row))
	require.NoError(t, store.BindMCPServerToWorkspace(ctx, row.ID, wsID))

	// Verify binding exists.
	servers, err := store.GetWorkspaceMCPServers(ctx, wsID)
	require.NoError(t, err)
	require.Len(t, servers, 1)

	// Delete the server — cascade should remove the binding.
	require.NoError(t, store.DeleteMCPServer(ctx, "admin", "_platform", row.ID))

	servers, err = store.GetWorkspaceMCPServers(ctx, wsID)
	require.NoError(t, err)
	assert.Empty(t, servers, "bindings must cascade-delete with the server")
}

// TestSeedWorkspaceMCPServers applies auto-apply rules at workspace create time.
func (s *MCPStoreIntegrationSuite) TestSeedWorkspaceMCPServers_AutoApplyAll() {
	t := s.T()
	store := &PgSecretStore{pool: s.pool}
	ctx := context.Background()

	// Create a platform MCP server with auto-apply target_type='all'.
	srv := &MCPServerRow{
		ID: "seed-all", OwnerType: "admin", OwnerID: "_platform",
		Name: "platform-wiki", Transport: "http", URL: "https://wiki.com",
		Ciphertext: []byte("x"), Enabled: true, CreatedAt: time.Now(), UpdatedAt: time.Now(),
	}
	require.NoError(t, store.CreateMCPServer(ctx, srv))
	require.NoError(t, store.CreateMCPServerAutoApply(ctx, srv.ID, "all", nil))

	// Create a workspace.
	wsID := "ws-seed-test"
	_, _ = s.pool.Exec(ctx, fmt.Sprintf(
		`INSERT INTO workspaces (id, user_id, name, phase, created_at, updated_at)
		 VALUES ('%s', 'user-seed', 'seed-ws', 'Active', now(), now())
		 ON CONFLICT DO NOTHING`, wsID))
	defer func() { _, _ = s.pool.Exec(ctx, "DELETE FROM workspaces WHERE id = $1", wsID) }()

	// Seed.
	require.NoError(t, store.SeedWorkspaceMCPServers(ctx, wsID, "user-seed", nil))

	// The platform server should be bound.
	servers, err := store.GetWorkspaceMCPServers(ctx, wsID)
	require.NoError(t, err)
	require.Len(t, servers, 1)
	assert.Equal(t, "platform-wiki", servers[0].Name)
}

// TestCountMCPServersByOwner returns the server count for quota enforcement.
func (s *MCPStoreIntegrationSuite) TestCountMCPServersByOwner() {
	t := s.T()
	store := &PgSecretStore{pool: s.pool}
	ctx := context.Background()

	for i := 0; i < 3; i++ {
		require.NoError(t, store.CreateMCPServer(ctx, &MCPServerRow{
			ID:         fmt.Sprintf("cnt-%d", i),
			OwnerType:  "user",
			OwnerID:    "quota-user",
			Name:       fmt.Sprintf("srv-%d", i),
			Transport:  "http",
			URL:        fmt.Sprintf("https://%d.com", i),
			Ciphertext: []byte("x"),
			Enabled:    true,
			CreatedAt:  time.Now(),
			UpdatedAt:  time.Now(),
		}))
	}

	count, err := store.CountMCPServersByOwner(ctx, "user", "quota-user")
	require.NoError(t, err)
	assert.Equal(t, 3, count)
}

// TestGetWorkspaceMCPServers_SkipsDisabled verifies disabled servers are omitted.
func (s *MCPStoreIntegrationSuite) TestGetWorkspaceMCPServers_SkipsDisabled() {
	t := s.T()
	store := &PgSecretStore{pool: s.pool}
	ctx := context.Background()

	wsID := "ws-disabled-test"
	_, _ = s.pool.Exec(ctx, fmt.Sprintf(
		`INSERT INTO workspaces (id, user_id, name, phase, created_at, updated_at)
		 VALUES ('%s', 'user-dis', 'dis-ws', 'Active', now(), now())
		 ON CONFLICT DO NOTHING`, wsID))
	defer func() { _, _ = s.pool.Exec(ctx, "DELETE FROM workspaces WHERE id = $1", wsID) }()

	for _, s := range []*MCPServerRow{
		{ID: "dis-on", OwnerType: "admin", OwnerID: "_platform", Name: "enabled-srv", Transport: "http", URL: "https://on.com", Ciphertext: []byte("x"), Enabled: true, CreatedAt: time.Now(), UpdatedAt: time.Now()},
		{ID: "dis-off", OwnerType: "admin", OwnerID: "_platform", Name: "disabled-srv", Transport: "http", URL: "https://off.com", Ciphertext: []byte("x"), Enabled: false, CreatedAt: time.Now(), UpdatedAt: time.Now()},
	} {
		require.NoError(t, store.CreateMCPServer(ctx, s))
		require.NoError(t, store.BindMCPServerToWorkspace(ctx, s.ID, wsID))
	}

	servers, err := store.GetWorkspaceMCPServers(ctx, wsID)
	require.NoError(t, err)
	require.Len(t, servers, 1, "only the enabled server should be returned")
	assert.Equal(t, "enabled-srv", servers[0].Name)
}

// Suppress unused import warning for json (used in potential future assertions).
var _ = json.Marshal
