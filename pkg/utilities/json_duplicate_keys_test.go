package utilities

import (
	"encoding/json"
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

// The complexity pin (r2's discriminating rewrite): path construction
// is LAZY — duplicates only. r1's absolute 1.5s bound did NOT
// discriminate (the known-bad eager implementation passed at ~1.0s on
// CI-class hardware); this pin scales the adversarial shape toward
// the MCP 1MiB cap AND ratio-pins the scan against the stdlib decode
// of the SAME body — self-normalizing across hardware. The lazy scan
// sits in the decode's own cost class (single-digit multiples); the
// eager quadratic sits orders above it (measured in the r2 mutation
// run: see the worklog — red by >4x against the restored eager
// implementation on this shape).
func TestFindDuplicateKeys_NoQuadraticBlowup(t *testing.T) {
	// ~620KB valid body: an 8000-level nest chain (within the stdlib
	// decoder's 10000-depth limit) ending in a 60K-key flat object —
	// every key pays the full ancestor depth under the eager build.
	var b strings.Builder
	b.WriteString(`{"a1":{"a2":`)
	for i := 3; i <= 8000; i++ {
		fmt.Fprintf(&b, `{"a%d":`, i)
	}
	b.WriteString(`{"flat":{`)
	for i := 0; i < 60000; i++ {
		if i > 0 {
			b.WriteString(",")
		}
		fmt.Fprintf(&b, `"k%d":1`, i)
	}
	b.WriteString("}}")
	for i := 8000; i >= 3; i-- {
		b.WriteString("}")
	}
	b.WriteString("}}")
	body := []byte(b.String())
	require.Less(t, len(body), 1<<20, "adversarial shape must stay under the MCP body cap")

	// Baseline: the stdlib decode of the same body — the pre-scanner
	// cost the seam already pays.
	decodeStart := time.Now()
	var into any
	require.NoError(t, json.Unmarshal(body, &into))
	decodeTime := time.Since(decodeStart)
	require.Greater(t, decodeTime, time.Millisecond,
		"decode baseline must be above timing noise for the ratio to be meaningful")

	scanStart := time.Now()
	dups, err := FindDuplicateKeys(body)
	scanTime := time.Since(scanStart)
	require.NoError(t, err)
	assert.Empty(t, dups)

	// Ratio: lazy sits within a small multiple of the decode; eager
	// sits orders above (the r2 mutation run on this shape measured
	// the restored eager build at 8.3s scan vs ~0.13s lazy — red by
	// 5.5x against the absolute belt alone).
	assert.LessOrEqual(t, scanTime, 20*decodeTime,
		"scan (%s) must stay within 20x the stdlib decode (%s) — the eager quadratic path build sits far outside",
		scanTime, decodeTime)
	assert.Less(t, scanTime, 1500*time.Millisecond,
		"absolute belt on top of the ratio (lazy scans this shape in ~100ms class; the restored eager build measured multi-second in the r2 mutation run)")
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
