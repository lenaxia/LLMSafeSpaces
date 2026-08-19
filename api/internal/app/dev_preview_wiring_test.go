// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package app

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/lenaxia/llmsafespaces/pkg/settings"
)

// fakeInstanceStore is a minimal settings.InstanceStore for wiring tests —
// the pkg/settings mock is unexported and cannot be reused across packages.
type fakeInstanceStore struct {
	mu     sync.Mutex
	data   map[string]json.RawMessage
	getErr error
}

func (f *fakeInstanceStore) GetAllInstanceSettings(_ context.Context) (map[string]json.RawMessage, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.getErr != nil {
		return nil, f.getErr
	}
	cp := make(map[string]json.RawMessage, len(f.data))
	for k, v := range f.data {
		cp[k] = v
	}
	return cp, nil
}

func (f *fakeInstanceStore) SetInstanceSetting(_ context.Context, key string, value json.RawMessage) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.data == nil {
		f.data = make(map[string]json.RawMessage)
	}
	f.data[key] = value
	return nil
}

func newDevPreviewTestService(store *fakeInstanceStore) *settings.InstanceService {
	return settings.NewInstanceService(store, nopLogger{})
}

// TestDevPreviewConfigFromSettings_EmptyStore_ServesSchemaDefaults: on an
// empty store the handler config must carry the schema defaults — this is
// the exact #946 failure (index-miss error + swallowed error + zero value
// hard-disabled the feature on every stock deployment).
func TestDevPreviewConfigFromSettings_EmptyStore_ServesSchemaDefaults(t *testing.T) {
	svc := newDevPreviewTestService(&fakeInstanceStore{})

	cfg := devPreviewConfigFromSettings(context.Background(), svc, nopLogger{})

	assert.True(t, cfg.Enabled, "empty store must boot with the schema default enabled=true")
	assert.EqualValues(t, 52428800, cfg.MaxResponseBytes)
	assert.Equal(t, 50, cfg.MaxConnsPerWorkspace)
}

// TestDevPreviewConfigFromSettings_StoreError_WarnsAndFallsBackToDefaults:
// a store failure at boot must produce the typed-key defaults, not the Get*
// zero values (which would silently disable the feature — the regression
// the #946 fix guards against).
func TestDevPreviewConfigFromSettings_StoreError_WarnsAndFallsBackToDefaults(t *testing.T) {
	svc := newDevPreviewTestService(&fakeInstanceStore{getErr: errors.New("db down")})

	cfg := devPreviewConfigFromSettings(context.Background(), svc, nopLogger{})

	assert.True(t, cfg.Enabled, "store error must fall back to default true, not the zero value false")
	assert.EqualValues(t, 52428800, cfg.MaxResponseBytes)
	assert.Equal(t, 50, cfg.MaxConnsPerWorkspace)
}

// TestDevPreviewConfigFromSettings_StoreValues_AreHonoured: values written
// through the service (the admin PUT path persists them there) must reach
// the handler config unchanged.
func TestDevPreviewConfigFromSettings_StoreValues_AreHonoured(t *testing.T) {
	store := &fakeInstanceStore{}
	svc := newDevPreviewTestService(store)
	ctx := context.Background()
	assert.NoError(t, svc.Set(ctx, "devPreview.enabled", false))
	assert.NoError(t, svc.Set(ctx, "devPreview.maxResponseBytes", 1048576))
	assert.NoError(t, svc.Set(ctx, "devPreview.maxConnsPerWorkspace", 7))

	cfg := devPreviewConfigFromSettings(ctx, svc, nopLogger{})

	assert.False(t, cfg.Enabled)
	assert.EqualValues(t, 1048576, cfg.MaxResponseBytes)
	assert.Equal(t, 7, cfg.MaxConnsPerWorkspace)
}

// TestDevPreviewConfigFromSettings_OutOfBandGarbage_ClampedToDefaults:
// Set() rejects out-of-range values, but the DB is writable out-of-band —
// non-positive values must clamp to the defaults rather than disabling the
// caps or panicking downstream.
func TestDevPreviewConfigFromSettings_OutOfBandGarbage_ClampedToDefaults(t *testing.T) {
	store := &fakeInstanceStore{}
	// Bypass the validator the way a direct SQL write would.
	rawBytes, _ := json.Marshal(-1)
	rawConns, _ := json.Marshal(0)
	store.data = map[string]json.RawMessage{
		"devPreview.maxResponseBytes":     rawBytes,
		"devPreview.maxConnsPerWorkspace": rawConns,
	}
	svc := newDevPreviewTestService(store)

	cfg := devPreviewConfigFromSettings(context.Background(), svc, nopLogger{})

	assert.True(t, cfg.Enabled, "untouched key keeps its default")
	assert.EqualValues(t, 52428800, cfg.MaxResponseBytes, "negative DB value must clamp to default")
	assert.Equal(t, 50, cfg.MaxConnsPerWorkspace, "zero DB value must clamp to default")
}
