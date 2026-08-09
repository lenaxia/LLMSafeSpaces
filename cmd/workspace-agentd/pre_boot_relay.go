// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

// pre_boot_relay.go implements Phase C of the 2026-06-23 cold-start
// optimization (item #1a). It pre-renders the relay-provider block in
// agent-config.json BEFORE opencode is started, eliminating the in-pod
// opencode-restart cycle that startRelayInjector imposes when called
// after opencode is already running.
//
// Pre-fix boot sequence (pre-2026-06-23):
//
//	1. opencode starts with provider config (no relay).
//	2. agentd's startRelayInjector waits for opencode to be healthy
//	   (up to 5 minutes, typically ~3-5s).
//	3. Injector calls /provider on opencode → fetches free model list.
//	4. Injector merges relay-provider block into agent-config.json.
//	5. Injector kills opencode → supervisor restarts it.
//	6. opencode boots a SECOND time with the merged config.
//
// We pay opencode boot twice. Step 6 alone is ~5-7s; the wait + fetch
// + write add another ~2s.
//
// Post-fix boot sequence (Phase A + B + C + D):
//
//	1. Controller publishes free models as a ConfigMap (Phase A).
//	2. Pod's credential-setup init container mounts the CM and
//	   copies models.json into /sandbox-cfg/free-models.json (Phase B).
//	3. agentd's materialize subcommand calls applyRelayConfigPreBoot
//	   (this file, Phase C). It reads the file + checks the bypass
//	   condition + writes the relay-provider block to agent-config.json
//	   atomically, all before opencode is started.
//	4. opencode starts with the FINAL config. Boots once.
//	5. The in-pod startRelayInjector goroutine still runs (Phase D
//	   teaches it to short-circuit if pre-boot injection succeeded).
//
// Failure semantics:
//
//   - Free-models file missing or empty: log, skip — opencode boots
//     with no relay block, the in-pod startRelayInjector will run
//     after opencode is up and inject the relay the legacy way.
//     Worst-case identical to pre-Phase-C behavior. NOT an error.
//
//   - INFERENCE_RELAY_BASEURL env not set: the controller did not
//     configure a relay. No-op. NOT an error.
//
//   - Personal opencode API key in auth.json: shouldSkipRelay returns
//     true → no relay injection (matches legacy behavior). The user
//     is paying for direct Zen access; do not interpose the relay.
//
//   - agent-config.json write fails: log, return error. The
//     materialize subcommand exits non-zero, kubelet sees CrashLoop —
//     this is a hard failure because we already fetched the models
//     and the CM is good; if we can't write the file, something else
//     is broken.

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"go.uber.org/zap"

	opencode "github.com/lenaxia/llmsafespaces/pkg/agent/opencode"
	"github.com/lenaxia/llmsafespaces/pkg/agentd"
)

// freeModelsFilePath is where the credential-setup init container
// drops the cluster-wide free-models catalog. The default is the
// production path; tests override via freeModelsTestPath.
const freeModelsFilePath = "/sandbox-cfg/free-models.json"

// freeModelsTestPath, when non-empty, overrides freeModelsFilePath.
// Production code never sets this; only test code does (via
// withFreeModelsAtTmp). Reading inside the package directly keeps
// the production hot path branch-free at the cost of a single
// unexported package var.
var freeModelsTestPath string

// adminPromptTestPath / allowedDirsTestPath, when non-empty, override
// the agentd.AdminPromptPath / agentd.AllowedDirsPath constants used
// when constructing the pre-boot relay ConfigWriter. Tests use these
// because /sandbox-runtime is not writable in CI. Production never sets
// them; agentd.AdminPromptPath / agentd.AllowedDirsPath stand.
var (
	adminPromptTestPath string
	allowedDirsTestPath string
)

// effectiveAdminPromptPath returns the admin-prompt source path,
// honoring the test override.
func effectiveAdminPromptPath() string {
	if adminPromptTestPath != "" {
		return adminPromptTestPath
	}
	return agentd.AdminPromptPath
}

// effectiveAllowedDirsPath returns the allowed-dirs source path,
// honoring the test override.
func effectiveAllowedDirsPath() string {
	if allowedDirsTestPath != "" {
		return allowedDirsTestPath
	}
	return agentd.AllowedDirsPath
}

// effectiveFreeModelsPath returns the path to read the catalog from,
// honoring the test override.
func effectiveFreeModelsPath() string {
	if freeModelsTestPath != "" {
		return freeModelsTestPath
	}
	return freeModelsFilePath
}

// preBootAuthJSONPath returns the path opencode reads auth.json from.
// Mirrors main.go's resolution: $XDG_DATA_HOME/opencode/auth.json if
// set, else $HOME/.local/opencode/auth.json. The credential-setup
// init container's symlink farm puts the actual file on tmpfs at
// /sandbox-runtime/rt/auth.json; we resolve through the symlink so
// shouldSkipRelay reads the same file opencode will read.
func preBootAuthJSONPath(home string) string {
	if xdg := os.Getenv("XDG_DATA_HOME"); xdg != "" {
		return filepath.Join(xdg, "opencode", "auth.json")
	}
	if home == "" {
		home = "/home/sandbox"
	}
	return filepath.Join(home, ".local", "opencode", "auth.json")
}

// catalogFromFile is the wire-format envelope written by the
// controller's freemodels package. Its fields must match
// controller/internal/freemodels/fetcher.go Catalog and Model exactly.
// A struct copy (not a shared types package) is intentional — agentd
// must not depend on the controller package, and the wire format is
// a small, stable contract documented in both places.
type catalogFromFile struct {
	Models []relayModelFromFile `json:"models"`
	// FetchedAt and Source are deliberately ignored — they're
	// diagnostic only.
}

type relayModelFromFile struct {
	ID           string `json:"id"`
	Name         string `json:"name"`
	ContextLimit int    `json:"context_limit"`
	OutputLimit  int    `json:"output_limit"`
}

// readFreeModelsFile loads the free-models catalog dropped by the
// credential-setup init container. Returns:
//
//   - (models, true, nil) on success.
//   - (nil, false, nil) when the file is absent. This is the
//     normal case for clusters that don't have the refresher
//     enabled, or pods created before the first controller fetch.
//     Caller should fall through to legacy in-pod injection.
//   - (nil, false, err) on JSON decode failure. The init container's
//     copy is a straight cp, so a parse error means the controller
//     wrote bad bytes — that's a real bug worth surfacing.
func readFreeModelsFile(path string) ([]opencode.RelayModel, bool, error) {
	f, err := os.Open(path) //nolint:gosec // G304: path is a constant
	if err != nil {
		if os.IsNotExist(err) {
			return nil, false, nil
		}
		return nil, false, fmt.Errorf("open %s: %w", path, err)
	}
	defer f.Close() //nolint:errcheck

	// 1 MiB ceiling — the catalog is JSON-encoded model metadata, a
	// few KB at most. Anything larger is a sign of corruption.
	var catalog catalogFromFile
	if err := json.NewDecoder(io.LimitReader(f, 1*1024*1024)).Decode(&catalog); err != nil {
		return nil, false, fmt.Errorf("decode %s: %w", path, err)
	}

	out := make([]opencode.RelayModel, 0, len(catalog.Models))
	for _, m := range catalog.Models {
		out = append(out, opencode.RelayModel(m))
	}
	return out, true, nil
}

// applyRelayConfigPreBoot is called by the materialize subcommand
// after FlushProviders + applyWorkspaceConfig have written the
// "vanilla" agent-config.json. It conditionally merges the relay
// block in BEFORE opencode is started, eliminating the in-pod
// opencode-restart cycle.
//
// Outcomes (logged via the supplied logger and returned as an
// outcome string for metrics):
//
//   - "skipped_no_relay_url": INFERENCE_RELAY_BASEURL env is unset.
//     The controller did not configure a relay. No-op.
//   - "skipped_no_catalog": /sandbox-cfg/free-models.json is absent.
//     Pre-first-controller-fetch, or refresher disabled. Falls
//     through to legacy in-pod injection.
//   - "skipped_empty_catalog": file present but contained zero
//     models. Same fallback behavior.
//   - "skipped_personal_key": user has a personal opencode API key.
//     Match legacy semantics — do not interpose the relay.
//   - "applied": relay block written to agent-config.json. opencode
//     will boot once with the final config.
//
// Returns nil on every outcome above. Returns an error only if
// reading the catalog file fails parse, or writing the agent-config
// fails — both indicate a real bug.
//
// Inputs:
//   - relayURL: full INFERENCE_RELAY_BASEURL (already includes the
//     path-segment secret if any).
//   - authJSONPath: opencode auth.json path (for bypass check).
//   - agentConfigPath: target agent-config.json path.
//
// The function uses the same buildRelayProviderEntry +
// updateAuthJSONForRelay helpers that the in-pod injector uses, so
// the resulting config is byte-identical to what the legacy path
// would have produced — by design, so the two paths are
// interchangeable.
func applyRelayConfigPreBoot(relayURL, authJSONPath, agentConfigPath string, logger *zap.Logger) (string, error) {
	if relayURL == "" {
		return "skipped_no_relay_url", nil
	}

	models, present, err := readFreeModelsFile(effectiveFreeModelsPath())
	if err != nil {
		return "error_catalog_decode", fmt.Errorf("read free-models catalog: %w", err)
	}
	if !present {
		return "skipped_no_catalog", nil
	}
	if len(models) == 0 {
		return "skipped_empty_catalog", nil
	}

	if skip, reason := shouldSkipRelay(authJSONPath); skip {
		if logger != nil {
			logger.Info("pre-boot relay: skipping",
				zap.String("reason", reason),
				zap.String("path", authJSONPath))
		}
		return "skipped_personal_key", nil
	}

	// Build the writer over the existing agent-config.json. The
	// materialize subcommand has already written providers + model
	// to this path; loadExisting captures both as initial sources.
	// SetRelay + Rebuild then merges the relay block in.
	//
	// The three options mirror main.go: applyRelayConfigPreBoot runs as
	// part of the materialize subcommand, BEFORE agentd's main writer
	// exists. The bootstrap subcommand has written adminPrompt to
	// agentd.AdminPromptPath and allowedDirs to agentd.AllowedDirsPath
	// (separate tmpfs files), and the built-in admin MCP server must be
	// injected. Without these options, the pre-boot relay Rebuild would
	// produce a config missing the admin system prompt, allowed external
	// directories, and the llmsafespaces MCP server — degrading the agent
	// until the first credential reload. Verified via write-path trace
	// (FlushProviders/applyMCPServersToConfig/applyWorkspaceConfig write
	// only {$schema, provider, model, mcp} — not the agent/mode blocks).
	writer := opencode.NewConfigWriter(agentConfigPath,
		opencode.WithAdminPromptPath(effectiveAdminPromptPath()),
		opencode.WithAllowedDirsPath(effectiveAllowedDirsPath()),
		opencode.WithPreMarshalHook(injectAgentdMCPServer),
	)
	writer.SetRelay(relayURL, models)
	if err := writer.Rebuild(); err != nil {
		return "error_config_write", fmt.Errorf("rebuild agent-config: %w", err)
	}

	if err := updateAuthJSONForRelay(authJSONPath); err != nil {
		// auth.json failure is logged but not fatal: opencode boots
		// with the relay-provider block in agent-config.json but
		// without an auth.json entry for it. The first request
		// through the relay will get a 401 from opencode; the user
		// can re-auth manually. Better than failing the whole boot.
		if logger != nil {
			logger.Warn("pre-boot relay: failed to update auth.json (relay block written but auth missing)",
				zap.Error(err),
				zap.String("path", authJSONPath))
		}
		return "applied_auth_failed", nil
	}

	if logger != nil {
		logger.Info("pre-boot relay: wrote relay config",
			zap.Int("models", len(models)),
			zap.String("path", agentConfigPath),
			zap.String("relayHost", relayURLHost(relayURL)))
	}
	return "applied", nil
}
