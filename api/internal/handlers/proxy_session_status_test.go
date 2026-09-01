package handlers

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lenaxia/llmsafespaces/api/internal/services/eventbroker"
	k8smocks "github.com/lenaxia/llmsafespaces/mocks/kubernetes"
)

func newHandlerWithMockK8s(t *testing.T) *ProxyHandler {
	t.Helper()
	handler, err := NewProxyHandler(k8smocks.NewMockKubernetesClient(), &testLogger{}, "default", nil, nil)
	require.NoError(t, err)
	return handler
}

// US-69.11: the tracker's onSessionIdle/onSessionActive callbacks are
// retired; session.status on the user stream now originates from
// usageBridge.SessionStatus (driven by the ABI consumer's SESSION_STATUS
// events). Same wire shapes, same owner-unknown skip, same nil-safety.

func TestBridgeSessionStatus_Idle_PublishesToUserBroker(t *testing.T) {
	handler := newHandlerWithMockK8s(t)
	t.Cleanup(stubUsageStream())

	broker := eventbroker.NewUserEventBroker()
	broker.RecordWorkspaceOwner("ws-1", "user-1")
	handler.userBroker = broker

	sub, err := broker.SubscribeUser("user-1")
	require.NoError(t, err)
	defer broker.UnsubscribeUser("user-1", sub)

	(&usageBridge{h: handler}).SessionStatus("ws-1", "s1", false)

	select {
	case evt := <-sub.Ch:
		assert.Equal(t, "session.status", evt.Type)
		assert.Equal(t, "idle", evt.Status)
		assert.Equal(t, "s1", evt.SessionID)
		assert.Equal(t, "ws-1", evt.WorkspaceID)
	default:
		t.Fatal("expected user-scoped SSE event for session.status idle")
	}
}

func TestBridgeSessionStatus_Busy_PublishesToUserBroker(t *testing.T) {
	handler := newHandlerWithMockK8s(t)
	t.Cleanup(stubUsageStream())

	broker := eventbroker.NewUserEventBroker()
	broker.RecordWorkspaceOwner("ws-1", "user-1")
	handler.userBroker = broker

	sub, err := broker.SubscribeUser("user-1")
	require.NoError(t, err)
	defer broker.UnsubscribeUser("user-1", sub)

	(&usageBridge{h: handler}).SessionStatus("ws-1", "s1", true)

	select {
	case evt := <-sub.Ch:
		assert.Equal(t, "session.status", evt.Type)
		assert.Equal(t, "busy", evt.Status)
		assert.Equal(t, "s1", evt.SessionID)
		assert.Equal(t, "ws-1", evt.WorkspaceID)
	default:
		t.Fatal("expected user-scoped SSE event for session.status busy")
	}
}

func TestBridgeSessionStatus_SkipsUserBrokerWhenOwnerUnknown(t *testing.T) {
	handler := newHandlerWithMockK8s(t)
	t.Cleanup(stubUsageStream())

	broker := eventbroker.NewUserEventBroker()
	handler.userBroker = broker

	sub, err := broker.SubscribeUser("user-1")
	require.NoError(t, err)
	defer broker.UnsubscribeUser("user-1", sub)

	(&usageBridge{h: handler}).SessionStatus("ws-unknown", "s1", false)

	select {
	case <-sub.Ch:
		t.Fatal("should not publish to user broker when owner unknown")
	default:
	}
}

func TestBridgeSessionStatus_NoPanicWhenUserBrokerNil(t *testing.T) {
	handler := newHandlerWithMockK8s(t)

	assert.NotPanics(t, func() {
		(&usageBridge{h: handler}).SessionStatus("ws-1", "s1", false)
		(&usageBridge{h: handler}).SessionStatus("ws-1", "s1", true)
	})
}
