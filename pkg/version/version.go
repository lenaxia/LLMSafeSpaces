// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

// Package version is the single source of truth for the build version string.
// The variables here are overridden at build time via -ldflags:
//
//	go build \
//		-ldflags "-X github.com/lenaxia/llmsafespaces/pkg/version.Version=v0.5.1
//		          -X github.com/lenaxia/llmsafespaces/pkg/version.CommitSHA=<sha>
//		          -X github.com/lenaxia/llmsafespaces/pkg/version.BuildTime=<ts>"
//
// When unset (e.g. an un-stamped local build), all three report "unknown" so
// a missing injection is obvious. The release workflow sets them from the git
// tag, sha, and build time.
//
// Each component that exposes a version surface (API /livez,
// /v1/admin/platform-info; workspace-agentd /v1/healthz; the controller
// startup log; the relay healthz endpoints) reads from here so we never have
// two competing version strings.
package version

// Version is the build version. Overridden at build time. See package doc.
var Version = "unknown"

// CommitSHA is the git commit the binary was built from. Overridden at
// build time. See package doc.
var CommitSHA = "unknown"

// BuildTime is the UTC build timestamp. Overridden at build time.
// See package doc.
var BuildTime = "unknown"
