// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package llmsafespaces

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
)

// Epic 68: workspace file upload + attachment references on send.
//
// UploadFile streams the body as multipart/form-data to
// POST /workspaces/{id}/uploads; the file lands on the workspace PVC
// under /workspace/uploads/<uuid>-<name> and the returned Path is what
// the files parameters of SendPromptAsync/Enqueue accept. The API composes
// the v1 attachment manifest into the dispatched text — callers pass
// paths, never manifest lines.

// UploadFile uploads a file into the workspace. The content is streamed
// (multipart writer over the request body), never buffered whole. The
// workspace must be Active; rejections surface as *APIError with the
// phase on 409s (Phase field) per Epic 68 D5.
func (s *WorkspacesService) UploadFile(ctx context.Context, workspaceID, filename string, content io.Reader) (*FileUpload, error) {
	pr, pw := io.Pipe()
	mw := multipart.NewWriter(pw)

	copyErr := make(chan error, 1)
	go func() {
		part, err := mw.CreateFormFile("file", filename)
		if err == nil {
			_, err = io.Copy(part, content)
		}
		if err == nil {
			err = mw.Close()
		}
		if err != nil {
			_ = pw.CloseWithError(err)
		} else {
			_ = pw.Close()
		}
		copyErr <- err
	}()

	url := s.c.baseURL + "/api/v1/workspaces/" + workspaceID + "/uploads"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, pr)
	if err != nil {
		_ = pw.CloseWithError(err)
		<-copyErr
		return nil, fmt.Errorf("create request: %w", err)
	}
	if err := s.c.authorize(ctx, req); err != nil {
		_ = pw.CloseWithError(err)
		<-copyErr
		return nil, err
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())

	resp, err := s.c.httpClient.Do(req)
	if err != nil {
		_ = pw.CloseWithError(err)
		<-copyErr
		return nil, fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()
	if cerr := <-copyErr; cerr != nil && resp.StatusCode < 400 {
		return nil, fmt.Errorf("stream upload body: %w", cerr)
	}

	if resp.StatusCode >= 400 {
		return nil, parseError(resp)
	}
	var uploaded FileUpload
	if err := json.NewDecoder(resp.Body).Decode(&uploaded); err != nil {
		return nil, fmt.Errorf("decode response: %w", err)
	}
	return &uploaded, nil
}
