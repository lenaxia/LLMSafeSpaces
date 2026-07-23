// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

func TestParseImageTag(t *testing.T) {
	cases := []struct {
		name  string
		image string
		want  string
	}{
		{"semver tag", "ghcr.io/lenaxia/llmsafespaces/api:0.4.5", "0.4.5"},
		{"registry with port", "registry.internal:5000/sandboxes/base:0.4.5", "0.4.5"},
		{"no slash, has tag", "nginx:1.27-alpine", "1.27-alpine"},
		{"no tag", "ghcr.io/lenaxia/llmsafespaces/api", ""},
		{"latest tag", "ghcr.io/repo/app:latest", "latest"},
		{"digest pin", "ghcr.io/repo/api@sha256:abc123def", "sha256:abc123def"},
		{"empty", "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, parseImageTag(tc.image))
		})
	}
}

// fakeSettingGetter is a test double for the base-image setting source.
type fakeSettingGetter struct {
	image string
	err   error
}

func (f *fakeSettingGetter) GetString(_ context.Context, _ string) (string, error) {
	return f.image, f.err
}

func deploymentWithImage(name, component, image string) *appsv1.Deployment {
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: "test-ns",
			Labels: map[string]string{
				"app.kubernetes.io/name":      "llmsafespaces",
				"app.kubernetes.io/component": component,
			},
		},
		Spec: appsv1.DeploymentSpec{
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{Image: image}},
				},
			},
		},
	}
}

func TestGetPlatformInfo(t *testing.T) {
	gin.SetMode(gin.TestMode)

	t.Run("returns versions from labeled deployments + setting", func(t *testing.T) {
		clientset := fake.NewSimpleClientset(
			deploymentWithImage("release-controller", "controller", "ghcr.io/lenaxia/llmsafespaces/controller:0.4.5"),
			deploymentWithImage("release-frontend", "frontend", "ghcr.io/lenaxia/llmsafespaces/frontend:0.4.5"),
			deploymentWithImage("relay-router", "relay-router", "ghcr.io/lenaxia/llmsafespaces/relay-router:0.4.5"),
			deploymentWithImage("release-api", "api", "ghcr.io/lenaxia/llmsafespaces/api:0.4.5"),
		)
		h := NewPlatformInfoHandler(clientset, "test-ns", &fakeSettingGetter{image: "ghcr.io/lenaxia/llmsafespaces/base:0.4.5"})

		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Request = httptest.NewRequest(http.MethodGet, "/admin/platform-info", nil)

		h.GetPlatformInfo(c)

		require.Equal(t, http.StatusOK, w.Code)
		var resp PlatformInfoResponse
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
		assert.Equal(t, "0.4.5", resp.Controller)
		assert.Equal(t, "0.4.5", resp.Frontend)
		assert.Equal(t, "0.4.5", resp.RelayRouter)
		assert.Equal(t, "0.4.5", resp.BaseRuntime)
	})

	t.Run("missing components are empty, not error", func(t *testing.T) {
		// Only controller deployed; frontend/relay-router absent.
		clientset := fake.NewSimpleClientset(
			deploymentWithImage("release-controller", "controller", "ghcr.io/x/controller:0.3.0"),
		)
		h := NewPlatformInfoHandler(clientset, "test-ns", &fakeSettingGetter{image: ""})

		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Request = httptest.NewRequest(http.MethodGet, "/admin/platform-info", nil)

		h.GetPlatformInfo(c)

		require.Equal(t, http.StatusOK, w.Code)
		var resp PlatformInfoResponse
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
		assert.Equal(t, "0.3.0", resp.Controller)
		assert.Empty(t, resp.Frontend)
		assert.Empty(t, resp.RelayRouter)
	})

	t.Run("unrelated deployments are ignored", func(t *testing.T) {
		// A deployment without the llmsafespaces name label must be skipped.
		other := deploymentWithImage("other", "api", "other:9.9.9")
		other.Labels["app.kubernetes.io/name"] = "something-else"
		clientset := fake.NewSimpleClientset(other)
		h := NewPlatformInfoHandler(clientset, "test-ns", &fakeSettingGetter{image: ""})

		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Request = httptest.NewRequest(http.MethodGet, "/admin/platform-info", nil)

		h.GetPlatformInfo(c)
		require.Equal(t, http.StatusOK, w.Code)
	})
}

// Integration test: exercises the full route registration → handler → JSON
// serialization wiring through the real gin engine (not a direct handler
// call), proving the route binding and response shape are correct end-to-end.
func TestPlatformInfoRouteIntegration(t *testing.T) {
	gin.SetMode(gin.TestMode)
	clientset := fake.NewSimpleClientset(
		deploymentWithImage("release-controller", "controller", "ghcr.io/x/controller:0.4.5"),
		deploymentWithImage("release-frontend", "frontend", "ghcr.io/x/frontend:0.4.5"),
	)
	h := NewPlatformInfoHandler(clientset, "test-ns", &fakeSettingGetter{image: "ghcr.io/x/base:0.4.5"})

	r := gin.New()
	g := r.Group("/api/v1/admin/platform-info")
	g.GET("", h.GetPlatformInfo)

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/admin/platform-info", nil)
	r.ServeHTTP(w, req)

	require.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, "application/json; charset=utf-8", w.Header().Get("Content-Type"))
	var resp PlatformInfoResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.Equal(t, "0.4.5", resp.Controller)
	assert.Equal(t, "0.4.5", resp.Frontend)
	assert.Equal(t, "0.4.5", resp.BaseRuntime)
}

func TestGetPlatformInfo_ErrorPaths(t *testing.T) {
	gin.SetMode(gin.TestMode)

	t.Run("K8s list failure returns 200 with empty component versions", func(t *testing.T) {
		clientset := fake.NewSimpleClientset()
		clientset.PrependReactor("list", "deployments", func(k8stesting.Action) (bool, runtime.Object, error) {
			return true, nil, errors.New("k8s api down")
		})
		h := NewPlatformInfoHandler(clientset, "test-ns", &fakeSettingGetter{image: "x/base:0.4.5"})

		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Request = httptest.NewRequest(http.MethodGet, "/admin/platform-info", nil)

		h.GetPlatformInfo(c)

		// Never 500 — the endpoint degrades to partial data so the admin still
		// sees the API version + base runtime.
		require.Equal(t, http.StatusOK, w.Code)
		var resp PlatformInfoResponse
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
		assert.Empty(t, resp.Controller)
		assert.Empty(t, resp.Frontend)
		assert.Equal(t, "0.4.5", resp.BaseRuntime)
	})

	t.Run("settings failure leaves baseRuntime empty, not error", func(t *testing.T) {
		clientset := fake.NewSimpleClientset(
			deploymentWithImage("c", "controller", "x/controller:0.4.5"),
		)
		h := NewPlatformInfoHandler(clientset, "test-ns", &fakeSettingGetter{err: errors.New("db down")})

		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Request = httptest.NewRequest(http.MethodGet, "/admin/platform-info", nil)

		h.GetPlatformInfo(c)

		require.Equal(t, http.StatusOK, w.Code)
		var resp PlatformInfoResponse
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
		assert.Equal(t, "0.4.5", resp.Controller)
		assert.Empty(t, resp.BaseRuntime)
	})
}
