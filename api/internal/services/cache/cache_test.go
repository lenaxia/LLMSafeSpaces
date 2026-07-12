// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package cache

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"fmt"
	"math/big"
	"strconv"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/go-redis/redis/v8"
	"github.com/lenaxia/llmsafespaces/api/internal/config"
	"github.com/lenaxia/llmsafespaces/api/internal/logger"
	"github.com/lenaxia/llmsafespaces/pkg/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Helper function to create a test config
func createTestConfig(redisAddr string) *config.Config {
	host, port, _ := splitHostPort(redisAddr)
	cfg := &config.Config{}
	cfg.Redis.Host = host
	cfg.Redis.Port = port
	cfg.Redis.Password = ""
	cfg.Redis.DB = 0
	cfg.Redis.PoolSize = 10
	return cfg
}

// Helper function to split host:port into separate values
func splitHostPort(addr string) (string, int, error) {
	for i := len(addr) - 1; i >= 0; i-- {
		if addr[i] == ':' {
			port, err := strconv.Atoi(addr[i+1:])
			if err != nil {
				return "", 0, err
			}
			return addr[:i], port, nil
		}
	}
	return "", 0, errors.New("invalid address format")
}

func setupMockRedis(t *testing.T) (*Service, *miniredis.Miniredis, func()) {
	// Create a mock Redis server
	mr, err := miniredis.Run()
	require.NoError(t, err, "Failed to create mock Redis")

	// Create a mock logger
	mockLogger, err := logger.New(true, "debug", "console")
	require.NoError(t, err, "Failed to create mock logger")

	// Create a mock config
	mockConfig := createTestConfig(mr.Addr())

	// Create Redis client
	client := redis.NewClient(&redis.Options{
		Addr:     mr.Addr(),
		Password: "",
		DB:       0,
		PoolSize: 10,
	})

	// Create the cache service with the mock Redis
	service := &Service{
		logger: mockLogger,
		config: mockConfig,
		client: client,
	}

	// Return the service, mock Redis, and a cleanup function
	return service, mr, func() {
		client.Close()
		mr.Close()
	}
}

// TestNew tests the New function
func TestNew(t *testing.T) {
	// Start a mock Redis server
	mr, err := miniredis.Run()
	require.NoError(t, err)
	defer mr.Close()

	// Create test dependencies
	log, err := logger.New(true, "debug", "console")
	require.NoError(t, err)

	// Create a valid config
	cfg := createTestConfig(mr.Addr())

	// Test successful creation
	service, err := New(cfg, log)
	assert.NoError(t, err)
	assert.NotNil(t, service)

	// Clean up
	err = service.Stop()
	assert.NoError(t, err)

	// Test connection failure
	badCfg := &config.Config{}
	badCfg.Redis.Host = "nonexistent"
	badCfg.Redis.Port = 6379

	service, err = New(badCfg, log)
	assert.Error(t, err)
	assert.Nil(t, service)
	assert.Contains(t, err.Error(), "failed to connect to Redis")
}

func TestPingCache(t *testing.T) {
	service, _, cleanup := setupMockRedis(t)
	defer cleanup()

	// Call the Ping method
	ctx := context.Background()
	err := service.Ping(ctx)
	assert.NoError(t, err, "Expected no error from Ping")
}

func TestGetSetDelete(t *testing.T) {
	service, mr, cleanup := setupMockRedis(t)
	defer cleanup()

	ctx := context.Background()
	key := "test_key"
	value := "test_value"

	// Test Set
	err := service.Set(ctx, key, value, time.Minute)
	assert.NoError(t, err, "Expected no error from Set")

	// Verify value was set in mock Redis
	got, err := mr.Get(key)
	assert.NoError(t, err, "Expected no error getting value from miniredis")
	assert.Equal(t, value, got, "Expected value %q in Redis, got %q", value, got)

	// Test Get
	gotValue, err := service.Get(ctx, key)
	assert.NoError(t, err, "Expected no error from Get")
	assert.Equal(t, value, gotValue, "Expected value %q, got %q", value, gotValue)

	// Test Delete
	err = service.Delete(ctx, key)
	assert.NoError(t, err, "Expected no error from Delete")

	// Verify key was deleted
	assert.False(t, mr.Exists(key), "Expected key %q to be deleted", key)

	// Test Get non-existent key
	gotValue, err = service.Get(ctx, "non_existent_key")
	assert.NoError(t, err, "Expected no error for non-existent key")
	assert.Equal(t, "", gotValue, "Expected empty string for non-existent key")
}

func TestGetSetObject(t *testing.T) {
	service, _, cleanup := setupMockRedis(t)
	defer cleanup()

	ctx := context.Background()
	key := "test_object_key"
	value := map[string]interface{}{
		"name":  "test",
		"value": 123,
		"tags":  []string{"tag1", "tag2"},
	}

	// Test SetObject
	err := service.SetObject(ctx, key, value, time.Minute)
	assert.NoError(t, err, "Expected no error from SetObject")

	// Test GetObject
	var retrievedValue map[string]interface{}
	err = service.GetObject(ctx, key, &retrievedValue)
	assert.NoError(t, err, "Expected no error from GetObject")

	// Check if retrieved value matches original
	assert.Equal(t, value["name"], retrievedValue["name"], "Expected name %v, got %v", value["name"], retrievedValue["name"])
	assert.Equal(t, float64(value["value"].(int)), retrievedValue["value"].(float64), "Expected value %v, got %v", value["value"], retrievedValue["value"])

	// Test GetObject for non-existent key
	var emptyValue map[string]interface{}
	err = service.GetObject(ctx, "non_existent_key", &emptyValue)
	assert.NoError(t, err, "Expected no error for non-existent key")
	assert.Nil(t, emptyValue, "Expected nil for non-existent key")
}

func TestSessionOperations(t *testing.T) {
	service, _, cleanup := setupMockRedis(t)
	defer cleanup()

	ctx := context.Background()
	sessionID := "test_session_id"
	session := types.CachedSession{
		SessionID:   sessionID,
		UserID:      "user123",
		WorkspaceID: "workspace456",
	}

	// SetSession stores the typed session
	err := service.SetSession(ctx, sessionID, session, time.Minute)
	assert.NoError(t, err)

	// GetSession retrieves it back with correct fields
	retrieved, err := service.GetSession(ctx, sessionID)
	assert.NoError(t, err)
	assert.Equal(t, session.SessionID, retrieved.SessionID)
	assert.Equal(t, session.UserID, retrieved.UserID)
	assert.Equal(t, session.WorkspaceID, retrieved.WorkspaceID)

	// DeleteSession removes it
	err = service.DeleteSession(ctx, sessionID)
	assert.NoError(t, err)

	// GetSession after delete returns nil, no error
	deleted, err := service.GetSession(ctx, sessionID)
	assert.NoError(t, err)
	assert.Nil(t, deleted)
}

// ---------------------------------------------------------------------------
// Edge cases: Get with missing key
// ---------------------------------------------------------------------------

func TestGet_MissingKey_ReturnsEmptyStringNoError(t *testing.T) {
	service, _, cleanup := setupMockRedis(t)
	defer cleanup()

	val, err := service.Get(context.Background(), "this_key_does_not_exist")
	assert.NoError(t, err)
	assert.Equal(t, "", val)
}

// ---------------------------------------------------------------------------
// Edge cases: GetObject with corrupt JSON
// ---------------------------------------------------------------------------

func TestGetObject_CorruptJSON_ReturnsError(t *testing.T) {
	service, mr, cleanup := setupMockRedis(t)
	defer cleanup()

	// Store invalid JSON directly into miniredis
	require.NoError(t, mr.Set("bad_json_key", "not-valid-json"))

	var result map[string]interface{}
	err := service.GetObject(context.Background(), "bad_json_key", &result)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "failed to unmarshal")
}

// ---------------------------------------------------------------------------
// Edge cases: Set with zero expiration
// ---------------------------------------------------------------------------

func TestSet_ZeroExpiration_KeyPersists(t *testing.T) {
	service, mr, cleanup := setupMockRedis(t)
	defer cleanup()

	ctx := context.Background()
	key := "persistent_key"

	err := service.Set(ctx, key, "value", 0)
	assert.NoError(t, err)

	// Key should still exist immediately
	val, err := mr.Get(key)
	assert.NoError(t, err)
	assert.Equal(t, "value", val)

	// Advance time significantly — key with 0 TTL should not expire
	mr.FastForward(24 * time.Hour)
	val, err = mr.Get(key)
	assert.NoError(t, err)
	assert.Equal(t, "value", val)
}

// ---------------------------------------------------------------------------
// Edge cases: SetSession / GetSession TTL expiry
// ---------------------------------------------------------------------------

func TestSetSession_GetSession_TTLExpiry(t *testing.T) {
	service, mr, cleanup := setupMockRedis(t)
	defer cleanup()

	ctx := context.Background()
	sessionID := "expiring-session"
	session := types.CachedSession{
		SessionID:   sessionID,
		UserID:      "user-ttl",
		WorkspaceID: "ws-ttl",
	}

	// Store with a 1-second TTL
	err := service.SetSession(ctx, sessionID, session, time.Second)
	require.NoError(t, err)

	// Retrievable before expiry
	retrieved, err := service.GetSession(ctx, sessionID)
	require.NoError(t, err)
	require.NotNil(t, retrieved)
	assert.Equal(t, session.UserID, retrieved.UserID)

	// Fast-forward past TTL
	mr.FastForward(2 * time.Second)

	// Must return nil after expiry
	expired, err := service.GetSession(ctx, sessionID)
	assert.NoError(t, err)
	assert.Nil(t, expired)
}

func TestGetSession_MissingKey_ReturnsNilNoError(t *testing.T) {
	service, _, cleanup := setupMockRedis(t)
	defer cleanup()

	result, err := service.GetSession(context.Background(), "missing-session-id")
	assert.NoError(t, err)
	assert.Nil(t, result)
}

func TestStartStop(t *testing.T) {
	service, _, cleanup := setupMockRedis(t)
	defer cleanup()

	// Test Start
	err := service.Start()
	assert.NoError(t, err, "Expected no error from Start")

	// Test Stop - this is already called in cleanup, but we test it explicitly
	err = service.Stop()
	assert.NoError(t, err, "Expected no error from Stop")
}

// ─── #465: Redis TLS support ────────────────────────────────────────────────

// TestNewCache_TLS_ConnectsAndPings stands up a TLS-enabled miniredis,
// points the cache service at it with cfg.Redis.TLS=true, and asserts the
// ping succeeds. Pre-fix (no TLS field): the client connected in plaintext
// and the TLS-enabled server rejected the connection with a silent timeout.
func TestNewCache_TLS_ConnectsAndPings(t *testing.T) {
	// Generate a self-signed cert for the test server at runtime.
	// Avoids embedding PEM bytes that might not parse; the cert is
	// single-use (lives only as long as the test).
	cert, err := selfSignedCert()
	require.NoError(t, err, "failed to generate test cert")
	mrTLS := &tls.Config{Certificates: []tls.Certificate{cert}}
	mr, err := miniredis.RunTLS(mrTLS)
	require.NoError(t, err, "Failed to create TLS-enabled mock Redis")
	defer mr.Close()

	cfg := createTestConfig(mr.Addr())
	cfg.Redis.TLS = true
	cfg.Redis.InsecureSkipVerify = true // self-signed cert; dev-only

	log, err := logger.New(true, "debug", "console")
	require.NoError(t, err)

	svc, err := New(cfg, log)
	require.NoError(t, err, "TLS-enabled cache service must construct and ping successfully")
	defer svc.Stop()

	// Functional smoke test: a SET + GET round-trip.
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	require.NoError(t, svc.Set(ctx, "tls-test", "ok", time.Minute))
	got, err := svc.Get(ctx, "tls-test")
	require.NoError(t, err)
	assert.Equal(t, "ok", got)
}

// TestNewCache_TLS_DisabledFallsBackToPlaintext is the regression guard
// for the default path: cfg.Redis.TLS=false (the chart default) must
// produce a plaintext client. Pre-existing behavior — pinned so the TLS
// addition is provably non-regressive.
func TestNewCache_TLS_DisabledFallsBackToPlaintext(t *testing.T) {
	// Plain miniredis (no TLS).
	mr, err := miniredis.Run()
	require.NoError(t, err)
	defer mr.Close()

	cfg := createTestConfig(mr.Addr())
	// cfg.Redis.TLS is false by default (zero value of bool).
	log, err := logger.New(true, "debug", "console")
	require.NoError(t, err)

	svc, err := New(cfg, log)
	require.NoError(t, err, "plaintext cache service must still construct when TLS field exists but is false")
	defer svc.Stop()
}

// (TestIsTruthy moved to config package — isTruthy is unexported there,
// so the cache package cannot test it directly. The config package's
// own tests cover the boolean parser for LLMSAFESPACES_REDIS_TLS.)

// selfSignedCert generates a fresh self-signed RSA cert + key pair for
// the TLS test server. Generated at runtime to avoid embedding PEM bytes
// that might not parse after copy-paste (the previous inline PEM had
// truncation issues). RSA-2048 is fast enough for test-only generation.
func selfSignedCert() (tls.Certificate, error) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("generate RSA key: %w", err)
	}
	tmpl := x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "localhost"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		KeyUsage:              x509.KeyUsageKeyEncipherment | x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		DNSNames:              []string{"localhost"},
	}
	der, err := x509.CreateCertificate(rand.Reader, &tmpl, &tmpl, &key.PublicKey, key)
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("create cert: %w", err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}, nil
}
