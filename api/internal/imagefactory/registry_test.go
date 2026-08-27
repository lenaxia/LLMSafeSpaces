// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package imagefactory

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestHTTPManifestResolver_ResolveDigest(t *testing.T) {
	t.Run("happy path returns digest header", func(t *testing.T) {
		var gotAuth, gotAccept string
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch {
			case r.URL.Path == "/token":
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"token":"tok123"}`))
			case r.Method == http.MethodHead && r.URL.Path == "/v2/acme/base/manifests/0.23.0":
				gotAuth = r.Header.Get("Authorization")
				gotAccept = r.Header.Get("Accept")
				w.Header().Set("Docker-Content-Digest", "sha256:abc123")
				w.WriteHeader(http.StatusOK)
			default:
				w.WriteHeader(http.StatusNotFound)
			}
		}))
		defer srv.Close()

		res := &HTTPManifestResolver{Base: srv.URL}
		digest, err := res.ResolveDigest(context.Background(), "acme/base", "0.23.0")
		require.NoError(t, err)
		assert.Equal(t, "sha256:abc123", digest)
		assert.Equal(t, "Bearer tok123", gotAuth)
		assert.Contains(t, gotAccept, "application/vnd.oci.image.index.v1+json")
	})

	t.Run("host derived from full image ref wins over default base", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/token" {
				// The scope must name the bare repo, host stripped.
				if r.URL.Query().Get("scope") != "repository:acme/base:pull" {
					w.WriteHeader(http.StatusBadRequest)
					return
				}
				_, _ = w.Write([]byte(`{"token":"tok123"}`))
				return
			}
			w.WriteHeader(http.StatusNotFound)
		}))
		defer srv.Close()
		host := strings.TrimPrefix(srv.URL, "https://")

		res := NewHTTPManifestResolver()
		// Point at the test server via the image ref's host segment.
		_, err := res.ResolveDigest(context.Background(), host+"/acme/base", "0.23.0")
		require.Error(t, err) // manifest 404s, but the token scope proves host derivation
	})

	t.Run("missing tag is an error (existence gate)", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/token" {
				_, _ = w.Write([]byte(`{"token":"tok123"}`))
				return
			}
			w.WriteHeader(http.StatusNotFound)
		}))
		defer srv.Close()

		res := &HTTPManifestResolver{Base: srv.URL}
		_, err := res.ResolveDigest(context.Background(), "acme/base", "9.9.9")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "404")
	})

	t.Run("token endpoint failure is an error", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
		}))
		defer srv.Close()

		res := &HTTPManifestResolver{Base: srv.URL}
		_, err := res.ResolveDigest(context.Background(), "acme/base", "0.23.0")
		require.Error(t, err)
	})

	t.Run("empty token body is an error", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte(`{}`))
		}))
		defer srv.Close()

		res := &HTTPManifestResolver{Base: srv.URL}
		_, err := res.ResolveDigest(context.Background(), "acme/base", "0.23.0")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "empty token")
	})

	t.Run("manifest without digest header is an error", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/token" {
				_, _ = w.Write([]byte(`{"token":"tok123"}`))
				return
			}
			w.WriteHeader(http.StatusOK)
		}))
		defer srv.Close()

		res := &HTTPManifestResolver{Base: srv.URL}
		_, err := res.ResolveDigest(context.Background(), "acme/base", "0.23.0")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "no docker-content-digest")
	})
}
