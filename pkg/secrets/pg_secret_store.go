// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package secrets

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	pkginterfaces "github.com/lenaxia/llmsafespaces/pkg/interfaces"
)

// PgSecretStore implements SecretStore using PostgreSQL.
type PgSecretStore struct {
	pool *pgxpool.Pool
}

// NewPgSecretStore creates a new PostgreSQL-backed secret store.
func NewPgSecretStore(pool *pgxpool.Pool) *PgSecretStore {
	return &PgSecretStore{pool: pool}
}

func (s *PgSecretStore) CreateSecret(ctx context.Context, secret *UserSecret) error {
	row := s.pool.QueryRow(ctx,
		`INSERT INTO user_secrets (user_id, name, type, ciphertext, key_version, metadata, global_default)
		 VALUES ($1, $2, $3, $4, $5, $6, $7)
		 RETURNING id, created_at, updated_at`,
		secret.UserID, secret.Name, secret.Type, secret.Ciphertext, secret.KeyVersion, secret.Metadata, secret.GlobalDefault)

	if err := row.Scan(&secret.ID, &secret.CreatedAt, &secret.UpdatedAt); err != nil {
		// 23505 = unique_violation. Wrap as ErrDuplicateSecret so the
		// handler can map to 409 via errors.Is rather than substring
		// matching on the pg error message.
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			return fmt.Errorf("%w: %s", ErrDuplicateSecret, secret.Name)
		}
		return err
	}
	return nil
}

func (s *PgSecretStore) GetSecret(ctx context.Context, userID, secretID string) (*UserSecret, error) {
	row := s.pool.QueryRow(ctx,
		`SELECT id, user_id, name, type, ciphertext, key_version, metadata, global_default, created_at, updated_at
		 FROM user_secrets WHERE id = $1 AND user_id = $2`, secretID, userID)

	var sec UserSecret
	err := row.Scan(&sec.ID, &sec.UserID, &sec.Name, &sec.Type, &sec.Ciphertext, &sec.KeyVersion, &sec.Metadata, &sec.GlobalDefault, &sec.CreatedAt, &sec.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("query user_secrets: %w", err)
	}
	return &sec, nil
}

func (s *PgSecretStore) GetSecretByName(ctx context.Context, userID, name string) (*UserSecret, error) {
	row := s.pool.QueryRow(ctx,
		`SELECT id, user_id, name, type, ciphertext, key_version, metadata, global_default, created_at, updated_at
		 FROM user_secrets WHERE user_id = $1 AND name = $2`, userID, name)

	var sec UserSecret
	err := row.Scan(&sec.ID, &sec.UserID, &sec.Name, &sec.Type, &sec.Ciphertext, &sec.KeyVersion, &sec.Metadata, &sec.GlobalDefault, &sec.CreatedAt, &sec.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("query user_secrets by name: %w", err)
	}
	return &sec, nil
}

func (s *PgSecretStore) ListSecrets(ctx context.Context, userID string) ([]*UserSecret, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT id, user_id, name, type, ciphertext, key_version, metadata, global_default, created_at, updated_at
		 FROM user_secrets WHERE user_id = $1 ORDER BY created_at DESC`, userID)
	if err != nil {
		return nil, fmt.Errorf("list user_secrets: %w", err)
	}
	defer rows.Close()

	var secrets []*UserSecret
	for rows.Next() {
		var sec UserSecret
		if err := rows.Scan(&sec.ID, &sec.UserID, &sec.Name, &sec.Type, &sec.Ciphertext, &sec.KeyVersion, &sec.Metadata, &sec.GlobalDefault, &sec.CreatedAt, &sec.UpdatedAt); err != nil {
			return nil, fmt.Errorf("scan user_secrets: %w", err)
		}
		secrets = append(secrets, &sec)
	}
	return secrets, rows.Err()
}

// ListGlobalDefaultSecrets returns all secrets owned by userID that have
// global_default=true. Used when seeding bindings on workspace creation.
func (s *PgSecretStore) ListGlobalDefaultSecrets(ctx context.Context, userID string) ([]*UserSecret, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT id, user_id, name, type, ciphertext, key_version, metadata, global_default, created_at, updated_at
		 FROM user_secrets WHERE user_id = $1 AND global_default = true ORDER BY created_at DESC`, userID)
	if err != nil {
		return nil, fmt.Errorf("list global default user_secrets: %w", err)
	}
	defer rows.Close()

	var secrets []*UserSecret
	for rows.Next() {
		var sec UserSecret
		if err := rows.Scan(&sec.ID, &sec.UserID, &sec.Name, &sec.Type, &sec.Ciphertext, &sec.KeyVersion, &sec.Metadata, &sec.GlobalDefault, &sec.CreatedAt, &sec.UpdatedAt); err != nil {
			return nil, fmt.Errorf("scan global default user_secrets: %w", err)
		}
		secrets = append(secrets, &sec)
	}
	return secrets, rows.Err()
}

func (s *PgSecretStore) UpdateSecret(ctx context.Context, secret *UserSecret) error {
	_, err := s.pool.Exec(ctx,
		`UPDATE user_secrets SET ciphertext = $1, key_version = $2, metadata = $3, global_default = $4, updated_at = $5
		 WHERE id = $6 AND user_id = $7`,
		secret.Ciphertext, secret.KeyVersion, secret.Metadata, secret.GlobalDefault, secret.UpdatedAt, secret.ID, secret.UserID)
	return err
}

// pgxTxKey is a private context key used to thread an active *pgx.Tx
// through callbacks invoked from inside ReEncryptUserSecrets. The
// PgKeyStore reads this key when present so its UpdateWrappedDEK runs
// inside the same transaction as the secret re-encryption (Bug 9 fix
// in worklog 0094 — the user_keys update and the user_secrets walk
// must be atomic; without this they were two separate statements with
// a partial-failure window between them).
type pgxTxKey struct{}

// withTx returns a context carrying tx for downstream stores to detect.
func withTx(ctx context.Context, tx pgx.Tx) context.Context {
	return context.WithValue(ctx, pgxTxKey{}, tx)
}

// txFromContext returns the tx threaded into ctx, if any.
func txFromContext(ctx context.Context) pgx.Tx {
	if tx, ok := ctx.Value(pgxTxKey{}).(pgx.Tx); ok {
		return tx
	}
	return nil
}

// ReEncryptUserSecrets walks every user_secrets row owned by userID
// inside a single SERIALIZABLE transaction, retrying on serialization
// failure. After the walk completes the commit closure runs in the
// same transaction so callers (KeyService.RotateKeyWithPassword) can
// update related rows (e.g. user_keys.wrapped_dek) atomically. If
// commit returns non-nil the entire transaction rolls back: secrets
// stay encrypted with the old DEK and user_keys stays unchanged.
//
// A row-count cap of maxRotateRows is enforced to prevent a malicious
// or pathological account from holding the rotation transaction open
// indefinitely.
//
// See Bug 9 in worklog 0085 / 0094.
func (s *PgSecretStore) ReEncryptUserSecrets(
	ctx context.Context,
	userID string,
	newKeyVersion int,
	transform func([]byte) ([]byte, error),
	commit func(ctx context.Context) error,
) error {
	const maxRetries = 3
	var err error
	for attempt := 0; attempt < maxRetries; attempt++ {
		err = s.runReEncryptTx(ctx, userID, newKeyVersion, transform, commit)
		if err == nil {
			return nil
		}
		// pgx 5.x exposes serialization_failure as SQLSTATE "40001"; we
		// retry transparently a small bounded number of times. Any
		// other error short-circuits the loop and is returned as-is.
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "40001" {
			continue
		}
		return err
	}
	return fmt.Errorf("rotate-key: serialization failure persisted after %d retries: %w", maxRetries, err)
}

// maxRotateRows is the largest number of secrets a single rotation may
// touch. Set high enough that no realistic user hits it (~16k secrets)
// but low enough that a runaway never holds the rotation tx open for
// minutes.
const maxRotateRows = 16384

func (s *PgSecretStore) runReEncryptTx(
	ctx context.Context,
	userID string,
	newKeyVersion int,
	transform func([]byte) ([]byte, error),
	commit func(ctx context.Context) error,
) error {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// Plain SELECT (no FOR UPDATE) — SERIALIZABLE / SSI uses predicate
	// locks and detects conflicting writes via 40001 abort. Adding
	// FOR UPDATE on top would acquire physical row locks that SSI
	// does not need and increases the abort rate when concurrent
	// secret CRUD touches the same user. The retry loop in the caller
	// handles 40001 transparently.
	rows, err := tx.Query(ctx,
		`SELECT id, ciphertext FROM user_secrets WHERE user_id = $1`, userID)
	if err != nil {
		return fmt.Errorf("select user_secrets: %w", err)
	}

	type pending struct {
		id            string
		newCiphertext []byte
	}
	pendings := make([]pending, 0, 64)
	for rows.Next() {
		if len(pendings) >= maxRotateRows {
			rows.Close()
			return fmt.Errorf("rotate-key: too many secrets (>%d) for user %s", maxRotateRows, userID)
		}
		var id string
		var ct []byte
		if err := rows.Scan(&id, &ct); err != nil {
			rows.Close()
			return fmt.Errorf("scan row: %w", err)
		}
		newCT, err := transform(ct)
		if err != nil {
			rows.Close()
			return fmt.Errorf("transform secret %s: %w", id, err)
		}
		pendings = append(pendings, pending{id: id, newCiphertext: newCT})
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate rows: %w", err)
	}

	now := time.Now()
	for _, p := range pendings {
		if _, err := tx.Exec(ctx,
			`UPDATE user_secrets SET ciphertext = $1, key_version = $2, updated_at = $3
			 WHERE id = $4 AND user_id = $5`,
			p.newCiphertext, newKeyVersion, now, p.id, userID); err != nil {
			return fmt.Errorf("update secret %s: %w", p.id, err)
		}
	}

	// Run caller's atomic-commit hook inside the same tx so user_keys
	// updates land or roll back together with the re-encrypted rows.
	if commit != nil {
		if err := commit(withTx(ctx, tx)); err != nil {
			return fmt.Errorf("commit hook: %w", err)
		}
	}

	return tx.Commit(ctx)
}

func (s *PgSecretStore) DeleteSecret(ctx context.Context, userID, secretID string) error {
	tag, err := s.pool.Exec(ctx,
		`DELETE FROM user_secrets WHERE id = $1 AND user_id = $2`, secretID, userID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("secret %s not found", secretID)
	}
	return nil
}

// SetBindings replaces the binding set for workspaceID atomically.
//
// Concurrency: takes a transaction-scoped advisory lock keyed on the
// workspace ID's hash so two concurrent SetBindings calls for the
// same workspace serialize. We use pg_try_advisory_xact_lock with a
// short retry loop rather than pg_advisory_xact_lock (which would
// block holding a pool connection indefinitely): under a thundering
// herd of concurrent SetBindings on the same workspace, blocking
// would exhaust pool connections and stall the entire API. The
// try-lock fails fast, sleeps a short jittered interval, and retries
// up to setBindingsLockMaxAttempts times.
//
// The lock auto-releases on commit/rollback so a panicking writer
// cannot deadlock future writers. Different workspaces hash to
// different lock numbers and proceed in parallel.
func (s *PgSecretStore) SetBindings(ctx context.Context, workspaceID string, secretIDs []string) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// pg_try_advisory_xact_lock takes a 64-bit signed bigint;
	// hashtext returns int4 which we cast. Try up to N times with a
	// short backoff so we don't hold the pool connection waiting on
	// a blocked lock.
	if err := s.acquireWorkspaceLock(ctx, tx, workspaceID); err != nil {
		return err
	}

	// Remove existing bindings for this workspace
	if _, err := tx.Exec(ctx,
		`DELETE FROM user_secret_bindings WHERE workspace_id = $1`, workspaceID); err != nil {
		return err
	}

	// Insert new bindings
	for _, sid := range secretIDs {
		if _, err := tx.Exec(ctx,
			`INSERT INTO user_secret_bindings (secret_id, workspace_id) VALUES ($1, $2)`,
			sid, workspaceID); err != nil {
			return err
		}
	}

	return tx.Commit(ctx)
}

// setBindingsLockMaxAttempts caps the wait time on the advisory lock.
// At ~10ms per attempt this is roughly 200ms of total wait — plenty
// for normal contention, fast enough to fail under pathological
// thundering-herd loads (where blocking would exhaust the pool).
const setBindingsLockMaxAttempts = 20

func (s *PgSecretStore) acquireWorkspaceLock(ctx context.Context, tx pgx.Tx, workspaceID string) error {
	for attempt := 0; attempt < setBindingsLockMaxAttempts; attempt++ {
		var got bool
		if err := tx.QueryRow(ctx,
			`SELECT pg_try_advisory_xact_lock(hashtext($1)::bigint)`,
			workspaceID).Scan(&got); err != nil {
			return fmt.Errorf("try-acquire workspace bindings lock: %w", err)
		}
		if got {
			return nil
		}
		// Short sleep with a tiny jitter would be ideal; for now a
		// flat 10ms is enough for the common case of two concurrent
		// SetBindings calls on the same workspace.
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(10 * time.Millisecond):
		}
	}
	return fmt.Errorf("workspace bindings lock contended for too long; try again")
}

// AddBindings atomically adds secretIDs to a workspace's binding set
// without touching existing bindings. Takes the same advisory lock
// as SetBindings so the two cannot interleave dangerously.
//
// The INSERT uses ON CONFLICT DO NOTHING so re-binding an already-
// bound secret is idempotent rather than a constraint violation.
func (s *PgSecretStore) AddBindings(ctx context.Context, workspaceID string, secretIDs []string) error {
	if len(secretIDs) == 0 {
		return nil
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if err := s.acquireWorkspaceLock(ctx, tx, workspaceID); err != nil {
		return err
	}

	for _, sid := range secretIDs {
		if _, err := tx.Exec(ctx,
			`INSERT INTO user_secret_bindings (secret_id, workspace_id)
			 VALUES ($1, $2)
			 ON CONFLICT (secret_id, workspace_id) DO NOTHING`,
			sid, workspaceID); err != nil {
			return fmt.Errorf("add binding (%s, %s): %w", sid, workspaceID, err)
		}
	}

	return tx.Commit(ctx)
}

func (s *PgSecretStore) GetBindings(ctx context.Context, workspaceID string) ([]*UserSecret, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT s.id, s.user_id, s.name, s.type, s.ciphertext, s.key_version, s.metadata, s.global_default, s.created_at, s.updated_at
		 FROM user_secrets s
		 JOIN user_secret_bindings b ON b.secret_id = s.id
		 WHERE b.workspace_id = $1
		 ORDER BY s.name`, workspaceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var secrets []*UserSecret
	for rows.Next() {
		var sec UserSecret
		if err := rows.Scan(&sec.ID, &sec.UserID, &sec.Name, &sec.Type, &sec.Ciphertext, &sec.KeyVersion, &sec.Metadata, &sec.GlobalDefault, &sec.CreatedAt, &sec.UpdatedAt); err != nil {
			return nil, err
		}
		secrets = append(secrets, &sec)
	}
	return secrets, rows.Err()
}

func (s *PgSecretStore) GetBindingsForSecret(ctx context.Context, secretID string) ([]string, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT workspace_id FROM user_secret_bindings WHERE secret_id = $1`, secretID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var workspaces []string
	for rows.Next() {
		var wsID string
		if err := rows.Scan(&wsID); err != nil {
			return nil, err
		}
		workspaces = append(workspaces, wsID)
	}
	return workspaces, rows.Err()
}

func (s *PgSecretStore) LogAudit(ctx context.Context, entry *AuditEntry) error {
	_, err := s.pool.Exec(ctx,
		`INSERT INTO secret_audit_log (user_id, action, secret_id, workspace_id, metadata, timestamp)
		 VALUES ($1, $2, $3, $4, $5, $6)`,
		entry.UserID, entry.Action, entry.SecretID, entry.WorkspaceID, entry.Metadata, entry.Timestamp)
	return err
}

func (s *PgSecretStore) QueryAudit(ctx context.Context, userID string, query AuditQuery) ([]*AuditEntry, error) {
	sql := `SELECT id, user_id, action, secret_id, workspace_id, metadata, timestamp
		    FROM secret_audit_log WHERE user_id = $1`
	args := []interface{}{userID}
	argIdx := 2

	if query.Action != "" {
		sql += fmt.Sprintf(" AND action = $%d", argIdx)
		args = append(args, query.Action)
		argIdx++
	}
	if query.SecretID != "" {
		sql += fmt.Sprintf(" AND secret_id = $%d", argIdx)
		args = append(args, query.SecretID)
		argIdx++
	}
	if query.WorkspaceID != "" {
		sql += fmt.Sprintf(" AND workspace_id = $%d", argIdx)
		args = append(args, query.WorkspaceID)
		argIdx++
	}
	if query.Since != nil {
		sql += fmt.Sprintf(" AND timestamp >= $%d", argIdx)
		args = append(args, *query.Since)
		argIdx++
	}
	if query.Until != nil {
		sql += fmt.Sprintf(" AND timestamp <= $%d", argIdx)
		args = append(args, *query.Until)
		argIdx++
	}

	sql += " ORDER BY timestamp DESC"

	limit := query.Limit
	if limit <= 0 {
		limit = 100
	}
	sql += fmt.Sprintf(" LIMIT $%d", argIdx)
	args = append(args, limit)
	argIdx++

	if query.Offset > 0 {
		sql += fmt.Sprintf(" OFFSET $%d", argIdx)
		args = append(args, query.Offset)
	}

	rows, err := s.pool.Query(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var entries []*AuditEntry
	for rows.Next() {
		var e AuditEntry
		var secretID, workspaceID *string
		if err := rows.Scan(&e.ID, &e.UserID, &e.Action, &secretID, &workspaceID, &e.Metadata, &e.Timestamp); err != nil {
			return nil, err
		}
		e.SecretID = secretID
		e.WorkspaceID = workspaceID
		entries = append(entries, &e)
	}
	return entries, rows.Err()
}

// AsyncAuditLogger wraps a SecretStore and logs audit entries
// asynchronously. The hot path (Log) never blocks: a full channel
// drops the entry and increments DroppedCount, which operators can
// scrape via Stats() and surface as an alert. Drains-on-Stop are
// idempotent (Stop may be called multiple times without panicking).
//
// AsyncAuditLogger is itself a SecretStore — every CRUD method
// delegates to the wrapped store, while LogAudit is the only method
// that becomes asynchronous. This means callers can compose:
//
//	auditedStore := NewAsyncAuditLogger(pgStore, 4096)
//	svc := NewSecretService(keys, auditedStore)
//
// and every audit write becomes non-blocking without further wiring.
type AsyncAuditLogger struct {
	store    SecretStore
	ch       chan *AuditEntry
	done     chan struct{}
	stopCtx  context.Context    // canceled by Stop() so in-flight LogAudit drains short-circuit
	stopFn   context.CancelFunc // cancels stopCtx
	stopOnce sync.Once
	closed   atomic.Bool
	dropped  atomic.Uint64
	written  atomic.Uint64
	failed   atomic.Uint64
	logger   pkginterfaces.LoggerInterface
}

// AsyncAuditStats is the snapshot returned by AsyncAuditLogger.Stats.
type AsyncAuditStats struct {
	Dropped uint64 // entries dropped because the channel was full
	Written uint64 // entries successfully persisted
	Failed  uint64 // entries that reached the worker but the store rejected
}

// NewAsyncAuditLogger creates an async audit logger with a buffered
// channel. logger is optional — when set, drop+failure events surface
// at Warn so operators can detect audit-pipeline degradation.
func NewAsyncAuditLogger(store SecretStore, bufSize int, logger pkginterfaces.LoggerInterface) *AsyncAuditLogger {
	stopCtx, stopFn := context.WithCancel(context.Background())
	l := &AsyncAuditLogger{
		store:   store,
		ch:      make(chan *AuditEntry, bufSize),
		done:    make(chan struct{}),
		stopCtx: stopCtx,
		stopFn:  stopFn,
		logger:  logger,
	}
	go l.run()
	return l
}

func (l *AsyncAuditLogger) run() {
	for entry := range l.ch {
		// Each write gets a fresh 5s timeout, but also inherits the
		// stopCtx so Stop() can cancel a hung write rather than
		// holding shutdown hostage on a doomed DB.
		ctx, cancel := context.WithTimeout(l.stopCtx, 5*time.Second)
		if err := l.store.LogAudit(ctx, entry); err != nil {
			l.failed.Add(1)
			if l.logger != nil {
				l.logger.Warn("audit logger: store rejected entry",
					"action", entry.Action, "userID", entry.UserID, "error", err.Error())
			}
		} else {
			l.written.Add(1)
		}
		cancel()
	}
	close(l.done)
}

// LogAudit on AsyncAuditLogger never blocks and never panics, even
// after Stop(). Entries are sent to the background goroutine via a
// buffered channel; a full channel drops the entry and increments the
// drop counter.
//
// There is a small race window between the closed-flag check and the
// channel send where a concurrent Stop could close the channel out
// from under a sender. The deferred recover catches the resulting
// "send on closed channel" panic, increments the drop counter, and
// returns normally. Without the recover, a request emitting an
// audit entry concurrently with shutdown would crash the process.
//
// The returned error is always nil — failures are observable via
// Stats() and the logger.
func (l *AsyncAuditLogger) LogAudit(_ context.Context, entry *AuditEntry) (retErr error) {
	if l.closed.Load() {
		l.dropped.Add(1)
		return nil
	}
	defer func() {
		if r := recover(); r != nil {
			l.dropped.Add(1)
			retErr = nil
		}
	}()
	select {
	case l.ch <- entry:
	default:
		l.dropped.Add(1)
		if l.logger != nil {
			// Warn at most once per drop; the volume is bounded by
			// the channel-fill rate so this is acceptable noise.
			l.logger.Warn("audit logger: channel full, dropping entry",
				"action", entry.Action, "userID", entry.UserID,
				"droppedTotal", l.dropped.Load())
		}
	}
	return nil
}

// Stop drains the channel and waits for completion. Idempotent: a
// second call returns immediately without panicking on the already-
// closed channel. Sets the closed flag BEFORE closing the channel so
// any concurrent LogAudit invocations see closed=true and take the
// drop path rather than panicking on a send-to-closed-channel.
//
// stopCtx is canceled AFTER the worker drains so a stuck-on-DB
// LogAudit eventually returns. There is still a small window between
// the closed-flag check and the channel send where a concurrent Stop
// could close the channel out from under a sender; the deferred
// recover in LogAudit catches that panic.
func (l *AsyncAuditLogger) Stop() {
	l.stopOnce.Do(func() {
		l.closed.Store(true)
		close(l.ch)
		<-l.done
		l.stopFn()
	})
}

// Stats returns a snapshot of the dropped / written / failed counters.
// Safe to call concurrently with logging.
func (l *AsyncAuditLogger) Stats() AsyncAuditStats {
	return AsyncAuditStats{
		Dropped: l.dropped.Load(),
		Written: l.written.Load(),
		Failed:  l.failed.Load(),
	}
}

// Pass-through methods so AsyncAuditLogger satisfies SecretStore.
// Every CRUD operation delegates to the wrapped store; only LogAudit
// is intercepted to make it asynchronous.

func (l *AsyncAuditLogger) CreateSecret(ctx context.Context, secret *UserSecret) error {
	return l.store.CreateSecret(ctx, secret)
}
func (l *AsyncAuditLogger) GetSecret(ctx context.Context, userID, secretID string) (*UserSecret, error) {
	return l.store.GetSecret(ctx, userID, secretID)
}
func (l *AsyncAuditLogger) GetSecretByName(ctx context.Context, userID, name string) (*UserSecret, error) {
	return l.store.GetSecretByName(ctx, userID, name)
}
func (l *AsyncAuditLogger) ListSecrets(ctx context.Context, userID string) ([]*UserSecret, error) {
	return l.store.ListSecrets(ctx, userID)
}
func (l *AsyncAuditLogger) ListGlobalDefaultSecrets(ctx context.Context, userID string) ([]*UserSecret, error) {
	return l.store.ListGlobalDefaultSecrets(ctx, userID)
}
func (l *AsyncAuditLogger) UpdateSecret(ctx context.Context, secret *UserSecret) error {
	return l.store.UpdateSecret(ctx, secret)
}
func (l *AsyncAuditLogger) DeleteSecret(ctx context.Context, userID, secretID string) error {
	return l.store.DeleteSecret(ctx, userID, secretID)
}
func (l *AsyncAuditLogger) ReEncryptUserSecrets(ctx context.Context, userID string, newKeyVersion int, transform func([]byte) ([]byte, error), commit func(context.Context) error) error {
	return l.store.ReEncryptUserSecrets(ctx, userID, newKeyVersion, transform, commit)
}
func (l *AsyncAuditLogger) SetBindings(ctx context.Context, workspaceID string, secretIDs []string) error {
	return l.store.SetBindings(ctx, workspaceID, secretIDs)
}
func (l *AsyncAuditLogger) AddBindings(ctx context.Context, workspaceID string, secretIDs []string) error {
	return l.store.AddBindings(ctx, workspaceID, secretIDs)
}
func (l *AsyncAuditLogger) GetBindings(ctx context.Context, workspaceID string) ([]*UserSecret, error) {
	return l.store.GetBindings(ctx, workspaceID)
}
func (l *AsyncAuditLogger) GetBindingsForSecret(ctx context.Context, secretID string) ([]string, error) {
	return l.store.GetBindingsForSecret(ctx, secretID)
}
func (l *AsyncAuditLogger) QueryAudit(ctx context.Context, userID string, query AuditQuery) ([]*AuditEntry, error) {
	return l.store.QueryAudit(ctx, userID, query)
}

// --- CredentialStore delegation (Epic 30) ---
// AsyncAuditLogger must satisfy CredentialStore so the type assertion in
// the injector methods (InjectSecrets / InjectSessionlessSecrets) succeeds.
// All methods delegate to the inner store.

func (l *AsyncAuditLogger) GetWorkspaceCredentials(ctx context.Context, workspaceID string) ([]CredentialBinding, error) {
	if cs, ok := l.store.(CredentialStore); ok {
		return cs.GetWorkspaceCredentials(ctx, workspaceID)
	}
	return nil, nil
}

func (l *AsyncAuditLogger) UpsertFreeTierCredential(ctx context.Context, ciphertext []byte) error {
	if cs, ok := l.store.(CredentialStore); ok {
		return cs.UpsertFreeTierCredential(ctx, ciphertext)
	}
	return fmt.Errorf("inner store does not implement CredentialStore")
}

func (l *AsyncAuditLogger) SeedWorkspaceCredentials(ctx context.Context, workspaceID, userID string, orgID *string) error {
	if cs, ok := l.store.(CredentialStore); ok {
		return cs.SeedWorkspaceCredentials(ctx, workspaceID, userID, orgID)
	}
	return fmt.Errorf("inner store does not implement CredentialStore")
}

func (l *AsyncAuditLogger) BindCredentialToAllUserWorkspaces(ctx context.Context, credentialID, userID string) error {
	if cs, ok := l.store.(CredentialStore); ok {
		return cs.BindCredentialToAllUserWorkspaces(ctx, credentialID, userID)
	}
	return fmt.Errorf("inner store does not implement CredentialStore")
}

func (l *AsyncAuditLogger) HasUserProviderCredential(ctx context.Context, userID, slug string) (bool, error) {
	if cs, ok := l.store.(CredentialStore); ok {
		return cs.HasUserProviderCredential(ctx, userID, slug)
	}
	return false, nil
}

// GetWorkspaceMCPServers delegates to the inner store's MCP server query.
// Epic 53: required so loadMCPServers works through the AsyncAuditLogger
// wrapper used in production (app.go wraps PgSecretStore in AsyncAuditLogger).
func (l *AsyncAuditLogger) GetWorkspaceMCPServers(ctx context.Context, workspaceID string) ([]MCPServerBindingRow, error) {
	if cs, ok := l.store.(CredentialStore); ok {
		return cs.GetWorkspaceMCPServers(ctx, workspaceID)
	}
	return nil, nil
}

// ReEncryptOrgCredentials re-encrypts all provider_credentials rows where
// owner_type='org' AND owner_id=orgID atomically within tx.
func (s *PgSecretStore) ReEncryptOrgCredentials(ctx context.Context, tx pgx.Tx, orgID string, oldDEK, newDEK []byte) (int, error) {
	rows, err := tx.Query(ctx, `
		SELECT id, ciphertext FROM provider_credentials
		WHERE owner_type = 'org' AND owner_id = $1
		FOR UPDATE
	`, orgID)
	if err != nil {
		return 0, fmt.Errorf("query org credentials for re-encrypt: %w", err)
	}

	type row struct {
		id         string
		ciphertext []byte
	}
	var toUpdate []row
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.id, &r.ciphertext); err != nil {
			rows.Close()
			return 0, fmt.Errorf("scan credential: %w", err)
		}
		toUpdate = append(toUpdate, r)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, fmt.Errorf("iterate credentials: %w", err)
	}

	count := 0
	for _, r := range toUpdate {
		plaintext, err := DecryptSecret(oldDEK, r.ciphertext)
		if err != nil {
			return count, fmt.Errorf("decrypt credential %s: %w", r.id, err)
		}
		newCiphertext, err := EncryptSecret(newDEK, plaintext)
		zeroBytes(plaintext)
		if err != nil {
			return count, fmt.Errorf("re-encrypt credential %s: %w", r.id, err)
		}
		if _, err := tx.Exec(ctx, `
			UPDATE provider_credentials SET ciphertext = $1, key_version = key_version + 1, updated_at = now()
			WHERE id = $2
		`, newCiphertext, r.id); err != nil {
			return count, fmt.Errorf("update credential %s: %w", r.id, err)
		}
		count++
	}
	return count, nil
}
