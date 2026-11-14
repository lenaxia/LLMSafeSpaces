// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package workspace

// pi_pins.go — the pi INSTANCE of the generalized overlay pin resolver
// (overlay_pins.go), design 0053 §5.4 / scoping report §2.3. Same
// CI-stamping contract as agentd/opencode (per-arch binary sha256s as
// index annotations, covered by the digest), separate ConfigMap cache
// and annotation namespace so the three artifacts can never satisfy
// each other's pins.

import (
	"context"
	"errors"
)

const (
	piAnnotationKeyAMD64 = "dev.llmsafespaces/pi.sha256-amd64"
	piAnnotationKeyARM64 = "dev.llmsafespaces/pi.sha256-arm64"

	// PiPinsConfigMapName caches the last successfully resolved pi pins
	// per digest so a controller restart during a registry outage does
	// not brick startup.
	PiPinsConfigMapName = "llmsafespaces-pi-pins"
)

// ErrPiPinsUnavailable wraps every pi resolution failure where the
// registry was unreachable AND no usable cache existed. Callers can
// errors.Is this to offer the manual-pin hint.
var ErrPiPinsUnavailable = errors.New("pi pins unavailable (registry unreachable and no usable cache)")

var piPinSource = overlayPinSource{
	name:            "pi",
	configMapName:   PiPinsConfigMapName,
	annotationAMD64: piAnnotationKeyAMD64,
	annotationARM64: piAnnotationKeyARM64,
	unavailable:     ErrPiPinsUnavailable,
}

// ResolvePiPinsWithCache is the pi startup entrypoint used by
// controller main. validatePiDeliveryConfig has already run.
func ResolvePiPinsWithCache(ctx context.Context, image, flagAMD64, flagARM64 string) (BinaryPins, error) {
	return resolveOverlayPins(ctx, piPinSource, image, flagAMD64, flagARM64)
}
