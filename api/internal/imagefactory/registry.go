// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package imagefactory

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// ManifestResolver resolves a container image tag to its manifest digest.
// It doubles as the existence gate for base-sync: a tag that cannot be
// resolved must never enter the catalog (GH Actions could not pull it).
type ManifestResolver interface {
	ResolveDigest(ctx context.Context, repo, tag string) (string, error)
}

// HTTPManifestResolver resolves digests from an OCI Distribution registry
// (ghcr shape: anonymous pull token + HEAD manifest). The token endpoint
// and www-authenticate flow match ghcr.io and standard registries.
type HTTPManifestResolver struct {
	// Base is the registry API origin, e.g. "https://ghcr.io". Defaults
	// to https://ghcr.io when empty.
	Base string
	// Client is the HTTP client; a default with a 15s timeout is used
	// when nil.
	Client *http.Client
}

// NewHTTPManifestResolver constructs a resolver with sane defaults.
func NewHTTPManifestResolver() *HTTPManifestResolver {
	return &HTTPManifestResolver{
		Base:   "https://ghcr.io",
		Client: &http.Client{Timeout: 15 * time.Second},
	}
}

type registryToken struct {
	Token string `json:"token"`
}

// ResolveDigest fetches an anonymous pull token, then HEADs the manifest
// (accepting OCI index / Docker manifest-list media types) and returns
// the docker-content-digest header value.
//
// image may be a full reference whose first path segment is a registry
// host (e.g. "ghcr.io/acme/base") — the host is derived from the ref and
// wins over the resolver's default Base. A bare repo ("acme/base") uses
// Base (ghcr.io by default).
func (r *HTTPManifestResolver) ResolveDigest(ctx context.Context, image, tag string) (string, error) {
	base := r.Base
	repo := image
	if first := strings.SplitN(image, "/", 2); len(first) == 2 &&
		(strings.Contains(first[0], ".") || strings.Contains(first[0], ":")) {
		base = "https://" + first[0]
		repo = first[1]
	}
	if base == "" {
		base = "https://ghcr.io"
	}
	client := r.Client
	if client == nil {
		client = &http.Client{Timeout: 15 * time.Second}
	}

	tokenURL := fmt.Sprintf("%s/token?scope=repository:%s:pull", base, repo)
	treq, err := http.NewRequestWithContext(ctx, http.MethodGet, tokenURL, nil)
	if err != nil {
		return "", fmt.Errorf("registry token request: %w", err)
	}
	tresp, err := client.Do(treq)
	if err != nil {
		return "", fmt.Errorf("registry token fetch: %w", err)
	}
	defer func() { _ = tresp.Body.Close() }()
	if tresp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("registry token fetch: status %d", tresp.StatusCode)
	}
	var tok registryToken
	if err := json.NewDecoder(tresp.Body).Decode(&tok); err != nil {
		return "", fmt.Errorf("registry token decode: %w", err)
	}
	if tok.Token == "" {
		return "", fmt.Errorf("registry token decode: empty token")
	}

	manifestURL := fmt.Sprintf("%s/v2/%s/manifests/%s", base, repo, tag)
	mreq, err := http.NewRequestWithContext(ctx, http.MethodHead, manifestURL, nil)
	if err != nil {
		return "", fmt.Errorf("manifest request: %w", err)
	}
	mreq.Header.Set("Authorization", "Bearer "+tok.Token)
	mreq.Header.Set("Accept", strings.Join([]string{
		"application/vnd.oci.image.index.v1+json",
		"application/vnd.docker.distribution.manifest.list.v2+json",
		"application/vnd.oci.image.manifest.v1+json",
		"application/vnd.docker.distribution.manifest.v2+json",
	}, ", "))
	mresp, err := client.Do(mreq)
	if err != nil {
		return "", fmt.Errorf("manifest head: %w", err)
	}
	defer func() { _ = mresp.Body.Close() }()
	if mresp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("manifest head: status %d", mresp.StatusCode)
	}
	digest := mresp.Header.Get("Docker-Content-Digest")
	if digest == "" {
		return "", fmt.Errorf("manifest head: no docker-content-digest header")
	}
	return digest, nil
}
