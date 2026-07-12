// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package controller

import (
	"time"

	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/lenaxia/llmsafespaces/controller/internal/relay"
	"github.com/lenaxia/llmsafespaces/controller/internal/workspace"
	"github.com/lenaxia/llmsafespaces/pkg/agent/opencode"
)

func init() {
	opencode.Register()
}

// orgStatusCacheTTL is the freshness window for the controller's in-memory
// org-status cache (D20). 30s balances staleness (a suspended org's workspaces
// keep running for up to this long after suspension) against API load (one
// status fetch per org per window).
const orgStatusCacheTTL = 30 * time.Second

func SetupControllers(mgr ctrl.Manager, inferenceRelayURL, apiServiceURL, apiInternalToken, defaultRuntimeClass string) error {
	logger := log.Log.WithName("controller")
	logger.Info("Setting up controllers")

	// US-43.19 / D20: build the org-status client the reconciler polls to drive
	// org-level workspace suspension. Empty apiServiceURL disables the feature
	// (the reconciler then never org-suspends — safe default for installs that
	// have not wired the internal endpoint).
	var orgStatusClient workspace.OrgStatusClient
	if apiServiceURL != "" {
		orgStatusClient = workspace.NewCachedOrgStatusClient(apiServiceURL, apiInternalToken, orgStatusCacheTTL, logger)
		logger.Info("org-status suspension enabled", "apiServiceURL", apiServiceURL, "cacheTTL", orgStatusCacheTTL)
	} else {
		logger.Info("org-status suspension disabled (--api-service-url unset)")
	}

	if err := (&workspace.WorkspaceReconciler{
		Client:              mgr.GetClient(),
		Scheme:              mgr.GetScheme(),
		InferenceRelayURL:   inferenceRelayURL,
		OrgStatusClient:     orgStatusClient,
		DefaultRuntimeClass: defaultRuntimeClass,
		APIServiceURL:       apiServiceURL,
	}).SetupWithManager(mgr); err != nil {
		logger.Error(err, "unable to create Workspace controller")
		return err
	}

	return nil
}

// RelayArtifactConfig holds the relay-proxy binary distribution settings the
// controller embeds into each relay VM's cloud-init. All fields are required
// when the relay controller is enabled: a VM without a download path produces
// a structurally provisioned but non-functional relay (systemd unit references
// a binary that never arrives).
type RelayArtifactConfig struct {
	// URLs are the base mirror URLs (cloud-init appends "/<binary>"). At least
	// one is required; multiple provide cross-cloud resilience.
	URLs []string
	// SHA256Arm64 is the hex SHA-256 of the arm64 relay-proxy binary.
	SHA256Arm64 string
	// SHA256Amd64 is the hex SHA-256 of the amd64 relay-proxy binary.
	SHA256Amd64 string
}

// SetupRelayController registers the InferenceRelay reconciler and the
// orphan detector (the periodic safety net that catches cloud VMs whose
// owner CR has gone away). It is feature-gated and only activated when
// enableRelay is true.
func SetupRelayController(mgr ctrl.Manager, namespace, routerURL string, enableRelay bool, artifact RelayArtifactConfig) error {
	if !enableRelay {
		return nil
	}

	logger := log.Log.WithName("controller")
	logger.Info("Setting up InferenceRelay controller")

	// Construct drivers once and share them between the reconciler and
	// the orphan detector so both observe the same provider set.
	drivers := map[string]relay.ProviderDriver{
		"aws": relay.NewAWSDriver(mgr.GetClient(), namespace, "aws-relay-irwa"),
		"oci": relay.NewOCIDriver(mgr.GetClient(), namespace, "oci-credentials"),
	}

	relayReconciler := &relay.InferenceRelayReconciler{
		Client:        mgr.GetClient(),
		Scheme:        mgr.GetScheme(),
		Namespace:     namespace,
		HealthChecker: relay.NewHealthChecker(routerURL),
		Drivers:       drivers,
		ExpectedCredentialSecrets: map[string]string{
			"aws": "aws-relay-irwa",
			"oci": "oci-credentials",
		},
		ArtifactURLs:        artifact.URLs,
		ArtifactSHA256Arm64: artifact.SHA256Arm64,
		ArtifactSHA256Amd64: artifact.SHA256Amd64,
	}

	if err := relayReconciler.SetupWithManager(mgr); err != nil {
		logger.Error(err, "unable to create InferenceRelay controller")
		return err
	}

	// Register the orphan detector as a manager runnable. It runs only
	// on the leader (NeedLeaderElection() returns true) so multi-replica
	// controllers don't race to destroy the same orphans. Catches the
	// case where the per-CR adopt + sweep paths missed an instance —
	// e.g. controller crashed mid-finalizer, or pre-fix-version VMs
	// with the legacy tag schema. See worklog 0473/0474.
	detector := &relay.OrphanDetector{
		Client:  mgr.GetClient(),
		Drivers: drivers,
		// Default 5min interval is set inside Start() if Interval is zero.
	}
	if err := mgr.Add(detector); err != nil {
		logger.Error(err, "unable to register relay OrphanDetector")
		return err
	}
	logger.Info("relay OrphanDetector registered (leader-only, default 5m interval)")

	return nil
}
