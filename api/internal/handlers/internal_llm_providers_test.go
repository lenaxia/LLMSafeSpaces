// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package handlers

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/lenaxia/llmsafespaces/pkg/secrets"
)

// fakeLLMProviderResolver backs the internal llm-providers endpoint tests.
type fakeLLMProviderResolver struct {
	providers []secrets.LLMProviderData
	err       error
	gotOwner  string
	gotWS     string
}

func (f *fakeLLMProviderResolver) ResolveLLMProviders(_ context.Context, ownerUserID, workspaceID string) ([]secrets.LLMProviderData, error) {
	f.gotOwner = ownerUserID
	f.gotWS = workspaceID
	return f.providers, f.err
}

func setupLLMProvidersRouter(t *testing.T, svc *fakeLLMProviderResolver, tokenSet bool) *gin.Engine {
	t.Helper()
	gin.SetMode(gin.TestMode)
	if tokenSet {
		t.Setenv("LLMSAFESPACES_INTERNAL_TOKEN", "sekret")
	}
	h := NewInternalLLMProvidersHandler(svc)
	r := gin.New()
	r.GET("/api/v1/internal/workspaces/:workspaceID/llm-providers", h.GetLLMProviders)
	return r
}

func TestInternalLLMProviders_HappyPath(t *testing.T) {
	svc := &fakeLLMProviderResolver{providers: []secrets.LLMProviderData{
		{Kind: "openai", Slug: "openai", APIKey: "sk-live", Models: []secrets.LLMModelConfig{{ID: "gpt-4o"}}},
	}}
	r := setupLLMProvidersRouter(t, svc, true)

	req := httptest.NewRequest("GET", "/api/v1/internal/workspaces/ws-1/llm-providers?ownerUserID=user-7", nil)
	req.Header.Set("X-Internal-Token", "sekret")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	for _, want := range []string{`"slug":"openai"`, "sk-live", `"gpt-4o"`} {
		if !strings.Contains(body, want) {
			t.Errorf("response missing %q: %s", want, body)
		}
	}
	if svc.gotOwner != "user-7" || svc.gotWS != "ws-1" {
		t.Errorf("resolver saw owner=%q ws=%q", svc.gotOwner, svc.gotWS)
	}
}

func TestInternalLLMProviders_FailClosedWithoutToken(t *testing.T) {
	r := setupLLMProvidersRouter(t, &fakeLLMProviderResolver{}, false)
	req := httptest.NewRequest("GET", "/api/v1/internal/workspaces/ws-1/llm-providers?ownerUserID=u", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusForbidden {
		t.Fatalf("unset token must fail closed 403, got %d", w.Code)
	}
}

func TestInternalLLMProviders_RejectsBadToken(t *testing.T) {
	r := setupLLMProvidersRouter(t, &fakeLLMProviderResolver{}, true)
	req := httptest.NewRequest("GET", "/api/v1/internal/workspaces/ws-1/llm-providers?ownerUserID=u", nil)
	req.Header.Set("X-Internal-Token", "wrong")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("bad token must 401, got %d", w.Code)
	}
}

func TestInternalLLMProviders_RequiresOwnerUserID(t *testing.T) {
	r := setupLLMProvidersRouter(t, &fakeLLMProviderResolver{}, true)
	req := httptest.NewRequest("GET", "/api/v1/internal/workspaces/ws-1/llm-providers", nil)
	req.Header.Set("X-Internal-Token", "sekret")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("missing ownerUserID must 400, got %d", w.Code)
	}
}

func TestInternalLLMProviders_ResolveFailureIs500WithoutDetail(t *testing.T) {
	r := setupLLMProvidersRouter(t, &fakeLLMProviderResolver{err: errors.New("db: boom credential material")}, true)
	req := httptest.NewRequest("GET", "/api/v1/internal/workspaces/ws-1/llm-providers?ownerUserID=u", nil)
	req.Header.Set("X-Internal-Token", "sekret")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("resolve failure must 500, got %d", w.Code)
	}
	if strings.Contains(w.Body.String(), "credential material") {
		t.Errorf("error body must not leak the underlying error: %s", w.Body.String())
	}
	if os.Getenv("LLMSAFESPACES_INTERNAL_TOKEN") == "" {
		t.Fatal("token env should be set for this test")
	}
}
