// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/lenaxia/llmsafespaces/pkg/secrets"
)

func main() {
	oldMasterFile := flag.String("old-master-file", "", "path to the OLD master KEK file (required)")
	newMasterFile := flag.String("new-master-file", "", "path to the NEW master KEK file (required)")
	databaseURL := flag.String("database-url", "", "PostgreSQL connection string (required)")
	redisURL := flag.String("redis-url", "", "Redis connection string (required for DEK cache flush)")
	table := flag.String("table", "all", "table to rotate: provider_credentials, api_keys, org_sso_configs, user_keys, or all")
	resumeFrom := flag.String("resume-from", "", "resume from this row ID (per table; for interrupted runs)")
	targetVersion := flag.Int("target-version", 2, "target key version (default: 2)")
	dryRun := flag.Bool("dry-run", false, "report counts without writing")
	flag.Parse()

	if *oldMasterFile == "" || *newMasterFile == "" {
		fmt.Fprintln(os.Stderr, "rotate-kek: --old-master-file and --new-master-file are required")
		flag.Usage()
		os.Exit(2)
	}
	if *databaseURL == "" {
		fmt.Fprintln(os.Stderr, "rotate-kek: --database-url is required")
		flag.Usage()
		os.Exit(2)
	}

	validTables := map[string]bool{"all": true, "provider_credentials": true, "api_keys": true, "org_sso_configs": true, "user_keys": true}
	if !validTables[*table] {
		fmt.Fprintf(os.Stderr, "rotate-kek: --table must be one of: all, provider_credentials, api_keys, org_sso_configs, user_keys (got %q)\n", *table)
		os.Exit(2)
	}

	if err := run(*oldMasterFile, *newMasterFile, *databaseURL, *redisURL, *table, *resumeFrom, *targetVersion, *dryRun); err != nil {
		fmt.Fprintf(os.Stderr, "rotate-kek: %v\n", err)
		os.Exit(1)
	}
}

func run(oldFile, newFile, dbURL, redisURL, table, resumeFrom string, targetVer int, dryRun bool) error {
	// Load old + new master keys.
	oldMaster, err := readMasterKeyFile(oldFile)
	if err != nil {
		return fmt.Errorf("reading old master file: %w", err)
	}
	newMaster, err := readMasterKeyFile(newFile)
	if err != nil {
		return fmt.Errorf("reading new master file: %w", err)
	}

	// Build old + new provider sets for every purpose string.
	purposes := []string{"provider-credentials", "org-credentials", "master-kek", "dek-cache"}
	oldProviders := make(map[string]secrets.RootKeyProvider, len(purposes))
	newProviders := make(map[string]secrets.RootKeyProvider, len(purposes))
	for _, p := range purposes {
		oldKey := deriveKey(oldMaster, p)
		newKey := deriveKey(newMaster, p)
		op, err := secrets.NewStaticKeyProvider(oldKey)
		if err != nil {
			return fmt.Errorf("old provider for %s: %w", p, err)
		}
		np, err := secrets.NewStaticKeyProvider(newKey)
		if err != nil {
			return fmt.Errorf("new provider for %s: %w", p, err)
		}
		oldProviders[p] = op
		newProviders[p] = np
	}

	// Connect to Postgres.
	pgStore, err := newPgRotationStore(dbURL) // nolint:staticcheck // SA4023 related-info anchor: constructor is a deliberate stub; see the comparison line below
	if err != nil {                           // nolint:staticcheck // SA4023: constructor is a deliberate stub (always errors, "not yet wired"); branch stays defensive until the store is implemented
		return fmt.Errorf("connect to Postgres: %w", err)
	}
	defer pgStore.Close()

	// Connect to Redis for DEK cache flush.
	var redisCacheStore secrets.RotationStore = pgStore
	if redisURL != "" {
		rc, err := newRedisCacheFlusher(redisURL) // nolint:staticcheck // SA4023 related-info anchor: constructor is a deliberate stub; see the comparison line below
		if err != nil {                           // nolint:staticcheck // SA4023: constructor is a deliberate stub (always errors, "not yet wired"); branch stays defensive until the store is implemented
			return fmt.Errorf("connect to Redis: %w", err)
		}
		defer rc.Close()
		redisCacheStore = newCompositeRotationStore(pgStore, rc)
	}

	coord := secrets.NewRotationCoordinator(redisCacheStore, oldProviders, newProviders)

	ctx := context.Background()
	if dryRun {
		fmt.Fprintln(os.Stderr, "DRY RUN — no writes will occur")
	}

	if table == "all" {
		results, err := coord.RotateAll(ctx, targetVer, dryRun)
		if err != nil {
			return err
		}
		totalProcessed := 0
		totalFailed := 0
		for _, tbl := range []string{"provider_credentials", "api_keys", "org_sso_configs", "user_keys"} {
			r := results[tbl]
			totalProcessed += r.Processed
			totalFailed += r.Failed
			fmt.Fprintf(os.Stderr, "  %s: processed=%d skipped=%d failed=%d\n", tbl, r.Processed, r.Skipped, r.Failed)
			for _, e := range r.Errors {
				fmt.Fprintf(os.Stderr, "    ERROR %s/%s: %v\n", tbl, e.RowID, e.Error)
			}
		}
		fmt.Fprintf(os.Stderr, "\nTotal: processed=%d failed=%d\n", totalProcessed, totalFailed)
		if totalFailed > 0 {
			return fmt.Errorf("%d rows failed rotation", totalFailed)
		}
		return nil
	}

	result, err := coord.RotateTable(ctx, table, resumeFrom, targetVer, dryRun)
	if err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "%s: processed=%d skipped=%d failed=%d\n", table, result.Processed, result.Skipped, result.Failed)
	for _, e := range result.Errors {
		fmt.Fprintf(os.Stderr, "  ERROR %s/%s: %v\n", table, e.RowID, e.Error)
	}
	if result.Failed > 0 {
		return fmt.Errorf("%d rows failed rotation", result.Failed)
	}
	return nil
}

// readMasterKeyFile reads a raw key value from a file (hex or raw bytes).
func readMasterKeyFile(path string) ([]byte, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	raw := strings.TrimSpace(string(data))
	if decoded, err := hexDecode(raw); err == nil {
		return decoded, nil
	}
	return []byte(raw), nil
}
