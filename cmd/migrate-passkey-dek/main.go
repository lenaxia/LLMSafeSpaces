// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

// Command migrate-passkey-dek re-wraps a single user's DEK from the legacy
// password-derived tier (Argon2id(password, salt) → AES-GCM) to the server-KEK
// tier (master RootKeyProvider.Encrypt). It is a one-shot operator tool for the
// cutover that collapses the two-tier DEK model to server-KEK-only.
//
// The script is idempotent: a user whose dek_source is already 'server_kek' is
// skipped. A wrong password fails closed (UnwrapDEK returns an auth-tag error
// before any write), so a botched run leaves the row untouched and can be retried.
//
// The script must be run with the SAME master-KEK material the API uses at boot.
// Provider selection mirrors api/internal/app newRootKeyProvider:
//   - static: reads LLMSAFESPACES_MASTER_SECRET_FILE / LLMSAFESPACES_MASTER_SECRET
//     (same env as the API), derives the "master-kek" purpose key via HKDF-SHA256.
//   - sealed: --sealed-key-file + --passphrase-file (the recommended production
//     self-hosted provider).
//
// Cloud-KMS providers are out of scope — those deployments already provision all
// users on the server-KEK tier (Epic 58), so this one-shot is unnecessary.
//
// Usage:
//
//	# Static provider (reads master secret from the API's env):
//	migrate-passkey-dek --db-url <pg-conn-str> --user alice@example.com
//
//	# Sealed provider:
//	migrate-passkey-dek --db-url <pg-conn-str> --user alice@example.com \
//	  --sealed-key-file /sealed/root-key.bin --passphrase-file /sealed/passphrase
//
// The password is read from --password-file (or the PASSKEY_MIGRATE_PASSWORD env
// var) and never persisted.
package main

import (
	"context"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/lenaxia/llmsafespaces/pkg/secrets"
)

const masterKEKPurpose = "master-kek"

func main() {
	var (
		dbURL          = flag.String("db-url", "", "PostgreSQL connection string (required)")
		userIdent      = flag.String("user", "", "user ID or email to migrate (required)")
		passwordFile   = flag.String("password-file", "", "path to a file holding the user's current password (or set PASSKEY_MIGRATE_PASSWORD)")
		sealedKeyFile  = flag.String("sealed-key-file", "", "sealed root-key file (sealed provider)")
		passphraseFile = flag.String("passphrase-file", "", "passphrase file for the sealed root key (sealed provider)")
		dryRun         = flag.Bool("dry-run", false, "report what would happen without writing")
	)
	flag.Parse()

	if *dbURL == "" || *userIdent == "" {
		fmt.Fprintln(os.Stderr, "migrate-passkey-dek: --db-url and --user are required")
		flag.Usage()
		os.Exit(2)
	}

	password, err := loadPassword(*passwordFile)
	if err != nil {
		fmt.Fprintf(os.Stderr, "migrate-passkey-dek: load password: %v\n", err)
		os.Exit(2)
	}
	if len(password) == 0 {
		fmt.Fprintln(os.Stderr, "migrate-passkey-dek: password is required (--password-file or PASSKEY_MIGRATE_PASSWORD)")
		os.Exit(2)
	}

	provider, err := buildRootKeyProvider(*sealedKeyFile, *passphraseFile)
	if err != nil {
		fmt.Fprintf(os.Stderr, "migrate-passkey-dek: build root key provider: %v\n", err)
		os.Exit(1)
	}

	if err := run(context.Background(), *dbURL, *userIdent, password, provider, *dryRun); err != nil {
		fmt.Fprintf(os.Stderr, "migrate-passkey-dek: %v\n", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, dbURL, userIdent string, password []byte, provider secrets.RootKeyProvider, dryRun bool) error {
	pool, err := pgxpool.New(ctx, dbURL)
	if err != nil {
		return fmt.Errorf("connect to Postgres: %w", err)
	}
	defer pool.Close()

	userID, err := resolveUserID(ctx, pool, userIdent)
	if err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "migrate-passkey-dek: resolved user %q → %s\n", userIdent, userID)

	return migrateUser(ctx, secrets.NewPgKeyStore(pool), userID, password, provider, dryRun)
}

// migrateUser performs the password→server_kek re-wrap against a KeyStore. It is
// the testable core of the migration (no PG dependency). Idempotent: a user
// already on a server-wrapped tier is skipped.
func migrateUser(ctx context.Context, store secrets.KeyStore, userID string, password []byte, provider secrets.RootKeyProvider, dryRun bool) error {
	record, err := store.GetUserKey(ctx, userID)
	if err != nil {
		return fmt.Errorf("get user key: %w", err)
	}
	if record == nil {
		return fmt.Errorf("user %s has no key material (nothing to migrate)", userID)
	}

	fmt.Fprintf(os.Stderr, "migrate-passkey-dek: current dek_source=%q\n", record.DEKSource)

	if record.DEKSource != "password" && record.DEKSource != "" {
		fmt.Fprintf(os.Stderr, "migrate-passkey-dek: dek_source is already %q — nothing to do (idempotent)\n", record.DEKSource)
		return nil
	}
	if len(record.Salt) == 0 {
		return fmt.Errorf("user %s has dek_source=%q but no salt — cannot unwrap with password (corrupt row?)", userID, record.DEKSource)
	}

	// Unwrap the plaintext DEK one last time via the password.
	kek, err := secrets.DeriveKEKFromPassword(password, record.Salt)
	if err != nil {
		return fmt.Errorf("derive KEK from password: %w", err)
	}
	dek, err := secrets.UnwrapDEK(kek, record.WrappedDEK)
	zero(kek)
	if err != nil {
		// Wrong password (AES-GCM auth-tag failure) or corrupt wrap. Fail closed.
		return fmt.Errorf("unwrap DEK with supplied password failed (wrong password or corrupt row): %w", err)
	}

	// Re-wrap under the master RootKeyProvider.
	rewrapped, err := provider.Encrypt(ctx, dek)
	zero(dek)
	if err != nil {
		return fmt.Errorf("re-wrap DEK under master KEK: %w", err)
	}

	if dryRun {
		fmt.Fprintf(os.Stderr, "migrate-passkey-dek: DRY RUN — would flip dek_source %q→\"server_kek\" and re-wrap %d-byte DEK\n", record.DEKSource, len(rewrapped))
		return nil
	}

	// Atomically flip the wrap + dek_source. Salt is nil for server_kek rows.
	// key_version reflects the active master-KEK version (same as fresh
	// server-KEK provisioning), not the prior password-tier version.
	keyVersion := secrets.ActiveVersionOf(provider)
	if err := store.UpdateWrappedDEKAndSource(ctx, userID, rewrapped, nil, keyVersion, "server_kek"); err != nil {
		return fmt.Errorf("commit migration (atomic update): %w", err)
	}

	// Verify the flip landed.
	after, err := store.GetUserKey(ctx, userID)
	if err != nil {
		return fmt.Errorf("re-read after migration: %w", err)
	}
	if after == nil || (after.DEKSource != "server_kek") {
		return fmt.Errorf("post-migration verification failed: dek_source=%q (expected server_kek)", safeDEKSource(after))
	}
	fmt.Fprintf(os.Stderr, "migrate-passkey-dek: OK — dek_source is now \"server_kek\" for user %s\n", userID)
	return nil
}

func safeDEKSource(r *secrets.UserKeyRecord) string {
	if r == nil {
		return "<nil>"
	}
	return r.DEKSource
}

// resolveUserID maps an email to a user ID. If the identifier already looks like
// a UUID (contains only hex + dashes), it is used verbatim. Otherwise it is
// looked up by email.
func resolveUserID(ctx context.Context, pool *pgxpool.Pool, ident string) (string, error) {
	if isUUIDLike(ident) {
		return ident, nil
	}
	var id string
	err := pool.QueryRow(ctx, `SELECT id FROM users WHERE lower(email) = lower($1)`, ident).Scan(&id)
	if err != nil {
		return "", fmt.Errorf("lookup user by email %q: %w", ident, err)
	}
	return id, nil
}

func isUUIDLike(s string) bool {
	// UUIDs are 8-4-4-4-12 hex; a cheap heuristic is "contains a dash and only
	// hex/dash chars". Non-UUID user IDs are possible in theory, but the platform
	// uses UUIDs (see 000001_initial_schema users.id).
	for _, r := range s {
		if !(r >= '0' && r <= '9') && !(r >= 'a' && r <= 'f') && !(r >= 'A' && r <= 'F') && r != '-' {
			return false
		}
	}
	return strings.Contains(s, "-")
}

func loadPassword(path string) ([]byte, error) {
	if env := os.Getenv("PASSKEY_MIGRATE_PASSWORD"); env != "" && path == "" {
		return []byte(env), nil
	}
	if path == "" {
		return nil, nil
	}
	data, err := os.ReadFile(path) //nolint:gosec // operator-supplied path
	if err != nil {
		return nil, err
	}
	return []byte(strings.TrimRight(string(data), "\r\n")), nil
}

// buildRootKeyProvider constructs the master RootKeyProvider, mirroring the API's
// newRootKeyProvider for the self-hosted providers. Preference:
//  1. --sealed-key-file + --passphrase-file (sealed).
//  2. static, from the same env vars the API reads.
func buildRootKeyProvider(sealedKeyFile, passphraseFile string) (secrets.RootKeyProvider, error) {
	if sealedKeyFile != "" || passphraseFile != "" {
		if sealedKeyFile == "" || passphraseFile == "" {
			return nil, errors.New("sealed provider requires both --sealed-key-file and --passphrase-file")
		}
		p, err := secrets.NewSealedKeyProvider(sealedKeyFile, passphraseFile)
		if err != nil {
			return nil, fmt.Errorf("sealed provider: %w", err)
		}
		return p, nil
	}
	master := activeMasterSecret()
	if master == nil {
		return nil, errors.New("no master KEK material: set --sealed-key-file/--passphrase-file or the LLMSAFESPACES_MASTER_SECRET(_FILE) env vars")
	}
	derived, err := secrets.DeriveKEKFromKey(master, []byte("llmsafespaces-server"), masterKEKPurpose)
	zero(master)
	if err != nil {
		return nil, fmt.Errorf("derive master-kek purpose key: %w", err)
	}
	p, err := secrets.NewStaticKeyProvider(derived)
	zero(derived)
	if err != nil {
		return nil, fmt.Errorf("static provider: %w", err)
	}
	return p, nil
}

// activeMasterSecret mirrors api/internal/app activeMasterSecret(): file mount
// first (LLMSAFESPACES_MASTER_SECRET_FILE, colon list, last = active), then the
// value env vars.
func activeMasterSecret() []byte {
	if materials := loadMasterSecretMaterials(); len(materials) > 0 {
		active := materials[len(materials)-1]
		if len(active) >= 32 {
			return active
		}
		return nil
	}
	if raw := os.Getenv("LLMSAFESPACES_MASTER_SECRET"); raw != "" {
		if m := decodeMasterRaw(raw); len(m) >= 32 {
			return m
		}
	}
	if raw := os.Getenv("LLMSAFESPACES_DEK_MASTER_KEY"); raw != "" {
		if m := decodeMasterRaw(raw); len(m) >= 32 {
			return m
		}
	}
	return nil
}

func loadMasterSecretMaterials() [][]byte {
	pathList := os.Getenv("LLMSAFESPACES_MASTER_SECRET_FILE")
	if pathList == "" {
		return nil
	}
	var out [][]byte
	for _, p := range strings.Split(pathList, ":") {
		if p == "" {
			continue
		}
		data, err := os.ReadFile(p) //nolint:gosec // operator-supplied path
		if err != nil {
			continue
		}
		out = append(out, decodeMasterRaw(strings.TrimSpace(string(data))))
	}
	return out
}

func decodeMasterRaw(raw string) []byte {
	if raw == "" {
		return nil
	}
	if decoded, err := hex.DecodeString(raw); err == nil {
		return decoded
	}
	return []byte(raw)
}

func zero(b []byte) {
	for i := range b {
		b[i] = 0
	}
}
