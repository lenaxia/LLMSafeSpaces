// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package imagefactory

import (
	"context"
	_ "embed"
	"fmt"
	"os"

	"gopkg.in/yaml.v3"
)

//go:embed catalog.seed.yaml
var seedFile []byte

// SeedCatalogData is the parsed structure of catalog.seed.yaml.
type SeedCatalogData struct {
	Architectures []string             `json:"architectures" yaml:"architectures"`
	Bases         []Base               `json:"bases" yaml:"bases"`
	Extensions    []SeedExtensionEntry `json:"extensions" yaml:"extensions"`
}

// SeedExtensionEntry is one extension in the seed file. The YAML uses
// camelCase for fileSpec (matching the JSON tag on FileSpec).
type SeedExtensionEntry struct {
	ID             string        `json:"id" yaml:"id"`
	Type           ExtensionType `json:"type" yaml:"type"`
	Value          string        `json:"value" yaml:"value"`
	FileSpec       *FileSpec     `json:"fileSpec,omitempty" yaml:"fileSpec,omitempty"`
	SupportedBases []string      `json:"supportedBases" yaml:"supportedBases"`
	Description    string        `json:"description,omitempty" yaml:"description,omitempty"`
}

// ToExtension converts a seed entry to a domain Extension.
func (s SeedExtensionEntry) ToExtension() Extension {
	return Extension{
		ID:             s.ID,
		Type:           s.Type,
		Value:          s.Value,
		FileSpec:       s.FileSpec,
		SupportedBases: s.SupportedBases,
		Description:    s.Description,
	}
}

// SeedCatalogStore is the subset of the DB store needed to seed the catalog.
type SeedCatalogStore interface {
	SetPlatformConfig(ctx context.Context, pc PlatformConfig) error
	UpsertBase(ctx context.Context, b Base) error
	GetExtension(ctx context.Context, id string) (Extension, error)
	PublishExtension(ctx context.Context, e Extension) error
}

// LoadSeed parses catalog.seed.yaml (embedded via go:embed). If an env
// var CATALOG_SEED_PATH is set, loads from that file instead (operator
// override). Returns the parsed seed data.
func LoadSeed() (SeedCatalogData, error) {
	data := seedFile
	if path := os.Getenv("CATALOG_SEED_PATH"); path != "" {
		//nolint:gosec // G304: path is operator-controlled (env var), not user input
		raw, err := os.ReadFile(path)
		if err != nil {
			return SeedCatalogData{}, fmt.Errorf("load seed from %s: %w", path, err)
		}
		data = raw
	}
	var seed SeedCatalogData
	if err := yaml.Unmarshal(data, &seed); err != nil {
		return SeedCatalogData{}, fmt.Errorf("load seed: parse yaml: %w", err)
	}
	return seed, nil
}

// SeedCatalog idempotently upserts the seed data into the store. For each
// extension: if it already exists in the DB, it is NOT overwritten (runtime
// admin changes — retire, review flag — take precedence). Only NEW extensions
// are inserted. A transient DB error on GetExtension propagates (does NOT
// fall through to insert). Bases are always upserted (they're version-pinned
// and immutable per version).
//
// Note: if a seed extension's value changes (e.g. playwright-deps package
// list), the existing row is NOT updated — the operator must retire the old
// extension and let the new one be inserted on next boot. This is intentional:
// extensions are immutable per design/0046 #7.
func SeedCatalog(ctx context.Context, store SeedCatalogStore) error {
	seed, err := LoadSeed()
	if err != nil {
		return err
	}
	if err := store.SetPlatformConfig(ctx, PlatformConfig{
		Architectures: seed.Architectures,
	}); err != nil {
		return fmt.Errorf("seed: platform config: %w", err)
	}
	for _, b := range seed.Bases {
		if err := store.UpsertBase(ctx, b); err != nil {
			return fmt.Errorf("seed: base %s/%s: %w", b.Name, b.Version, err)
		}
	}
	for _, ext := range seed.Extensions {
		_, err := store.GetExtension(ctx, ext.ID)
		if err == nil {
			continue
		}
		// Distinguish "not found" (insert new) from "DB error" (propagate).
		if !isSeedNotFound(err) {
			return fmt.Errorf("seed: extension %s: check existing: %w", ext.ID, err)
		}
		domainExt := ext.ToExtension()
		if err := store.PublishExtension(ctx, domainExt); err != nil {
			return fmt.Errorf("seed: extension %s: publish: %w", ext.ID, err)
		}
	}
	return nil
}

// isSeedNotFound reports whether the error is a not-found from the store.
// The store's ErrNotFound is in the database package; the seed loader is in
// a different package and can't import it without a cycle. We match on the
// error message instead — the store's sentinel is "not found".
func isSeedNotFound(err error) bool {
	return err != nil && err.Error() == "not found"
}
