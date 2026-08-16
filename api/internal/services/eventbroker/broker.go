// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package eventbroker

import (
	"sync"
	"sync/atomic"

	"github.com/prometheus/client_golang/prometheus"

	apierrors "github.com/lenaxia/llmsafespaces/api/internal/errors"
	apitypes "github.com/lenaxia/llmsafespaces/api/internal/types"
)

const (
	BrokerChannelBuffer   = 16
	numShards             = 16
	userChannelBuffer     = 128
	replayBufferSize      = 128
	MaxSubscribersPerUser = 20
	HeartbeatSentinelType = "_heartbeat"
)

// ErrTooManySubscribers is returned when SubscribeUser hits MaxSubscribersPerUser.
// It is a *apierrors.APIError (RateLimited/429) so the centralized HTTP error
// handler maps it automatically. Callers can still use errors.Is (backwards
// compat) and errors.As (typed path).
var ErrTooManySubscribers = &apierrors.APIError{
	Type:    apierrors.ErrorTypeRateLimit,
	Code:    "too_many_subscribers",
	Message: "too many active SSE subscribers for user",
}

var brokerDroppedEvents = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Name: "llmsafespaces_sse_broker_dropped_events_total",
		Help: "Events dropped because subscriber channel was full",
	},
	[]string{"broker", "event_type"},
)

func init() {
	prometheus.MustRegister(brokerDroppedEvents)
}

type Subscriber struct {
	Ch          chan apitypes.WorkspaceSSEEvent
	missedEvent atomic.Bool
	closed      atomic.Bool
	onDrop      func(eventType string)
}

// Send enqueues evt and reports whether it was delivered (buffered);
// dropped-on-full returns false so callers can count deliveries
// accurately (#906 review: the delivered counter previously incremented
// after drops too).
func (s *Subscriber) Send(evt apitypes.WorkspaceSSEEvent) (delivered bool) {
	if s.closed.Load() {
		return false
	}
	defer func() { _ = recover() }()
	if s.missedEvent.Load() {
		resync := apitypes.WorkspaceSSEEvent{Type: "resync"}
		select {
		case s.Ch <- resync:
			s.missedEvent.Store(false)
		default:
			if s.onDrop != nil {
				s.onDrop("resync")
			}
			return false
		}
	}
	select {
	case s.Ch <- evt:
		return true
	default:
		s.missedEvent.Store(true)
		if s.onDrop != nil {
			s.onDrop(evt.Type)
		}
		return false
	}
}

func (s *Subscriber) MarkClosed() {
	s.closed.Store(true)
}

type ReplayEntry struct {
	ID    uint64
	Event apitypes.WorkspaceSSEEvent
}

type replayBuffer struct {
	entries [replayBufferSize]ReplayEntry
	nextID  uint64
	count   int
	head    int
}

func newReplayBuffer() *replayBuffer {
	return &replayBuffer{nextID: 1}
}

func (rb *replayBuffer) appendLocked(evt apitypes.WorkspaceSSEEvent) uint64 {
	id := rb.nextID
	rb.nextID++
	rb.entries[rb.head] = ReplayEntry{ID: id, Event: evt}
	rb.head = (rb.head + 1) % replayBufferSize
	if rb.count < replayBufferSize {
		rb.count++
	}
	return id
}

func (rb *replayBuffer) sinceLocked(lastID uint64) ([]ReplayEntry, bool) {
	if rb.count == 0 {
		return nil, false
	}

	oldestID := rb.nextID - uint64(rb.count) //nolint:gosec // count is always [0, replayBufferSize]
	gapDetected := lastID > 0 && lastID < oldestID

	var result []ReplayEntry
	start := (rb.head - rb.count + replayBufferSize) % replayBufferSize
	for i := 0; i < rb.count; i++ {
		idx := (start + i) % replayBufferSize
		entry := rb.entries[idx]
		if entry.ID > lastID {
			result = append(result, entry)
		}
	}
	return result, gapDetected
}

type shard struct {
	mu       sync.Mutex
	userSubs map[string][]*Subscriber
	wsSubs   map[string][]*Subscriber
	replay   map[string]*replayBuffer
	wsOwner  map[string]string
}
