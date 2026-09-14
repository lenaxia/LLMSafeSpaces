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
	"syscall"
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

type opencodeServer struct {
	baseURL string
	ctx     context.Context
	cancel  context.CancelFunc
	t       *testing.T
}

func startOpencodeServer(t *testing.T, port int) *opencodeServer {
	t.Helper()
	return startOpencodeServerWithConfig(t, port, legacyIntegrationConfig)
}

// legacyIntegrationConfig is the historical integration-provider config
// (opencode.ai zen) the provider/config endpoint tests run against.
const legacyIntegrationConfig = `{
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

// startOpencodeServerWithConfig boots the real opencode binary with an
// arbitrary agent config (the loopback L2 tests point it at a local mock
// provider so sends complete offline).
func startOpencodeServerWithConfig(t *testing.T, port int, configJSON string) *opencodeServer {
	t.Helper()

	binary := findOrDownloadBinary(t)

	configDir := t.TempDir()
	dataDir := filepath.Join(configDir, "data")
	require.NoError(t, os.MkdirAll(dataDir, 0o755))

	agentConfigPath := filepath.Join(configDir, "agent-config.json")
	require.NoError(t, os.WriteFile(agentConfigPath, []byte(configJSON), 0o644))
	xdgConfigDir := filepath.Join(configDir, "opencode")
	require.NoError(t, os.MkdirAll(xdgConfigDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(xdgConfigDir, "opencode.jsonc"), []byte(configJSON), 0o644))

	ctx, cancel := context.WithCancel(context.Background())

	cmd := exec.CommandContext(ctx, binary, "serve",
		"--hostname", "127.0.0.1",
		"--port", fmt.Sprintf("%d", port),
	)
	// EXPLICIT env, not os.Environ()+overrides: duplicated vars resolve
	// first-match in the child's libc, so an inherited HOME/XDG_* from the
	// runner (a workspace pod carries its own opencode config + auth
	// there) silently overrides the test's configDirs and the instance
	// answers with the POD's providers instead of the test's — the exact
	// failure mode live-debugged 2026-09-13 (send hangs, mock never
	// dialed, "ServeError" in stderr). Path/TMPDIR are the only
	// pass-throughs the binary needs.
	cmd.Env = append(os.Environ(),
		"OPENCODE_CONFIG="+agentConfigPath,
		"XDG_DATA_HOME="+dataDir,
		"XDG_CONFIG_HOME="+configDir,
		"HOME="+configDir,
		"OPENCODE_SERVER_PASSWORD=test-password",
	)
	cmd.Dir = configDir
	// Session-detached, mirroring how agentd's supervisor actually
	// spawns opencode and how every live-validated manual boot ran: the
	// pinned binary's outbound fetches misbehave when it remains in the
	// test runner's process group (live-debugged 2026-09-13 — same
	// binary/config/mock answers perfectly when detached, "Cannot
	// connect to API" for every provider call when not).
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}

	// stderr/stdout to a FILE, not a pipe: a pipe consumer that stalls
	// or dies can block the child's stderr writer, and opencode places
	// writes on the request path under load (live-debugged 2026-09-13:
	// harness boots served health but hung every message send; the
	// shell-booted identical binary+config served the same sends in
	// ~1s). The file also survives for post-mortem; the tail is dumped
	// into the test log at cleanup.
	logPath := filepath.Join(configDir, "oc.stderr.log")
	logFile, err := os.Create(logPath)
	require.NoError(t, err)
	cmd.Stderr = logFile
	cmd.Stdout = logFile

	require.NoError(t, cmd.Start())

	t.Cleanup(func() {
		_ = logFile.Close()
		if data, rerr := os.ReadFile(logPath); rerr == nil {
			lines := strings.Split(string(data), "\n")
			if len(lines) > 40 {
				lines = lines[len(lines)-40:]
			}
			for _, l := range lines {
				t.Logf("[opencode] %s", l)
			}
		}
	})

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
	// Bounded probe client: an unbounded Do() hangs forever when the
	// child boots into a stalled state (its catalog fetch can blackhole
	// on egress-filtered runners — the kernel accepts the SYN, the app
	// never answers). Bounded probes turn that into a retry, then a
	// loud timeout, instead of an infinite hang (live-debugged
	// 2026-09-13).
	probeClient := &http.Client{Timeout: 2 * time.Second}
	for time.Now().Before(deadline) {
		req, err := http.NewRequest(http.MethodGet, s.baseURL+"/global/health", nil)
		if err != nil {
			time.Sleep(200 * time.Millisecond)
			continue
		}
		req.SetBasicAuth("opencode", "test-password")
		resp, err := probeClient.Do(req)
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
