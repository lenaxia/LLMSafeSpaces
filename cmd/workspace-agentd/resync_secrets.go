// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

// resync_secrets.go — US-70.3 Part A: POST /v1/resync-secrets on the
// user mux — the notify target and the secrets_resync backend.
//
// The handler runs the v2 conditional bootstrap pull IN-PROCESS: read
// the projected SA token fresh from disk (AC-14 — the kubelet rotates
// it in place for the pod's lifetime), present the on-disk batch
// envelope's manifest hash, and either no-op (304) or apply the fresh
// envelope through applySecretsBatch — the SAME pipeline the
// reload-secrets push uses (materialize → delivery → cache → writer →
// stage → session-aware restart), with the W2 apply-guard refusing any
// seq ≤ the applied anchor.
//
// Integrity (0050 finding 3): a request body is never read, never
// interpreted as a batch. The applied state can only come from the
// authenticated API pull.
//
// Failure doctrine: a failed pull keeps the LAST applied batch
// (last-good); the failure is loud in the response (typed reason) and
// the agentd log, but does NOT fabricate a delivery degrade — the
// reconcile loop (US-70.3 Part B) detects non-convergence instead.

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"sync"
	"time"

	"go.uber.org/zap"

	"github.com/lenaxia/llmsafespaces/pkg/agentd/secrets"
)

const (
	// bootstrapTokenPath is where the projected SA token volume mounts in
	// both agentd modes (controller wires the same MountPath on the
	// bootstrap init, the sidecar, and — US-70.3 — the single-container
	// main container).
	bootstrapTokenPath = "/var/run/bootstrap/token"
	// defaultBootstrapSecretsOut is the single-container batch-file
	// coordinate (the bootstrap subcommand's --out default). Sidecar mode
	// relocates it via LLMSAFESPACE_BOOTSTRAP_SECRETS_OUT to the pod-scoped
	// tmpfs path — the same coordinate the sidecar's boot phase pulls to.
	defaultBootstrapSecretsOut = "/sandbox-cfg/secrets.json"

	resyncMinIntervalEnv     = "LLMSAFESPACES_RESYNC_MIN_INTERVAL"
	resyncDefaultMinInterval = 2 * time.Second

	resyncReasonPullFailed       = "pull_failed"
	resyncReasonPullUnauthorized = "pull_unauthorized"
	resyncReasonApplyFailed      = "apply_failed"
)

func bootstrapTokenPathFromEnv() string {
	return envOrDefault("LLMSAFESPACE_BOOTSTRAP_TOKEN_FILE", bootstrapTokenPath)
}

func bootstrapSecretsOutFromEnv() string {
	return envOrDefault("LLMSAFESPACE_BOOTSTRAP_SECRETS_OUT", defaultBootstrapSecretsOut)
}

// resyncMinIntervalFromEnv resolves the I15 budget: how long after an
// admitted resync the next is refused (429). The notify path, the
// reconcile loop, and the secrets_resync MCP tool share this budget.
// Unparseable values fall back to the default — never to zero.
func resyncMinIntervalFromEnv() time.Duration {
	v := os.Getenv(resyncMinIntervalEnv)
	if v == "" {
		return resyncDefaultMinInterval
	}
	if d, err := time.ParseDuration(v); err == nil && d > 0 {
		return d
	}
	return resyncDefaultMinInterval
}

// resyncDeps bundles the handler's collaborators. reload mirrors the
// reload-secrets deps (same §D1 gate, same restart machinery); now is
// the rate-limiter clock seam (nil → time.Now).
type resyncDeps struct {
	cfg         materializeConfig
	reload      reloadSecretsDeps
	apiURL      string
	workspaceID string
	tokenPath   string
	batchPath   string
	minInterval time.Duration
	now         func() time.Time
}

// resyncSecretsHandler returns the POST /v1/resync-secrets handler.
func resyncSecretsHandler(d resyncDeps) http.HandlerFunc {
	minInterval := d.minInterval
	if minInterval <= 0 {
		minInterval = resyncMinIntervalFromEnv()
	}
	now := d.now
	if now == nil {
		now = time.Now
	}
	var mu sync.Mutex
	var lastAdmitted time.Time

	anchorPath := func() string {
		return revAnchorPath(d.cfg.toPaths().SecretsEnvPath)
	}
	anchorRev := func() string {
		return servedEnvRevAnchor(d.cfg.toPaths().SecretsEnvPath)
	}

	return func(w http.ResponseWriter, r *http.Request) {
		if !checkBasicAuthAny(r, d.reload.ControlPlanePassword, d.reload.OpencodePassword) {
			rejectUnauthorized(w)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}

		mu.Lock()
		if !lastAdmitted.IsZero() && now().Sub(lastAdmitted) < minInterval {
			remaining := minInterval - now().Sub(lastAdmitted)
			mu.Unlock()
			// The remaining floor (I15) is reported so the secrets_resync
			// MCP tool can surface a real retryAfterMs to a loop-spamming
			// agent instead of guessing the configured ceiling.
			w.WriteHeader(http.StatusTooManyRequests)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"status":       "rate_limited",
				"retryAfterMs": remaining.Milliseconds(),
			})
			return
		}
		lastAdmitted = now()
		mu.Unlock()

		// The request body is deliberately never read: this endpoint
		// applies server-built batches only (0050 finding-3).

		respondNotModified := func() {
			_ = json.NewEncoder(w).Encode(map[string]string{
				"status":     "not_modified",
				"appliedRev": anchorRev(),
			})
		}
		respondFailed := func(reason string) {
			w.WriteHeader(http.StatusBadGateway)
			_ = json.NewEncoder(w).Encode(map[string]string{"status": "failed", "reason": reason})
		}

		// applyFetched runs the shared pipeline for a resolved batch.
		// raw is the verbatim envelope bytes to persist first (the 200
		// path); nil applies the on-disk file as-is (the 304 heal path).
		ctx := r.Context()
		applyFetched := func(bf secrets.BatchFile, raw []byte) {
			if bf.Revision != nil {
				if anchor, aErr := readRevAnchor(anchorPath()); aErr == nil && anchor.AppliedSeq > 0 && bf.Revision.Seq <= anchor.AppliedSeq {
					log.Info("resync-secrets: fetched seq not newer than applied; keeping applied state",
						zap.Int64("fetchedSeq", bf.Revision.Seq),
						zap.Int64("appliedSeq", anchor.AppliedSeq))
					respondNotModified()
					return
				}
			}
			if raw != nil {
				if err := atomicWriteSecrets(d.batchPath, raw, 0o600); err != nil {
					log.Error("resync-secrets: batch write failed", zap.Error(err))
					w.WriteHeader(http.StatusInternalServerError)
					_ = json.NewEncoder(w).Encode(map[string]string{"status": "failed", "reason": resyncReasonApplyFailed, "error": err.Error()})
					return
				}
			}
			outcome, aErr := applySecretsBatch(ctx, d.cfg, d.reload, bf.Secrets, bf.Revision)
			if aErr != nil {
				w.WriteHeader(http.StatusInternalServerError)
				_ = json.NewEncoder(w).Encode(map[string]string{"status": "failed", "reason": resyncReasonApplyFailed, "error": aErr.message})
				return
			}
			if outcome.result.HasFailures() {
				mat, _, fail := outcome.result.Counts()
				pErr := fmt.Errorf("%d of %d secret(s) failed to materialize", fail, mat+fail)
				log.Error("resync-secrets: partial apply failure", zap.Error(pErr))
				w.WriteHeader(http.StatusInternalServerError)
				_ = json.NewEncoder(w).Encode(map[string]string{"status": "failed", "reason": resyncReasonApplyFailed, "error": pErr.Error()})
				return
			}
			if bf.Revision != nil {
				// I4: the reported rev is the anchor the apply produced —
				// never the server-advertised value alone. A stuck anchor
				// write means the applied state cannot be certified; fail
				// loudly rather than report a stale rev.
				if anchorRev() != anchorFromSeqHash(bf.Revision.Seq, bf.Revision.ManifestHash) {
					log.Error("resync-secrets: rev anchor did not land after apply")
					w.WriteHeader(http.StatusInternalServerError)
					_ = json.NewEncoder(w).Encode(map[string]string{"status": "failed", "reason": resyncReasonApplyFailed, "error": "rev anchor write failed"})
					return
				}
			}
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"status":     "applied",
				"appliedRev": anchorRev(),
				"restarted":  outcome.restarted,
			})
		}

		token, err := os.ReadFile(d.tokenPath)
		if err != nil {
			log.Warn("resync-secrets: SA token unreadable",
				zap.String("path", d.tokenPath), zap.Error(err))
			respondFailed(resyncReasonPullFailed)
			return
		}

		priorHash, _ := readPriorBatch(d.batchPath)
		payload, notModified, _, _, _, err := fetchBootstrapSecrets(r.Context(), d.apiURL, d.workspaceID, string(token), priorHash)
		if err != nil {
			reason := resyncReasonPullFailed
			if errors.Is(err, errBootstrapUnauthorized) {
				reason = resyncReasonPullUnauthorized
			}
			log.Warn("resync-secrets: pull failed; keeping last applied batch",
				zap.String("reason", reason), zap.Error(err))
			respondFailed(reason)
			return
		}
		if notModified {
			// Crash-window heal: the server is unchanged, but a batch may
			// have been PULLED (file written) without its apply completing —
			// an anchor strictly older than the file's seq is that state.
			// Healed only when the anchor exists: an ABSENT anchor means
			// the live state is a legacy push (the push invalidated it), and
			// applying the stale envelope would revert the push — boot's
			// cache-merge doctrine owns that window instead.
			if bf, lErr := secrets.LoadBatchFile(d.batchPath); lErr == nil && bf.Revision != nil {
				if anchor, aErr := readRevAnchor(anchorPath()); aErr == nil && anchor.AppliedSeq > 0 && bf.Revision.Seq > anchor.AppliedSeq {
					log.Info("resync-secrets: prior envelope was pulled but never applied; applying it",
						zap.Int64("fileSeq", bf.Revision.Seq),
						zap.Int64("appliedSeq", anchor.AppliedSeq))
					applyFetched(bf, nil)
					return
				}
			}
			respondNotModified()
			return
		}

		bf, err := secrets.ParseBatchFile(payload)
		if err != nil {
			log.Warn("resync-secrets: pulled batch unparseable", zap.Error(err))
			respondFailed(resyncReasonPullFailed)
			return
		}
		applyFetched(bf, payload)
	}
}
