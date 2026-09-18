// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package redact

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"regexp"
	"sort"
	"strings"
	"sync"
)

const defaultConfigPath = "/sandbox-cfg/redact-patterns.json"

type Pattern struct {
	Regex       string `json:"regex"`
	Replacement string `json:"replacement"`
}

type compiledPattern struct {
	re          *regexp.Regexp
	replacement string
}

// DynamicRule is an exact-value redaction rule. Unlike the static regex
// pipeline, the Value is matched literally (no regex metacharacter
// interpretation) and byte-exactly, so arbitrary key material — including
// non-UTF-8 bytes — can be registered as a rule. Rules are grouped by ID:
// RegisterDynamic replaces any existing group with the same ID, and
// UnregisterDynamic removes the whole group.
//
// Consumers with exact secret material (e.g. the relay-staging provider,
// which is the one place the platform legitimately knows a staged key's
// plaintext at seal/resolve time) register rules at the moment the material
// is known and unregister on revocation.
type DynamicRule struct {
	ID          string
	Value       string
	Replacement string
}

type dynamicEntry struct {
	value       string
	replacement string
}

type Redactor struct {
	patterns []compiledPattern

	mu      sync.RWMutex
	dynamic map[string][]dynamicEntry
}

var defaultPatterns = []Pattern{
	{`(?i)://[^:@\s]*:[^@\s]+@`, `://[REDACTED]@`},
	{`(?i)(bearer )\S+`, `${1}[REDACTED]`},
	{`gh[a-z]_[A-Za-z0-9]{36,}`, `[REDACTED-GH-TOKEN]`},
	{`(?i)("password"\s*:\s*)"[^"]*"`, `${1}"[REDACTED]"`},
	{`(?i)(password\s*[=:]\s*)\S+`, `${1}[REDACTED]`},
	{`(?i)(token\s*[=:]\s*)\S+`, `${1}[REDACTED]`},
	{`(?i)(secret\s*[=:]\s*)\S+`, `${1}[REDACTED]`},
	{`(?i)(api[_-]?key\s*[=:]\s*)\S+`, `${1}[REDACTED]`},
	{`(?i)(x-api-key\s*[=:]\s*)\S+`, `${1}[REDACTED]`},
	{`(?is)-----BEGIN .*PRIVATE KEY-----.*?-----END .*PRIVATE KEY-----`, `[REDACTED-PEM-KEY]`},
	{`(?i)AGE-SECRET-KEY-1[A-Z0-9]{40,}`, `[REDACTED-AGE-KEY]`},
	{`sk-[a-zA-Z0-9_\-]{4,}[A-Za-z0-9]{16,}`, `[REDACTED-SK-KEY]`},
	{`AKIA[A-Z0-9]{16}`, `[REDACTED-AWS-KEY]`},
	{`ey[A-Za-z0-9_\-]{10,}\.ey[A-Za-z0-9_\-]{10,}`, `[REDACTED-JWT]`},
	{`(?i)(authorization\s*:\s*)\S+`, `${1}[REDACTED]`},
	{`[A-Za-z0-9+/]{40,}={0,2}`, `[REDACTED-BASE64]`},
}

func newRedactorFromPatterns(patterns []Pattern) (*Redactor, error) {
	compiled := make([]compiledPattern, 0, len(patterns))
	for _, p := range patterns {
		re, err := regexp.Compile(p.Regex)
		if err != nil {
			return nil, fmt.Errorf("invalid pattern %q: %w", p.Regex, err)
		}
		compiled = append(compiled, compiledPattern{re: re, replacement: p.Replacement})
	}
	return &Redactor{patterns: compiled}, nil
}

func NewRedactor(extraPatterns []Pattern) (*Redactor, error) {
	all := make([]Pattern, len(defaultPatterns), len(defaultPatterns)+len(extraPatterns))
	copy(all, defaultPatterns)
	all = append(all, extraPatterns...)
	return newRedactorFromPatterns(all)
}

func (r *Redactor) Redact(input string) (string, error) {
	result := input
	// Dynamic exact-value rules run BEFORE the static pipeline: a static
	// pattern (e.g. `token=…`) can fragment a secret before the exact match
	// sees it, leaving partial key bytes in the output. The snapshot is
	// ordered longest-value-first so a value that prefixes another
	// registered value cannot fragment it — removal stays atomic and the
	// output deterministic regardless of map iteration order.
	for _, e := range r.dynamicSnapshot() {
		result = strings.ReplaceAll(result, e.value, e.replacement)
	}
	for _, p := range r.patterns {
		result = p.re.ReplaceAllString(result, p.replacement)
	}
	return result, nil
}

func (r *Redactor) dynamicSnapshot() []dynamicEntry {
	r.mu.RLock()
	defer r.mu.RUnlock()
	snapshot := make([]dynamicEntry, 0, len(r.dynamic))
	for _, entries := range r.dynamic {
		snapshot = append(snapshot, entries...)
	}
	sort.Slice(snapshot, func(i, j int) bool {
		if len(snapshot[i].value) != len(snapshot[j].value) {
			return len(snapshot[i].value) > len(snapshot[j].value)
		}
		return snapshot[i].value < snapshot[j].value
	})
	return snapshot
}

// RegisterDynamic adds exact-value rules, grouped by ID. Registering with an
// ID that already exists atomically replaces that ID's rules — rotation and
// re-registration are idempotent. Validation failures apply no partial state.
func (r *Redactor) RegisterDynamic(rules ...DynamicRule) error {
	for _, rule := range rules {
		if rule.ID == "" {
			return fmt.Errorf("dynamic redaction rule ID must not be empty")
		}
		if rule.Value == "" {
			return fmt.Errorf("dynamic redaction rule %q: value must not be empty", rule.ID)
		}
	}
	if len(rules) == 0 {
		return nil
	}
	byID := make(map[string][]dynamicEntry, len(rules))
	for _, rule := range rules {
		byID[rule.ID] = append(byID[rule.ID], dynamicEntry{
			value:       rule.Value,
			replacement: rule.Replacement,
		})
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.dynamic == nil {
		r.dynamic = make(map[string][]dynamicEntry, len(byID))
	}
	for id, entries := range byID {
		r.dynamic[id] = entries
	}
	return nil
}

// UnregisterDynamic removes every rule in the named group. Unknown IDs are a
// no-op.
func (r *Redactor) UnregisterDynamic(id string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.dynamic, id)
}

// DynamicRules snapshots the registered dynamic rules (dynamic
// longest-first order). The BYO resolve router (US-72.2) uses it to apply
// the precise exact-value staged-key rules to proxied bodies — bodies get
// dynamic-only redaction (the static heuristics would corrupt legitimate
// structured/vision payloads, e.g. long base64 image parts); diagnostics
// and logs get the full pipeline via Redact (design 0058 §4.9, K7).
func (r *Redactor) DynamicRules() []DynamicRule {
	entries := r.dynamicSnapshot()
	rules := make([]DynamicRule, 0, len(entries))
	for _, e := range entries {
		rules = append(rules, DynamicRule{Value: e.value, Replacement: e.replacement})
	}
	return rules
}

// RedactDynamicOnly applies exactly the dynamic exact-value rules (no
// static pipeline) — see DynamicRules for why the router's body path uses
// this surface.
func (r *Redactor) RedactDynamicOnly(input string) string {
	result := input
	for _, e := range r.dynamicSnapshot() {
		result = strings.ReplaceAll(result, e.value, e.replacement)
	}
	return result
}

var (
	cachedRedactor     *Redactor
	cachedRedactorOnce sync.Once
	cachedRedactorErr  error
)

func Redact(input string) (string, error) {
	r, err := defaultRedactor()
	if err != nil {
		return "", err
	}
	return r.Redact(input)
}

// RegisterDynamic registers exact-value rules on the package-level redactor
// (the shared pipeline agents and the relay router consume). See
// (*Redactor).RegisterDynamic for semantics.
func RegisterDynamic(rules ...DynamicRule) error {
	r, err := defaultRedactor()
	if err != nil {
		return err
	}
	return r.RegisterDynamic(rules...)
}

// UnregisterDynamic removes a rule group from the package-level redactor.
func UnregisterDynamic(id string) {
	r, err := defaultRedactor()
	if err != nil {
		return
	}
	r.UnregisterDynamic(id)
}

func defaultRedactor() (*Redactor, error) {
	cachedRedactorOnce.Do(func() {
		cachedRedactor, cachedRedactorErr = NewRedactorFromFile(defaultConfigPath)
	})
	return cachedRedactor, cachedRedactorErr
}

func NewRedactorFromFile(path string) (*Redactor, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return NewRedactor(nil)
	}
	if err != nil {
		return nil, fmt.Errorf("failed to read config %s: %w", path, err)
	}

	var extra []Pattern
	if err := json.Unmarshal(data, &extra); err != nil {
		return nil, fmt.Errorf("malformed config %s: %w", path, err)
	}

	return NewRedactor(extra)
}
