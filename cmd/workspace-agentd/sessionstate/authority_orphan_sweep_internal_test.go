// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package sessionstate

// authority_orphan_sweep_internal_test.go — the #1342 ordering pin:
// the orphan capture must run BEFORE the reseed's #1311 evidence sweep.
// That sweep legitimately clears busy-view in-flight state
// (clearBusyFromEvidence sets inFly=nil) for ledgered sessions; if the
// capture ran after it, ledgered sessions' orphaned parts would be
// silently dropped instead of folded as aborted (S12's backstop would
// be vacuous exactly in the ledger-wired production topology).

import (
	"context"
	"testing"

	abiv1 "github.com/lenaxia/llmsafespaces/pkg/abi/v1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type sweepEvidenceStore struct{}

func (sweepEvidenceStore) SessionStates(context.Context) (map[string]SessionSeed, error) {
	return map[string]SessionSeed{
		"ses_ledgered": {Status: abiv1.SessionStatus_SESSION_STATUS_IDLE},
	}, nil
}

func (sweepEvidenceStore) MessagePresence(_ context.Context, _ string, messageIDs []string) (map[string]bool, error) {
	present := map[string]bool{}
	for _, id := range messageIDs {
		present[id] = true
	}
	return present, nil
}

func (sweepEvidenceStore) PendingInputs(context.Context) (map[string][]*abiv1.InputRequest, error) {
	return map[string][]*abiv1.InputRequest{}, nil
}

func TestOrphanSweep_CapturesBeforeEvidenceSweep(t *testing.T) {
	a, err := New(Config{
		PlatformDir: t.TempDir(),
		Parser:      &reconcileNopParser{},
		Store:       sweepEvidenceStore{},
		Passwords:   []string{"pw"},
		Admitter:    &fakeAdmitter{},
		FastCursor:  true,
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = a.Close() })

	// A ledgered session mid-delivery (admitted row) whose turn is also
	// projected busy with a running tool part — the shape the evidence
	// sweep will clear (store says idle → turnEnded → busy-clear +
	// inFly=nil) inside the SAME reseed.
	seedRow(t, a, "ses_ledgered", "entry_1", "msg_assist_1", LedgerStateAdmitted)
	a.IngestForTest(&abiv1.Event{
		Type: abiv1.EventType_EVENT_TYPE_MESSAGE_START, SessionId: "ses_ledgered",
		Message: &abiv1.Message{Id: "msg_1", Parts: []*abiv1.Part{{
			Id:   "prt_orphan",
			Type: abiv1.PartType_PART_TYPE_TOOL,
			Payload: &abiv1.Part_Tool{Tool: &abiv1.ToolPart{
				CallId: "call_1", Name: "bash",
				State: &abiv1.ToolState{Status: abiv1.ToolStatus_TOOL_STATUS_RUNNING},
			}},
		}}},
	})

	require.NoError(t, a.Reseed(context.Background(), ReseedReasonGenerationChange))

	snap := a.State()
	rec, ok := snap.Sessions["ses_ledgered"]
	require.True(t, ok)
	var aborted *abiv1.ToolState
	for _, p := range rec.InFlightParts {
		if p.GetId() == "prt_orphan" {
			tool, ok := p.GetPayload().(*abiv1.Part_Tool)
			require.True(t, ok)
			aborted = tool.Tool.GetState()
		}
	}
	require.NotNil(t, aborted,
		"the orphaned part must survive the evidence sweep's busy-clear and land as aborted")
	assert.Equal(t, abiv1.ToolStatus_TOOL_STATUS_ERROR, aborted.GetStatus())
	assert.Equal(t, OrphanSweepReason, aborted.GetError())
	assert.Equal(t, int64(1), a.Metrics().OrphanPartsAborted)
}
