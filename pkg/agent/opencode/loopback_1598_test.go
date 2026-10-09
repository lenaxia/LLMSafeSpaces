// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package opencode

import (
	"context"
	"net/http"
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// --- #1598 ask 4: no-text responses carry their part detail -----------------

// TestSeam_SessionSend_NoTextParts_CapturesPartTypes pins that a
// response with no text parts still exposes every part's type in wire
// order plus the bounded excerpt of the first non-text part that
// carried text (the reasoning-only completion class — the classifier
// failure in the issue was undiagnosable without it).
func TestSeam_SessionSend_NoTextParts_CapturesPartTypes(t *testing.T) {
	c := newSeamServer(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"info":{"id":"msg_9","modelID":"cls"},"parts":[
			{"type":"step-start"},
			{"type":"reasoning","text":"the input is a positive sentiment"},
			{"type":"tool","name":"x"}
		]}`))
	})
	res, err := c.SessionSend(context.Background(), "ses_1", "classify", "p/cls", nil)
	require.NoError(t, err)
	assert.Equal(t, "", res.Text)
	assert.Equal(t, []string{"step-start", "reasoning", "tool"}, res.PartTypes, "part types in wire order, text parts included")
	assert.Equal(t, "the input is a positive sentiment", res.NonTextExcerpt, "first non-text part that carried text becomes the excerpt")
}

// TestSeam_SessionSend_ExcerptClippedToBound pins the excerpt bound:
// a 300-rune reasoning block is clipped to nonTextExcerptMaxRunes with
// an ellipsis marker — diagnosable, never a full completion leaked
// into an error string.
func TestSeam_SessionSend_ExcerptClippedToBound(t *testing.T) {
	long := strings.Repeat("r", nonTextExcerptMaxRunes+50)
	c := newSeamServer(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"info":{"id":"m","modelID":"m"},"parts":[{"type":"reasoning","text":"` + long + `"}]}`))
	})
	res, err := c.SessionSend(context.Background(), "ses_1", "t", "p/m", nil)
	require.NoError(t, err)
	require.Len(t, []rune(res.NonTextExcerpt), nonTextExcerptMaxRunes+1) // +1 ellipsis
	assert.True(t, strings.HasSuffix(res.NonTextExcerpt, "…"))
}

// TestSeam_SessionSend_EmptyResponse_Distinct pins that a response
// with NO parts at all yields empty PartTypes (zero parts is a
// different failure than wrong-shaped parts — the tool layer renders
// its own message for this shape).
func TestSeam_SessionSend_EmptyResponse_Distinct(t *testing.T) {
	c := newSeamServer(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"info":{"id":"m","modelID":"m"},"parts":[]}`))
	})
	res, err := c.SessionSend(context.Background(), "ses_1", "t", "p/m", nil)
	require.NoError(t, err)
	assert.Empty(t, res.PartTypes)
	assert.Empty(t, res.NonTextExcerpt)
}

// TestSeam_SessionSend_TextResponse_DetailHarmless: the part detail
// rides text-bearing responses too (callers ignore it; nothing about
// the success path changes).
func TestSeam_SessionSend_TextResponse_DetailHarmless(t *testing.T) {
	c := newSeamServer(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"info":{"id":"m","modelID":"m"},"parts":[
			{"type":"step-start"},
			{"type":"text","text":"the answer"}
		]}`))
	})
	res, err := c.SessionSend(context.Background(), "ses_1", "t", "p/m", nil)
	require.NoError(t, err)
	assert.Equal(t, "the answer", res.Text)
	assert.Equal(t, []string{"step-start", "text"}, res.PartTypes)
}

// --- #1598 ask 1: no concrete provider example in SplitModelRef ------------

// TestSeam_SplitModelRef_ErrorCarriesNoConcreteProvider pins that the
// degenerate-ref error illustrates the FORM only: a hardcoded provider
// example steers agents in workspaces where that provider is not
// configured straight into the unknown-provider refusal (the issue's
// own reproduction hit exactly this).
func TestSeam_SplitModelRef_ErrorCarriesNoConcreteProvider(t *testing.T) {
	_, _, err := SplitModelRef("classifier")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "provider-id/model-id")
	for _, banned := range []string{"anthropic", "openai", "claude", "gpt", "thekaocloud"} {
		assert.NotContains(t, err.Error(), banned, "no concrete provider/model example may appear")
	}
}

// --- #1598 ask 2: whole-catalog capability index ----------------------------

// TestSeam_ModelCapabilities_Index pins the one-call capability index
// (tri-state preserved: known-true, known-false, unknown-absent) that
// list_models joins for vision discovery.
func TestSeam_ModelCapabilities_Index(t *testing.T) {
	c := newSeamServer(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"providers":[{"id":"p","models":{` +
			`"vision":{"id":"vision","limit":{"context":1000},"capabilities":{"input":{"image":true}}},` +
			`"text":{"id":"text","limit":{"context":2000},"capabilities":{"input":{"image":false}}},` +
			`"declared":{"id":"declared","limit":{"context":3000},"capabilities":{"attachment":true,"input":{"image":false}}},` +
			`"nocaps":{"id":"nocaps","limit":{"context":4000}}}}]}`))
	})
	idx, err := c.ModelCapabilities(context.Background())
	require.NoError(t, err)
	require.Contains(t, idx, "p")

	vis := idx["p"]["vision"]
	assert.True(t, vis.ImageInputKnown)
	assert.True(t, vis.ImageInput)

	txt := idx["p"]["text"]
	assert.True(t, txt.ImageInputKnown)
	assert.False(t, txt.ImageInput)

	dec := idx["p"]["declared"]
	assert.True(t, dec.ImageInputKnown, "attachment signal alone makes it known")
	assert.True(t, dec.ImageInput, "attachment:true OR-merges over synthesized input.image:false")

	noc := idx["p"]["nocaps"]
	assert.False(t, noc.ImageInputKnown, "no capabilities block = unknown, never flattened to false")
	assert.False(t, noc.ImageInput)
}

// TestSeam_ModelCapabilities_RecordedFixture runs the index against
// the live-recorded 1.18.15 /config/providers shape (probe provider
// with attachment-declared and undeclared models, captured 2026-10-05).
func TestSeam_ModelCapabilities_RecordedFixture(t *testing.T) {
	raw, err := os.ReadFile("testdata/opencode-config-providers-recorded.json")
	require.NoError(t, err)
	c := newSeamServer(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(raw)
	})
	idx, err := c.ModelCapabilities(context.Background())
	require.NoError(t, err)

	declared := idx["probeprov"]["vision-declared"]
	assert.True(t, declared.ImageInputKnown)
	assert.True(t, declared.ImageInput, "recorded shape: attachment:true + synthesized image:false → vision-capable")

	undeclared := idx["probeprov"]["undeclared"]
	assert.True(t, undeclared.ImageInputKnown)
	assert.False(t, undeclared.ImageInput)
}

// TestSeam_ModelCapabilities_Non2xx: wire failure is an error, never
// a silently empty index (the caller decides fail-open).
func TestSeam_ModelCapabilities_Non2xx(t *testing.T) {
	c := newSeamServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	})
	_, err := c.ModelCapabilities(context.Background())
	require.Error(t, err)
}

// TestSeam_ModelCapabilities_SingleWireCall: the whole index is ONE
// GET /config/providers — N models must not mean N round-trips.
func TestSeam_ModelCapabilities_SingleWireCall(t *testing.T) {
	calls := 0
	c := newSeamServer(t, func(w http.ResponseWriter, r *http.Request) {
		calls++
		_, _ = w.Write([]byte(`{"providers":[{"id":"p","models":{"a":{"id":"a"},"b":{"id":"b"},"c":{"id":"c"}}}]}`))
	})
	_, err := c.ModelCapabilities(context.Background())
	require.NoError(t, err)
	assert.Equal(t, 1, calls)
}

// TestClipRunes bounds-checks the excerpt helper.
func TestClipRunes(t *testing.T) {
	assert.Equal(t, "", clipRunes("", 10))
	assert.Equal(t, "abc", clipRunes("abc", 10))
	assert.Equal(t, "abc…", clipRunes("abcdef", 3), "max runes of content, ellipsis marks the cut")
	assert.Equal(t, "", clipRunes("abc", 0))
}
