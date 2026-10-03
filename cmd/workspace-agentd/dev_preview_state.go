// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

// dev_preview_state.go — #1617: the space's dev-preview state, live.
//
// WORKSPACE_DEV_PREVIEW_ENABLED is projected by the controller at pod
// creation and can never change afterwards — a toggle mid-pod left
// feature_status and dev_preview_url reporting a stale value until a
// pod recreate. The API now pushes every toggle to the running pod
// (agentpush.PushDevPreviewState → POST /v1/dev-preview-state, the
// user-timezone channel's pattern); the push carries the ABSOLUTE state
// and wins over the boot env. A pod rebuild re-projects the env from
// the CRD, so the two sources converge on every boundary (restart,
// resume, upgrade).

import (
	"encoding/json"
	"io"
	"net/http"
	"os"
	"strconv"
	"sync/atomic"

	"go.uber.org/zap"
)

// devPreviewPushedAtomic holds the API's last live push ("" = never
// pushed — the boot env speaks; "true"/"false" = the pushed state).
var devPreviewPushedAtomic atomic.Value

func init() {
	devPreviewPushedAtomic.Store("")
}

// devPreviewState resolves the space's dev-preview state: the live push
// when one has arrived, the boot-env projection otherwise. reported is
// false only in the controller-skew case (no push AND no env).
func devPreviewState() (active, reported, live bool) {
	if v, _ := devPreviewPushedAtomic.Load().(string); v != "" {
		return v == "true", true, true
	}
	switch os.Getenv("WORKSPACE_DEV_PREVIEW_ENABLED") {
	case "true":
		return true, true, false
	case "false":
		return false, true, false
	}
	return false, false, false
}

// devPreviewStateHandler serves POST /v1/dev-preview-state — the API's
// live push on every SetDevPreview. §D1 carve-out pair (control-plane OR
// workspace password), identical to every other user-mux route the API
// drives.
func devPreviewStateHandler(workspacePassword, agentdPassword string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !checkBasicAuthAny(r, agentdPassword, workspacePassword) {
			rejectUnauthorized(w)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		var body struct {
			Enabled *bool `json:"enabled"`
		}
		if err := json.NewDecoder(io.LimitReader(r.Body, 256)).Decode(&body); err != nil || body.Enabled == nil {
			http.Error(w, "enabled (boolean) is required", http.StatusBadRequest)
			return
		}
		devPreviewPushedAtomic.Store(strconv.FormatBool(*body.Enabled))
		if log != nil {
			log.Debug("dev preview state updated", zap.Bool("enabled", *body.Enabled))
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"status": "ok", "enabled": *body.Enabled})
	}
}
