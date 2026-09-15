// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

// Epic 69 US-69.2: construction + wiring of the sessionstate authority.
// The authority itself (cmd/workspace-agentd/sessionstate) is dialect-free
// machinery; the opencode implementations of its two seams live here in the
// wiring layer, consistent with agentd's existing dialect handling (the
// tracker). US-69.3 relocates full contract translation behind the adapter
// seam and grows these implementations.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"

	"connectrpc.com/connect"
	"github.com/lenaxia/llmsafespaces/cmd/workspace-agentd/sessionstate"
	abiv1 "github.com/lenaxia/llmsafespaces/pkg/abi/v1"
	"github.com/lenaxia/llmsafespaces/pkg/agent"
	opencode "github.com/lenaxia/llmsafespaces/pkg/agent/opencode"
	"github.com/lenaxia/llmsafespaces/pkg/agent/systemnotices"
	agentd "github.com/lenaxia/llmsafespaces/pkg/agentd"
	"github.com/lenaxia/llmsafespaces/pkg/session"
	"github.com/lenaxia/llmsafespaces/pkg/version"
	"go.uber.org/zap"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// opencodeStoreReader is the US-69.3 StoreReader: opencode's session list
// is the store truth for statuses; the /question + /permission lists are
// the truth for pending inputs. Respects ctx; a wedged opencode surfaces as
// ctx deadline (M3.1).
type opencodeStoreReader struct {
	client *OpenCodeClient
}

func (r opencodeStoreReader) SessionStates(ctx context.Context) (map[string]sessionstate.SessionSeed, error) {
	sessions, err := r.client.ListSessions(ctx)
	if err != nil {
		return nil, err
	}
	out := make(map[string]sessionstate.SessionSeed, len(sessions))
	for _, s := range sessions {
		var status abiv1.SessionStatus
		switch s.Status {
		case "busy":
			status = abiv1.SessionStatus_SESSION_STATUS_BUSY
		case "error":
			status = abiv1.SessionStatus_SESSION_STATUS_ERROR
		default:
			status = abiv1.SessionStatus_SESSION_STATUS_IDLE
		}
		out[s.ID] = sessionstate.SessionSeed{Status: status}
	}
	d := &opencode.Dialect{}
	for _, in := range fetchList(ctx, r.client, "/question", d.ParseQuestionListItem) {
		if seed, ok := out[in.SessionID]; ok {
			seed.PendingInputs = append(seed.PendingInputs, questionToABI(in))
			out[in.SessionID] = seed
		}
	}
	for _, in := range fetchList(ctx, r.client, "/permission", d.ParsePermissionListItem) {
		if seed, ok := out[in.SessionID]; ok {
			seed.PendingInputs = append(seed.PendingInputs, permissionToABI(in))
			out[in.SessionID] = seed
		}
	}
	return out, nil
}

// fetchList GETs a JSON-array endpoint and maps each entry through parse.
// Unreachable/unimplemented endpoints (404, conn refused) are an
// authoritative-empty for PENDING INPUTS specifically — opencode versions
// without the endpoints never had questions; session-list errors above are
// the real store-read failure path.
func fetchList[T any](ctx context.Context, client *OpenCodeClient, path string, parse func(json.RawMessage) (T, error)) []T {
	resp, err := client.doRequest(ctx, path)
	if err != nil || resp.StatusCode >= 400 {
		if resp != nil {
			_ = resp.Body.Close()
		}
		return nil
	}
	defer func() { _ = resp.Body.Close() }()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	var items []json.RawMessage
	if json.Unmarshal(raw, &items) != nil {
		return nil
	}
	var out []T
	for _, item := range items {
		if v, err := parse(item); err == nil {
			out = append(out, v)
		}
	}
	return out
}

// Message-evidence paging bounds (#1311): the V1 list is newest-first with
// X-Next-Cursor pagination. 50/page × 40 pages = 2000 messages — a stranded
// admitted row's assistant message is recent in practice; a row deeper than
// the budget yields "no evidence" (error), never a false absence.
const (
	messageEvidencePageSize   = 50
	messageEvidencePageBudget = 40
)

// MessagePresence answers which of the given message IDs the harness store
// currently holds for a session — the #1311 reconcile pass's promotion
// evidence. Absence is only proven when the cursor exhausts (the
// verifydelivery rule); a page-budget overrun or transport error returns
// an error, which the sweep treats as NO evidence (rows untouched or
// status-evidence-only), never as absent.
func (r opencodeStoreReader) MessagePresence(ctx context.Context, sessionID string, messageIDs []string) (map[string]bool, error) {
	present := make(map[string]bool, len(messageIDs))
	if len(messageIDs) == 0 {
		return present, nil
	}
	want := make(map[string]struct{}, len(messageIDs))
	for _, id := range messageIDs {
		want[id] = struct{}{}
	}
	// Pre-seed every queried ID as absent so the returned map is
	// self-describing (each queried ID present with its verdict).
	for _, id := range messageIDs {
		present[id] = false
	}
	cursor := ""
	found := 0
	for page := 0; page < messageEvidencePageBudget; page++ {
		path := "/session/" + sessionID + "/message?limit=" + strconv.Itoa(messageEvidencePageSize)
		if cursor != "" {
			path += "&before=" + url.QueryEscape(cursor)
		}
		resp, err := r.client.doRequest(ctx, path)
		if err != nil {
			return nil, fmt.Errorf("GET %s: %w", path, err)
		}
		if resp.StatusCode >= 400 {
			_ = resp.Body.Close()
			return nil, fmt.Errorf("GET %s: status %d", path, resp.StatusCode)
		}
		var msgs []struct {
			Info struct {
				ID string `json:"id"`
			} `json:"info"`
		}
		decodeErr := json.NewDecoder(io.LimitReader(resp.Body, 8<<20)).Decode(&msgs)
		next := resp.Header.Get("X-Next-Cursor")
		_ = resp.Body.Close()
		if decodeErr != nil {
			return nil, fmt.Errorf("GET %s: decode: %w", path, decodeErr)
		}
		for _, m := range msgs {
			if _, ok := want[m.Info.ID]; ok && !present[m.Info.ID] {
				present[m.Info.ID] = true
				found++
			}
		}
		if found == len(want) {
			return present, nil
		}
		if next == "" {
			return present, nil // history exhausted — absence proven
		}
		cursor = next
	}
	return nil, fmt.Errorf("message evidence page budget exhausted for session %s — absence unproven", sessionID)
}

func questionToABI(q *agent.QuestionRequest) *abiv1.InputRequest {
	in := &abiv1.InputRequest{Id: q.ID, SessionId: q.SessionID, Kind: abiv1.InputKind_INPUT_KIND_QUESTION}
	if len(q.Questions) > 0 {
		inq := q.Questions[0]
		in.Question = inq.Question
		in.Header = inq.Header
		in.Multiple = inq.Multiple
		in.Custom = inq.Custom
		for _, o := range inq.Options {
			in.Options = append(in.Options, &abiv1.InputOption{Label: o.Label, Description: o.Description})
		}
	}
	if q.Tool != nil {
		in.Tool = &abiv1.ToolRef{MessageId: q.Tool.MessageID, CallId: q.Tool.CallID}
	}
	return in
}

func permissionToABI(p *agent.PermissionRequest) *abiv1.InputRequest {
	in := &abiv1.InputRequest{
		Id: p.ID, SessionId: p.SessionID, Kind: abiv1.InputKind_INPUT_KIND_PERMISSION,
		Permission: p.Permission, Patterns: p.Patterns, Always: p.Always,
	}
	if p.Tool != nil {
		in.Tool = &abiv1.ToolRef{MessageId: p.Tool.MessageID, CallId: p.Tool.CallID}
	}
	return in
}

// platformDirFromEnv resolves the platform/ PVC subPath mount (US-69.2).
func platformDirFromEnv() string {
	if v := os.Getenv("LLMSAFESPACES_PLATFORM_DIR"); v != "" {
		return v
	}
	return sessionstate.DefaultPlatformDir
}

// newStateAuthority builds the authority and returns it plus the raw-event
// hook for the SSE tracker. S1 shadow semantics: a missing/unwritable
// platform dir (staged rollout, chart not yet carrying the subPath) degrades
// to an in-memory cursor with a loud WARN — the surface must be additive
// and harmless (design 0055 M4). At S2 the authority flag makes the durable
// cursor a boot requirement instead.
func newStateAuthority(client *OpenCodeClient, password, controlPlanePassword string) *sessionstate.Authority {
	var authority *sessionstate.Authority
	actor, supportedActions := opencodeActionSurface(client, password)
	cfg := sessionstate.Config{
		PlatformDir:  platformDirFromEnv(),
		Parser:       &opencode.ABITranslator{},
		Store:        opencodeStoreReader{client: client},
		Admitter:     opencodeAdmitter{password: password},
		Actor:        actor,
		Capabilities: bootCapabilityReport(client, supportedActions),
		// US-69.12 stall wake (I6 wake-only recovery): a store refresh is
		// the nudge — events completing the stalled row's turn promote or
		// turn-end it; a still-missing promotion escalates via the
		// stalled-entries/wake-failure alerts. (A harness-specific
		// queue-poke lands only if a pinned version exposes one — the
		// probe discipline, never an invented route.)
		Wake: func(ctx context.Context, sessionID string) error {
			if authority == nil {
				return nil
			}
			log.Info("sessionstate: stall wake — reseeding from the store", zap.String("session", sessionID))
			return authority.Reseed(ctx, sessionstate.ReseedReasonStallWake)
		},
		// D6.1 pair: accept either credential across mixed-generation
		// windows; empty entries are skipped by the auth gate.
		Passwords: []string{controlPlanePassword, password},
		Logger:    log,
	}
	a, err := sessionstate.New(cfg)
	if err != nil {
		// Degraded mode: durable cursor unavailable (staged rollout, chart
		// not yet carrying the platform/ subPath). Seq resets on restart
		// until the volume lands — visible, loud, never fatal at S1.
		log.Warn("sessionstate: durable cursor unavailable — running with non-durable seq (staged rollout?)",
			zap.String("dir", cfg.PlatformDir), zap.Error(err))
		cfg.PlatformDir = os.TempDir() + "/llmsafespaces-sessionstate-cursor"
		a, err = sessionstate.New(cfg)
		if err != nil {
			log.Error("sessionstate: authority construction failed — module disabled for this boot", zap.Error(err))
			return nil
		}
	}
	authority = a // the stall-wake closure's target
	return a
}

// startStateAuthorityReseed drives the boot reseed: opencode may not be
// reachable yet, so retry with backoff until the first success. Generation
// changes reseed through the same serialized path. After the first success
// the crash-window ledger replay runs (#1311: ReplayUnresolvedDeliveries
// had no production caller — accepted-but-unadmitted rows never re-drove
// admission after an agentd restart).
func startStateAuthorityReseed(ctx context.Context, a *sessionstate.Authority, reason sessionstate.ReseedReason) {
	if a == nil {
		return
	}
	go func() {
		backoff := 500 * time.Millisecond
		const maxBackoff = 15 * time.Second
		for {
			if ctx.Err() != nil {
				return
			}
			if err := a.Reseed(ctx, reason); err == nil {
				a.ReplayUnresolvedDeliveries(ctx)
				return
			} else if ctx.Err() == nil {
				log.Debug("sessionstate: boot reseed retry", zap.Error(err))
			}
			select {
			case <-ctx.Done():
				return
			case <-time.After(backoff):
			}
			if backoff*2 > maxBackoff {
				backoff = maxBackoff
			} else {
				backoff *= 2
			}
		}
	}()
}

// bootCapabilityReport builds the static capability report served on every
// snapshot frame (US-69.4). Provenance: the 0053 overlay anchor
// (AGENTD_IMAGE_VOLUME — the digest-pinned delivery the self-verify gate
// enforces); BYO/legacy bases report UNPINNED (the M4 wiring rejects
// authority-flag-on for them). Harness version: ONE bounded boot-time
// discovery call (the report is static afterwards — M3.1: no harness calls
// on hot paths). Supported actions (US-69.9): the regression-pinned trio
// unconditionally + the two boot-probed V2 routes (opencodeActionSurface);
// file delivery parts are NotSupported on opencode per D3.
func bootCapabilityReport(client *OpenCodeClient, supportedActions []abiv1.ActionType) *abiv1.CapabilityReport {
	provenance := abiv1.Provenance_PROVENANCE_PLATFORM_PINNED
	if os.Getenv("AGENTD_IMAGE_VOLUME") != "1" {
		provenance = abiv1.Provenance_PROVENANCE_UNPINNED
	}
	harnessVersion := "unknown"
	if client != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		if _, v, err := client.IsHealthy(ctx); err == nil && v != "" {
			harnessVersion = v
		}
		cancel()
	}
	return &abiv1.CapabilityReport{
		Provenance:             provenance,
		Harness:                "opencode",
		HarnessVersion:         harnessVersion,
		AgentdVersion:          version.Version,
		SupportedActions:       supportedActions,
		SupportedDeliveryParts: []abiv1.DeliveryPartKind{abiv1.DeliveryPartKind_DELIVERY_PART_KIND_TEXT},
		AbiVersion:             "1",
	}
}

// --- #944 (r1 f3): the disk-pressure notice at the agentd send seams ----
//
// The injector moved seams twice and was orphaned twice (the package's
// own doc); the #1372 Act migration routes the authority regime's message
// writes AROUND the API-side adapter Wrap — so the notice is injected
// HERE, where every authority-regime message write funnels regardless of
// entrypoint: the actor's send (sync Act) and the admitter's Admit
// (outbox Deliver). Pod-local statfs of the workspace volume — fresher
// than the CRD status the API-side reader consumes — with the SAME
// systemnotices tier/notice text (one source of truth). Fail-open by
// construction: a read error, unknown total, or below-threshold ratio
// return the text unchanged. Flag-off keeps the API-side Wrap — no
// double injection (the regimes are disjoint).

// podDiskUsage reports (used, total) bytes of the workspace volume. Var
// for tests (the agentAddrAtomic seam convention).
var podDiskUsage = func() (used, total uint64, err error) {
	dir := os.Getenv("LLMSAFESPACES_WORKSPACE_DIR")
	if dir == "" {
		dir = "/workspace"
	}
	var st syscall.Statfs_t
	if err := syscall.Statfs(dir, &st); err != nil {
		return 0, 0, err
	}
	bs := uint64(st.Bsize)
	return (st.Blocks - st.Bfree - st.Bavail) * bs, st.Blocks * bs, nil
}

// withDiskNotice prepends the disk-pressure notice to an agent-bound
// message text when the workspace volume is at/above the warning tier
// (systemnotices' tiers, notice text, and ratio math — identical to the
// API-side injector).
func withDiskNotice(text string) string {
	used, total, err := podDiskUsage()
	if err != nil || total == 0 {
		return text
	}
	ratio := systemnotices.Ratio(int64(used), int64(total)) //nolint:gosec // statfs block counts bounded by the volume size (≤ EiB-class halves)
	notice := systemnotices.Notice(systemnotices.LevelForRatio(ratio), ratio)
	if notice == "" {
		return text
	}
	return notice + "\n\n" + text
}

// opencodeAdmitter is the US-69.7 admission seam implementation: POST the
// V2 prompt endpoint on the pod's opencode (localhost :4096, §D1 Basic
// credential). Delivery mode "steer" — the TUI's send semantics (#1288):
// admit-and-run-now, synchronous messageID, turn events flow on the
// session stream for promotion correlation (M2).
//
// History: this seam shipped with delivery:"queue" (0052 semantics:
// durable admission, drains on idle/wake) — but the pinned opencode
// 1.18.10 never drains that queue (#755, "messages vanished"; the API's
// adapter path abandoned queue for the same reason). The #1288 incident
// ran on queue-mode admission racing opencode restarts. Steer is what
// the TUI uses; the contract goldens (client_v2_contract_test) pin its
// prompt.admitted/prompted events.
type opencodeAdmitter struct {
	password string
}

// post issues one Basic-auth JSON POST against the pod's opencode
// (:4096, §D1 credential) — the admitter's transport discipline, shared
// shape with the actor's post.
func (o opencodeAdmitter) post(ctx context.Context, path string, payload any, out any) error {
	b, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, getAgentAddr()+path, bytes.NewReader(b))
	if err != nil {
		return err
	}
	req.SetBasicAuth("opencode", o.password)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode >= 400 {
		errBody, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("status %d: %s", resp.StatusCode, string(errBody))
	}
	if out != nil {
		return json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(out)
	}
	return nil
}

func (o opencodeAdmitter) Admit(ctx context.Context, sessionID, messageID, text, model string) (string, error) {
	if text == "" {
		return "", fmt.Errorf("admit: empty text")
	}
	text = withDiskNotice(text)
	// #1292b: the V2 prompt endpoint STRIPS per-prompt model overrides
	// (verified live: a steer body carrying glm-5.3 ran the session
	// default muse-spark). The model must be applied to the SESSION
	// before admission — the same contract as the adapter path's
	// SetSessionModel and the SwitchModel action ("pick a model →
	// session default changes"). Fail closed: a model the endpoint
	// rejects must fail the admission loudly, not silently run the
	// default.
	if model != "" {
		// Wire key is providerID on the pinned >= 1.18.15 (the repo's own
		// captured golden: {"id":"glm-5.3","providerID":"thekaocloud"},
		// #1119 stress finding 5) — "provider" is the <= 1.18.14 shape.
		wire := map[string]any{"id": model}
		if prov, id, ok := strings.Cut(model, "/"); ok && id != "" {
			wire = map[string]any{"id": id, "providerID": prov}
		}
		if err := o.post(ctx, "/api/session/"+sessionID+"/model", map[string]any{"model": wire}, nil); err != nil {
			return "", fmt.Errorf("admit: set session model: %w", err)
		}
	}
	// #1313: V1 message path, not V2 steer. The V2 prompt endpoint's
	// session runner does NOT include MCP tools in the model's function
	// definitions on opencode 1.18.15 (proven with controlled variables:
	// same session, same wording — steer → "no MCP tools", V1 → all
	// present). The TUI calls the session runner directly with the full
	// tool registry; V1 is the HTTP surface that matches. The V2 path
	// also strips model overrides (#1292b above). The ledger, admission
	// retry, dedup, and promotion correlation all stay — only the final
	// POST to opencode changes.
	// S2 (#1315): messageID is the entry-derived dedupe key — the pinned
	// harness validates the msg-prefix, uses it verbatim as the user
	// message's store ID, and upserts on collision (G1-verified live on
	// 1.18.15: same-ID re-POST returns the existing exchange, no second
	// user message). Idempotency belongs at the write.
	body := map[string]any{
		"messageID": messageID,
		"parts":     []map[string]any{{"type": "text", "text": text}},
	}
	b, err := json.Marshal(body)
	if err != nil {
		return "", err
	}
	url := fmt.Sprintf("%s/session/%s/message", getAgentAddr(), sessionID)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(b))
	if err != nil {
		return "", err
	}
	req.SetBasicAuth("opencode", o.password)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode >= 400 {
		errBody, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return "", fmt.Errorf("admit: status %d: %s", resp.StatusCode, string(errBody))
	}
	// V1 returns the assistant message synchronously — the info.id is
	// the message ID the ledger's promotion correlation needs.
	var out struct {
		Info struct {
			ID string `json:"id"`
		} `json:"info"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&out); err != nil {
		return "", err
	}
	return out.Info.ID, nil
}

// newSessionInterrupter builds the #1342 force-path interrupt: the Act
// interrupt verb through the opencode actor seam (V1 abort — the only
// interrupt route on pinned versions ≥ 1.18.10, regression-pinned by
// the US-69.9 surface). The restart path holds this seam and learns
// nothing about opencode wire shapes (Rule 12). I7 by construction:
// the abort mutates turn state only, never ledger/entry state.
func newSessionInterrupter(password string) sessionInterrupter {
	actor := opencodeActor{password: password}
	return func(ctx context.Context, sessionID string) error {
		_, err := actor.Act(ctx, sessionID, &abiv1.ActionRequest{
			SessionId: sessionID,
			Action:    &abiv1.ActionRequest_Interrupt{Interrupt: &abiv1.InterruptAction{}},
		})
		return err
	}
}

// --- US-69.9: the typed-actions seam (design 0055 M1 op 5) ---------------
//
// isQuestionID / isPermissionID discriminate the harness's prefixed
// request-ID namespaces (que_/per_) — the endpoints prefix-validate and
// a cross-kind post is a 400 Params error, never a 404 (captured live,
// 4a r6). Both helpers live HERE: this file is the opencode seam.
func isQuestionID(id string) bool   { return strings.HasPrefix(id, "que_") }
func isPermissionID(id string) bool { return strings.HasPrefix(id, "per_") }

// opencodeActor implements sessionstate.Actor: the five frozen-union verbs
// against the pod's opencode (localhost :4096, §D1 Basic credential — the
// admitter's transport discipline). Route selection is measured fact, not
// assumption: the three regression-pinned routes (V1 abort, V2 model, V1
// question/permission reply) are declared unconditionally; the two
// unpinned V2 routes (switchAgent, compact) are boot-probed — the
// 1.18.10 V2-interrupt removal is the precedent for never trusting route
// presence across pinned versions.

// opencodeActionSurface probes the two unpinned routes and returns the
// actor plus the capability declaration the boot report carries.
func opencodeActionSurface(client *OpenCodeClient, password string) (sessionstate.Actor, []abiv1.ActionType) {
	actions := []abiv1.ActionType{
		abiv1.ActionType_ACTION_TYPE_INTERRUPT,
		abiv1.ActionType_ACTION_TYPE_SWITCH_MODEL,
		abiv1.ActionType_ACTION_TYPE_ANSWER_QUESTION,
		// #1372 (S1 completion): the sessions-cluster verbs. Declared
		// unconditionally — their harness routes are the production
		// adapter path's own V1 routes (POST /session, POST
		// /session/:id/message, DELETE /session/:id, PATCH /session/:id),
		// the same regression-pinned confidence class as the trio above;
		// the boot probe below stays for the unpinned V2 routes only.
		abiv1.ActionType_ACTION_TYPE_CREATE_SESSION,
		abiv1.ActionType_ACTION_TYPE_SEND,
		abiv1.ActionType_ACTION_TYPE_DELETE_SESSION,
		abiv1.ActionType_ACTION_TYPE_RENAME_SESSION,
	}
	switchAgent, agentKey, compact := probeActionRoutes(client)
	if switchAgent {
		actions = append(actions, abiv1.ActionType_ACTION_TYPE_SWITCH_AGENT)
	}
	if compact {
		actions = append(actions, abiv1.ActionType_ACTION_TYPE_COMPACT)
	}
	return opencodeActor{password: password, agentKey: agentKey}, actions
}

// probeActionRoutes measures whether the pinned opencode serves the
// switchAgent and compact V2 routes. Discrimination (the established
// probeCapabilities discipline): route present → typed 400 (missing-key
// validation error, JSON); route absent → catch-all 204 / non-JSON 404.
// The switchAgent probe also learns the body's required key name (the
// same missing-key pointer that revealed the model id/modelID split).
func probeActionRoutes(client *OpenCodeClient) (switchAgent bool, agentKey string, compact bool) {
	agentKey = "agentID" // pinned floor default; a positive probe overrides
	if client == nil {
		return false, agentKey, false
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	const probeSID = "00000000000000000000000000"
	if code, body := postHarnessRaw(ctx, client.password, "/api/session/"+probeSID+"/switchAgent", map[string]any{}); code == 400 {
		switchAgent = true
		if m := missingAgentKeyRe.FindSubmatch(body); m != nil {
			agentKey = string(m[1])
		}
	}
	if code, _ := postHarnessRaw(ctx, client.password, "/api/session/"+probeSID+"/compact", map[string]any{}); code == 400 {
		compact = true
	}
	return switchAgent, agentKey, compact
}

var missingAgentKeyRe = regexp.MustCompile(`Missing key \[\\"?([a-zA-Z]+)\\"?\]`)

// postHarnessRaw is the probe transport: one POST, status + body back.
func postHarnessRaw(ctx context.Context, password, path string, body any) (int, []byte) {
	b, err := json.Marshal(body)
	if err != nil {
		return 0, nil
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, getAgentAddr()+path, bytes.NewReader(b))
	if err != nil {
		return 0, nil
	}
	req.SetBasicAuth(agentd.AuthUsername, password)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, nil
	}
	defer func() { _ = resp.Body.Close() }()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	return resp.StatusCode, data
}

// opencodeActor executes the typed actions against opencode.
type opencodeActor struct {
	password string
	// agentKey is the switchAgent body key (boot-probed).
	agentKey string
}

func (o opencodeActor) Act(ctx context.Context, sessionID string, req *abiv1.ActionRequest) (*abiv1.ActionResult, error) {
	switch a := req.GetAction().(type) {
	case *abiv1.ActionRequest_Interrupt:
		// V1 abort: the only interrupt route on pinned versions ≥ 1.18.10
		// (V2 interrupt was removed upstream — e2e regression-pinned).
		// I7: non-destructive by construction — this aborts the harness
		// turn only; ledger/entry states are never touched here.
		if _, err := o.post(ctx, "/session/"+sessionID+"/abort", map[string]any{}, nil); err != nil {
			return nil, err
		}
		return &abiv1.ActionResult{Result: &abiv1.ActionResult_Interrupt{Interrupt: &abiv1.InterruptResult{}}}, nil

	case *abiv1.ActionRequest_SwitchModel:
		m := a.SwitchModel.GetModel()
		// providerID key — the captured >= 1.18.15 golden (#1293 r1: the
		// pre-existing "provider" key contradicts it, same wire-drift class).
		wire := map[string]any{"id": m.GetId()}
		if m.GetProvider() != "" {
			wire["providerID"] = m.GetProvider()
		}
		if _, err := o.post(ctx, "/api/session/"+sessionID+"/model", map[string]any{"model": wire}, nil); err != nil {
			return nil, err
		}
		return &abiv1.ActionResult{Result: &abiv1.ActionResult_SwitchModel{SwitchModel: &abiv1.SwitchModelResult{Model: m}}}, nil

	case *abiv1.ActionRequest_SwitchAgent:
		if _, err := o.post(ctx, "/api/session/"+sessionID+"/switchAgent", map[string]any{o.agentKey: a.SwitchAgent.GetAgentId()}, nil); err != nil {
			return nil, err
		}
		return &abiv1.ActionResult{Result: &abiv1.ActionResult_SwitchAgent{SwitchAgent: &abiv1.SwitchAgentResult{AgentId: a.SwitchAgent.GetAgentId()}}}, nil

	case *abiv1.ActionRequest_AnswerQuestion:
		ans := a.AnswerQuestion
		// The reply form (#1302 contract delta) carries the permission
		// vocabulary directly — route straight to the permission reply
		// endpoint, no question-first probe, no lossy option encoding.
		// 4a D1: reply="reject" on a question id is the DISMISS exit —
		// question-reject for que_ ids (its 404 is the absence signal
		// for the fold); permission reply for per_ ids. No cross-kind
		// fallback (the harness prefix-validates: cross-kind posts are
		// 400 Params, never 404).
		// 4a D2: the optional message rides the permission reply body
		// (deny feedback the raw passthrough carried).
		if ans.GetReply() != "" {
			// r6: the harness PREFIX-VALIDATES request IDs — a que_-id
			// posted to a permission endpoint (or the reverse) is a 400
			// Params error, never a 404 (captured in
			// ask_terminal_states_1_18_15.json cross_prefix_contract).
			// Cross-kind fallbacks are therefore forbidden; the 404 from
			// the ask's OWN kind is the absence signal the authority's
			// resolve-by-absence consumes.
			if ans.GetReply() == "reject" {
				if isQuestionID(ans.GetInputId()) {
					// The dismiss exit on a question: question-reject
					// only; 404 → the typed NotFound surfaces for the
					// fold (the ask is gone — exactly S6).
					if _, err := o.post(ctx, "/question/"+ans.GetInputId()+"/reject", map[string]any{}, nil); err != nil {
						return nil, err
					}
					return &abiv1.ActionResult{Result: &abiv1.ActionResult_AnswerQuestion{AnswerQuestion: &abiv1.AnswerInputResult{InputId: ans.GetInputId()}}}, nil
				}
				body := map[string]any{"reply": "reject"}
				if ans.GetMessage() != "" {
					body["message"] = ans.GetMessage()
				}
				if _, err := o.post(ctx, "/permission/"+ans.GetInputId()+"/reply", body, nil); err != nil {
					return nil, err
				}
				return &abiv1.ActionResult{Result: &abiv1.ActionResult_AnswerQuestion{AnswerQuestion: &abiv1.AnswerInputResult{InputId: ans.GetInputId()}}}, nil
			}
			if isQuestionID(ans.GetInputId()) {
				// A non-reject reply vocabulary on a question id is a
				// malformed ask — surface it loudly (the frontend only
				// sends reply for permissions).
				return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("answer_question reply vocabulary is permission-only; input %s is a question (use option_ids/custom_text)", ans.GetInputId()))
			}
			body := map[string]any{"reply": ans.GetReply()}
			if ans.GetMessage() != "" {
				body["message"] = ans.GetMessage()
			}
			if _, err := o.post(ctx, "/permission/"+ans.GetInputId()+"/reply", body, nil); err != nil {
				return nil, err
			}
			return &abiv1.ActionResult{Result: &abiv1.ActionResult_AnswerQuestion{AnswerQuestion: &abiv1.AnswerInputResult{InputId: ans.GetInputId()}}}, nil
		}
		// opencode's unified reply contract (worklog 0069 live capture +
		// the frontend's input client): questions take {"answers": [[..]]}
		// (one array per question: selected labels and/or free text);
		// permissions take {"reply": "once"|"always"|"reject"} — 404 on
		// the question route means the input is a permission.
		options := append([]string{}, ans.GetOptionIds()...)
		if ans.GetCustomText() != "" {
			options = append(options, ans.GetCustomText())
		}
		// r6: prefix-aware — a que_ id answers the question endpoint only
		// (its 404 is the absence signal for the fold); a per_ id goes
		// straight to the permission reply. Unknown prefixes keep the
		// legacy probe order (agent-agnostic callers).
		if isPermissionID(ans.GetInputId()) {
			reply := "once"
			if len(options) > 0 {
				reply = options[0] // "once"|"always"|"reject" ride the same field
			}
			if _, err := o.post(ctx, "/permission/"+ans.GetInputId()+"/reply", map[string]any{"reply": reply}, nil); err != nil {
				return nil, err
			}
			return &abiv1.ActionResult{Result: &abiv1.ActionResult_AnswerQuestion{AnswerQuestion: &abiv1.AnswerInputResult{InputId: ans.GetInputId()}}}, nil
		}
		code, err := o.post(ctx, "/question/"+ans.GetInputId()+"/reply", map[string]any{"answers": [][]string{options}}, nil)
		if code == http.StatusNotFound && !isQuestionID(ans.GetInputId()) {
			reply := "once"
			if len(options) > 0 {
				reply = options[0]
			}
			if _, err := o.post(ctx, "/permission/"+ans.GetInputId()+"/reply", map[string]any{"reply": reply}, nil); err != nil {
				return nil, err
			}
		} else if err != nil {
			return nil, err
		}
		return &abiv1.ActionResult{Result: &abiv1.ActionResult_AnswerQuestion{AnswerQuestion: &abiv1.AnswerInputResult{InputId: ans.GetInputId()}}}, nil

	case *abiv1.ActionRequest_Compact:
		if _, err := o.post(ctx, "/api/session/"+sessionID+"/compact", map[string]any{}, nil); err != nil {
			return nil, err
		}
		return &abiv1.ActionResult{Result: &abiv1.ActionResult_Compact{Compact: &abiv1.CompactResult{}}}, nil

	case *abiv1.ActionRequest_CreateSession:
		// #1372: the adapter path's own create wire — POST /session with
		// the optional title (empty object when absent). The response
		// round-trips through the SAME exported translator the adapter
		// uses, then converts to the ABI result shape.
		body := map[string]any{}
		if a.CreateSession.GetTitle() != "" {
			body["title"] = a.CreateSession.GetTitle()
		}
		_, raw, err := o.do(ctx, http.MethodPost, "/session", body, nil)
		if err != nil {
			return nil, err
		}
		s, perr := opencode.ParseSessionWire(raw, "")
		if perr != nil {
			return nil, connect.NewError(connect.CodeInternal, perr)
		}
		return &abiv1.ActionResult{Result: &abiv1.ActionResult_CreateSession{
			CreateSession: &abiv1.CreateSessionResult{Session: sessionToABI(s)},
		}}, nil

	case *abiv1.ActionRequest_Send:
		// #1372: the synchronous send wire — the adapter's V1 message
		// route, parts + the per-prompt model OBJECT form (the shared
		// MessageModelOverrideWire seam; the differ enrichment stays
		// dead in production on both paths — WithFileDiffProducer has
		// zero production callers).
		sa := a.Send
		body := map[string]any{
			"parts": []map[string]any{{"type": "text", "text": withDiskNotice(sa.GetText())}},
		}
		if wire, ok := opencode.MessageModelOverrideWire(modelRefFromABI(sa.GetModel())); ok {
			body["model"] = wire
		}
		_, raw, err := o.do(ctx, http.MethodPost, "/session/"+sessionID+"/message", body, nil)
		if err != nil {
			return nil, err
		}
		msg, _, perr := opencode.ParseMessageWire(raw)
		if perr != nil {
			return nil, connect.NewError(connect.CodeInternal, perr)
		}
		return &abiv1.ActionResult{Result: &abiv1.ActionResult_Send{
			Send: &abiv1.SendResult{Message: messageToABI(&msg)},
		}}, nil

	case *abiv1.ActionRequest_DeleteSession:
		if _, _, err := o.do(ctx, http.MethodDelete, "/session/"+sessionID, nil, nil); err != nil {
			return nil, err
		}
		return &abiv1.ActionResult{Result: &abiv1.ActionResult_DeleteSession{
			DeleteSession: &abiv1.DeleteSessionResult{},
		}}, nil

	case *abiv1.ActionRequest_RenameSession:
		// PATCH, not POST: the pinned agent accepts POST /session/{id}
		// with 200 but IGNORES the body (the adapter comment's
		// silent-rename finding, 2026-09-13).
		if _, _, err := o.do(ctx, http.MethodPatch, "/session/"+sessionID,
			map[string]any{"title": a.RenameSession.GetTitle()}, nil); err != nil {
			return nil, err
		}
		return &abiv1.ActionResult{Result: &abiv1.ActionResult_RenameSession{
			RenameSession: &abiv1.RenameSessionResult{},
		}}, nil

	default:
		return nil, fmt.Errorf("opencodeActor: unhandled action %T", a)
	}
}

// post is the POST transport for the verbs that ignore the response
// body; do returns it.
func (o opencodeActor) post(ctx context.Context, path string, body any, out any) (int, error) {
	code, _, err := o.do(ctx, http.MethodPost, path, body, out)
	return code, err
}

// do is the action transport: one Basic-auth JSON request, expect 2xx. A
// non-2xx returns a typed connect error so the Act op surfaces the
// harness's status (InvalidArgument/NotFound etc.) instead of a generic
// 500. The response body is returned (bounded) for the caller's
// translation.
func (o opencodeActor) do(ctx context.Context, method, path string, body any, out any) (int, []byte, error) {
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return 0, nil, err
		}
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, getAgentAddr()+path, rdr)
	if err != nil {
		return 0, nil, err
	}
	req.SetBasicAuth(agentd.AuthUsername, o.password)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if resp.StatusCode >= 400 {
		code := connect.CodeInternal
		switch resp.StatusCode {
		case http.StatusBadRequest:
			code = connect.CodeInvalidArgument
		case http.StatusNotFound:
			code = connect.CodeNotFound
		case http.StatusUnauthorized:
			code = connect.CodeUnauthenticated
		}
		return resp.StatusCode, nil, connect.NewError(code, fmt.Errorf("%s %s: status %d: %s", method, path, resp.StatusCode, string(data)))
	}
	if out != nil {
		if err := json.Unmarshal(data, out); err != nil {
			return resp.StatusCode, data, err
		}
	}
	return resp.StatusCode, data, nil
}

// --- #1372: contract ↔ ABI converters for the sessions verbs -----------
//
// The wiring-side halves (the opencode seam's questionToABI/
// permissionToABI are the precedent); the API-side halves live beside
// inputRequestFromABI in the handlers. The parity pins (handler tests)
// hold both sides to the contract JSON.

func modelRefFromABI(m *abiv1.ModelRef) *session.ModelRef {
	if m == nil {
		return nil
	}
	return &session.ModelRef{ID: m.GetId(), Provider: m.GetProvider()}
}

func modelRefToABI(m *session.ModelRef) *abiv1.ModelRef {
	if m == nil {
		return nil
	}
	return &abiv1.ModelRef{Id: m.ID, Provider: m.Provider}
}

func sessionStatusToABI(s session.Status) abiv1.SessionStatus {
	switch s {
	case session.StatusIdle:
		return abiv1.SessionStatus_SESSION_STATUS_IDLE
	case session.StatusBusy:
		return abiv1.SessionStatus_SESSION_STATUS_BUSY
	case session.StatusError:
		return abiv1.SessionStatus_SESSION_STATUS_ERROR
	case session.StatusCompacting:
		return abiv1.SessionStatus_SESSION_STATUS_COMPACTING
	case session.StatusArchived:
		return abiv1.SessionStatus_SESSION_STATUS_ARCHIVED
	default:
		return abiv1.SessionStatus_SESSION_STATUS_UNKNOWN
	}
}

func costToABI(c *session.Cost) *abiv1.Cost {
	if c == nil {
		return nil
	}
	return &abiv1.Cost{
		InputTokens:      c.InputTokens,
		OutputTokens:     c.OutputTokens,
		ReasoningTokens:  c.ReasoningTokens,
		CacheReadTokens:  c.CacheReadTokens,
		CacheWriteTokens: c.CacheWriteTokens,
		TotalTokens:      c.TotalTokens,
		CostUsd:          c.CostUSD,
	}
}

func timeRangeToABI(tr *session.TimeRange) *abiv1.TimeRange {
	if tr == nil {
		return nil
	}
	out := &abiv1.TimeRange{}
	if !tr.StartedAt.IsZero() {
		out.StartedAt = timestamppb.New(tr.StartedAt)
	}
	if tr.CompletedAt != nil {
		out.CompletedAt = timestamppb.New(*tr.CompletedAt)
	}
	return out
}

func sessionToABI(s *session.Session) *abiv1.Session {
	if s == nil {
		return nil
	}
	return &abiv1.Session{
		Id:          s.ID,
		WorkspaceId: s.WorkspaceID,
		ParentId:    s.ParentID,
		Title:       s.Title,
		AgentId:     s.AgentID,
		Model:       modelRefToABI(s.Model),
		Status:      sessionStatusToABI(s.Status),
		Cost:        costToABI(s.Cost),
		Time:        timeRangeToABI(s.Time),
		Summary:     s.Summary,
		Archived:    s.Archived,
	}
}

func messageStatusToABI(t session.MessageType) abiv1.MessageType {
	switch t {
	case session.MessageUser:
		return abiv1.MessageType_MESSAGE_TYPE_USER
	case session.MessageAssistant:
		return abiv1.MessageType_MESSAGE_TYPE_ASSISTANT
	case session.MessageShell:
		return abiv1.MessageType_MESSAGE_TYPE_SHELL
	case session.MessageAgentSwitch:
		return abiv1.MessageType_MESSAGE_TYPE_AGENT_SWITCH
	case session.MessageModelSwitch:
		return abiv1.MessageType_MESSAGE_TYPE_MODEL_SWITCH
	case session.MessageCompaction:
		return abiv1.MessageType_MESSAGE_TYPE_COMPACTION
	case session.MessageSystem:
		return abiv1.MessageType_MESSAGE_TYPE_SYSTEM
	default:
		return abiv1.MessageType_MESSAGE_TYPE_UNSPECIFIED
	}
}

func toolStatusToABI(s session.ToolStatus) abiv1.ToolStatus {
	switch s {
	case session.ToolStatusPending:
		return abiv1.ToolStatus_TOOL_STATUS_PENDING
	case session.ToolStatusRunning:
		return abiv1.ToolStatus_TOOL_STATUS_RUNNING
	case session.ToolStatusCompleted:
		return abiv1.ToolStatus_TOOL_STATUS_COMPLETED
	case session.ToolStatusError:
		return abiv1.ToolStatus_TOOL_STATUS_ERROR
	default:
		return abiv1.ToolStatus_TOOL_STATUS_UNSPECIFIED
	}
}

func changeStatusToABI(s session.ChangeStatus) abiv1.ChangeStatus {
	switch s {
	case session.ChangeAdded:
		return abiv1.ChangeStatus_CHANGE_STATUS_ADDED
	case session.ChangeModified:
		return abiv1.ChangeStatus_CHANGE_STATUS_MODIFIED
	case session.ChangeDeleted:
		return abiv1.ChangeStatus_CHANGE_STATUS_DELETED
	case session.ChangeRenamed:
		return abiv1.ChangeStatus_CHANGE_STATUS_RENAMED
	default:
		return abiv1.ChangeStatus_CHANGE_STATUS_UNSPECIFIED
	}
}

func partToABI(p session.Part) *abiv1.Part {
	out := &abiv1.Part{Id: p.ID}
	switch p.Type {
	case session.PartText:
		out.Type = abiv1.PartType_PART_TYPE_TEXT
		out.Payload = &abiv1.Part_Text{Text: p.Text}
	case session.PartReasoning:
		out.Type = abiv1.PartType_PART_TYPE_REASONING
		out.Payload = &abiv1.Part_Reasoning{Reasoning: p.Reasoning}
	case session.PartTool:
		if p.Tool == nil {
			return nil
		}
		out.Type = abiv1.PartType_PART_TYPE_TOOL
		tool := &abiv1.ToolPart{
			CallId: p.Tool.CallID,
			Name:   p.Tool.Name,
			Input:  p.Tool.Input,
			Output: p.Tool.Output,
			State:  &abiv1.ToolState{Status: toolStatusToABI(p.Tool.State.Status), Error: p.Tool.State.Error},
		}
		if p.Tool.State.StartedAt != nil {
			tool.State.StartedAt = timestamppb.New(*p.Tool.State.StartedAt)
		}
		if p.Tool.State.CompletedAt != nil {
			tool.State.CompletedAt = timestamppb.New(*p.Tool.State.CompletedAt)
		}
		out.Payload = &abiv1.Part_Tool{Tool: tool}
	case session.PartFileChange:
		if p.FileChange == nil {
			return nil
		}
		out.Type = abiv1.PartType_PART_TYPE_FILE_CHANGE
		out.Payload = &abiv1.Part_FileChange{FileChange: &abiv1.FileDiff{
			Path:      p.FileChange.Path,
			OldPath:   p.FileChange.OldPath,
			Status:    changeStatusToABI(p.FileChange.Status),
			Patch:     p.FileChange.Patch,
			Additions: int32(p.FileChange.Additions), //nolint:gosec // display-only diff counts
			Deletions: int32(p.FileChange.Deletions), //nolint:gosec // display-only diff counts
		}}
	case session.PartCustom:
		if p.Custom == nil {
			return nil
		}
		out.Type = abiv1.PartType_PART_TYPE_CUSTOM
		out.Payload = &abiv1.Part_Custom{Custom: &abiv1.CustomPart{Kind: p.Custom.Kind, Data: p.Custom.Data}}
	default:
		return nil
	}
	return out
}

func messageToABI(m *session.Message) *abiv1.Message {
	if m == nil {
		return nil
	}
	out := &abiv1.Message{
		Id:        m.ID,
		SessionId: m.SessionID,
		Type:      messageStatusToABI(m.Type),
		Text:      m.Text,
		Command:   m.Command,
		FromAgent: m.FromAgent,
		ToAgent:   m.ToAgent,
		FromModel: modelRefToABI(m.FromModel),
		ToModel:   modelRefToABI(m.ToModel),
		Model:     modelRefToABI(m.Model),
		Cost:      costToABI(m.Cost),
	}
	if m.CreatedAt != nil {
		out.CreatedAt = timestamppb.New(*m.CreatedAt)
	}
	if m.ExitCode != nil {
		out.ExitCode = proto.Int32(int32(*m.ExitCode)) //nolint:gosec // display-only
	}
	if m.Error != nil {
		out.Error = &abiv1.Error{Code: m.Error.Code, Message: m.Error.Message}
	}
	for _, p := range m.Parts {
		if ap := partToABI(p); ap != nil {
			out.Parts = append(out.Parts, ap)
		}
	}
	return out
}

// PendingInputs is the lease diff's truth source: the live ask registries
// with STRICT failure semantics — anything but a clean 200-with-valid-JSON
// on each endpoint is an error (the caller skips the diff), because a
// transient 5xx on /question must never read as "no questions asked"
// (the r1 review's resolve/appeared flap). 404 and connection-refused are
// ALSO errors here, not absence: unlike the boot reseed (which treats
// them as never-had-questions), the lease pass runs against a live
// opencode that was answering moments ago — a refused connection is
// indeterminate, and skipping one 15s tick is free.
func (r opencodeStoreReader) PendingInputs(ctx context.Context) (map[string][]*abiv1.InputRequest, error) {
	d := &opencode.Dialect{}
	out := map[string][]*abiv1.InputRequest{}
	qs, err := fetchListStrict(ctx, r.client, "/question", d.ParseQuestionListItem)
	if err != nil {
		return nil, err
	}
	for _, q := range qs {
		if q.SessionID == "" || q.ID == "" {
			continue
		}
		out[q.SessionID] = append(out[q.SessionID], questionToABI(q))
	}
	ps, err := fetchListStrict(ctx, r.client, "/permission", d.ParsePermissionListItem)
	if err != nil {
		return nil, err
	}
	for _, p := range ps {
		if p.SessionID == "" || p.ID == "" {
			continue
		}
		out[p.SessionID] = append(out[p.SessionID], permissionToABI(p))
	}
	return out, nil
}

// fetchListStrict is fetchList with lease semantics: every failure mode
// (transport, non-2xx, malformed body, item parse) is an error — the
// caller must treat the list as indeterminate, never empty.
func fetchListStrict[T any](ctx context.Context, client *OpenCodeClient, path string, parse func(json.RawMessage) (T, error)) ([]T, error) {
	resp, err := client.doRequest(ctx, path)
	if err != nil {
		return nil, fmt.Errorf("lease gather %s: %w", path, err)
	}
	if resp.StatusCode >= 400 {
		_ = resp.Body.Close()
		return nil, fmt.Errorf("lease gather %s: status %d", path, resp.StatusCode)
	}
	raw, rerr := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	_ = resp.Body.Close()
	if rerr != nil {
		return nil, fmt.Errorf("lease gather %s: read: %w", path, rerr)
	}
	var items []json.RawMessage
	if json.Unmarshal(raw, &items) != nil || strings.TrimSpace(string(raw)) == "null" {
		return nil, fmt.Errorf("lease gather %s: malformed body", path)
	}
	out := make([]T, 0, len(items))
	for _, item := range items {
		v, perr := parse(item)
		if perr != nil {
			return nil, fmt.Errorf("lease gather %s: item parse: %w", path, perr)
		}
		out = append(out, v)
	}
	return out, nil
}
