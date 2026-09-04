package handlers

import (
	"context"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"

	"github.com/lenaxia/llmsafespaces/pkg/secrets"
	"github.com/lenaxia/llmsafespaces/pkg/types"
)

func b64url(s string) string { return base64.RawURLEncoding.EncodeToString([]byte(s)) }
func itoaExp(n int64) string { return strconv.FormatInt(n, 10) }

func mintTestJWT(exp time.Time) string {
	// Header/payload shape only — unverifiedJWTExp never checks the signature.
	return "eyJhbGciOiJIUzI1NiJ9." + b64url(`{"exp":`+itoaExp(exp.Unix())+`}`) + ".sig"
}

func TestUnverifiedJWTExp(t *testing.T) {
	exp := time.Now().Add(time.Hour)
	got, ok := unverifiedJWTExp(mintTestJWT(exp))
	if !ok || got.Unix() != exp.Unix() {
		t.Fatalf("exp roundtrip: ok=%v got=%v want=%v", ok, got.Unix(), exp.Unix())
	}
	if _, ok := unverifiedJWTExp("not-a-jwt"); ok {
		t.Fatal("malformed token must not parse")
	}
	if _, ok := unverifiedJWTExp("a.eyJub2V4cCI6MX0.c"); ok {
		t.Fatal("absent exp must return ok=false")
	}
}

func TestExpiryLeewayBoundary(t *testing.T) {
	// 30s past exp: inside the 60s leeway — still accepted.
	if exp, ok := unverifiedJWTExp(mintTestJWT(time.Now().Add(-30 * time.Second))); !ok || time.Now().After(exp.Add(jwtExpiryLeeway)) {
		t.Fatal("within leeway must not be rejected")
	}
	// 2min past exp: outside the leeway — rejected.
	if exp, ok := unverifiedJWTExp(mintTestJWT(time.Now().Add(-2 * time.Minute))); !ok || !time.Now().After(exp.Add(jwtExpiryLeeway)) {
		t.Fatal("past leeway must be rejected")
	}
}

// TestBootstrap_ExpiredTokenRejected is the handler-level regression pin
// for the defense-in-depth expiry check (#1244): the reviewer AUTHENTICATES
// (kind v1.35's TokenReview does not enforce exp — empirically), and the
// handler must still 401 a token past its own exp + leeway. Deleting the
// exp check in Bootstrap fails here first.
func TestBootstrap_ExpiredTokenRejected(t *testing.T) {
	gin.SetMode(gin.TestMode)
	const testNS = "llmsafespaces"
	const wsID = "ws-exp"
	reviewer := &staticTokenReviewer{
		username: "system:serviceaccount:" + testNS + ":workspace-" + wsID,
	}
	h := NewPodBootstrapHandler(reviewer, &noOpInjector{}, &wsMetaLookup{ws: &types.WorkspaceMetadata{
		ID: wsID, UserID: "u", DefaultModel: "m",
	}}, nil, testNS)
	router := gin.New()
	router.POST("/internal/v1/pod-bootstrap", h.Bootstrap)
	srv := httptest.NewServer(router)
	t.Cleanup(srv.Close)

	post := func(tok string) int {
		req, _ := http.NewRequest(http.MethodPost, srv.URL+"/internal/v1/pod-bootstrap",
			strings.NewReader(`{"workspaceID":"`+wsID+`"}`))
		req.Header.Set("Authorization", "Bearer "+tok)
		req.Header.Set("Content-Type", "application/json")
		resp, err := srv.Client().Do(req)
		require.NoError(t, err)
		resp.Body.Close()
		return resp.StatusCode
	}

	// Freshly-minted shape: exp in the future → NOT a 401 at the auth layer
	// (past auth the nil injector may 500 — the pin is specifically that
	// expiry does not reject a valid token).
	fresh := mintTestJWT(time.Now().Add(time.Hour))
	if code := post(fresh); code == http.StatusUnauthorized {
		t.Fatal("valid, unexpired token must not be rejected at the auth layer")
	}

	// Expired past the leeway → exactly 401 (the reviewer said YES; the
	// handler's own exp check is the only thing that can answer).
	expired := mintTestJWT(time.Now().Add(-2 * time.Minute))
	if code := post(expired); code != http.StatusUnauthorized {
		t.Fatalf("expired token past leeway: got %d, want 401", code)
	}

	// Inside the leeway → not rejected on expiry grounds.
	within := mintTestJWT(time.Now().Add(-30 * time.Second))
	if code := post(within); code == http.StatusUnauthorized {
		t.Fatal("token inside the clock-skew leeway must not be rejected")
	}
}

// noOpInjector satisfies bootstrapInjector so non-expiry requests pass
// through auth and the batch builder without touching SecretService.
type noOpInjector struct{}

func (noOpInjector) BuildWorkspaceBatch(_ context.Context, _, _ string) (*secrets.Batch, *secrets.BuildDegrade, error) {
	return &secrets.Batch{}, nil, nil
}
