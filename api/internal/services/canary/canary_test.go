// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package canary

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	promtestutil "github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	abitest "github.com/lenaxia/llmsafespaces/pkg/abi/abitest"
	abiv1 "github.com/lenaxia/llmsafespaces/pkg/abi/v1"
	abiconnect "github.com/lenaxia/llmsafespaces/pkg/abi/v1/abiconnect"
	"github.com/lenaxia/llmsafespaces/pkg/obs"
)

// --- fakes -----------------------------------------------------------------

type fakeClient struct {
	getSnapshot func(ctx context.Context, sessionID string) (*abiv1.SessionSnapshot, error)
	act         func(ctx context.Context, req *abiv1.ActionRequest) (*abiv1.ActionResult, error)

	snapIDs []string
	actReqs []*abiv1.ActionRequest
}

func (f *fakeClient) GetSnapshot(ctx context.Context, sessionID string) (*abiv1.SessionSnapshot, error) {
	f.snapIDs = append(f.snapIDs, sessionID)
	if f.getSnapshot != nil {
		return f.getSnapshot(ctx, sessionID)
	}
	return &abiv1.SessionSnapshot{SessionId: sessionID}, nil
}

func (f *fakeClient) Act(ctx context.Context, req *abiv1.ActionRequest) (*abiv1.ActionResult, error) {
	f.actReqs = append(f.actReqs, req)
	if f.act != nil {
		return f.act(ctx, req)
	}
	return &abiv1.ActionResult{
		Result: &abiv1.ActionResult_AnswerQuestion{AnswerQuestion: &abiv1.AnswerInputResult{
			InputId: req.GetAnswerQuestion().GetInputId(),
		}},
	}, nil
}

type fakeLogger struct {
	warns int
}

func (l *fakeLogger) Warn(msg string, keysAndValues ...interface{}) { l.warns++ }
func (l *fakeLogger) Info(msg string, keysAndValues ...interface{}) {}

func typedErr(code connect.Code, msg string) error {
	return connect.NewError(code, errors.New(msg))
}

// newTestService wires every seam to a capturing fake; individual tests
// override the behaviors they care about.
type harness struct {
	pick      func(ctx context.Context, class string) (string, error)
	resolve   func(ctx context.Context, workspaceID string) (string, string, error)
	client    *fakeClient
	log       *fakeLogger
	service   *Service
	pickMu    sync.Mutex
	pickCalls []string
}

func (h *harness) pickCount() int {
	h.pickMu.Lock()
	defer h.pickMu.Unlock()
	return len(h.pickCalls)
}

func newTestService(t *testing.T, mutate func(*harness)) *harness {
	t.Helper()
	h := &harness{
		client: &fakeClient{},
		log:    &fakeLogger{},
	}
	h.pick = func(ctx context.Context, class string) (string, error) {
		h.pickMu.Lock()
		h.pickCalls = append(h.pickCalls, class)
		h.pickMu.Unlock()
		return "ws-canary", nil
	}
	h.resolve = func(ctx context.Context, workspaceID string) (string, string, error) {
		return "http://127.0.0.1:4097", "pw", nil
	}
	if mutate != nil {
		mutate(h)
	}
	svc, err := New(Config{
		Interval:  DefaultInterval,
		Timeout:   DefaultTimeout,
		Classes:   []string{"python:3.11"},
		Pick:      h.pick,
		Resolve:   h.resolve,
		NewClient: func(baseURL, password string) Client { return h.client },
		Logger:    h.log,
	})
	require.NoError(t, err)
	h.service = svc
	return h
}

// --- construction ----------------------------------------------------------

func TestNew_RequiresSeamsWhenClassesConfigured(t *testing.T) {
	base := func() Config {
		return Config{
			Classes:   []string{"python:3.11"},
			Pick:      func(context.Context, string) (string, error) { return "ws", nil },
			Resolve:   func(context.Context, string) (string, string, error) { return "http://x", "pw", nil },
			NewClient: func(string, string) Client { return &fakeClient{} },
		}
	}
	for _, tc := range []struct {
		name   string
		want   string
		mutate func(*Config)
	}{
		{"nil pick", "Pick", func(c *Config) { c.Pick = nil }},
		{"nil resolve", "Resolve", func(c *Config) { c.Resolve = nil }},
		{"nil newclient", "NewClient", func(c *Config) { c.NewClient = nil }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := base()
			tc.mutate(&cfg)
			_, err := New(cfg)
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.want)
		})
	}
}

func TestNew_AllowsEmptyClassesWithoutSeams(t *testing.T) {
	_, err := New(Config{})
	require.NoError(t, err, "a disabled-shaped config (no classes) must construct — the knob is off")
}

func TestNew_AppliesDefaults(t *testing.T) {
	svc, err := New(Config{
		Classes:   []string{"base"},
		Pick:      func(context.Context, string) (string, error) { return "ws", nil },
		Resolve:   func(context.Context, string) (string, string, error) { return "http://x", "pw", nil },
		NewClient: func(string, string) Client { return &fakeClient{} },
	})
	require.NoError(t, err)
	assert.Equal(t, DefaultInterval, svc.cfg.Interval)
	assert.Equal(t, DefaultTimeout, svc.cfg.Timeout)
}

// --- synthetic identifiers ---------------------------------------------------

func TestSyntheticIDs_ArePrefixedAndClassDerived(t *testing.T) {
	assert.Equal(t, "canary-probe-session-python:3.11", SyntheticSessionID("python:3.11"))
	assert.Equal(t, "canary-probe-input-python:3.11", SyntheticInputID("python:3.11"))
}

// --- probe legs --------------------------------------------------------------

func TestProbeClass_SnapshotLivenessSynthetic(t *testing.T) {
	h := newTestService(t, func(h *harness) {
		h.client.getSnapshot = func(ctx context.Context, sessionID string) (*abiv1.SessionSnapshot, error) {
			return nil, typedErr(connect.CodeNotFound, "unknown session")
		}
	})
	h.service.probeClass(context.Background(), "snapshot-liveness")

	require.Len(t, h.client.snapIDs, 1)
	assert.Equal(t, SyntheticSessionID("snapshot-liveness"), h.client.snapIDs[0],
		"the snapshot leg probes a synthetic session — a typed not_found IS the liveness answer")
	assert.Equal(t, float64(1), promtestutil.ToFloat64(probeOutcomes.WithLabelValues("snapshot-liveness", LegSnapshot, OutcomeNotFound)))
}

// TestProbeClass_SnapshotWrongSessionIsError: a 2xx carrying a session
// other than the requested synthetic id is wire drift, not liveness —
// the regression guard for classify-anything-2xx (Rule 11 finding).
func TestProbeClass_SnapshotWrongSessionIsError(t *testing.T) {
	h := newTestService(t, func(h *harness) {
		h.client.getSnapshot = func(ctx context.Context, sessionID string) (*abiv1.SessionSnapshot, error) {
			return &abiv1.SessionSnapshot{SessionId: "someone-elses-session"}, nil
		}
	})
	h.service.probeClass(context.Background(), "shape-drift")
	assert.Equal(t, float64(1), promtestutil.ToFloat64(probeOutcomes.WithLabelValues("shape-drift", LegSnapshot, OutcomeError)),
		"a snapshot answering the wrong session must not read as healthy")
}

func TestProbeClass_SnapshotOK(t *testing.T) {
	h := newTestService(t, nil)
	h.service.probeClass(context.Background(), "snapshot-ok")
	assert.Equal(t, float64(1), promtestutil.ToFloat64(probeOutcomes.WithLabelValues("snapshot-ok", LegSnapshot, OutcomeOK)))
}

func TestProbeClass_ResolveResolved(t *testing.T) {
	h := newTestService(t, nil)
	h.service.probeClass(context.Background(), "resolve-resolved")

	require.Len(t, h.client.actReqs, 1)
	req := h.client.actReqs[0]
	// The request-shape contract: agentd's validateAction rejects an
	// answer_question without input_id AND without option_ids/custom_text;
	// the session id is the Act key. Pin all three so a probe-side drift
	// fails HERE, not as invalid_argument in production.
	assert.Equal(t, SyntheticSessionID("resolve-resolved"), req.GetSessionId())
	require.NotNil(t, req.GetAnswerQuestion())
	assert.Equal(t, SyntheticInputID("resolve-resolved"), req.GetAnswerQuestion().GetInputId())
	assert.NotEmpty(t, req.GetAnswerQuestion().GetCustomText())

	assert.Equal(t, float64(1), promtestutil.ToFloat64(probeOutcomes.WithLabelValues("resolve-resolved", LegResolve, OutcomeResolved)),
		"S6 satisfied: absence resolved cleanly (the post-1a world)")
}

func TestProbeClass_ResolveS6Violation(t *testing.T) {
	h := newTestService(t, func(h *harness) {
		h.client.act = func(ctx context.Context, req *abiv1.ActionRequest) (*abiv1.ActionResult, error) {
			return nil, typedErr(connect.CodeNotFound, "unknown input")
		}
	})
	h.service.probeClass(context.Background(), "s6-violation")

	assert.Equal(t, float64(1), promtestutil.ToFloat64(probeOutcomes.WithLabelValues("s6-violation", LegResolve, OutcomeNotFoundError)),
		"S6 violated: absence surfaced as an error instead of resolving (the pre-1a baseline this canary exists to expose)")
}

func TestProbeClass_ClassificationTable(t *testing.T) {
	for _, tc := range []struct {
		name   string
		actErr error
		want   string
	}{
		{"invalid argument", typedErr(connect.CodeInvalidArgument, "bad"), OutcomeInvalidArgument},
		{"unauthenticated", typedErr(connect.CodeUnauthenticated, "bad pw"), OutcomeUnauthenticated},
		{"not supported", typedErr(connect.CodeUnimplemented, "gated"), OutcomeNotSupported},
		{"unavailable", typedErr(connect.CodeUnavailable, "pod down"), OutcomeUnavailable},
		{"deadline via connect code", typedErr(connect.CodeDeadlineExceeded, "slow"), OutcomeTimeout},
		{"plain error", errors.New("boom"), OutcomeError},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newTestService(t, func(h *harness) {
				h.client.act = func(ctx context.Context, req *abiv1.ActionRequest) (*abiv1.ActionResult, error) {
					return nil, tc.actErr
				}
			})
			class := "classify:" + tc.name
			h.service.probeClass(context.Background(), class)
			assert.Equal(t, float64(1), promtestutil.ToFloat64(probeOutcomes.WithLabelValues(class, LegResolve, tc.want)))
		})
	}
}

func TestProbeClass_TimeoutClassification(t *testing.T) {
	h := newTestService(t, func(h *harness) {
		h.client.getSnapshot = func(ctx context.Context, sessionID string) (*abiv1.SessionSnapshot, error) {
			<-ctx.Done()
			return nil, ctx.Err()
		}
		h.client.act = func(ctx context.Context, req *abiv1.ActionRequest) (*abiv1.ActionResult, error) {
			<-ctx.Done()
			return nil, ctx.Err()
		}
	})
	h.service.cfg.Timeout = 10 * time.Millisecond
	h.service.probeClass(context.Background(), "wedge")

	assert.Equal(t, float64(1), promtestutil.ToFloat64(probeOutcomes.WithLabelValues("wedge", LegSnapshot, OutcomeTimeout)),
		"a wedged snapshot path is the canary's primary quarry")
	assert.Equal(t, float64(1), promtestutil.ToFloat64(probeOutcomes.WithLabelValues("wedge", LegResolve, OutcomeTimeout)))
}

func TestProbeClass_WrongResultShapeIsError(t *testing.T) {
	h := newTestService(t, func(h *harness) {
		h.client.act = func(ctx context.Context, req *abiv1.ActionRequest) (*abiv1.ActionResult, error) {
			return &abiv1.ActionResult{Result: &abiv1.ActionResult_Interrupt{Interrupt: &abiv1.InterruptResult{}}}, nil
		}
	})
	h.service.probeClass(context.Background(), "shape-drift")
	assert.Equal(t, float64(1), promtestutil.ToFloat64(probeOutcomes.WithLabelValues("shape-drift", LegResolve, OutcomeError)),
		"a 2xx with the wrong result oneof is wire drift, not success")
}

// --- targeting --------------------------------------------------------------

func TestProbeClass_NoTarget(t *testing.T) {
	h := newTestService(t, func(h *harness) {
		h.pick = func(ctx context.Context, class string) (string, error) { return "", nil }
	})
	h.service.probeClass(context.Background(), "no-target")

	assert.Equal(t, float64(1), promtestutil.ToFloat64(probeOutcomes.WithLabelValues("no-target", LegSnapshot, OutcomeNoTarget)))
	assert.Equal(t, float64(1), promtestutil.ToFloat64(probeOutcomes.WithLabelValues("no-target", LegResolve, OutcomeNoTarget)))
	assert.Empty(t, h.client.snapIDs, "no target → no RPCs")
	assert.Empty(t, h.client.actReqs)
	assert.Equal(t, 0, h.log.warns, "an empty class is a fleet shape, not a warning-worthy event")
}

func TestProbeClass_PickError(t *testing.T) {
	h := newTestService(t, func(h *harness) {
		h.pick = func(ctx context.Context, class string) (string, error) { return "", errors.New("list failed") }
	})
	h.service.probeClass(context.Background(), "pick-error")

	assert.Equal(t, float64(1), promtestutil.ToFloat64(probeOutcomes.WithLabelValues("pick-error", LegSnapshot, OutcomePickError)))
	assert.Equal(t, float64(1), promtestutil.ToFloat64(probeOutcomes.WithLabelValues("pick-error", LegResolve, OutcomePickError)))
	assert.Equal(t, 1, h.log.warns)
}

func TestProbeClass_UnresolvedTarget(t *testing.T) {
	h := newTestService(t, func(h *harness) {
		h.resolve = func(ctx context.Context, workspaceID string) (string, string, error) {
			return "", "", errors.New("no pod IP")
		}
	})
	h.service.probeClass(context.Background(), "unresolved-target")

	assert.Equal(t, float64(1), promtestutil.ToFloat64(probeOutcomes.WithLabelValues("unresolved-target", LegSnapshot, OutcomeUnresolvedTarget)))
	assert.Equal(t, float64(1), promtestutil.ToFloat64(probeOutcomes.WithLabelValues("unresolved-target", LegResolve, OutcomeUnresolvedTarget)))
	assert.Empty(t, h.client.snapIDs)
	assert.Equal(t, 1, h.log.warns)
}

// --- the loop ------------------------------------------------------------------

func TestRunOnce_DoesNotStampLoopLiveness(t *testing.T) {
	h := newTestService(t, nil)
	before := promtestutil.ToFloat64(obs.LoopLastRun().WithLabelValues(obs.LoopCanaryProbe))
	h.service.runOnce(context.Background())
	assert.Equal(t, before, promtestutil.ToFloat64(obs.LoopLastRun().WithLabelValues(obs.LoopCanaryProbe)),
		"the stamp is loop-owned: a direct pass is NOT loop liveness (#1322 discipline)")
}

func TestRun_ProbesEveryClassAndStampsLoopGauge(t *testing.T) {
	h := newTestService(t, func(h *harness) {})
	h.service.cfg.Interval = 5 * time.Millisecond
	h.service.cfg.Classes = []string{"python:3.11", "node:22"}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go h.service.Run(ctx)

	require.Eventually(t, func() bool {
		return h.pickCount() >= 2 &&
			promtestutil.ToFloat64(probeOutcomes.WithLabelValues("python:3.11", LegResolve, OutcomeResolved)) >= 1 &&
			promtestutil.ToFloat64(probeOutcomes.WithLabelValues("node:22", LegResolve, OutcomeResolved)) >= 1
	}, 3*time.Second, 5*time.Millisecond, "every configured class is probed each pass")

	require.Eventually(t, func() bool {
		return promtestutil.ToFloat64(obs.LoopLastRun().WithLabelValues(obs.LoopCanaryProbe)) > 0
	}, 3*time.Second, 5*time.Millisecond, "the loop stamps the shared family at end-of-pass")
}

func TestRun_EmptyClassesStillStamps(t *testing.T) {
	seriesBefore := promtestutil.CollectAndCount(probeOutcomes)
	svc, err := New(Config{Interval: 5 * time.Millisecond})
	require.NoError(t, err)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go svc.Run(ctx)

	require.Eventually(t, func() bool {
		return promtestutil.ToFloat64(obs.LoopLastRun().WithLabelValues(obs.LoopCanaryProbe)) > 0
	}, 3*time.Second, 5*time.Millisecond, "a living runner with nothing configured still stamps — dead runners must be distinguishable from idle ones")

	assert.Equal(t, seriesBefore, promtestutil.CollectAndCount(probeOutcomes), "no classes → no new probe series")
}

func TestRun_StopsOnContextCancel(t *testing.T) {
	h := newTestService(t, nil)
	h.service.cfg.Interval = 5 * time.Millisecond

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { h.service.Run(ctx); close(done) }()
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after ctx cancel")
	}
}

// --- scrape surface -------------------------------------------------------------

func TestMetrics_ScrapeCompleteness(t *testing.T) {
	ts := httptest.NewServer(promhttp.Handler())
	t.Cleanup(ts.Close)

	h := newTestService(t, nil)
	h.service.probeClass(context.Background(), "scrape:class")

	res, err := http.Get(ts.URL)
	require.NoError(t, err)
	defer func() { _ = res.Body.Close() }()
	body, err := io.ReadAll(res.Body)
	require.NoError(t, err)

	for _, name := range []string{
		"llmsafespaces_canary_probe_outcomes_total",
		"llmsafespaces_canary_probe_duration_seconds",
	} {
		assert.Contains(t, string(body), name, "metric scrapes (a wiring typo cannot silently drop the family)")
	}
}

// --- the production transport (wire-level, the #1308 pattern) -------------------

// TestNewAbiClient_WireRoundTrip runs the production client against a REAL
// generated handler over abitest behind a Basic-auth gate: pins the §D1
// credential discipline and the connect protocol end-to-end against the
// post-1a reference contract — a SEEDED pending input resolves
// (registry hit), an UNSEEDED input answers typed not_found (the S6
// trigger wire shape; the resolve-by-absence conversion lives one level
// up in agentd's authority, modeled by the classification fakes).
func TestNewAbiClient_WireRoundTrip(t *testing.T) {
	const password = "canary-wire-pw"
	var sawAuth bool
	srv := abitest.New()
	mux := http.NewServeMux()
	mux.Handle(abiconnect.NewHarnessABIServiceHandler(srv))
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user, pass, ok := r.BasicAuth()
		if !ok || user != "opencode" || pass != password {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		sawAuth = true
		mux.ServeHTTP(w, r)
	}))
	t.Cleanup(ts.Close)

	cl := NewAbiClient(ts.URL, password)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	snap, err := cl.GetSnapshot(ctx, SyntheticSessionID("wiretest"))
	require.NoError(t, err)
	assert.Equal(t, SyntheticSessionID("wiretest"), snap.GetSessionId())

	// Seeded registry hit → resolved.
	seeded := SyntheticInputID("wiretest")
	srv.SeedPendingInput(SyntheticSessionID("wiretest"), &abiv1.InputRequest{Id: seeded})
	res, err := cl.Act(ctx, &abiv1.ActionRequest{
		SessionId: SyntheticSessionID("wiretest"),
		Action: &abiv1.ActionRequest_AnswerQuestion{AnswerQuestion: &abiv1.AnswerInputAction{
			InputId:    seeded,
			CustomText: strPtr(canaryAnswerText),
		}},
	})
	require.NoError(t, err)
	assert.Equal(t, seeded, res.GetAnswerQuestion().GetInputId())

	// Unseeded input → the S6 trigger wire shape: typed not_found.
	_, err = cl.Act(ctx, &abiv1.ActionRequest{
		SessionId: SyntheticSessionID("wiretest"),
		Action: &abiv1.ActionRequest_AnswerQuestion{AnswerQuestion: &abiv1.AnswerInputAction{
			InputId:    SyntheticInputID("unseeded"),
			CustomText: strPtr(canaryAnswerText),
		}},
	})
	require.Error(t, err)
	assert.Equal(t, connect.CodeNotFound, connect.CodeOf(err))

	assert.True(t, sawAuth, "every wire request carries the §D1 Basic credential")

	// The unauthenticated path must surface a typed error, not a blob.
	bad := NewAbiClient(ts.URL, "wrong")
	_, err = bad.GetSnapshot(ctx, "s")
	require.Error(t, err)
	assert.Equal(t, connect.CodeUnauthenticated, connect.CodeOf(err))
	assert.False(t, strings.Contains(err.Error(), "password"), "credentials never appear in errors")
}
