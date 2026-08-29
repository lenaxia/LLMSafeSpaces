// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package opencode

// Delivery verification for the outbox ambiguity resolver (#987).
//
// opencode persists the user message BEFORE the turn starts
// (v1.18.10 prompt.ts: createUserMessage runs before the run loop), so
// "a user message with this exact text exists in the transcript at or
// after the send-window start" proves delivery even while the turn is
// still running. Absence is only definitive when the paged scan covered
// the full window (a message older than the window start, or history
// exhausted): newest-N truncation alone proves nothing.
//
// This is opencode-specific knowledge (the persist-first contract) and
// therefore lives behind the adapter seam — platform code (the outbox)
// only sees delivered/definitive/err.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/url"
	"strconv"
	"time"
)

const (
	// verifyPageSize is the history page size (GET /session/:id/message
	// ?limit=N, newest N ascending).
	verifyPageSize = 500
	// verifyPageBudget bounds pages per verification pass. 40 pages ×
	// 500 = 20k messages — beyond that the scan reports inconclusive
	// (coverage incomplete) rather than guessing.
	verifyPageBudget = 40
	// verifyClockSkew absorbs API-server vs agent-pod clock drift when
	// comparing the message creation time to the send-window start.
	verifyClockSkew = 2 * time.Minute
)

// VerifyDelivery reports whether a user message whose text part equals
// text exactly was persisted in the session transcript at or after
// since (minus a clock-skew margin).
//
// delivered=true is definitive proof the message reached the agent.
// definitive=true with delivered=false proves the message is ABSENT
// (full window coverage) and a re-send cannot duplicate. definitive=
// false means coverage was incomplete — treat as inconclusive and
// recheck later, never re-send. err is a transport failure (also
// inconclusive).
func (a *Adapter) VerifyDelivery(ctx context.Context, userID, workspaceID, sessionID, text string, since time.Time) (delivered, definitive bool, err error) {
	c, err := a.resolve(ctx, userID, workspaceID)
	if err != nil {
		return false, false, err
	}
	if a.v2Store {
		return a.verifyDeliveryV2(ctx, c, sessionID, text, since)
	}
	floor := since.Add(-verifyClockSkew).UnixMilli()
	cursor := ""
	for page := 0; page < verifyPageBudget; page++ {
		path := "/session/" + sessionID + "/message?limit=" + strconv.Itoa(verifyPageSize)
		if cursor != "" {
			path += "&before=" + url.QueryEscape(cursor)
		}
		resp, err := a.doGet(ctx, c, path)
		if err != nil {
			return false, false, fmt.Errorf("GET %s: %w", path, err)
		}
		if resp.StatusCode >= 400 {
			return false, false, a.httpError("GET "+path, resp)
		}
		var msgs []ocMessage
		if err := json.NewDecoder(io.LimitReader(resp.Body, 64<<20)).Decode(&msgs); err != nil {
			resp.Body.Close() //nolint:errcheck // best-effort drain
			return false, false, fmt.Errorf("GET %s: decode: %w", path, err)
		}
		resp.Body.Close() //nolint:errcheck // best-effort drain

		oldest := int64(math.MaxInt64)
		for _, m := range msgs {
			if m.Info.Time == nil {
				continue
			}
			created := m.Info.Time.Created
			if created < oldest {
				oldest = created
			}
			if created >= floor && m.Info.Role == "user" && userTextMatches(m, text) {
				return true, true, nil
			}
		}
		next := resp.Header.Get("X-Next-Cursor")
		if next == "" {
			return false, true, nil // history exhausted — absence is proven
		}
		if oldest != math.MaxInt64 && oldest < floor {
			return false, true, nil // scan passed the window start — absence is proven
		}
		cursor = next
	}
	return false, false, nil // page budget exhausted — coverage incomplete
}

// userTextMatches reports whether any text part of m equals text
// exactly. The platform sends prompts as a single text part; exact
// equality keeps the delivered-proof conservative.
func userTextMatches(m ocMessage, text string) bool {
	for _, p := range m.Parts {
		if p.Type == "text" && p.Text == text {
			return true
		}
	}
	return false
}

// verifyDeliveryV2 is the V2-store branch of VerifyDelivery (design
// 0052). The endpoint returns the full list (no paging on 1.18.15), so
// a single fetch is either definitive (match → delivered; list fully
// below the window start → absent) or inconclusive on transport error
// only — there is no partial-coverage window like the paged V1 scan.
func (a *Adapter) verifyDeliveryV2(ctx context.Context, c *Client, sessionID, text string, since time.Time) (bool, bool, error) {
	msgs, err := c.MessagesV2(ctx, sessionID)
	if err != nil {
		return false, false, err
	}
	floor := since.Add(-verifyClockSkew).UnixMilli()
	for _, m := range msgs {
		if m.Time.Created < floor {
			// Newest-first ordering: everything past this point predates
			// the send window — absence is proven.
			return false, true, nil
		}
		if m.Type == "user" && m.Text == text {
			return true, true, nil
		}
	}
	// List exhausted without crossing the floor: the window's messages
	// are all present and none matched — absence is proven.
	return false, true, nil
}
