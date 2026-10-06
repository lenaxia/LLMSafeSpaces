// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package secrets

import (
	"fmt"
	"regexp"
	"strings"
)

// Epic 55 identity validation. The constants below are the single source
// of truth for the kind enum and the slug regex. The DB CHECK constraints
// in api/migrations/000001_initial_schema.up.sql MUST stay in lockstep:
//
//   - kind CHECK: provider_credentials_kind_check
//   - slug CHECK: provider_credentials_slug_check
//
// A property test in pkg/secrets/credential_identity_test.go pins the
// Go-side and SQL-side definitions together.

// ValidKinds is the canonical SDK-class enum. Adding a new kind requires
// a coordinated migration that extends the DB CHECK constraint.
//
// Order matches the CHECK constraint declaration in the migration so the
// two are visually aligned during review.
var ValidKinds = []string{
	"openai",
	"anthropic",
	"google",
	"opencode",
	"bedrock",
	"azure_openai",
	"vertex",
	"cohere",
	"mistral",
	"perplexity",
	"groq",
	"xai",
	"openrouter",
	"together",
	"openai_compatible",
}

// validKindsSet is a lookup-optimized form of ValidKinds.
var validKindsSet = func() map[string]struct{} {
	m := make(map[string]struct{}, len(ValidKinds))
	for _, k := range ValidKinds {
		m[k] = struct{}{}
	}
	return m
}()

// SlugRegex is the canonical slug-validation regex. Must be byte-identical
// to the DB CHECK regex in 000001_initial_schema.up.sql:
//
//	CHECK (slug ~ '^[a-z0-9]([a-z0-9-]{0,62}[a-z0-9])?$')
//
// Semantics: 1-64 chars, lowercase alphanumeric + hyphens, must start AND
// end with alphanumeric (no leading/trailing hyphen).
const SlugRegex = `^[a-z0-9]([a-z0-9-]{0,62}[a-z0-9])?$`

var slugMatcher = regexp.MustCompile(SlugRegex)

// ValidateKind returns nil if kind is one of the recognized SDK-class
// enum values, otherwise an error describing the rejection. Empty kind
// is rejected with a distinct message so the handler can map both cases
// to HTTP 400 with a clear field-specific error.
func ValidateKind(kind string) error {
	if kind == "" {
		return fmt.Errorf("kind is required")
	}
	if _, ok := validKindsSet[kind]; !ok {
		return fmt.Errorf("kind %q is not a recognized SDK class; valid: %v", kind, ValidKinds)
	}
	return nil
}

// ValidateSlug returns nil if slug matches the canonical slug regex,
// otherwise an error describing the rejection.
//
// The error message names the constraint (length, charset, anchors)
// rather than echoing the regex verbatim, which is more useful to API
// consumers than "regex did not match".
func ValidateSlug(slug string) error {
	if slug == "" {
		return fmt.Errorf("slug is required")
	}
	if len(slug) > 64 {
		return fmt.Errorf("slug must be 1-64 characters (got %d)", len(slug))
	}
	if !slugMatcher.MatchString(slug) {
		return fmt.Errorf("slug must match %s (lowercase alphanumeric and hyphens, no leading/trailing hyphen)", SlugRegex)
	}
	return nil
}

// ValidateModelAllowlist rejects model_allowlist entries that can never
// reference a real catalog model: empty or whitespace-affected ids
// (#1575 ask 2 — mis-form protection moved from a silent delivery-time
// strip to a real create/update error). The literal "default" is NOT
// rejected: providers legitimately serve models named "default"
// (routers/aliases commonly do), and delivery decides it against the
// live catalog.
func ValidateModelAllowlist(ids []string) error {
	for _, id := range ids {
		if id == "" {
			return fmt.Errorf("model_allowlist ids must be non-empty (got \"\")")
		}
		if trimmed := strings.TrimSpace(id); trimmed != id {
			return fmt.Errorf("model_allowlist ids must carry no leading/trailing whitespace (got %q)", id)
		}
	}
	return nil
}
