// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package workspace

// overlay_pins.go — the generalized single-coordinate overlay pin
// resolver (#863 pattern, parameterized for the second platform overlay
// artifact per design 0053 §4.2 / README Rule 12: the second consumer
// is what pays for the abstraction, and both instances are concrete).
//
// Problem this solves: the delivery contract requires three values
// (digest-pinned image + two per-arch binary sha256s) to move together.
// Dependency bots (Renovate, Dependabot-style tooling) can update a
// docker tag+digest pair — a single line — but nothing can compute the
// sha256 of a file inside an image. A bot-initiated digest bump with
// stale hashes would exit-code every pod in the fleet (81/82 for
// agentd, 83/84 for opencode).
//
// Design: ONE Renovate-updatable coordinate per artifact. The operator
// pins repo:tag@sha256:<manifest-digest> exactly like any pinned image.
// CI stamps the per-arch binary sha256s onto the image INDEX as OCI
// annotations — covered by the digest itself, so they can never desync.
// At controller startup the resolver reads the annotations for the
// pinned digest and injects them into pods. The verify contract, exit
// codes, conditions, events, and alerts are per-artifact and unchanged.
//
// Failure modes (per artifact):
//   - registry unreachable at startup → fall back to that artifact's
//     ConfigMap cache IF it was written for the SAME digest; otherwise
//     fail startup (misconfiguration; fail closed rather than
//     unverifiable pods).
//   - index missing pin annotations → broken pipeline; fail closed.
//   - manual --<artifact>-binary-sha256-* flags → break-glass override,
//     per-arch, always win over annotations.

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	rest "k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
	ctrlconfig "sigs.k8s.io/controller-runtime/pkg/client/config"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

// annotationVersion is the shared CI-stamped version annotation on
// every platform overlay image index (informational; not a pin input).
const annotationVersion = "dev.llmsafespaces/version"

// overlayPinSource names one platform overlay artifact whose per-arch
// binary sha256 pins resolve from OCI index annotations into a
// dedicated ConfigMap cache. The two instances (agentd, opencode) share
// behavior but never state: annotation namespaces and ConfigMaps are
// per-artifact so one artifact's cache can never satisfy the other's
// resolution.
type overlayPinSource struct {
	name            string // "agentd" | "opencode" — error strings + log prefixes
	configMapName   string
	annotationAMD64 string
	annotationARM64 string
	// unavailable is the sentinel wrapped by every resolution failure
	// where the registry was unreachable AND no usable cache existed.
	// Callers errors.Is this to offer the manual-pin hint.
	unavailable error
}

// BinaryPins are the resolved per-arch binary sha256s of one overlay
// artifact.
type BinaryPins struct {
	SHA256AMD64 string
	SHA256ARM64 string
}

// ociAnnotations abstracts the annotation map of a fetched index so the
// fetch step is fakeable in tests.
type ociAnnotations map[string]string

// remoteIndexFetcher fetches the annotation map for a digest-pinned
// image reference. The production implementation uses
// go-containerregistry against the registry named in the reference.
// The context bounds the whole exchange (the caller wraps it in the
// boot timeout); without remote.WithContext ggcr uses
// context.Background() and a stalled registry hangs startup forever.
type remoteIndexFetcher func(ctx context.Context, source overlayPinSource, imageRef string) (ociAnnotations, error)

// sameDigest reports whether two image references pin the same
// manifest digest (compares the sha256 suffix; refs without one never
// match — resolution requires digest pinning).
func sameDigest(a, b string) bool {
	get := func(ref string) string {
		i := strings.LastIndex(ref, "@sha256:")
		if i < 0 {
			return ""
		}
		return ref[i+len("@sha256:"):]
	}
	da, db := get(a), get(b)
	return da != "" && da == db
}

// cachedPinResolver wraps the registry resolver with a ConfigMap cache.
type cachedPinResolver struct {
	client.Client
	Namespace string
	fetch     remoteIndexFetcher
	source    overlayPinSource
}

// Resolve fetches pins for the image; on success it (re)writes the
// cache; on failure it falls back to the cache only when the cache was
// written for the SAME image reference (same digest ⇒ same content).
func (r *cachedPinResolver) Resolve(ctx context.Context, image string) (BinaryPins, error) {
	ann, err := r.fetch(ctx, r.source, image)
	if err == nil {
		amd64, okA := ann[r.source.annotationAMD64]
		arm64, okB := ann[r.source.annotationARM64]
		if okA && okB && sha256HexRe.MatchString(amd64) && sha256HexRe.MatchString(arm64) {
			r.writeCache(ctx, image, amd64, arm64)
			return BinaryPins{SHA256AMD64: amd64, SHA256ARM64: arm64}, nil
		}
		return BinaryPins{}, fmt.Errorf("%s pins: index for %s is missing valid pin annotations", r.source.name, image)
	}

	cm := &corev1.ConfigMap{}
	getErr := r.Get(ctx, client.ObjectKey{Name: r.source.configMapName, Namespace: r.Namespace}, cm)
	if getErr != nil {
		if apierrors.IsNotFound(getErr) {
			return BinaryPins{}, fmt.Errorf("%w: registry fetch failed (%v) and no cache exists (first boot with an unreachable registry)", r.source.unavailable, err)
		}
		return BinaryPins{}, fmt.Errorf("%w: registry fetch failed (%v) and pin cache read failed (%v) — check RBAC for configmaps get on %s/%s", r.source.unavailable, err, getErr, r.Namespace, r.source.configMapName)
	}
	// Compare DIGESTS, not full refs: re-tagging the same digest
	// (:dev@X → :v1.2.0@X) must not invalidate a valid same-content
	// cache during an outage. The digest suffix is the content identity.
	if !sameDigest(cm.Data["image"], image) {
		return BinaryPins{}, fmt.Errorf("%w: registry fetch failed (%v) and cache holds a different digest (%s) — refusing to desync", r.source.unavailable, err, cm.Data["image"])
	}
	amd64, arm64 := cm.Data["sha256-amd64"], cm.Data["sha256-arm64"]
	if !sha256HexRe.MatchString(amd64) || !sha256HexRe.MatchString(arm64) {
		return BinaryPins{}, fmt.Errorf("%w: cached pins for %s are malformed", r.source.unavailable, image)
	}
	log.FromContext(ctx).Info(r.source.name+" pins: registry unavailable, using cached pins for pinned digest", "image", image)
	return BinaryPins{SHA256AMD64: amd64, SHA256ARM64: arm64}, nil
}

func (r *cachedPinResolver) writeCache(ctx context.Context, image, amd64, arm64 string) {
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: r.source.configMapName, Namespace: r.Namespace},
		Data: map[string]string{
			"image":        image,
			"sha256-amd64": amd64,
			"sha256-arm64": arm64,
		},
	}
	existing := &corev1.ConfigMap{}
	if err := r.Get(ctx, client.ObjectKey{Name: r.source.configMapName, Namespace: r.Namespace}, existing); err != nil {
		if apierrors.IsNotFound(err) {
			if err := r.Create(ctx, cm); err != nil {
				log.FromContext(ctx).Error(err, r.source.name+" pins: failed to write cache ConfigMap (continuing without cache)")
			}
			return
		}
		log.FromContext(ctx).Error(err, r.source.name+" pins: cache read failed (continuing without cache update)")
		return
	}
	existing.Data = cm.Data
	if err := r.Update(ctx, existing); err != nil {
		log.FromContext(ctx).Error(err, r.source.name+" pins: failed to update cache ConfigMap (continuing)")
	}
}

// fetchIndexAnnotations is the production remoteIndexFetcher: anonymous
// registry read of the index annotations for a digest-pinned reference.
func fetchIndexAnnotations(ctx context.Context, source overlayPinSource, imageRef string) (ociAnnotations, error) {
	if !strings.Contains(imageRef, "@") {
		return nil, fmt.Errorf("%s pins: reference %q is not digest-pinned — annotation resolution requires an immutable digest", source.name, imageRef)
	}
	ref, err := name.ParseReference(imageRef)
	if err != nil {
		return nil, fmt.Errorf("parsing %q: %w", imageRef, err)
	}
	idx, err := remote.Index(ref, remote.WithContext(ctx))
	if err != nil {
		return nil, fmt.Errorf("fetching index: %w", err)
	}
	manifest, err := idx.IndexManifest()
	if err != nil {
		return nil, fmt.Errorf("reading index manifest: %w", err)
	}
	return manifest.Annotations, nil
}

// loadConfig and prodFetchIndexAnnotations are the two production
// seams: the ambient-kubeconfig loader and the real registry fetcher.
// envtest swaps both to route the cluster path at the test API server
// and a hermetic fetcher (agentd_pins_envtest_test.go).
var (
	loadConfig                = ctrlconfig.GetConfig
	prodFetchIndexAnnotations = fetchIndexAnnotations
)

// resolvePinsFromCluster is the injectable core of the startup
// entrypoints (tests substitute the config loader and fetcher). A nil
// namespace falls back to the release default.
func resolvePinsFromCluster(ctx context.Context, loadConfig func() (*rest.Config, error), namespace string, fetcher remoteIndexFetcher, source overlayPinSource, image string) (BinaryPins, error) {
	cfg, err := loadConfig()
	if err != nil {
		return BinaryPins{}, fmt.Errorf("%s pins: loading kubeconfig for cache access: %w", source.name, err)
	}
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		return BinaryPins{}, fmt.Errorf("%s pins: scheme setup: %w", source.name, err)
	}
	c, err := client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		return BinaryPins{}, fmt.Errorf("%s pins: client setup: %w", source.name, err)
	}
	if namespace == "" {
		namespace = "llmsafespaces"
	}
	return (&cachedPinResolver{Client: c, Namespace: namespace, fetch: fetcher, source: source}).Resolve(ctx, image)
}

// resolveOverlayPins is the shared startup body behind both artifact
// entrypoints. Per-source flag validation has already run, guaranteeing
// both-or-neither hash flags: the both-set form returns them verbatim
// after hex validation (manual pin, no registry access); the neither
// form (the normal Renovate pin) resolves from the image index
// annotations via the ConfigMap-cached resolver. Runs before the
// manager starts so a broken pin fails fast.
func resolveOverlayPins(ctx context.Context, source overlayPinSource, image, flagAMD64, flagARM64 string) (BinaryPins, error) {
	if flagAMD64 != "" && flagARM64 != "" {
		if !sha256HexRe.MatchString(flagAMD64) || !sha256HexRe.MatchString(flagARM64) {
			return BinaryPins{}, fmt.Errorf("%s pins: manual hash overrides must be 64 hex chars (validation should have caught this)", source.name)
		}
		return BinaryPins{SHA256AMD64: flagAMD64, SHA256ARM64: flagARM64}, nil
	}
	return resolvePinsFromCluster(ctx, loadConfig, os.Getenv("POD_NAMESPACE"), prodFetchIndexAnnotations, source, image)
}
