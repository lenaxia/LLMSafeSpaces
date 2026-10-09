// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/lenaxia/llmsafespaces/pkg/diskrecovery"
)

// PIN (disk_recover round trip): params cross the socket snake_case,
// the report returns as the result map, and the full report survives
// the map round trip field-for-field (the sidecar re-marshals it into
// the HTTP response — a silent field drop would hollow the UI report).
func TestControlSocket_DiskRecoverRoundTrip(t *testing.T) {
	srv := newControlSocketServerForTest(t, "127.0.0.1:0")
	go srv.serve()
	want := diskrecovery.Report{
		DryRun: false, BeforeUsedBytes: 9600, BeforeTotalBytes: 10000,
		AfterUsedBytes: 6600, AfterTotalBytes: 10000,
		TargetRatio: 0.85, BeforeRatio: 0.96, AfterRatio: 0.66,
		BytesFreed: 3000, StoppedEarly: true, RuntimeBase: "opencode",
		Classes: []diskrecovery.ClassReport{
			{Class: "go-build-cache", Path: "/home/sandbox/.cache/go-build", Bytes: 3000, BytesFreed: 3000, Entries: 7, Status: diskrecovery.ClassFreed},
			{Class: "npm-cache", Path: "/home/sandbox/.npm", Bytes: 1000, Status: diskrecovery.ClassSkippedTargetMet},
		},
	}
	var gotReq diskrecovery.Request
	srv.diskRecover = recoverFunc(func(_ context.Context, req diskrecovery.Request) (diskrecovery.Report, error) {
		gotReq = req
		return want, nil
	})

	resp := mustDial(t, srv.addr(), `{"v":1,"id":7,"method":"disk_recover","params":{"dry_run":true,"target_ratio":0.8}}`)
	require.Nil(t, resp["error"], "unexpected error: %v", resp["error"])
	require.EqualValues(t, 7, resp["id"])
	require.True(t, gotReq.DryRun, "dry_run must cross the socket")
	require.InDelta(t, 0.8, gotReq.TargetRatio, 1e-9, "target_ratio must cross the socket")

	raw, err := json.Marshal(resp["result"])
	require.NoError(t, err)
	var got diskrecovery.Report
	require.NoError(t, json.Unmarshal(raw, &got))
	require.Equal(t, want, got, "report must survive the socket map round trip")
}

// PIN (engine unwired): a supervisor without the engine wired answers
// internal — never a silent no-op.
func TestControlSocket_DiskRecoverUnwired(t *testing.T) {
	srv := newControlSocketServerForTest(t, "127.0.0.1:0")
	go srv.serve()
	resp := mustDial(t, srv.addr(), `{"v":1,"id":1,"method":"disk_recover","params":{}}`)
	errObj, ok := resp["error"].(map[string]any)
	require.True(t, ok, "expected an error object, got %v", resp)
	require.Equal(t, "internal", errObj["code"])
}

// PIN (sentinel codes): ErrBusy → busy, ErrNoUsage → no_usage on the
// wire — the sidecar's HTTP handler maps them back to 409/503.
func TestControlSocket_DiskRecoverSentinelCodes(t *testing.T) {
	srv := newControlSocketServerForTest(t, "127.0.0.1:0")
	go srv.serve()
	srv.diskRecover = recoverFunc(func(context.Context, diskrecovery.Request) (diskrecovery.Report, error) {
		return diskrecovery.Report{}, diskrecovery.ErrBusy
	})
	resp := mustDial(t, srv.addr(), `{"v":1,"id":1,"method":"disk_recover","params":{}}`)
	errObj := resp["error"].(map[string]any)
	require.Equal(t, "busy", errObj["code"])

	srv.diskRecover = recoverFunc(func(context.Context, diskrecovery.Request) (diskrecovery.Report, error) {
		return diskrecovery.Report{}, diskrecovery.ErrNoUsage
	})
	resp = mustDial(t, srv.addr(), `{"v":1,"id":2,"method":"disk_recover","params":{}}`)
	errObj = resp["error"].(map[string]any)
	require.Equal(t, "no_usage", errObj["code"])
}

// PIN (client decode): the sidecar client maps the sentinel codes back
// to the engine's typed errors and decodes the report — the loop sidecar
// → supervisor → sidecar must be sentinel-transparent.
func TestControlClient_DiskRecoverDecodeAndSentinels(t *testing.T) {
	srv := newControlSocketServerForTest(t, "127.0.0.1:0")
	go srv.serve()
	want := diskrecovery.Report{BytesFreed: 11, RuntimeBase: "opencode", StoppedEarly: true}
	srv.diskRecover = recoverFunc(func(context.Context, diskrecovery.Request) (diskrecovery.Report, error) {
		return want, nil
	})
	cc := newControlClient(srv.addr())
	got, err := cc.DiskRecover(context.Background(), diskrecovery.Request{DryRun: true})
	require.NoError(t, err)
	require.Equal(t, want, got)

	srv.diskRecover = recoverFunc(func(context.Context, diskrecovery.Request) (diskrecovery.Report, error) {
		return diskrecovery.Report{}, diskrecovery.ErrBusy
	})
	_, err = cc.DiskRecover(context.Background(), diskrecovery.Request{})
	require.ErrorIs(t, err, diskrecovery.ErrBusy)
}

// recoverFunc adapts a function to the diskRecoverEngine seam.
type recoverFunc func(context.Context, diskrecovery.Request) (diskrecovery.Report, error)

func (f recoverFunc) Recover(ctx context.Context, req diskrecovery.Request) (diskrecovery.Report, error) {
	return f(ctx, req)
}
