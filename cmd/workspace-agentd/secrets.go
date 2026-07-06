// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

// secrets.go — Glue between the workspace-agentd binary and the
// pkg/agentd/secrets package. This file holds:
//
//   - materializeConfig: typed bundle of filesystem paths.
//   - loadMaterializeConfig: env-var driven path resolution with sensible
//     defaults that match the production pod layout.
//   - runMaterializeCommand: implements the `materialize` subcommand. The
//     subcommand reads /sandbox-cfg/secrets.json (or --from), applies the
//     batch via the secrets package, and exits non-zero ONLY on I/O or
//     parse failures. Per-secret validation skips do not block boot.
//   - reloadSecretsHandler: HTTP handler for /v1/reload-secrets. Same
//     semantics as the subcommand but driven by an HTTP request body and
//     with optional opencode restart on env/llm changes.
//   - buildEnvFrom: replaces the legacy buildEnv() string-mangling with a
//     proper FormatEnvLine/ParseEnvLine round-trip.
//
// Splitting this out of main.go gives the materialize logic a stable test
// surface and prevents a future change to main.go's HTTP wiring from
// silently regressing the secrets path.

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"go.uber.org/zap"

	"github.com/lenaxia/llmsafespaces/pkg/agent/opencode"
	"github.com/lenaxia/llmsafespaces/pkg/agentd"
	"github.com/lenaxia/llmsafespaces/pkg/agentd/secrets"
)

// reloadMu serializes concurrent calls to reloadSecretsHandler. Two
// simultaneous reloads (from two API replicas or from parallel credential
// binds) race through Materializer.reset() which calls RemoveAll on
// SecretsBaseDir and RemoveAll on SSHDir, and both then appendFile to
// SecretsEnvPath — producing duplicate env var entries. Holding this mutex
// for the materialize → enrich → flush → re-merge sequence ensures exactly
// one reload runs at a time per pod. The restart at the end is excluded from
// the lock to avoid holding it during the ~5s SIGTERM window.
var reloadMu sync.Mutex

// restartableProcess is the subset of *managedProcess needed by the
// session-aware restart logic. Extracting it as an interface lets tests
// pass a mock without constructing a real managedProcess supervisor.
type restartableProcess interface {
	restart()
}

// restartIdleCheckInterval is how often the deferred-restart goroutine
// polls the sessionStatusTracker for busy→idle transitions. 5s per
// the US-44.2 design.
const restartIdleCheckInterval = 5 * time.Second

// defaultMaxDefer bounds how long a deferred restart waits for busy sessions
// to idle before force-restarting (worklog 371 H1). Without it, a stuck
// session (infinite loop, hung MCP, deadlocked tool) defers the restart
// forever and the credential change never applies — silent non-application.
//
// Design 0045 Change 5: reduced from 2h to 15m. Rationale: with Change 4's
// tracker-empty semantic fix, the defer path is now reached only when the
// tracker has SSE-observed busy state — i.e., a session is *genuinely*
// running work. 15 minutes covers legitimate long-running agentic turns
// (reasoning models, slow tool calls, multi-step workflows) with generous
// headroom. Longer just wastes credential freshness for a stuck session;
// the force-restart at expiry logs a warning so operators can correlate
// the interruption.
const defaultMaxDefer = 15 * time.Minute

// sessionListerProbeTimeout bounds the cost of probing opencode's /session
// endpoint from the restart decision path. If opencode is unreachable the
// probe fails fast; if it is slow, we don't block the reload handler.
const sessionListerProbeTimeout = 3 * time.Second

// sessionLister returns the current live session IDs from opencode, or nil
// if opencode is unreachable. Used by the deferred-restart goroutine for
// pruning stale busy entries from the tracker (C2a): when opencode dies
// mid-busy and the supervisor respawns it, the tracker retains a stale
// "busy" entry for a session that no longer exists. Calling prune() with
// the live session list removes it.
//
// Design 0045 Change 4 removed the second (cold-start / C2b) use of the
// lister — trackerHasBusyOrUnknown no longer probes opencode when the
// tracker is empty; it returns false (not busy) directly. See
// design/0045_2026-07-06_boot-time-user-dek-delivery.md.
//
// Returns a non-nil slice (possibly empty) when opencode is reachable, nil
// when opencode is unreachable. "Empty non-nil" means "opencode is alive
// with zero sessions".
type sessionLister func(ctx context.Context) []string

// pruneFromLister prunes the tracker using the live session list from
// opencode. No-op if tracker is nil, lister is nil, the tracker is empty
// (nothing to prune — and the caller trackerHasBusyOrUnknown will probe
// opencode itself), or the probe fails (opencode unreachable — cannot
// verify, leave tracker as-is).
func pruneFromLister(ctx context.Context, tracker *sessionStatusTracker, lister sessionLister) {
	if tracker == nil || lister == nil || !tracker.hasAnyData() {
		return
	}
	probeCtx, cancel := context.WithTimeout(ctx, sessionListerProbeTimeout)
	defer cancel()
	if ids := lister(probeCtx); ids != nil {
		tracker.prune(ids)
	}
}

// trackerHasBusyOrUnknown reports whether the restart should be deferred.
// Returns true (defer) when:
//   - the tracker has data AND any session is busy.
//
// Returns false (proceed with restart) when:
//   - the tracker has data AND no session is busy, OR
//   - the tracker is empty. Design 0045 Change 4: an empty tracker means
//     the SSE stream (our only truth source for session busyness) has not
//     observed any busy session on this agentd's connection. /session
//     returns a list of session RECORDS from opencode.db, not a busyness
//     indicator: a session that was busy 8 hours ago but idle now still
//     appears in /session. Previously this branch probed /session and
//     deferred if any records existed, which caused cold-boot credential
//     updates to defer for the full maxDefer window (up to 2h) — the
//     RC2 bug from the 2026-07-06 incident.
//
// The remaining edge case is the brief window between agentd start and
// SSE reconnect (bounded by session_tracker.go's backoff, typically ~2s):
// a session that is genuinely busy in that window appears idle to the
// tracker. If a credential push lands in that window, the restart fires
// on a possibly-busy session. That trade is acceptable: (a) the window
// is bounded and short, (b) the previous branch silently held stale
// credentials for hours in the vastly more common cold-boot case, and
// (c) even the old branch was best-effort — SSE can disconnect at any
// time and the same window exists on any reconnect.
func trackerHasBusyOrUnknown(tracker *sessionStatusTracker) bool {
	if tracker != nil && tracker.hasAnyData() {
		return tracker.hasAnyBusy()
	}
	// Empty tracker → definitively not busy (see design 0045 Change 4).
	return false
}

// makeSessionAwareRestartDecision decides whether to restart opencode now or
// defer until sessions are idle. Returns true if the restart was initiated
// (immediately or via a deferred goroutine that has since fired), false if
// the restart was deferred to a background goroutine.
//
// Behavior:
//
//   - If proc is nil, returns true without doing anything (test/no-op path).
//   - If the tracker shows all sessions idle OR the tracker is empty
//     (design 0045 Change 4 — empty tracker = no busy signal observed via
//     SSE, so restart immediately), restarts immediately.
//   - If sessions are busy per the SSE tracker, defers the restart until
//     they idle or maxDefer elapses.
//
// The deferred goroutine:
//
//   - Polls every pollInterval, pruning stale entries via lister (C2a) and
//     re-checking busy state.
//   - Selects on ctx.Done() so it is canceled at agentd shutdown (H1a).
//   - Force-restarts after maxDefer (H1b) so credentials eventually apply
//     even if sessions stay busy forever (stuck tool, infinite loop).
//   - Is tracked by bgWg (H1c) so shutdown waits for it before proc.stop().
//
// maxDefer <= 0 falls back to defaultMaxDefer. pollInterval <= 0 falls back
// to restartIdleCheckInterval.
func makeSessionAwareRestartDecision(
	ctx context.Context,
	proc restartableProcess,
	tracker *sessionStatusTracker,
	pollInterval time.Duration,
	maxDefer time.Duration,
	lister sessionLister,
	bgWg *sync.WaitGroup,
) bool {
	if proc == nil {
		return true
	}
	if maxDefer <= 0 {
		maxDefer = defaultMaxDefer
	}
	if pollInterval <= 0 {
		pollInterval = restartIdleCheckInterval
	}
	// ctx is the agentd background lifecycle context (outlives any single HTTP
	// request). When nil (tests, or deps.BgCtx unset), fall back to
	// context.Background() so the deferred goroutine has a cancellable root.
	// This fallback lives here rather than in the handler so the handler's
	// only context source is r.Context() (avoids a contextcheck lint conflict
	// between the request-scoped and background contexts in the same scope).
	if ctx == nil {
		ctx = context.Background() //nolint:contextcheck // root context for a background goroutine that must outlive any HTTP request — intentionally not derived from a parent
	}

	// Prune stale entries before deciding (C2a).
	pruneFromLister(ctx, tracker, lister)

	if !trackerHasBusyOrUnknown(tracker) {
		proc.restart()
		return true
	}

	// Sessions are busy — defer. (Empty tracker is handled as "not busy"
	// above, per design 0045 Change 4; we can only reach here when the
	// tracker has SSE-observed busy state.)
	//
	// TOCTOU note: between the trackerHasBusyOrUnknown check above and the
	// listBusy() call below, an SSE event can transition the last busy
	// session to idle. In that case listBusy returns empty and we log the
	// "unknown status" branch below. The deferred goroutine's next poll
	// tick will observe idle and restart within pollInterval — no
	// permanent stall.
	var busy []string
	if tracker != nil {
		busy = tracker.listBusy()
	}
	if len(busy) > 0 {
		log.Info("session-aware restart: deferring restart, sessions are busy",
			zap.Strings("busySessions", busy),
			zap.Duration("maxDefer", maxDefer))
	} else {
		// TOCTOU race: hasAnyBusy → true was observed, but by the time we
		// called listBusy the last busy session transitioned to idle. The
		// deferred goroutine will observe this on its next poll tick and
		// restart within pollInterval.
		log.Info("session-aware restart: deferring restart, tracker raced to idle between check and log (will restart on next poll tick)",
			zap.Duration("maxDefer", maxDefer),
			zap.Duration("pollInterval", pollInterval))
	}

	runDeferred := func() {
		deadline := time.NewTimer(maxDefer)
		defer deadline.Stop()
		ticker := time.NewTicker(pollInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				log.Info("session-aware restart: deferred restart canceled by shutdown")
				return
			case <-deadline.C:
				log.Warn("session-aware restart: max-defer elapsed, force-restarting to apply credential change",
					zap.Duration("maxDefer", maxDefer),
					zap.Strings("busySessions", func() []string {
						if tracker != nil {
							return tracker.listBusy()
						}
						return nil
					}()))
				proc.restart()
				return
			case <-ticker.C:
				pruneFromLister(ctx, tracker, lister)
				if !trackerHasBusyOrUnknown(tracker) {
					log.Info("session-aware restart: all sessions now idle, applying deferred restart")
					proc.restart()
					return
				}
			}
		}
	}

	if bgWg != nil {
		bgWg.Add(1)
		go func() {
			defer bgWg.Done()
			runDeferred()
		}()
	} else {
		go runDeferred()
	}

	return false
}

// materializeConfig is the resolved set of filesystem paths used by the
// materialize subcommand and the reload handler. It maps 1:1 onto
// secrets.Paths but lives here so the binary can override defaults via
// environment variables (which the secrets package, by design, does not
// know about).
type materializeConfig struct {
	home            string
	secretsBaseDir  string
	sshDir          string
	agentConfigPath string
	secretsEnvPath  string
	gitCredsPath    string
	// enricherCacheDir is the directory used by the model enricher to cache
	// provider model lists between credential reloads. It must NOT be inside
	// secretsBaseDir because reset() deletes that directory on every Materialize
	// call, which would destroy the cache before it could be used.
	// Default: $HOME/.local/state/llmsafespaces (on the workspace PVC subPath:home,
	// outside SecretsBaseDir and SSHDir, never cleaned by reset()).
	enricherCacheDir string
	// reloadCachePath is where reloadSecretsHandler persists the last reload
	// batch for replay after a main-container restart (#443). It is on the
	// /sandbox-runtime tmpfs (survives container restart, wiped on pod death).
	// See agentd.ReloadSecretsCachePath.
	reloadCachePath string
}

func (c materializeConfig) toPaths() secrets.Paths {
	return secrets.Paths{
		Home:            c.home,
		SecretsBaseDir:  c.secretsBaseDir,
		SSHDir:          c.sshDir,
		AgentConfigPath: c.agentConfigPath,
		SecretsEnvPath:  c.secretsEnvPath,
		GitCredsPath:    c.gitCredsPath,
	}
}

// loadMaterializeConfig resolves filesystem paths. It honors the same
// LLMSAFESPACES_* env-var overrides used by the test suite; in production
// no overrides are set and defaults match the runtime pod layout.
//
// US-35.7: credential paths point to /sandbox-runtime (tmpfs) so reset()
// operates on tmpfs targets, not PVC-side symlinks. The symlinks are created
// by the credential-setup init container and point into the same tmpfs paths.
func loadMaterializeConfig() materializeConfig {
	home := envOrDefault("HOME", "/home/sandbox")
	return materializeConfig{
		home:             home,
		secretsBaseDir:   envOrDefault("LLMSAFESPACES_SECRETS_BASE_DIR", agentd.SecretsBasePath),
		sshDir:           envOrDefault("LLMSAFESPACES_SSH_DIR", "/sandbox-runtime/rt/ssh"),
		agentConfigPath:  envOrDefault("LLMSAFESPACES_AGENT_CONFIG_PATH", agentd.AgentConfigPath),
		secretsEnvPath:   envOrDefault("LLMSAFESPACES_SECRETS_ENV_PATH", agentd.SecretsEnvPath),
		gitCredsPath:     envOrDefault("LLMSAFESPACES_GIT_CREDS_PATH", "/sandbox-runtime/rt/git-credentials"),
		enricherCacheDir: envOrDefault("LLMSAFESPACES_ENRICHER_CACHE_DIR", home+"/.local/state/llmsafespaces"),
		reloadCachePath:  envOrDefault("LLMSAFESPACES_RELOAD_CACHE_PATH", agentd.ReloadSecretsCachePath),
	}
}

func envOrDefault(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// runMaterializeCommand implements the `materialize` subcommand.
//
// Exit codes:
//
//	0 — secrets file applied successfully (every secret either Materialized
//	    or Skipped). Skipped is not a failure: it means the input was
//	    structurally rejected, which is the security policy.
//	0 — secrets file is absent. Pods without user-supplied credentials
//	    boot normally.
//	2 — input file is unreadable or unparseable.
//	3 — at least one secret failed to apply due to an I/O error.
//
// The reason for distinguishing 2 from 3 is operability: 2 means the
// controller wrote a malformed secrets.json (bug in the API server); 3
// means the node filesystem is misbehaving (e.g. tmpfs full).
func runMaterializeCommand(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("materialize", flag.ContinueOnError)
	fs.SetOutput(stderr)
	from := fs.String("from", "/sandbox-cfg/secrets.json", "path to secrets.json")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	cfg := loadMaterializeConfig()

	// Load the base batch from secrets.json (written by the init container's
	// bootstrap — server-KEK creds only). Absent is the zero-credential /
	// pre-first-bind state and is handled as an empty batch below.
	baseSecrets, err := secrets.LoadSecretsFile(*from)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) && !strings.Contains(err.Error(), "no such file") {
			_, _ = fmt.Fprintf(stderr, "materialize: %v\n", err)
			return 2
		}
		baseSecrets = nil
	}

	// Replay the last reload-secrets batch (survives container restart on the
	// /sandbox-runtime tmpfs; wiped on pod death). This restores user-DEK
	// credentials (env-secrets, SSH keys, user LLM providers) that were
	// live-pushed after boot and would otherwise be lost when reset() runs
	// again on this restart (#443). Absent on first boot / fresh pod.
	cachedSecrets := loadReloadSecretsCache(cfg.reloadCachePath, stderr)

	// Merge: base (server-KEK) + cache (last live state). The cache is the
	// newer ground truth — it wins on any duplicate Type+Name.
	secretsList := mergeSecretBatches(baseSecrets, cachedSecrets)

	if len(secretsList) == 0 {
		// No credentials at all (first boot, zero-credential user, and no
		// prior reload). Still apply workspace-config.json so the default
		// model is written to agent-config.json even when no LLM credentials
		// are configured.
		applyWorkspaceConfig(cfg.toPaths().AgentConfigPath, *from)
		return 0
	}

	m := &secrets.Materializer{FS: secrets.RealFS(), Paths: cfg.toPaths()}
	result, err := m.Materialize(secretsList)
	reportResult(stderr, result)

	if err != nil && !errors.Is(err, secrets.ErrPartialFailure) {
		_, _ = fmt.Fprintf(stderr, "materialize: %v\n", err)
		return 3
	}

	// Enrich staged providers that have a custom BaseURL but no model list.
	// This fetches the live model list from the provider's /models endpoint
	// (e.g. ai.thekao.cloud/v1/models) so opencode uses the correct model IDs
	// instead of its internal hardcoded list. Results are cached to cacheDir
	// for providerModelCacheTTL so pod restarts don't re-fetch unnecessarily.
	httpClient := &http.Client{Timeout: 15 * time.Second}
	m.EnrichProviders(enrichProviderModels(context.Background(), cfg.enricherCacheDir, httpClient))

	// Flush staged llm-provider secrets to AgentConfigPath so opencode
	// reads them at startup. Without this, the config file is empty and
	// opencode boots with no provider credentials.
	if flushErr := m.FlushProviders(opencode.FormatOpenCodeConfig); flushErr != nil {
		_, _ = fmt.Fprintf(stderr, "materialize: flush providers: %v\n", flushErr)
		return 3
	}

	// Apply workspace-level default model if present. This file is
	// written by the API server alongside secrets.json.
	applyWorkspaceConfig(cfg.toPaths().AgentConfigPath, *from)

	// 2026-06-23 cold-start optimization (item #1a, Phase C): pre-render
	// the relay-provider block in agent-config.json BEFORE opencode is
	// started. This eliminates the in-pod opencode-restart cycle that
	// startRelayInjector imposes when called after opencode is running
	// (saves ~6-8s per cold start AND every resume).
	//
	// Inputs (all controlled by the controller's pod_builder):
	//   - $INFERENCE_RELAY_BASEURL: relay URL with embedded path-secret.
	//     Empty → no-op (relay disabled cluster-wide).
	//   - /sandbox-cfg/free-models.json: cluster-wide free-models catalog
	//     dropped by the credential-setup init container. Absent → no-op
	//     (Phase A refresher hasn't published yet, or it's disabled);
	//     the in-pod startRelayInjector will run after opencode boots.
	//   - $HOME/.local/opencode/auth.json: bypass check for personal
	//     opencode key. Skipped if the user is paying for direct Zen.
	//
	// Outcome is logged but not fatal. Failures of the catalog read or
	// the agent-config write are returned as exit 3 (the same as a
	// secrets I/O failure) so kubelet sees CrashLoop on a real bug.
	if outcome, err := applyRelayConfigPreBoot(
		os.Getenv("INFERENCE_RELAY_BASEURL"),
		preBootAuthJSONPath(cfg.home),
		cfg.toPaths().AgentConfigPath,
		log,
	); err != nil {
		_, _ = fmt.Fprintf(stderr, "materialize: pre-boot relay (%s): %v\n", outcome, err)
		return 3
	} else if outcome != "skipped_no_relay_url" && outcome != "skipped_no_catalog" {
		// Useful operability signal in pod logs.
		_, _ = fmt.Fprintf(stderr, "materialize: pre-boot relay outcome=%s\n", outcome)
	}

	if result != nil && result.HasFailures() {
		// Some I/O failure already logged via reportResult; exit 3 so the
		// runtime entrypoint can surface this to kubelet (CrashLoopBackOff
		// rather than silent partial-credential boot).
		return 3
	}

	// Design 0045 Change 3: persist the applied batch to the reload cache.
	//
	// Rationale (validated via stress-testing round 2): agentd's hasUserCreds
	// (healthz.go) reports UserCredsPresent based on the *cache file's*
	// existence, not on whether secrets.json contains user-DEK content. If
	// the init container's materialize does NOT write the cache, the FIRST
	// healthz-after-boot reports UserCredsPresent=false, which triggers a
	// spurious auto-push from the API's watcher — even when pod-bootstrap
	// already delivered user-DEK secrets via Change 1. The auto-push
	// applies the same batch and triggers an opencode restart ~30s into
	// pod life.
	//
	// Writing the cache here closes that loop: hasUserCreds returns true
	// on the first healthz call, the watcher observes UserCredsPresent=true,
	// and secretautopush emits "skipped_ucp_true" (no redundant push).
	//
	// Non-fatal on write failure: the cache only affects the auto-push
	// filtering signal. On write failure, agentd degrades to the pre-
	// Change-3 behavior (one spurious auto-push post-boot). No user-facing
	// impact beyond a wasted opencode restart.
	//
	// Empty-batch guard: skip cache write when there's nothing to persist.
	// Empty-path guard: production always resolves the path via
	// loadMaterializeConfig → agentd.ReloadSecretsCachePath; tests may
	// construct materializeConfig without reloadCachePath.
	if cfg.reloadCachePath != "" && len(secretsList) > 0 {
		if pErr := writeReloadSecretsCache(cfg.reloadCachePath, secretsList); pErr != nil {
			_, _ = fmt.Fprintf(stderr, "materialize: failed to persist reload cache (auto-push will still fire post-boot as fallback): %v\n", pErr)
		}
	}

	// Skips are intentional; do not fail the boot.
	return 0
}

// reportResult writes a human-readable per-secret summary to stderr so
// `kubectl logs <pod>` operators see materialization outcomes.
func reportResult(w io.Writer, r *secrets.MaterializeResult) {
	if r == nil {
		return
	}
	mat, skip, fail := r.Counts()
	_, _ = fmt.Fprintf(w, "materialize: %d materialized, %d skipped, %d failed\n", mat, skip, fail)
	for _, sr := range r.Results {
		if sr.Outcome == secrets.OutcomeMaterialized {
			continue
		}
		_, _ = fmt.Fprintf(w, "  - %s/%s: %s — %s\n", sr.Type, sr.Name, sr.Outcome, sr.Reason)
	}
}

// applyWorkspaceConfig reads workspace-config.json (sibling to secrets.json)
// and merges the default model into the agent config file. This ensures the
// workspace's model selection survives pod restarts.
//
// DefaultModel is stored as a flat catalog ID (e.g. "glm-5.1"). opencode
// requires the fully-qualified "providerID/modelID" form in agent-config.json.
// We resolve the providerID by scanning the provider map already written to
// agent-config.json by FlushProviders (which runs before this function).
// If no provider claims the model, the flat ID is written as a best-effort
// fallback (opencode will reject it at startup, but the per-prompt model
// override in the frontend still routes correctly for interactive sessions).
func applyWorkspaceConfig(agentConfigPath, secretsPath string) {
	// workspace-config.json lives alongside secrets.json in /sandbox-cfg/
	dir := filepath.Dir(secretsPath)
	configPath := filepath.Join(dir, "workspace-config.json")

	data, err := os.ReadFile(configPath)
	if err != nil {
		return // absent = no workspace config to apply
	}

	var wsCfg struct {
		DefaultModel string `json:"defaultModel"`
	}
	if json.Unmarshal(data, &wsCfg) != nil || wsCfg.DefaultModel == "" {
		return
	}

	// Read existing agent config (written by FlushProviders above).
	var cfg map[string]json.RawMessage
	existing, err := os.ReadFile(agentConfigPath)
	if err == nil && len(existing) > 0 {
		_ = json.Unmarshal(existing, &cfg)
	}
	if cfg == nil {
		cfg = map[string]json.RawMessage{}
	}

	// Resolve providerID from the provider map so opencode gets the fully-
	// qualified "providerID/modelID" form it requires. The provider map is
	// written by FlushProviders (called just before this function), so all
	// user-configured providers are already present.
	model := resolveModelWithProvider(cfg, wsCfg.DefaultModel)
	modelJSON, _ := json.Marshal(model)
	cfg["model"] = modelJSON

	if _, ok := cfg["$schema"]; !ok {
		schemaJSON, _ := json.Marshal("https://opencode.ai/config.json")
		cfg["$schema"] = schemaJSON
	}

	merged, _ := json.MarshalIndent(cfg, "", "  ")
	_ = os.WriteFile(agentConfigPath, merged, 0o600)
}

// resolveModelWithProvider scans the "provider" map in the agent config and
// returns "providerID/modelID" when the flat modelID is found in any provider's
// models map. Returns the flat modelID unchanged if no provider claims it
// (e.g. when the provider list hasn't been written yet, or the model was
// removed from the catalog since it was last selected).
func resolveModelWithProvider(cfg map[string]json.RawMessage, flatModelID string) string {
	if flatModelID == "" {
		return ""
	}
	// Already qualified — nothing to do.
	if strings.Contains(flatModelID, "/") {
		return flatModelID
	}

	providerRaw, ok := cfg["provider"]
	if !ok {
		return flatModelID
	}

	// provider map shape: {"providerID": {"models": {"modelID": {...}, ...}, ...}, ...}
	var providers map[string]struct {
		Models map[string]json.RawMessage `json:"models"`
	}
	if json.Unmarshal(providerRaw, &providers) != nil {
		return flatModelID
	}

	for providerID, p := range providers {
		if _, found := p.Models[flatModelID]; found {
			return providerID + "/" + flatModelID
		}
	}
	return flatModelID
}

// reloadSecretsDeps bundles the runtime dependencies that
// reloadSecretsHandler needs beyond the materialize config. Grouping them in
// a struct keeps the handler signature stable as dependencies are added and
// makes call sites self-documenting.
type reloadSecretsDeps struct {
	// Proc is the supervised opencode process. May be nil in tests; in
	// production it is a *managedProcess so the handler can restart
	// opencode after env/llm secret changes.
	Proc restartableProcess

	// OpencodePassword is the Basic-auth password every request to opencode
	// (PUT /auth/:providerID, POST /instance/dispose) must carry. Production
	// reads /sandbox-cfg/password at startup; tests pass "" since they
	// either skip the credential push (no llm-provider in the batch) or
	// stub the URL to a server that does not enforce auth. An empty
	// password produces 401 against real opencode and was the proximate
	// cause of Bug 1 (worklog 0125).
	OpencodePassword string

	// Tracker is the SSE session-status tracker. May be nil.
	Tracker *sessionStatusTracker

	// BgCtx is the agentd background-goroutine context. The deferred-restart
	// goroutine selects on it so it is canceled at shutdown (H1a). When
	// nil, context.Background() is used (goroutine lives until restart fires
	// or maxDefer elapses — tests only).
	BgCtx context.Context

	// BgWg tracks background goroutines for clean shutdown. The deferred-
	// restart goroutine registers here so main's shutdown waits for it
	// before proc.stop() (H1c). May be nil (tests only).
	BgWg *sync.WaitGroup

	// Lister probes opencode's /session endpoint for the live session list.
	// Used by the deferred-restart goroutine to prune stale busy entries
	// (C2a) — when opencode dies mid-busy and respawns, the tracker retains
	// a stale busy entry for a session that no longer exists. May be nil
	// (pruneFromLister is a no-op in that case).
	//
	// Design 0045 Change 4 removed the second (cold-start / C2b) use — the
	// restart decision no longer probes /session when the tracker is empty.
	Lister sessionLister

	// AgentConfigWriter is the single writer of agent-config.json. The
	// reload handler calls SetProviders + Rebuild after formatting the
	// staged credentials, replacing the old FlushProviders + manual relay
	// re-merge sequence. Required.
	AgentConfigWriter *AgentConfigWriter

	// RestartReasonMarkerPath overrides where the restart-reason marker is
	// written. Empty falls back to the package const RestartReasonMarkerPath
	// (production). Tests inject a path under t.TempDir() (or a sabotaged
	// path) to assert marker-write behavior without polluting /workspace.
	RestartReasonMarkerPath string
}

// reloadSecretsHandler returns the HTTP handler for /v1/reload-secrets.
func reloadSecretsHandler(cfg materializeConfig, deps reloadSecretsDeps) http.HandlerFunc {
	proc := deps.Proc
	opencodePassword := deps.OpencodePassword
	tracker := deps.Tracker
	lister := deps.Lister
	markerPath := deps.RestartReasonMarkerPath
	if markerPath == "" {
		markerPath = RestartReasonMarkerPath
	}

	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		var batch []secrets.Secret
		if err := json.NewDecoder(r.Body).Decode(&batch); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": "invalid json: " + err.Error()})
			return
		}
		// Capture the request context once and propagate it explicitly to the
		// downstream calls that need it. Threading a local ctx (rather than
		// repeated r.Context() calls) keeps the context lineage obvious to
		// readers and to the contextcheck linter.
		reqCtx := r.Context()

		// Serialize the materialize → enrich → flush → re-merge sequence.
		// Concurrent reloads (from two API replicas or parallel credential binds)
		// race through Materializer.reset() which RemoveAlls SecretsBaseDir and
		// SSHDir and appendFiles to SecretsEnvPath — producing duplicate env var
		// entries and interleaved agent-config.json writes. The restart at the
		// end is excluded from the lock to avoid holding it during the ~5s SIGTERM
		// window.
		reloadMu.Lock()

		m := &secrets.Materializer{FS: secrets.RealFS(), Paths: cfg.toPaths()}
		result, mErr := m.Materialize(batch)

		if mErr != nil && !errors.Is(mErr, secrets.ErrPartialFailure) {
			reloadMu.Unlock()
			log.Error("reload-secrets: materialize failed", zap.Error(mErr))
			w.WriteHeader(http.StatusInternalServerError)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": mErr.Error()})
			return
		}
		if result == nil {
			result = &secrets.MaterializeResult{}
		}

		// Persist the just-applied batch so a main-container restart (OOM,
		// panic, kubelet restart) can replay it. reset() inside the next boot's
		// Materialize would otherwise wipe these user-DEK credentials with no
		// way to restore them (#443). The cache lives on the /sandbox-runtime
		// tmpfs: survives container restart, wiped on pod death (US-35.7).
		// Written here — after Materialize succeeded — so the cached state
		// reflects what was materialized to the filesystem even if a later
		// step (writer rebuild) fails. Non-fatal: a write failure warns but
		// does not roll back the live materialization; the cost is only that
		// the creds will not survive the *next* restart.
		//
		// The empty-path guard defends existing tests that construct
		// materializeConfig without reloadCachePath: without it, an unset path
		// would create temp files in the test CWD and log a WARN on every
		// reload handler call. Production always resolves the path via
		// loadMaterializeConfig → agentd.ReloadSecretsCachePath.
		if cfg.reloadCachePath != "" {
			if pErr := writeReloadSecretsCache(cfg.reloadCachePath, batch); pErr != nil {
				log.Warn("reload-secrets: failed to persist reload batch for restart replay; "+
					"user-DEK credentials may be lost on the next container restart",
					zap.Error(pErr))
			}
		}

		// Enrich custom-endpoint providers with their live model list (same as
		// the boot-time materialize path). On reload, any cached model list is
		// reused so this is typically instant.
		reloadHTTPClient := &http.Client{Timeout: 15 * time.Second}
		m.EnrichProviders(enrichProviderModels(reqCtx, cfg.enricherCacheDir, reloadHTTPClient))

		// Format staged llm-provider secrets and update the AgentConfigWriter.
		// The writer is the sole writer of agent-config.json — it merges the
		// new providers with any existing model and relay config, then writes
		// atomically (temp + rename). This eliminates the four-writer race
		// that previously required a manual relay re-merge after FlushProviders.
		formatted, fmtErr := m.FormatProviders(opencode.FormatOpenCodeConfig)
		if fmtErr != nil {
			reloadMu.Unlock()
			log.Error("reload-secrets: format providers failed", zap.Error(fmtErr))
			w.WriteHeader(http.StatusInternalServerError)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": "format providers: " + fmtErr.Error()})
			return
		}
		if formatted != nil && deps.AgentConfigWriter != nil {
			if err := deps.AgentConfigWriter.setProviders(formatted); err != nil {
				reloadMu.Unlock()
				log.Error("reload-secrets: setProviders failed", zap.Error(err))
				w.WriteHeader(http.StatusInternalServerError)
				_ = json.NewEncoder(w).Encode(map[string]string{"error": "set providers: " + err.Error()})
				return
			}
			if rbErr := deps.AgentConfigWriter.rebuild(); rbErr != nil {
				// C1 regression fix: reset() already deleted agent-config.json.
				// If rebuild fails (e.g. disk full), the file is ABSENT. Restarting
				// opencode now would boot with no provider config — silent credential
				// loss. Abort with 500 (matching the old FlushProviders failure path)
				// so the running opencode keeps its in-memory config.
				reloadMu.Unlock()
				log.Error("reload-secrets: agent-config writer rebuild failed", zap.Error(rbErr))
				w.WriteHeader(http.StatusInternalServerError)
				_ = json.NewEncoder(w).Encode(map[string]string{"error": "agent-config rebuild: " + rbErr.Error()})
				return
			}
		}

		reloadMu.Unlock()

		mat, skip, fail := result.Counts()
		log.Info("secrets reloaded",
			zap.Int("materialized", mat),
			zap.Int("skipped", skip),
			zap.Int("failed", fail),
		)

		// Stage llm-provider credentials. StageCredentials writes to opencode's
		// auth.json but does NOT dispose the instance. The user triggers reload
		// explicitly via POST /api/v1/workspaces/:id/agent/reload (Epic 27a).
		if hasLLMProviders(batch) {
			staged := m.StagedProviders()
			if len(staged) > 0 {
				oc := opencode.NewClient(fmt.Sprintf("http://localhost:%d", agentd.AgentPort), opencodePassword, log)
				if err := oc.StageCredentials(reqCtx, staged); err != nil {
					log.Warn("reload-secrets: opencode stage failed; credentials remain in "+
						"auth.json on disk but in-memory provider state will not pick them up "+
						"until the next explicit reload or pod restart",
						zap.Error(err))
				}
			}
		}

		restarted := false
		if proc != nil && shouldRestart(batch) {
			if reason, names := classifySecretRestartReason(batch); reason != "" {
				if err := writeRestartReasonMarker(markerPath, reason, names); err != nil {
					log.Error("failed to write restart-reason marker", zap.Error(err))
				} else {
					logRestartReasonAtWrite(reason, names, log.Core())
				}
				// H2: record the restart in the Prometheus counter so ops
				// dashboards surface credential-change restarts. Recorded
				// UNCONDITIONALLY (after the marker/log block), not gated on
				// marker-write success — a full/read-only PVC must not suppress
				// the metric. This matches the crash path (main.go) and the OOM
				// path (oom_detection.go), which also record the metric
				// regardless of marker outcome. The reason label is the short
				// metric form (env_secrets / api_key) matching the help text and
				// the crash/oom reasons.
				pkgOpsMetrics.RecordRestart(workspaceIDFromEnv(), metricRestartReason(reason))
			}
			restarted = makeSessionAwareRestartDecision(deps.BgCtx, proc, tracker, restartIdleCheckInterval, defaultMaxDefer, lister, deps.BgWg) //nolint:contextcheck // deps.BgCtx is the agentd lifecycle context (not the request context) — the deferred goroutine must outlive the HTTP request
		}

		status := http.StatusOK
		if result.HasFailures() {
			status = http.StatusInternalServerError
		}
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"reloaded":  mat,
			"skipped":   skip,
			"failed":    fail,
			"results":   result.Results,
			"restarted": restarted,
		})
	}
}

// metricRestartReason maps a marker reason (from classifySecretRestartReason,
// used in the on-disk restart-reason marker) to the short Prometheus label
// used by opsMetrics.RecordRestart. The metric help text enumerates:
// env_secrets, api_key, crash, oom, user_requested. Unknown reasons pass
// through unchanged so the metric remains useful if new reasons are added.
func metricRestartReason(markerReason string) string {
	switch markerReason {
	case "env_secrets_changed":
		return "env_secrets"
	case "api_key_changed":
		return "api_key"
	default:
		return markerReason
	}
}

func shouldRestart(batch []secrets.Secret) bool {
	for _, s := range batch {
		if s.Type == "env-secret" || s.Type == "api-key" {
			return true
		}
	}
	return false
}

// hasLLMProviders returns true if the batch contains any llm-provider secrets.
func hasLLMProviders(batch []secrets.Secret) bool {
	for _, s := range batch {
		if s.Type == "llm-provider" {
			return true
		}
	}
	return false
}

// mergeSecretBatches combines a base batch with a layered batch, resolving
// duplicates in favor of the layered batch. The dedup key is Type+Name —
// the materializer's identity for a secret within a workspace. Metadata and
// Plaintext from the layered entry replace the base entry wholesale.
//
// Used at boot (#443): base = /sandbox-cfg/secrets.json (server-KEK creds),
// layered = the last reload-secrets cache (the newer, complete live state).
// The cache wins because a reload is a full-replace and therefore holds the
// most recent intended credential set.
func mergeSecretBatches(base, layered []secrets.Secret) []secrets.Secret {
	if len(base) == 0 {
		return layered
	}
	if len(layered) == 0 {
		return base
	}

	seen := make(map[string]int, len(base)+len(layered))
	merged := make([]secrets.Secret, 0, len(base)+len(layered))
	key := func(s secrets.Secret) string { return s.Type + "\x00" + s.Name }

	for _, s := range base {
		seen[key(s)] = len(merged)
		merged = append(merged, s)
	}
	for _, s := range layered {
		if idx, ok := seen[key(s)]; ok {
			merged[idx] = s
			continue
		}
		seen[key(s)] = len(merged)
		merged = append(merged, s)
	}
	return merged
}

// writeReloadSecretsCache atomically writes the batch as JSON to path with
// mode 0600. Atomicity uses a temp file in the same directory + os.Rename so
// a crash mid-write never leaves a truncated cache that would shadow the last
// known good state on the next restart.
//
// The file holds plaintext credentials, so 0600 is mandatory. The parent
// directory (/sandbox-runtime tmpfs) is created by the pod's init container
// and is writable by the sandbox user.
func writeReloadSecretsCache(path string, batch []secrets.Secret) error {
	data, err := json.Marshal(batch)
	if err != nil {
		return fmt.Errorf("marshal reload batch: %w", err)
	}
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".last-reload-secrets.*.tmp")
	if err != nil {
		return fmt.Errorf("create temp cache file: %w", err)
	}
	tmpName := tmp.Name()
	cleanup := func() { _ = os.Remove(tmpName) }

	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		cleanup()
		return fmt.Errorf("write cache: %w", err)
	}
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		cleanup()
		return fmt.Errorf("chmod cache: %w", err)
	}
	if err := tmp.Close(); err != nil {
		cleanup()
		return fmt.Errorf("close cache: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		cleanup()
		return fmt.Errorf("rename cache into place: %w", err)
	}
	return nil
}

// loadReloadSecretsCache reads and parses the persisted reload batch. It never
// returns an error: an absent file is the normal first-boot / fresh-pod state
// (returns nil), and a corrupt file degrades to base-only materialization with
// a warning on stderr so an operator can diagnose missing creds after a
// restart. A zero-length decode error is treated the same as absent.
func loadReloadSecretsCache(path string, stderr io.Writer) []secrets.Secret {
	data, err := os.ReadFile(path)
	if err != nil {
		if !os.IsNotExist(err) {
			_, _ = fmt.Fprintf(stderr, "materialize: read last-reload-secrets cache %q: %v\n", path, err)
		}
		return nil
	}
	var batch []secrets.Secret
	if err := json.Unmarshal(data, &batch); err != nil {
		_, _ = fmt.Fprintf(stderr,
			"materialize: ignoring corrupt last-reload-secrets cache %q (%v); "+
				"falling back to base secrets only — user-DEK credentials pushed since the last boot will NOT be restored\n",
			path, err)
		return nil
	}
	return batch
}

// buildEnvFrom returns the process environment with secrets-env entries
// merged in.
//
// Implementation: we delegate to bash itself rather than re-implement
// shell parsing in Go. Bash is the source of truth for what `source FILE`
// does, including handling values that contain newlines, single quotes
// (escaped via 'a'\”b'), and other shell-meaningful bytes. A pure Go
// parser would have to mirror bash's quoting rules exactly, which is the
// class of bug that produced G2 in the first place.
//
// We invoke `bash -c 'set -a; source FILE; env -0'` and parse the
// NUL-delimited output. Each record is KEY=VALUE; we filter to keys that
// were not already set in our parent environment so we only forward the
// secrets-introduced variables.
//
// If bash is unavailable or the file is missing/unreadable, we return the
// parent environment unchanged. The agent will run without user-injected
// env-secrets, which is a safe degradation.
func buildEnvFrom(path string) []string {
	parent := os.Environ()
	if _, err := os.Stat(path); err != nil {
		return parent
	}

	// Capture parent env as a set so we can identify which entries the
	// sourced file added.
	parentSet := make(map[string]struct{}, len(parent))
	for _, e := range parent {
		if i := strings.IndexByte(e, '='); i > 0 {
			parentSet[e[:i]] = struct{}{}
		}
	}

	// `set -a` causes every assignment in the sourced file to be exported,
	// even if the file omits the `export` keyword. `env -0` writes
	// NUL-delimited records so values containing newlines survive.
	// G204: bash + script body are constant; only `path` varies and it
	// is bound to $1 (positional argument), so even a path containing
	// shell metachars cannot escape the script body. noctx: this runs
	// at boot before context.Context is meaningful.
	//nolint:gosec,noctx // G204/noctx: positional bind, boot-time call
	out, err := exec.Command("bash", "-c",
		`set -a; source "$1"; env -0`,
		"_", path,
	).Output()
	if err != nil {
		log.Warn("buildEnvFrom: bash source failed; secrets env not loaded",
			zap.String("path", path), zap.Error(err))
		return parent
	}

	added := make([]string, 0)
	for _, rec := range strings.Split(string(out), "\x00") {
		if rec == "" {
			continue
		}
		i := strings.IndexByte(rec, '=')
		if i <= 0 {
			continue
		}
		key, val := rec[:i], rec[i+1:]
		if _, inParent := parentSet[key]; inParent {
			// Skip pre-existing env vars; we only want secrets-introduced ones.
			// (Bash's `env` will print all of them after `set -a; source`.)
			continue
		}
		added = append(added, key+"="+val)
	}
	return append(parent, added...)
}
