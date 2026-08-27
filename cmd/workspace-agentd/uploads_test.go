// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/lenaxia/llmsafespaces/pkg/agentd"
)

const (
	squatUUID = "00000000-0000-0000-0000-000000000001"
	nextUUID  = "00000000-0000-0000-0000-000000000002"
)

func uploadTestConfig(t *testing.T) fileUploadConfig {
	t.Helper()
	return fileUploadConfig{
		uploadsDir:  t.TempDir(),
		maxBytes:    defaultUploadMaxBytes,
		bodyTimeout: defaultUploadBodyTimeout,
		uuid:        fixedUUIDs(),
		create:      openUploadTmpFile,
		rename:      os.Rename,
	}
}

func fixedUUIDs(ids ...string) func() string {
	var i int
	var mu sync.Mutex
	return func() string {
		mu.Lock()
		defer mu.Unlock()
		if i < len(ids) {
			id := ids[i]
			i++
			return id
		}
		i++
		return fmt.Sprintf("00000000-0000-0000-0000-%012d", i)
	}
}

func putUpload(t *testing.T, h http.HandlerFunc, filename string, body []byte) *httptest.ResponseRecorder {
	t.Helper()
	target := "/v1/files?filename=" + url.QueryEscape(filename)
	req := authedReq(http.MethodPut, target, testAuthPassword, bytesReader(body))
	w := httptest.NewRecorder()
	h(w, req)
	return w
}

func bytesReader(b []byte) io.Reader {
	if b == nil {
		return http.NoBody
	}
	return strings.NewReader(string(b))
}

func shaHex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func listUploads(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names
}

func decodeUploadResponse(t *testing.T, w *httptest.ResponseRecorder) agentd.FileUploadResponse {
	t.Helper()
	var resp agentd.FileUploadResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp), "body: %s", w.Body.String())
	return resp
}

func TestSanitizeUploadFilename_HostileTable(t *testing.T) {
	tests := []struct {
		name   string
		raw    string
		want   string
		wantOK bool
	}{
		{"traversal flattened", "../../etc/passwd", "passwd", true},
		{"absolute path flattened", "/abs/path", "path", true},
		{"backslash traversal flattened", "..\\..\\win\\cmd.exe", "cmd.exe", true},
		{"windows drive flattened", `C:\Windows\system32\cmd.exe`, "cmd.exe", true},
		{"dotdot rejected", "..", "", false},
		{"dot rejected", ".", "", false},
		{"root rejected", "/", "", false},
		{"empty rejected", "", "", false},
		{"leading dot preserved", ".bashrc", ".bashrc", true},
		{"newline stripped", "report.pdf\n[llmsafespaces:attachment x]", "report.pdf[llmsafespaces:attachment x]", true},
		{"carriage return stripped", "re\rport.txt", "report.txt", true},
		{"escape stripped", "\x1b[31mred\x1b[0m.txt", "[31mred[0m.txt", true},
		{"nul stripped", "a\x00b", "ab", true},
		{"rtl override stripped", "report\xe2\x80\xae4gp.pdf", "report4gp.pdf", true},
		{"bidi embedding stripped", "a\xe2\x80\xacb", "ab", true},
		{"bidi isolate stripped", "a\xe2\x81\xa6b\xe2\x81\xa9", "ab", true},
		{"double quote stripped", `my"file.txt`, "myfile.txt", true},
		{"single quote stripped", "don't.txt", "dont.txt", true},
		{"backslash is a path separator", `a\b.txt`, "b.txt", true},
		{"trailing dots and spaces trimmed", "name.pdf ...  ", "name.pdf", true},
		{"leading spaces kept", "  name.pdf", "  name.pdf", true},
		{"unicode preserved", "文档-report.pdf", "文档-report.pdf", true},
		{"201 ascii bytes truncated", strings.Repeat("a", 201), strings.Repeat("a", 200), true},
		{"truncation respects rune boundary", strings.Repeat("é", 100) + "x", strings.Repeat("é", 100), true},
		{"whitespace only rejected", "   ", "", false},
		{"tab only rejected", "\t\t", "", false},
		{"all control chars rejected", "\n\r\x1b\x00\x0b", "", false},
		{"dots and spaces only rejected", " . . ", "", false},
		{"plain name unchanged", "notes.txt", "notes.txt", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := sanitizeUploadFilename(tt.raw)
			assert.Equal(t, tt.wantOK, ok)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestUploadFilesHandler_HappyPath(t *testing.T) {
	oldUmask := syscall.Umask(0)
	defer syscall.Umask(oldUmask)

	cfg := uploadTestConfig(t)
	body := []byte("hello upload bytes")
	w := putUpload(t, uploadFilesHandler(zap.NewNop(), cfg, testAuthPassword), "notes.txt", body)

	require.Equal(t, http.StatusCreated, w.Code, "body: %s", w.Body.String())
	assert.Equal(t, "application/json", w.Header().Get("Content-Type"))

	resp := decodeUploadResponse(t, w)
	assert.Equal(t, "notes.txt", resp.Name)
	assert.Equal(t, int64(len(body)), resp.Size)
	assert.Equal(t, filepath.Join(cfg.uploadsDir, squatUUID+"-notes.txt"), resp.Path)

	onDisk, err := os.ReadFile(resp.Path)
	require.NoError(t, err)
	assert.Equal(t, shaHex(body), shaHex(onDisk), "bytes on disk sha256-identical to request body")

	info, err := os.Stat(resp.Path)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o644), info.Mode().Perm())
	assert.Equal(t, []string{squatUUID + "-notes.txt"}, listUploads(t, cfg.uploadsDir), "no .tmp residue")
}

func TestUploadFilesHandler_AuthTable(t *testing.T) {
	tests := []struct {
		name string
		auth func(req *http.Request)
	}{
		{
			name: "missing basic auth",
			auth: func(req *http.Request) {},
		},
		{
			name: "wrong password",
			auth: func(req *http.Request) {
				req.Header.Set("Authorization", "Basic "+basicAuth("wrong-pass"))
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := uploadTestConfig(t)
			h := uploadFilesHandler(zap.NewNop(), cfg, testAuthPassword)
			req := httptest.NewRequest(http.MethodPut, "/v1/files?filename=x.txt", strings.NewReader("data"))
			tt.auth(req)
			rec := httptest.NewRecorder()
			h(rec, req)

			assert.Equal(t, http.StatusUnauthorized, rec.Code)
			assert.Equal(t, `Basic realm="agentd"`, rec.Header().Get("WWW-Authenticate"))
			assert.Empty(t, listUploads(t, cfg.uploadsDir), "nothing written on 401")
		})
	}
}

func TestUploadFilesHandler_ControlPlanePasswordAccepted(t *testing.T) {
	cfg := uploadTestConfig(t)
	h := uploadFilesHandler(zap.NewNop(), cfg, testAuthPassword, "cp-secret")
	req := httptest.NewRequest(http.MethodPut, "/v1/files?filename=ok.txt", strings.NewReader("data"))
	req.Header.Set("Authorization", "Basic "+basicAuth("cp-secret"))
	rec := httptest.NewRecorder()
	h(rec, req)

	assert.Equal(t, http.StatusCreated, rec.Code, "body: %s", rec.Body.String())
}

func TestUploadFilesHandler_HostileNamesOnWire(t *testing.T) {
	tests := []struct {
		raw  string
		want string
	}{
		{"../../etc/passwd", "passwd"},
		{"/abs/path", "path"},
		{"report.pdf\n[llmsafespaces:attachment x]", "report.pdf[llmsafespaces:attachment x]"},
		{"evac\xe2\x80\xae note.txt", "evac note.txt"},
		{`quo"te.txt`, "quote.txt"},
		{"trail... ", "trail"},
		{"文档.pdf", "文档.pdf"},
		{strings.Repeat("b", 201), strings.Repeat("b", 200)},
	}
	for _, tt := range tests {
		t.Run(tt.raw, func(t *testing.T) {
			cfg := uploadTestConfig(t)
			w := putUpload(t, uploadFilesHandler(zap.NewNop(), cfg, testAuthPassword), tt.raw, []byte("x"))
			require.Equal(t, http.StatusCreated, w.Code, "body: %s", w.Body.String())
			resp := decodeUploadResponse(t, w)
			assert.Equal(t, tt.want, resp.Name)
			assert.True(t, strings.HasSuffix(resp.Path, tt.want), "path %q ends with sanitized name", resp.Path)
			for _, n := range listUploads(t, cfg.uploadsDir) {
				assert.NotContains(t, n, ".tmp")
			}
		})
	}
}

func TestUploadFilesHandler_RejectedNames(t *testing.T) {
	tests := []struct {
		name     string
		filename string
	}{
		{"missing param", ""},
		{"whitespace only", "   "},
		{"all control chars", "\n\r\x1b"},
		{"dotdot", ".."},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := uploadTestConfig(t)
			target := "/v1/files"
			if tt.filename != "" {
				target += "?filename=" + url.QueryEscape(tt.filename)
			}
			req := authedReq(http.MethodPut, target, testAuthPassword, strings.NewReader("data"))
			rec := httptest.NewRecorder()
			uploadFilesHandler(zap.NewNop(), cfg, testAuthPassword)(rec, req)

			assert.Equal(t, http.StatusBadRequest, rec.Code, "body: %s", rec.Body.String())
			assert.Empty(t, listUploads(t, cfg.uploadsDir), "nothing written on 400")
		})
	}
}

func TestUploadFilesHandler_CapBoundary(t *testing.T) {
	cfg := uploadTestConfig(t)
	cfg.maxBytes = 1024
	h := uploadFilesHandler(zap.NewNop(), cfg, testAuthPassword)

	exact := make([]byte, 1024)
	_, _ = rand.Read(exact)
	w := putUpload(t, h, "exact.bin", exact)
	require.Equal(t, http.StatusCreated, w.Code, "body: %s", w.Body.String())
	assert.Equal(t, int64(1024), decodeUploadResponse(t, w).Size)

	over := make([]byte, 1025)
	_, _ = rand.Read(over)
	w = putUpload(t, h, "over.bin", over)
	assert.Equal(t, http.StatusRequestEntityTooLarge, w.Code, "body: %s", w.Body.String())
	assert.Equal(t, 1, len(listUploads(t, cfg.uploadsDir)), "rejection leaves no file and no .tmp")
	for _, n := range listUploads(t, cfg.uploadsDir) {
		assert.NotContains(t, n, "over", "rejected upload leaves nothing behind")
		assert.NotContains(t, n, ".tmp")
	}
}

func TestUploadFilesHandler_PipeStreamedSlowBody(t *testing.T) {
	cfg := uploadTestConfig(t)
	h := uploadFilesHandler(zap.NewNop(), cfg, testAuthPassword)

	pr, pw := io.Pipe()
	var payload []byte
	go func() {
		defer func() { _ = pw.Close() }()
		chunk := make([]byte, 512*1024)
		for i := 0; i < 6; i++ {
			_, _ = rand.Read(chunk)
			payload = append(payload, chunk...)
			if _, err := pw.Write(chunk); err != nil {
				return
			}
			time.Sleep(2 * time.Millisecond)
		}
	}()

	req := authedReq(http.MethodPut, "/v1/files?filename=streamed.bin", testAuthPassword, pr)
	rec := httptest.NewRecorder()
	h(rec, req)

	require.Equal(t, http.StatusCreated, rec.Code, "body: %s", rec.Body.String())
	resp := decodeUploadResponse(t, rec)
	assert.Equal(t, int64(3*1024*1024), resp.Size)

	onDisk, err := os.ReadFile(resp.Path)
	require.NoError(t, err)
	assert.Equal(t, shaHex(payload), shaHex(onDisk), "streamed body landed byte-identical")
}

func TestUploadFilesHandler_MidWriteFailureIsAtomic(t *testing.T) {
	cfg := uploadTestConfig(t)
	cfg.create = func(path string) (uploadSink, error) {
		f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
		if err != nil {
			return nil, err
		}
		return &failingWriteSink{f: f, failAfter: 4}, nil
	}

	w := putUpload(t, uploadFilesHandler(zap.NewNop(), cfg, testAuthPassword), "broken.bin", []byte("0123456789"))

	assert.Equal(t, http.StatusInternalServerError, w.Code, "body: %s", w.Body.String())
	assert.Empty(t, listUploads(t, cfg.uploadsDir), "no final file and no .tmp after mid-write failure")
}

func TestUploadFilesHandler_ENOSPC(t *testing.T) {
	cfg := uploadTestConfig(t)
	cfg.create = func(path string) (uploadSink, error) {
		f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
		if err != nil {
			return nil, err
		}
		return &enospcSink{f: f}, nil
	}

	w := putUpload(t, uploadFilesHandler(zap.NewNop(), cfg, testAuthPassword), "big.bin", []byte("0123456789"))

	assert.Equal(t, http.StatusInsufficientStorage, w.Code, "body: %s", w.Body.String())
	assert.Empty(t, listUploads(t, cfg.uploadsDir), ".tmp removed on ENOSPC")
}

func TestUploadFilesHandler_FsyncFailure(t *testing.T) {
	cfg := uploadTestConfig(t)
	cfg.create = func(path string) (uploadSink, error) {
		f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
		if err != nil {
			return nil, err
		}
		return &failingSyncSink{f: f}, nil
	}

	w := putUpload(t, uploadFilesHandler(zap.NewNop(), cfg, testAuthPassword), "nosync.bin", []byte("data"))

	assert.Equal(t, http.StatusInternalServerError, w.Code, "body: %s", w.Body.String())
	assert.Empty(t, listUploads(t, cfg.uploadsDir), ".tmp removed, no final file")
}

func TestUploadFilesHandler_RenameFailure(t *testing.T) {
	cfg := uploadTestConfig(t)
	collisionDir := filepath.Join(cfg.uploadsDir, squatUUID+"-target.bin")
	require.NoError(t, os.MkdirAll(collisionDir, 0o755))

	w := putUpload(t, uploadFilesHandler(zap.NewNop(), cfg, testAuthPassword), "target.bin", []byte("data"))

	assert.Equal(t, http.StatusInternalServerError, w.Code, "body: %s", w.Body.String())
	assert.Equal(t, []string{squatUUID + "-target.bin"}, listUploads(t, cfg.uploadsDir),
		".tmp removed; pre-existing collision target untouched")
	info, err := os.Stat(collisionDir)
	require.NoError(t, err)
	assert.True(t, info.IsDir(), "original target untouched")
}

func TestUploadFilesHandler_FsyncPrecedesRename(t *testing.T) {
	cfg := uploadTestConfig(t)
	var mu sync.Mutex
	var ops []string
	cfg.create = func(path string) (uploadSink, error) {
		f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
		if err != nil {
			return nil, err
		}
		return &recordingSink{f: f, ops: &ops, mu: &mu}, nil
	}
	cfg.rename = func(oldpath, newpath string) error {
		mu.Lock()
		ops = append(ops, "rename")
		mu.Unlock()
		return os.Rename(oldpath, newpath)
	}

	w := putUpload(t, uploadFilesHandler(zap.NewNop(), cfg, testAuthPassword), "order.txt", []byte("data"))
	require.Equal(t, http.StatusCreated, w.Code, "body: %s", w.Body.String())

	mu.Lock()
	defer mu.Unlock()
	assert.Equal(t, []string{"sync", "close", "rename"}, ops, "fsync and close both precede rename")
}

func TestUploadFilesHandler_TmpSquatSymlink(t *testing.T) {
	cfg := uploadTestConfig(t)
	cfg.uuid = fixedUUIDs(squatUUID, nextUUID)
	require.NoError(t, os.MkdirAll(cfg.uploadsDir, 0o755))

	outside := filepath.Join(t.TempDir(), "outside.txt")
	require.NoError(t, os.WriteFile(outside, []byte("SENTINEL"), 0o644))
	symlinkPath := filepath.Join(cfg.uploadsDir, squatUUID+"-target.bin.tmp")
	require.NoError(t, os.Symlink(outside, symlinkPath))

	w := putUpload(t, uploadFilesHandler(zap.NewNop(), cfg, testAuthPassword), "target.bin", []byte("payload"))

	require.Equal(t, http.StatusCreated, w.Code, "body: %s", w.Body.String())
	resp := decodeUploadResponse(t, w)
	assert.Equal(t, filepath.Join(cfg.uploadsDir, nextUUID+"-target.bin"), resp.Path, "succeeded on retried uuid")

	link, err := os.Readlink(symlinkPath)
	require.NoError(t, err, "symlink still a symlink — O_EXCL did not follow or replace it")
	assert.Equal(t, outside, link)
	outsideContent, err := os.ReadFile(outside)
	require.NoError(t, err)
	assert.Equal(t, "SENTINEL", string(outsideContent), "nothing written through the symlink")
}

func TestUploadFilesHandler_TmpSquatPlainFile(t *testing.T) {
	cfg := uploadTestConfig(t)
	cfg.uuid = fixedUUIDs(squatUUID, nextUUID)
	require.NoError(t, os.MkdirAll(cfg.uploadsDir, 0o755))
	squatPath := filepath.Join(cfg.uploadsDir, squatUUID+"-target.bin.tmp")
	require.NoError(t, os.WriteFile(squatPath, []byte("ADVERSARY"), 0o644))

	w := putUpload(t, uploadFilesHandler(zap.NewNop(), cfg, testAuthPassword), "target.bin", []byte("payload"))

	require.Equal(t, http.StatusCreated, w.Code, "body: %s", w.Body.String())
	resp := decodeUploadResponse(t, w)
	assert.Equal(t, filepath.Join(cfg.uploadsDir, nextUUID+"-target.bin"), resp.Path)

	squat, err := os.ReadFile(squatPath)
	require.NoError(t, err)
	assert.Equal(t, "ADVERSARY", string(squat), "squatted .tmp not clobbered")
	final, err := os.ReadFile(resp.Path)
	require.NoError(t, err)
	assert.Equal(t, "payload", string(final))
}

func TestUploadFilesHandler_EEXISTExhausted(t *testing.T) {
	cfg := uploadTestConfig(t)
	cfg.uuid = fixedUUIDs(squatUUID, squatUUID, squatUUID)
	require.NoError(t, os.MkdirAll(cfg.uploadsDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(cfg.uploadsDir, squatUUID+"-busy.bin.tmp"), []byte("X"), 0o644))

	w := putUpload(t, uploadFilesHandler(zap.NewNop(), cfg, testAuthPassword), "busy.bin", []byte("payload"))

	assert.Equal(t, http.StatusInternalServerError, w.Code, "body: %s", w.Body.String())
	assert.Equal(t, []string{squatUUID + "-busy.bin.tmp"}, listUploads(t, cfg.uploadsDir))
}

func TestUploadFilesHandler_UploadsDirAutoCreated(t *testing.T) {
	oldUmask := syscall.Umask(0)
	defer syscall.Umask(oldUmask)

	cfg := uploadTestConfig(t)
	cfg.uploadsDir = filepath.Join(cfg.uploadsDir, "nested", "uploads")
	h := uploadFilesHandler(zap.NewNop(), cfg, testAuthPassword)

	w := putUpload(t, h, "one.txt", []byte("one"))
	require.Equal(t, http.StatusCreated, w.Code, "body: %s", w.Body.String())

	info, err := os.Stat(cfg.uploadsDir)
	require.NoError(t, err)
	assert.True(t, info.IsDir())
	assert.Equal(t, os.FileMode(0o755), info.Mode().Perm())

	w = putUpload(t, h, "two.txt", []byte("two"))
	require.Equal(t, http.StatusCreated, w.Code, "second upload on existing dir succeeds")
}

func TestUploadFilesHandler_Concurrent32DistinctHashes(t *testing.T) {
	cfg := uploadTestConfig(t)
	srv := httptest.NewServer(uploadFilesHandler(zap.NewNop(), cfg, testAuthPassword))
	defer srv.Close()

	const n = 32
	payloads := make([][]byte, n)
	for i := range payloads {
		payloads[i] = make([]byte, 64*1024+i)
		_, _ = rand.Read(payloads[i])
	}

	paths := make([]string, n)
	codes := make([]int, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			req, err := http.NewRequest(http.MethodPut,
				fmt.Sprintf("%s/v1/files?filename=file-%d.bin", srv.URL, i),
				bytesReader(payloads[i]))
			if err != nil {
				return
			}
			req.Header.Set("Authorization", "Basic "+basicAuth(testAuthPassword))
			resp, err := srv.Client().Do(req)
			if err != nil {
				return
			}
			defer func() { _ = resp.Body.Close() }()
			codes[i] = resp.StatusCode
			var body agentd.FileUploadResponse
			if json.NewDecoder(resp.Body).Decode(&body) == nil {
				paths[i] = body.Path
			}
		}(i)
	}
	wg.Wait()

	distinct := make(map[string]bool)
	for i := 0; i < n; i++ {
		require.Equal(t, http.StatusCreated, codes[i], "upload %d failed", i)
		require.NotEmpty(t, paths[i], "upload %d missing path", i)
		distinct[paths[i]] = true

		onDisk, err := os.ReadFile(paths[i])
		require.NoError(t, err)
		assert.Equal(t, shaHex(payloads[i]), shaHex(onDisk), "upload %d corrupted", i)
	}
	assert.Len(t, distinct, n, "32 distinct uuid paths")
	assert.Len(t, listUploads(t, cfg.uploadsDir), n)
}

func TestUploadFilesHandler_SameNameTwiceDistinctPaths(t *testing.T) {
	cfg := uploadTestConfig(t)
	cfg.uuid = fixedUUIDs(squatUUID, nextUUID)
	h := uploadFilesHandler(zap.NewNop(), cfg, testAuthPassword)

	w1 := putUpload(t, h, "dup.txt", []byte("first"))
	require.Equal(t, http.StatusCreated, w1.Code)
	w2 := putUpload(t, h, "dup.txt", []byte("second"))
	require.Equal(t, http.StatusCreated, w2.Code)

	p1 := decodeUploadResponse(t, w1).Path
	p2 := decodeUploadResponse(t, w2).Path
	assert.NotEqual(t, p1, p2, "no overwrite")

	b1, _ := os.ReadFile(p1)
	b2, _ := os.ReadFile(p2)
	assert.Equal(t, "first", string(b1))
	assert.Equal(t, "second", string(b2))
}

func TestUploadFilesHandler_SlowlorisBodyStall(t *testing.T) {
	cfg := uploadTestConfig(t)
	cfg.bodyTimeout = 150 * time.Millisecond
	srv := httptest.NewServer(uploadFilesHandler(zap.NewNop(), cfg, testAuthPassword))
	defer srv.Close()

	pr, pw := io.Pipe()
	go func() {
		_, _ = pw.Write([]byte("partial"))
		time.Sleep(1 * time.Second)
		_, _ = pw.Write([]byte("too-late"))
		_ = pw.Close()
	}()

	req, err := http.NewRequest(http.MethodPut, srv.URL+"/v1/files?filename=slow.txt", pr)
	require.NoError(t, err)
	req.Header.Set("Authorization", "Basic "+basicAuth(testAuthPassword))
	resp, err := srv.Client().Do(req)
	if err == nil {
		defer func() { _ = resp.Body.Close() }()
		assert.Equal(t, http.StatusGatewayTimeout, resp.StatusCode, "write deadline aborts stalled body")
	}

	assert.Empty(t, listUploads(t, cfg.uploadsDir), "no file and no .tmp residue after slowloris abort")
}

func TestUploadFilesHandler_MethodNotAllowed(t *testing.T) {
	for _, method := range []string{http.MethodGet, http.MethodPost, http.MethodDelete} {
		t.Run(method, func(t *testing.T) {
			cfg := uploadTestConfig(t)
			req := authedReq(method, "/v1/files?filename=x.txt", testAuthPassword, strings.NewReader("data"))
			rec := httptest.NewRecorder()
			uploadFilesHandler(zap.NewNop(), cfg, testAuthPassword)(rec, req)

			assert.Equal(t, http.StatusMethodNotAllowed, rec.Code)
			assert.Equal(t, http.MethodPut, rec.Header().Get("Allow"))
			assert.Empty(t, listUploads(t, cfg.uploadsDir))
		})
	}
}

func TestUploadFilesHandler_NoPathLeakInResponses(t *testing.T) {
	cfg := uploadTestConfig(t)
	h := uploadFilesHandler(zap.NewNop(), cfg, testAuthPassword)

	success := putUpload(t, h, "ok.txt", []byte("data"))
	require.Equal(t, http.StatusCreated, success.Code)
	assert.NotContains(t, success.Body.String(), ".tmp")

	collision := uploadTestConfig(t)
	_ = os.MkdirAll(filepath.Join(collision.uploadsDir, squatUUID+"-fail.bin"), 0o755)
	failRename := uploadFilesHandler(zap.NewNop(), collision, testAuthPassword)
	w := putUpload(t, failRename, "fail.bin", []byte("data"))
	assert.Equal(t, http.StatusInternalServerError, w.Code)
	assert.NotContains(t, w.Body.String(), collision.uploadsDir, "no internal path leak")
	assert.NotContains(t, w.Body.String(), ".tmp")

	capCfg := uploadTestConfig(t)
	capCfg.maxBytes = 4
	w = putUpload(t, uploadFilesHandler(zap.NewNop(), capCfg, testAuthPassword), "cap.txt", []byte("toolarge"))
	assert.Equal(t, http.StatusRequestEntityTooLarge, w.Code)
	assert.NotContains(t, w.Body.String(), capCfg.uploadsDir)
	assert.NotContains(t, w.Body.String(), ".tmp")
}

func TestScrubUploadTmpFiles(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "a.tmp"), []byte("a"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "b.tmp"), []byte("b"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "c-report.txt"), []byte("c"), 0o644))

	removed, err := scrubUploadTmpFiles(dir)
	require.NoError(t, err)
	assert.Equal(t, 2, removed)
	assert.Equal(t, []string{"c-report.txt"}, listUploads(t, dir), "both .tmp gone, real upload untouched")
}

func TestScrubUploadTmpFiles_MissingDir(t *testing.T) {
	removed, err := scrubUploadTmpFiles(filepath.Join(t.TempDir(), "absent"))
	assert.NoError(t, err)
	assert.Equal(t, 0, removed)
}

func TestUploadConfigFromEnv(t *testing.T) {
	t.Run("defaults", func(t *testing.T) {
		t.Setenv("LLMSAFESPACES_UPLOADS_PATH", "")
		t.Setenv("UPLOAD_MAX_BYTES", "")
		t.Setenv("UPLOAD_TIMEOUT_MS", "")
		cfg := uploadConfigFromEnv()
		assert.Equal(t, agentd.UploadsPath, cfg.uploadsDir)
		assert.Equal(t, int64(25<<20), cfg.maxBytes)
		assert.Equal(t, 5*time.Minute, cfg.bodyTimeout)
		assert.NotNil(t, cfg.uuid)
		assert.NotNil(t, cfg.create)
		assert.NotNil(t, cfg.rename)
	})

	t.Run("overrides", func(t *testing.T) {
		t.Setenv("LLMSAFESPACES_UPLOADS_PATH", "/custom/uploads")
		t.Setenv("UPLOAD_MAX_BYTES", "4096")
		t.Setenv("UPLOAD_TIMEOUT_MS", "2000")
		cfg := uploadConfigFromEnv()
		assert.Equal(t, "/custom/uploads", cfg.uploadsDir)
		assert.Equal(t, int64(4096), cfg.maxBytes)
		assert.Equal(t, 2*time.Second, cfg.bodyTimeout)
	})

	t.Run("invalid values fall back", func(t *testing.T) {
		t.Setenv("UPLOAD_MAX_BYTES", "not-a-number")
		t.Setenv("UPLOAD_TIMEOUT_MS", "-5")
		cfg := uploadConfigFromEnv()
		assert.Equal(t, int64(25<<20), cfg.maxBytes)
		assert.Equal(t, 5*time.Minute, cfg.bodyTimeout)
	})

	t.Run("sub-second timeout floor", func(t *testing.T) {
		t.Setenv("UPLOAD_TIMEOUT_MS", "500")
		cfg := uploadConfigFromEnv()
		assert.Equal(t, 5*time.Minute, cfg.bodyTimeout)
	})
}

func TestUploadMetrics_Counters(t *testing.T) {
	pkgOpsMetrics.RecordUploadOutcome("ws-upl-1", uploadOutcomeAccepted)
	pkgOpsMetrics.RecordUploadOutcome("ws-upl-1", uploadOutcomeRejectedCap)
	pkgOpsMetrics.RecordUploadOutcome("ws-upl-1", uploadOutcomeRejectedCap)
	pkgOpsMetrics.RecordUploadOutcome("ws-upl-2", uploadOutcomeWriteError)
	pkgOpsMetrics.RecordUploadScrub("ws-upl-1", 3)

	assert.Equal(t, 1.0, testutil.ToFloat64(pkgOpsMetrics.fileUploads.WithLabelValues("ws-upl-1", string(uploadOutcomeAccepted))))
	assert.Equal(t, 2.0, testutil.ToFloat64(pkgOpsMetrics.fileUploads.WithLabelValues("ws-upl-1", string(uploadOutcomeRejectedCap))))
	assert.Equal(t, 1.0, testutil.ToFloat64(pkgOpsMetrics.fileUploads.WithLabelValues("ws-upl-2", string(uploadOutcomeWriteError))))
	assert.Equal(t, 3.0, testutil.ToFloat64(pkgOpsMetrics.uploadScrubRemoved.WithLabelValues("ws-upl-1")))
}

func TestUserMuxWiresUploadEndpoint(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("LLMSAFESPACES_UPLOADS_PATH", dir)

	mux := buildUserMux(context.Background(), &sync.WaitGroup{}, serverDeps{password: testAuthPassword})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	body := []byte("wired")
	req, err := http.NewRequest(http.MethodPut, srv.URL+"/v1/files?filename=wired.txt", bytesReader(body))
	require.NoError(t, err)
	req.Header.Set("Authorization", "Basic "+basicAuth(testAuthPassword))
	resp, err := srv.Client().Do(req)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	require.Equal(t, http.StatusCreated, resp.StatusCode)

	var uploaded agentd.FileUploadResponse
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&uploaded))
	assert.True(t, strings.HasPrefix(uploaded.Path, dir+string(os.PathSeparator)),
		"path %q lands under the env-overridden uploads root", uploaded.Path)
	assert.True(t, strings.HasSuffix(uploaded.Path, "-wired.txt"), "path %q", uploaded.Path)
	_, err = os.Stat(uploaded.Path)
	require.NoError(t, err, "file materialized at the returned path")

	unauth, err := http.NewRequest(http.MethodPut, srv.URL+"/v1/files?filename=no.txt", bytesReader(body))
	require.NoError(t, err)
	resp2, err := srv.Client().Do(unauth)
	require.NoError(t, err)
	defer func() { _ = resp2.Body.Close() }()
	assert.Equal(t, http.StatusUnauthorized, resp2.StatusCode)
}

// The fault-injection sinks hold *os.File as a named field (never
// embedded): embedding would promote os.File.ReadFrom and io.Copy would
// bypass the Write override entirely.

type failingWriteSink struct {
	f         *os.File
	failAfter int64
	written   int64
}

func (s *failingWriteSink) Write(p []byte) (int, error) {
	if s.written+int64(len(p)) > s.failAfter {
		return 0, errors.New("injected write failure")
	}
	s.written += int64(len(p))
	return s.f.Write(p)
}

func (s *failingWriteSink) Sync() error { return s.f.Sync() }

func (s *failingWriteSink) Close() error { return s.f.Close() }

type failingSyncSink struct{ f *os.File }

func (s *failingSyncSink) Write(p []byte) (int, error) { return s.f.Write(p) }
func (s *failingSyncSink) Sync() error                 { return errors.New("injected fsync failure") }
func (s *failingSyncSink) Close() error                { return s.f.Close() }

type enospcSink struct{ f *os.File }

func (s *enospcSink) Write(p []byte) (int, error) {
	return 0, &fsPathError{op: "write", err: syscall.ENOSPC}
}
func (s *enospcSink) Sync() error  { return s.f.Sync() }
func (s *enospcSink) Close() error { return s.f.Close() }

type fsPathError struct {
	op  string
	err error
}

func (e *fsPathError) Error() string { return e.op + ": " + e.err.Error() }
func (e *fsPathError) Unwrap() error { return e.err }

type recordingSink struct {
	f   *os.File
	ops *[]string
	mu  *sync.Mutex
}

func (s *recordingSink) record(op string) {
	s.mu.Lock()
	*s.ops = append(*s.ops, op)
	s.mu.Unlock()
}

func (s *recordingSink) Write(p []byte) (int, error) { return s.f.Write(p) }

func (s *recordingSink) Sync() error {
	if err := s.f.Sync(); err != nil {
		return err
	}
	s.record("sync")
	return nil
}

func (s *recordingSink) Close() error {
	if err := s.f.Close(); err != nil {
		return err
	}
	s.record("close")
	return nil
}
