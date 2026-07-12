// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"time"

	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	_ "k8s.io/client-go/plugin/pkg/client/auth/gcp"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	ctrlmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	"sigs.k8s.io/controller-runtime/pkg/webhook"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	"github.com/lenaxia/llmsafespaces/controller/internal/controller"
	"github.com/lenaxia/llmsafespaces/controller/internal/freemodels"
	"github.com/lenaxia/llmsafespaces/controller/internal/metrics"
	"github.com/lenaxia/llmsafespaces/controller/internal/webhooks"
	v1 "github.com/lenaxia/llmsafespaces/pkg/apis/llmsafespaces/v1"
)

var (
	scheme   = runtime.NewScheme()
	setupLog = ctrl.Log.WithName("setup")

	// ldflags injection targets — set by the build system.
	version   = "dev"
	commitSHA = "unknown"
	buildTime = "unknown"
)

func init() {
	_ = clientgoscheme.AddToScheme(scheme)
	_ = v1.AddToScheme(scheme)
}

func main() {
	var metricsAddr string
	var enableLeaderElection bool
	var probeAddr string
	var watchNamespaces string
	var allowedImageRegistries string
	var allowedStorageClassNames string
	var maxStorageGi int64
	var maxCPUMillicores int64
	var maxMemoryMi int64
	flag.StringVar(&metricsAddr, "metrics-addr", ":8080", "The address the metric endpoint binds to.")
	flag.StringVar(&probeAddr, "health-probe-bind-address", ":8081", "The address the probe endpoint binds to.")
	flag.BoolVar(&enableLeaderElection, "enable-leader-election", false,
		"Enable leader election for controller manager. "+
			"Enabling this will ensure there is only one active controller manager.")
	flag.StringVar(&watchNamespaces, "watch-namespaces", "",
		"Comma-separated list of namespaces the controller should watch. "+
			"Empty or '*' means watch all namespaces (default).")
	flag.StringVar(&allowedImageRegistries, "allowed-image-registries", "",
		"Comma-separated list of registry prefixes accepted as Workspace.spec.runtime "+
			"image references (e.g. 'ghcr.io/lenaxia/,registry.k8s.io/'). Empty list "+
			"means only RuntimeEnvironment-name references are allowed (G2 / F1.2.1).")
	flag.StringVar(&allowedStorageClassNames, "allowed-storage-class-names", "",
		"Comma-separated list of StorageClass names accepted as "+
			"Workspace.spec.storage.storageClassName. Empty means accept any (G2 / F1.2.9).")
	flag.Int64Var(&maxStorageGi, "max-workspace-storage-gi", 1024,
		"Maximum spec.storage.size in GiB. Set 0 to disable. (G2 / RT-6.1).")
	flag.Int64Var(&maxCPUMillicores, "max-workspace-cpu-millicores", 16000,
		"Maximum spec.resources.cpu in millicores (16000 = 16 cores). Set 0 to disable. (G4 / F1.2.3).")
	flag.Int64Var(&maxMemoryMi, "max-workspace-memory-mi", 65536,
		"Maximum spec.resources.memory in MiB (65536 = 64GiB). Set 0 to disable. (G4 / F1.2.3).")
	var inferenceRelayURL string
	flag.StringVar(&inferenceRelayURL, "inference-relay-url", "",
		"Self-hosted relay URL (InferenceRelay fleet, Epic 42). "+
			"When set, workspace pods route opencode free-tier requests through this URL for IP distribution. "+
			"Empty (the default) means workspace pods call https://opencode.ai/zen/v1 directly.")
	var enableRelayController bool
	flag.BoolVar(&enableRelayController, "enable-inference-relay", false,
		"Enable the InferenceRelay controller (Epic 42). When true, the controller reconciles InferenceRelay CRs and manages relay VMs.")
	var relayRouterURL string
	flag.StringVar(&relayRouterURL, "relay-router-url", "http://relay-router:8080",
		"URL of the in-cluster relay-router for /metrics scraping (Epic 42).")
	var relayArtifactURL string
	flag.StringVar(&relayArtifactURL, "relay-artifact-url", "",
		"Comma-separated base mirror URLs for the relay-proxy binary download (Epic 42). "+
			"The controller embeds these into each relay VM's cloud-init; the VM appends "+
			"\"/relay-proxy-<arch>\" and tries each mirror in order. Required when "+
			"--enable-inference-relay=true: without it the provisioned VM cannot obtain the binary.")
	var relayArtifactSHA256Arm64 string
	flag.StringVar(&relayArtifactSHA256Arm64, "relay-artifact-sha256-arm64", "",
		"Hex SHA-256 of the arm64 relay-proxy binary (Epic 42). Verified by cloud-init before exec. "+
			"Required when --enable-inference-relay=true and provisioning arm64 shapes (AWS t4g, OCI A1).")
	var relayArtifactSHA256Amd64 string
	flag.StringVar(&relayArtifactSHA256Amd64, "relay-artifact-sha256-amd64", "",
		"Hex SHA-256 of the amd64 relay-proxy binary (Epic 42). Verified by cloud-init before exec. "+
			"Required when --enable-inference-relay=true and provisioning amd64 shapes (GCP e2, AWS t3).")
	var apiServiceURL string
	flag.StringVar(&apiServiceURL, "api-service-url", "",
		"Root URL of the in-cluster API service (e.g. http://llmsafespaces-api.llmsafespaces.svc:8080) "+
			"used to poll org status for D20 org-level workspace suspension. "+
			"Empty disables org-suspension (the controller never org-suspends).")
	var defaultRuntimeClass string
	flag.StringVar(&defaultRuntimeClass, "default-runtime-class", "",
		"Default RuntimeClass for workspace pods (Epic 51 S51.1). "+
			"Set to 'gvisor' for production multi-tenant isolation. "+
			"Empty means runc (default K8s runtime). "+
			"Individual workspaces can override via spec.runtimeClass.")
	var maxWorkspacesPerTenant int
	flag.IntVar(&maxWorkspacesPerTenant, "max-workspaces-per-tenant", 0,
		"Maximum concurrent workspace pods per tenant (Epic 51 S51.2). "+
			"0 means unlimited. Recommended: 10-20 for multi-tenant deployments.")
	var maxCPUMillisPerTenant int64
	flag.Int64Var(&maxCPUMillisPerTenant, "max-cpu-millis-per-tenant", 0,
		"Maximum aggregate CPU requests (millicores) per tenant (Epic 51 S51.2). "+
			"0 means unlimited. Recommended: 8000 (8 cores) for multi-tenant.")
	var maxMemoryMiPerTenant int64
	flag.Int64Var(&maxMemoryMiPerTenant, "max-memory-mi-per-tenant", 0,
		"Maximum aggregate memory requests (MiB) per tenant (Epic 51 S51.2). "+
			"0 means unlimited. Recommended: 16384 (16GiB) for multi-tenant.")
	var enableFreeModelsRefresher bool
	flag.BoolVar(&enableFreeModelsRefresher, "enable-free-models-refresher", true,
		"Periodically fetch the opencode free-tier model catalog from models.dev "+
			"and publish it as a ConfigMap in POD_NAMESPACE. Workspace pods consume "+
			"this CM to pre-render their relay agent-config.json before opencode "+
			"boots, eliminating the in-pod opencode-restart cycle that the legacy "+
			"relay-injector goroutine imposed (~6-8s saved per cold start). Default "+
			"true; set false to disable and fall back to per-pod fetching.")
	var freeModelsRefreshInterval time.Duration
	flag.DurationVar(&freeModelsRefreshInterval, "free-models-refresh-interval", 6*time.Hour,
		"How often the free-models refresher fetches the catalog. The catalog "+
			"changes ~weekly so 6h is generous; lower values are fine but "+
			"increase load on models.dev.")
	var freeModelsAPIURL string
	flag.StringVar(&freeModelsAPIURL, "free-models-api-url", "",
		"Override URL for the free-models catalog. Empty defaults to "+
			"https://models.dev/api.json. Useful for air-gapped clusters that "+
			"mirror the catalog internally.")
	flag.Parse()

	// US-43.19 / D20: the shared secret authenticating controller→API internal
	// calls. Read from the same env var the API service uses
	// (LLMSAFESPACES_INTERNAL_TOKEN) so a single mounted Secret configures both
	// sides. Empty means no X-Internal-Token header is sent; the API endpoint
	// fails closed (403) in that case, so org-suspension is non-functional
	// until both sides are configured (the chart wires both by default).
	apiInternalToken := os.Getenv("LLMSAFESPACES_INTERNAL_TOKEN")

	ctrl.SetLogger(zap.New(zap.UseDevMode(true)))
	setupLog.Info("starting controller", "version", version, "commit", commitSHA, "built", buildTime)

	// Register custom metrics with the controller-runtime metrics registry
	// (not prometheus.DefaultRegisterer). controller-runtime v0.15+ serves
	// /metrics from its own private registry; registering on the global
	// default makes the metrics invisible to the scrape endpoint.
	if err := metrics.RegisterWith(ctrlmetrics.Registry); err != nil {
		setupLog.Error(err, "unable to register custom metrics")
		os.Exit(1)
	}

	// Create manager options
	options := ctrl.Options{
		Scheme:                 scheme,
		Metrics:                metricsserver.Options{BindAddress: metricsAddr},
		WebhookServer:          webhook.NewServer(webhook.Options{Port: 9443}),
		HealthProbeBindAddress: probeAddr,
		LeaderElection:         enableLeaderElection,
		LeaderElectionID:       "llmsafespaces-controller-leader-election",
	}

	// Restrict the cache (and thus the controllers) to a specific set of
	// namespaces if --watch-namespaces is set. Empty or "*" means cluster-wide.
	if nsMap := parseWatchNamespaces(watchNamespaces); nsMap != nil {
		options.Cache = cache.Options{DefaultNamespaces: nsMap}
		setupLog.Info("watching specific namespaces", "namespaces", watchNamespaces)
	} else {
		setupLog.Info("watching all namespaces")
	}

	if enableLeaderElection {
		leaseDuration := 15 * time.Second
		renewDeadline := 10 * time.Second
		retryPeriod := 2 * time.Second
		options.LeaseDuration = &leaseDuration
		options.RenewDeadline = &renewDeadline
		options.RetryPeriod = &retryPeriod
	}

	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), options)
	if err != nil {
		setupLog.Error(err, "unable to start manager")
		os.Exit(1)
	}

	// Register webhooks. We construct the decoder explicitly because
	// controller-runtime v0.15+ removed the InjectDecoder dependency-injection
	// pattern; webhooks now require their decoder to be set at construction.
	// Without this, all admission requests panic with nil-pointer-deref on
	// the nil decoder.
	webhookDecoder := admission.NewDecoder(mgr.GetScheme())

	mgr.GetWebhookServer().Register("/validate-llmsafespaces-dev-v1-runtimeenvironment", &webhook.Admission{
		Handler: &webhooks.RuntimeEnvironmentValidator{
			Decoder:                webhookDecoder,
			AllowedImageRegistries: splitNonEmpty(allowedImageRegistries, ","),
		},
	})

	// G2 — Workspace admission webhook closes F1.2.1 (registry allow-list),
	// F1.2.2 (status forge), F1.2.9 (storage class allow-list), and RT-6.1
	// (storage size upper bound). Configuration is operator-supplied via
	// flags so the same chart works in every deployment topology.
	mgr.GetWebhookServer().Register("/validate-llmsafespaces-dev-v1-workspace", &webhook.Admission{
		Handler: &webhooks.WorkspaceValidator{
			Decoder:                  webhookDecoder,
			AllowedImageRegistries:   splitNonEmpty(allowedImageRegistries, ","),
			AllowedStorageClassNames: splitNonEmpty(allowedStorageClassNames, ","),
			MaxStorageGi:             maxStorageGi,
			MaxCPUMillicores:         maxCPUMillicores,
			MaxMemoryMi:              maxMemoryMi,
		},
	})

	// Epic 51 S51.2 — per-tenant resource quota webhook. Enforces
	// max-workspaces / max-CPU / max-memory per tenant by counting existing
	// workspace pods at admission time. Only active when limits > 0;
	// disabled (no-op) by default for single-tenant deployments.
	if maxWorkspacesPerTenant > 0 || maxCPUMillisPerTenant > 0 || maxMemoryMiPerTenant > 0 {
		mgr.GetWebhookServer().Register("/validate-pod-tenant-quota", &webhook.Admission{
			Handler: &webhooks.PodTenantQuotaValidator{
				Decoder:                webhookDecoder,
				Client:                 mgr.GetClient(),
				MaxWorkspacesPerTenant: maxWorkspacesPerTenant,
				MaxCPUMillisPerTenant:  maxCPUMillisPerTenant,
				MaxMemoryMiPerTenant:   maxMemoryMiPerTenant,
			},
		})
		setupLog.Info("tenant quota webhook enabled",
			"maxWorkspaces", maxWorkspacesPerTenant,
			"maxCPUMillis", maxCPUMillisPerTenant,
			"maxMemoryMi", maxMemoryMiPerTenant)
	}

	// Set up controllers
	if err := controller.SetupControllers(mgr, inferenceRelayURL, apiServiceURL, apiInternalToken, defaultRuntimeClass); err != nil {
		setupLog.Error(err, "unable to set up controllers")
		os.Exit(1)
	}

	// Set up InferenceRelay controller (feature-gated)
	relayNamespace := os.Getenv("POD_NAMESPACE")
	if relayNamespace == "" {
		relayNamespace = "llmsafespaces"
	}
	if err := controller.SetupRelayController(mgr, relayNamespace, relayRouterURL, enableRelayController, controller.RelayArtifactConfig{
		URLs:        splitNonEmpty(relayArtifactURL, ","),
		SHA256Arm64: relayArtifactSHA256Arm64,
		SHA256Amd64: relayArtifactSHA256Amd64,
	}); err != nil {
		setupLog.Error(err, "unable to set up InferenceRelay controller")
		os.Exit(1)
	}

	// Seed the WorkspacesRunning gauge from current cluster state.
	// Without this, the gauge drifts negative on controller restart
	// because existing Active workspaces don't re-trigger the
	// Creating→Active transition that calls .Inc(), but Suspend
	// unconditionally calls .Dec().
	if err := mgr.Add(&workspaceGaugeSeeder{Client: mgr.GetClient()}); err != nil {
		setupLog.Error(err, "unable to add workspace gauge seeder")
		os.Exit(1)
	}

	// Free-models refresher (2026-06-23 cold-start optimization, item
	// #1a). Periodically fetches the opencode free-tier model catalog
	// from models.dev and publishes it as a ConfigMap that workspace
	// pods consume to pre-render their relay config before opencode
	// boots. NeedLeaderElection() returns true so only one replica
	// fetches at a time.
	if enableFreeModelsRefresher {
		fmNamespace := os.Getenv("POD_NAMESPACE")
		if fmNamespace == "" {
			fmNamespace = "llmsafespaces"
		}
		if err := mgr.Add(&freemodels.Refresher{
			Client:    mgr.GetClient(),
			Namespace: fmNamespace,
			Interval:  freeModelsRefreshInterval,
			Fetcher:   &freemodels.Fetcher{URL: freeModelsAPIURL},
		}); err != nil {
			setupLog.Error(err, "unable to add free-models refresher")
			os.Exit(1)
		}
		setupLog.Info("free-models refresher enabled",
			"namespace", fmNamespace,
			"interval", freeModelsRefreshInterval,
			"url", freeModelsAPIURL)
	}

	// Add health check endpoints
	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		setupLog.Error(err, "unable to set up health check")
		os.Exit(1)
	}
	if err := mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		setupLog.Error(err, "unable to set up ready check")
		os.Exit(1)
	}

	setupLog.Info("starting manager")
	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		setupLog.Error(err, "problem running manager")
		os.Exit(1)
	}
}

type workspaceGaugeSeeder struct {
	client.Client
}

func (s *workspaceGaugeSeeder) Start(ctx context.Context) error {
	logger := ctrl.Log.WithName("workspace-gauge-seeder")
	wsList := &v1.WorkspaceList{}
	if err := s.List(ctx, wsList); err != nil {
		return fmt.Errorf("seed workspaces running gauge: %w", err)
	}
	counts := map[[2]string]int{}
	inRecovery := 0
	inSafeMode := 0
	for _, ws := range wsList.Items {
		if ws.Status.Phase == v1.WorkspacePhaseActive {
			runtime := ws.Spec.Runtime
			secLevel := string(ws.Spec.SecurityLevel)
			counts[[2]string{runtime, secLevel}]++
		}
		// US-24.11: seed recovery + safe-mode gauges so they survive controller restart.
		if ws.Status.ConsecutiveFailures > 0 && ws.Status.Phase != v1.WorkspacePhaseActive {
			inRecovery++
		}
		if ws.Status.SafeMode {
			inSafeMode++
		}
	}
	for k, n := range counts {
		metrics.SeedWorkspacesRunning(k[0], k[1], n)
		logger.Info("seeded WorkspacesRunning gauge", "runtime", k[0], "security_level", k[1], "count", n)
	}
	metrics.WorkspacesInRecovery.Set(float64(inRecovery))
	metrics.WorkspaceSafeModeActive.Set(float64(inSafeMode))
	if inRecovery > 0 || inSafeMode > 0 {
		logger.Info("seeded recovery gauges", "in_recovery", inRecovery, "in_safe_mode", inSafeMode)
	}
	return nil
}
