// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package workspace

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8stypes "k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/watch"

	apiinterfaces "github.com/lenaxia/llmsafespaces/api/internal/interfaces"
	v1 "github.com/lenaxia/llmsafespaces/pkg/apis/llmsafespaces/v1"
	pkginterfaces "github.com/lenaxia/llmsafespaces/pkg/interfaces"
	"github.com/lenaxia/llmsafespaces/pkg/settings"
	"github.com/lenaxia/llmsafespaces/pkg/types"
)

type mockSettingsStore struct {
	data map[string]json.RawMessage
}

func (m *mockSettingsStore) GetAllInstanceSettings(_ context.Context) (map[string]json.RawMessage, error) {
	return m.data, nil
}
func (m *mockSettingsStore) SetInstanceSetting(_ context.Context, key string, value json.RawMessage) error {
	m.data[key] = value
	return nil
}

type testLogger struct{}

func (l *testLogger) Debug(msg string, keysAndValues ...interface{})            {}
func (l *testLogger) Info(msg string, keysAndValues ...interface{})             {}
func (l *testLogger) Warn(msg string, keysAndValues ...interface{})             {}
func (l *testLogger) Error(msg string, err error, keysAndValues ...interface{}) {}
func (l *testLogger) Fatal(msg string, err error, keysAndValues ...interface{}) {}
func (l *testLogger) With(keysAndValues ...interface{}) pkginterfaces.LoggerInterface {
	return l
}
func (l *testLogger) Sync() error { return nil }

func TestEnforceMaxActive_BelowCap_NoSuspension(t *testing.T) {
	store := &mockSettingsStore{data: make(map[string]json.RawMessage)}
	raw, _ := json.Marshal(3)
	store.data["workspace.maxActiveWorkspacesPerUser"] = raw

	instanceSvc := settings.NewInstanceService(store, nil)

	svc := &Service{
		logger:           &testLogger{},
		instanceSettings: instanceSvc,
		dbService: &mockDBForMaxActive{workspaces: []*types.WorkspaceMetadata{
			{ID: "ws-1", UpdatedAt: time.Now().Add(-1 * time.Hour)},
			{ID: "ws-2", UpdatedAt: time.Now()},
		}},
		k8sClient: &mockK8sForMaxActive{
			workspaces: map[string]*v1.Workspace{
				"ws-1": {ObjectMeta: metav1.ObjectMeta{Name: "ws-1"}, Status: v1.WorkspaceStatus{Phase: v1.WorkspacePhaseActive}},
				"ws-2": {ObjectMeta: metav1.ObjectMeta{Name: "ws-2"}, Status: v1.WorkspaceStatus{Phase: v1.WorkspacePhaseActive}},
			},
		},
		config: &Config{Namespace: "default"},
	}

	suspended, err := svc.enforceMaxActiveWorkspaces(context.Background(), "user-1", "ws-target")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if suspended != "" {
		t.Errorf("expected no suspension (below cap), got %q", suspended)
	}
}

func TestEnforceMaxActive_AtCap_SuspendsStalest(t *testing.T) {
	store := &mockSettingsStore{data: make(map[string]json.RawMessage)}
	raw, _ := json.Marshal(2)
	store.data["workspace.maxActiveWorkspacesPerUser"] = raw

	instanceSvc := settings.NewInstanceService(store, nil)

	now := time.Now()
	db := &mockDBForMaxActive{workspaces: []*types.WorkspaceMetadata{
		{ID: "ws-old", UserID: "user-1", UpdatedAt: now.Add(-2 * time.Hour)},
		{ID: "ws-new", UserID: "user-1", UpdatedAt: now},
	}}

	k8sMock := &mockK8sForMaxActive{
		workspaces: map[string]*v1.Workspace{
			"ws-old": {
				ObjectMeta: metav1.ObjectMeta{Name: "ws-old"},
				Status:     v1.WorkspaceStatus{Phase: v1.WorkspacePhaseActive},
			},
			"ws-new": {
				ObjectMeta: metav1.ObjectMeta{Name: "ws-new"},
				Status:     v1.WorkspaceStatus{Phase: v1.WorkspacePhaseActive},
			},
		},
	}

	svc := &Service{
		logger:           &testLogger{},
		instanceSettings: instanceSvc,
		dbService:        db,
		k8sClient:        k8sMock,
		config:           &Config{Namespace: "default"},
	}

	suspended, err := svc.enforceMaxActiveWorkspaces(context.Background(), "user-1", "ws-target")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if suspended != "ws-old" {
		t.Errorf("expected ws-old to be suspended (stalest), got %q", suspended)
	}
}

func TestEnforceMaxActive_NilSettings_NoEnforcement(t *testing.T) {
	svc := &Service{
		logger:           &testLogger{},
		instanceSettings: nil, // not configured
	}

	suspended, err := svc.enforceMaxActiveWorkspaces(context.Background(), "user-1", "ws-target")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if suspended != "" {
		t.Errorf("expected no enforcement when settings nil, got %q", suspended)
	}
}

func TestEnforceMaxActive_ExcludesTarget(t *testing.T) {
	store := &mockSettingsStore{data: make(map[string]json.RawMessage)}
	raw, _ := json.Marshal(1)
	store.data["workspace.maxActiveWorkspacesPerUser"] = raw

	instanceSvc := settings.NewInstanceService(store, nil)

	now := time.Now()
	db := &mockDBForMaxActive{workspaces: []*types.WorkspaceMetadata{
		// The target workspace is Running — should be excluded from count
		{ID: "ws-target", UpdatedAt: now},
	}}

	svc := &Service{
		logger:           &testLogger{},
		instanceSettings: instanceSvc,
		dbService:        db,
		k8sClient: &mockK8sForMaxActive{
			workspaces: map[string]*v1.Workspace{
				"ws-target": {ObjectMeta: metav1.ObjectMeta{Name: "ws-target"}, Status: v1.WorkspaceStatus{Phase: v1.WorkspacePhaseActive}},
			},
		},
		config: &Config{Namespace: "default"},
	}

	suspended, err := svc.enforceMaxActiveWorkspaces(context.Background(), "user-1", "ws-target")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if suspended != "" {
		t.Errorf("expected no suspension (target excluded from count), got %q", suspended)
	}
}

func TestEnforceMaxActive_SuspendedWorkspacesNotCounted(t *testing.T) {
	store := &mockSettingsStore{data: make(map[string]json.RawMessage)}
	raw, _ := json.Marshal(2)
	store.data["workspace.maxActiveWorkspacesPerUser"] = raw

	instanceSvc := settings.NewInstanceService(store, nil)

	now := time.Now()
	db := &mockDBForMaxActive{workspaces: []*types.WorkspaceMetadata{
		{ID: "ws-1", UpdatedAt: now},
		{ID: "ws-2", UpdatedAt: now.Add(-1 * time.Hour)},
		{ID: "ws-3", UpdatedAt: now.Add(-2 * time.Hour)},
	}}

	svc := &Service{
		logger:           &testLogger{},
		instanceSettings: instanceSvc,
		dbService:        db,
		k8sClient: &mockK8sForMaxActive{
			workspaces: map[string]*v1.Workspace{
				"ws-1": {ObjectMeta: metav1.ObjectMeta{Name: "ws-1"}, Status: v1.WorkspaceStatus{Phase: v1.WorkspacePhaseActive}},
				"ws-2": {ObjectMeta: metav1.ObjectMeta{Name: "ws-2"}, Status: v1.WorkspaceStatus{Phase: v1.WorkspacePhaseSuspended}},
				"ws-3": {ObjectMeta: metav1.ObjectMeta{Name: "ws-3"}, Status: v1.WorkspaceStatus{Phase: v1.WorkspacePhaseTerminated}},
			},
		},
		config: &Config{Namespace: "default"},
	}

	suspended, err := svc.enforceMaxActiveWorkspaces(context.Background(), "user-1", "ws-target")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// Only ws-1 is Running (ws-2 is Suspended, ws-3 is Terminated) — below cap of 2
	if suspended != "" {
		t.Errorf("expected no suspension (only 1 active), got %q", suspended)
	}
}

func TestEnforceMaxActive_StaleDBPhase_SkipsAlreadySuspended(t *testing.T) {
	store := &mockSettingsStore{data: make(map[string]json.RawMessage)}
	raw, _ := json.Marshal(2)
	store.data["workspace.maxActiveWorkspacesPerUser"] = raw

	instanceSvc := settings.NewInstanceService(store, nil)

	now := time.Now()
	db := &mockDBForMaxActive{workspaces: []*types.WorkspaceMetadata{
		{ID: "ws-stale", UserID: "user-1", UpdatedAt: now.Add(-2 * time.Hour)},
		{ID: "ws-real", UserID: "user-1", UpdatedAt: now},
	}}

	k8sMock := &mockK8sForMaxActive{
		workspaces: map[string]*v1.Workspace{
			"ws-stale": {
				ObjectMeta: metav1.ObjectMeta{Name: "ws-stale"},
				Status:     v1.WorkspaceStatus{Phase: v1.WorkspacePhaseSuspended},
			},
			"ws-real": {
				ObjectMeta: metav1.ObjectMeta{Name: "ws-real"},
				Status:     v1.WorkspaceStatus{Phase: v1.WorkspacePhaseActive},
			},
		},
	}

	svc := &Service{
		logger:           &testLogger{},
		instanceSettings: instanceSvc,
		dbService:        db,
		k8sClient:        k8sMock,
		config:           &Config{Namespace: "default"},
	}

	suspended, err := svc.enforceMaxActiveWorkspaces(context.Background(), "user-1", "ws-target")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if suspended != "" {
		t.Errorf("expected no suspension (ws-stale is actually Suspended, so only 1 active < cap 2), got %q", suspended)
	}
}

func TestEnforceMaxActive_StaleDBPhase_AllStale_NoSuspension(t *testing.T) {
	store := &mockSettingsStore{data: make(map[string]json.RawMessage)}
	raw, _ := json.Marshal(1)
	store.data["workspace.maxActiveWorkspacesPerUser"] = raw

	instanceSvc := settings.NewInstanceService(store, nil)

	now := time.Now()
	db := &mockDBForMaxActive{workspaces: []*types.WorkspaceMetadata{
		{ID: "ws-stale-1", UserID: "user-1", UpdatedAt: now.Add(-2 * time.Hour)},
		{ID: "ws-stale-2", UserID: "user-1", UpdatedAt: now.Add(-1 * time.Hour)},
	}}

	k8sMock := &mockK8sForMaxActive{
		workspaces: map[string]*v1.Workspace{
			"ws-stale-1": {
				ObjectMeta: metav1.ObjectMeta{Name: "ws-stale-1"},
				Status:     v1.WorkspaceStatus{Phase: v1.WorkspacePhaseSuspended},
			},
			"ws-stale-2": {
				ObjectMeta: metav1.ObjectMeta{Name: "ws-stale-2"},
				Status:     v1.WorkspaceStatus{Phase: v1.WorkspacePhaseSuspended},
			},
		},
	}

	svc := &Service{
		logger:           &testLogger{},
		instanceSettings: instanceSvc,
		dbService:        db,
		k8sClient:        k8sMock,
		config:           &Config{Namespace: "default"},
	}

	suspended, err := svc.enforceMaxActiveWorkspaces(context.Background(), "user-1", "ws-target")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if suspended != "" {
		t.Errorf("expected no suspension (all stale, 0 truly active < cap 1), got %q", suspended)
	}
}

func TestEnforceMaxActive_StaleDBPhase_Mixed_SuspendsCorrectStalest(t *testing.T) {
	store := &mockSettingsStore{data: make(map[string]json.RawMessage)}
	raw, _ := json.Marshal(2)
	store.data["workspace.maxActiveWorkspacesPerUser"] = raw

	instanceSvc := settings.NewInstanceService(store, nil)

	now := time.Now()
	// DB rows no longer carry phase (column dropped in migration 9). The
	// function must rely entirely on the CRD list to determine which
	// workspaces are actually active.
	db := &mockDBForMaxActive{workspaces: []*types.WorkspaceMetadata{
		{ID: "ws-stale", UserID: "user-1", UpdatedAt: now.Add(-3 * time.Hour)},
		{ID: "ws-real-1", UserID: "user-1", UpdatedAt: now.Add(-1 * time.Hour)},
		{ID: "ws-real-2", UserID: "user-1", UpdatedAt: now},
	}}

	k8sMock := &mockK8sForMaxActive{
		workspaces: map[string]*v1.Workspace{
			"ws-stale": {
				ObjectMeta: metav1.ObjectMeta{Name: "ws-stale"},
				Status:     v1.WorkspaceStatus{Phase: v1.WorkspacePhaseSuspended},
			},
			"ws-real-1": {
				ObjectMeta: metav1.ObjectMeta{Name: "ws-real-1"},
				Status:     v1.WorkspaceStatus{Phase: v1.WorkspacePhaseActive},
			},
			"ws-real-2": {
				ObjectMeta: metav1.ObjectMeta{Name: "ws-real-2"},
				Status:     v1.WorkspaceStatus{Phase: v1.WorkspacePhaseActive},
			},
		},
	}

	svc := &Service{
		logger:           &testLogger{},
		instanceSettings: instanceSvc,
		dbService:        db,
		k8sClient:        k8sMock,
		config:           &Config{Namespace: "default"},
	}

	suspended, err := svc.enforceMaxActiveWorkspaces(context.Background(), "user-1", "ws-target")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if suspended != "ws-real-1" {
		t.Errorf("expected ws-real-1 (stalest truly active) to be suspended, got %q", suspended)
	}
}

// TestEnforceMaxActive_AllAtCapAreCreating_ReturnsConflict pins the
// production regression observed at safespace.thekao.cloud on 2026-06-01:
// when every workspace consuming a capacity slot is in a non-suspendable
// transitional phase (Creating/Resuming), the previous implementation
// picked the stalest of those and called SuspendWorkspace, which rejects
// non-Active phases with a NewConflictError. The error then surfaced as
// a generic 500 ("failed to suspend stalest workspace ...: conflict ...
// cannot suspend workspace in phase \"Creating\"") on every activate
// attempt, with no actionable signal for the user.
//
// The fix returns a 409 directly from enforcement so the user gets a
// clear "wait for in-flight workspaces to settle or delete a Creating
// one" message instead of an internal server error.
func TestEnforceMaxActive_AllAtCapAreCreating_ReturnsConflict(t *testing.T) {
	store := &mockSettingsStore{data: make(map[string]json.RawMessage)}
	raw, _ := json.Marshal(2)
	store.data["workspace.maxActiveWorkspacesPerUser"] = raw

	instanceSvc := settings.NewInstanceService(store, nil)

	now := time.Now()
	db := &mockDBForMaxActive{workspaces: []*types.WorkspaceMetadata{
		{ID: "ws-creating-1", UserID: "user-1", UpdatedAt: now.Add(-2 * time.Hour)},
		{ID: "ws-creating-2", UserID: "user-1", UpdatedAt: now.Add(-1 * time.Hour)},
	}}

	k8sMock := &mockK8sForMaxActive{
		workspaces: map[string]*v1.Workspace{
			"ws-creating-1": {
				ObjectMeta: metav1.ObjectMeta{Name: "ws-creating-1"},
				Status:     v1.WorkspaceStatus{Phase: v1.WorkspacePhaseCreating},
			},
			"ws-creating-2": {
				ObjectMeta: metav1.ObjectMeta{Name: "ws-creating-2"},
				Status:     v1.WorkspaceStatus{Phase: v1.WorkspacePhaseCreating},
			},
		},
	}

	svc := &Service{
		logger:           &testLogger{},
		instanceSettings: instanceSvc,
		dbService:        db,
		k8sClient:        k8sMock,
		config:           &Config{Namespace: "default"},
	}

	suspended, err := svc.enforceMaxActiveWorkspaces(context.Background(), "user-1", "ws-target")
	if err == nil {
		t.Fatalf("expected ConflictError when at cap with no Active workspace to evict, got nil (suspended=%q)", suspended)
	}
	if suspended != "" {
		t.Errorf("no workspace should be reported as suspended on conflict, got %q", suspended)
	}
	// Error message must mention max active so the user knows what's
	// happening (vs. a generic 500).
	if !strings.Contains(err.Error(), "max active") {
		t.Errorf("expected error to mention 'max active', got %q", err.Error())
	}
}

// TestEnforceMaxActive_AtCapMixedActiveAndCreating_SuspendsActive pins
// the partial-capacity case: when some workspaces at the cap are Active
// and others are Creating, eviction must pick from the Active subset
// (only Active is suspendable) even if a Creating workspace is staler.
func TestEnforceMaxActive_AtCapMixedActiveAndCreating_SuspendsActive(t *testing.T) {
	store := &mockSettingsStore{data: make(map[string]json.RawMessage)}
	raw, _ := json.Marshal(2)
	store.data["workspace.maxActiveWorkspacesPerUser"] = raw

	instanceSvc := settings.NewInstanceService(store, nil)

	now := time.Now()
	db := &mockDBForMaxActive{workspaces: []*types.WorkspaceMetadata{
		// ws-creating is staler than ws-active, but it's not suspendable
		// so eviction must pick ws-active.
		{ID: "ws-creating", UserID: "user-1", UpdatedAt: now.Add(-3 * time.Hour)},
		{ID: "ws-active", UserID: "user-1", UpdatedAt: now.Add(-1 * time.Hour)},
	}}

	k8sMock := &mockK8sForMaxActive{
		workspaces: map[string]*v1.Workspace{
			"ws-creating": {
				ObjectMeta: metav1.ObjectMeta{Name: "ws-creating"},
				Status:     v1.WorkspaceStatus{Phase: v1.WorkspacePhaseCreating},
			},
			"ws-active": {
				ObjectMeta: metav1.ObjectMeta{Name: "ws-active"},
				Status:     v1.WorkspaceStatus{Phase: v1.WorkspacePhaseActive},
			},
		},
	}

	svc := &Service{
		logger:           &testLogger{},
		instanceSettings: instanceSvc,
		dbService:        db,
		k8sClient:        k8sMock,
		config:           &Config{Namespace: "default"},
	}

	suspended, err := svc.enforceMaxActiveWorkspaces(context.Background(), "user-1", "ws-target")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if suspended != "ws-active" {
		t.Errorf("expected ws-active to be suspended (only suspendable phase), got %q", suspended)
	}
}

// mockDBForMaxActive implements the subset of DatabaseService needed for enforcement tests.
type mockDBForMaxActive struct {
	apiinterfaces.DatabaseService // embed to satisfy interface
	workspaces                    []*types.WorkspaceMetadata
}

func (m *mockDBForMaxActive) ListWorkspaces(_ context.Context, _ string, _, _ int) ([]*types.WorkspaceMetadata, *types.PaginationMetadata, error) {
	return m.workspaces, nil, nil
}

func (m *mockDBForMaxActive) GetWorkspace(_ context.Context, workspaceID string) (*types.WorkspaceMetadata, error) {
	for _, ws := range m.workspaces {
		if ws.ID == workspaceID {
			return ws, nil
		}
	}
	return nil, fmt.Errorf("not found")
}

// mockK8sForMaxActive satisfies the k8s client interface for suspend calls.
type mockK8sForMaxActive struct {
	pkginterfaces.KubernetesClient
	workspaces map[string]*v1.Workspace
}

func (m *mockK8sForMaxActive) LlmsafespacesV1() (pkginterfaces.LLMSafespacesV1Interface, error) {
	return &mockLLMV1ForMaxActive{workspaces: m.workspaces}, nil
}

type mockLLMV1ForMaxActive struct {
	pkginterfaces.LLMSafespacesV1Interface
	workspaces map[string]*v1.Workspace
}

func (m *mockLLMV1ForMaxActive) Workspaces(_ string) pkginterfaces.WorkspaceInterface {
	return &mockWSInterfaceForMaxActive{workspaces: m.workspaces}
}

type mockWSInterfaceForMaxActive struct {
	pkginterfaces.WorkspaceInterface
	workspaces map[string]*v1.Workspace
}

func (m *mockWSInterfaceForMaxActive) Get(ctx context.Context, name string, _ metav1.GetOptions) (*v1.Workspace, error) {
	if ws, ok := m.workspaces[name]; ok {
		return ws, nil
	}
	return nil, fmt.Errorf("not found")
}

func (m *mockWSInterfaceForMaxActive) List(ctx context.Context, _ metav1.ListOptions) (*v1.WorkspaceList, error) {
	out := &v1.WorkspaceList{}
	for _, ws := range m.workspaces {
		out.Items = append(out.Items, *ws)
	}
	return out, nil
}

func (m *mockWSInterfaceForMaxActive) UpdateStatus(ctx context.Context, ws *v1.Workspace) (*v1.Workspace, error) {
	m.workspaces[ws.Name] = ws
	return ws, nil
}

func (m *mockWSInterfaceForMaxActive) Create(ctx context.Context, ws *v1.Workspace) (*v1.Workspace, error) {
	m.workspaces[ws.Name] = ws
	return ws, nil
}

func (m *mockWSInterfaceForMaxActive) Update(ctx context.Context, ws *v1.Workspace) (*v1.Workspace, error) {
	m.workspaces[ws.Name] = ws
	return ws, nil
}

func (m *mockWSInterfaceForMaxActive) Delete(ctx context.Context, name string, _ metav1.DeleteOptions) error {
	delete(m.workspaces, name)
	return nil
}

func (m *mockWSInterfaceForMaxActive) Watch(ctx context.Context, _ metav1.ListOptions) (watch.Interface, error) {
	return nil, fmt.Errorf("not implemented")
}

func (m *mockWSInterfaceForMaxActive) Patch(ctx context.Context, name string, _ k8stypes.PatchType, data []byte, _ metav1.PatchOptions) (*v1.Workspace, error) {
	return m.workspaces[name], nil
}

// TestEnforceMaxActive_EvictsByLastActivityAt_NotUpdatedAt verifies that
// eviction sorts by the CRD LastActivityAt annotation (user activity), not
// the DB UpdatedAt column (bumped by any row mutation). A workspace that
// was recently active (fresh annotation) but has an old UpdatedAt must NOT
// be evicted over one with a fresh UpdatedAt but stale activity.
func TestEnforceMaxActive_EvictsByLastActivityAt_NotUpdatedAt(t *testing.T) {
	store := &mockSettingsStore{data: make(map[string]json.RawMessage)}
	raw, _ := json.Marshal(2)
	store.data["workspace.maxActiveWorkspacesPerUser"] = raw

	instanceSvc := settings.NewInstanceService(store, nil)

	now := time.Now()

	// ws-active-user: DB UpdatedAt is OLD, but the user was recently active
	// (fresh LastActivityAt annotation). This workspace must NOT be evicted.
	// ws-idle-user: DB UpdatedAt is FRESH (some background op bumped the row),
	// but user hasn't touched it in hours. This one SHOULD be evicted.
	db := &mockDBForMaxActive{workspaces: []*types.WorkspaceMetadata{
		{ID: "ws-active-user", UserID: "user-1", UpdatedAt: now.Add(-3 * time.Hour)},
		{ID: "ws-idle-user", UserID: "user-1", UpdatedAt: now.Add(-5 * time.Minute)},
	}}

	k8sMock := &mockK8sForMaxActive{
		workspaces: map[string]*v1.Workspace{
			"ws-active-user": {
				ObjectMeta: metav1.ObjectMeta{
					Name: "ws-active-user",
					Annotations: map[string]string{
						v1.AnnotationLastActivityAt: now.Add(-1 * time.Minute).Format(time.RFC3339),
					},
				},
				Status: v1.WorkspaceStatus{Phase: v1.WorkspacePhaseActive},
			},
			"ws-idle-user": {
				ObjectMeta: metav1.ObjectMeta{
					Name: "ws-idle-user",
					Annotations: map[string]string{
						v1.AnnotationLastActivityAt: now.Add(-4 * time.Hour).Format(time.RFC3339),
					},
				},
				Status: v1.WorkspaceStatus{Phase: v1.WorkspacePhaseActive},
			},
		},
	}

	svc := &Service{
		logger:           &testLogger{},
		instanceSettings: instanceSvc,
		dbService:        db,
		k8sClient:        k8sMock,
		config:           &Config{Namespace: "default"},
	}

	suspended, err := svc.enforceMaxActiveWorkspaces(context.Background(), "user-1", "ws-target")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if suspended != "ws-idle-user" {
		t.Errorf("expected ws-idle-user (stalest activity) to be suspended, got %q", suspended)
	}
}

func TestParseStorageSize(t *testing.T) {
	tests := []struct {
		input string
		want  int64
	}{
		{"1Gi", 1 * 1024 * 1024 * 1024},
		{"10Gi", 10 * 1024 * 1024 * 1024},
		{"512Mi", 512 * 1024 * 1024},
		{"1Mi", 1 * 1024 * 1024},
		{"", 0},
		{"x", 0},
		{"GB", 0},
		{"5TB", 0},
	}
	for _, tt := range tests {
		got := parseStorageSize(tt.input)
		if got != tt.want {
			t.Errorf("parseStorageSize(%q) = %d, want %d", tt.input, got, tt.want)
		}
	}
}
