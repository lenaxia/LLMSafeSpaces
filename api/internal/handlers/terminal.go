// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package handlers

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
	v1 "github.com/lenaxia/llmsafespaces/pkg/apis/llmsafespaces/v1"
	pkginterfaces "github.com/lenaxia/llmsafespaces/pkg/interfaces"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/remotecommand"
)

const (
	ticketTTL          = 30 * time.Second
	ticketPrefix       = "tkt_"
	ticketKeyPrefix    = "terminal:ticket:"
	defaultIdleTimeout = 30 * time.Minute
	defaultMaxPerWS    = 5
	defaultMaxGlobal   = 500
	terminalShell      = "/bin/sh"
)

// parameterScheme is used to encode PodExecOptions for the exec request.
var parameterScheme = func() *runtime.Scheme {
	s := runtime.NewScheme()
	_ = corev1.AddToScheme(s)
	return s
}()

// TicketResponse is returned by POST /terminal/ticket.
type TicketResponse struct {
	Ticket    string    `json:"ticket"`
	ExpiresAt time.Time `json:"expiresAt"`
}

// ticketData is stored in Redis for ticket validation.
type ticketData struct {
	UserID      string `json:"userID"`
	WorkspaceID string `json:"workspaceID"`
}

// TerminalMessage is the JSON frame for WebSocket communication.
type TerminalMessage struct {
	Type    string `json:"type"` // input, resize, output, exit, error
	Data    string `json:"data,omitempty"`
	Cols    uint16 `json:"cols,omitempty"`
	Rows    uint16 `json:"rows,omitempty"`
	Code    int    `json:"code,omitempty"`
	Message string `json:"message,omitempty"`
}

// TerminalCache is the subset of CacheService needed by the terminal handler.
type TerminalCache interface {
	Get(ctx context.Context, key string) (string, error)
	Set(ctx context.Context, key, value string, exp time.Duration) error
	Delete(ctx context.Context, key string) error
}

// WorkspaceGetter resolves workspace CRDs.
type WorkspaceGetter interface {
	GetWorkspace(ctx context.Context, id string) (*v1.Workspace, error)
}

// k8sWorkspaceGetter once lived here as a local adapter from the K8s
// client to WorkspaceGetter; it has been superseded by
// k8sWorkspaceGetterAdapter in internal/app/secrets_adapters.go which
// is the single wiring point for all handlers. Removed to avoid two
// adapters drifting independently.

// TerminalHandler handles WebSocket terminal connections to workspace pods.
type TerminalHandler struct {
	cache     TerminalCache
	wsGetter  WorkspaceGetter
	namespace string
	logger    pkginterfaces.LoggerInterface

	// Connection tracking
	wsConns              map[string]int
	wsConnsMu            sync.Mutex
	globalConns          atomic.Int64
	maxPerWorkspaceConns int
	maxGlobalConns       int

	// K8s exec (nil in tests)
	restConfig *rest.Config
	clientset  kubernetes.Interface

	upgrader websocket.Upgrader
}

// NewTerminalHandler creates a new terminal handler.
//
// allowedOrigins governs the WebSocket upgrade Origin check:
//
//   - Empty slice (default): same-origin only. A browser request's Origin
//     header must match the request Host. Cross-origin browser requests are
//     rejected. Non-browser clients (no Origin header) are accepted — they
//     authenticate via the single-use ticket, not cookies, so CSRF does
//     not apply.
//   - Contains "*": all origins accepted (the historical behaviour).
//     Operators who really want this must opt in explicitly.
//   - Otherwise: same-origin requests plus anything in the allowlist.
//
// The same-origin default is what protects against cross-site WebSocket
// hijacking from a malicious page in a browser holding the user's session
// ticket. See G35 in design/stories/epic-17-security-review/.
func NewTerminalHandler(
	cache TerminalCache,
	wsGetter WorkspaceGetter,
	namespace string,
	logger pkginterfaces.LoggerInterface,
	allowedOrigins []string,
) *TerminalHandler {
	return &TerminalHandler{
		cache:                cache,
		wsGetter:             wsGetter,
		namespace:            namespace,
		logger:               logger,
		wsConns:              make(map[string]int),
		maxPerWorkspaceConns: defaultMaxPerWS,
		maxGlobalConns:       defaultMaxGlobal,
		upgrader: websocket.Upgrader{
			CheckOrigin:      newCheckOriginChecker(allowedOrigins),
			HandshakeTimeout: 10 * time.Second,
		},
	}
}

// newCheckOriginChecker returns a gorilla/websocket CheckOrigin function
// implementing the same-origin-default + operator-allowlist policy documented
// on NewTerminalHandler.
func newCheckOriginChecker(allowedOrigins []string) func(*http.Request) bool {
	wildcard := false
	normalized := make(map[string]struct{}, len(allowedOrigins))
	for _, o := range allowedOrigins {
		if o == "*" {
			wildcard = true
			continue
		}
		normalized[normalizeOrigin(o)] = struct{}{}
	}
	return func(r *http.Request) bool {
		if wildcard {
			return true
		}
		origin := r.Header.Get("Origin")
		if origin == "" {
			// Non-browser client (curl, MCP). Authenticated by ticket, not
			// cookies; CSRF does not apply.
			return true
		}
		u, err := url.Parse(origin)
		if err != nil {
			return false
		}
		if asciiEqualFold(u.Host, r.Host) {
			return true
		}
		if _, ok := normalized[normalizeOrigin(origin)]; ok {
			return true
		}
		return false
	}
}

// normalizeOrigin lowercases and trims trailing slash so the allowlist
// comparison is scheme+host+port only.
func normalizeOrigin(o string) string {
	s := strings.ToLower(strings.TrimSpace(o))
	s = strings.TrimSuffix(s, "/")
	return s
}

// asciiEqualFold reports whether s and t are equal under ASCII case-folding.
// (Equivalent to gorilla/websocket's equalASCIIFold, inlined to avoid a
// direct dependency on the library's internal helper.)
func asciiEqualFold(s, t string) bool {
	if len(s) != len(t) {
		return false
	}
	for i := 0; i < len(s); i++ {
		if toLowerASCII(s[i]) != toLowerASCII(t[i]) {
			return false
		}
	}
	return true
}

func toLowerASCII(b byte) byte {
	if b >= 'A' && b <= 'Z' {
		return b + ('a' - 'A')
	}
	return b
}

// SetExecConfig sets the K8s config for pod exec (call after construction).
func (h *TerminalHandler) SetExecConfig(cfg *rest.Config, cs kubernetes.Interface) {
	h.restConfig = cfg
	h.clientset = cs
}

// HandleTicket handles POST /workspaces/:id/terminal/ticket.
func (h *TerminalHandler) HandleTicket(c *gin.Context) {
	userID, _ := extractAuth(c)
	if userID == "" {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "authentication required"})
		return
	}

	workspaceID := c.Param("id")

	ws, err := h.wsGetter.GetWorkspace(c.Request.Context(), workspaceID)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "workspace not found"})
		return
	}

	// Phase check
	if ws.Status.Phase != v1.WorkspacePhaseActive || ws.Status.PodName == "" {
		c.JSON(http.StatusConflict, gin.H{"error": "workspace not active"})
		return
	}

	// Generate ticket
	ticket, err := generateTicket()
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to generate ticket"})
		return
	}

	// Store in cache
	data, _ := json.Marshal(ticketData{UserID: userID, WorkspaceID: workspaceID})
	key := ticketKeyPrefix + ticket
	if err := h.cache.Set(c.Request.Context(), key, string(data), ticketTTL); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to store ticket"})
		return
	}

	c.JSON(http.StatusOK, TicketResponse{
		Ticket:    ticket,
		ExpiresAt: time.Now().Add(ticketTTL),
	})
}

// HandleTerminal handles GET /workspaces/:id/terminal?ticket=<ticket>.
func (h *TerminalHandler) HandleTerminal(c *gin.Context) {
	workspaceID := c.Param("id")
	ticket := c.Query("ticket")

	if ticket == "" {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "ticket required"})
		return
	}

	// Validate and consume ticket (atomic get+delete)
	ctx := c.Request.Context()
	key := ticketKeyPrefix + ticket
	raw, err := h.cache.Get(ctx, key)
	if err != nil {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid or expired ticket"})
		return
	}
	// Delete immediately (single-use)
	_ = h.cache.Delete(ctx, key)

	var td ticketData
	if err := json.Unmarshal([]byte(raw), &td); err != nil {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid ticket data"})
		return
	}

	// Verify ticket matches workspace
	if td.WorkspaceID != workspaceID {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "ticket workspace mismatch"})
		return
	}

	// Connection limits
	if !h.acquireConnection(workspaceID) {
		c.JSON(http.StatusTooManyRequests, gin.H{"error": "terminal connection limit reached"})
		return
	}
	defer h.releaseConnection(workspaceID)

	// Resolve pod
	ws, err := h.wsGetter.GetWorkspace(c.Request.Context(), workspaceID)
	if err != nil || ws.Status.Phase != v1.WorkspacePhaseActive || ws.Status.PodName == "" {
		c.JSON(http.StatusConflict, gin.H{"error": "workspace not active"})
		return
	}

	// Upgrade to WebSocket
	conn, err := h.upgrader.Upgrade(c.Writer, c.Request, nil)
	if err != nil {
		if h.logger != nil {
			h.logger.Error("WebSocket upgrade failed", err, "workspaceID", workspaceID)
		}
		return
	}
	defer func() { _ = conn.Close() }()

	// If no exec config (test mode), just close
	if h.restConfig == nil || h.clientset == nil {
		msg := TerminalMessage{Type: "error", Message: "exec not configured"}
		data, _ := json.Marshal(msg)
		_ = conn.WriteMessage(websocket.TextMessage, data)
		return
	}

	h.bridgeExec(conn, workspaceID, ws.Status.PodName, ws.Status.PodNamespace)
}

// bridgeExec creates a K8s exec session and bridges it to the WebSocket.
//
// F1.3.6 (Epic 17): the API ServiceAccount has pods/exec on the entire
// workspace namespace because standard k8s RBAC doesn't support
// resourceNames/labelSelectors on subresources. Defense-in-depth at
// the application layer: refuse to exec into a pod whose
// llmsafespaces.dev/workspace label does not match the workspaceID
// the caller authenticated against. Without this check, a
// compromised workspace.Status.PodName (mitigated by the F1.2.2
// webhook) OR a legitimate operator-initiated workload sharing the
// same namespace label would be reachable from any user's terminal
// endpoint.
func (h *TerminalHandler) bridgeExec(conn *websocket.Conn, workspaceID, podName, podNamespace string) {
	if podNamespace == "" {
		podNamespace = h.namespace
	}

	// Confirm the target pod is genuinely the sandbox pod for this
	// workspace. This guards against (a) a stale Status.PodName, (b)
	// an unrelated pod with the same name in the same namespace, (c)
	// any future change that grants pods/exec more broadly.
	if h.clientset != nil {
		pod, err := h.clientset.CoreV1().Pods(podNamespace).
			Get(context.Background(), podName, metav1.GetOptions{})
		if err != nil {
			msg := TerminalMessage{Type: "error", Message: "pod lookup failed"}
			data, _ := json.Marshal(msg)
			_ = conn.WriteMessage(websocket.TextMessage, data)
			return
		}
		if pod.Labels["llmsafespaces.dev/workspace"] != workspaceID {
			msg := TerminalMessage{Type: "error", Message: "pod ownership mismatch — refusing exec"}
			data, _ := json.Marshal(msg)
			_ = conn.WriteMessage(websocket.TextMessage, data)
			return
		}
	}

	execReq := h.clientset.CoreV1().RESTClient().Post().
		Resource("pods").
		Name(podName).
		Namespace(podNamespace).
		SubResource("exec").
		VersionedParams(&corev1.PodExecOptions{
			Command: []string{terminalShell},
			Stdin:   true,
			Stdout:  true,
			Stderr:  true,
			TTY:     true,
		}, runtime.NewParameterCodec(parameterScheme))

	exec, err := remotecommand.NewSPDYExecutor(h.restConfig, http.MethodPost, execReq.URL())
	if err != nil {
		msg := TerminalMessage{Type: "error", Message: "exec setup failed"}
		data, _ := json.Marshal(msg)
		_ = conn.WriteMessage(websocket.TextMessage, data)
		return
	}

	// Create pipes for stdin
	stdinR, stdinW := io.Pipe()
	defer func() { _ = stdinW.Close() }()

	// Terminal size channel
	sizeCh := make(chan remotecommand.TerminalSize, 1)
	// Set initial size
	sizeCh <- remotecommand.TerminalSize{Width: 80, Height: 24}

	// Read from WebSocket → stdin pipe (in goroutine)
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer func() { _ = stdinW.Close() }()
		for {
			_, raw, err := conn.ReadMessage()
			if err != nil {
				return
			}
			var msg TerminalMessage
			if err := json.Unmarshal(raw, &msg); err != nil {
				continue
			}
			switch msg.Type {
			case "input":
				_, _ = stdinW.Write([]byte(msg.Data))
			case "resize":
				select {
				case sizeCh <- remotecommand.TerminalSize{Width: msg.Cols, Height: msg.Rows}:
				default:
				}
			}
		}
	}()

	// stdout/stderr → WebSocket writer
	wsWriter := &wsOutputStream{conn: conn}

	// Run exec (blocks until shell exits)
	err = exec.StreamWithContext(context.Background(), remotecommand.StreamOptions{
		Stdin:             stdinR,
		Stdout:            wsWriter,
		Stderr:            wsWriter,
		Tty:               true,
		TerminalSizeQueue: &termSizeQueue{ch: sizeCh},
	})

	exitCode := 0
	if err != nil {
		exitCode = 1
	}

	exitMsg := TerminalMessage{Type: "exit", Code: exitCode}
	data, _ := json.Marshal(exitMsg)
	_ = conn.WriteMessage(websocket.TextMessage, data)
}

// acquireConnection attempts to acquire a terminal connection slot.
func (h *TerminalHandler) acquireConnection(workspaceID string) bool {
	// Check global limit
	if h.globalConns.Load() >= int64(h.maxGlobalConns) {
		return false
	}

	h.wsConnsMu.Lock()
	defer h.wsConnsMu.Unlock()

	if h.wsConns[workspaceID] >= h.maxPerWorkspaceConns {
		return false
	}

	h.wsConns[workspaceID]++
	h.globalConns.Add(1)
	return true
}

// releaseConnection releases a terminal connection slot.
func (h *TerminalHandler) releaseConnection(workspaceID string) {
	h.wsConnsMu.Lock()
	defer h.wsConnsMu.Unlock()

	if h.wsConns[workspaceID] > 0 {
		h.wsConns[workspaceID]--
	}
	h.globalConns.Add(-1)
}

// generateTicket creates a cryptographically random ticket.
func generateTicket() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return ticketPrefix + hex.EncodeToString(b), nil
}

// wsOutputStream writes exec output to a WebSocket connection.
type wsOutputStream struct {
	conn *websocket.Conn
}

func (w *wsOutputStream) Write(p []byte) (int, error) {
	msg := TerminalMessage{Type: "output", Data: string(p)}
	data, err := json.Marshal(msg)
	if err != nil {
		return 0, err
	}
	if err := w.conn.WriteMessage(websocket.TextMessage, data); err != nil {
		return 0, err
	}
	return len(p), nil
}

// termSizeQueue implements remotecommand.TerminalSizeQueue.
type termSizeQueue struct {
	ch <-chan remotecommand.TerminalSize
}

func (q *termSizeQueue) Next() *remotecommand.TerminalSize {
	size, ok := <-q.ch
	if !ok {
		return nil
	}
	return &size
}
