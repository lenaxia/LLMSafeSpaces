// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package workspace

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	apierrors "github.com/lenaxia/llmsafespaces/api/internal/errors"
	v1 "github.com/lenaxia/llmsafespaces/pkg/apis/llmsafespaces/v1"
	"github.com/lenaxia/llmsafespaces/pkg/types"
)

// #1757 r1 pins: the lazy phrase backfill lives HERE — in the
// USER-SCOPED read, after verifyOwner. The r0 review caught the
// backfill firing inside the pre-authorization DB read (any
// authenticated caller could mint on a foreign workspace by probing
// its UUID); these pins hold the post-authorization convergence and
// the surfaced phraseId.

func phraseBackfillCRD() *v1.Workspace {
	return &v1.Workspace{
		ObjectMeta: metav1.ObjectMeta{Name: "ws-legacy", Namespace: "default"},
		Spec:       v1.WorkspaceSpec{Runtime: "python:3.11", Owner: v1.WorkspaceOwner{UserID: "user-1"}},
		Status:     v1.WorkspaceStatus{Phase: v1.WorkspacePhaseActive},
	}
}

// The real backfill pin (r1 Missing #2): a legacy row (NULL phrase)
// read by its OWNER converges — the seam's UPDATE+read-back fires,
// and the phrase surfaces on the response. verifyOwner re-resolves
// the workspace internally, hence two GetWorkspace returns.
func TestGetWorkspace_NullPhrase_BackfillsAfterVerifyOwner(t *testing.T) {
	f := newEnsureFixture(t)

	meta := &types.WorkspaceMetadata{ID: "ws-legacy", UserID: "user-1"} // PhraseID empty
	f.db.On("GetWorkspace", mock.Anything, "ws-legacy").Return(meta, nil).Twice()
	f.ws.On("Get", mock.Anything, "ws-legacy", mock.Anything).Return(phraseBackfillCRD(), nil).Once()
	f.db.On("EnsureWorkspacePhraseRegistered", mock.Anything, "ws-legacy").
		Return("amber-river-2", nil).Once()

	ws, err := f.svc.GetWorkspace(context.Background(), "user-1", "ws-legacy")
	require.NoError(t, err)
	assert.Equal(t, "amber-river-2", ws.PhraseID,
		"the converged phrase must surface on the read response")
	f.db.AssertCalled(t, "EnsureWorkspacePhraseRegistered", mock.Anything, "ws-legacy")
}

// An already-converged row never re-mints (the seam is idempotent,
// but the read must not even call it).
func TestGetWorkspace_WithPhrase_NeverBackfills(t *testing.T) {
	f := newEnsureFixture(t)

	meta := &types.WorkspaceMetadata{ID: "ws-minted", UserID: "user-1", PhraseID: "linen-orchard-3"}
	f.db.On("GetWorkspace", mock.Anything, "ws-minted").Return(meta, nil).Twice()
	f.ws.On("Get", mock.Anything, "ws-minted", mock.Anything).Return(phraseBackfillCRD(), nil).Once()

	ws, err := f.svc.GetWorkspace(context.Background(), "user-1", "ws-minted")
	require.NoError(t, err)
	assert.Equal(t, "linen-orchard-3", ws.PhraseID)
	f.db.AssertNotCalled(t, "EnsureWorkspacePhraseRegistered", mock.Anything, mock.Anything)
}

// The security property (r1 Robustness #1): a caller who fails
// verifyOwner NEVER triggers the backfill write — the foreign probe
// answers the ownership error with zero side effects.
func TestGetWorkspace_DeniedCaller_NeverBackfills(t *testing.T) {
	f := newEnsureFixture(t)

	meta := &types.WorkspaceMetadata{ID: "ws-foreign", UserID: "user-9"} // not user-1's
	// The read + verifyOwner's re-resolve both fetch the row; the
	// ownership check denies — the CRD is never fetched.
	f.db.On("GetWorkspace", mock.Anything, "ws-foreign").Return(meta, nil).Twice()

	_, err := f.svc.GetWorkspace(context.Background(), "user-1", "ws-foreign")
	require.Error(t, err)
	var apiErr *apierrors.APIError
	require.True(t, errors.As(err, &apiErr))
	assert.Equal(t, 403, apiErr.StatusCode())
	f.db.AssertNotCalled(t, "EnsureWorkspacePhraseRegistered", mock.Anything, mock.Anything,
		"a denied caller must never trigger the backfill write")
}

// r2 (verdict Missing #1): the creation-path mint is ASSERTED, not
// merely permitted — the r1 fixtures stubbed the seam Maybe(), so
// deleting the mint at creation failed nothing (reads would converge
// the row in production, but the creation contract needs its pin).
func TestCreateWorkspace_MintsPhraseAtCreation(t *testing.T) {
	f := newDefaultsFixture(t, nil)
	ctx := context.Background()

	f.ws.On("Create", mock.Anything, mock.Anything).
		Return(crdWorkspace("ws-1", "default", "user1", "1Gi"), nil)
	f.db.On("CreateWorkspace", ctx, mock.Anything).Return(nil)
	f.db.On("EnsureWorkspacePhraseRegistered", mock.Anything, "ws-1").
		Return("amber-river-7", nil).Once()

	req := types.CreateWorkspaceRequest{Name: "test", StorageSize: "1Gi", Runtime: "python:3.11"}
	_, err := f.svc.CreateWorkspace(ctx, "user1", req)
	require.NoError(t, err)
	// AssertCalled takes no message arg — the rationale lives here:
	// creation must mint; the Maybe() stubs elsewhere assert nothing.
	f.db.AssertCalled(t, "EnsureWorkspacePhraseRegistered", mock.Anything, "ws-1")
}
