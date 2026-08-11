// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package agent

import (
	"context"
	stderrors "errors"
	"fmt"
	"net/http"
	"sync"

	pkgerrors "github.com/lenaxia/llmsafespaces/pkg/errors"
)

type AgentType string

const (
	AgentTypeOpenCode AgentType = "opencode"
)

// ErrNoRunningPod is the canonical sentinel for "workspace pod is not
// running (empty podIP)". The opencode adapter wraps this via
// fmt.Errorf("workspace %s: %w", workspaceID, ErrNoRunningPod) so
// callers use errors.Is(err, agent.ErrNoRunningPod) without importing
// the opencode package. Previously defined in
// pkg/agent/opencode/agent_client.go — moved here (US-65.6-followup)
// to break the import cycle that forced handlers to import the
// opencode package for the sentinel.
var ErrNoRunningPod = &pkgerrors.StatusError{
	Status:  http.StatusNotFound,
	Code:    "no_running_pod",
	Message: "workspace pod not running",
}

// V2Delivery selects how the agent's V2 session runner admits a prompt.
// Generic equivalent of opencode.V2Delivery; allows proxy_v2.go to use
// V2 types without importing the opencode package.
type V2Delivery string

const (
	V2DeliveryQueue V2Delivery = "queue"
	V2DeliverySteer V2Delivery = "steer"
)

// V2PromptResponse is the response from a V2 prompt admission.
type V2PromptResponse struct {
	AdmittedSeq int    `json:"admittedSeq"`
	ID          string `json:"id"`
	SessionID   string `json:"sessionID"`
}

// V2SessionClient is the subset of agent client methods the proxy's V2
// session-queue paths use. Defined in pkg/agent so proxy_v2.go doesn't
// need to import pkg/agent/opencode.
type V2SessionClient interface {
	PromptV2(ctx context.Context, sessionID, text string, delivery V2Delivery) (*V2PromptResponse, error)
	InterruptV2(ctx context.Context, sessionID string) error
}

// V2ClientFactory builds a V2SessionClient for the given workspace.
type V2ClientFactory func(ctx context.Context, workspaceID string) (V2SessionClient, error)

// V2 error sentinels. Re-exported from pkg/agent/opencode; canonical
// location is here so callers don't import the opencode package.
var (
	ErrV2PromptConflict  = stderrors.New("agent V2: prompt conflict (id collision)")
	ErrV2SessionNotFound = stderrors.New("agent V2: session not found")
)

// IsSessionNotFound returns true if err is or wraps ErrV2SessionNotFound.
// Convenience for handlers that need to map to HTTP 404.
func IsSessionNotFound(err error) bool {
	return stderrors.Is(err, ErrV2SessionNotFound)
}

type CredentialState string

const (
	CredentialStatePresent CredentialState = "Present"
	CredentialStateMissing CredentialState = "Missing"
	CredentialStateInvalid CredentialState = "Invalid"
)

type CredentialCheckResult struct {
	State   CredentialState `json:"state"`
	Agent   AgentType       `json:"agent"`
	Message string          `json:"message,omitempty"`
}

type AgentRuntime interface {
	Type() AgentType
	ValidateCredentials(rawConfig []byte) (*CredentialCheckResult, error)
	FormatProviderConfig(providers []LLMProviderData) ([]byte, error)
}

// LLMProviderData is re-exported from pkg/secrets for use in the interface.
// This avoids a circular import between pkg/agent and pkg/secrets.
//
// Epic 55: see pkg/secrets/types.go for the authoritative documentation.
// Briefly — Kind is the SDK-class enum; Slug is the per-owner unique
// identity that becomes the literal key in agent-config.json's provider
// map (opencode persists it as `providerID` on session records).
type LLMProviderData struct {
	Kind       string           `json:"kind"`
	Slug       string           `json:"slug"`
	APIKey     string           `json:"apiKey"`
	BaseURL    string           `json:"baseURL,omitempty"`
	Models     []LLMModelConfig `json:"models,omitempty"`
	Default    string           `json:"default,omitempty"`
	SmallModel string           `json:"smallModel,omitempty"`
}

// LLMModelConfig specifies a model identifier, optional display label, and
// optional context/output token limits.
//
// ContextLimit and OutputLimit MUST be set together (both > 0) to be emitted
// into opencode's agent-config.json — opencode's published JSON Schema
// (https://opencode.ai/config.json) requires both `context` and `output` when
// the `limit` object is present. See pkg/secrets/types.go LLMModelConfig for
// the authoritative documentation.
type LLMModelConfig struct {
	ID           string `json:"id"`
	Label        string `json:"label,omitempty"`
	ContextLimit int    `json:"contextLimit,omitempty"`
	OutputLimit  int    `json:"outputLimit,omitempty"`
}

var (
	registryMu sync.RWMutex
	registry   = map[AgentType]AgentRuntime{}
)

func Get(agentType AgentType) (AgentRuntime, error) {
	registryMu.RLock()
	defer registryMu.RUnlock()
	a, ok := registry[agentType]
	if !ok {
		return nil, fmt.Errorf("unknown agent type: %s", agentType)
	}
	return a, nil
}

func Register(agentType AgentType, a AgentRuntime) {
	registryMu.Lock()
	defer registryMu.Unlock()
	registry[agentType] = a
}

func Unregister(agentType AgentType) {
	registryMu.Lock()
	defer registryMu.Unlock()
	delete(registry, agentType)
}
