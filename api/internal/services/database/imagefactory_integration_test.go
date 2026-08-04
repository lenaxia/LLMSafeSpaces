// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build integration

package database

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lenaxia/llmsafespaces/api/internal/imagefactory"
	"github.com/lenaxia/llmsafespaces/api/internal/testharness"
)

// These tests exercise the image-factory store against real PostgreSQL via
// the shared integration-test harness. They verify: SQL validity against the
// real migration-000013 schema, column names/types match, the partial unique
// indexes enforce scoped friendly-name uniqueness, the coalescing probe's
// ORDER BY CASE preference works, pq.Array round-trips with text[], and
// CHECK/ON CONFLICT constraints behave. These are exactly the failure modes
// a real-DB round-trip catches and sqlmock cannot (per design/0047).

func newIFService(h *testharness.Harness) *Service {
	return &Service{DB: h.SQLDB()}
}

func TestIntegration_IF_PlatformConfig(t *testing.T) {
	h := testharness.New(t)
	svc := newIFService(h)
	ctx := h.NewContext()

	pc, err := svc.GetPlatformConfig(ctx)
	require.NoError(t, err, "singleton row seeded by migration")
	assert.Contains(t, pc.Architectures, "linux/amd64")

	err = svc.SetPlatformConfig(ctx, imagefactory.PlatformConfig{
		Architectures: []string{"linux/amd64", "linux/arm64"},
	})
	require.NoError(t, err)

	pc, err = svc.GetPlatformConfig(ctx)
	require.NoError(t, err)
	assert.Equal(t, []string{"linux/amd64", "linux/arm64"}, pc.Architectures)
}

func TestIntegration_IF_Bases_CRUD(t *testing.T) {
	h := testharness.New(t)
	svc := newIFService(h)
	ctx := h.NewContext()

	err := svc.UpsertBase(ctx, imagefactory.Base{
		Name: "bookworm", Version: "0.6.0", Image: "ghcr.io/acme/base", Tag: "0.6.0", IsDefault: true,
	})
	require.NoError(t, err)

	b, err := svc.GetBase(ctx, "bookworm", "0.6.0")
	require.NoError(t, err)
	assert.Equal(t, "ghcr.io/acme/base", b.Image)
	assert.True(t, b.IsDefault)

	bases, err := svc.ListBases(ctx)
	require.NoError(t, err)
	assert.GreaterOrEqual(t, len(bases), 1)

	_, err = svc.GetBase(ctx, "ghost", "0.0.0")
	assert.ErrorIs(t, err, ErrNotFound)

	err = svc.DeleteBase(ctx, "bookworm", "0.6.0")
	require.NoError(t, err)

	_, err = svc.GetBase(ctx, "bookworm", "0.6.0")
	assert.ErrorIs(t, err, ErrNotFound)

	err = svc.DeleteBase(ctx, "bookworm", "0.6.0")
	assert.ErrorIs(t, err, ErrNotFound, "double-delete must return ErrNotFound")
}

func TestIntegration_IF_Extensions_CRUD(t *testing.T) {
	h := testharness.New(t)
	svc := newIFService(h)
	ctx := h.NewContext()

	err := svc.PublishExtension(ctx, imagefactory.Extension{
		ID:             "integ-ffmpeg",
		Type:           imagefactory.ExtensionTypeApt,
		Value:          "ffmpeg",
		SupportedBases: []string{"bookworm", "trixie"},
		Description:    "test extension",
	})
	require.NoError(t, err)

	ext, err := svc.GetExtension(ctx, "integ-ffmpeg")
	require.NoError(t, err)
	assert.Equal(t, imagefactory.ExtensionTypeApt, ext.Type)
	assert.False(t, ext.Retired)

	exts, err := svc.ListExtensions(ctx, false)
	require.NoError(t, err)
	found := false
	for _, e := range exts {
		if e.ID == "integ-ffmpeg" {
			found = true
		}
	}
	assert.True(t, found, "extension must appear in non-retired list")

	require.NoError(t, svc.RetireExtension(ctx, "integ-ffmpeg"))
	exts, _ = svc.ListExtensions(ctx, false)
	for _, e := range exts {
		if e.ID == "integ-ffmpeg" {
			t.Fatal("retired extension must NOT appear when includeRetired=false")
		}
	}
	exts, _ = svc.ListExtensions(ctx, true)
	found = false
	for _, e := range exts {
		if e.ID == "integ-ffmpeg" {
			found = true
		}
	}
	assert.True(t, found, "retired extension must appear when includeRetired=true")

	require.NoError(t, svc.SetExtensionReviewRequested(ctx, "integ-ffmpeg", true))
	ext, _ = svc.GetExtension(ctx, "integ-ffmpeg")
	assert.True(t, ext.ReviewRequested)
}

func TestIntegration_IF_Extension_FileSpecRoundTrip(t *testing.T) {
	h := testharness.New(t)
	svc := newIFService(h)
	ctx := h.NewContext()

	fs := &imagefactory.FileSpec{Path: "/etc/motd", Mode: "0644"}
	require.NoError(t, svc.PublishExtension(ctx, imagefactory.Extension{
		ID:             "integ-motd",
		Type:           imagefactory.ExtensionTypeFile,
		Value:          "welcome\n",
		FileSpec:       fs,
		SupportedBases: []string{"bookworm"},
	}))

	ext, err := svc.GetExtension(ctx, "integ-motd")
	require.NoError(t, err)
	require.NotNil(t, ext.FileSpec, "file_spec must round-trip through JSONB")
	assert.Equal(t, "/etc/motd", ext.FileSpec.Path)
	assert.Equal(t, "0644", ext.FileSpec.Mode)
}

func TestIntegration_IF_Configs_FriendlyNameScopedUniqueness(t *testing.T) {
	h := testharness.New(t)
	svc := newIFService(h)
	ctx := context.Background()

	rv := imagefactory.ResolvedValues{"ffmpeg": {Type: imagefactory.ExtensionTypeApt, Value: "ffmpeg"}}
	rvJSON, _ := json.Marshal(rv)

	// Two platform-scope configs with the same name must collide (partial unique index).
	_, err := svc.DB.ExecContext(ctx,
		`INSERT INTO image_factory_configs (hash, name, selection, resolved_values, base_name, base_version, scope, status)
		 VALUES ($1, 'ml-stack', $2, $3, 'bookworm', '0.6.0', 'platform', 'building')`,
		"s-aaa", `{ffmpeg}`, rvJSON)
	require.NoError(t, err)

	_, err = svc.DB.ExecContext(ctx,
		`INSERT INTO image_factory_configs (hash, name, selection, resolved_values, base_name, base_version, scope, status)
		 VALUES ($1, 'ml-stack', $2, $3, 'bookworm', '0.6.0', 'platform', 'building')`,
		"s-bbb", `{ffmpeg}`, rvJSON)
	assert.Error(t, err, "same friendly name in platform scope must violate unique index")

	// Same name in member scope for different ownerIDs must be fine.
	owner1 := "11111111-1111-1111-1111-111111111111"
	owner2 := "22222222-2222-2222-2222-222222222222"
	_, err = svc.DB.ExecContext(ctx,
		`INSERT INTO image_factory_configs (hash, name, selection, resolved_values, base_name, base_version, scope, owner_id, status)
		 VALUES ($1, 'my-cfg', $2, $3, 'bookworm', '0.6.0', 'member', $4, 'building')`,
		"s-ccc", `{ffmpeg}`, rvJSON, owner1)
	require.NoError(t, err)
	_, err = svc.DB.ExecContext(ctx,
		`INSERT INTO image_factory_configs (hash, name, selection, resolved_values, base_name, base_version, scope, owner_id, status)
		 VALUES ($1, 'my-cfg', $2, $3, 'bookworm', '0.6.0', 'member', $4, 'building')`,
		"s-ddd", `{ffmpeg}`, rvJSON, owner2)
	require.NoError(t, err, "same friendly name for different member owners must be fine")

	// Same name for the SAME owner must collide.
	_, err = svc.DB.ExecContext(ctx,
		`INSERT INTO image_factory_configs (hash, name, selection, resolved_values, base_name, base_version, scope, owner_id, status)
		 VALUES ($1, 'my-cfg', $2, $3, 'bookworm', '0.6.0', 'member', $4, 'building')`,
		"s-eee", `{ffmpeg}`, rvJSON, owner1)
	assert.Error(t, err, "same friendly name for same member owner must violate unique index")
}

func TestIntegration_IF_Configs_RoundTrip(t *testing.T) {
	h := testharness.New(t)
	svc := newIFService(h)
	ctx := context.Background()

	ownerID := "33333333-3333-3333-3333-333333333333"
	rv := imagefactory.ResolvedValues{
		"ffmpeg":    {Type: imagefactory.ExtensionTypeApt, Value: "ffmpeg"},
		"python313": {Type: imagefactory.ExtensionTypeMise, Value: "python@3.13"},
	}
	cfg := imagefactory.Config{
		Hash:           "s-roundtrip",
		Name:           "rt-cfg",
		Selection:      imagefactory.Selection{"ffmpeg", "python313"},
		ResolvedValues: rv,
		BaseName:       "bookworm",
		BaseVersion:    "0.6.0",
		Scope:          imagefactory.ScopeMember,
		OwnerID:        &ownerID,
		Status:         imagefactory.StatusBuilding,
	}
	require.NoError(t, svc.CreateConfig(ctx, &cfg))

	got, err := svc.GetConfig(ctx, cfg.ID)
	require.NoError(t, err)
	assert.Equal(t, "rt-cfg", got.Name)
	assert.Equal(t, "s-roundtrip", got.Hash)
	assert.Equal(t, imagefactory.ScopeMember, got.Scope)
	require.NotNil(t, got.OwnerID)
	assert.Equal(t, ownerID, *got.OwnerID)
	// resolved_values round-trip through JSONB
	assert.Equal(t, imagefactory.ExtensionTypeApt, got.ResolvedValues["ffmpeg"].Type)
	assert.Equal(t, "python@3.13", got.ResolvedValues["python313"].Value)
	// selection round-trips through pq.Array text[]
	assert.ElementsMatch(t, imagefactory.Selection{"ffmpeg", "python313"}, got.Selection)

	// Status update
	require.NoError(t, svc.SetConfigStatus(ctx, cfg.ID, imagefactory.StatusReady))
	got, _ = svc.GetConfig(ctx, cfg.ID)
	assert.Equal(t, imagefactory.StatusReady, got.Status)
}

func TestIntegration_IF_Builds_CoalescingProbe(t *testing.T) {
	h := testharness.New(t)
	svc := newIFService(h)
	ctx := context.Background()

	// Seed a config to attach builds to.
	ownerID := "44444444-4444-4444-4444-444444444444"
	rv := imagefactory.ResolvedValues{}
	rvJSON, _ := json.Marshal(rv)
	var configID string
	err := svc.DB.QueryRowContext(ctx,
		`INSERT INTO image_factory_configs (hash, name, selection, resolved_values, base_name, base_version, scope, owner_id, status)
		 VALUES ('s-coal', 'coal-cfg', '{}', $1, 'bookworm', '0.6.0', 'member', $2, 'building')
		 RETURNING id`,
		rvJSON, ownerID).Scan(&configID)
	require.NoError(t, err)

	// No build yet → probe returns nil.
	build, err := svc.GetInFlightOrSuccessfulBuild(ctx, "s-coal", "0.6.0")
	require.NoError(t, err)
	assert.Nil(t, build, "no build → nil (cache miss)")

	// Insert a dispatched (in-flight) build.
	ghRun1 := int64(100)
	require.NoError(t, svc.CreateBuild(ctx, &imagefactory.Build{
		ConfigID: configID, Hash: "s-coal", BaseName: "bookworm", BaseVersion: "0.6.0",
		ResolvedValues: rv, Architectures: []string{"linux/amd64"},
		Status: imagefactory.BuildDispatched, GHRunID: &ghRun1, CallbackToken: "tok-1",
	}))

	// Probe returns the in-flight build.
	build, err = svc.GetInFlightOrSuccessfulBuild(ctx, "s-coal", "0.6.0")
	require.NoError(t, err)
	require.NotNil(t, build)
	assert.Equal(t, imagefactory.BuildDispatched, build.Status)

	// Now insert a succeeded build for the same hash+version.
	ghRun2 := int64(200)
	succeededBuild := &imagefactory.Build{
		ConfigID: configID, Hash: "s-coal", BaseName: "bookworm", BaseVersion: "0.6.0",
		ResolvedValues: rv, Architectures: []string{"linux/amd64"},
		Status: imagefactory.BuildSucceeded, GHRunID: &ghRun2, CallbackToken: "tok-2",
	}
	require.NoError(t, svc.CreateBuild(ctx, succeededBuild))
	require.NoError(t, svc.MarkBuildSucceeded(ctx, succeededBuild.ID, "ghcr.io/ws:s-coal-0.6.0", "sha256:succeeded"))

	// Probe must now prefer the succeeded build (succeededBuild) over the
	// dispatched one (the first build, still at status=dispatched).
	build, err = svc.GetInFlightOrSuccessfulBuild(ctx, "s-coal", "0.6.0")
	require.NoError(t, err)
	require.NotNil(t, build)
	assert.Equal(t, imagefactory.BuildSucceeded, build.Status, "probe must prefer succeeded over dispatched")
	assert.Equal(t, succeededBuild.ID, build.ID, "probe must return the succeeded build, not the dispatched one")
	assert.Equal(t, "ghcr.io/ws:s-coal-0.6.0", build.ImageRef)
}

func TestIntegration_IF_KnownFailures_CRUD(t *testing.T) {
	h := testharness.New(t)
	svc := newIFService(h)
	ctx := context.Background()

	kf := imagefactory.KnownFailure{
		SelectionHash: "s-kf-test",
		Selection:     imagefactory.Selection{"ffmpeg"},
		BaseName:      "bookworm",
		Explanation:   "apt: dependency conflict",
		Retriable:     true,
	}
	require.NoError(t, svc.RecordKnownFailure(ctx, kf))

	got, err := svc.GetKnownFailure(ctx, "s-kf-test", "bookworm")
	require.NoError(t, err)
	assert.Equal(t, "apt: dependency conflict", got.Explanation)
	assert.True(t, got.Retriable)

	// Upsert (update existing)
	kf.Explanation = "updated reason"
	require.NoError(t, svc.RecordKnownFailure(ctx, kf))
	got, _ = svc.GetKnownFailure(ctx, "s-kf-test", "bookworm")
	assert.Equal(t, "updated reason", got.Explanation)

	// Toggle retriable
	require.NoError(t, svc.SetKnownFailureRetriable(ctx, "s-kf-test", "bookworm", false))
	got, _ = svc.GetKnownFailure(ctx, "s-kf-test", "bookworm")
	assert.False(t, got.Retriable)

	// List
	failures, err := svc.ListKnownFailures(ctx)
	require.NoError(t, err)
	assert.GreaterOrEqual(t, len(failures), 1)

	// Delete
	require.NoError(t, svc.DeleteKnownFailure(ctx, "s-kf-test", "bookworm"))
	_, err = svc.GetKnownFailure(ctx, "s-kf-test", "bookworm")
	assert.ErrorIs(t, err, ErrNotFound)
}

// TestIntegration_IF_GetLaunchableConfigByHash validates the JOIN query +
// pq.Array scan against real PostgreSQL — the failure modes sqlmock can't
// catch (ambiguous column refs, text[] scan, JOIN cardinality).
func TestIntegration_IF_GetLaunchableConfigByHash(t *testing.T) {
	h := testharness.New(t)
	svc := newIFService(h)
	ctx := context.Background()

	ownerID := "55555555-5555-5555-5555-555555555555"
	rv := imagefactory.ResolvedValues{
		"ffmpeg": {Type: "apt", Value: "ffmpeg"},
	}
	rvJSON, _ := json.Marshal(rv)

	// Seed a Ready config with a successful build.
	var configID string
	err := svc.DB.QueryRowContext(ctx,
		`INSERT INTO image_factory_configs (hash, name, selection, resolved_values, base_name, base_version, scope, owner_id, status)
		 VALUES ('s-launch', 'launch-cfg', '{ffmpeg,python-3.12}', $1, 'bookworm', '0.6.0', 'member', $2, 'ready')
		 RETURNING id`,
		rvJSON, ownerID).Scan(&configID)
	require.NoError(t, err)

	ghRun := int64(300)
	build := &imagefactory.Build{
		ConfigID: configID, Hash: "s-launch", BaseName: "bookworm", BaseVersion: "0.6.0",
		ResolvedValues: rv, Architectures: []string{"linux/amd64"},
		Status: imagefactory.BuildSucceeded, GHRunID: &ghRun, CallbackToken: "tok-l",
	}
	require.NoError(t, svc.CreateBuild(ctx, build))
	require.NoError(t, svc.MarkBuildSucceeded(ctx, build.ID, "ghcr.io/ws:s-launch-0.6.0", "sha256:abc"))

	// Happy path: Ready config with successful build → returns config + image_ref.
	cfg, imageRef, err := svc.GetLaunchableConfigByHash(ctx, "s-launch", imagefactory.ScopeMember, &ownerID, nil)
	require.NoError(t, err)
	assert.Equal(t, "launch-cfg", cfg.Name)
	assert.Equal(t, imagefactory.StatusReady, cfg.Status)
	assert.Equal(t, imagefactory.Selection{"ffmpeg", "python-3.12"}, cfg.Selection, "pq.Array scan must round-trip")
	assert.Equal(t, "ghcr.io/ws:s-launch-0.6.0", imageRef)

	// Wrong owner → ErrNotFound (scope filter works).
	_, _, err = svc.GetLaunchableConfigByHash(ctx, "s-launch", imagefactory.ScopeMember, strPtr("00000000-0000-0000-0000-000000000000"), nil)
	assert.ErrorIs(t, err, ErrNotFound)

	// Unknown hash → ErrNotFound.
	_, _, err = svc.GetLaunchableConfigByHash(ctx, "s-ghost", imagefactory.ScopeMember, &ownerID, nil)
	assert.ErrorIs(t, err, ErrNotFound)

	// Platform scope → ErrNotFound (this is a member-scope config).
	_, _, err = svc.GetLaunchableConfigByHash(ctx, "s-launch", imagefactory.ScopePlatform, nil, nil)
	assert.ErrorIs(t, err, ErrNotFound)
}
