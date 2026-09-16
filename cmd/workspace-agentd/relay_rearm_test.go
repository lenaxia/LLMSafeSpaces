// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

// relay_rearm_test.go — US-72.0/#910: the injector as the first consumer
// of the generalized re-arm loop. Red-first per the story test plan:
//
//	fault-injection e2e — catalog source down at T+0, up later → relay
//	applied, no manual restart (no second startRelayInjector call)
//	incident regression — the 2026-08-16 workspace 946a442f class
//	(decode /provider: unexpected EOF → permanent unresolvable default)
//	now recovers within bounded backoff
//	busy gate / deferred-kill gate — no attempt while sessions are busy
//	or a deferred restart is outstanding; no stacked restarts
//	HasRelay() short-circuit preserved once applied; readyz truthful
//	per cycle

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	promtestutil "github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lenaxia/llmsafespaces/pkg/agent"
	opencode "github.com/lenaxia/llmsafespaces/pkg/agent/opencode"
)

// providerFaultServer is a fault-injectable stand-in for the opencode
// /provider endpoint (the injector's catalog source). Modes:
//
//	0 — HTTP 500 (the router/catalog-outage class)
//	1 — truncated JSON body (the 2026-08-16 "decode /provider:
//	    unexpected EOF" class, workspace 946a442f)
//	2 — healthy free-model catalog
type providerFaultServer struct {
	srv      *httptest.Server
	mode     atomic.Int32
	hits     atomic.Int32
	inFlight atomic.Int32
	maxInFly atomic.Int32
}

const (
	providerModeFail500   = 0
	providerModeTruncated = 1
	providerModeOK        = 2
)

func newProviderFaultServer(mode int32) *providerFaultServer {
	p := &providerFaultServer{}
	p.mode.Store(mode)
	p.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		p.hits.Add(1)
		cur := p.inFlight.Add(1)
		for {
			max := p.maxInFly.Load()
			if cur <= max || p.maxInFly.CompareAndSwap(max, cur) {
				break
			}
		}
		defer p.inFlight.Add(-1)
		switch p.mode.Load() {
		case providerModeFail500:
			w.WriteHeader(http.StatusInternalServerError)
		case providerModeTruncated:
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"connected":["open`)) // the EOF class
		default:
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{
				"connected": ["opencode"],
				"all": [{"id":"opencode","models":{
					"free-model":{"id":"free-model","name":"Free Model","cost":{"input":0,"output":0},"limit":{"context":100000,"output":10000}}
				}}]
			}`))
		}
	}))
	return p
}

func (p *providerFaultServer) heal()            { p.mode.Store(providerModeOK) }
func (p *providerFaultServer) close()           { p.srv.Close() }
func (p *providerFaultServer) url() string      { return p.srv.URL }
func (p *providerFaultServer) hitCount() int    { return int(p.hits.Load()) }
func (p *providerFaultServer) maxInFlight() int { return int(p.maxInFly.Load()) }

// rearmRelayTick reads the relay injector's series of the shared re-arm
// vec (delta discipline — the vec accumulates across the package's tests).
func rearmRelayTick(outcome string) float64 {
	return promtestutil.ToFloat64(rearmLoopOutcomes.WithLabelValues(rearmLoopRelayInjector, outcome))
}

// relayRearmHarness wires one injector run against a fault server.
type relayRearmHarness struct {
	ctx      context.Context
	cancel   context.CancelFunc
	fault    *providerFaultServer
	writer   *opencode.ConfigWriter
	cfgPath  string
	authPath string
	kills    atomic.Int32
	// busyGate, when set, is the injector's Busy closure.
	busyGate func() bool
	// deferredGate, when set, is the injector's RestartDeferred closure.
	deferredGate func() bool
}

func newRelayRearmHarness(t *testing.T, mode int32) *relayRearmHarness {
	t.Helper()
	resetRelayState(t)
	dir := t.TempDir()
	h := &relayRearmHarness{
		fault:    newProviderFaultServer(mode),
		cfgPath:  filepath.Join(dir, "agent-config.json"),
		authPath: filepath.Join(dir, "auth.json"),
	}
	h.ctx, h.cancel = context.WithCancel(context.Background())
	t.Cleanup(func() {
		h.cancel()
		h.fault.close()
	})
	require.NoError(t, os.WriteFile(h.authPath,
		[]byte(`{"opencode":{"type":"api","key":"public"}}`), 0o600))
	h.writer = opencode.NewConfigWriter(h.cfgPath)
	return h
}

func (h *relayRearmHarness) start(t *testing.T) {
	t.Helper()
	startRelayInjector(h.ctx, relayInjectorConfig{
		RelayURL:          "https://relay.example.test/path",
		OpenCodeBaseURL:   h.fault.url(),
		OpenCodePassword:  "pw",
		AgentConfigPath:   h.cfgPath,
		AuthJSONPath:      h.authPath,
		AgentConfigWriter: h.writer,
		HealthCheck:       func() bool { return true },
		KillOpenCode:      func() { h.kills.Add(1) },
		FetchRetryDelay:   5 * time.Millisecond,
		FetchDeadline:     30 * time.Millisecond,
		RearmMinDelay:     20 * time.Millisecond,
		RearmMaxDelay:     80 * time.Millisecond,
		Busy:              h.busyGate,
		RestartDeferred:   h.deferredGate,
	})
}

// TestRelayRearm_BootFetchFailure_RearmsAndApplies is the #910 core
// scenario at agentd level: the catalog source is down through the entire
// boot window (terminal fetch failure — previously permanent for the
// pod's lifetime), then heals mid-pod-life. The re-arm loop must apply
// the relay config and trigger the (session-aware) restart with NO pod
// recreation and no second injector call.
func TestRelayRearm_BootFetchFailure_RearmsAndApplies(t *testing.T) {
	h := newRelayRearmHarness(t, providerModeFail500)
	bootFailed0 := promtestutil.ToFloat64(relayInjectorOutcomes.WithLabelValues(relayOutcomeFetchFailed))
	rearmFailed0 := rearmRelayTick(relayOutcomeFetchFailed)
	h.start(t)

	// Boot window exhausts → terminal → the re-arm loop's first cycle
	// also fails (source still down). Observable per cycle.
	require.Eventually(t, func() bool {
		return rearmRelayTick(relayOutcomeFetchFailed)-rearmFailed0 >= 1
	}, 5*time.Second, 5*time.Millisecond, "the re-arm loop must attempt after the boot window exhausts")
	assert.InDelta(t, 1.0, promtestutil.ToFloat64(relayInjectorOutcomes.WithLabelValues(relayOutcomeFetchFailed))-bootFailed0, 0,
		"exactly one boot-window fetch_failed tick")
	assert.Zero(t, h.kills.Load(), "no restart while the source is down")
	assert.Equal(t, int32(2), RelayFreeModelsState(), "degraded state stays surfaced while re-arm cycles fail")

	// Source heals mid-pod-life (router up at T+3m in the story's
	// fault-injection row; scaled here).
	h.fault.heal()

	require.Eventually(t, func() bool {
		return h.kills.Load() == 1 && h.writer.HasRelay()
	}, 5*time.Second, 5*time.Millisecond, "re-arm must apply the relay config and trigger the restart after healing")
	assert.Equal(t, int32(1), RelayFreeModelsState(), "successful re-arm clears the degrade state")

	// Config actually written with the relay provider block.
	data, err := os.ReadFile(h.cfgPath)
	require.NoError(t, err)
	var cfg map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(data, &cfg))
	assert.Contains(t, string(cfg["provider"]), "opencode-relay")

	// Exactly one in-flight attempt at any time, and the re-arm's
	// applied cycle is observable.
	assert.Equal(t, 1, h.fault.maxInFlight(), "re-arm attempts must be strictly sequential (one in-flight)")
	require.Eventually(t, func() bool { return rearmRelayTick(relayOutcomeSuccess) >= 1 },
		2*time.Second, 5*time.Millisecond, "the applied cycle must tick its outcome")
}

// TestRelayRearm_Incident20260816_Regression pins the 2026-08-16 incident
// class (workspace 946a442f): a free-models fetch that dies with
// `decode /provider: unexpected EOF` through the whole boot window
// previously left the relay-only default model PERMANENTLY unresolvable
// for the pod's lifetime. Now: bounded recovery — the re-arm loop keeps
// retrying on the capped schedule and applies as soon as the catalog
// answers, without manual intervention.
func TestRelayRearm_Incident20260816_Regression(t *testing.T) {
	h := newRelayRearmHarness(t, providerModeTruncated)
	h.start(t)

	// Boot terminal on the EOF class (a fetch ERROR, not an empty
	// catalog — the #906 F3 distinction).
	require.Eventually(t, func() bool {
		return RelayFreeModelsState() == 2
	}, 5*time.Second, 5*time.Millisecond, "the EOF class must land in the degraded state")
	assert.Zero(t, h.kills.Load())

	// Bounded recovery, not a permanent give-up: attempts continue.
	hitsAfterBoot := h.fault.hitCount()
	require.Eventually(t, func() bool { return h.fault.hitCount() > hitsAfterBoot },
		5*time.Second, 5*time.Millisecond, "re-arm must keep retrying after the incident-class boot failure")

	// The catalog heals (models.dev reachable again).
	h.fault.heal()
	require.Eventually(t, func() bool {
		return h.writer.HasRelay() && h.kills.Load() == 1
	}, 5*time.Second, 5*time.Millisecond,
		"the incident class must recover within the re-arm bounds — no pod recreation, no manual restart")
	assert.Equal(t, int32(1), RelayFreeModelsState())

	// No stacked restarts: exactly one kill, strictly sequential attempts.
	killCount := h.kills.Load()
	time.Sleep(150 * time.Millisecond)
	assert.Equal(t, killCount, h.kills.Load(), "exactly one restart — the loop disarms on success")
	assert.Equal(t, 1, h.fault.maxInFlight(), "never more than one in-flight fetch attempt")
}

// TestRelayRearm_BusyGate_NoAttemptWhileBusy: re-arm cycles must not run
// the attempt (not even the fetch) while any session is busy — the AC's
// "no mid-turn SSE drops" gate.
func TestRelayRearm_BusyGate_NoAttemptWhileBusy(t *testing.T) {
	h := newRelayRearmHarness(t, providerModeFail500)
	var gateCalls atomic.Int32
	h.busyGate = func() bool { return gateCalls.Add(1) <= 3 }
	h.start(t)

	// Boot terminal, then ≥2 busy-skipped cycles.
	require.Eventually(t, func() bool {
		return rearmRelayTick(rearmOutcomeBusy) >= 2
	}, 5*time.Second, 5*time.Millisecond, "busy cycles must tick the busy outcome")

	// While busy: zero re-arm fetches (boot-window hits only), zero kills.
	hitsAtGateOpen := h.fault.hitCount()
	require.Greater(t, hitsAtGateOpen, 0, "the boot window itself must have attempted")
	assert.Zero(t, h.kills.Load())

	// Gate opens (gate call 4+ returns false) and the source heals —
	// the next cycle applies.
	h.fault.heal()
	require.Eventually(t, func() bool {
		return h.kills.Load() == 1 && h.writer.HasRelay()
	}, 5*time.Second, 5*time.Millisecond, "re-arm must apply once the busy gate opens")
}

// TestRelayRearm_NoStackBehindDeferredKill is #910's named constraint:
// while a session-aware deferred restart is outstanding (the #1374
// progress-keyed defer), a re-arm attempt must not stack a second restart
// behind it. The gate reads the deferred-restart gauge maintained by
// makeSessionAwareRestartDecision itself.
func TestRelayRearm_NoStackBehindDeferredKill(t *testing.T) {
	tracker := newSessionStatusTracker()
	tracker.set("ses_gate", "busy")
	proc := newOrderRecordingProc()
	deferredCtx, deferredCancel := context.WithCancel(context.Background())
	defer deferredCancel()
	restarted := makeSessionAwareRestartDecision(deferredCtx, proc, tracker, restartDecisionConfig{
		PollInterval: 20 * time.Millisecond,
		StallBound:   10 * time.Minute, // never force in this window — defer only
	})
	require.False(t, restarted, "busy session → restart deferred")
	require.Eventually(t, func() bool { return anyRestartDeferred() },
		2*time.Second, 5*time.Millisecond, "the deferred goroutine must hold the gauge")

	h := newRelayRearmHarness(t, providerModeFail500)
	h.deferredGate = anyRestartDeferred
	h.start(t)

	// Boot terminal (source down), then re-arm cycles held at the
	// restart_deferred gate — no fetches past the boot window.
	require.Eventually(t, func() bool {
		return rearmRelayTick(rearmOutcomeRestartDeferred) >= 1
	}, 5*time.Second, 5*time.Millisecond, "cycles behind a deferred restart must tick restart_deferred")
	hitsWhileDeferred := h.fault.hitCount()
	require.Greater(t, hitsWhileDeferred, 0, "boot window attempted")
	assert.Zero(t, h.kills.Load(), "no restart may stack behind the deferred kill")
	time.Sleep(80 * time.Millisecond)
	assert.Equal(t, hitsWhileDeferred, h.fault.hitCount(),
		"no re-arm fetch while a deferred restart is outstanding")

	// The deferred restart fires (session idles) → gauge clears → the
	// next re-arm cycle proceeds on its own schedule.
	tracker.set("ses_gate", "idle")
	require.Eventually(t, func() bool { return !anyRestartDeferred() },
		2*time.Second, 5*time.Millisecond, "gauge must clear when the deferred restart fires")
	assert.Equal(t, 1, proc.restartCount(), "the deferred restart fired exactly once")

	h.fault.heal()
	require.Eventually(t, func() bool {
		return h.kills.Load() == 1 && h.writer.HasRelay()
	}, 5*time.Second, 5*time.Millisecond, "re-arm applies after the deferral clears")
}

// TestAnyRestartDeferred_GaugeLifecycle pins the gauge the re-arm gate
// reads: set for the life of a deferred-restart goroutine, cleared when
// the restart fires AND when the defer is canceled at shutdown.
func TestAnyRestartDeferred_GaugeLifecycle(t *testing.T) {
	require.False(t, anyRestartDeferred(), "quiescent baseline")

	tracker := newSessionStatusTracker()
	tracker.set("ses_g1", "busy")
	proc := newOrderRecordingProc()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	_ = makeSessionAwareRestartDecision(ctx, proc, tracker, restartDecisionConfig{
		PollInterval: 15 * time.Millisecond,
		StallBound:   10 * time.Minute,
	})
	require.Eventually(t, func() bool { return anyRestartDeferred() }, 2*time.Second, 5*time.Millisecond)

	tracker.set("ses_g1", "idle")
	require.Eventually(t, func() bool { return !anyRestartDeferred() }, 2*time.Second, 5*time.Millisecond,
		"gauge clears when the deferred restart fires")
	require.Equal(t, 1, proc.restartCount())

	// Canceled defer (shutdown): gauge clears without a restart.
	tracker2 := newSessionStatusTracker()
	tracker2.set("ses_g2", "busy")
	proc2 := newOrderRecordingProc()
	ctx2, cancel2 := context.WithCancel(context.Background())
	_ = makeSessionAwareRestartDecision(ctx2, proc2, tracker2, restartDecisionConfig{
		PollInterval: 15 * time.Millisecond,
		StallBound:   10 * time.Minute,
	})
	require.Eventually(t, func() bool { return anyRestartDeferred() }, 2*time.Second, 5*time.Millisecond)
	cancel2()
	require.Eventually(t, func() bool { return !anyRestartDeferred() }, 2*time.Second, 5*time.Millisecond,
		"gauge clears when the defer is canceled at shutdown")
	require.Zero(t, proc2.restartCount())
}

// TestRelayRearm_HasRelayShortCircuit_PreservedOnceApplied: once the
// relay is applied the loop disarms — no further fetches or restarts —
// and if relay state appears through any other path mid-re-arm, the next
// cycle short-circuits without attempting.
func TestRelayRearm_HasRelayShortCircuit_PreservedOnceApplied(t *testing.T) {
	t.Run("disarms after applied", func(t *testing.T) {
		h := newRelayRearmHarness(t, providerModeOK)
		h.start(t)
		require.Eventually(t, func() bool { return h.kills.Load() == 1 && h.writer.HasRelay() },
			5*time.Second, 5*time.Millisecond)
		hits := h.fault.hitCount()
		time.Sleep(150 * time.Millisecond)
		assert.Equal(t, hits, h.fault.hitCount(), "no fetches after the relay is applied")
		assert.Equal(t, int32(1), h.kills.Load(), "exactly one restart — HasRelay() short-circuit holds")
	})

	t.Run("already_applied mid-rearm skips the attempt", func(t *testing.T) {
		h := newRelayRearmHarness(t, providerModeFail500)
		already0 := rearmRelayTick(rearmOutcomeAlreadyApplied)
		h.start(t)
		// The boot window terminally fails; before the first re-arm
		// cycle lands, another path applies relay state.
		require.Eventually(t, func() bool { return RelayFreeModelsState() == 2 },
			5*time.Second, 5*time.Millisecond)
		_, err := h.writer.Apply(agent.AgentConfigInput{
			Relay: &agent.RelayState{URL: "https://relay.example.test/path", Models: []agent.RelayModel{{ID: "m"}}},
		})
		require.NoError(t, err)
		require.True(t, h.writer.HasRelay())

		require.Eventually(t, func() bool {
			return rearmRelayTick(rearmOutcomeAlreadyApplied)-already0 >= 1
		}, 5*time.Second, 5*time.Millisecond, "the next cycle must observe HasRelay and disarm")
		time.Sleep(100 * time.Millisecond)
		assert.Zero(t, h.kills.Load(), "an already-applied relay must not trigger another restart")
		assert.True(t, h.writer.HasRelay())
	})
}

// TestRelayRearm_BootSuccess_NeverEntersRearm: a healthy boot applies in
// the boot window — the re-arm loop is never entered (no rearm ticks).
func TestRelayRearm_BootSuccess_NeverEntersRearm(t *testing.T) {
	h := newRelayRearmHarness(t, providerModeOK)
	before := map[string]float64{
		relayOutcomeFetchFailed:    rearmRelayTick(relayOutcomeFetchFailed),
		relayOutcomeSuccess:        rearmRelayTick(relayOutcomeSuccess),
		rearmOutcomeAlreadyApplied: rearmRelayTick(rearmOutcomeAlreadyApplied),
		rearmOutcomeBusy:           rearmRelayTick(rearmOutcomeBusy),
	}
	h.start(t)
	require.Eventually(t, func() bool { return h.kills.Load() == 1 && h.writer.HasRelay() },
		5*time.Second, 5*time.Millisecond)
	time.Sleep(120 * time.Millisecond)
	for outcome, base := range before {
		assert.InDelta(t, base, rearmRelayTick(outcome), 0,
			"boot success must not enter the re-arm loop (%s)", outcome)
	}
}

// TestRelayReadyz_RelayInjected_TruthfulPerCycle: readyz evaluates the
// writer live — before a re-arm applies it reports false, after the
// re-arm applies it reports true, with no restart of agentd in between.
func TestRelayReadyz_RelayInjected_TruthfulPerCycle(t *testing.T) {
	withTestLogger(t)
	h := newRelayRearmHarness(t, providerModeFail500)
	deps := newReadyzDeps(t)
	deps.agentConfigWriter = h.writer
	deps.healthCache.snapshot.Store(&healthzCacheSnapshot{Initialized: true, Healthy: true, Version: "vtest"})
	ready := func() bool { return true }

	_, body := doReadyz(t, deps, ready)
	require.False(t, body.RelayInjected, "readyz must report RelayInjected=false while re-arm has not applied")

	h.start(t)
	// The source heals mid-pod-life — the re-arm applies; readyz must
	// flip to true with no agentd restart in between.
	h.fault.heal()
	require.Eventually(t, func() bool { return h.writer.HasRelay() }, 5*time.Second, 5*time.Millisecond)
	_, body = doReadyz(t, deps, ready)
	assert.True(t, body.RelayInjected, "readyz must report RelayInjected=true the moment the re-arm applies")
}

// TestRelayAttempt_OutcomeRetryabilityMatrix pins attempt's full
// decision table — every outcome's {retryable, applied} classification,
// driven end-to-end through the real attempt body (review finding: the
// mapping was only pinned for fetch_failed and success; a silent flip of
// any retryable row back to non-retryable would reintroduce the
// permanent-degrade class #910 exists to kill, and no test would catch
// it).
func TestRelayAttempt_OutcomeRetryabilityMatrix(t *testing.T) {
	withTestLogger(t)
	okSrv := newProviderFaultServer(providerModeOK)
	defer okSrv.close()
	failSrv := newProviderFaultServer(providerModeFail500)
	defer failSrv.close()
	emptySrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[]}`)) // clean fetch, empty catalog
	}))
	defer emptySrv.Close()

	base := func(t *testing.T) relayInjectorConfig {
		t.Helper()
		dir := t.TempDir()
		authPath := filepath.Join(dir, "auth.json")
		require.NoError(t, os.WriteFile(authPath,
			[]byte(`{"opencode":{"type":"api","key":"public"}}`), 0o600))
		return relayInjectorConfig{
			RelayURL:          "https://relay.example.test/path",
			OpenCodeBaseURL:   okSrv.url(),
			OpenCodePassword:  "pw",
			AgentConfigPath:   filepath.Join(dir, "agent-config.json"),
			AuthJSONPath:      authPath,
			AgentConfigWriter: opencode.NewConfigWriter(filepath.Join(dir, "agent-config.json")),
			HealthCheck:       func() bool { return true },
			KillOpenCode:      func() {},
			FetchRetryDelay:   5 * time.Millisecond,
			FetchDeadline:     30 * time.Millisecond,
		}
	}

	cases := []struct {
		name         string
		mutate       func(t *testing.T, cfg *relayInjectorConfig)
		wantOutcome  string
		wantRetry    bool
		wantApplied  bool
		wantFreeStat int32
	}{
		{
			name:         "unhealthy_timeout is retryable (K3: a >5m boot no longer loses relay forever)",
			mutate:       func(_ *testing.T, cfg *relayInjectorConfig) { cfg.HealthCheck = func() bool { return false } },
			wantOutcome:  relayOutcomeUnhealthyTimeout,
			wantRetry:    true,
			wantFreeStat: 2,
		},
		{
			name:        "fetch_failed is retryable (the incident class)",
			mutate:      func(_ *testing.T, cfg *relayInjectorConfig) { cfg.OpenCodeBaseURL = failSrv.url() },
			wantOutcome: relayOutcomeFetchFailed,
			wantRetry:   true,
		},
		{
			name:        "no_free_models is retryable (clean fetch, empty catalog)",
			mutate:      func(_ *testing.T, cfg *relayInjectorConfig) { cfg.OpenCodeBaseURL = emptySrv.URL },
			wantOutcome: relayOutcomeNoFreeModels,
			wantRetry:   true,
		},
		{
			name: "config_write_failed is retryable",
			mutate: func(t *testing.T, cfg *relayInjectorConfig) {
				// Writer path inside a nonexistent parent dir: Apply's
				// temp-file write fails (no root-skip needed, unlike the
				// chmod-based failure fixture).
				p := filepath.Join(t.TempDir(), "missing-parent", "agent-config.json")
				cfg.AgentConfigPath = p
				cfg.AgentConfigWriter = opencode.NewConfigWriter(p)
			},
			wantOutcome: relayOutcomeConfigWriteFailed,
			wantRetry:   true,
		},
		{
			name: "auth_write_failed is terminal (Apply already set relay state — a re-arm cycle would disarm via already_applied without retrying the auth write)",
			mutate: func(t *testing.T, cfg *relayInjectorConfig) {
				cfg.AuthJSONPath = filepath.Join(t.TempDir(), "missing-parent", "auth.json")
			},
			wantOutcome:  relayOutcomeAuthWriteFailed,
			wantFreeStat: 2,
		},
		{
			name: "writer_nil is terminal (defensive guard — a missing writer does not appear by retrying)",
			mutate: func(_ *testing.T, cfg *relayInjectorConfig) {
				cfg.AgentConfigWriter = nil
			},
			wantOutcome: relayOutcomeWriterNil,
		},
		{
			name: "skipped_personal_key is terminal (relay correctly bypassed)",
			mutate: func(t *testing.T, cfg *relayInjectorConfig) {
				require.NoError(t, os.WriteFile(cfg.AuthJSONPath,
					[]byte(`{"opencode":{"type":"api","key":"sk-personal"}}`), 0o600))
			},
			wantOutcome: relayOutcomeSkippedPersonalKey,
		},
		{
			name:         "success applies",
			wantOutcome:  relayOutcomeSuccess,
			wantApplied:  true,
			wantFreeStat: 1,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resetRelayState(t)
			cfg := base(t)
			if tc.mutate != nil {
				tc.mutate(t, &cfg)
			}
			res := cfg.attempt(context.Background(), log, 0)
			assert.Equal(t, tc.wantOutcome, res.outcome, "outcome")
			assert.Equal(t, tc.wantRetry, res.retryable, "retryable")
			assert.Equal(t, tc.wantApplied, res.applied, "applied")
			if tc.wantFreeStat != 0 {
				assert.Equal(t, tc.wantFreeStat, RelayFreeModelsState(), "relayFreeModelsState")
			}
			if tc.wantApplied {
				assert.True(t, cfg.AgentConfigWriter.HasRelay())
			}
		})
	}
}

// TestRelayRearm_PersonalKeyAddedMidLife_Disarms: the personal-key
// bypass decision is re-evaluated per cycle from auth.json — a key
// added mid-pod-life after a boot fetch failure disarms the loop on its
// next cycle (skipped_personal_key, non-retryable) without ever
// restarting.
func TestRelayRearm_PersonalKeyAddedMidLife_Disarms(t *testing.T) {
	h := newRelayRearmHarness(t, providerModeFail500)
	skipped0 := rearmRelayTick(relayOutcomeSkippedPersonalKey)
	h.start(t)

	// Boot terminally fails against the down source.
	require.Eventually(t, func() bool { return RelayFreeModelsState() == 2 },
		5*time.Second, 5*time.Millisecond)

	// The user binds a personal opencode key mid-pod-life.
	require.NoError(t, os.WriteFile(h.authPath,
		[]byte(`{"opencode":{"type":"api","key":"sk-_personal_midlife"}}`), 0o600))

	require.Eventually(t, func() bool {
		return rearmRelayTick(relayOutcomeSkippedPersonalKey)-skipped0 >= 1
	}, 5*time.Second, 5*time.Millisecond, "the next cycle must re-read auth.json and disarm")

	// Quiescence: the loop is gone, nothing was killed.
	time.Sleep(150 * time.Millisecond)
	assert.Zero(t, h.kills.Load(), "a personal-key disarm must never restart opencode")
	assert.False(t, h.writer.HasRelay())
}

// TestRelayRearm_PreKillDeferredCheck_SkipsRelayKill pins the SECOND
// stacking checkpoint (#910 review finding): a deferred restart that
// appears DURING an attempt (after the cycle-entry gate passed, before
// the restart trigger) must skip the relay kill — the outstanding
// deferred restart reads the just-written config — and the attempt
// still counts as applied (loop disarms; state 1).
func TestRelayRearm_PreKillDeferredCheck_SkipsRelayKill(t *testing.T) {
	h := newRelayRearmHarness(t, providerModeFail500)
	deferred0 := rearmRelayTick(rearmOutcomeRestartDeferred)
	// Gate: call #1 is the cycle-entry check (passes); call #2 is the
	// pre-kill check (deferred appeared mid-attempt) → skip the kill.
	var gateCalls atomic.Int32
	h.deferredGate = func() bool { return gateCalls.Add(1) >= 2 }
	h.start(t)

	// Boot terminally fails (never reaches the pre-kill check).
	require.Eventually(t, func() bool { return RelayFreeModelsState() == 2 },
		5*time.Second, 5*time.Millisecond)

	// Source heals; the next cycle passes its entry gate, fetches,
	// applies config + auth — and then observes the deferral before
	// the kill.
	h.fault.heal()
	require.Eventually(t, func() bool { return h.writer.HasRelay() },
		5*time.Second, 5*time.Millisecond, "the cycle must still apply the config")

	require.Eventually(t, func() bool { return rearmRelayTick(relayOutcomeSuccess) >= 1 },
		2*time.Second, 5*time.Millisecond, "the skip-kill cycle is an applied success")
	assert.Zero(t, h.kills.Load(), "the relay kill must be skipped behind the outstanding deferred restart")
	assert.Equal(t, int32(1), RelayFreeModelsState(), "the config IS applied — state flips")
	assert.InDelta(t, deferred0, rearmRelayTick(rearmOutcomeRestartDeferred), 0,
		"the in-attempt skip is not a restart_deferred cycle tick")
}

// TestRelayAttempt_CtxCanceledAfterFetch_NoApplyNoKill pins the
// shutdown-window guard: ctx dying while the fetch is in flight (or
// right as it completes) must land canceled with NO config write and NO
// restart trigger — the next agentd generation re-runs the injector.
func TestRelayAttempt_CtxCanceledAfterFetch_NoApplyNoKill(t *testing.T) {
	withTestLogger(t)
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "agent-config.json")
	authPath := filepath.Join(dir, "auth.json")
	require.NoError(t, os.WriteFile(authPath,
		[]byte(`{"opencode":{"type":"api","key":"public"}}`), 0o600))
	writer := opencode.NewConfigWriter(cfgPath)

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	var kills atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		cancel() // dies mid-flight: the fetch may error (canceled) or
		// succeed with the ctx already dead — both land canceled below.
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"connected":["opencode"],"all":[{"id":"opencode","models":{"free-model":{"id":"free-model","name":"Free Model","cost":{"input":0,"output":0},"limit":{"context":100000,"output":10000}}}}]}`))
	}))
	defer srv.Close()

	res := relayInjectorConfig{
		RelayURL:          "https://relay.example.test/path",
		OpenCodeBaseURL:   srv.URL,
		OpenCodePassword:  "pw",
		AgentConfigPath:   cfgPath,
		AuthJSONPath:      authPath,
		AgentConfigWriter: writer,
		HealthCheck:       func() bool { return true },
		KillOpenCode:      func() { kills.Add(1) },
		FetchRetryDelay:   5 * time.Millisecond,
		FetchDeadline:     30 * time.Millisecond,
	}.attempt(ctx, log, 0)

	assert.Equal(t, rearmOutcomeCanceled, res.outcome, "a dead ctx must never proceed past the fetch")
	assert.False(t, res.applied)
	assert.False(t, writer.HasRelay(), "no config write in the shutdown window")
	assert.Zero(t, kills.Load(), "no restart trigger in the shutdown window")
}
