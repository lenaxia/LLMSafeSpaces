// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package board

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// config_test.go — RED-first pins for the #1832 item 6 Task Board Config
// model: code defaults, the read-time merge (override wins / unset key
// inherits the global default), per-key validation, and unknown-key
// rejection. Landed BEFORE pkg/board/config.go (house red-first rule);
// the red state at this commit is the compile error (undefined
// DefaultBoardConfig / MergeBoardConfig / ValidateBoardConfigKey /
// DecodeBoardConfigOverride).

func strPtr(s string) *string { return &s }
func boolPtr(b bool) *bool    { return &b }
func intPtr(i int) *int       { return &i }

// TestBoardConfigDefaults pins the code defaults (#1832 item 6):
// reviewColumn=manual, autoAssignReviewer=false, defaultWorkerSlots=2
// (the platform's existing worker-slots default — service create,
// 000036 column default, MCP tool docs all say 2).
func TestBoardConfigDefaults(t *testing.T) {
	d := DefaultBoardConfig()
	assert.Equal(t, "manual", d.ReviewColumn)
	assert.False(t, d.AutoAssignReviewer)
	assert.Equal(t, 2, d.DefaultWorkerSlots)
}

// TestMergeBoardConfig_OverrideWins: a set override key always beats the
// base (global) value.
func TestMergeBoardConfig_OverrideWins(t *testing.T) {
	base := BoardConfig{ReviewColumn: "manual", AutoAssignReviewer: false, DefaultWorkerSlots: 2}
	o := &BoardConfigOverride{
		ReviewColumn:       strPtr("auto"),
		AutoAssignReviewer: boolPtr(true),
		DefaultWorkerSlots: intPtr(5),
	}
	got := MergeBoardConfig(base, o)
	assert.Equal(t, "auto", got.ReviewColumn)
	assert.True(t, got.AutoAssignReviewer)
	assert.Equal(t, 5, got.DefaultWorkerSlots)
}

// TestMergeBoardConfig_UnsetInherits: an unset (nil) override key
// inherits the base value — the unset=inherit contract at the heart of
// #1832 item 6's storage design.
func TestMergeBoardConfig_UnsetInherits(t *testing.T) {
	base := BoardConfig{ReviewColumn: "off", AutoAssignReviewer: true, DefaultWorkerSlots: 7}
	got := MergeBoardConfig(base, &BoardConfigOverride{DefaultWorkerSlots: intPtr(3)})
	assert.Equal(t, "off", got.ReviewColumn, "unset reviewColumn inherits base")
	assert.True(t, got.AutoAssignReviewer, "unset autoAssignReviewer inherits base")
	assert.Equal(t, 3, got.DefaultWorkerSlots)
}

// TestMergeBoardConfig_NilOverrideIsBase: no overrides at all renders
// the base verbatim.
func TestMergeBoardConfig_NilOverrideIsBase(t *testing.T) {
	base := BoardConfig{ReviewColumn: "auto", AutoAssignReviewer: true, DefaultWorkerSlots: 4}
	assert.Equal(t, base, MergeBoardConfig(base, nil))
	assert.Equal(t, base, MergeBoardConfig(base, &BoardConfigOverride{}))
}

// TestValidateBoardConfigKey pins per-key validation: every legal value
// passes, every malformed value (wrong type, out-of-domain enum, non-
// positive int, non-integral number) rejects as a GuardInput error, and
// any key outside the three known keys rejects.
func TestValidateBoardConfigKey(t *testing.T) {
	cases := []struct {
		name    string
		key     string
		raw     string
		wantErr string
	}{
		{"reviewColumn off", "reviewColumn", `"off"`, ""},
		{"reviewColumn manual", "reviewColumn", `"manual"`, ""},
		{"reviewColumn auto", "reviewColumn", `"auto"`, ""},
		{"reviewColumn bad enum", "reviewColumn", `"maybe"`, "reviewColumn"},
		{"reviewColumn wrong type", "reviewColumn", `true`, "reviewColumn"},
		{"autoAssignReviewer true", "autoAssignReviewer", `true`, ""},
		{"autoAssignReviewer false", "autoAssignReviewer", `false`, ""},
		{"autoAssignReviewer wrong type", "autoAssignReviewer", `"true"`, "autoAssignReviewer"},
		{"defaultWorkerSlots one", "defaultWorkerSlots", `1`, ""},
		{"defaultWorkerSlots large", "defaultWorkerSlots", `16`, ""},
		{"defaultWorkerSlots zero", "defaultWorkerSlots", `0`, "defaultWorkerSlots"},
		{"defaultWorkerSlots negative", "defaultWorkerSlots", `-2`, "defaultWorkerSlots"},
		{"defaultWorkerSlots fractional", "defaultWorkerSlots", `2.5`, "defaultWorkerSlots"},
		{"defaultWorkerSlots wrong type", "defaultWorkerSlots", `"3"`, "defaultWorkerSlots"},
		{"unknown key", "reviewQueue", `"off"`, "unknown"},
		{"case variant key is unknown", "ReviewColumn", `"auto"`, "unknown"},
		{"junk value", "reviewColumn", `{}`, "reviewColumn"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateBoardConfigKey(tc.key, json.RawMessage(tc.raw))
			if tc.wantErr == "" {
				assert.NoError(t, err)
				return
			}
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.wantErr)
			guard, ok := err.(*GuardError)
			require.True(t, ok, "validation failures render the GuardInput (400) class, got %T", err)
			assert.Equal(t, GuardInput, guard.Code)
		})
	}
}

// TestDecodeBoardConfigOverride round-trips a raw key/value map into the
// typed partial override, and rejects maps carrying unknown keys
// (nothing outside the known key set can EVER land in storage — writes
// validate first, reads fail closed on drift).
func TestDecodeBoardConfigOverride(t *testing.T) {
	t.Run("decodes all known keys", func(t *testing.T) {
		o, err := DecodeBoardConfigOverride(map[string]json.RawMessage{
			"reviewColumn":       json.RawMessage(`"auto"`),
			"autoAssignReviewer": json.RawMessage(`true`),
			"defaultWorkerSlots": json.RawMessage(`4`),
		})
		require.NoError(t, err)
		require.NotNil(t, o.ReviewColumn)
		require.NotNil(t, o.AutoAssignReviewer)
		require.NotNil(t, o.DefaultWorkerSlots)
		assert.Equal(t, "auto", *o.ReviewColumn)
		assert.True(t, *o.AutoAssignReviewer)
		assert.Equal(t, 4, *o.DefaultWorkerSlots)
	})

	t.Run("empty map decodes to the empty override", func(t *testing.T) {
		o, err := DecodeBoardConfigOverride(map[string]json.RawMessage{})
		require.NoError(t, err)
		assert.Nil(t, o.ReviewColumn)
		assert.Nil(t, o.AutoAssignReviewer)
		assert.Nil(t, o.DefaultWorkerSlots)
	})

	t.Run("unknown key rejects", func(t *testing.T) {
		_, err := DecodeBoardConfigOverride(map[string]json.RawMessage{
			"reviewColumn": json.RawMessage(`"auto"`),
			"nope":         json.RawMessage(`1`),
		})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "unknown")
	})

	t.Run("malformed value rejects", func(t *testing.T) {
		_, err := DecodeBoardConfigOverride(map[string]json.RawMessage{
			"defaultWorkerSlots": json.RawMessage(`0`),
		})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "defaultWorkerSlots")
	})
}

// TestBoardConfigOverrideWireShape pins the camelCase wire encoding and
// the omitempty partial-render: an override with one key set renders
// exactly that key (unset keys are absent, never null).
func TestBoardConfigOverrideWireShape(t *testing.T) {
	b, err := json.Marshal(&BoardConfigOverride{ReviewColumn: strPtr("auto")})
	require.NoError(t, err)
	assert.JSONEq(t, `{"reviewColumn":"auto"}`, string(b))

	eff, err := json.Marshal(BoardConfig{ReviewColumn: "manual", AutoAssignReviewer: false, DefaultWorkerSlots: 2})
	require.NoError(t, err)
	assert.JSONEq(t, `{"reviewColumn":"manual","autoAssignReviewer":false,"defaultWorkerSlots":2}`, string(eff))
}

// TestBoardConfigOverrideRowsRoundTrip pins the storage encoding seam:
// set keys encode to key/value rows; the rows decode back to the same
// partial override (the per-key jsonb row shape mirrors
// instance_settings).
func TestBoardConfigOverrideRowsRoundTrip(t *testing.T) {
	o := &BoardConfigOverride{AutoAssignReviewer: boolPtr(true), DefaultWorkerSlots: intPtr(6)}
	rows := o.rows()
	assert.Equal(t, []configRow{
		{Key: "autoAssignReviewer", Value: json.RawMessage(`true`)},
		{Key: "defaultWorkerSlots", Value: json.RawMessage(`6`)},
	}, rows)

	back, err := DecodeBoardConfigOverride(rowsMap(rows))
	require.NoError(t, err)
	require.NotNil(t, back.AutoAssignReviewer)
	require.NotNil(t, back.DefaultWorkerSlots)
	assert.Nil(t, back.ReviewColumn)
	assert.True(t, *back.AutoAssignReviewer)
	assert.Equal(t, 6, *back.DefaultWorkerSlots)

	// The empty override encodes to no rows (absent = inherit).
	assert.Empty(t, (&BoardConfigOverride{}).rows())
}

func rowsMap(rows []configRow) map[string]json.RawMessage {
	m := map[string]json.RawMessage{}
	for _, r := range rows {
		m[r.Key] = r.Value
	}
	return m
}

// TestBoardConfigKnownKeys pins the key vocabulary (room to grow: add a
// key here + its validation + a struct field).
func TestBoardConfigKnownKeys(t *testing.T) {
	keys := BoardConfigKeys()
	assert.ElementsMatch(t, []string{"reviewColumn", "autoAssignReviewer", "defaultWorkerSlots"}, keys)
	for _, k := range keys {
		assert.False(t, strings.Contains(k, " "), "config keys are camelCase wire names")
	}
}
