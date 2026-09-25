package utilities

import (
	"fmt"
	"strings"
	"testing"
	"time"

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
		// The escape-shadowed shape (the issue thread's own callout):
		// \u0073 escapes to "s", so both keys are "session_id" — a
		// raw-text comparison would MISS this; the Decoder resolves
		// escapes before comparison and the duplicate is caught.
		{"escape-shadowed duplicate key", `{"\u0073ession_id":1,"session_id":2}`, []string{"$.session_id"}},
		// Distinct-after-escape-resolution only via DIFFERENT strings
		// stays clean: same escapes, different keys.
		{"escape-resolved distinct keys", `{"\u0073ession_id":1,"\u0074ession_id":2}`, nil},
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

// The complexity pin (r1's validated finding): path construction is
// LAZY — duplicates only. The reviewer's adversarial shape (deep
// ancestor chain + wide key fan, fully VALID, sized under the MCP
// 1MiB cap and the stdlib decoder's 10000-depth limit) made the
// eager scan burn ~4.7s of single-thread CPU (~170x the stdlib
// decode) on the authenticated tools/call seam. The lazy scan must
// stay in the same cost class as the decode itself; the bound is
// generous (CI hardware varies) but two orders below the quadratic.
func TestFindDuplicateKeys_NoQuadraticBlowup(t *testing.T) {
	// ~530KB valid body: 2000-level nest chain, then a 40K-key flat
	// object at the bottom (every key pays full ancestor depth under
	// the eager build — the amplification shape).
	var b strings.Builder
	b.WriteString(`{"a1":{"a2":`)
	for i := 3; i <= 2000; i++ {
		fmt.Fprintf(&b, `{"a%d":`, i)
	}
	b.WriteString(`{"flat":{`)
	for i := 0; i < 40000; i++ {
		if i > 0 {
			b.WriteString(",")
		}
		fmt.Fprintf(&b, `"k%d":1`, i)
	}
	b.WriteString("}}")
	for i := 2000; i >= 3; i-- {
		b.WriteString("}")
	}
	b.WriteString("}}")
	body := []byte(b.String())
	require.Less(t, len(body), 1<<20, "adversarial shape must stay under the MCP body cap")

	start := time.Now()
	dups, err := FindDuplicateKeys(body)
	elapsed := time.Since(start)
	require.NoError(t, err)
	assert.Empty(t, dups)
	assert.Less(t, elapsed, 1500*time.Millisecond,
		"the lazy-path scan must not regress to quadratic (eager build measured ~4.7s on this class; decode alone is ~tens of ms)")
}

// Laziness must not cost detection: the SAME deep shape with one
// duplicate at the bottom still reports it, path intact.
func TestFindDuplicateKeys_DeepDuplicateStillFound(t *testing.T) {
	var b strings.Builder
	for i := 0; i < 3000; i++ {
		b.WriteString(`{"n":`)
	}
	b.WriteString(`{"x":1,"x":2}`)
	for i := 0; i < 3000; i++ {
		b.WriteString("}")
	}
	dups, err := FindDuplicateKeys([]byte(b.String()))
	require.NoError(t, err)
	require.Len(t, dups, 1)
	assert.True(t, strings.HasPrefix(dups[0], "$"), "the lazy path still renders the full ancestor path: %s", dups[0])
	assert.True(t, strings.HasSuffix(dups[0], ".x"), "the leaf key is named: %s", dups[0])
}
