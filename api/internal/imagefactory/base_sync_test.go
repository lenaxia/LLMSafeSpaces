// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package imagefactory

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestComputeBaseSync(t *testing.T) {
	catalog := []Base{
		{Name: "bookworm", Version: "0.6.0", Image: "ghcr.io/acme/base", Tag: "0.6.0"},
		{Name: "bookworm", Version: "0.21.2", Image: "ghcr.io/acme/base", Tag: "0.21.2", IsDefault: true},
	}

	tests := []struct {
		name    string
		version string
		bases   []Base
		wantNil bool
		want    *Base
	}{
		{
			name:    "lagging default yields new default row at platform version",
			version: "0.23.0",
			bases:   catalog,
			want: &Base{
				Name: "bookworm", Version: "0.23.0",
				Image: "ghcr.io/acme/base", Tag: "0.23.0",
				Digest: "sha256:abc", IsDefault: true,
			},
		},
		{
			name:    "v-prefixed platform version is normalized",
			version: "v0.23.0",
			bases:   catalog,
			want: &Base{
				Name: "bookworm", Version: "0.23.0",
				Image: "ghcr.io/acme/base", Tag: "0.23.0",
				Digest: "sha256:abc", IsDefault: true,
			},
		},
		{
			name:    "catalog already at platform version is a no-op",
			version: "0.21.2",
			bases:   catalog,
			wantNil: true,
		},
		{
			name:    "catalog ahead of platform version is a no-op (no downgrade)",
			version: "0.20.0",
			bases:   catalog,
			wantNil: true,
		},
		{
			name:    "unknown version string is a no-op",
			version: "unknown",
			bases:   catalog,
			wantNil: true,
		},
		{
			name:    "empty version string is a no-op",
			version: "",
			bases:   catalog,
			wantNil: true,
		},
		{
			name:    "version below MinBaseVersion floor is a no-op",
			version: "0.10.0",
			bases:   catalog,
			wantNil: true,
		},
		{
			name:    "no default row is a no-op (operator deleted it deliberately)",
			version: "0.23.0",
			bases: []Base{
				{Name: "bookworm", Version: "0.21.2", Image: "ghcr.io/acme/base", Tag: "0.21.2"},
			},
			wantNil: true,
		},
		{
			name:    "empty catalog is a no-op (nothing to derive name/image from)",
			version: "0.23.0",
			bases:   nil,
			wantNil: true,
		},
		{
			name:    "existing non-default row at platform version becomes default",
			version: "0.23.0",
			bases: []Base{
				{Name: "bookworm", Version: "0.21.2", Image: "ghcr.io/acme/base", Tag: "0.21.2", IsDefault: true},
				{Name: "bookworm", Version: "0.23.0", Image: "ghcr.io/acme/other", Tag: "0.23.0"},
			},
			want: &Base{
				Name: "bookworm", Version: "0.23.0",
				Image: "ghcr.io/acme/base", Tag: "0.23.0",
				Digest: "sha256:abc", IsDefault: true,
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ComputeBaseSync(BaseSyncInput{
				PlatformVersion: tt.version,
				Digest:          "sha256:abc",
				Bases:           tt.bases,
			})
			if tt.wantNil {
				assert.Nil(t, got)
				return
			}
			require.NotNil(t, got)
			assert.Equal(t, tt.want, got)
		})
	}
}

type fakeSyncStore struct {
	bases    []Base
	upserted []Base
	listErr  error
	upErr    error
}

func (f *fakeSyncStore) ListBases(ctx context.Context) ([]Base, error) {
	return f.bases, f.listErr
}

func (f *fakeSyncStore) UpsertBase(ctx context.Context, b Base) error {
	if f.upErr != nil {
		return f.upErr
	}
	f.upserted = append(f.upserted, b)
	return nil
}

type fakeResolver struct {
	digest string
	err    error
	calls  int
}

func (f *fakeResolver) ResolveDigest(ctx context.Context, repo, tag string) (string, error) {
	f.calls++
	if f.err != nil {
		return "", f.err
	}
	return f.digest, nil
}

func TestSyncBaseOnce(t *testing.T) {
	t.Run("lagging catalog upserts new default with resolved digest", func(t *testing.T) {
		store := &fakeSyncStore{bases: []Base{
			{Name: "bookworm", Version: "0.21.2", Image: "ghcr.io/acme/base", Tag: "0.21.2", IsDefault: true},
		}}
		res := &fakeResolver{digest: "sha256:deadbeef"}
		synced, err := SyncBaseOnce(context.Background(), store, res, "0.23.0")
		require.NoError(t, err)
		assert.True(t, synced)
		require.Len(t, store.upserted, 1)
		assert.Equal(t, "0.23.0", store.upserted[0].Version)
		assert.True(t, store.upserted[0].IsDefault)
		assert.Equal(t, "sha256:deadbeef", store.upserted[0].Digest)
	})

	t.Run("fresh catalog is a no-op without registry calls", func(t *testing.T) {
		store := &fakeSyncStore{bases: []Base{
			{Name: "bookworm", Version: "0.23.0", Image: "ghcr.io/acme/base", Tag: "0.23.0", IsDefault: true},
		}}
		res := &fakeResolver{digest: "sha256:deadbeef"}
		synced, err := SyncBaseOnce(context.Background(), store, res, "0.23.0")
		require.NoError(t, err)
		assert.False(t, synced)
		assert.Empty(t, store.upserted)
		assert.Zero(t, res.calls)
	})

	t.Run("unresolvable tag skips sync (never add a row for an unpublished image)", func(t *testing.T) {
		store := &fakeSyncStore{bases: []Base{
			{Name: "bookworm", Version: "0.21.2", Image: "ghcr.io/acme/base", Tag: "0.21.2", IsDefault: true},
		}}
		res := &fakeResolver{err: errors.New("404 not found")}
		synced, err := SyncBaseOnce(context.Background(), store, res, "0.24.0")
		require.Error(t, err)
		assert.False(t, synced)
		assert.Empty(t, store.upserted)
	})

	t.Run("upsert failure propagates", func(t *testing.T) {
		store := &fakeSyncStore{
			bases: []Base{
				{Name: "bookworm", Version: "0.21.2", Image: "ghcr.io/acme/base", Tag: "0.21.2", IsDefault: true},
			},
			upErr: errors.New("db down"),
		}
		res := &fakeResolver{digest: "sha256:deadbeef"}
		synced, err := SyncBaseOnce(context.Background(), store, res, "0.23.0")
		require.Error(t, err)
		assert.False(t, synced)
	})

	t.Run("list failure propagates", func(t *testing.T) {
		store := &fakeSyncStore{listErr: errors.New("db down")}
		res := &fakeResolver{digest: "sha256:deadbeef"}
		synced, err := SyncBaseOnce(context.Background(), store, res, "0.23.0")
		require.Error(t, err)
		assert.False(t, synced)
	})
}
