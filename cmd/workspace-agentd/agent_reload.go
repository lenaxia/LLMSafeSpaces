// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"go.uber.org/zap"

	opencode "github.com/lenaxia/llmsafespaces/pkg/agent/opencode"
	"github.com/lenaxia/llmsafespaces/pkg/agentd"
)

// agentReloadHandler triggers an opencode instance dispose. This is the
// only path in the system that calls dispose after Epic 27a ships.
// In-flight LLM streams are aborted; sessions persist in SQLite.
//
// Authentication: Basic auth against the workspace password at entry
// (#848) — the dispose is a disruption primitive. Defense-in-depth on
// top of the NetworkPolicy (which allows only the API server pod to
// reach port 4097); see auth.go.
//
// Idempotent: opencode's InstanceStore short-circuits on already-disposed
// entries; concurrent calls are safe.
// Design 0051 US-3 (§D1): control-plane route. workspacePassword is BOTH
// an accepted credential and the CLIENT credential for the opencode
// dispose call (opencode's own auth — the sidecar retains it as a client
// secret by design). extraAuth carries additional ACCEPTED credentials
// (the agentdPassword in sidecar mode; D6.1 mixed-generation window).
// #848's gate stays; the credential SET widened.
func agentReloadHandler(log *zap.Logger, workspacePassword string, extraAuth ...string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !checkBasicAuthAny(r, append([]string{workspacePassword}, extraAuth...)...) {
			rejectUnauthorized(w)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if r.Method != http.MethodPost {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}

		oc := opencode.NewClient(
			fmt.Sprintf("http://localhost:%d", agentd.AgentPort),
			workspacePassword,
			log,
		)

		ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
		defer cancel()

		if err := oc.DisposeInstance(ctx); err != nil {
			log.Error("agent reload: dispose failed", zap.Error(err))
			w.WriteHeader(http.StatusBadGateway)
			_ = json.NewEncoder(w).Encode(map[string]string{
				"error": "dispose failed: " + err.Error(),
			})
			return
		}

		log.Info("agent reload: dispose succeeded")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]any{"disposed": true})
	}
}
