// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

// Package version is the single source of truth for the build version string.
// The Version variable is overridden at build time via -ldflags:
//
//	go build -ldflags "-X github.com/lenaxia/llmsafespaces/pkg/version.Version=v0.4.5"
//
// When unset (e.g. local dev), Version is "dev". The release workflow sets
// it from the git tag.
//
// Each component that exposes a version surface (API /livez, /readyz;
// controller --version flag; CLI tools) reads from here so we never have
// two competing version strings.
package version

// Version is the build version. Overridden at build time. See package doc.
var Version = "dev"
