// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package middleware

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"

	"github.com/lenaxia/llmsafespaces/api/internal/mocks"
	"github.com/lenaxia/llmsafespaces/pkg/types"
)

func TestMeteringMiddleware_AuthenticatedGET_RecordsRead(t *testing.T) {
	gin.SetMode(gin.TestMode)
	meteringSvc := new(mocks.MockMeteringService)
	mw := NewMeteringMiddleware(meteringSvc)

	meteringSvc.On("Record", mock.MatchedBy(func(e types.UsageEvent) bool {
		return e.EventType == "api_call" && e.EventSubtype == "read" && e.ActorID == "user-1"
	})).Once()

	router := gin.New()
	router.Use(func(c *gin.Context) { c.Set("userID", "user-1"); c.Next() })
	router.Use(mw.Handler())
	router.GET("/test", func(c *gin.Context) { c.Status(http.StatusOK) })

	w := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/test", nil)
	router.ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)
	meteringSvc.AssertExpectations(t)
}

func TestMeteringMiddleware_AuthenticatedPOST_RecordsWrite(t *testing.T) {
	gin.SetMode(gin.TestMode)
	meteringSvc := new(mocks.MockMeteringService)
	mw := NewMeteringMiddleware(meteringSvc)

	meteringSvc.On("Record", mock.MatchedBy(func(e types.UsageEvent) bool {
		return e.EventType == "api_call" && e.EventSubtype == "write"
	})).Once()

	router := gin.New()
	router.Use(func(c *gin.Context) { c.Set("userID", "user-1"); c.Next() })
	router.Use(mw.Handler())
	router.POST("/test", func(c *gin.Context) { c.Status(http.StatusCreated) })

	w := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/test", nil)
	router.ServeHTTP(w, req)

	assert.Equal(t, http.StatusCreated, w.Code)
	meteringSvc.AssertExpectations(t)
}

func TestMeteringMiddleware_Unauthenticated_NoRecord(t *testing.T) {
	gin.SetMode(gin.TestMode)
	meteringSvc := new(mocks.MockMeteringService)
	mw := NewMeteringMiddleware(meteringSvc)

	router := gin.New()
	router.Use(mw.Handler())
	router.GET("/test", func(c *gin.Context) { c.Status(http.StatusOK) })

	w := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/test", nil)
	router.ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)
	meteringSvc.AssertNotCalled(t, "Record")
}

func TestMeteringMiddleware_SkipsHealthEndpoints(t *testing.T) {
	gin.SetMode(gin.TestMode)
	meteringSvc := new(mocks.MockMeteringService)
	mw := NewMeteringMiddleware(meteringSvc)

	router := gin.New()
	router.Use(func(c *gin.Context) { c.Set("userID", "user-1"); c.Next() })
	router.Use(mw.Handler())

	for _, path := range []string{"/health", "/livez", "/readyz", "/metrics"} {
		router.GET(path, func(c *gin.Context) { c.Status(http.StatusOK) })
	}

	for _, path := range []string{"/health", "/livez", "/readyz", "/metrics"} {
		w := httptest.NewRecorder()
		req := httptest.NewRequest("GET", path, nil)
		router.ServeHTTP(w, req)
		assert.Equal(t, http.StatusOK, w.Code, "path %s should return 200", path)
	}

	meteringSvc.AssertNotCalled(t, "Record")
}
