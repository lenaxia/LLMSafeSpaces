// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package middleware

import (
	"fmt"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/lenaxia/llmsafespaces/api/internal/services/sessionindex"
	"github.com/lenaxia/llmsafespaces/pkg/sessionref"
	"github.com/lenaxia/llmsafespaces/pkg/types"
)

// SessionRefResolution is design 0066 §4.4/§4.5 acceptance at the
// route edge: every id-taking route accepts the short id
// (case-insensitive), the legacy harness id (byte-identical
// passthrough — precedence 2 never rewrites, so full-ID addressing is
// unchanged), and the unambiguous alias (current title,
// workspace-scoped). Resolution failure on an ALIAS-form ref answers
// the structured 404 + did-you-mean payload immediately (§4.6); an
// id-form ref that matches nothing passes through untouched so the
// adapter stays the authority — index lag must never make a live
// session unaddressable (the #1663 lesson).
//
// A nil resolver disables the layer: every ref passes through
// byte-identical (the migration's degradation posture).
func SessionRefResolution(resolver sessionindex.RefResolver) gin.HandlerFunc {
	return func(c *gin.Context) {
		if resolver == nil {
			c.Next()
			return
		}
		ref := c.Param("sessionId")
		if ref == "" {
			c.Next()
			return
		}
		workspaceID := c.Param("id")
		if workspaceID == "" {
			workspaceID = c.Param("workspaceId") // the admin variant
		}
		res := sessionindex.ResolveSessionRef(c.Request.Context(), resolver, workspaceID, ref)
		switch {
		case res.MatchedBy != sessionindex.MatchPassthrough || res.CanonicalID != "":
			// Accepted (short/alias) or id-form passthrough: rewrite
			// the route param to the canonical harness id. For legacy
			// ids the rewrite is a byte-identical no-op.
			setParam(c, "sessionId", res.CanonicalID)
			c.Next()
		case res.AmbiguousAlias:
			c.AbortWithStatusJSON(http.StatusNotFound, gin.H{
				"error":       "session alias is ambiguous — " + pluralSessions(len(res.AliasMatches)) + " share this title; none was used (design 0066: ambiguity is never auto-resolved)",
				"suggestions": aliasSuggestions(workspaceID, res.AliasMatches),
			})
		default: // unresolved alias
			c.AbortWithStatusJSON(http.StatusNotFound, gin.H{
				"error":       "session not found",
				"suggestions": SuggestionsFor(c, resolver, workspaceID, ref),
			})
		}
	}
}

// SuggestionsFor computes the did-you-mean payload with the
// requester's identity (from WorkspaceAccessMiddleware's stored
// metadata) for the cross-workspace scope — nil-safe, best-effort.
func SuggestionsFor(c *gin.Context, resolver sessionindex.RefResolver, workspaceID, ref string) []any {
	if resolver == nil {
		return nil
	}
	userID := ""
	if meta, ok := c.Request.Context().Value(types.ContextKeyWorkspaceMeta).(*types.WorkspaceMetadata); ok {
		userID = meta.UserID
	}
	out := sessionindex.SuggestionsFor(c.Request.Context(), resolver, userID, workspaceID, ref)
	anyOut := make([]any, 0, len(out))
	for _, s := range out {
		anyOut = append(anyOut, s)
	}
	return anyOut
}

// aliasSuggestions renders an ambiguity's rows as the payload —
// ruling 8's exact row shape {title, shortId, sessionId, workspace,
// lastActive}, typed sessionref.Suggestion like every other payload
// path, newest first (FindSessionsByAlias's ORDER BY), capped at 3.
func aliasSuggestions(workspaceID string, rows []types.SessionListItem) []sessionref.Suggestion {
	if len(rows) > sessionref.MaxSuggestions {
		rows = rows[:sessionref.MaxSuggestions]
	}
	out := make([]sessionref.Suggestion, 0, len(rows))
	for _, row := range rows {
		s := sessionref.Suggestion{
			Title:     row.Title,
			ShortID:   row.ShortID,
			SessionID: row.ID,
			Workspace: workspaceID,
		}
		if t := lastActiveOfRow(row); !t.IsZero() {
			utc := t.UTC()
			s.LastActive = &utc
		}
		out = append(out, s)
	}
	return out
}

// lastActiveOfRow picks a row's freshest activity timestamp — the
// same semantics the ranked payload paths use (review r1: the
// ambiguity payload's lastActive must not diverge from every other
// surface's).
func lastActiveOfRow(row types.SessionListItem) time.Time {
	switch {
	case row.LastMessageAt != nil && row.LastSeenAt != nil:
		if row.LastMessageAt.After(*row.LastSeenAt) {
			return *row.LastMessageAt
		}
		return *row.LastSeenAt
	case row.LastMessageAt != nil:
		return *row.LastMessageAt
	case row.LastSeenAt != nil:
		return *row.LastSeenAt
	default:
		return time.Time{}
	}
}

func pluralSessions(n int) string {
	return fmt.Sprintf("%d session%s", n, map[bool]string{true: "", false: "s"}[n == 1])
}

// setParam rewrites one route parameter value in place. c.Param reads
// c.Params on every call, so handlers after this middleware observe
// the canonical value.
func setParam(c *gin.Context, key, value string) {
	for i := range c.Params {
		if c.Params[i].Key == key {
			c.Params[i].Value = value
			return
		}
	}
}
