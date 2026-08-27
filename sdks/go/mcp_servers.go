// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package llmsafespaces

import (
	"context"
)

// MCP server management (Epic 53). Three scopes over the same CRUD +
// bindings + auto-apply surface:
//
//	McpServers       → /me/mcp-servers       (caller's own servers)
//	OrgMcpServers    → /orgs/{orgId}/mcp-servers  (org admin)
//	AdminMcpServers  → /admin/mcp-servers     (platform admin; also
//	                   supports deleting auto-apply rules)
//
// Secrets (env/headers) are write-only: responses carry HasSecret only.

// McpServersService manages the caller's own MCP servers (/me scope).
type McpServersService struct{ c *Client }

// OrgMcpServersService manages an organization's MCP servers (org-admin scope).
type OrgMcpServersService struct{ c *Client }

// AdminMcpServersService manages platform MCP servers (admin scope).
type AdminMcpServersService struct{ c *Client }

func (s *McpServersService) List(ctx context.Context) ([]McpServer, error) {
	return listMcpServers(ctx, s.c, "/me/mcp-servers")
}
func (s *McpServersService) Get(ctx context.Context, id string) (*McpServer, error) {
	return getMcpServer(ctx, s.c, "/me/mcp-servers/"+id)
}
func (s *McpServersService) Create(ctx context.Context, req CreateMcpServerRequest) (*McpServer, error) {
	return createMcpServer(ctx, s.c, "/me/mcp-servers", req)
}
func (s *McpServersService) Update(ctx context.Context, id string, req UpdateMcpServerRequest) (*McpServer, error) {
	return updateMcpServer(ctx, s.c, "/me/mcp-servers/"+id, req)
}
func (s *McpServersService) Delete(ctx context.Context, id string) error {
	return s.c.do(ctx, "DELETE", "/me/mcp-servers/"+id, nil, nil)
}
func (s *McpServersService) Bind(ctx context.Context, id, workspaceID string) error {
	return bindMcpServer(ctx, s.c, "/me/mcp-servers/"+id+"/bindings", workspaceID)
}
func (s *McpServersService) Unbind(ctx context.Context, id, workspaceID string) error {
	return s.c.do(ctx, "DELETE", "/me/mcp-servers/"+id+"/bindings/"+workspaceID, nil, nil)
}
func (s *McpServersService) CreateAutoApply(ctx context.Context, id, targetType string, targetID *string) error {
	return createMcpAutoApply(ctx, s.c, "/me/mcp-servers/"+id+"/auto-apply", targetType, targetID)
}
func (s *McpServersService) ListAutoApply(ctx context.Context, id string) ([]McpAutoApplyRule, error) {
	return listMcpAutoApply(ctx, s.c, "/me/mcp-servers/"+id+"/auto-apply")
}

func (s *OrgMcpServersService) List(ctx context.Context, orgID string) ([]McpServer, error) {
	return listMcpServers(ctx, s.c, "/orgs/"+orgID+"/mcp-servers")
}
func (s *OrgMcpServersService) Get(ctx context.Context, orgID, id string) (*McpServer, error) {
	return getMcpServer(ctx, s.c, "/orgs/"+orgID+"/mcp-servers/"+id)
}
func (s *OrgMcpServersService) Create(ctx context.Context, orgID string, req CreateMcpServerRequest) (*McpServer, error) {
	return createMcpServer(ctx, s.c, "/orgs/"+orgID+"/mcp-servers", req)
}
func (s *OrgMcpServersService) Update(ctx context.Context, orgID, id string, req UpdateMcpServerRequest) (*McpServer, error) {
	return updateMcpServer(ctx, s.c, "/orgs/"+orgID+"/mcp-servers/"+id, req)
}
func (s *OrgMcpServersService) Delete(ctx context.Context, orgID, id string) error {
	return s.c.do(ctx, "DELETE", "/orgs/"+orgID+"/mcp-servers/"+id, nil, nil)
}
func (s *OrgMcpServersService) Bind(ctx context.Context, orgID, id, workspaceID string) error {
	return bindMcpServer(ctx, s.c, "/orgs/"+orgID+"/mcp-servers/"+id+"/bindings", workspaceID)
}
func (s *OrgMcpServersService) Unbind(ctx context.Context, orgID, id, workspaceID string) error {
	return s.c.do(ctx, "DELETE", "/orgs/"+orgID+"/mcp-servers/"+id+"/bindings/"+workspaceID, nil, nil)
}
func (s *OrgMcpServersService) CreateAutoApply(ctx context.Context, orgID, id, targetType string, targetID *string) error {
	return createMcpAutoApply(ctx, s.c, "/orgs/"+orgID+"/mcp-servers/"+id+"/auto-apply", targetType, targetID)
}
func (s *OrgMcpServersService) ListAutoApply(ctx context.Context, orgID, id string) ([]McpAutoApplyRule, error) {
	return listMcpAutoApply(ctx, s.c, "/orgs/"+orgID+"/mcp-servers/"+id+"/auto-apply")
}

func (s *AdminMcpServersService) List(ctx context.Context) ([]McpServer, error) {
	return listMcpServers(ctx, s.c, "/admin/mcp-servers")
}
func (s *AdminMcpServersService) Get(ctx context.Context, id string) (*McpServer, error) {
	return getMcpServer(ctx, s.c, "/admin/mcp-servers/"+id)
}
func (s *AdminMcpServersService) Create(ctx context.Context, req CreateMcpServerRequest) (*McpServer, error) {
	return createMcpServer(ctx, s.c, "/admin/mcp-servers", req)
}
func (s *AdminMcpServersService) Update(ctx context.Context, id string, req UpdateMcpServerRequest) (*McpServer, error) {
	return updateMcpServer(ctx, s.c, "/admin/mcp-servers/"+id, req)
}
func (s *AdminMcpServersService) Delete(ctx context.Context, id string) error {
	return s.c.do(ctx, "DELETE", "/admin/mcp-servers/"+id, nil, nil)
}
func (s *AdminMcpServersService) Bind(ctx context.Context, id, workspaceID string) error {
	return bindMcpServer(ctx, s.c, "/admin/mcp-servers/"+id+"/bindings", workspaceID)
}
func (s *AdminMcpServersService) Unbind(ctx context.Context, id, workspaceID string) error {
	return s.c.do(ctx, "DELETE", "/admin/mcp-servers/"+id+"/bindings/"+workspaceID, nil, nil)
}
func (s *AdminMcpServersService) CreateAutoApply(ctx context.Context, id, targetType string, targetID *string) error {
	return createMcpAutoApply(ctx, s.c, "/admin/mcp-servers/"+id+"/auto-apply", targetType, targetID)
}
func (s *AdminMcpServersService) ListAutoApply(ctx context.Context, id string) ([]McpAutoApplyRule, error) {
	return listMcpAutoApply(ctx, s.c, "/admin/mcp-servers/"+id+"/auto-apply")
}

// DeleteAutoApply removes an auto-apply rule. With a non-nil targetID the
// rule is scoped to (targetType, targetID); a nil targetID removes every
// rule of the given targetType (admin-only route variant).
func (s *AdminMcpServersService) DeleteAutoApply(ctx context.Context, id, targetType string, targetID *string) error {
	path := "/admin/mcp-servers/" + id + "/auto-apply/" + targetType
	if targetID != nil {
		path += "/" + *targetID
	}
	return s.c.do(ctx, "DELETE", path, nil, nil)
}

// --- shared scope helpers ---

func listMcpServers(ctx context.Context, c *Client, path string) ([]McpServer, error) {
	var resp struct {
		Servers []McpServer `json:"servers"`
	}
	if err := c.do(ctx, "GET", path, nil, &resp); err != nil {
		return nil, err
	}
	return resp.Servers, nil
}

func getMcpServer(ctx context.Context, c *Client, path string) (*McpServer, error) {
	var srv McpServer
	if err := c.do(ctx, "GET", path, nil, &srv); err != nil {
		return nil, err
	}
	return &srv, nil
}

func createMcpServer(ctx context.Context, c *Client, path string, req CreateMcpServerRequest) (*McpServer, error) {
	var srv McpServer
	if err := c.do(ctx, "POST", path, req, &srv); err != nil {
		return nil, err
	}
	return &srv, nil
}

func updateMcpServer(ctx context.Context, c *Client, path string, req UpdateMcpServerRequest) (*McpServer, error) {
	var srv McpServer
	if err := c.do(ctx, "PUT", path, req, &srv); err != nil {
		return nil, err
	}
	return &srv, nil
}

func bindMcpServer(ctx context.Context, c *Client, path, workspaceID string) error {
	return c.do(ctx, "POST", path, map[string]string{"workspaceId": workspaceID}, nil)
}

func createMcpAutoApply(ctx context.Context, c *Client, path, targetType string, targetID *string) error {
	body := map[string]any{"targetType": targetType}
	if targetID != nil {
		body["targetId"] = *targetID
	}
	return c.do(ctx, "POST", path, body, nil)
}

func listMcpAutoApply(ctx context.Context, c *Client, path string) ([]McpAutoApplyRule, error) {
	var resp struct {
		Rules []McpAutoApplyRule `json:"rules"`
	}
	if err := c.do(ctx, "GET", path, nil, &resp); err != nil {
		return nil, err
	}
	return resp.Rules, nil
}
