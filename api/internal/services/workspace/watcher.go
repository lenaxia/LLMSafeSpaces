// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package workspace

import (
	"context"
	"fmt"
	"sync"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/watch"

	"github.com/lenaxia/llmsafespaces/api/internal/services/metrics"
	v1 "github.com/lenaxia/llmsafespaces/pkg/apis/llmsafespaces/v1"
	pkginterfaces "github.com/lenaxia/llmsafespaces/pkg/interfaces"
)

type PhaseChangeCallback func(workspace *v1.Workspace)

// VersionSyncCallback is called with (workspaceID, imageTag, agentVersion)
// when the CRD's imageTag changes so the API can persist the new tag to
// its DB without a K8s Get. Optional — nil-safe on the watcher.
type VersionSyncCallback func(workspaceID, imageTag, agentVersion string)

// WorkspaceUpdateCallback is called on every Added/Modified event
// (NOT Deleted) for a Workspace, regardless of what changed. Used by
// the watcher-driven auto-push (worklog 0591): the callback filters
// on the exact state it cares about (Phase==Active +
// UserCredsPresent==false + bindings-exist) and no-ops otherwise.
//
// Distinct from PhaseChangeCallback because a pod-recreation may not
// change phase (e.g. `kubectl delete pod` with immediate controller
// respawn keeps status.Phase=Active) but does change UserCredsPresent
// (controller clears it on unreachable, then reports the new agentd's
// state on the next scrape). Filtering by phase alone misses those.
//
// Callback contract:
//   - MUST be fast + non-blocking. Slow callbacks stall the watch
//     goroutine, which delays the SSE stream and phase events for all
//     workspaces on this API replica.
//   - MUST tolerate repeat calls for the same workspace — the watcher
//     emits many Modified events per workspace lifetime, and the
//     callback's own state is the source of truth for "already
//     handled this update."
//   - MUST NOT panic. Panics propagate to the watch goroutine.
type WorkspaceUpdateCallback func(workspace *v1.Workspace)

type WorkspaceOwnerTracker interface {
	RecordWorkspaceOwner(workspaceID, userID string)
	CleanupWorkspace(workspaceID string)
}

// Watch tuning. The apiserver enforces a max watch lifetime of about 60 minutes
// (default). We pick a shorter explicit timeout so reconnects happen at
// predictable intervals; bookmarks keep us in sync with resourceVersion in the
// meantime so reconnects are O(1).
const (
	watchTimeoutSeconds    = 290
	watchBackoffInitial    = 2 * time.Second
	watchBackoffMax        = 30 * time.Second
	watchBackoffMultiplier = 2
)

type Watcher struct {
	k8sClient            pkginterfaces.KubernetesClient
	logger               pkginterfaces.LoggerInterface
	namespace            string
	onPhaseChange        PhaseChangeCallback
	onVersionSync        VersionSyncCallback     // nil-safe; set via SetVersionSyncCallback
	onWorkspaceUpdate    WorkspaceUpdateCallback // nil-safe; set via SetWorkspaceUpdateCallback
	userBroker           WorkspaceOwnerTracker
	stopCh               chan struct{}
	stopOnce             sync.Once
	knownPhases          map[string]string
	knownPhasesMu        sync.RWMutex
	knownImageTags       map[string]string // tracks last-seen imageTag per workspace
	knownImageTagsMu     sync.RWMutex
	watchRestartMu       sync.Mutex
	lastResourceVersion  string
	lastResourceVersionM sync.Mutex
}

func NewWatcher(
	k8sClient pkginterfaces.KubernetesClient,
	logger pkginterfaces.LoggerInterface,
	namespace string,
	onPhaseChange PhaseChangeCallback,
) (*Watcher, error) {
	if k8sClient == nil {
		return nil, fmt.Errorf("kubernetes client cannot be nil")
	}
	if onPhaseChange == nil {
		return nil, fmt.Errorf("phase change callback cannot be nil")
	}
	return &Watcher{
		k8sClient:      k8sClient,
		logger:         logger,
		namespace:      namespace,
		onPhaseChange:  onPhaseChange,
		stopCh:         make(chan struct{}),
		knownPhases:    make(map[string]string),
		knownImageTags: make(map[string]string),
	}, nil
}

func (w *Watcher) SetUserBroker(broker WorkspaceOwnerTracker) {
	w.userBroker = broker
}

// SetVersionSyncCallback sets the callback invoked when a workspace becomes
// Active with a non-empty imageTag. Must be called before Start(); calling
// after Start() is a data race (the watch goroutine reads onVersionSync without
// a lock). Follows the same contract as SetUserBroker.
func (w *Watcher) SetVersionSyncCallback(cb VersionSyncCallback) {
	w.onVersionSync = cb
}

// SetWorkspaceUpdateCallback installs a callback invoked on every
// Added/Modified event for any Workspace CRD. Optional; nil-safe.
// Must be called before Start() (same data-race constraint as
// SetVersionSyncCallback). See WorkspaceUpdateCallback docstring for
// the callback contract.
func (w *Watcher) SetWorkspaceUpdateCallback(cb WorkspaceUpdateCallback) {
	w.onWorkspaceUpdate = cb
}

func (w *Watcher) Start() error {
	go w.runWatchLoop()
	return nil
}

func (w *Watcher) Stop() {
	w.stopOnce.Do(func() {
		close(w.stopCh)
	})
}

func (w *Watcher) SetKnownPhase(name, phase string) {
	w.knownPhasesMu.Lock()
	defer w.knownPhasesMu.Unlock()
	w.knownPhases[name] = phase
}

func (w *Watcher) GetKnownPhase(name string) (string, bool) {
	w.knownPhasesMu.RLock()
	defer w.knownPhasesMu.RUnlock()
	phase, ok := w.knownPhases[name]
	return phase, ok
}

func (w *Watcher) GetAllKnownPhases() map[string]string {
	w.knownPhasesMu.RLock()
	defer w.knownPhasesMu.RUnlock()
	result := make(map[string]string, len(w.knownPhases))
	for k, v := range w.knownPhases {
		result[k] = v
	}
	return result
}

func (w *Watcher) runWatchLoop() {
	if err := w.seedResourceVersion(); err != nil {
		w.logger.Warn("Initial List for workspace watcher failed; will rely on Watch alone",
			"error", err.Error())
	}

	backoff := watchBackoffInitial
	for {
		select {
		case <-w.stopCh:
			return
		default:
		}

		cleanClose, err := w.watchOnce()
		if err != nil {
			w.logger.Warn("Workspace watch error; will retry",
				"error", err.Error(),
				"backoff", backoff.String())
			if !sleepCancellable(w.stopCh, backoff) {
				return
			}
			backoff = nextBackoff(backoff)
			continue
		}

		if cleanClose {
			w.logger.Debug("Workspace watch closed cleanly, reconnecting")
			backoff = watchBackoffInitial
		}
	}
}

// seedResourceVersion does an initial List to populate lastResourceVersion and
// knownPhases so the first Watch starts from a known position. Also records
// workspace ownership in the user broker for snapshot delivery (FM3, FM7).
// For workspaces already Active, onPhaseChange is invoked immediately so that
// SSE tracker subscriptions and other phase-dependent state are established
// on API restart (covers the case where the API restarts while workspaces are
// already Active — without this, the EnsureWatching loop in proxy_lifecycle
// runs against an empty knownPhases map because watcher.Start() is async).
// For workspaces already Active with a non-empty imageTag, the version sync
// callback is also invoked immediately so the DB reflects the current image tag.
func (w *Watcher) seedResourceVersion() error {
	v1Client, err := w.k8sClient.LlmsafespacesV1()
	if err != nil {
		return fmt.Errorf("initialize LLMSafespacesV1 client: %w", err)
	}
	list, err := v1Client.Workspaces(w.namespace).List(context.Background(), metav1.ListOptions{})
	if err != nil {
		return err
	}
	w.setResourceVersion(list.ResourceVersion)

	// Collect Active workspaces before releasing locks so callbacks fire
	// without holding any mutex (avoids lock-under-I/O).
	type versionEntry struct{ name, imageTag string }
	var toSync []versionEntry
	var activeWorkspaces []*v1.Workspace

	w.knownPhasesMu.Lock()
	w.knownImageTagsMu.Lock()
	for i := range list.Items {
		ws := &list.Items[i]
		phase := string(ws.Status.Phase)
		if phase != "" {
			w.knownPhases[ws.Name] = phase
		}
		if ws.Status.ImageTag != "" {
			w.knownImageTags[ws.Name] = ws.Status.ImageTag
		}
		if w.userBroker != nil && ws.Spec.Owner.UserID != "" {
			w.userBroker.RecordWorkspaceOwner(ws.Name, ws.Spec.Owner.UserID)
		}
		if ws.Status.Phase == v1.WorkspacePhaseActive {
			wsCopy := list.Items[i]
			activeWorkspaces = append(activeWorkspaces, &wsCopy)
			if ws.Status.ImageTag != "" {
				toSync = append(toSync, versionEntry{ws.Name, ws.Status.ImageTag})
			}
		}
	}
	w.knownImageTagsMu.Unlock()
	w.knownPhasesMu.Unlock()

	// Fire onPhaseChange for already-Active workspaces after releasing locks.
	// This ensures the SSE tracker starts watching them on API restart, even
	// though no phase transition event has been emitted for these workspaces.
	for _, ws := range activeWorkspaces {
		w.onPhaseChange(ws)
		// worklog 0591: also fire the workspace-update callback so
		// the API's watcher-driven auto-push can recover from an API
		// restart while a pod was in the UserCredsPresent=false state.
		// Without this, the auto-push would only fire on the next
		// Modified event after restart.
		if w.onWorkspaceUpdate != nil {
			w.onWorkspaceUpdate(ws)
		}
	}

	// Fire version sync callbacks after releasing locks to avoid holding
	// mutexes across DB I/O.
	if w.onVersionSync != nil {
		for _, e := range toSync {
			w.onVersionSync(e.name, e.imageTag, "")
		}
	}

	return nil
}

func (w *Watcher) watchOnce() (bool, error) {
	w.watchRestartMu.Lock()
	defer w.watchRestartMu.Unlock()

	timeoutSeconds := int64(watchTimeoutSeconds)
	allowBookmarks := true
	opts := metav1.ListOptions{
		ResourceVersion:     w.getResourceVersion(),
		TimeoutSeconds:      &timeoutSeconds,
		AllowWatchBookmarks: allowBookmarks,
	}

	startedAt := time.Now()
	v1Client, err := w.k8sClient.LlmsafespacesV1()
	if err != nil {
		return false, fmt.Errorf("initialize LLMSafespacesV1 client: %w", err)
	}
	watcher, err := v1Client.Workspaces(w.namespace).Watch(context.Background(), opts)
	if err != nil {
		return false, fmt.Errorf("starting workspace watch: %w", err)
	}
	defer watcher.Stop()

	eventCount := 0
	for {
		select {
		case <-w.stopCh:
			return true, nil
		case event, ok := <-watcher.ResultChan():
			if !ok {
				w.logger.Debug("Workspace watch channel closed",
					"livedFor", time.Since(startedAt).String(),
					"eventCount", eventCount,
					"resourceVersion", w.getResourceVersion())
				return true, nil
			}
			eventCount++

			if event.Type == watch.Error {
				status, _ := event.Object.(*metav1.Status)
				w.handleWatchError(status)
				if status != nil && status.Code == 410 {
					w.setResourceVersion("")
				}
				return false, fmt.Errorf("watch error event: %s", statusMessage(status))
			}

			w.handleEvent(event)
		}
	}
}

func (w *Watcher) handleEvent(event watch.Event) {
	if event.Type == watch.Bookmark {
		if obj, ok := event.Object.(*v1.Workspace); ok && obj.ResourceVersion != "" {
			w.setResourceVersion(obj.ResourceVersion)
		}
		return
	}

	workspace, ok := event.Object.(*v1.Workspace)
	if !ok {
		return
	}

	if workspace.ResourceVersion != "" {
		w.setResourceVersion(workspace.ResourceVersion)
	}

	name := workspace.Name

	if event.Type == watch.Deleted {
		w.knownPhasesMu.Lock()
		delete(w.knownPhases, name)
		w.knownPhasesMu.Unlock()
		w.knownImageTagsMu.Lock()
		delete(w.knownImageTags, name)
		w.knownImageTagsMu.Unlock()
		if w.userBroker != nil {
			w.userBroker.CleanupWorkspace(name)
		}
		return
	}

	newPhase := string(workspace.Status.Phase)
	newImageTag := workspace.Status.ImageTag

	if w.userBroker != nil && workspace.Spec.Owner.UserID != "" {
		w.userBroker.RecordWorkspaceOwner(name, workspace.Spec.Owner.UserID)
	}

	w.knownPhasesMu.Lock()
	oldPhase, existed := w.knownPhases[name]
	w.knownPhases[name] = newPhase
	w.knownPhasesMu.Unlock()

	if existed && oldPhase != newPhase {
		metrics.RecordWorkspacePhaseTransition(oldPhase, newPhase)
		w.onPhaseChange(workspace)
	}

	// Fire version sync when imageTag changes or when a workspace becomes Active
	// with a non-empty imageTag. This covers:
	//   - Creating/Resuming → Active: controller writes imageTag at the same time
	//     as the phase update, so newImageTag is set here.
	//   - Active → Active with updated imageTag: controller may update imageTag
	//     independently of phase (e.g. after an image refresh); we detect this
	//     by comparing against the last-known tag.
	// Guard: only fire when imageTag is non-empty and actually changed.
	if newImageTag != "" && w.onVersionSync != nil {
		w.knownImageTagsMu.Lock()
		oldTag := w.knownImageTags[name]
		if oldTag != newImageTag {
			w.knownImageTags[name] = newImageTag
			w.knownImageTagsMu.Unlock()
			w.onVersionSync(name, newImageTag, "")
		} else {
			w.knownImageTagsMu.Unlock()
		}
	}

	// worklog 0591: fire the workspace-update callback on every
	// Added/Modified event. The callback's own filters decide when to
	// act — see WorkspaceUpdateCallback docstring for the contract.
	// Fires AFTER phase-change and version-sync so those side effects
	// have been recorded first (the callback may read the CRD's
	// UserCredsPresent, which the controller updated on this same
	// watch event).
	if w.onWorkspaceUpdate != nil {
		w.onWorkspaceUpdate(workspace)
	}
}

func (w *Watcher) handleWatchError(status *metav1.Status) {
	if status == nil {
		w.logger.Warn("Workspace watch returned error event with nil status")
		return
	}
	w.logger.Warn("Workspace watch returned error event",
		"reason", string(status.Reason),
		"message", status.Message,
		"code", status.Code)
}

func (w *Watcher) getResourceVersion() string {
	w.lastResourceVersionM.Lock()
	defer w.lastResourceVersionM.Unlock()
	return w.lastResourceVersion
}

func (w *Watcher) setResourceVersion(rv string) {
	w.lastResourceVersionM.Lock()
	defer w.lastResourceVersionM.Unlock()
	w.lastResourceVersion = rv
}

func statusMessage(s *metav1.Status) string {
	if s == nil {
		return "<nil status>"
	}
	return s.Message
}

func nextBackoff(current time.Duration) time.Duration {
	next := current * watchBackoffMultiplier
	if next > watchBackoffMax {
		return watchBackoffMax
	}
	return next
}

func sleepCancellable(stopCh <-chan struct{}, d time.Duration) bool {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-stopCh:
		return false
	case <-timer.C:
		return true
	}
}
