// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package workspace

// agentd_pins.go — the agentd INSTANCE of the generalized overlay pin
// resolver (overlay_pins.go). #863 origin; single Renovate-updatable
// coordinate, per-arch binary sha256s as OCI index annotations stamped
// by the merge-agentd CI job.

import (
	"context"
	"errors"
)

const (
	annotationKeyAMD64 = "dev.llmsafespaces/agentd.sha256-amd64"
	annotationKeyARM64 = "dev.llmsafespaces/agentd.sha256-arm64"

	// AgentdPinsConfigMapName caches the last successfully resolved pins
	// per digest so a controller restart during a registry outage does
	// not brick startup.
	AgentdPinsConfigMapName = "llmsafespaces-agentd-pins"
)

// ErrAgentdPinsUnavailable wraps every agentd resolution failure where
// the registry was unreachable AND no usable cache existed. Callers can
// errors.Is this to offer the manual-pin hint.
var ErrAgentdPinsUnavailable = errors.New("agentd pins unavailable (registry unreachable and no usable cache)")

// errFetchUnavailable is the internal alias kept for test brevity.
var errFetchUnavailable = ErrAgentdPinsUnavailable

var agentdPinSource = overlayPinSource{
	name:            "agentd",
	configMapName:   AgentdPinsConfigMapName,
	annotationAMD64: annotationKeyAMD64,
	annotationARM64: annotationKeyARM64,
	unavailable:     ErrAgentdPinsUnavailable,
}

// ResolvePinsWithCache is the agentd startup entrypoint used by
// controller main. validateAgentdDeliveryConfig has already run.
func ResolvePinsWithCache(ctx context.Context, image, flagAMD64, flagARM64 string) (BinaryPins, error) {
	return resolveOverlayPins(ctx, agentdPinSource, image, flagAMD64, flagARM64)
}
