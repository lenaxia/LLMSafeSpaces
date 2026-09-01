// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package keyrewrap

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lenaxia/llmsafespaces/pkg/secrets"
)

// ── fakes ──────────────────────────────────────────────────────────────

type fakeReconcileStore struct {
	mu       sync.Mutex
	rows     map[string]*secrets.UserKeyReconcileRow
	lists    [][2]int // (limit, offset) of every ListUserKeysForReconcile call
	casCalls int
}

func newFakeReconcileStore() *fakeReconcileStore {
	return &fakeReconcileStore{rows: map[string]*secrets.UserKeyReconcileRow{}}
}

func (f *fakeReconcileStore) upsert(row secrets.UserKeyReconcileRow) {
	f.mu.Lock()
	defer f.mu.Unlock()
	cp := row
	f.rows[row.UserID] = &cp
}

func (f *fakeReconcileStore) ListUserKeysForReconcile(_ context.Context, limit, offset int) ([]secrets.UserKeyReconcileRow, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lists = append(f.lists, [2]int{limit, offset})
	all := make([]secrets.UserKeyReconcileRow, 0, len(f.rows))
	for _, r := range f.rows {
		all = append(all, *r)
	}
	// oldest-first with deterministic tiebreak, mirroring the SQL.
	sortRows(all)
	if offset >= len(all) {
		return nil, nil
	}
	end := offset + limit
	if end > len(all) {
		end = len(all)
	}
	return all[offset:end], nil
}

func (f *fakeReconcileStore) CompareAndSwapWrappedDEK(_ context.Context, userID string, expectedWrapped, newWrapped []byte, newVersion int, previous *secrets.RetainedWrap) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.casCalls++
	row, ok := f.rows[userID]
	if !ok || !bytesEqual(row.WrappedDEK, expectedWrapped) {
		return false, nil
	}
	row.WrappedDEK = append([]byte(nil), newWrapped...)
	row.KeyVersion = newVersion
	if previous == nil {
		row.WrappedDEKPrevious = nil
		row.WrappedDEKPreviousKEKVersion = nil
		row.WrappedDEKRetainedUntil = nil
	} else {
		row.WrappedDEKPrevious = append([]byte(nil), previous.Ciphertext...)
		v := previous.KEKVersion
		row.WrappedDEKPreviousKEKVersion = &v
		u := previous.Until
		row.WrappedDEKRetainedUntil = &u
	}
	return true, nil
}

func (f *fakeReconcileStore) DeleteExpiredRetainedWraps(_ context.Context, now time.Time) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var n int64
	for _, row := range f.rows {
		if row.WrappedDEKRetainedUntil != nil && !row.WrappedDEKRetainedUntil.After(now) {
			row.WrappedDEKPrevious = nil
			row.WrappedDEKPreviousKEKVersion = nil
			row.WrappedDEKRetainedUntil = nil
			n++
		}
	}
	return n, nil
}

func (f *fakeReconcileStore) get(userID string) *secrets.UserKeyReconcileRow {
	f.mu.Lock()
	defer f.mu.Unlock()
	cp := *f.rows[userID]
	return &cp
}

func (f *fakeReconcileStore) listCalls() [][2]int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([][2]int(nil), f.lists...)
}

func bytesEqual(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func sortRows(rows []secrets.UserKeyReconcileRow) {
	for i := 1; i < len(rows); i++ {
		for j := i; j > 0; j-- {
			a, b := rows[j-1], rows[j]
			if a.UpdatedAt.Before(b.UpdatedAt) || (a.UpdatedAt.Equal(b.UpdatedAt) && a.UserID < b.UserID) {
				break
			}
			rows[j-1], rows[j] = rows[j], rows[j-1]
		}
	}
}

type fakeRecoverer struct {
	dek   []byte
	jti   string
	err   error
	calls int
}

func (f *fakeRecoverer) GetCachedDEKForUser(_ context.Context, _ string) ([]byte, string, error) {
	f.calls++
	return f.dek, f.jti, f.err
}

type fakeSecretLister struct {
	secretRows []*secrets.UserSecret
	err        error
}

func (f *fakeSecretLister) ListSecrets(_ context.Context, _ string) ([]*secrets.UserSecret, error) {
	return f.secretRows, f.err
}

type fakeAudit struct {
	mu      sync.Mutex
	entries []*secrets.AuditEntry
}

func (f *fakeAudit) LogAudit(_ context.Context, entry *secrets.AuditEntry) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.entries = append(f.entries, entry)
	return nil
}

func (f *fakeAudit) byAction(action string) []*secrets.AuditEntry {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []*secrets.AuditEntry
	for _, e := range f.entries {
		if e.Action == action {
			out = append(out, e)
		}
	}
	return out
}

// failVerifyProvider delegates Encrypt to the inner provider but fails
// Decrypt for ciphertexts this provider itself produced — but only the
// first failFirstN of them. Simulates a wrap that encrypts fine but
// does not round-trip (verify-after-write failure) while legacy rows
// still fail the entry unwrap naturally; a second pass against the
// same provider then heals cleanly (halt resets per pass).
type failVerifyProvider struct {
	inner      secrets.RootKeyProvider
	failFirstN int
	mu         sync.Mutex
	produced   map[string]bool
	injected   int
}

func newFailVerifyProvider(inner secrets.RootKeyProvider, failFirstN int) *failVerifyProvider {
	return &failVerifyProvider{inner: inner, failFirstN: failFirstN, produced: map[string]bool{}}
}

func (p *failVerifyProvider) Encrypt(ctx context.Context, pt []byte) ([]byte, error) {
	ct, err := p.inner.Encrypt(ctx, pt)
	if err != nil {
		return nil, err
	}
	p.mu.Lock()
	p.produced[hex.EncodeToString(ct)] = true
	p.mu.Unlock()
	return ct, nil
}

func (p *failVerifyProvider) Decrypt(ctx context.Context, ct []byte) ([]byte, error) {
	p.mu.Lock()
	produced := p.produced[hex.EncodeToString(ct)]
	inject := produced && p.injected < p.failFirstN
	if inject {
		p.injected++
	}
	p.mu.Unlock()
	if inject {
		return nil, fmt.Errorf("injected verify failure")
	}
	return p.inner.Decrypt(ctx, ct)
}

// ── helpers ────────────────────────────────────────────────────────────

func mustProvider(t *testing.T) *secrets.StaticKeyProvider {
	t.Helper()
	p, err := secrets.NewStaticKeyProvider([]byte("0123456789abcdef0123456789abcdef"))
	require.NoError(t, err)
	return p
}

// testDEK builds a deterministic 32-byte DEK (AES-256 key size) whose
// leading bytes identify the fixture.
func testDEK(prefix string) []byte {
	b := make([]byte, 32)
	copy(b, prefix)
	for i := len(prefix); i < 32; i++ {
		b[i] = '0'
	}
	return b
}

func newTestService(t *testing.T, store *fakeReconcileStore, rec *fakeRecoverer, lister *fakeSecretLister, audit *fakeAudit, provider secrets.RootKeyProvider) *Service {
	t.Helper()
	if store == nil {
		store = newFakeReconcileStore()
	}
	if rec == nil {
		rec = &fakeRecoverer{}
	}
	if lister == nil {
		lister = &fakeSecretLister{}
	}
	if audit == nil {
		audit = &fakeAudit{}
	}
	s := New(store, rec, provider, lister, audit, nil, Config{BatchSize: 200, HaltOnVerifyFailures: 3})
	fixed := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	s.setClock(func() time.Time { return fixed })
	return s
}

func rowFor(userID string, dek []byte, provider secrets.RootKeyProvider) secrets.UserKeyReconcileRow {
	wrapped, _ := provider.Encrypt(context.Background(), dek)
	return secrets.UserKeyReconcileRow{
		UserID:     userID,
		KeyVersion: 1,
		WrappedDEK: wrapped,
		UpdatedAt:  time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC),
	}
}

func secretUnder(t *testing.T, userID, name string, dek []byte) *secrets.UserSecret {
	t.Helper()
	ct, err := secrets.EncryptSecret(dek, []byte("value-"+name))
	require.NoError(t, err)
	return &secrets.UserSecret{UserID: userID, Name: name, Ciphertext: ct}
}

// ── tests ──────────────────────────────────────────────────────────────

// Healthy skip: a row the current provider unwraps is counted and never
// touched — no recovery attempt, no secret listing, no CAS call.
func TestRunPass_HealthyRowSkipped(t *testing.T) {
	provider := mustProvider(t)
	store := newFakeReconcileStore()
	store.upsert(rowFor("user-healthy", testDEK("a-32-byte-dek-aaaaaaaaaaaaaaaaaaaaaa"), provider))
	rec := &fakeRecoverer{}
	lister := &fakeSecretLister{}
	audit := &fakeAudit{}
	s := newTestService(t, store, rec, lister, audit, provider)

	stats := s.runPass(context.Background())

	assert.Equal(t, 1, stats.rows[outcomeHealthy], "healthy row counted")
	assert.Equal(t, 0, rec.calls, "no recovery attempted for healthy row")
	assert.Equal(t, 0, store.casCalls, "no CAS write for healthy row")
	assert.Empty(t, audit.entries, "no audit rows for healthy row")
	row := store.get("user-healthy")
	assert.Nil(t, row.WrappedDEKPrevious, "healthy row gains no retention columns")
}

// Heal happy path: legacy row (current provider cannot unwrap) → DEK
// recovered from the session-cache walk → agreement via a decryptable
// secret → verify-after-write → CAS wins → retained wrap is the OLD wrap
// bytes re-encrypted under the CURRENT provider.
func TestRunPass_HealHappyPath(t *testing.T) {
	provider := mustProvider(t)
	dek := testDEK("b-32-byte-dek-bbbbbbbbbbbbbbbbbbbbbbb")

	// The legacy row was wrapped under a DIFFERENT (lost) key.
	oldProvider, err := secrets.NewStaticKeyProvider([]byte("fedcba9876543210fedcba9876543210"))
	require.NoError(t, err)
	legacyRow := rowFor("user-legacy", dek, oldProvider)

	store := newFakeReconcileStore()
	store.upsert(legacyRow)
	rec := &fakeRecoverer{dek: dek, jti: "jti-1"}
	lister := &fakeSecretLister{secretRows: []*secrets.UserSecret{secretUnder(t, "user-legacy", "gh", dek)}}
	audit := &fakeAudit{}
	s := newTestService(t, store, rec, lister, audit, provider)

	stats := s.runPass(context.Background())

	require.Equal(t, 1, stats.rows[outcomeHealed], "row healed")

	healed := store.get("user-legacy")

	// New active wrap round-trips under the current provider.
	gotDEK, derr := provider.Decrypt(context.Background(), healed.WrappedDEK)
	require.NoError(t, derr)
	assert.Equal(t, dek, gotDEK, "new active wrap decrypts to the recovered DEK")
	assert.Equal(t, secrets.ActiveVersionOf(provider), healed.KeyVersion)

	// Retained wrap: ciphertext-under-current == the old wrap bytes (W10).
	require.NotNil(t, healed.WrappedDEKPrevious, "heal must retain the previous wrap")
	recoveredOld, perr := provider.Decrypt(context.Background(), healed.WrappedDEKPrevious)
	require.NoError(t, perr, "retained wrap must decrypt under the CURRENT provider")
	assert.Equal(t, legacyRow.WrappedDEK, recoveredOld, "retained wrap hides the exact old wrap bytes")
	require.NotNil(t, healed.WrappedDEKPreviousKEKVersion)
	assert.Equal(t, secrets.ActiveVersionOf(provider), *healed.WrappedDEKPreviousKEKVersion)
	require.NotNil(t, healed.WrappedDEKRetainedUntil)
	assert.Equal(t, s.now().Add(defaultRetention), *healed.WrappedDEKRetainedUntil)

	// Audit: one key_rewrap_heal row for this user.
	heals := audit.byAction(auditActionHeal)
	require.Len(t, heals, 1)
	assert.Equal(t, "user-legacy", heals[0].UserID)
}

// W11: a user with ZERO secrets can never be healed unverified —
// outcome unwrappable_no_secret, audit, row untouched.
func TestRunPass_ZeroSecretsNeverHealed(t *testing.T) {
	provider := mustProvider(t)
	oldProvider, err := secrets.NewStaticKeyProvider([]byte("fedcba9876543210fedcba9876543210"))
	require.NoError(t, err)
	store := newFakeReconcileStore()
	store.upsert(rowFor("user-nosec", testDEK("c-32-byte-dek-cccccccccccccccccccccc"), oldProvider))
	rec := &fakeRecoverer{dek: testDEK("c-32-byte-dek-cccccccccccccccccccccc")}
	lister := &fakeSecretLister{secretRows: nil}
	audit := &fakeAudit{}
	s := newTestService(t, store, rec, lister, audit, provider)

	stats := s.runPass(context.Background())

	assert.Equal(t, 1, stats.rows[outcomeUnwrappableNoSecret])
	assert.Equal(t, 0, store.casCalls, "zero-secret row must never be written")
	rows := audit.byAction(auditActionUnwrappable)
	require.Len(t, rows, 1)
	assert.Contains(t, string(rows[0].Metadata), auditReasonNoSecretToVerify)
}

// W9 poison guard: a recovered DEK that decrypts NONE of the user's
// secrets is a corrupt source — no write, ever.
func TestRunPass_PoisonSourceDisagreement_NoWrite(t *testing.T) {
	provider := mustProvider(t)
	oldProvider, err := secrets.NewStaticKeyProvider([]byte("fedcba9876543210fedcba9876543210"))
	require.NoError(t, err)
	store := newFakeReconcileStore()
	store.upsert(rowFor("user-poison", testDEK("d-32-byte-dek-dddddddddddddddddddddd"), oldProvider))
	// Recovered DEK is random garbage — decrypts nothing the user has.
	rec := &fakeRecoverer{dek: testDEK("z-32-byte-dek-zzzzzzzzzzzzzzzzzzzzzzzz")}
	lister := &fakeSecretLister{secretRows: []*secrets.UserSecret{secretUnder(t, "user-poison", "gh", testDEK("d-32-byte-dek-dddddddddddddddddddddd"))}}
	audit := &fakeAudit{}
	s := newTestService(t, store, rec, lister, audit, provider)

	stats := s.runPass(context.Background())

	assert.Equal(t, 1, stats.rows[outcomeSourceDisagreement])
	assert.Equal(t, 0, store.casCalls, "poisoned source must never produce a write")
	row := store.get("user-poison")
	assert.Nil(t, row.WrappedDEKPrevious)
}

// No recovery source at all (owner offline, no session): surfaced
// unwrappable_no_source with an audit row — counted, never silent.
func TestRunPass_NoRecoverySource(t *testing.T) {
	provider := mustProvider(t)
	oldProvider, err := secrets.NewStaticKeyProvider([]byte("fedcba9876543210fedcba9876543210"))
	require.NoError(t, err)
	store := newFakeReconcileStore()
	store.upsert(rowFor("user-offline", testDEK("e"), oldProvider))
	rec := &fakeRecoverer{err: secrets.ErrDEKUnavailable}
	audit := &fakeAudit{}
	s := newTestService(t, store, rec, nil, audit, provider)

	stats := s.runPass(context.Background())

	assert.Equal(t, 1, stats.rows[outcomeUnwrappableNoSource])
	assert.Equal(t, 0, store.casCalls)
	rows := audit.byAction(auditActionUnwrappable)
	require.Len(t, rows, 1)
	assert.Contains(t, string(rows[0].Metadata), auditReasonNoRecoverySource)
}

// An INFRA error from the recovery source (PG/Redis outage, distinct
// from ErrDEKUnavailable) is a pass error — not a stranded-user
// unwrappable: no audit row, row retried next pass.
func TestRunPass_RecoveryInfraErrorIsPassErrorNotUnwrappable(t *testing.T) {
	provider := mustProvider(t)
	oldProvider, err := secrets.NewStaticKeyProvider([]byte("fedcba9876543210fedcba9876543210"))
	require.NoError(t, err)
	store := newFakeReconcileStore()
	store.upsert(rowFor("user-pgblip", testDEK("g"), oldProvider))
	rec := &fakeRecoverer{err: fmt.Errorf("list active jwt_sessions for user: connection refused")}
	audit := &fakeAudit{}
	s := newTestService(t, store, rec, nil, audit, provider)

	stats := s.runPass(context.Background())

	assert.Equal(t, 1, stats.rows[outcomeError])
	assert.Equal(t, 0, stats.rows[outcomeUnwrappableNoSource])
	assert.Empty(t, audit.entries, "infra blips must not mint unwrappable audit rows")
	assert.Equal(t, 0, store.casCalls)
}

// Verify-after-write failure: the new wrap does not round-trip → no
// write, row untouched, verify-failure halt accounting incremented.
// With batch size 2 the halt lands mid-second-batch: the current batch
// completes (all four rows counted) but NO third batch is requested.
func TestRunPass_VerifyFailure_NoWriteAndHaltsAtThreshold(t *testing.T) {
	inner := mustProvider(t)
	provider := newFailVerifyProvider(inner, 1000)
	dek := testDEK("f")
	oldProvider, err := secrets.NewStaticKeyProvider([]byte("fedcba9876543210fedcba9876543210"))
	require.NoError(t, err)

	store := newFakeReconcileStore()
	store.upsert(rowFor("user-v1", dek, oldProvider))
	store.upsert(rowFor("user-v2", dek, oldProvider))
	store.upsert(rowFor("user-v3", dek, oldProvider))
	store.upsert(rowFor("user-v4", dek, oldProvider))

	rec := &fakeRecoverer{dek: dek}
	lister := &fakeSecretLister{secretRows: []*secrets.UserSecret{secretUnder(t, "user-v1", "gh", dek)}}
	audit := &fakeAudit{}
	s := newTestService(t, store, rec, lister, audit, provider)
	s.cfg.BatchSize = 2

	stats := s.runPass(context.Background())

	assert.Equal(t, 4, stats.rows[outcomeVerifyFailed], "every attempted row counted (current batch completes)")
	assert.Equal(t, 0, store.casCalls, "verify failure must never produce a write")
	assert.Equal(t, 1.0, testutil.ToFloat64(haltedGauge), "halted gauge set at verify-failure threshold")
	assert.True(t, stats.halted, "pass reports halted")
	assert.Len(t, store.listCalls(), 2, "halt stops further batches this pass (two batches of two, no third)")
	assert.Len(t, audit.byAction(auditActionVerifyFail), 4, "audit row per failed attempt")
}

// Halt accounting is per pass: a new pass resets the verify-failure
// count and the halted gauge. The fail-first provider makes pass one
// halt; pass two verifies cleanly and heals.
func TestRunPass_HaltResetsNextPass(t *testing.T) {
	inner := mustProvider(t)
	provider := newFailVerifyProvider(inner, 3)
	dek := testDEK("0")
	oldProvider, err := secrets.NewStaticKeyProvider([]byte("fedcba9876543210fedcba9876543210"))
	require.NoError(t, err)

	store := newFakeReconcileStore()
	store.upsert(rowFor("user-h1", dek, oldProvider))
	store.upsert(rowFor("user-h2", dek, oldProvider))
	store.upsert(rowFor("user-h3", dek, oldProvider))
	rec := &fakeRecoverer{dek: dek}
	lister := &fakeSecretLister{secretRows: []*secrets.UserSecret{secretUnder(t, "user-h1", "gh", dek)}}
	s := newTestService(t, store, rec, lister, nil, provider)

	first := s.runPass(context.Background())
	assert.True(t, first.halted)
	assert.Equal(t, 3, first.rows[outcomeVerifyFailed])
	assert.Equal(t, 1.0, testutil.ToFloat64(haltedGauge))

	second := s.runPass(context.Background())
	assert.False(t, second.halted, "next pass starts un-halted")
	assert.Equal(t, 0.0, testutil.ToFloat64(haltedGauge), "halted gauge reset at pass start")
	assert.Equal(t, 3, second.rows[outcomeHealed], "pass two heals once verification recovers")
}

// CAS loss: a concurrent legitimate rotation changed the row between
// listing and the CAS — the rotation won; count + audit + skip.
func TestRunPass_CASLoss_Skips(t *testing.T) {
	provider := mustProvider(t)
	dek := testDEK("1")
	oldProvider, err := secrets.NewStaticKeyProvider([]byte("fedcba9876543210fedcba9876543210"))
	require.NoError(t, err)

	row := rowFor("user-raced", dek, oldProvider)
	store := newFakeReconcileStore()
	store.upsert(row)
	rec := &fakeRecoverer{dek: dek}
	lister := &fakeSecretLister{secretRows: []*secrets.UserSecret{secretUnder(t, "user-raced", "gh", dek)}}
	audit := &fakeAudit{}
	s := newTestService(t, store, rec, lister, audit, provider)

	// Legitimate rotation lands between the listing and the CAS.
	s.preCAS = func(userID string) {
		store.mu.Lock()
		defer store.mu.Unlock()
		if r, ok := store.rows[userID]; ok {
			w, _ := provider.Encrypt(context.Background(), dek)
			r.WrappedDEK = w
			r.KeyVersion = 99
		}
	}

	stats := s.runPass(context.Background())

	assert.Equal(t, 1, stats.rows[outcomeCASLost])
	assert.Equal(t, 1, store.casCalls, "CAS attempted exactly once — no retry storm")
	lost := audit.byAction(auditActionCASLost)
	require.Len(t, lost, 1)
	assert.Equal(t, "user-raced", lost[0].UserID)
}

// Kill switch: Run returns immediately without touching the store.
func TestRun_KillSwitch(t *testing.T) {
	provider := mustProvider(t)
	store := newFakeReconcileStore()
	rec := &fakeRecoverer{}
	lister := &fakeSecretLister{}
	s := New(store, rec, provider, lister, nil, nil, Config{Disabled: true})

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	done := make(chan struct{})
	go func() { s.Run(ctx); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run with kill switch did not return")
	}
	assert.Empty(t, store.listCalls(), "kill switch must not walk any rows")
}

// Retention cleanup: rows past retained_until get their previous
// columns NULLed as part of the pass.
func TestRunPass_RetentionCleanup(t *testing.T) {
	provider := mustProvider(t)
	store := newFakeReconcileStore()
	dek := testDEK("2")
	row := rowFor("user-retained", dek, provider)
	expired := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	ver := 1
	row.WrappedDEKPrevious = []byte("old-wrap")
	row.WrappedDEKPreviousKEKVersion = &ver
	row.WrappedDEKRetainedUntil = &expired
	store.upsert(row)
	audit := &fakeAudit{}
	s := newTestService(t, store, &fakeRecoverer{}, nil, audit, provider)

	stats := s.runPass(context.Background())

	assert.Equal(t, int64(1), stats.retentionCleaned, "one expired retention row cleaned")
	cleaned := store.get("user-retained")
	assert.Nil(t, cleaned.WrappedDEKPrevious)
	assert.Nil(t, cleaned.WrappedDEKPreviousKEKVersion)
	assert.Nil(t, cleaned.WrappedDEKRetainedUntil)
}

// Batch ordering + termination: the walk requests windows oldest-first
// (offset advancing by batch size) and stops after a short window.
func TestRunPass_BatchWalkOrdering(t *testing.T) {
	provider := mustProvider(t)
	store := newFakeReconcileStore()
	base := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	for i := 0; i < 5; i++ {
		row := rowFor(fmt.Sprintf("user-%02d", i), testDEK("x-32-byte-dek-xxxxxxxxxxxxxxxxxxxxxx"), provider)
		row.UpdatedAt = base.Add(time.Duration(i) * time.Hour)
		store.upsert(row)
	}
	s := newTestService(t, store, &fakeRecoverer{}, nil, nil, provider)
	s.cfg.BatchSize = 2

	stats := s.runPass(context.Background())

	assert.Equal(t, 5, stats.rows[outcomeHealthy])
	calls := store.listCalls()
	require.Len(t, calls, 3, "windows (0,2), (2,4), (4,6-short)")
	assert.Equal(t, [2]int{2, 0}, calls[0])
	assert.Equal(t, [2]int{2, 2}, calls[1])
	assert.Equal(t, [2]int{2, 4}, calls[2])
}

// Batch counters feed the metrics registry.
func TestMetrics_BatchesAndRowsCounted(t *testing.T) {
	provider := mustProvider(t)
	store := newFakeReconcileStore()
	store.upsert(rowFor("user-m1", testDEK("3"), provider))
	store.upsert(rowFor("user-m2", testDEK("4"), provider))
	s := newTestService(t, store, &fakeRecoverer{}, nil, nil, provider)
	s.cfg.BatchSize = 1

	batchesBefore := testutil.ToFloat64(batchesTotal)
	healthyBefore := testutil.ToFloat64(rowsTotal.WithLabelValues(outcomeHealthy))
	stats := s.runPass(context.Background())

	assert.Equal(t, 2, stats.batches)
	assert.Equal(t, 2.0, testutil.ToFloat64(batchesTotal)-batchesBefore)
	assert.GreaterOrEqual(t, testutil.ToFloat64(rowsTotal.WithLabelValues(outcomeHealthy))-healthyBefore, 2.0)
}

// Config from env: period, batch size, kill switch.
func TestConfigFromEnv(t *testing.T) {
	t.Setenv("LLMSAFESPACES_KEY_REWRAP_PERIOD", "90s")
	t.Setenv("LLMSAFESPACES_KEY_REWRAP_BATCH_SIZE", "50")
	t.Setenv("LLMSAFESPACES_KEY_REWRAP_DISABLED", "true")
	cfg := ConfigFromEnv()
	assert.Equal(t, 90*time.Second, cfg.Period)
	assert.Equal(t, 50, cfg.BatchSize)
	assert.True(t, cfg.Disabled)

	t.Setenv("LLMSAFESPACES_KEY_REWRAP_PERIOD", "not-a-duration")
	t.Setenv("LLMSAFESPACES_KEY_REWRAP_BATCH_SIZE", "0")
	t.Setenv("LLMSAFESPACES_KEY_REWRAP_DISABLED", "")
	cfg = ConfigFromEnv()
	assert.Equal(t, DefaultPeriod, cfg.Period, "invalid period falls back to default")
	assert.Equal(t, DefaultBatchSize, cfg.BatchSize, "invalid batch size falls back to default")
	assert.False(t, cfg.Disabled)

	// Fail-closed kill switch: an unparseable non-empty value disables.
	t.Setenv("LLMSAFESPACES_KEY_REWRAP_DISABLED", "yes-please")
	cfg = ConfigFromEnv()
	assert.True(t, cfg.Disabled, "unparseable non-empty DISABLED value must disable (fail-closed)")
}

// Nil logger tolerated everywhere (production always wires one).
func TestNilLoggerSafe(t *testing.T) {
	provider := mustProvider(t)
	store := newFakeReconcileStore()
	oldProvider, err := secrets.NewStaticKeyProvider([]byte("fedcba9876543210fedcba9876543210"))
	require.NoError(t, err)
	store.upsert(rowFor("user-nillog", testDEK("5"), oldProvider))
	s := newTestService(t, store, &fakeRecoverer{err: secrets.ErrDEKUnavailable}, nil, nil, provider)

	assert.NotPanics(t, func() { s.runPass(context.Background()) })
}

// Compile-time interface checks against the production contracts —
// including the real KeyService recovery seam (US-70.5: warm-cache walk).
var _ secrets.ReconcileKeyStore = (*fakeReconcileStore)(nil)
var _ DEKRecoverer = (*fakeRecoverer)(nil)
var _ DEKRecoverer = (*secrets.KeyService)(nil)

// Metadata must stay JSON (the audit column is jsonb).
func TestAuditMetadataIsJSON(t *testing.T) {
	entry := &secrets.AuditEntry{UserID: "u", Action: auditActionHeal, Metadata: json.RawMessage(`{"reason":"x"}`)}
	require.NotNil(t, entry)
	var v map[string]any
	require.NoError(t, json.Unmarshal(entry.Metadata, &v))
}
