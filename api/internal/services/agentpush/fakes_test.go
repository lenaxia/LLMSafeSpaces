// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package agentpush_test

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"sync"
)

type fakeResolver struct {
	ip  string
	err error
}

func (f *fakeResolver) GetWorkspacePodIP(_ context.Context, _, _ string) (string, error) {
	return f.ip, f.err
}

type fakeCache struct {
	evictedKeys []string
	mu          sync.Mutex
}

func (f *fakeCache) Evict(workspaceID string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.evictedKeys = append(f.evictedKeys, workspaceID)
}

type fakePasswordProvider struct {
	password string
	err      error
}

func (f *fakePasswordProvider) WorkspacePassword(_ context.Context, _ string) (string, error) {
	return f.password, f.err
}

// rewritingTransport lets tests use httptest.Server despite agentpush
// formatting pod-IP URLs. Every request is redirected to the target's
// host regardless of the original URL's host/port.
type rewritingTransport struct {
	target string
}

func (t *rewritingTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	if !strings.HasPrefix(t.target, "http://") && !strings.HasPrefix(t.target, "https://") {
		return nil, fmt.Errorf("rewritingTransport: bad target %q", t.target)
	}
	newURL := *r.URL
	host := strings.TrimPrefix(strings.TrimPrefix(t.target, "https://"), "http://")
	newURL.Scheme = "http"
	newURL.Host = host
	r2 := r.Clone(r.Context())
	r2.URL = &newURL
	r2.Host = host
	return http.DefaultTransport.RoundTrip(r2)
}
