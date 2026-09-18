// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package workspace

import (
	"context"
	"fmt"
	"net/http"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/lenaxia/llmsafespaces/pkg/secrets"
)

// staging_guard.go — the fail-loud startup guard (US-72.3 AC: "flag on
// without router: controller startup refuses, loud" — the rbac.scope=
// cluster gate precedent): enabling relay-only key delivery without a
// reachable, bootstrapped router is a deployment misconfiguration that
// must surface at boot, not as per-workspace StageFailed noise.

// ValidateRelayStagingFlags checks the pure flag contract: enabled requires
// the router URL and the credential-source API URL (worklog D1), and a
// namespace.
func ValidateRelayStagingFlags(enabled bool, routerURL, apiServiceURL, namespace string) error {
	if !enabled {
		return nil
	}
	if routerURL == "" {
		return fmt.Errorf("--llm-relay-router-url is required when --relay-only-key-delivery is enabled")
	}
	if apiServiceURL == "" {
		return fmt.Errorf("--api-service-url is required when --relay-only-key-delivery is enabled (the staging pass fetches bound provider credentials from the API's internal endpoint)")
	}
	if namespace == "" {
		return fmt.Errorf("--llm-relay-namespace is required when --relay-only-key-delivery is enabled")
	}
	return nil
}

// ProbeRelayRouter verifies the router is reachable and serving (GET
// /healthz). This is the loud half of the guard: a controller enabled for
// relay-only delivery must refuse to start against a dead router rather
// than fail every workspace reconcile.
func ProbeRelayRouter(ctx context.Context, routerURL string) error {
	httpClient := &http.Client{Timeout: 5 * time.Second}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, routerURL+"/healthz", nil)
	if err != nil {
		return fmt.Errorf("relay-only: building router probe: %w", err)
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("relay-only: llm-relay router not reachable at %s (--llm-relay-router-url): %w", routerURL, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("relay-only: llm-relay router healthz returned %d (URL %s)", resp.StatusCode, routerURL)
	}
	return nil
}

// ValidateRelayStagingStartup is the full boot gate: flags probed, router
// reachable, pub Secret readable + parseable (the router bootstrapped; this
// also proves the controller's name-scoped get-on-pub RBAC — a successful
// read implies the namespace exists, so no cluster-scoped Namespace GET is
// needed or performed), and the mint key created. c must be a DIRECT
// (non-cached) client — the controller-runtime cache is not started this
// early.
func ValidateRelayStagingStartup(ctx context.Context, cfg *RelayStagingConfig, c client.Client) error {
	if cfg == nil {
		return nil
	}
	if cfg.RouterURL == "" || cfg.Namespace == "" {
		return fmt.Errorf("relay-only: router URL and namespace are required")
	}
	if err := ProbeRelayRouter(ctx, cfg.RouterURL); err != nil {
		return err
	}
	pubSec := &corev1.Secret{}
	if err := c.Get(ctx, types.NamespacedName{Name: secrets.RelayPubSecretName, Namespace: cfg.Namespace}, pubSec); err != nil {
		if apierrors.IsNotFound(err) {
			return fmt.Errorf("relay-only: %s not found in %s — the router has not bootstrapped its keypair (or the controller lacks get RBAC on it)", secrets.RelayPubSecretName, cfg.Namespace)
		}
		return fmt.Errorf("relay-only: reading %s: %w", secrets.RelayPubSecretName, err)
	}
	if _, err := secrets.ParseHPKEPubPayload(pubSec.Data[secrets.RelayPubDataKey]); err != nil {
		return fmt.Errorf("relay-only: %s payload is shape-invalid at startup: %w", secrets.RelayPubSecretName, err)
	}
	// The mint key is controller-created (the router reads it lazily); the
	// guard creates it so the first mint never races the router's lazy
	// read. Reads go through the config's (construction-required, direct)
	// API reader — the same one the runtime pass uses.
	if _, err := (&WorkspaceReconciler{Client: c, RelayStaging: cfg}).ensureRelayMintKey(ctx); err != nil {
		return fmt.Errorf("relay-only: ensuring mint key: %w", err)
	}
	return nil
}
