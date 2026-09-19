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

	nextID       int
	titles       map[string]string // id -> title
	renamed      map[string]string
	deleted      []string
	aborted      []string
	msgArrived   map[string][]map[string]any
	msgDone      map[string]int // POST completions (deliveries AND refusals) — joins detached goroutines
	abortDropped map[string]bool
	sentBodies   map[string][]map[string]any // id -> decoded message bodies
	summaries    map[string]map[string]string
	busySet      map[string]bool
	models       map[string]string // session -> "prov/model"

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
		msgArrived:   map[string][]map[string]any{},
		msgDone:      map[string]int{},
		abortDropped: map[string]bool{},
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
				// Arrival sentinel: visible to tests BEFORE the busy wait,
				// so a test can prove the detached POST actually arrived
				// (and was held) rather than passing vacuously.
				f.msgArrived[id] = append(f.msgArrived[id], body)
				// Real wire shape: a BUSY session blocks the POST until
				// the turn ends, then delivers as the next turn (L2-proven
				// boundary delivery). ABORT during the wait drops the
				// queued message (V1 abort is destructive to queued input
				// — pinned by TestMCPSendMessage_AbortDropsQueued). Wait
				// exhaustion while still busy refuses rather than
				// delivering late.
				dropped := false
				for i := 0; i < 200 && f.busySet[id]; i++ {
					f.mu.Unlock()
					time.Sleep(25 * time.Millisecond)
					f.mu.Lock()
					if f.abortDropped[id] {
						dropped = true
					}
				}
				if dropped || f.busySet[id] {
					w.WriteHeader(http.StatusConflict)
					f.msgDone[id]++
					return
				}
				f.sentBodies[id] = append(f.sentBodies[id], body)
				f.msgDone[id]++
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
				f.abortDropped[id] = true // queued input is dropped (real V1 semantics)
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
			for id, busy := range f.busySet {
				if busy {
					out[id] = map[string]string{"type": "busy"}
				}
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
		case r.Method == http.MethodGet && strings.HasPrefix(path, "/session/"):
			// By-ID get (the SessionExists probe): unknown IDs return
			// the live-proven 404 NotFoundError shape.
			id := strings.TrimPrefix(path, "/session/")
			title, ok := f.titles[id]
			if !ok {
				w.WriteHeader(http.StatusNotFound)
				_, _ = w.Write([]byte(`{"name":"NotFoundError","data":{"message":"Session not found: ` + id + `"}}`))
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"id": id, "title": title})
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

// awaitMsgDone blocks until n /message POSTs have COMPLETED (delivered
// or refused) for the session — the join point for mcpSendMessage's
// detached goroutine, so no goroutine outlives the test (the package
// swaps the global log between tests; a live goroutine logging after
// test end is the cross-test race).
func (f *fakeAgent) awaitMsgDone(t *testing.T, id string, n int) {
	t.Helper()
	require.Eventually(t, func() bool {
		f.mu.Lock()
		defer f.mu.Unlock()
		return f.msgDone[id] >= n
	}, 7*time.Second, 25*time.Millisecond, "detached delivery goroutine must complete before test end")
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
	resetUserTimezone(t)
	out, err := mcpGetDatetime("")
	require.NoError(t, err)

	var res map[string]any
	require.NoError(t, json.Unmarshal([]byte(out), &res))
	for _, k := range []string{"utc", "local", "utc_offset", "source"} {
		require.Contains(t, res, k)
		require.NotEmpty(t, res[k], "%s must be non-empty", k)
	}
	assert.Equal(t, "pod", res["source"])
	assert.NotContains(t, res, "timezone", "pod fallback emits NO zone name — the key is absent, not empty-string faked")
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
	resetUserTimezone(t)
	out, err := callMCPTool(context.Background(), mcpTestPassword, "get_datetime", map[string]any{})
	require.NoError(t, err)
	assert.Contains(t, out, "utc")
	assert.Contains(t, out, `"source":"pod"`)

	out, err = callMCPTool(context.Background(), mcpTestPassword, "get_datetime", map[string]any{
		"timezone": "Asia/Tokyo",
	})
	require.NoError(t, err)
	assert.Contains(t, out, `"source":"argument"`)
	assert.Contains(t, out, "Asia/Tokyo")
	assert.Contains(t, out, "+09:00")
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
		"trigger_list", "trigger_create", "trigger_update", "trigger_delete",
		"trigger_fires", "trigger_rotate_webhook_secret",
		"workflow_list", "workflow_create", "workflow_update",
		"workflow_delete", "workflow_run", "workflow_runs",
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
		"trigger_list", "trigger_create", "trigger_update", "trigger_delete",
		"trigger_fires", "trigger_rotate_webhook_secret",
		"workflow_list", "workflow_create", "workflow_update",
		"workflow_delete", "workflow_run", "workflow_runs",
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
//
// Origin attribution contract (#1465): the caller's session ID arrives
// as an INJECTED from_session_id argument (the platform's opencode
// plugin stamps it from the harness's tool context — the LLM never
// supplies it; the tools/list schema does not advertise it). agentd
// validates it by ID (SessionExists — the #1452-immune probe), stamps
// the agent-message-v1 sentinel, and echoes origin in the result.
// Missing/invalid origin refuses delivery outright: every delivered
// agent message carries attribution.

func TestMCPSendMessage_IdleTarget(t *testing.T) {
	f := newFakeAgent()
	s1 := f.newSession("target")
	caller := f.newSession("caller")
	withAgentServer(t, f.handler(t))

	out, err := mcpSendMessage(context.Background(), mcpTestPassword, s1, "please continue", caller, "")
	require.NoError(t, err)
	assert.Contains(t, out, `"delivering"`)
	assert.Contains(t, out, fmt.Sprintf(`"origin":%q`, caller))
	assert.Contains(t, out, `"origin_mode":"injected"`)

	require.Eventually(t, func() bool {
		bodies := f.sentFor(s1)
		return len(bodies) == 1
	}, 5*time.Second, 50*time.Millisecond, "detached delivery must land")
	parts := f.sentFor(s1)[0]["parts"].([]any)
	delivered := parts[0].(map[string]any)["text"].(string)
	assert.True(t, strings.HasPrefix(delivered, "<!-- lsp:agent-message-v1 "),
		"delivered text must open with the sentinel, got %q", delivered)
	assert.Contains(t, delivered, `"fromSession":"`+caller+`"`)
	assert.Contains(t, delivered, `"mode":"injected"`)
	assert.True(t, strings.HasSuffix(delivered, "\nplease continue"), "payload follows the sentinel line")
	_, hasModel := f.sentFor(s1)[0]["model"]
	assert.False(t, hasModel, "no model override — the target runs its own default")
}

func TestMCPSendMessage_BusyTargetQueues(t *testing.T) {
	f := newFakeAgent()
	s1 := f.newSession("busy-target")
	caller := f.newSession("caller")
	f.busySet[s1] = true
	withAgentServer(t, f.handler(t))

	out, err := mcpSendMessage(context.Background(), mcpTestPassword, s1, "next: run the tests", caller, "")
	require.NoError(t, err)
	assert.Contains(t, out, "delivering_after_current_turn")
	assert.Contains(t, out, fmt.Sprintf(`"origin":%q`, caller))

	// While the target stays busy, the detached POST blocks server-side
	// — it must ARRIVE (sentinel) but never DELIVER (the L2 test proves
	// this shape on the real binary; the L1 fake mirrors it).
	require.Eventually(t, func() bool {
		f.mu.Lock()
		defer f.mu.Unlock()
		return len(f.msgArrived[s1]) == 1
	}, 2*time.Second, 25*time.Millisecond, "the detached POST must arrive and be held")
	require.Never(t, func() bool { return len(f.sentFor(s1)) > 0 }, 300*time.Millisecond, 50*time.Millisecond,
		"no delivery while the target is busy")

	// Stand in for the busy turn ending: the blocked POST then delivers
	// at the boundary.
	f.mu.Lock()
	f.busySet[s1] = false
	f.mu.Unlock()

	require.Eventually(t, func() bool {
		return len(f.sentFor(s1)) == 1
	}, 7*time.Second, 50*time.Millisecond, "delivery lands once the turn ends (boundary)")
	f.awaitMsgDone(t, s1, 1)
	parts := f.sentFor(s1)[0]["parts"].([]any)
	delivered := parts[0].(map[string]any)["text"].(string)
	assert.True(t, strings.HasPrefix(delivered, "<!-- lsp:agent-message-v1 "))
	assert.True(t, strings.HasSuffix(delivered, "\nnext: run the tests"))
}

func TestMCPSendMessage_UnknownSession(t *testing.T) {
	f := newFakeAgent()
	caller := f.newSession("caller")
	f.newSession("real")
	withAgentServer(t, f.handler(t))

	_, err := mcpSendMessage(context.Background(), mcpTestPassword, "ses_absent", "hi", caller, "")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not found")
}

func TestMCPSendMessage_MissingArgs(t *testing.T) {
	f := newFakeAgent()
	withAgentServer(t, f.handler(t))
	for _, tc := range []struct {
		name, target, message, injected, declared string
	}{
		{"no target", "", "hi", "ses_2", ""},
		{"blank message", "ses_1", "   ", "ses_2", ""},
		{"no origin at all", "ses_1", "hi", "", ""},
		{"blank injected only", "ses_1", "hi", "  ", ""},
		{"blank declared only", "ses_1", "hi", "", "  "},
	} {
		_, err := mcpSendMessage(context.Background(), mcpTestPassword, tc.target, tc.message, tc.injected, tc.declared)
		require.Error(t, err, tc.name)
	}
	assert.Empty(t, f.sentBodies, "no delivery without valid args")
	assert.Empty(t, f.msgArrived, "no wire traffic at all without valid args")
}

// The ALWAYS-ATTRIBUTED invariant: with NEITHER an injected origin
// NOR a self-declared one there is nothing to attribute — the only
// refusal left (#1469 hybrid ruling). With from_session_id
// schema-visible the model can always self-declare, so a degraded pod
// self-reports instead of breaking.
func TestMCPSendMessage_NoOriginAtAllRefusesDelivery(t *testing.T) {
	f := newFakeAgent()
	s1 := f.newSession("target")
	withAgentServer(t, f.handler(t))

	_, err := mcpSendMessage(context.Background(), mcpTestPassword, s1, "hi", "", "")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "from_session_id")
	assert.Contains(t, err.Error(), "plugin", "the error must name the platform-side cause")
	assert.Empty(t, f.msgArrived, "nothing may reach the wire — delivery without attribution is forbidden")
}

// The degraded-pod fallback: no injection (plugin absent), the model
// supplied from_session_id — accepted, validated, and labeled
// self-declared in BOTH the result and the sentinel.
func TestMCPSendMessage_SelfDeclaredFallback(t *testing.T) {
	f := newFakeAgent()
	s1 := f.newSession("target")
	caller := f.newSession("caller")
	withAgentServer(t, f.handler(t))

	out, err := mcpSendMessage(context.Background(), mcpTestPassword, s1, "fallback hi", "", caller)
	require.NoError(t, err)
	assert.Contains(t, out, fmt.Sprintf(`"origin":%q`, caller))
	assert.Contains(t, out, `"origin_mode":"self-declared"`)

	require.Eventually(t, func() bool { return len(f.sentFor(s1)) == 1 }, 5*time.Second, 50*time.Millisecond)
	f.awaitMsgDone(t, s1, 1)
	parts := f.sentFor(s1)[0]["parts"].([]any)
	delivered := parts[0].(map[string]any)["text"].(string)
	assert.True(t, strings.HasPrefix(delivered, "<!-- lsp:agent-message-v1 "))
	assert.Contains(t, delivered, `"fromSession":"`+caller+`"`)
	assert.Contains(t, delivered, `"mode":"self-declared"`)
}

func TestMCPSendMessage_SelfDeclaredUnknownIDRefusesDelivery(t *testing.T) {
	f := newFakeAgent()
	s1 := f.newSession("target")
	withAgentServer(t, f.handler(t))

	_, err := mcpSendMessage(context.Background(), mcpTestPassword, s1, "hi", "", "ses_ghost")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "ses_ghost")
	assert.Empty(t, f.msgArrived, "an unattributable origin must never deliver")
}

// Injection is the platform's attestation and wins over any
// model-supplied value — the mode label must reflect the trustworthy
// source, and the model's copy never downgrades it.
func TestMCPSendMessage_InjectionWinsOverDeclared(t *testing.T) {
	f := newFakeAgent()
	s1 := f.newSession("target")
	caller := f.newSession("caller")
	decoy := f.newSession("decoy")
	withAgentServer(t, f.handler(t))

	out, err := mcpSendMessage(context.Background(), mcpTestPassword, s1, "hi", caller, decoy)
	require.NoError(t, err)
	assert.Contains(t, out, fmt.Sprintf(`"origin":%q`, caller))
	assert.Contains(t, out, `"origin_mode":"injected"`)
	assert.NotContains(t, out, decoy)
}

func TestMCPSendMessage_OriginUnknownIDRefusesDelivery(t *testing.T) {
	f := newFakeAgent()
	s1 := f.newSession("target")
	withAgentServer(t, f.handler(t))

	_, err := mcpSendMessage(context.Background(), mcpTestPassword, s1, "hi", "ses_ghost", "")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "ses_ghost")
	assert.Empty(t, f.msgArrived, "an unattributable origin must never deliver")
}

func TestMCPSendMessage_OriginProbeIndeterminateRefusesDelivery(t *testing.T) {
	f := newFakeAgent()
	s1 := f.newSession("target")
	caller := f.newSession("caller")
	inner := f.handler(t)
	withAgentServer(t, func(w http.ResponseWriter, r *http.Request) {
		// Fail ONLY the bare by-ID GET; everything else serves normally.
		if r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/session/") &&
			!strings.Contains(strings.TrimPrefix(r.URL.Path, "/session/"), "/") {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		inner(w, r)
	})

	_, err := mcpSendMessage(context.Background(), mcpTestPassword, s1, "hi", caller, "")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "500")
	assert.Empty(t, f.msgArrived)
}

// Self-send (from == target): no special case — the sentinel carries
// the caller's own ID and delivery schedules as the next turn.
func TestMCPSendMessage_SelfSend(t *testing.T) {
	f := newFakeAgent()
	s1 := f.newSession("self")
	f.busySet[s1] = true // the caller's own turn is running the tool
	withAgentServer(t, f.handler(t))

	out, err := mcpSendMessage(context.Background(), mcpTestPassword, s1, "follow-up after this turn", s1, "")
	require.NoError(t, err)
	assert.Contains(t, out, "delivering_after_current_turn")
	assert.Contains(t, out, fmt.Sprintf(`"origin":%q`, s1))
	assert.Contains(t, out, `"origin_mode":"injected"`)

	f.mu.Lock()
	f.busySet[s1] = false
	f.mu.Unlock()
	require.Eventually(t, func() bool { return len(f.sentFor(s1)) == 1 }, 7*time.Second, 50*time.Millisecond)
	f.awaitMsgDone(t, s1, 1)
	parts := f.sentFor(s1)[0]["parts"].([]any)
	delivered := parts[0].(map[string]any)["text"].(string)
	assert.True(t, strings.HasPrefix(delivered, "<!-- lsp:agent-message-v1 "))
	assert.Contains(t, delivered, `"fromSession":"`+s1+`"`)
}

// A re-sent message whose payload already carries a sentinel (copied
// from a prior hop) must never double-sentinel: compose strips the
// leading line and stamps exactly one, carrying the CURRENT origin.
func TestMCPSendMessage_ResendNeverDoubleSentinels(t *testing.T) {
	f := newFakeAgent()
	s1 := f.newSession("target")
	caller := f.newSession("caller")
	withAgentServer(t, f.handler(t))

	preSentineled := "<!-- lsp:agent-message-v1 {\"fromSession\":\"ses_priorhop\"} -->\nforwarded payload"
	_, err := mcpSendMessage(context.Background(), mcpTestPassword, s1, preSentineled, caller, "")
	require.NoError(t, err)
	require.Eventually(t, func() bool { return len(f.sentFor(s1)) == 1 }, 5*time.Second, 50*time.Millisecond)
	f.awaitMsgDone(t, s1, 1)

	parts := f.sentFor(s1)[0]["parts"].([]any)
	delivered := parts[0].(map[string]any)["text"].(string)
	assert.Equal(t, 1, strings.Count(delivered, "<!-- lsp:agent-message-v1 "),
		"exactly one sentinel line, never stacked")
	assert.Contains(t, delivered, `"fromSession":"`+caller+`"`)
	assert.NotContains(t, delivered, "ses_priorhop", "the current origin replaces the prior hop's")
	assert.True(t, strings.HasSuffix(delivered, "\nforwarded payload"))
}

// The model-facing schema advertises ONLY the self-declared fallback
// (#1469 hybrid ruling): from_session_id is optional, lsp_injected_session
// (the platform plugin's harness-attested key) must stay invisible —
// an advertised attestation key invites spoofing the mode label.
func TestMCPSendMessage_SchemaAdvertisesFallbackOnly(t *testing.T) {
	req := mcpRequest{JSONRPC: "2.0", ID: 1, Method: "tools/list"}
	body, _ := json.Marshal(req)
	w := httptest.NewRecorder()
	r := mcpAuthedRequest(body)
	mcpHandler(mcpTestPassword)(w, r)

	var resp mcpResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	tools := resp.Result.(map[string]any)["tools"].([]any)
	var schema map[string]any
	for _, tool := range tools {
		tt := tool.(map[string]any)
		if tt["name"] == "send_message" {
			schema = tt["inputSchema"].(map[string]any)
		}
	}
	require.NotNil(t, schema, "send_message must be advertised")
	props := schema["properties"].(map[string]any)
	assert.ElementsMatch(t, []string{"session_id", "message", "from_session_id"}, mcpSchemaKeys(props),
		"only the self-declared fallback is advertised")
	assert.ElementsMatch(t, []string{"session_id", "message"}, schema["required"].([]any),
		"from_session_id is optional — injection needs no model help")
}

func mcpSchemaKeys(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// Full-stack JSON-RPC: the busy path returns immediately with the
// queued status while the POST lands in the background.
func TestMCPHandler_SendMessageFullStack(t *testing.T) {
	f := newFakeAgent()
	s1 := f.newSession("steer-me")
	f.busySet[s1] = true
	withAgentServer(t, f.handler(t))

	caller := f.newSession("caller")
	params, _ := json.Marshal(map[string]any{
		"name": "send_message",
		"arguments": map[string]any{
			"session_id":           s1,
			"message":              "pivot to the fallback design",
			"lsp_injected_session": caller, // the plugin's injection, as it arrives on the wire
		},
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
	text := content[0].(map[string]any)["text"].(string)
	assert.Contains(t, text, "delivering_after_current_turn")
	assert.Contains(t, text, fmt.Sprintf(`"origin":%q`, caller))
	assert.Contains(t, text, `"origin_mode":"injected"`)
	// End the fake busy turn so the blocked POST delivers at the boundary.
	f.mu.Lock()
	f.busySet[s1] = false
	f.mu.Unlock()
	require.Eventually(t, func() bool {
		return len(f.sentFor(s1)) == 1
	}, 7*time.Second, 50*time.Millisecond)
	f.awaitMsgDone(t, s1, 1)
	parts := f.sentFor(s1)[0]["parts"].([]any)
	delivered := parts[0].(map[string]any)["text"].(string)
	assert.True(t, strings.HasPrefix(delivered, "<!-- lsp:agent-message-v1 "))
	assert.True(t, strings.HasSuffix(delivered, "\npivot to the fallback design"))
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

// A session in retry backoff behaves like busy: the POST waits for the
// retrying turn to settle, so the label says after_current_turn.
func TestMCPSendMessage_RetryStatusTreatedAsBusy(t *testing.T) {
	f := newFakeAgent()
	s1 := f.newSession("retrying")
	caller := f.newSession("caller")
	withAgentServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/session/status" {
			fmt.Fprintf(w, `{"%s":{"type":"retry","attempt":2}}`, s1)
			return
		}
		f.handler(t)(w, r)
	})

	out, err := mcpSendMessage(context.Background(), mcpTestPassword, s1, "hold please", caller, "")
	require.NoError(t, err)
	assert.Contains(t, out, "delivering_after_current_turn")
	require.Eventually(t, func() bool { return len(f.sentFor(s1)) == 1 }, 7*time.Second, 25*time.Millisecond)
	f.awaitMsgDone(t, s1, 1)
}

// The tools' one interaction: a message queued mid-turn is DROPPED when
// the target is aborted (V1 abort is destructive to queued input — the
// description tells the agent to re-send). The fake models the drop;
// the pin keeps the warning honest.
func TestMCPSendMessage_AbortDropsQueued(t *testing.T) {
	f := newFakeAgent()
	s1 := f.newSession("busy-abort")
	f.busySet[s1] = true
	caller := f.newSession("caller")
	withAgentServer(t, f.handler(t))

	_, err := mcpSendMessage(context.Background(), mcpTestPassword, s1, "will be dropped", caller, "")
	require.NoError(t, err)
	require.Eventually(t, func() bool {
		f.mu.Lock()
		defer f.mu.Unlock()
		return len(f.msgArrived[s1]) == 1
	}, 2*time.Second, 25*time.Millisecond, "message must be queued (held)")

	_, err = mcpAbortSession(context.Background(), mcpTestPassword, s1)
	require.NoError(t, err)

	require.Never(t, func() bool { return len(f.sentFor(s1)) > 0 }, 500*time.Millisecond, 50*time.Millisecond,
		"the queued message must NOT deliver after abort — it was dropped")
	f.awaitMsgDone(t, s1, 1) // the refusal completed the handler

	// The detached goroutine captured its logger at spawn (mcp_tools.go),
	// so it can no longer race the global log swap — but still join its
	// completion window for cleanliness: the Warn is observable once the
	// refusal's error propagates. Best-effort settle, not a correctness
	// gate.
	time.Sleep(50 * time.Millisecond)
}

// compact with omitted session_id resolves the single running session;
// a RETRYING session counts as running (retry-as-busy convention).
func TestMCPCompact_OmittedID_ResolvesRetryingSession(t *testing.T) {
	f := newFakeAgent()
	s1 := f.newSession("backing-off")
	withAgentServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.URL.Path == "/session/status" {
			fmt.Fprintf(w, `{"%s":{"type":"retry","attempt":3}}`, s1)
			return
		}
		f.handler(t)(w, r)
	})

	out, err := mcpCompact(context.Background(), mcpTestPassword, "", "")
	require.NoError(t, err)
	assert.Contains(t, out, "scheduled", "retrying target takes the busy path (detached summarize)")
	assert.Contains(t, out, s1)
	require.Eventually(t, func() bool { return f.lastSummary(s1) != nil }, 7*time.Second, 25*time.Millisecond,
		"the detached summarize goroutine must complete before test end")
}

// --- get_datetime: user-timezone resolution ---------------------------------

func resetUserTimezone(t *testing.T) {
	t.Helper()
	userTimezoneAtomic.Store("")
}

func TestMCPGetDatetime_FallbackPodZone(t *testing.T) {
	resetUserTimezone(t)
	out, err := mcpGetDatetime("")
	require.NoError(t, err)
	var res map[string]any
	require.NoError(t, json.Unmarshal([]byte(out), &res))
	assert.Equal(t, "pod", res["source"])
	assert.NotContains(t, res, "timezone", "no zone known — the key is absent, not empty-string faked")
}

func TestMCPGetDatetime_BrowserZoneWinsWhenNoArg(t *testing.T) {
	resetUserTimezone(t)
	userTimezoneAtomic.Store("America/Los_Angeles")
	t.Cleanup(func() { userTimezoneAtomic.Store("") })

	out, err := mcpGetDatetime("")
	require.NoError(t, err)
	var res map[string]any
	require.NoError(t, json.Unmarshal([]byte(out), &res))
	assert.Equal(t, "browser", res["source"])
	assert.Equal(t, "America/Los_Angeles", res["timezone"])
	utc, err := time.Parse(time.RFC3339, res["utc"].(string))
	require.NoError(t, err)
	local, err := time.Parse(time.RFC3339, res["local"].(string))
	require.NoError(t, err)
	assert.WithinDuration(t, utc, local, 2*time.Minute)
	assert.Contains(t, []string{"-07:00", "-08:00"}, res["utc_offset"], "PDT or PST")
}

func TestMCPGetDatetime_ArgumentOverridesBrowser(t *testing.T) {
	resetUserTimezone(t)
	userTimezoneAtomic.Store("America/Los_Angeles")
	t.Cleanup(func() { userTimezoneAtomic.Store("") })

	out, err := mcpGetDatetime("Europe/Berlin")
	require.NoError(t, err)
	var res map[string]any
	require.NoError(t, json.Unmarshal([]byte(out), &res))
	assert.Equal(t, "argument", res["source"])
	assert.Equal(t, "Europe/Berlin", res["timezone"])
	assert.Contains(t, []string{"+01:00", "+02:00"}, res["utc_offset"])
}

func TestMCPGetDatetime_UnknownZoneRejected(t *testing.T) {
	resetUserTimezone(t)
	_, err := mcpGetDatetime("Mars/Olympus_Mons")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unknown timezone")
}

func TestUserTimezoneHandler_AuthAndValidation(t *testing.T) {
	h := userTimezoneHandler("cp-pw", "oc-pw")

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/user-timezone", strings.NewReader(`{"timezone":"America/New_York"}`))
	h(rr, req)
	assert.Equal(t, http.StatusUnauthorized, rr.Code, "unauthenticated push must be rejected")

	rr = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodPost, "/v1/user-timezone", strings.NewReader(`{"timezone":"Not/AZone"}`))
	req.SetBasicAuth("opencode", "oc-pw")
	h(rr, req)
	assert.Equal(t, http.StatusBadRequest, rr.Code)
	assert.Empty(t, userTimezone())

	rr = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodPost, "/v1/user-timezone", strings.NewReader(`{"timezone":"America/New_York"}`))
	req.SetBasicAuth("opencode", "oc-pw")
	h(rr, req)
	assert.Equal(t, http.StatusOK, rr.Code)
	assert.Equal(t, "America/New_York", userTimezone())
	t.Cleanup(func() { userTimezoneAtomic.Store("") })

	// The control-plane credential must also pass (the §D1 carve-out pair).
	userTimezoneAtomic.Store("")
	rr = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodPost, "/v1/user-timezone", strings.NewReader(`{"timezone":"Europe/Berlin"}`))
	req.SetBasicAuth("opencode", "cp-pw")
	h(rr, req)
	assert.Equal(t, http.StatusOK, rr.Code)
	assert.Equal(t, "Europe/Berlin", userTimezone())
}

// --- automation tools (trigger_* / workflow_*) ------------------------------

// automationCallRecord captures one wire request to the fake automation API.
type automationCallRecord struct {
	method string
	path   string
	query  string
	auth   string
	body   string
}

// newAutomationAPI spins up a fake /internal/v1/automation backend that
// records the last request and replies with the given status+body.
func newAutomationAPI(t *testing.T, status int, reply string) (*httptest.Server, *automationCallRecord) {
	t.Helper()
	rec := &automationCallRecord{}
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec.method, rec.path, rec.query, rec.auth = r.Method, r.URL.Path, r.URL.RawQuery, r.Header.Get("Authorization")
		if r.Body != nil && r.ContentLength > 0 {
			buf := make([]byte, r.ContentLength)
			_, _ = r.Body.Read(buf)
			rec.body = string(buf)
		}
		w.WriteHeader(status)
		_, _ = w.Write([]byte(reply))
	}))
	t.Cleanup(api.Close)
	return api, rec
}

func TestMCPAutomation_ListWire(t *testing.T) {
	api, rec := newAutomationAPI(t, 200, `{"triggers":[]}`)
	setupRenameWorkspaceEnv(t, api)

	out, err := mcpAutomation(context.Background(), "trigger_list", nil)
	require.NoError(t, err)
	assert.Equal(t, `{"triggers":[]}`, out, "platform body passes through verbatim")
	assert.Equal(t, http.MethodGet, rec.method)
	assert.Equal(t, "/internal/v1/automation/triggers", rec.path)
	assert.Equal(t, "workspaceID=ws-1", rec.query)
	assert.Equal(t, "Bearer sa-token", rec.auth)
	assert.Empty(t, rec.body)
}

func TestMCPAutomation_CreateStampsWorkspace(t *testing.T) {
	api, rec := newAutomationAPI(t, 201, `{"id":"t-1"}`)
	setupRenameWorkspaceEnv(t, api)

	// The schema-documented wrapper form: {"workflow": {...}}. The real
	// delegated handler binds FLAT — the wire body must be the unwrapped
	// object with the resolver-spelling workspace stamp.
	out, err := mcpAutomation(context.Background(), "workflow_create", map[string]any{
		"workflow": map[string]any{"name": "wf", "spec": map[string]any{"nodes": []any{}}},
	})
	require.NoError(t, err)
	assert.Contains(t, out, `"id":"t-1"`)
	assert.Equal(t, http.MethodPost, rec.method)
	assert.Equal(t, "/internal/v1/automation/workflows", rec.path)

	var wire map[string]json.RawMessage
	require.NoError(t, json.Unmarshal([]byte(rec.body), &wire))
	assert.Contains(t, wire, "workspaceID", "create body carries the resolver-spelling stamp")
	assert.NotContains(t, wire, "workflow", "wrapper key must NOT reach the wire (real handler binds flat)")
	assert.NotContains(t, wire, "trigger", "wrapper key must NOT reach the wire (real handler binds flat)")
	var ws string
	require.NoError(t, json.Unmarshal(wire["workspaceID"], &ws))
	assert.Equal(t, "ws-1", ws)
}

func TestMCPAutomation_UpdatePatchPurity(t *testing.T) {
	api, rec := newAutomationAPI(t, 200, `{"id":"11111111-1111-1111-1111-111111111111"}`)
	setupRenameWorkspaceEnv(t, api)

	_, err := mcpAutomation(context.Background(), "trigger_update", map[string]any{
		"id":    "11111111-1111-1111-1111-111111111111",
		"patch": map[string]any{"enabled": false},
	})
	require.NoError(t, err)
	assert.Equal(t, http.MethodPut, rec.method)
	assert.Equal(t, "/internal/v1/automation/triggers/11111111-1111-1111-1111-111111111111", rec.path)
	assert.Equal(t, `{"enabled":false}`, rec.body, "patch carries caller fields only — no id, no workspaceID")
}

func TestMCPAutomation_DeleteAndFiresWire(t *testing.T) {
	api, rec := newAutomationAPI(t, 204, "")
	setupRenameWorkspaceEnv(t, api)

	out, err := mcpAutomation(context.Background(), "trigger_delete", map[string]any{"id": "11111111-1111-1111-1111-111111111111"})
	require.NoError(t, err)
	assert.Equal(t, `{"status":204}`, out, "empty platform body degrades to status-only")
	assert.Equal(t, http.MethodDelete, rec.method)
	assert.Equal(t, "/internal/v1/automation/triggers/11111111-1111-1111-1111-111111111111", rec.path)
	assert.Equal(t, "workspaceID=ws-1", rec.query)

	_, err = mcpAutomation(context.Background(), "trigger_fires", map[string]any{"id": "11111111-1111-1111-1111-111111111111"})
	require.NoError(t, err)
	assert.Equal(t, http.MethodGet, rec.method)
	assert.Equal(t, "/internal/v1/automation/triggers/11111111-1111-1111-1111-111111111111/fires", rec.path)
}

func TestMCPAutomation_WorkflowRunInputWrapped(t *testing.T) {
	api, rec := newAutomationAPI(t, 202, `{"runId":"r-9"}`)
	setupRenameWorkspaceEnv(t, api)

	out, err := mcpAutomation(context.Background(), "workflow_run", map[string]any{
		"id":    "11111111-1111-1111-1111-111111111111",
		"input": map[string]any{"topic": "ship"},
	})
	require.NoError(t, err)
	assert.Contains(t, out, `"runId":"r-9"`)
	assert.Equal(t, http.MethodPost, rec.method)
	assert.Equal(t, "/internal/v1/automation/workflows/11111111-1111-1111-1111-111111111111/runs", rec.path)
	assert.Contains(t, rec.body, `"input"`)
	assert.Contains(t, rec.body, `"topic"`)
}

func TestMCPAutomation_ErrorPassthrough(t *testing.T) {
	api, _ := newAutomationAPI(t, 400, `{"error":{"message":"name is required"}}`)
	setupRenameWorkspaceEnv(t, api)

	_, err := mcpAutomation(context.Background(), "trigger_create", map[string]any{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "trigger_create failed")
	assert.Contains(t, err.Error(), "name is required", "platform error text surfaces verbatim")
}

func TestMCPAutomation_InvalidIDNeverDials(t *testing.T) {
	api, rec := newAutomationAPI(t, 200, `{}`)
	setupRenameWorkspaceEnv(t, api)

	_, err := mcpAutomation(context.Background(), "workflow_runs", map[string]any{"id": "../../etc/passwd"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "invalid automation id")
	assert.Empty(t, rec.method, "hostile ID rejected before any dial")

	_, err = mcpAutomation(context.Background(), "trigger_delete", map[string]any{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "invalid automation id")
	assert.Empty(t, rec.method)
}

func TestMCPAutomation_MissingDeps(t *testing.T) {
	api, _ := newAutomationAPI(t, 200, `{}`)
	setupRenameWorkspaceEnv(t, api)

	t.Setenv("WORKSPACE_ID", "")
	_, err := mcpAutomation(context.Background(), "trigger_list", nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "WORKSPACE_ID")

	t.Setenv("WORKSPACE_ID", "ws-1")
	t.Setenv("LLMSAFESPACE_API_URL", "")
	_, err = mcpAutomation(context.Background(), "trigger_list", nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "LLMSAFESPACE_API_URL")

	t.Setenv("LLMSAFESPACE_API_URL", api.URL)
	t.Setenv("LLMSAFESPACE_BOOTSTRAP_TOKEN_FILE", filepath.Join(t.TempDir(), "missing"))
	_, err = mcpAutomation(context.Background(), "trigger_list", nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "SA token unreadable")
}

func TestMCPAutomation_RotateWebhookSecret(t *testing.T) {
	api, rec := newAutomationAPI(t, 200, `{"webhookSecret":"whs_x","webhookUrl":"/api/v1/hooks/11111111-1111-1111-1111-111111111111"}`)
	setupRenameWorkspaceEnv(t, api)

	out, err := mcpAutomation(context.Background(), "trigger_rotate_webhook_secret", map[string]any{
		"id": "11111111-1111-1111-1111-111111111111",
	})
	require.NoError(t, err)
	assert.Contains(t, out, `"webhookSecret":"whs_x"`, "credential surfaces verbatim to the agent")
	assert.Equal(t, http.MethodPost, rec.method)
	assert.Equal(t, "/internal/v1/automation/triggers/11111111-1111-1111-1111-111111111111/rotate-secret", rec.path)
}

func TestMCPAutomation_UnknownTool(t *testing.T) {
	api, _ := newAutomationAPI(t, 200, `{}`)
	setupRenameWorkspaceEnv(t, api)
	_, err := mcpAutomation(context.Background(), "trigger_deploy", nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unknown automation tool")
}

// L1: full JSON-RPC through mcpHandler for a representative pair —
// trigger_create then trigger_fires, the iterate-on-automation loop.
func TestMCPHandler_AutomationFullStack(t *testing.T) {
	// The fake API enforces the REAL delegated bind contract: flat
	// CreateTriggerRequest (name required), wrapper bodies 400 — the
	// exact contract the API-layer integration tests pin against the
	// real TriggersHandler (import boundaries forbid mounting the real
	// handler in-process here).
	var createdName, createdPrompt string
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/triggers"):
			var fields map[string]json.RawMessage
			_ = json.NewDecoder(r.Body).Decode(&fields)
			if _, wrapped := fields["trigger"]; wrapped {
				w.WriteHeader(http.StatusBadRequest)
				_, _ = w.Write([]byte(`{"error":"Key: 'CreateTriggerRequest.Name' Error:Field validation for 'Name' failed on the 'required' tag"}`))
				return
			}
			_ = json.Unmarshal(fields["name"], &createdName)
			_ = json.Unmarshal(fields["prompt"], &createdPrompt)
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"id":"11111111-1111-1111-1111-111111111111"}`))
		case strings.HasSuffix(r.URL.Path, "/fires"):
			_, _ = w.Write([]byte(`{"fires":[{"status":"delivered"}]}`))
		}
	}))
	defer api.Close()
	setupRenameWorkspaceEnv(t, api)

	for _, call := range []struct {
		name string
		args map[string]any
	}{
		{"trigger_create", map[string]any{"trigger": map[string]any{"name": "cron", "prompt": "ship it"}}},
		{"trigger_fires", map[string]any{"id": "11111111-1111-1111-1111-111111111111"}},
	} {
		params, _ := json.Marshal(map[string]any{"name": call.name, "arguments": call.args})
		req := mcpRequest{JSONRPC: "2.0", ID: 42, Method: "tools/call", Params: params}
		body, _ := json.Marshal(req)
		w := httptest.NewRecorder()
		r := mcpAuthedRequest(body)
		mcpHandler(mcpTestPassword)(w, r)

		var resp mcpResponse
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp), "%s", call.name)
		result := resp.Result.(map[string]any)
		assert.Nil(t, result["isError"], "%s must succeed: %v", call.name, result)
	}

	assert.Equal(t, "cron", createdName, "unwrapped flat body reached the delegated bind")
	assert.Equal(t, "ship it", createdPrompt)
}

// Every advertised tool must be dispatchable: table over tools/list,
// each name probed through the real dispatcher. Catches the
// advertised-but-uncallable class (a tool added to tools/list and the
// executor but missing from the dispatcher's case list).
func TestMCPHandler_EveryAdvertisedToolDispatches(t *testing.T) {
	req := mcpRequest{JSONRPC: "2.0", ID: 1, Method: "tools/list"}
	body, _ := json.Marshal(req)
	w := httptest.NewRecorder()
	r := mcpAuthedRequest(body)
	mcpHandler(mcpTestPassword)(w, r)
	var resp mcpResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	tools := resp.Result.(map[string]any)["tools"].([]any)
	require.NotEmpty(t, tools)

	// Valid auth, empty args: every tool must get PAST the dispatcher
	// (its own arg validation may error — but never "unknown tool").
	for _, tool := range tools {
		name := tool.(map[string]any)["name"].(string)
		_, err := callMCPTool(context.Background(), mcpTestPassword, name, map[string]any{})
		if err == nil {
			continue
		}
		assert.NotContains(t, err.Error(), "unknown tool", "%s is advertised in tools/list but not dispatchable", name)
	}
}

// L1: rotate through the full JSON-RPC surface — the rotated secret
// must reach the tool response.
func TestMCPHandler_RotateWebhookSecretFullStack(t *testing.T) {
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"webhookSecret":"whs_l1","webhookUrl":"/api/v1/hooks/11111111-1111-1111-1111-111111111111"}`))
	}))
	defer api.Close()
	setupRenameWorkspaceEnv(t, api)

	params, _ := json.Marshal(map[string]any{
		"name":      "trigger_rotate_webhook_secret",
		"arguments": map[string]any{"id": "11111111-1111-1111-1111-111111111111"},
	})
	req := mcpRequest{JSONRPC: "2.0", ID: 77, Method: "tools/call", Params: params}
	body, _ := json.Marshal(req)
	w := httptest.NewRecorder()
	rr := mcpAuthedRequest(body)
	mcpHandler(mcpTestPassword)(w, rr)

	var resp mcpResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	result := resp.Result.(map[string]any)
	assert.Nil(t, result["isError"], "%v", result)
	content := result["content"].([]any)
	assert.Contains(t, content[0].(map[string]any)["text"], `"webhookSecret":"whs_l1"`)
}
