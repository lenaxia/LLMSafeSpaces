// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package secrets

import (
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestValidateKind_AcceptsAllEnumValues asserts every value in ValidKinds
// is accepted. If a kind is added to the enum but not added here, this test
// fails — forcing the test author to acknowledge the new kind.
func TestValidateKind_AcceptsAllEnumValues(t *testing.T) {
	for _, k := range ValidKinds {
		assert.NoError(t, ValidateKind(k), "ValidKinds entry %q must validate", k)
	}
}

// TestValidateKind_RejectsUnknown asserts non-enum values are rejected.
// Note: "custom" is deliberately included — this was the legacy free-form
// SDK kind for OpenAI-compatible endpoints. Epic 55 replaces it with
// "openai_compatible"; the validator must reject the old name so callers
// who haven't migrated get a clear error rather than a silent 500 at DB
// CHECK time.
func TestValidateKind_RejectsUnknown(t *testing.T) {
	cases := []string{
		"",
		"custom",  // legacy pre-Epic-55 value
		"OpenAI",  // wrong case
		"openai ", // trailing space
		"unknown-vendor",
		"openai_compat", // typo
	}
	for _, c := range cases {
		assert.Error(t, ValidateKind(c), "kind %q must be rejected", c)
	}
}

// TestValidateSlug_AcceptsValid covers the boundary cases of the slug
// regex: 1 char, max length, hyphens-internal, numbers, mixed.
func TestValidateSlug_AcceptsValid(t *testing.T) {
	cases := []string{
		"a",
		"openai",
		"thekaocloud",
		"litellm-prod",
		"litellm-prod-us-west",
		"a1",
		"x" + strings.Repeat("a", 62) + "y", // 64 chars, max allowed
		"1a",                                // starts with digit (allowed)
		"a1",
	}
	for _, c := range cases {
		assert.NoError(t, ValidateSlug(c), "slug %q must validate", c)
	}
}

// TestValidateSlug_RejectsInvalid covers every shape the regex must reject.
// Each rejected shape corresponds to an assertion in the SQL test file
// api/migrations/test/000001_initial_schema_test.sql — the two layers must
// agree on every shape.
func TestValidateSlug_RejectsInvalid(t *testing.T) {
	cases := []string{
		"",                            // empty
		"has space",                   // space
		"UPPER",                       // uppercase
		"-leading",                    // leading hyphen
		"trailing-",                   // trailing hyphen
		"has/slash",                   // slash
		"has_underscore",              // underscore (design Q2: hyphens only)
		"x" + strings.Repeat("a", 64), // 65 chars, too long
		"a-",                          // 2 chars ending in hyphen
		"--",                          // pure hyphens
		"a/b",                         // slashes
		"a.b",                         // dots
	}
	for _, c := range cases {
		assert.Error(t, ValidateSlug(c), "slug %q must be rejected", c)
	}
}

// TestSlugRegex_MatchesDBCheckConstraint asserts the Go-side SlugRegex
// constant is byte-identical to the regex literal in the DB CHECK
// constraint. This is the property test that prevents the two layers
// from drifting (the design's N1 concern from epic-55).
//
// Reads api/migrations/000001_initial_schema.up.sql and locates the
// CHECK regex line, then compares.
func TestSlugRegex_MatchesDBCheckConstraint(t *testing.T) {
	// Path is relative to the test file location.
	migrationPath := "../../api/migrations/000001_initial_schema.up.sql"
	raw, err := os.ReadFile(migrationPath)
	require.NoError(t, err, "must be able to read the migration file")

	content := string(raw)

	// Locate the slug CHECK regex. The migration declares it via the
	// inline CONSTRAINT in the CREATE TABLE body OR as an ALTER TABLE
	// ADD CONSTRAINT; we don't care which — we just need to find the
	// regex literal between single quotes.
	//
	// Look for the constraint name we know the migration uses.
	const marker = "provider_credentials_slug_check"
	idx := strings.Index(content, marker)
	require.Greater(t, idx, 0, "slug CHECK constraint not found in migration")

	// Extract the rest of the constraint declaration up to the next
	// closing paren.
	rest := content[idx:]
	// The constraint body is `CHECK ((slug ~ '<REGEX>'::text))` or
	// similar. Find the first '(' after CHECK, then the regex between
	// the first pair of single quotes.
	checkIdx := strings.Index(rest, "CHECK")
	require.GreaterOrEqual(t, checkIdx, 0, "CHECK keyword not found near constraint")
	body := rest[checkIdx:]
	firstQ := strings.Index(body, "'")
	require.GreaterOrEqual(t, firstQ, 0, "opening quote of regex not found")
	secondQ := strings.Index(body[firstQ+1:], "'")
	require.GreaterOrEqual(t, secondQ, 0, "closing quote of regex not found")
	dbRegex := body[firstQ+1 : firstQ+1+secondQ]

	assert.Equal(t, SlugRegex, dbRegex,
		"Go-side SlugRegex must be byte-identical to DB CHECK regex. "+
			"If you changed one, change the other in the same commit. "+
			"This is the cross-layer drift guard from epic-55 design N1.")
}

// TestValidKinds_MatchesDBCheckEnum asserts the Go-side ValidKinds slice
// is byte-identical to the DB CHECK enum values. Same drift-guard pattern
// as the slug regex test.
func TestValidKinds_MatchesDBCheckEnum(t *testing.T) {
	migrationPath := "../../api/migrations/000001_initial_schema.up.sql"
	raw, err := os.ReadFile(migrationPath)
	require.NoError(t, err)

	content := string(raw)

	const marker = "provider_credentials_kind_check"
	idx := strings.Index(content, marker)
	require.Greater(t, idx, 0, "kind CHECK constraint not found in migration")

	// The constraint is `CHECK ((kind = ANY (ARRAY['openai'::text, ...])))`.
	// Extract every quoted string inside the ARRAY[...] block.
	rest := content[idx:]
	arrayStart := strings.Index(rest, "ARRAY[")
	require.GreaterOrEqual(t, arrayStart, 0, "ARRAY[ not found in kind CHECK")
	arrayEnd := strings.Index(rest[arrayStart:], "]")
	require.Greater(t, arrayEnd, 0, "closing ] not found in kind CHECK")
	arrayBody := rest[arrayStart : arrayStart+arrayEnd]

	// Parse out each 'value' literal.
	var dbKinds []string
	for {
		q := strings.Index(arrayBody, "'")
		if q < 0 {
			break
		}
		rest := arrayBody[q+1:]
		end := strings.Index(rest, "'")
		if end < 0 {
			break
		}
		dbKinds = append(dbKinds, rest[:end])
		arrayBody = rest[end+1:]
	}

	require.NotEmpty(t, dbKinds, "must extract at least one kind from CHECK")

	// Compare as sets (order may differ between the Go slice and the
	// SQL ARRAY).
	goSet := make(map[string]bool, len(ValidKinds))
	for _, k := range ValidKinds {
		goSet[k] = true
	}
	dbSet := make(map[string]bool, len(dbKinds))
	for _, k := range dbKinds {
		dbSet[k] = true
	}

	// Find drift either way.
	var missingFromGo, missingFromDB []string
	for k := range dbSet {
		if !goSet[k] {
			missingFromGo = append(missingFromGo, k)
		}
	}
	for k := range goSet {
		if !dbSet[k] {
			missingFromDB = append(missingFromDB, k)
		}
	}

	assert.Empty(t, missingFromGo,
		"DB CHECK enum has kinds the Go ValidKinds slice does not — Go side must be updated")
	assert.Empty(t, missingFromDB,
		"Go ValidKinds slice has kinds the DB CHECK enum does not — DB migration must be updated")
}

// TestValidateModelAllowlist (#1575 ask 2): ids that can never reference
// a real catalog model are rejected at the boundary so operators get a
// real error instead of a silent delivery-time strip. The literal
// "default" is NOT invalid — providers serve models named default.
func TestValidateModelAllowlist(t *testing.T) {
	tests := []struct {
		name    string
		ids     []string
		wantErr bool
	}{
		{"nil allowlist", nil, false},
		{"empty allowlist", []string{}, false},
		{"ordinary ids", []string{"glm-5.1", "gpt-4o"}, false},
		{"default is a real catalog id", []string{"default", "glm-5.1"}, false},
		{"empty string id", []string{"glm-5.1", ""}, true},
		{"whitespace-only id", []string{" "}, true},
		{"leading whitespace", []string{" glm-5.1"}, true},
		{"trailing whitespace", []string{"glm-5.1 "}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateModelAllowlist(tt.ids)
			if (err != nil) != tt.wantErr {
				t.Errorf("ValidateModelAllowlist(%v) error = %v, wantErr %v", tt.ids, err, tt.wantErr)
			}
			if err != nil && !strings.Contains(err.Error(), "model_allowlist") {
				t.Errorf("error must name the field, got %q", err.Error())
			}
		})
	}
}
