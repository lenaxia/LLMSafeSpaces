// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package handlers

// board.go — the Task Board's REST projection (design 0067 §4.3a,
// issue #1699 Lane 2): the owner's manager surface at /api/v1/me/*,
// user-level only (ruling R7 — no org routes, no scope switcher).
//
// A thin adapter in the §4.2 sense: authenticate (AuthMiddleware, mounted
// by the router), resolve the actor (JWT user, surface=rest), translate
// transport to one BoardService call, render the engine's rows verbatim.
// No business rules live here — guards, validation, idempotency, and
// audit are the service's (pkg/board). Error classes map to HTTP through
// the same renderBoardError the internal automation seam uses, so the
// parity matrix (§4.3) sees identical outcomes per class.
//
// Deliberately NOT routed (engine-seam gaps, flagged on the PR): epic
// listing (no ListEpics seam), board search, and board health — the
// board_read projections the engine does not expose yet.

import (
	"context"

	"github.com/gin-gonic/gin"

	"github.com/lenaxia/llmsafespaces/pkg/board"
)

// boardManager is the narrow seam the REST adapter needs (§4.2: a
// projection is a thin adapter). Satisfied by *board.Service; narrowed
// for handler testability.
type boardManager interface {
	ListProjects(ctx context.Context, userID string) ([]board.Project, error)
	GetProject(ctx context.Context, projectID, shortIDOrID string) (*board.Project, error)
	CreateProject(ctx context.Context, actor board.Actor, in board.ProjectInput, requestID string) (*board.Project, error)
	UpdateProject(ctx context.Context, actor board.Actor, projectID, shortIDOrID string, in board.ProjectInput, requestID string) (*board.Project, error)
	ArchiveProject(ctx context.Context, actor board.Actor, projectID, requestID string) (*board.Project, error)
	UnarchiveProject(ctx context.Context, actor board.Actor, projectID, requestID string) (*board.Project, error)

	GetEpic(ctx context.Context, epicID, shortIDOrID string) (*board.Epic, error)
	CreateEpic(ctx context.Context, actor board.Actor, projectID, shortIDOrID string, in board.EpicInput, requestID string) (*board.Epic, error)
	UpdateEpic(ctx context.Context, actor board.Actor, epicID, shortIDOrID string, in board.EpicInput, requestID string) (*board.Epic, error)
	EpicActivate(ctx context.Context, actor board.Actor, epicID, shortIDOrID, requestID string) (*board.Epic, error)
	EpicPause(ctx context.Context, actor board.Actor, epicID, shortIDOrID, requestID string) (*board.Epic, error)
	EpicArchive(ctx context.Context, actor board.Actor, epicID, shortIDOrID, requestID string) (*board.Epic, error)
	EpicUnarchive(ctx context.Context, actor board.Actor, epicID, shortIDOrID, requestID string) (*board.Epic, error)
	CancelEpic(ctx context.Context, actor board.Actor, epicID, shortIDOrID, reason, requestID string) (*board.Epic, error)

	GetTask(ctx context.Context, taskID, shortIDOrID string) (*board.Task, error)
	ListTasks(ctx context.Context, projectID string, stage string) ([]board.Task, error)
	CreateTask(ctx context.Context, actor board.Actor, epicID, shortIDOrID string, in board.TaskInput, requestID string) (*board.Task, error)
	UpdateTask(ctx context.Context, actor board.Actor, taskID, shortIDOrID string, in board.TaskInput, requestID string) (*board.Task, error)
	MoveStage(ctx context.Context, actor board.Actor, taskID, shortIDOrID, toStage, evidenceURL, requestID string) (*board.Task, error)
	TaskStart(ctx context.Context, actor board.Actor, taskID, shortIDOrID, requestID string) (*board.Claim, error)
	CancelTask(ctx context.Context, actor board.Actor, taskID, shortIDOrID, reason, requestID string) (*board.Task, error)
	AddDependency(ctx context.Context, actor board.Actor, taskID, shortIDOrID, dependsOnID, requestID string) error
	RemoveDependency(ctx context.Context, actor board.Actor, taskID, shortIDOrID, dependsOnID, requestID string) error
}

// BoardHandler renders the owner's board over REST (user-level, R7).
type BoardHandler struct {
	board boardManager
}

// NewBoardHandler constructs the REST projection adapter. The seam is
// satisfied by *board.Service (app wiring); tests install a fake.
func NewBoardHandler(svc boardManager) *BoardHandler {
	return &BoardHandler{board: svc}
}

// boardActor stamps the REST projection: the JWT user (AuthMiddleware),
// user kind, REST surface, no claim token (manager surface — the two
// named token overrides, cancel and pending_input exits, carry none by
// design; in-flight writes fail the token guard and snap back).
// Unused until the projection impl lands (the stubs below carry no
// actor); kept beside the seam so the wiring commit cannot forget it.
func (h *BoardHandler) boardActor(c *gin.Context) board.Actor { //nolint:unused // red-first stub companion
	return board.Actor{Kind: board.ActorUser, UserID: c.GetString("userID"), Surface: board.SurfaceREST}
}

// --- stubs (red-first lane discipline: the pins in board_rest_test.go
// fail against these until the projection lands) ---

func (h *BoardHandler) ListProjects(c *gin.Context)     { renderBoardError(c, board.ErrNotImplemented) }
func (h *BoardHandler) CreateProject(c *gin.Context)    { renderBoardError(c, board.ErrNotImplemented) }
func (h *BoardHandler) GetProject(c *gin.Context)       { renderBoardError(c, board.ErrNotImplemented) }
func (h *BoardHandler) UpdateProject(c *gin.Context)    { renderBoardError(c, board.ErrNotImplemented) }
func (h *BoardHandler) ArchiveProject(c *gin.Context)   { renderBoardError(c, board.ErrNotImplemented) }
func (h *BoardHandler) UnarchiveProject(c *gin.Context) { renderBoardError(c, board.ErrNotImplemented) }
func (h *BoardHandler) ListProjectTasks(c *gin.Context) { renderBoardError(c, board.ErrNotImplemented) }
func (h *BoardHandler) CreateEpic(c *gin.Context)       { renderBoardError(c, board.ErrNotImplemented) }
func (h *BoardHandler) GetEpic(c *gin.Context)          { renderBoardError(c, board.ErrNotImplemented) }
func (h *BoardHandler) UpdateEpic(c *gin.Context)       { renderBoardError(c, board.ErrNotImplemented) }
func (h *BoardHandler) ActivateEpic(c *gin.Context)     { renderBoardError(c, board.ErrNotImplemented) }
func (h *BoardHandler) PauseEpic(c *gin.Context)        { renderBoardError(c, board.ErrNotImplemented) }
func (h *BoardHandler) ArchiveEpic(c *gin.Context)      { renderBoardError(c, board.ErrNotImplemented) }
func (h *BoardHandler) UnarchiveEpic(c *gin.Context)    { renderBoardError(c, board.ErrNotImplemented) }
func (h *BoardHandler) CancelEpic(c *gin.Context)       { renderBoardError(c, board.ErrNotImplemented) }
func (h *BoardHandler) CreateEpicTask(c *gin.Context)   { renderBoardError(c, board.ErrNotImplemented) }
func (h *BoardHandler) GetTask(c *gin.Context)          { renderBoardError(c, board.ErrNotImplemented) }
func (h *BoardHandler) UpdateTask(c *gin.Context)       { renderBoardError(c, board.ErrNotImplemented) }
func (h *BoardHandler) MoveStageTask(c *gin.Context)    { renderBoardError(c, board.ErrNotImplemented) }
func (h *BoardHandler) StartTask(c *gin.Context)        { renderBoardError(c, board.ErrNotImplemented) }
func (h *BoardHandler) CancelTask(c *gin.Context)       { renderBoardError(c, board.ErrNotImplemented) }
func (h *BoardHandler) AddDependency(c *gin.Context)    { renderBoardError(c, board.ErrNotImplemented) }
func (h *BoardHandler) RemoveDependency(c *gin.Context) { renderBoardError(c, board.ErrNotImplemented) }
