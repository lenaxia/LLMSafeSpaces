// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package server

// #1499 round-1: the COMPOSED rows — real router → AuthMiddleware →
// UserPromptsHandler → store stub, over HTTP. The handler-level rows
// prove the handler in isolation; these prove the wiring (auth gate,
// routing, envelopes on the wire).

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"github.com/lenaxia/llmsafespaces/api/internal/handlers"
	apilogger "github.com/lenaxia/llmsafespaces/api/internal/logger"
	imocks "github.com/lenaxia/llmsafespaces/api/internal/mocks"
	"github.com/lenaxia/llmsafespaces/api/internal/services/database"
	"github.com/lenaxia/llmsafespaces/pkg/types"
)

type routerPromptStore struct {
	mu      sync.Mutex
	rows    map[string]types.UserPrompt // "userID/id"
	byName  map[string]string           // "userID/name" -> id
	next    int
	deleted []string
}

func (s *routerPromptStore) ListUserPrompts(_ context.Context, userID string) ([]types.UserPrompt, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []types.UserPrompt
	for k, p := range s.rows {
		if len(k) > len(userID)+1 && k[:len(userID)] == userID {
			out = append(out, p)
		}
	}
	return out, nil
}

func (s *routerPromptStore) CreateUserPrompt(_ context.Context, userID, name, content string) (*types.UserPrompt, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, taken := s.byName[userID+"/"+name]; taken {
		return nil, database.ErrPromptNameTaken
	}
	s.next++
	id := time.Now().Format("150405.000000000")
	now := time.Now().UTC()
	p := types.UserPrompt{ID: id, Name: name, Content: content, CreatedAt: now, UpdatedAt: now}
	s.rows[userID+"/"+id] = p
	s.byName[userID+"/"+name] = id
	return &p, nil
}

func (s *routerPromptStore) UpdateUserPrompt(_ context.Context, userID, id string, name, content *string) (*types.UserPrompt, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.rows[userID+"/"+id]
	if !ok {
		return nil, database.ErrPromptNotFound
	}
	if name != nil {
		delete(s.byName, userID+"/"+p.Name)
		p.Name = *name
		s.byName[userID+"/"+p.Name] = id
	}
	if content != nil {
		p.Content = *content
	}
	p.UpdatedAt = time.Now().UTC()
	s.rows[userID+"/"+id] = p
	return &p, nil
}

func (s *routerPromptStore) DeleteUserPrompt(_ context.Context, userID, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.rows[userID+"/"+id]
	if !ok {
		return database.ErrPromptNotFound
	}
	delete(s.rows, userID+"/"+id)
	delete(s.byName, userID+"/"+p.Name)
	s.deleted = append(s.deleted, id)
	return nil
}

func newPromptsRouterFixture(t *testing.T) (*gin.Engine, *routerPromptStore) {
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
		if c.GetHeader("Authorization") == "" {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "authentication required"})
			return
		}
		c.Set("userID", "test-user")
		c.Next()
	}))
	auth.On("GetUserID", mock.Anything).Return("test-user")

	store := &routerPromptStore{rows: map[string]types.UserPrompt{}, byName: map[string]string{}}
	svc := &mockServices{auth: auth, metrics: met}
	router := NewRouter(svc, log, nil, RouterConfig{
		Debug:              false,
		UserPromptsHandler: handlers.NewUserPromptsHandler(store),
	})
	return router, store
}

func doReq(t *testing.T, router *gin.Engine, method, path, body string, authed bool) *httptest.ResponseRecorder {
	t.Helper()
	var reader *bytes.Reader
	if body != "" {
		reader = bytes.NewReader([]byte(body))
	} else {
		reader = bytes.NewReader(nil)
	}
	req := httptest.NewRequest(method, path, reader)
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	if authed {
		req.Header.Set("Authorization", "Bearer token")
	}
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	return rec
}

func TestPromptsRoutes_AuthGate(t *testing.T) {
	router, _ := newPromptsRouterFixture(t)
	for _, tc := range []struct{ m, p string }{
		{http.MethodGet, "/api/v1/me/prompts"},
		{http.MethodPost, "/api/v1/me/prompts"},
		{http.MethodPut, "/api/v1/me/prompts/x"},
		{http.MethodDelete, "/api/v1/me/prompts/x"},
	} {
		assert.Equal(t, http.StatusUnauthorized, doReq(t, router, tc.m, tc.p, "", false).Code,
			"%s %s must be behind the auth gate", tc.m, tc.p)
	}
}

func TestPromptsRoutes_CRUDOverHTTP(t *testing.T) {
	router, _ := newPromptsRouterFixture(t)

	rec := doReq(t, router, http.MethodPost, "/api/v1/me/prompts",
		`{"name":"  Weekly summary  ","content":"Summarize the week."}`, true)
	require.Equal(t, http.StatusCreated, rec.Code)
	var created types.UserPromptResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &created))
	assert.Equal(t, "Weekly summary", created.Prompt.Name, "trimmed name on the wire")

	rec = doReq(t, router, http.MethodGet, "/api/v1/me/prompts", "", true)
	require.Equal(t, http.StatusOK, rec.Code)
	var list types.UserPromptListResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &list))
	require.Len(t, list.Prompts, 1)

	rec = doReq(t, router, http.MethodPut, "/api/v1/me/prompts/"+created.Prompt.ID,
		`{"content":"New body"}`, true)
	require.Equal(t, http.StatusOK, rec.Code)
	var updated types.UserPromptResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &updated))
	assert.Equal(t, "New body", updated.Prompt.Content)

	rec = doReq(t, router, http.MethodPost, "/api/v1/me/prompts",
		`{"name":"Weekly summary","content":"dup"}`, true)
	assert.Equal(t, http.StatusConflict, rec.Code, "the trimmed-name uniqueness surfaces over HTTP")

	rec = doReq(t, router, http.MethodDelete, "/api/v1/me/prompts/"+created.Prompt.ID, "", true)
	assert.Equal(t, http.StatusNoContent, rec.Code)

	rec = doReq(t, router, http.MethodDelete, "/api/v1/me/prompts/"+created.Prompt.ID, "", true)
	assert.Equal(t, http.StatusNotFound, rec.Code)
}
