// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// #1507: the startup guard's documented contract — 0 (unset) and >=36
// are valid; anything else (including negatives) refuses startup.
func TestValidateWorkspaceTerminationGrace(t *testing.T) {
	tests := []struct {
		name    string
		grace   int64
		wantErr bool
	}{
		{"unset keeps the default", 0, false},
		{"at the floor accepts", 36, false},
		{"well above the floor accepts", 120, false},
		{"just below the floor rejects", 35, true},
		{"one second rejects", 1, true},
		{"negative rejects", -5, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := validateWorkspaceTerminationGrace(tc.grace)
			if tc.wantErr {
				assert.Error(t, err)
				return
			}
			assert.NoError(t, err)
		})
	}
}
