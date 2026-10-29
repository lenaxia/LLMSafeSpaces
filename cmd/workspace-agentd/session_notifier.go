// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

// session_notifier.go — session idle-subscriptions v1 (approved design
// /workspace/reports/session-subscription-design.md), §4.2/§4.3/§4.4/§4.6/§7:
// the transition tracker, debounce, suppression, and registration guards.
//
// Architecture (§2.2): the notifier is a sibling in-process consumer of
// Authority.Stream — the same ordered frame stream the API usagestream
// consumer dials — NOT a hook inside the sealed sessionstate module and
// NOT the harness SSE tracker (which would miss the synthetic #1722
// evidence settles, the exact bug class this feature must observe).
// Every state-changing path (harness folds, evidence settles, lease
// rederives, reseed orphan aborts) crosses applyLocked → fanoutLocked,
// so one subscriber sees every busy→idle transition regardless of
// origin, in seq order.
//
// The tracker maintains DERIVED busy per #1574 — streaming || active
// parts > 0 || queue depth > 0 — folding frames exactly as
// sessionstate's projection folds them (projection.go), never keying
// on raw status strings (the store reader is idle-biased,
// sessionstate_wiring.go:77-85). The emit condition (§4.2): derived
// busy flips true→false AND the final status is IDLE or ERROR (ruling
// 1: error settles notify — belt-and-suspenders; a worker that died
// mid-turn must wake its manager) AND no pending QUESTION/PERMISSION
// asks (a session blocked on the user is not "complete") AND the
// broadcaster is not a subtask — all latched through the saw-busy rule:
// unknown→idle never notifies, a busy we observed (this generation or
// persisted across a restart, §7.3) settles.
//
// Reseeded frames and fresh connection snapshots are resync points:
// rebuild derived state, emit nothing as a transition, but reconcile
// latch-vs-snapshot (latched busy + snapshot idle/error + gates ⇒
// treat as settle) — the same inference that makes the notifier
// independent of at-least-once frames (§4.5: a slow-consumer drop
// resubscribes and the stamped snapshot re-attributes the episode).
//
// Delivery (§4.5) is a seam: the vehicle (detached V1 POST + sentinel
// compose + per-subscriber rate bound) lives in
// session_notifier_delivery.go; this file owns the outbox ordering
// (intent persisted before delivery, cleared on success), the boot
// retry-once drain (RetryOutbox), and the §8 outcome counters at the
// notify-time decision points.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/lenaxia/llmsafespaces/cmd/workspace-agentd/sessionstate"
	abiv1 "github.com/lenaxia/llmsafespaces/pkg/abi/v1"
	"github.com/lenaxia/llmsafespaces/pkg/agent/opencode"
	"github.com/lenaxia/llmsafespaces/pkg/eventbus"
	"github.com/lenaxia/llmsafespaces/pkg/session"
	"go.uber.org/zap"
	"google.golang.org/protobuf/proto"
)

// notifyDebounceDefault is ruling 3: 5s per broadcaster, covering only
// the idle→busy boundary flutter (the #1722 settle gate already
// prevents mid-turn misfires).
const notifyDebounceDefault = 5 * time.Second

// notifyResubscribeBackoff bounds the Stream reconnect cadence after a
// slow-consumer drop (M3.4) — a fixed short backoff; the connection
// itself is the resync.
const notifyResubscribeBackoff = 500 * time.Millisecond

// Registration refusals (§4.1/§7.1/§7.2): sentinel-wrapped so the MCP
// layer (task 3) renders each loudly and distinctively.
var (
	errNotifySelfSubscription = errors.New("self-subscription refused")
	errNotifyCycle            = errors.New("subscription cycle refused")
	errNotifyCap              = errors.New("subscriber cap reached")
	errNotifySubtaskSide      = errors.New("subtask sessions are excluded from subscriptions")
)

// settleNotice is one due notification handed to the delivery seam.
// Canonical ids both ends; task 2 resolves phrases/titles for the
// message body (the v1 sentinel wording, §4.5).
type settleNotice struct {
	Broadcaster string
	Subscriber  string
	Event       string // subscriptionEventIdle | subscriptionEventError
	Title       string
	EnqueuedAt  time.Time
}

// settleDeliverer is the delivery seam (task 2: suppression tracking +
// detached V1 POST + rate bound). Returning nil marks the notice
// delivered (outbox cleared); an error keeps the intent in the outbox
// for the boot retry. Implementations must not call back into the
// notifier.
type settleDeliverer interface {
	DeliverSettle(ctx context.Context, notice settleNotice) error
}

// settlePublisher is the event-bus seam (design 0072): one settle
// becomes ONE topic event — session.{id}.{idle|error} — appended to
// the workspace stream; the notifier CONSUMER GROUP turns it back
// into per-subscriber deliveries with consumer-group ack/pending as
// the at-least-once engine (replacing the outbox's write-before-send
// on the bus path). eventbus.Bus satisfies this; the narrow interface
// keeps the notifier's tests free of a transport.
type settlePublisher interface {
	Publish(ctx context.Context, ev eventbus.Event) (string, error)
}

// logOnlySettleDeliverer is the task-1 default: delivery "succeeds"
// with a log line — the outbox exercises its full lifecycle without a
// network leg, and wiring (task 4) swaps in the real vehicle.
type logOnlySettleDeliverer struct {
	logger *zap.Logger
}

func (d logOnlySettleDeliverer) DeliverSettle(_ context.Context, notice settleNotice) error {
	if d.logger != nil {
		d.logger.Info("session notifier: settle notification due (log-only deliverer)",
			zap.String("broadcaster", notice.Broadcaster),
			zap.String("subscriber", notice.Subscriber),
			zap.String("event", notice.Event))
	}
	return nil
}

// ---- §4.4 suppression (already-notified) ----

// mutationToolNames are the ToolPart names that count as mutating work
// for the suppression rule: edit/write/bash exactly (the design's
// list). A read/grep/list after the send does not un-suppress — the
// subscriber's copy of the WORLD is what the rule reasons about.
var mutationToolNames = map[string]bool{
	"edit":  true,
	"write": true,
	"bash":  true,
}

// sendNotifyToolName is the one informing ToolPart: a COMPLETED
// send_message from the broadcaster to a target thread makes that
// target "already notified" until the broadcaster mutates again.
const sendNotifyToolName = "send_message"

// suppressionTracker is §4.4's already-notified ledger, derived
// incrementally off the same ordered frame stream as the transition
// tracker: per broadcaster, the seq of the last COMPLETED send_message
// per resolved target, and the seq of the last edit/write/bash part
// end (completed OR errored — an errored bash may still have mutated;
// false suppression is the costly case). In-memory only: it derives
// from frames this generation saw, and a restart's empty tracker
// simply never suppresses — the conservative direction.
//
// Guarded by the notifier's mu (single-writer fold on the Run loop;
// read at collect time under the same lock).
type suppressionTracker struct {
	// broadcaster → subscriber (canonical) → seq of B's last completed
	// send_message to that target.
	lastSend map[string]map[string]uint64
	// broadcaster → seq of B's last edit/write/bash part end.
	lastMut map[string]uint64
}

func newSuppressionTracker() suppressionTracker {
	return suppressionTracker{
		lastSend: map[string]map[string]uint64{},
		lastMut:  map[string]uint64{},
	}
}

// observeSend records a completed send_message leg (monotonic per
// pair — frames arrive in seq order, but resync/replay shapes never
// regress the ledger).
func (t *suppressionTracker) observeSend(broadcaster, target string, seq uint64) {
	if broadcaster == "" || target == "" || seq <= 0 {
		return
	}
	m := t.lastSend[broadcaster]
	if m == nil {
		m = map[string]uint64{}
		t.lastSend[broadcaster] = m
	}
	if prev, ok := m[target]; !ok || seq > prev {
		m[target] = seq
	}
}

// observeMutation records a mutating part end (monotonic per
// broadcaster).
func (t *suppressionTracker) observeMutation(broadcaster string, seq uint64) {
	if broadcaster == "" || seq <= 0 {
		return
	}
	if prev, ok := t.lastMut[broadcaster]; !ok || seq > prev {
		t.lastMut[broadcaster] = seq
	}
}

// suppresses answers the §4.4 rule for one (broadcaster, subscriber)
// pair: SUPPRESS iff a completed send exists for this target AND its
// seq postdates the broadcaster's last mutation — B's last word to S
// is newer than B's last change, so S already holds the final state.
func (t *suppressionTracker) suppresses(broadcaster, subscriber string) bool {
	sends, ok := t.lastSend[broadcaster]
	if !ok {
		return false
	}
	seq, ok := sends[subscriber]
	if !ok {
		return false
	}
	return seq > t.lastMut[broadcaster]
}

// sendMessageDestination parses a send_message ToolPart's raw JSON
// input for the target ref: `destination` (canonical, #1849) with the
// one-release `session_id` legacy alias. Unparseable input yields ""
// — recorded as no send (the conservative direction: never suppress
// on a target we cannot name).
func sendMessageDestination(input []byte) string {
	input = bytes.TrimSpace(input)
	if len(input) == 0 {
		return ""
	}
	var args struct {
		Destination string `json:"destination"`
		SessionID   string `json:"session_id"` // one-release legacy alias
	}
	if err := json.Unmarshal(input, &args); err != nil {
		return ""
	}
	if args.Destination != "" {
		return args.Destination
	}
	return args.SessionID
}

// canonicalNotifyDestination resolves a send_message destination to
// the registry's canonical key space: the short-id replica answers
// phrase-form refs (any case); canonical harness ids pass through
// byte-equal. An unresolvable or cold-replica phrase passes through
// unchanged and simply never matches a canonical subscriber key —
// no suppression, the safe direction (the §7.8 cold-window posture).
func canonicalNotifyDestination(dest string) string {
	dest = strings.TrimSpace(dest)
	if dest == "" {
		return ""
	}
	if id, ok := currentSessionShortIDs().resolveShortID(dest); ok {
		return id
	}
	return dest
}

// observeSuppressionLocked folds one sequenced event's tool legs into
// the suppression ledger. Only PART_END tool parts count: send legs
// must be COMPLETED (a failed send informed nobody — §4.4's
// "successful" clause), mutation legs count completed OR errored.
// Caller holds mu.
func (n *sessionNotifier) observeSuppressionLocked(broadcaster string, seq uint64, evt *abiv1.Event) {
	if evt.GetType() != abiv1.EventType_EVENT_TYPE_PART_END || seq <= 0 {
		return
	}
	toolPart, ok := evt.GetPart().GetPayload().(*abiv1.Part_Tool)
	if !ok || toolPart.Tool == nil {
		return
	}
	tool := toolPart.Tool
	switch name := tool.GetName(); {
	case name == sendNotifyToolName:
		if tool.GetState().GetStatus() != abiv1.ToolStatus_TOOL_STATUS_COMPLETED {
			return
		}
		if target := canonicalNotifyDestination(sendMessageDestination(tool.GetInput())); target != "" {
			n.suppression.observeSend(broadcaster, target, seq)
		}
	case mutationToolNames[name]: // edit / write / bash
		switch tool.GetState().GetStatus() {
		case abiv1.ToolStatus_TOOL_STATUS_COMPLETED, abiv1.ToolStatus_TOOL_STATUS_ERROR:
			n.suppression.observeMutation(broadcaster, seq)
		}
	}
}

// sessionFacts is the §4.6 lineage/lifecycle seam. ParentID is NOT in
// the authority projection (investigation C1) — the wiring layer
// refreshes these facts from ListSessions (opencode's parentID wire
// field, translate.go) and the notifier consults them at registration
// and at notify time.
type sessionFacts interface {
	IsSubtask(sessionID string) bool
	// IsGone reports archived-or-deleted — the lazy row-cleanup
	// predicate (§4.1 auto-removal).
	IsGone(sessionID string) bool
}

// sessionNotifierConfig wires the notifier. Stream and Snapshot are
// the authority seams (production: Authority.Stream / Authority.State);
// Facts is the lineage seam (task 4 wires the ListSessions refresher);
// Deliver is the delivery seam (task 2). Bus (design 0072) switches
// the settle leg from the direct per-subscriber fan-out to
// publish-once + the notifier consumer group — nil keeps v1
// point-to-point semantics (the default; every v1 test pins it).
type sessionNotifierConfig struct {
	Store    *sessionSubscriptionStore
	Stream   func(ctx context.Context) (<-chan *abiv1.StreamFrame, func(), error)
	Snapshot func() sessionstate.StateSnapshot
	Facts    sessionFacts
	// FactsPull is the optional ListSessions adapter RefreshFacts
	// pulls through (task 4's ticker); nil disables refresh.
	FactsPull func(ctx context.Context) ([]session.Session, error)
	// Debounce <= 0 ⇒ notifyDebounceDefault.
	Debounce time.Duration
	Deliver  settleDeliverer
	// Bus: optional eventbus producer seam (design 0072 §3).
	Bus    settlePublisher
	Logger *zap.Logger
}

// sessionNotifier is the subscription engine. Concurrency shape: the
// Run loop is the single tracker/debounce mutator (frames are ordered
// — the whole point of the stream); registration/prune/deliver calls
// from other goroutines take mu for the pieces they touch. Store
// persistence carries its own lock.
type sessionNotifier struct {
	cfg      sessionNotifierConfig
	store    *sessionSubscriptionStore
	facts    sessionFacts
	debounce time.Duration
	logger   *zap.Logger

	mu          sync.Mutex
	tracker     map[string]*notifierSession // canonical id → derived state
	debounces   map[string]*armedDebounce   // canonical broadcaster id → timer
	suppression suppressionTracker          // §4.4 already-notified ledger (mu-guarded)
	// deliverCtx is the Run loop's context (the delivery leg's
	// cancellation parent); nil until Run starts — timer legs fired
	// before/without Run fall back to a fresh background ctx.
	deliverCtx context.Context
}

// armedDebounce is one broadcaster's pending settle notification. One
// per busy episode: armed on the eligible flip, canceled on
// return-to-derived-busy, fired once (§4.3).
type armedDebounce struct {
	kind  string // subscriptionEventIdle | subscriptionEventError
	timer *time.Timer
}

// notifierSession is the notifier's mirror of the projection fold for
// one session — exactly the fields derived busy needs (the #1574
// definition, projection.go:331-339), plus the pending-ask set the
// settle gate reads and the title the notice body carries.
type notifierSession struct {
	streaming bool // the status-event busy-mark analog
	status    abiv1.SessionStatus
	parts     map[string]*abiv1.Part // in-flight parts by id
	pending   map[string]struct{}    // pending QUESTION/PERMISSION ask ids
	queue     int32                  // admission queue depth (snapshots only)
	title     string
}

func newNotifierSession() *notifierSession {
	return &notifierSession{
		status:  abiv1.SessionStatus_SESSION_STATUS_UNKNOWN,
		parts:   map[string]*abiv1.Part{},
		pending: map[string]struct{}{},
	}
}

// notifierActiveParts mirrors projection.activePartCount: every
// non-Tool part counts, Tool parts count unless terminal (a terminal
// tool part is a dead leg — renderable but never busy).
func notifierActiveParts(parts map[string]*abiv1.Part) int {
	n := 0
	for _, p := range parts {
		if tool, isTool := p.GetPayload().(*abiv1.Part_Tool); isTool {
			switch tool.Tool.GetState().GetStatus() {
			case abiv1.ToolStatus_TOOL_STATUS_COMPLETED, abiv1.ToolStatus_TOOL_STATUS_ERROR:
				continue
			}
		}
		n++
	}
	return n
}

// derivedBusy is the single busy definition (never raw status
// strings), INCLUDING the terminal ERROR veto (projection.go:365-367):
// an errored session does nothing autonomously.
func (s *notifierSession) derivedBusy() bool {
	busy := s.streaming || notifierActiveParts(s.parts) > 0 || s.queue > 0
	if s.status == abiv1.SessionStatus_SESSION_STATUS_ERROR {
		return false
	}
	return busy
}

// pendingUserInputs is the settle gate's ask count (QUESTION/PERMISSION
// only — the #1574 carve-out: blocked on the USER, surfaced as its own
// signal, never busy).
func (s *notifierSession) pendingUserInputs() int { return len(s.pending) }

// upsertPart stores a private clone (the frame is shared with every
// other subscriber; retained pointers would race later mutations —
// the projection's discipline).
func (s *notifierSession) upsertPart(p *abiv1.Part) {
	if p == nil || p.GetId() == "" {
		return
	}
	s.parts[p.GetId()] = proto.Clone(p).(*abiv1.Part)
}

// newSessionNotifier constructs the engine with defaulted seams.
func newSessionNotifier(cfg sessionNotifierConfig) *sessionNotifier {
	if cfg.Debounce <= 0 {
		cfg.Debounce = notifyDebounceDefault
	}
	if cfg.Logger == nil {
		cfg.Logger = zap.NewNop()
	}
	if cfg.Deliver == nil {
		cfg.Deliver = logOnlySettleDeliverer{logger: cfg.Logger}
	}
	return &sessionNotifier{
		cfg:         cfg,
		store:       cfg.Store,
		facts:       cfg.Facts,
		debounce:    cfg.Debounce,
		logger:      cfg.Logger,
		tracker:     map[string]*notifierSession{},
		debounces:   map[string]*armedDebounce{},
		suppression: newSuppressionTracker(),
	}
}

// notifyDebounceFromEnv resolves AGENTD_NOTIFY_DEBOUNCE (ruling 3, the
// AGENTD_* pattern of main.go:248): a Go duration string; malformed or
// non-positive values warn once and fall back to the default.
func notifyDebounceFromEnv(getenv func(string) string, logger *zap.Logger) time.Duration {
	v := strings.TrimSpace(getenv("AGENTD_NOTIFY_DEBOUNCE"))
	if v == "" {
		return notifyDebounceDefault
	}
	d, err := time.ParseDuration(v)
	if err != nil || d <= 0 {
		if logger != nil {
			logger.Warn("session notifier: malformed AGENTD_NOTIFY_DEBOUNCE — using default",
				zap.String("value", v),
				zap.Duration("default", notifyDebounceDefault))
		}
		return notifyDebounceDefault
	}
	return d
}

// Subscribe registers one edge after the §4.1 guards. Idempotent
// re-subscribe updates the event set. Returns the stored row (phrase
// emission is the MCP layer's job, task 3).
func (n *sessionNotifier) Subscribe(broadcaster, subscriber string, events []string) (*SubscriptionRow, error) {
	if n == nil || n.store == nil {
		return nil, errors.New("session subscriptions: store unavailable")
	}
	broadcaster, subscriber = strings.TrimSpace(broadcaster), strings.TrimSpace(subscriber)
	if broadcaster == "" || subscriber == "" {
		return nil, errors.New("session subscriptions: broadcaster and subscriber ids are required")
	}
	evs, err := validateSubscriptionEvents(events)
	if err != nil {
		return nil, err
	}
	if broadcaster == subscriber {
		notifyCount(notifyOutcomeRefusedSelf)
		return nil, fmt.Errorf("%w: session %s cannot watch itself (notification → turn → idle → notification, forever)",
			errNotifySelfSubscription, n.phrase(broadcaster))
	}
	if n.facts != nil {
		// §4.6 both sides: parents receive subtask results synchronously
		// through the task tool (broadcaster side is pure noise); subtasks
		// return and terminate (subscriber side never takes wake-up turns).
		if n.facts.IsSubtask(broadcaster) {
			notifyCount(notifyOutcomeSkippedSubtask)
			return nil, fmt.Errorf("%w: broadcaster %s is a subtask", errNotifySubtaskSide, n.phrase(broadcaster))
		}
		if n.facts.IsSubtask(subscriber) {
			notifyCount(notifyOutcomeSkippedSubtask)
			return nil, fmt.Errorf("%w: subscriber %s is a subtask", errNotifySubtaskSide, n.phrase(subscriber))
		}
	}
	if n.store.wouldCreateCycle(broadcaster, subscriber) {
		notifyCount(notifyOutcomeRefusedCycle)
		return nil, fmt.Errorf("%w: %s watching %s would close a wake loop (each settle would trigger the next, forever)",
			errNotifyCycle, n.phrase(subscriber), n.phrase(broadcaster))
	}
	if !n.store.hasRow(broadcaster, subscriber) && n.store.subscriberCount(broadcaster) >= maxSubscribersPerBroadcaster {
		notifyCount(notifyOutcomeRefusedCap)
		return nil, fmt.Errorf("%w: %s already has %d subscribers",
			errNotifyCap, n.phrase(broadcaster), maxSubscribersPerBroadcaster)
	}
	row := n.store.subscribe(broadcaster, subscriber, evs, time.Now())
	n.logger.Info("session notifier: subscribed",
		zap.String("broadcaster", broadcaster),
		zap.String("subscriber", subscriber),
		zap.Strings("events", evs))
	return row, nil
}

// Unsubscribe removes one edge; idempotent (absent edge → false).
func (n *sessionNotifier) Unsubscribe(broadcaster, subscriber string) bool {
	if n == nil || n.store == nil {
		return false
	}
	return n.store.unsubscribe(strings.TrimSpace(broadcaster), strings.TrimSpace(subscriber))
}

// phrase renders a canonical id agent-facing where the log/text wants
// it: the replica's phrase when warm, the canonical id in the heal
// window (§7.8 — never worse than the id).
func (n *sessionNotifier) phrase(canonicalID string) string {
	if p := currentSessionShortIDs().shortIDFor(canonicalID); p != "" {
		return p
	}
	return canonicalID
}

// ---- Frame consumption ----

// Run is the consumer loop: subscribe, consume ordered frames until
// the channel closes (the M3.4 slow-consumer drop — fanoutLocked
// cancels us, the pump closes the channel), resubscribe with backoff.
// Every (re)connection's first frame is the stamped snapshot, which is
// the resync that re-attributes any in-flight busy episode (§4.5's
// no-replay resilience).
func (n *sessionNotifier) Run(ctx context.Context) {
	defer n.cancelAllDebounces()
	n.mu.Lock()
	n.deliverCtx = ctx
	n.mu.Unlock()
	for ctx.Err() == nil {
		if n.cfg.Stream == nil {
			return // unwired (task 4 wires main); nothing to consume
		}
		frames, stop, err := n.cfg.Stream(ctx)
		if err != nil {
			if !notifySleep(ctx, notifyResubscribeBackoff) {
				return
			}
			continue
		}
		drained := false
		for !drained {
			select {
			case f, ok := <-frames:
				if !ok {
					drained = true
					continue
				}
				//nolint:contextcheck // the fold path deliberately drops Run's ctx: debounce timers fire past the frame that armed them (no per-frame ctx exists at the timer leg), and delivery parents on the snapshotted Run ctx via deliveryContext instead of this chain
				n.handleFrame(f)
			case <-ctx.Done():
				stop()
				return
			}
		}
		stop()
		// Dropped as a slow consumer: the resubscribe's snapshot frame
		// performs the latch-vs-snapshot reconciliation.
		if !notifySleep(ctx, notifyResubscribeBackoff) {
			return
		}
	}
}

// notifySleep waits d or ctx.Done(); reports whether the wait elapsed.
func notifySleep(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return true
	case <-ctx.Done():
		return false
	}
}

// handleFrame dispatches one stream frame. Snapshot frames and Reseeded
// frames are resyncs (rebuild + reconcile, no transition emission);
// event frames fold into the tracker.
func (n *sessionNotifier) handleFrame(f *abiv1.StreamFrame) {
	if n == nil || f == nil {
		return
	}
	switch {
	case f.GetSnapshot() != nil:
		n.resyncFromPodSnapshot(f.GetSnapshot().GetSnapshot())
	case f.GetReseeded() != nil:
		n.resyncFromAuthority()
	case f.GetEvent() != nil:
		n.applySequencedEvent(f.GetEvent())
	}
}

// canonicalFrameSessionID picks the canonical (harness) key off an
// emission-surface id pair: the phrase translation put the phrase in
// the wire field and kept the harness id additive in the internal
// field — the internal one is the registry's key space.
func canonicalFrameSessionID(wireID, internalID string) string {
	if internalID != "" {
		return internalID
	}
	return wireID
}

// applySequencedEvent folds one ordered event into the tracker and
// runs the busy-edge machinery on the derived flip.
func (n *sessionNotifier) applySequencedEvent(se *abiv1.SequencedEvent) {
	evt := se.GetEvent()
	if evt == nil {
		return
	}
	sid := canonicalFrameSessionID(evt.GetSessionId(), evt.GetInternalSessionId())
	if sid == "" && evt.GetSession() != nil {
		sid = evt.GetSession().GetId()
	}
	if sid == "" {
		return
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	st := n.tracker[sid]
	fresh := st == nil
	if fresh {
		st = newNotifierSession()
		n.tracker[sid] = st
	}
	prevBusy := st.derivedBusy()
	if fresh {
		// Attribution across a snapshot gap (§4.2's busy → … → unknown →
		// idle case): a session a resync snapshot no longer listed left
		// the persisted busy edge open — the live settle event closing
		// it notifies, because we observed the busy that opened it.
		// Without the latch a fresh projection's unknown→idle stays
		// silent (the never-observed case).
		if l := n.store.latchFor(sid); l.LastDerivedBusy && l.SawBusy {
			prevBusy = true
		}
	}
	n.foldEventLocked(st, evt, sid)
	n.observeSuppressionLocked(sid, se.GetSeq(), evt)
	n.observeBusyEdgeLocked(sid, prevBusy, st)
}

// foldEventLocked mirrors the projection fold (projection.go:122-245)
// for exactly the derived-busy inputs. Any drift here is a divergence
// bug by construction — keep the arms in lockstep with
// applyContractLocked.
func (n *sessionNotifier) foldEventLocked(st *notifierSession, evt *abiv1.Event, sid string) {
	switch evt.GetType() {
	case abiv1.EventType_EVENT_TYPE_SESSION_STATUS:
		st.status = evt.GetStatus()
		switch evt.GetStatus() {
		case abiv1.SessionStatus_SESSION_STATUS_BUSY, abiv1.SessionStatus_SESSION_STATUS_COMPACTING:
			st.streaming = true // compaction is autonomous progress (#1602)
		case abiv1.SessionStatus_SESSION_STATUS_IDLE, abiv1.SessionStatus_SESSION_STATUS_ERROR:
			st.streaming = false
			st.parts = map[string]*abiv1.Part{} // the projection clears in-flight state on turn end
		}
	case abiv1.EventType_EVENT_TYPE_SESSION_UPDATED:
		if s := evt.GetSession(); s != nil {
			// The payload id can retarget the record (projection.go:164-181).
			target := st
			if pid := canonicalFrameSessionID(s.GetId(), ""); pid != "" && pid != sid {
				t := n.tracker[pid]
				if t == nil {
					t = newNotifierSession()
					n.tracker[pid] = t
				}
				target = t
			}
			target.title = s.GetTitle()
		}
	case abiv1.EventType_EVENT_TYPE_MESSAGE_START:
		st.streaming = true // busy-from-data
		for _, p := range evt.GetMessage().GetParts() {
			st.upsertPart(p)
		}
	case abiv1.EventType_EVENT_TYPE_MESSAGE_END:
		for _, p := range evt.GetMessage().GetParts() {
			st.upsertPart(p)
		}
	case abiv1.EventType_EVENT_TYPE_PART_START:
		st.streaming = true // busy-from-data
		st.upsertPart(evt.GetPart())
	case abiv1.EventType_EVENT_TYPE_PART_DELTA:
		pid := evt.GetPartId()
		if pid == "" {
			pid = evt.GetPart().GetId()
		}
		if pid != "" {
			if _, ok := st.parts[pid]; !ok {
				st.parts[pid] = &abiv1.Part{Id: pid, Type: abiv1.PartType_PART_TYPE_TEXT,
					Payload: &abiv1.Part_Text{Text: evt.GetDelta()}}
			}
		}
	case abiv1.EventType_EVENT_TYPE_PART_END:
		if p := evt.GetPart(); p != nil {
			st.upsertPart(p)
		} else if pid := evt.GetPartId(); pid != "" {
			delete(st.parts, pid)
		}
	case abiv1.EventType_EVENT_TYPE_INPUT_REQUEST:
		if in := evt.GetInput(); in.GetId() != "" {
			switch in.GetKind() {
			case abiv1.InputKind_INPUT_KIND_QUESTION, abiv1.InputKind_INPUT_KIND_PERMISSION:
				st.pending[in.GetId()] = struct{}{}
			}
		}
	case abiv1.EventType_EVENT_TYPE_INPUT_RESOLVED:
		delete(st.pending, evt.GetInput().GetId())
	case abiv1.EventType_EVENT_TYPE_ERROR:
		// A failed step clears busy; the store confirms on the next
		// reseed. Parts stay (renderable) — the ERROR veto in
		// derivedBusy keeps them from holding busy.
		st.streaming = false
		if evt.GetError() != nil {
			st.status = abiv1.SessionStatus_SESSION_STATUS_ERROR
		}
	}
}

// observeBusyEdgeLocked runs the emit machinery on one derived busy
// edge: busy onset latches saw-busy and cancels any armed debounce;
// the settle edge persists the clear and — through the §4.2 gates —
// arms the per-broadcaster debounce.
func (n *sessionNotifier) observeBusyEdgeLocked(sid string, prevBusy bool, st *notifierSession) {
	switch {
	case !prevBusy && st.derivedBusy():
		// Busy observed: the attribution the saw-busy latch exists for.
		n.store.setLatch(sid, sessionLatch{LastDerivedBusy: true, SawBusy: true})
		// An armed timer canceled by return-to-busy inside the window is
		// §4.3's debounce doing its job — count it (§8).
		if _, armed := n.debounces[sid]; armed {
			notifyCount(notifyOutcomeDebounced)
		}
		n.cancelDebounceLocked(sid)
	case prevBusy && !st.derivedBusy():
		// The settle edge: persist the clear (the restart latch must not
		// re-fire this episode), then through the gates, the debounce.
		n.store.setLatch(sid, sessionLatch{LastDerivedBusy: false, SawBusy: true})
		kind, ok := settleKindOf(st)
		if !ok {
			return
		}
		if !n.settleGatesPassLocked(sid, st) {
			return
		}
		n.armDebounceLocked(sid, kind)
	}
}

// settleKindOf maps the post-settle status to the event kind (ruling
// 1: ERROR settles notify, as its own kind with its own wording).
func settleKindOf(st *notifierSession) (string, bool) {
	switch st.status {
	case abiv1.SessionStatus_SESSION_STATUS_IDLE:
		return subscriptionEventIdle, true
	case abiv1.SessionStatus_SESSION_STATUS_ERROR:
		return subscriptionEventError, true
	}
	return "", false
}

// settleGatesPassLocked is the §4.2 gate set beyond the flip itself:
// no pending user inputs (blocked-on-question is not "complete" — the
// next flip fires when the answered turn truly ends), broadcaster is
// not a subtask, and someone is actually listening (no rows ⇒ no
// timer for an unwatched session).
func (n *sessionNotifier) settleGatesPassLocked(sid string, st *notifierSession) bool {
	if st.pendingUserInputs() > 0 {
		notifyCount(notifyOutcomeSkippedPendingInput)
		return false
	}
	if n.facts != nil && n.facts.IsSubtask(sid) {
		notifyCount(notifyOutcomeSkippedSubtask)
		return false
	}
	if len(n.store.rowsForBroadcaster(sid)) == 0 {
		return false
	}
	return true
}

// armDebounceLocked arms (or re-arms) the broadcaster's settle timer.
// Caller holds mu.
func (n *sessionNotifier) armDebounceLocked(sid, kind string) {
	n.cancelDebounceLocked(sid)
	a := &armedDebounce{kind: kind}
	a.timer = time.AfterFunc(n.debounce, func() { n.debounceFired(sid, kind) })
	n.debounces[sid] = a
}

// cancelDebounceLocked stops + forgets an armed timer (return-to-busy
// within the window — the idle→busy flutter — cancels the notify).
func (n *sessionNotifier) cancelDebounceLocked(sid string) {
	if a, ok := n.debounces[sid]; ok {
		a.timer.Stop()
		delete(n.debounces, sid)
	}
}

func (n *sessionNotifier) cancelAllDebounces() {
	n.mu.Lock()
	defer n.mu.Unlock()
	for sid := range n.debounces {
		n.cancelDebounceLocked(sid)
	}
}

// debounceFired is the timer body: still-armed check (the cancel may
// have raced the fire), still-idle check (derived busy returned true
// between the arming and now — the timer lost the race to the fold),
// then dispatch. Once per busy episode by construction: the armed
// entry is deleted here and only re-armed on a fresh settle edge.
func (n *sessionNotifier) debounceFired(sid, kind string) {
	n.mu.Lock()
	if a, ok := n.debounces[sid]; !ok || a.kind != kind {
		n.mu.Unlock()
		return
	}
	delete(n.debounces, sid)
	if st := n.tracker[sid]; st != nil && st.derivedBusy() {
		n.mu.Unlock()
		notifyCount(notifyOutcomeDebounced) // the timer lost the race to a new turn — same §4.3 cancel
		n.logger.Info("session notifier: debounce lost the race to a new turn — notification canceled",
			zap.String("broadcaster", sid))
		return
	}
	// Bus path (design 0072): ONE topic event per settle — the stream
	// entry is the durable intent; the notifier consumer group fans
	// out per subscriber under consumer-group ack/pending. The direct
	// v1 leg stays the default (Bus nil) and the failure fallback.
	if n.cfg.Bus != nil {
		title := ""
		if st := n.tracker[sid]; st != nil {
			title = st.title
		}
		n.mu.Unlock()
		n.publishSettle(sid, kind, title)
		return
	}
	notices := n.collectSettlesLocked(sid, kind)
	n.mu.Unlock()
	_ = n.deliverNotices(n.deliveryContext(), notices)
}

// publishSettle is the bus leg's publish: session.{sid}.{kind} to the
// workspace stream, payload {session, settle, title}. A publish
// failure falls back to the DIRECT v1 leg — a lost wake-up is the
// failure that matters (§4.5); a duplicate (bus leg retried by the
// pending machinery after this process already fell back) is
// tolerated by contract.
func (n *sessionNotifier) publishSettle(sid, kind, title string) {
	_, err := n.cfg.Bus.Publish(n.deliveryContext(), eventbus.Event{
		Topic: eventbus.SessionSettleTopic(sid, kind),
		At:    time.Now(),
		Payload: map[string]string{
			"session": sid,
			"settle":  kind,
			"title":   title,
		},
	})
	if err == nil {
		notifyCount(notifyOutcomeBusPublished)
		n.logger.Debug("session notifier: settle published to the workspace event bus",
			zap.String("broadcaster", sid),
			zap.String("settle", kind))
		return
	}
	n.logger.Warn("session notifier: bus publish failed — falling back to the direct delivery leg",
		zap.String("broadcaster", sid),
		zap.String("settle", kind),
		zap.Error(err))
	n.mu.Lock()
	notices := n.collectSettlesLocked(sid, kind)
	n.mu.Unlock()
	_ = n.deliverNotices(n.deliveryContext(), notices)
}

// ConsumeSettleEvent is the notifier consumer group's handler
// (design 0072 §3): one session.{id}.{idle|error} bus message becomes
// the SAME per-subscriber deliveries the direct leg makes — identical
// suppression, rate bound, and compose; the only difference is the
// delivery layer: an un-acked message (hard failure) is redelivered
// by the consumer group's pending machinery, not retried by the
// outbox (the stream entry is the durable intent — no outbox enqueue
// on this path). Rate refusals and suppressions are FINAL outcomes
// (acked), never errors.
func (n *sessionNotifier) ConsumeSettleEvent(ctx context.Context, msg eventbus.Message) error {
	sid, kind, ok := eventbus.ParseSessionSettle(msg.Event.Topic)
	if !ok {
		return nil // foreign topic: pattern-filtered upstream; ack
	}
	if kind != subscriptionEventIdle && kind != subscriptionEventError {
		return nil // vocabulary is a convention — unknown settle kinds ack unread
	}
	title := msg.Event.Payload["title"]
	n.mu.Lock()
	notices := n.selectSettleNoticesLocked(sid, kind)
	n.mu.Unlock()
	for i := range notices {
		if title != "" && notices[i].Title == "" {
			notices[i].Title = title // the bus payload's title survives a cold tracker
		}
	}
	return n.deliverNotices(ctx, notices)
}

// selectSettleNoticesLocked runs the per-subscriber selection for one
// settle: event-set match, subscriber-side subtask exclusion (§4.6),
// §4.4 suppression — the identical gates on both delivery layers
// (direct v1 and bus consumer; design 0072 preserves the semantics
// and changes only the transport). Returns the delivery notices;
// caller holds mu.
func (n *sessionNotifier) selectSettleNoticesLocked(sid, kind string) []settleNotice {
	var notices []settleNotice
	now := time.Now()
	for _, row := range n.store.rowsForBroadcaster(sid) {
		if !subscriptionEventMatches(row.Events, kind) {
			continue
		}
		// Subscriber-side subtask exclusion (§4.6): registration
		// refuses, but the facts refresh can reveal parentage the
		// registration-time snapshot did not know.
		if n.facts != nil && n.facts.IsSubtask(row.Subscriber) {
			notifyCount(notifyOutcomeSkippedSubtask)
			continue
		}
		// §4.4 already-notified: the broadcaster's last completed
		// send_message to this subscriber postdates its last
		// edit/write/bash — the subscriber holds the final state, the
		// notification would be a duplicate the agent must triage.
		if n.suppression.suppresses(sid, row.Subscriber) {
			notifyCount(notifyOutcomeSuppressed)
			continue
		}
		title := ""
		if st := n.tracker[sid]; st != nil {
			title = st.title
		}
		notices = append(notices, settleNotice{
			Broadcaster: sid, Subscriber: row.Subscriber, Event: kind, Title: title, EnqueuedAt: now,
		})
	}
	return notices
}

// collectSettlesLocked enqueues (persists) the outbox intents for the
// selected settles and returns the delivery notices. Ordering
// contract: the intent is durable BEFORE any delivery is attempted
// (ruling 4). The bus path does NOT pass through here — the stream
// entry is its durable intent. Caller holds mu.
func (n *sessionNotifier) collectSettlesLocked(sid, kind string) []settleNotice {
	notices := n.selectSettleNoticesLocked(sid, kind)
	for _, notice := range notices {
		n.store.enqueue(pendingNotification{
			Broadcaster: notice.Broadcaster, Subscriber: notice.Subscriber,
			Event: notice.Event, EnqueuedAt: notice.EnqueuedAt,
		})
	}
	return notices
}

// subscriptionEventMatches answers whether the row's event set wakes
// on this settle kind.
func subscriptionEventMatches(events []string, kind string) bool {
	for _, ev := range events {
		if ev == kind {
			return true
		}
	}
	return false
}

// deliverNotices runs the delivery seam OUTSIDE the tracker lock (the
// seam must never call back into the notifier or block the fold) and
// clears the outbox intent per success (kept on failure — the boot
// retry's input; duplicates are tolerated by contract). NO artificial
// timeout wraps the seam call (§4.5): a busy subscriber queues
// server-side at its turn boundary and that queue-wait IS the wake-up
// — the vehicle (session_notifier_delivery.go) detaches the POST via
// context.WithoutCancel, so the cancellation parent here matters only
// to vehicles that choose to honor it. §8: one warn-level log per
// delivered notification with both phrases.
//
// The returned error aggregates HARD failures only (the bus consumer
// propagates it to leave the message pending — the consumer group's
// redelivery is the retry; the v1 direct leg ignores it, its retry
// being the outbox). Rate refusals are final outcomes, never errors.
func (n *sessionNotifier) deliverNotices(ctx context.Context, notices []settleNotice) error {
	var hard error
	for _, notice := range notices {
		err := n.cfg.Deliver.DeliverSettle(ctx, notice)
		if err != nil {
			if errors.Is(err, errNotifyRateLimited) {
				// The storm backstop refused this wake-up: drop the
				// intent (a boot retry would only deliver a stale
				// wake-up after the storm the bound just stopped) and
				// count the refusal.
				n.store.dismiss(pendingNotification{
					Broadcaster: notice.Broadcaster, Subscriber: notice.Subscriber,
					Event: notice.Event, EnqueuedAt: notice.EnqueuedAt,
				})
				notifyCount(notifyOutcomeRefusedRate)
				n.logger.Warn("session notifier: notification refused by the per-subscriber rate bound — dropped",
					zap.String("broadcaster", n.phrase(notice.Broadcaster)),
					zap.String("subscriber", n.phrase(notice.Subscriber)),
					zap.String("event", notice.Event))
				continue
			}
			hard = errors.Join(hard, fmt.Errorf("subscriber %s: %w", notice.Subscriber, err))
			// Delivery-layer-neutral wording: the retry owns the
			// redelivery on both layers (the outbox's boot retry on
			// the v1 leg; the consumer group's pending on the bus
			// leg) — naming one would misdirect incidents on the
			// other.
			n.logger.Warn("session notifier: settle delivery failed — the delivery layer's retry owns the redelivery",
				zap.String("broadcaster", notice.Broadcaster),
				zap.String("subscriber", notice.Subscriber),
				zap.String("event", notice.Event),
				zap.Error(err))
			continue
		}
		notifyCount(notifyOutcomeSent)
		n.logger.Warn("session notifier: settle notification delivered",
			zap.String("broadcaster", n.phrase(notice.Broadcaster)),
			zap.String("subscriber", n.phrase(notice.Subscriber)),
			zap.String("event", notice.Event))
		n.store.dismiss(pendingNotification{
			Broadcaster: notice.Broadcaster, Subscriber: notice.Subscriber,
			Event: notice.Event, EnqueuedAt: notice.EnqueuedAt,
		})
	}
	return hard
}

// RetryOutbox is ruling 4's boot drain: every intent persisted by a
// PREVIOUS generation (write-before-send left it on disk when the
// process died or the POST failed) is retried exactly ONCE; success
// clears it, failure drops it with a loud warn — at-least-once for the
// fired case, never an unbounded retry ladder. Duplicates are
// tolerated by contract (advisory wake-ups). Safe to call alongside
// Run: dismissal is by full-value equality, so a live generation's
// fresh intent (new EnqueuedAt) is never mistaken for a retry.
func (n *sessionNotifier) RetryOutbox(ctx context.Context) {
	if n == nil || n.store == nil || n.cfg.Deliver == nil {
		return
	}
	for _, p := range n.store.pendingNotifications() {
		notice := settleNotice{
			Broadcaster: p.Broadcaster, Subscriber: p.Subscriber,
			Event: p.Event, Title: n.titleFor(p.Broadcaster), EnqueuedAt: p.EnqueuedAt,
		}
		err := n.cfg.Deliver.DeliverSettle(ctx, notice)
		n.store.dismiss(p)
		if err != nil {
			if errors.Is(err, errNotifyRateLimited) {
				notifyCount(notifyOutcomeRefusedRate)
			}
			notifyCount(notifyOutcomeOutboxRetriedDropped)
			n.logger.Warn("session notifier: outbox retry failed — notification DROPPED (retry-once exhausted, ruling 4)",
				zap.String("broadcaster", n.phrase(p.Broadcaster)),
				zap.String("subscriber", n.phrase(p.Subscriber)),
				zap.String("event", p.Event),
				zap.Time("enqueuedAt", p.EnqueuedAt),
				zap.Error(err))
			continue
		}
		notifyCount(notifyOutcomeOutboxRetriedSent)
		n.logger.Warn("session notifier: outbox retry delivered",
			zap.String("broadcaster", n.phrase(p.Broadcaster)),
			zap.String("subscriber", n.phrase(p.Subscriber)),
			zap.String("event", p.Event))
	}
}

// titleFor reads the tracker's cached title for the compose body (the
// outbox intent carries none — titles live in the fold, and a restart
// may or may not have re-seen a SESSION_UPDATED yet; empty is the
// documented degraded wording).
func (n *sessionNotifier) titleFor(sid string) string {
	n.mu.Lock()
	defer n.mu.Unlock()
	if st := n.tracker[sid]; st != nil {
		return st.title
	}
	return ""
}

// deliveryContext snapshots the Run loop's context for the delivery
// leg. Kept as a named function so the fallback literal lives in one
// audited place: timer-fired deliveries before Run (or on a
// hand-driven fixture) have no parent — the real vehicle detaches via
// WithoutCancel anyway (§4.5), so the parent choice only affects
// pre-detach decisions and test seams.
func (n *sessionNotifier) deliveryContext() context.Context {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.deliverCtx != nil {
		return n.deliverCtx
	}
	return context.Background()
}

// ---- Resync (snapshots + Reseeded) ----

// resyncFromAuthority handles a Reseeded frame: the projection was
// rebuilt from store truth — resync from the stamped snapshot, emit
// nothing as a transition, but reconcile latch-vs-snapshot (§4.2). A
// nil Snapshot seam (unwired authority) degrades to dropping tracker
// state: no attribution, no emission — the safe direction.
func (n *sessionNotifier) resyncFromAuthority() {
	if n.cfg.Snapshot == nil {
		n.mu.Lock()
		n.tracker = map[string]*notifierSession{}
		n.mu.Unlock()
		return
	}
	n.resyncFromState(n.cfg.Snapshot())
}

// resyncFromPodSnapshot handles a connection's mandatory first frame.
// The snapshot's BusyComponents carry the projection's own derived
// busy — authoritative inputs, the same formula the fold mirrors.
func (n *sessionNotifier) resyncFromPodSnapshot(pod *abiv1.PodSnapshot) {
	n.mu.Lock()
	defer n.mu.Unlock()
	fresh := map[string]*notifierSession{}
	for _, snap := range pod.GetSessions() {
		id := canonicalFrameSessionID(snap.GetSessionId(), snap.GetInternalSessionId())
		if id == "" {
			continue
		}
		st := newNotifierSession()
		st.status = snap.GetStatus()
		st.streaming = snap.GetBusy().GetStreaming()
		st.queue = snap.GetBusy().GetQueueDepth()
		for _, p := range snap.GetInFlightParts() {
			st.upsertPart(p)
		}
		for _, in := range snap.GetPendingInputs() {
			switch in.GetKind() {
			case abiv1.InputKind_INPUT_KIND_QUESTION, abiv1.InputKind_INPUT_KIND_PERMISSION:
				if in.GetId() != "" {
					st.pending[in.GetId()] = struct{}{}
				}
			}
		}
		fresh[id] = st
	}
	n.reconcileAndSwapLocked(fresh)
}

// resyncFromState is the Authority.State() flavor (Reseeded frames).
func (n *sessionNotifier) resyncFromState(snap sessionstate.StateSnapshot) {
	n.mu.Lock()
	defer n.mu.Unlock()
	fresh := make(map[string]*notifierSession, len(snap.Sessions))
	for id, view := range snap.Sessions {
		if view == nil {
			continue
		}
		st := newNotifierSession()
		st.status = view.Status
		st.streaming = view.BusyComponents.GetStreaming()
		st.queue = view.BusyComponents.GetQueueDepth()
		for _, p := range view.InFlightParts {
			st.upsertPart(p)
		}
		for _, in := range view.PendingInputs {
			switch in.GetKind() {
			case abiv1.InputKind_INPUT_KIND_QUESTION, abiv1.InputKind_INPUT_KIND_PERMISSION:
				if in.GetId() != "" {
					st.pending[in.GetId()] = struct{}{}
				}
			}
		}
		fresh[id] = st
	}
	n.reconcileAndSwapLocked(fresh)
}

// reconcileAndSwapLocked runs the latch-vs-snapshot reconciliation per
// session and swaps the tracker to the fresh state (titles survive —
// neither snapshot flavor carries one). Caller holds mu.
//
// The rule (§4.2/§7.3): a busy the latch attributes (tracker busy OR
// the persisted LastDerivedBusy edge — the restart case) meeting a
// snapshot idle/error through the gates is a SETTLE — the episode
// finished while we were not watching live (dropped, reseeded, or
// restarted) and the saw-busy latch is the attribution. Without the
// latch (never observed busy), unknown→idle never notifies. Sessions
// the snapshot no longer lists are dropped without emission (a reseed
// rebuilt from store truth; absence is not a settle we can attribute).
func (n *sessionNotifier) reconcileAndSwapLocked(fresh map[string]*notifierSession) {
	// Preserve titles across the swap.
	for id, st := range fresh {
		if old := n.tracker[id]; old != nil && st.title == "" {
			st.title = old.title
		}
	}
	for id, st := range fresh {
		latch := n.store.latchFor(id)
		prevObserved := false
		if old := n.tracker[id]; old != nil {
			prevObserved = old.derivedBusy()
		}
		if !prevObserved && latch.LastDerivedBusy {
			prevObserved = true // the persisted busy edge (restart attribution)
		}
		nowBusy := st.derivedBusy()
		switch {
		case nowBusy:
			n.store.setLatch(id, sessionLatch{LastDerivedBusy: true, SawBusy: true})
		case prevObserved:
			n.store.setLatch(id, sessionLatch{LastDerivedBusy: false, SawBusy: latch.SawBusy})
			if !latch.SawBusy {
				break // busy edge latched but busy never observed — unattributable
			}
			if kind, ok := settleKindOf(st); ok && n.settleGatesPassLocked(id, st) {
				n.armDebounceLocked(id, kind)
			}
		}
	}
	n.tracker = fresh
}

// ---- Facts refresh + lazy cleanup ----

// RefreshFacts pulls ListSessions (the FactsPull seam) into the facts
// snapshot and prunes dead rows (§4.1 lazy cleanup). Task 4 wires the
// ticker; safe to call ad hoc.
func (n *sessionNotifier) RefreshFacts(ctx context.Context) {
	if n == nil || n.cfg.FactsPull == nil || n.facts == nil {
		return
	}
	sessions, err := n.cfg.FactsPull(ctx)
	if err != nil {
		n.logger.Warn("session notifier: facts refresh failed — keeping current facts", zap.Error(err))
		return
	}
	if hf, ok := n.facts.(*harnessSessionFacts); ok {
		hf.Load(sessions)
	}
	if dropped := n.store.pruneSessions(n.facts.IsGone); dropped > 0 {
		n.logger.Info("session notifier: pruned subscriptions of archived/deleted sessions",
			zap.Int("rows", dropped))
	}
}

// harnessSessionFacts is the ListSessions-backed sessionFacts: the
// ParentID join the design requires on BOTH sides (§4.6 — ParentID is
// durable on the wire list path but absent from the authority
// projection, investigation C1), plus lifecycle. Deleted detection is
// hysteresis-based: an id PREVIOUSLY listed that vanished is deleted
// (sticky) — the #1452 class (live sessions that never appear in the
// list) is immune because it was never previously-present. Archived is
// read directly off the record.
type harnessSessionFacts struct {
	mu          sync.Mutex
	subtasks    map[string]bool
	gone        map[string]bool
	prevPresent map[string]bool
}

func newHarnessSessionFacts() *harnessSessionFacts {
	return &harnessSessionFacts{
		subtasks:    map[string]bool{},
		gone:        map[string]bool{},
		prevPresent: map[string]bool{},
	}
}

// Load folds one ListSessions pull into the facts snapshot.
func (f *harnessSessionFacts) Load(sessions []session.Session) {
	if f == nil {
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	present := map[string]bool{}
	for i := range sessions {
		s := &sessions[i]
		if s.ID == "" {
			continue
		}
		present[s.ID] = true
		if s.ParentID != "" {
			f.subtasks[s.ID] = true
		}
		if s.Archived {
			f.gone[s.ID] = true // sticky: rows were already dropped lazily
		}
	}
	for id := range f.prevPresent {
		if !present[id] {
			f.gone[id] = true // previously listed, now absent — deleted
		}
	}
	f.prevPresent = present
}

func (f *harnessSessionFacts) IsSubtask(sessionID string) bool {
	if f == nil {
		return false
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.subtasks[sessionID]
}

func (f *harnessSessionFacts) IsGone(sessionID string) bool {
	if f == nil {
		return false
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.gone[sessionID]
}

// pullHarnessSessionList is the FactsPull adapter over the loopback
// seam client (seamClientWithPassword's type — task 4 wires it): the
// raw GET /session body parsed through the contract translator — the
// ONLY surface that carries ParentID (translate.go).
func pullHarnessSessionList(ctx context.Context, client *opencode.Client) ([]session.Session, error) {
	raw, err := client.SessionListRaw(ctx)
	if err != nil {
		return nil, err
	}
	return opencode.ParseSessionListWire(raw, os.Getenv("WORKSPACE_ID"))
}
