// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package workspace

import (
	"context"
	"fmt"
	"sort"
	"time"

	apierrors "github.com/lenaxia/llmsafespaces/api/internal/errors"

	v1 "github.com/lenaxia/llmsafespaces/pkg/apis/llmsafespaces/v1"
	"github.com/lenaxia/llmsafespaces/pkg/settings"
)

// SetInstanceSettings injects the instance settings service for enforcement.
func (s *Service) SetInstanceSettings(svc *settings.InstanceService) {
	s.instanceSettings = svc
}

// enforceMaxActiveWorkspaces checks if the user is at their active workspace
// cap and suspends the stalest workspace if needed. Returns the ID of the
// suspended workspace (empty if none was suspended).
func (s *Service) enforceMaxActiveWorkspaces(ctx context.Context, userID, targetWorkspaceID string) (string, error) {
	if s.instanceSettings == nil {
		return "", nil
	}

	maxActive, err := s.instanceSettings.GetInt(ctx, settings.KeyWorkspaceMaxActivePerUser.Name())
	if err != nil {
		// If settings unavailable, don't block activation
		s.logger.Warn("failed to read maxActiveWorkspacesPerUser, skipping enforcement",
			"error", err.Error(),
		)
		return "", nil
	}

	// List user's workspaces (DB rows for ordering by UpdatedAt) and fetch
	// the live phase from CRDs. Phase is owned by the CRD; the DB no longer
	// caches it.
	result, _, err := s.dbService.ListWorkspaces(ctx, userID, 100, 0)
	if err != nil {
		return "", fmt.Errorf("failed to list workspaces for enforcement: %w", err)
	}

	phaseByID := s.fetchUserWorkspaceStates(ctx, userID)

	// activeCount counts every phase that consumes capacity (Active,
	// Creating, Resuming) for the cap check. evictable lists only the
	// phases the suspend RPC will actually accept (Active) — Creating
	// and Resuming workspaces cannot be suspended (SuspendWorkspace
	// rejects them with NewConflictError), so picking one as the
	// stalest produces a 500 to the user. The cap still includes them
	// because they DO consume slots; we just can't free those slots
	// via auto-suspend, only by waiting for them to reach Active or by
	// the user explicitly deleting them.
	type evictCandidate struct {
		ID           string
		LastActivity time.Time
	}
	var activeCount int
	var evictable []evictCandidate
	for _, ws := range result {
		if ws.ID == targetWorkspaceID {
			continue
		}
		st := phaseByID[ws.ID]
		if !isActivePhase(st.Phase) {
			continue
		}
		activeCount++
		if st.Phase == v1.WorkspacePhaseActive {
			// Use LastActivityAt from the CRD annotation (authoritative,
			// written by ActivityTracker on user interaction). Fall back
			// to DB UpdatedAt for pre-US-23.3 workspaces without the
			// annotation.
			activity := st.LastActivityAt
			if activity.IsZero() {
				activity = ws.UpdatedAt
			}
			evictable = append(evictable, evictCandidate{ID: ws.ID, LastActivity: activity})
		}
	}

	if activeCount < maxActive {
		return "", nil
	}

	if len(evictable) == 0 {
		// Cap is full but every workspace at the cap is in a non-
		// suspendable transitional phase. Surface a 409 so the user
		// understands the situation rather than getting a 500 from
		// SuspendWorkspace's own conflict error.
		return "", apierrors.NewConflictError(
			"workspace",
			targetWorkspaceID,
			fmt.Errorf("user at max active workspaces (%d) but no workspace is in Active phase to evict; wait for in-flight workspaces to settle or delete a Creating one", maxActive),
		)
	}

	// Sort by last user activity (oldest first) for stalest selection.
	// Uses LastActivityAt (CRD annotation, written by ActivityTracker)
	// not DB UpdatedAt (bumped by background ops unrelated to user).
	sort.Slice(evictable, func(i, j int) bool {
		return evictable[i].LastActivity.Before(evictable[j].LastActivity)
	})

	// Suspend the stalest evictable workspace
	stalest := evictable[0]
	if err := s.SuspendWorkspace(ctx, userID, stalest.ID); err != nil {
		return "", fmt.Errorf("failed to suspend stalest workspace %s: %w", stalest.ID, err)
	}

	s.logger.Info("auto-suspended workspace due to max active limit",
		"suspended_workspace", stalest.ID,
		"user_id", userID,
		"max_active", maxActive,
	)

	return stalest.ID, nil
}

// parseStorageSize converts a K8s quantity string (e.g. "1Gi", "512Mi") to bytes.
func parseStorageSize(s string) int64 {
	if len(s) < 3 {
		return 0
	}
	suffix := s[len(s)-2:]
	numStr := s[:len(s)-2]
	var n int64
	for _, c := range numStr {
		if c < '0' || c > '9' {
			return 0
		}
		n = n*10 + int64(c-'0')
	}
	switch suffix {
	case "Gi":
		return n * 1024 * 1024 * 1024
	case "Mi":
		return n * 1024 * 1024
	default:
		return 0
	}
}

func isActivePhase(p v1.WorkspacePhase) bool {
	return p == v1.WorkspacePhaseActive || p == v1.WorkspacePhaseCreating || p == v1.WorkspacePhaseResuming
}
