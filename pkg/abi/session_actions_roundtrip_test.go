// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package abi_test

import (
	"context"
	"errors"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/lenaxia/llmsafespaces/pkg/abi/abitest"
	abiv1 "github.com/lenaxia/llmsafespaces/pkg/abi/v1"
	abiconnect "github.com/lenaxia/llmsafespaces/pkg/abi/v1/abiconnect"
	"google.golang.org/protobuf/testing/protocmp"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/google/go-cmp/cmp"
)

// messageFixtureTime is the deterministic instant the reference
// implementation stamps on send results (2026-09-15T00:00:00Z).
var messageFixtureTime = time.Date(2026, 9, 15, 0, 0, 0, 0, time.UTC)

func assertConnectCode(t *testing.T, err error, want connect.Code) {
	t.Helper()
	if err == nil {
		t.Fatalf("expected error %s, got success", want)
	}
	connErr := new(connect.Error)
	if !errors.As(err, &connErr) {
		t.Fatalf("expected *connect.Error, got %T: %v", err, err)
	}
	if connErr.Code() != want {
		t.Fatalf("code = %v, want %v (%v)", connErr.Code(), want, err)
	}
}

// TestSessionActionsExpressible (#1372): the sessions-cluster verbs —
// create/send/delete/rename — codegen and round-trip over both connect
// codecs against the reference implementation, carrying the contract
// Session/Message shapes in their results. Abort needs no new verb: it IS
// InterruptAction (D1).
func TestSessionActionsExpressible(t *testing.T) {
	for _, codec := range []string{"proto", "json"} {
		t.Run("codec_"+codec, func(t *testing.T) {
			srv := abitest.New()
			ts := httptest.NewServer(srv.Handler())
			defer ts.Close()

			base, err := url.Parse(ts.URL)
			if err != nil {
				t.Fatal(err)
			}
			client := abiconnect.NewHarnessABIServiceClient(ts.Client(), base.String(), connect.WithCodec(codecByName(codec)))

			t.Run("create_session", func(t *testing.T) {
				res, err := client.Act(context.Background(), connect.NewRequest(&abiv1.ActionRequest{
					Action: &abiv1.ActionRequest_CreateSession{CreateSession: &abiv1.CreateSessionAction{Title: "New chat"}},
				}))
				if err != nil {
					t.Fatal(err)
				}
				want := &abiv1.ActionResult{
					Result: &abiv1.ActionResult_CreateSession{CreateSession: &abiv1.CreateSessionResult{
						Session: &abiv1.Session{Id: "ses_new", Title: "New chat", Status: abiv1.SessionStatus_SESSION_STATUS_IDLE},
					}},
				}
				if diff := cmp.Diff(want, res.Msg, protocmp.Transform()); diff != "" {
					t.Fatalf("create_session result mismatch (-want +got):\n%s", diff)
				}
			})

			t.Run("send", func(t *testing.T) {
				res, err := client.Act(context.Background(), connect.NewRequest(&abiv1.ActionRequest{
					SessionId: "ses_new",
					Action: &abiv1.ActionRequest_Send{Send: &abiv1.SendAction{
						Text:  "hello",
						Model: &abiv1.ModelRef{Id: "glm-5.3", Provider: "thekaocloud"},
					}},
				}))
				if err != nil {
					t.Fatal(err)
				}
				want := &abiv1.ActionResult{
					SessionId: "ses_new",
					Result: &abiv1.ActionResult_Send{Send: &abiv1.SendResult{
						Message: &abiv1.Message{
							Id:        "msg_1",
							SessionId: "ses_new",
							Type:      abiv1.MessageType_MESSAGE_TYPE_ASSISTANT,
							CreatedAt: timestamppb.New(messageFixtureTime),
							Parts: []*abiv1.Part{{
								Id:      "prt_1",
								Type:    abiv1.PartType_PART_TYPE_TEXT,
								Payload: &abiv1.Part_Text{Text: "hi there"},
							}},
							Model: &abiv1.ModelRef{Id: "glm-5.3", Provider: "thekaocloud"},
						},
					}},
				}
				if diff := cmp.Diff(want, res.Msg, protocmp.Transform()); diff != "" {
					t.Fatalf("send result mismatch (-want +got):\n%s", diff)
				}
			})

			t.Run("delete_session", func(t *testing.T) {
				res, err := client.Act(context.Background(), connect.NewRequest(&abiv1.ActionRequest{
					SessionId: "ses_new",
					Action:    &abiv1.ActionRequest_DeleteSession{DeleteSession: &abiv1.DeleteSessionAction{}},
				}))
				if err != nil {
					t.Fatal(err)
				}
				if res.Msg.GetDeleteSession() == nil || res.Msg.GetSessionId() != "ses_new" {
					t.Fatalf("delete_session result = %v, want deleteSession arm on ses_new", res.Msg)
				}
			})

			t.Run("rename_session", func(t *testing.T) {
				res, err := client.Act(context.Background(), connect.NewRequest(&abiv1.ActionRequest{
					SessionId: "ses_new",
					Action: &abiv1.ActionRequest_RenameSession{RenameSession: &abiv1.RenameSessionAction{
						Title: "Renamed",
					}},
				}))
				if err != nil {
					t.Fatal(err)
				}
				if res.Msg.GetRenameSession() == nil {
					t.Fatalf("rename_session result = %v, want renameSession arm", res.Msg)
				}
			})

			t.Run("send_requires_text", func(t *testing.T) {
				_, err := client.Act(context.Background(), connect.NewRequest(&abiv1.ActionRequest{
					SessionId: "ses_new",
					Action:    &abiv1.ActionRequest_Send{Send: &abiv1.SendAction{}},
				}))
				assertConnectCode(t, err, connect.CodeInvalidArgument)
			})

			t.Run("rename_requires_title", func(t *testing.T) {
				_, err := client.Act(context.Background(), connect.NewRequest(&abiv1.ActionRequest{
					SessionId: "ses_new",
					Action:    &abiv1.ActionRequest_RenameSession{RenameSession: &abiv1.RenameSessionAction{}},
				}))
				assertConnectCode(t, err, connect.CodeInvalidArgument)
			})

			t.Run("delete_requires_session", func(t *testing.T) {
				_, err := client.Act(context.Background(), connect.NewRequest(&abiv1.ActionRequest{
					Action: &abiv1.ActionRequest_DeleteSession{DeleteSession: &abiv1.DeleteSessionAction{}},
				}))
				assertConnectCode(t, err, connect.CodeInvalidArgument)
			})
		})
	}
}
