// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package board

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// plan_arms_test.go — design 0070 §1a/§3: the unified plan document's
// epic-arm resolution. Exactly one of the new-epic arm (the epic
// object, or the legacy epicTitle spelling) and the existing-epic arm
// (epicId) — both/neither/two-spelling refusals are GuardInputs, never
// a silent pick. Pure unit tier (the resolution is DB-free; the
// attach-transaction behavior is pinned in the integration tier).

func TestResolvePlanEpicArm(t *testing.T) {
	t.Run("epic object (create)", func(t *testing.T) {
		arm, err := resolvePlanEpicArm(PlanInput{
			Epic:  &PlanEpicSpec{Title: "E", Outcome: "o", Tags: []string{"x"}},
			Tasks: []PlanTaskInput{{Key: "k", Title: "T", Description: "d"}},
		})
		require.NoError(t, err)
		require.NotNil(t, arm.create)
		assert.Equal(t, "E", arm.create.Title)
		assert.Equal(t, "o", arm.create.Outcome)
		assert.Equal(t, []string{"x"}, arm.create.Tags)
		assert.Empty(t, arm.attach)
	})

	t.Run("legacy epicTitle spelling (create)", func(t *testing.T) {
		arm, err := resolvePlanEpicArm(PlanInput{
			EpicTitle: "E", Outcome: "o", Tags: []string{"x"},
			Tasks: []PlanTaskInput{{Key: "k", Title: "T", Description: "d"}},
		})
		require.NoError(t, err)
		require.NotNil(t, arm.create)
		assert.Equal(t, "E", arm.create.Title)
	})

	t.Run("epicId (attach)", func(t *testing.T) {
		arm, err := resolvePlanEpicArm(PlanInput{
			EpicID: "epc-bbbb2222",
			Tasks:  []PlanTaskInput{{Key: "k", Title: "T", Description: "d"}},
		})
		require.NoError(t, err)
		assert.Equal(t, "epc-bbbb2222", arm.attach)
		assert.Nil(t, arm.create)
	})

	t.Run("both arms refused", func(t *testing.T) {
		_, err := resolvePlanEpicArm(PlanInput{
			Epic:   &PlanEpicSpec{Title: "E"},
			EpicID: "epc-bbbb2222",
			Tasks:  []PlanTaskInput{{Key: "k", Title: "T", Description: "d"}},
		})
		require.Error(t, err)
		ge, ok := err.(*GuardError)
		require.True(t, ok)
		assert.Equal(t, GuardInput, ge.Code)
		assert.Contains(t, err.Error(), "not both")
	})

	t.Run("legacy title plus epicId refused", func(t *testing.T) {
		_, err := resolvePlanEpicArm(PlanInput{
			EpicTitle: "E", EpicID: "epc-bbbb2222",
			Tasks: []PlanTaskInput{{Key: "k", Title: "T", Description: "d"}},
		})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "not both")
	})

	t.Run("neither arm refused", func(t *testing.T) {
		_, err := resolvePlanEpicArm(PlanInput{
			Tasks: []PlanTaskInput{{Key: "k", Title: "T", Description: "d"}},
		})
		require.Error(t, err)
		ge, ok := err.(*GuardError)
		require.True(t, ok)
		assert.Equal(t, GuardInput, ge.Code)
		assert.Contains(t, err.Error(), "neither is present")
	})

	t.Run("two create-arm spellings refused", func(t *testing.T) {
		_, err := resolvePlanEpicArm(PlanInput{
			Epic: &PlanEpicSpec{Title: "E"}, EpicTitle: "E2",
			Tasks: []PlanTaskInput{{Key: "k", Title: "T", Description: "d"}},
		})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "spelled once")
	})
}
