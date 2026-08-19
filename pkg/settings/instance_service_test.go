// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package settings

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	pkginterfaces "github.com/lenaxia/llmsafespaces/pkg/interfaces"
)

// mockInstanceStore is a test double for InstanceStore.
type mockInstanceStore struct {
	mu         sync.Mutex
	data       map[string]json.RawMessage
	getErr     error
	setErr     error
	getCalls   int64
	getLatency time.Duration
}

func newMockStore() *mockInstanceStore {
	return &mockInstanceStore{data: make(map[string]json.RawMessage)}
}

func (m *mockInstanceStore) GetAllInstanceSettings(_ context.Context) (map[string]json.RawMessage, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	atomic.AddInt64(&m.getCalls, 1)
	if m.getLatency > 0 {
		m.mu.Unlock()
		time.Sleep(m.getLatency)
		m.mu.Lock()
	}
	if m.getErr != nil {
		return nil, m.getErr
	}
	cp := make(map[string]json.RawMessage, len(m.data))
	for k, v := range m.data {
		cp[k] = v
	}
	return cp, nil
}

func (m *mockInstanceStore) SetInstanceSetting(_ context.Context, key string, value json.RawMessage) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.setErr != nil {
		return m.setErr
	}
	m.data[key] = value
	return nil
}

func (m *mockInstanceStore) set(key string, value any) {
	raw, _ := json.Marshal(value)
	m.mu.Lock()
	m.data[key] = raw
	m.mu.Unlock()
}

// mockLogger satisfies pkginterfaces.LoggerInterface for testing.
type mockLogger struct{}

func (l *mockLogger) Debug(msg string, keysAndValues ...interface{})            {}
func (l *mockLogger) Info(msg string, keysAndValues ...interface{})             {}
func (l *mockLogger) Warn(msg string, keysAndValues ...interface{})             {}
func (l *mockLogger) Error(msg string, err error, keysAndValues ...interface{}) {}
func (l *mockLogger) Fatal(msg string, err error, keysAndValues ...interface{}) {}
func (l *mockLogger) With(keysAndValues ...interface{}) pkginterfaces.LoggerInterface {
	return l
}
func (l *mockLogger) Sync() error { return nil }

func newTestService(store *mockInstanceStore) *InstanceService {
	return NewInstanceService(store, &mockLogger{})
}

func TestInstanceService_GetBool_ReturnsDBValue(t *testing.T) {
	store := newMockStore()
	store.set("auth.registrationEnabled", false)
	svc := newTestService(store)

	val, err := svc.GetBool(context.Background(), "auth.registrationEnabled")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if val != false {
		t.Errorf("expected false, got %v", val)
	}
}

func TestInstanceService_GetBool_ReturnsDefault(t *testing.T) {
	store := newMockStore()
	svc := newTestService(store)

	val, err := svc.GetBool(context.Background(), "auth.registrationEnabled")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// Schema default is true
	if val != true {
		t.Errorf("expected true (default), got %v", val)
	}
}

func TestInstanceService_GetBool_UnknownKey(t *testing.T) {
	store := newMockStore()
	svc := newTestService(store)

	_, err := svc.GetBool(context.Background(), "nonexistent.key")
	if err == nil {
		t.Error("expected error for unknown key")
	}
}

func TestInstanceService_GetBool_TypeMismatch(t *testing.T) {
	store := newMockStore()
	store.set("auth.lockoutAttempts", 5) // int, not bool
	svc := newTestService(store)

	_, err := svc.GetBool(context.Background(), "auth.lockoutAttempts")
	if err == nil {
		t.Error("expected type mismatch error")
	}
}

func TestInstanceService_GetInt_ReturnsDBValue(t *testing.T) {
	store := newMockStore()
	store.set("auth.lockoutAttempts", 10)
	svc := newTestService(store)

	val, err := svc.GetInt(context.Background(), "auth.lockoutAttempts")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if val != 10 {
		t.Errorf("expected 10, got %d", val)
	}
}

func TestInstanceService_GetInt_ReturnsDefault(t *testing.T) {
	store := newMockStore()
	svc := newTestService(store)

	val, err := svc.GetInt(context.Background(), "auth.lockoutAttempts")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if val != 5 {
		t.Errorf("expected 5 (default), got %d", val)
	}
}

func TestInstanceService_GetString_ReturnsDBValue(t *testing.T) {
	store := newMockStore()
	store.set("workspace.defaultImage", "custom:v1")
	svc := newTestService(store)

	val, err := svc.GetString(context.Background(), "workspace.defaultImage")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if val != "custom:v1" {
		t.Errorf("expected custom:v1, got %q", val)
	}
}

func TestInstanceService_GetStrings_ReturnsDBValue(t *testing.T) {
	store := newMockStore()
	store.set("workspace.defaultNetworkAccess.egressDomains", []string{"example.com", "api.openai.com"})
	svc := newTestService(store)

	val, err := svc.GetStrings(context.Background(), "workspace.defaultNetworkAccess.egressDomains")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(val) != 2 || val[0] != "example.com" {
		t.Errorf("unexpected value: %v", val)
	}
}

func TestInstanceService_GetAll_MergesWithDefaults(t *testing.T) {
	store := newMockStore()
	store.set("auth.registrationEnabled", false)
	svc := newTestService(store)

	all, err := svc.GetAll(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Overridden value
	if all["auth.registrationEnabled"] != false {
		t.Errorf("expected false for overridden key")
	}
	// Default value
	if all["auth.lockoutEnabled"] != false {
		t.Errorf("expected false (default) for non-overridden key")
	}
	// All keys present
	if len(all) != len(InstanceSettings()) {
		t.Errorf("expected %d keys, got %d", len(InstanceSettings()), len(all))
	}
}

func TestInstanceService_Set_ValidValue(t *testing.T) {
	store := newMockStore()
	svc := newTestService(store)

	err := svc.Set(context.Background(), "auth.lockoutAttempts", 10)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Verify persisted
	val, err := svc.GetInt(context.Background(), "auth.lockoutAttempts")
	if err != nil {
		t.Fatalf("unexpected error on get: %v", err)
	}
	if val != 10 {
		t.Errorf("expected 10, got %d", val)
	}
}

func TestInstanceService_Set_InvalidValue_WrongType(t *testing.T) {
	store := newMockStore()
	svc := newTestService(store)

	err := svc.Set(context.Background(), "auth.lockoutAttempts", "not an int")
	if err == nil {
		t.Error("expected validation error for wrong type")
	}
}

func TestInstanceService_Set_InvalidValue_OutOfRange(t *testing.T) {
	store := newMockStore()
	svc := newTestService(store)

	err := svc.Set(context.Background(), "auth.lockoutAttempts", 999)
	if err == nil {
		t.Error("expected validation error for out of range")
	}
}

func TestInstanceService_Set_InvalidValue_BadEnum(t *testing.T) {
	store := newMockStore()
	svc := newTestService(store)

	err := svc.Set(context.Background(), "rateLimiting.strategy", "invalid_strategy")
	if err == nil {
		t.Error("expected validation error for invalid enum")
	}
}

func TestInstanceService_Set_UnknownKey(t *testing.T) {
	store := newMockStore()
	svc := newTestService(store)

	err := svc.Set(context.Background(), "nonexistent.key", true)
	if err == nil {
		t.Error("expected error for unknown key")
	}
}

// TestInstanceService_DevPreview_DefaultsAndRoundTrip locks the wiring issue
// #946 fixed: the Epic 66 keys must serve their defaults on an empty store
// (app.go reads devPreview.enabled at boot — an index miss previously made
// GetBool fail, the error was swallowed, and the feature booted disabled),
// and Set must accept them (the admin PUT /settings path — previously
// rejected as unknown key, leaving no way to enable or disable the feature).
func TestInstanceService_DevPreview_DefaultsAndRoundTrip(t *testing.T) {
	store := newMockStore()
	svc := newTestService(store)
	ctx := context.Background()

	enabled, err := svc.GetBool(ctx, "devPreview.enabled")
	if err != nil {
		t.Fatalf("GetBool(devPreview.enabled): %v", err)
	}
	if !enabled {
		t.Error("default devPreview.enabled = false, want true")
	}

	maxBytes, err := svc.GetInt(ctx, "devPreview.maxResponseBytes")
	if err != nil {
		t.Fatalf("GetInt(devPreview.maxResponseBytes): %v", err)
	}
	if maxBytes != 52428800 {
		t.Errorf("default devPreview.maxResponseBytes = %d, want 52428800", maxBytes)
	}

	maxConns, err := svc.GetInt(ctx, "devPreview.maxConnsPerWorkspace")
	if err != nil {
		t.Fatalf("GetInt(devPreview.maxConnsPerWorkspace): %v", err)
	}
	if maxConns != 50 {
		t.Errorf("default devPreview.maxConnsPerWorkspace = %d, want 50", maxConns)
	}

	if err := svc.Set(ctx, "devPreview.enabled", false); err != nil {
		t.Fatalf("Set(devPreview.enabled, false): %v", err)
	}
	enabled, err = svc.GetBool(ctx, "devPreview.enabled")
	if err != nil {
		t.Fatalf("GetBool(devPreview.enabled) after Set: %v", err)
	}
	if enabled {
		t.Error("devPreview.enabled after Set(false) = true, want false")
	}

	if err := svc.Set(ctx, "devPreview.maxConnsPerWorkspace", 10); err != nil {
		t.Fatalf("Set(devPreview.maxConnsPerWorkspace, 10): %v", err)
	}
	maxConns, err = svc.GetInt(ctx, "devPreview.maxConnsPerWorkspace")
	if err != nil {
		t.Fatalf("GetInt(devPreview.maxConnsPerWorkspace) after Set: %v", err)
	}
	if maxConns != 10 {
		t.Errorf("devPreview.maxConnsPerWorkspace after Set = %d, want 10", maxConns)
	}
}

func TestInstanceService_Set_InvalidatesCache(t *testing.T) {
	store := newMockStore()
	store.set("auth.lockoutAttempts", 5)
	svc := newTestService(store)

	// Prime cache
	val, _ := svc.GetInt(context.Background(), "auth.lockoutAttempts")
	if val != 5 {
		t.Fatalf("expected 5, got %d", val)
	}

	// Set new value
	if err := svc.Set(context.Background(), "auth.lockoutAttempts", 20); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Should get new value immediately (cache invalidated)
	val, _ = svc.GetInt(context.Background(), "auth.lockoutAttempts")
	if val != 20 {
		t.Errorf("expected 20 after set, got %d", val)
	}
}

func TestInstanceService_Singleflight_PreventsDuplicateDBCalls(t *testing.T) {
	store := newMockStore()
	store.set("auth.registrationEnabled", true)
	store.getLatency = 10 * time.Millisecond // simulate DB latency so singleflight can coalesce
	svc := newTestService(store)
	// Set TTL to 0 to force cache miss every time
	svc.ttl = 0

	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = svc.GetBool(context.Background(), "auth.registrationEnabled")
		}()
	}
	wg.Wait()

	// With singleflight, concurrent calls during the same load should coalesce.
	// We can't guarantee exactly 1 call due to timing, but it should be far fewer than 10.
	calls := atomic.LoadInt64(&store.getCalls)
	if calls > 5 {
		t.Errorf("expected singleflight to reduce DB calls, got %d", calls)
	}
}

func TestInstanceService_DBError_ReturnsError(t *testing.T) {
	store := newMockStore()
	store.getErr = fmt.Errorf("connection refused")
	svc := newTestService(store)

	_, err := svc.GetBool(context.Background(), "auth.registrationEnabled")
	if err == nil {
		t.Error("expected error when DB is down")
	}
}

func TestInstanceService_Set_DBWriteError(t *testing.T) {
	store := newMockStore()
	store.setErr = fmt.Errorf("write failed")
	svc := newTestService(store)

	err := svc.Set(context.Background(), "auth.lockoutAttempts", 10)
	if err == nil {
		t.Error("expected error when DB write fails")
	}
}

func TestInstanceService_CacheTTL_RefreshesAfterExpiry(t *testing.T) {
	store := newMockStore()
	store.set("auth.lockoutAttempts", 5)
	svc := newTestService(store)
	svc.ttl = 10 * time.Millisecond

	// Prime cache
	val, _ := svc.GetInt(context.Background(), "auth.lockoutAttempts")
	if val != 5 {
		t.Fatalf("expected 5, got %d", val)
	}

	// Update store directly (simulating another pod's write)
	store.set("auth.lockoutAttempts", 99)

	// Wait for TTL to expire
	time.Sleep(20 * time.Millisecond)

	// Should get new value after TTL
	val, _ = svc.GetInt(context.Background(), "auth.lockoutAttempts")
	if val != 99 {
		t.Errorf("expected 99 after TTL expiry, got %d", val)
	}
}

func TestInstanceService_Start_LoadsCache(t *testing.T) {
	store := newMockStore()
	store.set("auth.registrationEnabled", false)
	svc := newTestService(store)

	if err := svc.Start(); err != nil {
		t.Fatalf("Start failed: %v", err)
	}

	// Cache should be populated
	svc.mu.RLock()
	loaded := svc.data != nil
	svc.mu.RUnlock()
	if !loaded {
		t.Error("expected cache to be populated after Start")
	}
}

func TestInstanceService_Start_DBDown_ReturnsError(t *testing.T) {
	store := newMockStore()
	store.getErr = fmt.Errorf("connection refused")
	svc := newTestService(store)

	err := svc.Start()
	if err == nil {
		t.Error("expected error when DB is down on Start")
	}
}
