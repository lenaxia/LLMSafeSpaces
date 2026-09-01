// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

// Package contractstream is the API-side on-demand proxy of a pod's ABI
// contract stream (US-69.10, design 0055 S3/D1-B): one upstream
// connection per workspace while ≥1 browser subscriber is attached,
// fanning raw StreamFrames out to every subscriber. Browsers run the
// stamped-snapshot client rule themselves — this proxy forwards frames,
// it never folds.
package contractstream

import (
	"context"
	"net/http"
	"sync"
	"time"

	"connectrpc.com/connect"

	abiv1 "github.com/lenaxia/llmsafespaces/pkg/abi/v1"
	abiconnect "github.com/lenaxia/llmsafespaces/pkg/abi/v1/abiconnect"
	agentd "github.com/lenaxia/llmsafespaces/pkg/agentd"
)

// Resync is delivered on a subscriber's channel when frames were dropped
// (slow consumer) or the upstream violated the protocol: the client must
// re-subscribe — a fresh Subscribe delivers a fresh stamped snapshot.
type Resync struct{}

// Resolve yields the pod's ABI base URL + password for a workspace
// (resume-safe re-resolution semantics — called on every (re)connect).
type Resolve func(ctx context.Context, workspaceID string) (baseURL, password string, err error)

// Manager owns one refcounted pod stream per workspace.
type Manager struct {
	mu      sync.Mutex
	streams map[string]*workspaceStream

	resolve          Resolve
	connect          func(ctx context.Context, baseURL, password string) (FrameSource, error)
	logger           Logger
	onUpstreamChange func(workspaceID string, open bool)
}

// Logger is the minimal seam (the API's LoggerInterface satisfies it).
type Logger interface {
	Warn(msg string, keysAndValues ...interface{})
}

// FrameSource is one live pod Events connection: frames until the channel
// closes or ctx is canceled (reconnect is the Manager's decision).
type FrameSource interface {
	Frames() <-chan *abiv1.StreamFrame
	Err() error
}

// NewManager builds the manager. connect is injectable for tests; nil
// uses the default connect-stream source.
func NewManager(resolve Resolve, logger Logger, connect func(ctx context.Context, baseURL, password string) (FrameSource, error)) *Manager {
	return &Manager{
		streams: map[string]*workspaceStream{},
		resolve: resolve,
		connect: connect,
		logger:  logger,
	}
}

// SetOnUpstreamChange wires the optional upstream-lifecycle hook (the
// metrics seam — a prometheus gauge in the handler layer; the package
// stays import-clean). Fires open=true when a workspace's upstream is
// created (first attach) and open=false when it is torn down (last
// detach). Must be set before serving; read under mu.
func (m *Manager) SetOnUpstreamChange(fn func(workspaceID string, open bool)) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.onUpstreamChange = fn
}

// Subscribe attaches one browser connection to the workspace's stream,
// creating the upstream on first attach. The returned channel carries
// frames and possibly one Resync (channel-closed after). The unsubscribe
// func must be called exactly once; the upstream disconnects on last
// detach (D1-B scale-to-zero).
func (m *Manager) Subscribe(ctx context.Context, workspaceID string) (<-chan any, func(), error) {
	m.mu.Lock()
	ws, ok := m.streams[workspaceID]
	created := false
	if !ok {
		ws = &workspaceStream{
			workspaceID: workspaceID,
			cancel:      make(chan struct{}),
			done:        make(chan struct{}),
			subs:        map[*subscriber]struct{}{},
		}
		m.streams[workspaceID] = ws
		created = true
	}
	// Register the subscriber BEFORE starting the upstream: the first
	// connect delivers the pod's snapshot immediately, and a subscriber
	// added after `go run` could miss it (fan-out to zero subscribers —
	// the client then wedges into violation-reconnect).
	sub := ws.add()
	hook := m.onUpstreamChange
	if created {
		//nolint:contextcheck // the upstream lifecycle is refcount-driven
		// (the cancel channel is the context — D1-B attach/detach), not
		// request-scoped; per-connection ctxs derive inside runOnce.
		go m.run(ws)
	}
	m.mu.Unlock()
	if created && hook != nil {
		hook(workspaceID, true)
	}
	unsub := func() {
		m.mu.Lock()
		ws.remove(sub)
		detached := false
		if ws.refs == 0 && m.streams[workspaceID] == ws {
			delete(m.streams, workspaceID)
			close(ws.cancel) // last detach: scale to zero (one close — the map guard is the arbiter)
			detached = true
		}
		hook := m.onUpstreamChange
		m.mu.Unlock()
		if detached && hook != nil {
			hook(workspaceID, false)
		}
	}
	return sub.ch, unsub, nil
}

// workspaceStream is the refcounted upstream + fan-out for one workspace.
type workspaceStream struct {
	workspaceID string
	cancel      chan struct{}
	done        chan struct{}

	mu   sync.Mutex
	refs int
	subs map[*subscriber]struct{}
}

type subscriber struct {
	ch chan any
}

func (w *workspaceStream) add() *subscriber {
	s := &subscriber{ch: make(chan any, 64)}
	w.mu.Lock()
	defer w.mu.Unlock()
	w.subs[s] = struct{}{}
	w.refs++
	return s
}

func (w *workspaceStream) remove(s *subscriber) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if _, ok := w.subs[s]; !ok {
		return
	}
	delete(w.subs, s)
	w.refs--
}

// fanout delivers to every subscriber under the stream lock (sends are
// non-blocking); a full buffer drops THAT subscriber — drained, marked
// with a Resync sentinel, closed — never blocking the upstream.
func (w *workspaceStream) fanout(v any) {
	w.mu.Lock()
	defer w.mu.Unlock()
	for s := range w.subs {
		select {
		case s.ch <- v:
		default:
			w.dropLocked(s)
		}
	}
}

// dropLocked evicts a slow consumer: stale buffered frames are drained so
// the Resync sentinel is the NEXT thing the client sees, then the channel
// closes (the SSE handler ends the response; the client re-subscribes and
// re-snapshots). Called with w.mu held.
func (w *workspaceStream) dropLocked(s *subscriber) {
	delete(w.subs, s)
	w.refs--
	for {
		select {
		case <-s.ch:
			continue
		default:
		}
		break
	}
	s.ch <- Resync{} // room: just drained ≥1
	close(s.ch)
}

// run owns the upstream connection lifecycle: connect → forward frames →
// on reseed/terminal error, reconnect (a fresh connection delivers a
// fresh stamped snapshot to the SAME subscribers — the protocol's own
// resync, no browser round trip). Exits when canceled (last detach).
func (m *Manager) run(w *workspaceStream) {
	defer close(w.done)
	for {
		select {
		case <-w.cancel:
			return
		default:
		}
		m.runOnce(w)
		select {
		case <-w.cancel:
			return
		default:
		}
		// brief pause before reconnect (a tight loop against a dead pod
		// would burn the API).
		pauseCh := w.cancel
		<-pauseChOrTimer(pauseCh)
	}
}

func (m *Manager) runOnce(w *workspaceStream) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		select {
		case <-w.cancel:
			cancel()
		case <-ctx.Done():
		}
	}()
	base, pw, err := m.resolve(ctx, w.workspaceID)
	if err != nil {
		if m.logger != nil {
			m.logger.Warn("contractstream: resolve failed", "workspace", w.workspaceID, "error", err)
		}
		return
	}
	src, err := m.connect(ctx, base, pw)
	if err != nil {
		if m.logger != nil {
			m.logger.Warn("contractstream: upstream connect failed", "workspace", w.workspaceID, "error", err)
		}
		return
	}
	seeded := false
	for {
		select {
		case <-w.cancel:
			return
		case <-ctx.Done():
			return
		case f, ok := <-src.Frames():
			if !ok {
				return // terminal: reconnect
			}
			if f == nil {
				continue
			}
			if !seeded && f.GetSnapshot() == nil {
				// Protocol: the first frame must be the snapshot. A
				// violation means reconnect (fresh snapshot).
				return
			}
			if f.GetSnapshot() != nil {
				seeded = true
			}
			if f.GetReseeded() != nil {
				// Mandatory re-snapshot (I3): reconnect delivers the
				// fresh snapshot to the same subscribers.
				return
			}
			w.fanout(f)
		}
	}
}

// pauseChOrTimer pauses ~1s unless canceled first.
func pauseChOrTimer(cancel chan struct{}) <-chan struct{} {
	ch := make(chan struct{})
	go func() {
		defer close(ch)
		select {
		case <-cancel:
		case <-time.After(time.Second):
		}
	}()
	return ch
}

// --- the default pod connection (generated connect client) ---------------

// connectSource adapts one Events server-stream to FrameSource: frames on
// the channel until the stream ends or ctx is canceled (connect streams
// are received on ONE goroutine — this is it).
type connectSource struct {
	frames chan *abiv1.StreamFrame
	errMu  sync.Mutex
	err    error
}

func (s *connectSource) Frames() <-chan *abiv1.StreamFrame { return s.frames }

func (s *connectSource) Err() error {
	s.errMu.Lock()
	defer s.errMu.Unlock()
	return s.err
}

// basicAuthClient injects the §D1 Basic credential on every request
// (abiclient's transport discipline).
func basicAuthClient(password string) *http.Client {
	return &http.Client{
		Transport: &basicAuthTransport{password: password, inner: http.DefaultTransport},
		Timeout:   0, // streams: no overall timeout; reads are ctx-bounded
	}
}

type basicAuthTransport struct {
	password string
	inner    http.RoundTripper
}

func (t *basicAuthTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.SetBasicAuth(agentd.AuthUsername, t.password)
	return t.inner.RoundTrip(r)
}

// ConnectStream is the default FrameSource factory: the generated connect
// client's Events op over Basic auth.
func ConnectStream(ctx context.Context, baseURL, password string) (FrameSource, error) {
	s := &connectSource{frames: make(chan *abiv1.StreamFrame, 64)}
	svc := abiconnect.NewHarnessABIServiceClient(basicAuthClient(password), baseURL)
	stream, err := svc.Events(ctx, connect.NewRequest(&abiv1.EventsRequest{}))
	if err != nil {
		return nil, err
	}
	go func() {
		defer close(s.frames)
		for {
			f, rerr := receiveFrame(ctx, stream)
			if rerr != nil {
				if ctx.Err() == nil {
					s.errMu.Lock()
					s.err = rerr
					s.errMu.Unlock()
				}
				return
			}
			if f == nil {
				return // clean server end
			}
			select {
			case s.frames <- f:
			case <-ctx.Done():
				return
			}
		}
	}()
	return s, nil
}

// receiveFrame receives one frame. A nil frame with nil error means the
// server ended the stream cleanly.
func receiveFrame(ctx context.Context, stream *connect.ServerStreamForClient[abiv1.StreamFrame]) (*abiv1.StreamFrame, error) {
	type received struct {
		frame *abiv1.StreamFrame
		err   error
	}
	ch := make(chan received, 1)
	go func() {
		if stream.Receive() {
			ch <- received{frame: stream.Msg()}
			return
		}
		ch <- received{err: stream.Err()}
	}()
	select {
	case r := <-ch:
		return r.frame, r.err
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}
