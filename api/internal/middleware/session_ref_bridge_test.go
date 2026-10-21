// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package middleware_test

// session_ref_bridge_test.go — the #1816 resolved-id bridge's CONTINUITY
// pin (review r1 finding 1): the REAL SessionRefResolution middleware
// and the REAL platform MCP client (pkg/mcp HTTPClient.SendMessage)
// joined end-to-end over HTTP — the production middleware resolves the
// phrase, the production header constant carries the canonical id, and
// the production client matches the contract-stream frames on it. A
// rename or semantic drift on EITHER side redds this test, not silent
// phrase-send breakage.

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lenaxia/llmsafespaces/api/internal/middleware"
	"github.com/lenaxia/llmsafespaces/api/internal/services/sessionindex"
	llmmcp "github.com/lenaxia/llmsafespaces/pkg/mcp"

	"github.com/lenaxia/llmsafespaces/pkg/types"
)

// bridgeResolver satisfies sessionindex.RefResolver for one phrase→id
// pair (the middleware's resolution path — precedence 1, exact short
// id, case-insensitive).
type bridgeResolver struct {
	phrase, canonical string
}

func (b *bridgeResolver) FindSessionByShortID(_ context.Context, _, shortID string) (string, error) {
	if strings.EqualFold(shortID, b.phrase) {
		return b.canonical, nil
	}
	return "", nil
}

func (b *bridgeResolver) FindSessionsByAlias(_ context.Context, _, _ string) ([]types.SessionListItem, error) {
	return nil, nil
}

func (b *bridgeResolver) ListSessionIndex(_ context.Context, _ string) ([]types.SessionListItem, error) {
	return nil, nil
}

func (b *bridgeResolver) ListWorkspaceIDsByOwner(_ context.Context, _ string) ([]string, error) {
	return nil, nil
}

func (b *bridgeResolver) FindRefCandidatesAcrossWorkspaces(_ context.Context, _ []string, _ string) ([]types.SessionListItem, error) {
	return nil, nil
}

var _ sessionindex.RefResolver = (*bridgeResolver)(nil)

// TestSessionRefBridge_MiddlewareToMCPClient runs the production
// middleware and the production MCP client over one httptest server:
// a phrase-addressed session_message completes from LIVE stream frames
// (frames keyed by the canonical harness id), proving header constant,
// resolution, and client consumption as one seam.
func TestSessionRefBridge_MiddlewareToMCPClient(t *testing.T) {
	gin.SetMode(gin.TestMode)
	const (
		phrase    = "amber-falcon-glides"
		canonical = "ses_bridge_canonical_1"
	)

	seenPromptParam := make(chan string, 1)
	router := gin.New()
	group := router.Group("/api/v1/workspaces/:id", middleware.SessionRefResolution(&bridgeResolver{phrase: phrase, canonical: canonical}))
	group.POST("/sessions/:sessionId/prompt", func(c *gin.Context) {
		seenPromptParam <- c.Param("sessionId")
		c.Status(http.StatusAccepted)
	})
	router.GET("/api/v1/workspaces/:id/contract-events", func(c *gin.Context) {
		c.Header("Content-Type", "text/event-stream")
		flusher := c.Writer.(http.Flusher)
		snap := fmt.Sprintf(`{"snapshot":{"atSeq":1,"snapshot":{"sessions":[{"sessionId":%q,"status":"SESSION_STATUS_BUSY"}]}}}`, canonical)
		fmt.Fprintf(c.Writer, "data: %s\n\n", snap)
		flusher.Flush()
		delta := fmt.Sprintf(`{"event":{"seq":2,"event":{"type":"EVENT_TYPE_PART_DELTA","sessionId":%q,"delta":"live"}}}`, canonical)
		fmt.Fprintf(c.Writer, "data: %s\n\n", delta)
		flusher.Flush()
		idle := fmt.Sprintf(`{"event":{"seq":3,"event":{"type":"EVENT_TYPE_SESSION_STATUS","sessionId":%q,"status":"SESSION_STATUS_IDLE"}}}`, canonical)
		fmt.Fprintf(c.Writer, "data: %s\n\n", idle)
		flusher.Flush()
		<-c.Request.Context().Done()
	})
	ts := httptest.NewServer(router)
	defer ts.Close()

	client := &llmmcp.HTTPClient{BaseURL: ts.URL, HTTPClient: ts.Client()}

	start := time.Now()
	resp, err := client.SendMessage(context.Background(), "ws-1", phrase, "hi", 5*time.Second)
	require.NoError(t, err)
	assert.Equal(t, "live", resp, "the response must come from live frames keyed by the canonical id")
	assert.Less(t, time.Since(start), 3*time.Second,
		"the bridge must complete the wait from live frames, not the timeout+fallback")

	select {
	case got := <-seenPromptParam:
		assert.Equal(t, canonical, got,
			"the middleware must rewrite the phrase to the canonical id for the route handler")
	default:
		t.Fatal("the prompt route never fired")
	}
}

// TestSessionRefBridge_HeaderConstantIsTheJoinedSeam pins that the
// middleware emits EXACTLY the constant the client reads — the string
// is defined once (pkg/types) and consumed by both sides.
func TestSessionRefBridge_HeaderConstantIsTheJoinedSeam(t *testing.T) {
	assert.Equal(t, "X-Session-Ref-Resolved", types.HeaderSessionRefResolved)
}

// TestSessionRefBridge_PhraseKeyedFrames (r2 missing-test 2): the same
// continuous middleware→client seam under the post-#1824 REFRESHED-POD
// wire shape — frames carry the PHRASE as session_id with the harness
// id additive (internal_session_id). The real client must complete the
// wait from live frames keyed by EITHER space.
func TestSessionRefBridge_PhraseKeyedFrames(t *testing.T) {
	gin.SetMode(gin.TestMode)
	const (
		phrase    = "amber-falcon-glides"
		canonical = "ses_bridge_canonical_2"
	)

	seenPromptParam := make(chan string, 1)
	router := gin.New()
	group := router.Group("/api/v1/workspaces/:id", middleware.SessionRefResolution(&bridgeResolver{phrase: phrase, canonical: canonical}))
	group.POST("/sessions/:sessionId/prompt", func(c *gin.Context) {
		seenPromptParam <- c.Param("sessionId")
		c.Status(http.StatusAccepted)
	})
	router.GET("/api/v1/workspaces/:id/contract-events", func(c *gin.Context) {
		c.Header("Content-Type", "text/event-stream")
		flusher := c.Writer.(http.Flusher)
		snap := fmt.Sprintf(`{"snapshot":{"atSeq":1,"snapshot":{"sessions":[{"sessionId":%q,"internalSessionId":%q,"status":"SESSION_STATUS_BUSY"}]}}}`, phrase, canonical)
		fmt.Fprintf(c.Writer, "data: %s\n\n", snap)
		flusher.Flush()
		delta := fmt.Sprintf(`{"event":{"seq":2,"event":{"type":"EVENT_TYPE_PART_DELTA","sessionId":%q,"internalSessionId":%q,"delta":"live-phrase"}}}`, phrase, canonical)
		fmt.Fprintf(c.Writer, "data: %s\n\n", delta)
		flusher.Flush()
		idle := fmt.Sprintf(`{"event":{"seq":3,"event":{"type":"EVENT_TYPE_SESSION_STATUS","sessionId":%q,"internalSessionId":%q,"status":"SESSION_STATUS_IDLE"}}}`, phrase, canonical)
		fmt.Fprintf(c.Writer, "data: %s\n\n", idle)
		flusher.Flush()
		<-c.Request.Context().Done()
	})
	ts := httptest.NewServer(router)
	defer ts.Close()

	client := &llmmcp.HTTPClient{BaseURL: ts.URL, HTTPClient: ts.Client()}

	start := time.Now()
	resp, err := client.SendMessage(context.Background(), "ws-1", phrase, "hi", 5*time.Second)
	require.NoError(t, err)
	assert.Equal(t, "live-phrase", resp, "phrase-keyed frames complete the wait via the caller ref")
	assert.Less(t, time.Since(start), 3*time.Second)

	select {
	case got := <-seenPromptParam:
		assert.Equal(t, canonical, got, "the middleware still rewrites the route param to the canonical id")
	default:
		t.Fatal("the prompt route never fired")
	}
}
