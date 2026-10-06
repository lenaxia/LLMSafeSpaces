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

// --- #1565: the /v1/user-timezone parse boundary is strict ---
// (the #1561/#1564 convention: exactly one JSON document, loud
// diagnostics, bounded reads. The wire's one legit client is this
// repo's own agentpush.PushUserTimezone, which POSTs a single
// json.Marshal document.)

// tzRound trips one POST through the handler directly.
func tzRound(t *testing.T, body string) *httptest.ResponseRecorder {
	t.Helper()
	userTimezoneAtomic.Store("")
	t.Cleanup(func() { userTimezoneAtomic.Store("") })
	req := httptest.NewRequest(http.MethodPost, "/v1/user-timezone", strings.NewReader(body))
	req.SetBasicAuth("opencode", mcpTestPassword)
	w := httptest.NewRecorder()
	userTimezoneHandler(mcpTestPassword, mcpTestPassword)(w, req)
	return w
}

// Trailing data after the first JSON value was silently skipped AND
// the 256-byte io.LimitReader hid everything past the cap — a
// corrupted push was silently half-honored (first document stored,
// rest never examined). One JSON document per request.
func TestUserTimezone_TrailingDataRejected(t *testing.T) {
	w := tzRound(t, `{"timezone":"UTC"} {"junk":true}`)

	require.Equal(t, http.StatusBadRequest, w.Code, "body: %s", w.Body.String())
	assert.Contains(t, w.Body.String(), "trailing data after offset 18",
		"the diagnostic must say what and where")
	assert.Equal(t, "", userTimezone(),
		"a rejected push must not store the first half of a corrupted body")
}

// The accepted side of the boundary, pinned: a whitespace-only
// remainder stays accepted.
func TestUserTimezone_TrailingNewlineAccepted(t *testing.T) {
	w := tzRound(t, "{\"timezone\":\"Asia/Tokyo\"}\n")

	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
	assert.Equal(t, "Asia/Tokyo", userTimezone())
}

// Parse errors carry diagnostics (#1564): the bare "bad request" that
// masked what actually failed is pinned out.
func TestUserTimezone_InvalidJSONDiagnostic(t *testing.T) {
	w := tzRound(t, `{"timezone":"U`)

	require.Equal(t, http.StatusBadRequest, w.Code)
	assert.Contains(t, w.Body.String(), "unexpected EOF",
		"the decoder's detail must ride the error, not a bare 'bad request'")
}

// Oversize rides 413 with the cap named — including the cap trip in
// the trailing scan (valid first document + whitespace running past
// the cap: the LimitReader era ACCEPTED this shape, invisible bytes
// and all; #1564's exact-doc-plus-trailing-byte classification pin,
// ported to this wire).
func TestUserTimezone_BodyCapTrailingScan413(t *testing.T) {
	body := `{"timezone":"UTC"}` + strings.Repeat(" ", 300)
	w := tzRound(t, body)

	require.Equal(t, http.StatusRequestEntityTooLarge, w.Code, "body: %s", w.Body.String())
	assert.Contains(t, w.Body.String(), "byte cap")
	assert.Equal(t, "", userTimezone())
}

// Additive request-object keys stay tolerated, pinned as a decision:
// the API server is version-skewed from the pods it pushes to —
// additive keys are forward-compat surface (the #1564 request-object
// ruling).
func TestUserTimezone_RequestAdditiveKeysTolerated(t *testing.T) {
	w := tzRound(t, `{"timezone":"Europe/Berlin","future_rev_key":1}`)

	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
	assert.Equal(t, "Europe/Berlin", userTimezone())
}
