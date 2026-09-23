// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package controller

import (
	"context"
	"fmt"
	"time"

	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/lenaxia/llmsafespaces/controller/internal/relay"
	"github.com/lenaxia/llmsafespaces/controller/internal/workspace"
	"github.com/lenaxia/llmsafespaces/pkg/redact"
	"github.com/lenaxia/llmsafespaces/pkg/secrets"
	// opencode import removed; registration happens in controller main.
)

// RegisterAgentRuntime is called from controller main to register the
// opencode agent runtime. The actual opencode.Register() call happens in
// the controller main package (the allowed construction layer).
func RegisterAgentRuntime() {
	// No-op; controller main calls opencode.Register() directly.
}

// orgStatusCacheTTL is the freshness window for the controller's in-memory
// org-status cache (D20). 30s balances staleness (a suspended org's workspaces
// keep running for up to this long after suspension) against API load (one
// status fetch per org per window).
const orgStatusCacheTTL = 30 * time.Second

// AgentdDelivery is the #863 image-volume agentd delivery configuration.
// Zero value (empty Image) = legacy baked-in mode. When set, every
// workspace pod gets a digest-pinned image volume + RO mount + sha256
// verify pins; validateAgentdDeliveryConfig enforces the all-or-nothing
// contract at startup.
type AgentdDelivery = workspace.AgentdDeliveryConfig

// OpencodeDelivery is the design 0053 §4.2 image-volume opencode
// delivery configuration. Zero value (empty Image) = baked-in mode (S1
// default, opt-in). When set, every workspace pod gets a digest-pinned
// image volume + RO mount (workspace container only) + sha256 verify
// pins; validateOpencodeDeliveryConfig enforces the all-or-nothing
// contract at startup.
type OpencodeDelivery = workspace.OpencodeDeliveryConfig

func SetupControllers(mgr ctrl.Manager, inferenceRelayURL, apiServiceURL, apiPublicURL, apiInternalToken, defaultRuntimeClass, previewOriginBaseDomain string, uploadStaging workspace.UploadStagingConfig, agentdDelivery AgentdDelivery, opencodeDelivery OpencodeDelivery, agentdSidecarEnabled bool, maxConcurrentReconciles int, relayStaging *workspace.RelayStagingConfig, workspaceTerminationGrace int64) error {
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
		Client:                    mgr.GetClient(),
		Scheme:                    mgr.GetScheme(),
		InferenceRelayURL:         inferenceRelayURL,
		OrgStatusClient:           orgStatusClient,
		DefaultRuntimeClass:       defaultRuntimeClass,
		APIServiceURL:             apiServiceURL,
		APIPublicURL:              apiPublicURL,
		PreviewOriginBaseDomain:   previewOriginBaseDomain,
		AgentdImage:               agentdDelivery.Image,
		AgentdBinarySHA256AMD64:   agentdDelivery.BinarySHA256AMD64,
		AgentdBinarySHA256ARM64:   agentdDelivery.BinarySHA256ARM64,
		OpencodeImage:             opencodeDelivery.Image,
		OpencodeBinarySHA256AMD64: opencodeDelivery.BinarySHA256AMD64,
		OpencodeBinarySHA256ARM64: opencodeDelivery.BinarySHA256ARM64,
		AgentdSidecarEnabled:      agentdSidecarEnabled,
		UploadStaging:             uploadStaging,
		Recorder:                  mgr.GetEventRecorderFor("workspace-controller"),
		// Clamped upstream (main.go, 1..64); <=0 keeps controller-runtime's
		// fully-serial default so unit tests that don't set it are unchanged.
		MaxConcurrentReconciles: maxConcurrentReconciles,
		// Epic 72 / US-72.3: nil (the default, --relay-only-key-delivery
		// off) means the staging pass is a no-op — zero behavior change.
		RelayStaging: relayStaging,
		// #1507: 0 keeps the pod_builder default (40s).
		WorkspaceTerminationGraceSeconds: workspaceTerminationGrace,
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

// RelayStagingNotArmedExitCode (design 0061 §3, M1 — crash-loud arming):
// the controller's distinct exit code for "deployed relay-only but not
// armed" — the fifth rung of the fail-closed doctrine's ladder
// (81/82: agentd verify; 83/84: opencode verify; 85: relay staging not
// armed). A CrashLoop with this code is one describe-pod away from its
// reason; the enable line ("relay-only key delivery enabled") is the
// armed contract its absence betrays. The #1548 split-brain (an inert
// staging binary booted green while workspaces starved) is the class
// this code makes undeployable.
const RelayStagingNotArmedExitCode = 85

// ArmingStartupGuardWindow (design 0061 §3): the bounded startup window
// within which enabled=true must reach armed state — the startup
// guard's existing 30s budget, named here as the design's constant (no
// new timer machinery; the guard's context construction reads THIS).
var ArmingStartupGuardWindow = 30 * time.Second

// SetupRelayStaging constructs the US-72.3 relay staging config from the
// deployment flags and runs the FAIL-LOUD startup guard (the
// rbac.scope=cluster gate precedent): enabled without a reachable,
// bootstrapped router is a deployment misconfiguration that must surface
// at boot. Returns (nil, nil) when the flag is off — zero behavior change.
// The direct (non-cached) client is used because the manager's cache is
// not started this early, and the llm-relay namespace may sit outside the
// controller's watch scope.
func SetupRelayStaging(mgr ctrl.Manager, enabled bool, routerURL, namespace string, tokenTTL time.Duration, apiServiceURL, apiInternalToken string) (*workspace.RelayStagingConfig, error) {
	if !enabled {
		return nil, nil
	}
	if tokenTTL < time.Second || tokenTTL > 7*24*time.Hour {
		return nil, fmt.Errorf("--relay-token-ttl must be within 1s..7d, got %s", tokenTTL)
	}
	if err := workspace.ValidateRelayStagingFlags(true, routerURL, apiServiceURL, namespace); err != nil {
		return nil, err
	}
	redactor, err := redact.NewRedactor(nil)
	if err != nil {
		return nil, fmt.Errorf("relay staging redactor: %w", err)
	}
	// Every llm-relay READ goes through the direct API reader: the cached
	// client cannot serve the llm-relay namespace (out-of-scope namespaces
	// fail without an API call; an in-scope informer would need LIST+WATCH
	// the Role withholds — review r3 finding 1). Writes bypass the cache
	// and stay on the reconciler client. The reader is REQUIRED at
	// construction — no cached-client fallback exists.
	cfg, err := workspace.NewRelayStagingConfig(
		routerURL,
		namespace,
		tokenTTL,
		workspace.NewCachedLLMProviderSource(apiServiceURL, apiInternalToken, 0),
		workspace.NewHTTPRelayRouterClient(routerURL, namespace, mgr.GetAPIReader()),
		secrets.RedactStagedKeys{Redactor: redactor},
		mgr.GetAPIReader(),
	)
	if err != nil {
		return nil, err
	}
	directClient, err := client.New(mgr.GetConfig(), client.Options{Scheme: mgr.GetScheme()})
	if err != nil {
		return nil, fmt.Errorf("building startup-guard client: %w", err)
	}
	guardCtx, cancel := context.WithTimeout(context.Background(), ArmingStartupGuardWindow)
	defer cancel()
	if err := workspace.ValidateRelayStagingStartup(guardCtx, cfg, directClient); err != nil {
		return nil, fmt.Errorf("startup guard FAILED — refusing to start (fix the router/namespace/RBAC configuration or disable --relay-only-key-delivery): %w", err)
	}
	log.Log.WithName("controller").Info("relay-only key delivery enabled",
		"routerURL", routerURL, "namespace", namespace, "tokenTTL", tokenTTL)
	return cfg, nil
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
