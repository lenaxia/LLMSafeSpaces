// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package app

import (
	"context"

	"github.com/lenaxia/llmsafespaces/api/internal/handlers"
	pkginterfaces "github.com/lenaxia/llmsafespaces/pkg/interfaces"
	"github.com/lenaxia/llmsafespaces/pkg/settings"
)

// devPreviewConfigFromSettings reads the three Epic 66 instance settings and
// builds the DevPreviewHandler config. Read errors (e.g. a future key
// registered in KnownKeys but missing from the schema — the #946 failure
// mode) log a warning and fall back to the typed-key defaults rather than
// silently disabling the feature with the Get* zero value. The Default()
// type assertions are pinned by TestInstanceSettings_DevPreviewKeys.
func devPreviewConfigFromSettings(ctx context.Context, instanceSettings *settings.InstanceService, log pkginterfaces.LoggerInterface) handlers.DevPreviewConfig {
	cfg := handlers.DevPreviewConfig{}

	enabled, err := instanceSettings.GetBool(ctx, settings.KeyDevPreviewEnabled.Name())
	if err != nil {
		log.Warn("dev-preview kill-switch read failed; using schema default", "key", settings.KeyDevPreviewEnabled.Name(), "error", err)
		enabled = settings.KeyDevPreviewEnabled.Default().(bool)
	}

	maxBytes, err := instanceSettings.GetInt(ctx, settings.KeyDevPreviewMaxResponseBytes.Name())
	if err != nil {
		log.Warn("dev-preview max-response-bytes read failed; using schema default", "key", settings.KeyDevPreviewMaxResponseBytes.Name(), "error", err)
		maxBytes = settings.KeyDevPreviewMaxResponseBytes.Default().(int)
	}

	maxConns, err := instanceSettings.GetInt(ctx, settings.KeyDevPreviewMaxConnsPerWorkspace.Name())
	if err != nil {
		log.Warn("dev-preview max-conns read failed; using schema default", "key", settings.KeyDevPreviewMaxConnsPerWorkspace.Name(), "error", err)
		maxConns = settings.KeyDevPreviewMaxConnsPerWorkspace.Default().(int)
	}

	// Clamp non-positive values written directly to the DB past the schema
	// validator (Set rejects out-of-range, but the DB is writable out-of-band).
	if maxBytes <= 0 {
		maxBytes = settings.KeyDevPreviewMaxResponseBytes.Default().(int)
	}
	if maxConns <= 0 {
		maxConns = settings.KeyDevPreviewMaxConnsPerWorkspace.Default().(int)
	}

	cfg.Enabled = enabled
	cfg.MaxResponseBytes = int64(maxBytes)
	cfg.MaxConnsPerWorkspace = maxConns
	return cfg
}
