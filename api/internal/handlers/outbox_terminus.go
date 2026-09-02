// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package handlers

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/lenaxia/llmsafespaces/api/internal/services/outbox"
)

// outbox_terminus.go — design 0055 M2/M4 + D1-B (Epic 69 US-69.8): the
// outbox's delivery terminus switches from the API-side adapter (+ the
// text-scan verify oracle) to the agentd delivery ledger.
//
//   - Delivery = POST/poll (D1-B): the deliverer POSTs (entryID, attempt)
//     to the pod's ABI surface (agentd :4097, §D1 Basic credential) and
//     polls the ledger to `admitted` within a short inline window. The
//     outbox never needs the event stream.
//   - I10 state mapping (enforced here, the ONLY consumer-side mapping):
//     ledger `admitted` ⟹ outbox completion (terminal, 0052 semantics);
//     `promoted`/`turn-ended`/`stalled` also complete (stalled additionally
//     alerts agentd-side — wake-only recovery is the ledger's);
//     `ledgered` past the inline window is a RETRYABLE error (the entry
//     stays delivering — agentd's own retry loop owns admission, and the
//     ledger is the truth source, so nothing here is ambiguous);
//     `failed` re-arms at attempt+1.
//   - I5/I6: on an outbox retry (Attempts > 0) the deliverer resolves the
//     PRIOR attempt's ledger state BEFORE re-POSTing — admitted is never
//     re-admitted (no second row, no duplicate turn); only a failed prior
//     attempt creates the attempt+1 row.
//
// The text-scan oracle (verifydelivery.go, deleted per the #1219
// admission-ID matrix disposition) was bypassed entirely on this
// terminus: the ledger IS the oracle (its disposition per the US-69.6
// spike: deleted on this path).

const (
	agentdDeliverInlineWindow = 10 * time.Second
	agentdDeliverPollEvery    = 100 * time.Millisecond
)

// agentdDeliverer is the outbox Deliverer against the pod's ledger.
type agentdDeliverer struct {
	// resolve maps (workspace, session) → (baseURL, password) with the
	// proxy's pod-IP re-resolution semantics (A7: resume-safe routing).
	resolve func(ctx context.Context, workspaceID, sessionID string) (baseURL, password string, err error)
	// client + baseURL are the pinned variant for tests with a fixed
	// stub; production always goes through resolve.
	client  *http.Client
	baseURL string
	// inlineWindow/pollEvery tune the D1-B inline-first window (tests
	// shrink them; production uses the constants).
	inlineWindow time.Duration
	pollEvery    time.Duration
}

func (d *agentdDeliverer) window() time.Duration {
	if d.inlineWindow > 0 {
		return d.inlineWindow
	}
	return agentdDeliverInlineWindow
}

func (d *agentdDeliverer) every() time.Duration {
	if d.pollEvery > 0 {
		return d.pollEvery
	}
	return agentdDeliverPollEvery
}

func (d *agentdDeliverer) endpoint(ctx context.Context, workspaceID, sessionID string) (string, string, error) {
	if d.resolve != nil {
		return d.resolve(ctx, workspaceID, sessionID)
	}
	return d.baseURL, "pw", nil
}

// deliver implements the terminus: check prior attempt → POST (if needed)
// → inline poll to a completion-eligible state.
func (d *agentdDeliverer) deliver(ctx context.Context, workspaceID, sessionID string, e outbox.Entry) error {
	base, pw, err := d.endpoint(ctx, workspaceID, sessionID)
	if err != nil {
		return fmt.Errorf("agentd terminus: resolve: %w", err)
	}

	// I10/I6: resolve the prior attempt first — a retry must never
	// manufacture a second turn.
	if e.Attempts > 0 {
		prior, err := ledgerLookup(ctx, d.httpClient(), base, pw, e.ID, attemptOf(e.Attempts))
		if err == nil {
			if done, _ := completionFor(prior); done {
				return nil // admitted-or-later: complete without re-POSTing
			}
			if prior == ledgerStateLedgered {
				// Admission still owned by agentd's retry loop — poll the
				// PRIOR attempt's window, never re-POST.
				st, timedOut, perr := pollToCompletion(ctx, d.httpClient(), base, pw, e.ID, attemptOf(e.Attempts), d.window(), d.every())
				if perr == nil {
					if done, _ := completionFor(st); done {
						return nil
					}
				}
				if timedOut {
					return &retryableError{fmt.Errorf("agentd terminus: attempt %d still %s (agentd owns admission)", e.Attempts, prior)}
				}
				return perr
			}
			// failed (or unknown non-completing terminal): fall through
			// to a fresh POST at attempt+1 — the re-arm path.
		}
		// not-found on the prior attempt (ledger rotated / pre-enable):
		// fall through to a fresh POST at attempt+1 — same as failed.
	}

	attempt := attemptOf(e.Attempts) + 1
	if _, err := ledgerPost(ctx, d.httpClient(), base, pw, sessionID, e.ID, attempt, e.Text, e.Model); err != nil {
		return err
	}
	st, timedOut, perr := pollToCompletion(ctx, d.httpClient(), base, pw, e.ID, attempt, d.window(), d.every())
	if perr != nil {
		return perr
	}
	if done, _ := completionFor(st); done {
		return nil
	}
	if timedOut {
		return &retryableError{fmt.Errorf("agentd terminus: ledgered but not admitted within %s (agentd owns admission)", agentdDeliverInlineWindow)}
	}
	return &retryableError{fmt.Errorf("agentd terminus: terminal state %s", st)}
}

// agentdHTTPClient bounds every pod-local request by the inline window: a
// hung agentd connection can never pin an outbox worker indefinitely
// (request contexts carry their own deadlines on top of this cap).
var agentdHTTPClient = &http.Client{Timeout: agentdDeliverInlineWindow}

func (d *agentdDeliverer) httpClient() *http.Client {
	if d.client != nil {
		return d.client
	}
	return agentdHTTPClient
}

// completionFor is the I10 mapping — the single place the ledger states
// map to outbox completion. admitted is the terminal 0052 semantic;
// promoted/turn-ended/stalled are strictly later-than-admitted (they imply
// it); stalled additionally triggers the agentd-side wake+alert (the
// ledger's, not the outbox's).
func completionFor(state string) (bool, string) {
	switch state {
	case ledgerStateAdmitted, ledgerStatePromoted, ledgerStateTurnEnded, ledgerStateStalled:
		return true, state
	default:
		return false, state
	}
}

const (
	ledgerStateLedgered  = "LEDGER_STATE_LEDGERED"
	ledgerStateAdmitted  = "LEDGER_STATE_ADMITTED"
	ledgerStatePromoted  = "LEDGER_STATE_PROMOTED"
	ledgerStateTurnEnded = "LEDGER_STATE_TURN_ENDED"
	ledgerStateStalled   = "LEDGER_STATE_STALLED"
	ledgerStateFailed    = "LEDGER_STATE_FAILED"
)

type retryableError struct{ err error }

func (r *retryableError) Error() string { return r.err.Error() }
func (r *retryableError) Unwrap() error { return r.err }

// attemptOf converts the outbox's int attempt counter to the ledger's
// uint32 (bounded by the retry policy — never near int/uint32 limits).
func attemptOf(n int) uint32 {
	return uint32(n) //nolint:gosec // G115: bounded by retry policy
}

func isAmbiguous(err error) bool {
	_, ok := err.(*outbox.AmbiguousError)
	return ok
}

// The Connect-protocol JSON transport (unary POST + response envelope),
// matching what the generated clients emit — kept dependency-light here so
// the deliverer has zero generated-code coupling in the API binary path.

const abiDeliverPath = "/llmsafespaces.abi.v1.HarnessABIService/Deliver"
const abiStatusPath = "/llmsafespaces.abi.v1.HarnessABIService/GetDeliveryStatus"

func (d *agentdDeliverer) post(ctx context.Context, path string, pw string, payload any, out any) error {
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, d.baseURL+path, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.SetBasicAuth("opencode", pw)
	req.Header.Set("Content-Type", "application/json")
	resp, err := d.httpClient().Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 400 {
		return fmt.Errorf("agentd terminus: %s: status %d: %s", path, resp.StatusCode, string(data))
	}
	if out == nil {
		return nil
	}
	// Connect JSON envelope: {"messageId":..., "code":..., "message":...}
	var env struct {
		Message json.RawMessage `json:"message"`
		Error   *struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(data, &env); err != nil {
		return fmt.Errorf("agentd terminus: decode envelope: %w", err)
	}
	if env.Error != nil {
		return fmt.Errorf("agentd terminus: %s: %s", env.Error.Code, env.Error.Message)
	}
	if len(env.Message) == 0 {
		return fmt.Errorf("agentd terminus: %s: empty message envelope", path)
	}
	return json.Unmarshal(env.Message, out)
}

func ledgerPost(ctx context.Context, hc *http.Client, base, pw, sessionID, entryID string, attempt uint32, text string, model json.RawMessage) (string, error) {
	d := &agentdDeliverer{client: hc, baseURL: base}
	parts := []map[string]any{{"text": text}}
	payload := map[string]any{
		"sessionId": sessionID,
		"entryId":   entryID,
		"attempt":   attempt,
		"parts":     parts,
	}
	if len(model) > 0 {
		var m struct {
			ID       string `json:"id"`
			Provider string `json:"provider"`
		}
		if json.Unmarshal(model, &m) == nil && m.ID != "" {
			payload["model"] = map[string]any{"id": m.ID, "provider": m.Provider}
		}
	}
	var ack struct {
		State string `json:"state"`
	}
	if err := d.post(ctx, abiDeliverPath, pw, payload, &ack); err != nil {
		return "", err
	}
	return ack.State, nil
}

func ledgerLookup(ctx context.Context, hc *http.Client, base, pw, entryID string, attempt uint32) (string, error) {
	d := &agentdDeliverer{client: hc, baseURL: base}
	var st struct {
		State string `json:"state"`
	}
	err := d.post(ctx, abiStatusPath, pw, map[string]any{"entryId": entryID, "attempt": attempt}, &st)
	return st.State, err
}

func pollToCompletion(ctx context.Context, hc *http.Client, base, pw, entryID string, attempt uint32, window, every time.Duration) (state string, timedOut bool, err error) {
	deadline := time.Now().Add(window)
	for {
		st, lerr := ledgerLookup(ctx, hc, base, pw, entryID, attempt)
		if lerr == nil {
			if done, _ := completionFor(st); done {
				return st, false, nil
			}
			if st == ledgerStateFailed {
				// Terminal for THIS attempt: retryable at the outbox
				// layer (attempt+1 re-arm) — the deliverer returns so the
				// outbox's own backoff governs the next POST.
				return st, false, &retryableError{fmt.Errorf("agentd terminus: attempt %d failed", attempt)}
			}
			state = st
		}
		if !time.Now().Before(deadline) {
			return state, true, nil
		}
		select {
		case <-ctx.Done():
			return state, false, ctx.Err()
		case <-time.After(every):
		}
	}
}

// ValidateDeliveryFlags enforces the M4 flag matrix at wiring time (D4:
// single regime — the illegal combination is a boot error, never a
// supported dual-delivery mode).
func ValidateDeliveryFlags(stateAuthority, v2Delivery bool) error {
	if stateAuthority && !v2Delivery {
		return fmt.Errorf("AGENTD_STATE_AUTHORITY requires OPENCODE_V2_DELIVERY (design 0055 M4: authority-on with V2-off is an illegal combination — single delivery regime)")
	}
	return nil
}
