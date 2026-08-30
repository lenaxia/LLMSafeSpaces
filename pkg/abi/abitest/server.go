// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

// Package abitest is the reference in-memory implementation of the harness
// ABI. It exists so the generated contract tests run against a real connect
// server over real HTTP (issue #1135: "generated contract tests run in CI
// against a reference in-memory implementation") and to seed the shape of the
// production sessionstate implementation (US-69.2). It is test scaffolding:
// production code must not import it.
package abitest

import (
	"context"
	"net/http"
	"sync"

	"connectrpc.com/connect"
	abiv1 "github.com/lenaxia/llmsafespaces/pkg/abi/v1"
	abiconnect "github.com/lenaxia/llmsafespaces/pkg/abi/v1/abiconnect"
)

// Server is an in-memory HarnessABI service. Its capability report declares
// text delivery parts and every action except compact — the same capability
// split the opencode adapter will ship with (D3: file parts are
// NotSupported until harness support lands).
type Server struct {
	mu         sync.Mutex
	deliveries map[string]*abiv1.DeliveryStatus
	handler    http.Handler
}

func New() *Server {
	s := &Server{deliveries: map[string]*abiv1.DeliveryStatus{}}
	path, handler := abiconnect.NewHarnessABIServiceHandler(s)
	mux := http.NewServeMux()
	mux.Handle(path, handler)
	s.handler = mux
	return s
}

// Handler returns the HTTP handler serving the ABI at the connect protocol
// path.
func (s *Server) Handler() http.Handler { return s.handler }

func (s *Server) Capabilities() *abiv1.CapabilityReport {
	return &abiv1.CapabilityReport{
		Provenance:     abiv1.Provenance_PROVENANCE_PLATFORM_PINNED,
		Harness:        "opencode",
		HarnessVersion: "1.18.15",
		AgentdVersion:  "test",
		SupportedActions: []abiv1.ActionType{
			abiv1.ActionType_ACTION_TYPE_INTERRUPT,
			abiv1.ActionType_ACTION_TYPE_SWITCH_MODEL,
			abiv1.ActionType_ACTION_TYPE_SWITCH_AGENT,
			abiv1.ActionType_ACTION_TYPE_ANSWER_QUESTION,
		},
		SupportedDeliveryParts: []abiv1.DeliveryPartKind{
			abiv1.DeliveryPartKind_DELIVERY_PART_KIND_TEXT,
		},
		AbiVersion: "1",
		V2Delivery: true,
	}
}

func (s *Server) Events(ctx context.Context, req *connect.Request[abiv1.EventsRequest], stream *connect.ServerStream[abiv1.StreamFrame]) error {
	frame := &abiv1.SnapshotFrame{
		AtSeq:        0,
		Capabilities: s.Capabilities(),
		Snapshot: &abiv1.PodSnapshot{
			Sessions: []*abiv1.SessionSnapshot{referenceSessionSnapshot("sess-ref")},
		},
	}
	if err := stream.Send(&abiv1.StreamFrame{Frame: &abiv1.StreamFrame_Snapshot{Snapshot: frame}}); err != nil {
		return err
	}
	event := &abiv1.Event{Type: abiv1.EventType_EVENT_TYPE_SESSION_STATUS, SessionId: "sess-ref", Status: abiv1.SessionStatus_SESSION_STATUS_IDLE}
	if err := stream.Send(&abiv1.StreamFrame{Frame: &abiv1.StreamFrame_Event{Event: &abiv1.SequencedEvent{Seq: 1, Event: event}}}); err != nil {
		return err
	}
	return stream.Send(&abiv1.StreamFrame{Frame: &abiv1.StreamFrame_Reseeded{Reseeded: &abiv1.ReseedNotice{Seq: 2, Reason: abiv1.ReseedReason_RESEED_REASON_BOOT}}})
}

func (s *Server) GetSnapshot(ctx context.Context, req *connect.Request[abiv1.GetSnapshotRequest]) (*connect.Response[abiv1.SessionSnapshot], error) {
	if req.Msg.GetSessionId() == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errString("session_id is required"))
	}
	return connect.NewResponse(referenceSessionSnapshot(req.Msg.GetSessionId())), nil
}

func (s *Server) Deliver(ctx context.Context, req *connect.Request[abiv1.DeliveryRequest]) (*connect.Response[abiv1.DeliveryAck], error) {
	m := req.Msg
	switch {
	case m.GetSessionId() == "":
		return nil, connect.NewError(connect.CodeInvalidArgument, errString("session_id is required"))
	case m.GetEntryId() == "":
		return nil, connect.NewError(connect.CodeInvalidArgument, errString("entry_id is required"))
	case m.GetAttempt() == 0:
		return nil, connect.NewError(connect.CodeInvalidArgument, errString("attempt must be >= 1"))
	case len(m.GetParts()) == 0:
		return nil, connect.NewError(connect.CodeInvalidArgument, errString("at least one delivery part is required"))
	}
	for _, p := range m.GetParts() {
		if f := p.GetFile(); f != nil {
			err := connect.NewError(connect.CodeUnimplemented, errString("file delivery parts are not supported by this harness"))
			detail, derr := connect.NewErrorDetail(&abiv1.NotSupported{Capability: "delivery.file_parts", Detail: "path " + f.GetPath()})
			if derr != nil {
				return nil, connect.NewError(connect.CodeInternal, derr)
			}
			err.AddDetail(detail)
			return nil, err
		}
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	key := m.GetEntryId() + "/" + itoa(m.GetAttempt())
	if prev, ok := s.deliveries[key]; ok {
		return connect.NewResponse(&abiv1.DeliveryAck{EntryId: prev.GetEntryId(), Attempt: prev.GetAttempt(), State: prev.GetState()}), nil
	}
	s.deliveries[key] = &abiv1.DeliveryStatus{
		EntryId: m.GetEntryId(),
		Attempt: m.GetAttempt(),
		State:   abiv1.LedgerState_LEDGER_STATE_LEDGERED,
	}
	return connect.NewResponse(&abiv1.DeliveryAck{EntryId: m.GetEntryId(), Attempt: m.GetAttempt(), State: abiv1.LedgerState_LEDGER_STATE_LEDGERED}), nil
}

func (s *Server) GetDeliveryStatus(ctx context.Context, req *connect.Request[abiv1.GetDeliveryStatusRequest]) (*connect.Response[abiv1.DeliveryStatus], error) {
	if req.Msg.GetEntryId() == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errString("entry_id is required"))
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	key := req.Msg.GetEntryId() + "/" + itoa(req.Msg.GetAttempt())
	st, ok := s.deliveries[key]
	if !ok {
		return nil, connect.NewError(connect.CodeNotFound, errString("no ledger entry for this id/attempt"))
	}
	return connect.NewResponse(st), nil
}

func (s *Server) Act(ctx context.Context, req *connect.Request[abiv1.ActionRequest]) (*connect.Response[abiv1.ActionResult], error) {
	if req.Msg.GetSessionId() == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errString("session_id is required"))
	}
	switch a := req.Msg.GetAction().(type) {
	case *abiv1.ActionRequest_Interrupt:
		return connect.NewResponse(&abiv1.ActionResult{
			SessionId: req.Msg.GetSessionId(),
			Result:    &abiv1.ActionResult_Interrupt{Interrupt: &abiv1.InterruptResult{}},
		}), nil
	case *abiv1.ActionRequest_SwitchModel:
		if a.SwitchModel.GetModel() == nil || a.SwitchModel.GetModel().GetId() == "" {
			return nil, connect.NewError(connect.CodeInvalidArgument, errString("switch_model requires a model"))
		}
		return connect.NewResponse(&abiv1.ActionResult{
			SessionId: req.Msg.GetSessionId(),
			Result: &abiv1.ActionResult_SwitchModel{SwitchModel: &abiv1.SwitchModelResult{
				Model: a.SwitchModel.GetModel(),
			}},
		}), nil
	case *abiv1.ActionRequest_SwitchAgent:
		if a.SwitchAgent.GetAgentId() == "" {
			return nil, connect.NewError(connect.CodeInvalidArgument, errString("switch_agent requires agent_id"))
		}
		return connect.NewResponse(&abiv1.ActionResult{
			SessionId: req.Msg.GetSessionId(),
			Result: &abiv1.ActionResult_SwitchAgent{SwitchAgent: &abiv1.SwitchAgentResult{
				AgentId: a.SwitchAgent.GetAgentId(),
			}},
		}), nil
	case *abiv1.ActionRequest_AnswerQuestion:
		if a.AnswerQuestion.GetInputId() == "" {
			return nil, connect.NewError(connect.CodeInvalidArgument, errString("answer_question requires input_id"))
		}
		return connect.NewResponse(&abiv1.ActionResult{
			SessionId: req.Msg.GetSessionId(),
			Result: &abiv1.ActionResult_AnswerQuestion{AnswerQuestion: &abiv1.AnswerInputResult{
				InputId: a.AnswerQuestion.GetInputId(),
			}},
		}), nil
	case *abiv1.ActionRequest_Compact:
		err := connect.NewError(connect.CodeUnimplemented, errString("compact is not supported by this harness"))
		detail, derr := connect.NewErrorDetail(&abiv1.NotSupported{Capability: "action.compact"})
		if derr != nil {
			return nil, connect.NewError(connect.CodeInternal, derr)
		}
		err.AddDetail(detail)
		return nil, err
	default:
		err := connect.NewError(connect.CodeUnimplemented, errString("action not declared in the capability report"))
		detail, derr := connect.NewErrorDetail(&abiv1.NotSupported{Capability: "action.unknown"})
		if derr != nil {
			return nil, connect.NewError(connect.CodeInternal, derr)
		}
		err.AddDetail(detail)
		return nil, err
	}
}

func referenceSessionSnapshot(id string) *abiv1.SessionSnapshot {
	return &abiv1.SessionSnapshot{
		SessionId: id,
		Status:    abiv1.SessionStatus_SESSION_STATUS_BUSY,
		InFlightParts: []*abiv1.Part{{
			Id:      "part-inflight",
			Type:    abiv1.PartType_PART_TYPE_TEXT,
			Payload: &abiv1.Part_Text{Text: "partial answer"},
		}},
		QueueDepth: 1,
		PendingInputs: []*abiv1.InputRequest{{
			Id:       "input-1",
			Kind:     abiv1.InputKind_INPUT_KIND_QUESTION,
			Header:   "Question",
			Question: "Proceed?",
			Options:  []*abiv1.InputOption{{Label: "yes"}, {Label: "no"}},
		}},
	}
}

type errString string

func (e errString) Error() string { return string(e) }

func itoa(v uint32) string {
	if v == 0 {
		return "0"
	}
	var buf [10]byte
	i := len(buf)
	for v > 0 {
		i--
		buf[i] = byte('0' + v%10)
		v /= 10
	}
	return string(buf[i:])
}
