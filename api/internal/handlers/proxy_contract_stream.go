// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package handlers

import (
	"context"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"google.golang.org/protobuf/encoding/protojson"

	"github.com/lenaxia/llmsafespaces/api/internal/services/contractstream"
	abiv1 "github.com/lenaxia/llmsafespaces/pkg/abi/v1"
)

// proxy_contract_stream.go — US-69.10 (design 0055 S3/D1-B): the
// API-proxied contract stream. Browsers subscribe per workspace; the
// manager holds ONE pod Events connection while ≥1 subscriber is attached
// and forwards raw StreamFrames — the browser runs the stamped-snapshot
// client rule itself (snapshot@S replaces state, seq ≤ S discarded).
// Snapshot-first is the pod's protocol; a reseed upstream reconnects and
// re-snapshots the SAME subscribers. Behind AGENTD_STATE_AUTHORITY (D4).

const contractStreamHeartbeat = 25 * time.Second

// contractStreamManager lazily builds the process-wide manager (the
// resolve seam re-resolves the pod per (re)connect — resume-safe, A7).
var (
	contractStreamOnce   sync.Once
	contractStreamMgrPtr *contractstream.Manager
)

func (h *ProxyHandler) contractStreamManager() *contractstream.Manager {
	contractStreamOnce.Do(func() {
		contractStreamMgrPtr = h.buildContractStreamManager()
	})
	return contractStreamMgrPtr
}

// buildContractStreamManager is the production wiring (split out for the
// test seam: SetContractStreamManagerForTest injects a fake).
func (h *ProxyHandler) buildContractStreamManager() *contractstream.Manager {
	return contractstream.NewManager(
		func(ctx context.Context, workspaceID string) (string, string, error) {
			return h.agentdEndpoint(ctx, workspaceID)
		},
		h.logger,
		contractstream.ConnectStream,
	)
}

// SetContractStreamManagerForTest swaps the process manager (handler
// tests drive a fake frame source).
func (h *ProxyHandler) SetContractStreamManagerForTest(m *contractstream.Manager) {
	contractStreamOnce.Do(func() { contractStreamMgrPtr = m })
}

// ContractEvents serves GET /workspaces/:id/contract-events (SSE): raw
// ABI StreamFrames as protojson `data:` lines. Reconnecting delivers a
// fresh stamped snapshot (the client rule's resync).
func (h *ProxyHandler) ContractEvents(c *gin.Context) {
	workspaceID := c.Param("id")

	if !h.agentdTerminus {
		c.JSON(http.StatusNotImplemented, gin.H{"error": gin.H{
			"code":       "not_supported",
			"capability": "abi.contract_stream",
			"detail":     "the contract stream requires AGENTD_STATE_AUTHORITY (design 0055 M4/D4)",
		}})
		return
	}

	frames, unsub, err := h.contractStreamManager().Subscribe(c.Request.Context(), workspaceID)
	if err != nil {
		c.JSON(http.StatusConflict, gin.H{"error": gin.H{"code": "unresolved", "detail": err.Error()}})
		return
	}
	defer unsub()

	c.Header("Content-Type", "text/event-stream")
	c.Header("Cache-Control", "no-cache")
	c.Header("Connection", "keep-alive")
	c.Header("X-Accel-Buffering", "no")
	c.Writer.WriteHeader(http.StatusOK)
	c.Writer.Flush()

	rc := http.NewResponseController(c.Writer)
	_ = rc.SetWriteDeadline(time.Now().Add(writeDeadlineWindow))

	heartbeat := time.NewTicker(contractStreamHeartbeat)
	defer heartbeat.Stop()

	marshal := protojson.MarshalOptions{UseProtoNames: false}
	for {
		select {
		case <-c.Request.Context().Done():
			return
		case <-heartbeat.C:
			if _, werr := fmt.Fprint(c.Writer, ":\n\n"); werr != nil {
				return
			}
			c.Writer.Flush()
			_ = rc.SetWriteDeadline(time.Now().Add(writeDeadlineWindow))
		case v, open := <-frames:
			if !open {
				return
			}
			var data []byte
			switch f := v.(type) {
			case contractstream.Resync:
				// Slow consumer or protocol break: one named event tells
				// the client to re-subscribe (fresh snapshot).
				if _, werr := fmt.Fprint(c.Writer, "event: resync\ndata: {}\n\n"); werr != nil {
					return
				}
				c.Writer.Flush()
				_ = rc.SetWriteDeadline(time.Now().Add(writeDeadlineWindow))
				continue
			case *abiv1.StreamFrame:
				data, err = marshal.Marshal(f)
				if err != nil {
					continue // one undecodable frame never kills the stream
				}
			default:
				continue
			}
			if _, werr := fmt.Fprintf(c.Writer, "data: %s\n\n", data); werr != nil {
				return
			}
			c.Writer.Flush()
			_ = rc.SetWriteDeadline(time.Now().Add(writeDeadlineWindow))
		}
	}
}
