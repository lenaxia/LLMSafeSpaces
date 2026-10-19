// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package board

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestEventPublisher_Plumbing: the option, the setter, and the
// nil-publisher fail-open contract of publishCommitted (the §4.6 seam's
// unit surface — the ordering/replay pins live in the integration suite).
func TestEventPublisher_Plumbing(t *testing.T) {
	s := &Service{}

	s.publishCommitted(Event{ID: "e1", Action: "unit"})
	assert.True(t, true, "nil publisher is a no-op (writes never depend on the projection)")

	var got []Event
	s.SetEventPublisher(func(evt Event) { got = append(got, evt) })
	s.publishCommitted(Event{ID: "e2", Action: "create_task"})
	s.publishCommitted(Event{ID: "e3", Action: "reclaim"})
	assert.Equal(t, []string{"e2", "e3"}, []string{got[0].ID, got[1].ID})

	s.SetEventPublisher(nil)
	s.publishCommitted(Event{ID: "e4"})
	assert.Len(t, got, 2, "replacing with nil disables publishing")

	s2 := &Service{}
	var viaOption string
	WithEventPublisher(func(evt Event) { viaOption = evt.ID })(s2)
	s2.publishCommitted(Event{ID: "e5"})
	assert.Equal(t, "e5", viaOption)
}
