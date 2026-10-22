// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package board

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
)

// criteria_check_test.go — #1837 P2b: setCriteriaMet safety. The OLD
// integer form was check-only (accidental pass-through silently marked
// criteria met) and combined silently with acceptanceCriteria replace.
// The NEW object form carries {index, met}; the integer form stays
// accepted as {index, met:true} (wire compat); combined use and
// out-of-range indices are LOUD errors. Red-first: this file landed
// BEFORE CriteriaCheck and validateCriteriaUpdate.

func TestCriteriaCheck_UnmarshalForms(t *testing.T) {
	t.Run("integer form is check-off (wire compat)", func(t *testing.T) {
		var c CriteriaCheck
		require.NoError(t, json.Unmarshal([]byte(`2`), &c))
		require.Equal(t, 2, c.Index)
		require.True(t, c.Met, "the legacy integer form checks OFF — it cannot uncheck")
	})

	t.Run("object form unchecks", func(t *testing.T) {
		var c CriteriaCheck
		require.NoError(t, json.Unmarshal([]byte(`{"index":1,"met":false}`), &c))
		require.Equal(t, 1, c.Index)
		require.False(t, c.Met)
	})

	t.Run("object form default met=true", func(t *testing.T) {
		var c CriteriaCheck
		require.NoError(t, json.Unmarshal([]byte(`{"index":0}`), &c))
		require.True(t, c.Met)
	})

	t.Run("object form without index refused", func(t *testing.T) {
		var c CriteriaCheck
		require.Error(t, json.Unmarshal([]byte(`{"met":true}`), &c))
	})

	t.Run("garbage refused", func(t *testing.T) {
		var c CriteriaCheck
		require.Error(t, json.Unmarshal([]byte(`"x"`), &c))
		require.Error(t, json.Unmarshal([]byte(`true`), &c))
	})
}

func TestCriteriaCheck_MarshalIsObjectForm(t *testing.T) {
	b, err := json.Marshal(CriteriaCheck{Index: 1, Met: false})
	require.NoError(t, err)
	require.JSONEq(t, `{"index":1,"met":false}`, string(b))
}

func TestValidateCriteriaUpdate_Matrix(t *testing.T) {
	existing := []AcceptanceCriterion{{Text: "c0"}, {Text: "c1", Met: true}, {Text: "c2"}}
	replacement := []AcceptanceCriterion{{Text: "r0"}}

	t.Run("check-off via object form ok", func(t *testing.T) {
		require.NoError(t, validateCriteriaUpdate(TaskInput{
			SetCriteriaMet: &CriteriaCheck{Index: 1, Met: true},
		}, existing))
	})

	t.Run("uncheck via object form ok", func(t *testing.T) {
		require.NoError(t, validateCriteriaUpdate(TaskInput{
			SetCriteriaMet: &CriteriaCheck{Index: 1, Met: false},
		}, existing))
	})

	t.Run("legacy integer form ok", func(t *testing.T) {
		require.NoError(t, validateCriteriaUpdate(TaskInput{
			SetCriteriaMet: &CriteriaCheck{Index: 0, Met: true},
		}, existing))
	})

	t.Run("combined with criteria replace REFUSED", func(t *testing.T) {
		err := validateCriteriaUpdate(TaskInput{
			AcceptanceCriteria: replacement,
			SetCriteriaMet:     &CriteriaCheck{Index: 0, Met: true},
		}, existing)
		require.Error(t, err)
		require.Contains(t, err.Error(), "one or the other")
		var ge *GuardError
		require.ErrorAs(t, err, &ge)
		require.Equal(t, GuardInput, ge.Code)
	})

	t.Run("out of range refused (existing set)", func(t *testing.T) {
		err := validateCriteriaUpdate(TaskInput{
			SetCriteriaMet: &CriteriaCheck{Index: 3, Met: true},
		}, existing)
		require.Error(t, err)
		require.Contains(t, err.Error(), "bounds")
	})

	t.Run("negative refused", func(t *testing.T) {
		err := validateCriteriaUpdate(TaskInput{
			SetCriteriaMet: &CriteriaCheck{Index: -1, Met: true},
		}, existing)
		require.Error(t, err)
		require.Contains(t, err.Error(), "bounds")
	})

	t.Run("bounds checked against the replacement too", func(t *testing.T) {
		err := validateCriteriaUpdate(TaskInput{
			SetCriteriaMet: &CriteriaCheck{Index: 1, Met: true},
		}, []AcceptanceCriterion{{Text: "only-one"}})
		require.Error(t, err)
		require.Contains(t, err.Error(), "bounds")
	})

	t.Run("plain criteria replace ok", func(t *testing.T) {
		require.NoError(t, validateCriteriaUpdate(TaskInput{AcceptanceCriteria: replacement}, existing))
	})

	t.Run("no criteria touch ok", func(t *testing.T) {
		require.NoError(t, validateCriteriaUpdate(TaskInput{}, existing))
	})
}

// The legacy input shape compiles alongside the new one (callers that
// built SetCriteriaMet from *int keep working through the same
// conversion).
func TestTaskInput_SetCriteriaMetFieldTakesCriteriaCheck(t *testing.T) {
	in := TaskInput{SetCriteriaMet: &CriteriaCheck{Index: 0, Met: false}}
	require.NotNil(t, in.SetCriteriaMet)
	require.False(t, in.SetCriteriaMet.Met)
}
