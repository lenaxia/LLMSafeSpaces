// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

// admission_watch.go — #2045: the prompt-path admission canary.
//
// opencode answers plain HTTP quickly (fast 401s, green /global/health)
// while ONE session's per-session prompt pipeline is dead — every
// POST /session/:id/message for that session hangs to the client
// timeout (the "No user message found in stream" wedge, Nov 13-15
// 2026). The supervisor's health model (TCP dial + /global/health)
// structurally cannot see it; the incident needed a manual restart.
//
// This component counts ADMISSION failures — POSTs that die awaiting
// response headers (client timeout / context deadline) — per session:
//
//   - a fast provider 4xx refusal returns quickly and NEVER counts
//     (auth/quota errors are not the wedge);
//   - a slow response BODY is a successful admission (http.Client.Do
//     returned when the headers landed; the drain never classifies)
//     and NEVER counts;
//   - a canceled context (agentd shutdown) is explicitly NOT a
//     deadline and never counts.
//
// ≥3 classified failures on the SAME session inside a 10-minute window
// declare a wedge suspect and request ONE supervised restart through
// the EXISTING session-aware decision seam (makeSessionAwareRestartDecision
// — busy-session deferral, the progress-keyed stall force path,
// markers, metrics, and the restart log all apply unchanged; reused,
// not reinvented). A fire-anchored storm guard bounds restarts to at
// most one per 30 minutes per workspace: anchored on the last FIRE
// (not the last attempt) so a persistent wedge keeps retrying at the
// ~30-minute cadence instead of starving itself silent; suppressed
// requests still count via workspace_restarts_suppressed_total.
//
// The request arm is TIMESTAMPED (re-arms every window), never a
// boolean latch: under a continuous failure stream the ring never
// decays below the threshold, and a latched request would go silent
// forever after the first fire — the exact opposite of the incident's
// recovery goal.

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"sync"
	"time"

	"go.uber.org/zap"
)

// RestartReasonPromptPathWedge's reason string lives in restart_reason.go
// (marker + metric label) and control_socket.go (closed enum).

// admissionWatch tunables (the issue's R1/R2 numbers).
const (
	// admissionWindow is the strike ring's width.
	admissionWindow = 10 * time.Minute
	// admissionStrikes is the per-session threshold inside the window.
	admissionStrikes = 3
	// admissionStormGuard bounds FIRED restarts per workspace.
	admissionStormGuard = 30 * time.Minute
)

// errAdmissionTimeout classifies a POST /session/:id/message that died
// awaiting response headers (client whole-request timeout or context
// deadline). Only this class feeds the canary.
type errAdmissionTimeout struct {
	sessionID string
	cause     error
}

func (e *errAdmissionTimeout) Error() string {
	return fmt.Sprintf("POST /session/%s/message: admission timeout (awaiting headers): %v", e.sessionID, e.cause)
}

func (e *errAdmissionTimeout) Unwrap() error { return e.cause }

// classifyAdmission returns an *errAdmissionTimeout wrapping err when
// err is the admission-timeout class (context.DeadlineExceeded or a
// url.Error whose wrapped error is a timeout — http.Client's
// whole-request timeout shape); nil otherwise. context.Canceled is
// explicitly NOT a deadline: agentd shutdown never reads as a wedge.
// A slow BODY never reaches the classifier (Do already returned).
func classifyAdmission(sessionID string, err error) error {
	if err == nil {
		return nil
	}
	// Already classified (a caller re-feeding an errAdmissionTimeout —
	// e.g. the turn-resume seam receiving one from a wrapped prompter):
	// stays classified, session id preserved.
	var prior *errAdmissionTimeout
	if errors.As(err, &prior) {
		return prior
	}
	if errors.Is(err, context.Canceled) {
		return nil
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return &errAdmissionTimeout{sessionID: sessionID, cause: err}
	}
	var ue *url.Error
	if errors.As(err, &ue) && ue.Timeout() {
		return &errAdmissionTimeout{sessionID: sessionID, cause: err}
	}
	return nil
}

// isAdmissionTimeout reports whether err (possibly wrapped) is the
// classified admission-timeout class.
func isAdmissionTimeout(err error) bool {
	var at *errAdmissionTimeout
	return errors.As(err, &at)
}

// admissionSessionState is one session's canary ledger.
type admissionSessionState struct {
	// strikes is the admission-failure ring (pruned to the window).
	strikes []time.Time
	// lastSuspectAt stamps the window's suspect declaration (the
	// suspects counter dedupes on it; suspectSessions reads it).
	lastSuspectAt time.Time
	// requestedAt stamps the last restart request for this session's
	// strike run; it re-arms after admissionWindow, so a CONTINUOUS
	// failure stream (the ring never decaying below the threshold)
	// still re-requests every window instead of latching silent after
	// the first fire — the storm guard bounds how often those requests
	// fire, the re-arm bounds how long they can stop.
	requestedAt time.Time
}

func (st *admissionSessionState) prune(cutoff time.Time) {
	kept := st.strikes[:0]
	for _, at := range st.strikes {
		if at.After(cutoff) {
			kept = append(kept, at)
		}
	}
	st.strikes = kept
}

// admissionWatch counts classified admission failures per session and
// escalates wedge suspects into the session-aware restart seam. The
// zero-config constructor is safe for every topology; the fire trigger
// is installed at boot (managed process or control-socket proc) and
// may stay nil (observability only).
type admissionWatch struct {
	mu  sync.Mutex
	now func() time.Time

	window    time.Duration
	threshold int
	storm     time.Duration

	sessions map[string]*admissionSessionState
	// lastFire anchors the storm guard on the last FIRED restart (not
	// the last request): a suppressed request does not push the guard
	// forward, so a persistent wedge retries at the cadence instead of
	// starving.
	lastFire time.Time

	// fire requests the supervised restart with the current suspect
	// list. Called ASYNC (send paths must never block seconds on a
	// supervised restart — the same posture as the health watchdog's
	// untracked `go restarter.restart()`).
	fire func(suspects []string)
}

// pkgAdmissionWatch is the process-wide canary the two send paths feed
// (ResumeTurn via the turn-resume supervisor; the adapter send_message
// background delivery). The fire trigger is installed at boot.
var pkgAdmissionWatch = newAdmissionWatch(nil)

func newAdmissionWatch(fire func(suspects []string)) *admissionWatch {
	return &admissionWatch{
		now:       time.Now,
		window:    admissionWindow,
		threshold: admissionStrikes,
		storm:     admissionStormGuard,
		sessions:  map[string]*admissionSessionState{},
		fire:      fire,
	}
}

// setFire installs (or replaces) the restart trigger.
func (w *admissionWatch) setFire(fire func(suspects []string)) {
	w.mu.Lock()
	w.fire = fire
	w.mu.Unlock()
}

// record feeds one send-path error to the canary. Non-classified
// errors (fast 4xx, canceled contexts, plain failures) are ignored —
// they never strike. A threshold crossing declares the session a wedge
// suspect (counter deduped per session per window) and, if the
// session's request has re-armed, requests the restart through the
// storm guard.
func (w *admissionWatch) record(err error) {
	if w == nil {
		return
	}
	var at *errAdmissionTimeout
	if !errors.As(err, &at) || at.sessionID == "" {
		return
	}
	now := w.now()

	var (
		newSuspect bool
		suppressed bool
		fire       func([]string)
		suspects   []string
	)
	w.mu.Lock()
	st := w.sessionState(at.sessionID)
	st.prune(now.Add(-w.window))
	st.strikes = append(st.strikes, now)
	if len(st.strikes) >= w.threshold {
		if now.Sub(st.lastSuspectAt) >= w.window {
			st.lastSuspectAt = now
			newSuspect = true
		}
		if now.Sub(st.requestedAt) >= w.window {
			st.requestedAt = now
			if w.lastFire.IsZero() || now.Sub(w.lastFire) >= w.storm {
				w.lastFire = now
				fire = w.fire
				suspects = w.suspectSessionsLocked(now)
			} else {
				suppressed = true
			}
		}
	}
	w.mu.Unlock()

	pkgOpsMetrics.RecordMessageAdmissionFailure(workspaceIDFromEnv())
	if newSuspect {
		pkgOpsMetrics.RecordSessionWedgeSuspect(workspaceIDFromEnv())
		log.Warn("prompt-path admission canary: session wedge-suspect declared",
			zap.String("sessionID", at.sessionID),
			zap.Int("strikes", w.strikeCount(at.sessionID)),
			zap.Duration("window", w.window))
	}
	if suppressed {
		pkgOpsMetrics.RecordRestartSuppressed(workspaceIDFromEnv(), RestartReasonPromptPathWedge)
		log.Info("prompt-path admission canary: restart request suppressed by the storm guard",
			zap.Duration("sinceLastFire", now.Sub(w.lastFireStamp())),
			zap.Duration("stormGuard", w.storm))
	}
	if fire != nil {
		go fire(suspects)
	}
}

// noteSuspect declares a suspect from the R4 turn-resume detector (a
// repeated resume-prompt failure within the resume cooldown). Shares
// the same per-session per-window dedupe as the ring's declarations —
// the suspects counter ticks once per session per window regardless of
// which detector declared first. Reports whether this call declared.
func (w *admissionWatch) noteSuspect(sessionID string) bool {
	if w == nil || sessionID == "" {
		return false
	}
	now := w.now()
	w.mu.Lock()
	st := w.sessionState(sessionID)
	if now.Sub(st.lastSuspectAt) < w.window {
		w.mu.Unlock()
		return false
	}
	st.lastSuspectAt = now
	w.mu.Unlock()
	pkgOpsMetrics.RecordSessionWedgeSuspect(workspaceIDFromEnv())
	return true
}

// suspect reports whether the session currently holds ≥threshold
// strikes inside the window.
func (w *admissionWatch) suspect(sessionID string) bool {
	if w == nil {
		return false
	}
	now := w.now()
	w.mu.Lock()
	defer w.mu.Unlock()
	st, ok := w.sessions[sessionID]
	if !ok {
		return false
	}
	st.prune(now.Add(-w.window))
	return len(st.strikes) >= w.threshold
}

// suspectSessionsLocked lists the currently-suspect sessions (sorted —
// stable restart-reason attribution). Caller holds w.mu.
func (w *admissionWatch) suspectSessionsLocked(now time.Time) []string {
	var out []string
	for sid, st := range w.sessions {
		st.prune(now.Add(-w.window))
		if len(st.strikes) >= w.threshold {
			out = append(out, sid)
		}
	}
	// Deterministic order for the restart reason string.
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j] < out[j-1]; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}

func (w *admissionWatch) sessionState(sessionID string) *admissionSessionState {
	st, ok := w.sessions[sessionID]
	if !ok {
		st = &admissionSessionState{}
		w.sessions[sessionID] = st
	}
	return st
}

// strikeCount is the session's current in-window strike count
// (post-prune; log attribution).
func (w *admissionWatch) strikeCount(sessionID string) int {
	w.mu.Lock()
	defer w.mu.Unlock()
	st, ok := w.sessions[sessionID]
	if !ok {
		return 0
	}
	st.prune(w.now().Add(-w.window))
	return len(st.strikes)
}

// lastFireStamp mirrors the storm-guard anchor for log attribution.
func (w *admissionWatch) lastFireStamp() time.Time {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.lastFire
}

// wedgeRestartReason renders the restart-log reason: the class plus
// the offending session id(s) — "prompt_path_wedge sessions=a,b".
func wedgeRestartReason(suspects []string) string {
	if len(suspects) == 0 {
		return RestartReasonPromptPathWedge
	}
	return RestartReasonPromptPathWedge + " sessions=" + strings.Join(suspects, ",")
}

// writeWedgeMarker stamps the wedge class's restart-reason marker at
// the decision site (every restart class writes its own marker — the
// relayKillWithMarker pattern).
func writeWedgeMarker() {
	if err := writeRestartReasonMarker(markerPathFromEnv(), RestartReasonPromptPathWedge, nil); err != nil {
		log.Error("failed to write restart-reason marker", zap.Error(err))
		pkgOpsMetrics.RecordMarkerWriteFailure(workspaceIDFromEnv(), RestartReasonPromptPathWedge)
	} else {
		logRestartReasonAtWrite(RestartReasonPromptPathWedge, nil, log.Core())
	}
}

// wedgeRestartFunc is the single-container (managed-process) fire
// trigger: marker + restart metric + restart-log entry (reason carries
// the offending session ids) + the EXISTING session-aware restart
// decision — busy-session deferral, the progress-keyed stall force
// path, interrupt-first grace, and the deferral metric all apply
// unchanged. pollInterval is captured at CONSTRUCTION (reading the
// package var at fire time races test overrides — the deferral pins
// shrink it).
func wedgeRestartFunc(
	bgCtx context.Context,
	bgWg *sync.WaitGroup,
	proc restartableProcess,
	tracker *sessionStatusTracker,
	lister sessionLister,
	interrupter sessionInterrupter,
	restarts *agentRestartLog,
	pollInterval time.Duration,
) func(suspects []string) {
	return func(suspects []string) {
		writeWedgeMarker()
		pkgOpsMetrics.RecordRestart(workspaceIDFromEnv(), RestartReasonPromptPathWedge)
		if restarts != nil {
			restarts.record(wedgeRestartReason(suspects), "", "", time.Now())
		}
		log.Warn("prompt-path wedge: requesting session-aware opencode restart",
			zap.String("reason", wedgeRestartReason(suspects)))
		//nolint:contextcheck // bgCtx is the agentd background lifecycle context — the deferred goroutine must outlive the send path and be canceled at shutdown
		makeSessionAwareRestartDecision(bgCtx, proc, tracker, restartDecisionConfig{
			PollInterval: pollInterval,
			StallBound:   restartStallBound,
			GraceWindow:  defaultInterruptGrace,
			Lister:       lister,
			Interrupter:  interrupter,
			BgWg:         bgWg,
		})
	}
}

// wedgeRestartProc is the sidecar fire trigger: the wedge restart
// crosses the control socket with the closed-enum reason (never
// misattributed as credential_reload — the supervisor's rev gate only
// ever suppresses credential reloads) and the OUTCOME-trUTHFUL metric
// pair is recorded in this (sidecar) registry — the one the agentd
// PodMonitor scrapes (the socketReloadProc pattern).
type wedgeRestartProc struct {
	cc       *controlClient
	restarts *agentRestartLog
}

func (p *wedgeRestartProc) fire(suspects []string) {
	if p == nil || p.cc == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	res, err := p.cc.Restart(ctx, RestartReasonPromptPathWedge, int(defaultRestartGrace/time.Second))
	if err != nil {
		// Transport failure: the restart's OUTCOME is unknown (the
		// supervisor answers synchronously — the child may well have
		// been restarted). Log loudly; no counter (the truthful-when-
		// observable contract).
		log.Warn("prompt-path wedge: restart outcome unobservable — control socket round trip failed",
			zap.Error(err))
	} else if res != nil && res.Restarted {
		pkgOpsMetrics.RecordRestart(workspaceIDFromEnv(), RestartReasonPromptPathWedge)
	} else if res != nil && !res.InProgress {
		pkgOpsMetrics.RecordRestartSuppressed(workspaceIDFromEnv(), RestartReasonPromptPathWedge)
	}
	writeWedgeMarker()
	if p.restarts != nil {
		lastErr := ""
		if err != nil {
			lastErr = err.Error()
		}
		p.restarts.record(wedgeRestartReason(suspects), "", lastErr, time.Now())
	}
}

// installAdmissionWatchRestart wires the single-container fire trigger
// into the process-wide canary (main.go, after the managed process and
// its collaborators exist). Nil proc (server-only boot) leaves the
// canary in observability-only mode.
func installAdmissionWatchRestart(
	bgCtx context.Context,
	bgWg *sync.WaitGroup,
	proc restartableProcess,
	tracker *sessionStatusTracker,
	lister sessionLister,
	interrupter sessionInterrupter,
	restarts *agentRestartLog,
) {
	if proc == nil {
		return
	}
	pi := restartIdleCheckInterval // construction-time capture (test overrides race guard)
	pkgAdmissionWatch.setFire(wedgeRestartFunc(bgCtx, bgWg, proc, tracker, lister, interrupter, restarts, pi))
}

// installAdmissionWatchRestartSidecar wires the sidecar fire trigger
// (buildSidecarDeps topology: restarts cross the control socket).
func installAdmissionWatchRestartSidecar(cc *controlClient, restarts *agentRestartLog) {
	if cc == nil {
		return
	}
	pkgAdmissionWatch.setFire((&wedgeRestartProc{cc: cc, restarts: restarts}).fire)
}
