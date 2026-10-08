// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package opencode

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestSeam_SessionArchiveWire pins the #1627 pod-identity session
// surface wire shapes: method, path, body, SA bearer.
func TestSeam_SessionArchiveWire(t *testing.T) {
	t.Run("archive", func(t *testing.T) {
		c, rec := newAutomationServer(t, 204, "")
		err := c.SessionArchive(ctx(), "sa-token", "ws-1", "ses-1", true)
		require.NoError(t, err)
		assert.Equal(t, "POST", rec.method)
		assert.Equal(t, "/internal/v1/session-archive", rec.path)
		assert.Contains(t, rec.body, `"workspaceID":"ws-1"`)
		assert.Contains(t, rec.body, `"sessionID":"ses-1"`)
		assert.Contains(t, rec.body, `"archived":true`)
	})

	t.Run("unarchive", func(t *testing.T) {
		c, rec := newAutomationServer(t, 204, "")
		err := c.SessionArchive(ctx(), "sa-token", "ws-1", "ses-1", false)
		require.NoError(t, err)
		assert.Contains(t, rec.body, `"archived":false`)
	})

	t.Run("unindexed session surfaces 404", func(t *testing.T) {
		c, _ := newAutomationServer(t, 404, `{"error":"not found: session"}`)
		err := c.SessionArchive(ctx(), "sa-token", "ws-1", "ses-ghost", true)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "404")
	})
}

func TestSeam_SessionDeleteWire(t *testing.T) {
	c, rec := newAutomationServer(t, 204, "")
	err := c.PlatformSessionDelete(ctx(), "sa-token", "ws-1", "ses-1")
	require.NoError(t, err)
	assert.Equal(t, "POST", rec.method)
	assert.Equal(t, "/internal/v1/session-delete", rec.path)
	assert.Contains(t, rec.body, `"workspaceID":"ws-1"`)
	assert.Contains(t, rec.body, `"sessionID":"ses-1"`)
}

func TestSeam_SessionArchivedSetWire(t *testing.T) {
	c, rec := newAutomationServer(t, 200, `{"archived":["ses-1","ses-3"]}`)
	set, err := c.SessionArchivedSet(ctx(), "sa-token", "ws-1")
	require.NoError(t, err)
	assert.Equal(t, "GET", rec.method)
	assert.Equal(t, "/internal/v1/session-archived", rec.path)
	assert.Equal(t, []string{"ses-1", "ses-3"}, set)
}
