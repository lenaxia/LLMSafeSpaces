package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/lenaxia/llmsafespaces/api/internal/config"
	"github.com/lenaxia/llmsafespaces/api/internal/logger"
	"github.com/lenaxia/llmsafespaces/api/internal/mocks"
	"github.com/lenaxia/llmsafespaces/pkg/secrets"
	"github.com/lenaxia/llmsafespaces/pkg/types"
	pkgutil "github.com/lenaxia/llmsafespaces/pkg/utilities"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

func newDEKTestService(t *testing.T) (*Service, *mocks.MockDatabaseService, *mocks.MockCacheService, *dekJKeyService) {
	t.Helper()
	log, _ := logger.New(true, "debug", "console")
	cfg := &config.Config{}
	cfg.Auth.JWTSecret = "test-secret-dek-1234567890"
	cfg.Auth.TokenDuration = 24 * time.Hour
	cfg.Auth.APIKeyPrefix = "lsp_"
	mockDb := new(mocks.MockDatabaseService)
	mockCache := new(mocks.MockCacheService)
	svc, err := New(cfg, log, mockDb, mockCache)
	require.NoError(t, err)
	ks := &dekJKeyService{}
	svc.SetKeyService(ks)
	return svc, mockDb, mockCache, ks
}

type dekJKeyService struct {
	dek       []byte
	sessionID string
	getDEKErr error
}

func (d *dekJKeyService) InitializeUserKeysServerKEK(_ context.Context, _ string, _ string) error {
	return nil
}
func (d *dekJKeyService) UnlockDEK(_ context.Context, _ string, _ []byte, _ string, _ time.Duration) error {
	return nil
}

func (d *dekJKeyService) UnlockDEKWithSigningKey(ctx context.Context, userID string, password []byte, sessionID string, ttl time.Duration, _ []byte) error {
	return d.UnlockDEK(ctx, userID, password, sessionID, ttl)
}

func (d *dekJKeyService) DeleteDurableSessionsForUser(_ context.Context, _ string) error {
	return nil
}
func (d *dekJKeyService) HasKeys(_ context.Context, _ string) (bool, error) {
	return true, nil
}
func (d *dekJKeyService) GetDEK(_ context.Context, sessionID string, _ []byte) ([]byte, error) {
	if d.getDEKErr != nil {
		return nil, d.getDEKErr
	}
	if d.dek != nil && (d.sessionID == "" || d.sessionID == sessionID) {
		cp := make([]byte, len(d.dek))
		copy(cp, d.dek)
		return cp, nil
	}
	return nil, errors.New("DEK not available")
}
func (d *dekJKeyService) CacheDEK(_ context.Context, _ string, _ []byte, _ time.Duration) error {
	return nil
}

func withMasterKey(t *testing.T, svc *Service) {
	t.Helper()
	key := make([]byte, 32)
	_, err := rand.Read(key)
	require.NoError(t, err)
	svc.SetMasterKey(key)
}

func TestCreateAPIKey_WithDecryptAccess_StoresWrappedDEK(t *testing.T) {
	svc, mockDb, _, ks := newDEKTestService(t)
	withMasterKey(t, svc)
	ctx := context.Background()

	dek := make([]byte, 32)
	_, _ = rand.Read(dek)
	ks.dek = dek
	ks.sessionID = "jwt-session-1"

	mockDb.On("CreateAPIKey", ctx, mock.MatchedBy(func(k *types.APIKey) bool {
		return k.Name == "my-key" &&
			k.DecryptAccess == true &&
			len(k.WrappedDEK) > 0 &&
			len(k.KekSalt) > 0 &&
			len(k.KeyCiphertext) > 0 &&
			k.DekSynced == true
	})).Return(nil)

	apiKey, err := svc.CreateAPIKey(ctx, "user-1", types.CreateAPIKeyRequest{
		Name:          "my-key",
		DecryptAccess: true,
	}, "jwt-session-1", nil)

	require.NoError(t, err)
	require.NotNil(t, apiKey)
	assert.True(t, apiKey.DecryptAccess)
	assert.True(t, len(apiKey.Key) > 32, "full key must be returned")
	mockDb.AssertExpectations(t)
}

func TestCreateAPIKey_WithDecryptAccess_RequiresJWTSession(t *testing.T) {
	svc, _, _, _ := newDEKTestService(t)
	withMasterKey(t, svc)
	ctx := context.Background()

	apiKey, err := svc.CreateAPIKey(ctx, "user-1", types.CreateAPIKeyRequest{
		Name:          "my-key",
		DecryptAccess: true,
	}, "", nil)

	assert.Error(t, err)
	assert.Nil(t, apiKey)
	assert.Contains(t, err.Error(), "JWT session required")
}

func TestCreateAPIKey_WithDecryptAccess_RequiresMasterKey(t *testing.T) {
	svc, _, _, _ := newDEKTestService(t)
	ctx := context.Background()

	apiKey, err := svc.CreateAPIKey(ctx, "user-1", types.CreateAPIKeyRequest{
		Name:          "my-key",
		DecryptAccess: true,
	}, "jwt-session-1", nil)

	assert.Error(t, err)
	assert.Nil(t, apiKey)
	assert.Contains(t, err.Error(), "root key not configured")
}

func TestCreateAPIKey_WithDecryptAccess_DEKNotAvailable(t *testing.T) {
	svc, mockDb, _, _ := newDEKTestService(t)
	withMasterKey(t, svc)
	ctx := context.Background()

	mockDb.On("CreateAPIKey", ctx, mock.Anything).Return(nil).Maybe()

	apiKey, err := svc.CreateAPIKey(ctx, "user-1", types.CreateAPIKeyRequest{
		Name:          "my-key",
		DecryptAccess: true,
	}, "jwt-session-1", nil)

	assert.Error(t, err)
	assert.Nil(t, apiKey)
	assert.Contains(t, err.Error(), "DEK not available")
}

func TestCreateAPIKey_WithoutDecryptAccess_NoWrappedDEK(t *testing.T) {
	svc, mockDb, _, _ := newDEKTestService(t)
	ctx := context.Background()

	mockDb.On("CreateAPIKey", ctx, mock.MatchedBy(func(k *types.APIKey) bool {
		return k.Name == "my-key" &&
			k.DecryptAccess == false &&
			k.WrappedDEK == nil &&
			k.KekSalt == nil &&
			k.KeyCiphertext == nil
	})).Return(nil)

	apiKey, err := svc.CreateAPIKey(ctx, "user-1", types.CreateAPIKeyRequest{
		Name:          "my-key",
		DecryptAccess: false,
	}, "", nil)

	require.NoError(t, err)
	require.NotNil(t, apiKey)
	assert.False(t, apiKey.DecryptAccess)
	mockDb.AssertExpectations(t)
}

func TestValidateAPIKey_WithDecryptAccess_UnlocksDEK(t *testing.T) {
	svc, mockDb, mockCache, ks := newDEKTestService(t)
	withMasterKey(t, svc)
	ctx := context.Background()

	dek := make([]byte, 32)
	_, _ = rand.Read(dek)
	ks.dek = dek
	ks.sessionID = "jwt-session-1"

	var storedKey *types.APIKey
	mockDb.On("CreateAPIKey", ctx, mock.Anything).Return(nil).Run(func(args mock.Arguments) {
		k := args.Get(1).(*types.APIKey)
		cp := *k
		storedKey = &cp
	})

	apiKey, err := svc.CreateAPIKey(ctx, "user-1", types.CreateAPIKeyRequest{
		Name:          "dek-key",
		DecryptAccess: true,
	}, "jwt-session-1", nil)
	require.NoError(t, err)
	require.NotNil(t, storedKey)

	rawKey := apiKey.Key
	h := sha256.Sum256([]byte(rawKey))
	keyHash := hex.EncodeToString(h[:])

	user := &types.User{ID: "user-1"}
	mockCache.On("Get", mock.Anything, "apikey:"+pkgutil.HashString(rawKey)).Return("", errors.New("not found")).Once()
	mockDb.On("GetUserByAPIKey", mock.Anything, keyHash).Return(user, nil).Once()
	mockDb.On("GetAPIKeyRecordByHash", mock.Anything, keyHash).Return(storedKey, nil).Once()
	mockCache.On("Set", mock.Anything, "apikey:"+pkgutil.HashString(rawKey), "user-1", mock.Anything).Return(nil).Once()
	mockCache.On("Set", mock.Anything, mock.MatchedBy(func(key string) bool {
		return len(key) > 4 && key[:4] == "dek:"
	}), mock.Anything, mock.Anything).Return(nil).Once()

	userID, err := svc.ValidateToken(context.Background(), rawKey)
	require.NoError(t, err)
	assert.Equal(t, "user-1", userID)

	mockDb.AssertExpectations(t)
}

func TestValidateAPIKey_WithoutDecryptAccess_NoDEK(t *testing.T) {
	svc, mockDb, mockCache, _ := newDEKTestService(t)
	ctx := context.Background()

	mockDb.On("CreateAPIKey", ctx, mock.Anything).Return(nil)
	apiKey, err := svc.CreateAPIKey(ctx, "user-1", types.CreateAPIKeyRequest{
		Name:          "no-dek-key",
		DecryptAccess: false,
	}, "", nil)
	require.NoError(t, err)

	rawKey := apiKey.Key
	h := sha256.Sum256([]byte(rawKey))
	keyHash := hex.EncodeToString(h[:])

	keyRec := &types.APIKey{ID: "k1", UserID: "user-1", DecryptAccess: false}
	user := &types.User{ID: "user-1"}
	mockCache.On("Get", mock.Anything, "apikey:"+pkgutil.HashString(rawKey)).Return("", errors.New("not found")).Once()
	mockDb.On("GetUserByAPIKey", mock.Anything, keyHash).Return(user, nil).Once()
	mockDb.On("GetAPIKeyRecordByHash", mock.Anything, keyHash).Return(keyRec, nil).Once()
	mockCache.On("Set", mock.Anything, "apikey:"+pkgutil.HashString(rawKey), "user-1", mock.Anything).Return(nil).Once()

	userID, err := svc.ValidateToken(context.Background(), rawKey)
	require.NoError(t, err)
	assert.Equal(t, "user-1", userID)

	mockDb.AssertExpectations(t)
}

func TestValidateAPIKey_WrappedDEKCorrupt_Fails(t *testing.T) {
	svc, mockDb, mockCache, _ := newDEKTestService(t)

	corruptKey := &types.APIKey{
		ID:            "k1",
		UserID:        "user-1",
		DecryptAccess: true,
		WrappedDEK:    []byte("corrupt-data-not-valid-aes-gcm"),
		KekSalt:       make([]byte, 32),
	}

	rawKey := "lsp_" + hex.EncodeToString(make([]byte, 32))
	h := sha256.Sum256([]byte(rawKey))
	keyHash := hex.EncodeToString(h[:])

	user := &types.User{ID: "user-1"}
	mockCache.On("Get", mock.Anything, "apikey:"+pkgutil.HashString(rawKey)).Return("", errors.New("not found")).Once()
	mockDb.On("GetUserByAPIKey", mock.Anything, keyHash).Return(user, nil).Once()
	mockDb.On("GetAPIKeyRecordByHash", mock.Anything, keyHash).Return(corruptKey, nil).Once()
	mockCache.On("Set", mock.Anything, "apikey:"+pkgutil.HashString(rawKey), "user-1", mock.Anything).Return(nil).Once()

	_, err := svc.ValidateToken(context.Background(), rawKey)
	assert.NoError(t, err)
}

func TestCreateAPIKey_DEKRoundTrip(t *testing.T) {
	svc, mockDb, _, ks := newDEKTestService(t)
	withMasterKey(t, svc)
	ctx := context.Background()

	originalDEK := make([]byte, 32)
	_, _ = rand.Read(originalDEK)
	ks.dek = originalDEK
	ks.sessionID = "jwt-session-rt"

	var storedKey *types.APIKey
	mockDb.On("CreateAPIKey", ctx, mock.Anything).Return(nil).Run(func(args mock.Arguments) {
		k := args.Get(1).(*types.APIKey)
		cp := *k
		storedKey = &cp
	})

	apiKey, err := svc.CreateAPIKey(ctx, "user-1", types.CreateAPIKeyRequest{
		Name:          "roundtrip-key",
		DecryptAccess: true,
	}, "jwt-session-rt", nil)
	require.NoError(t, err)

	rawKey := apiKey.Key
	apiKEK, err := secrets.DeriveKEKFromKey([]byte(rawKey), storedKey.KekSalt, "llmsafespaces-apikey-kek")
	require.NoError(t, err)

	recoveredDEK, err := secrets.DecryptSecret(apiKEK, storedKey.WrappedDEK)
	require.NoError(t, err)
	assert.Equal(t, originalDEK, recoveredDEK, "round-tripped DEK must match original")

	provider := svc.rootKeyProvider
	require.NotNil(t, provider)
	recoveredRaw, err := provider.Decrypt(context.Background(), storedKey.KeyCiphertext)
	require.NoError(t, err)
	assert.Equal(t, rawKey, string(recoveredRaw), "key_ciphertext must decrypt to raw key")

	t.Log("DEK round-trip verified: wrapped DEK correctly recovers original DEK")
}

func TestCreateAPIKey_NonDecryptKey_GetsKeyCiphertext(t *testing.T) {
	svc, mockDb, _, _ := newDEKTestService(t)

	masterKey := make([]byte, 32)
	_, _ = rand.Read(masterKey)
	provider, err := secrets.NewStaticKeyProvider(masterKey)
	require.NoError(t, err)
	svc.SetRootKeyProvider(provider)

	var storedKey *types.APIKey
	mockDb.On("CreateAPIKey", mock.Anything, mock.AnythingOfType("*types.APIKey")).Run(func(args mock.Arguments) {
		storedKey = args.Get(1).(*types.APIKey)
	}).Return(nil)

	apiKey, err := svc.CreateAPIKey(context.Background(), "user1", types.CreateAPIKeyRequest{
		Name:          "no-decrypt",
		DecryptAccess: false,
	}, "", nil)
	require.NoError(t, err)
	require.NotNil(t, apiKey)

	assert.False(t, storedKey.DecryptAccess)
	assert.Nil(t, storedKey.WrappedDEK)
	assert.Nil(t, storedKey.KekSalt)
	assert.NotNil(t, storedKey.KeyCiphertext, "non-decrypt key should still have key_ciphertext")
	assert.True(t, storedKey.KeyCiphertext != nil)

	recoveredRaw, decErr := provider.Decrypt(context.Background(), storedKey.KeyCiphertext)
	require.NoError(t, decErr)
	assert.Equal(t, apiKey.Key, string(recoveredRaw), "key_ciphertext must decrypt to raw key even for non-decrypt keys")
}

func TestCreateAPIKey_NoRootKeyProvider_NoKeyCiphertext(t *testing.T) {
	svc, mockDb, _, _ := newDEKTestService(t)

	var storedKey *types.APIKey
	mockDb.On("CreateAPIKey", mock.Anything, mock.AnythingOfType("*types.APIKey")).Run(func(args mock.Arguments) {
		storedKey = args.Get(1).(*types.APIKey)
	}).Return(nil)

	apiKey, err := svc.CreateAPIKey(context.Background(), "user1", types.CreateAPIKeyRequest{
		Name:          "basic",
		DecryptAccess: false,
	}, "", nil)
	require.NoError(t, err)
	require.NotNil(t, apiKey)
	assert.Nil(t, storedKey.KeyCiphertext, "without RootKeyProvider, key_ciphertext should be nil")
}

func TestValidateAPIKey_ConstantTimeCompare_RejectsMismatch(t *testing.T) {
	svc, mockDb, mockCache, _ := newDEKTestService(t)

	masterKey := make([]byte, 32)
	_, _ = rand.Read(masterKey)
	provider, err := secrets.NewStaticKeyProvider(masterKey)
	require.NoError(t, err)
	svc.SetRootKeyProvider(provider)

	rawKey := "lsp_a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4e5f6"
	h := sha256.Sum256([]byte(rawKey))
	keyHash := hex.EncodeToString(h[:])

	ct, err := provider.Encrypt(context.Background(), []byte(rawKey))
	require.NoError(t, err)

	keyRecord := &types.APIKey{
		ID:            "kr1",
		KeyCiphertext: ct,
		DecryptAccess: false,
	}

	mockCache.On("Get", mock.Anything, "apikey:"+pkgutil.HashString(rawKey)).Return("", errors.New("miss"))
	mockDb.On("GetUserByAPIKey", mock.Anything, keyHash).Return(&types.User{ID: "u1", Active: true}, nil)
	mockDb.On("GetAPIKeyRecordByHash", mock.Anything, keyHash).Return(keyRecord, nil)
	mockCache.On("Set", mock.Anything, mock.Anything, "u1", mock.Anything).Return(nil)

	userID, err := svc.validateAPIKey(context.Background(), rawKey, "")
	assert.NoError(t, err)
	assert.Equal(t, "u1", userID)

	wrongKey := "lsp_0000000000000000000000000000000000000000000000000000000000000000"
	wrongHash := sha256.Sum256([]byte(wrongKey))
	wrongHashStr := hex.EncodeToString(wrongHash[:])

	mockDb.On("GetUserByAPIKey", mock.Anything, wrongHashStr).Return(&types.User{ID: "u1", Active: true}, nil)
	mockDb.On("GetAPIKeyRecordByHash", mock.Anything, wrongHashStr).Return(keyRecord, nil)
	mockCache.On("Get", mock.Anything, "apikey:"+pkgutil.HashString(wrongKey)).Return("", errors.New("miss"))

	userID2, err2 := svc.validateAPIKey(context.Background(), wrongKey, "")
	assert.Error(t, err2, "wrong key should fail constant-time comparison")
	assert.Equal(t, "", userID2)
}

func TestCreateAPIKey_WithSealedKeyProvider(t *testing.T) {
	svc, mockDb, _, _ := newDEKTestService(t)

	tmpDir := t.TempDir()
	sealedPath := tmpDir + "/sealed"
	passPath := tmpDir + "/passphrase"
	passphrase := []byte("test-sealed-passphrase")

	rootKey := make([]byte, 32)
	_, _ = rand.Read(rootKey)
	require.NoError(t, secrets.SealRootKey(sealedPath, passphrase, rootKey))
	require.NoError(t, writeToFile(passPath, passphrase))

	provider, err := secrets.NewSealedKeyProvider(sealedPath, passPath)
	require.NoError(t, err)
	svc.SetRootKeyProvider(provider)

	var storedKey *types.APIKey
	mockDb.On("CreateAPIKey", mock.Anything, mock.AnythingOfType("*types.APIKey")).Run(func(args mock.Arguments) {
		storedKey = args.Get(1).(*types.APIKey)
	}).Return(nil)

	apiKey, err := svc.CreateAPIKey(context.Background(), "user1", types.CreateAPIKeyRequest{
		Name:          "sealed-test",
		DecryptAccess: false,
	}, "", nil)
	require.NoError(t, err)
	require.NotNil(t, apiKey)
	assert.NotNil(t, storedKey.KeyCiphertext)

	recoveredRaw, decErr := provider.Decrypt(context.Background(), storedKey.KeyCiphertext)
	require.NoError(t, decErr)
	assert.Equal(t, apiKey.Key, string(recoveredRaw))
}

func TestValidateAPIKey_DEKNotSynced_SkipsUnwrap(t *testing.T) {
	svc, mockDb, mockCache, ks := newDEKTestService(t)

	masterKey := make([]byte, 32)
	_, _ = rand.Read(masterKey)
	provider, err := secrets.NewStaticKeyProvider(masterKey)
	require.NoError(t, err)
	svc.SetRootKeyProvider(provider)

	originalDEK := []byte("original-dek-32-bytes-long-enough!!")
	ks.dek = originalDEK
	ks.sessionID = "jwt-session-1"

	rawKey := "lsp_sync_test_a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4"
	h := sha256.Sum256([]byte(rawKey))
	keyHash := hex.EncodeToString(h[:])

	ct, encErr := provider.Encrypt(context.Background(), []byte(rawKey))
	require.NoError(t, encErr)

	kekSalt := make([]byte, 32)
	_, _ = rand.Read(kekSalt)
	apiKEK, _ := secrets.DeriveKEKFromKey([]byte(rawKey), kekSalt, "llmsafespaces-apikey-kek")
	wrappedDEK, _ := secrets.EncryptSecret(apiKEK, originalDEK)

	keyRecord := &types.APIKey{
		ID:            "kr-sync",
		KeyCiphertext: ct,
		DecryptAccess: true,
		KekSalt:       kekSalt,
		WrappedDEK:    wrappedDEK,
		DekSynced:     false,
	}

	mockCache.On("Get", mock.Anything, mock.Anything).Return("", errors.New("miss"))
	mockDb.On("GetUserByAPIKey", mock.Anything, keyHash).Return(&types.User{ID: "u1", Active: true}, nil)
	mockDb.On("GetAPIKeyRecordByHash", mock.Anything, keyHash).Return(keyRecord, nil)
	mockCache.On("Set", mock.Anything, mock.Anything, "u1", mock.Anything).Return(nil)

	userID, err := svc.validateAPIKey(context.Background(), rawKey, "")
	assert.NoError(t, err, "auth should still succeed with dek_synced=false")
	assert.Equal(t, "u1", userID)

	sessionID := "apikey:" + pkgutil.HashString(rawKey)
	cachedDEK, cacheErr := ks.GetDEK(context.Background(), sessionID, nil)
	assert.Error(t, cacheErr, "DEK should NOT be cached when dek_synced=false")
	assert.Nil(t, cachedDEK)
}

func writeToFile(path string, data []byte) error {
	return os.WriteFile(path, data, 0600)
}

func TestIPInAnyCIDR(t *testing.T) {
	tests := []struct {
		name     string
		clientIP string
		cidrs    []string
		want     bool
	}{
		{"matching CIDR", "10.0.1.5", []string{"10.0.0.0/8"}, true},
		{"non-matching CIDR", "192.168.1.5", []string{"10.0.0.0/8"}, false},
		{"multiple CIDRs first match", "10.0.1.5", []string{"192.168.0.0/16", "10.0.0.0/8"}, true},
		{"multiple CIDRs no match", "172.16.1.5", []string{"10.0.0.0/8", "192.168.0.0/16"}, false},
		{"empty CIDR list", "10.0.1.5", []string{}, false},
		{"invalid IP", "not-an-ip", []string{"10.0.0.0/8"}, false},
		{"invalid CIDR skipped", "10.0.1.5", []string{"not-a-cidr", "10.0.0.0/8"}, true},
		{"IPv4 in /24", "10.0.1.100", []string{"10.0.1.0/24"}, true},
		{"IPv4 outside /24", "10.0.2.1", []string{"10.0.1.0/24"}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, ipInAnyCIDR(tt.clientIP, tt.cidrs))
		})
	}
}

func TestValidateAPIKey_CIDREnforcement(t *testing.T) {
	svc, mockDb, mockCache, _ := newDEKTestService(t)

	masterKey := make([]byte, 32)
	_, _ = rand.Read(masterKey)
	provider, err := secrets.NewStaticKeyProvider(masterKey)
	require.NoError(t, err)
	svc.SetRootKeyProvider(provider)

	rawKey := "lsp_cidr_test_a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4"
	h := sha256.Sum256([]byte(rawKey))
	keyHash := hex.EncodeToString(h[:])

	ct, encErr := provider.Encrypt(context.Background(), []byte(rawKey))
	require.NoError(t, encErr)

	keyRecord := &types.APIKey{
		ID:            "kr-cidr",
		KeyCiphertext: ct,
		DecryptAccess: false,
		AllowedCIDRs:  []string{"10.0.0.0/8", "172.16.0.0/12"},
	}

	mockCache.On("Get", mock.Anything, "apikey:"+pkgutil.HashString(rawKey)).Return("", errors.New("miss"))
	mockDb.On("GetUserByAPIKey", mock.Anything, keyHash).Return(&types.User{ID: "u1", Active: true}, nil)
	mockDb.On("GetAPIKeyRecordByHash", mock.Anything, keyHash).Return(keyRecord, nil)
	mockCache.On("Set", mock.Anything, mock.Anything, "u1", mock.Anything).Return(nil)

	userID, err := svc.validateAPIKey(context.Background(), rawKey, "10.0.1.5")
	assert.NoError(t, err)
	assert.Equal(t, "u1", userID)

	mockCache.On("Get", mock.Anything, mock.Anything).Return("", errors.New("miss")).Once()
	userID, err = svc.validateAPIKey(context.Background(), rawKey, "192.168.1.5")
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "not in allowed ranges")
	assert.Equal(t, "", userID)
}

func TestValidateAPIKey_CIDRCacheBypass_Blocked(t *testing.T) {
	svc, mockDb, mockCache, _ := newDEKTestService(t)

	masterKey := make([]byte, 32)
	_, _ = rand.Read(masterKey)
	provider, err := secrets.NewStaticKeyProvider(masterKey)
	require.NoError(t, err)
	svc.SetRootKeyProvider(provider)

	rawKey := "lsp_cache_bypass_a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4"
	h := sha256.Sum256([]byte(rawKey))
	keyHash := hex.EncodeToString(h[:])

	ct, encErr := provider.Encrypt(context.Background(), []byte(rawKey))
	require.NoError(t, encErr)

	keyRecord := &types.APIKey{
		ID:            "kr-cache",
		KeyCiphertext: ct,
		DecryptAccess: false,
		AllowedCIDRs:  []string{"10.0.0.0/8"},
	}

	mockCache.On("Get", mock.Anything, "apikey:"+pkgutil.HashString(rawKey)).Return("u1", nil)
	mockDb.On("GetAPIKeyRecordByHash", mock.Anything, keyHash).Return(keyRecord, nil)

	userID, err := svc.validateAPIKey(context.Background(), rawKey, "192.168.1.5")
	assert.Error(t, err, "CIDR must be enforced even on cache hit")
	assert.Contains(t, err.Error(), "not in allowed ranges")
	assert.Equal(t, "", userID)
}

func TestValidateAPIKey_CIDRCacheHit_AllowedIP(t *testing.T) {
	svc, mockDb, mockCache, _ := newDEKTestService(t)

	masterKey := make([]byte, 32)
	_, _ = rand.Read(masterKey)
	provider, err := secrets.NewStaticKeyProvider(masterKey)
	require.NoError(t, err)
	svc.SetRootKeyProvider(provider)

	rawKey := "lsp_cache_allowed_a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4e5f6a1b2"
	h := sha256.Sum256([]byte(rawKey))
	keyHash := hex.EncodeToString(h[:])

	ct, encErr := provider.Encrypt(context.Background(), []byte(rawKey))
	require.NoError(t, encErr)

	keyRecord := &types.APIKey{
		ID:            "kr-cache2",
		KeyCiphertext: ct,
		DecryptAccess: false,
		AllowedCIDRs:  []string{"10.0.0.0/8"},
	}

	mockCache.On("Get", mock.Anything, "apikey:"+pkgutil.HashString(rawKey)).Return("u1", nil)
	mockDb.On("GetAPIKeyRecordByHash", mock.Anything, keyHash).Return(keyRecord, nil)

	userID, err := svc.validateAPIKey(context.Background(), rawKey, "10.0.1.5")
	assert.NoError(t, err, "CIDR check should pass for allowed IP on cache hit")
	assert.Equal(t, "u1", userID)
}
