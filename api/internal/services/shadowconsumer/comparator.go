// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package shadowconsumer

import (
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/lenaxia/llmsafespaces/pkg/agent/opencode/wire"
)

// parseDialect is the API-side dialect probe: the minimal shapes the
// proxy tracker derives busy and part presence from. Deliberately
// independent of pkg/agent/opencode's translator — the comparator's value
// is comparing TWO independent derivations.
func parseDialect(raw []byte) (sessionID, status, partID string, ok bool) {
	var env struct {
		Type       string `json:"type"`
		Properties json.RawMessage
	}
	if err := json.Unmarshal(raw, &env); err != nil || env.Type == "" {
		return "", "", "", false
	}
	var props struct {
		SessionID string `json:"sessionID"`
		Status    struct {
			Type string `json:"type"`
		} `json:"status"`
		Part struct {
			ID string `json:"id"`
		} `json:"part"`
	}
	_ = json.Unmarshal(env.Properties, &props)
	if props.SessionID == "" {
		return "", "", "", false
	}
	switch env.Type {
	case wire.EventTypeSessionStatus:
		return props.SessionID, props.Status.Type, "", true
	case wire.EventTypeMessagePartUpdated:
		if props.Part.ID == "" {
			return props.SessionID, "", "", false
		}
		return props.SessionID, "", props.Part.ID, true
	case wire.EventTypeSessionIdle:
		return props.SessionID, "idle", "", true
	case wire.EventTypeSessionCreated, wire.EventTypeSessionUpdated:
		// Set maintenance: the session exists in the store (the ABI fold
		// creates records from SESSION_UPDATED); no busy change.
		return props.SessionID, "", "", true
	default:
		return "", "", "", false
	}
}

// Divergence classes (issue #1139): busy mismatch, part mismatch/loss,
// session-set mismatch, seq anomalies, snapshot inconsistency.
const (
	ClassBusyMismatch      = "busy_mismatch"
	ClassPartMismatch      = "part_mismatch"
	ClassSessionSet        = "session_set_mismatch"
	ClassSeqNonMonotonic   = "seq_non_monotonic"
	ClassSnapshotInconsist = "snapshot_inconsistency"
)

// Divergence is one observed difference, recorded as an artifact.
type Divergence struct {
	Time      time.Time `json:"time"`
	Class     string    `json:"class"`
	Detail    string    `json:"detail"`
	ABI       string    `json:"abi,omitempty"`
	Ref       string    `json:"ref,omitempty"`
	Explained bool      `json:"explained"`
	Note      string    `json:"note,omitempty"`
}

// Comparator diffs the ABI fold against the reference derivation and
// tracks seq-stall observability (the 0050 D1 progress signal).
type Comparator struct {
	recorder *Recorder

	mu        sync.Mutex
	lastABI   *Fold
	lastRef   ReferenceState
	lastSeq   uint64
	lastSeqAt time.Time
	stalled   bool
	stallSec  float64
	divs      []Divergence
	divCount  map[string]int64

	refSource *ReferenceFold

	stallThreshold time.Duration
	onDivergence   func(class string)
}

// WithDivergenceHook installs a per-divergence callback (the API wiring
// uses it to increment shadow_divergence_total{class} on the process
// registry).
func (c *Comparator) WithDivergenceHook(fn func(class string)) *Comparator {
	c.mu.Lock()
	c.onDivergence = fn
	c.mu.Unlock()
	return c
}

func NewComparator(rec *Recorder) *Comparator {
	return &Comparator{
		recorder:       rec,
		divCount:       map[string]int64{},
		stallThreshold: 30 * time.Second,
		lastSeqAt:      time.Now(),
	}
}

// WithDivergenceHook installs a per-divergence callback (the API wiring
// uses it to increment shadow_divergence_total{class} on the process
// registry; the harness uses it for assertions).

// SetReferenceSource lets the comparator read the current reference fold
// (used for harness-driven convergence checks).
func (c *Comparator) SetReferenceSource(r *ReferenceFold) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.refSource = r
}

// SetStallThreshold tunes the seq-stall detector (soak scenarios).
func (c *Comparator) SetStallThreshold(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.stallThreshold = d
}

// ObserveABI records an ABI-side fold. Seq monotonicity is checked on
// every observation (cheap, ordering-valid); state diffs run only at
// checkpoints (CompareNow) — per-observation diffing across an async
// pipeline manufactures phantom divergences from propagation lag.
func (c *Comparator) ObserveABI(f *Fold) {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := time.Now()
	if f.Seq < c.lastSeq {
		c.recordLocked(Divergence{Time: now, Class: ClassSeqNonMonotonic,
			Detail: fmt.Sprintf("abi seq regressed %d -> %d", c.lastSeq, f.Seq)})
	}
	if f.Seq > c.lastSeq {
		c.stalled = false
	}
	c.lastSeq = f.Seq
	c.lastSeqAt = now
	c.lastABI = f
}

// ObserveReference records an API-side derivation snapshot.
func (c *Comparator) ObserveReference(ref ReferenceState) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.lastRef = ref
}

// CompareNow diffs the two folds at a checkpoint (scenario boundaries,
// quiescent polls) and records divergences. Returns the unexplained count.
// A checkpoint against a still-converging pair records NOTHING: the ABI seq
// counts a superset of the reference's projectable events, so a seq-based
// lag guard cannot prove per-session quiescence — only agreement can.
func (c *Comparator) CompareNow() int {
	if !c.Converged() {
		return 0
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.diffLocked(time.Now())
	n := 0
	for _, d := range c.divs {
		if !d.Explained {
			n++
		}
	}
	return n
}

// NoteHarnessDeath mirrors the API tracker's agent-death semantics: busy
// clears reference-side; the ABI side reaches the same state via reseed →
// store truth, so a transient busy mismatch inside the reseed window is
// EXPLAINED (not a divergence).
func (c *Comparator) NoteHarnessDeath() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.refSource != nil {
		c.refSource.ObserveHarnessDeath()
		c.lastRef = c.refSource.Snapshot()
		c.markExplainedBusyLocked("harness death: reseed window")
	}
}

// NoteGenerationReseed mirrors the API tracker's generation-change
// reconcile: the reference adopts store truth (busy per session, in-flight
// cleared); the ABI side reaches the same state via its own reseed.
func (c *Comparator) NoteGenerationReseed(sessionBusy map[string]bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.refSource != nil {
		c.refSource.ObserveReconcile(sessionBusy)
		c.lastRef = c.refSource.Snapshot()
	}
}

func (c *Comparator) markExplainedBusyLocked(note string) {
	kept := c.divs[:0]
	for _, d := range c.divs {
		if d.Class == ClassBusyMismatch && !d.Explained {
			d.Explained = true
			d.Note = note
		}
		kept = append(kept, d)
	}
	c.divs = kept
}

func (c *Comparator) diffLocked(now time.Time) {
	if c.lastABI == nil || c.lastRef.Sessions == nil {
		return
	}
	// Lag guard: the ABI fold must have ingested at least as many events
	// as the reference observed (ABI seq counts a superset of the
	// reference's projectable set).
	if c.lastRef.Observed > 0 && uint64(c.lastRef.Observed) > c.lastABI.Seq { //nolint:gosec // G115: Observed is a small per-stream counter, never near int64 max
		return
	}
	abiSessions := c.lastABI.Sessions

	// Session set: a session present on one side only is a divergence
	// (after the first observation window — the shadow lags one stream).
	for id := range c.lastRef.Sessions {
		if _, ok := abiSessions[id]; !ok {
			c.recordLocked(Divergence{Time: now, Class: ClassSessionSet,
				Detail: fmt.Sprintf("session %s in reference but not ABI fold", id)})
		}
	}

	for id, ref := range c.lastRef.Sessions {
		abi, ok := abiSessions[id]
		if !ok {
			continue
		}
		abiBusy := abi.GetStatus() == statusBusy
		if ref.Busy != abiBusy {
			c.recordLocked(Divergence{Time: now, Class: ClassBusyMismatch, Detail: fmt.Sprintf("session %s busy: ref=%v abi=%v(status=%d)", id, ref.Busy, abiBusy, abi.GetStatus()),
				ABI: fmt.Sprintf("busy=%v", abiBusy), Ref: fmt.Sprintf("busy=%v", ref.Busy)})
		}
		if got := len(abi.GetInFlightParts()); got != ref.PartCount() {
			c.recordLocked(Divergence{Time: now, Class: ClassPartMismatch, Detail: fmt.Sprintf("session %s parts: ref=%d abi=%d", id, ref.PartCount(), got),
				ABI: fmt.Sprintf("%d", got), Ref: fmt.Sprintf("%d", ref.PartCount())})
		}
	}
}

func (c *Comparator) recordLocked(d Divergence) {
	c.divs = append(c.divs, d)
	c.divCount[d.Class]++
	if c.onDivergence != nil {
		c.onDivergence(d.Class)
	}
	if c.recorder != nil {
		c.recorder.RecordDivergence(d)
	}
}

// Converged reports whether both sides have been observed and currently
// agree (used by the scenario harness's eventual assertions).
func (c *Comparator) Converged() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.lastABI == nil || c.lastRef.Sessions == nil {
		return false
	}
	abi := c.lastABI.Sessions
	for id, ref := range c.lastRef.Sessions {
		a, ok := abi[id]
		if !ok {
			return false
		}
		if ref.Busy != (a.GetStatus() == statusBusy) {
			return false
		}
		if ref.PartCount() != len(a.GetInFlightParts()) {
			return false
		}
	}
	return true
}

// SeqStalled reports whether no seq advance has been observed within the
// stall threshold while the consumer is running (M3.3: starvation must be
// observable, not guessed).
func (c *Comparator) SeqStalled() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.stalled {
		return true
	}
	return time.Since(c.lastSeqAt) > c.stallThreshold
}

// SeqStallSeconds returns the current (or last) stall duration.
func (c *Comparator) SeqStallSeconds() float64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	if s := time.Since(c.lastSeqAt).Seconds(); s > c.stallSec {
		return s
	}
	return c.stallSec
}

// Unexplained counts divergences not marked explained.
func (c *Comparator) Unexplained() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	n := 0
	for _, d := range c.divs {
		if !d.Explained {
			n++
		}
	}
	return n
}

// Counts returns divergence counts per class (the shadow_divergence_total
// backing data).
func (c *Comparator) Counts() map[string]int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make(map[string]int64, len(c.divCount))
	for k, v := range c.divCount {
		out[k] = v
	}
	return out
}

// Debug renders the current both-sides state (harness failure dumps).
func (c *Comparator) Debug() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := fmt.Sprintf("lastSeq=%d stalled=%v\n", c.lastSeq, c.stalled)
	if c.lastABI != nil {
		out += fmt.Sprintf("ABI fold: seq=%d sessions=%d", c.lastABI.Seq, len(c.lastABI.Sessions))
		for id, s := range c.lastABI.Sessions {
			out += fmt.Sprintf(" [%s status=%d parts=%d]", id, s.GetStatus(), len(s.GetInFlightParts()))
		}
		out += "\n"
	} else {
		out += "ABI fold: <none>\n"
	}
	out += fmt.Sprintf("REF fold: sessions=%d", len(c.lastRef.Sessions))
	for id, s := range c.lastRef.Sessions {
		out += fmt.Sprintf(" [%s busy=%v parts=%d]", id, s.Busy, s.PartCount())
	}
	out += "\n"
	return out + c.reportLocked()
}

// Report renders all divergences for failure messages and exit reports.
func (c *Comparator) Report() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.reportLocked()
}

func (c *Comparator) reportLocked() string {
	out := ""
	for _, d := range c.divs {
		flag := ""
		if d.Explained {
			flag = " [explained: " + d.Note + "]"
		}
		out += fmt.Sprintf("%s %s: %s%s\n", d.Time.Format(time.RFC3339), d.Class, d.Detail, flag)
	}
	return out
}

const statusBusy = 3 // abiv1.SessionStatus_SESSION_STATUS_BUSY
