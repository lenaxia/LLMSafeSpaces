// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package secrets

import (
	"context"
	"time"
)

// RetainedWrap is the time-boxed retention record the re-wrap
// reconciler (US-70.4) writes alongside every CAS heal. Ciphertext is
// the row's PREVIOUS wrapped_dek bytes, re-encrypted under the CURRENT
// master KEK (W10): retention adds zero plaintext at rest, and a bad
// heal stays reversible with the keys the deployment already has —
// decrypt Ciphertext with the current provider to recover the old wrap
// bytes, then restore the row by hand (runbook: helm/KEK-ROTATION.md
// class of operator action).
type RetainedWrap struct {
	Ciphertext []byte
	// KEKVersion is the master-KEK version Ciphertext is wrapped under
	// (the provider's active version at heal time).
	KEKVersion int
	// Until is the retention deadline; the reconciler's pass NULLs the
	// previous columns once it passes.
	Until time.Time
}

// UserKeyReconcileRow is one user_keys row as the re-wrap reconciler
// sees it: the active wrap, the retained-wrap shadow columns, and the
// mutable ordering timestamp that drives the oldest-first walk.
type UserKeyReconcileRow struct {
	UserID     string
	KeyVersion int
	// WrappedDEK is the active wrap the current master provider must be
	// able to decrypt; a failure is the reconciler's entry condition.
	WrappedDEK []byte
	// WrappedDEKPrevious is nil unless a prior heal retained a wrap that
	// has not yet passed wrapped_dek_retained_until.
	WrappedDEKPrevious           []byte
	WrappedDEKPreviousKEKVersion *int
	WrappedDEKRetainedUntil      *time.Time
	UpdatedAt                    time.Time
}

// ReconcileKeyStore is the user_keys data-access surface the re-wrap
// reconciler needs beyond the plain KeyStore: batched oldest-first
// listing, compare-and-swap heal writes with retention, and retention
// cleanup. Implemented by PgKeyStore; test doubles live with the
// reconciler service.
type ReconcileKeyStore interface {
	// ListUserKeysForReconcile returns one walk window, ORDER BY
	// updated_at ASC (oldest = highest risk first), user_id ASC as the
	// deterministic tiebreak. offset advances by limit per window; a
	// window returning fewer than limit rows ends the pass.
	ListUserKeysForReconcile(ctx context.Context, limit, offset int) ([]UserKeyReconcileRow, error)

	// CompareAndSwapWrappedDEK conditionally rewrites the row's active
	// wrap: the UPDATE applies only when the row's current wrapped_dek
	// byte-equals expectedWrapped. Returns won=false (nil error) when
	// zero rows matched — a concurrent legitimate rotation won the race
	// and the caller must back off, never retry-storm. previous == nil
	// clears the retention columns; non-nil retains the prior wrap per
	// W10.
	CompareAndSwapWrappedDEK(ctx context.Context, userID string, expectedWrapped, newWrapped []byte, newVersion int, previous *RetainedWrap) (won bool, err error)

	// DeleteExpiredRetainedWraps NULLs the previous-wrap columns of every
	// row whose wrapped_dek_retained_until has passed. The active wrap is
	// untouched (and updated_at is NOT bumped — the walk ordering tracks
	// active-wrap lifecycle, not retention bookkeeping). Returns the
	// number of rows cleaned.
	DeleteExpiredRetainedWraps(ctx context.Context, now time.Time) (int64, error)
}
