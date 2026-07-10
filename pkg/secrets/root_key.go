package secrets

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"os"
	"sort"
)

const (
	sealedSaltSize   = 32
	sealedNonceSize  = 12
	sealedKeyInfoStr = "llmsafespaces-sealed-root"
	sealedMagicV1    = "LSKP-S"

	// staticCiphertextPrefix is the self-identifying prefix wrapping every
	// ciphertext produced by a local provider (Static or Sealed) under
	// US-57.1. It lets CompositeProvider route Decrypt by prefix without
	// trial-and-error across providers — see composite_provider.go.
	//
	// "lkms" stands for "llmsafespaces local kek material"; the v1 suffix
	// leaves room for a future format bump (e.g. switching AES-GCM to
	// XChaCha20-Poly1305) without colliding with this prefix.
	//
	// Ciphertexts written before US-57.1 have no prefix (raw 12-byte
	// nonce + AES-GCM blob); the local providers' Decrypt still accepts
	// those via the un-prefixed fallback path so existing deployments
	// keep working through the upgrade.
	staticCiphertextPrefix = "lkms:v1:"
)

type RootKeyProvider interface {
	Encrypt(ctx context.Context, plaintext []byte) ([]byte, error)
	Decrypt(ctx context.Context, ciphertext []byte) ([]byte, error)
}

// VersionedProvider is implemented by providers that expose an active key
// version (US-50.3/50.4). Callers that need the version for key_version column
// writes assert this interface on the concrete provider — it is intentionally
// NOT on RootKeyProvider so a future external provider (Vault Transit, which
// handles versioning server-side) doesn't need to implement it.
type VersionedProvider interface {
	ActiveVersion() int
}

// ActiveVersionOf returns the active key version of a provider, or 1 if the
// provider does not implement VersionedProvider (e.g. nil or a future external
// provider). This is the safe default — version 1 is the initial migration
// default for all tables.
func ActiveVersionOf(p RootKeyProvider) int {
	if p == nil {
		return 1
	}
	if vp, ok := p.(VersionedProvider); ok {
		v := vp.ActiveVersion()
		if v > 0 {
			return v
		}
	}
	return 1
}

// keyEntry pairs a versioned key with its version number. The provider holds
// a slice sorted by version descending so Decrypt tries the newest first.
type keyEntry struct {
	version int
	key     []byte
}

// StaticKeyProvider holds one or more versioned keys. Encrypt always uses the
// highest-version (active) key; Decrypt tries each key newest-to-oldest and
// returns the first success. This enables zero-downtime KEK rotation (US-50.4,
// design D4): during the transition window the provider holds both old and new
// keys so ciphertexts encrypted under either version decrypt correctly.
type StaticKeyProvider struct {
	entries []keyEntry // sorted by version descending
}

// NewStaticKeyProvider constructs a single-key provider at version 1. This is
// the backward-compatible constructor used everywhere except the rotation window.
func NewStaticKeyProvider(key []byte) (*StaticKeyProvider, error) {
	if len(key) != 32 {
		return nil, fmt.Errorf("static key must be 32 bytes, got %d", len(key))
	}
	cp := make([]byte, 32)
	copy(cp, key)
	return &StaticKeyProvider{entries: []keyEntry{{version: 1, key: cp}}}, nil
}

// NewStaticKeyProviderMultiVersion constructs a multi-key provider for the
// rotation transition window (US-50.4). activeVersion is the highest version
// (the one Encrypt uses); keyByVersion maps every version to its key material.
// At least one entry at activeVersion must exist. Entries are stored sorted by
// version descending so Decrypt tries the newest first.
func NewStaticKeyProviderMultiVersion(activeVersion int, keyByVersion map[int][]byte) (*StaticKeyProvider, error) {
	if len(keyByVersion) == 0 {
		return nil, fmt.Errorf("at least one key entry is required")
	}
	activeKey, ok := keyByVersion[activeVersion]
	if !ok {
		return nil, fmt.Errorf("activeVersion %d not present in keyByVersion map", activeVersion)
	}
	if len(activeKey) != 32 {
		return nil, fmt.Errorf("key for version %d must be 32 bytes, got %d", activeVersion, len(activeKey))
	}
	for ver, k := range keyByVersion {
		if len(k) != 32 {
			return nil, fmt.Errorf("key for version %d must be 32 bytes, got %d", ver, len(k))
		}
	}
	entries := make([]keyEntry, 0, len(keyByVersion))
	for ver, k := range keyByVersion {
		cp := make([]byte, 32)
		copy(cp, k)
		entries = append(entries, keyEntry{version: ver, key: cp})
	}
	// Sort descending by version so Decrypt tries the newest (active) key first.
	sort.Slice(entries, func(i, j int) bool {
		return entries[i].version > entries[j].version
	})
	return &StaticKeyProvider{entries: entries}, nil
}

// ActiveVersion returns the highest version the provider can encrypt with
// (US-50.3 uses this to populate key_version columns on encrypt).
func (p *StaticKeyProvider) ActiveVersion() int {
	if len(p.entries) == 0 {
		return 0
	}
	return p.entries[0].version
}

func (p *StaticKeyProvider) Encrypt(ctx context.Context, plaintext []byte) ([]byte, error) {
	rawCT, err := EncryptSecret(p.entries[0].key, plaintext)
	if err != nil {
		return nil, err
	}
	return wrapWithPrefix(staticCiphertextPrefix, rawCT), nil
}

func (p *StaticKeyProvider) Decrypt(ctx context.Context, ciphertext []byte) ([]byte, error) {
	rawCT, err := unwrapPrefix(staticCiphertextPrefix, ciphertext)
	if err != nil {
		return nil, err
	}
	// rawCT is the inner blob when the prefix matched; for legacy
	// un-prefixed ciphertexts unwrapPrefix returns the original bytes
	// unchanged (see its doc comment) so the multi-key decrypt below
	// handles both shapes via a single code path.
	var lastErr error
	for _, e := range p.entries {
		pt, err := DecryptSecret(e.key, rawCT)
		if err == nil {
			return pt, nil
		}
		lastErr = err
	}
	return nil, lastErr
}

// SealedKeyProvider holds the unsealed root key in process memory. It defends
// against attackers who can read the sealed file or the node disk but NOT the
// passphrase: the on-disk file is Argon2id-wrapped and is useless without the
// passphrase. It does NOT defend against process-level compromise of the API
// pod — once the key is unsealed at boot it lives in memory, and an attacker
// who can run code in the pod can call Decrypt exactly as the application does.
// See pkg/secrets/README.md for the full threat model.
//
// US-50.4 multi-key support (NewStaticKeyProviderMultiVersion) is NOT mirrored
// here yet. The sealed provider is constructed once at boot from a single
// sealed file; multi-file rotation-window support for the sealed path will be
// added alongside US-50.5 (rotate-kek CLI) when the rotation workflow is
// exercised end-to-end. The StaticKeyProvider covers the default Helm path.
type SealedKeyProvider struct {
	key []byte
}

func NewSealedKeyProvider(sealedKeyPath, passphrasePath string) (*SealedKeyProvider, error) {
	passphrase, err := os.ReadFile(passphrasePath)
	if err != nil {
		return nil, fmt.Errorf("reading passphrase file: %w", err)
	}

	sealedData, err := os.ReadFile(sealedKeyPath)
	if err != nil {
		return nil, fmt.Errorf("reading sealed key file: %w", err)
	}

	key, err := unsealKey(passphrase, sealedData)
	if err != nil {
		return nil, fmt.Errorf("unseal: %w", err)
	}

	return &SealedKeyProvider{key: key}, nil
}

func (p *SealedKeyProvider) Encrypt(ctx context.Context, plaintext []byte) ([]byte, error) {
	rawCT, err := EncryptSecret(p.key, plaintext)
	if err != nil {
		return nil, err
	}
	return wrapWithPrefix(staticCiphertextPrefix, rawCT), nil
}

func (p *SealedKeyProvider) Decrypt(ctx context.Context, ciphertext []byte) ([]byte, error) {
	rawCT, err := unwrapPrefix(staticCiphertextPrefix, ciphertext)
	if err != nil {
		return nil, err
	}
	return DecryptSecret(p.key, rawCT)
}

// wrapWithPrefix prepends prefix + base64(rawCT) to produce the
// self-identifying ciphertext format used by CompositeProvider dispatch.
// The base64 layer keeps the prefix byte-aligned for prefix-sniffing
// (raw AES-GCM blobs can contain any byte including ':' which would
// otherwise confuse prefix detection).
func wrapWithPrefix(prefix string, rawCT []byte) []byte {
	enc := base64.StdEncoding.EncodeToString(rawCT)
	out := make([]byte, 0, len(prefix)+len(enc))
	out = append(out, prefix...)
	out = append(out, enc...)
	return out
}

// unwrapPrefix inspects ciphertext and routes by prefix:
//
//   - If ciphertext begins with prefix: strip prefix + base64-decode the
//     remainder, returning the inner raw blob for the caller to AES-GCM
//     decrypt. A base64 decode failure on a prefix-matching ciphertext is
//     returned as ErrDecryptionFailed (the prefix identified this as ours,
//     so the row is corrupt rather than routed wrong).
//   - If ciphertext begins with a DIFFERENT known-provider prefix
//     (foreign routing signal): return ErrNotMyCiphertext so the composite
//     tries the next provider. We detect "foreign prefix" by checking for
//     any of the registered provider prefixes (see knownForeignPrefixes).
//   - If ciphertext has no recognised prefix: treat as a legacy un-prefixed
//     blob (pre-US-57.1 format) and return it verbatim. This is the
//     backward-compatibility path — every row written before US-57.1 lands
//     here, and the caller's AES-GCM decrypt handles it as before.
//
// The legacy-fallback branch is the reason unwrapPrefix takes the prefix
// as an argument rather than being a method on the provider: a future
// non-local provider (KMS) calls unwrapPrefix with its own prefix, and
// its legacy fallback is "return ErrNotMyCiphertext" because cloud-KMS
// ciphertexts always carry the cloud prefix — there is no pre-KMS legacy
// format to fall back to for those providers. The local-provider legacy
// fallback is the unique exception.
func unwrapPrefix(prefix string, ciphertext []byte) ([]byte, error) {
	if bytes.HasPrefix(ciphertext, []byte(prefix)) {
		body := ciphertext[len(prefix):]
		raw, err := base64.StdEncoding.DecodeString(string(body))
		if err != nil {
			return nil, fmt.Errorf("%w: base64 decode of %q-prefixed ciphertext: %v",
				ErrDecryptionFailed, prefix, err)
		}
		return raw, nil
	}
	// Foreign prefix check — list is small (one entry today: the KMS
	// prefix family). Adding a new provider means adding its prefix here
	// so existing providers correctly return ErrNotMyCiphertext rather
	// than falling through to the legacy-blob path.
	for _, fp := range knownForeignPrefixes {
		if bytes.HasPrefix(ciphertext, []byte(fp)) {
			return nil, ErrNotMyCiphertext
		}
	}
	// No recognised prefix — legacy un-prefixed blob. Return as-is.
	return ciphertext, nil
}

// knownForeignPrefixes is the set of ciphertext prefixes from providers
// other than the local static/sealed provider. Maintained alongside
// provider implementations; today only the AWS KMS prefix lives here.
// GCP KMS will add its prefix when US-57.3 lands.
//
// Kept as a package-level slice rather than registered dynamically so the
// full set is visible at the call site — dynamic registration would make
// routing behaviour depend on init order, which is exactly the kind of
// magic the codebase rules out (README-LLM.md §3, "Explicit Over Implicit").
var knownForeignPrefixes = []string{
	"aws-kms:v1:",
}

func SealRootKey(path string, passphrase, rootKey []byte) error {
	salt := make([]byte, sealedSaltSize)
	if _, err := rand.Read(salt); err != nil {
		return fmt.Errorf("generating salt: %w", err)
	}

	kek, err := DeriveSealedKEK(passphrase, salt, sealedKeyInfoStr)
	if err != nil {
		return fmt.Errorf("deriving KEK: %w", err)
	}

	ct, err := EncryptSecret(kek, rootKey)
	if err != nil {
		return fmt.Errorf("encrypting root key: %w", err)
	}

	// V1 format (US-50.11): magic || salt || ciphertext. The magic marks the
	// info-domain-separated KEK derivation so unsealKey can route V1 vs the
	// legacy salt||ciphertext layout.
	sealed := make([]byte, 0, len(sealedMagicV1)+sealedSaltSize+len(ct))
	sealed = append(sealed, []byte(sealedMagicV1)...)
	sealed = append(sealed, salt...)
	sealed = append(sealed, ct...)

	return os.WriteFile(path, sealed, 0600)
}

// unsealKey routes by magic prefix. V1 files (magic "LSKP-S", US-50.11) use
// the info-domain-separated KEK; files without the prefix are legacy V0 and
// use plain Argon2id without an HKDF info string.
//
// A random V0 salt starting with the ASCII bytes "LSKP-S" would misdetect as
// V1; that is a 1/2^48 event and would surface as a clean decrypt failure
// (wrong KEK), never silent data corruption.
func unsealKey(passphrase, sealedData []byte) ([]byte, error) {
	if bytes.HasPrefix(sealedData, []byte(sealedMagicV1)) {
		return unsealKeyV1(passphrase, sealedData)
	}
	return unsealKeyV0(passphrase, sealedData)
}

// unsealKeyV1 reads the V1 layout: magic || salt || ciphertext.
func unsealKeyV1(passphrase, sealedData []byte) ([]byte, error) {
	body := sealedData[len(sealedMagicV1):]
	if len(body) < sealedSaltSize+sealedNonceSize+16 {
		return nil, fmt.Errorf("sealed data too short: %d bytes", len(sealedData))
	}
	salt := body[:sealedSaltSize]
	ct := body[sealedSaltSize:]

	kek, err := DeriveSealedKEK(passphrase, salt, sealedKeyInfoStr)
	if err != nil {
		return nil, fmt.Errorf("deriving KEK: %w", err)
	}

	rootKey, err := DecryptSecret(kek, ct)
	if err != nil {
		return nil, fmt.Errorf("decrypting sealed key: %w", err)
	}

	if len(rootKey) != 32 {
		return nil, fmt.Errorf("unsealed key must be 32 bytes, got %d", len(rootKey))
	}
	return rootKey, nil
}

// unsealKeyV0 reads the legacy layout: salt || ciphertext, with the KEK
// derived via Argon2id without an HKDF info string. Retained so sealed-key
// files produced before US-50.11 continue to unseal.
func unsealKeyV0(passphrase, sealedData []byte) ([]byte, error) {
	if len(sealedData) < sealedSaltSize+sealedNonceSize+16 {
		return nil, fmt.Errorf("sealed data too short: %d bytes", len(sealedData))
	}
	salt := sealedData[:sealedSaltSize]
	ct := sealedData[sealedSaltSize:]

	kek, err := DeriveKEKFromPassword(passphrase, salt)
	if err != nil {
		return nil, fmt.Errorf("deriving KEK: %w", err)
	}

	rootKey, err := DecryptSecret(kek, ct)
	if err != nil {
		return nil, fmt.Errorf("decrypting sealed key: %w", err)
	}

	if len(rootKey) != 32 {
		return nil, fmt.Errorf("unsealed key must be 32 bytes, got %d", len(rootKey))
	}
	return rootKey, nil
}
