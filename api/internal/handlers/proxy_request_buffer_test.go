// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package handlers

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8sfake "k8s.io/client-go/kubernetes/fake"

	k8smocks "github.com/lenaxia/llmsafespaces/mocks/kubernetes"
	v1 "github.com/lenaxia/llmsafespaces/pkg/apis/llmsafespaces/v1"
	"github.com/lenaxia/llmsafespaces/pkg/types"
)

func metricValue(t *testing.T, name, workspaceID string) float64 {
	t.Helper()
	mfs, err := prometheus.DefaultGatherer.Gather()
	require.NoError(t, err)
	for _, mf := range mfs {
		if mf.GetName() != name {
			continue
		}
		for _, m := range mf.GetMetric() {
			if len(m.GetLabel()) == 0 {
				continue
			}
			if m.GetLabel()[0].GetValue() == workspaceID {
				switch {
				case m.GetGauge() != nil:
					return m.GetGauge().GetValue()
				case m.GetCounter() != nil:
					return m.GetCounter().GetValue()
				}
			}
		}
	}
	return 0
}

func metricHistogramCount(t *testing.T, name, workspaceID string) float64 {
	t.Helper()
	mfs, err := prometheus.DefaultGatherer.Gather()
	require.NoError(t, err)
	for _, mf := range mfs {
		if mf.GetName() != name {
			continue
		}
		for _, m := range mf.GetMetric() {
			if len(m.GetLabel()) == 0 {
				continue
			}
			if m.GetLabel()[0].GetValue() == workspaceID && m.GetHistogram() != nil {
				return float64(m.GetHistogram().GetSampleCount())
			}
		}
	}
	return 0
}

func newTestBuffer(maxSize int, timeout, poll time.Duration) *requestBuffer {
	return newRequestBuffer(maxSize, timeout, poll, &testLogger{})
}

func makeBufferedReq(forward func() error, deadline time.Time) *bufferedRequest {
	return &bufferedRequest{
		forward:  forward,
		result:   make(chan error, 1),
		deadline: deadline,
		cancelCh: make(chan struct{}),
	}
}

func TestRequestBuffer_TryEnqueueAcceptsUpToMaxSize(t *testing.T) {
	b := newTestBuffer(3, time.Second, 10*time.Millisecond)
	block := make(chan struct{})
	blockingForward := func() error { <-block; return nil }
	for i := 0; i < 3; i++ {
		req := makeBufferedReq(blockingForward, time.Now().Add(time.Hour))
		assert.True(t, b.tryEnqueue("ws-enq", req), "request %d should be accepted", i)
	}
	overflow := makeBufferedReq(blockingForward, time.Now().Add(time.Hour))
	assert.False(t, b.tryEnqueue("ws-enq", overflow), "4th request should be rejected")
	close(block)
}

func TestRequestBuffer_SingleDrainerProcessesSequentially(t *testing.T) {
	b := newTestBuffer(5, time.Second, 5*time.Millisecond)

	var inFlight int32
	var maxInFlight int32
	releaseChans := make([]chan struct{}, 5)
	for i := range releaseChans {
		releaseChans[i] = make(chan struct{})
	}

	forward := func(idx int) func() error {
		return func() error {
			cur := atomic.AddInt32(&inFlight, 1)
			for {
				old := atomic.LoadInt32(&maxInFlight)
				if cur <= old || atomic.CompareAndSwapInt32(&maxInFlight, old, cur) {
					break
				}
			}
			<-releaseChans[idx]
			atomic.AddInt32(&inFlight, -1)
			return nil
		}
	}

	var wg sync.WaitGroup
	wg.Add(5)
	for i := 0; i < 5; i++ {
		req := makeBufferedReq(forward(i), time.Now().Add(time.Hour))
		require.True(t, b.tryEnqueue("ws-one-drainer", req))
		go func(r *bufferedRequest) {
			defer wg.Done()
			<-r.result
		}(req)
	}

	for i := 0; i < 5; i++ {
		close(releaseChans[i])
	}
	wg.Wait()

	assert.LessOrEqual(t, int(atomic.LoadInt32(&maxInFlight)), 1, "at most one drainer forwards at a time")
}

func TestRequestBuffer_FIFOOrderPreserved(t *testing.T) {
	b := newTestBuffer(8, time.Second, 2*time.Millisecond)

	var mu sync.Mutex
	var order []int
	var wg sync.WaitGroup
	wg.Add(6)

	for i := 0; i < 6; i++ {
		idx := i
		req := makeBufferedReq(func() error {
			mu.Lock()
			order = append(order, idx)
			mu.Unlock()
			return nil
		}, time.Now().Add(time.Hour))
		require.True(t, b.tryEnqueue("ws-fifo", req))
		go func(r *bufferedRequest) {
			defer wg.Done()
			<-r.result
		}(req)
		time.Sleep(5 * time.Millisecond)
	}

	wg.Wait()
	mu.Lock()
	assert.Equal(t, []int{0, 1, 2, 3, 4, 5}, order, "forward order must match enqueue order")
	mu.Unlock()
}

func TestRequestBuffer_RetriesOnConnectionErrorThenSucceeds(t *testing.T) {
	b := newTestBuffer(2, time.Second, 3*time.Millisecond)

	var attempts int32
	req := makeBufferedReq(func() error {
		if atomic.AddInt32(&attempts, 1) <= 3 {
			return fmt.Errorf("dial tcp 10.0.0.1:4096: connection refused")
		}
		return nil
	}, time.Now().Add(time.Second))

	require.True(t, b.tryEnqueue("ws-retry", req))

	select {
	case ferr := <-req.result:
		assert.NoError(t, ferr)
		assert.GreaterOrEqual(t, atomic.LoadInt32(&attempts), int32(4))
	case <-time.After(2 * time.Second):
		t.Fatal("drainer did not succeed within deadline")
	}
}

func TestRequestBuffer_TimesOutWhenDeadlinePassed(t *testing.T) {
	b := newTestBuffer(2, time.Second, 5*time.Millisecond)

	req := makeBufferedReq(func() error {
		return fmt.Errorf("dial tcp: connection refused")
	}, time.Now().Add(-time.Millisecond))

	require.True(t, b.tryEnqueue("ws-timeout", req))

	select {
	case ferr := <-req.result:
		assert.ErrorIs(t, ferr, errBufferTimeout)
	case <-time.After(2 * time.Second):
		t.Fatal("drainer did not time out")
	}
}

func TestRequestBuffer_SkipsCanceledRequestProcessesNext(t *testing.T) {
	b := newTestBuffer(2, time.Second, 5*time.Millisecond)

	canceled := makeBufferedReq(func() error {
		t.Fatal("canceled request must not be forwarded")
		return nil
	}, time.Now().Add(time.Hour))
	close(canceled.cancelCh)

	var secondDone int32
	second := makeBufferedReq(func() error {
		atomic.StoreInt32(&secondDone, 1)
		return nil
	}, time.Now().Add(time.Hour))

	require.True(t, b.tryEnqueue("ws-cancel", canceled))
	time.Sleep(20 * time.Millisecond)
	require.True(t, b.tryEnqueue("ws-cancel", second))

	select {
	case <-second.result:
	case <-time.After(2 * time.Second):
		t.Fatal("second request was not processed")
	}
	assert.Equal(t, int32(1), atomic.LoadInt32(&secondDone))
}

func TestRequestBuffer_GaugeUpdatedOnEnqueueAndPop(t *testing.T) {
	ws := "ws-gauge"
	b := newTestBuffer(3, time.Second, 50*time.Millisecond)

	block := make(chan struct{})
	req1 := makeBufferedReq(func() error {
		<-block
		return nil
	}, time.Now().Add(time.Hour))
	req2 := makeBufferedReq(func() error { return nil }, time.Now().Add(time.Hour))

	require.True(t, b.tryEnqueue(ws, req1))
	require.Eventually(t, func() bool {
		return metricValue(t, "workspace_request_buffer_size", ws) == 1
	}, 200*time.Millisecond, 5*time.Millisecond)

	require.True(t, b.tryEnqueue(ws, req2))
	assert.Equal(t, 2.0, metricValue(t, "workspace_request_buffer_size", ws))

	close(block)

	require.Eventually(t, func() bool {
		return metricValue(t, "workspace_request_buffer_size", ws) == 0
	}, 200*time.Millisecond, 5*time.Millisecond)
	<-req2.result
}

func TestRequestBuffer_PerWorkspaceIsolation(t *testing.T) {
	b := newTestBuffer(1, time.Second, 5*time.Millisecond)

	blockA := make(chan struct{})
	reqA := makeBufferedReq(func() error {
		<-blockA
		return nil
	}, time.Now().Add(time.Hour))
	require.True(t, b.tryEnqueue("ws-iso-a", reqA))

	reqAFull := makeBufferedReq(func() error { return nil }, time.Now().Add(time.Hour))
	assert.False(t, b.tryEnqueue("ws-iso-a", reqAFull), "workspace A buffer should be full")

	reqB := makeBufferedReq(func() error { return nil }, time.Now().Add(time.Hour))
	assert.True(t, b.tryEnqueue("ws-iso-b", reqB), "workspace B buffer must be independent")
	<-reqB.result

	close(blockA)
	<-reqA.result
}

func TestSetRequestBufferConfig_ZeroConfigKeepsBufferingEnabled(t *testing.T) {
	handler, err := NewProxyHandler(k8smocks.NewMockKubernetesClient(), &testLogger{}, "default", nil, nil)
	require.NoError(t, err)

	handler.SetRequestBufferConfig(0, 0)

	require.NotNil(t, handler.requestBuffer)
	assert.Equal(t, defaultBufferMaxSize, handler.requestBuffer.maxSize, "zero-value config must not disable buffering in production")
	assert.Equal(t, defaultBufferTimeout, handler.requestBuffer.timeout)
}

type flapTransport struct {
	server   *httptest.Server
	failIP   string
	failN    int32
	attempts int32
}

func (t *flapTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	host := req.URL.Host
	if strings.HasPrefix(host, t.failIP) {
		if atomic.AddInt32(&t.attempts, 1) <= t.failN {
			return nil, fmt.Errorf("dial tcp %s: connection refused", t.failIP)
		}
	}
	req.URL.Scheme = "http"
	req.URL.Host = strings.TrimPrefix(t.server.URL, "http://")
	return http.DefaultTransport.RoundTrip(req)
}

type bufferTestEnv struct {
	router  *gin.Engine
	handler *ProxyHandler
}

func newBufferTestEnv(t *testing.T, httpClient *http.Client, workspaceID, podIP string) *bufferTestEnv {
	t.Helper()
	gin.SetMode(gin.TestMode)

	k8sMock := k8smocks.NewMockKubernetesClient()
	llmMock := k8smocks.NewMockLLMSafespacesV1Interface()
	wsMock := k8smocks.NewMockWorkspaceInterface()

	k8sMock.On("LlmsafespacesV1").Return(llmMock, nil)
	llmMock.On("Workspaces", "default").Return(wsMock)

	fakeClientset := k8sfake.NewSimpleClientset()
	k8sMock.On("Clientset").Return(fakeClientset)

	crd := makeWorkspaceCRDWithStatus(workspaceID, podIP, string(v1.WorkspacePhaseActive), workspaceID)
	wsMock.On("Get", mock.Anything, workspaceID, metav1.GetOptions{}).Return(crd, nil)

	secret := makePasswordSecret(workspaceID, "test-password")
	_, err := fakeClientset.CoreV1().Secrets("default").Create(context.Background(), secret, metav1.CreateOptions{})
	require.NoError(t, err)

	handler, err := NewProxyHandler(k8sMock, &testLogger{}, "default", httpClient, nil)
	require.NoError(t, err)

	router := gin.New()
	proxy := router.Group("/api/v1/workspaces/:id")
	{
		proxy.GET("/sessions/:sessionId/message", handler.GetHistory)
	}
	registerLegacyMessageTransport(router, handler)

	return &bufferTestEnv{router: router, handler: handler}
}

func (e *bufferTestEnv) doRequest(t *testing.T, method, path string, body string) *httptest.ResponseRecorder {
	t.Helper()
	var bodyReader io.Reader
	if body != "" {
		bodyReader = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, path, bodyReader)
	if bodyReader != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	w := httptest.NewRecorder()
	e.router.ServeHTTP(w, req)
	return w
}

func TestProxyBuffer_BufferedMessageSucceedsAfterFlap(t *testing.T) {
	var backendHits int32
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&backendHits, 1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	t.Cleanup(func() { backend.Close() })

	transport := &flapTransport{server: backend, failIP: "10.0.0.1:4096", failN: 3}
	httpClient := &http.Client{Transport: transport, Timeout: 5 * time.Second}

	env := newBufferTestEnv(t, httpClient, "ws-buf-ok", "10.0.0.1")
	env.handler.requestBuffer = newRequestBuffer(10, 2*time.Second, 5*time.Millisecond, env.handler.logger)
	w := env.doRequest(t, "POST", "/api/v1/workspaces/ws-buf-ok/legacy-message/s1", `{"message":"hi"}`)

	assert.Equal(t, http.StatusOK, w.Code, "buffered request must succeed after upstream recovers; body=%s", w.Body.String())
	assert.GreaterOrEqual(t, atomic.LoadInt32(&backendHits), int32(1))
	// The flap burns 3 connection-refused attempts before the 4th reaches the
	// backend; the buffer must have held the request and retried, so the
	// backend was only hit after the recoverable flap window elapsed.
	assert.Equal(t, int32(4), atomic.LoadInt32(&transport.attempts), "exactly 4 transport attempts (3 refused + 1 success) prove retry-buffering, not a single shot")
	// wait_seconds was observed → the request transited the buffer, not a dumb single retry.
	assert.GreaterOrEqual(t, metricHistogramCount(t, "workspace_request_buffer_wait_seconds", "ws-buf-ok"), 1.0)
}

func TestProxyBuffer_TimesOutWithRestartingMessage(t *testing.T) {
	httpClient := &http.Client{Transport: &alwaysFailTransport{}, Timeout: 5 * time.Second}
	env := newBufferTestEnv(t, httpClient, "ws-buf-to", "10.0.0.9")
	env.handler.requestBuffer = newRequestBuffer(10, 150*time.Millisecond, 10*time.Millisecond, env.handler.logger)

	before := metricValue(t, "workspace_request_buffer_timeout_total", "ws-buf-to")
	w := env.doRequest(t, "POST", "/api/v1/workspaces/ws-buf-to/legacy-message/s1", `{"message":"hi"}`)

	assert.Equal(t, http.StatusServiceUnavailable, w.Code)
	assert.Contains(t, w.Body.String(), "Workspace is restarting, please try again in a moment")
	assert.Equal(t, before+1, metricValue(t, "workspace_request_buffer_timeout_total", "ws-buf-to"))
}

func TestProxyBuffer_BufferFullRejectsWith429(t *testing.T) {
	httpClient := &http.Client{Transport: &alwaysFailTransport{}, Timeout: 5 * time.Second}
	env := newBufferTestEnv(t, httpClient, "ws-buf-full", "10.0.0.7")
	env.handler.requestBuffer = newRequestBuffer(2, 300*time.Millisecond, 5*time.Millisecond, env.handler.logger)

	before := metricValue(t, "workspace_request_buffer_full_total", "ws-buf-full")
	var wg sync.WaitGroup
	results := make(chan int, 3)
	for i := 0; i < 3; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			w := env.doRequest(t, "POST", "/api/v1/workspaces/ws-buf-full/legacy-message/s1", `{"message":"hi"}`)
			results <- w.Code
		}()
	}
	wg.Wait()
	close(results)

	var tooMany int
	for code := range results {
		if code == http.StatusTooManyRequests {
			tooMany++
		}
	}
	assert.GreaterOrEqual(t, tooMany, 1, "at least one request must be rejected for full buffer")
	assert.GreaterOrEqual(t, metricValue(t, "workspace_request_buffer_full_total", "ws-buf-full"), before+1)
}

func TestProxyBuffer_GETHistoryNotBufferedReturns503(t *testing.T) {
	httpClient := &http.Client{Transport: &alwaysFailTransport{}, Timeout: 5 * time.Second}
	env := newBufferTestEnv(t, httpClient, "ws-buf-get", "10.0.0.5")
	env.handler.requestBuffer = newRequestBuffer(10, 2*time.Second, 5*time.Millisecond, env.handler.logger)

	start := time.Now()
	w := env.doRequest(t, "GET", "/api/v1/workspaces/ws-buf-get/sessions/s1/message", "")
	elapsed := time.Since(start)

	assert.Equal(t, http.StatusServiceUnavailable, w.Code)
	assert.Contains(t, w.Body.String(), "workspace connection failed")
	assert.Less(t, elapsed, 500*time.Millisecond, "GET must fail fast, not buffer")
}

func TestProxyBuffer_DisabledFallsBackTo503(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(func() { backend.Close() })

	transport := &flapTransport{server: backend, failIP: "10.0.0.1:4096", failN: 3}
	httpClient := &http.Client{Transport: transport, Timeout: 5 * time.Second}

	env := newBufferTestEnv(t, httpClient, "ws-buf-off", "10.0.0.1")
	env.handler.requestBuffer = newRequestBuffer(0, 2*time.Second, 5*time.Millisecond, env.handler.logger)

	w := env.doRequest(t, "POST", "/api/v1/workspaces/ws-buf-off/legacy-message/s1", `{"message":"hi"}`)

	assert.Equal(t, http.StatusServiceUnavailable, w.Code)
	assert.Contains(t, w.Body.String(), "workspace connection failed")
}

func TestProxyBuffer_ClientDisconnectDuringBufferingNoDeadlock(t *testing.T) {
	httpClient := &http.Client{Transport: &alwaysFailTransport{}, Timeout: 5 * time.Second}
	env := newBufferTestEnv(t, httpClient, "ws-buf-disc", "10.0.0.3")
	env.handler.requestBuffer = newRequestBuffer(5, 5*time.Second, 20*time.Millisecond, env.handler.logger)

	ctx, cancel := context.WithCancel(context.Background())
	req := httptest.NewRequest("POST", "/api/v1/workspaces/ws-buf-disc/legacy-message/s1",
		strings.NewReader(`{"message":"hi"}`)).WithContext(ctx)
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	done := make(chan struct{})
	go func() {
		env.router.ServeHTTP(w, req)
		close(done)
	}()

	time.Sleep(80 * time.Millisecond)
	cancel()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("handler did not return after client disconnect (deadlock)")
	}
}

func TestRequestBuffer_PrunesEmptyQueueFromMap(t *testing.T) {
	b := newTestBuffer(2, time.Second, 5*time.Millisecond)

	req := makeBufferedReq(func() error { return nil }, time.Now().Add(time.Hour))
	require.True(t, b.tryEnqueue("ws-prune", req))
	<-req.result

	require.Eventually(t, func() bool {
		return b.queueCount() == 0
	}, 200*time.Millisecond, 5*time.Millisecond, "empty queue must be pruned from the map")
}

func TestRequestBuffer_RetryPredicateClassifiesSentinels(t *testing.T) {
	assert.False(t, isRetryableBufferErr(nil), "nil must not be retryable")
	assert.False(t, isRetryableBufferErr(errBufferTimeout), "timeout is terminal")
	assert.False(t, isRetryableBufferErr(errBufferCommitted), "committed-after-partial-write is terminal (F1)")
	assert.False(t, isRetryableBufferErr(errClientGone), "client-gone is terminal")
	assert.True(t, isRetryableBufferErr(errBufferRetryLater), "retry-later is retryable")
	assert.True(t, isRetryableBufferErr(fmt.Errorf("dial tcp: connection refused")), "connection error is retryable")
	assert.True(t, isRetryableBufferErr(fmt.Errorf("wrapped: %w", errBufferRetryLater)), "wrapped retry-later is retryable")
}

// ---------------------------------------------------------------------------
// C5 (worklog 371): global buffer byte cap.
// ---------------------------------------------------------------------------

// newTestBufferWithGlobalCap creates a buffer with a specific global byte cap
// so tests can verify the cap rejects oversized requests without needing to
// allocate ~500MB.
func newTestBufferWithGlobalCap(maxSize int, timeout, poll time.Duration, globalCap int64) *requestBuffer {
	return newRequestBufferWithGlobalCap(maxSize, timeout, poll, globalCap, &testLogger{})
}

// TestRequestBuffer_C5_GlobalByteCap_RejectsOversizedRequest verifies that
// when the global byte cap is reached, new requests are rejected (returns
// false from tryEnqueue). Pre-fix, the buffer had no global cap and a
// platform-wide restart could buffer ~1TB, OOM-killing the API server.
func TestRequestBuffer_C5_GlobalByteCap_RejectsOversizedRequest(t *testing.T) {
	// 100-byte global cap, 10-slot per-workspace cap. The per-workspace cap
	// is generous so the global cap is the binding constraint.
	b := newTestBufferWithGlobalCap(10, time.Second, 5*time.Millisecond, 100)

	block := make(chan struct{})
	blockingForward := func() error { <-block; return nil }

	// Enqueue a 60-byte request — succeeds (60 ≤ 100).
	req1 := makeBufferedReq(blockingForward, time.Now().Add(time.Hour))
	req1.bodySize = 60
	require.True(t, b.tryEnqueue("ws-c5-a", req1), "60-byte request must fit under 100-byte cap")

	// Enqueue a 50-byte request — rejected (60+50=110 > 100).
	req2 := makeBufferedReq(blockingForward, time.Now().Add(time.Hour))
	req2.bodySize = 50
	assert.False(t, b.tryEnqueue("ws-c5-b", req2),
		"50-byte request must be rejected — global cap (100) would be exceeded by 60+50=110")

	// A 0-byte request (no body) must still succeed — it doesn't consume budget.
	req3 := makeBufferedReq(blockingForward, time.Now().Add(time.Hour))
	req3.bodySize = 0
	assert.True(t, b.tryEnqueue("ws-c5-c", req3),
		"0-byte request must always succeed (doesn't consume global byte budget)")

	close(block)
	<-req1.result
	<-req3.result
}

// TestRequestBuffer_C5_GlobalByteCap_ReleasedOnPop verifies that bytes are
// returned to the global budget when a request is popped from the queue,
// allowing subsequent requests to be admitted.
func TestRequestBuffer_C5_GlobalByteCap_ReleasedOnPop(t *testing.T) {
	b := newTestBufferWithGlobalCap(10, time.Second, 5*time.Millisecond, 100)

	// Enqueue and complete a 60-byte request.
	req1 := makeBufferedReq(func() error { return nil }, time.Now().Add(time.Hour))
	req1.bodySize = 60
	require.True(t, b.tryEnqueue("ws-c5-rel", req1))
	<-req1.result

	// The 60 bytes must have been released — a new 60-byte request fits.
	req2 := makeBufferedReq(func() error { return nil }, time.Now().Add(time.Hour))
	req2.bodySize = 60
	assert.True(t, b.tryEnqueue("ws-c5-rel", req2),
		"after the first request drained, the 60-byte budget must be available again")
	<-req2.result
}

// TestRequestBuffer_C5_GlobalByteCap_ZeroBodySizeAlwaysAdmitted verifies that
// requests without a body (GET history, etc.) are never rejected by the byte
// cap — they don't consume memory budget. This is important because GETs are
// not buffered in practice (only SendMessage sets bufferable=true), but the
// guard must still be correct.
func TestRequestBuffer_C5_GlobalByteCap_ZeroBodySizeAlwaysAdmitted(t *testing.T) {
	// Cap of 1 byte — any positive body would be rejected, but 0-byte bodies pass.
	b := newTestBufferWithGlobalCap(1, time.Second, 5*time.Millisecond, 1)

	for i := 0; i < 20; i++ {
		req := makeBufferedReq(func() error { return nil }, time.Now().Add(time.Hour))
		req.bodySize = 0
		require.True(t, b.tryEnqueue("ws-c5-zero", req),
			"0-byte request %d must be admitted regardless of global cap", i)
		<-req.result
	}
}

// TestRequestBuffer_C5_GlobalByteCap_ConcurrentNoOversubscribe verifies that
// concurrent enqueues cannot breach the global cap. N goroutines each try to
// reserve budget that, individually, fits — but collectively exceeds the cap.
// The CAS loop must serialize them so the cap is never exceeded.
func TestRequestBuffer_C5_GlobalByteCap_ConcurrentNoOversubscribe(t *testing.T) {
	// 1000-byte cap. 50 goroutines × 30 bytes each = 1500 bytes requested.
	// At most floor(1000/30)=33 should succeed.
	const cap = int64(1000)
	const perReq = 30
	const goroutines = 50
	b := newTestBufferWithGlobalCap(goroutines, time.Second, 5*time.Millisecond, cap)

	block := make(chan struct{})
	var wg sync.WaitGroup
	successes := make(chan bool, goroutines)
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			req := makeBufferedReq(func() error { <-block; return nil }, time.Now().Add(time.Hour))
			req.bodySize = perReq
			successes <- b.tryEnqueue("ws-c5-conc", req)
		}()
	}
	wg.Wait()
	close(successes)

	admitted := 0
	for ok := range successes {
		if ok {
			admitted++
		}
	}
	assert.LessOrEqual(t, admitted, int(cap/perReq),
		"at most floor(cap/perReq)=%d requests must be admitted; got %d (CAS loop must prevent oversubscription)",
		cap/perReq, admitted)
	assert.Greater(t, admitted, 0, "at least one request must be admitted")

	close(block)
}

func TestRequestBuffer_TerminalErrorNotRetried(t *testing.T) {
	b := newTestBuffer(2, time.Second, 5*time.Millisecond)

	var attempts int32
	terminalErr := fmt.Errorf("creating proxy request: invalid target")
	req := makeBufferedReq(func() error {
		atomic.AddInt32(&attempts, 1)
		return terminalErr
	}, time.Now().Add(time.Hour))
	require.True(t, b.tryEnqueue("ws-term", req))

	select {
	case ferr := <-req.result:
		assert.ErrorIs(t, ferr, terminalErr)
		assert.Equal(t, int32(1), atomic.LoadInt32(&attempts), "terminal (non-connection) error must not be retried")
	case <-time.After(2 * time.Second):
		t.Fatal("terminal error was not delivered")
	}
}

func TestRequestBuffer_RetriesOnConnectionSlotUnavailable(t *testing.T) {
	b := newTestBuffer(2, time.Second, 3*time.Millisecond)

	slot := make(chan struct{}, 1)
	var attempts int32
	req := makeBufferedReq(func() error {
		n := atomic.AddInt32(&attempts, 1)
		if n <= 2 {
			return errBufferRetryLater
		}
		return nil
	}, time.Now().Add(time.Second))
	close(slot)
	require.True(t, b.tryEnqueue("ws-slot", req))

	select {
	case ferr := <-req.result:
		assert.NoError(t, ferr)
		assert.GreaterOrEqual(t, atomic.LoadInt32(&attempts), int32(3))
	case <-time.After(2 * time.Second):
		t.Fatal("retryable errBufferRetryLater did not recover")
	}
}

type sessionIndexSpy struct {
	messages int32
}

func (s *sessionIndexSpy) RecordMessage(_, _, _ string, _ time.Time) { atomic.AddInt32(&s.messages, 1) }
func (s *sessionIndexSpy) ListByWorkspace(_ context.Context, _ string) ([]types.SessionListItem, error) {
	return nil, nil
}
func (s *sessionIndexSpy) DeleteByWorkspace(_ context.Context, _ string) error  { return nil }
func (s *sessionIndexSpy) DeleteSession(_ context.Context, _, _ string) error   { return nil }
func (s *sessionIndexSpy) UpsertTitle(_ context.Context, _, _, _ string) error  { return nil }
func (s *sessionIndexSpy) UpsertParent(_ context.Context, _, _, _ string) error { return nil }
func (s *sessionIndexSpy) UpsertContextUsed(_ context.Context, _, _ string, _ int64) error {
	return nil
}
func (s *sessionIndexSpy) UpdateLastSeen(_ context.Context, _, _ string) error { return nil }
func (s *sessionIndexSpy) Start() error                                        { return nil }
func (s *sessionIndexSpy) Stop() error                                         { return nil }

func TestProxyBuffer_BufferedSuccessRecordsMessageAndKeepsSession(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	t.Cleanup(func() { backend.Close() })

	transport := &flapTransport{server: backend, failIP: "10.0.0.1:4096", failN: 2}
	httpClient := &http.Client{Transport: transport, Timeout: 5 * time.Second}

	env := newBufferTestEnv(t, httpClient, "ws-buf-succ", "10.0.0.1")
	env.handler.requestBuffer = newRequestBuffer(10, 2*time.Second, 5*time.Millisecond, env.handler.logger)

	spy := &sessionIndexSpy{}
	env.handler.sessionIndex = spy

	w := env.doRequest(t, "POST", "/api/v1/workspaces/ws-buf-succ/legacy-message/s1", `{"message":"hi"}`)

	assert.Equal(t, http.StatusOK, w.Code, "body=%s", w.Body.String())
	assert.Equal(t, int32(1), atomic.LoadInt32(&spy.messages), "buffered success must run the success block (RecordMessage)")
	assert.True(t, env.handler.isSessionActive(context.Background(), "ws-buf-succ", "s1"), "session must remain active after buffered success (removeActiveSession must NOT have run)")
}

func TestProxyBuffer_FIFOOrderAcrossConcurrentMessages(t *testing.T) {
	// This test verifies the drain loop's serial FIFO guarantee: when two
	// requests are in the same workspace queue, they are forwarded in
	// enqueue order regardless of how long each forward takes.
	//
	// The previous version sent two concurrent HTTP requests through the
	// Gin test server with a 10ms sleep between goroutine launches, then
	// asserted the backend received them in seq order. That was flaky
	// because the 10ms sleep doesn't guarantee enqueue order — goroutine
	// scheduling and HTTP processing are non-deterministic. The race was
	// in the enqueue path, not the drain path.
	//
	// This version enqueues directly into the buffer with deterministic
	// ordering and a forward function that fails N times before succeeding.
	// This tests the actual FIFO guarantee (serial drain, documented at
	// proxy_request_buffer.go:122) without the non-deterministic HTTP layer.

	var arrivalMu sync.Mutex
	var arrivals []string

	var sharedAttempts int32
	const failN = 3

	makeForward := func(body string) func() error {
		return func() error {
			if atomic.AddInt32(&sharedAttempts, 1) <= failN {
				return fmt.Errorf("dial tcp 10.0.0.1:4096: connection refused")
			}
			arrivalMu.Lock()
			arrivals = append(arrivals, body)
			arrivalMu.Unlock()
			return nil
		}
	}

	buf := newTestBuffer(10, 3*time.Second, 5*time.Millisecond)

	// Enqueue seq:0 FIRST, then seq:1. No goroutines, no HTTP — deterministic.
	req0 := makeBufferedReq(makeForward(`{"seq":0}`), time.Now().Add(3*time.Second))
	req1 := makeBufferedReq(makeForward(`{"seq":1}`), time.Now().Add(3*time.Second))
	require.True(t, buf.tryEnqueue("ws-fifo", req0), "seq:0 must be enqueued")
	require.True(t, buf.tryEnqueue("ws-fifo", req1), "seq:1 must be enqueued")

	// Wait for both to complete (forward returns nil = success).
	<-req0.result
	<-req1.result

	arrivalMu.Lock()
	defer arrivalMu.Unlock()
	require.Len(t, arrivals, 2, "both messages must reach the forward function")
	assert.Equal(t, `{"seq":0}`, arrivals[0],
		"drain loop must forward in FIFO enqueue order (seq:0 was enqueued first)")
	assert.Equal(t, `{"seq":1}`, arrivals[1],
		"drain loop must forward in FIFO enqueue order (seq:1 was enqueued second)")
}

func TestProxyBuffer_ParkedRequestsReleaseConnectionSlotGETAdmitted(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	t.Cleanup(func() { backend.Close() })

	transport := &flapTransport{server: backend, failIP: "10.0.0.1:4096", failN: 1000}
	httpClient := &http.Client{Transport: transport, Timeout: 5 * time.Second}

	env := newBufferTestEnv(t, httpClient, "ws-buf-slot", "10.0.0.1")
	env.handler.requestBuffer = newRequestBuffer(5, 150*time.Millisecond, 50*time.Millisecond, env.handler.logger)

	for i := 0; i < 5; i++ {
		go func() {
			env.doRequest(t, "POST", "/api/v1/workspaces/ws-buf-slot/legacy-message/s1", `{"message":"hi"}`)
		}()
	}

	require.Eventually(t, func() bool {
		return env.handler.connectionCount("ws-buf-slot") == 0
	}, 300*time.Millisecond, 5*time.Millisecond, "parked requests must hold 0 connection slots")

	getW := env.doRequest(t, "GET", "/api/v1/workspaces/ws-buf-slot/sessions/s1/message", "")
	assert.NotEqual(t, http.StatusTooManyRequests, getW.Code, "GET must be admitted (not connection-limited) while POSTs are parked")
	assert.NotContains(t, getW.Body.String(), "connection limit reached")
}

func TestProxyBuffer_ParkedRequestDoesNotDoubleReleaseConnectionSlot(t *testing.T) {
	httpClient := &http.Client{Transport: &alwaysFailTransport{}, Timeout: 5 * time.Second}
	env := newBufferTestEnv(t, httpClient, "ws-dblrel", "10.0.0.1")
	env.handler.requestBuffer = newRequestBuffer(10, 100*time.Millisecond, 10*time.Millisecond, env.handler.logger)

	require.True(t, env.handler.acquireConnection("ws-dblrel"), "pre-acquire one long-lived slot")
	defer env.handler.releaseConnection("ws-dblrel")
	require.Equal(t, 1, env.handler.connectionCount("ws-dblrel"))

	const n = 5
	var wg sync.WaitGroup
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func() {
			defer wg.Done()
			env.doRequest(t, "POST", "/api/v1/workspaces/ws-dblrel/legacy-message/s1", `{"message":"hi"}`)
		}()
	}
	wg.Wait()

	assert.Equal(t, 1, env.handler.connectionCount("ws-dblrel"),
		"after N parked requests time out, connCount must be exactly 1 (the long-lived slot); a double-release would drive it toward 0")

	maxConn := maxConnectionsPerWorkspace
	for i := 0; i < maxConn; i++ {
		require.True(t, env.handler.acquireConnection("ws-dblrel-lim"), "pre-acquire slot %d/%d", i, maxConn)
	}
	require.False(t, env.handler.acquireConnection("ws-dblrel-lim"),
		"the %dth connection must be rejected — the FD/memory safety limit is still enforced", maxConn+1)
	for i := 0; i < maxConn; i++ {
		env.handler.releaseConnection("ws-dblrel-lim")
	}
}

func TestRequestBuffer_CommittedWriteNotRetriedForwardCalledOnce(t *testing.T) {
	b := newTestBuffer(2, time.Second, 5*time.Millisecond)

	var calls int32
	committedErr := fmt.Errorf("upstream stream cut short: %w", errBufferCommitted)
	req := makeBufferedReq(func() error {
		atomic.AddInt32(&calls, 1)
		return committedErr
	}, time.Now().Add(time.Hour))
	require.True(t, b.tryEnqueue("ws-committed", req))

	select {
	case ferr := <-req.result:
		assert.ErrorIs(t, ferr, errBufferCommitted, "committed-after-partial-write must be delivered terminal, not retried")
		assert.ErrorIs(t, ferr, committedErr)
		assert.Equal(t, int32(1), atomic.LoadInt32(&calls), "forward must be invoked exactly once — a second call would write a second response onto the committed writer")
	case <-time.After(2 * time.Second):
		t.Fatal("committed outcome was not delivered")
	}
}

func TestProxyBuffer_DefaultConfigBufferFullReachable(t *testing.T) {
	httpClient := &http.Client{Transport: &alwaysFailTransport{}, Timeout: 5 * time.Second}
	env := newBufferTestEnv(t, httpClient, "ws-dflt-full", "10.0.0.1")
	env.handler.requestBuffer = newRequestBuffer(defaultBufferMaxSize, 2*time.Second, 20*time.Millisecond, env.handler.logger)

	before := metricValue(t, "workspace_request_buffer_full_total", "ws-dflt-full")

	var allInFlight sync.WaitGroup
	allInFlight.Add(11)
	var startAll sync.WaitGroup
	startAll.Add(1)
	var wg sync.WaitGroup
	wg.Add(11)
	var tooManyCount int32
	var tooManyBodiesMu sync.Mutex
	var tooManyBodies []string
	for i := 0; i < 11; i++ {
		go func() {
			defer wg.Done()
			allInFlight.Done()
			startAll.Wait()
			w := env.doRequest(t, "POST", "/api/v1/workspaces/ws-dflt-full/legacy-message/s1", `{"message":"hi"}`)
			if w.Code == http.StatusTooManyRequests {
				atomic.AddInt32(&tooManyCount, 1)
				tooManyBodiesMu.Lock()
				tooManyBodies = append(tooManyBodies, w.Body.String())
				tooManyBodiesMu.Unlock()
			}
		}()
	}
	allInFlight.Wait()
	startAll.Add(-1)
	wg.Wait()

	assert.GreaterOrEqual(t, atomic.LoadInt32(&tooManyCount), int32(1), "at least one of the 11 must hit the buffer-full path")
	tooManyBodiesMu.Lock()
	for _, body := range tooManyBodies {
		assert.Contains(t, body, "Too many requests during restart", "429 must come from the buffer-full path, not the connection-limit path")
		assert.NotContains(t, body, "connection limit reached")
	}
	tooManyBodiesMu.Unlock()
	assert.GreaterOrEqual(t, metricValue(t, "workspace_request_buffer_full_total", "ws-dflt-full"), before+1,
		"workspace_request_buffer_full_total must increment under the default (10) config")
}
