// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package handlers

import (
	"sync"
	"testing"
)

// TestProxyHost_ConcurrentLazyInit (#1641): the lazy init of
// ProxyHandler.resolvers must be race-free when many goroutines hit
// host() on a struct-literal-constructed handler (the upload storm
// shape). Red against the unsynchronized nil-check under -race; green
// with the RWMutex-guarded init. The v0.36.0 release run's race
// detector produced this exact stack (proxy_connections.go host() ←
// getPassword() ← UploadFile under TestUpload_ConcurrentStorm…).
func TestProxyHost_ConcurrentLazyInit(t *testing.T) {
	h := &ProxyHandler{}
	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if h.host() == nil {
				t.Error("host() returned nil")
			}
		}()
	}
	wg.Wait()
	if h.host() == nil {
		t.Fatal("host() nil after wait")
	}
}
