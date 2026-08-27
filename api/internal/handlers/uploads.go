// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/lenaxia/llmsafespaces/api/internal/services/metrics"
	"github.com/lenaxia/llmsafespaces/pkg/agent/systemnotices"
	"github.com/lenaxia/llmsafespaces/pkg/agentd"
	v1 "github.com/lenaxia/llmsafespaces/pkg/apis/llmsafespaces/v1"
)

// Epic 68 US-68.2 — POST /api/v1/workspaces/:id/uploads.
//
// Streaming multipart upload that forwards the single "file" part to the
// workspace pod's agentd user-mux endpoint (PUT /v1/files on :4097, US-68.1)
// under Basic auth. Gate order is the D16 contract: auth/access (middleware)
// → phase (Active only) → disk (critical ratio) → cap. The file part is
// NEVER buffered: it is piped from the client's multipart stream straight
// into the agentd request body inside a cap+1 LimitReader envelope, so a
// chunked overrun aborts the upstream request mid-stream. Client
// disconnects propagate via the request context (the agentd request is a
// context child, and its body is an io.Pipe the copy goroutine closes with
// the failure).

const (
	uploadFileFieldName = "file"
	// uploadEnvelopeAllowance covers multipart framing (preamble,
	// boundaries, part headers, disposition with a max-length filename) so
	// a file of exactly the cap is not locally rejected for its envelope.
	uploadEnvelopeAllowance = int64(64 * 1024)
	defaultUploadMaxBytes   = int64(25 << 20)
	// defaultUploadStreamTimeout mirrors agentd's own body deadline
	// (defaultUploadBodyTimeout, 5 min): the API hop must not abort a body
	// agentd would still accept. The sibling 5 s JSON-POST convention
	// (agentpush, agent reload) does not fit a 25 MiB stream.
	defaultUploadStreamTimeout = 5 * time.Minute
	uploadTimeoutEnvFloorMs    = 1000
	uploadResponseReadCap      = int64(64 * 1024)
	uploadNonFileFieldDrainCap = int64(1 << 20)
	uploadPostScanDrainCap     = int64(1 << 20)
)

const (
	uploadReasonSuccess   = "success"
	uploadReasonCap       = "cap"
	uploadReasonPhase     = "phase"
	uploadReasonDisk      = "disk"
	uploadReasonAgentdErr = "agentd_error"
)

var (
	errUploadNoFilePart        = errors.New(`multipart body must contain a file part named "file"`)
	errUploadMultipleFileParts = errors.New(`multipart body must contain exactly one file part named "file"`)
	errUploadEmptyFilename     = errors.New("file part has an empty or invalid filename")
	errUploadMalformedBody     = errors.New("malformed multipart body")
	errUploadOverCap           = errors.New("upload exceeds size cap")
)

// uploadMaxBytesEnv and uploadStreamTimeoutEnv follow the DISK_*_THRESHOLD
// package-var pattern (systemnotices) and agentd's UPLOAD_MAX_BYTES /
// UPLOAD_TIMEOUT_MS knobs, which these mirror on the API hop.
var (
	uploadMaxBytesEnv      = envUploadInt64Or("UPLOAD_MAX_BYTES", defaultUploadMaxBytes)
	uploadStreamTimeoutEnv = envUploadDurationMsOr("UPLOAD_TIMEOUT_MS", defaultUploadStreamTimeout)
)

func envUploadInt64Or(key string, def int64) int64 {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil && n > 0 {
			return n
		}
	}
	return def
}

func envUploadDurationMsOr(key string, def time.Duration) time.Duration {
	if v := os.Getenv(key); v != "" {
		if ms, err := strconv.Atoi(v); err == nil && ms > uploadTimeoutEnvFloorMs {
			return time.Duration(ms) * time.Millisecond
		}
	}
	return def
}

// uploadForwardError is the typed classification of a failed agentd
// dispatch: the HTTP status to emit, the metric reason, and a
// public-safe message (agentd internals never leak).
type uploadForwardError struct {
	status int
	reason string
	public string
}

func (e *uploadForwardError) Error() string { return e.public }

// SetUploadLimitsForTest overrides the per-request upload cap and agentd
// stream timeout. Zero values fall back to the env-derived defaults.
func (h *ProxyHandler) SetUploadLimitsForTest(maxBytes int64, streamTimeout time.Duration) {
	h.uploadMaxBytesOverride = maxBytes
	h.uploadStreamTimeoutOverride = streamTimeout
}

func (h *ProxyHandler) uploadCap() int64 {
	if h.uploadMaxBytesOverride > 0 {
		return h.uploadMaxBytesOverride
	}
	return uploadMaxBytesEnv
}

func (h *ProxyHandler) uploadStreamTimeout() time.Duration {
	if h.uploadStreamTimeoutOverride > 0 {
		return h.uploadStreamTimeoutOverride
	}
	return uploadStreamTimeoutEnv
}

// UploadFile handles POST /api/v1/workspaces/:id/uploads.
//
//	@Summary      Upload a file into the workspace
//	@Description  Streams a multipart/form-data file part to the workspace pod's agentd ingest endpoint; the file lands on the workspace PVC under /workspace/uploads/. Gates: workspace phase must be Active (409), disk usage below the critical threshold (507), body within the upload cap (413).
//	@Tags         workspaces
//	@Accept       multipart/form-data
//	@Produce      json
//	@Param        id   path string true "Workspace ID"
//	@Param        file formData file true "Single file part (field name \"file\")"
//	@Success      201 {object} agentd.FileUploadResponse
//	@Failure      400 {object} object{error=string} "malformed multipart body"
//	@Failure      401 {object} object{error=string} "authentication required"
//	@Failure      404 {object} object{error=string} "workspace not found"
//	@Failure      409 {object} object{error=string,phase=string} "workspace not Active"
//	@Failure      413 {object} object{error=string} "file exceeds size cap"
//	@Failure      415 {object} object{error=string} "content type must be multipart/form-data"
//	@Failure      502 {object} object{error=string} "workspace agent upload failed"
//	@Failure      504 {object} object{error=string} "workspace agent timed out"
//	@Failure      507 {object} object{error=string} "workspace disk is full"
//	@Router       /workspaces/{id}/uploads [post]
func (h *ProxyHandler) UploadFile(c *gin.Context) {
	workspaceID := c.Param("id")
	if workspaceID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "workspace ID required"})
		return
	}

	workspace, ok := h.resolveWorkspaceCRDForUpload(c, workspaceID)
	if !ok {
		return
	}

	if workspace.Status.Phase != phaseActive || workspace.Status.PodIP == "" {
		metrics.RecordUploadRequest(uploadReasonPhase)
		c.JSON(http.StatusConflict, gin.H{
			"error": "workspace not active",
			"phase": workspace.Status.Phase,
		})
		return
	}

	_, criticalThreshold := systemnotices.Thresholds()
	if diskPressureRatio(workspace.Status.DiskUsedBytes, workspace.Status.DiskTotalBytes) >= criticalThreshold {
		metrics.RecordUploadRequest(uploadReasonDisk)
		c.JSON(http.StatusInsufficientStorage, gin.H{"error": "workspace disk is full"})
		return
	}

	cap := h.uploadCap()
	if c.Request.ContentLength > cap+uploadEnvelopeAllowance {
		metrics.RecordUploadRequest(uploadReasonCap)
		c.JSON(http.StatusRequestEntityTooLarge, gin.H{"error": "request exceeds upload size cap"})
		return
	}

	mediaType, params, err := mime.ParseMediaType(c.Request.Header.Get("Content-Type"))
	if err != nil || mediaType != "multipart/form-data" || params["boundary"] == "" {
		c.JSON(http.StatusUnsupportedMediaType, gin.H{"error": "content type must be multipart/form-data"})
		return
	}

	mr := multipart.NewReader(c.Request.Body, params["boundary"])
	filePart, filename, err := locateUploadFilePart(mr)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	password, err := h.getPassword(c.Request.Context(), workspaceID)
	if err != nil {
		h.logger.Error("upload: failed to get workspace password", err, "workspaceID", workspaceID)
		metrics.RecordUploadRequest(uploadReasonAgentdErr)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to retrieve workspace credentials"})
		return
	}

	resp, fwdErr := h.forwardUploadToAgentd(c.Request.Context(), workspace.Status.PodIP, filename, password, filePart, cap)
	if fwdErr != nil {
		metrics.RecordUploadRequest(fwdErr.reason)
		c.JSON(fwdErr.status, gin.H{"error": fwdErr.public})
		return
	}
	defer func() { _ = resp.Body.Close() }()

	switch resp.StatusCode {
	case http.StatusCreated:
		var uploaded agentd.FileUploadResponse
		if err := json.NewDecoder(io.LimitReader(resp.Body, uploadResponseReadCap)).Decode(&uploaded); err != nil {
			h.logger.Warn("upload: agentd response undecodable", "workspaceID", workspaceID, "status", resp.StatusCode)
			metrics.RecordUploadRequest(uploadReasonAgentdErr)
			c.JSON(http.StatusBadGateway, gin.H{"error": "workspace agent returned an invalid response"})
			return
		}
		if err := rejectExtraUploadFileParts(mr); err != nil {
			h.logger.Warn("upload: client sent multiple file parts", "workspaceID", workspaceID)
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		metrics.RecordUploadRequest(uploadReasonSuccess)
		c.JSON(http.StatusCreated, uploaded)

	case http.StatusRequestEntityTooLarge:
		metrics.RecordUploadRequest(uploadReasonCap)
		c.JSON(http.StatusRequestEntityTooLarge, gin.H{"error": "file exceeds size cap"})

	default:
		h.logger.Warn("upload: agentd rejected",
			"workspaceID", workspaceID, "status", resp.StatusCode)
		metrics.RecordUploadRequest(uploadReasonAgentdErr)
		c.JSON(http.StatusBadGateway, gin.H{"error": "workspace agent upload failed"})
	}
}

// resolveWorkspaceCRDForUpload mirrors the workspace resolution of
// proxyToWorkspaceWithErrBody: gin-context cache when present (tests),
// otherwise a K8s CRD get — a workspace deleted mid-request 404s here,
// before any agentd dial.
func (h *ProxyHandler) resolveWorkspaceCRDForUpload(c *gin.Context, workspaceID string) (*v1.Workspace, bool) {
	if cached, exists := c.Get("workspace"); exists {
		if ws, ok := cached.(*v1.Workspace); ok && ws != nil {
			return ws, true
		}
	}
	v1Client, err := h.k8sClient.LlmsafespacesV1()
	if err != nil {
		h.logger.Error("Failed to get LLMSafespacesV1 client", err, "workspaceID", workspaceID)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "internal error"})
		return nil, false
	}
	workspace, err := v1Client.Workspaces(h.namespace).Get(c.Request.Context(), workspaceID, metav1.GetOptions{})
	if err != nil {
		h.logger.Error("Failed to get workspace CRD", err, "workspaceID", workspaceID)
		c.JSON(http.StatusNotFound, gin.H{"error": "workspace not found"})
		return nil, false
	}
	return workspace, true
}

// locateUploadFilePart walks the multipart stream, draining non-file form
// fields (U1.2.18), and returns the first part named "file" together with
// its sanitized filename (D9: API-side sanitization before forwarding).
// The part's body is NOT read — streaming starts later, straight into the
// agentd request.
func locateUploadFilePart(mr *multipart.Reader) (*multipart.Part, string, error) {
	for {
		p, err := mr.NextPart()
		if errors.Is(err, io.EOF) {
			return nil, "", errUploadNoFilePart
		}
		if err != nil {
			return nil, "", fmt.Errorf("%w", errUploadMalformedBody)
		}
		if p.FormName() != uploadFileFieldName {
			_, _ = io.Copy(io.Discard, io.LimitReader(p, uploadNonFileFieldDrainCap))
			continue
		}
		filename, ok := agentd.SanitizeFilename(p.FileName())
		if !ok {
			return nil, "", errUploadEmptyFilename
		}
		return p, filename, nil
	}
}

// rejectExtraUploadFileParts scans the remainder of the multipart stream
// for additional "file" parts (U1.2.10). It runs after the agentd upload
// completed but before the client is acknowledged, so a two-file-part
// request is rejected 400; the already-stored first file becomes an orphan
// (accepted, D19-adjacent: retry-orphan semantics). The scan drains at
// most uploadPostScanDrainCap bytes — beyond that (or on any tail read
// error) the tail is ignored: the first file already succeeded and the
// tail is the client's problem.
func rejectExtraUploadFileParts(mr *multipart.Reader) error {
	drained := int64(0)
	for {
		p, err := mr.NextPart()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return nil
		}
		if p.FormName() == uploadFileFieldName {
			return errUploadMultipleFileParts
		}
		n, _ := io.Copy(io.Discard, io.LimitReader(p, uploadNonFileFieldDrainCap))
		drained += n
		if drained >= uploadPostScanDrainCap {
			return nil
		}
	}
}

// forwardUploadToAgentd streams the file part into a PUT /v1/files request
// against the pod's agentd user mux. The body is an io.Pipe fed by a copy
// goroutine reading the (cap+1-limited) file part; over-cap truncation and
// mid-stream client failures close the pipe with an error, aborting the
// agentd request so no partial file survives (agentd removes its .tmp on
// any body error — US-68.1 D3).
func (h *ProxyHandler) forwardUploadToAgentd(
	ctx context.Context,
	podIP, filename, password string,
	filePart *multipart.Part,
	cap int64,
) (*http.Response, *uploadForwardError) {
	pr, pw := io.Pipe()
	limited := io.LimitReader(filePart, cap+1)

	type copyResult struct {
		n   int64
		err error
	}
	copyCh := make(chan copyResult, 1)
	go func() {
		n, err := io.Copy(pw, limited)
		if err == nil && n > cap {
			_ = pw.CloseWithError(errUploadOverCap)
			copyCh <- copyResult{n: n, err: errUploadOverCap}
			return
		}
		if err != nil {
			_ = pw.CloseWithError(err)
		} else {
			_ = pw.Close()
		}
		copyCh <- copyResult{n: n, err: err}
	}()

	query := url.Values{}
	query.Set("filename", filename)
	target := fmt.Sprintf("http://%s:%d/v1/files?%s", podIP, agentd.AgentdPort, query.Encode())

	upCtx, cancel := context.WithTimeout(ctx, h.uploadStreamTimeout())
	defer cancel()

	req, err := http.NewRequestWithContext(upCtx, http.MethodPut, target, pr)
	if err != nil {
		cancel()
		_ = pr.CloseWithError(err)
		<-copyCh
		return nil, &uploadForwardError{
			status: http.StatusInternalServerError,
			reason: uploadReasonAgentdErr,
			public: "internal error",
		}
	}
	req.SetBasicAuth(agentd.AuthUsername, password)
	req.Header.Set("Content-Type", "application/octet-stream")

	resp, err := h.httpClient.Do(req)
	if err != nil {
		cancel()
		_ = pr.CloseWithError(err)
		cr := <-copyCh
		if errors.Is(cr.err, errUploadOverCap) {
			return nil, &uploadForwardError{
				status: http.StatusRequestEntityTooLarge,
				reason: uploadReasonCap,
				public: "file exceeds size cap",
			}
		}
		if isUploadTimeout(err) {
			return nil, &uploadForwardError{
				status: http.StatusGatewayTimeout,
				reason: uploadReasonAgentdErr,
				public: "workspace agent timed out",
			}
		}
		return nil, &uploadForwardError{
			status: http.StatusBadGateway,
			reason: uploadReasonAgentdErr,
			public: "workspace agent unreachable",
		}
	}

	cr := <-copyCh
	if errors.Is(cr.err, errUploadOverCap) {
		_ = resp.Body.Close()
		return nil, &uploadForwardError{
			status: http.StatusRequestEntityTooLarge,
			reason: uploadReasonCap,
			public: "file exceeds size cap",
		}
	}
	if cr.err != nil {
		// The copy failed after agentd already answered (e.g. agentd
		// responded early with a smaller cap and dropped the rest of
		// the body). agentd's status is authoritative for what landed
		// on disk — fall through and map it below.
		h.logger.Warn("upload: body stream ended with error after agentd response", "error", cr.err.Error())
	}

	return resp, nil
}

// isUploadTimeout classifies transport errors produced by the per-request
// deadline (or the shared client's own timeout) for 504 mapping.
func isUploadTimeout(err error) bool {
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return true
	}
	return os.IsTimeout(err)
}
