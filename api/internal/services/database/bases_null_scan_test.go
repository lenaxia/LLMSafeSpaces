// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build !race

package database

// bases_null_scan_test.go — production incident 2026-08-26 19:45 UTC:
// GET /api/v1/image-factory/catalog returned 500 ("failed to load
// bases") because an operator-inserted image_factory_bases row carried
// digest NULL and ListBases scans into a plain string (`converting
// NULL to string is unsupported`). The API's own admin handlers never
// write NULL, but the table has no NOT NULL constraint — any direct
// SQL (the operational path this repo's own runbooks use for catalog
// fixes) can poison every catalog read.
//
// Contract pinned here: ListBases/GetBase must tolerate NULL in ANY
// nullable column (digest arriving as ""), so reader availability no
// longer depends on writer discipline. Regression form: a row with
// NULL digest is returned with Digest "" and no error — pre-fix, the
// scan fails.

import (
	"context"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/stretchr/testify/require"
)

func TestListBases_NullDigestTolerated(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer func() { _ = db.Close() }()

	mock.ExpectQuery(`SELECT name, version, image, tag, digest, is_default FROM image_factory_bases`).
		WillReturnRows(sqlmock.NewRows([]string{"name", "version", "image", "tag", "digest", "is_default"}).
			AddRow("bookworm", "0.21.2", "ghcr.io/lenaxia/llmsafespaces/base", "0.21.2", nil, true).
			AddRow("bookworm", "0.8.0", "ghcr.io/lenaxia/llmsafespaces/base", "0.8.0", "", false))

	svc := &Service{DB: db}
	bases, err := svc.ListBases(context.Background())
	require.NoError(t, err, "a NULL digest row must not fail the catalog read (incident 2026-08-26)")
	require.Len(t, bases, 2)
	require.Equal(t, "", bases[0].Digest, "NULL digest reads as empty string — tag-form reference")
	require.Equal(t, "0.8.0", bases[1].Tag)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestGetBase_NullDigestTolerated(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer func() { _ = db.Close() }()

	mock.ExpectQuery(`SELECT name, version, image, tag, digest, is_default FROM image_factory_bases WHERE name`).
		WithArgs("bookworm", "0.21.2").
		WillReturnRows(sqlmock.NewRows([]string{"name", "version", "image", "tag", "digest", "is_default"}).
			AddRow("bookworm", "0.21.2", "ghcr.io/lenaxia/llmsafespaces/base", "0.21.2", nil, true))

	svc := &Service{DB: db}
	b, err := svc.GetBase(context.Background(), "bookworm", "0.21.2")
	require.NoError(t, err)
	require.NotNil(t, b)
	require.Equal(t, "", b.Digest)
	require.NoError(t, mock.ExpectationsWereMet())
}
