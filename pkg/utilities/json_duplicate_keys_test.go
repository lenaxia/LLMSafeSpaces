package utilities

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// FindDuplicateKeys pins the detector that backs the MCP seam's
// -32602 duplicate-key refusal (#1530 comment-thread exposure: Go's
// encoding/json collapses duplicated object keys silently, last wins
// — the KEY twin of the value-fragment emission slip the recovery
// guard handles; keys must misbind NEVER, so the seam refuses).
func TestFindDuplicateKeys(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want []string
	}{
		{"clean flat object", `{"a":1,"b":2}`, nil},
		{"nested clean", `{"params":{"arguments":{"session_id":"ses_1","message":"hi"}}}`, nil},
		{"duplicated key at top level", `{"jsonrpc":"2.0","jsonrpc":"2.0"}`, []string{"$.jsonrpc"}},
		{"duplicated key nested", `{"params":{"arguments":{"session_id":"ses_A","session_id":"ses_B"}}}`,
			[]string{"$.params.arguments.session_id"}},
		{"two duplicates reported", `{"a":1,"a":2,"b":{"c":1,"c":2}}`, []string{"$.a", "$.b.c"}},
		{"same key in DIFFERENT objects is not a duplicate", `{"a":{"x":1},"b":{"x":2}}`, nil},
		{"key repeated across array elements is fine", `{"arr":[{"x":1},{"x":2}]}`, nil},
		{"duplicate inside array element", `{"arr":[{"x":1,"x":2}]}`, []string{"$.arr[0].x"}},
		{"empty object and array", `{"a":{},"b":[]}`, nil},
		{"scalars, nulls, escapes", `{"s":"a\"b","n":null,"t":true,"f":false,"z":0}`, nil},
		{"key with dot and quote chars", `{"a.b":{"c\"d":1,"c\"d":2}}`, []string{`$.a.b.c"d`}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := FindDuplicateKeys([]byte(tc.in))
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
		})
	}
}

// Malformed input must ERROR (never silently pass): the seam's
// refusal decision depends on the scanner being sound on every input
// it accepts.
func TestFindDuplicateKeys_MalformedErrors(t *testing.T) {
	for _, bad := range []string{
		``,
		`{`,
		`{"a":1,`,
		`{"a":1}}`,
		`not json`,
		`{"a":1} trailing`,
		`[1,2,`,
	} {
		_, err := FindDuplicateKeys([]byte(bad))
		assert.Error(t, err, "input %q must error", bad)
	}
}
