// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

// relay_liveness.go — US-72.4 (design 0058 §4.5/§4.8): relay-only
// liveness for token-emitted providers. The builder (API-side) marks
// llm-provider entries with relay metadata (relay / relayExpiresAt /
// relayRevision); the durable batch file carries those entries into the
// pod; this monitor scans it and watches the relay path:
//
//   - token expiry is honored LOCALLY (metadata relayExpiresAt → the
//     token_expired degrade) — no router round-trip needed to know a
//     token is past its TTL;
//   - reachability + credential health is probed through the router
//     itself (GET {baseURL}/models with the scoped token — the exact
//     request EnrichProviders issues; the router serves it from the
//     staged catalog, zero upstream). A transport failure is
//     relay_unreachable; a machine-readable router rejection surfaces
//     its reason verbatim (credential_stale / scope_violation /
//     sanitization_refused / quota_exceeded — the CredentialStale /
//     CredentialRejected feeds the US-72.3 classifier consumes);
//   - a boot-time outage degrades LOUDLY and re-arms on the generalized
//     bounded-backoff loop (rearm_loop.go, #910/US-72.0) — the pod
//     never crashloops and never strands: the §4.8 no-fallback
//     property's bounded-retry half. A healthy-cadence watchdog keeps
//     probing (30s default) so mid-pod-life outages are caught too; the
//     CAS guard keeps exactly one re-arm loop in flight.
//
// Surfacing: cached snapshot only — healthz/readyz/statusz perform no
// I/O (US-22.1 contract). The controller mirrors the degrade code into
// SecretsDelivery (the US-70.1 path) where the US-72.3 classification
// picks it up. Security: the token exists in agentd memory by design
// (it IS the delivered credential, D3) but is NEVER logged and never
// leaves a probe to anything but the provider's own router path.

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"go.uber.org/zap"

	"github.com/lenaxia/llmsafespaces/pkg/agentd"
	agentdsecrets "github.com/lenaxia/llmsafespaces/pkg/agentd/secrets"
	sec "github.com/lenaxia/llmsafespaces/pkg/secrets"
)

// Degrade codes (design 0058 §4.5/§4.6; the US-72.3 classifier's
// vocabulary — classifyRelayStale / relayRouterRejection).
const (
	relayDegradeUnreachable  = "relay_unreachable"
	relayDegradeTokenExpired = "token_expired"
)

// relayLivenessLoopName is the re-arm metric series for this consumer
// (rearm_loop.go's generalized vec — the injector is series one, this
// the second per US-72.0's story).
const relayLivenessLoopName = "relay_liveness"

// Probe outcome strings (the re-arm Attempt's taxonomy; also reused as
// the degrade code where they coincide).
const (
	relayProbeOutcomeSuccess = "success"
	// relayEvalAbsent is evaluate's "nothing relay-fronted observed"
	// sentinel — not a degrade, and terminal for a re-arm attempt (there
	// is nothing to recover by probing; standing down re-opens the CAS
	// guard for a future relay batch).
	relayEvalAbsent = "absent"
)

// relayRouterRejectBodies is the router's machine-readable rejection
// body shape (cmd/relay-router byoReject): {"error":"relay_rejected",
// "reason":"<code>","detail":"..."}.
type relayRouterRejectBody struct {
	Error  string `json:"error"`
	Reason string `json:"reason"`
}

// relayProviderRef is one relay-fronted provider observed in the batch.
type relayProviderRef struct {
	Slug      string
	BaseURL   string // the router path (router origin + /w/<ws>/<slug>/v1)
	Token     string // scoped relay token — memory only, never logged
	ExpiresAt time.Time
	HasExpiry bool
}

// relayRegistry owns the batch-file scan. Rescanned on every monitor
// tick so a resync delivering fresh tokens (new revision, renewed
// expiry) is picked up without a pod restart — and so a batch that
// arrives after agentd starts (sidecar boot ordering) is not missed.
type relayRegistry struct {
	batchPath string
	mu        sync.Mutex
	refs      []relayProviderRef
	revision  string
}

func newRelayRegistry(batchPath string) *relayRegistry {
	return &relayRegistry{batchPath: batchPath}
}

// rescan reloads the relay entries from the durable batch file. Absent
// or unparsable file ⇒ empty refs (nothing relay-fronted observed —
// present=false, no degrade: a flag-off pod must stay silent).
func (r *relayRegistry) rescan() {
	bf, err := agentdsecrets.LoadBatchFile(r.batchPath)
	if err != nil {
		r.mu.Lock()
		r.refs = nil
		r.mu.Unlock()
		return
	}
	var refs []relayProviderRef
	revision := ""
	for _, s := range bf.Secrets {
		if s.Type != string(sec.SecretTypeLLMProvider) || s.Metadata[sec.RelayMetadataKey] != "true" {
			continue
		}
		var pd sec.LLMProviderData
		if json.Unmarshal([]byte(s.Plaintext), &pd) != nil || pd.BaseURL == "" {
			continue
		}
		ref := relayProviderRef{Slug: pd.Slug, BaseURL: pd.BaseURL, Token: pd.APIKey}
		if exp := s.Metadata[sec.RelayMetadataExpiresAtKey]; exp != "" {
			if t, err := time.Parse(time.RFC3339, exp); err == nil {
				ref.ExpiresAt, ref.HasExpiry = t, true
			}
		}
		if rev := s.Metadata[sec.RelayMetadataRevisionKey]; rev != "" {
			revision = rev
		}
		refs = append(refs, ref)
	}
	r.mu.Lock()
	r.refs, r.revision = refs, revision
	r.mu.Unlock()
}

func (r *relayRegistry) snapshot() ([]relayProviderRef, string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]relayProviderRef, len(r.refs))
	copy(out, r.refs)
	return out, r.revision
}

// relayLivenessMonitor is the cached-state owner. evaluate() runs on
// the watchdog cadence and (via the re-arm Attempt) on the bounded
// backoff; snapshot() is what healthz/readyz/statusz read.
type relayLivenessMonitor struct {
	registry *relayRegistry
	client   *http.Client
	now      func() time.Time

	// evalMu serializes evaluations (watchdog vs re-arm attempt): a
	// probe cycle holds it so state writes stay strictly ordered.
	evalMu sync.Mutex
	// stateMu guards ONLY the cached snapshot — a probe never blocks a
	// healthz read.
	stateMu sync.Mutex
	state   agentd.RelayHealth

	// armed is the single-re-arm CAS guard (0 = no re-arm loop in
	// flight). Set when arming; cleared by the re-arm loop itself on
	// success/already-applied (the only paths that end it while the
	// outage is resolved — a ctx cancel ends the process anyway).
	armed int32
}

func newRelayLivenessMonitor(batchPath string, client *http.Client) *relayLivenessMonitor {
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Second}
	}
	return &relayLivenessMonitor{
		registry: newRelayRegistry(batchPath),
		client:   client,
		now:      time.Now,
	}
}

// snapshot returns the cached RelayHealth (never I/O).
func (m *relayLivenessMonitor) snapshot() *agentd.RelayHealth {
	m.stateMu.Lock()
	defer m.stateMu.Unlock()
	s := m.state
	return &s
}

// healthyNow reports whether the cached state is a non-degraded,
// observed relay set (the re-arm Applied gate).
func (m *relayLivenessMonitor) healthyNow() bool {
	m.stateMu.Lock()
	defer m.stateMu.Unlock()
	return m.state.Present && m.state.Reachable && m.state.DegradedReason == ""
}

// evaluate runs one full cycle: rescan → local expiry → credential
// probes → state store. Returns the resulting degrade code ("" healthy,
// "absent" when nothing relay-fronted is observed — no re-arm for the
// not-present case: there is nothing to recover by probing).
func (m *relayLivenessMonitor) evaluate(ctx context.Context) string {
	m.evalMu.Lock()
	defer m.evalMu.Unlock()

	m.registry.rescan()
	refs, revision := m.registry.snapshot()
	now := m.now()

	next := agentd.RelayHealth{AppliedRevision: revision}
	if len(refs) == 0 {
		m.store(next)
		return relayEvalAbsent
	}
	next.Present = true
	next.RouterURL = routerOrigin(refs[0].BaseURL)

	// Local expiry first (honor ExpiresAt — no probe needed to know).
	earliest := time.Time{}
	for _, ref := range refs {
		if ref.HasExpiry && (earliest.IsZero() || ref.ExpiresAt.Before(earliest)) {
			earliest = ref.ExpiresAt
		}
	}
	if !earliest.IsZero() {
		next.TokenExpiresAt = earliest.UTC().Format(time.RFC3339)
		if now.After(earliest) {
			next.Reachable = false
			next.DegradedReason = relayDegradeTokenExpired
			next.LastProbeAt = now.Unix()
			m.store(next)
			return relayDegradeTokenExpired
		}
	}

	// Credential-level probe per provider (the router serves /models
	// from its staged catalog — zero upstream, zero key bytes).
	code := ""
	for _, ref := range refs {
		probeCode, reachable := m.probeOne(ctx, ref)
		if !reachable {
			next.Reachable = false
		}
		if probeCode != "" && codePriority(probeCode) > codePriority(code) {
			code = probeCode
		}
	}
	next.Reachable = code == ""
	next.DegradedReason = code
	next.LastProbeAt = now.Unix()
	m.store(next)
	return code
}

// store swaps the cached snapshot (under stateMu only).
func (m *relayLivenessMonitor) store(s agentd.RelayHealth) {
	m.stateMu.Lock()
	m.state = s
	m.stateMu.Unlock()
}

// codePriority orders co-present degrade codes: token expiry is the
// most terminal (renewal owns it), corruption/stale next, rejections
// (operator-visible anomalies), generic unreachability last.
func codePriority(code string) int {
	switch code {
	case "":
		return 0
	case relayDegradeTokenExpired:
		return 4
	case "credential_stale":
		return 3
	case "scope_violation", "sanitization_refused", "quota_exceeded":
		return 2
	default: // relay_unreachable and anything unknown
		return 1
	}
}

// probeOne issues GET {baseURL}/models with the scoped token — the
// exact request EnrichProviders issues through the router. Returns the
// degrade code ("" healthy) and whether the relay path answered.
func (m *relayLivenessMonitor) probeOne(ctx context.Context, ref relayProviderRef) (string, bool) {
	target := strings.TrimSuffix(ref.BaseURL, "/") + "/models"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return relayDegradeUnreachable, false
	}
	req.Header.Set("Authorization", "Bearer "+ref.Token)
	req.Header.Set("Accept", "application/json")

	resp, err := m.client.Do(req)
	if err != nil {
		// Transport failure (router down / DNS / timeout). The error may
		// name the URL but never the token — still, keep it out of the
		// degrade path: the code is the signal.
		return relayDegradeUnreachable, false
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode == http.StatusOK {
		return "", true
	}
	// Parse the router's machine-readable rejection body when present.
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
	var reject relayRouterRejectBody
	if json.Unmarshal(body, &reject) == nil && reject.Error == "relay_rejected" && reject.Reason != "" {
		if reject.Reason == "unauthorized" {
			// HMAC-valid-but-rejected or expired token: the local expiry
			// clock already caught the expired case above, so this is a
			// signing-key / keyID mismatch — the wrong-pub corruption
			// class (§4.2's one escalating cause).
			return "credential_stale", false
		}
		return reject.Reason, false
	}
	// Any other non-200 (5xx, malformed body): the relay path is not
	// serving this workspace — the generic unreachable code.
	return relayDegradeUnreachable, false
}

// routerOrigin derives scheme://host:port from a provider baseURL (the
// router's origin, no path) for reporting — never a secret.
func routerOrigin(baseURL string) string {
	u, err := url.Parse(baseURL)
	if err != nil || u.Host == "" {
		return ""
	}
	return u.Scheme + "://" + u.Host
}

// relayLivenessConfig parameterizes the loop (all test seams).
type relayLivenessConfig struct {
	// Interval is the healthy-cadence watchdog tick (0 → default 30s;
	// env-overridable LLMSAFESPACE_RELAY_LIVENESS_INTERVAL).
	Interval time.Duration
	// RearmMinDelay/RearmMaxDelay bound the degraded-phase backoff (0 →
	// the #910 defaults 5m/30m via rearm_loop.go).
	RearmMinDelay time.Duration
	RearmMaxDelay time.Duration
	// Logger; nil → the package logger captured at start.
	log *zap.Logger
}

func relayLivenessIntervalFromEnv() time.Duration {
	if v := os.Getenv("LLMSAFESPACE_RELAY_LIVENESS_INTERVAL"); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d > 0 {
			return d
		}
	}
	return 30 * time.Second
}

// startRelayLiveness launches the monitor loop (watchdog + CAS-guarded
// re-arm). Runs for the pod's lifetime; a flag-off pod (no relay
// entries ever) ticks cheaply: one batch-file rescan per interval, no
// HTTP. The monitor is returned immediately for healthz wiring.
func startRelayLiveness(ctx context.Context, wg *sync.WaitGroup, mon *relayLivenessMonitor, cfg relayLivenessConfig) {
	lg := cfg.log
	if lg == nil {
		lg = log
	}
	interval := cfg.Interval
	if interval <= 0 {
		interval = relayLivenessIntervalFromEnv()
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			// First tick runs immediately: boot-time outage detection
			// (§4.8) must not wait a full interval.
			code := mon.evaluate(ctx)
			if code != "" && code != relayEvalAbsent {
				lg.Warn("relay liveness: relay path degraded",
					zap.String("code", code))
				mon.armRearm(ctx, cfg, lg)
			}
			select {
			case <-ctx.Done():
				return
			case <-time.After(interval):
			}
		}
	}()
}

// armRearm starts the bounded re-arm loop unless one is already in
// flight (the CAS guard — exactly one re-arm by construction). The
// re-arm Attempt is one evaluate cycle; success (or the healthy-cadence
// watchdog having healed the state first — the Applied gate) disarms
// and re-opens the guard so a LATER outage can arm again.
func (m *relayLivenessMonitor) armRearm(ctx context.Context, cfg relayLivenessConfig, lg *zap.Logger) {
	if !atomic.CompareAndSwapInt32(&m.armed, 0, 1) {
		return
	}
	disarm := func() { atomic.StoreInt32(&m.armed, 0) }
	startRearmLoop(ctx, rearmLoopConfig{
		Loop: relayLivenessLoopName,
		Applied: func() bool {
			if m.healthyNow() {
				disarm() // the watchdog healed it mid-backoff — stand down
				return true
			}
			return false
		},
		Attempt: func(attemptCtx context.Context) rearmAttemptResult {
			code := m.evaluate(attemptCtx)
			switch code {
			case "", relayEvalAbsent:
				// Healthy — or the relay batch vanished (flag flipped off
				// mid-life): nothing to recover either way; stand down and
				// re-open the guard.
				disarm()
				return rearmAttemptResult{outcome: relayProbeOutcomeSuccess, applied: true}
			default:
				lg.Warn("relay liveness: re-arm attempt failed", zap.String("code", code))
				return rearmAttemptResult{outcome: code, retryable: true}
			}
		},
		MinDelay: cfg.RearmMinDelay,
		MaxDelay: cfg.RearmMaxDelay,
	})
}

// relayLivenessSnapshotFor is the deps-adapter the HTTP handlers read
// (nil-safe: a nil monitor reports nothing — tests, partial wiring).
func relayLivenessSnapshotFor(m *relayLivenessMonitor) func() *agentd.RelayHealth {
	return func() *agentd.RelayHealth {
		if m == nil {
			return nil
		}
		return m.snapshot()
	}
}

// relayLivenessWarning renders the healthz warning line (the
// spawnEnvWarning precedent) — never secrets, only the code.
func relayLivenessWarning(h *agentd.RelayHealth) string {
	if h == nil || !h.Present || h.DegradedReason == "" {
		return ""
	}
	return "degraded:" + h.DegradedReason
}
