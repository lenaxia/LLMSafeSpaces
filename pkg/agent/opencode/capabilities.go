package opencode

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/lenaxia/llmsafespaces/pkg/agentd"
	"go.uber.org/zap"
)

// Capabilities is what the adapter needs to know about the opencode binary
// it is talking to. Upstream does not offer interface stability across
// patch releases (the v2 route group was deleted in 1.18.10 and revived
// with a different model-ref schema by 1.18.15 — see #1119), so the adapter
// detects instead of assuming. Every field must be probed from the live
// binary; none may be inferred from a version comment.
type Capabilities struct {
	// V2PromptRoute is true when POST /api/session/:sid/prompt exists.
	V2PromptRoute bool
	// ModelRefIDKey is true when Model.Ref serializes as {"id", "providerID"}
	// (observed on opencode >= 1.18.15). False means the legacy
	// {"modelID", "providerID"} shape (<= 1.18.14). The per-prompt model
	// override is silently rejected when the wrong shape is sent (#1119
	// stress finding 5) — this probe is what prevents a repeat.
	ModelRefIDKey bool
	// Probed is true once a probe completed (successfully or not); callers
	// can distinguish "not yet probed" from "probed, route absent".
	Probed bool
}

// missingKeyRe matches the missing-required-key pointer inside a 400
// InvalidRequestError from the model-set route. The live body is
// JSON-in-JSON, so the inner quotes arrive backslash-escaped
// (captured 1.18.15: `Missing key\n  at [\"model\"][\"id\"]`); the
// optional \\? keeps one regex correct for both escaped and bare forms.
var missingKeyRe = regexp.MustCompile(`\[\\?"model\\?"\]\[\\?"(id|modelID)\\?"\]`)

// probeCapabilities classifies the binary with read-only requests. Every
// probe sends a payload that CANNOT be accepted (empty model object; empty
// prompt body), so no session state is ever mutated — responses are
// validation errors whose shape carries the information.
func (c *Client) probeCapabilities(ctx context.Context) Capabilities {
	caps := Capabilities{Probed: true}

	// Route existence: POST /api/session/:sid/prompt with an invalid body.
	// Route present  -> 400 InvalidRequestError (JSON, "_tag":"InvalidRequestError")
	// Route absent   -> 404 or the SPA HTML fallback (non-JSON).
	if code, body := c.postRaw(ctx, "/api/session/00000000000000000000000000/prompt", map[string]any{}); code == 400 &&
		strings.Contains(string(body), "InvalidRequestError") {
		caps.V2PromptRoute = true
	}

	// Model.Ref shape: POST /api/session/:sid/model with an empty model
	// object. The 400 names the first missing required key:
	//   >= 1.18.15: Missing key ["model"]["id"]
	//   <= 1.18.14: Missing key ["model"]["modelID"]
	// The target session is nonexistent; payload validation firing (400
	// before 404) is itself part of the observed contract. If the server
	// checks existence first (404), the probe is indeterminate and the
	// adapter falls back to the pinned-floor shape with a warning.
	if code, body := c.postRaw(ctx, "/api/session/00000000000000000000000000/model", map[string]any{"model": map[string]any{}}); code == 400 {
		if m := missingKeyRe.FindSubmatch(body); m != nil {
			caps.ModelRefIDKey = string(m[1]) == "id"
		}
	}
	return caps
}

// Capabilities returns the probed capabilities, probing lazily once.
// On transport failure it returns the zero value with Probed=true and the
// adapter must treat the run as degraded (log loudly, use floor defaults).
func (c *Client) Capabilities(ctx context.Context) (Capabilities, error) {
	c.capsOnce.Do(func() {
		if c.baseURL == "" {
			c.cached = Capabilities{Probed: true}
			return
		}
		pctx, cancel := context.WithTimeout(ctx, probeTimeout)
		defer cancel()
		c.cached = c.probeCapabilities(pctx)
		if c.logger != nil && (!c.cached.V2PromptRoute || !c.cached.ModelRefIDKey) {
			// Indeterminate probes (transport error vs genuinely absent
			// route) degrade to the pinned-floor shapes; a missing prompt
			// route on a binary the platform intends to drive via V2 is a
			// deployment error worth failing loudly on.
			c.logger.Warn("opencode capability probe incomplete — adapter will use pinned-floor shapes",
				zap.Bool("v2PromptRoute", c.cached.V2PromptRoute),
				zap.Bool("modelRefIDKey", c.cached.ModelRefIDKey))
		}
	})
	return c.cached, nil
}

// probeTimeout bounds each capability probe. Probes are single-round-trip
// validation-error requests against a local agent process; anything slower
// than this is a transport problem, not a slow probe.
const probeTimeout = 5 * time.Second

// modelRefID is the Model.Ref wire shape observed on opencode >= 1.18.15
// (missing-key probe: `Missing key ["model"]["id"]`; a live 204 setModel
// with this shape was verified on the production runtime, #1119).
type modelRefID struct {
	ID         string `json:"id"`
	ProviderID string `json:"providerID"`
}

// modelRefWire converts the caller-facing V2ModelRef into the wire shape
// the probed binary expects. nil passes through as nil (no override).
// Indeterminate probes default to the pinned runtime floor (1.18.15,
// id-key shape) — the platform's runbook floor — and the incomplete probe
// was already warned about in Capabilities().
func (c *Client) modelRefWire(ctx context.Context, m *V2ModelRef) (any, error) {
	if m == nil {
		return nil, nil
	}
	caps, err := c.Capabilities(ctx)
	if err != nil {
		return nil, fmt.Errorf("probe capabilities for model ref: %w", err)
	}
	if caps.ModelRefIDKey {
		return modelRefID{ID: m.ModelID, ProviderID: m.ProviderID}, nil
	}
	return V2ModelRef{ModelID: m.ModelID, ProviderID: m.ProviderID}, nil
}

// postRaw issues a JSON POST with the standard opencode basic auth and
// returns the status code and (bounded) body. It is for capability probes:
// payloads are constructed to be rejected by validation, so a non-2xx is
// the expected outcome, never an error.
func (c *Client) postRaw(ctx context.Context, path string, body any) (int, []byte) {
	payload, err := json.Marshal(body)
	if err != nil {
		return 0, nil
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+path, bytes.NewReader(payload))
	if err != nil {
		return 0, nil
	}
	req.Header.Set("Content-Type", "application/json")
	req.SetBasicAuth(agentd.AuthUsername, c.password)
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return 0, nil
	}
	defer func() { _ = resp.Body.Close() }() //nolint:errcheck // probe path
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
	return resp.StatusCode, b
}
