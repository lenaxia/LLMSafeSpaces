// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package workflows

// #1470: a routine fire that FAILS with a surviving session (PreserveOnFailure
// semantics — keep on failure; PreserveAlways likewise) must still
// origin-record and index that session. agentd's error envelope now carries
// sessionId iff the session exists at node completion (ephemeral modes tear
// down and omit it; the transport-error branch has no response object and
// stays blind). The delivered branch prefers the same first-class field,
// so drifted output shapes can no longer orphan a live session either.

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/lenaxia/llmsafespaces/pkg/types"
	wf "github.com/lenaxia/llmsafespaces/pkg/workflows"
)

func TestExecuteRoutine_FailedFire_PreserveOnFailure_RecordsSurvivingSession(t *testing.T) {
	store := newMockSchedulerStore()
	agentd := newMockAgentd()
	agentd.errCodes["routine-agent"] = "script_failed"
	agentd.sessionIDs["routine-agent"] = "ses_fail1"
	index := &recordingSessionIndex{}
	wsID := "ws-1"
	trigger := &wf.TriggerRow{ID: "trig-f1", Name: "Watchdog", WorkspaceID: &wsID, Prompt: "test",
		CaptureMode: types.CaptureFull, PreserveSession: types.PreserveOnFailure}
	fire := &wf.TriggerFireRow{ID: "fire-f1", TriggerID: "trig-f1", InputEnvelope: json.RawMessage(`{}`)}
	sched := &Scheduler{Store: store, Activator: &mockActivator{}, AgentdClient: agentd, Logger: noopLogger{},
		SessionIndex: index}

	sched.executeRoutine(context.Background(), noopLogger{}, trigger, fire)

	require.Equal(t, "failed", store.statuses["fire-f1"])
	require.Equal(t, 1, store.triggerFail["trig-f1"], "failure accounting is unchanged")

	origin, ok := store.sessionOrigins["ses_fail1"]
	require.True(t, ok, "failed PreserveOnFailure fire must origin-record its surviving session")
	require.Equal(t, types.SessionOriginRoutine, origin.Origin)
	require.NotNil(t, origin.FireID)
	require.Equal(t, "fire-f1", *origin.FireID)
	require.Equal(t, "Watchdog", origin.Title)

	titles := index.titleCalls()
	require.Len(t, titles, 1, "the surviving session must be indexed")
	require.Equal(t, sessionIndexTitleCall{workspaceID: "ws-1", sessionID: "ses_fail1", title: "Watchdog"}, titles[0])
	require.Len(t, index.messageCalls(), 1)
}

func TestExecuteRoutine_FailedFire_PreserveAlways_RecordsSurvivingSession(t *testing.T) {
	store := newMockSchedulerStore()
	agentd := newMockAgentd()
	agentd.errCodes["routine-agent"] = "schema_mismatch"
	agentd.sessionIDs["routine-agent"] = "ses_fail2"
	index := &recordingSessionIndex{}
	wsID := "ws-1"
	trigger := &wf.TriggerRow{ID: "trig-f2", Name: "Weather Bot", WorkspaceID: &wsID, Prompt: "test",
		CaptureMode: types.CaptureFull, PreserveSession: types.PreserveAlways}
	fire := &wf.TriggerFireRow{ID: "fire-f2", TriggerID: "trig-f2", InputEnvelope: json.RawMessage(`{}`)}
	sched := &Scheduler{Store: store, Activator: &mockActivator{}, AgentdClient: agentd, Logger: noopLogger{},
		SessionIndex: index}

	sched.executeRoutine(context.Background(), noopLogger{}, trigger, fire)

	require.Equal(t, "failed", store.statuses["fire-f2"])
	require.Len(t, store.sessionOrigins, 1, "failed PreserveAlways fire must origin-record its surviving session")
	require.Len(t, index.titleCalls(), 1)
}

func TestExecuteRoutine_FailedFire_NoSessionInEnvelope_RecordsNothing(t *testing.T) {
	store := newMockSchedulerStore()
	agentd := newMockAgentd()
	agentd.errCodes["routine-agent"] = "script_failed" // old agentd / torn-down ephemeral: no sessionId
	index := &recordingSessionIndex{}
	wsID := "ws-1"
	trigger := &wf.TriggerRow{ID: "trig-f3", Name: "Chores", WorkspaceID: &wsID, Prompt: "test",
		CaptureMode: types.CaptureFull, PreserveSession: types.PreserveOnFailure}
	fire := &wf.TriggerFireRow{ID: "fire-f3", TriggerID: "trig-f3", InputEnvelope: json.RawMessage(`{}`)}
	sched := &Scheduler{Store: store, Activator: &mockActivator{}, AgentdClient: agentd, Logger: noopLogger{},
		SessionIndex: index}

	sched.executeRoutine(context.Background(), noopLogger{}, trigger, fire)

	require.Equal(t, "failed", store.statuses["fire-f3"])
	require.Empty(t, store.sessionOrigins, "no surviving session in the envelope — nothing to record")
	require.Empty(t, index.titleCalls())
}

func TestExecuteRoutine_TransportError_BlindToSession_RecordsNothing(t *testing.T) {
	store := newMockSchedulerStore()
	agentd := newMockAgentd()
	agentd.errors["routine-agent"] = fmt.Errorf("agentd node execute returned 500: boom")
	index := &recordingSessionIndex{}
	wsID := "ws-1"
	trigger := &wf.TriggerRow{ID: "trig-f4", Name: "Watchdog", WorkspaceID: &wsID, Prompt: "test",
		CaptureMode: types.CaptureFull, PreserveSession: types.PreserveOnFailure}
	fire := &wf.TriggerFireRow{ID: "fire-f4", TriggerID: "trig-f4", InputEnvelope: json.RawMessage(`{}`)}
	sched := &Scheduler{Store: store, Activator: &mockActivator{}, AgentdClient: agentd, Logger: noopLogger{},
		SessionIndex: index}

	sched.executeRoutine(context.Background(), noopLogger{}, trigger, fire)

	require.Equal(t, "failed", store.statuses["fire-f4"])
	require.Empty(t, store.sessionOrigins,
		"the transport-error branch has no response object — a session agentd created is unreachable there (documented #1470 residual)")
	require.Empty(t, index.titleCalls())
}

func TestExecuteRoutine_Delivered_EnvelopeSessionIDPreferredOverOutput(t *testing.T) {
	store := newMockSchedulerStore()
	agentd := newMockAgentd()
	// Drifted output shape: no session_id in the payload (#1470 sub-case 2).
	agentd.outputs["routine-agent"] = json.RawMessage(`{"response":"ok"}`)
	agentd.sessionIDs["routine-agent"] = "ses_drift1"
	index := &recordingSessionIndex{}
	wsID := "ws-1"
	trigger := &wf.TriggerRow{ID: "trig-f5", Name: "Weather Bot", WorkspaceID: &wsID, Prompt: "test",
		CaptureMode: types.CaptureFull, PreserveSession: types.PreserveAlways}
	fire := &wf.TriggerFireRow{ID: "fire-f5", TriggerID: "trig-f5", InputEnvelope: json.RawMessage(`{}`)}
	sched := &Scheduler{Store: store, Activator: &mockActivator{}, AgentdClient: agentd, Logger: noopLogger{},
		SessionIndex: index}

	sched.executeRoutine(context.Background(), noopLogger{}, trigger, fire)

	require.Equal(t, "delivered", store.statuses["fire-f5"])
	require.Len(t, store.sessionOrigins, 1, "envelope sessionId must rescue the drifted-output session")
	require.Len(t, index.titleCalls(), 1)
	require.Equal(t, "ses_drift1", index.titleCalls()[0].sessionID)
}

func TestExecuteRoutine_Delivered_OldAgentdOutputParseBackCompat(t *testing.T) {
	store := newMockSchedulerStore()
	agentd := newMockAgentd()
	agentd.outputs["routine-agent"] = json.RawMessage(`{"response":"ok","session_id":"ses_old1"}`)
	index := &recordingSessionIndex{}
	wsID := "ws-1"
	trigger := &wf.TriggerRow{ID: "trig-f6", Name: "Weather Bot", WorkspaceID: &wsID, Prompt: "test",
		CaptureMode: types.CaptureFull, PreserveSession: types.PreserveAlways}
	fire := &wf.TriggerFireRow{ID: "fire-f6", TriggerID: "trig-f6", InputEnvelope: json.RawMessage(`{}`)}
	sched := &Scheduler{Store: store, Activator: &mockActivator{}, AgentdClient: agentd, Logger: noopLogger{},
		SessionIndex: index}

	sched.executeRoutine(context.Background(), noopLogger{}, trigger, fire)

	require.Equal(t, "delivered", store.statuses["fire-f6"])
	require.Len(t, store.sessionOrigins, 1, "output-payload session_id must keep working (old-agentd skew)")
	require.Equal(t, "ses_old1", index.titleCalls()[0].sessionID)
}
