// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package opencode

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lenaxia/llmsafespaces/pkg/agentd"
)

// client_modelinfo_test.go — pins ModelInfo's OR-merge of the two
// independent image-input signals in /config/providers:
//
//   - capabilities.input.image — opencode's models.dev catalog merge;
//     for custom-endpoint models this is a SYNTHESIZED false (probe
//     2026-10-05: every model of a custom provider carries the same
//     all-text-only block, multimodal ones included)
//   - capabilities.attachment — the credential-declared capability,
//     flowed config → catalog; the only truthful signal a custom
//     vision model can carry
//
// Either signal present = KNOWN; either true = vision-capable. No
// signal = UNKNOWN (#1307 fail-safe: never refuse on unknown).

func modelInfoServer(t *testing.T, body string) *Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/config/providers" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		user, pass, ok := r.BasicAuth()
		if !ok || user != agentd.AuthUsername || pass != "pw" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return NewClient(srv.URL, "pw", nil)
}

func TestModelInfo_ImageSignalVariants(t *testing.T) {
	cases := []struct {
		name string
		json string
		// wantKnown/wantInput describe the EXPECTED merged view.
		wantKnown bool
		wantInput bool
	}{
		{
			// models.dev-known vision model.
			name:      "image true",
			json:      `{"providers":[{"id":"p","models":{"m":{"id":"m","capabilities":{"input":{"image":true}}}}}]}`,
			wantKnown: true,
			wantInput: true,
		},
		{
			// models.dev-known text-only model.
			name:      "image false",
			json:      `{"providers":[{"id":"p","models":{"m":{"id":"m","capabilities":{"input":{"image":false}}}}}]}`,
			wantKnown: true,
			wantInput: false,
		},
		{
			// THE CLASSIFIER CASE: custom-endpoint vision model —
			// image is the synthesized false, attachment is the
			// credential's declared true. Declared truth wins the OR.
			name:      "attachment true overrides synthesized image false",
			json:      `{"providers":[{"id":"p","models":{"m":{"id":"m","capabilities":{"attachment":true,"input":{"image":false}}}}}]}`,
			wantKnown: true,
			wantInput: true,
		},
		{
			// Declared text-only: attachment false + synthesized false.
			name:      "attachment false image false",
			json:      `{"providers":[{"id":"p","models":{"m":{"id":"m","capabilities":{"attachment":false,"input":{"image":false}}}}}]}`,
			wantKnown: true,
			wantInput: false,
		},
		{
			// Attachment alone, image block absent.
			name:      "attachment true no image block",
			json:      `{"providers":[{"id":"p","models":{"m":{"id":"m","capabilities":{"attachment":true}}}}]}`,
			wantKnown: true,
			wantInput: true,
		},
		{
			// No capabilities at all: UNKNOWN, never text-only (#1307).
			name:      "no capabilities unknown",
			json:      `{"providers":[{"id":"p","models":{"m":{"id":"m"}}}]}`,
			wantKnown: false,
			wantInput: false,
		},
		{
			// Partial blocks stay unknown (pointerized all the way
			// down — #1307 review r1 finding 1).
			name:      "empty capabilities unknown",
			json:      `{"providers":[{"id":"p","models":{"m":{"id":"m","capabilities":{}}}}]}`,
			wantKnown: false,
			wantInput: false,
		},
		{
			name:      "input without image unknown",
			json:      `{"providers":[{"id":"p","models":{"m":{"id":"m","capabilities":{"input":{"text":true}}}}}]}`,
			wantKnown: false,
			wantInput: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := modelInfoServer(t, tc.json)
			info, err := c.ModelInfo(context.Background(), "p", "m")
			require.NoError(t, err)
			require.NotNil(t, info)
			assert.Equal(t, tc.wantKnown, info.ImageInputKnown)
			assert.Equal(t, tc.wantInput, info.ImageInput)
		})
	}
}

func TestModelInfo_ModelAbsent_Error(t *testing.T) {
	c := modelInfoServer(t, `{"providers":[{"id":"p","models":{"other":{"id":"other"}}}]}`)
	_, err := c.ModelInfo(context.Background(), "p", "m")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not found in the workspace catalog")
}
