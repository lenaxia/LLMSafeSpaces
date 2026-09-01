// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

// Test seam for the US-70.5 demolition: the pod-side batch-body push
// handler is gone and production enters applySecretsBatch through the
// resync pull only. Pipeline-level pins (size budgets, writer rebuild,
// restart metrics, concurrency) still need an HTTP-shaped entry over
// the REAL pipeline — this helper renders the request/response contract
// the deleted push handler exposed, minus everything push-specific.

import (
	"encoding/json"
	"net/http"

	"github.com/lenaxia/llmsafespaces/pkg/agentd/secrets"
)

// applyBatchHandler parses a JSON batch body and drives applySecretsBatch
// as a legacy (unrevisioned) apply, mapping the outcome to the HTTP shape
// the pipeline pins assert on (200 with counts; 500 with the per-entry
// failure reasons; 400 on unparseable input).
func applyBatchHandler(cfg materializeConfig, deps applySecretsDeps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !checkBasicAuthAny(r, deps.ControlPlanePassword, deps.OpencodePassword) {
			rejectUnauthorized(w)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		var batch []secrets.Secret
		if err := json.NewDecoder(r.Body).Decode(&batch); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": "invalid json: " + err.Error()})
			return
		}
		outcome, aErr := applySecretsBatch(r.Context(), cfg, deps, batch, nil)
		if aErr != nil {
			w.WriteHeader(aErr.status)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": aErr.message})
			return
		}
		mat, skip, fail := outcome.result.Counts()
		status := http.StatusOK
		if outcome.result.HasFailures() {
			status = http.StatusInternalServerError
		}
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"reloaded":  mat,
			"skipped":   skip,
			"failed":    fail,
			"results":   outcome.result.Results,
			"restarted": outcome.restarted,
		})
	}
}
