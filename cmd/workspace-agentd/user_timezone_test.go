package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Route-level pin (repo convention: resync_secrets_test.go): the path
// served by buildUserMux must be exactly what agentpush pushes —
// deleting the server.go registration fails this test.
func TestUserTimezoneRoute_Registered(t *testing.T) {
	mux := buildUserMux(context.Background(), nil, serverDeps{password: mcpTestPassword})
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/user-timezone", strings.NewReader(`{"timezone":"Asia/Tokyo"}`))
	req.SetBasicAuth("opencode", mcpTestPassword)
	mux.ServeHTTP(rr, req)
	require.Equal(t, http.StatusOK, rr.Code, "route must be served on the user mux")
	assert.Equal(t, "Asia/Tokyo", userTimezone())
	t.Cleanup(func() { userTimezoneAtomic.Store("") })

	// Unauthenticated through the real mux: 401.
	rr = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodPost, "/v1/user-timezone", strings.NewReader(`{"timezone":"Asia/Tokyo"}`))
	mux.ServeHTTP(rr, req)
	assert.Equal(t, http.StatusUnauthorized, rr.Code)
}
