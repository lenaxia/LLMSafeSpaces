//go:build integration

package board_test

import (
	"context"
	"fmt"

	"github.com/lenaxia/llmsafespaces/pkg/board"
)

func (s *BoardSuite) TestDbgBusy() {
	ws := s.seedWorkspace()
	p := s.seedProject(ws, 4)
	e := s.seedEpic(p, board.EpicActive)
	task := s.seedTask(e, "dbg", nil, false)
	src := &fakeBusySource{}
	svc := s.busySvc(src)
	c, err := svc.ClaimNext(context.Background(), "worker-"+ws, ws)
	s.Require().NoError(err)
	fmt.Printf("DBG claimed short=%s want=%s stage=%s\n", c.Task.ShortID, task.ShortID, c.Task.Stage)
	rows, err := svc.ListTasks(context.Background(), p.ID, "", "")
	s.Require().NoError(err)
	for _, r := range rows {
		fmt.Printf("DBG row short=%s stage=%s assignee=%q lease=%v\n", r.ShortID, r.Stage, r.AssigneeSession, r.LeaseExpiresAt)
	}
	rt := s.reloadTask(task.ID)
	fmt.Printf("DBG reload short=%s stage=%s assignee=%q\n", rt.ShortID, rt.Stage, rt.AssigneeSession)
}
