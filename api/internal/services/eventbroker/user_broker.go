// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package eventbroker

import (
	"hash/fnv"
	"sync"

	"github.com/prometheus/client_golang/prometheus"

	apitypes "github.com/lenaxia/llmsafespaces/api/internal/types"
)

type UserEventBroker struct {
	shards [numShards]shard
}

func NewUserEventBroker() *UserEventBroker {
	b := &UserEventBroker{}
	for i := range b.shards {
		b.shards[i] = shard{
			userSubs: make(map[string][]*Subscriber),
			wsSubs:   make(map[string][]*Subscriber),
			replay:   make(map[string]*replayBuffer),
			wsOwner:  make(map[string]string),
		}
	}
	return b
}

func (b *UserEventBroker) UserSubscriberCount(userID string) int {
	sh := b.userShard(userID)
	sh.mu.Lock()
	defer sh.mu.Unlock()
	return len(sh.userSubs[userID])
}

func (b *UserEventBroker) userShard(userID string) *shard {
	h := fnv.New32a()
	_, _ = h.Write([]byte(userID))
	return &b.shards[h.Sum32()&(numShards-1)]
}

func (b *UserEventBroker) wsShard(workspaceID string) *shard {
	h := fnv.New32a()
	_, _ = h.Write([]byte(workspaceID))
	return &b.shards[h.Sum32()&(numShards-1)]
}

func (b *UserEventBroker) SubscribeUser(userID string) (*Subscriber, error) {
	sh := b.userShard(userID)
	sh.mu.Lock()
	defer sh.mu.Unlock()

	if len(sh.userSubs[userID]) >= MaxSubscribersPerUser {
		return nil, ErrTooManySubscribers
	}

	s := &Subscriber{
		Ch: make(chan apitypes.WorkspaceSSEEvent, userChannelBuffer),
		onDrop: func(eventType string) {
			brokerDroppedEvents.WithLabelValues("user", eventType).Inc()
		},
	}
	sh.userSubs[userID] = append(sh.userSubs[userID], s)
	return s, nil
}

func (b *UserEventBroker) UnsubscribeUser(userID string, s *Subscriber) {
	s.MarkClosed()
	sh := b.userShard(userID)
	sh.mu.Lock()
	defer sh.mu.Unlock()

	subs := sh.userSubs[userID]
	for i, sub := range subs {
		if sub == s {
			sh.userSubs[userID] = append(subs[:i], subs[i+1:]...)
			break
		}
	}
	if len(sh.userSubs[userID]) == 0 {
		delete(sh.userSubs, userID)
	}
}

func (b *UserEventBroker) SubscribeWorkspace(workspaceID string) (*Subscriber, error) {
	sh := b.wsShard(workspaceID)
	sh.mu.Lock()
	defer sh.mu.Unlock()

	if len(sh.wsSubs[workspaceID]) >= MaxSubscribersPerUser {
		return nil, ErrTooManySubscribers
	}

	s := &Subscriber{
		Ch: make(chan apitypes.WorkspaceSSEEvent, userChannelBuffer),
		onDrop: func(eventType string) {
			brokerDroppedEvents.WithLabelValues("workspace", eventType).Inc()
		},
	}
	sh.wsSubs[workspaceID] = append(sh.wsSubs[workspaceID], s)
	return s, nil
}

func (b *UserEventBroker) UnsubscribeWorkspace(workspaceID string, s *Subscriber) {
	s.MarkClosed()
	sh := b.wsShard(workspaceID)
	sh.mu.Lock()
	defer sh.mu.Unlock()

	subs := sh.wsSubs[workspaceID]
	for i, sub := range subs {
		if sub == s {
			sh.wsSubs[workspaceID] = append(subs[:i], subs[i+1:]...)
			break
		}
	}
	if len(sh.wsSubs[workspaceID]) == 0 {
		delete(sh.wsSubs, workspaceID)
	}
}

func (b *UserEventBroker) WorkspaceSubscriberCount(workspaceID string) int {
	sh := b.wsShard(workspaceID)
	sh.mu.Lock()
	defer sh.mu.Unlock()
	return len(sh.wsSubs[workspaceID])
}

func (b *UserEventBroker) PublishToUser(userID string, evt apitypes.WorkspaceSSEEvent) {
	sh := b.userShard(userID)
	sh.mu.Lock()

	if sh.replay[userID] == nil {
		sh.replay[userID] = newReplayBuffer()
	}
	id := sh.replay[userID].appendLocked(evt)
	evt.EventID = id

	targets := make([]*Subscriber, len(sh.userSubs[userID]))
	copy(targets, sh.userSubs[userID])
	sh.mu.Unlock()

	for _, s := range targets {
		s.Send(evt)
	}
}

// deliveredEvents counts events actually handed to workspace-stream
// subscribers (#901 G4): distinguishes zero-emitted / zero-delivered /
// zero-subscribed — during the 2026-08-16 incident the drop counter had
// no series (never dropped) while users received nothing.
var deliveredEvents = prometheus.NewCounterVec(prometheus.CounterOpts{
	Name: "llmsafespaces_sse_broker_delivered_events_total",
	Help: "Events delivered to workspace-stream subscribers, by workspace and event type",
}, []string{"workspace_id", "type"})

var registerDeliveredOnce sync.Once

func (b *UserEventBroker) PublishToWorkspace(workspaceID string, evt apitypes.WorkspaceSSEEvent) {
	sh := b.wsShard(workspaceID)
	sh.mu.Lock()
	targets := make([]*Subscriber, len(sh.wsSubs[workspaceID]))
	copy(targets, sh.wsSubs[workspaceID])
	sh.mu.Unlock()

	registerDeliveredOnce.Do(func() { prometheus.MustRegister(deliveredEvents) })
	for _, s := range targets {
		// Heartbeat sentinels keep streams alive; counting them as
		// delivered AGENT events would swamp the signal (#906 r5).
		if evt.Type == HeartbeatSentinelType {
			s.Send(evt)
			continue
		}
		if s.Send(evt) {
			deliveredEvents.WithLabelValues(workspaceID, evt.Type).Inc()
		}
	}
}

func (b *UserEventBroker) Replay(userID string, lastID uint64) ([]ReplayEntry, bool) {
	sh := b.userShard(userID)
	sh.mu.Lock()
	defer sh.mu.Unlock()

	rb := sh.replay[userID]
	if rb == nil {
		return nil, false
	}
	return rb.sinceLocked(lastID)
}

func (b *UserEventBroker) RecordWorkspaceOwner(workspaceID, userID string) {
	sh := b.wsShard(workspaceID)
	sh.mu.Lock()
	sh.wsOwner[workspaceID] = userID
	sh.mu.Unlock()
}

func (b *UserEventBroker) WorkspaceOwner(workspaceID string) string {
	sh := b.wsShard(workspaceID)
	sh.mu.Lock()
	defer sh.mu.Unlock()
	return sh.wsOwner[workspaceID]
}

func (b *UserEventBroker) CleanupWorkspace(workspaceID string) {
	sh := b.wsShard(workspaceID)
	sh.mu.Lock()
	delete(sh.wsOwner, workspaceID)
	sh.mu.Unlock()
}

func (b *UserEventBroker) CleanupUser(userID string) {
	sh := b.userShard(userID)
	sh.mu.Lock()
	delete(sh.replay, userID)
	sh.mu.Unlock()
}
