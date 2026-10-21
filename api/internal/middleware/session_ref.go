// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package middleware

import (
	"context"
	"fmt"
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/lenaxia/llmsafespaces/api/internal/services/sessionindex"
	"github.com/lenaxia/llmsafespaces/pkg/sessionref"
	"github.com/lenaxia/llmsafespaces/pkg/shortid"
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
		case res.DegradedLookup && !shortid.IsSeamIDForm(ref):
			// The alias lookup errored AND the ref could never be a
			// seam id (a title/slug). Passing it downstream would let
			// the write paths register a ghost row keyed by title text
			// (review r2 finding B); claiming a miss would violate
			// 5xx-never-suggests. Answer the honest availability 503.
			c.AbortWithStatusJSON(http.StatusServiceUnavailable, gin.H{
				"error": "session resolution temporarily unavailable — retry",
			})
		case res.MatchedBy != sessionindex.MatchPassthrough || res.CanonicalID != "":
			// Accepted (phrase/alias) or deprecated-id passthrough:
			// rewrite the route param to the canonical harness id
			// (internal key), and attach the #1756 one-release input
			// deprecation notice on the RESPONSE when the caller
			// addressed by a deprecated form (full id or #1745 hash).
			if res.DeprecatedInput {
				c.Header("Deprecation", "true")
				// RFC 8594: Sunset is an HTTP-date, not free text.
				// The deprecation window is one release (#1756 §2):
				// deprecated in v0.40.0, removed in v0.41.0. The
				// "address by the 3-word phrase" guidance lives in
				// the agentd deprecation notes and the CHANGELOG.
				c.Header("Sunset", "Tue, 01 Dec 2026 00:00:00 GMT")
			}
			// #1816 resolved-id bridge: tell the caller which
			// canonical id the ref mapped to. The platform MCP
			// client (pkg/mcp) consumes this to match the ABI
			// contract stream — whose frames still carry harness
			// ids until the deferred short_id M2 cutover — instead
			// of burning the whole wait budget on a phrase-typed
			// filter that never matches. Correlation for a caller
			// that just addressed the session under authz, on the
			// response it already receives — §1 governs
			// session-bearing payload BODIES; this retires with the
			// ABI short_id M2 cutover (phrase-native frames). Only
			// a true RESOLUTION (phrase/alias) reports: a
			// passthrough caller already holds the canonical id.
			if res.MatchedBy != sessionindex.MatchPassthrough && res.CanonicalID != "" {
				c.Header("X-Session-Ref-Resolved", res.CanonicalID)
			}
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
			Title:   row.Title,
			ShortID: row.ShortID,
			// InternalID: SessionID is ranking/clause internals only
			// (json:"-") — carry the canonical harness key, not the
			// wire phrase, for parity with the resolver's candidates.
			SessionID: row.InternalID,
			Workspace: workspaceID,
		}
		if t := sessionindex.LastActiveOf(row); !t.IsZero() {
			utc := t.UTC()
			s.LastActive = &utc
		}
		out = append(out, s)
	}
	return out
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

// WorkspaceRefResolver resolves #1757 workspace refs and is satisfied
// by the *database.Service. WorkspaceRefResolution is the #1757
// workspace-ref edge: the workspace :id param accepts the workspace
// PHRASE as the canonical address; the UUID passes through
// byte-identical with the deprecation notice attached; a NAME
// resolves only when it is the requester's single workspace by that
// name (names are caller-owned and may collide — ambiguity never
// resolves here; an ambiguous or absent name passes through
// UNREWRITTEN so the access gate that follows answers with its
// ordinary 404 — the workspace surface carries no did-you-mean
// machinery). Phrase-form resolution failure answers the structured
// 404; a lookup ERROR answers 503 (a degraded lookup is
// availability, never a confident miss — §4.4/§4.6 5xx-never-
// suggests discipline, session-lane precedent). When a phrase
// resolves, the §4.3 anti-enumeration masking marker is set: a
// downstream ownership denial on a phrase-addressed workspace is
// answered by the access middleware with the byte-identical 404 the
// unknown phrase gets.
//
// Mounted ONLY on the workspace :id group (router idGroup); the
// admin :workspaceId routes are NOT phrase-wired (deferred — #1792
// tracks the completion: admin-group phrase addressing).
type WorkspaceRefResolver interface {
	FindWorkspaceIDByPhrase(ctx context.Context, phrase string) (string, error)
	FindWorkspaceIDByName(ctx context.Context, userID, name string) (string, error)
}

// ContextKeyWorkspaceRefPhrase is set (gin context) when the caller
// addressed the workspace by a PHRASE that resolved — the access
// middleware folds a Forbidden verdict into the byte-identical 404
// the unknown phrase gets (§4.3 anti-enumeration, #1757).
const ContextKeyWorkspaceRefPhrase = "workspaceRefPhrase"

// WorkspaceRefResolution rewrites the :id param when the caller used
// a phrase (or an unambiguous name). Nil resolver disables the layer.
func WorkspaceRefResolution(resolver WorkspaceRefResolver) gin.HandlerFunc {
	return func(c *gin.Context) {
		if resolver == nil {
			c.Next()
			return
		}
		ref := c.Param("id")
		if ref == "" {
			ref = c.Param("workspaceId")
		}
		if ref == "" {
			c.Next()
			return
		}
		switch {
		case shortid.IsWorkspacePhraseForm(ref):
			id, err := resolver.FindWorkspaceIDByPhrase(c.Request.Context(), ref)
			if err != nil {
				// Degraded lookup: 503, never a confident miss (a
				// transient DB outage must not answer "no workspace
				// holds the phrase").
				c.AbortWithStatusJSON(http.StatusServiceUnavailable, gin.H{
					"error": "workspace phrase lookup temporarily unavailable — retry",
				})
				return
			}
			if id != "" {
				setParam(c, "id", id)
				setParam(c, "workspaceId", id)
				// §4.3 masking marker: the access middleware folds a
				// Forbidden verdict on a phrase-addressed workspace
				// into the byte-identical 404 below.
				c.Set(ContextKeyWorkspaceRefPhrase, shortid.Normalize(ref))
				c.Next()
				return
			}
			// Phrase-shaped and unresolvable: a 404 with the honest
			// message (suggestions are session-scoped machinery; a
			// workspace miss says so plainly). The NORMALIZED ref is
			// echoed, never the raw input.
			c.AbortWithStatusJSON(http.StatusNotFound, gin.H{
				"error": "workspace not found: no workspace holds the phrase " + shortid.Normalize(ref),
			})
		default:
			// UUID or name: names resolve unambiguously (deprecated
			// path); UUIDs pass through byte-identical. Neither
			// probes the DB for shape validation — the access
			// middleware that follows is the authority.
			if looksLikeWorkspaceUUID(ref) {
				c.Next()
				return
			}
			// The workspace-access middleware runs AFTER this layer
			// (it needs the rewritten UUID), so the requester id comes
			// from the auth middleware's context value.
			userID := c.GetString("userID")
			if userID == "" {
				c.Next()
				return
			}
			id, err := resolver.FindWorkspaceIDByName(c.Request.Context(), userID, ref)
			if err != nil {
				// Same 5xx-never-suggests discipline as the phrase
				// arm: a degraded name lookup is availability.
				c.AbortWithStatusJSON(http.StatusServiceUnavailable, gin.H{
					"error": "workspace lookup temporarily unavailable — retry",
				})
				return
			}
			if id != "" {
				setParam(c, "id", id)
				setParam(c, "workspaceId", id)
				// The name form is in its #1757 deprecation window:
				// mirror the session lane's one-release notice on the
				// response when the rewrite fires — RFC 8594 shape
				// (Deprecation: true + HTTP-date Sunset), never free
				// text (#1756 §2 finding 1 parity).
				c.Header("Deprecation", "true")
				c.Header("Sunset", "Tue, 01 Dec 2026 00:00:00 GMT")
			}
			// Absent or ambiguous name: passthrough UNREWRITTEN — the
			// gate answers with its ordinary verdict (no suggestions
			// on the workspace surface).
			c.Next()
		}
	}
}

// looksLikeWorkspaceUUID is the cheap non-probing guard for the
// platform's workspace UUID shape — used only to skip the name lookup
// for obviously-UUID refs (no validation; the access middleware
// owns the verdict).
func looksLikeWorkspaceUUID(s string) bool {
	if len(s) != 36 {
		return false
	}
	for i, c := range s {
		switch i {
		case 8, 13, 18, 23:
			if c != '-' {
				return false
			}
		default:
			isHex := (c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')
			if !isHex {
				return false
			}
		}
	}
	return true
}
