// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package secretsreconcile

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lenaxia/llmsafespaces/api/internal/services/metrics"
	"github.com/lenaxia/llmsafespaces/pkg/secrets"
)

// fakeLister scripts the Active-workspace enumeration.
type fakeLister struct {
	mu   sync.Mutex
	list []ActiveWorkspace
	err  error
}

func (f *fakeLister) ListActiveWorkspaces(context.Context) ([]ActiveWorkspace, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	cp := make([]ActiveWorkspace, len(f.list))
	copy(cp, f.list)
	return cp, f.err
}

func (f *fakeLister) set(spawnedRev string, ids ...string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i := range f.list {
		for _, id := range ids {
			if f.list[i].WorkspaceID == id {
				f.list[i].SpawnedRev = spawnedRev
			}
		}
	}
}

// fakeRevisions is a per-workspace revision authority covering the full
// live-manifest loop seam: the STORED rows (CurrentRevision), the LIVE
// manifest tier (ManifestFor — what the rows currently derive), and the
// conditional mint (EnsureRevision, counting every seq-minting call).
//
// The live manifest defaults to the stored row's hash (drift-free), so
// tests script drift explicitly via manifests[..].
type fakeRevisions struct {
	mu   sync.Mutex
	rows map[string]fakeRevRow

	// manifests scripts the LIVE manifest hash per workspace. A missing
	// entry falls back to the stored row's hash; a workspace with
	// neither defaults to the owner's empty-set hash.
	manifests map[string]string

	mints map[string]int

	manifestErrs map[string]error
	rowErrs      map[string]error
	mintErrs     map[string]error
}

type fakeRevRow struct {
	seq  int64
	hash string
}

func newFakeRevisions(rows map[string]fakeRevRow) *fakeRevisions {
	return &fakeRevisions{
		rows:         rows,
		manifests:    make(map[string]string),
		mints:        make(map[string]int),
		manifestErrs: make(map[string]error),
		rowErrs:      make(map[string]error),
		mintErrs:     make(map[string]error),
	}
}

func (f *fakeRevisions) CurrentRevision(_ context.Context, workspaceID string) (int64, string, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.rowErrs[workspaceID]; err != nil {
		return 0, "", false, err
	}
	row, ok := f.rows[workspaceID]
	if !ok {
		return 0, "", false, nil
	}
	return row.seq, row.hash, true, nil
}

func (f *fakeRevisions) ManifestFor(_ context.Context, ownerUserID, workspaceID string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.manifestErrs[workspaceID]; err != nil {
		return "", err
	}
	if hash, ok := f.manifests[workspaceID]; ok {
		return hash, nil
	}
	if row, ok := f.rows[workspaceID]; ok {
		return row.hash, nil
	}
	return secrets.ManifestHash(ownerUserID, nil), nil
}

func (f *fakeRevisions) EnsureRevision(_ context.Context, workspaceID, manifestHash string) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.mintErrs[workspaceID]; err != nil {
		return 0, err
	}
	if row, ok := f.rows[workspaceID]; ok && row.hash == manifestHash {
		return row.seq, nil
	}
	next := f.rows[workspaceID].seq + 1
	f.rows[workspaceID] = fakeRevRow{seq: next, hash: manifestHash}
	f.mints[workspaceID]++
	return next, nil
}

func (f *fakeRevisions) mintCount(ws string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.mints[ws]
}

func (f *fakeRevisions) row(ws string) (fakeRevRow, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	row, ok := f.rows[ws]
	return row, ok
}

func (f *fakeRevisions) drift(ws, liveHash string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.manifests[ws] = liveHash
}

// fakeNotifier records notify dispatches.
type fakeNotifier struct {
	mu     sync.Mutex
	calls  []notifyCall
	onCall func(userID, workspaceID string) error
}

type notifyCall struct {
	userID      string
	workspaceID string
}

func (f *fakeNotifier) Notify(_ context.Context, userID, workspaceID string) error {
	f.mu.Lock()
	f.calls = append(f.calls, notifyCall{userID: userID, workspaceID: workspaceID})
	onCall := f.onCall
	f.mu.Unlock()
	if onCall != nil {
		return onCall(userID, workspaceID)
	}
	return nil
}

func (f *fakeNotifier) dispatched() []notifyCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	cp := make([]notifyCall, len(f.calls))
	copy(cp, f.calls)
	return cp
}

func emptyManifestHash(owner string) string {
	return secrets.ManifestHash(owner, nil)
}

func newTestService(lister WorkspaceLister, revisions RevisionSource, notifier Notifier) *Service {
	return New(lister, revisions, notifier,
		WithInterval(time.Minute),
		WithBackoff(5*time.Second, 10*time.Minute),
	)
}

func convergedGauge(t *testing.T, ws string) float64 {
	t.Helper()
	return testutil.ToFloat64(metrics.SecretsDeliveryConvergedGauge().WithLabelValues(ws))
}

func divergentCount(t *testing.T, reason string) float64 {
	t.Helper()
	return testutil.ToFloat64(metrics.SecretsDeliveryDivergentCounter().WithLabelValues(reason))
}

func resetReconcileMetrics() {
	metrics.SecretsDeliveryConvergedGauge().Reset()
	metrics.SecretsDeliveryDivergentCounter().Reset()
	metrics.SecretsReconcilePassesCounter().Reset()
	metrics.SecretsReconcileSkipsCounter().Reset()
}

func skipCount(t *testing.T, reason string) float64 {
	t.Helper()
	return testutil.ToFloat64(metrics.SecretsReconcileSkipsCounter().WithLabelValues(reason))
}

func TestRunPass_ConvergedWorkspaceDoesNotNotify(t *testing.T) {
	resetReconcileMetrics()
	lister := &fakeLister{list: []ActiveWorkspace{
		{WorkspaceID: "ws-1", OwnerUserID: "user-1", SpawnedRev: "9:aa:bb"},
	}}
	revisions := newFakeRevisions(map[string]fakeRevRow{"ws-1": {seq: 9, hash: "aa"}})
	notifier := &fakeNotifier{}
	svc := newTestService(lister, revisions, notifier)

	require.NoError(t, svc.runPass(context.Background()))

	assert.Equal(t, 1.0, convergedGauge(t, "ws-1"))
	assert.Empty(t, notifier.dispatched(), "converged workspace must never be notified")
	assert.Equal(t, 0.0, divergentCount(t, "stale_seq"))
	assert.Equal(t, 0, revisions.mintCount("ws-1"), "row matches the live manifest — no mint")
	assert.Equal(t, 1.0, testutil.ToFloat64(metrics.SecretsReconcilePassesCounter().WithLabelValues("success")))
}

// TestRunPass_LiveManifestDriftMintsAndNotifies is the review-gap pin
// (design 0057 law 2/4): a mutation whose notify failed and whose pull
// never ran leaves the stored ROW and the pod EQUALLY stale — comparing
// row vs pod calls that converged. The loop must compare the LIVE
// manifest (rows as they are NOW) against the stored row, mint the
// drift as a new seq, and notify.
func TestRunPass_LiveManifestDriftMintsAndNotifies(t *testing.T) {
	resetReconcileMetrics()
	lister := &fakeLister{list: []ActiveWorkspace{
		{WorkspaceID: "ws-1", OwnerUserID: "user-1", SpawnedRev: "5:aa:bb"},
	}}
	revisions := newFakeRevisions(map[string]fakeRevRow{"ws-1": {seq: 5, hash: "aa"}})
	revisions.drift("ws-1", "cc") // bind landed in rows; notify failed; no pull
	notifier := &fakeNotifier{}
	svc := newTestService(lister, revisions, notifier)

	require.NoError(t, svc.runPass(context.Background()))

	assert.Equal(t, 1, revisions.mintCount("ws-1"),
		"row-vs-live drift must mint the drift as a new seq")
	row, ok := revisions.row("ws-1")
	require.True(t, ok)
	assert.Equal(t, int64(6), row.seq)
	assert.Equal(t, "cc", row.hash)

	assert.Equal(t, 0.0, convergedGauge(t, "ws-1"))
	assert.Equal(t, 1.0, divergentCount(t, "stale_seq"),
		"the mint moved expected past the pod's applied seq — stale, not converged")
	calls := notifier.dispatched()
	require.Len(t, calls, 1)
	assert.Equal(t, "user-1", calls[0].userID)
	assert.Equal(t, "ws-1", calls[0].workspaceID)
}

// TestRunPass_NoRowMintsFirstRevision pins the first-observation path:
// a workspace no pull ever built gets its row minted by the loop. An
// empty live manifest mints the empty-set hash and classifies
// legacy_format under M1(a) — counter + one notify, gauge stays
// converged (M1b); a non-empty one flips missing_rev → notify.
func TestRunPass_NoRowMintsFirstRevision(t *testing.T) {
	resetReconcileMetrics()
	lister := &fakeLister{list: []ActiveWorkspace{
		{WorkspaceID: "ws-empty", OwnerUserID: "user-1", SpawnedRev: ""},
		{WorkspaceID: "ws-loaded", OwnerUserID: "user-1", SpawnedRev: ""},
	}}
	revisions := newFakeRevisions(map[string]fakeRevRow{})
	revisions.drift("ws-loaded", "content-hash")
	notifier := &fakeNotifier{}
	svc := newTestService(lister, revisions, notifier)

	require.NoError(t, svc.runPass(context.Background()))

	row, ok := revisions.row("ws-empty")
	require.True(t, ok, "the loop mints the row for a never-built workspace")
	assert.Equal(t, int64(1), row.seq)
	assert.Equal(t, emptyManifestHash("user-1"), row.hash)
	assert.Equal(t, 1.0, convergedGauge(t, "ws-empty"),
		"M1(b): legacy_format keeps the gauge converged — no page, no SLO burn")
	assert.Equal(t, 1.0, divergentCount(t, "legacy_format"),
		"M1(a): the unreported rev is observable in the counter")

	row, ok = revisions.row("ws-loaded")
	require.True(t, ok)
	assert.Equal(t, int64(1), row.seq)
	assert.Equal(t, "content-hash", row.hash)
	assert.Equal(t, 0.0, convergedGauge(t, "ws-loaded"))
	assert.Equal(t, 1.0, divergentCount(t, "missing_rev"))
	calls := notifier.dispatched()
	require.Len(t, calls, 2, "both the legacy_format and missing_rev workspaces notify once")
	notified := map[string]bool{}
	for _, c := range calls {
		notified[c.workspaceID] = true
	}
	assert.True(t, notified["ws-loaded"] && notified["ws-empty"])
}

// TestRunPass_DriftFromSecretsToEmptyManifestNotifies pins the
// empty-manifest edge: a workspace that just lost its last secret must
// NOTIFY (the pod still serves the old content) — the mint turns the
// empty-set drift into a seq the pod has not applied.
func TestRunPass_DriftFromSecretsToEmptyManifestNotifies(t *testing.T) {
	resetReconcileMetrics()
	lister := &fakeLister{list: []ActiveWorkspace{
		{WorkspaceID: "ws-1", OwnerUserID: "user-1", SpawnedRev: "3:content:bb"},
	}}
	revisions := newFakeRevisions(map[string]fakeRevRow{"ws-1": {seq: 3, hash: "content"}})
	revisions.drift("ws-1", emptyManifestHash("user-1"))
	notifier := &fakeNotifier{}
	svc := newTestService(lister, revisions, notifier)

	require.NoError(t, svc.runPass(context.Background()))

	assert.Equal(t, 1, revisions.mintCount("ws-1"))
	assert.Equal(t, 0.0, convergedGauge(t, "ws-1"))
	assert.Equal(t, 1.0, divergentCount(t, "stale_seq"))
	assert.Len(t, notifier.dispatched(), 1,
		"drift from secrets to empty must notify — the pod still serves revoked content")
}

// TestRunPass_MatchingRowNeverMints pins the write budget: a row that
// already equals the live manifest costs ZERO EnsureRevision mints.
func TestRunPass_MatchingRowNeverMints(t *testing.T) {
	resetReconcileMetrics()
	lister := &fakeLister{list: []ActiveWorkspace{
		{WorkspaceID: "ws-1", OwnerUserID: "user-1", SpawnedRev: "9:aa:bb"},
	}}
	revisions := newFakeRevisions(map[string]fakeRevRow{"ws-1": {seq: 9, hash: "aa"}})
	notifier := &fakeNotifier{}
	svc := newTestService(lister, revisions, notifier)

	require.NoError(t, svc.runPass(context.Background()))
	assert.Equal(t, 0, revisions.mintCount("ws-1"))
}

// TestRunPass_StaleSeqNotifiesAndCounts: expected (row == live) is
// ahead of the pod — the classic stale_seq divergence.
func TestRunPass_StaleSeqNotifiesAndCounts(t *testing.T) {
	resetReconcileMetrics()
	lister := &fakeLister{list: []ActiveWorkspace{
		{WorkspaceID: "ws-1", OwnerUserID: "user-1", SpawnedRev: "4:aa:bb"},
	}}
	revisions := newFakeRevisions(map[string]fakeRevRow{"ws-1": {seq: 9, hash: "aa"}})
	notifier := &fakeNotifier{}
	svc := newTestService(lister, revisions, notifier)

	require.NoError(t, svc.runPass(context.Background()))

	assert.Equal(t, 0.0, convergedGauge(t, "ws-1"))
	assert.Equal(t, 1.0, divergentCount(t, "stale_seq"))
	assert.Equal(t, 0, revisions.mintCount("ws-1"), "no drift — the row is current")
	calls := notifier.dispatched()
	require.Len(t, calls, 1)
	assert.Equal(t, "user-1", calls[0].userID)
	assert.Equal(t, "ws-1", calls[0].workspaceID)
}

func TestRunPass_MissingRevNotifies(t *testing.T) {
	resetReconcileMetrics()
	lister := &fakeLister{list: []ActiveWorkspace{
		{WorkspaceID: "ws-1", OwnerUserID: "user-1", SpawnedRev: ""},
	}}
	revisions := newFakeRevisions(map[string]fakeRevRow{"ws-1": {seq: 2, hash: "non-empty"}})
	notifier := &fakeNotifier{}
	svc := newTestService(lister, revisions, notifier)

	require.NoError(t, svc.runPass(context.Background()))

	assert.Equal(t, 0.0, convergedGauge(t, "ws-1"))
	assert.Equal(t, 1.0, divergentCount(t, "missing_rev"))
	assert.Len(t, notifier.dispatched(), 1)
}

// TestRunPass_EmptyManifestWithoutReportedRevIsLegacyNotConverged pins
// validation M1(a): an unreported spawnedRev can never certify
// convergence — a legacy pod serving revoked plaintext reports nothing.
// The empty-manifest case classifies legacy_format (not converged, not
// missing_rev): the counter records it, the notify fires once (a v2 pod
// resolves it by anchoring the revisioned empty pull), and per M1(b)
// the gauge is NOT marked divergent (no page).
func TestRunPass_EmptyManifestWithoutReportedRevIsLegacyNotConverged(t *testing.T) {
	resetReconcileMetrics()
	lister := &fakeLister{list: []ActiveWorkspace{
		{WorkspaceID: "ws-empty", OwnerUserID: "user-1", SpawnedRev: ""},
	}}
	revisions := newFakeRevisions(map[string]fakeRevRow{"ws-empty": {seq: 3, hash: emptyManifestHash("user-1")}})
	notifier := &fakeNotifier{}
	svc := newTestService(lister, revisions, notifier)

	require.NoError(t, svc.runPass(context.Background()))

	assert.Equal(t, 1.0, divergentCount(t, "legacy_format"),
		"an unreported rev with an empty manifest is legacy_format — never converged")
	assert.Equal(t, 1.0, convergedGauge(t, "ws-empty"),
		"M1(b): legacy_format must NOT mark the per-workspace gauge divergent — no page during mixed-fleet rollouts")
	assert.Len(t, notifier.dispatched(), 1,
		"one notify: a v2 pod anchors the revisioned empty pull and reports a parseable seq; backoff bounds a legacy pod")
}

// TestRunPass_EmptyManifestLegacyPodSelfHealsOnReport pins the v2
// self-heal of the M1(a) change: after the empty-manifest legacy_format
// classification notifies, the pod reports a parseable anchored seq —
// the next pass classifies a true seq match (converged, no counter).
func TestRunPass_EmptyManifestLegacyPodSelfHealsOnReport(t *testing.T) {
	resetReconcileMetrics()
	lister := &fakeLister{list: []ActiveWorkspace{
		{WorkspaceID: "ws-empty", OwnerUserID: "user-1", SpawnedRev: ""},
	}}
	emptyHash := emptyManifestHash("user-1")
	revisions := newFakeRevisions(map[string]fakeRevRow{"ws-empty": {seq: 3, hash: emptyHash}})
	notifier := &fakeNotifier{}
	svc := newTestService(lister, revisions, notifier)

	require.NoError(t, svc.runPass(context.Background()))
	assert.Equal(t, 1.0, divergentCount(t, "legacy_format"))

	// The pod anchored the revisioned empty pull and now reports.
	lister.set("3:"+emptyHash+":x", "ws-empty")
	require.NoError(t, svc.runPass(context.Background()))

	assert.Equal(t, 1.0, convergedGauge(t, "ws-empty"),
		"a parseable matching seq certifies convergence — the self-heal closed the loop")
	assert.Equal(t, 1.0, divergentCount(t, "legacy_format"),
		"no further legacy_format observations once the rev is parseable")
}

func TestRunPass_LegacyFormatNotifiesOnceThenBacksOff(t *testing.T) {
	resetReconcileMetrics()
	lister := &fakeLister{list: []ActiveWorkspace{
		{WorkspaceID: "ws-legacy", OwnerUserID: "user-1", SpawnedRev: "barehashnoformat"},
		{WorkspaceID: "ws-badseq", OwnerUserID: "user-1", SpawnedRev: "notanumber:aa:bb"},
	}}
	revisions := newFakeRevisions(map[string]fakeRevRow{
		"ws-legacy": {seq: 4, hash: "non-empty"},
		"ws-badseq": {seq: 4, hash: "non-empty"},
	})
	notifier := &fakeNotifier{}
	svc := newTestService(lister, revisions, notifier)

	require.NoError(t, svc.runPass(context.Background()))
	assert.Equal(t, 2.0, divergentCount(t, "legacy_format"))
	assert.Len(t, notifier.dispatched(), 2, "legacy-format pods get one notify attempt each")

	// M1(b): the alert-storm fix — legacy pods never mark the gauge
	// divergent (they can never converge by notify; the old mux 404s
	// resync), so LLMSafeSpacesSecretsDeliveryDivergent cannot page per
	// pod during a rollout.
	assert.Equal(t, 1.0, convergedGauge(t, "ws-legacy"),
		"legacy_format keeps the gauge converged — the counter carries the signal")
	assert.Equal(t, 1.0, convergedGauge(t, "ws-badseq"))

	require.NoError(t, svc.runPass(context.Background()))
	assert.Len(t, notifier.dispatched(), 2,
		"mixed-fleet legacy pods must not be loop-notified — backoff suppresses until eligible")
	assert.Equal(t, 4.0, divergentCount(t, "legacy_format"),
		"divergence stays visible every pass even while notify is suppressed")
}

// TestRunPass_LegacyPodDriftToEmptyStillNotifies pins the M1(a) edge:
// a legacy pod (bare rev) whose manifest drifts to empty MUST notify —
// the mint turns the empty-set drift into a seq the pod never applied,
// and classify must not route the legacy rev through the empty-manifest
// arm (a revoked secret is still being served).
func TestRunPass_LegacyPodDriftToEmptyStillNotifies(t *testing.T) {
	resetReconcileMetrics()
	lister := &fakeLister{list: []ActiveWorkspace{
		{WorkspaceID: "ws-legacy", OwnerUserID: "user-1", SpawnedRev: "barehash"},
	}}
	revisions := newFakeRevisions(map[string]fakeRevRow{"ws-legacy": {seq: 3, hash: "content"}})
	revisions.drift("ws-legacy", emptyManifestHash("user-1"))
	notifier := &fakeNotifier{}
	svc := newTestService(lister, revisions, notifier)

	require.NoError(t, svc.runPass(context.Background()))

	assert.Equal(t, 1, revisions.mintCount("ws-legacy"),
		"the drift to empty mints a new seq")
	assert.Equal(t, 1.0, divergentCount(t, "legacy_format"),
		"a bare rev classifies legacy_format regardless of the manifest")
	assert.Len(t, notifier.dispatched(), 1,
		"a legacy pod drifting to empty must be notified — it keeps serving the revoked plaintext")
	assert.Equal(t, 1.0, convergedGauge(t, "ws-legacy"),
		"M1(b) still applies: the gauge stays converged, the counter+notify carry it")
}

func TestRunPass_NotifyFailureCountsNotifyFailedAndBacksOff(t *testing.T) {
	resetReconcileMetrics()
	lister := &fakeLister{list: []ActiveWorkspace{
		{WorkspaceID: "ws-1", OwnerUserID: "user-1", SpawnedRev: "1:aa:bb"},
	}}
	revisions := newFakeRevisions(map[string]fakeRevRow{"ws-1": {seq: 5, hash: "aa"}})
	notifier := &fakeNotifier{onCall: func(_, _ string) error { return errors.New("connection refused") }}
	svc := newTestService(lister, revisions, notifier)

	require.NoError(t, svc.runPass(context.Background()))

	assert.Equal(t, 1.0, divergentCount(t, "notify_failed"))
	require.NoError(t, svc.runPass(context.Background()))
	assert.Len(t, notifier.dispatched(), 1, "failed notify enters backoff — second pass suppresses")
}

func TestRunPass_BackoffGrowsAndJitters(t *testing.T) {
	resetReconcileMetrics()
	lister := &fakeLister{list: []ActiveWorkspace{
		{WorkspaceID: "ws-1", OwnerUserID: "user-1", SpawnedRev: "1:aa:bb"},
	}}
	revisions := newFakeRevisions(map[string]fakeRevRow{"ws-1": {seq: 5, hash: "aa"}})
	notifier := &fakeNotifier{}
	svc := newTestService(lister, revisions, notifier)

	now := time.Unix(1_700_000_000, 0)
	svc.now = func() time.Time { return now }
	svc.jitter = func() float64 { return 0 } // deterministic waits

	deltas := []time.Duration{5 * time.Second, 10 * time.Second, 20 * time.Second}
	for i, want := range deltas {
		require.NoError(t, svc.runPass(context.Background()))
		st := svc.state("ws-1")
		require.NotNil(t, st)
		got := st.nextEligible.Sub(now)
		assert.Equal(t, want, got, "attempt %d must double the backoff", i+1)
		now = now.Add(got)
	}

	svc.jitter = func() float64 { return 1 } // +25% band
	require.NoError(t, svc.runPass(context.Background()))
	st := svc.state("ws-1")
	assert.Equal(t, 40*time.Second+10*time.Second, st.nextEligible.Sub(now),
		"jitter must stretch the wait by up to +25%%")
}

func TestRunPass_BackoffCapsAtMax(t *testing.T) {
	resetReconcileMetrics()
	lister := &fakeLister{list: []ActiveWorkspace{
		{WorkspaceID: "ws-1", OwnerUserID: "user-1", SpawnedRev: "1:aa:bb"},
	}}
	revisions := newFakeRevisions(map[string]fakeRevRow{"ws-1": {seq: 5, hash: "aa"}})
	notifier := &fakeNotifier{}
	svc := newTestService(lister, revisions, notifier)

	now := time.Unix(1_700_000_000, 0)
	svc.now = func() time.Time { return now }
	svc.jitter = func() float64 { return 0 }

	var lastWait time.Duration
	for i := 0; i < 40; i++ {
		require.NoError(t, svc.runPass(context.Background()))
		st := svc.state("ws-1")
		require.NotNil(t, st)
		lastWait = st.nextEligible.Sub(now)
		if lastWait > 10*time.Minute {
			t.Fatalf("backoff exceeded cap after %d attempts: %v", i+1, lastWait)
		}
		now = now.Add(lastWait)
	}
	assert.Equal(t, 10*time.Minute, lastWait, "the wait must saturate at the cap")
}

func TestRunPass_ConvergenceResetsBackoff(t *testing.T) {
	resetReconcileMetrics()
	lister := &fakeLister{list: []ActiveWorkspace{
		{WorkspaceID: "ws-1", OwnerUserID: "user-1", SpawnedRev: "1:aa:bb"},
	}}
	revisions := newFakeRevisions(map[string]fakeRevRow{"ws-1": {seq: 5, hash: "aa"}})
	notifier := &fakeNotifier{}
	svc := newTestService(lister, revisions, notifier)

	now := time.Unix(1_700_000_000, 0)
	svc.now = func() time.Time { return now }
	svc.jitter = func() float64 { return 0 }

	require.NoError(t, svc.runPass(context.Background()))
	require.NotNil(t, svc.state("ws-1"), "divergence creates backoff state")

	lister.set("5:aa:bb", "ws-1")
	require.NoError(t, svc.runPass(context.Background()))
	assert.Nil(t, svc.state("ws-1"), "observed convergence must reset the backoff state")

	lister.set("1:aa:bb", "ws-1")
	revisions.drift("ws-1", "bb")
	require.NoError(t, svc.runPass(context.Background()))
	assert.Len(t, notifier.dispatched(), 2,
		"a fresh divergence after convergence must notify immediately, not inherit the old backoff")
}

func TestRunPass_LevelTriggeredCoalescesNewStateMidBackoff(t *testing.T) {
	resetReconcileMetrics()
	lister := &fakeLister{list: []ActiveWorkspace{
		{WorkspaceID: "ws-1", OwnerUserID: "user-1", SpawnedRev: "1:aa:bb"},
	}}
	revisions := newFakeRevisions(map[string]fakeRevRow{"ws-1": {seq: 5, hash: "aa"}})
	notifier := &fakeNotifier{}
	svc := newTestService(lister, revisions, notifier)

	now := time.Unix(1_700_000_000, 0)
	svc.now = func() time.Time { return now }
	svc.jitter = func() float64 { return 0 }

	require.NoError(t, svc.runPass(context.Background()))
	require.Len(t, notifier.dispatched(), 1)

	// The live manifest moves again mid-backoff (rapid binds). No
	// queue, no reset: the next eligible attempt simply carries the
	// newest state — and the mint has already folded it into the row.
	revisions.drift("ws-1", "cc")
	require.NoError(t, svc.runPass(context.Background()))
	assert.Len(t, notifier.dispatched(), 1, "still in backoff — suppressed")

	now = now.Add(svc.state("ws-1").nextEligible.Sub(now))
	require.NoError(t, svc.runPass(context.Background()))
	assert.Len(t, notifier.dispatched(), 2, "eligible again — exactly one notify for the newest state")
	row, ok := revisions.row("ws-1")
	require.True(t, ok)
	assert.Equal(t, "cc", row.hash, "the drift was minted even while notify was suppressed")
}

func TestRunPass_ListFailureCountsErrorPass(t *testing.T) {
	resetReconcileMetrics()
	lister := &fakeLister{err: errors.New("apiserver down")}
	svc := newTestService(lister, newFakeRevisions(map[string]fakeRevRow{}), &fakeNotifier{})

	err := svc.runPass(context.Background())
	require.Error(t, err)
	assert.Equal(t, 1.0, testutil.ToFloat64(metrics.SecretsReconcilePassesCounter().WithLabelValues("error")))
	assert.Equal(t, 0.0, testutil.ToFloat64(metrics.SecretsReconcilePassesCounter().WithLabelValues("success")))
}

// TestRunPass_ManifestReadErrorSkipsWorkspace: a ManifestFor failure
// (store blip) skips THAT workspace only — counted, never fatal to the
// pass; the rest of the fleet still reconciles.
func TestRunPass_ManifestReadErrorSkipsWorkspace(t *testing.T) {
	resetReconcileMetrics()
	lister := &fakeLister{list: []ActiveWorkspace{
		{WorkspaceID: "ws-broken", OwnerUserID: "user-1", SpawnedRev: "5:aa:bb"},
		{WorkspaceID: "ws-ok", OwnerUserID: "user-1", SpawnedRev: "7:aa:bb"},
	}}
	revisions := newFakeRevisions(map[string]fakeRevRow{"ws-ok": {seq: 7, hash: "aa"}})
	revisions.manifestErrs["ws-broken"] = errors.New("db blip")
	notifier := &fakeNotifier{}
	svc := newTestService(lister, revisions, notifier)

	require.NoError(t, svc.runPass(context.Background()),
		"one unreadable manifest must not fail the pass")
	assert.Equal(t, 1.0, convergedGauge(t, "ws-ok"),
		"the healthy workspace still reconciles")
	assert.Empty(t, notifier.dispatched(), "the skipped workspace is not notified")
	assert.Equal(t, 1.0, skipCount(t, "manifest_read"),
		"m4: the skip is observable in secrets_reconcile_skips_total, not as a pass error")
	assert.Equal(t, 1.0, testutil.ToFloat64(metrics.SecretsReconcilePassesCounter().WithLabelValues("success")))
}

// TestRunPass_RevisionRowErrorSkipsWorkspace: same isolation for the
// stored-row read.
func TestRunPass_RevisionRowErrorSkipsWorkspace(t *testing.T) {
	resetReconcileMetrics()
	lister := &fakeLister{list: []ActiveWorkspace{
		{WorkspaceID: "ws-broken", OwnerUserID: "user-1", SpawnedRev: "5:aa:bb"},
		{WorkspaceID: "ws-ok", OwnerUserID: "user-1", SpawnedRev: "7:aa:bb"},
	}}
	revisions := newFakeRevisions(map[string]fakeRevRow{"ws-ok": {seq: 7, hash: "aa"}})
	revisions.rowErrs["ws-broken"] = errors.New("db blip")
	svc := newTestService(lister, revisions, &fakeNotifier{})

	require.NoError(t, svc.runPass(context.Background()))
	assert.Equal(t, 1.0, convergedGauge(t, "ws-ok"),
		"one unreadable workspace must not abort the pass — the rest still reconcile")
	assert.Equal(t, 1.0, skipCount(t, "row_read"),
		"m4: the stored-row skip lands in secrets_reconcile_skips_total{reason=row_read}")
	assert.Equal(t, 1.0, testutil.ToFloat64(metrics.SecretsReconcilePassesCounter().WithLabelValues("success")))
}

// TestRunPass_MintErrorSkipsWorkspace: a failed mint is latency, never
// correctness — the workspace is skipped this pass and retried next.
func TestRunPass_MintErrorSkipsWorkspace(t *testing.T) {
	resetReconcileMetrics()
	lister := &fakeLister{list: []ActiveWorkspace{
		{WorkspaceID: "ws-broken", OwnerUserID: "user-1", SpawnedRev: "5:aa:bb"},
		{WorkspaceID: "ws-ok", OwnerUserID: "user-1", SpawnedRev: "7:aa:bb"},
	}}
	revisions := newFakeRevisions(map[string]fakeRevRow{"ws-ok": {seq: 7, hash: "aa"}})
	revisions.drift("ws-broken", "new-hash")
	revisions.mintErrs["ws-broken"] = errors.New("converge failed")
	notifier := &fakeNotifier{}
	svc := newTestService(lister, revisions, notifier)

	require.NoError(t, svc.runPass(context.Background()))
	assert.Empty(t, notifier.dispatched(), "a workspace whose mint failed must not notify")
	assert.Equal(t, 1.0, convergedGauge(t, "ws-ok"))
	assert.Equal(t, 1.0, skipCount(t, "mint"),
		"m4: the mint skip lands in secrets_reconcile_skips_total{reason=mint}")
	assert.Equal(t, 1.0, testutil.ToFloat64(metrics.SecretsReconcilePassesCounter().WithLabelValues("success")))
}

func TestRunPass_RemovesGaugeForDeactivatedWorkspace(t *testing.T) {
	resetReconcileMetrics()
	lister := &fakeLister{list: []ActiveWorkspace{
		{WorkspaceID: "ws-1", OwnerUserID: "user-1", SpawnedRev: "5:aa:bb"},
	}}
	revisions := newFakeRevisions(map[string]fakeRevRow{"ws-1": {seq: 5, hash: "aa"}})
	svc := newTestService(lister, revisions, &fakeNotifier{})
	require.NoError(t, svc.runPass(context.Background()))
	require.Equal(t, 1.0, convergedGauge(t, "ws-1"))

	lister.mu.Lock()
	lister.list = nil
	lister.mu.Unlock()
	require.NoError(t, svc.runPass(context.Background()))
	assert.Equal(t, 0.0, convergedGauge(t, "ws-1"),
		"the gauge series must be withdrawn once the workspace is no longer Active")
}

func TestClassify_Table(t *testing.T) {
	emptyHash := emptyManifestHash("user-1")
	cases := []struct {
		name       string
		spawnedRev string
		seq        int64
		hash       string
		hasRow     bool
		converged  bool
		reason     string
	}{
		{"seq match", "5:aa:bb", 5, "aa", true, true, ""},
		{"seq behind", "4:aa:bb", 5, "aa", true, false, reasonStaleSeq},
		{"seq ahead", "6:aa:bb", 5, "aa", true, false, reasonStaleSeq},
		// M1(a): an unreported rev never certifies convergence — even
		// with an empty manifest it is legacy_format, never converged.
		{"no rev, empty manifest", "", 1, emptyHash, true, false, reasonLegacyFormat},
		{"no rev, non-empty manifest", "", 1, "content", true, false, reasonMissingRev},
		{"no rev, no row", "", 0, "", false, false, reasonMissingRev},
		{"rev but no row", "3:aa:bb", 0, "", false, false, reasonMissingRev},
		{"bare hash", "deadbeef", 3, "aa", true, false, reasonLegacyFormat},
		{"non-numeric seq", "x:aa:bb", 3, "aa", true, false, reasonLegacyFormat},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := classify(tc.spawnedRev, "user-1", tc.seq, tc.hash, tc.hasRow)
			assert.Equal(t, tc.converged, got.converged)
			if !tc.converged {
				assert.Equal(t, tc.reason, got.reason)
			}
		})
	}
}

func TestStart_StopRunsImmediateFirstPassThenStops(t *testing.T) {
	resetReconcileMetrics()
	var passes atomic.Int64
	lister := &fakeLister{list: []ActiveWorkspace{
		{WorkspaceID: "ws-1", OwnerUserID: "user-1", SpawnedRev: "1:aa:bb"},
	}}
	revisions := newFakeRevisions(map[string]fakeRevRow{"ws-1": {seq: 5, hash: "aa"}})
	notifier := &fakeNotifier{onCall: func(_, _ string) error { return nil }}
	decorated := &countingNotifier{inner: notifier, passes: &passes}

	svc := New(lister, revisions, decorated,
		WithInterval(10*time.Millisecond),
		WithBackoff(time.Nanosecond, time.Minute),
	)
	require.NoError(t, svc.Start(context.Background()))
	time.Sleep(80 * time.Millisecond)
	require.NoError(t, svc.Stop())

	assert.GreaterOrEqual(t, passes.Load(), int64(2),
		"the immediate first pass plus at least one tick must have run")
	require.NoError(t, svc.Stop(), "Stop must be idempotent")
}

type countingNotifier struct {
	inner  Notifier
	passes *atomic.Int64
}

func (c *countingNotifier) Notify(ctx context.Context, userID, workspaceID string) error {
	c.passes.Add(1)
	return c.inner.Notify(ctx, userID, workspaceID)
}

func TestIntervalFromEnv(t *testing.T) {
	t.Setenv("LLMSAFESPACES_SECRETS_RECONCILE_INTERVAL", "90s")
	assert.Equal(t, 90*time.Second, IntervalFromEnv(60*time.Second))
	t.Setenv("LLMSAFESPACES_SECRETS_RECONCILE_INTERVAL", "bogus")
	assert.Equal(t, 60*time.Second, IntervalFromEnv(60*time.Second),
		"an unparseable override falls back to the default, never blocks startup")
	t.Setenv("LLMSAFESPACES_SECRETS_RECONCILE_INTERVAL", "")
	assert.Equal(t, 60*time.Second, IntervalFromEnv(60*time.Second))
}

// TestRunPassZeroDecrypts_ThousandWorkspacesWithinPeriod: AC-12b with
// the live-manifest loop — every workspace's manifest is recomputed
// from rows (3 reads), the row is read, and drift mints (≤1 write).
// The REAL *secrets.SecretService runs the manifest tier with every
// decrypt dependency fatal-on-touch; the pass must sustain the 60s
// period with zero decrypt operations.
func TestRunPassZeroDecrypts_ThousandWorkspacesWithinPeriod(t *testing.T) {
	resetReconcileMetrics()

	const fleet = 1000
	list := make([]ActiveWorkspace, 0, fleet)
	rows := make(map[string]revRow, fleet)
	for i := 0; i < fleet; i++ {
		ws := fmt.Sprintf("ws-%04d", i)
		list = append(list, ActiveWorkspace{
			WorkspaceID: ws,
			OwnerUserID: "user-1",
			SpawnedRev:  "5:stale:bb",
		})
		// Every row is stale against the live manifest, so the pass
		// exercises the mint write for the whole fleet too.
		rows[ws] = revRow{seq: 5, hash: "stale"}
	}
	revisions := newPanickingRevisionSource(t, rows, newFleetManifestFixture("user-1"))
	lister := &fakeLister{list: list}
	notifier := &fakeNotifier{}

	loop := newTestService(lister, revisions, notifier)
	loop.now = func() time.Time { return time.Unix(1_700_000_000, 0) }
	loop.jitter = func() float64 { return 0 }

	budget := time.Minute
	start := time.Now()
	require.NoError(t, loop.runPass(context.Background()))
	elapsed := time.Since(start)
	assert.Less(t, elapsed, budget,
		"a 1,000-workspace pass must sustain the 60s period (took %s); the compare never leaves rows + revision tier", elapsed)
	assert.Len(t, notifier.dispatched(), fleet,
		"every stale row was minted forward — the whole fleet diverges and notifies")
	assert.Equal(t, fleet, revisions.mintedCount(),
		"exactly one mint per workspace (up-to-1 write)")

	// Second pass: rows now match the live manifest — zero mints, zero
	// decrypts, and the only divergence left is the pod's stale seq.
	mintsBefore := revisions.mintedCount()
	require.NoError(t, loop.runPass(context.Background()))
	assert.Equal(t, mintsBefore, revisions.mintedCount(),
		"a converged row must never be re-minted")
}
