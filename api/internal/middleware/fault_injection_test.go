// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package middleware

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	logmock "github.com/lenaxia/llmsafespaces/mocks/logger"
	"github.com/lenaxia/llmsafespaces/pkg/interfaces"
)

func TestParseFaultInjectionRules_ValidSingleRule(t *testing.T) {
	rules, err := ParseFaultInjectionRules("5:POST:/internal/v1/pod-bootstrap")
	require.NoError(t, err)
	require.Len(t, rules, 1)
	assert.Equal(t, FaultInjectionRule{Count: 5, Method: "POST", PathPrefix: "/internal/v1/pod-bootstrap"}, rules[0])
}

func TestParseFaultInjectionRules_MultipleRules(t *testing.T) {
	rules, err := ParseFaultInjectionRules("2:POST:/internal/v1/pod-bootstrap,10:GET:/api/v1/workspaces")
	require.NoError(t, err)
	require.Len(t, rules, 2)
	assert.Equal(t, FaultInjectionRule{Count: 2, Method: "POST", PathPrefix: "/internal/v1/pod-bootstrap"}, rules[0])
	assert.Equal(t, FaultInjectionRule{Count: 10, Method: "GET", PathPrefix: "/api/v1/workspaces"}, rules[1])
}

func TestParseFaultInjectionRules_WhitespaceAndEmptySegmentsTrimmed(t *testing.T) {
	rules, err := ParseFaultInjectionRules(" 5 : POST : /internal/v1/pod-bootstrap , ,3:GET:/api/v1/workspaces,")
	require.NoError(t, err)
	require.Len(t, rules, 2)
	assert.Equal(t, FaultInjectionRule{Count: 5, Method: "POST", PathPrefix: "/internal/v1/pod-bootstrap"}, rules[0])
	assert.Equal(t, FaultInjectionRule{Count: 3, Method: "GET", PathPrefix: "/api/v1/workspaces"}, rules[1])
}

func TestParseFaultInjectionRules_EmptySpecIsAbsent(t *testing.T) {
	for _, spec := range []string{"", "   ", ",", " , , "} {
		rules, err := ParseFaultInjectionRules(spec)
		require.NoError(t, err, "spec %q must parse as feature-absent, not error", spec)
		assert.Empty(t, rules, "spec %q must yield zero rules", spec)
	}
}

func TestParseFaultInjectionRules_MalformedRulesError(t *testing.T) {
	cases := []struct {
		name string
		spec string
	}{
		{"non-numeric count", "five:POST:/x"},
		{"zero count", "0:POST:/x"},
		{"negative count", "-1:POST:/x"},
		{"float count", "1.5:POST:/x"},
		{"empty method", "5::/x"},
		{"lowercase method", "5:get:/x"},
		{"unknown method", "5:FETCH:/x"},
		{"missing prefix", "5:POST:"},
		{"prefix without leading slash", "5:POST:internal/v1"},
		{"missing method and prefix", "5"},
		{"missing prefix segment", "5:POST"},
		{"empty rule text", "5: : "},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rules, err := ParseFaultInjectionRules(tc.spec)
			require.Error(t, err, "spec %q must be rejected", tc.spec)
			assert.Nil(t, rules)
			assert.Contains(t, err.Error(), FaultInjectionEnvVar,
				"error must name the env var so operators can find the knob")
			assert.Contains(t, err.Error(), fmt.Sprintf("%q", strings.TrimSpace(tc.spec)),
				"error must name the bad rule verbatim")
		})
	}
}

func TestParseFaultInjectionRules_BadRuleInListFailsWholeParse(t *testing.T) {
	_, err := ParseFaultInjectionRules("1:POST:/x,0:GET:/y")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "0:GET:/y")
}

func newFaultTestEngine(t *testing.T, log interfaces.LoggerInterface, rules ...FaultInjectionRule) *gin.Engine {
	t.Helper()
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(FaultInjectionMiddleware(log, rules))
	r.Any("/internal/v1/pod-bootstrap", func(c *gin.Context) {
		c.String(http.StatusOK, "bootstrapped")
	})
	r.Any("/api/v1/workspaces", func(c *gin.Context) {
		c.String(http.StatusOK, "workspaces")
	})
	return r
}

func newFaultMockLogger() *logmock.MockLogger {
	l := logmock.NewMockLogger()
	l.On("Warn", mock.Anything, mock.Anything).Maybe()
	return l
}

func doFaultRequest(r *gin.Engine, method, path string) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	req := httptest.NewRequest(method, path, nil)
	r.ServeHTTP(w, req)
	return w
}

func TestFaultInjectionMiddleware_UnsetFeaturePassthroughByteIdentical(t *testing.T) {
	log := newFaultMockLogger()
	plain := gin.New()
	plain.POST("/internal/v1/pod-bootstrap", func(c *gin.Context) {
		c.String(http.StatusOK, "bootstrapped")
	})

	r := newFaultTestEngine(t, log)

	want := doFaultRequest(plain, http.MethodPost, "/internal/v1/pod-bootstrap")
	got := doFaultRequest(r, http.MethodPost, "/internal/v1/pod-bootstrap")

	assert.Equal(t, want.Code, got.Code)
	assert.Equal(t, want.Body.String(), got.Body.String())
	assert.Equal(t, want.Result().Header, got.Result().Header)
	assert.Empty(t, log.Calls, "no log line may be emitted when no rule is configured")
}

func TestFaultInjectionMiddleware_MatchFailsWithMarkerBody(t *testing.T) {
	log := newFaultMockLogger()
	r := newFaultTestEngine(t, log, FaultInjectionRule{Count: 2, Method: "POST", PathPrefix: "/internal/v1/pod-bootstrap"})

	w := doFaultRequest(r, http.MethodPost, "/internal/v1/pod-bootstrap")
	require.Equal(t, http.StatusInternalServerError, w.Code)

	var body map[string]any
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
	errStr, isString := body["error"].(string)
	require.True(t, isString, "error value must be a string per the #862 error contract, got %T", body["error"])
	assert.Equal(t, "fault injection: POST /internal/v1/pod-bootstrap", errStr)
}

func TestFaultInjectionMiddleware_ExhaustsThenPassesThrough(t *testing.T) {
	log := newFaultMockLogger()
	r := newFaultTestEngine(t, log, FaultInjectionRule{Count: 2, Method: "POST", PathPrefix: "/internal/v1/pod-bootstrap"})

	for i := 0; i < 2; i++ {
		w := doFaultRequest(r, http.MethodPost, "/internal/v1/pod-bootstrap")
		require.Equal(t, http.StatusInternalServerError, w.Code, "request %d must fail", i+1)
	}

	w := doFaultRequest(r, http.MethodPost, "/internal/v1/pod-bootstrap")
	require.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, "bootstrapped", w.Body.String(), "passthrough must reach the handler byte-identically")

	w = doFaultRequest(r, http.MethodPost, "/internal/v1/pod-bootstrap")
	assert.Equal(t, http.StatusOK, w.Code, "exhaustion is permanent for the process lifetime")
}

func TestFaultInjectionMiddleware_NonMatchingMethodPassesThrough(t *testing.T) {
	log := newFaultMockLogger()
	r := newFaultTestEngine(t, log, FaultInjectionRule{Count: 3, Method: "POST", PathPrefix: "/internal/v1/pod-bootstrap"})

	for i := 0; i < 5; i++ {
		w := doFaultRequest(r, http.MethodGet, "/internal/v1/pod-bootstrap")
		assert.Equal(t, http.StatusOK, w.Code, "GET must never be faulted by a POST-only rule")
	}
}

func TestFaultInjectionMiddleware_NonMatchingPrefixPassesThrough(t *testing.T) {
	log := newFaultMockLogger()
	r := newFaultTestEngine(t, log, FaultInjectionRule{Count: 3, Method: "POST", PathPrefix: "/internal/v1/pod-bootstrap"})

	for i := 0; i < 5; i++ {
		w := doFaultRequest(r, http.MethodPost, "/api/v1/workspaces")
		assert.Equal(t, http.StatusOK, w.Code)
	}
}

func TestFaultInjectionMiddleware_PrefixMatchesSubpaths(t *testing.T) {
	log := newFaultMockLogger()
	r := newFaultTestEngine(t, log, FaultInjectionRule{Count: 1, Method: "POST", PathPrefix: "/internal/v1"})
	r.POST("/internal/v1/other", func(c *gin.Context) {
		c.String(http.StatusOK, "other")
	})

	w := doFaultRequest(r, http.MethodPost, "/internal/v1/other")
	require.Equal(t, http.StatusInternalServerError, w.Code)
	assert.Contains(t, w.Body.String(), "fault injection: POST /internal/v1")

	w = doFaultRequest(r, http.MethodPost, "/internal/v1/other")
	assert.Equal(t, http.StatusOK, w.Code)
}

func TestFaultInjectionMiddleware_RulesAreIndependent(t *testing.T) {
	log := newFaultMockLogger()
	r := newFaultTestEngine(t, log,
		FaultInjectionRule{Count: 1, Method: "POST", PathPrefix: "/internal/v1/pod-bootstrap"},
		FaultInjectionRule{Count: 2, Method: "GET", PathPrefix: "/api/v1/workspaces"},
	)

	w := doFaultRequest(r, http.MethodPost, "/internal/v1/pod-bootstrap")
	require.Equal(t, http.StatusInternalServerError, w.Code)

	for i := 0; i < 2; i++ {
		w := doFaultRequest(r, http.MethodGet, "/api/v1/workspaces")
		require.Equal(t, http.StatusInternalServerError, w.Code, "rule B budget untouched by rule A, request %d", i+1)
	}

	assert.Equal(t, http.StatusOK, doFaultRequest(r, http.MethodGet, "/api/v1/workspaces").Code)
	assert.Equal(t, http.StatusOK, doFaultRequest(r, http.MethodPost, "/internal/v1/pod-bootstrap").Code)
}

func TestFaultInjectionMiddleware_EachFailureLogsOneWarnLine(t *testing.T) {
	log := newFaultMockLogger()
	r := newFaultTestEngine(t, log, FaultInjectionRule{Count: 3, Method: "POST", PathPrefix: "/internal/v1/pod-bootstrap"})

	for i := 0; i < 3; i++ {
		doFaultRequest(r, http.MethodPost, "/internal/v1/pod-bootstrap")
	}
	doFaultRequest(r, http.MethodPost, "/internal/v1/pod-bootstrap")

	var warnCalls int
	for _, call := range log.Calls {
		if call.Method == "Warn" {
			warnCalls++
		}
	}
	assert.Equal(t, 3, warnCalls, "exactly one warn line per injected failure; passthrough logs nothing")

	for _, call := range log.Calls {
		if call.Method != "Warn" {
			continue
		}
		msg, _ := call.Arguments.Get(0).(string)
		assert.Contains(t, msg, "fault injection")
		kv, _ := call.Arguments.Get(1).([]interface{})
		flat := fmt.Sprint(kv)
		assert.Contains(t, flat, "POST")
		assert.Contains(t, flat, "/internal/v1/pod-bootstrap")
	}
}

func TestFaultInjectionMiddleware_ConcurrentRequestsFireExactlyCountFailures(t *testing.T) {
	const total = 64
	const count = 7

	log := newLockedWarnLogger()
	r := newFaultTestEngine(t, log, FaultInjectionRule{Count: count, Method: "POST", PathPrefix: "/internal/v1/pod-bootstrap"})

	var wg sync.WaitGroup
	results := make([]int, total)
	for i := 0; i < total; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			w := doFaultRequest(r, http.MethodPost, "/internal/v1/pod-bootstrap")
			results[i] = w.Code
		}(i)
	}
	wg.Wait()

	failed, ok := 0, 0
	for _, code := range results {
		switch code {
		case http.StatusInternalServerError:
			failed++
		case http.StatusOK:
			ok++
		default:
			t.Fatalf("unexpected status %d", code)
		}
	}
	assert.Equal(t, count, failed, "exactly COUNT concurrent requests must fail")
	assert.Equal(t, total-count, ok)
	assert.Equal(t, count, log.warnCount(), "exactly one warn line per injected failure")
}

type lockedWarnLogger struct {
	mu    sync.Mutex
	warns int
}

func newLockedWarnLogger() *lockedWarnLogger {
	return &lockedWarnLogger{}
}

func (l *lockedWarnLogger) warnCount() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.warns
}

func (l *lockedWarnLogger) Debug(_ string, _ ...interface{}) {}
func (l *lockedWarnLogger) Info(_ string, _ ...interface{})  {}
func (l *lockedWarnLogger) Warn(_ string, _ ...interface{}) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.warns++
}
func (l *lockedWarnLogger) Error(_ string, _ error, _ ...interface{}) {}
func (l *lockedWarnLogger) Fatal(_ string, _ error, _ ...interface{}) {}
func (l *lockedWarnLogger) With(_ ...interface{}) interfaces.LoggerInterface {
	return l
}
func (l *lockedWarnLogger) Sync() error { return nil }
