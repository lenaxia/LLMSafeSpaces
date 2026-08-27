// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package llmsafespaces

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// Epic 67: workspace upload + files-on-send surface. Wire-level tests —
// paths, methods, multipart framing, and body shapes against a mock API.

func TestWorkspacesService_UploadFile(t *testing.T) {
	var gotPath, gotMethod, gotContentType string
	var gotFormFile []byte
	var gotFilename string
	c := newUploadTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotMethod = r.URL.Path, r.Method
		gotContentType = r.Header.Get("Content-Type")
		f, header, err := r.FormFile("file")
		if err != nil {
			t.Errorf("missing file part: %v", err)
			w.WriteHeader(400)
			return
		}
		defer f.Close()
		gotFilename = header.Filename
		gotFormFile, _ = io.ReadAll(f)
		w.WriteHeader(201)
		_ = json.NewEncoder(w).Encode(FileUpload{
			Path: "/workspace/uploads/11111111-2222-3333-4444-555555555555-notes.txt",
			Name: "notes.txt",
			Size: int64(len(gotFormFile)),
		})
	})

	up, err := c.Workspaces.UploadFile(context.Background(), "ws-1", "notes.txt", strings.NewReader("hello"))
	if err != nil {
		t.Fatalf("upload: %v", err)
	}
	if gotPath != "/api/v1/workspaces/ws-1/uploads" || gotMethod != http.MethodPost {
		t.Fatalf("wire: %s %s", gotMethod, gotPath)
	}
	if !strings.HasPrefix(gotContentType, "multipart/form-data") {
		t.Fatalf("content type: %q", gotContentType)
	}
	if gotFilename != "notes.txt" || string(gotFormFile) != "hello" {
		t.Fatalf("part: %q %q", gotFilename, gotFormFile)
	}
	if up.Name != "notes.txt" || up.Size != 5 || !strings.HasPrefix(up.Path, "/workspace/uploads/") {
		t.Fatalf("response: %+v", up)
	}
}

func TestWorkspacesService_UploadFile_StreamedNotBuffered(t *testing.T) {
	c := newUploadTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		n, _ := io.Copy(io.Discard, r.Body)
		w.WriteHeader(201)
		_ = json.NewEncoder(w).Encode(FileUpload{Path: "/workspace/uploads/x", Name: "x", Size: n})
	})
	slow := &oneByteReader{r: strings.NewReader("streamed-body")}
	if _, err := c.Workspaces.UploadFile(context.Background(), "ws-1", "big.bin", slow); err != nil {
		t.Fatalf("upload: %v", err)
	}
	if slow.reads < 2 {
		t.Fatalf("body was buffered before send: %d reads", slow.reads)
	}
}

type oneByteReader struct {
	r     io.Reader
	reads int
}

func (o *oneByteReader) Read(p []byte) (int, error) {
	o.reads++
	if len(p) > 1 {
		p = p[:1]
	}
	return o.r.Read(p)
}

func TestWorkspacesService_UploadFile_ErrorMapping(t *testing.T) {
	tests := []struct {
		name       string
		status     int
		body       string
		wantStatus int
		wantMsg    string
	}{
		{"phase gate", 409, `{"error":"workspace not active","phase":"Suspended"}`, 409, "workspace not active"},
		{"cap", 413, `{"error":"file exceeds size cap"}`, 413, "file exceeds size cap"},
		{"disk", 507, `{"error":"workspace disk is full"}`, 507, "workspace disk is full"},
		{"agentd unreachable", 502, `{"error":"workspace agent unreachable"}`, 502, "workspace agent unreachable"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := newUploadTestClient(t, func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tt.status)
				_, _ = w.Write([]byte(tt.body))
			})
			_, err := c.Workspaces.UploadFile(context.Background(), "ws-1", "f.txt", strings.NewReader("x"))
			apiErr, ok := err.(*APIError)
			if !ok {
				t.Fatalf("want APIError, got %T %v", err, err)
			}
			if apiErr.Status != tt.wantStatus || !strings.Contains(apiErr.Message, tt.wantMsg) {
				t.Fatalf("got %d %q, want %d containing %q", apiErr.Status, apiErr.Message, tt.wantStatus, tt.wantMsg)
			}
		})
	}
}

func TestWorkspacesService_UploadFile_PhaseSurfaced(t *testing.T) {
	c := newUploadTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(409)
		_, _ = w.Write([]byte(`{"error":"workspace not active","phase":"Suspended"}`))
	})
	_, err := c.Workspaces.UploadFile(context.Background(), "ws-1", "f.txt", strings.NewReader("x"))
	apiErr, ok := err.(*APIError)
	if !ok {
		t.Fatalf("want APIError, got %T %v", err, err)
	}
	if apiErr.Phase != "Suspended" {
		t.Fatalf("phase not surfaced: %+v", apiErr)
	}
}

func TestSendPromptAsync_Files(t *testing.T) {
	var gotBody map[string]any
	c := newUploadTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/workspaces/ws-1/sessions/ses_1/prompt" {
			t.Errorf("path: %s", r.URL.Path)
		}
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.WriteHeader(202)
	})
	err := c.Sessions.SendPromptAsync(context.Background(), "ws-1", "ses_1", "review please",
		"/workspace/uploads/11111111-2222-3333-4444-555555555555-notes.txt")
	if err != nil {
		t.Fatalf("prompt: %v", err)
	}
	parts, _ := gotBody["parts"].([]any)
	if len(parts) != 1 {
		t.Fatalf("parts: %+v", gotBody)
	}
	first, _ := parts[0].(map[string]any)
	if first["type"] != "text" || first["text"] != "review please" {
		t.Fatalf("part: %+v", first)
	}
	files, _ := gotBody["files"].([]any)
	if len(files) != 1 || files[0] != "/workspace/uploads/11111111-2222-3333-4444-555555555555-notes.txt" {
		t.Fatalf("files: %+v", gotBody)
	}
}

func TestSendPromptAsync_PartsShape_NoDeadMessageField(t *testing.T) {
	var raw map[string]json.RawMessage
	c := newUploadTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&raw)
		w.WriteHeader(202)
	})
	if err := c.Sessions.SendPromptAsync(context.Background(), "ws-1", "ses_1", "hi", ""); err != nil {
		t.Fatalf("prompt: %v", err)
	}
	if _, dead := raw["message"]; dead {
		t.Fatal("prompt body carries dead 'message' field — the API extracts text from parts")
	}
	if _, dead := raw["content"]; dead {
		t.Fatal("prompt body carries dead 'content' field — the API extracts text from parts")
	}
	if _, ok := raw["parts"]; !ok {
		t.Fatal("prompt body missing parts")
	}
}

func TestEnqueue_Files(t *testing.T) {
	var gotBody map[string]any
	c := newUploadTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/workspaces/ws-1/sessions/ses_1/queue" {
			t.Errorf("path: %s", r.URL.Path)
		}
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.WriteHeader(202)
		_, _ = w.Write([]byte(`{"messageID":"qm-1"}`))
	})
	id, err := c.Sessions.Enqueue(context.Background(), "ws-1", "ses_1", "later",
		"/workspace/uploads/11111111-2222-3333-4444-555555555555-notes.txt")
	if err != nil || id != "qm-1" {
		t.Fatalf("enqueue: %v %q", err, id)
	}
	files, _ := gotBody["files"].([]any)
	if len(files) != 1 || files[0] != "/workspace/uploads/11111111-2222-3333-4444-555555555555-notes.txt" {
		t.Fatalf("files: %+v", gotBody)
	}
	if gotBody["text"] != "later" {
		t.Fatalf("text: %+v", gotBody)
	}
}

func newUploadTestClient(t *testing.T, handler http.HandlerFunc) *Client {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	return New(srv.URL, WithAPIKey("k"))
}
