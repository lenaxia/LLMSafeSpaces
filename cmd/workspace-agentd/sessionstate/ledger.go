// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package sessionstate

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	abiv1 "github.com/lenaxia/llmsafespaces/pkg/abi/v1"
	"go.uber.org/zap"
)

// LedgerState is the M2 state machine:
//
//	ledgered → admitted → promoted → turn-ended
//
// with `failed` (per attempt, terminal for THAT attempt — re-armable at
// attempt+1) and `stalled` (admitted past the promotion deadline —
// recovered by WAKE ONLY, never re-admitted; I6).
type LedgerState int

const (
	LedgerStateUnspecified LedgerState = iota
	LedgerStateLedgered
	LedgerStateAdmitted
	LedgerStatePromoted
	LedgerStateTurnEnded
	LedgerStateStalled
	LedgerStateFailed
)

// String returns the lowercase wire name (metric labels, logs).
func (s LedgerState) String() string {
	switch s {
	case LedgerStateLedgered:
		return "ledgered"
	case LedgerStateAdmitted:
		return "admitted"
	case LedgerStatePromoted:
		return "promoted"
	case LedgerStateTurnEnded:
		return "turn_ended"
	case LedgerStateStalled:
		return "stalled"
	case LedgerStateFailed:
		return "failed"
	}
	return "unknown"
}

func (s LedgerState) proto() abiv1.LedgerState {
	switch s {
	case LedgerStateLedgered:
		return abiv1.LedgerState_LEDGER_STATE_LEDGERED
	case LedgerStateAdmitted:
		return abiv1.LedgerState_LEDGER_STATE_ADMITTED
	case LedgerStatePromoted:
		return abiv1.LedgerState_LEDGER_STATE_PROMOTED
	case LedgerStateTurnEnded:
		return abiv1.LedgerState_LEDGER_STATE_TURN_ENDED
	case LedgerStateStalled:
		return abiv1.LedgerState_LEDGER_STATE_STALLED
	case LedgerStateFailed:
		return abiv1.LedgerState_LEDGER_STATE_FAILED
	default:
		return abiv1.LedgerState_LEDGER_STATE_UNSPECIFIED
	}
}

// ledgerKey identifies a WAL row: the outbox's (entryID, attempt) — the
// dedupe scope of I5.
type ledgerKey struct {
	EntryID string
	Attempt uint32
}

// ledgerRecord is one WAL row. Terminal outcome retention (compaction
// policy) is independent of any consumer cursor; the seq-cursor meta row
// (format header) is uncompactable by construction — it is not a
// ledgerRecord.
type ledgerRecord struct {
	Seq       uint64      `json:"seq"`
	SessionID string      `json:"session_id"`
	EntryID   string      `json:"entry_id"`
	Attempt   uint32      `json:"attempt"`
	State     LedgerState `json:"state"`
	Text      string      `json:"text"` // joined delivery parts (multi-text join rule, D3)
	Model     string      `json:"model,omitempty"`
	MessageID string      `json:"message_id,omitempty"`
	Failure   string      `json:"failure,omitempty"`
	UpdatedAt time.Time   `json:"updated_at"`
	wakeFired bool
}

const ledgerFormatVersion = 1

// deliveryLedger is the PVC-backed WAL on the platform/ subPath. Format:
// one JSON record per line; the FIRST line is the format header
// {"format":1} — written at creation, never compacted, never a delivery
// record. Every mutation is an append + fsync BEFORE the caller proceeds
// (I9: the ledger() ack — the 202 equivalent — implies durability).
type deliveryLedger struct {
	mu       sync.Mutex
	path     string
	file     *os.File
	nextSeq  uint64
	rows     map[ledgerKey]*ledgerRecord
	deadline time.Duration
}

func openDeliveryLedger(path string) (*deliveryLedger, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return nil, fmt.Errorf("sessionstate ledger: dir: %w", err)
	}
	l := &deliveryLedger{
		path:     path,
		nextSeq:  1,
		rows:     map[ledgerKey]*ledgerRecord{},
		deadline: defaultPromotionDeadline,
	}
	created := false
	if _, err := os.Stat(path); os.IsNotExist(err) {
		created = true
	} else if err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR|os.O_APPEND, 0o600) //nolint:gosec // G302: platform/ payload contract (uid-isolated)
	if err != nil {
		return nil, fmt.Errorf("sessionstate ledger: open: %w", err)
	}
	l.file = f
	if created {
		if err := l.appendSyncLocked(map[string]any{"format": ledgerFormatVersion}); err != nil {
			_ = f.Close()
			return nil, fmt.Errorf("sessionstate ledger: header: %w", err)
		}
		return l, nil
	}
	// Replay (crash/suspend resume path): rebuild rows from the WAL;
	// the header validates the format version.
	data, err := os.ReadFile(path) //nolint:gosec // deployment-configured platform/ coordinate
	if err != nil {
		_ = f.Close()
		return nil, err
	}
	lines := strings.Split(string(data), "\n")
	first := true
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var probe map[string]any
		if err := json.Unmarshal([]byte(line), &probe); err != nil {
			// A torn tail line (crash mid-append) is truncated away: the
			// previous fsynced records are the truth.
			break
		}
		if first {
			first = false
			if v, _ := probe["format"].(float64); int(v) != ledgerFormatVersion {
				_ = f.Close()
				return nil, fmt.Errorf("sessionstate ledger: unknown format %v", probe["format"])
			}
			continue
		}
		var rec ledgerRecord
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			break // torn tail
		}
		key := ledgerKey{EntryID: rec.EntryID, Attempt: rec.Attempt}
		if existing, ok := l.rows[key]; !ok || rec.Seq > existing.Seq {
			cp := rec
			l.rows[key] = &cp
		}
		if rec.Seq >= l.nextSeq {
			l.nextSeq = rec.Seq + 1
		}
	}
	return l, nil
}

const defaultPromotionDeadline = 10 * time.Minute

// appendSyncLocked writes one JSON line and fsyncs (I9). mu held.
func (l *deliveryLedger) appendSyncLocked(v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	b = append(b, '\n')
	if _, err := l.file.Write(b); err != nil {
		return err
	}
	return l.file.Sync()
}

func (l *deliveryLedger) writeLocked(rec *ledgerRecord) error {
	rec.Seq = l.nextSeq
	l.nextSeq++
	rec.UpdatedAt = time.Now().UTC()
	if err := l.appendSyncLocked(rec); err != nil {
		return err
	}
	cp := *rec
	l.rows[ledgerKey{EntryID: rec.EntryID, Attempt: rec.Attempt}] = &cp
	return nil
}

// ledger idempotently records a delivery: a duplicate (entryID, attempt)
// returns the existing row WITHOUT a new WAL record (I5 dedupe). The
// returned bool reports whether this call created the row. The ack implies
// fsync-persistence (I9) — a 202 from Deliver is exactly this return.
func (l *deliveryLedger) ledger(sessionID, entryID string, attempt uint32, parts []string) (*ledgerRecord, bool, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	key := ledgerKey{EntryID: entryID, Attempt: attempt}
	if existing, ok := l.rows[key]; ok {
		return snapshotRecord(existing), false, nil
	}
	rec := &ledgerRecord{
		SessionID: sessionID,
		EntryID:   entryID,
		Attempt:   attempt,
		State:     LedgerStateLedgered,
		Text:      strings.Join(parts, "\n"), // D3 multi-text join rule
	}
	if err := l.writeLocked(rec); err != nil {
		return nil, false, err
	}
	return snapshotRecord(l.rows[key]), true, nil
}

// transition moves a row to `to` when the from-set matches; no-op (with
// the current row returned) otherwise. State monotonicity: admitted→
// promoted→turn-ended is one-way; stalled is admitted-with-alarm; failed
// is per-attempt terminal.
func (l *deliveryLedger) transition(key ledgerKey, to LedgerState, allowed map[LedgerState]bool, messageID, failure string) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	rec, ok := l.rows[key]
	if !ok {
		return fmt.Errorf("sessionstate ledger: no row for %s/%d", key.EntryID, key.Attempt)
	}
	if !allowed[rec.State] {
		return nil // already at/ past the target: idempotent no-op
	}
	if to == LedgerStateAdmitted {
		rec.MessageID = messageID
	}
	if to == LedgerStateFailed {
		rec.Failure = failure
	}
	rec.State = to
	rec.UpdatedAt = time.Now().UTC()
	// A transition is a NEW WAL line with a fresh Seq — replay's
	// last-writer-wins merge (same key, highest Seq) must resolve to the
	// latest state, not the first occurrence.
	rec.Seq = l.nextSeq
	l.nextSeq++
	return l.appendSyncLocked(rec)
}

var fromLedgered = map[LedgerState]bool{LedgerStateLedgered: true}
var fromAdmitted = map[LedgerState]bool{LedgerStateAdmitted: true, LedgerStateStalled: true}

func (l *deliveryLedger) markAdmitted(entryID string, attempt uint32, messageID string) error {
	return l.transition(ledgerKey{entryID, attempt}, LedgerStateAdmitted, fromLedgered, messageID, "")
}

func (l *deliveryLedger) markPromoted(entryID string, attempt uint32, messageID string) error {
	return l.transition(ledgerKey{entryID, attempt}, LedgerStatePromoted, fromAdmitted, messageID, "")
}

func (l *deliveryLedger) markFailed(entryID string, attempt uint32, reason string) error {
	return l.transition(ledgerKey{entryID, attempt}, LedgerStateFailed, fromLedgered, "", reason)
}

// markTurnEnded terminates the session's promoted rows at turn end.
func (l *deliveryLedger) markTurnEnded(sessionID string) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	for key, rec := range l.rows {
		if rec.SessionID != sessionID || rec.State != LedgerStatePromoted {
			continue
		}
		rec.State = LedgerStateTurnEnded
		rec.Seq = l.nextSeq
		l.nextSeq++
		if err := l.appendSyncLocked(rec); err != nil {
			return err
		}
		_ = key
	}
	return nil
}

// checkStalls moves admitted rows older than the deadline to stalled and
// fires the wake exactly once per row (I6: wake-only recovery — there is
// no re-admission path from this state, by construction and by API).
// Returns the stall/wake-failure counts of THIS pass (US-69.12 metrics).
func (l *deliveryLedger) checkStalls(ctx context.Context, wake func(context.Context, string) error, now time.Time) (stalled, wakeFailures int) {
	l.mu.Lock()
	var toStall []*ledgerRecord
	for _, rec := range l.rows {
		if rec.State == LedgerStateAdmitted && now.Sub(rec.UpdatedAt) > l.deadline {
			rec.State = LedgerStateStalled
			rec.Seq = l.nextSeq
			l.nextSeq++
			if err := l.appendSyncLocked(rec); err != nil && logger() != nil {
				logger().Warn("sessionstate ledger: stall append failed", zap.Error(err))
			}
			toStall = append(toStall, rec)
		}
	}
	l.mu.Unlock()
	for _, rec := range toStall {
		if rec.wakeFired {
			continue
		}
		rec.wakeFired = true
		stalled++
		if err := wake(ctx, rec.SessionID); err != nil {
			wakeFailures++
			if logger() != nil {
				logger().Warn("sessionstate ledger: stall wake failed", zap.String("session", rec.SessionID), zap.Error(err))
			}
		}
	}
	return stalled, wakeFailures
}

// depths returns the per-state row counts (the ledger funnel: ledgered →
// admitted → promoted/turn-ended, with stalled and failed visible).
func (l *deliveryLedger) depths() map[string]int64 {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := map[string]int64{}
	for _, rec := range l.rows {
		out[rec.State.String()]++
	}
	return out
}

// oldestAdmittedAge is how long the oldest admitted-unpromoted row has
// waited (seconds; 0 when none) — the promotion-stall signal (#1119
// class, made visible before it crosses the stall deadline).
func (l *deliveryLedger) oldestAdmittedAge(now time.Time) float64 {
	l.mu.Lock()
	defer l.mu.Unlock()
	oldest := time.Duration(0)
	for _, rec := range l.rows {
		if rec.State != LedgerStateAdmitted {
			continue
		}
		if age := now.Sub(rec.UpdatedAt); age > oldest {
			oldest = age
		}
	}
	return oldest.Seconds()
}

// stalledCount returns the current stalled-row count.
func (l *deliveryLedger) stalledCount() int64 {
	l.mu.Lock()
	defer l.mu.Unlock()
	var n int64
	for _, rec := range l.rows {
		if rec.State == LedgerStateStalled {
			n++
		}
	}
	return n
}

func (l *deliveryLedger) status(entryID string, attempt uint32) (*ledgerRecord, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	rec, ok := l.rows[ledgerKey{entryID, attempt}]
	if !ok {
		return nil, false
	}
	return snapshotRecord(rec), true
}

// admittedAnywhere reports the best live outcome across ALL attempts of
// an entry. The #1288 incident: the API-side outbox retries an entry on
// a doubling ladder (10s/20s/40s/80s) and each re-POST carried
// attempt+1; attempt-scoped status lookups treated every new attempt as
// fresh, so an entry whose attempt-1 admission HAD reached opencode
// (message persisted, turn completed) was re-POSTed as a new message —
// five identical turns from one user send. Admission dedup is keyed by
// the OUTBOX ENTRY ID (stable across the retry ladder); any prior
// attempt that reached ADMITTED or later makes a new attempt return
// that outcome instead of re-POSTing.
func (l *deliveryLedger) admittedAnywhere(entryID string) (*ledgerRecord, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	var best *ledgerRecord
	for key, rec := range l.rows {
		if key.EntryID != entryID {
			continue
		}
		if rec.State == LedgerStateAdmitted || rec.State == LedgerStatePromoted || rec.State == LedgerStateTurnEnded {
			if best == nil || rec.Attempt > best.Attempt {
				best = rec
			}
		}
	}
	if best == nil {
		return nil, false
	}
	return snapshotRecord(best), true
}

// queueDepth is ledger-derived (M2): ledgered ∪ admitted-unpromoted.
func (l *deliveryLedger) queueDepth(sessionID string) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	n := 0
	for _, rec := range l.rows {
		if rec.SessionID != sessionID {
			continue
		}
		if rec.State == LedgerStateLedgered || rec.State == LedgerStateAdmitted || rec.State == LedgerStateStalled {
			n++
		}
	}
	return n
}

// compact rewrites the WAL dropping terminal rows (turn-ended/failed)
// older than retention. The format header is preserved (uncompactable);
// in-retention terminal outcomes survive. Outcomes' retention is
// independent of any consumer cursor by construction (the ledger has no
// consumer cursors).
func (l *deliveryLedger) compact(retentionCutoff, now time.Time) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	tmp := l.path + ".compact"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600) //nolint:gosec // G302: platform/ payload contract
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintf(f, "{\"format\":%d}\n", ledgerFormatVersion); err != nil {
		_ = f.Close()
		return err
	}
	// Deterministic order: WAL sequence.
	type kv struct {
		key ledgerKey
		rec *ledgerRecord
	}
	var kept []kv
	for key, rec := range l.rows {
		kept = append(kept, kv{key, rec})
	}
	sort.Slice(kept, func(i, j int) bool { return kept[i].rec.Seq < kept[j].rec.Seq })
	for _, e := range kept {
		rec := e.rec
		if (rec.State == LedgerStateTurnEnded || rec.State == LedgerStateFailed) && rec.UpdatedAt.Before(retentionCutoff) {
			delete(l.rows, e.key)
			continue
		}
		b, err := json.Marshal(rec)
		if err != nil {
			_ = f.Close()
			return err
		}
		if _, err := f.Write(append(b, '\n')); err != nil {
			_ = f.Close()
			return err
		}
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp, l.path); err != nil {
		return err
	}
	nf, err := os.OpenFile(l.path, os.O_CREATE|os.O_RDWR|os.O_APPEND, 0o600) //nolint:gosec // G302: platform/ payload contract (uid-isolated)
	if err != nil {
		return err
	}
	_ = l.file.Close()
	l.file = nf
	return nil
}

func (l *deliveryLedger) close() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.file == nil {
		return nil
	}
	err := l.file.Close()
	l.file = nil
	return err
}

func snapshotRecord(r *ledgerRecord) *ledgerRecord {
	cp := *r
	return &cp
}

var ledgerLogger *zap.Logger

func logger() *zap.Logger { return ledgerLogger }

// --- the delivery driver ----------------------------------------------------

// Admitter is the dialect seam for admission: POST the harness's prompt
// endpoint. The wiring layer injects the opencode implementation
// (localhost :4096, Basic §D1 credential — same seam class as
// EventParser/StoreReader). Returns the harness's message ID for
// promotion correlation.
type Admitter interface {
	Admit(ctx context.Context, sessionID, text, model string) (string, error)
}

// deliveryDriver owns Deliver + admission + promotion correlation.
// Per-session single-flight: at most one admission in flight per session
// (I5 — bounds every ambiguity window to exactly one entry). The lock map
// is INJECTED (the authority's) so actions (US-69.9) join the same
// sole-writer domain; a nil injection owns a private map (tests).
type deliveryDriver struct {
	ledger *deliveryLedger
	admit  Admitter
	cfg    Config

	sessionLock func(string) *sync.Mutex

	ownMu       sync.Mutex
	ownLocks    map[string]*sync.Mutex
	promotionMu sync.Mutex
}

func newDeliveryDriver(l *deliveryLedger, admit Admitter, cfg Config, lock func(string) *sync.Mutex) *deliveryDriver {
	d := &deliveryDriver{ledger: l, admit: admit, cfg: cfg, sessionLock: lock, ownLocks: map[string]*sync.Mutex{}}
	if lock == nil {
		d.sessionLock = d.privateSessionLock
	}
	return d
}

// privateSessionLock is the fallback single-flight domain (no injection).
func (d *deliveryDriver) privateSessionLock(sessionID string) *sync.Mutex {
	d.ownMu.Lock()
	defer d.ownMu.Unlock()
	m, ok := d.ownLocks[sessionID]
	if !ok {
		m = &sync.Mutex{}
		d.ownLocks[sessionID] = m
	}
	return m
}

// deliver is the Deliver op core: idempotent ledger ack (the 202
// equivalent; I9 fsync inside), then asynchronous admission with retry.
// Admission failure returns the row to ledgered (retryable) or failed
// (terminal per attempt) — never an error to the caller: the accept seam
// accepts.
func (d *deliveryDriver) deliver(ctx context.Context, sessionID, entryID string, attempt uint32, parts []string, model string) (*ledgerRecord, bool, error) {
	rec, created, err := d.ledger.ledger(sessionID, entryID, attempt, parts)
	if err != nil {
		return nil, false, err
	}
	if !created {
		return rec, false, nil // duplicate (entryID, attempt): deduped (I5)
	}
	// Admission is asynchronous against agentd's own queue (M3.1): the
	// ack returns immediately; the driver admits under single-flight.
	//nolint:gosec // G118: admission is queue-scoped by design (M2 async
	// against agentd's own queue), not request-scoped — the 202 must not
	// couple admission lifetime to the request.
	go d.driveAdmission(sessionID, entryID, attempt, strings.Join(parts, "\n"), model)
	return rec, true, nil
}

const admissionAttempts = 5

// couple admission lifetime to the accepting request's context.
//
//nolint:contextcheck // admission is queue-scoped (M2): the 202 must not
func (d *deliveryDriver) driveAdmission(sessionID, entryID string, attempt uint32, text, model string) {
	backoff := 200 * time.Millisecond
	for i := 0; i < admissionAttempts; i++ {
		if d.attemptAdmission(sessionID, entryID, attempt, text, model) {
			return
		}
		// The backoff sleeps OUTSIDE the session lock: an action (US-69.9)
		// queuing behind a failing admission waits at most ONE attempt
		// (~10s), not the whole retry chain — the sole-writer invariant
		// holds per attempt (nothing is in flight while unlocked).
		time.Sleep(backoff)
		backoff *= 2
	}
	if err := d.ledger.markFailed(entryID, attempt, "admission exhausted retries"); err != nil && logger() != nil {
		logger().Warn("sessionstate: failed append failed", zap.Error(err))
	}
}

// attemptAdmission runs ONE admission attempt under the session's
// single-flight lock. Returns true when the row reached a terminal state
// (admitted by this attempt — or already terminal via a replay).
func (d *deliveryDriver) attemptAdmission(sessionID, entryID string, attempt uint32, text, model string) bool {
	m := d.sessionLock(sessionID)
	m.Lock()
	defer m.Unlock()
	// Re-read state under the session lock: a replay may have already
	// admitted this row (crash-window resolution), and a prior attempt's
	// failure may have re-armed at attempt+1.
	if rec, ok := d.ledger.status(entryID, attempt); ok && rec.State != LedgerStateLedgered {
		return true
	}
	// #1288 cross-attempt dedup: the retry ladder re-POSTs with attempt+1
	// under the SAME entry ID — if ANY prior attempt of this entry already
	// reached opencode (admitted or later), admit idempotently with that
	// outcome instead of manufacturing a second message.
	if rec, ok := d.ledger.admittedAnywhere(entryID); ok {
		if err := d.ledger.markAdmitted(entryID, attempt, rec.MessageID); err != nil && logger() != nil {
			logger().Warn("sessionstate: cross-attempt dedup append failed", zap.Error(err))
		}
		return true
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second) //nolint:contextcheck // queue-scoped by design
	msgID, err := d.admit.Admit(ctx, sessionID, text, model)
	cancel()
	if err == nil {
		if err := d.ledger.markAdmitted(entryID, attempt, msgID); err != nil && logger() != nil {
			logger().Warn("sessionstate: admitted append failed", zap.Error(err))
		}
		return true
	}
	return false
}

// replayUnresolved resumes after agentd death/suspend: every ledgered row
// (the ack-survived, un-admitted window) drives admission again —
// exactly-once per attempt holds because the WAL's admitted rows are
// skipped (I6: admitted is never re-admitted).
func (d *deliveryDriver) replayUnresolved(ctx context.Context) {
	d.ledger.mu.Lock()
	var pending []ledgerKey
	for key, rec := range d.ledger.rows {
		if rec.State == LedgerStateLedgered {
			pending = append(pending, key)
		}
	}
	d.ledger.mu.Unlock()
	for _, key := range pending {
		rec, ok := d.ledger.status(key.EntryID, key.Attempt)
		if !ok || rec.State != LedgerStateLedgered {
			continue
		}
		//nolint:gosec // G118: replay path — no request scope exists.
		go d.driveAdmission(rec.SessionID, rec.EntryID, rec.Attempt, rec.Text, rec.Model)
	}
}

// observeEvent consumes the authority's own contract events for
// promotion correlation: a message event carrying an admitted row's
// messageID promotes it (I12 stitch by ID).
func (d *deliveryDriver) observeEvent(evt *abiv1.Event) {
	if evt == nil {
		return
	}
	switch evt.GetType() {
	case abiv1.EventType_EVENT_TYPE_MESSAGE_START, abiv1.EventType_EVENT_TYPE_MESSAGE_END:
		mid := evt.GetMessageId()
		if mid == "" {
			return
		}
		d.promotionMu.Lock()
		defer d.promotionMu.Unlock()
		d.ledger.mu.Lock()
		keys := make([]ledgerKey, 0, 1)
		for key, rec := range d.ledger.rows {
			if rec.State == LedgerStateAdmitted && rec.MessageID == mid {
				keys = append(keys, key)
			}
		}
		d.ledger.mu.Unlock()
		for _, key := range keys {
			_ = d.ledger.markPromoted(key.EntryID, key.Attempt, mid)
		}
	}
}

// observeTurnEnded terminates the session's promoted rows at turn end.
func (d *deliveryDriver) observeTurnEnded(sessionID string) {
	_ = d.ledger.markTurnEnded(sessionID)
}
