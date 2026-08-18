// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package database

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestStringArray_RoundTrip pins pq.Array-compatible encoding for every
// shape the store binds: empty, nil, plain, and elements containing the
// three characters that must be escaped (quote, backslash, comma).
func TestStringArray_RoundTrip(t *testing.T) {
	cases := []struct {
		name string
		in   []string
	}{
		{"nil", nil},
		{"empty", []string{}},
		{"single", []string{"ffmpeg"}},
		{"multi", []string{"ffmpeg", "python313", "motd-welcome"}},
		{"escapes", []string{`say "hi"`, `back\slash`, `com,ma`}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			v, err := stringArray(tc.in).Value()
			require.NoError(t, err)
			if tc.in == nil {
				assert.Nil(t, v)
				return
			}
			var got stringArray
			require.NoError(t, got.Scan(v))
			assert.Equal(t, stringArray(tc.in), got)
		})
	}
}

func TestStringArray_Scan_BadInput(t *testing.T) {
	var a stringArray
	assert.Error(t, a.Scan(42), "unsupported src type must error, not panic")
	var b stringArray
	require.NoError(t, b.Scan(nil))
	assert.Nil(t, b)
	var c stringArray
	require.NoError(t, c.Scan("NULL"))
	assert.Nil(t, c)
}
