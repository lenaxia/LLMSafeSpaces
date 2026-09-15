// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package opencode

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/lenaxia/llmsafespaces/pkg/agent"
	"github.com/lenaxia/llmsafespaces/pkg/session"
)

// textOnlyWedgeMarker is the live-verified #1307 wedge signature (issue
// body, reproduced 1:1 against the live litellm): a text-only provider
// (litellm model_info supports_vision:false) answers any replay carrying
// a non-text content part with a 400 whose body names the invalid
// messages.content.type. The marker is deliberately the full
// "messages.content.type is invalid" phrase — a tight, provider-agnostic
// substring of the real body — so unrelated 400s never classify.
const textOnlyWedgeMarker = "messages.content.type is invalid"

// textOnlyWedgeError classifies a non-2xx response body as the #1307
// wedge and renders the actionable error (cause + remediation, not the
// raw provider body alone). Returns nil when the response does not carry
// the signature — callers fall through to their generic status error.
//
// Deliberately route-agnostic (it lives in the shared httpError funnel):
// the marker string is replay-specific — it can only appear in a body an
// agent surfaced from a provider rejecting replayed content, on any send
// route (message POST, V2 prompt, summarize). Non-send 400s never carry
// it; if that ever changes, tighten to the send funnels.
func textOnlyWedgeError(path string, status int, body string) error {
	if status != http.StatusBadRequest || !strings.Contains(body, textOnlyWedgeMarker) {
		return nil
	}
	return fmt.Errorf("%w: %w: %s returned %d: the session's history contains an image, and the active model only accepts text — switch to a vision-capable model (PUT /workspaces/{id}/model or the model picker) to continue: %s",
		agent.ErrHTTPStatus, agent.ErrImageInTextOnlyHistory, path, status, body)
}

// --- Read-time repair (#1307, the #1374 pattern) ---

// imageOmissionNotice is the honest record written in place of image
// content when the session's active model is text-only: the user and the
// agent see WHY the image is gone and how to recover the session.
const imageOmissionNotice = "[image omitted] The active model only accepts text, so image content was removed from this view. Sends on this session fail until a vision-capable model is selected — switch models to continue."

// filePartData renders the Custom.Data payload for a file part. The
// data URL itself is never included; on downgrade the payload carries an
// explicit omission notice instead (omitted=true).
func filePartData(mime, filename string, omitted bool, notice string) json.RawMessage {
	payload := map[string]any{
		"type": "file",
		"mime": mime,
	}
	if filename != "" {
		payload["filename"] = filename
	}
	if omitted {
		payload["omitted"] = true
		payload["notice"] = notice
	}
	data, _ := json.Marshal(payload)
	return data
}

// deriveImageMIME returns the mime for a file part: the declared mime
// when present, else the mime embedded in a data URL ("data:image/png;
// base64,..." → "image/png" — the incident's minimal shape carried only
// a uri). Empty when the part is not identifiable as an image.
func deriveImageMIME(mime, dataURL string) string {
	if strings.HasPrefix(mime, "image/") {
		return mime
	}
	if rest, ok := strings.CutPrefix(dataURL, "data:"); ok {
		if mt, _, _ := strings.Cut(rest, ";"); strings.HasPrefix(mt, "image/") {
			return mt
		}
	}
	return ""
}

// imageMIME reports whether a file-part mime or data URL identifies an
// image (the incident shapes: mime "image/png"; url/uri
// "data:image/...;base64,...").
func imageMIME(mime, dataURL string) bool {
	return strings.HasPrefix(mime, "image/") || strings.HasPrefix(dataURL, "data:image/")
}

// imageDictPlaceholder is the runbook's surgery shape, platform-side: an
// embedded image dict in a tool's structured output is replaced by a
// text placeholder (never silently deleted).
func imageDictPlaceholder() map[string]any {
	return map[string]any{"type": "text", "text": imageOmissionNotice}
}

// looksLikeImageDict reports whether a decoded JSON object is an
// image-bearing dict (mime image/* or a data:image url/uri).
func looksLikeImageDict(m map[string]any) bool {
	mime, _ := m["mime"].(string)
	url, _ := m["url"].(string)
	uri, _ := m["uri"].(string)
	return imageMIME(mime, url) || imageMIME("", uri)
}

// toolOutputHasEmbeddedImage scans a tool part's raw output for embedded
// image dicts (the runbook's structured-copy shape — shallow walk of
// arrays/objects, matching the wire's actual nesting).
func toolOutputHasEmbeddedImage(raw json.RawMessage) bool {
	if len(raw) == 0 {
		return false
	}
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return false
	}
	return valueHasImageDict(v)
}

func valueHasImageDict(v any) bool {
	switch t := v.(type) {
	case map[string]any:
		if looksLikeImageDict(t) {
			return true
		}
		for _, child := range t {
			if valueHasImageDict(child) {
				return true
			}
		}
	case []any:
		for _, child := range t {
			if valueHasImageDict(child) {
				return true
			}
		}
	}
	return false
}

// replaceEmbeddedImageDicts returns raw with every embedded image dict
// replaced by the text placeholder (the runbook's surgery, applied at
// serve time). Returns the original bytes when nothing matched.
func replaceEmbeddedImageDicts(raw json.RawMessage) json.RawMessage {
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return raw
	}
	if !replaceImageDicts(&v) {
		return raw
	}
	out, err := json.Marshal(v)
	if err != nil {
		return raw
	}
	return out
}

func replaceImageDicts(v *any) bool {
	switch t := (*v).(type) {
	case map[string]any:
		if looksLikeImageDict(t) {
			*v = imageDictPlaceholder()
			return true
		}
		changed := false
		for k, child := range t {
			c := child
			if replaceImageDicts(&c) {
				t[k] = c
				changed = true
			}
		}
		return changed
	case []any:
		changed := false
		for i := range t {
			if replaceImageDicts(&t[i]) {
				changed = true
			}
		}
		return changed
	}
	return false
}

// stripEmbeddedImageData rewrites every embedded image dict in a raw
// JSON value to its metadata marker ({type:"file", mime, filename}),
// dropping the data URL. Applied at translate time: image BYTES never
// cross the seam regardless of model capability (a multi-MB base64 blob
// in a served history page is pure bloat — the platform transcript does
// not render it). The omission NOTICE is a separate, repair-time
// concern (only a text-only session gets one).
func stripEmbeddedImageData(raw json.RawMessage) json.RawMessage {
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return raw
	}
	if !stripImageData(&v) {
		return raw
	}
	out, err := json.Marshal(v)
	if err != nil {
		return raw
	}
	return out
}

func stripImageData(v *any) bool {
	switch t := (*v).(type) {
	case map[string]any:
		if looksLikeImageDict(t) {
			mime, _ := t["mime"].(string)
			filename, _ := t["filename"].(string)
			*v = map[string]any{"type": "file", "mime": mime, "filename": filename}
			return true
		}
		changed := false
		for k, child := range t {
			c := child
			if stripImageData(&c) {
				t[k] = c
				changed = true
			}
		}
		return changed
	case []any:
		changed := false
		for i := range t {
			if stripImageData(&t[i]) {
				changed = true
			}
		}
		return changed
	}
	return false
}

// StripImageDataURLs removes image data URLs from a raw opencode message
// array — the MCP session_read surface (#1307 review r1 finding 3): the
// raw store previously shipped multi-megabyte base64 blobs to MCP
// consumers. Every image-bearing dict (mime image/* or a data:image/
// url/uri) keeps its metadata, loses its url/uri bytes, and gains an
// explicit imageOmitted marker so the record stays honest. All other
// content is preserved. Non-image input round-trips unchanged.
func StripImageDataURLs(raw []byte) []byte {
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return raw
	}
	if !stripDataURLs(&v) {
		return raw
	}
	out, err := json.Marshal(v)
	if err != nil {
		return raw
	}
	return out
}

func stripDataURLs(v *any) bool {
	switch t := (*v).(type) {
	case map[string]any:
		changed := false
		if looksLikeImageDict(t) {
			delete(t, "url")
			delete(t, "uri")
			t["imageOmitted"] = true
			changed = true
		}
		for k, child := range t {
			c := child
			if stripDataURLs(&c) {
				t[k] = c
				changed = true
			}
		}
		return changed
	case []any:
		changed := false
		for i := range t {
			if stripDataURLs(&t[i]) {
				changed = true
			}
		}
		return changed
	}
	return false
}

// sessionHasImageParts is the fast local scan: does the served page carry
// ANY image-bearing content (file parts or tool outputs with embedded
// image dicts)? No remote calls — the repairOrphanedRunningTools
// scan-first discipline (the common page pays nothing).
func sessionHasImageParts(msgs []session.Message) bool {
	for i := range msgs {
		for _, p := range msgs[i].Parts {
			if p.Custom != nil && p.Custom.Kind == "file" {
				var fp struct {
					MIME string `json:"mime"`
				}
				if json.Unmarshal(p.Custom.Data, &fp) == nil && strings.HasPrefix(fp.MIME, "image/") {
					return true
				}
			}
			if p.Tool != nil && toolOutputHasEmbeddedImage(p.Tool.Output) {
				return true
			}
		}
	}
	return false
}

// repairTextOnlyHistoryImages is the #1307 read-time repair for
// already-wedged sessions, following repairOrphanedRunningTools (#1374)
// and its STRICT failure semantics: when the served page carries
// image-bearing content AND the session's active model is KNOWN
// text-only, the image content is downgraded to an explicit, honest
// omission notice naming the recovery (switch to a vision-capable
// model). Any failure to resolve the model or its capability leaves the
// transcript untouched — unknown capability is possibly-vision, and a
// transport failure must never read as "text-only" (the #1310 lesson).
// The harness's durable store is never written.
func repairTextOnlyHistoryImages(ctx context.Context, c *Client, sessionID string, msgs []session.Message) {
	if !sessionHasImageParts(msgs) {
		return // the common page carries no image — no remote calls
	}
	model, err := c.SessionModelRef(ctx, sessionID)
	if err != nil || model == nil {
		return // indeterminate — render as-is
	}
	info, err := c.ModelInfo(ctx, model.Provider, model.ID)
	if err != nil || info == nil {
		return // capability unknown — never strip (fail-safe direction)
	}
	if !info.ImageInputKnown {
		return // capability unknown — never strip (fail-safe direction)
	}
	if info.ImageInput {
		return
	}
	for i := range msgs {
		for j := range msgs[i].Parts {
			p := &msgs[i].Parts[j]
			if p.Custom != nil && p.Custom.Kind == "file" {
				var fp struct {
					MIME     string `json:"mime"`
					Filename string `json:"filename"`
				}
				if json.Unmarshal(p.Custom.Data, &fp) == nil && strings.HasPrefix(fp.MIME, "image/") {
					p.Custom.Data = filePartData(fp.MIME, fp.Filename, true, imageOmissionNotice)
				}
			}
			if p.Tool != nil {
				p.Tool.Output = replaceEmbeddedImageDicts(p.Tool.Output)
			}
		}
	}
}
