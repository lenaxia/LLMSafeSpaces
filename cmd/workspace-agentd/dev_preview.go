// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

// Epic 66: Dev Preview — authenticated HTTP/WS tunnel from API to localhost
// dev servers (Vite, Next, etc.) running inside the workspace pod.
//
// GET /v1/dev-preview/:port/* — forwards to http://localhost:<port>/<path>.
// The API server (proxy.go-equivalent) calls this on the user mux (4097).
// Auth: Basic auth with the workspace password (same pattern as
// workflowDeleteSessionHandler). The NetworkPolicy is the primary gate
// (only the API pod can reach 4097); Basic auth is belt-and-suspenders.
//
// Port denylist: 4096/4097/4098 (agent surfaces) + privileged ports (<1024).
// The workspace owner already has equivalent localhost reachability via the
// terminal proxy (Epic 14) — the denylist is not the security boundary.

import (
	"fmt"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strconv"
	"strings"
)

var devPreviewDeniedPorts = map[int]string{
	4096: "opencode (4096)",
	4097: "agentd user mux (4097)",
	4098: "agentd admin mux (4098)",
}

func devPreviewHandler(password string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !checkBasicAuth(r, password) {
			rejectUnauthorized(w)
			return
		}

		portStr := strings.TrimPrefix(r.URL.Path, "/v1/dev-preview/")
		if idx := strings.Index(portStr, "/"); idx >= 0 {
			portStr = portStr[:idx]
		}

		port, err := strconv.Atoi(portStr)
		if err != nil {
			http.Error(w, "port must be numeric", http.StatusBadRequest)
			return
		}

		if port < 1 || port > 65535 {
			http.Error(w, fmt.Sprintf("port out of range: %d", port), http.StatusBadRequest)
			return
		}

		if port < 1024 {
			http.Error(w, fmt.Sprintf("privileged port denied: %d", port), http.StatusBadRequest)
			return
		}

		if reason, denied := devPreviewDeniedPorts[port]; denied {
			http.Error(w, fmt.Sprintf("port denied: %s", reason), http.StatusBadRequest)
			return
		}

		host := "localhost:" + strconv.Itoa(port)
		target := &url.URL{
			Scheme: "http",
			Host:   host,
		}

		stripPrefix := "/v1/dev-preview/" + portStr

		proxy := &httputil.ReverseProxy{
			Rewrite: func(r *httputil.ProxyRequest) {
				r.SetURL(target)
				r.Out.Host = host
				if strings.HasPrefix(r.Out.URL.Path, stripPrefix) {
					r.Out.URL.Path = strings.TrimPrefix(r.Out.URL.Path, stripPrefix)
					if r.Out.URL.Path == "" {
						r.Out.URL.Path = "/"
					}
				}
				// Strip the agentd Basic auth credential — the dev server
				// has no use for it and shouldn't see it.
				r.Out.Header.Del("Authorization")
			},
			ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
				http.Error(w, "dev server unreachable on port "+strconv.Itoa(port), http.StatusBadGateway)
			},
		}

		proxy.ServeHTTP(w, r)
	}
}
