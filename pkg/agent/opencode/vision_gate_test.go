// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package opencode

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/lenaxia/llmsafespaces/pkg/agent"
	"github.com/lenaxia/llmsafespaces/pkg/session"
)

// wedgeBody is the exact provider-failure surface from the #1307 incident
// (verified 1:1 against the live litellm): opencode answers the message
// POST with HTTP 400 and the upstream error body, and EVERY subsequent
// turn replays the same fatal history — the permanent-wedge signature.
const wedgeBody = `{"name":"ProviderServerError","data":{"message":"Provider request failed with HTTP 400: litellm.BadRequestError: ZaiException - messages.content.type is invalid, allowed values: ['text']"}}`

func TestAdapterSend_ClassifiesTextOnlyWedge400(t *testing.T) {
	tests := []struct {
		name       string
		status     int
		body       string
		wantWedges bool
	}{
		{
			name:       "incident signature",
			status:     http.StatusBadRequest,
			body:       wedgeBody,
			wantWedges: true,
		},
		{
			name:   "generic 400 is not classified",
			status: http.StatusBadRequest,
			body:   `{"name":"BadRequest","data":{"message":"Expected object | null"}}`,
		},
		{
			name:   "500 is not classified",
			status: http.StatusInternalServerError,
			body:   wedgeBody,
		},
		{
			name:   "200 does not classify",
			status: http.StatusOK,
			body:   wedgeBody,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tt.status)
				_, _ = w.Write([]byte(tt.body))
			}))
			defer srv.Close()
			a := newTestAdapter(t, srv)

			_, err := a.Send(context.Background(), "", "ws-1", "ses_wedge", "hello", session.SendOpts{})
			if tt.status < 400 {
				require.NoError(t, err)
				return
			}
			require.Error(t, err)
			require.ErrorIs(t, err, agent.ErrHTTPStatus, "definitive-rejection classification must be preserved")
			if tt.wantWedges {
				require.ErrorIs(t, err, agent.ErrImageInTextOnlyHistory, "wedge 400 must classify as image-in-text-only-history")
			} else {
				require.NotErrorIs(t, err, agent.ErrImageInTextOnlyHistory, "non-wedge failures must not classify")
			}
		})
	}
}

// TestAdapterSend_WedgeErrorMessageActionable pins the user-facing error
// text: it must name the cause (image in history, text-only model) so the
// raw provider body is not the only signal.
func TestAdapterSend_WedgeErrorMessageActionable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(wedgeBody))
	}))
	defer srv.Close()
	a := newTestAdapter(t, srv)

	_, err := a.Send(context.Background(), "", "ws-1", "ses_wedge", "hello", session.SendOpts{})
	require.Error(t, err)
	lowered := strings.ToLower(err.Error())
	require.Contains(t, lowered, "image")
	require.Contains(t, lowered, "text-only")
}

// TestAdapterSendAsync_ClassifiesTextOnlyWedge400 pins the same
// classification on the V2 prompt path (dormant but revival-wired — it
// must not regress the sentinel when revived).
func TestAdapterSendAsync_ClassifiesTextOnlyWedge400(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(wedgeBody))
	}))
	defer srv.Close()
	a := newTestAdapter(t, srv)

	_, err := a.SendAsync(context.Background(), "", "ws-1", "ses_wedge", "hello", session.SendOpts{})
	require.Error(t, err)
	require.ErrorIs(t, err, agent.ErrImageInTextOnlyHistory)
}

// --- #1307 read-time repair (the #1374 pattern: strict failure semantics) ---

// v1HistoryWithImage is the incident transcript shape: a user turn whose
// parts carry the read tool's PNG as a file part (mime + data URL).
const v1HistoryWithImage = `[{"info":{"id":"msg_img","sessionID":"ses_wedge","role":"user","time":{"created":1786374881330}},"parts":[
	{"type":"text","text":"read the screenshot"},
	{"type":"file","mime":"image/png","filename":"shot.png","url":"data:image/png;base64,aVBORw0KGgo="}
]},
{"info":{"id":"msg_img2","sessionID":"ses_wedge","role":"assistant","time":{"created":1786374885800}},"parts":[
	{"type":"text","text":"Here is what the screenshot shows."}
]}]`

const sessionGLM = `{"id":"ses_wedge","title":"w","model":{"id":"glm-5.3","providerID":"thekao"},"time":{"created":1,"updated":2}}`

func registerWedgeFlow(f *fakeOpencode, providersBody string, providersStatus int) {
	f.register("GET", "/session/ses_wedge/message", v1HistoryWithImage, 200)
	f.register("GET", "/session/ses_wedge", sessionGLM, 200)
	f.register("GET", "/config/providers", providersBody, providersStatus)
}

const providersTextOnly = `{"providers":[{"id":"thekao","models":{"glm-5.3":{"id":"glm-5.3","limit":{"context":1000},"capabilities":{"input":{"image":false}}}}}]}`
const providersVision = `{"providers":[{"id":"thekao","models":{"glm-5.3":{"id":"glm-5.3","limit":{"context":1000},"capabilities":{"input":{"image":true}}}}}]}`
const providersNoCap = `{"providers":[{"id":"thekao","models":{"glm-5.3":{"id":"glm-5.3","limit":{"context":1000}}}}]}`

func requireDowngradedImagePart(t *testing.T, msgs []session.Message) {
	t.Helper()
	found := false
	for _, m := range msgs {
		for _, p := range m.Parts {
			if p.Custom != nil && p.Custom.Kind == "file" {
				data := string(p.Custom.Data)
				require.Contains(t, data, "image/png", "file part keeps its mime metadata")
				require.Contains(t, data, "shot.png", "file part keeps its filename")
				require.Contains(t, data, "omitted", "downgrade must be an explicit omission notice")
				require.NotContains(t, data, "base64", "no image bytes on the wire")
				found = true
			}
		}
	}
	require.True(t, found, "served page must keep an honest file-part record")
}

func requireUndowngradedImagePart(t *testing.T, msgs []session.Message) {
	t.Helper()
	for _, m := range msgs {
		for _, p := range m.Parts {
			if p.Custom != nil && p.Custom.Kind == "file" {
				require.NotContains(t, string(p.Custom.Data), "omitted", "no downgrade may occur on this path")
			}
		}
	}
}

// TestGetHistory_FilePartCarriesMetadataNotBlob: the translated file part
// keeps mime+filename (an honest, renderable record) and NEVER the data
// URL — regardless of model capability, image bytes stay off the platform
// wire.
func TestGetHistory_FilePartCarriesMetadataNotBlob(t *testing.T) {
	f := newFakeOpencode(t)
	registerWedgeFlow(f, providersVision, 200)
	a := newTestAdapter(t, f.Server)

	msgs, err := a.GetHistory(context.Background(), "", "ws-1", "ses_wedge")
	require.NoError(t, err)
	require.Len(t, msgs, 2)
	encoded, err := json.Marshal(msgs)
	require.NoError(t, err)
	require.NotContains(t, string(encoded), "data:image", "image data URL must not reach the platform history")
	require.NotContains(t, string(encoded), "base64")
	requireUndowngradedImagePart(t, msgs)
	var sawFile bool
	for _, p := range msgs[0].Parts {
		if p.Custom != nil && p.Custom.Kind == "file" {
			sawFile = true
			require.Contains(t, string(p.Custom.Data), "image/png")
			require.Contains(t, string(p.Custom.Data), "shot.png")
		}
	}
	require.True(t, sawFile, "file part must survive translation as a Custom part")
}

// v1HistoryMinimalFilePart is the incident's EXACT minimal shape: a file
// part with a data-URL uri and NO mime field — detection must derive the
// image mime from the URL, not rely on a declared mime.
const v1HistoryMinimalFilePart = `[{"info":{"id":"msg_min","sessionID":"ses_wedge","role":"user","time":{"created":1786374881330}},"parts":[
	{"type":"file","uri":"data:image/png;base64,aVBORw0KGgo="}
]}]`

func TestGetHistory_Repair_MinimalFilePartShapeDerivedFromURI(t *testing.T) {
	f := newFakeOpencode(t)
	f.register("GET", "/session/ses_wedge/message", v1HistoryMinimalFilePart, 200)
	f.register("GET", "/session/ses_wedge", sessionGLM, 200)
	f.register("GET", "/config/providers", providersTextOnly, 200)
	a := newTestAdapter(t, f.Server)

	msgs, err := a.GetHistory(context.Background(), "", "ws-1", "ses_wedge")
	require.NoError(t, err)
	found := false
	for _, p := range msgs[0].Parts {
		if p.Custom != nil && p.Custom.Kind == "file" {
			found = true
			require.Contains(t, string(p.Custom.Data), "image/png", "mime derived from the data URL")
			require.Contains(t, string(p.Custom.Data), "omitted", "derived image part is downgraded for a text-only model")
		}
	}
	require.True(t, found)
}

// TestGetHistory_Repair_PartialCapabilitiesBlocksStayUnknown is the r1
// finding-1 regression: a PARTIAL capabilities block
// ("capabilities":{}, "input":{}, "input":{"text":true}) is UNKNOWN
// capability — the value-struct decode flattened these to known-false
// and the repair stripped images from possibly-vision models.
// TestStripImageDataURLs pins the MCP session_read surface (r1 finding
// 3): the raw opencode message array must never carry image data URLs —
// image-bearing dicts keep their metadata and gain an explicit omission
// marker, everything else is byte-preserving.
func TestStripImageDataURLs(t *testing.T) {
	raw := []byte(`[{"info":{"id":"m1","role":"user"},"parts":[
		{"type":"text","text":"read the screenshot"},
		{"type":"file","mime":"image/png","filename":"shot.png","url":"data:image/png;base64,aVBORw0KGgo="},
		{"type":"file","uri":"data:image/jpeg;base64,aGVsbG8="}
	]},
	{"info":{"id":"m2","role":"assistant"},"parts":[
		{"type":"tool","tool":"read","state":{"status":"completed","structured":{"parts":[{"type":"file","mime":"image/png","uri":"data:image/png;base64,aVBORw0KGgo="}]}}}
	]}]`)

	out := string(StripImageDataURLs(raw))
	require.NotContains(t, out, "base64", "no image bytes on the MCP surface")
	require.NotContains(t, out, "data:image", "no data URLs on the MCP surface")
	require.Contains(t, out, "image/png", "mime metadata preserved")
	require.Contains(t, out, "shot.png", "filename metadata preserved")
	require.Contains(t, out, "imageOmitted", "explicit omission marker")
	require.Contains(t, out, "read the screenshot", "non-image content untouched")

	// Non-image JSON round-trips byte-identically.
	plain := []byte(`[{"info":{"id":"m","role":"user"},"parts":[{"type":"text","text":"hi"}]}]`)
	require.JSONEq(t, string(plain), string(StripImageDataURLs(plain)))
}

func TestGetHistory_Repair_PartialCapabilitiesBlocksStayUnknown(t *testing.T) {
	shapes := []string{
		`{"providers":[{"id":"thekao","models":{"glm-5.3":{"id":"glm-5.3","capabilities":{}}}}]}`,
		`{"providers":[{"id":"thekao","models":{"glm-5.3":{"id":"glm-5.3","capabilities":{"input":{}}}}}]}`,
		`{"providers":[{"id":"thekao","models":{"glm-5.3":{"id":"glm-5.3","capabilities":{"input":{"text":true}}}}}]}`,
	}
	for _, providers := range shapes {
		f := newFakeOpencode(t)
		registerWedgeFlow(f, providers, 200)
		a := newTestAdapter(t, f.Server)

		msgs, err := a.GetHistory(context.Background(), "", "ws-1", "ses_wedge")
		require.NoError(t, err)
		requireUndowngradedImagePart(t, msgs)
	}
}

func TestGetHistory_Repair_TextOnlyModelDowngrades(t *testing.T) {
	f := newFakeOpencode(t)
	registerWedgeFlow(f, providersTextOnly, 200)
	a := newTestAdapter(t, f.Server)

	msgs, err := a.GetHistory(context.Background(), "", "ws-1", "ses_wedge")
	require.NoError(t, err)
	requireDowngradedImagePart(t, msgs)
	require.Contains(t, f.requests, "GET /session/ses_wedge", "session model must be resolved")
	require.Contains(t, f.requests, "GET /config/providers", "capability must be resolved")
}

func TestGetHistory_Repair_VisionModelNoDowngrade(t *testing.T) {
	f := newFakeOpencode(t)
	registerWedgeFlow(f, providersVision, 200)
	a := newTestAdapter(t, f.Server)

	msgs, err := a.GetHistory(context.Background(), "", "ws-1", "ses_wedge")
	require.NoError(t, err)
	requireUndowngradedImagePart(t, msgs)
}

// TestGetHistory_Repair_SessionFetchFailureLeavesTranscriptUntouched: a
// transport failure resolving the session model must never read as
// "text-only" — render as-is (the #1310 lesson carried into #1374's
// STRICT failure semantics).
func TestGetHistory_Repair_SessionFetchFailureLeavesTranscriptUntouched(t *testing.T) {
	f := newFakeOpencode(t)
	f.register("GET", "/session/ses_wedge/message", v1HistoryWithImage, 200)
	f.register("GET", "/session/ses_wedge", `{"error":"boom"}`, 500)
	f.register("GET", "/config/providers", providersTextOnly, 200)
	a := newTestAdapter(t, f.Server)

	msgs, err := a.GetHistory(context.Background(), "", "ws-1", "ses_wedge")
	require.NoError(t, err)
	requireUndowngradedImagePart(t, msgs)
	require.NotContains(t, f.requests, "GET /config/providers", "capability must not be fetched when the model is unresolvable")
}

// TestGetHistory_Repair_CapabilityFetchFailureLeavesTranscriptUntouched:
// unknown capability (catalog unreachable) is treated as possibly-vision —
// never strips (the #1307 fail-safe direction).
func TestGetHistory_Repair_CapabilityFetchFailureLeavesTranscriptUntouched(t *testing.T) {
	f := newFakeOpencode(t)
	registerWedgeFlow(f, `{"error":"boom"}`, 500)
	a := newTestAdapter(t, f.Server)

	msgs, err := a.GetHistory(context.Background(), "", "ws-1", "ses_wedge")
	require.NoError(t, err)
	requireUndowngradedImagePart(t, msgs)
}

// TestGetHistory_Repair_CapabilityAbsentLeavesTranscriptUntouched: a
// catalog entry without capability metadata is UNKNOWN — unknown is not
// text-only.
func TestGetHistory_Repair_CapabilityAbsentLeavesTranscriptUntouched(t *testing.T) {
	f := newFakeOpencode(t)
	registerWedgeFlow(f, providersNoCap, 200)
	a := newTestAdapter(t, f.Server)

	msgs, err := a.GetHistory(context.Background(), "", "ws-1", "ses_wedge")
	require.NoError(t, err)
	requireUndowngradedImagePart(t, msgs)
}

// TestGetHistory_Repair_NoModelOnSessionLeavesTranscriptUntouched: no
// session model → nothing to gate on.
func TestGetHistory_Repair_NoModelOnSessionLeavesTranscriptUntouched(t *testing.T) {
	f := newFakeOpencode(t)
	f.register("GET", "/session/ses_wedge/message", v1HistoryWithImage, 200)
	f.register("GET", "/session/ses_wedge", `{"id":"ses_wedge","title":"w"}`, 200)
	a := newTestAdapter(t, f.Server)

	msgs, err := a.GetHistory(context.Background(), "", "ws-1", "ses_wedge")
	require.NoError(t, err)
	requireUndowngradedImagePart(t, msgs)
}

// TestGetHistory_Repair_NoImagePartsMakesNoRemoteCalls: the common page
// carries no image part — the repair must not add a single request
// (the repairOrphanedRunningTools scan-first discipline).
func TestGetHistory_Repair_NoImagePartsMakesNoRemoteCalls(t *testing.T) {
	f := newFakeOpencode(t)
	f.register("GET", "/session/ses_plain/message", `[{"info":{"id":"m1","sessionID":"ses_plain","role":"user","time":{"created":1}},"parts":[{"type":"text","text":"hi"}]}]`, 200)
	a := newTestAdapter(t, f.Server)

	msgs, err := a.GetHistory(context.Background(), "", "ws-1", "ses_plain")
	require.NoError(t, err)
	require.Len(t, msgs, 1)
	require.Equal(t, []string{"GET /session/ses_plain/message"}, f.requests)
}

// v2HistoryWithImage is the V2-store twin: the image rides both a file
// content part and a tool state.structured copy (the runbook's dual-write).
const v2HistoryWithImage = `{"data":[
	{"id":"msg_a2","type":"assistant","time":{"created":2},"model":{"id":"glm-5.3","providerID":"thekao"},"content":[
		{"type":"text","id":"c1","text":"Reading the screenshot"},
		{"type":"tool","id":"c2","name":"read","state":{"status":"completed","input":{"filePath":"shot.png"},
			"structured":{"type":"tool","parts":[{"type":"text","text":"read shot.png"},{"type":"file","mime":"image/png","filename":"shot.png","uri":"data:image/png;base64,aVBORw0KGgo="}]}}},
		{"type":"file","id":"c3","mime":"image/png","filename":"shot.png","url":"data:image/png;base64,aVBORw0KGgo="}
	]}
]}`

func TestGetHistoryV2_RepairDowngradesFileAndToolStructuredImage(t *testing.T) {
	f := newFakeOpencode(t)
	f.register("GET", "/api/session/ses_wedge/message", v2HistoryWithImage, 200)
	f.register("GET", "/session/ses_wedge", sessionGLM, 200)
	f.register("GET", "/config/providers", providersTextOnly, 200)
	base := newTestAdapter(t, f.Server)
	a := NewAdapterV2(base)

	msgs, err := a.GetHistory(context.Background(), "", "ws-1", "ses_wedge")
	require.NoError(t, err)
	require.Len(t, msgs, 1)
	encoded, err := json.Marshal(msgs)
	require.NoError(t, err)
	require.NotContains(t, string(encoded), "base64", "V2 path must not leak image bytes (previously Raw rode Custom.Data verbatim)")
	require.NotContains(t, string(encoded), "data:image")

	// File content part: honest metadata + omission notice.
	sawFile, sawTool := false, false
	for _, p := range msgs[0].Parts {
		if p.Custom != nil && p.Custom.Kind == "file" {
			sawFile = true
			require.Contains(t, string(p.Custom.Data), "omitted")
			require.Contains(t, string(p.Custom.Data), "shot.png")
		}
		if p.Tool != nil && p.Tool.Name == "read" && len(p.Tool.Output) > 0 {
			sawTool = true
			out := string(p.Tool.Output)
			require.Contains(t, out, "omitted", "embedded image dict in structured output is replaced by a placeholder")
			require.Contains(t, out, "read shot.png", "non-image structured content survives")
		}
	}
	require.True(t, sawFile)
	require.True(t, sawTool)
}

// TestGetHistoryV2_FilePartStripsBlobRegardlessOfCapability: even on a
// vision-capable session the data URL never rides the platform wire.
func TestGetHistoryV2_FilePartStripsBlobRegardlessOfCapability(t *testing.T) {
	f := newFakeOpencode(t)
	f.register("GET", "/api/session/ses_wedge/message", v2HistoryWithImage, 200)
	f.register("GET", "/session/ses_wedge", sessionGLM, 200)
	f.register("GET", "/config/providers", providersVision, 200)
	base := newTestAdapter(t, f.Server)
	a := NewAdapterV2(base)

	msgs, err := a.GetHistory(context.Background(), "", "ws-1", "ses_wedge")
	require.NoError(t, err)
	encoded, err := json.Marshal(msgs)
	require.NoError(t, err)
	require.NotContains(t, string(encoded), "base64")
	sawFile := false
	for _, p := range msgs[0].Parts {
		if p.Custom != nil && p.Custom.Kind == "file" {
			sawFile = true
			require.Contains(t, string(p.Custom.Data), "shot.png")
			require.NotContains(t, string(p.Custom.Data), "omitted")
		}
	}
	require.True(t, sawFile)
}
