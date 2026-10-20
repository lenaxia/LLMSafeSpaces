// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package app

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/lenaxia/llmsafespaces/api/internal/interfaces"
	"github.com/lenaxia/llmsafespaces/api/internal/middleware"
	imocks "github.com/lenaxia/llmsafespaces/api/internal/mocks"
	"github.com/lenaxia/llmsafespaces/api/internal/services/workspace"
	kmocks "github.com/lenaxia/llmsafespaces/mocks/kubernetes"
	lmocks "github.com/lenaxia/llmsafespaces/mocks/logger"
	v1 "github.com/lenaxia/llmsafespaces/pkg/apis/llmsafespaces/v1"
	"github.com/lenaxia/llmsafespaces/pkg/types"
)

// #1757 r1 e2e (minimal tier, the e2e_suspend fidelity: real workspace
// SERVICE + real ref/access middleware chain over recording stores —
// only the DB and k8s seams are faked): create mints and surfaces the
// phrase, the phrase ADDRESSES the workspace end-to-end, an unknown
// phrase answers the structured 404, and a foreign phrase is masked
// as the byte-identical 404 (§4.3).

// phraseE2EDB is the recording store: workspaces by id, phrase index,
// and the #1757 DatabaseService phrase seam (mint persists + read-back).
type phraseE2EDB struct {
	// Nil-interface embed: satisfies DatabaseService for the fixture;
	// any seam the flows actually exercise is implemented below, and
	// an unimplemented call panics loudly rather than passing silently.
	interfaces.DatabaseService
	mu    sync.Mutex
	rows  map[string]*types.WorkspaceMetadata
	byPhr map[string]string // phrase → id
}

func (d *phraseE2EDB) GetWorkspace(_ context.Context, id string) (*types.WorkspaceMetadata, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if m, ok := d.rows[id]; ok {
		c := *m
		return &c, nil
	}
	return nil, nil
}

func (d *phraseE2EDB) EnsureWorkspacePhraseRegistered(_ context.Context, id string) (string, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	m, ok := d.rows[id]
	if !ok {
		return "", errors.New("no such workspace")
	}
	if m.PhraseID != "" {
		return m.PhraseID, nil
	}
	m.PhraseID = "e2e-minted-7"
	d.byPhr[m.PhraseID] = id
	return m.PhraseID, nil
}

func (d *phraseE2EDB) FindWorkspaceIDByPhrase(_ context.Context, phrase string) (string, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if id, ok := d.byPhr[strings.ToLower(strings.TrimSpace(phrase))]; ok {
		return id, nil
	}
	return "", nil
}

func (d *phraseE2EDB) FindWorkspaceIDByName(_ context.Context, userID, name string) (string, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, m := range d.rows {
		if m.UserID == userID && strings.EqualFold(m.Name, name) {
			return m.ID, nil
		}
	}
	return "", nil
}

func (d *phraseE2EDB) put(m *types.WorkspaceMetadata) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.rows[m.ID] = m
	if m.PhraseID != "" {
		d.byPhr[m.PhraseID] = m.ID
	}
}

func newPhraseE2ERouter(t *testing.T) (*gin.Engine, *phraseE2EDB, *kmocks.MockWorkspaceInterface, *workspace.Service) {
	t.Helper()
	gin.SetMode(gin.TestMode)

	log := lmocks.NewMockLogger()
	log.On("Info", mock.Anything, mock.Anything).Maybe()
	log.On("Warn", mock.Anything, mock.Anything).Maybe()
	log.On("Error", mock.Anything, mock.Anything, mock.Anything).Maybe()

	k8s := kmocks.NewMockKubernetesClient()
	v1i := kmocks.NewMockLLMSafespacesV1Interface()
	wsI := kmocks.NewMockWorkspaceInterface()
	cache := &imocks.MockCacheService{}
	met := &imocks.MockMetricsService{}
	met.On("RecordRequest", mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything).Maybe()
	k8s.On("LlmsafespacesV1").Return(v1i, nil)
	v1i.On("Workspaces", "default").Return(wsI)

	db := &phraseE2EDB{rows: map[string]*types.WorkspaceMetadata{}, byPhr: map[string]string{}}
	svc, err := workspace.New(log, k8s, db, cache, met, &workspace.Config{Namespace: "default", OpencodePort: 4096})
	require.NoError(t, err)

	r := gin.New()
	g := r.Group("/api/v1/workspaces/:id", func(c *gin.Context) {
		c.Set("userID", "phrase-e2e-user")
		c.Next()
	})
	g.Use(middleware.WorkspaceRefResolution(db), middleware.WorkspaceAccessMiddleware(svc))
	// The REAL GET shape: the gated handler calls the user-scoped
	// service read (where the #1757 convergence lives) and returns it.
	g.GET("", func(c *gin.Context) {
		ws, err := svc.GetWorkspace(c.Request.Context(), "phrase-e2e-user", c.Param("id"))
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		c.JSON(http.StatusOK, ws)
	})
	return r, db, wsI, svc
}

func phraseCRD(id string) *v1.Workspace {
	return &v1.Workspace{
		ObjectMeta: metav1.ObjectMeta{Name: id, Namespace: "default"},
		Spec:       v1.WorkspaceSpec{Runtime: "python:3.11", Owner: v1.WorkspaceOwner{UserID: "phrase-e2e-user"}},
		Status:     v1.WorkspaceStatus{Phase: v1.WorkspacePhaseActive},
	}
}

// Happy path: creation-path mint surfaced on read; the phrase addresses
// the workspace through the REAL ref+access chain; the unknown phrase
// 404s; the foreign phrase is masked byte-identically (§4.3).
func TestE2E_WorkspacePhrase_AddressAndMasking(t *testing.T) {
	router, db, wsI, _ := newPhraseE2ERouter(t)

	// A converged own workspace + a foreign one holding a phrase.
	db.put(&types.WorkspaceMetadata{ID: "ws-own", UserID: "phrase-e2e-user", Name: "lab", PhraseID: "amber-river-7"})
	db.put(&types.WorkspaceMetadata{ID: "ws-foreign", UserID: "user-9", Name: "evil", PhraseID: "opal-meadow-9"})
	wsI.On("Get", mock.Anything, "ws-own", mock.Anything).Return(phraseCRD("ws-own"), nil).Maybe()

	// 1) The phrase resolves end-to-end: 200 with the canonical UUID.
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/workspaces/amber-river-7", nil))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var body struct {
		ID       string `json:"id"`
		PhraseID string `json:"phraseId"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	assert.Equal(t, "ws-own", body.ID, "the phrase must resolve to the canonical UUID")

	// 2) Unknown phrase: the structured 404.
	rec = httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/workspaces/linen-orchard-4", nil))
	require.Equal(t, http.StatusNotFound, rec.Code)
	assert.Contains(t, rec.Body.String(), "workspace not found")

	// 3) Foreign phrase: the §4.3 mask — byte-identical 404 shape, never
	// a distinguishable 403.
	rec = httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/workspaces/opal-meadow-9", nil))
	require.Equal(t, http.StatusNotFound, rec.Code,
		"foreign phrase must be masked as 404: %s", rec.Body.String())
	assert.Contains(t, rec.Body.String(), "no workspace holds the phrase opal-meadow-9")
}

// The convergence workflow: a LEGACY row (NULL phrase) is read by its
// owner through the full chain — the user-scoped read backfills
// POST-authorization and the phrase surfaces on the response.
func TestE2E_WorkspacePhrase_LegacyRowConvergesOnAuthorizedRead(t *testing.T) {
	router, db, wsI, _ := newPhraseE2ERouter(t)

	db.put(&types.WorkspaceMetadata{ID: "ws-legacy", UserID: "phrase-e2e-user", Name: "old"})
	wsI.On("Get", mock.Anything, "ws-legacy", mock.Anything).Return(phraseCRD("ws-legacy"), nil).Maybe()

	// The access gate resolves the UUID (pre-authz, pure read)…
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/workspaces/ws-legacy", nil))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	// …and the user-scoped read surfaces the converged phrase.
	var body struct {
		PhraseID string `json:"phraseId"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	assert.Equal(t, "e2e-minted-7", body.PhraseID,
		"an authorized read of a legacy row must surface the converged phrase")
}
