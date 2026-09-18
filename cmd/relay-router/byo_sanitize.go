// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
)

// byoIdentityHeaders extends the fleet router's hop-by-hop set (proxy.go)
// with the identity/platform headers that must never reach an upstream:
// client-supplied X-Relay-* (router-internal), platform headers, and the
// workspace identity (the token is the only credential the router accepts;
// the upstream Authorization is injected after resolve, §4.7).
var byoIdentityHeaders = map[string]struct{}{
	"X-Relay-Token":     {},
	"X-Relay-Status":    {},
	"X-Workspace-Id":    {},
	"X-Llmsafespaces-*": {},
	"X-Forwarded-For":   {},
	"X-Forwarded-Host":  {},
	"X-Real-Ip":         {},
	"Cookie":            {},
	"Host":              {},
}

func byoHeaderBlocked(name string) bool {
	canonical := http.CanonicalHeaderKey(name)
	for pattern := range byoIdentityHeaders {
		if strings.HasSuffix(pattern, "*") {
			if strings.HasPrefix(canonical, strings.TrimSuffix(pattern, "*")) {
				return true
			}
			continue
		}
		if canonical == pattern {
			return true
		}
	}
	return false
}

// copyByoHeaders copies allow-listed headers: hop-by-hop (routerHopHeaders)
// and identity headers (byoIdentityHeaders, incl. client Authorization —
// §4.7 rule 1) never survive.
func copyByoHeaders(dst, src http.Header) {
	for key, values := range src {
		if _, isHop := routerHopHeaders[http.CanonicalHeaderKey(key)]; isHop {
			continue
		}
		if byoHeaderBlocked(key) {
			continue
		}
		for _, value := range values {
			dst.Add(key, value)
		}
	}
}

var errMethodNotAllowed = errors.New("method not allowed by relay path allowlist")
var errPathNotAllowed = errors.New("path not allowed by relay path allowlist")
var errModelNotAllowed = errors.New("model not in token allowlist")
var errModelMissing = errors.New("request has no model field")

// byoAllowedMethods: the router is not a general proxy — chat-completions
// class endpoints plus /models only (§4.7 rule 5).
var byoAllowedMethods = map[string]map[string]bool{ // method → path-suffix-set
	http.MethodGet:    {"/models": true},
	http.MethodPost:   {"/chat/completions": true, "/completions": true, "/embeddings": true},
	http.MethodDelete: {},
}

func byoMethodPathAllowed(method, subPath string) error {
	if !strings.HasPrefix(subPath, "/") {
		subPath = "/" + subPath
	}
	paths, ok := byoAllowedMethods[method]
	if !ok {
		return fmt.Errorf("%w: %s", errMethodNotAllowed, method)
	}
	if !paths[subPath] {
		return fmt.Errorf("%w: %s", errPathNotAllowed, subPath)
	}
	return nil
}

// byoRequestBodyShape carries the parsed request fields the sanitization
// stage needs without re-reading the body.
type byoRequestBodyShape struct {
	Model  string          `json:"model"`
	Stream bool            `json:"stream"`
	Raw    json.RawMessage `json:"-"`
}

// parseByoRequestBody parses a JSON request body and extracts the model
// field for the allowlist check (§4.7 rule 4 — defense in depth on top of
// the batch-side enforcement the platform already applies).
func parseByoRequestBody(body []byte) (byoRequestBodyShape, error) {
	var shape byoRequestBodyShape
	if len(body) == 0 {
		return shape, nil
	}
	if err := json.Unmarshal(body, &shape); err != nil {
		return byoRequestBodyShape{}, fmt.Errorf("parsing request body: %w", err)
	}
	return shape, nil
}

func byoModelAllowed(shape byoRequestBodyShape, allowlist []string) error {
	if len(allowlist) == 0 {
		// No allowlist in the token means no restriction was staged —
		// unreachable in production (US-72.3 always stages one).
		return nil
	}
	if shape.Model == "" {
		return errModelMissing
	}
	for _, m := range allowlist {
		if m == shape.Model {
			return nil
		}
	}
	return fmt.Errorf("%w: %s", errModelNotAllowed, shape.Model)
}
