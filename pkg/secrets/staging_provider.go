// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package secrets

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/cloudflare/circl/hpke"
	"github.com/cloudflare/circl/kem"
	"github.com/lenaxia/llmsafespaces/pkg/redact"
)

// StagingProvider implements the D2 envelope of design 0058 §4.2 (Epic 72,
// US-72.1): a provider key is sealed under a router-scoped key and stored as
// an opaque envelope; the plaintext key exists only in controller and router
// memory. Two interchangeable algorithms are discriminated by the envelope:
//
//   - KMS mode (prod): a 32-byte local KEK is generated once, wrapped by a
//     cloud KMS (any RootKeyProvider), and stored ONLY wrapped in the
//     `llm-relay-kek` Secret. Controller and router each unwrap it exactly
//     once at construction; sealing and resolving are local AES-256-GCM.
//   - HPKE mode (dev / no-KMS production): the controller seals to the
//     router's public key (RFC 9180 via github.com/cloudflare/circl), so it
//     never holds decrypt capability.
//
// Wire format: `stg:v1:<alg>:<keyID>:<base64(payload)>`. The header
// (version, algorithm, plaintext keyID) is bound as AEAD associated data, so
// an envelope cannot claim an algorithm or key it was not sealed under.
const stagingEnvelopePrefix = "stg:v1:"

const (
	StagingAlgAESGCM = "aes-256-gcm"
	StagingAlgHPKE   = "hpke"
)

// Keys of the `llm-relay-kek` Secret (llm-relay namespace). The Secret
// carries the KMS-wrapped KEK blob and the key id — never plaintext KEK
// bytes.
const (
	KEKSecretKeyID   = "key-id"
	KEKSecretWrapped = "wrapped-kek"
)

const stagingKEKSize = 32

const stagingHPKEInfo = "llmsafespaces-staging-hpke-v1"

var (
	ErrNotStagingEnvelope       = errors.New("not a staging envelope")
	ErrStagingEnvelopeVersion   = errors.New("unsupported staging envelope version")
	ErrStagingEnvelopeAlgorithm = errors.New("unknown staging envelope algorithm")
	ErrStagingAlgorithmMismatch = errors.New("envelope algorithm does not match this staging provider mode")
	ErrStagingKeyIDMismatch     = errors.New("envelope keyID does not match this provider's key")
	ErrStagingEnvelopeAuth      = errors.New("staging envelope authentication failed")
	ErrInvalidStagingKeyID      = errors.New("staging keyID must be non-empty and contain no ':'")
)

// StagingSealer is the controller-side capability: seal a provider key into
// an envelope. Implementations: KMSStagingProvider (prod), HPKEStagingSealer
// (dev).
type StagingSealer interface {
	Seal(ctx context.Context, providerKey []byte) (string, error)
	KeyID() string
}

// StagingResolver is the router-side capability: recover the provider key
// from an envelope. The HPKE sealer deliberately does NOT implement it — in
// dev mode the controller holds no decrypt capability by construction.
type StagingResolver interface {
	Resolve(ctx context.Context, envelope string) ([]byte, error)
	KeyID() string
}

// StagedKeyRedactor is the seam between staged key material and the
// redaction engine (owner direction 2026-09-17; design 0058 §4.9 addendum).
// The staging provider is the one place the platform legitimately knows a
// staged key's plaintext at seal and resolve time; it registers the material
// as exact-value redaction rules so the payload pipeline (static rules +
// dynamic staged-key rules, applied at the router's sanitization stage in
// US-72.2) can never echo a key back through the relay. Revocation (Secret
// deletion, D2) unregisters via UnregisterStagedKey with the same ID.
type StagedKeyRedactor interface {
	RegisterStagedKey(id string, material []byte) error
	UnregisterStagedKey(id string)
}

// RedactStagedKeys adapts a *redact.Redactor to StagedKeyRedactor.
type RedactStagedKeys struct {
	Redactor *redact.Redactor
}

func (a RedactStagedKeys) RegisterStagedKey(id string, material []byte) error {
	if a.Redactor == nil {
		return errors.New("RedactStagedKeys: no *redact.Redactor configured")
	}
	return a.Redactor.RegisterDynamic(StagedKeyMaterialRules(id, material)...)
}

func (a RedactStagedKeys) UnregisterStagedKey(id string) {
	if a.Redactor == nil {
		return
	}
	a.Redactor.UnregisterDynamic(id)
}

// StagedKeyRedactionReplacement is the marker dynamic staged-key rules emit.
const StagedKeyRedactionReplacement = "[REDACTED-STAGED-KEY]"

// StagedKeyRedactionID derives the redaction rule-group ID for an envelope.
// Both controller (seal time) and router (resolve time) derive it from the
// envelope alone, so the two sides converge on one ID per staged credential
// without sharing state, and revocation (either side) removes exactly that
// credential's rules.
func StagedKeyRedactionID(envelope string) string {
	sum := sha256.Sum256([]byte(envelope))
	return "staged:" + base64.RawURLEncoding.EncodeToString(sum[:12])
}

// StagedKeyMaterialRules builds one exact-value rule per plausible encoding
// of the staged key material (raw, standard base64, URL-safe base64), so a
// key echoed verbatim or base64-wrapped through a payload is caught even
// when no static pattern matches its shape.
func StagedKeyMaterialRules(id string, material []byte) []redact.DynamicRule {
	return []redact.DynamicRule{
		{ID: id, Value: string(material), Replacement: StagedKeyRedactionReplacement},
		{ID: id, Value: base64.StdEncoding.EncodeToString(material), Replacement: StagedKeyRedactionReplacement},
		{ID: id, Value: base64.URLEncoding.EncodeToString(material), Replacement: StagedKeyRedactionReplacement},
	}
}

// stagingEnvelope is the parsed wire form. Header is the AAD.
type stagingEnvelope struct {
	alg     string
	keyID   string
	header  string
	payload []byte
}

func parseStagingEnvelope(envelope string) (*stagingEnvelope, error) {
	parts := strings.SplitN(envelope, ":", 5)
	if len(parts) != 5 || parts[0] != "stg" {
		return nil, ErrNotStagingEnvelope
	}
	if parts[1] != "v1" {
		return nil, fmt.Errorf("%w: %q", ErrStagingEnvelopeVersion, parts[1])
	}
	switch parts[2] {
	case StagingAlgAESGCM, StagingAlgHPKE:
	default:
		return nil, fmt.Errorf("%w: %q", ErrStagingEnvelopeAlgorithm, parts[2])
	}
	if parts[3] == "" {
		return nil, fmt.Errorf("%w: empty keyID", ErrInvalidStagingKeyID)
	}
	payload, err := base64.StdEncoding.DecodeString(parts[4])
	if err != nil {
		return nil, fmt.Errorf("%w: malformed payload", ErrNotStagingEnvelope)
	}
	return &stagingEnvelope{
		alg:     parts[2],
		keyID:   parts[3],
		header:  strings.Join(parts[:4], ":"),
		payload: payload,
	}, nil
}

func validateStagingKeyID(keyID string) error {
	if keyID == "" || strings.Contains(keyID, ":") {
		return ErrInvalidStagingKeyID
	}
	return nil
}

func encryptWithAAD(key, plaintext, aad []byte) ([]byte, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, fmt.Errorf("generating nonce: %w", err)
	}
	return gcm.Seal(nonce, nonce, plaintext, aad), nil
}

func decryptWithAAD(key, blob, aad []byte) ([]byte, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	if len(blob) < gcm.NonceSize() {
		return nil, ErrStagingEnvelopeAuth
	}
	nonce, ct := blob[:gcm.NonceSize()], blob[gcm.NonceSize():]
	plaintext, err := gcm.Open(nil, nonce, ct, aad)
	if err != nil {
		return nil, ErrStagingEnvelopeAuth
	}
	return plaintext, nil
}

// GenerateStagingKEK returns fresh 32-byte local KEK material for the
// `llm-relay-kek` Secret bootstrap. The caller wraps it immediately via
// BuildKEKSecretData and discards the plaintext.
func GenerateStagingKEK() ([]byte, error) {
	return GenerateDEK()
}

// BuildKEKSecretData wraps kek with the KMS provider and returns the
// `llm-relay-kek` Secret data map: the wrapped blob plus the key id. This is
// the ONLY writer of that Secret's shape; plaintext KEK bytes never appear
// in it (pinned by TestKEKNeverInSecret).
func BuildKEKSecretData(ctx context.Context, kms RootKeyProvider, keyID string, kek []byte) (map[string][]byte, error) {
	if err := validateStagingKeyID(keyID); err != nil {
		return nil, err
	}
	if len(kek) != stagingKEKSize {
		return nil, fmt.Errorf("staging KEK must be %d bytes, got %d", stagingKEKSize, len(kek))
	}
	wrapped, err := kms.Encrypt(ctx, kek)
	if err != nil {
		return nil, fmt.Errorf("wrapping staging KEK: %w", err)
	}
	return map[string][]byte{
		KEKSecretKeyID:   []byte(keyID),
		KEKSecretWrapped: wrapped,
	}, nil
}

// KEKSecretData is the parsed `llm-relay-kek` Secret payload.
type KEKSecretData struct {
	KeyID      string
	WrappedKEK []byte
}

func ParseKEKSecretData(data map[string][]byte) (KEKSecretData, error) {
	keyID, ok := data[KEKSecretKeyID]
	if !ok || len(keyID) == 0 {
		return KEKSecretData{}, fmt.Errorf("kek secret missing %q", KEKSecretKeyID)
	}
	if err := validateStagingKeyID(string(keyID)); err != nil {
		return KEKSecretData{}, err
	}
	wrapped, ok := data[KEKSecretWrapped]
	if !ok || len(wrapped) == 0 {
		return KEKSecretData{}, fmt.Errorf("kek secret missing %q", KEKSecretWrapped)
	}
	return KEKSecretData{KeyID: string(keyID), WrappedKEK: wrapped}, nil
}

// KMSStagingProvider is the prod-mode envelope provider. Construction
// performs the single KMS Decrypt that unwraps the local KEK; Seal and
// Resolve are local AES-256-GCM (D2: KMS is O(boot), never O(request)). The
// controller uses Seal; the router uses Resolve; both hold the same
// trusted-plane KEK.
type KMSStagingProvider struct {
	keyID     string
	kek       []byte
	redaction StagedKeyRedactor
}

func NewKMSStagingProvider(ctx context.Context, kms RootKeyProvider, secretData map[string][]byte, redaction StagedKeyRedactor) (*KMSStagingProvider, error) {
	parsed, err := ParseKEKSecretData(secretData)
	if err != nil {
		return nil, err
	}
	kek, err := kms.Decrypt(ctx, parsed.WrappedKEK)
	if err != nil {
		return nil, fmt.Errorf("unwrapping staging KEK: %w", err)
	}
	if len(kek) != stagingKEKSize {
		return nil, fmt.Errorf("unwrapped staging KEK must be %d bytes, got %d", stagingKEKSize, len(kek))
	}
	cp := make([]byte, len(kek))
	copy(cp, kek)
	return &KMSStagingProvider{keyID: parsed.KeyID, kek: cp, redaction: redaction}, nil
}

func (p *KMSStagingProvider) KeyID() string { return p.keyID }

func (p *KMSStagingProvider) Seal(ctx context.Context, providerKey []byte) (string, error) {
	if len(providerKey) == 0 {
		return "", errors.New("staging seal: provider key must not be empty")
	}
	blob, err := encryptWithAAD(p.kek, providerKey, []byte(stagingEnvelopeHeader(StagingAlgAESGCM, p.keyID)))
	if err != nil {
		return "", fmt.Errorf("staging seal: %w", err)
	}
	envelope := stagingEnvelopePrefix + StagingAlgAESGCM + ":" + p.keyID + ":" +
		base64.StdEncoding.EncodeToString(blob)
	if err := registerStagedKey(p.redaction, envelope, providerKey); err != nil {
		return "", err
	}
	return envelope, nil
}

func (p *KMSStagingProvider) Resolve(ctx context.Context, envelope string) ([]byte, error) {
	env, err := parseStagingEnvelope(envelope)
	if err != nil {
		return nil, err
	}
	if env.alg != StagingAlgAESGCM {
		return nil, fmt.Errorf("%w: envelope is %q, provider is %q", ErrStagingAlgorithmMismatch, env.alg, StagingAlgAESGCM)
	}
	if env.keyID != p.keyID {
		return nil, fmt.Errorf("%w: envelope claims %q, provider holds %q", ErrStagingKeyIDMismatch, env.keyID, p.keyID)
	}
	providerKey, err := decryptWithAAD(p.kek, env.payload, []byte(env.header))
	if err != nil {
		return nil, fmt.Errorf("staging resolve: %w", err)
	}
	if err := registerStagedKey(p.redaction, envelope, providerKey); err != nil {
		return nil, err
	}
	return providerKey, nil
}

func stagingEnvelopeHeader(alg, keyID string) string {
	return "stg:v1:" + alg + ":" + keyID
}

func registerStagedKey(redaction StagedKeyRedactor, envelope string, material []byte) error {
	if redaction == nil {
		return nil
	}
	if err := redaction.RegisterStagedKey(StagedKeyRedactionID(envelope), material); err != nil {
		return fmt.Errorf("registering staged-key redaction rule: %w", err)
	}
	return nil
}

// stagingSuite is the pinned HPKE cipher suite: DHKEM(X25519, HKDF-SHA256) /
// HKDF-SHA256 / AES-128-GCM (RFC 9180 base suite).
func stagingSuite() hpke.Suite {
	return hpke.NewSuite(hpke.KEM_X25519_HKDF_SHA256, hpke.KDF_HKDF_SHA256, hpke.AEAD_AES128GCM)
}

func stagingKEMScheme() kem.Scheme {
	return hpke.KEM_X25519_HKDF_SHA256.Scheme()
}

// HPKEKeyID derives the envelope keyID for an HPKE keypair generation. Both
// controller (seal) and router (resolve) derive it from the generation
// counter, so envelope keyIDs and Secret generations cannot drift apart.
func HPKEKeyID(generation int64) string {
	return "hpke-g" + strconv.FormatInt(generation, 10)
}

// HPKEKeyPairPayload is the `llm-relay-hpke-key` Secret payload: the private
// key, its co-located public key copy, and the monotonic generation counter.
// Design 0058 §4.2: payloads are generation-tagged and self-contained so a
// replica can assert integrity without reading any other Secret.
type HPKEKeyPairPayload struct {
	PrivateKey []byte `json:"privateKey"`
	PublicKey  []byte `json:"publicKey"`
	Generation int64  `json:"generation"`
}

// HPKEPubPayload is the `llm-relay-hpke-pub` Secret payload: the public key
// the controller seals against, plus the matching generation.
type HPKEPubPayload struct {
	PublicKey  []byte `json:"publicKey"`
	Generation int64  `json:"generation"`
}

// GenerateHPKEKeyPairPayload generates a fresh keypair at the given
// generation (first boot uses 1; rotation increments).
func GenerateHPKEKeyPairPayload(generation int64) (*HPKEKeyPairPayload, error) {
	if generation < 1 {
		return nil, fmt.Errorf("hpke keypair generation must be >= 1, got %d", generation)
	}
	_, priv, err := stagingKEMScheme().GenerateKeyPair()
	if err != nil {
		return nil, fmt.Errorf("generating hpke keypair: %w", err)
	}
	privBytes, pubBytes, err := marshalKeyPair(priv)
	if err != nil {
		return nil, err
	}
	return &HPKEKeyPairPayload{PrivateKey: privBytes, PublicKey: pubBytes, Generation: generation}, nil
}

func marshalKeyPair(priv kem.PrivateKey) (privBytes, pubBytes []byte, err error) {
	privBytes, err = priv.MarshalBinary()
	if err != nil {
		return nil, nil, fmt.Errorf("marshaling hpke private key: %w", err)
	}
	pubBytes, err = priv.Public().MarshalBinary()
	if err != nil {
		return nil, nil, fmt.Errorf("marshaling hpke public key: %w", err)
	}
	return privBytes, pubBytes, nil
}

// Public returns the pub-Secret view of this keypair payload.
func (p *HPKEKeyPairPayload) Public() HPKEPubPayload {
	return HPKEPubPayload{PublicKey: p.PublicKey, Generation: p.Generation}
}

func (p *HPKEKeyPairPayload) Marshal() ([]byte, error) {
	return json.Marshal(p)
}

func ParseHPKEKeyPairPayload(data []byte) (*HPKEKeyPairPayload, error) {
	var p HPKEKeyPairPayload
	if err := json.Unmarshal(data, &p); err != nil {
		return nil, fmt.Errorf("malformed hpke keypair payload: %w", err)
	}
	if len(p.PrivateKey) == 0 || len(p.PublicKey) == 0 || p.Generation < 1 {
		return nil, errors.New("hpke keypair payload requires privateKey, publicKey and generation >= 1")
	}
	return &p, nil
}

func (p *HPKEPubPayload) Marshal() ([]byte, error) {
	return json.Marshal(p)
}

func ParseHPKEPubPayload(data []byte) (*HPKEPubPayload, error) {
	var p HPKEPubPayload
	if err := json.Unmarshal(data, &p); err != nil {
		return nil, fmt.Errorf("malformed hpke pub payload: %w", err)
	}
	if len(p.PublicKey) == 0 || p.Generation < 1 {
		return nil, errors.New("hpke pub payload requires publicKey and generation >= 1")
	}
	return &p, nil
}

// AssertHPKEKeyPair is the self-contained integrity assert of design 0058
// §42: derive the public key from the loaded private key and compare against
// the co-located copy, plus generation monotonicity. prevGeneration is the
// highest generation previously observed by the caller (0 on first load);
// equality passes — informer re-lists redeliver the same object, so only a
// strict decrease is corruption. It never reads across Secrets: the pub
// Secret belongs to the controller, and the deliberate private-then-pub torn
// window of a routine rotation must never read as corruption to a peer.
func AssertHPKEKeyPair(p *HPKEKeyPairPayload, prevGeneration int64) error {
	if p.Generation < prevGeneration {
		return fmt.Errorf("hpke keypair generation regressed: %d < %d", p.Generation, prevGeneration)
	}
	priv, err := stagingKEMScheme().UnmarshalBinaryPrivateKey(p.PrivateKey)
	if err != nil {
		return fmt.Errorf("hpke keypair private key unparseable: %w", err)
	}
	_, derivedPub, err := marshalKeyPair(priv)
	if err != nil {
		return err
	}
	if !bytes.Equal(derivedPub, p.PublicKey) {
		return errors.New("hpke keypair public key does not match its private key")
	}
	return nil
}

// HPKEStagingSealer is the dev-mode controller capability: seal provider
// keys to the router's public key. It holds no private key and implements
// no Resolve — the controller cannot decrypt what it sealed.
type HPKEStagingSealer struct {
	keyID     string
	pub       kem.PublicKey
	redaction StagedKeyRedactor
}

func NewHPKEStagingSealer(publicKey []byte, keyID string, redaction StagedKeyRedactor) (*HPKEStagingSealer, error) {
	if err := validateStagingKeyID(keyID); err != nil {
		return nil, err
	}
	pub, err := stagingKEMScheme().UnmarshalBinaryPublicKey(publicKey)
	if err != nil {
		return nil, fmt.Errorf("parsing hpke public key: %w", err)
	}
	return &HPKEStagingSealer{keyID: keyID, pub: pub, redaction: redaction}, nil
}

func (s *HPKEStagingSealer) KeyID() string { return s.keyID }

func (s *HPKEStagingSealer) Seal(ctx context.Context, providerKey []byte) (string, error) {
	if len(providerKey) == 0 {
		return "", errors.New("staging seal: provider key must not be empty")
	}
	header := stagingEnvelopeHeader(StagingAlgHPKE, s.keyID)
	sender, err := stagingSuite().NewSender(s.pub, []byte(stagingHPKEInfo))
	if err != nil {
		return "", fmt.Errorf("staging seal: %w", err)
	}
	enc, sealer, err := sender.Setup(nil)
	if err != nil {
		return "", fmt.Errorf("staging seal: %w", err)
	}
	ct, err := sealer.Seal(providerKey, []byte(header))
	if err != nil {
		return "", fmt.Errorf("staging seal: %w", err)
	}
	payload := append(append([]byte{}, enc...), ct...)
	envelope := stagingEnvelopePrefix + StagingAlgHPKE + ":" + s.keyID + ":" +
		base64.StdEncoding.EncodeToString(payload)
	if err := registerStagedKey(s.redaction, envelope, providerKey); err != nil {
		return "", err
	}
	return envelope, nil
}

// HPKEStagingResolver is the dev-mode router capability: open envelopes
// sealed to the router's keypair. Dual-key resolve windows and prior-key
// retention are router-side machinery (US-72.2) built on this primitive.
type HPKEStagingResolver struct {
	keyID     string
	priv      kem.PrivateKey
	redaction StagedKeyRedactor
}

func NewHPKEStagingResolver(privateKey []byte, keyID string, redaction StagedKeyRedactor) (*HPKEStagingResolver, error) {
	if err := validateStagingKeyID(keyID); err != nil {
		return nil, err
	}
	priv, err := stagingKEMScheme().UnmarshalBinaryPrivateKey(privateKey)
	if err != nil {
		return nil, fmt.Errorf("parsing hpke private key: %w", err)
	}
	return &HPKEStagingResolver{keyID: keyID, priv: priv, redaction: redaction}, nil
}

func (r *HPKEStagingResolver) KeyID() string { return r.keyID }

func (r *HPKEStagingResolver) Resolve(ctx context.Context, envelope string) ([]byte, error) {
	env, err := parseStagingEnvelope(envelope)
	if err != nil {
		return nil, err
	}
	if env.alg != StagingAlgHPKE {
		return nil, fmt.Errorf("%w: envelope is %q, provider is %q", ErrStagingAlgorithmMismatch, env.alg, StagingAlgHPKE)
	}
	if env.keyID != r.keyID {
		return nil, fmt.Errorf("%w: envelope claims %q, resolver holds %q", ErrStagingKeyIDMismatch, env.keyID, r.keyID)
	}
	encLen := stagingKEMScheme().CiphertextSize()
	if len(env.payload) <= encLen {
		return nil, ErrStagingEnvelopeAuth
	}
	enc, ct := env.payload[:encLen], env.payload[encLen:]
	receiver, err := stagingSuite().NewReceiver(r.priv, []byte(stagingHPKEInfo))
	if err != nil {
		return nil, fmt.Errorf("staging resolve: %w", err)
	}
	opener, err := receiver.Setup(enc)
	if err != nil {
		return nil, fmt.Errorf("staging resolve: %w", err)
	}
	providerKey, err := opener.Open(ct, []byte(env.header))
	if err != nil {
		return nil, fmt.Errorf("staging resolve: %w", ErrStagingEnvelopeAuth)
	}
	if err := registerStagedKey(r.redaction, envelope, providerKey); err != nil {
		return nil, err
	}
	return providerKey, nil
}

// Compile-time capability pins.
var (
	_ StagingSealer     = (*KMSStagingProvider)(nil)
	_ StagingResolver   = (*KMSStagingProvider)(nil)
	_ StagingSealer     = (*HPKEStagingSealer)(nil)
	_ StagingResolver   = (*HPKEStagingResolver)(nil)
	_ StagedKeyRedactor = RedactStagedKeys{}
)
