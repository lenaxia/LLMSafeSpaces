// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package workspace

// opencode_pins.go — the opencode INSTANCE of the generalized overlay
// pin resolver (overlay_pins.go), design 0053 §4.2. Same CI stamping
// contract as agentd (per-arch binary sha256s as index annotations,
// covered by the digest), separate ConfigMap cache and annotation
// namespace so the two artifacts can never satisfy each other's pins.

import (
	"context"
	"errors"
)

const (
	opencodeAnnotationKeyAMD64 = "dev.llmsafespaces/opencode.sha256-amd64"
	opencodeAnnotationKeyARM64 = "dev.llmsafespaces/opencode.sha256-arm64"

	// OpencodePinsConfigMapName caches the last successfully resolved
	// opencode pins per digest so a controller restart during a registry
	// outage does not brick startup.
	OpencodePinsConfigMapName = "llmsafespaces-opencode-pins"
)

// ErrOpencodePinsUnavailable wraps every opencode resolution failure
// where the registry was unreachable AND no usable cache existed.
// Callers can errors.Is this to offer the manual-pin hint.
var ErrOpencodePinsUnavailable = errors.New("opencode pins unavailable (registry unreachable and no usable cache)")

// errOpencodeFetchUnavailable is the internal alias kept for test
// brevity.
var errOpencodeFetchUnavailable = ErrOpencodePinsUnavailable

var opencodePinSource = overlayPinSource{
	name:            "opencode",
	configMapName:   OpencodePinsConfigMapName,
	annotationAMD64: opencodeAnnotationKeyAMD64,
	annotationARM64: opencodeAnnotationKeyARM64,
	unavailable:     ErrOpencodePinsUnavailable,
}

// ResolveOpencodePinsWithCache is the opencode startup entrypoint used
// by controller main. validateOpencodeDeliveryConfig has already run.
func ResolveOpencodePinsWithCache(ctx context.Context, image, flagAMD64, flagARM64 string) (BinaryPins, error) {
	return resolveOverlayPins(ctx, opencodePinSource, image, flagAMD64, flagARM64)
}
