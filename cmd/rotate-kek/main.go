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
	// --resume-from is a per-table cursor; RotateAll takes none. Silently
	// ignoring the flag would strand pre-cursor rows (PR #1409 review).
	if table == "all" && resumeFrom != "" {
		return fmt.Errorf("--resume-from applies per table; pair it with --table <provider_credentials|api_keys|org_sso_configs|user_keys>")
	}

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
		oldKey := secrets.DeriveServerKey(oldMaster, p)
		if oldKey == nil {
			return fmt.Errorf("old master key is shorter than 32 bytes (got %d)", len(oldMaster))
		}
		newKey := secrets.DeriveServerKey(newMaster, p)
		if newKey == nil {
			return fmt.Errorf("new master key is shorter than 32 bytes (got %d)", len(newMaster))
		}
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
	pgStore, err := newPgRotationStore(dbURL)
	if err != nil {
		return fmt.Errorf("connect to Postgres: %w", err)
	}
	defer pgStore.Close()

	// Connect to Redis for DEK cache flush.
	var redisCacheStore secrets.RotationStore = pgStore
	if redisURL != "" {
		rc, err := newRedisCacheFlusher(redisURL)
		if err != nil {
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
			fmt.Fprintf(os.Stderr, "  %s: processed=%d skipped=%d failed=%d %s\n", tbl, r.Processed, r.Skipped, r.Failed, lastRowIDField(r.LastRowID))
			for _, e := range r.Errors {
				fmt.Fprintf(os.Stderr, "    ERROR %s/%s: %v\n", tbl, e.RowID, e.Error)
			}
			printResumeHint(tbl, r.LastRowID, dryRun)
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
	fmt.Fprintf(os.Stderr, "%s: processed=%d skipped=%d failed=%d %s\n", table, result.Processed, result.Skipped, result.Failed, lastRowIDField(result.LastRowID))
	for _, e := range result.Errors {
		fmt.Fprintf(os.Stderr, "  ERROR %s/%s: %v\n", table, e.RowID, e.Error)
	}
	printResumeHint(table, result.LastRowID, dryRun)
	// The runbook promises the Redis DEK cache is flushed automatically on
	// success — for every invocation shape, including the documented
	// per-table recovery path. RotateAll flushes inside the coordinator; a
	// single-table run flushes here. The flush runs after the walk even
	// with per-row failures (matching RotateAll): flushing is always safe —
	// evicted DEKs are re-derived on demand and still decrypt via the
	// still-mounted old key during the rotation window. With no --redis-url
	// the store's flush is the pg-only no-op.
	if !dryRun {
		if err := redisCacheStore.FlushDEKCache(ctx); err != nil {
			return fmt.Errorf("flush DEK cache: %w", err)
		}
	}
	if result.Failed > 0 {
		return fmt.Errorf("%d rows failed rotation", result.Failed)
	}
	return nil
}

// lastRowIDField renders the last-row-id=<id> report field. In an apply run
// the operator can resume an interruption with --resume-from <id>; in a
// dry-run the value is informational only (see printResumeHint).
func lastRowIDField(id string) string {
	if id == "" {
		return ""
	}
	return "last-row-id=" + id
}

// printResumeHint tells the operator how to continue after this table. For an
// apply run that is the --resume-from recovery path for interruptions
// (helm/KEK-ROTATION.md); for a dry-run nothing was written, so resuming from
// the reported id on the apply run would SKIP every row before it — the hint
// says to re-run plainly instead (already-rotated rows are filtered by
// version, so a plain re-run is safe and complete).
func printResumeHint(table, lastRowID string, dryRun bool) {
	if lastRowID == "" {
		return
	}
	if dryRun {
		fmt.Fprintf(os.Stderr, "  (to apply: re-run without --dry-run and without --resume-from — a plain re-run is safe; --resume-from is only for resuming interrupted apply runs)\n")
		return
	}
	fmt.Fprintf(os.Stderr, "  (to resume this table if interrupted: rotate-kek --table %s --resume-from %s ...)\n", table, lastRowID)
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
