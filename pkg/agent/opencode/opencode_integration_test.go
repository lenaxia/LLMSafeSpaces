//go:build integration
// +build integration

package opencode

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const opencodeVersion = "1.18.10"

func findOrDownloadBinary(t *testing.T) string {
	t.Helper()

	if p := os.Getenv("OPENCODE_BINARY"); p != "" {
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}

	cacheDir := filepath.Join(os.TempDir(), "opencode-test-binaries")
	binPath := filepath.Join(cacheDir, fmt.Sprintf("opencode-%s", opencodeVersion))

	if _, err := os.Stat(binPath); err == nil {
		return binPath
	}

	t.Logf("Downloading opencode v%s ...", opencodeVersion)
	require.NoError(t, os.MkdirAll(cacheDir, 0o755))

	arch := "x64"
	url := fmt.Sprintf(
		"https://github.com/anomalyco/opencode/releases/download/v%s/opencode-linux-%s.tar.gz",
		opencodeVersion, arch,
	)

	tmp := filepath.Join(cacheDir, "opencode.tar.gz")
	out, err := os.Create(tmp)
	require.NoError(t, err)

	resp, err := http.Get(url)
	if err != nil {
		out.Close()
		require.NoError(t, err)
	}
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode, "download failed: %s", url)

	_, err = io.Copy(out, resp.Body)
	out.Close()
	require.NoError(t, err)

	require.NoError(t, exec.Command("tar", "-xzf", tmp, "-C", cacheDir, "opencode").Run())

	extracted := filepath.Join(cacheDir, "opencode")
	require.NoError(t, os.Rename(extracted, binPath))
	require.NoError(t, os.Chmod(binPath, 0o755))
	require.NoError(t, os.Remove(tmp))

	return binPath
}

func writeOpencodeConfig(t *testing.T, configDir string) {
	t.Helper()

	configContent := `{
		"$schema": "https://opencode.ai/config.json",
		"provider": {
			"opencode": {
				"options": {
					"apiKey": "public",
					"baseURL": "https://opencode.ai/zen/v1"
				}
			}
		}
	}`

	// Write to XDG config dir (opencode discovers this automatically)
	xdgConfigDir := filepath.Join(configDir, "opencode")
	require.NoError(t, os.MkdirAll(xdgConfigDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(xdgConfigDir, "opencode.jsonc"), []byte(configContent), 0o644))

	// Also write to OPENCODE_CONFIG path (last writer wins in opencode)
	require.NoError(t, os.WriteFile(filepath.Join(configDir, "agent-config.json"), []byte(configContent), 0o644))
}

type opencodeServer struct {
	baseURL string
	ctx     context.Context
	cancel  context.CancelFunc
	t       *testing.T
}

func startOpencodeServer(t *testing.T, port int) *opencodeServer {
	t.Helper()

	binary := findOrDownloadBinary(t)

	configDir := t.TempDir()
	dataDir := filepath.Join(configDir, "data")
	require.NoError(t, os.MkdirAll(dataDir, 0o755))

	writeOpencodeConfig(t, configDir)

	ctx, cancel := context.WithCancel(context.Background())

	agentConfigPath := filepath.Join(configDir, "agent-config.json")
	cmd := exec.CommandContext(ctx, binary, "serve",
		"--hostname", "127.0.0.1",
		"--port", fmt.Sprintf("%d", port),
	)
	cmd.Env = append(os.Environ(),
		"OPENCODE_CONFIG="+agentConfigPath,
		"XDG_DATA_HOME="+dataDir,
		"XDG_CONFIG_HOME="+configDir,
		"HOME="+configDir,
		"OPENCODE_SERVER_PASSWORD=test-password",
	)
	cmd.Dir = configDir

	stderr, err := cmd.StderrPipe()
	require.NoError(t, err)

	require.NoError(t, cmd.Start())

	go func() {
		scanner := bufio.NewScanner(stderr)
		for scanner.Scan() {
			t.Logf("[opencode] %s", scanner.Text())
		}
	}()

	t.Cleanup(func() {
		if cmd.Process != nil {
			cmd.Process.Signal(os.Interrupt)
			done := make(chan error, 1)
			go func() { done <- cmd.Wait() }()
			select {
			case <-done:
			case <-time.After(5 * time.Second):
				cmd.Process.Kill()
			}
		}
	})

	srv := &opencodeServer{
		baseURL: fmt.Sprintf("http://127.0.0.1:%d", port),
		ctx:     ctx,
		cancel:  cancel,
		t:       t,
	}

	srv.waitForHealthy(30 * time.Second)
	return srv
}

func (s *opencodeServer) waitForHealthy(timeout time.Duration) {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		req, err := http.NewRequest(http.MethodGet, s.baseURL+"/global/health", nil)
		if err != nil {
			time.Sleep(200 * time.Millisecond)
			continue
		}
		req.SetBasicAuth("opencode", "test-password")
		resp, err := http.DefaultClient.Do(req)
		if err == nil {
			var body map[string]interface{}
			json.NewDecoder(resp.Body).Decode(&body)
			resp.Body.Close()
			if healthy, _ := body["healthy"].(bool); healthy {
				return
			}
		}
		time.Sleep(200 * time.Millisecond)
	}
	s.t.Fatal("opencode did not become healthy within " + timeout.String())
}

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
