// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package handlers

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/textproto"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	dto "github.com/prometheus/client_model/go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8sfake "k8s.io/client-go/kubernetes/fake"

	k8smocks "github.com/lenaxia/llmsafespaces/mocks/kubernetes"
	v1 "github.com/lenaxia/llmsafespaces/pkg/apis/llmsafespaces/v1"

	"github.com/lenaxia/llmsafespaces/api/internal/services/metrics"
)

type uploadCaptureTransport struct {
	server *httptest.Server
	mu     sync.Mutex
	urls   []string
}

func (t *uploadCaptureTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	t.mu.Lock()
	t.urls = append(t.urls, req.URL.String())
	t.mu.Unlock()
	req.URL.Scheme = "http"
	req.URL.Host = strings.TrimPrefix(t.server.URL, "http://")
	return http.DefaultTransport.RoundTrip(req)
}

func (t *uploadCaptureTransport) requestCount() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return len(t.urls)
}

func (t *uploadCaptureTransport) requestURL(i int) string {
	t.mu.Lock()
	defer t.mu.Unlock()
	if i >= len(t.urls) {
		return ""
	}
	return t.urls[i]
}

// fakeAgentdRecording is what the fake agentd observed for one PUT.
type fakeAgentdRecording struct {
	mu        sync.Mutex
	method    string
	url       *url.URL
	basicUser string
	basicPass string
	readErr   error
	received  []byte
}

// newFakeAgentd serves PUT /v1/files the way the real agentd endpoint does:
// Basic auth is enforced, the body is streamed to completion (or abort), and
// a 201 {path,name,size} echoes the query filename and the byte count
// actually received. onBody is invoked with the bytes read once the body
// terminates (nil error) or aborts.
func newFakeAgentd(t *testing.T, status int, respBody string) (*fakeAgentdRecording, *httptest.Server) {
	t.Helper()
	rec := &fakeAgentdRecording{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPut || r.URL.Path != "/v1/files" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		n, err := io.ReadAll(r.Body)
		rec.mu.Lock()
		rec.method = r.Method
		rec.url = r.URL
		rec.readErr = err
		rec.received = n
		user, pass, _ := r.BasicAuth()
		rec.basicUser = user
		rec.basicPass = pass
		rec.mu.Unlock()

		if user != "opencode" || pass != "test-password" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		if respBody == "" {
			fmt.Fprintf(w, `{"path":"/workspace/uploads/00000000-0000-0000-0000-000000000001-%s","name":%q,"size":%d}`,
				r.URL.Query().Get("filename"), r.URL.Query().Get("filename"), len(n))
			return
		}
		_, _ = w.Write([]byte(respBody))
	}))
	t.Cleanup(srv.Close)
	return rec, srv
}

type uploadEnv struct {
	handler   *ProxyHandler
	router    *gin.Engine
	k8sMock   *k8smocks.MockKubernetesClient
	llmMock   *k8smocks.MockLLMSafespacesV1Interface
	wsMock    *k8smocks.MockWorkspaceInterface
	clientset *k8sfake.Clientset
	transport *uploadCaptureTransport
}

func newUploadEnv(t *testing.T, transport *uploadCaptureTransport) *uploadEnv {
	t.Helper()
	gin.SetMode(gin.TestMode)

	k8sMock := k8smocks.NewMockKubernetesClient()
	llmMock := k8smocks.NewMockLLMSafespacesV1Interface()
	wsMock := k8smocks.NewMockWorkspaceInterface()
	k8sMock.On("LlmsafespacesV1").Return(llmMock, nil)
	llmMock.On("Workspaces", "default").Return(wsMock)

	fakeClientset := k8sfake.NewSimpleClientset()
	k8sMock.On("Clientset").Return(fakeClientset)

	httpClient := &http.Client{Transport: transport, Timeout: 10 * time.Second}
	handler, err := NewProxyHandler(k8sMock, &testLogger{}, "default", httpClient, nil)
	require.NoError(t, err)

	router := gin.New()
	grp := router.Group("/api/v1/workspaces/:id")
	grp.POST("/uploads", handler.UploadFile)

	return &uploadEnv{
		handler:   handler,
		router:    router,
		k8sMock:   k8sMock,
		llmMock:   llmMock,
		wsMock:    wsMock,
		clientset: fakeClientset,
		transport: transport,
	}
}

func newUploadEnvWithFakeAgentd(t *testing.T, status int, respBody string) (*uploadEnv, *fakeAgentdRecording) {
	t.Helper()
	rec, srv := newFakeAgentd(t, status, respBody)
	return newUploadEnv(t, &uploadCaptureTransport{server: srv}), rec
}

func (e *uploadEnv) setupPassword(t *testing.T, password string) {
	t.Helper()
	_, err := e.clientset.CoreV1().Secrets("default").Create(
		context.Background(), makePasswordSecret("ws-1", password), metav1.CreateOptions{})
	require.NoError(t, err)
}

func makeUploadWorkspaceCRD(name, podIP, phase string, diskUsed, diskTotal int64) *v1.Workspace {
	return &v1.Workspace{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
		Spec:       v1.WorkspaceSpec{Owner: v1.WorkspaceOwner{UserID: "user-1"}, Runtime: "python:3.11"},
		Status: v1.WorkspaceStatus{
			Phase:          v1.WorkspacePhase(phase),
			PodIP:          podIP,
			DiskUsedBytes:  diskUsed,
			DiskTotalBytes: diskTotal,
		},
	}
}

func (e *uploadEnv) setupWorkspace(t *testing.T, ws *v1.Workspace) {
	t.Helper()
	e.wsMock.On("Get", mock.Anything, ws.Name, metav1.GetOptions{}).Return(ws, nil).Maybe()
}

type uploadPartSpec struct {
	field    string
	filename string
	content  []byte
}

// buildMultipart renders a multipart/form-data body. filename == "" emits a
// plain value field; otherwise a file part carries the filename bytes raw
// inside a quoted-string, escaped only where the MIME grammar requires
// (quoted-pair for " and \).
func buildMultipart(t *testing.T, parts ...uploadPartSpec) (*bytes.Buffer, string) {
	t.Helper()
	body := &bytes.Buffer{}
	mw := multipart.NewWriter(body)
	for _, p := range parts {
		if p.filename == "" {
			_ = mw.WriteField(p.field, string(p.content))
			continue
		}
		hdr := make(textproto.MIMEHeader)
		hdr.Set("Content-Disposition",
			`form-data; name="`+p.field+`"; filename="`+mimeQuoteFilename(p.filename)+`"`)
		hdr.Set("Content-Type", "application/octet-stream")
		fw, err := mw.CreatePart(hdr)
		require.NoError(t, err)
		_, err = fw.Write(p.content)
		require.NoError(t, err)
	}
	require.NoError(t, mw.Close())
	return body, mw.FormDataContentType()
}

func mimeQuoteFilename(s string) string {
	return strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(s)
}

// buildSmugglingMultipart renders one file part whose Content-Disposition
// value is injected verbatim — control characters included — the way a
// header-smuggling client would send it.
func buildSmugglingMultipart(t *testing.T, disposition string) (*bytes.Buffer, string) {
	t.Helper()
	body := &bytes.Buffer{}
	mw := multipart.NewWriter(body)
	hdr := make(textproto.MIMEHeader)
	hdr.Set("Content-Disposition", disposition)
	fw, err := mw.CreatePart(hdr)
	require.NoError(t, err)
	_, err = fw.Write([]byte("x"))
	require.NoError(t, err)
	require.NoError(t, mw.Close())
	return body, mw.FormDataContentType()
}

// opaqueReader hides the underlying reader's length so httptest.NewRequest
// cannot infer Content-Length (chunked-transfer shape).
type opaqueReader struct{ r io.Reader }

func (o opaqueReader) Read(p []byte) (int, error) { return o.r.Read(p) }

type uploadResponse struct {
	Path string `json:"path"`
	Name string `json:"name"`
	Size int64  `json:"size"`
}

func doUpload(env *uploadEnv, body io.Reader, contentType string) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/workspaces/ws-1/uploads", body)
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	env.router.ServeHTTP(w, req)
	return w
}

func resetUploadMetrics(t *testing.T) {
	t.Helper()
	metrics.UploadsCounter().Reset()
}

func uploadMetricValue(t *testing.T, reason string) float64 {
	t.Helper()
	m, err := metrics.UploadsCounter().GetMetricWithLabelValues(reason)
	require.NoError(t, err)
	var d dto.Metric
	require.NoError(t, m.Write(&d))
	return d.Counter.GetValue()
}

func activeUploadWS() *v1.Workspace {
	return makeUploadWorkspaceCRD("ws-1", "10.0.0.1", "Active", 0, 0)
}

// --- U1.2.1 happy path ---

func TestUpload_HappyPath(t *testing.T) {
	env, rec := newUploadEnvWithFakeAgentd(t, http.StatusCreated, "")
	env.setupPassword(t, "test-password")
	env.setupWorkspace(t, activeUploadWS())

	content := []byte("hello upload bytes")
	body, ct := buildMultipart(t, uploadPartSpec{field: "file", filename: "notes.txt", content: content})
	w := doUpload(env, body, ct)

	require.Equal(t, http.StatusCreated, w.Code, "body: %s", w.Body.String())

	var resp uploadResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.Equal(t, "notes.txt", resp.Name)
	assert.Equal(t, int64(len(content)), resp.Size)
	assert.Equal(t, "/workspace/uploads/00000000-0000-0000-0000-000000000001-notes.txt", resp.Path)

	rec.mu.Lock()
	defer rec.mu.Unlock()
	assert.Equal(t, content, rec.received, "agentd must receive exactly the file bytes")
	assert.NoError(t, rec.readErr)
}

// --- U1.2.2 forwards Basic auth + targets the agentd user mux :4097 ---

func TestUpload_ForwardsBasicAuthAndTargetsAgentdUserMux(t *testing.T) {
	env, rec := newUploadEnvWithFakeAgentd(t, http.StatusCreated, "")
	env.setupPassword(t, "test-password")
	env.setupWorkspace(t, activeUploadWS())

	body, ct := buildMultipart(t, uploadPartSpec{field: "file", filename: "notes.txt", content: []byte("x")})
	w := doUpload(env, body, ct)
	require.Equal(t, http.StatusCreated, w.Code)

	require.Equal(t, 1, env.transport.requestCount())
	assert.Equal(t,
		"http://10.0.0.1:4097/v1/files?filename=notes.txt",
		env.transport.requestURL(0),
		"upload must target the agentd USER mux (4097), not opencode 4096 or admin 4098")

	rec.mu.Lock()
	defer rec.mu.Unlock()
	assert.Equal(t, "opencode", rec.basicUser)
	assert.Equal(t, "test-password", rec.basicPass)
	assert.Equal(t, http.MethodPut, rec.method)
}

// --- U1.2.3 streaming proof: the agentd hop receives file bytes while the
// client is still producing. A fully-buffering implementation deadlocks the
// handshake (producer waits for the agentd-read signal before writing the
// final chunk; a buffering API would never dial agentd until the producer
// finished, which it never does) and the test fails on the 10s timeout. ---

const cap1MiB = int64(1 << 20)

// thresholdSignalingReader closes ch once threshold bytes have been read —
// proves the agentd hop is receiving file bytes WHILE the client is still
// producing them.
type thresholdSignalingReader struct {
	r         io.Reader
	seen      int64
	threshold int64
	ch        chan struct{}
	once      *sync.Once
}

func (t *thresholdSignalingReader) Read(p []byte) (int, error) {
	n, err := t.r.Read(p)
	t.seen += int64(n)
	if t.seen >= t.threshold {
		t.once.Do(func() { close(t.ch) })
	}
	return n, err
}

func TestUpload_Streams_WithoutFullBuffering(t *testing.T) {
	agentdHasMiB := make(chan struct{})
	agentdSum := make(chan []byte, 1)
	var signalOnce sync.Once
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sig := &thresholdSignalingReader{r: r.Body, threshold: cap1MiB, ch: agentdHasMiB, once: &signalOnce}
		h := sha256.New()
		n, err := io.Copy(h, sig)
		if err != nil {
			return
		}
		w.WriteHeader(http.StatusCreated)
		fmt.Fprintf(w, `{"path":"/workspace/uploads/x-big.bin","name":"big.bin","size":%d}`, n)
		agentdSum <- h.Sum(nil)
	}))
	t.Cleanup(srv.Close)
	env := newUploadEnv(t, &uploadCaptureTransport{server: srv})
	env.handler.SetUploadLimitsForTest(2*cap1MiB, 0)
	env.setupPassword(t, "test-password")
	env.setupWorkspace(t, activeUploadWS())

	chunk1 := make([]byte, 512*1024)
	chunk2 := make([]byte, 512*1024)
	chunk3 := []byte("tail")
	_, _ = rand.Read(chunk1)
	_, _ = rand.Read(chunk2)
	want := append(append(append([]byte{}, chunk1...), chunk2...), chunk3...)

	pr, pw := io.Pipe()
	producerErr := make(chan error, 1)
	mw := multipart.NewWriter(pw)
	go func() {
		hdr := make(textproto.MIMEHeader)
		hdr.Set("Content-Disposition", `form-data; name="file"; filename="big.bin"`)
		fw, err := mw.CreatePart(hdr)
		if err != nil {
			producerErr <- fmt.Errorf("create part: %w", err)
			_ = pw.CloseWithError(err)
			return
		}
		for _, chunk := range [][]byte{chunk1, chunk2} {
			if _, err := fw.Write(chunk); err != nil {
				producerErr <- fmt.Errorf("write chunk: %w", err)
				_ = pw.CloseWithError(err)
				return
			}
		}
		select {
		case <-agentdHasMiB:
		case <-time.After(10 * time.Second):
			producerErr <- errors.New("streaming violation: agentd received nothing while producer was still writing — API buffered the body")
			_ = pw.Close()
			return
		}
		if _, err := fw.Write(chunk3); err != nil {
			producerErr <- fmt.Errorf("write tail: %w", err)
			_ = pw.CloseWithError(err)
			return
		}
		producerErr <- mw.Close()
		_ = pw.Close()
	}()

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/workspaces/ws-1/uploads", opaqueReader{pr})
	req.Header.Set("Content-Type", mw.FormDataContentType())
	handlerDone := make(chan struct{})
	go func() {
		defer close(handlerDone)
		env.router.ServeHTTP(w, req)
	}()

	require.NoError(t, <-producerErr)
	select {
	case <-handlerDone:
	case <-time.After(10 * time.Second):
		t.Fatal("handler did not finish after the producer closed the body")
	}
	require.Equal(t, http.StatusCreated, w.Code, "body: %s", w.Body.String())

	var resp uploadResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.Equal(t, int64(len(want)), resp.Size, "agentd-received byte count must equal the file size")
	select {
	case got := <-agentdSum:
		wantSum := sha256.Sum256(want)
		assert.Equal(t, wantSum[:], got, "agentd must receive the file bytes unmodified")
	case <-time.After(5 * time.Second):
		t.Fatal("agentd never finished reading the body")
	}
}

// --- U1.2.4 cap enforced locally when Content-Length is known ---

func TestUpload_CapRejectedLocally_WhenContentLengthKnown(t *testing.T) {
	resetUploadMetrics(t)
	env, _ := newUploadEnvWithFakeAgentd(t, http.StatusCreated, "")
	env.handler.SetUploadLimitsForTest(1024, 0)
	env.setupPassword(t, "test-password")
	env.setupWorkspace(t, activeUploadWS())

	content := make([]byte, 256*1024)
	body, ct := buildMultipart(t, uploadPartSpec{field: "file", filename: "big.bin", content: content})
	require.Greater(t, int64(body.Len()), 1024+uploadEnvelopeAllowance)

	w := doUpload(env, body, ct)
	assert.Equal(t, http.StatusRequestEntityTooLarge, w.Code)
	assert.Equal(t, 0, env.transport.requestCount(), "over-cap request must not dial agentd")
	assert.Equal(t, 1.0, uploadMetricValue(t, "cap"))
}

// --- U1.2.5 / U1.2.14 chunked overrun cut at cap+1 → 413 ---

func TestUpload_CapChunkedOverrun_CutAtLimit(t *testing.T) {
	resetUploadMetrics(t)
	env, _ := newUploadEnvWithFakeAgentd(t, http.StatusCreated, "")
	env.handler.SetUploadLimitsForTest(1024, 0)
	env.setupPassword(t, "test-password")
	env.setupWorkspace(t, activeUploadWS())

	content := make([]byte, 4096)
	body, ct := buildMultipart(t, uploadPartSpec{field: "file", filename: "big.bin", content: content})

	w := doUpload(env, opaqueReader{body}, ct)
	assert.Equal(t, http.StatusRequestEntityTooLarge, w.Code)
	assert.Equal(t, 1.0, uploadMetricValue(t, "cap"))
}

// --- U1.2.14 spoofed Content-Length: claims small, streams big ---

func TestUpload_ContentLengthSpoof_StillCapped(t *testing.T) {
	env, _ := newUploadEnvWithFakeAgentd(t, http.StatusCreated, "")
	env.handler.SetUploadLimitsForTest(1024, 0)
	env.setupPassword(t, "test-password")
	env.setupWorkspace(t, activeUploadWS())

	content := make([]byte, 8192)
	body, ct := buildMultipart(t, uploadPartSpec{field: "file", filename: "spoof.bin", content: content})

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/workspaces/ws-1/uploads", opaqueReader{body})
	req.Header.Set("Content-Type", ct)
	req.ContentLength = 512 // spoofed: under cap
	env.router.ServeHTTP(w, req)

	assert.Equal(t, http.StatusRequestEntityTooLarge, w.Code)
}

// --- exact-cap boundary (D4): exactly cap passes, cap+1 fails ---

func TestUpload_CapBoundary(t *testing.T) {
	tests := []struct {
		name     string
		size     int
		wantCode int
	}{
		{name: "exactly cap accepted", size: 1024, wantCode: http.StatusCreated},
		{name: "cap plus one rejected", size: 1025, wantCode: http.StatusRequestEntityTooLarge},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			env, _ := newUploadEnvWithFakeAgentd(t, http.StatusCreated, "")
			env.handler.SetUploadLimitsForTest(1024, 0)
			env.setupPassword(t, "test-password")
			env.setupWorkspace(t, activeUploadWS())

			content := make([]byte, tt.size)
			body, ct := buildMultipart(t, uploadPartSpec{field: "file", filename: "exact.bin", content: content})

			w := doUpload(env, opaqueReader{body}, ct)
			assert.Equal(t, tt.wantCode, w.Code, "body: %s", w.Body.String())
		})
	}
}

// --- U1.2.6 / I3 client disconnect propagates to the agentd request ---

func TestUpload_ClientDisconnect_AbortsAgentdRequest(t *testing.T) {
	agentdReadDone := make(chan error, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, err := io.Copy(io.Discard, r.Body)
		agentdReadDone <- err
	}))
	t.Cleanup(srv.Close)
	env := newUploadEnv(t, &uploadCaptureTransport{server: srv})
	env.handler.SetUploadLimitsForTest(1<<20, 0)
	env.setupPassword(t, "test-password")
	env.setupWorkspace(t, activeUploadWS())

	ctx, cancel := context.WithCancel(context.Background())
	producerStarted := make(chan struct{})
	pr, pw := io.Pipe()
	mw := multipart.NewWriter(pw)
	go func() {
		hdr := make(textproto.MIMEHeader)
		hdr.Set("Content-Disposition", `form-data; name="file"; filename="cut.bin"`)
		fw, err := mw.CreatePart(hdr)
		if err != nil {
			_ = pw.CloseWithError(err)
			return
		}
		chunk := make([]byte, 64*1024)
		if _, err := fw.Write(chunk); err != nil {
			_ = pw.CloseWithError(err)
			return
		}
		close(producerStarted)
		<-ctx.Done()
		_ = pw.CloseWithError(ctx.Err())
	}()

	handlerDone := make(chan struct{})
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/workspaces/ws-1/uploads", opaqueReader{pr}).WithContext(ctx)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	go func() {
		defer close(handlerDone)
		env.router.ServeHTTP(w, req)
	}()

	<-producerStarted
	time.Sleep(100 * time.Millisecond)
	cancel()

	select {
	case <-handlerDone:
	case <-time.After(10 * time.Second):
		t.Fatal("handler did not return after client disconnect")
	}
	select {
	case err := <-agentdReadDone:
		assert.Error(t, err, "agentd body read must be aborted when the client disconnects")
	case <-time.After(10 * time.Second):
		t.Fatal("agentd body read never terminated after client disconnect")
	}
}

// --- U1.2.7 phase gate table: all 9 CRD phases ---

func TestUpload_PhaseGateTable(t *testing.T) {
	phases := []string{
		"Pending", "Creating", "Active", "Suspending", "Suspended",
		"Resuming", "Terminating", "Terminated", "Failed",
	}
	for _, phase := range phases {
		t.Run(phase, func(t *testing.T) {
			resetUploadMetrics(t)
			env, _ := newUploadEnvWithFakeAgentd(t, http.StatusCreated, "")
			env.setupPassword(t, "test-password")
			env.setupWorkspace(t, makeUploadWorkspaceCRD("ws-1", "10.0.0.1", phase, 0, 0))

			body, ct := buildMultipart(t, uploadPartSpec{field: "file", filename: "notes.txt", content: []byte("x")})
			w := doUpload(env, body, ct)

			if phase == "Active" {
				assert.Equal(t, http.StatusCreated, w.Code)
				return
			}
			assert.Equal(t, http.StatusConflict, w.Code, "body: %s", w.Body.String())
			assert.Contains(t, w.Body.String(), `"phase":"`+phase+`"`)
			assert.Equal(t, 0, env.transport.requestCount(), "non-Active phase must not reach agentd")
			assert.Equal(t, 1.0, uploadMetricValue(t, "phase"))
		})
	}
}

func TestUpload_PhaseGate_ActiveWithoutPodIP(t *testing.T) {
	env, _ := newUploadEnvWithFakeAgentd(t, http.StatusCreated, "")
	env.setupPassword(t, "test-password")
	env.setupWorkspace(t, makeUploadWorkspaceCRD("ws-1", "", "Active", 0, 0))

	body, ct := buildMultipart(t, uploadPartSpec{field: "file", filename: "notes.txt", content: []byte("x")})
	w := doUpload(env, body, ct)

	assert.Equal(t, http.StatusConflict, w.Code)
	assert.Contains(t, w.Body.String(), `"phase":"Active"`)
}

// --- U1.2.8 disk gate: ratio >= critical → 507; below → pass; unknown → open ---

func TestUpload_DiskGateTable(t *testing.T) {
	tests := []struct {
		name       string
		used       int64
		total      int64
		wantCode   int
		wantMetric string
	}{
		{name: "ratio 0.95 gated", used: 95, total: 100, wantCode: http.StatusInsufficientStorage, wantMetric: "disk"},
		{name: "ratio above critical gated", used: 99, total: 100, wantCode: http.StatusInsufficientStorage, wantMetric: "disk"},
		{name: "ratio 0.949 passes", used: 949, total: 1000, wantCode: http.StatusCreated},
		{name: "unknown total fails open", used: 500, total: 0, wantCode: http.StatusCreated},
		{name: "zero usage passes", used: 0, total: 100, wantCode: http.StatusCreated},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resetUploadMetrics(t)
			env, _ := newUploadEnvWithFakeAgentd(t, http.StatusCreated, "")
			env.setupPassword(t, "test-password")
			env.setupWorkspace(t, makeUploadWorkspaceCRD("ws-1", "10.0.0.1", "Active", tt.used, tt.total))

			body, ct := buildMultipart(t, uploadPartSpec{field: "file", filename: "notes.txt", content: []byte("x")})
			w := doUpload(env, body, ct)

			assert.Equal(t, tt.wantCode, w.Code, "body: %s", w.Body.String())
			if tt.wantMetric != "" {
				assert.Equal(t, 1.0, uploadMetricValue(t, tt.wantMetric))
				assert.Equal(t, 0, env.transport.requestCount(), "disk-gated request must not reach agentd")
			}
		})
	}
}

// --- U1.2.9 agentd error mapping ---

func TestUpload_AgentdErrorMapping(t *testing.T) {
	t.Run("connection refused maps to 502", func(t *testing.T) {
		resetUploadMetrics(t)
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
		t.Cleanup(srv.Close)
		env := newUploadEnv(t, &uploadCaptureTransport{server: srv})
		env.handler.httpClient = &http.Client{Transport: &alwaysFailTransport{}, Timeout: 2 * time.Second}
		env.setupPassword(t, "test-password")
		env.setupWorkspace(t, activeUploadWS())

		body, ct := buildMultipart(t, uploadPartSpec{field: "file", filename: "x.txt", content: []byte("x")})
		w := doUpload(env, body, ct)

		assert.Equal(t, http.StatusBadGateway, w.Code)
		assert.NotContains(t, w.Body.String(), "dial tcp", "agentd internals must not leak")
		assert.Equal(t, 1.0, uploadMetricValue(t, "agentd_error"))
	})

	t.Run("agentd 413 maps to 413", func(t *testing.T) {
		resetUploadMetrics(t)
		env, _ := newUploadEnvWithFakeAgentd(t, http.StatusRequestEntityTooLarge, `{"error":"file exceeds size cap"}`)
		env.setupPassword(t, "test-password")
		env.setupWorkspace(t, activeUploadWS())

		body, ct := buildMultipart(t, uploadPartSpec{field: "file", filename: "x.txt", content: []byte("x")})
		w := doUpload(env, body, ct)

		assert.Equal(t, http.StatusRequestEntityTooLarge, w.Code)
		assert.Equal(t, 1.0, uploadMetricValue(t, "cap"))
	})

	t.Run("agentd 500 maps to 502", func(t *testing.T) {
		resetUploadMetrics(t)
		env, _ := newUploadEnvWithFakeAgentd(t, http.StatusInternalServerError, `{"error":"storage unavailable"}`)
		env.setupPassword(t, "test-password")
		env.setupWorkspace(t, activeUploadWS())

		body, ct := buildMultipart(t, uploadPartSpec{field: "file", filename: "x.txt", content: []byte("x")})
		w := doUpload(env, body, ct)

		assert.Equal(t, http.StatusBadGateway, w.Code)
		assert.NotContains(t, w.Body.String(), "storage unavailable", "agentd error text must not leak")
		assert.Equal(t, 1.0, uploadMetricValue(t, "agentd_error"))
	})

	t.Run("timeout maps to 504", func(t *testing.T) {
		resetUploadMetrics(t)
		block := make(chan struct{})
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = io.Copy(io.Discard, r.Body)
			<-block
			w.WriteHeader(http.StatusCreated)
		}))
		t.Cleanup(srv.Close)
		t.Cleanup(func() { close(block) })
		env := newUploadEnv(t, &uploadCaptureTransport{server: srv})
		env.handler.SetUploadLimitsForTest(0, 150*time.Millisecond)
		env.setupPassword(t, "test-password")
		env.setupWorkspace(t, activeUploadWS())

		body, ct := buildMultipart(t, uploadPartSpec{field: "file", filename: "x.txt", content: []byte("x")})
		w := doUpload(env, body, ct)

		assert.Equal(t, http.StatusGatewayTimeout, w.Code)
		assert.Equal(t, 1.0, uploadMetricValue(t, "agentd_error"))
	})
}

// --- U1.2.10 malformed multipart shapes ---

func TestUpload_MalformedMultipart(t *testing.T) {
	tests := []struct {
		name  string
		parts []uploadPartSpec
	}{
		{name: "no file part", parts: []uploadPartSpec{{field: "note", filename: "", content: []byte("hi")}}},
		{name: "wrong field name", parts: []uploadPartSpec{{field: "attachment", filename: "a.txt", content: []byte("x")}}},
		{name: "two file parts", parts: []uploadPartSpec{
			{field: "file", filename: "a.txt", content: []byte("a")},
			{field: "file", filename: "b.txt", content: []byte("b")},
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			env, _ := newUploadEnvWithFakeAgentd(t, http.StatusCreated, "")
			env.setupPassword(t, "test-password")
			env.setupWorkspace(t, activeUploadWS())

			body, ct := buildMultipart(t, tt.parts...)
			w := doUpload(env, body, ct)
			assert.Equal(t, http.StatusBadRequest, w.Code, "body: %s", w.Body.String())
		})
	}
}

func TestUpload_EmptyMultipartBody(t *testing.T) {
	env, _ := newUploadEnvWithFakeAgentd(t, http.StatusCreated, "")
	env.setupPassword(t, "test-password")
	env.setupWorkspace(t, activeUploadWS())

	body, ct := buildMultipart(t)
	w := doUpload(env, body, ct)
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

// --- U1.2.13 empty filename ---

func TestUpload_EmptyFilename_Rejected(t *testing.T) {
	env, _ := newUploadEnvWithFakeAgentd(t, http.StatusCreated, "")
	env.setupPassword(t, "test-password")
	env.setupWorkspace(t, activeUploadWS())

	body := &bytes.Buffer{}
	mw := multipart.NewWriter(body)
	hdr := make(textproto.MIMEHeader)
	hdr.Set("Content-Disposition", `form-data; name="file"; filename=""`)
	fw, err := mw.CreatePart(hdr)
	require.NoError(t, err)
	_, _ = fw.Write([]byte("x"))
	require.NoError(t, mw.Close())

	w := doUpload(env, body, mw.FormDataContentType())
	assert.Equal(t, http.StatusBadRequest, w.Code)
	assert.Equal(t, 0, env.transport.requestCount(), "invalid filename must not reach agentd")
}

// --- U1.2.11 API-side sanitization before forwarding ---
//
// Raw control bytes (ESC, CR, LF) cannot survive Go's MIME header parser —
// those are covered by the smuggling test below. This table covers every
// hostile shape that CAN arrive as a filename value.

func TestUpload_FilenameSanitizedBeforeForwarding(t *testing.T) {
	tests := []struct {
		raw  string
		want string
	}{
		{raw: "../../etc/passwd", want: "passwd"},
		{raw: "/abs/path/file.txt", want: "file.txt"},
		{raw: `..\..\win\cmd.exe`, want: "cmd.exe"},
		{raw: `a\b.txt`, want: "b.txt"},
		{raw: "report\xe2\x80\xae4gp.pdf", want: "report4gp.pdf"},
		{raw: "a\xe2\x80\xacb", want: "ab"},
		{raw: `my"file.txt`, want: "myfile.txt"},
		{raw: "don't.txt", want: "dont.txt"},
		{raw: "name.pdf ...  ", want: "name.pdf"},
		{raw: "文档-report.pdf", want: "文档-report.pdf"},
		{raw: ".bashrc", want: ".bashrc"},
		{raw: "notes.txt", want: "notes.txt"},
	}
	for _, tt := range tests {
		t.Run(fmt.Sprintf("%q", tt.raw), func(t *testing.T) {
			env, _ := newUploadEnvWithFakeAgentd(t, http.StatusCreated, "")
			env.setupPassword(t, "test-password")
			env.setupWorkspace(t, activeUploadWS())

			body, ct := buildMultipart(t, uploadPartSpec{field: "file", filename: tt.raw, content: []byte("x")})
			w := doUpload(env, body, ct)

			require.Equal(t, http.StatusCreated, w.Code, "body: %s", w.Body.String())
			require.Equal(t, 1, env.transport.requestCount())
			got := env.transport.requestURL(0)
			assert.True(t, strings.Contains(got, "filename="+url.QueryEscape(tt.want)),
				"forwarded URL %q must carry sanitized filename %q", got, tt.want)
			assert.NotContains(t, got, "%0A", "newline must not survive into the forwarded query")
			assert.NotContains(t, got, "%0D", "carriage return must not survive into the forwarded query")
		})
	}
}

// --- U1.2.15 CRLF / control chars in Content-Disposition: header-smuggling
// attempts never reach the agentd request. Raw CR/LF/ESC inside the
// disposition value is rejected by the MIME parser (400); a CRLF-split
// smuggled part header is dropped — the API→agentd request carries only
// headers the API itself sets ---

func TestUpload_DispositionSmuggling_Neutralized(t *testing.T) {
	tests := []struct {
		name        string
		disposition string
		wantCode    int
	}{
		{name: "raw CR in filename", disposition: `form-data; name="file"; filename="re` + "\r" + `port.txt"`, wantCode: http.StatusBadRequest},
		{name: "raw LF in filename", disposition: `form-data; name="file"; filename="re` + "\n" + `port.txt"`, wantCode: http.StatusBadRequest},
		{name: "raw ESC in filename", disposition: `form-data; name="file"; filename="` + "\x1b" + `[31mred.txt"`, wantCode: http.StatusBadRequest},
		{name: "CRLF header split", disposition: `form-data; name="file"; filename="a.txt"` + "\r\n" + `X-Smuggled: yes`, wantCode: http.StatusCreated},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var smuggledHeaderSeen bool
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("X-Smuggled") != "" {
					smuggledHeaderSeen = true
				}
				if r.Method != http.MethodPut {
					w.WriteHeader(http.StatusNotFound)
					return
				}
				n, err := io.ReadAll(r.Body)
				if err != nil {
					w.WriteHeader(http.StatusInternalServerError)
					return
				}
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusCreated)
				fmt.Fprintf(w, `{"path":"/workspace/uploads/a.txt","name":"a.txt","size":%d}`, len(n))
			}))
			t.Cleanup(srv.Close)
			env := newUploadEnv(t, &uploadCaptureTransport{server: srv})
			env.setupPassword(t, "test-password")
			env.setupWorkspace(t, activeUploadWS())

			body, ct := buildSmugglingMultipart(t, tt.disposition)
			w := doUpload(env, body, ct)

			assert.Equal(t, tt.wantCode, w.Code, "body: %s", w.Body.String())
			assert.False(t, smuggledHeaderSeen, "part headers must never be forwarded to agentd")
			if tt.wantCode == http.StatusBadRequest {
				assert.Equal(t, 0, env.transport.requestCount(), "smuggled disposition must not reach agentd")
			}
		})
	}
}

// --- U1.2.12 wrong method per gin conventions ---

func TestUpload_WrongMethod_NotFound(t *testing.T) {
	env, _ := newUploadEnvWithFakeAgentd(t, http.StatusCreated, "")
	for _, method := range []string{http.MethodGet, http.MethodPut} {
		w := httptest.NewRecorder()
		req := httptest.NewRequest(method, "/api/v1/workspaces/ws-1/uploads", nil)
		env.router.ServeHTTP(w, req)
		assert.Equal(t, http.StatusNotFound, w.Code, "%s must not be routed", method)
	}
}

// --- U1.2.17 wrong content type ---

func TestUpload_WrongContentType_UnsupportedMediaType(t *testing.T) {
	tests := []struct {
		name        string
		contentType string
	}{
		{name: "json body", contentType: "application/json"},
		{name: "multipart without boundary", contentType: "multipart/form-data"},
		{name: "urlencoded", contentType: "application/x-www-form-urlencoded"},
		{name: "empty", contentType: ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			env, _ := newUploadEnvWithFakeAgentd(t, http.StatusCreated, "")
			env.setupPassword(t, "test-password")
			env.setupWorkspace(t, activeUploadWS())

			w := doUpload(env, strings.NewReader(`x`), tt.contentType)
			assert.Equal(t, http.StatusUnsupportedMediaType, w.Code)
			assert.Equal(t, 0, env.transport.requestCount())
		})
	}
}

// --- U1.2.18 non-file form fields ignored ---

func TestUpload_NonFileFieldsIgnored(t *testing.T) {
	env, rec := newUploadEnvWithFakeAgentd(t, http.StatusCreated, "")
	env.setupPassword(t, "test-password")
	env.setupWorkspace(t, activeUploadWS())

	content := []byte("file-bytes")
	body, ct := buildMultipart(t,
		uploadPartSpec{field: "caption", filename: "", content: []byte("my caption")},
		uploadPartSpec{field: "file", filename: "notes.txt", content: content},
		uploadPartSpec{field: "trailing", filename: "", content: []byte("after")},
	)
	w := doUpload(env, body, ct)

	require.Equal(t, http.StatusCreated, w.Code, "body: %s", w.Body.String())
	rec.mu.Lock()
	defer rec.mu.Unlock()
	assert.Equal(t, content, rec.received, "only the file part's bytes must be forwarded")
}

// --- U1.2.19 agentd garbage / non-JSON response ---

func TestUpload_AgentdGarbageResponse_502(t *testing.T) {
	tests := []struct {
		name     string
		status   int
		respBody string
	}{
		{name: "201 with garbage body", status: http.StatusCreated, respBody: "not-json{["},
		{name: "200 with empty body", status: http.StatusOK, respBody: ""},
		{name: "418 teapot", status: http.StatusTeapot, respBody: "short and stout"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resetUploadMetrics(t)
			env, _ := newUploadEnvWithFakeAgentd(t, tt.status, tt.respBody)
			env.setupPassword(t, "test-password")
			env.setupWorkspace(t, activeUploadWS())

			body, ct := buildMultipart(t, uploadPartSpec{field: "file", filename: "x.txt", content: []byte("x")})
			w := doUpload(env, body, ct)

			assert.Equal(t, http.StatusBadGateway, w.Code)
			assert.NotContains(t, w.Body.String(), "short and stout", "agentd body must not leak")
			assert.Equal(t, 1.0, uploadMetricValue(t, "agentd_error"))
		})
	}
}

// --- U1.2.20 workspace CRD gone mid-request ---

func TestUpload_WorkspaceCRDGone_404_NoAgentdCall(t *testing.T) {
	env, _ := newUploadEnvWithFakeAgentd(t, http.StatusCreated, "")
	env.setupPassword(t, "test-password")
	env.wsMock.On("Get", mock.Anything, "ws-1", metav1.GetOptions{}).
		Return((*v1.Workspace)(nil), errors.New("workspaces.llmsafespaces.dev \"ws-1\" not found")).Maybe()

	body, ct := buildMultipart(t, uploadPartSpec{field: "file", filename: "x.txt", content: []byte("x")})
	w := doUpload(env, body, ct)

	assert.Equal(t, http.StatusNotFound, w.Code)
	assert.Equal(t, 0, env.transport.requestCount(), "no agentd call when the workspace CRD is gone")
}

// --- U1.2.16 / D16 / I15 gate order: phase before disk before cap ---

func TestUpload_GateOrder_PhaseBeforeDiskBeforeCap(t *testing.T) {
	content := make([]byte, 64*1024)

	t.Run("non-Active wins over disk and cap", func(t *testing.T) {
		resetUploadMetrics(t)
		env, _ := newUploadEnvWithFakeAgentd(t, http.StatusCreated, "")
		env.handler.SetUploadLimitsForTest(1024, 0)
		env.setupPassword(t, "test-password")
		env.setupWorkspace(t, makeUploadWorkspaceCRD("ws-1", "10.0.0.1", "Suspended", 99, 100))

		body, ct := buildMultipart(t, uploadPartSpec{field: "file", filename: "x.bin", content: content})
		w := doUpload(env, body, ct)

		assert.Equal(t, http.StatusConflict, w.Code)
		assert.Contains(t, w.Body.String(), `"phase":"Suspended"`)
		assert.Equal(t, 1.0, uploadMetricValue(t, "phase"))
		assert.Equal(t, 0.0, uploadMetricValue(t, "disk"))
		assert.Equal(t, 0.0, uploadMetricValue(t, "cap"))
	})

	t.Run("disk wins over cap", func(t *testing.T) {
		resetUploadMetrics(t)
		env, _ := newUploadEnvWithFakeAgentd(t, http.StatusCreated, "")
		env.handler.SetUploadLimitsForTest(1024, 0)
		env.setupPassword(t, "test-password")
		env.setupWorkspace(t, makeUploadWorkspaceCRD("ws-1", "10.0.0.1", "Active", 99, 100))

		body, ct := buildMultipart(t, uploadPartSpec{field: "file", filename: "x.bin", content: content})
		w := doUpload(env, body, ct)

		assert.Equal(t, http.StatusInsufficientStorage, w.Code)
		assert.Equal(t, 1.0, uploadMetricValue(t, "disk"))
		assert.Equal(t, 0.0, uploadMetricValue(t, "cap"))
	})

	t.Run("cap reported when phase and disk pass", func(t *testing.T) {
		resetUploadMetrics(t)
		env, _ := newUploadEnvWithFakeAgentd(t, http.StatusCreated, "")
		env.handler.SetUploadLimitsForTest(1024, 0)
		env.setupPassword(t, "test-password")
		env.setupWorkspace(t, activeUploadWS())

		body, ct := buildMultipart(t, uploadPartSpec{field: "file", filename: "x.bin", content: content})
		w := doUpload(env, body, ct)

		assert.Equal(t, http.StatusRequestEntityTooLarge, w.Code)
		assert.Equal(t, 1.0, uploadMetricValue(t, "cap"))
	})
}

// --- U1.2.21 success metric ---

func TestUpload_Metrics_SuccessCounted(t *testing.T) {
	resetUploadMetrics(t)
	env, _ := newUploadEnvWithFakeAgentd(t, http.StatusCreated, "")
	env.setupPassword(t, "test-password")
	env.setupWorkspace(t, activeUploadWS())

	body, ct := buildMultipart(t, uploadPartSpec{field: "file", filename: "ok.txt", content: []byte("x")})
	w := doUpload(env, body, ct)
	require.Equal(t, http.StatusCreated, w.Code)

	assert.Equal(t, 1.0, uploadMetricValue(t, "success"))
}

// --- I5 agentd down (pod restarting), phase still Active → 502, no panic ---

func TestUpload_AgentdDown_502(t *testing.T) {
	resetUploadMetrics(t)
	env, _ := newUploadEnvWithFakeAgentd(t, http.StatusCreated, "")
	env.handler.httpClient = &http.Client{Transport: &alwaysFailTransport{}, Timeout: 2 * time.Second}
	env.setupPassword(t, "test-password")
	env.setupWorkspace(t, activeUploadWS())

	body, ct := buildMultipart(t, uploadPartSpec{field: "file", filename: "x.txt", content: []byte("x")})
	w := doUpload(env, body, ct)

	assert.Equal(t, http.StatusBadGateway, w.Code)
	assert.Equal(t, 1.0, uploadMetricValue(t, "agentd_error"))
}

// --- I9 phase-transition race: agentd closes mid-upload → clean failure ---

func TestUpload_AgentdClosesMidStream_CleanFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, io.LimitReader(r.Body, 1))
		panic(http.ErrAbortHandler)
	}))
	t.Cleanup(srv.Close)
	env := newUploadEnv(t, &uploadCaptureTransport{server: srv})
	env.setupPassword(t, "test-password")
	env.setupWorkspace(t, activeUploadWS())

	content := make([]byte, 256*1024)
	body, ct := buildMultipart(t, uploadPartSpec{field: "file", filename: "big.bin", content: content})
	w := doUpload(env, opaqueReader{body}, ct)

	assert.Equal(t, http.StatusBadGateway, w.Code, "body: %s", w.Body.String())
	assert.NotContains(t, w.Body.String(), "EOF", "no transport internals in the client response")
}
