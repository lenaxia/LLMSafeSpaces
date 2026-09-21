// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package handlers

// #1499: the user-prompts handler rows — happy CRUD, the validation
// matrix (name/content caps, control characters), the typed-error HTTP
// mapping (409 name-taken, 404 unknown), and owner scoping (userID
// from context only — a body can never address another user's rows).

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lenaxia/llmsafespaces/api/internal/services/database"
	"github.com/lenaxia/llmsafespaces/pkg/types"
)

type stubUserPromptStore struct {
	rows     map[string]types.UserPrompt // key: "userID/promptID"
	byName   map[string]string           // key: "userID/name" -> promptID
	nextID   int
	deleted  []string
	failWith error
}

func newStubUserPromptStore() *stubUserPromptStore {
	return &stubUserPromptStore{rows: map[string]types.UserPrompt{}, byName: map[string]string{}}
}

func (s *stubUserPromptStore) ListUserPrompts(_ context.Context, userID string) ([]types.UserPrompt, error) {
	if s.failWith != nil {
		return nil, s.failWith
	}
	var out []types.UserPrompt
	for k, p := range s.rows {
		if strings.HasPrefix(k, userID+"/") {
			out = append(out, p)
		}
	}
	return out, nil
}

func (s *stubUserPromptStore) CreateUserPrompt(_ context.Context, userID, name, content string) (*types.UserPrompt, error) {
	if s.failWith != nil {
		return nil, s.failWith
	}
	if _, taken := s.byName[userID+"/"+name]; taken {
		return nil, database.ErrPromptNameTaken
	}
	s.nextID++
	id := fmt.Sprintf("p%04d", s.nextID)
	now := time.Now().UTC()
	p := types.UserPrompt{ID: id, Name: name, Content: content, CreatedAt: now, UpdatedAt: now}
	s.rows[userID+"/"+id] = p
	s.byName[userID+"/"+name] = id
	return &p, nil
}

func (s *stubUserPromptStore) UpdateUserPrompt(_ context.Context, userID, id string, name, content *string) (*types.UserPrompt, error) {
	if s.failWith != nil {
		return nil, s.failWith
	}
	p, ok := s.rows[userID+"/"+id]
	if !ok {
		return nil, database.ErrPromptNotFound
	}
	if name != nil {
		if other, taken := s.byName[userID+"/"+*name]; taken && other != id {
			return nil, database.ErrPromptNameTaken
		}
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

func (s *stubUserPromptStore) DeleteUserPrompt(_ context.Context, userID, id string) error {
	p, ok := s.rows[userID+"/"+id]
	if !ok {
		return database.ErrPromptNotFound
	}
	delete(s.rows, userID+"/"+id)
	delete(s.byName, userID+"/"+p.Name)
	s.deleted = append(s.deleted, id)
	return nil
}

func userPromptCall(method, path, body string) (*gin.Context, *httptest.ResponseRecorder) {
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	var req *http.Request
	if body != "" {
		req = httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
	} else {
		req = httptest.NewRequest(method, path, nil)
	}
	c.Request = req
	c.Set("userID", "user-1")
	return c, w
}

func TestUserPrompts_CRUDRoundTrip(t *testing.T) {
	store := newStubUserPromptStore()
	h := NewUserPromptsHandler(store)

	c, w := userPromptCall("POST", "/", `{"name":"Weekly summary","content":"Summarize the week."}`)
	h.Create(c)
	require.Equal(t, http.StatusCreated, w.Code)
	var created types.UserPromptResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &created))
	require.Equal(t, "Weekly summary", created.Prompt.Name)

	c, w = userPromptCall("GET", "/", "")
	h.List(c)
	require.Equal(t, http.StatusOK, w.Code)
	var list types.UserPromptListResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &list))
	require.Len(t, list.Prompts, 1)
	assert.Equal(t, "Weekly summary", list.Prompts[0].Name, "the named envelope carries the row")

	c, w = userPromptCall("PUT", "/", `{"name":"Renamed"}`)
	c.Params = gin.Params{{Key: "id", Value: created.Prompt.ID}}
	h.Update(c)
	require.Equal(t, http.StatusOK, w.Code)
	var updated types.UserPromptResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &updated))
	assert.Equal(t, "Renamed", updated.Prompt.Name)
	assert.Equal(t, "Summarize the week.", updated.Prompt.Content, "nil content keeps the stored value")

	c, _ = userPromptCall("DELETE", "/", "")
	c.Params = gin.Params{{Key: "id", Value: created.Prompt.ID}}
	h.Delete(c)
	// Bare test contexts never flush the writer (real routing does);
	// the writer's recorded status is the assertion surface.
	assert.Equal(t, http.StatusNoContent, c.Writer.Status())
	assert.Equal(t, []string{created.Prompt.ID}, store.deleted)
}

func TestUserPrompts_CreateValidation(t *testing.T) {
	store := newStubUserPromptStore()
	h := NewUserPromptsHandler(store)

	cases := []struct {
		name string
		body string
		want int
	}{
		{"empty name", `{"name":"","content":"x"}`, http.StatusBadRequest},
		{"name over 100 runes", `{"name":"` + strings.Repeat("a", 101) + `","content":"x"}`, http.StatusBadRequest},
		{"name with control char", "{\"name\":\"a\u0000b\",\"content\":\"x\"}", http.StatusBadRequest},
		{"empty content", `{"name":"n","content":""}`, http.StatusBadRequest},
		{"content over cap", `{"name":"n","content":"` + strings.Repeat("x", 65537) + `"}`, http.StatusBadRequest},
		{"invalid json", `{`, http.StatusBadRequest},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, w := userPromptCall("POST", "/", tc.body)
			h.Create(c)
			assert.Equal(t, tc.want, w.Code)
		})
	}
}

func TestUserPrompts_NameConflictIs409(t *testing.T) {
	store := newStubUserPromptStore()
	h := NewUserPromptsHandler(store)

	c, _ := userPromptCall("POST", "/", `{"name":"dup","content":"x"}`)
	h.Create(c)

	c, w := userPromptCall("POST", "/", `{"name":"dup","content":"y"}`)
	h.Create(c)
	assert.Equal(t, http.StatusConflict, w.Code)
}

func TestUserPrompts_UnknownIDIs404(t *testing.T) {
	store := newStubUserPromptStore()
	h := NewUserPromptsHandler(store)

	c, w := userPromptCall("PUT", "/", `{"name":"n"}`)
	c.Params = gin.Params{{Key: "id", Value: "nope"}}
	h.Update(c)
	assert.Equal(t, http.StatusNotFound, w.Code)

	c, w = userPromptCall("DELETE", "/", "")
	c.Params = gin.Params{{Key: "id", Value: "nope"}}
	h.Delete(c)
	assert.Equal(t, http.StatusNotFound, w.Code)
}

func TestUserPrompts_UpdateValidationMirrorsCreate(t *testing.T) {
	store := newStubUserPromptStore()
	h := NewUserPromptsHandler(store)

	c, _ := userPromptCall("POST", "/", `{"name":"valid","content":"x"}`)
	h.Create(c)
	id := store.byName["user-1/valid"]

	cases := []struct {
		name string
		body string
	}{
		{"bad name", `{"name":"` + strings.Repeat("a", 101) + `"}`},
		{"bad content", `{"content":"` + strings.Repeat("x", 65537) + `"}`},
		{"nothing to update", `{}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, w := userPromptCall("PUT", "/", tc.body)
			c.Params = gin.Params{{Key: "id", Value: id}}
			h.Update(c)
			assert.Equal(t, http.StatusBadRequest, w.Code)
		})
	}
}

func TestUserPrompts_StoreFailureIs500(t *testing.T) {
	store := newStubUserPromptStore()
	store.failWith = assert.AnError
	h := NewUserPromptsHandler(store)

	c, w := userPromptCall("GET", "/", "")
	h.List(c)
	assert.Equal(t, http.StatusInternalServerError, w.Code)
}
