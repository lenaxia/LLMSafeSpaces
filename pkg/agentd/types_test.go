// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package agentd

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestHealthzResponse_JSON(t *testing.T) {
	resp := HealthzResponse{Healthy: true, Version: "1.2.27", UptimeSeconds: 3600}
	data, err := json.Marshal(resp)
	assert.NoError(t, err)
	assert.Contains(t, string(data), `"healthy":true`)
	assert.Contains(t, string(data), `"version":"1.2.27"`)
	assert.Contains(t, string(data), `"uptime_seconds":3600`)
}

func TestReadyzResponse_JSON(t *testing.T) {
	resp := ReadyzResponse{
		Ready:               true,
		ProvidersConnected:  []string{"opencode"},
		ProvidersConfigured: 1,
		AgentVersion:        "1.2.27",
		AgentType:           "opencode",
	}
	data, err := json.Marshal(resp)
	assert.NoError(t, err)

	var decoded ReadyzResponse
	assert.NoError(t, json.Unmarshal(data, &decoded))
	assert.True(t, decoded.Ready)
	assert.Equal(t, []string{"opencode"}, decoded.ProvidersConnected)
	assert.Equal(t, 1, decoded.ProvidersConfigured)
}

func TestStatuszResponse_JSON(t *testing.T) {
	resp := StatuszResponse{
		Healthy:             true,
		Ready:               true,
		Connected:           []string{"opencode"},
		ProvidersConfigured: 1,
		Sessions: []SessionInfo{
			{ID: "ses_1", Title: "Debug auth", Status: "idle"},
			{ID: "ses_2", Title: "", Status: "busy"},
		},
		SessionsActive: 2,
		SessionsError:  0,
		LastError:      "",
		AgentType:      "opencode",
		AgentVersion:   "1.2.27",
		UptimeSeconds:  7200,
		Disk:           &DiskUsage{UsedBytes: 1024 * 1024 * 50, TotalBytes: 1024 * 1024 * 1024},
		Memory:         &MemoryUsage{UsedBytes: 256 * 1024 * 1024, TotalBytes: 1024 * 1024 * 1024},
		Context:        &ContextUsage{UsedTokens: 25000, TotalTokens: 128000},
	}
	data, err := json.Marshal(resp)
	assert.NoError(t, err)

	var decoded StatuszResponse
	assert.NoError(t, json.Unmarshal(data, &decoded))
	assert.Equal(t, 2, decoded.SessionsActive)
	assert.Equal(t, 7200, decoded.UptimeSeconds)
	assert.Len(t, decoded.Sessions, 2)
	assert.Equal(t, "ses_1", decoded.Sessions[0].ID)
	assert.Equal(t, "Debug auth", decoded.Sessions[0].Title)
	assert.Equal(t, "idle", decoded.Sessions[0].Status)
	assert.Equal(t, "busy", decoded.Sessions[1].Status)
	assert.NotNil(t, decoded.Disk)
	assert.Equal(t, int64(1024*1024*50), decoded.Disk.UsedBytes)
	assert.Equal(t, int64(1024*1024*1024), decoded.Disk.TotalBytes)
	assert.NotNil(t, decoded.Memory)
	assert.Equal(t, int64(256*1024*1024), decoded.Memory.UsedBytes)
	assert.Equal(t, int64(1024*1024*1024), decoded.Memory.TotalBytes)
	assert.NotNil(t, decoded.Context)
	assert.Equal(t, int64(25000), decoded.Context.UsedTokens)
	assert.Equal(t, int64(128000), decoded.Context.TotalTokens)
}

func TestStatuszResponse_NilDisk(t *testing.T) {
	resp := StatuszResponse{Healthy: true, Sessions: []SessionInfo{}}
	data, err := json.Marshal(resp)
	assert.NoError(t, err)
	assert.NotContains(t, string(data), `"disk"`)
}

func TestStatuszResponse_EmptySessions(t *testing.T) {
	resp := StatuszResponse{}
	data, err := json.Marshal(resp)
	assert.NoError(t, err)
	var decoded StatuszResponse
	assert.NoError(t, json.Unmarshal(data, &decoded))
	assert.Nil(t, decoded.Sessions)
	assert.Nil(t, decoded.Disk)
}

func TestReadyzResponse_NotReady(t *testing.T) {
	resp := ReadyzResponse{Ready: false, ProvidersConnected: []string{}}
	data, err := json.Marshal(resp)
	assert.NoError(t, err)
	assert.Contains(t, string(data), `"ready":false`)
}

func TestSessionInfo_JSON(t *testing.T) {
	s := SessionInfo{ID: "ses_abc", Title: "My Chat", Status: "idle"}
	data, err := json.Marshal(s)
	assert.NoError(t, err)
	assert.Contains(t, string(data), `"id":"ses_abc"`)
	assert.Contains(t, string(data), `"title":"My Chat"`)
	assert.Contains(t, string(data), `"status":"idle"`)
}

func TestSessionInfo_OmitsEmptyTitle(t *testing.T) {
	s := SessionInfo{ID: "ses_1", Status: "busy"}
	data, err := json.Marshal(s)
	assert.NoError(t, err)
	assert.NotContains(t, string(data), `"title"`)
}

func TestDiskUsage_JSON(t *testing.T) {
	d := DiskUsage{UsedBytes: 500, TotalBytes: 1000}
	data, err := json.Marshal(d)
	assert.NoError(t, err)
	var decoded DiskUsage
	assert.NoError(t, json.Unmarshal(data, &decoded))
	assert.Equal(t, int64(500), decoded.UsedBytes)
	assert.Equal(t, int64(1000), decoded.TotalBytes)
}

func TestFileUploadResponse_JSON(t *testing.T) {
	resp := FileUploadResponse{
		Path: "/workspace/uploads/00000000-0000-0000-0000-000000000001-notes.txt",
		Name: "notes.txt",
		Size: 1234,
	}
	data, err := json.Marshal(resp)
	assert.NoError(t, err)
	assert.JSONEq(t, `{"path":"/workspace/uploads/00000000-0000-0000-0000-000000000001-notes.txt","name":"notes.txt","size":1234}`, string(data))

	var decoded FileUploadResponse
	assert.NoError(t, json.Unmarshal(data, &decoded))
	assert.Equal(t, resp, decoded)
}
