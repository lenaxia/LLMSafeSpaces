// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package imagefactory

import (
	"context"
	"fmt"
	"strings"
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
	// Mirrors the store contract (#936): is_default applies only on
	// INSERT and only when NO default exists (the NOT EXISTS guard).
	key := b.Name + "/" + b.Version
	if existing, ok := f.bases[key]; ok {
		f.bases[key] = Base{Name: b.Name, Version: b.Version, Image: b.Image, Tag: b.Tag, Digest: b.Digest, IsDefault: existing.IsDefault}
		return nil
	}
	hasDefault := false
	for _, e := range f.bases {
		if e.IsDefault {
			hasDefault = true
			break
		}
	}
	if f.bases == nil {
		f.bases = map[string]Base{}
	}
	f.bases[key] = Base{Name: b.Name, Version: b.Version, Image: b.Image, Tag: b.Tag, Digest: b.Digest, IsDefault: b.IsDefault && !hasDefault}
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

// podmanSetIDs is the rootless-podman catalog set: one apt row (engine +
// setuid mapping helpers + compose + docker shim) and five baked config
// files. The set lets workspace users run nested containers without root
// or added capabilities (README-LLM §Nested Containers; validated by the
// S5.7 leg on runc and runsc).
var podmanSetIDs = []string{
	"podman",
	"podman-subuid",
	"podman-subgid",
	"podman-containers-conf",
	"podman-storage-conf",
	"podman-profile",
}

func TestLoadSeed_PodmanSet(t *testing.T) {
	t.Parallel()
	seed, err := LoadSeed()
	require.NoError(t, err)

	byID := map[string]SeedExtensionEntry{}
	for _, ext := range seed.Extensions {
		byID[ext.ID] = ext
	}

	for _, id := range podmanSetIDs {
		ext, ok := byID[id]
		require.True(t, ok, "extension %s should be in seed", id)
		assert.Equal(t, []string{"bookworm"}, ext.SupportedBases, "%s", id)
	}

	apt := byID["podman"]
	assert.Equal(t, ExtensionTypeApt, apt.Type)
	assert.NotContains(t, apt.Value, "\n",
		"podman apt value must be single-line (breaks Dockerfile apt block)")
	for _, pkg := range []string{"podman", "uidmap", "podman-compose", "podman-docker"} {
		assert.Contains(t, apt.Value, pkg,
			"podman apt value must install %s (uidmap = setuid newuidmap/newgidmap; podman-compose = compose front-end; podman-docker = /usr/bin/docker shim so docker-fluent agents just work)", pkg)
	}

	paths := map[string]string{
		"podman-subuid":          "/etc/subuid",
		"podman-subgid":          "/etc/subgid",
		"podman-containers-conf": "/etc/containers/containers.conf",
		"podman-storage-conf":    "/etc/containers/storage.conf",
		"podman-profile":         "/etc/profile.d/podman.sh",
	}
	for id, path := range paths {
		ext := byID[id]
		assert.Equal(t, ExtensionTypeFile, ext.Type, "%s", id)
		require.NotNil(t, ext.FileSpec, "%s", id)
		assert.Equal(t, path, ext.FileSpec.Path, "%s", id)
		assert.NotEmpty(t, ext.Value, "%s", id)
	}

	// The subuid/subgid rows must map the sandbox user (uid 1000, the
	// workspace pod's runAsNonRoot uid) into a disjoint subordinate range —
	// the newuidmap contract that makes unprivileged user namespaces work.
	assert.Equal(t, "sandbox:100000:65536\n", byID["podman-subuid"].Value)
	assert.Equal(t, "sandbox:100000:65536\n", byID["podman-subgid"].Value)
}

// TestSeedCatalog_PodmanSetResolvesAndRenders: the full set resolves
// against the seeded catalog, passes ValidateResolved, and renders a
// Dockerfile where the apt block (Debian's stock /etc/containers defaults)
// precedes the file block (the platform overrides) — the ordering that
// makes the baked containers.conf/storage.conf win.
func TestSeedCatalog_PodmanSetResolvesAndRenders(t *testing.T) {
	t.Parallel()
	store := &fakeSeedStore{}
	require.NoError(t, SeedCatalog(context.Background(), store))

	exts := map[string]Extension{}
	for _, id := range podmanSetIDs {
		ext, err := store.GetExtension(context.Background(), id)
		require.NoError(t, err)
		exts[id] = ext
	}

	rv, err := ResolveSelection(podmanSetIDs, exts, "bookworm")
	require.NoError(t, err)
	require.NoError(t, ValidateResolved(rv))

	base := Base{Name: "bookworm", Version: "2026.09.0", Image: "ghcr.io/lenaxia/llmsafespaces/base", Tag: "2026.09.0"}
	out, err := RenderDockerfile(rv, base)
	require.NoError(t, err)
	mustContain(t, out, "uidmap")
	mustContain(t, out, `"/etc/subuid"`)
	mustContain(t, out, `"/etc/profile.d/podman.sh"`)

	aptIdx := strings.Index(out, "apt-get install")
	fileIdx := strings.Index(out, `"/etc/subuid"`)
	require.GreaterOrEqual(t, aptIdx, 0, "apt block missing")
	require.GreaterOrEqual(t, fileIdx, 0, "file block missing")
	assert.Less(t, aptIdx, fileIdx,
		"apt block must precede the file overrides so baked /etc/containers configs overwrite package defaults")
}

// TestSeedCatalog_BootAfterDefaultMove_NoSecondDefault (#936 C2): the
// full boot path — runtime default moved to a non-seed base, seed row
// deleted, SeedCatalog re-runs — must not mint a second default.
func TestSeedCatalog_BootAfterDefaultMove_NoSecondDefault(t *testing.T) {
	store := &fakeSeedStore{}
	// Pre-state: operator moved the default to tx; bw (seed base) deleted.
	require.NoError(t, store.SeedUpsertBase(context.Background(), Base{Name: "bw", Version: "1.0", Image: "i"}))
	require.NoError(t, store.UpsertBase(context.Background(), Base{Name: "tx", Version: "1.0", Image: "i2", IsDefault: true}))
	require.NoError(t, store.DeleteBase(context.Background(), "bw", "1.0"))

	seed := &SeedCatalogData{Architectures: []string{"linux/amd64"}, Bases: []Base{{Name: "bw", Version: "1.0", Image: "i", IsDefault: true}}}
	require.NoError(t, seedCatalogWith(context.Background(), store, seed))

	defaults := 0
	defName := ""
	for _, b := range store.bases {
		if b.IsDefault {
			defaults++
			defName = b.Name
		}
	}
	assert.Equal(t, 1, defaults, "boot seed after a default move must not create a second default")
	assert.Equal(t, "tx", defName, "the runtime default survives the boot seed")
}

// TestSeedCatalog_FreshInstall_CarriesDefault: empty store → seed's
// default applies (fresh installs still get a default).
func TestSeedCatalog_FreshInstall_CarriesDefault(t *testing.T) {
	store := &fakeSeedStore{}
	seed := &SeedCatalogData{Architectures: []string{"linux/amd64"}, Bases: []Base{{Name: "bw", Version: "1.0", Image: "i", IsDefault: true}}}
	require.NoError(t, seedCatalogWith(context.Background(), store, seed))
	bw, ok := store.bases["bw/1.0"]
	require.True(t, ok)
	assert.True(t, bw.IsDefault, "fresh install: seed default applies")
}

func (f *fakeSeedStore) DeleteBase(_ context.Context, name, version string) error {
	delete(f.bases, name+"/"+version)
	return nil
}
