// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package workflows

// Trigger input resolution (design 0059 D6): ONE resolver, consumed by
// BOTH fire paths — Scheduler.fireWorkflowTarget (cron) and the webhook
// receiver. Before 0059 the two paths each wired envelope→run.Input by
// hand; they share it now.

import (
	"bytes"
	"encoding/json"
	"fmt"

	"github.com/lenaxia/llmsafespaces/pkg/types"
)

// TriggerInputSpec is the trigger-carried half of the resolution input:
// the mapping mode (InputFrom) and the optional static input document
// (Input). Zero values mean legacy envelope mode with no static input.
type TriggerInputSpec struct {
	InputFrom string
	Input     json.RawMessage
}

// NormalizeTriggerInputFrom maps an unset/empty mode to the legacy
// default "envelope" (rows scanned after migration 000031 always carry a
// value; in-memory rows may not).
func NormalizeTriggerInputFrom(inputFrom string) string {
	if inputFrom == "" {
		return types.TriggerInputFromEnvelope
	}
	return inputFrom
}

// TriggerOptedIn reports whether a trigger opted in to fire-time input
// validation (D3): any non-default inputFrom, or a static input document
// present. Legacy un-opted triggers keep byte-identical envelope behavior
// and no validation (§3.6).
func TriggerOptedIn(inputFrom string, input json.RawMessage) bool {
	return NormalizeTriggerInputFrom(inputFrom) != types.TriggerInputFromEnvelope || StaticInputPresent(input)
}

// StaticInputPresent reports whether raw carries a static input document:
// a non-nil, non-JSON-null document. The JSON literal null means "no
// static input" — same as an absent key (mirrors the #1433 null-means-
// absent convention on inputSchema).
func StaticInputPresent(raw json.RawMessage) bool {
	return len(raw) > 0 && !bytes.Equal(bytes.TrimSpace(raw), jsonNullLiteral)
}

// ResolveTriggerInput computes the input a fired run should carry per the
// trigger's mapping (design 0059 §3.4):
//
//	base := body mode → envelope.body; mapped → nil; else the envelope itself
//	no static input   → base (the envelope path returns the envelope bytes
//	                    VERBATIM — legacy triggers are byte-identical)
//	base is an object → shallow top-level merge, static keys win
//	otherwise         → the static document verbatim
//
// Shallow merge is deliberate (deep-merge semantics are unspecified
// territory; the first 90% use case is "provide `topic` at the top
// level"). The envelope stays reachable under its own keys in envelope
// mode; mapped mode exists for additionalProperties:false schemas a
// merged envelope would violate.
func ResolveTriggerInput(spec TriggerInputSpec, envelope json.RawMessage) (json.RawMessage, error) {
	var base json.RawMessage
	switch NormalizeTriggerInputFrom(spec.InputFrom) {
	case types.TriggerInputFromBody:
		body, err := envelopeBody(envelope)
		if err != nil {
			return nil, fmt.Errorf("extract envelope body: %w", err)
		}
		base = body
	case types.TriggerInputFromMapped:
		// The static document IS the input; there is no base.
	default:
		base = envelope
	}

	if !StaticInputPresent(spec.Input) {
		// No overlay: base bytes pass through untouched.
		return base, nil
	}

	var baseObj map[string]json.RawMessage
	if len(base) > 0 {
		// A decode error here means "base is a non-object JSON value" —
		// exactly the verbatim-static case below. Both fire paths marshal
		// the envelope themselves, so base is valid JSON by construction.
		_ = json.Unmarshal(base, &baseObj)
	}
	if baseObj != nil {
		var staticObj map[string]json.RawMessage
		if err := json.Unmarshal(spec.Input, &staticObj); err != nil || staticObj == nil {
			// Static input in envelope/body modes is validated to be an
			// object at write time (V5); a non-object here can only come
			// from a row written outside the API — fall through to
			// verbatim static.
			return spec.Input, nil
		}
		merged := make(map[string]json.RawMessage, len(baseObj)+len(staticObj))
		for k, v := range baseObj {
			merged[k] = v
		}
		for k, v := range staticObj {
			merged[k] = v // static wins
		}
		out, err := json.Marshal(merged)
		if err != nil {
			return nil, fmt.Errorf("merge static input: %w", err)
		}
		return out, nil
	}
	// Non-object base (body mode with an array/string/number payload) or
	// mapped mode: the static document is the run input verbatim.
	return spec.Input, nil
}

// envelopeBody extracts the parsed JSON payload from a webhook fire
// envelope (the receiver stores the posted document under "body").
func envelopeBody(envelope json.RawMessage) (json.RawMessage, error) {
	if len(envelope) == 0 {
		return nil, nil
	}
	var env struct {
		Body *json.RawMessage `json:"body"`
	}
	if err := json.Unmarshal(envelope, &env); err != nil {
		return nil, fmt.Errorf("envelope is not valid JSON: %w", err)
	}
	if env.Body == nil {
		return nil, nil
	}
	return *env.Body, nil
}

// envelopePropertySets pins the top-level key set each source's system
// envelope carries — the shapes built by Scheduler.fireWorkflowTarget
// (cron: engine.go) and HandleWebhook (webhook_receiver.go). V6's wiring
// guard compares a workflow schema's top-level `required` against these.
var envelopePropertySets = map[string][]string{
	types.TriggerSourceCron:    {"source", "received_at"},
	types.TriggerSourceWebhook: {"source", "received_at", "headers", "body"},
}

// RequiredNonEnvelopeProperties returns the top-level `required` property
// names of schema that the given source's envelope key set provides no
// value for — the V6 wiring-guard predicate (design 0059 D4). Syntactic
// only: no deep schema walk (fire-time validation is the backstop for
// opted-in triggers). A schema that cannot be parsed requires nothing
// beyond the envelope — the guard skips it (a garbage stored schema is a
// workflow defect, caught at fire time or on the next workflow update).
func RequiredNonEnvelopeProperties(schema json.RawMessage, sourceType string) []string {
	if isSchemalessInputSchema(schema) {
		return nil
	}
	var doc struct {
		Required []string `json:"required"`
	}
	if err := json.Unmarshal(schema, &doc); err != nil {
		return nil
	}
	envelopeKeys := envelopePropertySets[sourceType]
	var missing []string
	for _, req := range doc.Required {
		found := false
		for _, k := range envelopeKeys {
			if k == req {
				found = true
				break
			}
		}
		if !found {
			missing = append(missing, req)
		}
	}
	return missing
}
