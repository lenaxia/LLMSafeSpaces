package main

// mcp_tools.go — the workspace self-management MCP tools served on
// /v1/mcp. Every opencode interaction goes through the contained wire
// seam (pkg/agent/opencode loopback methods) — this file holds NO
// opencode wire knowledge (Rule 12: one seam, no scatter). The two
// platform-facing paths are the SA-token internal API (rename_workspace)
// and pure local computation (get_datetime).

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"go.uber.org/zap"

	"github.com/lenaxia/llmsafespaces/pkg/agent/opencode"
	"github.com/lenaxia/llmsafespaces/pkg/board"
	"github.com/lenaxia/llmsafespaces/pkg/session/agentmessage"
	"github.com/lenaxia/llmsafespaces/pkg/shortid"
)

// titleMaxLength bounds session titles and workspace names handed to
// the tools (the workspaces.name column is varchar(255); opencode titles
// are free-form but an LLM-generated novel helps nobody).
const titleMaxLength = 200

// workspaceNameMaxLength mirrors the API-side validation and the
// workspaces.name column.
const workspaceNameMaxLength = 255

// Image attachment guards: per-file and total size caps plus a mime
// allowlist — data URLs are the only path image bytes take, and both
// caps are enforced BEFORE any wire call.
const (
	imageMaxFileBytes = 5 << 20
	imageMaxTotal     = 8 << 20
)

// mcpWorkspaceRenameTimeout bounds the platform API call.
const mcpWorkspaceRenameTimeout = 15 * time.Second

// seamClientWithPassword is seamClient for tests and callers carrying
// the credential explicitly.
func seamClientWithPassword(password string) *opencode.Client {
	return opencode.NewLoopbackClient(getAgentAddr(), password)
}

// --- rename_session -------------------------------------------------------

// mcpRenameSession renames a session in THIS workspace's opencode via
// the seam. The platform's session index learns the new title through
// the existing sessionstate projection, which reads titles from
// opencode.
func mcpRenameSession(ctx context.Context, password, sessionID, title string) (string, error) {
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" {
		return "", fmt.Errorf("sessionId is required")
	}
	// Design 0066: the ref resolves first (short id / alias accepted;
	// legacy ids pass through byte-identical).
	origRef := sessionID
	resolved, rerr := resolveSessionRef(ctx, password, sessionID)
	if rerr != nil {
		return "", rerr
	}
	sessionID = resolved
	title = strings.TrimSpace(title)
	if title == "" {
		return "", fmt.Errorf("title is required")
	}
	if len(title) > titleMaxLength {
		return "", fmt.Errorf("title exceeds maximum length (%d)", titleMaxLength)
	}
	client := seamClientWithPassword(password)
	if err := client.SessionRename(ctx, sessionID, title); err != nil {
		if isSessionNotFoundErr(err) {
			// #1663 near-miss clause, same as the other id-taking
			// tools: the rename is refused either way — suggest-only.
			return "", fmt.Errorf("failed to rename session: %w%s",
				err, sessionSuggestClause(ctx, client, sessionID))
		}
		return "", fmt.Errorf("failed to rename session: %w", err)
	}
	id, idDegraded := emitIDFields(sessionID)
	res := map[string]string{
		"status":     "renamed",
		"session_id": id,
		"title":      title,
	}
	if idDegraded {
		res["session_id_deprecated"] = "true"
	}
	// #1756 §2: the deprecation note rides ONLY deprecated-form
	// inputs — never an empty-string field on clean refs (r3 f4).
	if note := strings.TrimSpace(deprecationNote(origRef)); note != "" {
		res["deprecation"] = note
	}
	out, _ := json.Marshal(res)
	return string(out), nil
}

// --- rename_workspace -----------------------------------------------------

// mcpRenameWorkspace renames the workspace THIS pod belongs to. The
// display name lives in PostgreSQL behind the API, so the tool POSTs to
// /internal/v1/workspace-rename with the projected SA token — the same
// credential surface as pod-bootstrap. Workspace identity comes from
// pod env (WORKSPACE_ID), never from tool arguments: a pod can only
// ever name itself.
func mcpRenameWorkspace(ctx context.Context, name string) (string, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return "", fmt.Errorf("name is required")
	}
	if len(name) > workspaceNameMaxLength {
		return "", fmt.Errorf("name exceeds maximum length (%d)", workspaceNameMaxLength)
	}

	workspaceID := strings.TrimSpace(os.Getenv("WORKSPACE_ID"))
	if workspaceID == "" {
		return "", fmt.Errorf("rename_workspace unavailable: WORKSPACE_ID is not set in this pod")
	}
	apiURL := strings.TrimSpace(os.Getenv("LLMSAFESPACE_API_URL"))
	if apiURL == "" {
		return "", fmt.Errorf("rename_workspace unavailable: LLMSAFESPACE_API_URL is not set in this pod")
	}
	token, err := os.ReadFile(bootstrapTokenPathFromEnv())
	if err != nil {
		return "", fmt.Errorf("rename_workspace unavailable: pod SA token unreadable: %w", err)
	}

	ctx, cancel := context.WithTimeout(ctx, mcpWorkspaceRenameTimeout)
	defer cancel()
	body, err := json.Marshal(map[string]string{"workspaceID": workspaceID, "name": name})
	if err != nil {
		return "", err
	}
	//nolint:gosec // G704: apiURL is controller-set via LLMSAFESPACE_API_URL env, not user/tool-controllable (bootstrap.go precedent)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		strings.TrimSuffix(apiURL, "/")+"/internal/v1/workspace-rename", bytes.NewReader(body))
	if err != nil {
		return "", fmt.Errorf("failed to build rename request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+string(token))
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req) //nolint:gosec // G704: controller-set origin, see above
	if err != nil {
		return "", fmt.Errorf("failed to reach the platform: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	switch resp.StatusCode {
	case http.StatusNoContent, http.StatusOK:
		out, _ := json.Marshal(map[string]string{
			"status":       "renamed",
			"workspace_id": workspaceID,
			"name":         name,
		})
		return string(out), nil
	case http.StatusUnauthorized, http.StatusForbidden:
		return "", fmt.Errorf("the platform rejected this pod's identity (status %d) — secrets_resync may refresh a stale credential, otherwise retry later", resp.StatusCode)
	case http.StatusNotFound:
		return "", fmt.Errorf("workspace %s not found on the platform", workspaceID)
	default:
		errBody, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return "", fmt.Errorf("platform returned status %d for workspace rename: %s", resp.StatusCode, string(errBody))
	}
}

// --- call_with_model ------------------------------------------------------

// imageMIMEByExt maps allowed image extensions to mimes; "" = rejected.
func imageMIMEByExt(name string) string {
	switch strings.ToLower(filepath.Ext(name)) {
	case ".png":
		return "image/png"
	case ".jpg", ".jpeg":
		return "image/jpeg"
	case ".gif":
		return "image/gif"
	case ".webp":
		return "image/webp"
	default:
		return ""
	}
}

// loadImages reads workspace image files into seam attachments under
// the size/mime guards. Only REGULAR files are admitted and the read
// itself is size-capped: a FIFO passes a size-0 stat and then blocks
// ReadFile forever (file I/O ignores the context — the tool call would
// wedge), and special/symlinked files would stream unbounded bytes into
// memory and onto the wire (review finding, PR #1364).
func loadImages(paths []string) ([]opencode.ImageAttachment, error) {
	if len(paths) == 0 {
		return nil, nil
	}
	var out []opencode.ImageAttachment
	var total int64
	for _, p := range paths {
		p = strings.TrimSpace(p)
		if p == "" {
			return nil, fmt.Errorf("image path is empty")
		}
		mime := imageMIMEByExt(p)
		if mime == "" {
			return nil, fmt.Errorf("%s: unsupported image type (allowed: png, jpg, jpeg, gif, webp)", filepath.Base(p))
		}
		// O_NONBLOCK: opening a FIFO's read end BLOCKS until a writer
		// appears — the open itself is the wedge, before any stat/gate
		// can run. Non-blocking open returns immediately; on regular
		// files the flag is a no-op.
		f, err := os.OpenFile(p, os.O_RDONLY|syscall.O_NONBLOCK, 0) //nolint:gosec // agent-supplied workspace path, gated below
		if err != nil {
			return nil, fmt.Errorf("%s: unreadable: %w", filepath.Base(p), err)
		}
		info, err := f.Stat()
		if err != nil {
			_ = f.Close()
			return nil, fmt.Errorf("%s: unreadable: %w", filepath.Base(p), err)
		}
		if !info.Mode().IsRegular() {
			_ = f.Close()
			return nil, fmt.Errorf("%s: not a regular file (FIFOs, devices, and sockets are refused)", filepath.Base(p))
		}
		if info.Size() > imageMaxFileBytes {
			_ = f.Close()
			return nil, fmt.Errorf("%s: exceeds the %d MiB per-image cap", filepath.Base(p), imageMaxFileBytes>>20)
		}
		// Cap the READ, not just the stat: stat-then-read is a TOCTOU —
		// the file can grow (or be swapped) between the two.
		data, err := io.ReadAll(io.LimitReader(f, imageMaxFileBytes+1))
		_ = f.Close()
		if err != nil {
			return nil, fmt.Errorf("%s: unreadable: %w", filepath.Base(p), err)
		}
		if int64(len(data)) > imageMaxFileBytes {
			return nil, fmt.Errorf("%s: exceeds the %d MiB per-image cap", filepath.Base(p), imageMaxFileBytes>>20)
		}
		total += int64(len(data))
		if total > imageMaxTotal {
			return nil, fmt.Errorf("images exceed the %d MiB combined cap", imageMaxTotal>>20)
		}
		out = append(out, opencode.ImageAttachment{
			Filename: filepath.Base(p),
			MIME:     mime,
			Data:     data,
		})
	}
	return out, nil
}

// mcpCallWithModel runs ONE synchronous model call with a per-prompt
// override and returns the response text as the tool result.
//
// Why a transient carrier session (and why the exchange is still
// in-line): opencode BLOCKS messages to a busy session until its turn
// ends (live-proven on 1.18.15), and the calling agent's session is
// definitionally busy while this tool executes — sending synchronously
// into it would deadlock. The single-call primitive therefore needs an
// idle session; a fresh one per call also guarantees clean context (no
// bleed between calls). The exchange lands in the CURRENT conversation
// through the tool call + result — the same channel every tool uses —
// and the carrier is deleted so history stays clean.
func mcpCallWithModel(ctx context.Context, password, prompt, model string, images []string) (string, error) {
	prompt = strings.TrimSpace(prompt)
	if prompt == "" {
		return "", fmt.Errorf("prompt is required")
	}
	providerID, modelID, err := opencode.SplitModelRef(model)
	if err != nil {
		return "", bareModelRefError(ctx, password, model)
	}
	imgs, err := loadImages(images)
	if err != nil {
		return "", err
	}

	client := seamClientWithPassword(password)

	// Catalog pre-check: a parsed provider/model pair that the
	// workspace's own catalog doesn't offer would otherwise surface as
	// an opaque seam 500 at send time (the model override is accepted
	// shape-wise and rejected deep in generation). Refuse loud and
	// self-correctingly BEFORE the carrier exists. Fail-open by
	// design: an unreachable/unparseable/empty catalog must not block
	// calls the wire would accept (#1307's fail-safe direction —
	// absent data is unknown, not invalid).
	if err := verifyModelInCatalog(ctx, client, providerID, modelID); err != nil {
		return "", err
	}

	// Vision pre-check: when images ride the call, refuse early (and
	// helpfully) when the target model's catalog entry is KNOWN to lack
	// image input (#1307 fail-safe direction: absent capability metadata
	// is unknown, not text-only — refusing on unknown would break image
	// calls on custom vision gateways that carry no metadata).
	//
	// The catalog is resolved from the opencode server LIVE per call
	// (no agentd-side snapshot — pinned by
	// TestMCPCallWithModel_VisionGateResolvesPerCall), but the server
	// itself reads its config only at process start: a capability
	// declared after boot is invisible until the workspace agent
	// restarts. The error must say so (#1598 ask 3) — a remedy that
	// silently changes nothing is worse than no remedy.
	if len(imgs) > 0 {
		info, err := client.ModelInfo(ctx, providerID, modelID)
		if err == nil && info.ImageInputKnown && !info.ImageInput {
			return "", fmt.Errorf("model %s does not accept image input per the workspace catalog — pick a vision-capable model (find one with the list_models tool), or, when this is a custom-endpoint model that really does accept images, ask the user to declare attachment:true on the credential's model entry (custom models absent from opencode's catalog are marked text-only by default until declared). The catalog is frozen at workspace agent startup and does not hot-reload: a newly declared capability takes effect only after the workspace agent restarts (ask the user to restart the workspace) — editing the config or PATCHing /config changes nothing until then", model)
		}
		// Catalog lookup failure / unknown capability is not fatal: the
		// send itself will surface any real incompatibility.
	}

	sessionID, err := client.SessionCreate(ctx, "call_with_model")
	if err != nil {
		return "", fmt.Errorf("failed to create a carrier session: %w", err)
	}
	defer func() {
		cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		_ = seamClientWithPassword(password).SessionDelete(cleanupCtx, sessionID)
		cancel()
	}()

	res, err := client.SessionSend(ctx, sessionID, prompt, model, imgs)
	if err != nil {
		return "", fmt.Errorf("model call failed: %w", err)
	}
	if res.Text == "" {
		return "", fmt.Errorf("the model returned no text parts%s", noTextPartsDetail(res))
	}
	out, _ := json.Marshal(map[string]any{
		"model": modelID,
		"text":  res.Text,
	})
	return string(out), nil
}

// noTextPartsDetail renders the #1598 ask-4 diagnostics for a response
// that carried no text: every part's type (with repeats counted, wire
// order) plus the bounded excerpt of the first non-text part that
// carried text. A response with NO parts at all gets its own message —
// an empty completion is a different failure than a wrong-shaped one.
func noTextPartsDetail(res *opencode.SendResult) string {
	if len(res.PartTypes) == 0 {
		return " (the response carried no parts at all — an empty completion)"
	}
	var rendered []string
	seen := map[string]int{}
	for _, pt := range res.PartTypes {
		seen[pt]++
		if seen[pt] == 1 {
			rendered = append(rendered, pt)
		}
	}
	// Counts per type, first-occurrence wire order (step-start×1,
	// reasoning×2, tool×1).
	withCounts := make([]string, 0, len(rendered))
	for _, pt := range rendered {
		withCounts = append(withCounts, fmt.Sprintf("%s×%d", pt, seen[pt]))
	}
	detail := fmt.Sprintf(" (response parts: %s)", strings.Join(withCounts, ", "))
	if res.NonTextExcerpt != "" {
		detail += fmt.Sprintf("; first non-text excerpt: %q", res.NonTextExcerpt)
	}
	return detail
}

// modelCatalogHint is appended to every model-selection failure so the
// caller can self-correct in one hop instead of guessing names.
const modelCatalogHint = `use the list_models tool to see the workspace's valid model names`

// quotedList renders did-you-mean candidates: 'a', 'b' or 'c' (sorted,
// deduped — several providers may offer the same bare model ID, and the
// same name can surface twice across signal sources).
func quotedList(items []string) string {
	sorted := append([]string(nil), items...)
	sort.Strings(sorted)
	quoted := make([]string, 0, len(sorted))
	for _, it := range sorted {
		if len(quoted) > 0 && quoted[len(quoted)-1] == "'"+it+"'" {
			continue
		}
		quoted = append(quoted, "'"+it+"'")
	}
	return strings.Join(quoted, ", ")
}

// bareModelRefError builds the failure for a model reference with no
// provider prefix ("classifier", not "thekaocloud/classifier" —
// SplitModelRef already rejected it). When the catalog is reachable it
// looks for connected models whose ID matches the bare name and
// surfaces them as did-you-mean candidates; a catalog that cannot be
// read degrades to the plain guidance (the lookup is advisory, never
// a gate).
func bareModelRefError(ctx context.Context, password, model string) error {
	bare := strings.ToLower(strings.Trim(strings.TrimSpace(model), "/"))
	// No concrete provider example here (#1598 ask 1): workspaces vary
	// in which providers are configured, and the did-you-mean lookup
	// below supplies REAL candidates whenever the catalog can be read —
	// a hardcoded example names providers that 500 in this workspace.
	msg := fmt.Sprintf("provider must be included: pass the model as provider-id/model-id, got %q", model)
	if models, err := seamClientWithPassword(password).AvailableModels(ctx); err == nil {
		var candidates []string
		for _, m := range models {
			if strings.ToLower(m.ID) == bare && bare != "" {
				candidates = append(candidates, m.Provider+"/"+m.ID)
			}
		}
		if len(candidates) > 0 {
			msg = fmt.Sprintf("provider must be included, found matching model name: did you mean %s? (%q has no provider prefix)", quotedList(candidates), model)
		}
	}
	return fmt.Errorf("%s — %s", msg, modelCatalogHint)
}

// verifyModelInCatalog fails LOUD when a parsed provider/model pair is
// absent from a REACHABLE, non-empty catalog, naming what IS available
// so the failure is self-correcting. Nil return = proceed, including
// on every catalog problem (fail-open; see mcpCallWithModel).
func verifyModelInCatalog(ctx context.Context, client *opencode.Client, providerID, modelID string) error {
	models, err := client.AvailableModels(ctx)
	if err != nil || len(models) == 0 {
		return nil
	}
	providerSeen := false
	for _, m := range models {
		if m.Provider != providerID {
			continue
		}
		providerSeen = true
		if m.ID == modelID {
			return nil
		}
	}
	if !providerSeen {
		providers := map[string]bool{}
		for _, m := range models {
			providers[m.Provider] = true
		}
		names := make([]string, 0, len(providers))
		for p := range providers {
			names = append(names, p)
		}
		if near := caseInsensitiveMatches(providerID, names); len(near) > 0 {
			return fmt.Errorf("provider %q is not in this workspace's model catalog (connected providers: %s; did you mean %s?) — %s",
				providerID, quotedList(names), quotedList(near), modelCatalogHint)
		}
		return fmt.Errorf("provider %q is not in this workspace's model catalog (connected providers: %s) — %s",
			providerID, quotedList(names), modelCatalogHint)
	}
	var ids []string
	for _, m := range models {
		if m.Provider == providerID {
			ids = append(ids, m.ID)
		}
	}
	sort.Strings(ids)
	// The GATE stays case-sensitive (opencode keys are exact); only the
	// suggestion is fuzzy — a case-typo'd ref gets the same one-hop
	// self-correction a bare name gets (review r2 robustness-1).
	if near := caseInsensitiveMatches(modelID, ids); len(near) > 0 {
		return fmt.Errorf("model %q is not offered by provider %q in this workspace's catalog (available: %s; did you mean %s?) — %s",
			modelID, providerID, quotedList(ids), quotedList(near), modelCatalogHint)
	}
	return fmt.Errorf("model %q is not offered by provider %q in this workspace's catalog (available: %s) — %s",
		modelID, providerID, quotedList(ids), modelCatalogHint)
}

// caseInsensitiveMatches returns the subset of candidates equal to want
// modulo case — the typo class a suggestion can fix with certainty
// (no fuzzy/prefix matching: a wrong-but-similar name is not a typo).
func caseInsensitiveMatches(want string, candidates []string) []string {
	var out []string
	for _, c := range candidates {
		if strings.EqualFold(c, want) {
			out = append(out, c)
		}
	}
	return out
}

// --- list_models ----------------------------------------------------------

// mcpListModels returns the workspace's usable model catalog — one
// entry per model of every CONNECTED provider, as provider/model
// references ready for call_with_model's model argument. Vision
// capability (the vision-gate refusal's remedy and the canonical
// call_with_model use case) rides each entry as a tri-state: true /
// false when known, ABSENT when the catalog carries no signal (#1307
// discipline — unknown is never flattened into false). Read-only
// catalog queries through the seam; no LLM call is made.
func mcpListModels(ctx context.Context, password string) (string, error) {
	client := seamClientWithPassword(password)
	models, err := client.AvailableModels(ctx)
	if err != nil {
		return "", fmt.Errorf("failed to read the model catalog: %w", err)
	}
	// Capability join (#1598 ask 2): one GET /config/providers for the
	// whole index. Fail-open on its error — the name catalog alone is
	// still a valid answer; vision fields simply go absent.
	caps, capsErr := client.ModelCapabilities(ctx)
	if capsErr != nil {
		log.Warn("list_models: capability catalog unreadable; serving names without vision fields",
			zap.Error(capsErr))
	}
	type entry struct {
		Model         string `json:"model"`
		Name          string `json:"name,omitempty"`
		ContextWindow int64  `json:"contextWindow,omitempty"`
		MaxOutput     int64  `json:"maxOutput,omitempty"`
		Vision        *bool  `json:"vision,omitempty"`
	}
	out := make([]entry, 0, len(models))
	for _, m := range models {
		e := entry{
			Model:         m.Provider + "/" + m.ID,
			Name:          m.DisplayName,
			ContextWindow: m.ContextWindow,
			MaxOutput:     m.MaxOutput,
		}
		if capsErr == nil {
			if info, ok := caps[m.Provider][m.ID]; ok && info.ImageInputKnown {
				v := info.ImageInput
				e.Vision = &v
			}
		}
		out = append(out, e)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Model < out[j].Model })
	payload, _ := json.Marshal(map[string]any{
		"count":  len(out),
		"models": out,
	})
	return string(payload), nil
}

// --- create_session -------------------------------------------------------

// mcpCreateSession creates a NEW top-level agent session — a peer of
// the current one, running independently in this shared workspace —
// and hands it its starting prompt fire-and-forget: the tool returns
// the session_id as soon as the session exists, while the first turn
// runs in the background. Delivery is a detached POST with no
// artificial timeout: it completes whenever the turn completes (a
// client-side cap would serve no correctness purpose — opencode owns
// the turn once admitted).
//
// Idle-subscriptions v1 (design §5): subscribe=true additionally
// registers the CALLER→child watch edge after creation + short-id
// registration. The flag never gates creation itself — creation never
// needed an origin — so every refusal path degrades to a loud warning
// with "subscribed": false, never a failed create.
func mcpCreateSession(ctx context.Context, password, prompt, title string, subscribe bool, injectedSession string) (string, error) {
	prompt = strings.TrimSpace(prompt)
	if prompt == "" {
		return "", fmt.Errorf("prompt is required")
	}
	title = strings.TrimSpace(title)
	if len(title) > titleMaxLength {
		return "", fmt.Errorf("title exceeds maximum length (%d)", titleMaxLength)
	}

	client := seamClientWithPassword(password)
	sessionID, err := client.SessionCreate(ctx, title)
	if err != nil {
		return "", fmt.Errorf("failed to create session: %w", err)
	}

	// Design 0066 §4.2: agentd asks the control plane to register the
	// new session (the API's registration seam is the single mint
	// owner) and returns the short id beside the harness id. On
	// control-plane timeout the harness id alone is returned —
	// degraded, self-heals at the proxy's first observation of the
	// session.
	shortID := ""
	if platClient, saToken, workspaceID, perr := podPlatformClient(); perr == nil {
		regCtx, regCancel := context.WithTimeout(ctx, 5*time.Second)
		if sid, rerr := platClient.RegisterSessionShortID(regCtx, saToken, workspaceID, sessionID); rerr == nil && sid != "" {
			shortID = sid
			currentSessionShortIDs().put(sid, sessionID)
		}
		regCancel()
	}

	// §5 create_session+subscribe: the CALLER watches the NEW session.
	// The child is top-level by construction (create_session births
	// peers, never subtasks) — only the CALLER's side needs the
	// subagent check. The default event set (idle+error) applies.
	subscribed := false
	var subWarn string
	if subscribe {
		subscribed, subWarn = subscribeCallerToCreatedSession(ctx, password, client, sessionID, injectedSession)
	}

	logger := log
	go func() {
		if _, err := seamClientWithPassword(password).SessionSend(context.WithoutCancel(ctx), sessionID, prompt, "", nil); err != nil {
			logger.Warn("create_session: background prompt delivery failed",
				zap.String("sessionID", sessionID), zap.Error(err))
		}
	}()

	// #1756 §1: the tool's identifier output IS the phrase; the
	// harness id never emits (degraded window: the seam timed out, the
	// phrase arrives via the replica's next pull — emit the harness id
	// with the deprecation clause).
	result := map[string]any{
		"status":        "created",
		"session_id":    sessionID,
		"prompt_status": "delivering_in_background",
	}
	if shortID != "" {
		result["session_id"] = shortID
	} else {
		result["deprecation"] = "session registered without a phrase (platform unavailable); address it by phrase after the replica converges — re-list with session_list"
	}
	if subscribe {
		result["subscribed"] = subscribed
		if subWarn != "" {
			result["warning"] = subWarn
		}
	}
	out, _ := json.Marshal(result)
	return string(out), nil
}

// --- get_datetime ---------------------------------------------------------

// mcpGetDatetime reports the current instant in UTC and the USER's
// timezone when known. Resolution order: an explicit `timezone`
// argument (IANA name — use when the zone is known from conversation
// context), then the platform's live browser-reported zone (pushed by
// the API when the user's browser connects), then the pod's local zone
// (TZ env or UTC). The `source` field says which won.
func mcpGetDatetime(timezoneArg string) (string, error) {
	now := time.Now()

	location := time.Local
	source := "pod"
	name := ""

	switch tz := userTimezone(); {
	case timezoneArg != "":
		loc, err := time.LoadLocation(timezoneArg)
		if err != nil {
			return "", fmt.Errorf("unknown timezone %q (IANA names like \"America/Los_Angeles\"; the tool cannot guess)", timezoneArg)
		}
		location, source, name = loc, "argument", timezoneArg
	case tz != "":
		// already validated at the endpoint
		if loc, err := time.LoadLocation(tz); err == nil {
			location, source, name = loc, "browser", tz
		}
	}

	local := now.In(location)
	out, _ := json.Marshal(getDatetimeResult{
		UTC:       now.UTC().Format(time.RFC3339),
		Local:     local.Format(time.RFC3339),
		Timezone:  name, // omitempty: ABSENT when unknown, never faked
		UTCOffset: local.Format("-07:00"),
		Source:    source,
	})
	return string(out), nil
}

// getDatetimeResult is the typed tool output. Timezone is omitempty by
// contract: the pod fallback emits NO zone name rather than an empty
// string (review finding — the shipped claims and the emission must
// agree; a typed struct also satisfies the no-map-shapes rule).
type getDatetimeResult struct {
	UTC       string `json:"utc"`
	Local     string `json:"local"`
	Timezone  string `json:"timezone,omitempty"`
	UTCOffset string `json:"utc_offset"`
	Source    string `json:"source"`
}

// --- session archive / delete (#1627) --------------------------------------

// podPlatformClient resolves the pod-identity deps and returns a client
// pointed at the platform's internal API (the automation transport).
func podPlatformClient() (*opencode.Client, string, string, error) {
	saToken, apiURL, workspaceID, err := automationDeps()
	if err != nil {
		return nil, "", "", err
	}
	return opencode.NewLoopbackClient(apiURL, ""), saToken, workspaceID, nil
}

// mcpSessionArchive archives (true) or unarchives (false) a session of
// THIS workspace via the platform. Archiving is a PLATFORM-level
// read-only marker: the agent-side history is untouched, the user's
// chat sends to that session are rejected with 409 session_archived,
// history stays viewable, and unarchiving restores chat instantly.
// In-pod peer sends are NOT blocked (enforcement is proxy-side by the
// owner's ruling) — the marker is for the USER's session list.
// #1879: the success reply carries archived_visibility — "verified"
// when the post-write confirmation read of the archived set agrees
// (and the pod's cache is refreshed: this pod's listings converge
// immediately), "unverified" when the read failed or disagreed (the
// state still holds; listings may lag up to one cache window).
func mcpSessionArchive(ctx context.Context, password, sessionID string, archived bool) (string, error) {
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" {
		return "", fmt.Errorf("sessionId is required")
	}
	// Design 0066: resolve the ref (short id / alias) to the canonical
	// harness id BEFORE the platform call — the platform's index keys
	// on the canonical id.
	origRef := sessionID
	resolved, rerr := resolveSessionRef(ctx, password, sessionID)
	if rerr != nil {
		return "", rerr
	}
	sessionID = resolved
	client, saToken, workspaceID, err := podPlatformClient()
	if err != nil {
		return "", err
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	if err := client.SessionArchive(ctx, saToken, workspaceID, sessionID, archived); err != nil {
		if isSessionNotFoundErr(err) {
			// #1663: the platform's index lookup 404s the mistyped id;
			// the pod's live inventory still names the near match. The
			// archive is refused either way — suggest-only.
			return "", fmt.Errorf("failed to archive session: %w%s",
				err, sessionSuggestClause(ctx, seamClientWithPassword(password), sessionID))
		}
		return "", fmt.Errorf("failed to archive session: %w", err)
	}
	// #1879: the write landed — now prove it is VISIBLE. A confirmation
	// read of the archived set (bypassing the ~15s throttle) both (a)
	// refreshes this pod's shared cache so the pod's own listings
	// converge immediately instead of after the TTL window, and (b)
	// lets the tool report verified/unverified. A failed or
	// disagreeing read never fails the tool (the platform write is the
	// state truth) and never fabricates cache freshness.
	confirmed := forceArchivedSetRefresh(ctx)
	visibility := "unverified"
	if confirmed != nil && confirmed[sessionID] == archived {
		visibility = "verified"
	}
	status := "archived"
	if !archived {
		status = "unarchived"
	}
	id, idDegraded := emitIDFields(sessionID)
	res := map[string]string{
		"status":              status,
		"session_id":          id,
		"archived_visibility": visibility,
	}
	if idDegraded {
		res["session_id_deprecated"] = "true"
	}
	// #1756 §2: the deprecation note rides ONLY deprecated-form
	// inputs — never an empty-string field on clean refs (r3 f4).
	if note := strings.TrimSpace(deprecationNote(origRef)); note != "" {
		res["deprecation"] = note
	}
	out, _ := json.Marshal(res)
	return string(out), nil
}

// mcpDeleteSession HARD-deletes a session — both sides: the platform's
// index entry AND the agent-side session in this workspace's store.
// There is no undo; archived is not deleted (archive is the reversible
// one). Refuses to delete the caller's OWN current session: that turn
// is still running, and the tool result would have nowhere to land
// (the abort_session convention).
func mcpDeleteSession(ctx context.Context, password, sessionID, injectedSession string) (string, error) {
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" {
		return "", fmt.Errorf("sessionId is required")
	}
	// Design 0066: resolve the ref first; the self-guard and the
	// platform delete then operate on the canonical harness id.
	origRef := sessionID
	resolved, rerr := resolveSessionRef(ctx, password, sessionID)
	if rerr != nil {
		return "", rerr
	}
	sessionID = resolved
	// Self-delete guard: the plugin-injected current session first; on
	// a degraded pod (no working plugin) the single busy session IS the
	// caller — the same fallback the metadata default uses (review r1
	// finding 4: without it the guard is inert exactly where the
	// plugin is missing). Ambiguity (0 or multiple busy) allows the
	// delete: the target is an explicitly named ID, not a guess.
	// (#1836: the logic lives in resolveCallerSession, shared with the
	// delete bulk twin.)
	if self := resolveCallerSession(ctx, password, injectedSession); self != "" && sessionID == self {
		return "", fmt.Errorf("refusing to delete your own current session — its turn (this tool call) is still running; finish your turn and let the user or a peer session delete it, or ask the user")
	}
	client, saToken, workspaceID, err := podPlatformClient()
	if err != nil {
		return "", err
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if err := client.PlatformSessionDelete(ctx, saToken, workspaceID, sessionID); err != nil {
		if isSessionNotFoundErr(err) {
			// #1663 near-miss clause, same as archive: the delete is
			// refused either way — destructive verbs never act on a
			// guess, but the caller learns the live id.
			return "", fmt.Errorf("failed to delete session: %w%s",
				err, sessionSuggestClause(ctx, seamClientWithPassword(password), sessionID))
		}
		return "", fmt.Errorf("failed to delete session: %w", err)
	}
	id, idDegraded := emitIDFields(sessionID)
	res := map[string]string{
		"status":     "deleted",
		"session_id": id,
	}
	if idDegraded {
		res["session_id_deprecated"] = "true"
	}
	// #1756 §2: the deprecation note rides ONLY deprecated-form
	// inputs — never an empty-string field on clean refs (r3 f4).
	if note := strings.TrimSpace(deprecationNote(origRef)); note != "" {
		res["deprecation"] = note
	}
	out, _ := json.Marshal(res)
	return string(out), nil
}

// --- session bulk variants (#1836) ------------------------------------------

// The #1836 session siblings: session_archive_bulk and
// session_delete_bulk — ONE agent round-trip for the N-session sweep
// (the lived pain: archiving ~48 sessions cost ~48 context-resending
// single-field calls). The batch adds ONLY the per-item envelope:
// every item is one full delegation to the single-session seams above
// (resolveSessionRef → the pod-identity platform write), serial, in
// order — no second write path, no concurrency, exactly what N single
// calls do minus the round-trips.
//
// Contract (issue #1836, the #1937/#1944 vocabulary):
//   - Per-item result array {id, status: done|failed, error?} —
//     partial failures reported per item, never all-or-nothing.
//   - Idempotent retry: re-submitting the batch (or its failed subset)
//     is safe. Archive: the platform seam is idempotent for repeat
//     writes (the root row always matches), so an already-archived
//     item returns done, not conflict. Delete (destructive — the
//     ruling recorded on #1836 for the delete batch): a delete's
//     already-done state is row ABSENCE, indistinguishable from a
//     miss and from a foreign row; all three render the SAME bare
//     not_found item. Rendering absence as done would fabricate
//     outcomes for probes and leak row existence; the re-submit
//     itself never errors, never double-writes.
//   - Per-item validation: one malformed/blank/non-string id sinks
//     only its own item; the rest of the batch still processes.
//   - Batch cap: board.BulkMaxItems (the merged #1937 constant — one
//     cap number across every bulk surface) with a clear overflow
//     error; a batch-level problem poisons every item, so empty and
//     over-cap refuse the whole call with zero wire spend.
const (
	sessionBulkDone   = "done"
	sessionBulkFailed = "failed"
)

// Per-item error codes (the board bulk's vocabulary at the session
// seam; sessions have no typed engine guards here, so the classes are
// the tool seam's own).
const (
	sessionBulkErrInvalidInput = "invalid_input"       // malformed item: blank, or not a string
	sessionBulkErrNotFound     = "not_found"           // resolve miss, platform 404, or (delete) absence
	sessionBulkErrInternal     = "internal_error"      // transport/5xx — honest, never a fabricated miss
	sessionBulkErrSelfDelete   = "self_delete_refused" // the caller's own current session (delete bulk)
)

// sessionBulkItemError is one failed item's error: the class code plus
// its detail (the same message the single tool renders — resolve
// misses carry the did-you-mean clause; this surface is
// workspace-scoped by pod identity, so there is no cross-user
// disclosure class to defend — the suggestions the single tools show
// are safe per item too).
type sessionBulkItemError struct {
	Code   string `json:"code"`
	Detail string `json:"detail"`
}

// sessionBulkItem is one submitted ref's outcome. Ref echoes the
// caller's id VERBATIM (phrase, alias, or raw — self-identifying, the
// #1937 rule); ArchivedVisibility rides done archive items only
// (#1879 semantics per item, from ONE shared post-write confirmation
// read); Error is present exactly on failed items.
type sessionBulkItem struct {
	Ref                string                `json:"id"`
	Status             string                `json:"status"`
	ArchivedVisibility string                `json:"archived_visibility,omitempty"`
	Error              *sessionBulkItemError `json:"error,omitempty"`
}

// sessionBulkValidateCount is the shared batch-level guard (#1836
// contracts 3+4 — a batch-level problem poisons every item, so it
// refuses whole, spending zero wire calls).
func sessionBulkValidateCount(n int) error {
	if n == 0 {
		return fmt.Errorf("sessionIds must carry at least one session")
	}
	if n > board.BulkMaxItems {
		return fmt.Errorf("bulk batch capped at %d items (got %d) — split the batch",
			board.BulkMaxItems, n)
	}
	return nil
}

// sessionBulkString coerces one array entry to a trimmed ref. ok is
// false for non-string or blank entries — the caller renders that
// item's own invalid_input row, never a batch failure.
func sessionBulkString(raw any) (string, bool) {
	s, ok := raw.(string)
	if !ok {
		return "", false
	}
	s = strings.TrimSpace(s)
	return s, s != ""
}

// sessionBulkInvalidItem renders a malformed entry. The ref echoes a
// printable form of what was submitted — the caller's own entry,
// distinguishable from its batch-mates (review r1 F2: a non-string
// echo of "" made [42, "  ", 3.5] three indistinguishable blank rows;
// now 42 renders "42", 3.5 renders "3.5", a blank string stays "").
func sessionBulkInvalidItem(raw any) sessionBulkItem {
	ref, ok := raw.(string)
	if !ok {
		ref = fmt.Sprintf("%v", raw)
	}
	return sessionBulkItem{
		Ref:    ref,
		Status: sessionBulkFailed,
		Error: &sessionBulkItemError{Code: sessionBulkErrInvalidInput,
			Detail: "not a non-blank session id string — this item only; the rest of the batch still processed"},
	}
}

func sessionBulkFailedItem(ref, code, detail string) sessionBulkItem {
	return sessionBulkItem{Ref: ref, Status: sessionBulkFailed,
		Error: &sessionBulkItemError{Code: code, Detail: detail}}
}

// bulkSuggester renders the #1663 near-miss clause per not-found item
// from ONE shared session-list fetch per bulk call (review r1's cost
// note: the singles pay one list read per call — done per ITEM that
// made a 50-miss batch cost 50 extra reads; the batch pays one total,
// fetched lazily, only when some item actually needs a clause; a
// failed fetch degrades to no suggestion, the single tool's rule).
type bulkSuggester struct {
	ctx      context.Context
	password string
	once     sync.Once
	sessions []opencode.SessionSummary
	fetched  bool
}

func (b *bulkSuggester) clause(ref string) string {
	b.once.Do(func() {
		sessions, err := seamClientWithPassword(b.password).SessionList(b.ctx)
		if err != nil {
			return
		}
		b.sessions = sessions
		b.fetched = true
	})
	if !b.fetched {
		return ""
	}
	return suggestRefClauseFrom(ref, b.sessions)
}

// resolveCallerSession is the self-identity the delete tools guard on:
// the plugin-injected current session first; on a degraded pod (no
// working plugin) the single busy session IS the caller (ambiguity —
// 0 or multiple busy — yields "", allowing the delete: the target is
// an explicitly named id, not a guess). Extracted from mcpDeleteSession
// for the bulk twin (#1836); behavior-identical.
func resolveCallerSession(ctx context.Context, password, injectedSession string) string {
	self := strings.TrimSpace(injectedSession)
	if self == "" {
		client := seamClientWithPassword(password)
		if busy, err := client.GetSessionStatuses(ctx); err == nil {
			var busyIDs []string
			for id, st := range busy {
				if st == "busy" || st == "retry" {
					busyIDs = append(busyIDs, id)
				}
			}
			if len(busyIDs) == 1 {
				self = busyIDs[0]
			}
		}
	}
	return self
}

// mcpSessionArchiveBulk archives (true) or unarchives (false) a batch
// of sessions — session_archive's batch twin. ONE shared direction
// across the batch (per-item direction would be N single calls); the
// direction itself is validated at dispatch (the REQUIRED DIRECTION
// discipline — no default, a malformed call archives nothing). Items
// run serially, each one full resolve + platform write; after the
// loop ONE forceArchivedSetRefresh both refreshes this pod's cache
// (listings converge immediately) and verifies every done item
// (#1879 — verified/unverified per item, never a fabricated
// freshness, never a failed write masked).
func mcpSessionArchiveBulk(ctx context.Context, password string, refs []any, archived bool) (string, error) {
	if err := sessionBulkValidateCount(len(refs)); err != nil {
		return "", err
	}
	client, saToken, workspaceID, err := podPlatformClient()
	if err != nil {
		return "", err
	}
	results := make([]sessionBulkItem, 0, len(refs))
	// resolvedOf maps each done item's verbatim ref to its canonical
	// harness id — the confirmation read keys on canonical ids.
	resolvedOf := make(map[string]string, len(refs))
	// One shared near-miss clause source per call (the r1 cost note —
	// per-item list fetches would cost one read per miss).
	suggest := &bulkSuggester{ctx: ctx, password: password}
	for _, raw := range refs {
		ref, ok := sessionBulkString(raw)
		if !ok {
			results = append(results, sessionBulkInvalidItem(raw))
			continue
		}
		resolved, rerr := resolveSessionRef(ctx, password, ref)
		if rerr != nil {
			// The resolver's message carries the did-you-mean clause
			// already (same as the single tool's resolve failure).
			results = append(results, sessionBulkFailedItem(ref, sessionBulkErrNotFound, rerr.Error()))
			continue
		}
		itemCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
		werr := client.SessionArchive(itemCtx, saToken, workspaceID, resolved, archived)
		cancel()
		if werr != nil {
			if isSessionNotFoundErr(werr) {
				// #1663 near-miss clause, same as the single tool: the
				// item is refused either way — suggest-only.
				results = append(results, sessionBulkFailedItem(ref, sessionBulkErrNotFound,
					fmt.Sprintf("failed to archive session: %v%s", werr,
						suggest.clause(resolved))))
				continue
			}
			// Transient class: the item's own internal_error row —
			// never a fabricated miss (a retry converges).
			results = append(results, sessionBulkFailedItem(ref, sessionBulkErrInternal,
				fmt.Sprintf("failed to archive session: %v", werr)))
			continue
		}
		resolvedOf[ref] = resolved
		results = append(results, sessionBulkItem{Ref: ref, Status: sessionBulkDone})
	}
	// ONE shared confirmation read (#1879, batch-amortized): refreshes
	// this pod's archived-set cache and verifies every done item. A
	// failed or disagreeing read renders unverified — the write is the
	// state truth; the tool never fails on it. Skipped when NOTHING
	// landed (r2: an all-failed batch has no visibility to prove — one
	// wasted platform read per such call otherwise).
	var confirmed map[string]bool
	if len(resolvedOf) > 0 {
		confirmed = forceArchivedSetRefresh(ctx)
	}
	for i := range results {
		if results[i].Status != sessionBulkDone {
			continue
		}
		visibility := "unverified"
		if confirmed != nil && confirmed[resolvedOf[results[i].Ref]] == archived {
			visibility = "verified"
		}
		results[i].ArchivedVisibility = visibility
	}
	out, _ := json.Marshal(map[string]any{"results": results})
	return string(out), nil
}

// mcpDeleteSessionBulk hard-deletes a batch of sessions —
// delete_session's batch twin. The self-delete guard is per item
// (resolved against the caller identity computed ONCE): the caller's
// own session renders self_delete_refused and the batch keeps
// processing — the platform is never asked for it. Idempotent retry
// is the destructive-variant ruling recorded on #1836: absence ≡ miss
// ≡ foreign → the same bare not_found item (the platform 404 and the
// resolve miss already agree byte-for-byte in spirit — both are the
// item's not-found verdict, never a conflict, never a batch error).
func mcpDeleteSessionBulk(ctx context.Context, password string, refs []any, injectedSession string) (string, error) {
	if err := sessionBulkValidateCount(len(refs)); err != nil {
		return "", err
	}
	self := resolveCallerSession(ctx, password, injectedSession)
	client, saToken, workspaceID, err := podPlatformClient()
	if err != nil {
		return "", err
	}
	results := make([]sessionBulkItem, 0, len(refs))
	// One shared near-miss clause source per call (the r1 cost note —
	// per-item list fetches would cost one read per miss).
	suggest := &bulkSuggester{ctx: ctx, password: password}
	for _, raw := range refs {
		ref, ok := sessionBulkString(raw)
		if !ok {
			results = append(results, sessionBulkInvalidItem(raw))
			continue
		}
		resolved, rerr := resolveSessionRef(ctx, password, ref)
		if rerr != nil {
			results = append(results, sessionBulkFailedItem(ref, sessionBulkErrNotFound, rerr.Error()))
			continue
		}
		if self != "" && resolved == self {
			results = append(results, sessionBulkFailedItem(ref, sessionBulkErrSelfDelete,
				"refusing to delete your own current session — its turn (this tool call) is still running; finish your turn and let the user or a peer session delete it, or ask the user"))
			continue
		}
		itemCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		werr := client.PlatformSessionDelete(itemCtx, saToken, workspaceID, resolved)
		cancel()
		if werr != nil {
			if isSessionNotFoundErr(werr) {
				// Absence ≡ miss ≡ foreign (#1836 destructive ruling):
				// the platform's 404 IS the verdict — same code either
				// way, the near-miss clause the single tool appends.
				results = append(results, sessionBulkFailedItem(ref, sessionBulkErrNotFound,
					fmt.Sprintf("failed to delete session: %v%s", werr,
						suggest.clause(resolved))))
				continue
			}
			results = append(results, sessionBulkFailedItem(ref, sessionBulkErrInternal,
				fmt.Sprintf("failed to delete session: %v", werr)))
			continue
		}
		results = append(results, sessionBulkItem{Ref: ref, Status: sessionBulkDone})
	}
	out, _ := json.Marshal(map[string]any{"results": results})
	return string(out), nil
}

// --- session_metadata -----------------------------------------------------

// archivedSetCache serves the platform's archived-session set for the
// metadata annotation (#1627 §4: MCP listings carry archive status).
// One workspace per pod; ~15s TTL (the readyz providerCache
// convention). On fetch failure the last known set is served stale —
// the annotation is advisory and must never fail the tool.
var (
	archivedSetCacheMu        sync.Mutex
	archivedSetCacheVal       map[string]bool
	archivedSetCacheAt        time.Time // retry throttle — stamped on every refresh ATTEMPT
	archivedSetCacheFetchedAt time.Time // last SUCCESSFUL fetch — the #1735 filter's freshness window
)

const archivedSetCacheTTL = 15 * time.Second

func archivedSessionSet(ctx context.Context) map[string]bool {
	archivedSetCacheMu.Lock()
	if !archivedSetCacheAt.IsZero() && time.Since(archivedSetCacheAt) < archivedSetCacheTTL {
		cached := archivedSetCacheVal
		archivedSetCacheMu.Unlock()
		return cached
	}
	archivedSetCacheMu.Unlock()

	fetched := fetchArchivedSessionSet(ctx)
	archivedSetCacheMu.Lock()
	archivedSetCacheAt = time.Now()
	if fetched != nil {
		archivedSetCacheVal = fetched
		archivedSetCacheFetchedAt = time.Now()
	}
	archivedSetCacheMu.Unlock()
	return archivedSetCacheVal
}

// filterableArchivedSet is the #1735 freshness-gated read: the listing
// filters may only rely on data whose last SUCCESSFUL fetch is within
// the TTL. During a platform outage the #1627 annotation keeps serving
// the last-known set (staleness there is cosmetic), but a filter must
// not silently hide rows on stale-forever data — past the window it
// returns nil (no-op filter: everything lists). Review r1 finding 1.
func filterableArchivedSet() map[string]bool {
	archivedSetCacheMu.Lock()
	defer archivedSetCacheMu.Unlock()
	if archivedSetCacheFetchedAt.IsZero() || time.Since(archivedSetCacheFetchedAt) >= archivedSetCacheTTL {
		return nil
	}
	return archivedSetCacheVal
}

// forceArchivedSetRefresh fetches the archived set UNGATED — no TTL, no
// throttle (#1879: the post-archive confirmation read must observe the
// write immediately). On success the shared cache is updated (val,
// throttle, and the freshness watermark) so this pod's listings and
// annotations converge without waiting out the window. On ANY failure
// it returns nil and leaves the cache untouched — a failed
// confirmation must never fabricate freshness (the filter would then
// trust a set it never saw).
func forceArchivedSetRefresh(ctx context.Context) map[string]bool {
	fetched := fetchArchivedSessionSet(ctx)
	if fetched == nil {
		return nil
	}
	archivedSetCacheMu.Lock()
	archivedSetCacheVal = fetched
	archivedSetCacheAt = time.Now()
	archivedSetCacheFetchedAt = time.Now()
	archivedSetCacheMu.Unlock()
	return fetched
}

// stampArchivedSetStale marks every row of a DEGRADED default listing
// with `"archived_set_stale":true` (#1879): the unfiltered degrade is
// correct (a stale set must not keep hiding rows, #1735 r1 f1) but it
// must never be SILENT — the incident's orchestrator re-archived
// already-archived sessions because the listing looked authoritative.
// Same splice discipline as the filter: each element's original bytes
// are preserved (no re-marshal — json.Marshal would compact and
// HTML-escape titles containing &<>; unknown fields survive) and the
// marker is inserted before the closing brace. On ANY parse failure
// the original body is returned unmarked.
func stampArchivedSetStale(body []byte) string {
	var elems []json.RawMessage
	if err := json.Unmarshal(body, &elems); err != nil {
		return string(body)
	}
	if elems == nil {
		// A JSON null body unmarshals into a nil slice without error —
		// byte-identity still holds (null is never rewritten to [])
		// (review r1 nit).
		return string(body)
	}
	out := make([]byte, 0, len(body)+len(elems)*len(`,"archived_set_stale":true`))
	out = append(out, '[')
	for i, e := range elems {
		if i > 0 {
			out = append(out, ',')
		}
		out = append(out, stampElemArchivedStale(e)...)
	}
	out = append(out, ']')
	return string(out)
}

// stampElemArchivedStale appends the marker field to one raw JSON
// object element. Non-object elements pass through untouched; an empty
// object becomes just the marker.
func stampElemArchivedStale(e json.RawMessage) []byte {
	trimmed := bytes.TrimRight(e, " \t\r\n")
	if len(trimmed) == 0 || trimmed[len(trimmed)-1] != '}' {
		return e
	}
	inner := trimmed[:len(trimmed)-1]
	if bytes.Equal(bytes.TrimSpace(inner), []byte("{")) {
		return []byte(`{"archived_set_stale":true}`)
	}
	res := make([]byte, 0, len(inner)+len(`,"archived_set_stale":true`)+1)
	res = append(res, inner...)
	res = append(res, `,"archived_set_stale":true`...)
	return append(res, '}')
}

// fetchArchivedSessionSet pulls the archived IDs from the platform's
// pod-identity endpoint. Nil on ANY failure (degrade silently — an
// unreachable platform must not break metadata, and fetch errors must
// not leak origins into tool output).
func fetchArchivedSessionSet(ctx context.Context) map[string]bool {
	client, saToken, workspaceID, err := podPlatformClient()
	if err != nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	ids, err := client.SessionArchivedSet(ctx, saToken, workspaceID)
	if err != nil {
		return nil
	}
	set := make(map[string]bool, len(ids))
	for _, id := range ids {
		set[id] = true
	}
	return set
}

// filterArchivedFromSessionList drops entries whose id is in the
// platform's archived set (#1735: archived sessions are opt-in on the
// listing surfaces). Byte-exact element preservation — each surviving
// element's bytes are spliced from the original body (raw-element
// concatenation, NOT a re-marshal: json.Marshal would compact and
// HTML-escape, rewriting titles containing &<>; review r2 finding 1);
// only the inter-element separators normalize to ",". On ANY parse
// failure the original body is returned unfiltered. Same advisory
// posture as the metadata annotation (#1627 §4): the platform fact
// must never fail the tool, and an empty/stale archived set
// short-circuits untouched.
//
// #1879 single-capture contract: the second return reports whether the
// set that BUILT this output was provably fresh (the filter ran) or
// degraded (the output is the UNFILTERED list — the caller must stamp
// it). The decision rides the ONE filterableArchivedSet() read inside
// this function, never a second read afterwards: a concurrent cache
// refresh (a sibling session_archive's force-refresh, or the throttled
// background refresh) between two reads could flip nil→fresh in the
// window and an unfiltered listing would ship UNSTAMPED — the exact
// silent degrade #1879 exists to prevent (review r1 finding).
func filterArchivedFromSessionList(ctx context.Context, body []byte) (string, bool) {
	archivedSessionSet(ctx) // shared-cache refresh/throttle (annotation source)
	archivedSet := filterableArchivedSet()
	if archivedSet == nil {
		// Degraded: no provably-fresh set — the documented degrade is
		// the unfiltered list (#1735 r1 f1); the caller stamps it.
		return string(body), false
	}
	if len(archivedSet) == 0 {
		return string(body), true
	}
	var elems []json.RawMessage
	if err := json.Unmarshal(body, &elems); err != nil {
		return string(body), true
	}
	out := make([]byte, 0, len(body))
	out = append(out, '[')
	dropped := 0
	wrote := 0
	for _, e := range elems {
		var id struct {
			ID string `json:"id"`
		}
		if err := json.Unmarshal(e, &id); err == nil && archivedSet[id.ID] {
			dropped++
			continue
		}
		if wrote > 0 {
			out = append(out, ',')
		}
		out = append(out, e...)
		wrote++
	}
	if dropped == 0 {
		return string(body), true
	}
	out = append(out, ']')
	return string(out), true
}

// mcpSessionMetadata aggregates read-only workspace + session vitals:
// per-session title/model/age/tokens/message-count/context fill, busy
// flags, and the workspace's own identity.
//
// Scope resolution (#1627 ruling 5, BREAKING): an explicit session_id
// wins; otherwise the DEFAULT is the CURRENT session only — the
// plugin-injected lsp_injected_session when present, else the single
// busy session (the calling session — the compact fallback for pods
// without a working plugin). all_sessions=true preserves the OLD
// every-session behavior explicitly — minus archived sessions, which
// are opt-in (#1735): the aggregate listing defaults to non-archived;
// include_archived adds them back. Single-ID scopes (explicit or
// resolved) are NEVER filtered — an archived session still resolves by
// id (read-only history stays addressable; the filter applies to
// listings/aggregates only).
//
// Security posture: every field is either already exposed by
// session_list/session_read (strictly more sensitive surfaces behind
// the same Basic gate) or is public pod identity (WORKSPACE_ID). No
// env values, credentials, tokens, or platform internals are included.
func mcpSessionMetadata(ctx context.Context, password, sessionID, injectedSession string, allSessions, includeArchived bool) (string, error) {
	client := seamClientWithPassword(password)
	sessionID = strings.TrimSpace(sessionID)
	injectedSession = strings.TrimSpace(injectedSession)

	scope := sessionID
	if scope == "" && !allSessions {
		switch {
		case injectedSession != "":
			scope = injectedSession
		default:
			resolved, err := resolveSingleBusySession(ctx, client)
			if err != nil {
				return "", fmt.Errorf("no explicit session_id and no injectable current session — pass session_id explicitly, or all_sessions:true for every session: %v", err)
			}
			scope = resolved
		}
	}

	sessions, err := client.SessionList(ctx)
	if err != nil {
		return "", fmt.Errorf("failed to list sessions: %w", err)
	}
	// Design 0066: an explicit scope is an ADDRESS — short ids
	// (case-insensitive) and unambiguous aliases (title or slug)
	// resolve against the live rows; legacy ids pass through
	// byte-identical.
	if scope != "" {
		resolved := scope
		folded := strings.ToLower(strings.TrimSpace(scope))
		if !shortid.IsIDForm(scope) {
			var matches []opencode.SessionSummary
			for _, s := range sessions {
				if strings.ToLower(strings.TrimSpace(s.Title)) == folded ||
					(s.Slug != "" && strings.ToLower(strings.TrimSpace(s.Slug)) == folded) {
					matches = append(matches, s)
				}
			}
			switch len(matches) {
			case 1:
				resolved = matches[0].ID
			case 0:
				return "", fmt.Errorf("session %s not found%s", scope, suggestRefClauseFrom(scope, sessions))
			default:
				return "", fmt.Errorf("session alias %q is ambiguous — %d sessions share it; none was used (use the exact session id or short id)",
					scope, len(matches))
			}
		} else if shortid.IsShortIDForm(scope) {
			if id, ok := currentSessionShortIDs().resolveShortID(scope); ok {
				resolved = id
			} else if pullSessionShortIDsRateLimited(ctx) {
				// Same on-miss convergence as every other tool (review
				// r1 skeptical pass: one shared precedence, no diverging
				// shortcut here).
				if id, ok := currentSessionShortIDs().resolveShortID(scope); ok {
					resolved = id
				}
			}
		}
		scope = resolved
	}
	busy, err := client.GetSessionStatuses(ctx)
	if err != nil {
		return "", fmt.Errorf("failed to read session statuses: %w", err)
	}
	archivedSet := archivedSessionSet(ctx)
	// #1879: one capture, two uses — the aggregate's freshness gate and
	// the top-level staleness marker must describe the SAME set the
	// rows below are annotated from (a per-row re-read could race a
	// background refresh and lie about the shape just served).
	freshArchivedSet := filterableArchivedSet()

	match := func(s opencode.SessionSummary) bool {
		if scope != "" {
			return s.ID == scope
		}
		// #1735: the aggregate (scope=="") defaults to non-archived;
		// include_archived is the explicit opt-in. Single-ID scopes
		// never reach this branch. The filter set is freshness-gated —
		// a stale set (platform outage past the TTL) no-ops (r1 f1).
		return includeArchived || !freshArchivedSet[s.ID]
	}
	type sessionMeta struct {
		ID string `json:"session_id"`
		// #1756 §1 parity with session_list rows: the harness id
		// ships FLAGGED when the replica is cold, never silent.
		IDDeprecated  bool    `json:"id_deprecated,omitempty"`
		Title         string  `json:"title"`
		Agent         string  `json:"agent,omitempty"`
		Model         string  `json:"model,omitempty"`
		Busy          bool    `json:"busy"`
		Archived      bool    `json:"archived,omitempty"`
		CreatedAt     string  `json:"created_at,omitempty"`
		UpdatedAt     string  `json:"updated_at,omitempty"`
		Age           string  `json:"age,omitempty"`
		Messages      int     `json:"messages"`
		InContext     int     `json:"in_context"`
		Tokens        int64   `json:"tokens_total,omitempty"`
		ContextTokens int64   `json:"context_tokens,omitempty"`
		ContextLimit  int64   `json:"context_limit,omitempty"`
		ContextFill   float64 `json:"context_fill,omitempty"`
	}
	var metas []sessionMeta
	for _, s := range sessions {
		if !match(s) {
			continue
		}
		mID, mIDDegraded := emitIDFields(s.ID)
		m := sessionMeta{
			ID:           mID,
			IDDeprecated: mIDDegraded,
			Title:        s.Title,
			Agent:        s.Agent,
			Busy:         busy[s.ID] == "busy" || busy[s.ID] == "retry",
			Archived:     archivedSet[s.ID],
		}
		if s.Time.Created > 0 {
			created := time.UnixMilli(s.Time.Created)
			m.CreatedAt = created.UTC().Format(time.RFC3339)
			m.Age = time.Since(created).Round(time.Second).String()
		}
		if s.Time.Updated > 0 {
			m.UpdatedAt = time.UnixMilli(s.Time.Updated).UTC().Format(time.RFC3339)
		}
		if s.Model != nil {
			m.Model = s.Model.ProviderID + "/" + s.Model.ID
		}
		if s.Tokens != nil {
			m.Tokens = s.Tokens.Input + s.Tokens.Output + s.Tokens.Reasoning
		}
		if m.Messages, err = client.SessionMessageCount(ctx, s.ID); err != nil {
			m.Messages = -1
		}
		if m.InContext, err = client.SessionContextCount(ctx, s.ID); err != nil {
			m.InContext = -1
		}
		m.ContextTokens = client.SessionPromptTokens(ctx, s.ID)
		if s.Model != nil && m.ContextTokens > 0 {
			if info, err := client.ModelInfo(ctx, s.Model.ProviderID, s.Model.ID); err == nil && info.ContextLimit > 0 {
				m.ContextLimit = info.ContextLimit
				m.ContextFill = float64(m.ContextTokens) / float64(info.ContextLimit)
			}
		}
		metas = append(metas, m)
	}
	if scope != "" && len(metas) == 0 {
		// The scope id failed to match ANY live session — the #1663
		// near-miss clause rides along (ids are already in hand; no
		// extra fetch). The echoed id is caller-facing (phrase when
		// the replica knows it — #1816 emission closure).
		ids := make([]string, 0, len(sessions))
		for _, s := range sessions {
			ids = append(ids, s.ID)
		}
		return "", fmt.Errorf("session %s not found%s", emitID(scope), suggestSessionID(scope, ids))
	}

	version := ""
	if len(sessions) > 0 {
		version = sessions[0].Version
	}
	outFields := map[string]any{
		"workspace_id":  os.Getenv("WORKSPACE_ID"),
		"agent_version": version,
		"sessions":      metas,
	}
	// #1879: the archived annotations (and, on the aggregate, the
	// archived filter) ride a ~15s pod cache of index-only state. When
	// the last successful fetch is beyond the window the result is
	// still served (never a failed tool) but marked: callers comparing
	// archive state across surfaces deserve to know it may be stale.
	if freshArchivedSet == nil {
		outFields["archived_set_stale"] = true
	}
	out, _ := json.Marshal(outFields)
	return string(out), nil
}

// --- compact --------------------------------------------------------------

// mcpCompact compacts a session's history (the working mechanism on
// pinned opencode is the V1 summarize route; the V2 compact endpoint is
// absent — 503 — on 1.18.15).
//
// Busy targets (including the calling agent's OWN session, which is
// busy while this tool runs): the summarize request queues server-side
// and completes exactly when the current turn ends (live-proven
// run-at-boundary semantics), so the tool fires it detached and returns
// immediately. Idle targets compact synchronously.
func mcpCompact(ctx context.Context, password, sessionID, model string) (string, error) {
	client := seamClientWithPassword(password)
	origRef := sessionID

	if sessionID == "" {
		resolved, err := resolveSingleBusySession(ctx, client)
		if err != nil {
			return "", err
		}
		sessionID = resolved
		origRef = "" // defaulted target — no caller ref to deprecate
	} else {
		// Design 0066: an explicit target is an ADDRESS — resolve it
		// (short id / unambiguous alias; legacy byte-identical).
		resolved, rerr := resolveSessionRef(ctx, password, sessionID)
		if rerr != nil {
			return "", rerr
		}
		sessionID = resolved
	}

	// Summarizing model: explicit override, else the session's current
	// model (the natural choice — same routing the session already uses).
	providerID, modelID := "", ""
	if model != "" {
		var err error
		if providerID, modelID, err = opencode.SplitModelRef(model); err != nil {
			return "", err
		}
	} else {
		sessions, err := client.SessionList(ctx)
		if err != nil {
			return "", fmt.Errorf("failed to resolve the session's model: %w", err)
		}
		for _, s := range sessions {
			if s.ID == sessionID && s.Model != nil {
				providerID, modelID = s.Model.ProviderID, s.Model.ID
			}
		}
		if providerID == "" {
			return "", fmt.Errorf("session %s has no current model — pass model explicitly", emitID(sessionID))
		}
	}

	busy, err := client.GetSessionStatuses(ctx)
	if err != nil {
		return "", fmt.Errorf("failed to read session status: %w", err)
	}

	if busy[sessionID] == "busy" || busy[sessionID] == "retry" {
		// Detached: completes at the turn boundary; the tool must not
		// wait (the waiting turn may be the caller's own).
		logger := log
		go func() {
			if err := client.SessionSummarize(context.WithoutCancel(ctx), sessionID, providerID, modelID); err != nil {
				logger.Warn("compact: background summarize failed",
					zap.String("sessionID", sessionID), zap.Error(err))
			}
		}()
		id, idDegraded := emitIDFields(sessionID)
		res := map[string]string{
			"status":     "scheduled",
			"session_id": id,
			"detail":     "session is busy (likely your current turn); compaction runs when the current turn ends",
		}
		if idDegraded {
			res["session_id_deprecated"] = "true"
		}
		if note := strings.TrimSpace(deprecationNote(origRef)); note != "" {
			res["deprecation"] = note
		}
		out, _ := json.Marshal(res)
		return string(out), nil
	}

	if err := client.SessionSummarize(ctx, sessionID, providerID, modelID); err != nil {
		if isSessionNotFoundErr(err) {
			// #1663 near-miss clause on the synchronous idle path (the
			// busy path is detached/log-only — no error surface). The
			// compaction is refused either way — suggest-only.
			return "", fmt.Errorf("compaction failed: %w%s",
				err, sessionSuggestClause(ctx, client, sessionID))
		}
		return "", fmt.Errorf("compaction failed: %w", err)
	}
	id2, id2Degraded := emitIDFields(sessionID)
	res2 := map[string]string{
		"status":     "compacted",
		"session_id": id2,
	}
	if id2Degraded {
		res2["session_id_deprecated"] = "true"
	}
	if note := strings.TrimSpace(deprecationNote(origRef)); note != "" {
		res2["deprecation"] = note
	}
	out, _ := json.Marshal(res2)
	return string(out), nil
}

// resolveSingleBusySession implements the omitted-session_id default:
// the caller's session is the busy one (its turn is executing this
// tool). Unambiguous only when exactly one session is busy.
func resolveSingleBusySession(ctx context.Context, client *opencode.Client) (string, error) {
	busy, err := client.GetSessionStatuses(ctx)
	if err != nil {
		return "", fmt.Errorf("failed to read session statuses: %w", err)
	}
	// retry-as-busy, matching mcpSendMessage/session_metadata: a session
	// in backoff is definitionally running.
	var busyIDs []string
	for id, st := range busy {
		if st == "busy" || st == "retry" {
			busyIDs = append(busyIDs, id)
		}
	}
	switch len(busyIDs) {
	case 1:
		return busyIDs[0], nil
	case 0:
		return "", fmt.Errorf("no session is currently running — pass session_id explicitly (find IDs with session_metadata)")
	default:
		// #1816 emission closure: the caller sees PHRASES when the
		// replica knows them — the raw harness ids never surface in
		// the refusal text.
		shown := make([]string, 0, len(busyIDs))
		for _, id := range busyIDs {
			shown = append(shown, emitID(id))
		}
		return "", fmt.Errorf("multiple sessions are busy (%s) — pass session_id explicitly", strings.Join(shown, ", "))
	}
}

// --- send_message ---------------------------------------------------------

// splitDuplicatedArgFragment detects the #1530 emission-duplication
// shape in a session_id argument: the value carries an escaped JSON
// tail — `TARGET","lsp_injected_session":"ORIGIN"}` (trailing brace
// optional) — the bytes a model emits when it duplicates the tail of
// a similar prior send_message call visible in its context. Probes
// (scripts/1530-arg-mutation-probe.mjs) falsified the plugin/
// serializer race: the escapes exist only in JSON SOURCE TEXT, so
// the corruption predates serialization. Both extracted ids must be
// well-formed session ids; anything else is NOT the shape and
// returns ok=false (the caller keeps the original value and its
// normal error path — the guard never mangles clean input).
const dupFragmentMark = `","lsp_injected_session":"`

// sesIDPattern is a PREFIX-RESTRICTED SUBSET of the adapter seam's
// agent-id grammar (pkg/agent/opencode loopback.go
// sessionIDPattern — #1364's deliberate widening admits hyphens; the
// seam itself has NO ses_ requirement, so this pattern covers
// strictly less): the ses_ prefix plus the seam's charset and 1-128
// TOTAL-length discipline (the {1,124} bound is 128 minus the
// 4-char ses_ prefix). A non-ses_-prefixed leading id falls to the
// documented lossy error path (fail-safe: never misdelivers; all
// live ids are ses_-prefixed). Diverging from the seam charset
// would silently shrink recovery coverage relative to ids the
// platform itself validates (r3: a hyphenated leading id failed the
// shape and fell back to the lossy pre-fix path). The recovered
// target is re-validated by SessionExists
// immediately after; the embedded origin is cosmetic, untrusted
// data — the pattern exists to reject shapes that merely CONTAIN
// the fragment mark by accident, not to second-guess the id format.
var sesIDPattern = regexp.MustCompile(`^ses_[A-Za-z0-9_-]{1,124}$`)

func splitDuplicatedArgFragment(v string) (target, embeddedOrigin string, ok bool) {
	i := strings.Index(v, dupFragmentMark)
	if i <= 0 {
		return "", "", false
	}
	target = v[:i]
	if !recoverableRefForm(target) {
		return "", "", false
	}
	rest := strings.TrimSuffix(strings.TrimSuffix(v[i+len(dupFragmentMark):], "}"), `"`)
	if !recoverableRefForm(rest) {
		return "", "", false
	}
	return target, rest, true
}

// recoverableRefForm is the #1530 recovery's ref acceptance: a
// harness-id or a 3-word phrase (#1816 — phrases are the canonical
// address now, and the emission slip follows the address form the
// caller sees in context; the recovery must keep working for them).
func recoverableRefForm(v string) bool {
	return sesIDPattern.MatchString(v) || shortid.IsShortIDForm(v)
}

// mcpSendMessage delivers a text message to another session in this
// workspace, fire-and-forget: the reply (if any) stays in the target
// session — nothing returns to the caller (the same philosophy as
// create_session; the task tool is the blocking/returns-result path).
//
// Origin attribution (#1465, #1469; #1849 tightening): the origin is
// ALWAYS attributed and platform-attested. ONE source:
//
//   - INJECTED: the platform's opencode plugin (tool.execute.before)
//     stamps lsp_injected_session with the harness's own session
//     identity — the model never supplies it and the schema does not
//     advertise it. Sentinel/result mode: "injected".
//
// The former SELF-DECLARED fallback (from_session_id) is removed: the
// agent is never responsible for supplying its own return address, and
// the extra long-id argument was an emission-corruption surface (#1849
// factorial — send_message corrupted 5/5 with the long prose key
// opening the object; every added key is another resume-after-string
// seam). Absent injection (plugin missing/drifted — a degraded pod) →
// tool error, no delivery, report-the-platform.
// The origin validates by-ID (SessionExists — immune to the #1452
// list-visibility gap). The sentinel is provenance metadata, NOT
// authentication — every session shares the pod credential, so the
// origin is a return address; the mode makes the degree of platform
// attestation visible, nothing more.
//
// Busy targets queue the message server-side and deliver it when their
// current turn ends (live-proven run-at-boundary semantics — the same
// POST shape that powers compact's scheduling), so delivery is detached
// either way and the tool reports which case applies.
//
// #1530 emission-duplication recovery: a session_id carrying the
// model-emission duplication shape (leading TARGET id + pasted
// ","lsp_injected_session":"ORIGIN"} JSON tail — 13 live instances,
// root-caused to model emission, NOT a plugin/serializer race; probes
// in scripts/1530-arg-mutation-probe.mjs) is recovered to its leading
// id, delivered, and warned — never silently lost to a resolution
// error. See splitDuplicatedArgFragment for the exact shape contract.
func mcpSendMessage(ctx context.Context, password, sessionID, message, injectedSession string) (string, error) {
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" {
		return "", fmt.Errorf("destination is required")
	}
	// #1530 emission-duplication recovery: a session_id carrying the
	// duplicated-argument fragment (model emission slip — see
	// splitDuplicatedArgFragment) still encodes the intended target
	// in its leading id. Recover it and deliver; the result warns
	// loudly instead of losing the message to a resolution error
	// (the 13-instance misfire class — messages were lost though the
	// intent was mechanically recoverable).
	recoveredTarget, embeddedOrigin, recovered := splitDuplicatedArgFragment(sessionID)
	if recovered {
		sessionID = recoveredTarget
	}
	message = strings.TrimSpace(message)
	if message == "" {
		return "", fmt.Errorf("message is required")
	}
	// #1814: placeholder-class auto-reject. The mid-write emission
	// corruption mode replaces the drafted body with a literal
	// placeholder token as the call fires — the sender rarely
	// notices, and every such delivery costs the recipient a turn to
	// diagnose. NARROW EXACT-MATCH set only (trimmed, case-folded):
	// "ok"/"x"/probe traffic and sentences merely containing a
	// placeholder word pass untouched — this is not a length
	// heuristic. Rejection happens BEFORE any delivery.
	if isPlaceholderClassPayload(message) {
		return "", fmt.Errorf("message rejected: placeholder-class payload (%q) — your drafted text was likely replaced by a placeholder token mid-write (a known emission-corruption mode). Re-read your drafted message; if emission corruption ate it, re-compose the intended content and resend. Nothing was delivered", message)
	}
	origin, mode := "", ""
	if strings.TrimSpace(injectedSession) != "" {
		origin, mode = strings.TrimSpace(injectedSession), agentmessage.ModeInjected
	} else {
		// #1849 tightening: the from_session_id self-declared fallback
		// is removed — the agent is never responsible for its own
		// return address. Absent injection means the pod's opencode
		// plugin is missing or drifted (degraded); refuse delivery and
		// say so instead of accepting an unattested origin.
		return "", fmt.Errorf("no origin: the platform could not inject your session ID — the workspace's opencode plugin is absent or outdated (degraded pod). Nothing was delivered; report the missing plugin to the platform")
	}

	client := seamClientWithPassword(password)

	// Design 0066: BOTH addresses resolve first — target and origin
	// accept short ids and unambiguous aliases; legacy ids pass
	// through byte-identical (the #1530 recovery above stays on the
	// raw id, which resolution then confirms or leaves alone).
	sendOrigRef := sessionID // pre-resolution target, for the §2 note
	for _, pair := range []struct{ name, ref string }{{"session_id", sessionID}, {"origin", origin}} {
		resolved, rerr := resolveSessionRef(ctx, password, pair.ref)
		if rerr != nil {
			return "", fmt.Errorf("%s: %w", pair.name, rerr)
		}
		if pair.name == "session_id" {
			sessionID = resolved
		} else {
			origin = resolved
		}
	}

	// Validate BOTH ends by ID (unknown IDs fail loudly here rather
	// than silently in the detached delivery; the by-ID probe is
	// immune to the #1452 list-visibility gap). A not-found id gets a
	// near-miss did-you-mean clause (#1663: the zombie-session hour —
	// a single-dropped-char id met a bare "not found"); suggest-only,
	// never an auto-correct — the caller re-issues with the exact id.
	for _, id := range []string{sessionID, origin} {
		exists, err := client.SessionExists(ctx, id)
		if err != nil {
			return "", fmt.Errorf("failed to resolve session %s: %w", emitID(id), err)
		}
		if !exists {
			// #1816: the visible id is caller-facing (phrase when
			// known); the suggest clause still RANKS on the canonical
			// id (#1806 f8 discipline).
			return "", fmt.Errorf("session %s not found in this workspace (IDs from session_list)%s",
				emitID(id), sessionSuggestClause(ctx, client, id))
		}
	}

	// Stamp the origin sentinel (idempotent: a payload that already
	// carries a leading sentinel from a prior hop is stripped first).
	// #1919: the sentinel's fromSession is AGENT- and USER-facing (the
	// recipient's transcript, the frontend origin badge) — it emits the
	// PHRASE when the replica knows it, exactly like every other id
	// emission in this result (#1756 §1); the raw harness id survives
	// only in the degraded cold-replica window, where it is still the
	// caller's only addressable form. Validation above stays canonical;
	// a phrase-valued fromSession remains a valid reply address
	// (resolveSessionRef accepts short ids).
	composed, err := agentmessage.Compose(message, agentmessage.Origin{FromSession: emitID(origin), Mode: mode})
	if err != nil {
		return "", fmt.Errorf("failed to stamp message origin: %w", err)
	}

	// #1525 self-send guard: target == origin delivers silently
	// otherwise (the message boomerangs as the caller's next turn) —
	// the orchestrator's 11-misfire class. WARNING, never a refusal
	// (legitimate self-notes exist per the issue's ruling); the result
	// carries a loud warning field plus a plain-language line so the
	// misaddress is visible at SEND time, not after echo-inspection.
	//
	// #1530 composes into the same field: a recovered emission-
	// duplication send (and its embedded-origin mismatch, if any)
	// appends its own warning so every anomaly is visible at send
	// time while normal sends stay warning-free.
	var warnParts []string
	if sessionID == origin {
		warnParts = append(warnParts,
			"target is your own session — this message will come back to you as your next turn, not reach another agent. If you meant a different session, re-check the ID (session_list / session_metadata); if this is an intentional self-note, carry on.")
	}
	if recovered {
		// #1816 emission closure: the warning's ids are caller-facing
		// text — phrases when the replica knows them (parity with the
		// session_id/origin fields emitted below), never raw harness
		// ids on the warm path.
		warn := "session_id carried a duplicated-argument fragment (emission slip): recovered target " + emitID(sessionID) +
			" from the leading id and delivered there — re-check the ID you pass (session_list / session_metadata)."
		// r2 f1: the mismatch verdict compares CANONICAL-to-CANONICAL —
		// a fragment whose embedded origin (any id space: phrase or
		// harness) resolves to the SAME session as the resolved origin
		// is NOT a mismatch; comparing the raw form against the
		// resolved canonical emitted the false "X differs from X"
		// clause.
		embeddedCanonical := embeddedOrigin
		if embeddedOrigin != origin {
			if embeddedResolved, rerr := resolveSessionRef(ctx, password, embeddedOrigin); rerr == nil {
				embeddedCanonical = embeddedResolved
			}
		}
		if embeddedCanonical != origin {
			warn += " The fragment's embedded origin " + emitID(embeddedOrigin) + " differs from the resolved origin " + emitID(origin) + " — treated as cosmetic (the embedded copy is not trusted; the resolved origin wins attribution)."
		}
		warnParts = append(warnParts, warn)
	}

	busy, err := client.GetSessionStatuses(ctx)
	if err != nil {
		return "", fmt.Errorf("failed to read session status: %w", err)
	}

	// Advisory label only (TOCTOU: the status may flip between read and
	// POST) — delivery correctness never depends on it; the POST itself
	// queues server-side either way. "retry" targets (backing off after
	// stream errors) behave like busy: the POST waits for the retrying
	// turn to settle — same reporting bucket, matching how the rest of
	// the codebase treats retry-as-busy.
	status := "delivering"
	switch busy[sessionID] {
	case "busy", "retry":
		status = "delivering_after_current_turn"
	}

	// Detached delivery: WithoutCancel so it survives the tool response;
	// server-side queuing handles busy targets. Delivery is not retried
	// (same loss semantics as create_session — stated in the description).
	// The logger is captured at spawn: this goroutine routinely outlives
	// the tool call, and reading the package global later races with
	// tests that swap it (the withObservedLog guard's documented
	// constraint — found by CI's race detector, PR #1382 r5).
	logger := log
	go func() {
		if _, err := client.SessionSend(context.WithoutCancel(ctx), sessionID, composed, "", nil); err != nil {
			logger.Warn("send_message: background delivery failed",
				zap.String("sessionID", sessionID), zap.Error(err))
		}
	}()

	// #1756 §1: origin is a canonical harness id post-resolution —
	// emit the phrase when the replica knows it, flagged when cold
	// (parity with session_id; found by the pre-push adversarial
	// verify — the raw id was the last unflagged full-id leak).
	originID, originDegraded := emitIDFields(origin)
	result := map[string]string{
		"status":      status,
		"session_id":  emitID(sessionID),
		"origin":      originID,
		"origin_mode": mode,
	}
	if len(warnParts) > 0 {
		result["warning"] = strings.Join(warnParts, " ")
	}
	// #1756 §1/§2 parity with rename/archive/delete/abort: the id is
	// the phrase (flagged when the replica is cold) and a deprecated
	// input form gets the notice.
	if _, degraded := emitIDFields(sessionID); degraded {
		result["session_id_deprecated"] = "true"
	}
	if originDegraded {
		result["origin_deprecated"] = "true"
	}
	if note := strings.TrimSpace(deprecationNote(sendOrigRef)); note != "" {
		result["deprecation"] = note
	}
	out, _ := json.Marshal(result)
	return string(out), nil
}

// --- session subscriptions (idle-subscriptions v1, design §5) --------------

// sessionNotifierAtomic holds the process-wide subscription engine
// (the sessionShortIDsAtomic precedent). The MCP handlers reach the
// engine through currentSessionNotifier(); task 4's main wiring swaps
// in the fully-wired engine (authority stream + facts ticker + the
// V1-POST deliverer) via setSessionNotifier; tests swap in isolated
// stores. The default carries the production registry store and the
// ListSessions-backed lineage facts so the surface is functional the
// moment the process boots — edges registered before the stream
// wiring lands are durable in the store and start firing once the
// consumer runs.
var sessionNotifierAtomic atomic.Value // *sessionNotifier

func init() {
	sessionNotifierAtomic.Store(newSessionNotifier(sessionNotifierConfig{
		Store: newSessionSubscriptionStore(defaultSessionSubscriptionsPath),
		Facts: newHarnessSessionFacts(),
	}))
}

func currentSessionNotifier() *sessionNotifier {
	if v, ok := sessionNotifierAtomic.Load().(*sessionNotifier); ok {
		return v
	}
	return nil
}

// setSessionNotifier swaps the engine (main wiring + tests).
func setSessionNotifier(n *sessionNotifier) { sessionNotifierAtomic.Store(n) }

// subscriptionRowsFor is the nil-safe registry read behind
// list_subscriptions (the engine's store; nil engine ⇒ empty).
func subscriptionRowsFor(subscriber string) []SubscriptionRow {
	if n := currentSessionNotifier(); n != nil && n.store != nil {
		return n.store.rowsForSubscriber(subscriber)
	}
	return nil
}

// resolveSubscriptionCaller resolves the CALLER (subscriber) identity
// for the subscription tools: lsp_injected_session ONLY — the #1849
// send_message posture (refuse an unattested origin; the agent is
// never responsible for its own identity). The resolveCallerSession
// degraded-pod fallback (single-busy-session heuristic,
// mcp_tools.go's delete tools) is deliberately NOT used here: a wrong
// guess registers a wrong durable edge, a silent failure the caller
// cannot see. The injected ref resolves through the same §4.4
// precedence as every other address and is validated by ID against
// the harness (the by-ID probe is immune to the #1452 list gap).
func resolveSubscriptionCaller(ctx context.Context, password, injectedSession string) (string, error) {
	self := strings.TrimSpace(injectedSession)
	if self == "" {
		return "", fmt.Errorf("no caller identity: the platform could not inject your session ID — the workspace's opencode plugin is absent or outdated (degraded pod). Nothing was subscribed; report the missing plugin to the platform")
	}
	resolved, err := resolveSessionRef(ctx, password, self)
	if err != nil {
		return "", fmt.Errorf("caller identity: %w", err)
	}
	client := seamClientWithPassword(password)
	exists, err := client.SessionExists(ctx, resolved)
	if err != nil {
		return "", fmt.Errorf("failed to resolve caller session %s: %w", emitID(resolved), err)
	}
	if !exists {
		return "", fmt.Errorf("caller session %s not found in this workspace%s",
			emitID(resolved), sessionSuggestClause(ctx, client, resolved))
	}
	return resolved, nil
}

// subscriptionLineage pulls the raw wire session list — the ONLY
// surface carrying parentID (investigation C1; the authority
// projection has no parent field) — and returns the id→parent map
// ("" = top-level). The pull also folds into the engine's lineage
// facts when they are the ListSessions-backed kind, so the notifier's
// own registration/notify-time subtask guards (§4.6
// belt-and-suspenders) see the same data.
func subscriptionLineage(ctx context.Context, client *opencode.Client) (map[string]string, error) {
	sessions, err := pullHarnessSessionList(ctx, client)
	if err != nil {
		return nil, err
	}
	if n := currentSessionNotifier(); n != nil {
		if hf, ok := n.facts.(*harnessSessionFacts); ok {
			hf.Load(sessions)
		}
	}
	parents := make(map[string]string, len(sessions))
	for i := range sessions {
		if sessions[i].ID != "" {
			parents[sessions[i].ID] = sessions[i].ParentID
		}
	}
	return parents, nil
}

// mcpSubscribeSession registers caller→target — "watch this session,
// wake me when its turn completes" (design §5). Order: fast-fail
// event validation (no seam traffic), caller identity (#1849),
// target resolution + existence (did-you-mean on miss), lineage both
// sides (§4.6), then the engine's §4.1 guards (self/cycle/cap) and
// the durable write. Every refusal is loud; nothing is guessed.
// subscriptionEventsFromTopics is the design 0072 topics alias: the
// event-bus vocabulary meeting the v1 registry. Each topic must be
// session.{id}.{idle|error|*} addressing THIS broadcaster (any id
// form — the comparison is post-resolution, so a caller may spell
// the id either way); "*" expands to both events. Anything else —
// lease.*, task.*, a foreign session's topics — is a loud refusal
// (arbitrary pattern subscription is a future subscribe_events tool,
// not an overload of this one). Returns the topic-derived event set
// for union with `events`.
func subscriptionEventsFromTopics(topics []string, broadcaster string) ([]string, error) {
	var out []string
	for _, raw := range topics {
		topic := strings.TrimSpace(raw)
		parts := strings.Split(topic, ".")
		if len(parts) != 3 || parts[0] != "session" || parts[2] == "" {
			return nil, fmt.Errorf("topics entries must be session.<id>.idle, session.<id>.error, or session.<id>.* — got %q (other topic families are not watchable here)", raw)
		}
		seg := strings.TrimSpace(parts[1])
		if seg != "*" {
			if resolved := canonicalNotifyDestination(seg); resolved != broadcaster && seg != broadcaster {
				return nil, fmt.Errorf("topic %q addresses a different session than %s", raw, emitID(broadcaster))
			}
		}
		switch parts[2] {
		case subscriptionEventIdle, subscriptionEventError:
			out = append(out, parts[2])
		case "*":
			out = append(out, subscriptionEventIdle, subscriptionEventError)
		default:
			return nil, fmt.Errorf("topic %q settles on %q — only idle, error, or * are watchable", raw, parts[2])
		}
	}
	return out, nil
}

func mcpSubscribeSession(ctx context.Context, password, targetRef string, events []string, topics []string, injectedSession string) (string, error) {
	targetRef = strings.TrimSpace(targetRef)
	if targetRef == "" {
		return "", fmt.Errorf("session_id is required")
	}
	// Fast-fail the closed vocabulary before any seam round trip.
	if _, err := validateSubscriptionEvents(events); err != nil {
		return "", err
	}
	caller, err := resolveSubscriptionCaller(ctx, password, injectedSession)
	if err != nil {
		return "", err
	}
	client := seamClientWithPassword(password)
	target, rerr := resolveSessionRef(ctx, password, targetRef)
	if rerr != nil {
		return "", fmt.Errorf("session_id: %w", rerr)
	}
	exists, err := client.SessionExists(ctx, target)
	if err != nil {
		return "", fmt.Errorf("failed to resolve session %s: %w", emitID(target), err)
	}
	if !exists {
		// #1663: a not-found id gets the near-miss did-you-mean clause;
		// suggest-only, never an auto-correct.
		return "", fmt.Errorf("session %s not found in this workspace (IDs from session_list)%s",
			emitID(target), sessionSuggestClause(ctx, client, target))
	}
	// The design 0072 topics alias: validate against the RESOLVED
	// broadcaster (any id spelling the caller used), then union into
	// the v1 event set — one registry row, two vocabularies.
	if len(topics) > 0 {
		derived, terr := subscriptionEventsFromTopics(topics, target)
		if terr != nil {
			return "", terr
		}
		events = append(events, derived...)
		if len(events) == 0 {
			events = nil // sanitize to the validator's default-both
		}
		if _, verr := validateSubscriptionEvents(events); verr != nil {
			return "", verr
		}
	}
	// §4.6 subtask exclusion, BOTH sides, at registration: the parent
	// receives subtask results synchronously through the task tool
	// (broadcaster side is pure noise); subtasks return and terminate
	// (subscriber side never takes wake-up turns).
	parents, lerr := subscriptionLineage(ctx, client)
	if lerr != nil {
		return "", fmt.Errorf("could not verify session lineage: %w", lerr)
	}
	if parents[target] != "" {
		return "", fmt.Errorf("%w: broadcaster %s is a subtask (its parent receives its result synchronously through the task tool)",
			errNotifySubtaskSide, emitID(target))
	}
	if parents[caller] != "" {
		return "", fmt.Errorf("%w: subscriber %s is a subtask (subagents return to their parent, never take wake-up turns)",
			errNotifySubtaskSide, emitID(caller))
	}
	row, serr := currentSessionNotifier().Subscribe(target, caller, events)
	if serr != nil {
		return "", serr
	}
	if row == nil {
		return "", fmt.Errorf("subscription registry unavailable — nothing was subscribed")
	}
	// #1756 §1: the identifier output IS the phrase (flagged when the
	// replica is cold); a deprecated input form gets the notice.
	id, degraded := emitIDFields(target)
	result := map[string]any{
		"status":     "subscribed",
		"session_id": id,
		"events":     row.Events,
	}
	if degraded {
		result["session_id_deprecated"] = "true"
	}
	if note := strings.TrimSpace(deprecationNote(targetRef)); note != "" {
		result["deprecation"] = note
	}
	out, _ := json.Marshal(result)
	return string(out), nil
}

// mcpUnsubscribeSession removes the caller's watch edge on the target
// — idempotent by contract (absent edge ⇒ not_subscribed, never an
// error). No existence probe: removing an edge for a vanished session
// IS the lazy-cleanup path; resolution failures still refuse loudly.
func mcpUnsubscribeSession(ctx context.Context, password, targetRef, injectedSession string) (string, error) {
	targetRef = strings.TrimSpace(targetRef)
	if targetRef == "" {
		return "", fmt.Errorf("session_id is required")
	}
	caller, err := resolveSubscriptionCaller(ctx, password, injectedSession)
	if err != nil {
		return "", err
	}
	target, rerr := resolveSessionRef(ctx, password, targetRef)
	if rerr != nil {
		return "", fmt.Errorf("session_id: %w", rerr)
	}
	status := "unsubscribed"
	if !currentSessionNotifier().Unsubscribe(target, caller) {
		status = "not_subscribed"
	}
	// #1756 §1 parity with subscribe.
	id, degraded := emitIDFields(target)
	result := map[string]any{
		"status":     status,
		"session_id": id,
	}
	if degraded {
		result["session_id_deprecated"] = "true"
	}
	if note := strings.TrimSpace(deprecationNote(targetRef)); note != "" {
		result["deprecation"] = note
	}
	out, _ := json.Marshal(result)
	return string(out), nil
}

// mcpListSubscriptions lists outbound watch edges: the caller's by
// default (caller identity required — #1849 posture), or the named
// session's when session_id is passed (explicit target, no caller
// identity needed). Phrases emitted, never raw harness ids on the
// warm path (#1756 §1).
func mcpListSubscriptions(ctx context.Context, password, targetRef, injectedSession string) (string, error) {
	owner := ""
	if ref := strings.TrimSpace(targetRef); ref != "" {
		resolved, rerr := resolveSessionRef(ctx, password, ref)
		if rerr != nil {
			return "", fmt.Errorf("session_id: %w", rerr)
		}
		owner = resolved
	} else {
		caller, err := resolveSubscriptionCaller(ctx, password, injectedSession)
		if err != nil {
			return "", err
		}
		owner = caller
	}
	rows := subscriptionRowsFor(owner)
	type subRow struct {
		SessionID string   `json:"session_id"`
		Events    []string `json:"events"`
		CreatedAt string   `json:"created_at"`
	}
	outRows := make([]subRow, 0, len(rows))
	for _, row := range rows {
		outRows = append(outRows, subRow{
			SessionID: emitID(row.Broadcaster),
			Events:    row.Events,
			CreatedAt: row.CreatedAt.UTC().Format(time.RFC3339),
		})
	}
	// nil Events marshals as null — emit [] not null for an empty set.
	for i := range outRows {
		if outRows[i].Events == nil {
			outRows[i].Events = []string{}
		}
	}
	payload, _ := json.Marshal(map[string]any{
		"count":         len(outRows),
		"subscriptions": outRows,
	})
	return string(payload), nil
}

// subscribeCallerToCreatedSession is create_session's §5 subscribe
// leg: register caller→child after creation + short-id registration.
// The flag NEVER gates creation (creation never needed an origin), so
// every refusal here degrades to (false, warning) — the loud-but-
// non-fatal posture — instead of failing the create. The child is
// top-level by construction, so only the CALLER's side needs the
// subagent check (§4.6).
func subscribeCallerToCreatedSession(ctx context.Context, password string, client *opencode.Client, childID, injectedSession string) (bool, string) {
	self := strings.TrimSpace(injectedSession)
	if self == "" {
		return false, "subscribe requested but no caller identity: the platform could not inject your session ID (degraded pod) — the session was created; subscribe explicitly with subscribe_session once the plugin is back"
	}
	caller, err := resolveSessionRef(ctx, password, self)
	if err != nil {
		return false, fmt.Sprintf("subscribe skipped: caller identity did not resolve: %v", err)
	}
	exists, err := client.SessionExists(ctx, caller)
	if err != nil || !exists {
		return false, "subscribe skipped: caller identity could not be verified against the harness (degraded pod?) — the session was created; subscribe explicitly with subscribe_session"
	}
	parents, lerr := subscriptionLineage(ctx, client)
	if lerr != nil {
		return false, "subscribe skipped: could not verify caller lineage: " + lerr.Error()
	}
	if parents[caller] != "" {
		return false, "subscribe flag ignored: the caller is a subagent session (parent set) — subagents return to their parent and never take wake-up turns; the new session was created, but you are not subscribed to it"
	}
	if _, serr := currentSessionNotifier().Subscribe(childID, caller, nil); serr != nil {
		return false, "subscribe failed: " + serr.Error()
	}
	return true, ""
}

// --- abort_session --------------------------------------------------------

// mcpAbortSession stops a session's current turn via the consolidated
// V1 abort (Client.Abort — the same method the API proxy's interrupt
// path uses). History survives; the in-flight turn is cut DESTRUCTIVELY
// — any queued-but-undelivered message may be dropped (the description
// tells the agent to re-send). Aborting an idle session is a no-op.
func mcpAbortSession(ctx context.Context, password, sessionID string) (string, error) {
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" {
		return "", fmt.Errorf("sessionId is required")
	}
	// Design 0066: resolve the ref first (abort is destructive — an
	// ambiguous alias REFUSES here, never guesses).
	origRef := sessionID
	resolved, rerr := resolveSessionRef(ctx, password, sessionID)
	if rerr != nil {
		return "", rerr
	}
	sessionID = resolved
	client := seamClientWithPassword(password)
	if err := client.Abort(ctx, sessionID); err != nil {
		if isSessionNotFoundErr(err) {
			// #1663: aborting a mistyped id silently no-ops into a
			// mystery — the clause names the live id instead. Still a
			// refusal: abort is destructive and never acts on a guess.
			return "", fmt.Errorf("failed to abort session: %w%s", err, sessionSuggestClause(ctx, client, sessionID))
		}
		return "", fmt.Errorf("failed to abort session: %w", err)
	}
	id, idDegraded := emitIDFields(sessionID)
	res := map[string]string{
		"status":     "aborted",
		"session_id": id,
	}
	if idDegraded {
		res["session_id_deprecated"] = "true"
	}
	// #1756 §2: the deprecation note rides ONLY deprecated-form
	// inputs — never an empty-string field on clean refs (r3 f4).
	if note := strings.TrimSpace(deprecationNote(origRef)); note != "" {
		res["deprecation"] = note
	}
	out, _ := json.Marshal(res)
	return string(out), nil
}

// --- automation: triggers + workflows --------------------------------------

// automationDeps resolves the SA token + workspace id from pod env (the
// /internal/v1/automation surface's credentials — same bootstrap token
// as rename_workspace).
func automationDeps() (saToken, apiURL, workspaceID string, err error) {
	workspaceID = strings.TrimSpace(os.Getenv("WORKSPACE_ID"))
	if workspaceID == "" {
		return "", "", "", fmt.Errorf("automation unavailable: WORKSPACE_ID is not set in this pod")
	}
	apiURL = strings.TrimSpace(os.Getenv("LLMSAFESPACE_API_URL"))
	if apiURL == "" {
		return "", "", "", fmt.Errorf("automation unavailable: LLMSAFESPACE_API_URL is not set in this pod")
	}
	raw, err := os.ReadFile(bootstrapTokenPathFromEnv())
	if err != nil {
		return "", "", "", fmt.Errorf("automation unavailable: pod SA token unreadable: %w", err)
	}
	return strings.TrimSpace(string(raw)), strings.TrimSuffix(apiURL, "/"), workspaceID, nil
}

// mcpAutomation executes one automation seam call and wraps the result
// for the tool surface. Body/patch JSON is passed through verbatim —
// the platform validates and its error text names invalid fields.
func mcpAutomation(ctx context.Context, password, name string, body map[string]any) (string, error) {
	saToken, apiURL, workspaceID, err := automationDeps()
	if err != nil {
		return "", err
	}
	client := opencode.NewLoopbackClient(apiURL, "")
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	var res *opencode.AutomationResponse
	switch name {
	case "trigger_list":
		res, err = client.TriggerList(ctx, saToken, workspaceID)
	case "trigger_create":
		res, err = client.TriggerCreate(ctx, saToken, workspaceID, passThroughBody(body, "trigger"))
	case "trigger_update":
		res, err = client.TriggerUpdate(ctx, saToken, workspaceID, toolArgID(body), patchWithoutID(body))
	case "trigger_delete":
		res, err = client.TriggerDelete(ctx, saToken, workspaceID, toolArgID(body))
	case "trigger_fires":
		res, err = client.TriggerFires(ctx, saToken, workspaceID, toolArgID(body))
	case "trigger_rotate_webhook_secret":
		res, err = client.TriggerRotateWebhookSecret(ctx, saToken, workspaceID, toolArgID(body))
	case "workflow_list":
		res, err = client.WorkflowList(ctx, saToken, workspaceID)
	case "workflow_create":
		res, err = client.WorkflowCreate(ctx, saToken, workspaceID, passThroughBody(body, "workflow"))
	case "workflow_update":
		res, err = client.WorkflowUpdate(ctx, saToken, workspaceID, toolArgID(body), patchWithoutID(body))
	case "workflow_delete":
		res, err = client.WorkflowDelete(ctx, saToken, workspaceID, toolArgID(body))
	case "workflow_run":
		var input json.RawMessage
		if raw, ok := body["input"]; ok {
			b, _ := json.Marshal(raw)
			input = b
		}
		res, err = client.WorkflowRun(ctx, saToken, workspaceID, toolArgID(body), input)
	case "workflow_runs":
		res, err = client.WorkflowRuns(ctx, saToken, workspaceID, toolArgID(body))
	case "board_read":
		// board_read's v1 shape is the item form (GetTaskInWorkspace):
		// the worker's own task by shortId/UUID. The list/search/health
		// projections ride the engine follow-ups (#1699) and grow INTO
		// this tool when their seams land.
		res, err = client.BoardTaskGet(ctx, saToken, workspaceID, boardTaskRef(body))
	case "board_heartbeat":
		if err := requireBoardClaimToken(name, body); err != nil {
			return "", err
		}
		res, err = client.BoardHeartbeat(ctx, saToken, workspaceID, passThroughBody(body, "task"))
	case "board_report_throttle":
		if err := requireBoardClaimToken(name, body); err != nil {
			return "", err
		}
		res, err = client.BoardReportThrottle(ctx, saToken, workspaceID, passThroughBody(body, "task"))
	case "board_record_verdict":
		// #1947 — the verdict channel: the spawned reviewer persona's
		// ONE authorized write. Worker projection only: the claim token
		// is REQUIRED (the service-side agent guard refuses a missing or
		// wrong token), so unlike board_move_stage there is no
		// token-less manager arm on this tool.
		if err := requireBoardClaimToken(name, body); err != nil {
			return "", err
		}
		res, err = client.BoardRecordVerdict(ctx, saToken, workspaceID, passThroughBody(body, "task"))
	case "board_move_stage":
		// #1832 WS2 dual-route: a caller holding a claim token rides
		// the worker projection (unchanged); a token-less caller rides
		// the manager-grade path — open to ANY in-workspace session,
		// spawned workers included (the #1747 §5 manager gate is
		// repealed, ruling 2026-10-27 / tsk-copper-refectory-generates;
		// the claim-token fence remains the real in-flight protection).
		if tok, _ := boardToolArgs(body)["claimToken"].(string); strings.TrimSpace(tok) != "" {
			res, err = client.BoardMoveStage(ctx, saToken, workspaceID, passThroughBody(body, "task"))
		} else {
			res, err = dispatchBoardManagerTool(ctx, client, password, name, body, saToken, workspaceID)
		}
	case "board_update_story":
		// #1946 dual-route (the reviewer-persona criteria grant): a
		// claim-token-carrying call rides the worker projection's
		// scoped set-criteria-met route — the reviewer persona records
		// its criterion flags on its OWN claimed row (persona
		// provenance arbitrated at the engine; an implementer's
		// token-carrying call refuses there — no persona claim). A
		// token-less caller rides the manager path, open to any
		// in-workspace session (the #1747 §5 gate is repealed, ruling
		// 2026-10-27 / tsk-copper-refectory-generates), EXCEPT the
		// self-certification pin inside dispatch (post-file-merge, so
		// every met-flag-writing shape is watched — inline
		// setCriteriaMet, patchFile criteria replace, and the create
		// shapes' pre-met payloads; #1946's grant does not widen).
		if tok, _ := boardToolArgs(body)["claimToken"].(string); strings.TrimSpace(tok) != "" {
			res, err = client.BoardSetCriteriaMet(ctx, saToken, workspaceID, passThroughBody(body, "task"))
		} else {
			res, err = dispatchBoardManagerTool(ctx, client, password, name, body, saToken, workspaceID)
		}

	// --- Task Board MANAGER tools (#1747): the full manager set at
	// project/epic/task level, workspace-scoped by the pod identity,
	// open to ANY in-workspace session (the #1747 §5 manager grant is
	// repealed, ruling 2026-10-27 / tsk-copper-refectory-generates —
	// the resolved session identity rides the wire as audit
	// attribution only). #1837 P2a: board_list/board_health join the
	// manager set (the workspace-scoped overview reads).
	case "board_create_project", "board_update_project",
		"board_project_archive", "board_project_unarchive", "board_project_pause", "board_project_resume",
		"board_create_epic", "board_update_epic",
		"board_epic_activate", "board_epic_pause", "board_epic_archive", "board_epic_unarchive",
		"board_delete_epic",
		"board_create_story",
		"board_add_dependency", "board_remove_dependency",
		"board_delete_story", "board_pause_story", "board_resume_story",
		"board_rearm_story",
		"board_move_stage_bulk",
		"board_delete_stories",
		"board_import_plan",
		"board_list", "board_health",
		"board_get_config", "board_set_config":
		res, err = dispatchBoardManagerTool(ctx, client, password, name, body, saToken, workspaceID)
	default:
		return "", fmt.Errorf("unknown automation tool: %s", name)
	}
	if err != nil {
		return "", fmt.Errorf("%s failed: %w", name, err)
	}
	if res.Body == "" {
		return fmt.Sprintf(`{"status":%d}`, res.Status), nil
	}
	return res.Body, nil
}

// passThroughBody marshals the caller's object argument (nested under
// key, flat form tolerated) for verbatim platform submission; an empty
// argument stays an empty object. The tool schema documents the nested
// form; the flat tolerance keeps the wire contract forgiving.
func passThroughBody(body map[string]any, key string) json.RawMessage {
	if nested, ok := body[key].(map[string]any); ok {
		body = nested
	}
	out, err := json.Marshal(body)
	if err != nil {
		return json.RawMessage("{}")
	}
	return out
}

// toolArgID extracts the tool argument "id" as a validated string.
func toolArgID(body map[string]any) string {
	id, _ := body["id"].(string)
	return id
}

// --- board worker tools (#1701, design 0067 §4.3c) ---------------------------

// boardToolArgs unwraps the documented flat argument form, tolerating
// the nested {"task": {...}} wrapper (passThroughBody's convention).
func boardToolArgs(body map[string]any) map[string]any {
	if nested, ok := body["task"].(map[string]any); ok {
		return nested
	}
	return body
}

// boardTaskRef extracts the task reference for board_read's path:
// taskId (UUID) preferred, shortId (tsk-…) fallback — the service's
// own pick order. An empty ref reaches validateBoardRef, which refuses
// it pre-wire.
func boardTaskRef(body map[string]any) string {
	args := boardToolArgs(body)
	if id, _ := args["taskId"].(string); strings.TrimSpace(id) != "" {
		return strings.TrimSpace(id)
	}
	sid, _ := args["shortId"].(string)
	return strings.TrimSpace(sid)
}

// requireBoardClaimToken enforces the claim-token discipline pre-wire:
// every in-flight stage/lease write (heartbeat, throttle, move_stage)
// carries the token the fresh worker received in its charter envelope.
// The platform re-arbitrates the token against the live row (a stale
// token 409s there — the authoritative guard); this is the shape
// refusal, the session_archive-boolean discipline: a token-less call
// never spends a wire round trip.
func requireBoardClaimToken(name string, body map[string]any) error {
	tok, _ := boardToolArgs(body)["claimToken"].(string)
	if strings.TrimSpace(tok) == "" {
		return fmt.Errorf("%s failed: claimToken is required — the in-flight guard from your charter envelope (reads need no token; every stage/lease write does)", name)
	}
	return nil
}

// patchWithoutID unwraps the tool's "patch" argument (routing and scoping
// fields stripped) — the id rides the URL, workspaceID the query, and the
// wire body carries caller content only.
func patchWithoutID(body map[string]any) json.RawMessage {
	patch, ok := body["patch"].(map[string]any)
	if !ok {
		patch = body
	}
	clean := make(map[string]any, len(patch))
	for k, v := range patch {
		if k == "id" || k == "workspaceID" || k == "workspaceId" {
			continue
		}
		clean[k] = v
	}
	out, err := json.Marshal(clean)
	if err != nil {
		return json.RawMessage("{}")
	}
	return out
}

// placeholderClassPayloads is the #1814 exact-match reject set for
// send_message bodies: the literal placeholder tokens the mid-write
// emission-corruption mode substitutes for drafted text (observed
// across worker sessions and the orchestrator), plus the asdf-class
// keyboard-mash single tokens. Deliberately NARROW: short legitimate
// payloads and probe traffic ("ok", "x", "test", "ping") are NOT
// members, and containment never matches — only whole-body equality
// after trim+case-fold rejects.
// isPlaceholderClassPayload reports whether the (already-trimmed)
// message body is ENTIRELY a placeholder-class token. The set lives in
// pkg/board (#1849): send_message and the task-seed guard share ONE
// vocabulary by construction.
func isPlaceholderClassPayload(message string) bool {
	return board.IsPlaceholderClass(message)
}
