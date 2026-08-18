// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package pgarray

import (
	"database/sql/driver"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Wire compatibility with lib/pq v1.12.3 — every expectation below was
// probed against the real pq.Array before writing this package
// (see package doc). If pq semantics ever matter again, these tests pin
// the behavior we shipped against.

func TestValue_Matrix(t *testing.T) {
	tests := []struct {
		name string
		in   []string
		want driver.Value
	}{
		{"nil slice → SQL NULL", nil, nil},
		{"empty → {}", []string{}, `{}`},
		{"single quoted", []string{"a"}, `{"a"}`},
		{"comma element escaped", []string{"a,b", "c"}, `{"a,b","c"}`},
		{"quote element escaped", []string{`c"d`}, `{"c\"d"}`},
		{"backslash escaped", []string{`e\f`}, `{"e\\f"}`},
		{"empty-string element", []string{"a", ""}, `{"a",""}`},
		{"unicode passthrough", []string{"héllo wörld"}, `{"héllo wörld"}`},
		{"whitespace passthrough", []string{"\tnl\n"}, "{\"\tnl\n\"}"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := Array(tt.in).Value()
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestScan_Matrix(t *testing.T) {
	tests := []struct {
		name    string
		src     any
		want    []string
		wantNil bool
	}{
		{"NULL → nil slice", nil, nil, true},
		{"{} → empty non-nil", []byte(`{}`), []string{}, false},
		{"string input", `{a,"b c"}`, []string{"a", "b c"}, false},
		{"byte input", []byte(`{"a,b"}`), []string{"a,b"}, false},
		{"escapes", []byte(`{"c\"d","e\\f"}`), []string{`c"d`, `e\f`}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var out []string
			require.NoError(t, Array(&out).Scan(tt.src))
			if tt.wantNil {
				assert.Nil(t, out)
			} else {
				assert.Equal(t, tt.want, out)
				assert.NotNil(t, out)
			}
		})
	}
}

func TestScan_Errors(t *testing.T) {
	t.Run("NULL element", func(t *testing.T) {
		var out []string
		require.Error(t, Array(&out).Scan(`{NULL}`), "string column cannot hold SQL NULL")
	})
	t.Run("malformed literal", func(t *testing.T) {
		var out []string
		require.Error(t, Array(&out).Scan(`not-an-array`))
	})
	t.Run("unterminated quote", func(t *testing.T) {
		var out []string
		require.Error(t, Array(&out).Scan(`{"a`))
	})
	t.Run("unsupported src type", func(t *testing.T) {
		var out []string
		require.Error(t, Array(&out).Scan(42))
	})
}

// Round-trip through encode→decode for adversarial content.
func TestRoundTrip_Adversarial(t *testing.T) {
	in := []string{"", `,`, `"`, `\`, `{}`, "a\"b\\c,d", "héllo", "\t\n "}
	v, err := Array(in).Value()
	require.NoError(t, err)
	var out []string
	require.NoError(t, Array(&out).Scan(v))
	assert.Equal(t, in, out)
}

// Named slice types scan natively — the capability pq.Array lacked and
// the reason the codebase carried (*[]string) casts.
type selection []string

func TestArray_NamedTypeScan(t *testing.T) {
	var sel selection
	require.NoError(t, Array(&sel).Scan(`{"k"}`))
	assert.Equal(t, selection{"k"}, sel)

	// Value side on named types too.
	v, err := Array(selection{"z"}).Value()
	require.NoError(t, err)
	assert.Equal(t, `{"z"}`, v)
}

func TestStringArray_Type(t *testing.T) {
	v, err := StringArray{"a", "b"}.Value()
	require.NoError(t, err)
	assert.Equal(t, `{"a","b"}`, v)

	var s StringArray
	require.NoError(t, s.Scan([]byte(`{"x","y"}`)))
	assert.Equal(t, StringArray{"x", "y"}, s)
}

func TestArray_PanicsOnMisuse(t *testing.T) {
	assert.Panics(t, func() { Array((*[]string)(nil)) }, "nil pointer: programming error")
	assert.Panics(t, func() { Array(42) }, "non-slice: programming error")
}

// Scan into a non-pointer slice must fail loudly (driver misuse), not
// silently no-op.
func TestArray_ScanIntoNonPointerFails(t *testing.T) {
	s := []string{"orig"}
	v, err := Array(s).Value()
	require.NoError(t, err)
	assert.Panics(t, func() { _ = Array(s).Scan(v) })
}
