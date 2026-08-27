// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package imagefactory

import (
	"context"
	"fmt"
	"strings"
)

// Base-sync (release-train tracking): the factory catalog's default base
// must track the deployed platform release without operator action. The
// 2026-08-27 finding: the catalog was five releases stale (bookworm
// 0.21.2 default while the platform ran 0.23.0) because base rows only
// enter via admin upsert — nothing reconciled them. This is the same
// "catalog drifts, nothing reconciles it" class as the 2026-08-25
// incident (worklog 0821).
//
// Semantics:
//
//   - The base image moves as one release train with the platform: the
//     API's own build version identifies the base tag that exists.
//   - Existing builds/configs are immutable: sync only INSERTS a new
//     (name, version) row and moves the default in the same transaction
//     (UpsertBase semantics). Pinned configs keep their pin; the
//     base-update pill remains their migration path.
//   - A row is only added when the tag verifiably exists in the
//     registry (digest resolved) — never publish a catalog row for an
//     image GH Actions cannot pull.

// BaseSyncInput is the decision input for ComputeBaseSync.
type BaseSyncInput struct {
	// PlatformVersion is the deployed API build version (pkg/version),
	// optionally "v"-prefixed. "unknown"/empty/garbage → no-op.
	PlatformVersion string
	// Digest is the registry-resolved digest for the target tag. May be
	// empty at compute time; SyncBaseOnce fills it via the resolver.
	Digest string
	// Bases is the current catalog.
	Bases []Base
}

// ComputeBaseSync returns the base row to upsert (always IsDefault=true)
// when the catalog's default lags the deployed platform release, or nil
// when no action is warranted. The target row's Name and Image derive
// from the current default — the platform releases base images under
// the same repository, only the tag advances.
func ComputeBaseSync(in BaseSyncInput) *Base {
	v := NormalizePlatformVersion(in.PlatformVersion)
	if v == "" {
		return nil
	}
	if CompareVersions(v, MinBaseVersion) < 0 {
		return nil
	}

	var def *Base
	for i := range in.Bases {
		if in.Bases[i].IsDefault {
			def = &in.Bases[i]
			break
		}
	}
	if def == nil {
		// An operator deliberately removed the default (or the catalog is
		// empty) — never mint one from nothing; the seed owns first-boot.
		return nil
	}
	if CompareVersions(v, def.Version) <= 0 {
		return nil
	}

	return &Base{
		Name:      def.Name,
		Version:   v,
		Image:     def.Image,
		Tag:       v,
		Digest:    in.Digest,
		IsDefault: true,
	}
}

// NormalizePlatformVersion strips a leading "v" from a release tag and
// returns "" for values that cannot be a release version.
func NormalizePlatformVersion(v string) string {
	v = strings.TrimPrefix(strings.TrimSpace(v), "v")
	if v == "" {
		return ""
	}
	digit := false
	for _, r := range v {
		if r >= '0' && r <= '9' {
			digit = true
			break
		}
	}
	if !digit {
		return ""
	}
	return v
}

// BaseSyncStore is the subset needed to reconcile the catalog default.
type BaseSyncStore interface {
	ListBases(ctx context.Context) ([]Base, error)
	UpsertBase(ctx context.Context, b Base) error
}

// SyncBaseOnce runs one reconciliation pass: compute the wanted row,
// verify the image exists (digest resolve — also the existence gate),
// upsert + move the default. Returns true when a row was written.
// Errors are retried by the caller's next tick; nothing partial is
// ever committed (UpsertBase is transactional).
func SyncBaseOnce(ctx context.Context, store BaseSyncStore, resolver ManifestResolver, platformVersion string) (bool, error) {
	bases, err := store.ListBases(ctx)
	if err != nil {
		return false, fmt.Errorf("base sync: list bases: %w", err)
	}
	want := ComputeBaseSync(BaseSyncInput{PlatformVersion: platformVersion, Bases: bases})
	if want == nil {
		return false, nil
	}
	digest, err := resolver.ResolveDigest(ctx, want.Image, want.Tag)
	if err != nil {
		return false, fmt.Errorf("base sync: resolve %s:%s: %w", want.Image, want.Tag, err)
	}
	want.Digest = digest
	if err := store.UpsertBase(ctx, *want); err != nil {
		return false, fmt.Errorf("base sync: upsert %s/%s: %w", want.Name, want.Version, err)
	}
	return true, nil
}
