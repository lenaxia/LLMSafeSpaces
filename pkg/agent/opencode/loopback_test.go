// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package opencode

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const seamTestPassword = "seam-test-pw"

// newSeamServer spins a fake opencode recording requests; the returned
// client targets it.
func newSeamServer(t *testing.T, h http.HandlerFunc) *Client {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return NewLoopbackClient(srv.URL, seamTestPassword)
}

// recordingFake is a minimal stateful fake opencode for the seam tests.
type recordingFake struct {
	mu         sync.Mutex
	created    []string // session titles, in order
	renamed    map[string]string
	deleted    []string
	sent       map[string][]map[string]any // sessionID -> message bodies
	summaries  map[string]map[string]string
	listBody   string
	statusBody string
	busy       bool
}

func newRecordingFake() *recordingFake {
	return &recordingFake{
		renamed:    map[string]string{},
		sent:       map[string][]map[string]any{},
		summaries:  map[string]map[string]string{},
		listBody:   `[{"id":"ses_1","title":"t","time":{"created":1,"updated":2}}]`,
		statusBody: `{}`,
	}
}

func (f *recordingFake) handler(t *testing.T) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// The §D1 gate: every seam request carries the Basic credential.
		user, pass, ok := r.BasicAuth()
		require.True(t, ok, "seam requests must be Basic-authenticated")
		require.Equal(t, "opencode", user)
		require.Equal(t, seamTestPassword, pass)

		f.mu.Lock()
		defer f.mu.Unlock()
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/session":
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			title, _ := body["title"].(string)
			f.created = append(f.created, title)
			_ = json.NewEncoder(w).Encode(map[string]any{"id": "ses_new", "title": title})
		case (r.Method == http.MethodPost || r.Method == http.MethodPatch) && strings.HasPrefix(r.URL.Path, "/session/ses_"):
			id := strings.SplitN(strings.TrimPrefix(r.URL.Path, "/session/"), "/", 2)[0]
			var raw map[string]any
			_ = json.NewDecoder(r.Body).Decode(&raw)
			if strings.HasSuffix(r.URL.Path, "/message") {
				f.sent[id] = append(f.sent[id], raw)
				_ = json.NewEncoder(w).Encode(map[string]any{
					"info":  map[string]any{"id": "msg_1", "role": "assistant", "modelID": "m1"},
					"parts": []map[string]any{{"type": "text", "text": "answer"}},
				})
				return
			}
			if strings.HasSuffix(r.URL.Path, "/summarize") {
				prov, _ := raw["providerID"].(string)
				model, _ := raw["modelID"].(string)
				f.summaries[id] = map[string]string{"providerID": prov, "modelID": model}
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write([]byte("true"))
				return
			}
			title, _ := raw["title"].(string)
			f.renamed[id] = title
			w.WriteHeader(http.StatusOK)
		case r.Method == http.MethodDelete:
			id := strings.TrimPrefix(r.URL.Path, "/session/")
			f.deleted = append(f.deleted, id)
			w.WriteHeader(http.StatusOK)
		case r.Method == http.MethodGet && r.URL.Path == "/session":
			_, _ = w.Write([]byte(f.listBody))
		case r.Method == http.MethodGet && r.URL.Path == "/session/status":
			if f.busy {
				_, _ = w.Write([]byte(`{"ses_1":{"type":"busy"}}`))
				return
			}
			_, _ = w.Write([]byte(f.statusBody))
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/context"):
			_, _ = w.Write([]byte(`{"data":[{"id":"m1"},{"id":"m2"}]}`))
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/config/providers"):
			_, _ = w.Write([]byte(`{"providers":[{"id":"p1","models":{"m1":{"id":"m1","limit":{"context":1000},"capabilities":{"input":{"image":true}}}}}]}`))
		default:
			t.Errorf("unexpected seam request: %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}
}

func TestSeam_SessionCreate_TitleOmittedWhenEmpty(t *testing.T) {
	f := newRecordingFake()
	c := newSeamServer(t, f.handler(t))

	id, err := c.SessionCreate(context.Background(), "")
	require.NoError(t, err)
	assert.Equal(t, "ses_new", id)
	require.Len(t, f.created, 1)
	assert.Equal(t, "", f.created[0])

	_, err = c.SessionCreate(context.Background(), "T")
	require.NoError(t, err)
	assert.Equal(t, "T", f.created[1])
}

func TestSeam_SessionCreate_Non2xx(t *testing.T) {
	c := newSeamServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":"boom"}`))
	})
	_, err := c.SessionCreate(context.Background(), "")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "500")
	assert.Contains(t, err.Error(), "/session")
}

func TestSeam_SessionRename_ExactWire(t *testing.T) {
	f := newRecordingFake()
	c := newSeamServer(t, f.handler(t))
	require.NoError(t, c.SessionRename(context.Background(), "ses_1", "New"))
	assert.Equal(t, "New", f.renamed["ses_1"])
}

// The verb is load-bearing: POST is accepted-and-ignored by the real
// agent (live-proven 2026-09-13). Pin PATCH.
func TestSeam_SessionRename_UsesPatchNotPost(t *testing.T) {
	var method, path string
	c := newSeamServer(t, func(w http.ResponseWriter, r *http.Request) {
		method, path = r.Method, r.URL.Path
		w.WriteHeader(http.StatusOK)
	})
	require.NoError(t, c.SessionRename(context.Background(), "ses_1", "t"))
	assert.Equal(t, http.MethodPatch, method)
	assert.Equal(t, "/session/ses_1", path)
}

func TestSeam_SessionRename_Non2xx(t *testing.T) {
	c := newSeamServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	})
	err := c.SessionRename(context.Background(), "ses_x", "t")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "404")
}

func TestSeam_SessionDelete(t *testing.T) {
	f := newRecordingFake()
	c := newSeamServer(t, f.handler(t))
	require.NoError(t, c.SessionDelete(context.Background(), "ses_1"))
	assert.Equal(t, []string{"ses_1"}, f.deleted)
}

func TestSeam_SessionSend_ModelOverrideObjectForm(t *testing.T) {
	f := newRecordingFake()
	c := newSeamServer(t, f.handler(t))

	res, err := c.SessionSend(context.Background(), "ses_1", "hello", "prov/model-x", nil)
	require.NoError(t, err)
	assert.Equal(t, "answer", res.Text)
	assert.Equal(t, "m1", res.ModelID)
	assert.Equal(t, "msg_1", res.MessageID)

	body := f.sent["ses_1"][0]
	model, ok := body["model"].(map[string]any)
	require.True(t, ok, "model override must be the OBJECT form — the string form 400s (#909)")
	assert.Equal(t, "model-x", model["modelID"])
	assert.Equal(t, "prov", model["providerID"])
	parts := body["parts"].([]any)
	require.Len(t, parts, 1)
	text := parts[0].(map[string]any)
	assert.Equal(t, "text", text["type"])
	assert.Equal(t, "hello", text["text"])
}

func TestSeam_SessionSend_NoModelOmitted(t *testing.T) {
	f := newRecordingFake()
	c := newSeamServer(t, f.handler(t))
	_, err := c.SessionSend(context.Background(), "ses_1", "hi", "", nil)
	require.NoError(t, err)
	_, has := f.sent["ses_1"][0]["model"]
	assert.False(t, has, "empty model must be omitted — session default applies")
}

func TestSeam_SessionSend_ImagePartsAsDataURLs(t *testing.T) {
	f := newRecordingFake()
	c := newSeamServer(t, f.handler(t))
	png := []byte{0x89, 0x50, 0x4e, 0x47}

	_, err := c.SessionSend(context.Background(), "ses_1", "look", "p/m", []ImageAttachment{
		{Filename: "a.png", MIME: "image/png", Data: png},
	})
	require.NoError(t, err)

	parts := f.sent["ses_1"][0]["parts"].([]any)
	require.Len(t, parts, 2)
	file := parts[1].(map[string]any)
	assert.Equal(t, "file", file["type"])
	assert.Equal(t, "image/png", file["mime"])
	assert.Equal(t, "a.png", file["filename"])
	wantURL := "data:image/png;base64," + base64.StdEncoding.EncodeToString(png)
	assert.Equal(t, wantURL, file["url"])
}

func TestSeam_SessionSend_BareModelRejectedPreCall(t *testing.T) {
	c := newSeamServer(t, func(w http.ResponseWriter, r *http.Request) {
		t.Error("no wire call may happen for an unexpressible model ref")
	})
	_, err := c.SessionSend(context.Background(), "ses_1", "t", "flatmodel", nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "provider/model")
}

func TestSeam_SplitModelRef(t *testing.T) {
	p, m, err := SplitModelRef("a/b")
	require.NoError(t, err)
	assert.Equal(t, "a", p)
	assert.Equal(t, "b", m)

	p, m, err = SplitModelRef("a/b/c")
	require.NoError(t, err)
	assert.Equal(t, "a", p)
	assert.Equal(t, "b/c", m)

	for _, bad := range []string{"flat", "/b", "a/", "  ", "a//"} {
		_, _, err := SplitModelRef(bad)
		assert.Error(t, err, "%q must be rejected", bad)
	}
}

func TestSeam_SessionSend_BusyBlocks(t *testing.T) {
	// The live-proven busy semantics: the POST does not 409 — it blocks.
	// A caller deadline must surface as a context error, never a hang.
	release := make(chan struct{})
	c := newSeamServer(t, func(w http.ResponseWriter, r *http.Request) {
		<-release
		w.WriteHeader(http.StatusOK)
	})
	defer close(release)
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	_, err := c.SessionSend(ctx, "ses_1", "t", "", nil)
	require.Error(t, err)
	assert.ErrorIs(t, err, context.DeadlineExceeded)
}

func TestSeam_SessionSummarize_ExactWire(t *testing.T) {
	f := newRecordingFake()
	c := newSeamServer(t, f.handler(t))
	require.NoError(t, c.SessionSummarize(context.Background(), "ses_1", "prov", "m"))
	assert.Equal(t, map[string]string{"providerID": "prov", "modelID": "m"}, f.summaries["ses_1"])
}

func TestSeam_SessionSummarize_V2AbsenceClass(t *testing.T) {
	c := newSeamServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(`{"message":"Session compact is not available yet"}`))
	})
	err := c.SessionSummarize(context.Background(), "ses_1", "p", "m")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "503")
}

func TestSeam_SessionList(t *testing.T) {
	f := newRecordingFake()
	c := newSeamServer(t, f.handler(t))
	list, err := c.SessionList(context.Background())
	require.NoError(t, err)
	require.Len(t, list, 1)
	assert.Equal(t, "ses_1", list[0].ID)
}

func TestSeam_SessionMessagesRaw_CursorHeader(t *testing.T) {
	c := newSeamServer(t, func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "2", r.URL.Query().Get("limit"))
		w.Header().Set("X-Next-Cursor", "cur_1")
		_, _ = w.Write([]byte(`[{"info":{"id":"m1"}}]`))
	})
	body, next, err := c.SessionMessagesRaw(context.Background(), "ses_1", 2, "")
	require.NoError(t, err)
	assert.Contains(t, string(body), "m1")
	assert.Equal(t, "cur_1", next)
}

func TestSeam_SessionMessagesRaw_CursorForwarded(t *testing.T) {
	c := newSeamServer(t, func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "cur_1", r.URL.Query().Get("before"), "cursor must ride the `before` param (SDK contract)")
		_, _ = w.Write([]byte(`[]`))
	})
	_, _, err := c.SessionMessagesRaw(context.Background(), "ses_1", 2, "cur_1")
	require.NoError(t, err)
}

func TestSeam_SessionMessageCount_BoundedWalk(t *testing.T) {
	pages := 0
	c := newSeamServer(t, func(w http.ResponseWriter, r *http.Request) {
		pages++
		w.Header().Set("X-Next-Cursor", fmt.Sprintf("cur_%d", pages))
		// Two full pages then a short one.
		if pages >= 3 {
			w.Header().Del("X-Next-Cursor")
			_, _ = w.Write([]byte(`[{"info":{"id":"x"}}]`))
			return
		}
		arr := make([]string, 500)
		for i := range arr {
			arr[i] = `{"info":{"id":"x"}}`
		}
		_, _ = w.Write([]byte("[" + strings.Join(arr, ",") + "]"))
	})
	n, err := c.SessionMessageCount(context.Background(), "ses_1")
	require.NoError(t, err)
	assert.Equal(t, 1001, n)
	assert.Equal(t, 3, pages)
}

func TestSeam_SessionContextCount(t *testing.T) {
	f := newRecordingFake()
	c := newSeamServer(t, f.handler(t))
	n, err := c.SessionContextCount(context.Background(), "ses_1")
	require.NoError(t, err)
	assert.Equal(t, 2, n)
}

func TestSeam_SessionPromptTokens_Epic36Formula(t *testing.T) {
	c := newSeamServer(t, func(w http.ResponseWriter, r *http.Request) {
		// Last assistant message carries input+cache.read+cache.write.
		_, _ = w.Write([]byte(`[
			{"info":{"role":"assistant","tokens":{"input":100,"cache":{"read":1000,"write":50}}}},
			{"info":{"role":"user"}},
			{"info":{"role":"assistant","tokens":{"input":84,"cache":{"read":46208,"write":0}}}}
		]`))
	})
	got := c.SessionPromptTokens(context.Background(), "ses_1")
	assert.Equal(t, int64(84+46208+0), got, "most recent assistant input+cache.read+cache.write")
}

func TestSeam_SessionPromptTokens_NoAssistant(t *testing.T) {
	c := newSeamServer(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`[{"info":{"role":"user"}}]`))
	})
	assert.Equal(t, int64(0), c.SessionPromptTokens(context.Background(), "ses_1"))
}

func TestSeam_ModelInfo(t *testing.T) {
	f := newRecordingFake()
	c := newSeamServer(t, f.handler(t))
	info, err := c.ModelInfo(context.Background(), "p1", "m1")
	require.NoError(t, err)
	assert.Equal(t, int64(1000), info.ContextLimit)
	assert.True(t, info.ImageInput)
}

func TestSeam_ModelInfo_NotFound(t *testing.T) {
	f := newRecordingFake()
	c := newSeamServer(t, f.handler(t))
	_, err := c.ModelInfo(context.Background(), "p1", "absent")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "catalog")
}

// Session IDs are interpolated into URL paths — hostile IDs must be
// rejected before ANY request is built (no dial, no path injection).
func TestSeam_SessionIDValidation(t *testing.T) {
	c := newSeamServer(t, func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("no request may reach the wire for an invalid session id: %s %s", r.Method, r.URL.Path)
	})
	ctx := context.Background()
	for _, bad := range []string{"ses_\x7f", "../etc", "a/b", "", "ses x", "ses%20x"} {
		assert.Error(t, c.SessionRename(ctx, bad, "t"), "rename %q", bad)
		assert.Error(t, c.SessionDelete(ctx, bad), "delete %q", bad)
		_, err := c.SessionSend(ctx, bad, "t", "", nil)
		assert.Error(t, err, "send %q", bad)
		assert.Error(t, c.SessionSummarize(ctx, bad, "p", "m"), "summarize %q", bad)
		_, _, err = c.SessionMessagesRaw(ctx, bad, 5, "")
		assert.Error(t, err, "messages %q", bad)
		_, err = c.SessionContextCount(ctx, bad)
		assert.Error(t, err, "context %q", bad)
		_, err = c.SessionMessageCount(ctx, bad)
		assert.Error(t, err, "count %q", bad)
	}
	// Valid opaque IDs still pass.
	assert.NoError(t, validateSessionID("ses_f66ef51e3ffeFahEENfR3r2ia4"))
}

// Mid-turn, the in-flight assistant message carries a zeroed token
// stamp until step completion (live-proven 2026-09-14: busy sessions
// reported no context usage because the scan stopped at the
// placeholder). The scan must skip zero-total stamps and report the
// last COMPLETED step.
func TestSeam_SessionPromptTokens_SkipsInFlightZeroStamp(t *testing.T) {
	c := newSeamServer(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`[
			{"info":{"role":"assistant","tokens":{"input":455,"cache":{"read":559168,"write":0}}}},
			{"info":{"role":"user"}},
			{"info":{"role":"assistant","tokens":{"input":0,"cache":{"read":0,"write":0}}}}
		]`))
	})
	got := c.SessionPromptTokens(context.Background(), "ses_1")
	assert.Equal(t, int64(455+559168), got, "must report the last COMPLETED step, skipping the zeroed in-flight stamp")
}

// First-turn mid-flight edge: the only assistant stamp is the zeroed
// in-flight one → 0 is the honest answer (nothing has completed yet).
func TestSeam_SessionPromptTokens_OnlyZeroStamps(t *testing.T) {
	c := newSeamServer(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`[
			{"info":{"role":"user"}},
			{"info":{"role":"assistant","tokens":{"input":0,"cache":{"read":0,"write":0}}}}
		]`))
	})
	assert.Equal(t, int64(0), c.SessionPromptTokens(context.Background(), "ses_1"))
}

func TestSeam_SessionAbort_ExactWire(t *testing.T) {
	var method, path, body string
	c := newSeamServer(t, func(w http.ResponseWriter, r *http.Request) {
		method, path = r.Method, r.URL.Path
		buf := make([]byte, r.ContentLength)
		_, _ = r.Body.Read(buf)
		body = string(buf)
		w.WriteHeader(http.StatusOK)
	})
	require.NoError(t, c.SessionAbort(context.Background(), "ses_1"))
	assert.Equal(t, http.MethodPost, method)
	assert.Equal(t, "/session/ses_1/abort", path)
	assert.Equal(t, "{}", body, "V1 abort takes an empty JSON body")
}

func TestSeam_SessionAbort_Non2xx(t *testing.T) {
	c := newSeamServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	})
	err := c.SessionAbort(context.Background(), "ses_x")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "404")
}

func TestSeam_SessionAbort_InvalidID(t *testing.T) {
	c := newSeamServer(t, func(w http.ResponseWriter, r *http.Request) {
		t.Error("no wire call for an invalid id")
	})
	assert.Error(t, c.SessionAbort(context.Background(), "../x"))
}
