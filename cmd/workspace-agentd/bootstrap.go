// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/lenaxia/llmsafespaces/pkg/agentd"
)

// bootstrapResponse is the JSON envelope returned by POST /internal/v1/pod-bootstrap.
// Secrets is the decrypted credential array (same shape as secrets.json);
// WorkspaceConfig carries the default-model selection read from PostgreSQL
// (F1 — previously delivered via the K8s Secret's workspace-config.json key);
// AdminPrompt is the merged platform→org→role→user system prompt;
// AllowedExternalDirectories is the instance setting that the AgentConfigWriter
// merges into mode.permissions.external_directory as "allow" rules (stops
// agents prompting for /tmp/* on every session).
type bootstrapResponse struct {
	Secrets                    json.RawMessage `json:"secrets"`
	WorkspaceConfig            json.RawMessage `json:"workspaceConfig,omitempty"`
	AdminPrompt                string          `json:"adminPrompt,omitempty"`
	AllowedExternalDirectories []string        `json:"allowedExternalDirectories,omitempty"`
}

// bootstrapRequest is the JSON body POSTed to the API.
type bootstrapRequest struct {
	WorkspaceID string `json:"workspaceID"`
}

// runBootstrapCommand implements the `workspace-agentd bootstrap` subcommand.
// It fetches decrypted secrets from the API using a projected ServiceAccount
// token and writes them to /sandbox-cfg/secrets.json (+ workspace-config.json).
//
// The subcommand NEVER blocks pod boot: on any failure (network error, API
// down, malformed response, missing token) it writes an empty secrets array
// and exits 0. The existing POST /v1/reload-secrets live-push path handles
// credential delivery on the user's first activation — identical to the
// behavior when a workspace has no credential bindings.
//
// Exit codes: 0 always (even on failure — graceful degradation), except 2
// for flag-parse errors or missing required --workspace-id.
func runBootstrapCommand(args []string, _ io.Writer, stderr io.Writer) int {
	fs := flag.NewFlagSet("bootstrap", flag.ContinueOnError)
	fs.SetOutput(stderr)

	workspaceID := fs.String("workspace-id", "", "workspace ID to fetch secrets for (required)")
	apiURL := fs.String("api-url", os.Getenv("LLMSAFESPACE_API_URL"), "API service base URL")
	tokenFile := fs.String("token-file", "/var/run/bootstrap/token", "projected SA token file")
	out := fs.String("out", "/sandbox-cfg/secrets.json", "output secrets.json path")
	// adminPromptOut is the file the bootstrap subcommand writes the merged
	// platform→org→role→user system prompt to, if the API returns a non-empty
	// AdminPrompt. Defaults to agentd.AdminPromptPath (/sandbox-runtime/admin-prompt.md).
	// Symmetric with --out; exposed as a flag so tests can write to a
	// t.TempDir() rather than the production tmpfs path.
	adminPromptOut := fs.String("admin-prompt-out", agentd.AdminPromptPath, "output admin-prompt.md path")
	// allowedDirsOut is the file the bootstrap subcommand writes the instance's
	// allowedExternalDirectories setting to (as a JSON array of glob patterns),
	// if the API returns a non-empty list. The AgentConfigWriter reads it via
	// loadAllowedDirs and merges each pattern into mode.permissions.external_directory
	// as an "allow" rule. Defaults to agentd.AllowedDirsPath. Symmetric with
	// --admin-prompt-out; exposed as a flag so tests can target a t.TempDir().
	allowedDirsOut := fs.String("allowed-dirs-out", agentd.AllowedDirsPath, "output allowed-dirs.json path")

	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *workspaceID == "" {
		_, _ = fmt.Fprintln(stderr, "bootstrap: --workspace-id is required")
		return 2
	}

	if err := os.MkdirAll(filepath.Dir(*out), 0o750); err != nil {
		_, _ = fmt.Fprintf(stderr, "bootstrap: failed to create output dir: %v\n", err)
		writeEmptySecrets(*out)
		return 0
	}

	token, err := os.ReadFile(*tokenFile)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "bootstrap: token file unreadable (%s): %v\n", *tokenFile, err)
		writeEmptySecrets(*out)
		return 0
	}

	secrets, wsCfg, adminPrompt, allowedDirs, err := fetchBootstrapSecrets(*apiURL, *workspaceID, string(token))
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "bootstrap: fetch failed: %v\n", err)
		writeEmptySecrets(*out)
		return 0
	}

	if err := atomicWriteSecrets(*out, secrets); err != nil {
		_, _ = fmt.Fprintf(stderr, "bootstrap: failed to write secrets.json: %v\n", err)
		return 0
	}

	if len(wsCfg) > 0 && string(wsCfg) != "null" {
		cfgPath := filepath.Join(filepath.Dir(*out), "workspace-config.json")
		if err := atomicWriteSecrets(cfgPath, wsCfg); err != nil {
			_, _ = fmt.Fprintf(stderr, "bootstrap: failed to write workspace-config.json: %v\n", err)
		}
	}

	if adminPrompt != "" {
		if err := atomicWriteSecrets(*adminPromptOut, []byte(adminPrompt)); err != nil {
			_, _ = fmt.Fprintf(stderr, "bootstrap: failed to write admin-prompt.md: %v\n", err)
		} else {
			_, _ = fmt.Fprintf(stderr, "bootstrap: wrote admin prompt (%d bytes)\n", len(adminPrompt))
		}
	}

	if len(allowedDirs) > 0 {
		dirsJSON, mErr := json.Marshal(allowedDirs)
		if mErr != nil {
			_, _ = fmt.Fprintf(stderr, "bootstrap: failed to marshal allowed-dirs: %v\n", mErr)
		} else if err := atomicWriteSecrets(*allowedDirsOut, dirsJSON); err != nil {
			_, _ = fmt.Fprintf(stderr, "bootstrap: failed to write allowed-dirs.json: %v\n", err)
		} else {
			_, _ = fmt.Fprintf(stderr, "bootstrap: wrote allowed-dirs (%d patterns)\n", len(allowedDirs))
		}
	}

	_, _ = fmt.Fprintf(stderr, "bootstrap: success, %d bytes secrets\n", len(secrets))
	return 0
}

func fetchBootstrapSecrets(apiURL, workspaceID, token string) (json.RawMessage, json.RawMessage, string, []string, error) {
	body, err := json.Marshal(bootstrapRequest{WorkspaceID: workspaceID})
	if err != nil {
		return nil, nil, "", nil, err
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	//nolint:gosec // G704: apiURL is controller-set via LLMSAFESPACE_API_URL env var, not user-controllable
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, apiURL+"/internal/v1/pod-bootstrap", bytes.NewReader(body))
	if err != nil {
		return nil, nil, "", nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")

	client := &http.Client{}
	//nolint:gosec // G704: apiURL is controller-set, not user-controllable
	resp, err := client.Do(req)
	if err != nil {
		return nil, nil, "", nil, err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return nil, nil, "", nil, fmt.Errorf("API returned %d", resp.StatusCode)
	}

	var result bootstrapResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, nil, "", nil, fmt.Errorf("decode response: %w", err)
	}

	if len(result.Secrets) == 0 {
		result.Secrets = json.RawMessage("[]")
	}
	return result.Secrets, result.WorkspaceConfig, result.AdminPrompt, result.AllowedExternalDirectories, nil
}

func writeEmptySecrets(path string) {
	_ = atomicWriteSecrets(path, []byte("[]"))
}

func atomicWriteSecrets(path string, data []byte) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".bootstrap-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()

	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}
