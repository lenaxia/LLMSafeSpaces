// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package database

import (
	"context"
	"database/sql"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lenaxia/llmsafespaces/api/internal/imagefactory"
)

// newMockService creates a *Service with a sqlmock DB for store unit tests.
// Uses the default regex query matcher (QueryMatcherRegexp) so test cases
// match on a substring/pattern rather than the exact SQL string — keeps the
// tests robust against cosmetic SQL changes (whitespace, column reordering)
// while still asserting the right statement ran.
func newMockService(t *testing.T) (*Service, sqlmock.Sqlmock) {
	t.Helper()
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	return &Service{DB: db}, mock
}

// qAny is a regex that matches any query. Use for store methods whose exact
// SQL the test doesn't care about — it only cares about the args and the
// returned rows.
const qAny = `.+`

func mustJSON(t *testing.T, v interface{}) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	require.NoError(t, err)
	return b
}

// ── Platform config ─────────────────────────────────────────────────────

func TestGetPlatformConfig(t *testing.T) {
	t.Parallel()
	svc, mock := newMockService(t)
	rows := sqlmock.NewRows([]string{"architectures"}).AddRow(`{linux/amd64,linux/arm64}`)
	mock.ExpectQuery(`SELECT architectures FROM image_factory_platform_config WHERE id = 1`).
		WillReturnRows(rows)
	pc, err := svc.GetPlatformConfig(context.Background())
	require.NoError(t, err)
	assert.Equal(t, []string{"linux/amd64", "linux/arm64"}, pc.Architectures)
}

func TestSetPlatformConfig(t *testing.T) {
	t.Parallel()
	svc, mock := newMockService(t)
	mock.ExpectExec(qAny).
		WithArgs(sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 1))
	err := svc.SetPlatformConfig(context.Background(),
		imagefactory.PlatformConfig{Architectures: []string{"linux/amd64"}})
	require.NoError(t, err)
}

// ── Bases ───────────────────────────────────────────────────────────────

func TestGetBase_NotFound(t *testing.T) {
	t.Parallel()
	svc, mock := newMockService(t)
	mock.ExpectQuery(qAny).WillReturnError(sql.ErrNoRows)
	_, err := svc.GetBase(context.Background(), "ghost", "0.0.0")
	assert.ErrorIs(t, err, ErrNotFound)
}

func TestListBases(t *testing.T) {
	t.Parallel()
	svc, mock := newMockService(t)
	rows := sqlmock.NewRows([]string{"name", "version", "image", "tag", "digest", "is_default"}).
		AddRow("bookworm", "0.6.0", "img", "0.6.0", "", true).
		AddRow("trixie", "0.1.0", "img2", "", "sha256:abc", false)
	mock.ExpectQuery(qAny).WillReturnRows(rows)
	bases, err := svc.ListBases(context.Background())
	require.NoError(t, err)
	assert.Len(t, bases, 2)
	assert.Equal(t, "bookworm", bases[0].Name)
	assert.True(t, bases[0].IsDefault)
}

// ── Extensions ──────────────────────────────────────────────────────────

func TestGetExtension_Happy(t *testing.T) {
	t.Parallel()
	svc, mock := newMockService(t)
	rows := sqlmock.NewRows([]string{"id", "type", "value", "file_spec", "supported_bases", "retired", "review_requested", "description"}).
		AddRow("ffmpeg", "apt", "ffmpeg", nil, "{bookworm,trixie}", false, false, "FFmpeg")
	mock.ExpectQuery(qAny).WillReturnRows(rows)
	ext, err := svc.GetExtension(context.Background(), "ffmpeg")
	require.NoError(t, err)
	assert.Equal(t, "ffmpeg", ext.ID)
	assert.Equal(t, imagefactory.ExtensionTypeApt, ext.Type)
	assert.False(t, ext.Retired)
}

func TestPublishExtension_WithFileSpec(t *testing.T) {
	t.Parallel()
	svc, mock := newMockService(t)
	mock.ExpectExec(qAny).
		WithArgs("motd", "file", "hi\n", sqlmock.AnyArg(), sqlmock.AnyArg(), false, false, "").
		WillReturnResult(sqlmock.NewResult(0, 1))
	err := svc.PublishExtension(context.Background(), imagefactory.Extension{
		ID:             "motd",
		Type:           imagefactory.ExtensionTypeFile,
		Value:          "hi\n",
		FileSpec:       &imagefactory.FileSpec{Path: "/etc/motd"},
		SupportedBases: []string{"bookworm"},
	})
	require.NoError(t, err)
}

// ── Known failures ──────────────────────────────────────────────────────

func TestGetKnownFailure_NotFound(t *testing.T) {
	t.Parallel()
	svc, mock := newMockService(t)
	mock.ExpectQuery(qAny).WillReturnError(sql.ErrNoRows)
	_, err := svc.GetKnownFailure(context.Background(), "s-abc", "bookworm")
	assert.ErrorIs(t, err, ErrNotFound)
}

func TestRecordKnownFailure_Upsert(t *testing.T) {
	t.Parallel()
	svc, mock := newMockService(t)
	mock.ExpectExec(qAny).
		WillReturnResult(sqlmock.NewResult(0, 1))
	err := svc.RecordKnownFailure(context.Background(), imagefactory.KnownFailure{
		SelectionHash: "s-abc",
		Selection:     imagefactory.Selection{"ffmpeg"},
		BaseName:      "bookworm",
		Explanation:   "apt fetch failed",
		Retriable:     true,
	})
	require.NoError(t, err)
}

func TestDeleteKnownFailure_NotFound(t *testing.T) {
	t.Parallel()
	svc, mock := newMockService(t)
	mock.ExpectExec(qAny).WillReturnResult(sqlmock.NewResult(0, 0))
	err := svc.DeleteKnownFailure(context.Background(), "s-ghost", "bookworm")
	assert.ErrorIs(t, err, ErrNotFound)
}

// ── Configs ─────────────────────────────────────────────────────────────

func TestCreateConfig(t *testing.T) {
	t.Parallel()
	svc, mock := newMockService(t)
	ownerID := "user-1"
	rv := imagefactory.ResolvedValues{
		"ffmpeg": {Type: imagefactory.ExtensionTypeApt, Value: "ffmpeg"},
	}
	mock.ExpectQuery(qAny).
		WithArgs("s-hash", "ml-stack", sqlmock.AnyArg(), sqlmock.AnyArg(),
			"bookworm", "0.6.0", "member", ownerID, nil, "building").
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow("cfg-1"))
	cfg := imagefactory.Config{
		Hash:           "s-hash",
		Name:           "ml-stack",
		Selection:      imagefactory.Selection{"ffmpeg"},
		ResolvedValues: rv,
		BaseName:       "bookworm",
		BaseVersion:    "0.6.0",
		Scope:          imagefactory.ScopeMember,
		OwnerID:        &ownerID,
		Status:         imagefactory.StatusBuilding,
	}
	require.NoError(t, svc.CreateConfig(context.Background(), cfg))
}

func TestGetConfig_NotFound(t *testing.T) {
	t.Parallel()
	svc, mock := newMockService(t)
	mock.ExpectQuery(qAny).WillReturnError(sql.ErrNoRows)
	_, err := svc.GetConfig(context.Background(), "cfg-ghost")
	assert.ErrorIs(t, err, ErrNotFound)
}

func TestListVisibleConfigs(t *testing.T) {
	t.Parallel()
	svc, mock := newMockService(t)
	rvJSON := mustJSON(t, imagefactory.ResolvedValues{})
	rows := sqlmock.NewRows(configColumnsForTest()).
		AddRow("cfg-1", "s-a", "my-cfg", "{ffmpeg}", rvJSON,
			"bookworm", "0.6.0", "member", "user-1", nil, "ready").
		AddRow("cfg-2", "s-b", "platform-cfg", "{python313}", rvJSON,
			"bookworm", "0.6.0", "platform", nil, nil, "ready")
	mock.ExpectQuery(qAny).
		WithArgs("user-1", nil).
		WillReturnRows(rows)
	cfgs, err := svc.ListVisibleConfigs(context.Background(), strPtr("user-1"), nil)
	require.NoError(t, err)
	assert.Len(t, cfgs, 2)
	assert.Equal(t, "my-cfg", cfgs[0].Name)
	assert.NotNil(t, cfgs[0].OwnerID)
	assert.Nil(t, cfgs[1].OwnerID)
}

// ── Builds ──────────────────────────────────────────────────────────────

func TestCreateBuild(t *testing.T) {
	t.Parallel()
	svc, mock := newMockService(t)
	rv := imagefactory.ResolvedValues{}
	ghRun := int64(12345)
	mock.ExpectQuery(qAny).
		WithArgs("cfg-1", "s-hash", "bookworm", "0.6.0", sqlmock.AnyArg(),
			sqlmock.AnyArg(), "dispatched", &ghRun, "tok-abc", nil).
		WillReturnRows(sqlmock.NewRows([]string{"id", "started_at"}).
			AddRow("build-1", time.Now()))
	err := svc.CreateBuild(context.Background(), imagefactory.Build{
		ConfigID:       "cfg-1",
		Hash:           "s-hash",
		BaseName:       "bookworm",
		BaseVersion:    "0.6.0",
		ResolvedValues: rv,
		Architectures:  []string{"linux/amd64"},
		Status:         imagefactory.BuildDispatched,
		GHRunID:        &ghRun,
		CallbackToken:  "tok-abc",
	})
	require.NoError(t, err)
}

// GetInFlightOrSuccessfulBuild is the coalescing probe — the most security/
// cost-critical store method. Verify all three branches.
func TestGetInFlightOrSuccessfulBuild_PrefersSucceededOverDispatched(t *testing.T) {
	t.Parallel()
	svc, mock := newMockService(t)
	rvJSON := mustJSON(t, imagefactory.ResolvedValues{})
	mock.ExpectQuery(qAny).
		WillReturnRows(sqlmock.NewRows(buildColumnsForTest()).
			AddRow("b-1", "cfg-1", "s-hash", "bookworm", "0.6.0", rvJSON,
				"{linux/amd64}", "ghcr.io/ws:s-hash-0.6.0", "sha256:ok",
				"succeeded", nil, nil, nil, nil, nil, time.Now(), time.Now()))
	build, err := svc.GetInFlightOrSuccessfulBuild(context.Background(), "s-hash", "0.6.0")
	require.NoError(t, err)
	require.NotNil(t, build)
	assert.Equal(t, imagefactory.BuildSucceeded, build.Status)
}

func TestGetInFlightOrSuccessfulBuild_ReturnsDispatchedIfNoSucceeded(t *testing.T) {
	t.Parallel()
	svc, mock := newMockService(t)
	rvJSON := mustJSON(t, imagefactory.ResolvedValues{})
	ghRun := int64(99)
	mock.ExpectQuery(qAny).
		WillReturnRows(sqlmock.NewRows(buildColumnsForTest()).
			AddRow("b-2", "cfg-1", "s-hash", "bookworm", "0.6.0", rvJSON,
				"{linux/amd64}", nil, nil,
				"dispatched", ghRun, "tok", nil, nil, nil, time.Now(), nil))
	build, err := svc.GetInFlightOrSuccessfulBuild(context.Background(), "s-hash", "0.6.0")
	require.NoError(t, err)
	require.NotNil(t, build)
	assert.Equal(t, imagefactory.BuildDispatched, build.Status)
}

func TestGetInFlightOrSuccessfulBuild_ReturnsNilWhenNone(t *testing.T) {
	t.Parallel()
	svc, mock := newMockService(t)
	mock.ExpectQuery(qAny).WillReturnError(sql.ErrNoRows)
	build, err := svc.GetInFlightOrSuccessfulBuild(context.Background(), "s-hash", "0.6.0")
	require.NoError(t, err, "no rows is not an error — it's a cache miss")
	assert.Nil(t, build)
}

func TestMarkBuildSucceeded(t *testing.T) {
	t.Parallel()
	svc, mock := newMockService(t)
	mock.ExpectExec(qAny).
		WithArgs("ghcr.io/ws:s-1", "sha256:abc", "build-1").
		WillReturnResult(sqlmock.NewResult(0, 1))
	err := svc.MarkBuildSucceeded(context.Background(), "build-1", "ghcr.io/ws:s-1", "sha256:abc")
	require.NoError(t, err)
}

func TestMarkBuildFailed(t *testing.T) {
	t.Parallel()
	svc, mock := newMockService(t)
	mock.ExpectExec(qAny).
		WithArgs("apt: 404", "package not found", "build-1").
		WillReturnResult(sqlmock.NewResult(0, 1))
	err := svc.MarkBuildFailed(context.Background(), "build-1", "apt: 404", "package not found")
	require.NoError(t, err)
}

func TestGetBuildByGHRunID_NotFound(t *testing.T) {
	t.Parallel()
	svc, mock := newMockService(t)
	mock.ExpectQuery(qAny).WillReturnError(sql.ErrNoRows)
	_, err := svc.GetBuildByGHRunID(context.Background(), 999)
	assert.ErrorIs(t, err, ErrNotFound)
}

func TestListRejectedConfigsForFailure(t *testing.T) {
	t.Parallel()
	svc, mock := newMockService(t)
	rvJSON := mustJSON(t, imagefactory.ResolvedValues{})
	rows := sqlmock.NewRows(configColumnsForTest()).
		AddRow("cfg-1", "s-abc", "ml-stack", "{ffmpeg}", rvJSON,
			"bookworm", "0.6.0", "member", "user-1", nil, "rejected")
	mock.ExpectQuery(qAny).
		WithArgs("s-abc", "bookworm").
		WillReturnRows(rows)
	cfgs, err := svc.ListRejectedConfigsForFailure(context.Background(), "s-abc", "bookworm")
	require.NoError(t, err)
	assert.Len(t, cfgs, 1)
	assert.Equal(t, imagefactory.StatusRejected, cfgs[0].Status)
}

// ── helpers ─────────────────────────────────────────────────────────────

// strPtr lives in pg_org_store_test.go (same package).

func configColumnsForTest() []string { return splitCols(configColumns) }
func buildColumnsForTest() []string  { return splitCols(buildColumns) }

func splitCols(commaCols string) []string {
	parts := strings.Split(commaCols, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}
