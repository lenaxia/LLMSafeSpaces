// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package handlers

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	v1 "github.com/lenaxia/llmsafespaces/pkg/apis/llmsafespaces/v1"
	"github.com/lenaxia/llmsafespaces/pkg/interfaces"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// RelayAdminHandler serves the relay admin setup wizard and status dashboard endpoints.
// The relay fleet supports AWS (paid primary), OCI (free secondary), and GCP (optional)
// providers — matching the InferenceRelay CRD enum `aws;oci;gcp`.
type RelayAdminHandler struct {
	clientset       kubernetes.Interface
	llmClient       interfaces.LLMSafespacesV1Interface
	namespace       string
	routerNamespace string
	routerSvcURL    string
	httpClient      *http.Client
	logger          interfaces.LoggerInterface
}

// NewRelayAdminHandler creates a new relay admin handler.
// namespace is the workspace namespace (for Secrets, CRDs).
// routerNamespace is the namespace where the relay-router Deployment lives.
func NewRelayAdminHandler(clientset kubernetes.Interface, llmClient interfaces.LLMSafespacesV1Interface, namespace, routerNamespace, routerSvcURL string) *RelayAdminHandler {
	return &RelayAdminHandler{
		clientset:       clientset,
		llmClient:       llmClient,
		namespace:       namespace,
		routerNamespace: routerNamespace,
		routerSvcURL:    routerSvcURL,
		httpClient:      &http.Client{Timeout: 5 * time.Second},
	}
}

// SetHTTPClient overrides the HTTP client (for testing).
func (h *RelayAdminHandler) SetHTTPClient(client *http.Client) {
	if client != nil {
		h.httpClient = client
	}
}

// SetLogger injects a logger. When set, scrapeRouterMetrics emits Warn-level
// lines on its three error paths (request build, HTTP transport, response
// read) and on non-2xx responses (#475). Without this, dashboard
// misconfigurations surface only as silently-zero counters — operators see
// a "working" admin relay status with no signal pointing at the root cause.
// Production wiring always injects a logger; nil is tolerated (defense in
// depth — a nil logger MUST NOT crash the handler).
func (h *RelayAdminHandler) SetLogger(l interfaces.LoggerInterface) { h.logger = l }

// ─── US-43.2: Setup checklist ───────────────────────────────────────────────

type setupResponse struct {
	Deployed       bool `json:"deployed"`
	RouterDeployed bool `json:"routerDeployed"`
	CRDInstalled   bool `json:"crdInstalled"`
	AWSConfigured  bool `json:"awsConfigured"`
	OCIConfigured  bool `json:"ociConfigured"`
	GCPConfigured  bool `json:"gcpConfigured"`
}

// GetSetup returns the prerequisite checklist state for the relay setup wizard.
//
// The checklist is network-stack agnostic: it verifies LLMSafeSpaces-owned
// prerequisites (relay-router Deployment, InferenceRelay CRD, provider
// credentials) but does NOT probe the network path between the router and
// relay VMs. Post-WG-removal (worklog 0442) the router dials relay VMs by
// public IP over HTTP with per-VM token auth; reachability is verified
// downstream via instance health in GetStatus.
func (h *RelayAdminHandler) GetSetup(c *gin.Context) {
	ctx := c.Request.Context()
	resp := setupResponse{}

	if err := h.checkRouter(ctx, &resp); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "router check failed: " + err.Error()})
		return
	}
	if err := h.checkCRD(ctx, &resp); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "CRD check failed: " + err.Error()})
		return
	}
	h.checkAWSSecret(ctx, &resp)
	h.checkOCISecret(ctx, &resp)
	h.checkGCPSecret(ctx, &resp)
	resp.Deployed = h.isFleetDeployed(ctx)

	c.JSON(http.StatusOK, resp)
}

func (h *RelayAdminHandler) checkRouter(ctx context.Context, resp *setupResponse) error {
	_, err := h.clientset.AppsV1().Deployments(h.routerNamespace).Get(ctx, "relay-router", metav1.GetOptions{})
	if err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		return err
	}
	resp.RouterDeployed = true
	return nil
}

func (h *RelayAdminHandler) checkCRD(ctx context.Context, resp *setupResponse) error {
	resources, err := h.clientset.Discovery().ServerResourcesForGroupVersion("llmsafespaces.dev/v1")
	if err != nil {
		if apierrors.IsNotFound(err) || strings.Contains(err.Error(), "empty response") {
			return nil
		}
		return err
	}
	for _, r := range resources.APIResources {
		if r.Name == "inferencerelays" {
			resp.CRDInstalled = true
			break
		}
	}
	return nil
}

func (h *RelayAdminHandler) checkAWSSecret(ctx context.Context, resp *setupResponse) {
	_, err := h.clientset.CoreV1().Secrets(h.namespace).Get(ctx, "aws-relay-irwa", metav1.GetOptions{})
	resp.AWSConfigured = err == nil
}

func (h *RelayAdminHandler) checkOCISecret(ctx context.Context, resp *setupResponse) {
	_, err := h.clientset.CoreV1().Secrets(h.namespace).Get(ctx, "oci-credentials", metav1.GetOptions{})
	resp.OCIConfigured = err == nil
}

func (h *RelayAdminHandler) checkGCPSecret(ctx context.Context, resp *setupResponse) {
	_, err := h.clientset.CoreV1().Secrets(h.namespace).Get(ctx, "gcp-credentials", metav1.GetOptions{})
	resp.GCPConfigured = err == nil
}

func (h *RelayAdminHandler) isFleetDeployed(ctx context.Context) bool {
	relays, err := h.llmClient.InferenceRelays().List(ctx, metav1.ListOptions{Limit: 1})
	if err != nil {
		return false
	}
	return len(relays.Items) > 0
}

// ─── US-43.1: Status dashboard ──────────────────────────────────────────────

type instanceStatus struct {
	ID                 string          `json:"id"`
	Provider           string          `json:"provider"`
	Region             string          `json:"region"`
	Shape              string          `json:"shape"`
	PublicIP           string          `json:"publicIP"`
	State              string          `json:"state"`
	Healthy            bool            `json:"healthy"`
	Metrics            instanceMetrics `json:"metrics"`
	Cost               instanceCost    `json:"cost"`
	LastProvisionError string          `json:"lastProvisionError,omitempty"`
}

type instanceMetrics struct {
	RequestsToday    int64 `json:"requestsToday"`
	Requests429Today int64 `json:"requests429Today"`
	TotalRequests    int64 `json:"totalRequests"`
	EgressBytes      int64 `json:"egressBytes"`
	EgressLimitBytes int64 `json:"egressLimitBytes"`
	ActiveStreams    int64 `json:"activeStreams"`
}

type instanceCost struct {
	MonthlyEstimate int64 `json:"monthlyEstimate"`
	SpentThisMonth  int64 `json:"spentThisMonth"`
}

type conditionInfo struct {
	Type    string `json:"type"`
	Status  string `json:"status"`
	Reason  string `json:"reason"`
	Message string `json:"message"`
}

type alertInfo struct {
	Name       string `json:"name"`
	Expression string `json:"expression"`
	Firing     bool   `json:"firing"`
}

type eventInfo struct {
	Timestamp string `json:"timestamp"`
	Type      string `json:"type"`
	Message   string `json:"message"`
	Severity  string `json:"severity"`
}

type statusResponse struct {
	Deployed        bool             `json:"deployed"`
	Overall         string           `json:"overall"`
	HealthyReplicas int              `json:"healthyReplicas"`
	TotalReplicas   int              `json:"totalReplicas"`
	FallbackActive  bool             `json:"fallbackActive"`
	ActiveStreams   int64            `json:"activeStreams"`
	Instances       []instanceStatus `json:"instances"`
	Conditions      []conditionInfo  `json:"conditions"`
	RecentEvents    []eventInfo      `json:"recentEvents"`
	Alerts          []alertInfo      `json:"alerts"`
}

// GetStatus returns the full fleet status by aggregating CR status + router metrics.
func (h *RelayAdminHandler) GetStatus(c *gin.Context) {
	ctx := c.Request.Context()

	relays, err := h.llmClient.InferenceRelays().List(ctx, metav1.ListOptions{})
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to list InferenceRelays: " + err.Error()})
		return
	}

	if len(relays.Items) == 0 {
		c.JSON(http.StatusOK, statusResponse{Deployed: false})
		return
	}

	relay := relays.Items[0]
	routerMetrics := h.scrapeRouterMetrics(ctx)

	// Build a provider→shape lookup from spec for the status response
	shapeByProvider := make(map[string]string)
	for _, p := range relay.Spec.Providers {
		shapeByProvider[p.Provider] = p.Shape
	}

	resp := statusResponse{
		Deployed:        true,
		Overall:         "healthy",
		HealthyReplicas: relay.Status.HealthyReplicas,
		TotalReplicas:   len(relay.Status.Instances),
		ActiveStreams:   routerMetrics.activeStreams,
		Instances:       make([]instanceStatus, 0, len(relay.Status.Instances)),
	}

	for _, cond := range relay.Status.Conditions {
		resp.Conditions = append(resp.Conditions, conditionInfo{
			Type:    string(cond.Type),
			Status:  string(cond.Status),
			Reason:  cond.Reason,
			Message: cond.Message,
		})
		if cond.Type == string(v1.InferenceRelayConditionFallbackActive) && cond.Status == metav1.ConditionTrue {
			resp.FallbackActive = true
		}
		if cond.Type == string(v1.InferenceRelayConditionDegraded) && cond.Status == metav1.ConditionTrue {
			resp.Overall = "degraded"
		}
	}

	if relay.Status.HealthyReplicas == 0 && len(relay.Status.Instances) > 0 {
		resp.Overall = "unhealthy"
	} else if relay.Status.HealthyReplicas < len(relay.Status.Instances) && resp.Overall != "unhealthy" {
		resp.Overall = "degraded"
	}

	for _, inst := range relay.Status.Instances {
		is := instanceStatus{
			ID:       inst.ID,
			Provider: inst.Provider,
			Region:   inst.Region,
			Shape:    shapeByProvider[inst.Provider],
			PublicIP: inst.PublicIP,
			State:    inst.State,
			Healthy:  inst.Healthy,
			Metrics: instanceMetrics{
				RequestsToday:    routerMetrics.requestsByRelay[inst.ID],
				Requests429Today: routerMetrics.requests429ByRelay[inst.ID],
				TotalRequests:    int64(inst.TotalRequests),
				EgressBytes:      inst.EgressBytes,
				EgressLimitBytes: egressLimitForProvider(inst.Provider),
				ActiveStreams:    routerMetrics.streamsByRelay[inst.ID],
			},
			Cost:               computeCost(inst.Provider, inst.Healthy),
			LastProvisionError: inst.LastProvisionError,
		}
		resp.Instances = append(resp.Instances, is)
	}

	resp.Alerts = buildAlerts(relay.Status.HealthyReplicas, len(relay.Status.Instances))

	resp.RecentEvents = []eventInfo{}
	if relay.Status.LastRotation != nil {
		resp.RecentEvents = append(resp.RecentEvents, eventInfo{
			Timestamp: relay.Status.LastRotation.Format(time.RFC3339),
			Type:      "Rotated",
			Message:   "Last rotation of relay fleet",
			Severity:  "info",
		})
	}

	c.JSON(http.StatusOK, resp)
}

// ─── US-43.5: OCI credentials ───────────────────────────────────────────────

type ociCredsRequest struct {
	Tenancy     string `json:"tenancy" binding:"required"`
	User        string `json:"user" binding:"required"`
	Fingerprint string `json:"fingerprint" binding:"required"`
	Key         string `json:"key" binding:"required"`
	Region      string `json:"region" binding:"required"`
}

const maxRelayBodyBytes = 1 << 20 // 1 MiB max for relay credential request bodies

// SaveOCICreds saves OCI credentials to a K8s Secret.
func (h *RelayAdminHandler) SaveOCICreds(c *gin.Context) {
	ctx := c.Request.Context()

	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, maxRelayBodyBytes)
	var req ociCredsRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "tenancy, user, fingerprint, key, and region are required"})
		return
	}

	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "oci-credentials",
			Namespace: h.namespace,
		},
		Data: map[string][]byte{
			"tenancy":     []byte(req.Tenancy),
			"user":        []byte(req.User),
			"fingerprint": []byte(req.Fingerprint),
			"key":         []byte(req.Key),
			"region":      []byte(req.Region),
		},
	}

	if err := h.upsertSecret(ctx, secret); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to save OCI credentials: " + err.Error()})
		return
	}

	c.JSON(http.StatusOK, gin.H{"configured": true})
}

// ─── GCP credentials ────────────────────────────────────────────────────────

type gcpCredsRequest struct {
	ServiceAccountJSON string `json:"serviceAccountJson" binding:"required"`
}

// SaveGCPCreds saves GCP service account JSON to a K8s Secret.
func (h *RelayAdminHandler) SaveGCPCreds(c *gin.Context) {
	ctx := c.Request.Context()

	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, maxRelayBodyBytes)
	var req gcpCredsRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "serviceAccountJson is required"})
		return
	}

	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "gcp-credentials",
			Namespace: h.namespace,
		},
		Data: map[string][]byte{
			"service-account-json": []byte(req.ServiceAccountJSON),
		},
	}

	if err := h.upsertSecret(ctx, secret); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to save GCP credentials: " + err.Error()})
		return
	}

	c.JSON(http.StatusOK, gin.H{"configured": true})
}

// ─── AWS credentials ────────────────────────────────────────────────────────

type awsCredsRequest struct {
	AccessKeyID     string `json:"accessKeyId" binding:"required"`
	SecretAccessKey string `json:"secretAccessKey" binding:"required"`
	Region          string `json:"region" binding:"required"`
}

// SaveAWSCreds saves AWS IAM access key credentials for EC2 provisioning.
func (h *RelayAdminHandler) SaveAWSCreds(c *gin.Context) {
	ctx := c.Request.Context()

	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, maxRelayBodyBytes)
	var req awsCredsRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "accessKeyId, secretAccessKey, and region are required"})
		return
	}

	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "aws-relay-irwa",
			Namespace: h.namespace,
		},
		Data: map[string][]byte{
			"accessKeyId":     []byte(req.AccessKeyID),
			"secretAccessKey": []byte(req.SecretAccessKey),
			"region":          []byte(req.Region),
		},
	}

	if err := h.upsertSecret(ctx, secret); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to save AWS credentials: " + err.Error()})
		return
	}

	c.JSON(http.StatusOK, gin.H{"configured": true})
}

// ─── US-43.6: Deploy relay fleet ────────────────────────────────────────────

type deployRequest struct {
	UpstreamURL string   `json:"upstreamURL,omitempty"`
	Providers   []string `json:"providers" binding:"required"`
}

// Deploy creates or updates the InferenceRelay CR.
// Valid providers are "aws", "oci", and "gcp" — matching the CRD enum validation.
func (h *RelayAdminHandler) Deploy(c *gin.Context) {
	ctx := c.Request.Context()

	var req deployRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "providers is required"})
		return
	}

	if req.UpstreamURL == "" {
		req.UpstreamURL = "https://opencode.ai/zen/v1"
	}

	if len(req.Providers) == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "at least one provider is required"})
		return
	}

	providers := make([]v1.RelayProviderSpec, 0, len(req.Providers))
	for _, p := range req.Providers {
		switch p {
		case "aws":
			providers = append(providers, v1.RelayProviderSpec{
				Provider:       "aws",
				Region:         "us-east-1",
				CredentialsRef: corev1.LocalObjectReference{Name: "aws-relay-irwa"},
			})
		case "oci":
			providers = append(providers, v1.RelayProviderSpec{
				Provider:       "oci",
				Region:         "us-ashburn-1",
				CredentialsRef: corev1.LocalObjectReference{Name: "oci-credentials"},
			})
		case "gcp":
			providers = append(providers, v1.RelayProviderSpec{
				Provider:       "gcp",
				Region:         "us-west1",
				CredentialsRef: corev1.LocalObjectReference{Name: "gcp-credentials"},
			})
		default:
			c.JSON(http.StatusBadRequest, gin.H{"error": fmt.Sprintf("unknown provider: %s (valid: aws, oci, gcp)", p)})
			return
		}
	}

	relay := &v1.InferenceRelay{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "llmsafespaces.dev/v1",
			Kind:       "InferenceRelay",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name: "relay-fleet",
		},
		Spec: v1.InferenceRelaySpec{
			UpstreamURL: req.UpstreamURL,
			Providers:   providers,
		},
	}

	existing, err := h.llmClient.InferenceRelays().Get(ctx, "relay-fleet", metav1.GetOptions{})
	if err != nil && !apierrors.IsNotFound(err) {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to check existing relay: " + err.Error()})
		return
	}
	// Gate on apierrors.IsNotFound(err), not `existing != nil`. The typed
	// client at pkg/kubernetes/client_crds.go pre-allocates an empty struct
	// and returns it alongside the NotFound error, so a nil-pointer check is
	// always false and we would always fall into the Update branch — which
	// then fails with NotFound on a fresh cluster (worklog 0385).
	if apierrors.IsNotFound(err) {
		_, err = h.llmClient.InferenceRelays().Create(ctx, relay)
	} else {
		relay.ResourceVersion = existing.ResourceVersion
		_, err = h.llmClient.InferenceRelays().Update(ctx, relay)
	}

	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to deploy relay fleet: " + err.Error()})
		return
	}

	c.JSON(http.StatusOK, gin.H{"deployed": true})
}

// ─── US-43.7: Rotate + Pause ────────────────────────────────────────────────

// Rotate triggers manual rotation of a specific relay instance.
func (h *RelayAdminHandler) Rotate(c *gin.Context) {
	ctx := c.Request.Context()
	relayID := c.Param("id")

	if relayID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "relay id is required"})
		return
	}

	existing, err := h.llmClient.InferenceRelays().Get(ctx, "relay-fleet", metav1.GetOptions{})
	if err != nil {
		if apierrors.IsNotFound(err) {
			c.JSON(http.StatusNotFound, gin.H{"error": "relay fleet not found"})
		} else {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to get relay fleet: " + err.Error()})
		}
		return
	}

	applyAnnotation(existing, "relay.llmsafespaces.dev/rotate", relayID)
	_, err = h.llmClient.InferenceRelays().Update(ctx, existing)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to trigger rotation: " + err.Error()})
		return
	}

	c.JSON(http.StatusOK, gin.H{"rotating": relayID})
}

// Pause pauses the relay fleet — stops provisioning/replacing VMs.
func (h *RelayAdminHandler) Pause(c *gin.Context) {
	ctx := c.Request.Context()

	existing, err := h.llmClient.InferenceRelays().Get(ctx, "relay-fleet", metav1.GetOptions{})
	if err != nil {
		if apierrors.IsNotFound(err) {
			c.JSON(http.StatusNotFound, gin.H{"error": "relay fleet not found"})
		} else {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to get relay fleet: " + err.Error()})
		}
		return
	}

	applyAnnotation(existing, "relay.llmsafespaces.dev/paused", "true")
	_, err = h.llmClient.InferenceRelays().Update(ctx, existing)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to pause relay fleet: " + err.Error()})
		return
	}

	c.JSON(http.StatusOK, gin.H{"paused": true})
}

// Resume removes the pause annotation — controller resumes provisioning/replacing VMs.
func (h *RelayAdminHandler) Resume(c *gin.Context) {
	ctx := c.Request.Context()

	existing, err := h.llmClient.InferenceRelays().Get(ctx, "relay-fleet", metav1.GetOptions{})
	if err != nil {
		if apierrors.IsNotFound(err) {
			c.JSON(http.StatusNotFound, gin.H{"error": "relay fleet not found"})
		} else {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to get relay fleet: " + err.Error()})
		}
		return
	}

	if existing.Annotations != nil {
		delete(existing.Annotations, "relay.llmsafespaces.dev/paused")
	}

	_, err = h.llmClient.InferenceRelays().Update(ctx, existing)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to resume relay fleet: " + err.Error()})
		return
	}

	c.JSON(http.StatusOK, gin.H{"paused": false})
}

// ─── Helpers ────────────────────────────────────────────────────────────────

func applyAnnotation(relay *v1.InferenceRelay, key, value string) {
	if relay.Annotations == nil {
		relay.Annotations = map[string]string{}
	}
	relay.Annotations[key] = value
}

func (h *RelayAdminHandler) upsertSecret(ctx context.Context, desired *corev1.Secret) error {
	existing, err := h.clientset.CoreV1().Secrets(h.namespace).Get(ctx, desired.Name, metav1.GetOptions{})
	if err != nil {
		if !apierrors.IsNotFound(err) {
			return err
		}
		_, err = h.clientset.CoreV1().Secrets(h.namespace).Create(ctx, desired, metav1.CreateOptions{})
		return err
	}
	desired.ResourceVersion = existing.ResourceVersion
	_, err = h.clientset.CoreV1().Secrets(h.namespace).Update(ctx, desired, metav1.UpdateOptions{})
	return err
}

type routerMetricsData struct {
	activeStreams      int64
	requestsByRelay    map[string]int64
	requests429ByRelay map[string]int64
	streamsByRelay     map[string]int64
}

func (h *RelayAdminHandler) scrapeRouterMetrics(ctx context.Context) routerMetricsData {
	data := routerMetricsData{
		requestsByRelay:    make(map[string]int64),
		requests429ByRelay: make(map[string]int64),
		streamsByRelay:     make(map[string]int64),
	}
	url := strings.TrimRight(h.routerSvcURL, "/") + "/metrics"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		// Unreachable in practice (the only failure mode is an invalid
		// method or URL); logged for completeness so the silent-zero
		// failure mode of #475 is closed on every path.
		if h.logger != nil {
			h.logger.Warn("relay router /metrics request build failed",
				"url", url, "error", err.Error())
		}
		return data
	}
	resp, err := h.httpClient.Do(req)
	if err != nil {
		// The most common failure mode in production: NetworkPolicy
		// missing the API ingress rule (#466), DNS resolving slowly,
		// router restarting, or the configured routerSvcURL pointing at
		// the wrong place. Pre-fix this returned silently; operators saw
		// zero counters with no diagnostic.
		if h.logger != nil {
			h.logger.Warn("relay router /metrics scrape failed",
				"url", url, "error", err.Error())
		}
		return data
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		// Non-2xx: the router is up but the scrape path is unhealthy
		// (5xx from router crash, 404 from a wrong path, etc.). Pre-fix
		// resp.StatusCode was never inspected at all.
		if h.logger != nil {
			h.logger.Warn("relay router /metrics returned non-2xx",
				"url", url, "status_code", resp.StatusCode)
		}
		return data
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		if h.logger != nil {
			h.logger.Warn("relay router /metrics response read failed",
				"url", url, "error", err.Error())
		}
		return data
	}
	parseRouterMetrics(string(body), &data)
	return data
}

// parseRouterMetrics extracts per-relay (per-instance) metrics from the
// router's Prometheus output. The router emits the relay ID under the
// "relay" label and the HTTP status under "status" on
// relay_router_requests_total — there is no separate
// relay_router_requests_429_total metric. See worklog 0464 for the bug
// these label names previously did not match.
func parseRouterMetrics(raw string, data *routerMetricsData) {
	lines := strings.Split(raw, "\n")
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		parts := strings.Fields(line)
		if len(parts) < 2 {
			continue
		}
		var val int64
		parseInt(parts[len(parts)-1], &val)

		// fallback_active is a global gauge with no labels.
		// active_streams here is also kept as a global aggregate for the
		// admin UX summary, computed from the per-relay sum below.

		switch {
		case strings.HasPrefix(line, "relay_router_active_streams{"):
			relayID := extractLabel(line, "relay")
			if relayID == "" {
				continue
			}
			data.streamsByRelay[relayID] = val
			data.activeStreams += val
		case strings.HasPrefix(line, "relay_router_requests_total{"):
			relayID := extractLabel(line, "relay")
			if relayID == "" {
				continue
			}
			data.requestsByRelay[relayID] += val
			if extractLabel(line, "status") == "429" {
				data.requests429ByRelay[relayID] += val
			}
		}
	}
}

func extractLabel(line, label string) string {
	prefix := label + "=\""
	start := strings.Index(line, prefix)
	if start < 0 {
		return ""
	}
	start += len(prefix)
	end := strings.IndexByte(line[start:], '"')
	if end < 0 {
		return ""
	}
	return line[start : start+end]
}

func parseInt(s string, out *int64) {
	var n int64
	for _, c := range s {
		if c >= '0' && c <= '9' {
			n = n*10 + int64(c-'0')
		} else {
			break
		}
	}
	*out = n
}

// egressLimitForProvider returns the egress limit in bytes for each provider.
// AWS: 100 GB (Lightsail free tier). OCI: 10 TB (Always Free). GCP: 1 GB.
func egressLimitForProvider(provider string) int64 {
	switch provider {
	case "aws":
		return 100 * 1024 * 1024 * 1024
	case "oci":
		return 10 * 1024 * 1024 * 1024 * 1024
	case "gcp":
		return 1 * 1024 * 1024 * 1024
	default:
		return 1 * 1024 * 1024 * 1024
	}
}

// computeCost returns the monthly cost estimate in cents for a provider.
// AWS is the only paid provider (~$7/month for t4g.micro). OCI and GCP are free.
func computeCost(provider string, healthy bool) instanceCost {
	if !healthy {
		return instanceCost{}
	}
	switch provider {
	case "aws":
		return instanceCost{MonthlyEstimate: 700}
	default:
		return instanceCost{}
	}
}

func buildAlerts(healthy, total int) []alertInfo {
	return []alertInfo{
		{
			Name:       "RelayFleetDegraded",
			Expression: "llmsafespaces_relay_healthy_replicas < 2",
			Firing:     healthy < total,
		},
		{
			Name:       "RelayFleetCritical",
			Expression: "llmsafespaces_relay_healthy_replicas == 0",
			Firing:     healthy == 0 && total > 0,
		},
		{
			Name:       "RelayProvisioningFailed",
			Expression: "llmsafespaces_relay_provisioning_failed == 1",
			Firing:     false,
		},
		{
			Name:       "Relay429RateHigh",
			Expression: "llmsafespaces_relay_429_rate > 0.3",
			Firing:     false,
		},
	}
}
