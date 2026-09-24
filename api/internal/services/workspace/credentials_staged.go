// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package workspace

import (
	"context"
	"fmt"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/util/retry"

	v1 "github.com/lenaxia/llmsafespaces/pkg/apis/llmsafespaces/v1"
)

// ReportRelayBatchOutcome surfaces the API's relay batch outcome on
// the Workspace CRD (design 0061 §6, M4). A degraded batch writes the
// EXISTING CredentialsStaged condition as False/<degrade reason>; the
// next clean batch writes it back to True — via the same status path
// the controller's conditions ride (UpdateStatus). No new condition
// type, no new surface: the US-72.3 condition, fed by the batch
// outcome, so a degraded workspace answers "why are there no models"
// on the object itself, visible to the UI and alertable.
//
// The degrade reason is an INPUT by design: relay_staging_not_ready
// today, relay_fallback_delivery when M2's fallback lands — the writer
// neither enumerates nor interprets reasons (the builder owns the
// vocabulary). Callers under flag-off never invoke this (the
// condition's W15 contract: absent when relay-only key delivery is
// disabled — the pod-bootstrap hook gates on the installed relay
// source before calling).
//
// Condition-transition semantics mirror the controller's setCondition
// (health.go): a repeated same-state write refreshes the message
// WITHOUT bumping LastTransitionTime, so the API's batch writes and
// the controller's staging writes cannot flap the transition clock.
func (s *Service) ReportRelayBatchOutcome(ctx context.Context, workspaceID, degradeReason string) error {
	wsClient, err := s.workspaceCRDClient()
	if err != nil {
		return fmt.Errorf("initialize workspace client: %w", err)
	}

	// Same optimistic-concurrency lane as the resume path: a concurrent
	// controller status write retries the get→upsert→UpdateStatus cycle.
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		current, err := wsClient.Get(ctx, workspaceID, metav1.GetOptions{})
		if err != nil {
			return fmt.Errorf("get workspace %s: %w", workspaceID, err)
		}
		setCredentialsStagedCondition(current, degradeReason)
		_, err = wsClient.UpdateStatus(ctx, current)
		return err
	})
}

// setCredentialsStagedCondition upserts CredentialsStaged from the
// batch outcome on an in-memory CRD (exported semantics, local shape —
// the controller's setCondition twin for the API side).
func setCredentialsStagedCondition(ws *v1.Workspace, degradeReason string) {
	status, reason, message := "True", v1.ReasonCredentialsStaged,
		"credentials staged: clean relay batch delivered"
	if degradeReason != "" {
		status, reason = "False", degradeReason
		message = fmt.Sprintf(
			"credentials degraded: no relay credentials were delivered (%s); the workspace will serve no models until staging recovers — see the relay staging pass",
			degradeReason)
	}
	for i := range ws.Status.Conditions {
		if ws.Status.Conditions[i].Type == v1.WorkspaceConditionCredentialsStaged {
			if ws.Status.Conditions[i].Status == status && ws.Status.Conditions[i].Reason == reason {
				ws.Status.Conditions[i].Message = message
				return
			}
			ws.Status.Conditions[i].Status = status
			ws.Status.Conditions[i].Reason = reason
			ws.Status.Conditions[i].Message = message
			ws.Status.Conditions[i].LastTransitionTime = metav1.Now()
			return
		}
	}
	ws.Status.Conditions = append(ws.Status.Conditions, v1.WorkspaceCondition{
		Type:               v1.WorkspaceConditionCredentialsStaged,
		Status:             status,
		Reason:             reason,
		Message:            message,
		LastTransitionTime: metav1.Now(),
	})
}
