// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package app

import (
	"github.com/lenaxia/llmsafespaces/api/internal/services/agentpush"
	"github.com/lenaxia/llmsafespaces/api/internal/services/workspace"
)

// The workspace service's dev-preview pusher seam is satisfied by
// agentpush.Service (compile-time pin — the app wiring passes the
// concrete service; #1617).
var _ workspace.DevPreviewStatePusher = (*agentpush.Service)(nil)
