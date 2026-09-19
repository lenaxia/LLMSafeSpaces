//go:build integration

package opencode

import (
	"bufio"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type providerResponse struct {
	Connected []string `json:"connected"`
	All       []struct {
		ID     string `json:"id"`
		Models map[string]struct {
			ID    string `json:"id"`
			Name  string `json:"name"`
			Limit *struct {
				Context int `json:"context"`
				Output  int `json:"output"`
			} `json:"limit"`
		} `json:"models"`
	} `json:"all"`
}

type configProvidersResponse struct {
	Providers []struct {
		ID     string `json:"id"`
		Models map[string]struct {
			ID    string `json:"id"`
			Limit struct {
				Context int64 `json:"context"`
			} `json:"limit"`
		} `json:"models"`
	} `json:"providers"`
}

func doGet(url string) (*http.Response, error) {
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.SetBasicAuth("opencode", "test-password")
	return http.DefaultClient.Do(req)
}

func TestOpencode_ProviderEndpoint_IncludesLimitContext(t *testing.T) {
	srv := startOpencodeServer(t, 14098)

	resp, err := doGet(srv.baseURL + "/provider")
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)

	var providerResp providerResponse
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal(body, &providerResp))

	assert.NotEmpty(t, providerResp.Connected, "at least one provider must be connected")

	foundLimitContext := false
	for _, p := range providerResp.All {
		for modelID, m := range p.Models {
			if m.Limit != nil && m.Limit.Context > 0 {
				t.Logf("model %s/%s: limit.context=%d", p.ID, modelID, m.Limit.Context)
				foundLimitContext = true
			}
		}
	}
	assert.True(t, foundLimitContext,
		"at least one model must have limit.context > 0 in /provider response.\n"+
			"This is the data source for contextTotal. If absent, the context bar will always show Unknown.\n"+
			"Raw: %s", string(body))
}

func TestOpencode_ConfigProvidersEndpoint_IncludesLimitContext(t *testing.T) {
	srv := startOpencodeServer(t, 14099)

	resp, err := doGet(srv.baseURL + "/config/providers")
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)

	var configResp configProvidersResponse
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal(body, &configResp))

	assert.NotEmpty(t, configResp.Providers, "providers list must not be empty")

	foundLimitContext := false
	for _, p := range configResp.Providers {
		for modelID, m := range p.Models {
			if m.Limit.Context > 0 {
				t.Logf("config/providers: %s/%s: limit.context=%d", p.ID, modelID, m.Limit.Context)
				foundLimitContext = true
			}
		}
	}
	assert.True(t, foundLimitContext,
		"at least one model must have limit.context > 0 in /config/providers.\n"+
			"agentd ModelContextLimit() reads this. If absent, contextTotal will always be 0.\n"+
			"Raw: %s", string(body))
}

func TestOpencode_SSEEventEnvelope_HasTypeField(t *testing.T) {
	srv := startOpencodeServer(t, 14100)

	req, err := http.NewRequest(http.MethodGet, srv.baseURL+"/event", nil)
	require.NoError(t, err)
	req.SetBasicAuth("opencode", "test-password")

	transport := &http.Transport{DisableKeepAlives: true}
	client := &http.Client{Transport: transport, Timeout: 15 * time.Second}

	resp, err := client.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.Equal(t, "text/event-stream", strings.ToLower(resp.Header.Get("Content-Type")),
		"SSE endpoint must return text/event-stream content type")

	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 64*1024), 64*1024)

	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue
		}

		data := strings.TrimPrefix(line, "data: ")
		var evt struct {
			ID         string          `json:"id"`
			Type       string          `json:"type"`
			Properties json.RawMessage `json:"properties"`
		}
		if json.Unmarshal([]byte(data), &evt) != nil {
			continue
		}

		if evt.Type == "" {
			continue
		}

		t.Logf("SSE event: type=%s id=%s", evt.Type, evt.ID)

		assert.NotEmpty(t, evt.Type, "event type must not be empty")
		assert.NotNil(t, evt.Properties, "event must have properties field")

		if evt.Type == "server.heartbeat" {
			assert.NotEmpty(t, evt.ID, "heartbeat events must have id field (present since opencode 1.15)")
		}

		resp.Body.Close()
		return
	}

	t.Fatal("must receive at least one SSE event from opencode")
}
