// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

// Package keyrewrap is the US-70.4 login-independent re-wrap reconciler
// (design 0052 §4.5 item 5, issue #1208).
//
// The 2026-08-28 v0.25.1 incident: a June-era user_keys.wrapped_dek row
// the login-gated migration never touched → GetDEKServerSide unwrap
// failure → silent sessionless degrade. A login-gated migration cannot
// converge rows whose owners never return. This service walks user_keys
// on a period and heals those rows without any login:
//
//   - Row unwrap attempt with the CURRENT master provider. Success →
//     healthy, skip.
//   - Failure → recovery, in order: (a) the session-cache DEK walk
//     (KeyService.GetDEKForUser — the K1 Redis / K2 jwt_sessions
//     sources, while they exist; demolition is US-70.5); (b) nothing
//     else. No source → outcome unwrappable_no_source, surfaced via
//     metric + audit + alert (never silently labeled).
//   - Source agreement (W9): a recovered DEK must decrypt at least one
//     existing secret for that user before any re-wrap — defense
//     against a corrupt cache poisoning the durable row. A user with
//     ZERO secrets surfaces unwrappable:no_secret_to_verify and is
//     NEVER healed unverified (W11).
//   - Verify-after-write: the new wrap must round-trip through the
//     provider BEFORE any write; a non-round-tripping wrap is never
//     committed (mirrors healLegacyDEK's discipline).
//   - CAS write: the heal applies only if the row still byte-equals the
//     wrap that was listed; a concurrent legitimate rotation wins, the
//     reconciler backs off (outcome cas_lost — no retry storm).
//   - Retained wrap (W10): the previous wrap is preserved time-boxed
//     (30 days) AS CIPHERTEXT re-wrapped under the CURRENT KEK —
//     retention adds zero plaintext at rest and a bad heal stays
//     reversible with current keys. Rows past the deadline have the
//     previous columns NULLed as part of every pass.
//   - Staged rollout + kill switch: batches walk oldest-first (oldest =
//     highest risk — they predate the most rotation windows); ≥3
//     verify failures in one pass halt further batches this pass and
//     raise llmsafespaces_key_rewrap_halted; the
//     LLMSAFESPACES_KEY_REWRAP_DISABLED env var is a full kill switch.
//
// Delivery coupling: NONE by design. Degraded-DEK workspaces converge
// via US-70.3's reconcile loop after the row heals — the next batch
// build/pull decrypts cleanly; this service never notifies pods.
//
// Config is env-only (no values.yaml surface): period
// (LLMSAFESPACES_KEY_REWRAP_PERIOD), batch size
// (LLMSAFESPACES_KEY_REWRAP_BATCH_SIZE), kill switch
// (LLMSAFESPACES_KEY_REWRAP_DISABLED).
package keyrewrap

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"strconv"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"

	pkginterfaces "github.com/lenaxia/llmsafespaces/pkg/interfaces"
	"github.com/lenaxia/llmsafespaces/pkg/secrets"
)

// Defaults. Period 10m: a mike-class row converges within one period
// with the owner offline (AC). Batch 200: bounds per-pass decrypt work;
// heals move rows to the END of the updated_at walk order, so a pass
// terminates after ceil(N/batch)+1 windows. Retention 30d per W10.
const (
	DefaultPeriod        = 10 * time.Minute
	DefaultBatchSize     = 200
	defaultRetention     = 30 * 24 * time.Hour
	defaultHaltThreshold = 3
)

// Env knobs (env-only config; no values.yaml keys).
const (
	EnvPeriod    = "LLMSAFESPACES_KEY_REWRAP_PERIOD"
	EnvBatchSize = "LLMSAFESPACES_KEY_REWRAP_BATCH_SIZE"
	EnvDisabled  = "LLMSAFESPACES_KEY_REWRAP_DISABLED"
)

// Row outcomes (the llmsafespaces_key_rewrap_rows_total label domain).
const (
	outcomeHealthy             = "healthy"
	outcomeHealed              = "healed"
	outcomeUnwrappableNoSource = "unwrappable_no_source"
	outcomeUnwrappableNoSecret = "unwrappable_no_secret"
	outcomeSourceDisagreement  = "source_disagreement"
	outcomeVerifyFailed        = "verify_failed"
	outcomeCASLost             = "cas_lost"
	outcomeError               = "error"
)

// Audit actions (secret_audit_log.action values).
const (
	auditActionHeal        = "key_rewrap_heal"
	auditActionUnwrappable = "key_rewrap_unwrappable"
	auditActionCASLost     = "key_rewrap_cas_lost"
	auditActionVerifyFail  = "key_rewrap_verify_failed"
)

// Audit metadata reason strings.
const (
	auditReasonNoRecoverySource   = "no_recovery_source"
	auditReasonNoSecretToVerify   = "no_secret_to_verify"
	auditReasonSourceDisagreement = "source_disagreement"
)

// errVerifyHalt is the error-shaped log payload for the halt event.
var errVerifyHalt = errors.New("verify-after-write failures reached the halt threshold")

// Metrics (package-level promauto, the outbox service precedent —
// registered once, safe across Service instances incl. tests).
var (
	rowsTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "llmsafespaces_key_rewrap_rows_total",
		Help: "user_keys rows walked by the re-wrap reconciler, by outcome. outcome=healed plateauing is the convergence signal (US-70.4); verify_failed/source_disagreement/unwrappable_* are the should-be-zero tail.",
	}, []string{"outcome"})

	batchesTotal = promauto.NewCounter(prometheus.CounterOpts{
		Name: "llmsafespaces_key_rewrap_batches_total",
		Help: "batch windows processed by the re-wrap reconciler.",
	})

	haltedGauge = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "llmsafespaces_key_rewrap_halted",
		Help: "1 when the re-wrap reconciler halted batch processing this pass (verify-failure threshold); resets at the next pass.",
	})

	retentionCleanedTotal = promauto.NewCounter(prometheus.CounterOpts{
		Name: "llmsafespaces_key_rewrap_retention_cleaned_total",
		Help: "user_keys rows whose expired retained-wrap columns were NULLed by the pass.",
	})
)

// DEKRecoverer is the recovery-source seam — satisfied by
// *secrets.KeyService (GetDEKForUser: Redis session cache K1, then the
// jwt_sessions walk K2 while that source exists).
type DEKRecoverer interface {
	GetDEKForUser(ctx context.Context, userID string) (dek []byte, jti string, err error)
}

// UserSecretLister is the cheapest existing per-user secret listing —
// satisfied by secrets.SecretStore (PgSecretStore). W9 agreement needs
// one ciphertext the recovered DEK decrypts; user secret cardinalities
// are tens, so the full listing is the right cost/complexity trade.
type UserSecretLister interface {
	ListSecrets(ctx context.Context, userID string) ([]*secrets.UserSecret, error)
}

// Config is the reconciler's tunable surface. Construct via ConfigFromEnv
// in production; tests build it directly.
type Config struct {
	Period    time.Duration
	BatchSize int
	Disabled  bool
	// HaltOnVerifyFailures is the per-pass verify-failure count at which
	// further batches are halted (I11 blast-radius control).
	HaltOnVerifyFailures int
}

// ConfigFromEnv resolves the env-only configuration. Invalid values
// fall back to defaults (the uploads env convention) — EXCEPT the kill
// switch, which fails closed: an unparseable non-empty DISABLED value
// disables the reconciler (a typo must not silently re-enable writes).
func ConfigFromEnv() Config {
	cfg := Config{
		Period:               DefaultPeriod,
		BatchSize:            DefaultBatchSize,
		HaltOnVerifyFailures: defaultHaltThreshold,
	}
	if v := os.Getenv(EnvPeriod); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d > 0 {
			cfg.Period = d
		}
	}
	if v := os.Getenv(EnvBatchSize); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			cfg.BatchSize = n
		}
	}
	if v := os.Getenv(EnvDisabled); v != "" {
		if b, err := strconv.ParseBool(v); err == nil {
			cfg.Disabled = b
		} else {
			cfg.Disabled = true
		}
	}
	return cfg
}

// Service is the re-wrap reconciler. Construct with New; run with Run
// (blocks until ctx cancellation — the jwt_sessions janitor lifecycle
// pattern: started as a background goroutine in app.Run, stopped by the
// app's root-context cancel during Shutdown).
type Service struct {
	store     secrets.ReconcileKeyStore
	recoverer DEKRecoverer
	provider  secrets.RootKeyProvider
	secrets   UserSecretLister
	audit     secrets.AuditWriter
	logger    pkginterfaces.LoggerInterface
	cfg       Config
	// now is the injectable clock (retention deadlines). Nil = time.Now;
	// production never sets it, tests substitute a deterministic clock.
	now func() time.Time
	// preCAS is a test seam invoked immediately before the CAS write —
	// lets tests inject a concurrent legitimate rotation between the
	// listing and the compare-and-swap. Nil in production.
	preCAS func(userID string)
}

// New constructs the reconciler. store, recoverer, and provider are
// required; a nil secret lister makes every heal surface
// unwrappable_no_secret (fail-closed W11); nil audit/logger degrade to
// no-op observability (production always wires both).
func New(
	store secrets.ReconcileKeyStore,
	recoverer DEKRecoverer,
	provider secrets.RootKeyProvider,
	secretLister UserSecretLister,
	audit secrets.AuditWriter,
	logger pkginterfaces.LoggerInterface,
	cfg Config,
) *Service {
	if cfg.Period <= 0 {
		cfg.Period = DefaultPeriod
	}
	if cfg.BatchSize <= 0 {
		cfg.BatchSize = DefaultBatchSize
	}
	if cfg.HaltOnVerifyFailures <= 0 {
		cfg.HaltOnVerifyFailures = defaultHaltThreshold
	}
	return &Service{
		store:     store,
		recoverer: recoverer,
		provider:  provider,
		secrets:   secretLister,
		audit:     audit,
		logger:    logger,
		cfg:       cfg,
	}
}

// setClock installs a deterministic clock (test-only helper; in-package
// tests use it, mirroring KeyService.setClock).
func (s *Service) setClock(now func() time.Time) { s.now = now }

func (s *Service) nowOr() time.Time {
	if s.now != nil {
		return s.now()
	}
	return time.Now()
}

// Run blocks until ctx is canceled: one immediate startup pass (a
// mike-class row heals without waiting a full period), then one pass
// per period. The kill switch returns before any walk.
func (s *Service) Run(ctx context.Context) {
	if s.cfg.Disabled {
		s.warn("keyrewrap: kill switch active (" + EnvDisabled + "); user_keys re-wrap walk disabled")
		return
	}
	s.info("keyrewrap: re-wrap reconciler started",
		"period", s.cfg.Period.String(), "batchSize", s.cfg.BatchSize)
	s.runPass(ctx)
	ticker := time.NewTicker(s.cfg.Period)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.runPass(ctx)
		}
	}
}

// passStats is one pass's accounting (returned so tests assert without
// scraping the metrics registry).
type passStats struct {
	rows             map[string]int
	batches          int
	retentionCleaned int64
	halted           bool
}

// runPass walks user_keys in batches, oldest-first. Verify failures are
// the halt trigger (I11): at HaltOnVerifyFailures the walk stops taking
// further batches; the halted state is logged and surfaced on the
// llmsafespaces_key_rewrap_halted gauge, which resets at the next pass.
func (s *Service) runPass(ctx context.Context) passStats {
	stats := passStats{rows: map[string]int{}}
	haltedGauge.Set(0)

	cleaned, err := s.store.DeleteExpiredRetainedWraps(ctx, s.nowOr())
	if err != nil {
		s.warn("keyrewrap: retention cleanup failed (will retry next pass)", "error", err.Error())
	} else if cleaned > 0 {
		retentionCleanedTotal.Add(float64(cleaned))
		stats.retentionCleaned = cleaned
		s.info("keyrewrap: cleaned expired retained wraps", "count", cleaned)
	}

	verifyFailures := 0
	for offset := 0; ; offset += s.cfg.BatchSize {
		if ctx.Err() != nil {
			return stats
		}
		rows, err := s.store.ListUserKeysForReconcile(ctx, s.cfg.BatchSize, offset)
		if err != nil {
			s.warn("keyrewrap: batch listing failed (pass aborted; will retry next pass)",
				"offset", offset, "error", err.Error())
			return stats
		}
		if len(rows) == 0 {
			return stats
		}
		stats.batches++
		batchesTotal.Inc()

		for _, row := range rows {
			if ctx.Err() != nil {
				return stats
			}
			outcome := s.processRow(ctx, row)
			stats.rows[outcome]++
			rowsTotal.WithLabelValues(outcome).Inc()
			if outcome == outcomeVerifyFailed {
				verifyFailures++
				if verifyFailures >= s.cfg.HaltOnVerifyFailures {
					stats.halted = true
					haltedGauge.Set(1)
					s.error(errVerifyHalt, "keyrewrap: verify-failure threshold reached; halting further batches this pass",
						"verifyFailures", verifyFailures, "nextOffset", offset+len(rows))
				}
			}
		}

		if stats.halted || len(rows) < s.cfg.BatchSize {
			return stats
		}
	}
}

// processRow applies the heal decision tree to one row. Every branch
// returns an outcome label; writes happen ONLY on the healed path (post
// agreement + verify + CAS).
func (s *Service) processRow(ctx context.Context, row secrets.UserKeyReconcileRow) string {
	// Attribute provider decrypts (AuditedProvider reads this context
	// key) so the walk's unwrap attempts are auditable per user.
	ctx = secrets.ContextWithDecryptUser(ctx, row.UserID)

	if _, err := s.provider.Decrypt(ctx, row.WrappedDEK); err == nil {
		return outcomeHealthy
	}

	dek, _, err := s.recoverer.GetDEKForUser(ctx, row.UserID)
	if err != nil {
		// ErrDEKUnavailable is the legitimate "no live source" case
		// (owner offline, no session). Any other error is infrastructure
		// (PG/Redis outage) — count it as a pass error, not a stranded
		// user, so a transient blip does not mint unwrappable audit rows.
		if !errors.Is(err, secrets.ErrDEKUnavailable) {
			s.warn("keyrewrap: DEK recovery errored (row skipped this pass)",
				"userID", row.UserID, "error", err.Error())
			return outcomeError
		}
		s.auditRow(ctx, row.UserID, auditActionUnwrappable, auditReasonNoRecoverySource, row.KeyVersion)
		return outcomeUnwrappableNoSource
	}

	if s.secrets == nil {
		// Fail-closed W11: no listing means no verifiable source.
		s.auditRow(ctx, row.UserID, auditActionUnwrappable, auditReasonNoSecretToVerify, row.KeyVersion)
		return outcomeUnwrappableNoSecret
	}
	userSecrets, err := s.secrets.ListSecrets(ctx, row.UserID)
	if err != nil {
		s.warn("keyrewrap: user secret listing failed (row skipped this pass)",
			"userID", row.UserID, "error", err.Error())
		return outcomeError
	}
	if len(userSecrets) == 0 {
		s.auditRow(ctx, row.UserID, auditActionUnwrappable, auditReasonNoSecretToVerify, row.KeyVersion)
		return outcomeUnwrappableNoSecret
	}
	if !decryptsAnySecret(dek, userSecrets) {
		s.auditRow(ctx, row.UserID, auditActionUnwrappable, auditReasonSourceDisagreement, row.KeyVersion)
		return outcomeSourceDisagreement
	}

	// Verify-after-write: never commit a wrap that does not round-trip
	// under the provider that produced it (design 0052 §4.5).
	newWrap, err := s.provider.Encrypt(ctx, dek)
	if err != nil {
		s.warn("keyrewrap: re-wrap encrypt failed (row untouched)", "userID", row.UserID, "error", err.Error())
		return outcomeError
	}
	verified, verr := s.provider.Decrypt(ctx, newWrap)
	if verr != nil || !bytes.Equal(verified, dek) {
		s.auditRow(ctx, row.UserID, auditActionVerifyFail, "new_wrap_did_not_round_trip", row.KeyVersion)
		return outcomeVerifyFailed
	}

	// Retained wrap (W10): the previous wrap bytes as ciphertext under
	// the CURRENT KEK — reversible with current keys, zero plaintext at
	// rest.
	prevCT, err := s.provider.Encrypt(ctx, row.WrappedDEK)
	if err != nil {
		s.warn("keyrewrap: retained-wrap encrypt failed (row untouched)", "userID", row.UserID, "error", err.Error())
		return outcomeError
	}
	version := secrets.ActiveVersionOf(s.provider)
	previous := &secrets.RetainedWrap{
		Ciphertext: prevCT,
		KEKVersion: version,
		Until:      s.nowOr().Add(defaultRetention),
	}

	if s.preCAS != nil {
		s.preCAS(row.UserID)
	}
	won, err := s.store.CompareAndSwapWrappedDEK(ctx, row.UserID, row.WrappedDEK, newWrap, version, previous)
	if err != nil {
		s.warn("keyrewrap: CAS write failed (row untouched, retried next pass)",
			"userID", row.UserID, "error", err.Error())
		return outcomeError
	}
	if !won {
		s.auditRow(ctx, row.UserID, auditActionCASLost, "concurrent_rotation_won", row.KeyVersion)
		return outcomeCASLost
	}

	s.auditHeal(ctx, row.UserID, row.KeyVersion, version, previous.Until)
	s.info("keyrewrap: healed user_keys row (re-wrapped under current KEK; previous wrap retained)",
		"userID", row.UserID, "fromVersion", row.KeyVersion, "toVersion", version)
	return outcomeHealed
}

// decryptsAnySecret reports whether dek successfully decrypts at least
// one of the user's secret ciphertexts (the W9 source-agreement check).
func decryptsAnySecret(dek []byte, userSecrets []*secrets.UserSecret) bool {
	for _, sec := range userSecrets {
		if len(sec.Ciphertext) == 0 {
			continue
		}
		if _, err := secrets.DecryptSecret(dek, sec.Ciphertext); err == nil {
			return true
		}
	}
	return false
}

// auditRow writes the uniform unwrappable/verify/cas-lost audit shape.
func (s *Service) auditRow(ctx context.Context, userID, action, reason string, fromVersion int) {
	if s.audit == nil {
		return
	}
	meta, _ := json.Marshal(map[string]any{
		"reason":      reason,
		"fromVersion": fromVersion,
		"source":      "keyrewrap",
	})
	_ = s.audit.LogAudit(ctx, &secrets.AuditEntry{
		UserID:    userID,
		Action:    action,
		Metadata:  meta,
		Timestamp: s.nowOr(),
	})
}

// auditHeal records a successful heal with the retention deadline so
// operators can reconstruct the reversal window from the audit trail.
func (s *Service) auditHeal(ctx context.Context, userID string, fromVersion, toVersion int, until time.Time) {
	if s.audit == nil {
		return
	}
	meta, _ := json.Marshal(map[string]any{
		"reason":        "healed",
		"fromVersion":   fromVersion,
		"toVersion":     toVersion,
		"retainedUntil": until.UTC().Format(time.RFC3339),
		"source":        "keyrewrap",
	})
	_ = s.audit.LogAudit(ctx, &secrets.AuditEntry{
		UserID:    userID,
		Action:    auditActionHeal,
		Metadata:  meta,
		Timestamp: s.nowOr(),
	})
}

func (s *Service) warn(msg string, fields ...interface{}) {
	if s.logger != nil {
		s.logger.Warn(msg, fields...)
	}
}

func (s *Service) info(msg string, fields ...interface{}) {
	if s.logger != nil {
		s.logger.Info(msg, fields...)
	}
}

func (s *Service) error(err error, msg string, fields ...interface{}) {
	if s.logger != nil {
		s.logger.Error(msg, err, fields...)
	}
}
