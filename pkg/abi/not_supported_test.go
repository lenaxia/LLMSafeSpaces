// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package abi_test

import (
	"context"
	"errors"
	"net/http/httptest"
	"net/url"
	"testing"

	"connectrpc.com/connect"
	"github.com/lenaxia/llmsafespaces/pkg/abi/abitest"
	abiv1 "github.com/lenaxia/llmsafespaces/pkg/abi/v1"
	abiconnect "github.com/lenaxia/llmsafespaces/pkg/abi/v1/abiconnect"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

// TestNotSupportedExpressible proves the NotSupported wire shape codegens and
// round-trips for both capability-gated refusals the design names: file
// delivery parts (D3) and undeclared typed actions (M1 op 5) — over both
// connect codecs, against the reference implementation.
func TestNotSupportedExpressible(t *testing.T) {
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

			t.Run("file_part_delivery", func(t *testing.T) {
				req := connect.NewRequest(&abiv1.DeliveryRequest{
					SessionId: "sess-1",
					EntryId:   "entry-1",
					Attempt:   1,
					Parts: []*abiv1.DeliveryPart{
						{Part: &abiv1.DeliveryPart_File{File: &abiv1.FileReference{
							Path: "/workspace/uploads/1111-notes.txt", Name: "notes.txt",
						}}},
					},
				})
				_, err := client.Deliver(context.Background(), req)
				assertNotSupported(t, err, "delivery.file_parts")
			})

			t.Run("undeclared_action", func(t *testing.T) {
				req := connect.NewRequest(&abiv1.ActionRequest{
					SessionId: "sess-1",
					Action:    &abiv1.ActionRequest_Compact{Compact: &abiv1.CompactAction{}},
				})
				_, err := client.Act(context.Background(), req)
				assertNotSupported(t, err, "action.compact")
			})

			t.Run("supported_paths_still_work", func(t *testing.T) {
				req := connect.NewRequest(&abiv1.DeliveryRequest{
					SessionId: "sess-1",
					EntryId:   "entry-2",
					Attempt:   1,
					Parts:     []*abiv1.DeliveryPart{{Part: &abiv1.DeliveryPart_Text{Text: "hello"}}},
				})
				resp, err := client.Deliver(context.Background(), req)
				if err != nil {
					t.Fatalf("text delivery failed: %v", err)
				}
				if resp.Msg.GetState() != abiv1.LedgerState_LEDGER_STATE_LEDGERED {
					t.Fatalf("text delivery state = %v, want LEDGERED", resp.Msg.GetState())
				}
			})
		})
	}
}

func assertNotSupported(t *testing.T, err error, capability string) {
	t.Helper()
	if err == nil {
		t.Fatalf("expected %s refusal, got success", capability)
	}
	connErr := new(connect.Error)
	if !errors.As(err, &connErr) {
		t.Fatalf("expected *connect.Error, got %T: %v", err, err)
	}
	if connErr.Code() != connect.CodeUnimplemented {
		t.Fatalf("code = %v, want Unimplemented (capability-gated NotSupported)", connErr.Code())
	}
	var detail *abiv1.NotSupported
	if d := findNotSupportedDetail(connErr); d != nil {
		detail = d
	}
	if detail == nil {
		t.Fatalf("no NotSupported detail on error: %v", connErr)
	}
	if detail.GetCapability() != capability {
		t.Errorf("detail.Capability = %q, want %q", detail.GetCapability(), capability)
	}
}

// codecByName returns the connect codec for a protocol codec name. The
// built-in codecs are unexported, so the JSON variant is implemented here —
// which also proves the wire shape round-trips through an independently
// implemented codec rather than the library's own.
func codecByName(name string) connect.Codec {
	switch name {
	case "proto":
		return protoCodec{}
	case "json":
		return jsonCodec{}
	default:
		panic("unknown codec " + name)
	}
}

type protoCodec struct{}

func (protoCodec) Name() string { return "proto" }

func (protoCodec) Marshal(a any) ([]byte, error) { return proto.Marshal(a.(proto.Message)) }

func (protoCodec) Unmarshal(b []byte, a any) error { return proto.Unmarshal(b, a.(proto.Message)) }

type jsonCodec struct{}

func (jsonCodec) Name() string { return "json" }

func (jsonCodec) Marshal(a any) ([]byte, error) { return protojson.Marshal(a.(proto.Message)) }

func (jsonCodec) Unmarshal(b []byte, a any) error { return protojson.Unmarshal(b, a.(proto.Message)) }

func findNotSupportedDetail(err *connect.Error) *abiv1.NotSupported {
	for _, d := range err.Details() {
		v, verr := d.Value()
		if verr != nil {
			continue
		}
		if ns, ok := v.(*abiv1.NotSupported); ok {
			return ns
		}
	}
	return nil
}
