// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package middleware

import (
	"time"

	"github.com/gin-gonic/gin"

	"github.com/lenaxia/llmsafespaces/api/internal/interfaces"
	"github.com/lenaxia/llmsafespaces/pkg/types"
)

type MeteringMiddleware struct {
	meteringSvc interfaces.MeteringRecorder
}

func NewMeteringMiddleware(svc interfaces.MeteringRecorder) *MeteringMiddleware {
	return &MeteringMiddleware{meteringSvc: svc}
}

func (m *MeteringMiddleware) Handler() gin.HandlerFunc {
	return func(c *gin.Context) {
		c.Next()

		userID := c.GetString("userID")
		if userID == "" {
			return
		}

		path := c.Request.URL.Path
		for _, skip := range skippedPaths {
			if path == skip {
				return
			}
		}

		method := c.Request.Method
		subtype := "read"
		if method == "POST" || method == "PUT" || method == "DELETE" || method == "PATCH" {
			subtype = "write"
		}

		m.meteringSvc.Record(types.UsageEvent{
			Owner:        types.BillingOwner{ID: userID, Type: types.OwnerTypeUser},
			ActorID:      userID,
			EventType:    "api_call",
			EventSubtype: subtype,
			Quantity:     1,
			Source:       "api",
			EventTime:    time.Now(),
		})
	}
}

var skippedPaths = []string{"/health", "/livez", "/readyz", "/metrics"}
