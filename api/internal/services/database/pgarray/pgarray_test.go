// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package pgarray

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type NamedStrings []string // mirrors database.Selection-style named slices

func TestValueRendersPqCompatibleLiteral(t *testing.T) {
	tests := []struct {
		name string
		in   any
		want any
	}{
		{"two strings", []string{"bookworm", "trixie"}, `{"bookworm","trixie"}`},
		{"single", []string{"ffmpeg"}, `{"ffmpeg"}`},
		{"empty slice", []string{}, `{}`},
		{"nil slice binds NULL", []string(nil), nil},
		{"nil pointer binds NULL", (*[]string)(nil), nil},
		{"named slice type", NamedStrings{"a", "b"}, `{"a","b"}`},
		{"pointer to slice", &[]string{"x"}, `{"x"}`},
		{"quoted on special chars", []string{`a,b`, `c"d`, `e\f`, `{g}`}, `{"a,b","c\"d","e\\f","{g}"}`},
		{"empty string element", []string{""}, `{""}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := New(tt.in).Value()
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestValueRejectsNonStringSlices(t *testing.T) {
	_, err := New([]int{1, 2}).Value()
	assert.Error(t, err, "numeric arrays are out of scope by design; fail loudly if one appears")
	_, err = New("not a slice").Value()
	assert.Error(t, err)
}

func TestScanParsesBothPqAndPostgresLiteralForms(t *testing.T) {
	tests := []struct {
		name string
		src  any
		want []string
	}{
		{"pq bind form (all quoted)", `{"bookworm","trixie"}`, []string{"bookworm", "trixie"}},
		{"postgres output form (unquoted)", "{bookworm,trixie}", []string{"bookworm", "trixie"}},
		{"single", "{ffmpeg}", []string{"ffmpeg"}},
		{"bytes type", []byte("{a,b}"), []string{"a", "b"}},
		{"empty array", "{}", []string{}},
		{"SQL NULL as nil", nil, nil},
		{"NULL literal", "NULL", nil},
		{"empty string", "", nil},
		{"quoted elements", `{"a,b","c\"d"}`, []string{"a,b", `c"d`}},
		{"escaped backslash", `{"e\\f"}`, []string{`e\f`}},
		{"empty string element", `{""}`, []string{""}},
		{"quoted brace elem", `{"{g}"}`, []string{"{g}"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var dst []string
			require.NoError(t, New(&dst).Scan(tt.src))
			assert.Equal(t, tt.want, dst)
		})
	}
}

func TestScanIntoNamedSliceType(t *testing.T) {
	var sel NamedStrings
	require.NoError(t, New(&sel).Scan("{python313,ffmpeg}"))
	assert.Equal(t, NamedStrings{"python313", "ffmpeg"}, sel)
}

func TestScanNullPreservesNilNotEmpty(t *testing.T) {
	dst := []string{"stale"}
	require.NoError(t, New(&dst).Scan(nil))
	assert.Nil(t, dst, "SQL NULL must produce a nil slice so a later bind emits NULL, not {}")
}

func TestScanRejectsMalformedLiteral(t *testing.T) {
	var dst []string
	err := New(&dst).Scan("bookworm") // missing braces
	assert.Error(t, err, "unbraced literal must not parse")
}

func TestScanRejectsNonPointerDestination(t *testing.T) {
	err := New([]string{}).Scan("{a}")
	assert.Error(t, err, "Scan needs a pointer to write through")
}

func TestRoundTripSemantic(t *testing.T) {
	// Bind always emits lib/pq's fully-quoted form; PostgreSQL's own output
	// quotes only when needed. Both parse; round-trip is asserted
	// semantically, not byte-identically.
	tests := []struct {
		wire string
		want []string
	}{
		{`{bookworm,trixie}`, []string{"bookworm", "trixie"}},
		{`{"bookworm","trixie"}`, []string{"bookworm", "trixie"}},
		{`{ffmpeg}`, []string{"ffmpeg"}},
		{`{}`, []string{}},
	}
	for _, tt := range tests {
		var dst []string
		require.NoError(t, New(&dst).Scan(tt.wire))
		assert.Equal(t, tt.want, dst)
		v, err := New(dst).Value()
		require.NoError(t, err)
		require.NotNil(t, v)
		var back []string
		require.NoError(t, New(&back).Scan(v))
		assert.Equal(t, tt.want, back)
	}
}
