// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

// Package sessionalerts persists D6 (#998) hung-session escalations to
// the session_alerts table. The escalation sweep runs inside the SSE
// watch reconciler and must never block on the database, so writes go
// through the same bounded-queue + background-drainer pattern as the
// session index: RecordAlert is non-blocking; a full queue drops the
// oldest queued alert (alerts are best-effort durability — the SSE path
// remains the primary surface).
package sessionalerts

import (
	"context"
	"sync"
	"time"

	"github.com/lenaxia/llmsafespaces/api/internal/interfaces"
	"github.com/lenaxia/llmsafespaces/api/internal/logger"
	"github.com/lenaxia/llmsafespaces/pkg/types"
)

// AlertRetention bounds how far back ListByWorkspace serves alerts.
// Older rows are retained in the table (cheap) but hidden from the API
// so a long-lived workspace does not accumulate an unbounded response.
const AlertRetention = 24 * time.Hour

type Service struct {
	db     interfaces.DatabaseService
	logger *logger.Logger
	queue  chan alertEvent
	closeC chan struct{}
	wg     sync.WaitGroup
}

type alertEvent struct {
	workspaceID       string
	sessionID         string
	alert             string
	oldestBusySeconds int
}

// New creates a SessionAlertsService.
func New(db interfaces.DatabaseService, log *logger.Logger) *Service {
	return &Service{
		db:     db,
		logger: log,
		queue:  make(chan alertEvent, 256),
		closeC: make(chan struct{}),
	}
}

// Start begins the background drainer goroutine.
func (s *Service) Start() error {
	s.wg.Add(1)
	go s.drain()
	if s.logger != nil {
		s.logger.Info("SessionAlertsService started")
	}
	return nil
}

// Stop signals the drainer to stop, flushing queued alerts.
func (s *Service) Stop() error {
	close(s.closeC)
	s.wg.Wait()
	if s.logger != nil {
		s.logger.Info("SessionAlertsService stopped")
	}
	return nil
}

// RecordAlert is non-blocking: pushes to a bounded channel. A full
// queue drops the oldest queued alert — durability here is best-effort.
func (s *Service) RecordAlert(workspaceID, sessionID, alert string, oldestBusySeconds int) {
	select {
	case s.queue <- alertEvent{workspaceID: workspaceID, sessionID: sessionID, alert: alert, oldestBusySeconds: oldestBusySeconds}:
	default:
		if s.logger != nil {
			s.logger.Warn("session_alerts: channel full, dropping oldest alert",
				"workspaceID", workspaceID, "sessionID", sessionID,
				"queueSize", len(s.queue))
		}
		select {
		case <-s.queue:
		default:
		}
		s.queue <- alertEvent{workspaceID: workspaceID, sessionID: sessionID, alert: alert, oldestBusySeconds: oldestBusySeconds}
	}
}

// ListByWorkspace returns persisted alerts newest-first, filtered to
// the retention window.
func (s *Service) ListByWorkspace(ctx context.Context, workspaceID string, limit int) ([]types.SessionAlert, error) {
	alerts, err := s.db.ListSessionAlerts(ctx, workspaceID, limit)
	if err != nil {
		return nil, err
	}
	cutoff := time.Now().Add(-AlertRetention)
	kept := alerts[:0]
	for _, a := range alerts {
		if a.CreatedAt.After(cutoff) {
			kept = append(kept, a)
		}
	}
	return kept, nil
}

func (s *Service) drain() {
	defer s.wg.Done()
	for {
		select {
		case ev := <-s.queue:
			s.flush(ev)
		case <-s.closeC:
			for len(s.queue) > 0 {
				s.flush(<-s.queue)
			}
			return
		}
	}
}

func (s *Service) flush(ev alertEvent) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := s.db.InsertSessionAlert(ctx, ev.workspaceID, ev.sessionID, ev.alert, ev.oldestBusySeconds); err != nil {
		if s.logger != nil {
			s.logger.Error("session_alerts: insert failed", err,
				"workspaceID", ev.workspaceID, "sessionID", ev.sessionID)
		}
	}
}
