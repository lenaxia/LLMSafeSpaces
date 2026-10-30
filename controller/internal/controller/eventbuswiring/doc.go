// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

// Package eventbuswiring holds the design-0072 armed-posture envtest.
// It lives in its OWN package (not controller, where its sibling
// TestEnvtestSetupWiring_DirectReaderServesRuntimeResolution lives)
// for one mechanical reason: controller-runtime v0.20's controller-name
// registry is PROCESS-GLOBAL and never unregisters (pkg/controller/
// name.go checkName), so a second SetupControllers call in the same
// test binary collides ("controller with name workspace already
// exists"). A separate package is a separate test binary — the armed
// posture gets a real apiserver + real manager of its own.
package eventbuswiring
