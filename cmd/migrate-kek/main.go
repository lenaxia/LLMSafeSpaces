// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

// Command migrate-kek re-encrypts KEK-protected database rows from the local
// static/sealed format to cloud KMS format (zero-downtime, resumable).
// It is the cross-provider dual of rotate-kek (within-provider rotation,
// same format).
//
// Usage:
//
//	migrate-kek --db-url <pg-conn-str> --kms aws \
//	  --master-key-file /path/to/old/master/secret \
//	  --aws-region us-east-1 \
//	  --aws-credentials-file /path/to/credentials \
//	  --aws-key-arn-provider arn:aws:kms:...:key/... \
//	  --aws-key-arn-org arn:aws:kms:...:key/... \
//	  --aws-key-arn-master arn:aws:kms:...:key/...
//
//	migrate-kek --db-url <pg-conn-str> --kms gcp \
//	  --master-key-file /path/to/old/master/secret \
//	  --gcp-credentials-file /path/to/sa.json \
//	  --gcp-key-name-provider projects/.../cryptoKeys/... \
//	  --gcp-key-name-org projects/.../cryptoKeys/... \
//	  --gcp-key-name-master projects/.../cryptoKeys/...
//
// Supports --table, --resume-from, --dry-run, --redis-url.
// See docs/runbooks/migrate-kek.md for the operational workflow.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"strings"

	gcpKMS "cloud.google.com/go/kms/apiv1"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/kms"
	"google.golang.org/api/option"

	"github.com/lenaxia/llmsafespaces/pkg/secrets"
)

func main() {
	var (
		dbURL              string
		masterKeyFile      string
		kmsProvider        string
		awsRegion          string
		awsCredentialsFile string
		awsKeyArnProvider  string
		awsKeyArnOrg       string
		awsKeyArnMaster    string
		gcpCredentialsFile string
		gcpKeyNameProvider string
		gcpKeyNameOrg      string
		gcpKeyNameMaster   string
		table              string
		resumeFrom         string
		redisURL           string
		dryRun             bool
	)
	flag.StringVar(&dbURL, "db-url", "", "PostgreSQL connection string (required)")
	flag.StringVar(&masterKeyFile, "master-key-file", "", "path to the old master KEK file (required)")
	flag.StringVar(&kmsProvider, "kms", "", "target KMS provider: aws or gcp")
	flag.StringVar(&awsRegion, "aws-region", "", "AWS region (required for --kms aws)")
	flag.StringVar(&awsCredentialsFile, "aws-credentials-file", "", "path to AWS credentials file")
	flag.StringVar(&awsKeyArnProvider, "aws-key-arn-provider", "", "AWS KMS key ARN for provider-credentials")
	flag.StringVar(&awsKeyArnOrg, "aws-key-arn-org", "", "AWS KMS key ARN for org-credentials")
	flag.StringVar(&awsKeyArnMaster, "aws-key-arn-master", "", "AWS KMS key ARN for master-kek")
	flag.StringVar(&gcpCredentialsFile, "gcp-credentials-file", "", "path to GCP service-account JSON file")
	flag.StringVar(&gcpKeyNameProvider, "gcp-key-name-provider", "", "GCP KMS key resource name for provider-credentials")
	flag.StringVar(&gcpKeyNameOrg, "gcp-key-name-org", "", "GCP KMS key resource name for org-credentials")
	flag.StringVar(&gcpKeyNameMaster, "gcp-key-name-master", "", "GCP KMS key resource name for master-kek")
	flag.StringVar(&table, "table", "all", "table to migrate: provider_credentials, api_keys, org_sso_configs, or all")
	flag.StringVar(&resumeFrom, "resume-from", "", "resume from this row ID (per table; for interrupted runs)")
	flag.StringVar(&redisURL, "redis-url", "", "Redis connection string (required for DEK cache flush)")
	flag.BoolVar(&dryRun, "dry-run", false, "report counts without writing")
	flag.Parse()

	if err := validate(masterKeyFile, dbURL, kmsProvider, awsRegion, awsCredentialsFile, gcpCredentialsFile, table); err != nil {
		fmt.Fprintf(os.Stderr, "migrate-kek: %v\n", err)
		flag.Usage()
		os.Exit(2)
	}

	if err := run(dbURL, masterKeyFile, kmsProvider, awsRegion, awsCredentialsFile,
		gcpCredentialsFile, awsKeyArnProvider, awsKeyArnOrg, awsKeyArnMaster,
		gcpKeyNameProvider, gcpKeyNameOrg, gcpKeyNameMaster,
		table, resumeFrom, redisURL, dryRun); err != nil {
		fmt.Fprintf(os.Stderr, "migrate-kek: %v\n", err)
		os.Exit(1)
	}
}

func validate(masterKeyFile, dbURL, kmsProvider, awsRegion, awsCredsFile, gcpCredsFile, table string) error {
	if masterKeyFile == "" || dbURL == "" {
		return fmt.Errorf("--master-key-file and --db-url are required")
	}
	if kmsProvider == "" {
		return fmt.Errorf("--kms is required (aws or gcp)")
	}
	switch kmsProvider {
	case "aws":
		if awsRegion == "" || awsCredsFile == "" {
			return fmt.Errorf("--aws-region and --aws-credentials-file are required for --kms aws")
		}
	case "gcp":
		if gcpCredsFile == "" {
			return fmt.Errorf("--gcp-credentials-file is required for --kms gcp")
		}
	default:
		return fmt.Errorf("--kms must be aws or gcp (got %q)", kmsProvider)
	}
	validTables := map[string]bool{"all": true, "provider_credentials": true, "api_keys": true, "org_sso_configs": true}
	if !validTables[table] {
		return fmt.Errorf("--table must be one of: all, provider_credentials, api_keys, org_sso_configs (got %q)", table)
	}
	return nil
}

func run(dbURL, masterKeyFile, kmsProvider, awsRegion, awsCredsFile, gcpCredsFile string,
	awsKeyArnProvider, awsKeyArnOrg, awsKeyArnMaster string,
	gcpKeyNameProvider, gcpKeyNameOrg, gcpKeyNameMaster string,
	table, resumeFrom, redisURL string, dryRun bool,
) error {
	// Load old master key for local fallback provider.
	oldMaster, err := readMasterKeyFile(masterKeyFile)
	if err != nil {
		return fmt.Errorf("reading old master file: %w", err)
	}

	// Build per-purpose source composites (KMS-primary + static-fallback).
	// Build per-purpose target KMS providers.
	sources := make(map[string]secrets.RootKeyProvider, 3)
	targets := make(map[string]secrets.RootKeyProvider, 3)
	purposes := []struct {
		key    string
		awsARN string
		gcpKey string
	}{
		{"provider-credentials", awsKeyArnProvider, gcpKeyNameProvider},
		{"org-credentials", awsKeyArnOrg, gcpKeyNameOrg},
		{"master-kek", awsKeyArnMaster, gcpKeyNameMaster},
	}

	for _, p := range purposes {
		var kmsProv secrets.RootKeyProvider
		switch kmsProvider {
		case "aws":
			if p.awsARN == "" {
				return fmt.Errorf("--aws-key-arn-* flag for purpose %q is required", p.key)
			}
			prov, err := newAWSKMSProvider(awsRegion, awsCredsFile, p.awsARN)
			if err != nil {
				return fmt.Errorf("constructing AWS KMS provider for %s: %w", p.key, err)
			}
			kmsProv = prov
		case "gcp":
			if p.gcpKey == "" {
				return fmt.Errorf("--gcp-key-name-* flag for purpose %q is required", p.key)
			}
			prov, err := newGPCKMSProvider(gcpCredsFile, p.gcpKey)
			if err != nil {
				return fmt.Errorf("constructing GCP KMS provider for %s: %w", p.key, err)
			}
			kmsProv = prov
		}
		targets[p.key] = kmsProv

		// Build the local fallback from the old master key.
		// For the "master-kek" purpose (api_keys + org_sso_configs),
		// the fallback must be multi-version: legacy api_keys rows may
		// be encrypted under "dek-cache"-derived (v1) keys, while
		// current rows use "master-kek"-derived (v2) keys. A
		// single-key fallback would silently fail to decrypt v1 rows.
		var local secrets.RootKeyProvider
		if p.key == "master-kek" {
			dekCacheKey := deriveKey(oldMaster, "dek-cache")
			masterKekKey := deriveKey(oldMaster, "master-kek")
			if dekCacheKey == nil || masterKekKey == nil {
				return fmt.Errorf("deriving keys for purpose %q from old master key", p.key)
			}
			local, err = secrets.NewStaticKeyProviderMultiVersion(2, map[int][]byte{
				1: dekCacheKey,
				2: masterKekKey,
			})
			if err != nil {
				return fmt.Errorf("constructing multi-version local fallback for %s: %w", p.key, err)
			}
		} else {
			purposeKey := deriveKey(oldMaster, p.key)
			if purposeKey == nil {
				return fmt.Errorf("deriving local key for purpose %q from old master key", p.key)
			}
			local, err = secrets.NewStaticKeyProvider(purposeKey)
			if err != nil {
				return fmt.Errorf("constructing local fallback for %s: %w", p.key, err)
			}
		}

		composite, err := secrets.NewCompositeProvider(kmsProv, local)
		if err != nil {
			return fmt.Errorf("constructing source composite for %s: %w", p.key, err)
		}
		sources[p.key] = composite
	}

	// Connect to Postgres.
	pgStore, err := newPgMigrationStore(dbURL)
	if err != nil {
		return fmt.Errorf("connect to Postgres: %w", err)
	}
	defer pgStore.Close()

	var store secrets.MigrationStore = pgStore
	if redisURL != "" {
		rc, err := newRedisCacheFlusher(redisURL)
		if err != nil {
			return fmt.Errorf("connect to Redis: %w", err)
		}
		defer rc.Close()
		store = &compositeMigrationStore{pg: pgStore, redis: rc}
	}

	coord := secrets.NewMigrationCoordinator(store, sources, targets)

	ctx := context.Background()
	if dryRun {
		fmt.Fprintln(os.Stderr, "DRY RUN — no writes will occur")
	}

	if table == "all" {
		results, err := coord.MigrateAll(ctx, dryRun)
		if err != nil {
			return err
		}
		totalProcessed, totalFailed := 0, 0
		for _, tbl := range []string{"provider_credentials", "api_keys", "org_sso_configs"} {
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
			return fmt.Errorf("%d rows failed migration", totalFailed)
		}
		return nil
	}

	result, err := coord.MigrateTable(ctx, table, resumeFrom, dryRun)
	if err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "%s: processed=%d skipped=%d failed=%d\n", table, result.Processed, result.Skipped, result.Failed)
	for _, e := range result.Errors {
		fmt.Fprintf(os.Stderr, "  ERROR %s/%s: %v\n", table, e.RowID, e.Error)
	}
	if result.Failed > 0 {
		return fmt.Errorf("%d rows failed migration", result.Failed)
	}
	return nil
}

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

func newAWSKMSProvider(region, credsFile, keyArn string) (*secrets.AWSKMSProvider, error) {
	cfg, err := config.LoadDefaultConfig(context.Background(),
		config.WithRegion(region),
		config.WithSharedCredentialsFiles([]string{credsFile}),
	)
	if err != nil {
		return nil, err
	}
	return secrets.NewAWSKMSProvider(kms.NewFromConfig(cfg), keyArn), nil
}

func newGPCKMSProvider(credsFile, keyName string) (*secrets.GPCKMSProvider, error) {
	client, err := gcpKMS.NewKeyManagementClient(context.Background(),
		option.WithAuthCredentialsFile(option.ServiceAccount, credsFile),
	)
	if err != nil {
		return nil, err
	}
	return secrets.NewGPCKMSProvider(client, keyName), nil
}
