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

	"github.com/lenaxia/llmsafespaces/api/internal/imagefactory"
	"github.com/lenaxia/llmsafespaces/api/internal/services/database"
)

// fakeResolveStore layers ResolveHash onto the shared fakeIFStore so the
// composed value satisfies the full imageFactoryStore interface.
type fakeResolveStore struct {
	fakeIFStore
	resolveRes imagefactory.HashResolution
	resolveErr error
	lastHash   string
}

func (f *fakeResolveStore) ResolveHash(ctx context.Context, hash string) (imagefactory.HashResolution, error) {
	f.lastHash = hash
	return f.resolveRes, f.resolveErr
}

func TestResolveHash_Handler(t *testing.T) {
	setup := func(store *fakeResolveStore) *gin.Engine {
		gin.SetMode(gin.TestMode)
		r := gin.New()
		h := &ImageFactoryHandler{store: store}
		r.GET("/api/v1/image-factory/resolve/:hash", h.ResolveHash)
		return r
	}

	t.Run("happy path returns resolution", func(t *testing.T) {
		store := &fakeResolveStore{
			resolveRes: imagefactory.HashResolution{
				Hash:      "s-abc123def4567890",
				Selection: imagefactory.Selection{"ffmpeg"},
				BaseName:  "bookworm",
				Versions:  []string{"0.23.0", "0.21.2"},
			},
		}
		r := setup(store)
		w := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/api/v1/image-factory/resolve/s-abc123def4567890", nil)
		r.ServeHTTP(w, req)
		require.Equal(t, http.StatusOK, w.Code)
		var body imagefactory.HashResolution
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
		assert.Equal(t, "s-abc123def4567890", body.Hash)
		assert.Equal(t, "bookworm", body.BaseName)
		assert.Equal(t, []string{"0.23.0", "0.21.2"}, body.Versions)
		assert.Equal(t, "s-abc123def4567890", store.lastHash)
	})

	t.Run("malformed hash is 422 without a store call", func(t *testing.T) {
		store := &fakeResolveStore{}
		r := setup(store)
		w := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/api/v1/image-factory/resolve/not-a-hash", nil)
		r.ServeHTTP(w, req)
		assert.Equal(t, http.StatusUnprocessableEntity, w.Code)
		assert.Empty(t, store.lastHash)
	})

	t.Run("uppercase hex is rejected (hashes are lowercase by construction)", func(t *testing.T) {
		store := &fakeResolveStore{}
		r := setup(store)
		w := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/api/v1/image-factory/resolve/s-ABC123DEF4567890", nil)
		r.ServeHTTP(w, req)
		assert.Equal(t, http.StatusUnprocessableEntity, w.Code)
	})

	t.Run("unknown hash is 404", func(t *testing.T) {
		store := &fakeResolveStore{resolveErr: database.ErrNotFound}
		r := setup(store)
		w := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/api/v1/image-factory/resolve/s-abc123def4567890", nil)
		r.ServeHTTP(w, req)
		assert.Equal(t, http.StatusNotFound, w.Code)
	})

	t.Run("store error is 500", func(t *testing.T) {
		store := &fakeResolveStore{resolveErr: errors.New("db down")}
		r := setup(store)
		w := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/api/v1/image-factory/resolve/s-abc123def4567890", nil)
		r.ServeHTTP(w, req)
		assert.Equal(t, http.StatusInternalServerError, w.Code)
	})
}
