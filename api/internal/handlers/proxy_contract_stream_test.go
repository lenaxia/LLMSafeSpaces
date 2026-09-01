// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package handlers

import (
	"bufio"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	k8sfake "k8s.io/client-go/kubernetes/fake"

	k8smocks "github.com/lenaxia/llmsafespaces/mocks/kubernetes"

	"github.com/lenaxia/llmsafespaces/api/internal/services/contractstream"
	abiv1 "github.com/lenaxia/llmsafespaces/pkg/abi/v1"
)

// --- US-69.10: the API edge of the contract stream (flag gate + the SSE
// wire shape browsers will consume: protojson frames, snapshot-first). ---

// fakePodSource is the injectable pod Events stream.
type fakePodSource struct {
	frames chan *abiv1.StreamFrame
}

func (f *fakePodSource) Frames() <-chan *abiv1.StreamFrame { return f.frames }
func (f *fakePodSource) Err() error                        { return nil }

func TestContractEvents_FlagOffIsTyped501(t *testing.T) {
	gin.SetMode(gin.TestMode)
	k8sMock := k8smocks.NewMockKubernetesClient()
	llmMock := k8smocks.NewMockLLMSafespacesV1Interface()
	wsMock := k8smocks.NewMockWorkspaceInterface()
	k8sMock.On("LlmsafespacesV1").Return(llmMock, nil)
	llmMock.On("Workspaces", "default").Return(wsMock)
	k8sMock.On("Clientset").Return(k8sfake.NewSimpleClientset())
	handler, err := NewProxyHandler(k8sMock, &testLogger{}, "default", &http.Client{}, nil)
	require.NoError(t, err)
	// Flag off: the surface does not exist (D4).
	handler.SetAgentdTerminus(false)

	router := gin.New()
	router.GET("/api/v1/workspaces/:id/contract-events", handler.ContractEvents)
	srv := httptest.NewServer(router)
	t.Cleanup(srv.Close)

	res, err := http.Get(srv.URL + "/api/v1/workspaces/ws-1/contract-events")
	require.NoError(t, err)
	defer func() { _ = res.Body.Close() }()
	body := make([]byte, 512)
	n, _ := res.Body.Read(body)

	require.Equal(t, http.StatusNotImplemented, res.StatusCode)
	assert.Contains(t, string(body[:n]), "abi.contract_stream")
}

func TestContractEvents_SSEWireSnapshotFirst(t *testing.T) {
	gin.SetMode(gin.TestMode)

	k8sMock := k8smocks.NewMockKubernetesClient()
	llmMock := k8smocks.NewMockLLMSafespacesV1Interface()
	wsMock := k8smocks.NewMockWorkspaceInterface()
	k8sMock.On("LlmsafespacesV1").Return(llmMock, nil)
	llmMock.On("Workspaces", "default").Return(wsMock)
	k8sMock.On("Clientset").Return(k8sfake.NewSimpleClientset())
	handler, err := NewProxyHandler(k8sMock, &testLogger{}, "default", &http.Client{}, nil)
	require.NoError(t, err)
	handler.SetAgentdTerminus(true)

	src := &fakePodSource{frames: make(chan *abiv1.StreamFrame, 4)}
	handler.SetContractStreamManagerForTest(contractstream.NewManager(
		func(ctx context.Context, ws string) (string, string, error) { return "http://pod", "pw", nil },
		nil,
		func(ctx context.Context, base, pw string) (contractstream.FrameSource, error) { return src, nil },
	))

	router := gin.New()
	router.GET("/api/v1/workspaces/:id/contract-events", handler.ContractEvents)
	srv := httptest.NewServer(router)
	t.Cleanup(srv.Close)

	ctx, cancel := context.WithCancel(context.Background())
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+"/api/v1/workspaces/ws-1/contract-events", nil)
	require.NoError(t, err)

	lines := make(chan string, 64)
	go func() {
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return
		}
		defer func() { _ = resp.Body.Close() }()
		assert.Equal(t, "text/event-stream", resp.Header.Get("Content-Type"))
		sc := bufio.NewScanner(resp.Body)
		for sc.Scan() {
			select {
			case lines <- sc.Text():
			case <-ctx.Done():
				return
			}
		}
	}()

	// The pod delivers snapshot → event, as its protocol guarantees.
	src.frames <- &abiv1.StreamFrame{Frame: &abiv1.StreamFrame_Snapshot{Snapshot: &abiv1.SnapshotFrame{AtSeq: 7}}}
	src.frames <- &abiv1.StreamFrame{Frame: &abiv1.StreamFrame_Event{Event: &abiv1.SequencedEvent{Seq: 8}}}

	first := nextDataLine(t, lines)
	require.Contains(t, first, `"snapshot"`, "first delivered frame is the snapshot")
	require.Contains(t, first, `"atSeq":"7"`, "protojson (camelCase) — matches the generated TS types")

	second := nextDataLine(t, lines)
	require.Contains(t, second, `"event"`)
	require.Contains(t, second, `"seq":"8"`, "events follow with their seq (the client discard rule's input)")

	cancel()
}

// nextDataLine skips blank/heartbeat lines, returning the next data: line.
func nextDataLine(t *testing.T, lines <-chan string) string {
	t.Helper()
	deadline := time.After(3 * time.Second)
	for {
		select {
		case l := <-lines:
			if strings.HasPrefix(l, "data:") {
				return l
			}
		case <-deadline:
			t.Fatal("no data line within deadline")
		}
	}
}
