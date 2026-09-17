// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package redact_test

import (
	"encoding/base64"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/lenaxia/llmsafespaces/pkg/redact"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const stagedReplacement = "[REDACTED-STAGED-KEY]"

func TestRegisterDynamicRedactsExactValue(t *testing.T) {
	tests := []struct {
		name  string
		value string
		input string
	}{
		{
			name:  "bare value",
			value: "k1AbCdE9x2",
			input: "the key is k1AbCdE9x2 and that is all",
		},
		{
			name:  "value embedded in JSON field",
			value: "k1AbCdE9x2",
			input: `{"apiKey":"k1AbCdE9x2","model":"gpt-x"}`,
		},
		{
			name:  "value with regex metacharacters is matched literally",
			value: "a.b*c(d)e[f]",
			input: "leak: a.b*c(d)e[f] end",
		},
		{
			name:  "multiple occurrences all redacted",
			value: "k1AbCdE9x2",
			input: "first k1AbCdE9x2 then k1AbCdE9x2 again",
		},
		{
			name:  "empty replacement deletes the value",
			value: "k1AbCdE9x2",
			input: "leak k1AbCdE9x2 end",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r, err := redact.NewRedactor(nil)
			require.NoError(t, err)

			replacement := stagedReplacement
			if tc.name == "empty replacement deletes the value" {
				replacement = ""
			}
			require.NoError(t, r.RegisterDynamic(redact.DynamicRule{
				ID:          "staged:test",
				Value:       tc.value,
				Replacement: replacement,
			}))

			out, err := r.Redact(tc.input)
			require.NoError(t, err)
			assert.NotContains(t, out, tc.value)
			if replacement != "" {
				assert.Contains(t, out, replacement)
			}
		})
	}
}

func TestRegisterDynamicAppliesBeforeStaticRules(t *testing.T) {
	r, err := redact.NewRedactor(nil)
	require.NoError(t, err)

	// The static `token=` rule would rewrite the tail of this value to
	// `token=[REDACTED]`, fragmenting it. The dynamic exact-value rule must
	// run first so the whole secret is removed atomically.
	value := "prefix-token=hunter2secret"
	require.NoError(t, r.RegisterDynamic(redact.DynamicRule{
		ID:          "staged:order",
		Value:       value,
		Replacement: stagedReplacement,
	}))

	out, err := r.Redact("leak prefix-token=hunter2secret end")
	require.NoError(t, err)
	assert.Equal(t, "leak "+stagedReplacement+" end", out)
}

func TestRegisterDynamicReplacesExistingGroup(t *testing.T) {
	r, err := redact.NewRedactor(nil)
	require.NoError(t, err)

	require.NoError(t, r.RegisterDynamic(redact.DynamicRule{
		ID: "staged:rotate", Value: "oldkey123", Replacement: stagedReplacement,
	}))
	require.NoError(t, r.RegisterDynamic(redact.DynamicRule{
		ID: "staged:rotate", Value: "newkey456", Replacement: stagedReplacement,
	}))

	out, err := r.Redact("oldkey123 and newkey456")
	require.NoError(t, err)
	assert.Contains(t, out, "oldkey123", "re-registering an ID must drop the old value's rule")
	assert.NotContains(t, out, "newkey456")
}

func TestUnregisterDynamicRemovesProtection(t *testing.T) {
	r, err := redact.NewRedactor(nil)
	require.NoError(t, err)

	require.NoError(t, r.RegisterDynamic(redact.DynamicRule{
		ID: "staged:revoke", Value: "k1AbCdE9x2", Replacement: stagedReplacement,
	}))
	out, err := r.Redact("has k1AbCdE9x2 inside")
	require.NoError(t, err)
	assert.NotContains(t, out, "k1AbCdE9x2")

	r.UnregisterDynamic("staged:revoke")
	out, err = r.Redact("has k1AbCdE9x2 inside")
	require.NoError(t, err)
	assert.Contains(t, out, "k1AbCdE9x2", "after unregistration the exact value is no longer redacted")

	// Unregistering an unknown ID is a no-op, not an error path.
	r.UnregisterDynamic("staged:never-registered")
}

func TestRegisterDynamicValidation(t *testing.T) {
	r, err := redact.NewRedactor(nil)
	require.NoError(t, err)

	tests := []struct {
		name  string
		rules []redact.DynamicRule
	}{
		{"empty ID", []redact.DynamicRule{{ID: "", Value: "v", Replacement: "r"}}},
		{"empty value", []redact.DynamicRule{{ID: "id", Value: "", Replacement: "r"}}},
		{"empty rules slice", nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if tc.name == "empty rules slice" {
				require.NoError(t, r.RegisterDynamic(tc.rules...))
				return
			}
			require.Error(t, r.RegisterDynamic(tc.rules...))
			// A failed registration must not leave partial state behind.
			out, err := r.Redact("v")
			require.NoError(t, err)
			assert.Equal(t, "v", out)
		})
	}
}

func TestRegisterDynamicBinaryValue(t *testing.T) {
	r, err := redact.NewRedactor(nil)
	require.NoError(t, err)

	raw := []byte{0x01, 0xff, 0xfe, 'a', 'b', 0x00, 'c'}
	require.NoError(t, r.RegisterDynamic(redact.DynamicRule{
		ID: "staged:binary", Value: string(raw), Replacement: stagedReplacement,
	}))

	payload := "prefix:" + string(raw) + ":suffix"
	out, err := r.Redact(payload)
	require.NoError(t, err)
	assert.Equal(t, "prefix:"+stagedReplacement+":suffix", out)
}

func TestStaticPipelineGolden(t *testing.T) {
	// Golden pin of the 16 static rules' behavior, one row per rule, so the
	// dynamic-rule extension cannot change static behavior unnoticed. Run
	// with and without a registered dynamic rule.
	golden := []struct {
		name  string
		input string
		want  string
	}{
		{"1 url creds", "https://user:pass@example.com/db", "https://[REDACTED]@example.com/db"},
		{"2 bearer", "Authorization: Bearer abc.def.ghi", "Authorization: [REDACTED] [REDACTED]"},
		{"3 github token", "ghp_ABCDEFGHIJKLMNOPQRSTUVWXYZ1234567890", "[REDACTED-GH-TOKEN]"},
		{"4 json password", `{"password":"hunter2"}`, `{"password":"[REDACTED]"}`},
		{"5 password equals", "password=hunter2", "password=[REDACTED]"},
		{"6 token equals", "token=hunter2", "token=[REDACTED]"},
		{"7 secret equals", "secret=hunter2", "secret=[REDACTED]"},
		{"8 api key equals", "api_key=hunter2abc123", "api_key=[REDACTED]"},
		{"9 x-api-key", "x-api-key: hunter2abc123", "x-api-key: [REDACTED]"},
		{
			"10 pem key",
			"-----BEGIN RSA PRIVATE KEY-----\nMIIEowIBAAK\n-----END RSA PRIVATE KEY-----",
			"[REDACTED-PEM-KEY]",
		},
		{
			"11 age key",
			"AGE-SECRET-KEY-1QYQSZQGPQYQSZQGPQYQSZQGPQYQSZQGPQYQSZQGPQYQ",
			"[REDACTED-AGE-KEY]",
		},
		{"12 sk key", "sk-abcdABCD1234567890abcdABCD1234567890", "[REDACTED-SK-KEY]"},
		{"13 aws key", "AKIAIOSFODNN7EXAMPLE", "[REDACTED-AWS-KEY]"},
		{
			"14 jwt",
			"eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxIn0.SflKxwRJSMeK",
			"[REDACTED-JWT].SflKxwRJSMeK",
		},
		{"15 authorization header", "authorization: Basic abcdef123456", "authorization: [REDACTED] abcdef123456"},
		{
			"16 long base64",
			"ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/",
			"[REDACTED-BASE64]",
		},
	}
	for _, mode := range []string{"no-dynamic", "with-dynamic"} {
		t.Run(mode, func(t *testing.T) {
			r, err := redact.NewRedactor(nil)
			require.NoError(t, err)
			if mode == "with-dynamic" {
				require.NoError(t, r.RegisterDynamic(redact.DynamicRule{
					ID: "staged:golden", Value: "k1AbCdE9x2", Replacement: stagedReplacement,
				}))
			}
			for _, g := range golden {
				t.Run(g.name, func(t *testing.T) {
					out, err := r.Redact(g.input)
					require.NoError(t, err)
					assert.Equal(t, g.want, out)
				})
			}
		})
	}
}

func TestPackageLevelRegisterDynamic(t *testing.T) {
	id := "staged:package-level-test"
	value := "pkgLvlKey9x8y7z"
	require.NoError(t, redact.RegisterDynamic(redact.DynamicRule{
		ID: id, Value: value, Replacement: stagedReplacement,
	}))
	defer redact.UnregisterDynamic(id)

	out, err := redact.Redact("echo " + value + " back")
	require.NoError(t, err)
	assert.NotContains(t, out, value)
	assert.Contains(t, out, stagedReplacement)

	redact.UnregisterDynamic(id)
	out, err = redact.Redact("echo " + value + " back")
	require.NoError(t, err)
	assert.Contains(t, out, value)
}

func TestDynamicPrefixOverlapAtomicAndDeterministic(t *testing.T) {
	// R1 regression pin (review iteration 1): when one registered value is a
	// strict prefix of another, the shorter firing first would fragment the
	// longer and leak its tail — and map-iteration order made that
	// non-deterministic. Longest-first snapshot ordering must keep removal
	// atomic and the output identical on every run.
	r, err := redact.NewRedactor(nil)
	require.NoError(t, err)

	short := "sk-proj-sharedprefix"
	long := "sk-proj-sharedprefixAAAsuffixXYZ"
	require.NoError(t, r.RegisterDynamic(
		redact.DynamicRule{ID: "staged:short", Value: short, Replacement: stagedReplacement},
		redact.DynamicRule{ID: "staged:long", Value: long, Replacement: stagedReplacement},
	))

	payload := "leak: " + long + " and lone " + short + " end"
	want := "leak: " + stagedReplacement + " and lone " + stagedReplacement + " end"
	for i := 0; i < 500; i++ {
		out, err := r.Redact(payload)
		require.NoError(t, err)
		if out != want {
			t.Fatalf("iteration %d: non-deterministic or residue-leaking output: %q", i, out)
		}
	}
	assert.NotContains(t, want, "AAAsuffixXYZ")
}

func TestDynamicRulesConcurrent(t *testing.T) {
	r, err := redact.NewRedactor(nil)
	require.NoError(t, err)
	require.NoError(t, r.RegisterDynamic(redact.DynamicRule{
		ID: "staged:concurrent", Value: "concurrentKey1", Replacement: stagedReplacement,
	}))

	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			id := fmt.Sprintf("staged:concurrent-%d", i)
			value := fmt.Sprintf("concurrentKey%d", i)
			for j := 0; j < 50; j++ {
				if err := r.RegisterDynamic(redact.DynamicRule{
					ID: id, Value: value, Replacement: stagedReplacement,
				}); err != nil {
					errs <- fmt.Errorf("worker %d iter %d: register: %w", i, j, err)
					return
				}
				out, err := r.Redact("x " + value + " y token=leaked")
				if err != nil {
					errs <- fmt.Errorf("worker %d iter %d: redact: %w", i, j, err)
					return
				}
				if strings.Contains(out, value) || !strings.Contains(out, "token=[REDACTED]") {
					errs <- fmt.Errorf("worker %d iter %d: bad output %q", i, j, out)
					return
				}
				r.UnregisterDynamic(id)
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
}

func TestDynamicRuleBase64EncodingCaught(t *testing.T) {
	// Demonstrates the registration policy the staging provider uses: an
	// exact-value rule per plausible encoding of the key material, so a key
	// echoed base64-wrapped is caught even though no static rule matches a
	// short encoded blob.
	r, err := redact.NewRedactor(nil)
	require.NoError(t, err)

	material := []byte("k1AbCdE9x2") // short enough to evade every static rule
	rules := []redact.DynamicRule{
		{ID: "staged:enc", Value: string(material), Replacement: stagedReplacement},
		{ID: "staged:enc", Value: base64.StdEncoding.EncodeToString(material), Replacement: stagedReplacement},
		{ID: "staged:enc", Value: base64.URLEncoding.EncodeToString(material), Replacement: stagedReplacement},
	}
	require.NoError(t, r.RegisterDynamic(rules...))

	for _, enc := range []string{
		string(material),
		base64.StdEncoding.EncodeToString(material),
		base64.URLEncoding.EncodeToString(material),
	} {
		payload := "smuggled [" + enc + "] in transit"
		out, err := r.Redact(payload)
		require.NoError(t, err)
		assert.True(t, !strings.Contains(out, enc), "encoding %q leaked: %s", enc, out)
	}
}
