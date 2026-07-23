// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/lenaxia/llmsafespaces/api/internal/handlers"
	apilogger "github.com/lenaxia/llmsafespaces/api/internal/logger"
	imocks "github.com/lenaxia/llmsafespaces/api/internal/mocks"
)

// testSettingGetter satisfies the unexported handlers.settingGetter interface
// (structural typing) so the router test can construct the handler without a
// live database.
type testSettingGetter struct{ image string }

func (t *testSettingGetter) GetString(_ context.Context, _ string) (string, error) {
	return t.image, nil
}

func deploymentForRouter(component, image string) *appsv1.Deployment {
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-" + component,
			Namespace: "test-ns",
			Labels: map[string]string{
				"app.kubernetes.io/name":      "llmsafespaces",
				"app.kubernetes.io/component": component,
			},
		},
		Spec: appsv1.DeploymentSpec{
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{Containers: []corev1.Container{{Image: image}}},
			},
		},
	}
}

// newPlatformInfoRouter builds a real NewRouter with the PlatformInfoHandler
// wired and an auth middleware that stamps userRole=role. This proves the
// /api/v1/admin/platform-info route is registered through the live wiring
// path and is protected by AdminGuard.
func newPlatformInfoRouter(t *testing.T, role string) *gin.Engine {
	t.Helper()
	gin.SetMode(gin.TestMode)
	log, err := apilogger.New(false, "error", "json")
	require.NoError(t, err)

	auth := &imocks.MockAuthMiddlewareService{}
	met := &imocks.MockMetricsService{}
	met.On("RecordRequest", mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything).Maybe()
	met.On("IncrementActiveConnections", mock.Anything, mock.Anything).Maybe()
	met.On("DecrementActiveConnections", mock.Anything, mock.Anything).Maybe()

	auth.On("AuthMiddleware").Return(gin.HandlerFunc(func(c *gin.Context) {
		c.Set("userID", "test-user")
		c.Set("userRole", role)
		c.Next()
	}))
	auth.On("GetUserID", mock.Anything).Return("test-user")

	clientset := fake.NewSimpleClientset(
		deploymentForRouter("controller", "ghcr.io/x/controller:0.4.5"),
	)
	svc := &mockServices{auth: auth, metrics: met}
	cfg := RouterConfig{
		Debug:               false,
		PlatformInfoHandler: handlers.NewPlatformInfoHandler(clientset, "test-ns", &testSettingGetter{image: "ghcr.io/x/base:0.4.5"}),
	}
	return NewRouter(svc, log, nil, cfg)
}

func TestPlatformInfoRoute_AdminGuard_BlocksNonAdmin(t *testing.T) {
	router := newPlatformInfoRouter(t, "user")

	req := httptest.NewRequest(http.MethodGet, "/api/v1/admin/platform-info", nil)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusNotFound, rec.Code, "non-admin must get 404, body=%s", rec.Body.String())
}

func TestPlatformInfoRoute_AdminGuard_AllowsAdmin(t *testing.T) {
	router := newPlatformInfoRouter(t, "admin")

	req := httptest.NewRequest(http.MethodGet, "/api/v1/admin/platform-info", nil)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code, "admin must get 200, body=%s", rec.Body.String())
	assert.Contains(t, rec.Body.String(), "0.4.5")
}
