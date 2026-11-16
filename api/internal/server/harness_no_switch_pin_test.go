// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package server

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// harness_no_switch_pin_test.go — pins the ABSENCE the owner ruling
// (2026-11-16) demands: workspaces do NOT switch harnesses, and no
// switch or recreate endpoint exists at any tier. The bind-once
// surface is exactly three entrances — the create param, the explicit
// bind (PUT /workspaces/{id}/harness), and the first-session body —
// and NOTHING else. If a future change adds a switch/recreate/unbind/
// migrate route for harnesses, this pin fails and the change must
// either drop the route or amend the ruling first.
// (Round-0/r2 review history: earlier rounds claimed this pin existed
// while it had never been committed — the router comment, CHANGELOG
// and worklog cited a phantom. This file is the pin, for real.)
func TestHarnessNoSwitchOrRecreateEndpointExists(t *testing.T) {
	// --- 1. The effective gin route table (the real registration ---
	// surface; catches routes registered by any builder path).
	router, _ := newRouterFixture(t)
	harnessRouteCount := 0
	for _, r := range router.Routes() {
		path := strings.ToLower(r.Path)
		if strings.Contains(path, "harness") {
			harnessRouteCount++
			if r.Method != "PUT" || !strings.HasSuffix(path, "/workspaces/:id/harness") {
				t.Errorf("harness-bearing route %s %s: the ruling admits exactly ONE harness route (the bind PUT); switch/recreate/unbind paths must not exist", r.Method, r.Path)
			}
		}
		for _, banned := range []string{"switch", "recreate", "migrate", "unbind"} {
			if strings.Contains(path, banned) {
				t.Errorf("route %s %s contains the banned %q vocabulary — workspaces do not switch harnesses (owner ruling 2026-11-16)", r.Method, r.Path, banned)
			}
		}
	}
	if harnessRouteCount == 0 {
		t.Errorf("the bind route PUT /workspaces/:id/harness must be registered — the bind-once surface requires it")
	}

	// --- 2. The router source: registration text the fixture may ---
	// conditionally skip (nil-guarded builders) still cannot use the
	// banned vocabulary or register other harness routes.
	routerSrc, err := os.ReadFile("router.go")
	if err != nil {
		t.Skipf("router source not available: %v", err)
	}
	harnessRoutes := regexp.MustCompile(`(?i)(GET|POST|PUT|PATCH|DELETE)\("([^"]*harness[^"]*)"`)
	found := map[string]bool{}
	for _, m := range harnessRoutes.FindAllStringSubmatch(string(routerSrc), -1) {
		found[m[2]] = true
	}
	if !found["/harness"] {
		t.Errorf("expected the bind route PUT /harness in router.go registrations; got %v", found)
	}
	for path := range found {
		if path != "/harness" {
			t.Errorf("unexpected harness-bearing route registration %q in router.go: exactly one (the bind) is admitted", path)
		}
	}
	banned := regexp.MustCompile(`(?i)(GET|POST|PUT|PATCH|DELETE)\("[^"]*(switch|recreate|migrate|unbind)[^"]*"`)
	if loc := banned.FindString(string(routerSrc)); loc != "" {
		t.Errorf("route registration matching the banned switch/recreate vocabulary exists in router.go: %q", loc)
	}

	// --- 3. The OpenAPI contract carries the same absence. ---
	specSrc, err := os.ReadFile("../../../sdks/openapi.yaml")
	if err != nil {
		t.Skipf("openapi spec not available: %v", err)
	}
	for _, line := range strings.Split(string(specSrc), "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "/") && strings.HasSuffix(trimmed, ":") {
			path := strings.TrimSuffix(trimmed, ":")
			for _, word := range []string{"switch", "recreate", "migrate", "unbind"} {
				if strings.Contains(path, word) {
					t.Errorf("openapi path %q contains the banned %q vocabulary — no switch/recreate endpoint may exist at any tier", path, word)
				}
			}
		}
	}
}
