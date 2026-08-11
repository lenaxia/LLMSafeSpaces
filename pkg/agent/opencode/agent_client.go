// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package opencode

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"go.uber.org/zap"

	"github.com/lenaxia/llmsafespaces/pkg/agent"
	"github.com/lenaxia/llmsafespaces/pkg/agentd"
	"github.com/lenaxia/llmsafespaces/pkg/secrets"
)

// ErrNoRunningPod is re-exported from pkg/agent (the canonical location).
// Callers should use agent.ErrNoRunningPod; this alias exists for
// backward compatibility with code that still imports this package.
var ErrNoRunningPod = agent.ErrNoRunningPod

// PasswordResolver resolves the opencode Basic-auth password for a workspace.
// The API-side implementation reads from the K8s Secret cache (pwCache);
// agentd-side reads from /sandbox-cfg/password.
type PasswordResolver func(ctx context.Context, workspaceID string) (string, error)

// PodIPResolver resolves the pod IP for a workspace.
type PodIPResolver interface {
	GetWorkspacePodIP(ctx context.Context, userID, workspaceID string) (string, error)
}

// AgentClient abstracts all direct opencode HTTP communication at the
// workspace level (US-29.1). Each method resolves podIP and password
// internally from the injected resolvers, keeping callers clean of auth
// concerns. The interface is caller-shaped — consumers ask for what
// they need, not for a raw HTTP client.
//
// userID is required for workspace ownership verification (the PodIPResolver
// enforces that the caller owns the workspace before returning the IP).
type AgentClient interface {
	ListModels(ctx context.Context, userID, workspaceID string) ([]byte, error)
	PatchConfig(ctx context.Context, userID, workspaceID string, config map[string]any) error
	DisposeInstance(ctx context.Context, userID, workspaceID string) error
	GetSessionStatuses(ctx context.Context, userID, workspaceID string) (map[string]string, error)
	StageCredentials(ctx context.Context, userID, workspaceID string, providers []secrets.LLMProviderData) error
}

// WorkspaceClientOption configures a WorkspaceClient at construction.
type WorkspaceClientOption func(*WorkspaceClient)

// WithWorkspaceHTTPClient injects a shared *http.Client so connections are
// pooled across all workspace calls (M11-a). When unset, a tuned default is
// used (see newTunedHTTPClient). The client must not set a per-request
// Timeout that would interfere with caller context deadlines.
func WithWorkspaceHTTPClient(hc *http.Client) WorkspaceClientOption {
	return func(w *WorkspaceClient) {
		if hc != nil {
			w.httpClient = hc
		}
	}
}

// newTunedHTTPClient returns an *http.Client configured for multi-workspace
// scale. The defaults Go uses (MaxIdleConns=100, MaxIdleConnsPerHost=2) are
// too low for an API replica talking to hundreds of distinct pod IPs — each
// workspace pod is a separate host, so MaxIdleConnsPerHost=2 evicts pooled
// connections almost immediately. Tuning to 500/10 keeps ~50 workspaces'
// connections warm simultaneously.
func newTunedHTTPClient() *http.Client {
	return &http.Client{
		// No hard client-level timeout — the caller's context deadline
		// is the correct per-request boundary. A fixed client.Timeout
		// covers the entire exchange including body read, which breaks
		// sync Send (blocks on LLM completion, typically 10-120s) and
		// GetHistory on large sessions (#746).
		Transport: &http.Transport{
			MaxIdleConns:        500,
			MaxIdleConnsPerHost: 10,
			MaxConnsPerHost:     50,
			IdleConnTimeout:     90 * time.Second,
		},
	}
}

// WorkspaceClient implements AgentClient by resolving each call to a
// specific pod IP + password, then delegating to the low-level Client.
// It is constructed once and shared across handlers; each call resolves
// the workspace's current podIP + password fresh, so pod migrations and
// password rotations are transparent to callers.
//
// The shared httpClient enables connection pooling across all workspace
// calls (M11-a). The agentPort field replaces the former package-level
// var so tests are parallel-safe (M1-a).
type WorkspaceClient struct {
	passwordResolver PasswordResolver
	podIPResolver    PodIPResolver
	httpClient       *http.Client
	agentPort        int
	logger           *zap.Logger
}

// NewWorkspaceClient creates an AgentClient that resolves workspace →
// podIP + password on each call. The httpClient is shared across all
// calls for connection pooling; agentPort defaults to agentd.AgentPort.
func NewWorkspaceClient(pw PasswordResolver, ip PodIPResolver, logger *zap.Logger, opts ...WorkspaceClientOption) *WorkspaceClient {
	if logger == nil {
		logger = zap.NewNop()
	}
	w := &WorkspaceClient{
		passwordResolver: pw,
		podIPResolver:    ip,
		httpClient:       newTunedHTTPClient(),
		agentPort:        agentd.AgentPort,
		logger:           logger,
	}
	for _, opt := range opts {
		opt(w)
	}
	return w
}

// resolve returns a low-level Client configured for the workspace's pod.
// The shared httpClient is injected so connections are pooled across calls.
//
// Shared between WorkspaceClient (the legacy AgentClient implementation)
// and Adapter (the US-65.3 seam). Both go through this function so
// resolution logic — pod IP lookup, password lookup, baseURL
// construction — has one source of truth (PR #714 review R3).
func (w *WorkspaceClient) resolve(ctx context.Context, userID, workspaceID string) (*Client, error) {
	return resolveWorkspaceClient(ctx, w.podIPResolver, w.passwordResolver, w.agentPort, w.httpClient, w.logger, userID, workspaceID)
}

// resolveWorkspaceClient is the shared resolution helper used by both
// WorkspaceClient.resolve and Adapter.resolve. Returns a *Client
// configured for the workspace's pod + password, or an error wrapping
// ErrNoRunningPod when the pod IP is empty.
func resolveWorkspaceClient(
	ctx context.Context,
	ipResolver PodIPResolver,
	pwResolver PasswordResolver,
	agentPort int,
	httpClient *http.Client,
	logger *zap.Logger,
	userID, workspaceID string,
) (*Client, error) {
	podIP, err := ipResolver.GetWorkspacePodIP(ctx, userID, workspaceID)
	if err != nil {
		return nil, fmt.Errorf("resolve pod IP for workspace %s: %w", workspaceID, err)
	}
	if podIP == "" {
		return nil, fmt.Errorf("workspace %s: %w", workspaceID, ErrNoRunningPod)
	}
	password, err := pwResolver(ctx, workspaceID)
	if err != nil {
		return nil, fmt.Errorf("resolve password for workspace %s: %w", workspaceID, err)
	}
	baseURL := fmt.Sprintf("http://%s:%d", podIP, agentPort)
	return NewClient(baseURL, password, logger, WithHTTPClient(httpClient)), nil
}

func (w *WorkspaceClient) ListModels(ctx context.Context, userID, workspaceID string) ([]byte, error) {
	c, err := w.resolve(ctx, userID, workspaceID)
	if err != nil {
		return nil, err
	}
	return c.ListModels(ctx)
}

func (w *WorkspaceClient) PatchConfig(ctx context.Context, userID, workspaceID string, config map[string]any) error {
	c, err := w.resolve(ctx, userID, workspaceID)
	if err != nil {
		return err
	}
	return c.PatchConfig(ctx, config)
}

func (w *WorkspaceClient) DisposeInstance(ctx context.Context, userID, workspaceID string) error {
	c, err := w.resolve(ctx, userID, workspaceID)
	if err != nil {
		return err
	}
	return c.DisposeInstance(ctx)
}

func (w *WorkspaceClient) GetSessionStatuses(ctx context.Context, userID, workspaceID string) (map[string]string, error) {
	c, err := w.resolve(ctx, userID, workspaceID)
	if err != nil {
		return nil, err
	}
	return c.GetSessionStatuses(ctx)
}

func (w *WorkspaceClient) StageCredentials(ctx context.Context, userID, workspaceID string, providers []secrets.LLMProviderData) error {
	c, err := w.resolve(ctx, userID, workspaceID)
	if err != nil {
		return err
	}
	return c.StageCredentials(ctx, providers)
}

// Compile-time check that WorkspaceClient satisfies AgentClient.
var _ AgentClient = (*WorkspaceClient)(nil)
