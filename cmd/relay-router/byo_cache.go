// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"context"
	"sort"
	"strings"
	"sync"

	corev1 "k8s.io/api/core/v1"

	"github.com/lenaxia/llmsafespaces/pkg/redact"
	"github.com/lenaxia/llmsafespaces/pkg/secrets"
)

// Envelope Secret contract (US-72.3 writes; the router only reads).
const (
	byoEnvWorkspaceLabel = "llmsafespaces.dev/workspace-id"
	byoEnvProviderLabel  = "llmsafespaces.dev/provider-slug"
	byoEnvDataKey        = "envelope"
)

type envCacheKey struct {
	workspaceID  string
	providerSlug string
}

// byoEnvelopeCache is the informer-backed staged-envelope store: the
// router's view of the per-workspace, per-provider ciphertext (D2 —
// ciphertext ONLY; the resolve path decrypts per request and discards).
// Revocation = Secret deletion → watch eviction → next resolve fails
// closed (bounded by watch propagation).
type byoEnvelopeCache struct {
	mu    sync.RWMutex
	envs  map[envCacheKey]string
	names map[string]envCacheKey
}

func newByoEnvelopeCache() *byoEnvelopeCache {
	return &byoEnvelopeCache{
		envs:  map[envCacheKey]string{},
		names: map[string]envCacheKey{},
	}
}

// Apply ingests a watch-delivered Secret. Non-envelope Secrets (missing
// labels or the data key) are ignored.
func (c *byoEnvelopeCache) Apply(sec *corev1.Secret) {
	ws := sec.Labels[byoEnvWorkspaceLabel]
	slug := sec.Labels[byoEnvProviderLabel]
	envelope := string(sec.Data[byoEnvDataKey])
	if ws == "" || slug == "" || envelope == "" {
		return
	}
	key := envCacheKey{workspaceID: ws, providerSlug: slug}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.names[sec.Name] = key
	c.envs[key] = envelope
}

// Evict removes a Secret's entry (delete event). Kept as a no-op for
// unknown names so a stale delete can never evict a re-created Secret.
func (c *byoEnvelopeCache) Evict(secretName string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if key, ok := c.names[secretName]; ok {
		delete(c.names, secretName)
		delete(c.envs, key)
	}
}

// Envelope returns the cached ciphertext for a (workspace, provider).
func (c *byoEnvelopeCache) Envelope(workspaceID, providerSlug string) (string, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	env, ok := c.envs[envCacheKey{workspaceID: workspaceID, providerSlug: providerSlug}]
	return env, ok
}

// Len reports the cached entry count (metrics/tests).
func (c *byoEnvelopeCache) Len() int {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return len(c.envs)
}

// resolveDispatcher routes an envelope to the resolver its algorithm
// discriminator names: KMS-mode envelopes to the boot-constructed KEK
// provider (one per deployment), HPKE envelopes to the keypair manager's
// generation-keyed resolvers. This is the per-request local resolve of D2.
type resolveDispatcher struct {
	keys *byoKeyManager
	kms  secrets.StagingResolver // nil in HPKE (dev) deployments
}

func (d resolveDispatcher) Resolve(ctx context.Context, envelope string) ([]byte, error) {
	alg, _, err := secrets.InspectStagingEnvelope(envelope)
	if err != nil {
		return nil, err
	}
	switch alg {
	case secrets.StagingAlgAESGCM:
		if d.kms == nil {
			return nil, errUnknownStagingKey
		}
		return d.kms.Resolve(ctx, envelope)
	case secrets.StagingAlgHPKE:
		return d.keys.Resolve(ctx, envelope)
	default:
		return nil, errUnknownStagingKey
	}
}

// exactValueMatcher applies the redactor's dynamic exact-value rules to
// proxied bodies: full-buffer for requests, streaming with a bounded
// carry window for responses. Values are matched longest-first (same
// ordering as the redaction engine) so a value that prefixes another
// cannot fragment it.
type exactValueMatcher struct {
	rules []redact.DynamicRule
}

func newExactValueMatcher(r *redact.Redactor) *exactValueMatcher {
	rules := r.DynamicRules()
	sort.SliceStable(rules, func(i, j int) bool {
		return len(rules[i].Value) > len(rules[j].Value)
	})
	return &exactValueMatcher{rules: rules}
}

// Apply redacts a complete buffer (request bodies — already fully read for
// the model-allowlist check).
func (m *exactValueMatcher) Apply(s string) string {
	for _, r := range m.rules {
		s = strings.ReplaceAll(s, r.Value, r.Replacement)
	}
	return s
}

// maxRuleLen is the longest registered value; a rule can only span a chunk
// boundary within that many bytes, so the streaming writer holds back
// maxRuleLen-1 bytes between flushes.
func (m *exactValueMatcher) maxRuleLen() int {
	if len(m.rules) == 0 {
		return 0
	}
	return len(m.rules[0].Value)
}

// streamRedactor transforms a byte stream chunk-by-chunk, carrying a tail
// between chunks so a staged-key match split across reads is still caught
// whole. Flush must be called at EOF.
type streamRedactor struct {
	m      *exactValueMatcher
	carry  []byte
	active bool
}

func newStreamRedactor(m *exactValueMatcher) *streamRedactor {
	return &streamRedactor{m: m, active: len(m.rules) > 0}
}

// Write returns the redacted bytes safe to emit for the given chunk, and
// the number of input bytes consumed (the remainder is held in carry).
func (s *streamRedactor) Write(chunk []byte) []byte {
	if !s.active {
		return chunk
	}
	buf := append(s.carry, chunk...)
	s.carry = nil
	out := s.m.Apply(string(buf))
	hold := s.m.maxRuleLen() - 1
	if hold < 0 {
		hold = 0
	}
	if len(out) <= hold {
		s.carry = []byte(out)
		return nil
	}
	safe, tail := out[:len(out)-hold], out[len(out)-hold:]
	s.carry = []byte(tail)
	return []byte(safe)
}

// Flush emits everything remaining.
func (s *streamRedactor) Flush() []byte {
	if !s.active {
		return nil
	}
	out := s.carry
	s.carry = nil
	return []byte(out)
}
