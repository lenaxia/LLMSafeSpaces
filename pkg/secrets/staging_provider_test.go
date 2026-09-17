// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package secrets

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/lenaxia/llmsafespaces/pkg/redact"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// countingKMS wraps a RootKeyProvider and counts Encrypt/Decrypt calls. The
// US-72.1 contract is KMS O(boot): BuildKEKSecretData performs exactly one
// Encrypt, NewKMSStagingProvider exactly one Decrypt, and steady-state
// Seal/Resolve performs zero KMS calls (D2, design 0058 §4.2).
type countingKMS struct {
	mu        sync.Mutex
	encrypts  int
	decrypts  int
	inner     RootKeyProvider
	encryptFn func() error
}

func newCountingKMS(t testing.TB) *countingKMS {
	t.Helper()
	key := make([]byte, 32)
	for i := range key {
		key[i] = byte(i + 1)
	}
	inner, err := NewStaticKeyProvider(key)
	require.NoError(t, err)
	return &countingKMS{inner: inner}
}

func (c *countingKMS) Encrypt(ctx context.Context, plaintext []byte) ([]byte, error) {
	c.mu.Lock()
	c.encrypts++
	c.mu.Unlock()
	if c.encryptFn != nil {
		if err := c.encryptFn(); err != nil {
			return nil, err
		}
	}
	return c.inner.Encrypt(ctx, plaintext)
}

func (c *countingKMS) Decrypt(ctx context.Context, ciphertext []byte) ([]byte, error) {
	c.mu.Lock()
	c.decrypts++
	c.mu.Unlock()
	return c.inner.Decrypt(ctx, ciphertext)
}

func (c *countingKMS) counts() (int, int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.encrypts, c.decrypts
}

// recordingRedactor records registrations so tests can assert the seal and
// resolve legs both register staged key material.
type recordingRedactor struct {
	mu      sync.Mutex
	current map[string][]byte
	history []string
	fail    bool
}

func newRecordingRedactor() *recordingRedactor {
	return &recordingRedactor{current: map[string][]byte{}}
}

func (r *recordingRedactor) RegisterStagedKey(id string, material []byte) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.fail {
		return fmt.Errorf("registration refused")
	}
	cp := make([]byte, len(material))
	copy(cp, material)
	r.current[id] = cp
	r.history = append(r.history, id)
	return nil
}

func (r *recordingRedactor) UnregisterStagedKey(id string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.current, id)
}

// kmsFixture builds the prod-mode pair: KEK Secret data via a counting fake
// KMS, plus the KMS decrypt failures needed to construct a provider.
func kmsFixture(t *testing.T) (kms *countingKMS, secretData map[string][]byte, kek []byte) {
	t.Helper()
	kek, err := GenerateStagingKEK()
	require.NoError(t, err)
	kms = newCountingKMS(t)
	secretData, err = BuildKEKSecretData(context.Background(), kms, "llm-relay-kek-v1", kek)
	require.NoError(t, err)
	return kms, secretData, kek
}

func hpkeFixture(t *testing.T) (keyPair *HPKEKeyPairPayload, sealer *HPKEStagingSealer, resolver *HPKEStagingResolver) {
	t.Helper()
	keyPair, err := GenerateHPKEKeyPairPayload(1)
	require.NoError(t, err)
	sealer, err = NewHPKEStagingSealer(keyPair.PublicKey, HPKEKeyID(keyPair.Generation), nil)
	require.NoError(t, err)
	resolver, err = NewHPKEStagingResolver(keyPair.PrivateKey, HPKEKeyID(keyPair.Generation), nil)
	require.NoError(t, err)
	return keyPair, sealer, resolver
}

func TestStagingRoundtripKMSHPKE(t *testing.T) {
	_, secretData, _ := kmsFixture(t)
	kmsProvider, err := NewKMSStagingProvider(context.Background(), newCountingKMS(t), secretData, nil)
	require.NoError(t, err)
	_, hpkeSealer, hpkeResolver := hpkeFixture(t)

	keys := [][]byte{
		[]byte("sk-provider-key-0123456789abcdefghij"),
		[]byte("x"),
		bytes.Repeat([]byte("A"), 4096),
	}
	for i, key := range keys {
		t.Run(fmt.Sprintf("kms key %d", i), func(t *testing.T) {
			envelope, err := kmsProvider.Seal(context.Background(), key)
			require.NoError(t, err)
			assert.True(t, strings.HasPrefix(envelope, "stg:v1:"+StagingAlgAESGCM+":llm-relay-kek-v1:"), "envelope %q", envelope)
			got, err := kmsProvider.Resolve(context.Background(), envelope)
			require.NoError(t, err)
			assert.Equal(t, key, got)
		})
		t.Run(fmt.Sprintf("hpke key %d", i), func(t *testing.T) {
			envelope, err := hpkeSealer.Seal(context.Background(), key)
			require.NoError(t, err)
			assert.True(t, strings.HasPrefix(envelope, "stg:v1:"+StagingAlgHPKE+":"), "envelope %q", envelope)
			got, err := hpkeResolver.Resolve(context.Background(), envelope)
			require.NoError(t, err)
			assert.Equal(t, key, got)
		})
	}
}

func TestStagingSealRejectsEmptyProviderKey(t *testing.T) {
	_, secretData, _ := kmsFixture(t)
	kmsProvider, err := NewKMSStagingProvider(context.Background(), newCountingKMS(t), secretData, nil)
	require.NoError(t, err)
	_, hpkeSealer, _ := hpkeFixture(t)

	_, err = kmsProvider.Seal(context.Background(), nil)
	require.Error(t, err)
	_, err = hpkeSealer.Seal(context.Background(), nil)
	require.Error(t, err)
}

func TestStagingEnvelopeVersionDiscriminates(t *testing.T) {
	_, secretData, _ := kmsFixture(t)
	kmsProvider, err := NewKMSStagingProvider(context.Background(), newCountingKMS(t), secretData, nil)
	require.NoError(t, err)

	envelope, err := kmsProvider.Seal(context.Background(), []byte("some-provider-key"))
	require.NoError(t, err)

	v2 := strings.Replace(envelope, "stg:v1:", "stg:v2:", 1)
	_, err = kmsProvider.Resolve(context.Background(), v2)
	require.ErrorIs(t, err, ErrStagingEnvelopeVersion)

	_, err = kmsProvider.Resolve(context.Background(), "not-an-envelope")
	require.ErrorIs(t, err, ErrNotStagingEnvelope)
}

func TestStagingCrossAlgorithmConfusionRejected(t *testing.T) {
	_, secretData, _ := kmsFixture(t)
	kmsProvider, err := NewKMSStagingProvider(context.Background(), newCountingKMS(t), secretData, nil)
	require.NoError(t, err)
	_, hpkeSealer, hpkeResolver := hpkeFixture(t)

	hpkeEnvelope, err := hpkeSealer.Seal(context.Background(), []byte("cross-alg-key"))
	require.NoError(t, err)
	_, err = kmsProvider.Resolve(context.Background(), hpkeEnvelope)
	require.ErrorIs(t, err, ErrStagingAlgorithmMismatch)

	kmsEnvelope, err := kmsProvider.Seal(context.Background(), []byte("cross-alg-key"))
	require.NoError(t, err)
	_, err = hpkeResolver.Resolve(context.Background(), kmsEnvelope)
	require.ErrorIs(t, err, ErrStagingAlgorithmMismatch)
}

func TestStagingTamperedEnvelopeFailsLoud(t *testing.T) {
	_, secretData, _ := kmsFixture(t)
	kmsProvider, err := NewKMSStagingProvider(context.Background(), newCountingKMS(t), secretData, nil)
	require.NoError(t, err)
	_, hpkeSealer, hpkeResolver := hpkeFixture(t)
	providerKey := []byte("tamper-target-key-0123456789")

	kmsEnvelope, err := kmsProvider.Seal(context.Background(), providerKey)
	require.NoError(t, err)
	hpkeEnvelope, err := hpkeSealer.Seal(context.Background(), providerKey)
	require.NoError(t, err)

	mutate := func(env string, f func(string) string) string { return f(env) }

	tampered := []struct {
		name     string
		envelope string
	}{
		{"kms keyID swapped", mutate(kmsEnvelope, func(e string) string {
			return strings.Replace(e, "llm-relay-kek-v1", "llm-relay-kek-v2", 1)
		})},
		{"kms body byte flipped", mutate(kmsEnvelope, func(e string) string {
			body := []byte(e)
			body[len(body)-4] ^= 0x01
			return string(body)
		})},
		{"kms truncated", kmsEnvelope[:len(kmsEnvelope)-8]},
		{"hpke body byte flipped", mutate(hpkeEnvelope, func(e string) string {
			body := []byte(e)
			body[len(body)-4] ^= 0x01
			return string(body)
		})},
		{"hpke truncated", hpkeEnvelope[:len(hpkeEnvelope)-8]},
	}
	for _, tc := range tampered {
		t.Run(tc.name, func(t *testing.T) {
			if strings.HasPrefix(tc.name, "kms") {
				got, err := kmsProvider.Resolve(context.Background(), tc.envelope)
				require.Error(t, err)
				assert.Nil(t, got)
			} else {
				got, err := hpkeResolver.Resolve(context.Background(), tc.envelope)
				require.Error(t, err)
				assert.Nil(t, got)
			}
		})
	}
}

func TestStagingKeyIDMismatchRejected(t *testing.T) {
	_, secretData, _ := kmsFixture(t)
	kmsProvider, err := NewKMSStagingProvider(context.Background(), newCountingKMS(t), secretData, nil)
	require.NoError(t, err)

	envelope, err := kmsProvider.Seal(context.Background(), []byte("keyid-mismatch-key"))
	require.NoError(t, err)

	// A provider constructed for a different KEK id must refuse before any
	// decrypt: the envelope's plaintext keyID is bound as AAD and checked.
	other, err := NewKMSStagingProvider(context.Background(), newCountingKMS(t),
		mustBuildOtherKEKSecret(t), nil)
	require.NoError(t, err)
	_, err = other.Resolve(context.Background(), envelope)
	require.Error(t, err)
}

func mustBuildOtherKEKSecret(t *testing.T) map[string][]byte {
	t.Helper()
	kek, err := GenerateStagingKEK()
	require.NoError(t, err)
	data, err := BuildKEKSecretData(context.Background(), newCountingKMS(t), "llm-relay-kek-OTHER", kek)
	require.NoError(t, err)
	return data
}

func TestKEKNeverInSecret(t *testing.T) {
	kms, secretData, kek := kmsFixture(t)

	encodings := []string{
		string(kek),
		base64.StdEncoding.EncodeToString(kek),
		base64.URLEncoding.EncodeToString(kek),
		hex.EncodeToString(kek),
	}
	artifacts := map[string]string{}
	for k, v := range secretData {
		artifacts["secret["+k+"]"] = string(v)
	}
	marshaled, err := json.Marshal(secretData)
	require.NoError(t, err)
	artifacts["secret[json]"] = string(marshaled)

	for artName, art := range artifacts {
		for _, enc := range encodings {
			assert.NotContains(t, art, enc, "artifact %s leaks the plaintext KEK", artName)
		}
	}

	// The envelope never carries the provider key either.
	provider, err := NewKMSStagingProvider(context.Background(), newCountingKMS(t), secretData, nil)
	require.NoError(t, err)
	providerKey := []byte("provider-key-never-in-envelope-1234")
	envelope, err := provider.Seal(context.Background(), providerKey)
	require.NoError(t, err)
	assert.NotContains(t, envelope, string(providerKey))
	assert.NotContains(t, envelope, base64.StdEncoding.EncodeToString(providerKey))

	e, d := kms.counts()
	assert.Equal(t, 1, e, "KEK build must invoke KMS Encrypt exactly once")
	assert.Equal(t, 0, d)
}

func TestCiphertextOnlyInformer(t *testing.T) {
	_, secretData, _ := kmsFixture(t)
	controller, err := NewKMSStagingProvider(context.Background(), newCountingKMS(t), secretData, nil)
	require.NoError(t, err)

	// The informer cache (US-72.2) stores envelope strings only.
	informer := map[string]string{}
	providerKey := []byte("informer-cached-key-9876543210")
	envelope, err := controller.Seal(context.Background(), providerKey)
	require.NoError(t, err)
	informer["ws-abc.provider-slug"] = envelope
	assert.NotContains(t, envelope, string(providerKey))

	routerKMS := newCountingKMS(t)
	router, err := NewKMSStagingProvider(context.Background(), routerKMS, secretData, nil)
	require.NoError(t, err)
	_, d := routerKMS.counts()
	assert.Equal(t, 1, d, "router unwraps the KEK exactly once at construction")

	got, err := router.Resolve(context.Background(), informer["ws-abc.provider-slug"])
	require.NoError(t, err)
	assert.Equal(t, providerKey, got)

	_, d = routerKMS.counts()
	assert.Equal(t, 1, d, "steady-state resolve must not touch KMS")
}

func TestResolveZeroKMSCallsAtSteadyState(t *testing.T) {
	_, secretData, _ := kmsFixture(t)
	kms := newCountingKMS(t)
	provider, err := NewKMSStagingProvider(context.Background(), kms, secretData, nil)
	require.NoError(t, err)

	_, d := kms.counts()
	assert.Equal(t, 1, d, "construction unwraps the KEK once")

	key := []byte("steady-state-key-0123456789abcdef")
	envelope, err := provider.Seal(context.Background(), key)
	require.NoError(t, err)
	for i := 0; i < 100; i++ {
		got, err := provider.Resolve(context.Background(), envelope)
		require.NoError(t, err)
		assert.Equal(t, key, got)
	}
	_, d = kms.counts()
	assert.Equal(t, 1, d, "100 seal+resolve cycles must add zero KMS calls")
}

func TestHPKESealerHoldsNoDecryptCapability(t *testing.T) {
	_, sealer, _ := hpkeFixture(t)

	var x any = sealer
	_, isResolver := x.(StagingResolver)
	assert.False(t, isResolver, "the HPKE sealer (controller side) must not implement resolve")

	_, _, resolver := hpkeFixture(t)
	var y any = resolver
	_, isSealer := y.(StagingSealer)
	assert.False(t, isSealer, "the HPKE resolver (router side) must not implement seal")
}

func TestHPKEKeypairPayloadRoundtrip(t *testing.T) {
	kp, err := GenerateHPKEKeyPairPayload(7)
	require.NoError(t, err)

	data, err := kp.Marshal()
	require.NoError(t, err)
	parsed, err := ParseHPKEKeyPairPayload(data)
	require.NoError(t, err)
	assert.Equal(t, kp, parsed)

	pub := kp.Public()
	pubData, err := pub.Marshal()
	require.NoError(t, err)
	pubParsed, err := ParseHPKEPubPayload(pubData)
	require.NoError(t, err)
	assert.Equal(t, int64(7), pubParsed.Generation)
	assert.Equal(t, kp.PublicKey, pubParsed.PublicKey)
}

func TestHPKEKeypairPayloadRejectsGarbage(t *testing.T) {
	tests := []struct {
		name string
		data []byte
	}{
		{"not json", []byte("garbage")},
		{"empty private key", []byte(`{"privateKey":"","publicKey":"cHVi","generation":1}`)},
		{"empty public key", []byte(`{"privateKey":"cHJpdg==","publicKey":"","generation":1}`)},
		{"negative generation", []byte(`{"privateKey":"cHJpdg==","publicKey":"cHVi","generation":-1}`)},
	}
	for _, tc := range tests {
		t.Run(tc.name+"/keypair", func(t *testing.T) {
			_, err := ParseHPKEKeyPairPayload(tc.data)
			require.Error(t, err)
		})
	}
	_, err := ParseHPKEPubPayload([]byte("garbage"))
	require.Error(t, err)
	_, err = ParseHPKEPubPayload([]byte(`{"publicKey":"","generation":1}`))
	require.Error(t, err)
}

func TestAssertHPKEKeyPair(t *testing.T) {
	kp, err := GenerateHPKEKeyPairPayload(5)
	require.NoError(t, err)

	require.NoError(t, AssertHPKEKeyPair(kp, 0), "fresh keypair at generation 5 vs prev 0")
	require.NoError(t, AssertHPKEKeyPair(kp, 5), "informer redelivery of the same generation must pass (non-strict >=)")

	require.Error(t, AssertHPKEKeyPair(kp, 6), "generation regression must fail")

	corrupted := *kp
	corrupted.PublicKey = []byte("corrupted-public-key-bytes")
	require.Error(t, AssertHPKEKeyPair(&corrupted, 5), "mismatched co-located public key must fail the self-contained assert")

	badPriv := *kp
	badPriv.PrivateKey = []byte("not-a-private-key")
	require.Error(t, AssertHPKEKeyPair(&badPriv, 5), "corrupt private key must fail the assert, not panic")
}

func TestHPKERotationChangesKeyID(t *testing.T) {
	gen1, sealer1, resolver1 := hpkeFixture(t)
	envelope1, err := sealer1.Seal(context.Background(), []byte("rotation-key"))
	require.NoError(t, err)
	require.Equal(t, "hpke-g1", sealer1.KeyID())

	gen2, err := GenerateHPKEKeyPairPayload(2)
	require.NoError(t, err)
	sealer2, err := NewHPKEStagingSealer(gen2.PublicKey, HPKEKeyID(gen2.Generation), nil)
	require.NoError(t, err)
	envelope2, err := sealer2.Seal(context.Background(), []byte("rotation-key"))
	require.NoError(t, err)

	assert.NotEqual(t, envelope1, envelope2)
	assert.Contains(t, envelope1, "hpke-g1")
	assert.Contains(t, envelope2, "hpke-g2")

	// A resolver holding only the new key refuses the old-key envelope
	// loudly by keyID — the discrimination US-72.2's dual-key window builds
	// on. (The dual-key resolver itself lands with the router story.)
	resolver2, err := NewHPKEStagingResolver(gen2.PrivateKey, HPKEKeyID(gen2.Generation), nil)
	require.NoError(t, err)
	_, err = resolver1.Resolve(context.Background(), envelope2)
	require.Error(t, err)
	_, err = resolver2.Resolve(context.Background(), envelope1)
	require.Error(t, err)
	got, err := resolver2.Resolve(context.Background(), envelope2)
	require.NoError(t, err)
	assert.Equal(t, []byte("rotation-key"), got)
	_ = gen1
}

func TestStagingKeyIDValidation(t *testing.T) {
	kek, err := GenerateStagingKEK()
	require.NoError(t, err)
	kms := newCountingKMS(t)
	_, err = BuildKEKSecretData(context.Background(), kms, "bad:id", kek)
	require.Error(t, err, "keyID containing ':' would corrupt envelope parsing")

	_, err = BuildKEKSecretData(context.Background(), kms, "", kek)
	require.Error(t, err)

	shortKEK := []byte("too-short")
	_, err = BuildKEKSecretData(context.Background(), kms, "k1", shortKEK)
	require.Error(t, err)

	_, err = NewHPKEStagingSealer([]byte("pk"), "bad:id", nil)
	require.Error(t, err)
	_, err = NewHPKEStagingResolver([]byte("sk"), "", nil)
	require.Error(t, err)
}

func TestStagedKeyRedactionID(t *testing.T) {
	_, secretData, _ := kmsFixture(t)
	provider, err := NewKMSStagingProvider(context.Background(), newCountingKMS(t), secretData, nil)
	require.NoError(t, err)

	e1, err := provider.Seal(context.Background(), []byte("same-key-material"))
	require.NoError(t, err)
	e2, err := provider.Seal(context.Background(), []byte("same-key-material"))
	require.NoError(t, err)

	assert.Equal(t, StagedKeyRedactionID(e1), StagedKeyRedactionID(e1), "deterministic")
	assert.NotEqual(t, StagedKeyRedactionID(e1), StagedKeyRedactionID(e2), "fresh nonce → distinct envelope → distinct id")
	assert.True(t, strings.HasPrefix(StagedKeyRedactionID(e1), "staged:"))
}

func TestStagingRegistersKeyMaterialWithRedactionEngine(t *testing.T) {
	rec := newRecordingRedactor()

	_, secretData, _ := kmsFixture(t)
	sealer, err := NewKMSStagingProvider(context.Background(), newCountingKMS(t), secretData, rec)
	require.NoError(t, err)

	providerKey := []byte("k1AbCdE9x2")
	envelope, err := sealer.Seal(context.Background(), providerKey)
	require.NoError(t, err)

	id := StagedKeyRedactionID(envelope)
	require.Contains(t, rec.history, id, "seal registers the staged key")
	assert.Equal(t, providerKey, rec.current[id])

	// A registration failure must fail the seal loudly — never stage a key
	// that escaped the redaction engine.
	rec.fail = true
	_, err = sealer.Seal(context.Background(), []byte("another-key-9876543210"))
	require.Error(t, err)
	rec.fail = false

	// The resolve leg registers as well.
	resolver, err := NewKMSStagingProvider(context.Background(), newCountingKMS(t), secretData, rec)
	require.NoError(t, err)
	got, err := resolver.Resolve(context.Background(), envelope)
	require.NoError(t, err)
	assert.Equal(t, providerKey, got)
	assert.Equal(t, providerKey, rec.current[id], "resolve registers the staged key")

	// Revocation lifecycle: unregister drops it.
	rec.UnregisterStagedKey(id)
	assert.NotContains(t, rec.current, id)
}

func TestStagedKeyMaterialRulesEncodings(t *testing.T) {
	material := []byte("k1AbCdE9x2")
	rules := StagedKeyMaterialRules("staged:enc", material)
	require.Len(t, rules, 3)
	assert.Equal(t, "staged:enc", rules[0].ID)
	assert.Equal(t, string(material), rules[0].Value)
	assert.Equal(t, base64.StdEncoding.EncodeToString(material), rules[1].Value)
	assert.Equal(t, base64.URLEncoding.EncodeToString(material), rules[2].Value)
	for _, rule := range rules {
		assert.Equal(t, StagedKeyRedactionReplacement, rule.Replacement)
	}
}

// TestStagedKeyRedactionIntegration proves the combined pipeline end to end:
// a sealed-then-resolved key's plaintext (in its plausible encodings) is
// scrubbed from sample payloads by the static+dynamic redaction pipeline.
// The router's application of the pipeline to proxied traffic is US-72.2.
func TestStagedKeyRedactionIntegration(t *testing.T) {
	r, err := redact.NewRedactor(nil)
	require.NoError(t, err)
	hook := RedactStagedKeys{Redactor: r}

	_, hpkeSealer, hpkeResolver := hpkeFixtureWithRedaction(t, hook)
	_, secretData, _ := kmsFixture(t)
	kmsProvider, err := NewKMSStagingProvider(context.Background(), newCountingKMS(t), secretData, hook)
	require.NoError(t, err)

	// A key short/odd enough that NO static rule matches it — the dynamic
	// rule must be doing the work.
	providerKey := []byte("qZ7#k9mW2p")

	modes := []struct {
		name string
		seal func() (string, error)
	}{
		{"kms", func() (string, error) { return kmsProvider.Seal(context.Background(), providerKey) }},
		{"hpke", func() (string, error) { return hpkeSealer.Seal(context.Background(), providerKey) }},
	}
	for _, mode := range modes {
		t.Run(mode.name, func(t *testing.T) {
			envelope, err := mode.seal()
			require.NoError(t, err)

			var got []byte
			if mode.name == "kms" {
				got, err = kmsProvider.Resolve(context.Background(), envelope)
			} else {
				got, err = hpkeResolver.Resolve(context.Background(), envelope)
			}
			require.NoError(t, err)
			require.Equal(t, providerKey, got)

			payloads := []string{
				"model echoed the key: " + string(providerKey),
				`{"authorization":"` + string(providerKey) + `","model":"m"}`,
				"b64 smuggle: " + base64.StdEncoding.EncodeToString(providerKey),
				"url-b64 smuggle: " + base64.URLEncoding.EncodeToString(providerKey),
			}
			for _, payload := range payloads {
				out, err := r.Redact(payload)
				require.NoError(t, err)
				assert.NotContains(t, out, string(providerKey),
					"raw key material leaked from payload %q", payload)
				assert.NotContains(t, out, base64.StdEncoding.EncodeToString(providerKey))
				assert.Contains(t, out, StagedKeyRedactionReplacement)
			}

			// Static rules still fire alongside the dynamic rule.
			out, err := r.Redact("also leaking token=staticrule")
			require.NoError(t, err)
			assert.Contains(t, out, "token=[REDACTED]")

			// Revocation: unregister removes the dynamic protection;
			// the static pipeline is unaffected.
			id := StagedKeyRedactionID(envelope)
			hook.UnregisterStagedKey(id)
			out, err = r.Redact("model echoed the key: " + string(providerKey))
			require.NoError(t, err)
			assert.Contains(t, out, string(providerKey), "after revocation the exact value is no longer dynamic-redacted")
			out, err = r.Redact("also leaking token=staticrule")
			require.NoError(t, err)
			assert.Contains(t, out, "token=[REDACTED]")
		})
	}
}

func hpkeFixtureWithRedaction(t *testing.T, hook StagedKeyRedactor) (*HPKEKeyPairPayload, *HPKEStagingSealer, *HPKEStagingResolver) {
	t.Helper()
	kp, err := GenerateHPKEKeyPairPayload(1)
	require.NoError(t, err)
	sealer, err := NewHPKEStagingSealer(kp.PublicKey, HPKEKeyID(kp.Generation), hook)
	require.NoError(t, err)
	resolver, err := NewHPKEStagingResolver(kp.PrivateKey, HPKEKeyID(kp.Generation), hook)
	require.NoError(t, err)
	return kp, sealer, resolver
}

func TestRedactStagedKeysAdaptsRedactor(t *testing.T) {
	var _ StagedKeyRedactor = RedactStagedKeys{}

	// A nil inner redactor fails loudly rather than panicking.
	nilAdapter := RedactStagedKeys{}
	require.Error(t, nilAdapter.RegisterStagedKey("staged:x", []byte("material")))
	nilAdapter.UnregisterStagedKey("staged:x")
}

// TestResealRegistrationLifecycle pins the R2 lifecycle contract (review
// iteration 1): the re-seal path mints a fresh envelope and therefore a
// fresh rule group — the re-sealer (US-72.3 controller reconcile) must
// unlink the superseded group — while the resolve side is structurally
// bounded because re-resolving the same envelope replaces its group in
// place.
func TestResealRegistrationLifecycle(t *testing.T) {
	rec := newRecordingRedactor()
	_, secretData, _ := kmsFixture(t)
	provider, err := NewKMSStagingProvider(context.Background(), newCountingKMS(t), secretData, rec)
	require.NoError(t, err)

	material := []byte("rotate-me-key-0123456789")
	env1, err := provider.Seal(context.Background(), material)
	require.NoError(t, err)
	require.Len(t, rec.current, 1)

	env2, err := provider.Seal(context.Background(), material)
	require.NoError(t, err)
	require.Len(t, rec.current, 2, "a re-seal without unlink leaves the superseded group — the obligation exists for exactly this")

	rec.UnregisterStagedKey(StagedKeyRedactionID(env1))
	require.Len(t, rec.current, 1, "the documented re-seal unlink restores the bound")

	resolver, err := NewKMSStagingProvider(context.Background(), newCountingKMS(t), secretData, rec)
	require.NoError(t, err)
	for i := 0; i < 25; i++ {
		_, err := resolver.Resolve(context.Background(), env2)
		require.NoError(t, err)
	}
	assert.Len(t, rec.current, 1, "repeated resolve of the same envelope must replace in place, not accumulate")
}

func TestKMSConstructionFailsLoud(t *testing.T) {
	// Corrupt wrapped blob → construction fails, no silent fallback.
	_, secretData, _ := kmsFixture(t)
	secretData["wrapped-kek"] = []byte("aws-kms:v1:bm90LXZhbGlk")
	_, err := NewKMSStagingProvider(context.Background(), newCountingKMS(t), secretData, nil)
	require.Error(t, err)

	// Missing keys → parse error.
	bad := map[string][]byte{"key-id": []byte("k1")}
	_, err = NewKMSStagingProvider(context.Background(), newCountingKMS(t), bad, nil)
	require.Error(t, err)
}
