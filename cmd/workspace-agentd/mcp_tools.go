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
	"strings"
	"syscall"
	"time"

	"go.uber.org/zap"

	"github.com/lenaxia/llmsafespaces/pkg/agent/opencode"
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
		return "", fmt.Errorf("session_id is required")
	}
	title = strings.TrimSpace(title)
	if title == "" {
		return "", fmt.Errorf("title is required")
	}
	if len(title) > titleMaxLength {
		return "", fmt.Errorf("title exceeds maximum length (%d)", titleMaxLength)
	}
	if err := seamClientWithPassword(password).SessionRename(ctx, sessionID, title); err != nil {
		return "", fmt.Errorf("failed to rename session: %w", err)
	}
	out, _ := json.Marshal(map[string]string{
		"status":     "renamed",
		"session_id": sessionID,
		"title":      title,
	})
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
		return "", err
	}
	imgs, err := loadImages(images)
	if err != nil {
		return "", err
	}

	client := seamClientWithPassword(password)

	// Vision pre-check: when images ride the call, refuse early (and
	// helpfully) when the target model's catalog entry is KNOWN to lack
	// image input (#1307 fail-safe direction: absent capability metadata
	// is unknown, not text-only — refusing on unknown would break image
	// calls on custom vision gateways that carry no metadata).
	if len(imgs) > 0 {
		info, err := client.ModelInfo(ctx, providerID, modelID)
		if err == nil && info.ImageInputKnown && !info.ImageInput {
			return "", fmt.Errorf("model %s does not accept image input per the workspace catalog — pick a vision-capable model", model)
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
		return "", fmt.Errorf("the model returned no text parts")
	}
	out, _ := json.Marshal(map[string]any{
		"model": modelID,
		"text":  res.Text,
	})
	return string(out), nil
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
func mcpCreateSession(ctx context.Context, password, prompt, title string) (string, error) {
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

	go func() {
		if _, err := seamClientWithPassword(password).SessionSend(context.WithoutCancel(ctx), sessionID, prompt, "", nil); err != nil {
			log.Warn("create_session: background prompt delivery failed",
				zap.String("sessionID", sessionID), zap.Error(err))
		}
	}()

	out, _ := json.Marshal(map[string]string{
		"status":        "created",
		"session_id":    sessionID,
		"prompt_status": "delivering_in_background",
	})
	return string(out), nil
}

// --- get_datetime ---------------------------------------------------------

// mcpGetDatetime reports the current instant in UTC and the pod's local
// timezone. Pods default to UTC; TZ may pin otherwise.
func mcpGetDatetime() (string, error) {
	now := time.Now()
	out, _ := json.Marshal(map[string]string{
		"utc":        now.UTC().Format(time.RFC3339),
		"local":      now.Format(time.RFC3339),
		"timezone":   now.Location().String(),
		"utc_offset": now.Format("-07:00"),
	})
	return string(out), nil
}

// --- session_metadata -----------------------------------------------------

// mcpSessionMetadata aggregates read-only workspace + session vitals:
// per-session title/model/age/tokens/message-count/context fill, busy
// flags, and the workspace's own identity.
//
// Security posture: every field is either already exposed by
// session_list/session_read (strictly more sensitive surfaces behind
// the same Basic gate) or is public pod identity (WORKSPACE_ID). No
// env values, credentials, tokens, or platform internals are included.
func mcpSessionMetadata(ctx context.Context, password, sessionID string) (string, error) {
	client := seamClientWithPassword(password)
	sessionID = strings.TrimSpace(sessionID)

	sessions, err := client.SessionList(ctx)
	if err != nil {
		return "", fmt.Errorf("failed to list sessions: %w", err)
	}
	busy, err := client.GetSessionStatuses(ctx)
	if err != nil {
		return "", fmt.Errorf("failed to read session statuses: %w", err)
	}

	match := func(s opencode.SessionSummary) bool {
		return sessionID == "" || s.ID == sessionID
	}
	type sessionMeta struct {
		ID            string  `json:"session_id"`
		Title         string  `json:"title"`
		Agent         string  `json:"agent,omitempty"`
		Model         string  `json:"model,omitempty"`
		Busy          bool    `json:"busy"`
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
		m := sessionMeta{
			ID:    s.ID,
			Title: s.Title,
			Agent: s.Agent,
			Busy:  busy[s.ID] == "busy",
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
	if sessionID != "" && len(metas) == 0 {
		return "", fmt.Errorf("session %s not found", sessionID)
	}

	version := ""
	if len(sessions) > 0 {
		version = sessions[0].Version
	}
	out, _ := json.Marshal(map[string]any{
		"workspace_id":  os.Getenv("WORKSPACE_ID"),
		"agent_version": version,
		"sessions":      metas,
	})
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

	if sessionID == "" {
		resolved, err := resolveSingleBusySession(ctx, client)
		if err != nil {
			return "", err
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
			return "", fmt.Errorf("session %s has no current model — pass model explicitly", sessionID)
		}
	}

	busy, err := client.GetSessionStatuses(ctx)
	if err != nil {
		return "", fmt.Errorf("failed to read session status: %w", err)
	}

	if busy[sessionID] == "busy" {
		// Detached: completes at the turn boundary; the tool must not
		// wait (the waiting turn may be the caller's own).
		go func() {
			if err := client.SessionSummarize(context.WithoutCancel(ctx), sessionID, providerID, modelID); err != nil {
				log.Warn("compact: background summarize failed",
					zap.String("sessionID", sessionID), zap.Error(err))
			}
		}()
		out, _ := json.Marshal(map[string]string{
			"status":     "scheduled",
			"session_id": sessionID,
			"detail":     "session is busy (likely your current turn); compaction runs when the current turn ends",
		})
		return string(out), nil
	}

	if err := client.SessionSummarize(ctx, sessionID, providerID, modelID); err != nil {
		return "", fmt.Errorf("compaction failed: %w", err)
	}
	out, _ := json.Marshal(map[string]string{
		"status":     "compacted",
		"session_id": sessionID,
	})
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
	var busyIDs []string
	for id, st := range busy {
		if st == "busy" {
			busyIDs = append(busyIDs, id)
		}
	}
	switch len(busyIDs) {
	case 1:
		return busyIDs[0], nil
	case 0:
		return "", fmt.Errorf("no session is currently running — pass session_id explicitly (find IDs with session_metadata)")
	default:
		return "", fmt.Errorf("multiple sessions are busy (%s) — pass session_id explicitly", strings.Join(busyIDs, ", "))
	}
}

// --- send_message ---------------------------------------------------------

// mcpSendMessage delivers a text message to another session in this
// workspace, fire-and-forget: the reply (if any) stays in the target
// session — nothing returns to the caller (the same philosophy as
// create_session; the task tool is the blocking/returns-result path).
//
// Busy targets queue the message server-side and deliver it when their
// current turn ends (live-proven run-at-boundary semantics — the same
// POST shape that powers compact's scheduling), so delivery is detached
// either way and the tool reports which case applies.
func mcpSendMessage(ctx context.Context, password, sessionID, message string) (string, error) {
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" {
		return "", fmt.Errorf("session_id is required")
	}
	message = strings.TrimSpace(message)
	if message == "" {
		return "", fmt.Errorf("message is required")
	}

	client := seamClientWithPassword(password)

	// Reject unknown IDs up front rather than failing silently in the
	// detached delivery (the caller cannot see the goroutine's error).
	sessions, err := client.SessionList(ctx)
	if err != nil {
		return "", fmt.Errorf("failed to resolve the target session: %w", err)
	}
	known := false
	for _, s := range sessions {
		if s.ID == sessionID {
			known = true
			break
		}
	}
	if !known {
		return "", fmt.Errorf("session %s not found in this workspace (IDs from session_list)", sessionID)
	}

	busy, err := client.GetSessionStatuses(ctx)
	if err != nil {
		return "", fmt.Errorf("failed to read session status: %w", err)
	}

	status := "delivering"
	if busy[sessionID] == "busy" {
		status = "delivering_after_current_turn"
	}

	// Detached delivery: WithoutCancel so it survives the tool response;
	// server-side queuing handles busy targets. Delivery is not retried
	// (same loss semantics as create_session — stated in the description).
	go func() {
		if _, err := client.SessionSend(context.WithoutCancel(ctx), sessionID, message, "", nil); err != nil {
			log.Warn("send_message: background delivery failed",
				zap.String("sessionID", sessionID), zap.Error(err))
		}
	}()

	out, _ := json.Marshal(map[string]string{
		"status":     status,
		"session_id": sessionID,
	})
	return string(out), nil
}

// --- abort_session --------------------------------------------------------

// mcpAbortSession stops a session's current turn via the V1 abort route
// (the seam's SessionAbort). Non-destructive to history and the
// delivery ledger; aborting an idle session is a server-side no-op.
func mcpAbortSession(ctx context.Context, password, sessionID string) (string, error) {
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" {
		return "", fmt.Errorf("session_id is required")
	}
	if err := seamClientWithPassword(password).SessionAbort(ctx, sessionID); err != nil {
		return "", fmt.Errorf("failed to abort session: %w", err)
	}
	out, _ := json.Marshal(map[string]string{
		"status":     "aborted",
		"session_id": sessionID,
	})
	return string(out), nil
}
