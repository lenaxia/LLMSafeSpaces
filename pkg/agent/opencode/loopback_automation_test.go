// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package opencode

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newAutomationServer records one request per call and serves canned
// responses; the client under test carries the SA token.
func newAutomationServer(t *testing.T, status int, resp string) (*Client, *automationRecorder) {
	t.Helper()
	rec := &automationRecorder{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// The §D1-adjacent pod-identity gate: SA bearer token required.
		auth := r.Header.Get("Authorization")
		require.Equal(t, "Bearer sa-token", auth, "automation calls carry the SA token")
		rec.method, rec.path = r.Method, r.URL.Path
		rec.query = r.URL.RawQuery
		buf := make([]byte, r.ContentLength)
		if r.ContentLength > 0 {
			_, _ = r.Body.Read(buf)
			rec.body = string(buf)
		}
		w.WriteHeader(status)
		_, _ = w.Write([]byte(resp))
	}))
	t.Cleanup(srv.Close)
	return NewLoopbackClient(srv.URL, "pw"), rec
}

type automationRecorder struct {
	method, path, query, body string
}

func TestSeam_TriggerCRUDWire(t *testing.T) {
	cases := []struct {
		name   string
		call   func(c *Client) (*AutomationResponse, error)
		method string
		path   string
		body   string
	}{
		{"list", func(c *Client) (*AutomationResponse, error) { return c.TriggerList(ctx(), "sa-token", "ws-1") }, "GET", "/internal/v1/automation/triggers", ""},
		{"create", func(c *Client) (*AutomationResponse, error) {
			return c.TriggerCreate(ctx(), "sa-token", "ws-1", json.RawMessage(`{"name":"nightly","sourceType":"cron","sourceConfig":{"schedule":"0 3 * * *"}}`))
		}, "POST", "/internal/v1/automation/triggers", `"workspaceID":"ws-1"`},
		{"get", func(c *Client) (*AutomationResponse, error) {
			return c.TriggerGet(ctx(), "sa-token", "ws-1", "0f8d2c1a-3b4e-4f5a-9c6d-7e8f9a0b1c2d")
		}, "GET", "/internal/v1/automation/triggers/0f8d2c1a-3b4e-4f5a-9c6d-7e8f9a0b1c2d", ""},
		{"update", func(c *Client) (*AutomationResponse, error) {
			return c.TriggerUpdate(ctx(), "sa-token", "ws-1", "0f8d2c1a-3b4e-4f5a-9c6d-7e8f9a0b1c2d", json.RawMessage(`{"enabled":false}`))
		}, "PUT", "/internal/v1/automation/triggers/0f8d2c1a-3b4e-4f5a-9c6d-7e8f9a0b1c2d", `"enabled":false`},
		{"delete", func(c *Client) (*AutomationResponse, error) {
			return c.TriggerDelete(ctx(), "sa-token", "ws-1", "0f8d2c1a-3b4e-4f5a-9c6d-7e8f9a0b1c2d")
		}, "DELETE", "/internal/v1/automation/triggers/0f8d2c1a-3b4e-4f5a-9c6d-7e8f9a0b1c2d", ""},
		{"fires", func(c *Client) (*AutomationResponse, error) {
			return c.TriggerFires(ctx(), "sa-token", "ws-1", "0f8d2c1a-3b4e-4f5a-9c6d-7e8f9a0b1c2d")
		}, "GET", "/internal/v1/automation/triggers/0f8d2c1a-3b4e-4f5a-9c6d-7e8f9a0b1c2d/fires", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, rec := newAutomationServer(t, 200, `{"ok":true}`)
			res, err := tc.call(c)
			require.NoError(t, err)
			assert.Equal(t, tc.method, rec.method)
			assert.Equal(t, tc.path, rec.path)
			// CREATE carries the workspace in-body (the resolver's body
			// path); every other method carries it in-query.
			if tc.name == "create" {
				assert.Contains(t, rec.body, `"workspaceID":"ws-1"`)
			} else {
				assert.Contains(t, rec.query, "workspaceID=ws-1")
			}
			if tc.body != "" {
				assert.Contains(t, rec.body, tc.body)
			}
			assert.Contains(t, res.Body, `"ok":true`)
		})
	}
}

func TestSeam_WorkflowCRUDWire(t *testing.T) {
	cases := []struct {
		name   string
		call   func(c *Client) (*AutomationResponse, error)
		method string
		path   string
	}{
		{"list", func(c *Client) (*AutomationResponse, error) { return c.WorkflowList(ctx(), "sa-token", "ws-1") }, "GET", "/internal/v1/automation/workflows"},
		{"create", func(c *Client) (*AutomationResponse, error) {
			return c.WorkflowCreate(ctx(), "sa-token", "ws-1", json.RawMessage(`{"name":"nightly"}`))
		}, "POST", "/internal/v1/automation/workflows"},
		{"get", func(c *Client) (*AutomationResponse, error) {
			return c.WorkflowGet(ctx(), "sa-token", "ws-1", "1a2b3c4d-5e6f-4a7b-8c9d-0e1f2a3b4c5d")
		}, "GET", "/internal/v1/automation/workflows/1a2b3c4d-5e6f-4a7b-8c9d-0e1f2a3b4c5d"},
		{"update", func(c *Client) (*AutomationResponse, error) {
			return c.WorkflowUpdate(ctx(), "sa-token", "ws-1", "1a2b3c4d-5e6f-4a7b-8c9d-0e1f2a3b4c5d", json.RawMessage(`{"name":"x"}`))
		}, "PUT", "/internal/v1/automation/workflows/1a2b3c4d-5e6f-4a7b-8c9d-0e1f2a3b4c5d"},
		{"delete", func(c *Client) (*AutomationResponse, error) {
			return c.WorkflowDelete(ctx(), "sa-token", "ws-1", "1a2b3c4d-5e6f-4a7b-8c9d-0e1f2a3b4c5d")
		}, "DELETE", "/internal/v1/automation/workflows/1a2b3c4d-5e6f-4a7b-8c9d-0e1f2a3b4c5d"},
		{"run", func(c *Client) (*AutomationResponse, error) {
			return c.WorkflowRun(ctx(), "sa-token", "ws-1", "1a2b3c4d-5e6f-4a7b-8c9d-0e1f2a3b4c5d", nil)
		}, "POST", "/internal/v1/automation/workflows/1a2b3c4d-5e6f-4a7b-8c9d-0e1f2a3b4c5d/runs"},
		{"runs", func(c *Client) (*AutomationResponse, error) {
			return c.WorkflowRuns(ctx(), "sa-token", "ws-1", "1a2b3c4d-5e6f-4a7b-8c9d-0e1f2a3b4c5d")
		}, "GET", "/internal/v1/automation/workflows/1a2b3c4d-5e6f-4a7b-8c9d-0e1f2a3b4c5d/runs"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, rec := newAutomationServer(t, 200, `{}`)
			_, err := tc.call(c)
			require.NoError(t, err)
			assert.Equal(t, tc.method, rec.method)
			assert.Equal(t, tc.path, rec.path)
		})
	}
}

func TestSeam_AutomationCreateStampsWorkspaceID(t *testing.T) {
	c, rec := newAutomationServer(t, 201, `{"id":"tr-9"}`)
	_, err := c.TriggerCreate(ctx(), "sa-token", "ws-7", json.RawMessage(`{"name":"x","workspaceId":"ws-OTHER"}`))
	require.NoError(t, err)
	assert.Contains(t, rec.body, `"workspaceID":"ws-7"`, "the SA-derived workspaceID overrides any caller-supplied id")
	assert.NotContains(t, rec.body, "ws-OTHER")
}

func TestSeam_AutomationErrorSurfacesBody(t *testing.T) {
	c, _ := newAutomationServer(t, 400, `{"error":"invalid cron source config: bad schedule"}`)
	_, err := c.TriggerCreate(ctx(), "sa-token", "ws-1", json.RawMessage(`{}`))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "400")
	assert.Contains(t, err.Error(), "invalid cron source config", "the platform's field-naming error text is load-bearing")
}

func TestSeam_AutomationInvalidIDRejected(t *testing.T) {
	c, _ := newAutomationServer(t, 200, `{}`)
	for _, fn := range []func() error{
		func() error { _, err := c.TriggerGet(ctx(), "sa-token", "ws-1", "../etc"); return err },
		func() error { _, err := c.TriggerDelete(ctx(), "sa-token", "ws-1", "tr/x"); return err },
		func() error { _, err := c.WorkflowRun(ctx(), "sa-token", "ws-1", "wf?x", nil); return err },
	} {
		assert.Error(t, fn())
	}
}

func TestSeam_WorkflowRunEmptyInputDefaults(t *testing.T) {
	c, rec := newAutomationServer(t, 202, `{"id":"run-1"}`)
	_, err := c.WorkflowRun(ctx(), "sa-token", "ws-1", "1a2b3c4d-5e6f-4a7b-8c9d-0e1f2a3b4c5d", nil)
	require.NoError(t, err)
	assert.JSONEq(t, `{"input":{}}`, rec.body, "nil input sends CreateWorkflowRunRequest{input:{}}")
}

func ctx() context.Context { return context.Background() }

var _ = fmt.Sprintf
var _ = strings.TrimSpace
