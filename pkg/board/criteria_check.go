// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package board

// criteria_check.go — #1837 P2b: setCriteriaMet safety. The old
// integer form was check-only (an accidental pass-through silently
// marked criteria met, weakening the in_review→done guard) and combined
// silently with an acceptanceCriteria replace. The object form carries
// {index, met}; both forms are validated loudly (bounds, mutual
// exclusion) and the mutated criterion text is echoed by the surfaces.

import (
	"encoding/json"
	"fmt"
	"strconv"
)

// CriteriaCheck is the setCriteriaMet payload: which criterion (0-based
// index) and the met flag to write. The legacy bare-integer wire form
// (N) decodes as {index: N, met: true} — it can check off but can never
// uncheck; the object form does both.
type CriteriaCheck struct {
	Index int  `json:"index"`
	Met   bool `json:"met"`
}

// UnmarshalJSON accepts the object form {index, met?} (met defaults
// true) and the legacy integer form N (check-off). Anything else is a
// loud error — never a silent zero value.
func (c *CriteriaCheck) UnmarshalJSON(b []byte) error {
	if len(b) == 0 {
		return fmt.Errorf("setCriteriaMet: empty payload")
	}
	if b[0] == '{' {
		var obj struct {
			Index *int `json:"index"`
			Met   *bool `json:"met"`
		}
		if err := json.Unmarshal(b, &obj); err != nil {
			return fmt.Errorf("setCriteriaMet: %w", err)
		}
		if obj.Index == nil {
			return fmt.Errorf("setCriteriaMet: {index, met} requires index")
		}
		c.Index = *obj.Index
		c.Met = obj.Met == nil || *obj.Met
		return nil
	}
	n, err := strconv.Atoi(string(b))
	if err != nil {
		return fmt.Errorf("setCriteriaMet: expected {index, met} or a legacy integer index, got %s", string(b))
	}
	c.Index = n
	c.Met = true
	return nil
}

// validateCriteriaUpdate arbitrates the criteria-touching fields of one
// UpdateTask payload against the task's EFFECTIVE criteria set (the
// replacement when provided, else the existing): the replace and the
// check-off are mutually exclusive, and a check-off out of bounds is a
// loud error — never a silent no-op (the old behavior) and never an
// accidental pass-through check.
func validateCriteriaUpdate(in TaskInput, existing []AcceptanceCriterion) error {
	if in.SetCriteriaMet == nil {
		return nil
	}
	if in.AcceptanceCriteria != nil {
		return &GuardError{Code: GuardInput,
			Detail: "acceptanceCriteria and setCriteriaMet are mutually exclusive — send one or the other (a replace rewrites every flag; a check-off mutates one)"}
	}
	if in.SetCriteriaMet.Index < 0 || in.SetCriteriaMet.Index >= len(existing) {
		return &GuardError{Code: GuardInput,
			Detail: fmt.Sprintf("setCriteriaMet index %d is out of bounds (criteria hold %d entries)", in.SetCriteriaMet.Index, len(existing))}
	}
	return nil
}
