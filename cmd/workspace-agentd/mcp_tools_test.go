package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// withAgentServer swaps the agent addr to a fake opencode for one test
// and restores it after.
func withAgentServer(t *testing.T, h http.HandlerFunc) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	old := agentAddrAtomic.Load().(string)
	agentAddrAtomic.Store(srv.URL)
	t.Cleanup(func() { agentAddrAtomic.Store(old) })
	return srv
}

// fakeAgent is the stateful L1 fake: an in-memory opencode sufficient
// for every tool path — sessions, renames, sends (with part recording),
// statuses (busy set), summarize, context, catalog, message pages.
type fakeAgent struct {
	mu sync.Mutex

	nextID     int
	titles     map[string]string // id -> title
	renamed    map[string]string
	deleted    []string
	aborted    []string
	sentBodies map[string][]map[string]any // id -> decoded message bodies
	summaries  map[string]map[string]string
	busySet    map[string]bool
	models     map[string]string // session -> "prov/model"

	catalogImage map[string]bool // "prov/model" -> image input

	failCreate   bool
	failSend     bool
	sendEmpty    bool // return no text parts
	summarizeLat time.Duration
}

func newFakeAgent() *fakeAgent {
	return &fakeAgent{
		nextID:       1,
		titles:       map[string]string{},
		renamed:      map[string]string{},
		sentBodies:   map[string][]map[string]any{},
		summaries:    map[string]map[string]string{},
		busySet:      map[string]bool{},
		models:       map[string]string{},
		catalogImage: map[string]bool{"p/vision": true, "p/text": false},
	}
}

func (f *fakeAgent) newSession(title string) string {
	f.nextID++
	id := fmt.Sprintf("ses_%d", f.nextID)
	f.titles[id] = title
	f.models[id] = "p/text"
	return id
}

func (f *fakeAgent) handler(t *testing.T) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		user, pass, ok := r.BasicAuth()
		if !ok || user != "opencode" || pass != mcpTestPassword {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		f.mu.Lock()
		defer f.mu.Unlock()
		path := r.URL.Path
		switch {
		case r.Method == http.MethodPost && path == "/session":
			if f.failCreate {
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			title, _ := body["title"].(string)
			_, hasTitle := body["title"]
			if !hasTitle {
				// title omitted = auto-title (assertable via list)
				title = "auto"
			}
			id := f.newSession(title)
			_ = json.NewEncoder(w).Encode(map[string]any{"id": id, "title": title})
		case r.Method == http.MethodPost && strings.HasSuffix(path, "/summarize"):
			id := sessionIDFromPath(path, "/summarize")
			if f.summarizeLat > 0 {
				f.mu.Unlock()
				time.Sleep(f.summarizeLat)
				f.mu.Lock()
			}
			var body map[string]string
			_ = json.NewDecoder(r.Body).Decode(&body)
			f.summaries[id] = body
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("true"))
		case (r.Method == http.MethodPost || r.Method == http.MethodPatch) && strings.HasPrefix(path, "/session/"):
			id, action := splitSessionPath(path)
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			switch action {
			case "/message":
				if f.failSend {
					w.WriteHeader(http.StatusBadGateway)
					return
				}
				f.sentBodies[id] = append(f.sentBodies[id], body)
				if m, ok := body["model"].(map[string]any); ok {
					if pid, ok := m["providerID"].(string); ok {
						if mid, ok := m["modelID"].(string); ok {
							f.models[id] = pid + "/" + mid
						}
					}
				}
				parts := []map[string]any{{"type": "text", "text": "answer"}}
				if f.sendEmpty {
					parts = []map[string]any{{"type": "tool", "name": "x"}}
				}
				_ = json.NewEncoder(w).Encode(map[string]any{
					"info":  map[string]any{"id": "msg_1", "role": "assistant", "modelID": f.models[id]},
					"parts": parts,
				})
			case "/abort":
				// NOTE: no locking here — the handler holds f.mu for the
				// whole request (locked at entry); re-locking deadlocks.
				f.aborted = append(f.aborted, id)
				f.busySet[id] = false
				w.WriteHeader(http.StatusOK)
			default: // rename
				f.renamed[id], _ = body["title"].(string)
				w.WriteHeader(http.StatusOK)
			}
		case r.Method == http.MethodDelete && strings.HasPrefix(path, "/session/"):
			id := strings.TrimPrefix(path, "/session/")
			f.deleted = append(f.deleted, id)
			w.WriteHeader(http.StatusOK)
		case r.Method == http.MethodGet && path == "/session":
			var list []map[string]any
			for id, title := range f.titles {
				prov, model := "p", "text"
				if strings.Contains(f.models[id], "/") {
					prov, model, _ = strings.Cut(f.models[id], "/")
				}
				list = append(list, map[string]any{
					"id": id, "title": title, "agent": "build", "version": "1.18.15",
					"model": map[string]any{"id": model, "providerID": prov},
					"time":  map[string]int64{"created": 1789274402332, "updated": 1789274854576},
					"tokens": map[string]any{
						"input": 100, "output": 50, "reasoning": 10,
						"cache": map[string]int64{"read": 400, "write": 0},
					},
				})
			}
			_ = json.NewEncoder(w).Encode(list)
		case r.Method == http.MethodGet && path == "/session/status":
			out := map[string]map[string]string{}
			for id := range f.busySet {
				out[id] = map[string]string{"type": "busy"}
			}
			_ = json.NewEncoder(w).Encode(out)
		case r.Method == http.MethodGet && strings.Contains(path, "/context"):
			_, _ = w.Write([]byte(`{"data":[{"id":"m1"},{"id":"m2"},{"id":"m3"}]}`))
		case r.Method == http.MethodGet && strings.Contains(path, "/config/providers"):
			_, _ = w.Write([]byte(`{"providers":[{"id":"p","models":{
				"vision":{"id":"vision","limit":{"context":1000},"capabilities":{"input":{"image":true}}},
				"text":{"id":"text","limit":{"context":2000},"capabilities":{"input":{"image":false}}}
			}}]}`))
		case r.Method == http.MethodGet && strings.Contains(path, "/message"):
			_, _ = w.Write([]byte(`[{"info":{"role":"assistant","tokens":{"input":84,"cache":{"read":916,"write":0}}}}]`))
		default:
			t.Errorf("fakeAgent: unexpected %s %s", r.Method, path)
			w.WriteHeader(http.StatusNotFound)
		}
	}
}

func sessionIDFromPath(path, suffix string) string {
	return strings.TrimSuffix(strings.TrimPrefix(path, "/session/"), suffix)
}

func splitSessionPath(path string) (id, action string) {
	rest := strings.TrimPrefix(path, "/session/")
	if i := strings.Index(rest, "/"); i >= 0 {
		return rest[:i], rest[i:]
	}
	return rest, ""
}

func (f *fakeAgent) sentFor(id string) []map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]map[string]any{}, f.sentBodies[id]...)
}

func (f *fakeAgent) lastSummary(id string) map[string]string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.summaries[id]
}

// --- rename_session -------------------------------------------------------

func TestMCPRenameSession_HappyPath(t *testing.T) {
	f := newFakeAgent()
	withAgentServer(t, f.handler(t))

	out, err := mcpRenameSession(context.Background(), mcpTestPassword, "ses_9", "New Title")
	require.NoError(t, err)
	assert.Equal(t, "New Title", f.renamed["ses_9"])
	var res map[string]any
	require.NoError(t, json.Unmarshal([]byte(out), &res))
	assert.Equal(t, "renamed", res["status"])
}

func TestMCPRenameSession_MissingArgs(t *testing.T) {
	for _, fn := range []struct {
		name string
		call func() error
	}{
		{"no id", func() error { _, err := mcpRenameSession(context.Background(), mcpTestPassword, "", "t"); return err }},
		{"no title", func() error {
			_, err := mcpRenameSession(context.Background(), mcpTestPassword, "ses_1", "")
			return err
		}},
		{"blank title", func() error {
			_, err := mcpRenameSession(context.Background(), mcpTestPassword, "ses_1", "   ")
			return err
		}},
	} {
		require.Error(t, fn.call(), fn.name)
	}
}

func TestMCPRenameSession_TitleTooLong(t *testing.T) {
	_, err := mcpRenameSession(context.Background(), mcpTestPassword, "ses_1", strings.Repeat("a", 201))
	require.Error(t, err)
}

func TestMCPRenameSession_Non200(t *testing.T) {
	withAgentServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	})
	_, err := mcpRenameSession(context.Background(), mcpTestPassword, "ses_missing", "t")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "404")
}

// --- rename_workspace -----------------------------------------------------

func setupRenameWorkspaceEnv(t *testing.T, apiSrv *httptest.Server) {
	t.Helper()
	t.Setenv("WORKSPACE_ID", "ws-1")
	t.Setenv("LLMSAFESPACE_API_URL", apiSrv.URL)
	token := filepath.Join(t.TempDir(), "token")
	require.NoError(t, os.WriteFile(token, []byte("sa-token"), 0o600))
	t.Setenv("LLMSAFESPACE_BOOTSTRAP_TOKEN_FILE", token)
}

func TestMCPRenameWorkspace_HappyPath(t *testing.T) {
	var gotAuth, gotPath, gotBody string
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotPath = r.URL.Path
		buf := make([]byte, r.ContentLength)
		_, _ = r.Body.Read(buf)
		gotBody = string(buf)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer api.Close()
	setupRenameWorkspaceEnv(t, api)

	out, err := mcpRenameWorkspace(context.Background(), "  Renamed  ")
	require.NoError(t, err)
	assert.Equal(t, "Bearer sa-token", gotAuth)
	assert.Equal(t, "/internal/v1/workspace-rename", gotPath)
	assert.Contains(t, gotBody, `"workspaceID":"ws-1"`)
	assert.Contains(t, gotBody, `"name":"Renamed"`)
	var res map[string]any
	require.NoError(t, json.Unmarshal([]byte(out), &res))
	assert.Equal(t, "renamed", res["status"])
}

func TestMCPRenameWorkspace_MissingName(t *testing.T) {
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("API must not be called for an invalid name")
	}))
	defer api.Close()
	setupRenameWorkspaceEnv(t, api)

	_, err := mcpRenameWorkspace(context.Background(), "")
	require.Error(t, err)
	_, err = mcpRenameWorkspace(context.Background(), "   ")
	require.Error(t, err)
}

func TestMCPRenameWorkspace_NameTooLong(t *testing.T) {
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("API must not be called for an invalid name")
	}))
	defer api.Close()
	setupRenameWorkspaceEnv(t, api)

	_, err := mcpRenameWorkspace(context.Background(), strings.Repeat("a", 256))
	require.Error(t, err)
}

func TestMCPRenameWorkspace_MissingEnv(t *testing.T) {
	t.Setenv("WORKSPACE_ID", "")
	t.Setenv("LLMSAFESPACE_API_URL", "http://api.example.com")

	_, err := mcpRenameWorkspace(context.Background(), "x")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "WORKSPACE_ID")
}

func TestMCPRenameWorkspace_MissingTokenFile(t *testing.T) {
	t.Setenv("WORKSPACE_ID", "ws-1")
	t.Setenv("LLMSAFESPACE_API_URL", "http://api.example.com")
	t.Setenv("LLMSAFESPACE_BOOTSTRAP_TOKEN_FILE", filepath.Join(t.TempDir(), "absent"))

	_, err := mcpRenameWorkspace(context.Background(), "x")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "token")
}

func TestMCPRenameWorkspace_APIRejects(t *testing.T) {
	for _, tc := range []struct {
		status int
		want   string
	}{
		{http.StatusUnauthorized, "identity"},
		{http.StatusForbidden, "identity"},
		{http.StatusNotFound, "not found"},
		{http.StatusInternalServerError, "500"},
	} {
		api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(tc.status)
		}))
		setupRenameWorkspaceEnv(t, api)

		_, err := mcpRenameWorkspace(context.Background(), "x")
		require.Error(t, err, "status %d must error", tc.status)
		assert.Contains(t, err.Error(), tc.want)
		api.Close()
	}
}

// --- call_with_model ------------------------------------------------------

func TestMCPCallWithModel_HappyPath_WireContract(t *testing.T) {
	f := newFakeAgent()
	withAgentServer(t, f.handler(t))

	out, err := mcpCallWithModel(context.Background(), mcpTestPassword, "describe this", "p/vision", nil)
	require.NoError(t, err)

	// The carrier: one created session (titled for auditability) that is
	// deleted after the call.
	require.Len(t, f.titles, 1)
	var carrierID string
	for id := range f.titles {
		carrierID = id
	}
	assert.Equal(t, "call_with_model", f.titles[carrierID])
	assert.Equal(t, []string{carrierID}, f.deleted)

	// The send: model override in the OBJECT form + text part.
	bodies := f.sentFor(carrierID)
	require.Len(t, bodies, 1)
	model, ok := bodies[0]["model"].(map[string]any)
	require.True(t, ok, "model override must be the V1 object form")
	assert.Equal(t, "vision", model["modelID"])
	assert.Equal(t, "p", model["providerID"])
	parts := bodies[0]["parts"].([]any)
	require.Len(t, parts, 1)
	assert.Equal(t, "describe this", parts[0].(map[string]any)["text"])

	var res map[string]any
	require.NoError(t, json.Unmarshal([]byte(out), &res))
	assert.Equal(t, "vision", res["model"])
	assert.Equal(t, "answer", res["text"])
}

func TestMCPCallWithModel_ImagesAsDataURLs(t *testing.T) {
	f := newFakeAgent()
	withAgentServer(t, f.handler(t))
	dir := t.TempDir()
	png := filepath.Join(dir, "pic.PNG")
	pngBytes := []byte{0x89, 0x50, 0x4e, 0x47, 0x00}
	require.NoError(t, os.WriteFile(png, pngBytes, 0o600))

	_, err := mcpCallWithModel(context.Background(), mcpTestPassword, "look", "p/vision", []string{png})
	require.NoError(t, err)

	var carrierID string
	for id := range f.titles {
		carrierID = id
	}
	parts := f.sentFor(carrierID)[0]["parts"].([]any)
	require.Len(t, parts, 2)
	file := parts[1].(map[string]any)
	assert.Equal(t, "file", file["type"])
	assert.Equal(t, "image/png", file["mime"], "extension match must be case-insensitive")
	assert.Equal(t, "pic.PNG", file["filename"])
	want := "data:image/png;base64," + base64.StdEncoding.EncodeToString(pngBytes)
	assert.Equal(t, want, file["url"])
}

func TestMCPCallWithModel_VisionIncapableModelRefused(t *testing.T) {
	f := newFakeAgent()
	withAgentServer(t, f.handler(t))
	dir := t.TempDir()
	png := filepath.Join(dir, "a.png")
	require.NoError(t, os.WriteFile(png, []byte("x"), 0o600))

	_, err := mcpCallWithModel(context.Background(), mcpTestPassword, "look", "p/text", []string{png})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "image input")
	assert.Empty(t, f.sentBodies, "no send may happen for a refused model")
	assert.Empty(t, f.deleted, "no carrier was created")
}

func TestMCPCallWithModel_BadImageInputs(t *testing.T) {
	f := newFakeAgent()
	withAgentServer(t, f.handler(t))
	dir := t.TempDir()
	big := filepath.Join(dir, "big.png")
	require.NoError(t, os.WriteFile(big, make([]byte, imageMaxFileBytes+1), 0o600))
	txt := filepath.Join(dir, "a.txt")
	require.NoError(t, os.WriteFile(txt, []byte("x"), 0o600))
	absent := filepath.Join(dir, "nope.png")

	for _, tc := range []struct {
		path string
		want string
	}{
		{big, "per-image cap"},
		{txt, "unsupported image type"},
		{absent, "unreadable"},
	} {
		_, err := mcpCallWithModel(context.Background(), mcpTestPassword, "p", "p/vision", []string{tc.path})
		require.Error(t, err)
		assert.Contains(t, err.Error(), tc.want)
	}
	assert.Empty(t, f.titles, "guards run before any carrier is created")
}

func TestMCPCallWithModel_CombinedCap(t *testing.T) {
	f := newFakeAgent()
	withAgentServer(t, f.handler(t))
	dir := t.TempDir()
	var paths []string
	for i := 0; i < 3; i++ {
		p := filepath.Join(dir, fmt.Sprintf("img%d.png", i))
		require.NoError(t, os.WriteFile(p, make([]byte, 3<<20), 0o600)) // 3 MiB each: per-file OK, 9 MiB combined
		paths = append(paths, p)
	}

	_, err := mcpCallWithModel(context.Background(), mcpTestPassword, "p", "p/vision", paths)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "combined cap")
}

func TestMCPCallWithModel_BareModelRejected(t *testing.T) {
	f := newFakeAgent()
	withAgentServer(t, f.handler(t))
	_, err := mcpCallWithModel(context.Background(), mcpTestPassword, "p", "flatmodel", nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "provider/model")
	assert.Empty(t, f.titles)
}

func TestMCPCallWithModel_EmptyPromptRejected(t *testing.T) {
	_, err := mcpCallWithModel(context.Background(), mcpTestPassword, "  ", "a/b", nil)
	require.Error(t, err)
}

func TestMCPCallWithModel_CreateFails(t *testing.T) {
	f := newFakeAgent()
	f.failCreate = true
	withAgentServer(t, f.handler(t))
	_, err := mcpCallWithModel(context.Background(), mcpTestPassword, "p", "p/vision", nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "carrier session")
}

func TestMCPCallWithModel_MessageFailsStillCleansUp(t *testing.T) {
	f := newFakeAgent()
	f.failSend = true
	withAgentServer(t, f.handler(t))

	_, err := mcpCallWithModel(context.Background(), mcpTestPassword, "p", "p/vision", nil)
	require.Error(t, err)
	assert.Len(t, f.deleted, 1, "carrier must be deleted even when the send fails")
}

func TestMCPCallWithModel_NoTextParts(t *testing.T) {
	f := newFakeAgent()
	f.sendEmpty = true
	withAgentServer(t, f.handler(t))
	_, err := mcpCallWithModel(context.Background(), mcpTestPassword, "p", "p/vision", nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no text")
	assert.Len(t, f.deleted, 1)
}

// --- create_session -------------------------------------------------------

func TestMCPCreateSession_HappyPath(t *testing.T) {
	f := newFakeAgent()
	f.summarizeLat = 0
	var sendDone = make(chan struct{})
	srv := withAgentServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/message") {
			f.handler(t)(w, r)
			select {
			case <-sendDone:
			default:
				close(sendDone)
			}
			return
		}
		f.handler(t)(w, r)
	})

	start := time.Now()
	out, err := mcpCreateSession(context.Background(), mcpTestPassword, "go build the feature", "Feature build")
	elapsed := time.Since(start)
	require.NoError(t, err)
	assert.Less(t, elapsed, time.Second, "create_session must return before delivery blocks")

	var res map[string]any
	require.NoError(t, json.Unmarshal([]byte(out), &res))
	id := res["session_id"].(string)
	assert.Equal(t, "created", res["status"])
	assert.Equal(t, "Feature build", f.titles[id])

	select {
	case <-sendDone:
	case <-time.After(5 * time.Second):
		t.Fatal("background prompt delivery never happened")
	}
	require.Eventually(t, func() bool {
		return len(f.sentFor(id)) == 1
	}, 5*time.Second, 50*time.Millisecond)
	bodies := f.sentFor(id)
	parts := bodies[0]["parts"].([]any)
	assert.Equal(t, "go build the feature", parts[0].(map[string]any)["text"])
	_, hasModel := bodies[0]["model"]
	assert.False(t, hasModel, "no model override — the new session runs its own default")
	_ = srv
}

func TestMCPCreateSession_EmptyPrompt(t *testing.T) {
	withAgentServer(t, func(w http.ResponseWriter, r *http.Request) {
		t.Error("no opencode call may happen without a prompt")
	})
	_, err := mcpCreateSession(context.Background(), mcpTestPassword, "  ", "")
	require.Error(t, err)
}

func TestMCPCreateSession_CreateFails(t *testing.T) {
	f := newFakeAgent()
	f.failCreate = true
	withAgentServer(t, f.handler(t))
	_, err := mcpCreateSession(context.Background(), mcpTestPassword, "p", "")
	require.Error(t, err)
}

func TestMCPCreateSession_TitleOptional(t *testing.T) {
	f := newFakeAgent()
	withAgentServer(t, f.handler(t))
	out, err := mcpCreateSession(context.Background(), mcpTestPassword, "p", "")
	require.NoError(t, err)
	require.Len(t, f.titles, 1)
	var id string
	for k := range f.titles {
		id = k
	}
	assert.Equal(t, "auto", f.titles[id], "empty title omitted → agent auto-titles")
	assert.Contains(t, out, id)
}

// --- get_datetime ---------------------------------------------------------

func TestMCPGetDatetime(t *testing.T) {
	out, err := mcpGetDatetime()
	require.NoError(t, err)

	var res map[string]any
	require.NoError(t, json.Unmarshal([]byte(out), &res))
	for _, k := range []string{"utc", "local", "timezone", "utc_offset"} {
		require.Contains(t, res, k)
		require.NotEmpty(t, res[k], "%s must be non-empty", k)
	}
	utc, err := time.Parse(time.RFC3339, res["utc"].(string))
	require.NoError(t, err, "utc must be RFC3339")
	assert.Equal(t, time.UTC.String(), utc.Location().String())
	local, err := time.Parse(time.RFC3339, res["local"].(string))
	require.NoError(t, err, "local must be RFC3339 (carries its own offset)")
	assert.WithinDuration(t, utc, local, 2*time.Minute, "utc and local are the same instant")
	assert.Regexp(t, `^[+-]\d{2}:\d{2}$|^Z$`, res["utc_offset"].(string))
}

// --- session_metadata -----------------------------------------------------

func TestMCPSessionMetadata_AllSessions(t *testing.T) {
	f := newFakeAgent()
	s1 := f.newSession("one")
	s2 := f.newSession("two")
	f.busySet[s1] = true
	withAgentServer(t, f.handler(t))
	t.Setenv("WORKSPACE_ID", "ws-meta")

	out, err := mcpSessionMetadata(context.Background(), mcpTestPassword, "")
	require.NoError(t, err)
	var res map[string]any
	require.NoError(t, json.Unmarshal([]byte(out), &res))

	assert.Equal(t, "ws-meta", res["workspace_id"])
	assert.Equal(t, "1.18.15", res["agent_version"])
	sessions := res["sessions"].([]any)
	require.Len(t, sessions, 2)
	byID := map[string]map[string]any{}
	for _, s := range sessions {
		m := s.(map[string]any)
		byID[m["session_id"].(string)] = m
	}
	require.Contains(t, byID, s1)
	require.Contains(t, byID, s2)
	one := byID[s1]
	assert.Equal(t, true, one["busy"])
	assert.Equal(t, float64(3), one["in_context"])
	assert.Equal(t, float64(1000), one["context_tokens"], "Epic36 formula from the fake's message page")
	assert.Equal(t, float64(2000), one["context_limit"], "p/text catalog limit")
	assert.InDelta(t, 0.5, one["context_fill"], 0.0001, "1000/2000")
	assert.Equal(t, "p/text", one["model"])
	assert.Equal(t, float64(160), one["tokens_total"], "input+output+reasoning")
	assert.NotEmpty(t, one["age"])
	assert.NotEmpty(t, one["created_at"])
	assert.Equal(t, false, byID[s2]["busy"])
}

func TestMCPSessionMetadata_SingleSession(t *testing.T) {
	f := newFakeAgent()
	s1 := f.newSession("one")
	f.newSession("two")
	withAgentServer(t, f.handler(t))

	out, err := mcpSessionMetadata(context.Background(), mcpTestPassword, s1)
	require.NoError(t, err)
	var res map[string]any
	require.NoError(t, json.Unmarshal([]byte(out), &res))
	assert.Len(t, res["sessions"].([]any), 1)
}

func TestMCPSessionMetadata_UnknownSession(t *testing.T) {
	f := newFakeAgent()
	withAgentServer(t, f.handler(t))
	_, err := mcpSessionMetadata(context.Background(), mcpTestPassword, "ses_absent")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not found")
}

// Secrecy contract: the output carries no credential material, no env
// values beyond WORKSPACE_ID, no platform internals.
func TestMCPSessionMetadata_Secrecy(t *testing.T) {
	f := newFakeAgent()
	f.newSession("one")
	withAgentServer(t, f.handler(t))
	t.Setenv("WORKSPACE_ID", "ws-meta")
	t.Setenv("LLMSAFESPACE_API_URL", "http://llmsafespaces-api.llmsafespaces.svc:8080")
	t.Setenv("AGENTD_ADMIN_TOKEN", "super-secret-admin-token")

	out, err := mcpSessionMetadata(context.Background(), mcpTestPassword, "")
	require.NoError(t, err)
	for _, forbidden := range []string{
		mcpTestPassword,
		"super-secret-admin-token",
		"svc.cluster.local",
		"Authorization",
		"127.0.0.1",
		"password",
	} {
		assert.NotContains(t, out, forbidden, "metadata must not leak %q", forbidden)
	}
}

// --- compact --------------------------------------------------------------

func TestMCPCompact_IdleSynchronous(t *testing.T) {
	f := newFakeAgent()
	s1 := f.newSession("one")
	withAgentServer(t, f.handler(t))

	out, err := mcpCompact(context.Background(), mcpTestPassword, s1, "")
	require.NoError(t, err)
	assert.Contains(t, out, "compacted")
	summary := f.lastSummary(s1)
	require.NotNil(t, summary)
	// Default summarizing model = the session's current model.
	assert.Equal(t, "p", summary["providerID"])
	assert.Equal(t, "text", summary["modelID"])
}

func TestMCPCompact_ExplicitModel(t *testing.T) {
	f := newFakeAgent()
	s1 := f.newSession("one")
	withAgentServer(t, f.handler(t))

	_, err := mcpCompact(context.Background(), mcpTestPassword, s1, "p/vision")
	require.NoError(t, err)
	summary := f.lastSummary(s1)
	assert.Equal(t, "p", summary["providerID"])
	assert.Equal(t, "vision", summary["modelID"])
}

func TestMCPCompact_BusyScheduled(t *testing.T) {
	f := newFakeAgent()
	s1 := f.newSession("mine")
	f.busySet[s1] = true
	withAgentServer(t, f.handler(t))

	start := time.Now()
	out, err := mcpCompact(context.Background(), mcpTestPassword, s1, "")
	elapsed := time.Since(start)
	require.NoError(t, err)
	assert.Less(t, elapsed, time.Second, "busy compact must return immediately (run-at-boundary)")
	assert.Contains(t, out, "scheduled")

	require.Eventually(t, func() bool {
		return f.lastSummary(s1) != nil
	}, 5*time.Second, 50*time.Millisecond, "detached summarize must fire")
}

func TestMCPCompact_OmittedID_ResolvesSingleBusy(t *testing.T) {
	f := newFakeAgent()
	s1 := f.newSession("mine")
	f.busySet[s1] = true
	withAgentServer(t, f.handler(t))

	out, err := mcpCompact(context.Background(), mcpTestPassword, "", "")
	require.NoError(t, err)
	assert.Contains(t, out, s1)

	// Wait for the detached summarize before ending: the goroutine
	// outlives this test otherwise, and once withAgentServer's cleanup
	// closes the stub its error path reads the package-level `log` —
	// racing the next test's withObservedLog global swap (CI data race,
	// 2026-09-14). BusyScheduled waits for the same reason.
	require.Eventually(t, func() bool {
		return f.lastSummary(s1) != nil
	}, 5*time.Second, 50*time.Millisecond, "detached summarize must fire before the test ends")
}

func TestMCPCompact_OmittedID_NoBusy(t *testing.T) {
	f := newFakeAgent()
	f.newSession("idle")
	withAgentServer(t, f.handler(t))

	_, err := mcpCompact(context.Background(), mcpTestPassword, "", "")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "session_id")
}

func TestMCPCompact_OmittedID_AmbiguousBusy(t *testing.T) {
	f := newFakeAgent()
	s1 := f.newSession("a")
	s2 := f.newSession("b")
	f.busySet[s1] = true
	f.busySet[s2] = true
	withAgentServer(t, f.handler(t))

	_, err := mcpCompact(context.Background(), mcpTestPassword, "", "")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "multiple")
	assert.Contains(t, err.Error(), s1)
}

func TestMCPCompact_SummarizeFails(t *testing.T) {
	f := newFakeAgent()
	s1 := f.newSession("one")
	withAgentServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/summarize") {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte(`{"message":"Session compact is not available yet"}`))
			return
		}
		f.handler(t)(w, r)
	})

	_, err := mcpCompact(context.Background(), mcpTestPassword, s1, "")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "503")
}

// --- dispatcher + tools/list integration ----------------------------------

func TestCallMCPTool_RenameSession(t *testing.T) {
	f := newFakeAgent()
	withAgentServer(t, f.handler(t))
	out, err := callMCPTool(context.Background(), mcpTestPassword, "rename_session", map[string]any{
		"session_id": "ses_9",
		"title":      "T",
	})
	require.NoError(t, err)
	assert.Contains(t, out, "renamed")
}

func TestCallMCPTool_RenameSession_MissingID(t *testing.T) {
	_, err := callMCPTool(context.Background(), mcpTestPassword, "rename_session", map[string]any{
		"title": "T",
	})
	assert.Error(t, err)
}

func TestCallMCPTool_GetDatetime(t *testing.T) {
	out, err := callMCPTool(context.Background(), mcpTestPassword, "get_datetime", map[string]any{})
	require.NoError(t, err)
	assert.Contains(t, out, "utc")
}

func TestCallMCPTool_SessionMetadata(t *testing.T) {
	f := newFakeAgent()
	f.newSession("one")
	withAgentServer(t, f.handler(t))
	t.Setenv("WORKSPACE_ID", "ws-dispatch")

	out, err := callMCPTool(context.Background(), mcpTestPassword, "session_metadata", map[string]any{})
	require.NoError(t, err)
	assert.Contains(t, out, "ws-dispatch")
}

func TestCallMCPTool_Compact(t *testing.T) {
	f := newFakeAgent()
	s1 := f.newSession("one")
	withAgentServer(t, f.handler(t))

	out, err := callMCPTool(context.Background(), mcpTestPassword, "compact", map[string]any{
		"session_id": s1,
	})
	require.NoError(t, err)
	assert.Contains(t, out, "compacted")
}

func TestMCPHandler_ToolsList_IncludesNewTools(t *testing.T) {
	req := mcpRequest{JSONRPC: "2.0", ID: 7, Method: "tools/list"}
	body, _ := json.Marshal(req)
	w := httptest.NewRecorder()
	r := mcpAuthedRequest(body)
	mcpHandler(mcpTestPassword)(w, r)

	var resp mcpResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	tools := resp.Result.(map[string]any)["tools"].([]any)
	names := map[string]bool{}
	for _, tool := range tools {
		names[tool.(map[string]any)["name"].(string)] = true
	}
	for _, want := range []string{
		"session_list", "session_read", "dev_preview_url", "secrets_resync",
		"rename_session", "rename_workspace", "call_with_model",
		"create_session", "send_message", "get_datetime", "session_metadata",
		"compact", "abort_session",
	} {
		assert.True(t, names[want], "%s must be in tools/list", want)
	}
}

// Every tool sits behind the Basic gate — the per-tool 401 probe.
func TestMCPHandler_EveryToolRequiresAuth(t *testing.T) {
	for _, tool := range []string{
		"session_list", "session_read", "rename_session", "rename_workspace",
		"call_with_model", "create_session", "send_message", "abort_session", "get_datetime",
		"session_metadata", "compact", "secrets_resync", "dev_preview_url",
	} {
		params, _ := json.Marshal(map[string]any{"name": tool, "arguments": map[string]any{}})
		req := mcpRequest{JSONRPC: "2.0", ID: 1, Method: "tools/call", Params: params}
		body, _ := json.Marshal(req)
		w := httptest.NewRecorder()
		r := httptest.NewRequest(http.MethodPost, "/v1/mcp", newBodyReader(body))
		r.Header.Set("Authorization", "Basic "+basicAuth("wrong"))
		mcpHandler(mcpTestPassword)(w, r)
		assert.Equal(t, http.StatusUnauthorized, w.Code, "%s must sit behind the gate", tool)
	}
}

// --- full-stack JSON-RPC integration (L1) ---------------------------------

func TestMCPHandler_RenameWorkspaceFullStack(t *testing.T) {
	var gotPath string
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.WriteHeader(http.StatusNoContent)
	}))
	defer api.Close()
	setupRenameWorkspaceEnv(t, api)

	params, _ := json.Marshal(map[string]any{
		"name":      "rename_workspace",
		"arguments": map[string]any{"name": "Full Stack"},
	})
	req := mcpRequest{JSONRPC: "2.0", ID: 8, Method: "tools/call", Params: params}
	body, _ := json.Marshal(req)
	w := httptest.NewRecorder()
	r := mcpAuthedRequest(body)
	mcpHandler(mcpTestPassword)(w, r)

	var resp mcpResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	result := resp.Result.(map[string]any)
	assert.Nil(t, result["isError"], "tool must succeed: %v", result)
	content := result["content"].([]any)
	first := content[0].(map[string]any)
	assert.Contains(t, first["text"], "renamed")
	assert.Equal(t, "/internal/v1/workspace-rename", gotPath)
}

func TestMCPHandler_CallWithModelFullStack(t *testing.T) {
	f := newFakeAgent()
	withAgentServer(t, f.handler(t))
	dir := t.TempDir()
	png := filepath.Join(dir, "cam.png")
	require.NoError(t, os.WriteFile(png, []byte("imagedata"), 0o600))

	params, _ := json.Marshal(map[string]any{
		"name": "call_with_model",
		"arguments": map[string]any{
			"prompt": "what is this",
			"model":  "p/vision",
			"images": []string{png},
		},
	})
	req := mcpRequest{JSONRPC: "2.0", ID: 9, Method: "tools/call", Params: params}
	body, _ := json.Marshal(req)
	w := httptest.NewRecorder()
	r := mcpAuthedRequest(body)
	mcpHandler(mcpTestPassword)(w, r)

	var resp mcpResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	result := resp.Result.(map[string]any)
	assert.Nil(t, result["isError"], "%v", result)

	var carrierID string
	require.Len(t, f.titles, 1)
	for id := range f.titles {
		carrierID = id
	}
	parts := f.sentFor(carrierID)[0]["parts"].([]any)
	require.Len(t, parts, 2)
	file := parts[1].(map[string]any)
	assert.Contains(t, file["url"], "data:image/png;base64,")
}

func TestMCPHandler_CompactBusyFullStack(t *testing.T) {
	f := newFakeAgent()
	s1 := f.newSession("current")
	f.busySet[s1] = true
	withAgentServer(t, f.handler(t))

	params, _ := json.Marshal(map[string]any{
		"name":      "compact",
		"arguments": map[string]any{},
	})
	req := mcpRequest{JSONRPC: "2.0", ID: 10, Method: "tools/call", Params: params}
	body, _ := json.Marshal(req)
	w := httptest.NewRecorder()
	r := mcpAuthedRequest(body)
	mcpHandler(mcpTestPassword)(w, r)

	var resp mcpResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	result := resp.Result.(map[string]any)
	assert.Nil(t, result["isError"], "%v", result)
	content := result["content"].([]any)
	text := content[0].(map[string]any)["text"].(string)
	assert.Contains(t, text, "scheduled")
	require.Eventually(t, func() bool {
		return f.lastSummary(s1) != nil
	}, 5*time.Second, 50*time.Millisecond)
}

// The FIFO wedge (review finding, PR #1364): a named pipe passes a
// size-0 stat, then ReadFile blocks forever — file I/O ignores the
// context, so the tool call would wedge. Regular-file gate + capped
// read must refuse it up front.
func TestMCPCallWithModel_FIFORefused(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("mkfifo is linux/unix-only")
	}
	f := newFakeAgent()
	withAgentServer(t, f.handler(t))
	dir := t.TempDir()
	fifo := filepath.Join(dir, "pipe.png")
	require.NoError(t, syscall.Mkfifo(fifo, 0o600))

	done := make(chan error, 1)
	go func() {
		_, err := mcpCallWithModel(context.Background(), mcpTestPassword, "p", "p/vision", []string{fifo})
		done <- err
	}()
	select {
	case err := <-done:
		require.Error(t, err)
		assert.Contains(t, err.Error(), "not a regular file")
	case <-time.After(5 * time.Second):
		t.Fatal("loadImages wedged on the FIFO — the regular-file gate is missing")
	}
}

// A file that grows between stat and read (TOCTOU) is capped by the
// read itself, not trusted by its earlier stat.
func TestMCPCallWithModel_ReadCapTOCTOU(t *testing.T) {
	f := newFakeAgent()
	withAgentServer(t, f.handler(t))
	dir := t.TempDir()
	oversize := filepath.Join(dir, "big.png")
	require.NoError(t, os.WriteFile(oversize, make([]byte, imageMaxFileBytes+2), 0o600))

	_, err := mcpCallWithModel(context.Background(), mcpTestPassword, "p", "p/vision", []string{oversize})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "per-image cap")
}

// --- send_message ---------------------------------------------------------

func TestMCPSendMessage_IdleTarget(t *testing.T) {
	f := newFakeAgent()
	s1 := f.newSession("target")
	withAgentServer(t, f.handler(t))

	out, err := mcpSendMessage(context.Background(), mcpTestPassword, s1, "please continue")
	require.NoError(t, err)
	assert.Contains(t, out, `"delivering"`)

	require.Eventually(t, func() bool {
		bodies := f.sentFor(s1)
		return len(bodies) == 1
	}, 5*time.Second, 50*time.Millisecond, "detached delivery must land")
	parts := f.sentFor(s1)[0]["parts"].([]any)
	assert.Equal(t, "please continue", parts[0].(map[string]any)["text"])
	_, hasModel := f.sentFor(s1)[0]["model"]
	assert.False(t, hasModel, "no model override — the target runs its own default")
}

func TestMCPSendMessage_BusyTargetQueues(t *testing.T) {
	f := newFakeAgent()
	s1 := f.newSession("busy-target")
	f.busySet[s1] = true
	withAgentServer(t, f.handler(t))

	out, err := mcpSendMessage(context.Background(), mcpTestPassword, s1, "next: run the tests")
	require.NoError(t, err)
	assert.Contains(t, out, "delivering_after_current_turn")
	require.Eventually(t, func() bool {
		return len(f.sentFor(s1)) == 1
	}, 5*time.Second, 50*time.Millisecond, "delivery POST fires detached")
}

func TestMCPSendMessage_UnknownSession(t *testing.T) {
	f := newFakeAgent()
	f.newSession("real")
	withAgentServer(t, f.handler(t))

	_, err := mcpSendMessage(context.Background(), mcpTestPassword, "ses_absent", "hi")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not found")
}

func TestMCPSendMessage_MissingArgs(t *testing.T) {
	f := newFakeAgent()
	withAgentServer(t, f.handler(t))
	_, err := mcpSendMessage(context.Background(), mcpTestPassword, "", "hi")
	require.Error(t, err)
	_, err = mcpSendMessage(context.Background(), mcpTestPassword, "ses_1", "   ")
	require.Error(t, err)
	assert.Empty(t, f.sentBodies, "no delivery without valid args")
}

// Full-stack JSON-RPC: the busy path returns immediately with the
// queued status while the POST lands in the background.
func TestMCPHandler_SendMessageFullStack(t *testing.T) {
	f := newFakeAgent()
	s1 := f.newSession("steer-me")
	f.busySet[s1] = true
	withAgentServer(t, f.handler(t))

	params, _ := json.Marshal(map[string]any{
		"name":      "send_message",
		"arguments": map[string]any{"session_id": s1, "message": "pivot to the fallback design"},
	})
	req := mcpRequest{JSONRPC: "2.0", ID: 11, Method: "tools/call", Params: params}
	body, _ := json.Marshal(req)
	w := httptest.NewRecorder()
	r := mcpAuthedRequest(body)
	mcpHandler(mcpTestPassword)(w, r)

	var resp mcpResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	result := resp.Result.(map[string]any)
	assert.Nil(t, result["isError"], "%v", result)
	content := result["content"].([]any)
	assert.Contains(t, content[0].(map[string]any)["text"], "delivering_after_current_turn")
	require.Eventually(t, func() bool {
		return len(f.sentFor(s1)) == 1
	}, 5*time.Second, 50*time.Millisecond)
}

// --- abort_session --------------------------------------------------------

func TestMCPAbortSession_HappyPath(t *testing.T) {
	f := newFakeAgent()
	s1 := f.newSession("busy")
	f.busySet[s1] = true
	withAgentServer(t, f.handler(t))

	out, err := mcpAbortSession(context.Background(), mcpTestPassword, s1)
	require.NoError(t, err)
	assert.Contains(t, out, "aborted")
	assert.Equal(t, []string{s1}, f.aborted)
}

func TestMCPAbortSession_MissingID(t *testing.T) {
	_, err := mcpAbortSession(context.Background(), mcpTestPassword, "  ")
	require.Error(t, err)
}

func TestMCPAbortSession_Non2xx(t *testing.T) {
	withAgentServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	})
	_, err := mcpAbortSession(context.Background(), mcpTestPassword, "ses_x")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "404")
}

func TestMCPHandler_AbortSessionFullStack(t *testing.T) {
	f := newFakeAgent()
	s1 := f.newSession("runaway")
	f.busySet[s1] = true
	withAgentServer(t, f.handler(t))

	params, _ := json.Marshal(map[string]any{
		"name":      "abort_session",
		"arguments": map[string]any{"session_id": s1},
	})
	req := mcpRequest{JSONRPC: "2.0", ID: 12, Method: "tools/call", Params: params}
	body, _ := json.Marshal(req)
	w := httptest.NewRecorder()
	r := mcpAuthedRequest(body)
	mcpHandler(mcpTestPassword)(w, r)

	var resp mcpResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	result := resp.Result.(map[string]any)
	assert.Nil(t, result["isError"], "%v", result)
	assert.Equal(t, []string{s1}, f.aborted)
}
