// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package database

import (
	"errors"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/lib/pq"
	"github.com/stretchr/testify/assert"
)

// TestIsUniqueViolation_Drivers (#936): the production pool is pgx
// (pgconn.PgError); the legacy lib/pq type also flows through some
// paths. The driver-agnostic SQLState interface must catch BOTH — the
// original pq-only type assertion silently never matched pgx errors,
// which is exactly what the CI integration run proved.
func TestIsUniqueViolation_Drivers(t *testing.T) {
	pgxE := &pgconn.PgError{Code: "23505", Message: "dup"}
	assert.True(t, isUniqueViolation(pgxE), "pgx unique violation must match")
	assert.True(t, isUniqueViolation(errors.Join(errors.New("wrap"), pgxE)), "wrapped pgx via errors.Join")

	pqE := &pq.Error{Code: "23505"}
	assert.True(t, isUniqueViolation(pqE), "lib/pq unique violation must match")

	assert.False(t, isUniqueViolation(&pgconn.PgError{Code: "23503"}), "FK violation is not unique")
	assert.False(t, isUniqueViolation(errors.New("plain")))
	assert.False(t, isUniqueViolation(nil))
}
