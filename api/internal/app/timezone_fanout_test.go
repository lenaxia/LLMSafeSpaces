package app

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lenaxia/llmsafespaces/api/internal/handlers"
	"github.com/lenaxia/llmsafespaces/api/internal/services/agentpush"
	"github.com/lenaxia/llmsafespaces/pkg/types"
)

type fakeTZLister struct {
	result *types.WorkspaceListResult
	err    error
	calls  int
}

func (f *fakeTZLister) ListWorkspaces(_ context.Context, _ string, _ types.ListOptions) (*types.WorkspaceListResult, error) {
	f.calls++
	return f.result, f.err
}

type recordingTZPusher struct {
	targets []string
	tz      string
	err     error
}

func (r *recordingTZPusher) PushUserTimezone(_ context.Context, _, workspaceID, tz string) error {
	r.targets = append(r.targets, workspaceID)
	r.tz = tz
	return r.err
}

// fanOutTimezonePush's contract, executed against the real closure: only
// Active/Creating/Resuming workspaces receive the push, a per-pod
// failure does not abort the fan-out, list failures abort without
// panic, and nil collaborators are no-ops.
func TestFanOutTimezonePush_PhaseFilter(t *testing.T) {
	lister := &fakeTZLister{result: &types.WorkspaceListResult{Items: []types.WorkspaceListItem{
		{ID: "ws-active", Phase: "Active"},
		{ID: "ws-creating", Phase: "Creating"},
		{ID: "ws-resuming", Phase: "Resuming"},
		{ID: "ws-suspended", Phase: "Suspended"},
		{ID: "ws-failed", Phase: "Failed"},
	}}}
	pusher := &recordingTZPusher{}

	fanOutTimezonePush(context.Background(), pusher, lister, nil, "u-1", "Europe/Berlin")

	assert.Equal(t, 1, lister.calls)
	require.Len(t, pusher.targets, 3)
	assert.ElementsMatch(t, []string{"ws-active", "ws-creating", "ws-resuming"}, pusher.targets)
	assert.Equal(t, "Europe/Berlin", pusher.tz)
}

func TestFanOutTimezonePush_PodFailureContinues(t *testing.T) {
	lister := &fakeTZLister{result: &types.WorkspaceListResult{Items: []types.WorkspaceListItem{
		{ID: "ws-a", Phase: "Active"},
		{ID: "ws-b", Phase: "Active"},
	}}}
	pusher := &recordingTZPusher{err: assert.AnError}

	require.NotPanics(t, func() {
		fanOutTimezonePush(context.Background(), pusher, lister, nil, "u-1", "X")
	})
	assert.Len(t, pusher.targets, 2, "a failing pod does not abort the fan-out")
}

func TestFanOutTimezonePush_ListFailureNoPanic(t *testing.T) {
	lister := &fakeTZLister{err: assert.AnError}
	pusher := &recordingTZPusher{}
	require.NotPanics(t, func() {
		fanOutTimezonePush(context.Background(), pusher, lister, nil, "u-1", "X")
	})
	assert.Empty(t, pusher.targets)
}

func TestFanOutTimezonePush_NilCollaborators(t *testing.T) {
	require.NotPanics(t, func() {
		fanOutTimezonePush(context.Background(), nil, nil, nil, "u", "X")
	})
}

// The handlers.TimezonePusher interface is satisfied by agentpush.Service
// (compile-time pin — the app wiring passes the concrete service).
var _ handlers.TimezonePusher = (*agentpush.Service)(nil)

func TestFanOutTimezonePush_NilResultNoPanic(t *testing.T) {
	lister := &fakeTZLister{result: nil}
	pusher := &recordingTZPusher{}
	require.NotPanics(t, func() {
		fanOutTimezonePush(context.Background(), pusher, lister, nil, "u-1", "X")
	})
	assert.Empty(t, pusher.targets)
}
