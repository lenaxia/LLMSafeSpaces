// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package handlers

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// #1757 r1 pins: the phrase form of targetWorkspaceId. The r0 review
// caught this path shipping DEAD — the #1729 gate resolved the raw
// phrase (404 for every valid target) and the resolved UUID was
// clobbered back to the phrase before persistence. These pins hold
// the fixed contract: resolve FIRST, gate on the RESOLVED UUID,
// persist the UUID, mask a foreign phrase as the not-found 404, and
// answer a degraded lookup with 503 (never a confident miss).

// fakePhraseResolver is the phrase-lookup stand-in: phrase → UUID
// ("" = miss), with optional fault injection.
type fakePhraseResolver struct {
	metas map[string]string
	err   error
}

func (f *fakePhraseResolver) FindWorkspaceIDByPhrase(_ context.Context, phrase string) (string, error) {
	if f.err != nil {
		return "", f.err
	}
	return f.metas[phrase], nil
}

// newPhraseTargetRouter wires the user REST trigger route with the
// #1729 authorizer AND the #1757 phrase resolver (mirrors
// newRESTTriggerRouter; the acting user is user-7).
func newPhraseTargetRouter(t *testing.T, resolver workspacePhraseResolver) (*gin.Engine, *mockTriggerStore) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	store := newMockTriggerStore()
	h := NewUserTriggersHandler(store, nil, &mockEncryptor{})
	h.SetWorkspaceOwnershipAuthorizer(crossWSAuthorizer())
	h.SetWorkspacePhraseResolver(resolver)
	r := gin.New()
	g := r.Group("/api/v1/me/triggers")
	g.Use(func(c *gin.Context) { c.Set("userID", "user-7"); c.Next() })
	g.POST("", h.UserCreate)
	return r, store
}

const phraseCreateBody = `{
	"name":"phrase-target-routine",
	"sourceType":"cron",
	"sourceConfig":{"expr":"5 * * * *"},
	"prompt":"run in the target workspace"
}`

func phraseBody(target string) string {
	return `{"targetWorkspaceId":"` + target + `",` + phraseCreateBody[1:]
}

// The happy path, pinned end to end: the phrase resolves to the UUID,
// the #1729 gate resolves the UUID (never the raw phrase), and the
// STORED key is the UUID — the phrase must never persist.
func TestTriggerCreate_PhraseTarget_ResolvesGatesAndPersistsUUID(t *testing.T) {
	res := &fakePhraseResolver{metas: map[string]string{"amber-river-7": "ws-2"}}
	r, store := newPhraseTargetRouter(t, res)

	w := doREST(t, r, "POST", "/api/v1/me/triggers",
		phraseBody("amber-river-7"))
	require.Equal(t, http.StatusCreated, w.Code, "body: %s", w.Body.String())

	require.Len(t, store.triggers, 1)
	for _, row := range store.triggers {
		require.NotNil(t, row.WorkspaceID, "resolved target must persist")
		assert.Equal(t, "ws-2", *row.WorkspaceID,
			"the stored key is the RESOLVED UUID, never the phrase")
	}
}

// The dead-path pin from the other side: a phrase the resolver maps
// to a workspace that is NOT in the authorizer's index cannot 404 by
// resolving the raw phrase — the gate sees the RESOLVED UUID's
// verdict. Here: phrase resolves to ws-evil (real, foreign) — the
// masked 404 below covers it; this case pins that a phrase mapping to
// an OWNED workspace succeeds even when the phrase string itself is
// unknown to the authorizer (the gate never sees the phrase).
func TestTriggerCreate_PhraseTarget_GateNeverResolvesRawPhrase(t *testing.T) {
	res := &fakePhraseResolver{metas: map[string]string{"opal-meadow-9": "ws-orgown"}}
	r, store := newPhraseTargetRouter(t, res)

	w := doREST(t, r, "POST", "/api/v1/me/triggers",
		phraseBody("opal-meadow-9"))
	require.Equal(t, http.StatusCreated, w.Code, "body: %s", w.Body.String())
	for _, row := range store.triggers {
		require.NotNil(t, row.WorkspaceID)
		assert.Equal(t, "ws-orgown", *row.WorkspaceID)
	}
}

// §4.3 masking on the trigger surface: a phrase that resolves to a
// REAL but FOREIGN workspace answers the byte-identical 404 the
// unknown phrase gets — never a distinguishable 403.
func TestTriggerCreate_PhraseTarget_ForeignPhraseMasksAs404(t *testing.T) {
	res := &fakePhraseResolver{metas: map[string]string{"amber-river-7": "ws-evil"}}
	r, store := newPhraseTargetRouter(t, res)

	w := doREST(t, r, "POST", "/api/v1/me/triggers",
		phraseBody("amber-river-7"))
	assert.Equal(t, http.StatusNotFound, w.Code,
		"foreign phrase must answer the masked 404, not 403: %s", w.Body.String())
	assert.Contains(t, w.Body.String(), "target workspace not found",
		"byte-identical to the unknown-phrase body")
	assert.Empty(t, store.triggers, "rejected create must not persist")
}

func TestTriggerCreate_PhraseTarget_UnknownPhraseIs404(t *testing.T) {
	res := &fakePhraseResolver{metas: map[string]string{}}
	r, store := newPhraseTargetRouter(t, res)

	w := doREST(t, r, "POST", "/api/v1/me/triggers",
		phraseBody("opal-meadow-9"))
	assert.Equal(t, http.StatusNotFound, w.Code, "body: %s", w.Body.String())
	assert.Contains(t, w.Body.String(), "target workspace not found")
	assert.Empty(t, store.triggers)
}

// 5xx-never-suggests: a degraded phrase lookup answers the
// availability 503 — an infra fault is never a confident miss.
func TestTriggerCreate_PhraseTarget_ResolverErrorIs503(t *testing.T) {
	res := &fakePhraseResolver{err: errors.New("db unavailable")}
	r, store := newPhraseTargetRouter(t, res)

	w := doREST(t, r, "POST", "/api/v1/me/triggers",
		phraseBody("amber-river-7"))
	assert.Equal(t, http.StatusServiceUnavailable, w.Code,
		"degraded lookup must 503, not 404: %s", w.Body.String())
	assert.Empty(t, store.triggers)
}

// Unwired resolver (deferred injection not configured): fail closed
// with the same 404 as an unknown phrase — never a fallthrough that
// could persist the raw phrase.
func TestTriggerCreate_PhraseTarget_NilResolverFailsClosed(t *testing.T) {
	r, store := newPhraseTargetRouter(t, nil)

	w := doREST(t, r, "POST", "/api/v1/me/triggers",
		phraseBody("amber-river-7"))
	assert.Equal(t, http.StatusNotFound, w.Code, "body: %s", w.Body.String())
	assert.Empty(t, store.triggers)
}

// The UUID form is untouched by the phrase layer: no resolver probe,
// the gate and persistence see the raw UUID (regression guard for
// the r1 rewrite).
func TestTriggerCreate_UUIDTarget_SkipsPhraseLayer(t *testing.T) {
	res := &fakePhraseResolver{metas: map[string]string{}}
	r, store := newPhraseTargetRouter(t, res)

	w := doREST(t, r, "POST", "/api/v1/me/triggers",
		phraseBody("ws-2"))
	require.Equal(t, http.StatusCreated, w.Code, "body: %s", w.Body.String())
	for _, row := range store.triggers {
		require.NotNil(t, row.WorkspaceID)
		assert.Equal(t, "ws-2", *row.WorkspaceID)
	}
}
