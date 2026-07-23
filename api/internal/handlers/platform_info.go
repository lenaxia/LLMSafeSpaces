// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package handlers

import (
	"context"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/lenaxia/llmsafespaces/pkg/version"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// settingGetter is the minimal slice of InstanceService the handler needs
// (reading the default base-image setting). Kept as an interface so the
// handler is unit-testable without a live database.
type settingGetter interface {
	GetString(ctx context.Context, key string) (string, error)
}

// PlatformInfoResponse reports the running version of each platform
// component, as discovered from the deployed image tags / settings.
type PlatformInfoResponse struct {
	API         string `json:"api"`
	Controller  string `json:"controller"`
	Frontend    string `json:"frontend"`
	RelayRouter string `json:"relayRouter"`
	BaseRuntime string `json:"baseRuntime"`
}

// PlatformInfoHandler aggregates platform component versions for the admin
// "Versions" display. Versions are read from the deployed Deployment image
// tags (the most truthful "what is running" signal) rather than from each
// component self-reporting, so no per-component code changes are required.
type PlatformInfoHandler struct {
	clientset       kubernetes.Interface
	namespace       string
	baseImageGetter settingGetter
}

// NewPlatformInfoHandler creates a handler. baseImageGetter reads the
// workspace.defaultImage instance setting (typically *settings.InstanceService).
func NewPlatformInfoHandler(clientset kubernetes.Interface, namespace string, baseImageGetter settingGetter) *PlatformInfoHandler {
	return &PlatformInfoHandler{
		clientset:       clientset,
		namespace:       namespace,
		baseImageGetter: baseImageGetter,
	}
}

// parseImageTag extracts the version tag (or digest) from a container image
// reference. It correctly handles registries with ports (e.g.
// registry:5000/repo:tag) by only treating a ':' after the final '/' as the
// tag separator. Returns "" when no tag is present.
func parseImageTag(image string) string {
	if image == "" {
		return ""
	}
	// Digest pin: repo@sha256:... — return everything after the '@'.
	if at := strings.LastIndex(image, "@"); at >= 0 {
		return image[at+1:]
	}
	// Isolate the repo:name fragment after the final '/' so a registry port
	// (registry:5000/...) is not mistaken for the tag separator.
	lastSlash := strings.LastIndex(image, "/")
	repoAndTag := image[lastSlash+1:]
	colon := strings.LastIndex(repoAndTag, ":")
	if colon < 0 {
		return ""
	}
	return repoAndTag[colon+1:]
}

// GetPlatformInfo handles GET /api/v1/admin/platform-info.
func (h *PlatformInfoHandler) GetPlatformInfo(c *gin.Context) {
	ctx := c.Request.Context()
	resp := PlatformInfoResponse{API: version.Version}

	if h.clientset != nil {
		deps, err := h.clientset.AppsV1().Deployments(h.namespace).List(ctx, metav1.ListOptions{
			LabelSelector: "app.kubernetes.io/name=llmsafespaces",
		})
		if err == nil {
			for _, d := range deps.Items {
				containers := d.Spec.Template.Spec.Containers
				if len(containers) == 0 {
					continue
				}
				tag := parseImageTag(containers[0].Image)
				switch d.Labels["app.kubernetes.io/component"] {
				case "controller":
					resp.Controller = tag
				case "frontend":
					resp.Frontend = tag
				case "relay-router":
					resp.RelayRouter = tag
				}
			}
		}
	}

	if h.baseImageGetter != nil {
		if baseImage, err := h.baseImageGetter.GetString(ctx, "workspace.defaultImage"); err == nil {
			resp.BaseRuntime = parseImageTag(baseImage)
		}
	}

	c.JSON(http.StatusOK, resp)
}
