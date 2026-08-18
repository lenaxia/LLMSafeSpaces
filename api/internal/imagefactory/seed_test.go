// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package imagefactory

import (
	"context"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type fakeSeedStore struct {
	platformCfg       PlatformConfig
	bases             map[string]Base
	extensions        map[string]Extension
	getExtensionError error // when set, GetExtension returns this instead of checking the map
}

func (f *fakeSeedStore) SetPlatformConfig(_ context.Context, pc PlatformConfig) error {
	f.platformCfg = pc
	return nil
}
func (f *fakeSeedStore) UpsertBase(_ context.Context, b Base) error {
	if f.bases == nil {
		f.bases = map[string]Base{}
	}
	f.bases[b.Name+"/"+b.Version] = b
	return nil
}
func (f *fakeSeedStore) SeedUpsertBase(_ context.Context, b Base) error {
	// Mirrors the store contract: insert-only is_default — an existing
	// row keeps its runtime default (#936).
	key := b.Name + "/" + b.Version
	if existing, ok := f.bases[key]; ok {
		keepDefault := existing.IsDefault
		f.bases[key] = Base{Name: b.Name, Version: b.Version, Image: b.Image, Tag: b.Tag, Digest: b.Digest, IsDefault: keepDefault}
		return nil
	}
	if f.bases == nil {
		f.bases = map[string]Base{}
	}
	f.bases[key] = b
	return nil
}
func (f *fakeSeedStore) GetExtension(_ context.Context, id string) (Extension, error) {
	if f.getExtensionError != nil {
		return Extension{}, f.getExtensionError
	}
	if e, ok := f.extensions[id]; ok {
		return e, nil
	}
	return Extension{}, errNotFoundSeed
}
func (f *fakeSeedStore) PublishExtension(_ context.Context, e Extension) error {
	if f.extensions == nil {
		f.extensions = map[string]Extension{}
	}
	f.extensions[e.ID] = e
	return nil
}

var errNotFoundSeed = &notFoundErr{}

type notFoundErr struct{}

func (e *notFoundErr) Error() string { return "not found" }

func TestLoadSeed_ParsesEmbeddedYAML(t *testing.T) {
	t.Parallel()
	seed, err := LoadSeed()
	require.NoError(t, err)
	assert.NotEmpty(t, seed.Architectures)
	assert.NotEmpty(t, seed.Bases)
	assert.NotEmpty(t, seed.Extensions)

	// Verify specific entries
	assert.Equal(t, "bookworm", seed.Bases[0].Name)
	assert.True(t, seed.Bases[0].IsDefault)

	found := false
	for _, ext := range seed.Extensions {
		if ext.ID == "ffmpeg" {
			assert.Equal(t, ExtensionTypeApt, ext.Type)
			found = true
		}
	}
	assert.True(t, found, "ffmpeg extension should be in seed")

	// Verify R and Julia were added (statistics / data science)
	rFound := false
	juliaFound := false
	for _, ext := range seed.Extensions {
		if ext.ID == "r-base" {
			assert.Equal(t, ExtensionTypeApt, ext.Type)
			rFound = true
		}
		if ext.ID == "julia" {
			assert.Equal(t, ExtensionTypeMise, ext.Type)
			juliaFound = true
		}
	}
	assert.True(t, rFound, "r-base extension should be in seed")
	assert.True(t, juliaFound, "julia extension should be in seed")
}

func TestSeedCatalog_IdempotentUpsert(t *testing.T) {
	t.Parallel()
	store := &fakeSeedStore{}

	// First seed — inserts everything
	err := SeedCatalog(context.Background(), store)
	require.NoError(t, err)

	assert.Equal(t, []string{"linux/amd64", "linux/arm64"}, store.platformCfg.Architectures)
	assert.NotEmpty(t, store.bases, "bases should be seeded")
	assert.NotEmpty(t, store.extensions, "extensions should be seeded")

	// Count extensions
	extCount := len(store.extensions)

	// Modify an extension at runtime (simulate admin retire)
	for id := range store.extensions {
		ext := store.extensions[id]
		ext.Retired = true
		store.extensions[id] = ext
		break
	}

	// Second seed — should NOT overwrite the runtime modification
	err = SeedCatalog(context.Background(), store)
	require.NoError(t, err)

	// Same count (no new extensions — all already exist)
	assert.Equal(t, extCount, len(store.extensions))

	// The runtime-retired extension should still be retired
	for _, ext := range store.extensions {
		if ext.Retired {
			// Found the one we retired — seed didn't reset it
			return
		}
	}
	t.Fatal("runtime-retired extension was overwritten by seed")
}

func TestSeedCatalog_FileExtensionWithFileSpec(t *testing.T) {
	t.Parallel()
	store := &fakeSeedStore{}
	require.NoError(t, SeedCatalog(context.Background(), store))

	motd, ok := store.extensions["motd-welcome"]
	require.True(t, ok, "motd-welcome extension should exist")
	assert.Equal(t, ExtensionTypeFile, motd.Type)
	require.NotNil(t, motd.FileSpec)
	assert.Equal(t, "/etc/motd", motd.FileSpec.Path)
}

func TestSeedCatalog_TransientDBErrorPropagates(t *testing.T) {
	t.Parallel()
	store := &fakeSeedStore{
		getExtensionError: fmt.Errorf("connection refused"),
	}
	err := SeedCatalog(context.Background(), store)
	require.Error(t, err, "transient DB error on GetExtension must propagate")
	assert.Contains(t, err.Error(), "connection refused")
	assert.NotContains(t, err.Error(), "publish",
		"must NOT fall through to PublishExtension on a non-not-found error")
}

func TestLoadSeed_NoDuplicateIDs(t *testing.T) {
	t.Parallel()
	seed, err := LoadSeed()
	require.NoError(t, err)
	seen := map[string]bool{}
	for _, ext := range seed.Extensions {
		assert.False(t, seen[ext.ID], "duplicate extension ID: %s", ext.ID)
		seen[ext.ID] = true
	}
}

func TestLoadSeed_PlaywrightDepsNoTrailingNewline(t *testing.T) {
	t.Parallel()
	seed, err := LoadSeed()
	require.NoError(t, err)
	for _, ext := range seed.Extensions {
		if ext.ID == "playwright-deps" {
			assert.NotContains(t, ext.Value, "\n",
				"playwright-deps value must not contain newlines (breaks Dockerfile apt block)")
			assert.Contains(t, ext.Value, "libnss3")
			return
		}
	}
	t.Fatal("playwright-deps extension not found in seed")
}
