// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package workspace

// Single-coordinate agentd pin resolution (#863 follow-up).
//
// Problem this solves: the original contract required three values
// (digest-pinned image + two per-arch binary sha256s) to move together.
// Dependency bots (Renovate, Dependabot-style tooling) can update a
// docker tag+digest pair — a single line — but nothing can compute the
// sha256 of a file inside an image. A bot-initiated digest bump with
// stale hashes would exit-81 every pod in the fleet.
//
// Design: ONE Renovate-updatable coordinate. The operator pins
// repo:tag@sha256:<manifest-digest> exactly like any pinned image. CI
// (merge-agentd) stamps the per-arch binary sha256s onto the image
// INDEX as OCI annotations — covered by the digest itself, so they can
// never desync. At controller startup the resolver reads the
// annotations for the pinned digest and injects them into pods exactly
// as the old env-pin path did. The entrypoint verify contract, exit
// codes, conditions, events, and alerts are unchanged.
//
// Failure modes:
//   - registry unreachable at startup → fall back to a ConfigMap cache
//     IF it was written for the SAME digest; otherwise fail startup
//     (misconfiguration; fail closed rather than unverifiable pods).
//   - index missing pin annotations → broken pipeline; fail closed.
//   - manual --agentd-binary-sha256-* flags → break-glass override,
//     per-arch, always win over annotations.

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	ctrlconfig "sigs.k8s.io/controller-runtime/pkg/client/config"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

const (
	annotationKeyAMD64 = "dev.llmsafespaces/agentd.sha256-amd64"
	annotationKeyARM64 = "dev.llmsafespaces/agentd.sha256-arm64"
	annotationVersion  = "dev.llmsafespaces/version"

	// AgentdPinsConfigMapName caches the last successfully resolved pins
	// per digest so a controller restart during a registry outage does
	// not brick startup.
	AgentdPinsConfigMapName = "llmsafespaces-agentd-pins"
)

var errFetchUnavailable = errors.New("registry fetch unavailable")

// ociAnnotations abstracts the annotation map of a fetched index so the
// fetch step is fakeable in tests.
type ociAnnotations map[string]string

// remoteIndexFetcher fetches the annotation map for a digest-pinned
// image reference. The production implementation uses
// go-containerregistry against the registry named in the reference.
type remoteIndexFetcher func(imageRef string) (ociAnnotations, error)

// AgentdBinaryPins are the resolved per-arch binary sha256s.
type AgentdBinaryPins struct {
	SHA256AMD64 string
	SHA256ARM64 string
}

// registryPinResolver reads pins from the image index annotations.
type registryPinResolver struct {
	fetch remoteIndexFetcher
}

func (r *registryPinResolver) Resolve(ctx context.Context, image string) (AgentdBinaryPins, error) {
	ann, err := r.fetch(image)
	if err != nil {
		return AgentdBinaryPins{}, fmt.Errorf("agentd pins: fetching index for %s: %w", image, err)
	}
	amd64, okA := ann[annotationKeyAMD64]
	arm64, okB := ann[annotationKeyARM64]
	if !okA || !okB || !sha256HexRe.MatchString(amd64) || !sha256HexRe.MatchString(arm64) {
		return AgentdBinaryPins{}, fmt.Errorf("agentd pins: index for %s is missing valid %s/%s annotations (broken merge-agentd pipeline?)", image, annotationKeyAMD64, annotationKeyARM64)
	}
	return AgentdBinaryPins{SHA256AMD64: amd64, SHA256ARM64: arm64}, nil
}

// cachedPinResolver wraps the registry resolver with a ConfigMap cache.
type cachedPinResolver struct {
	client.Client
	Namespace string
	fetch     remoteIndexFetcher
}

// Resolve fetches pins for the image; on success it (re)writes the
// cache; on failure it falls back to the cache only when the cache was
// written for the SAME image reference (same digest ⇒ same content).
func (r *cachedPinResolver) Resolve(ctx context.Context, image string) (AgentdBinaryPins, error) {
	ann, err := r.fetch(image)
	if err == nil {
		amd64, okA := ann[annotationKeyAMD64]
		arm64, okB := ann[annotationKeyARM64]
		if okA && okB && sha256HexRe.MatchString(amd64) && sha256HexRe.MatchString(arm64) {
			r.writeCache(ctx, image, amd64, arm64)
			return AgentdBinaryPins{SHA256AMD64: amd64, SHA256ARM64: arm64}, nil
		}
		return AgentdBinaryPins{}, fmt.Errorf("agentd pins: index for %s is missing valid pin annotations", image)
	}

	cm := &corev1.ConfigMap{}
	getErr := r.Get(ctx, client.ObjectKey{Name: AgentdPinsConfigMapName, Namespace: r.Namespace}, cm)
	if getErr != nil {
		return AgentdBinaryPins{}, fmt.Errorf("agentd pins: registry fetch failed (%v) and no pin cache exists: %w", err, errFetchUnavailable)
	}
	if cm.Data["image"] != image {
		return AgentdBinaryPins{}, fmt.Errorf("agentd pins: registry fetch failed (%v) and cache holds a different digest (%s) — refusing to desync", err, cm.Data["image"])
	}
	amd64, arm64 := cm.Data["sha256-amd64"], cm.Data["sha256-arm64"]
	if !sha256HexRe.MatchString(amd64) || !sha256HexRe.MatchString(arm64) {
		return AgentdBinaryPins{}, fmt.Errorf("agentd pins: cached pins for %s are malformed", image)
	}
	log.FromContext(ctx).Info("agentd pins: registry unavailable, using cached pins for pinned digest", "image", image)
	return AgentdBinaryPins{SHA256AMD64: amd64, SHA256ARM64: arm64}, nil
}

func (r *cachedPinResolver) writeCache(ctx context.Context, image, amd64, arm64 string) {
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: AgentdPinsConfigMapName, Namespace: r.Namespace},
		Data: map[string]string{
			"image":        image,
			"sha256-amd64": amd64,
			"sha256-arm64": arm64,
		},
	}
	existing := &corev1.ConfigMap{}
	if err := r.Get(ctx, client.ObjectKey{Name: AgentdPinsConfigMapName, Namespace: r.Namespace}, existing); err != nil {
		if apierrors.IsNotFound(err) {
			if err := r.Create(ctx, cm); err != nil {
				log.FromContext(ctx).Error(err, "agentd pins: failed to write cache ConfigMap (continuing without cache)")
			}
			return
		}
		log.FromContext(ctx).Error(err, "agentd pins: cache read failed (continuing without cache update)")
		return
	}
	existing.Data = cm.Data
	if err := r.Update(ctx, existing); err != nil {
		log.FromContext(ctx).Error(err, "agentd pins: failed to update cache ConfigMap (continuing)")
	}
}

// resolveAgentdPins merges explicit flags (break-glass overrides, win
// where set) with annotation-resolved pins for anything unset.
func resolveAgentdPins(ctx context.Context, fetcher remoteIndexFetcher, image, flagAMD64, flagARM64 string) (AgentdBinaryPins, error) {
	resolved := AgentdBinaryPins{SHA256AMD64: flagAMD64, SHA256ARM64: flagARM64}
	if resolved.SHA256AMD64 != "" && resolved.SHA256ARM64 != "" {
		return resolved, nil
	}
	ann, err := fetcher(image)
	if err != nil {
		return AgentdBinaryPins{}, fmt.Errorf("agentd pins: resolving %s: %w", image, err)
	}
	if resolved.SHA256AMD64 == "" {
		v, ok := ann[annotationKeyAMD64]
		if !ok || !sha256HexRe.MatchString(v) {
			return AgentdBinaryPins{}, fmt.Errorf("agentd pins: no valid %s annotation and no --agentd-binary-sha256-amd64 override", annotationKeyAMD64)
		}
		resolved.SHA256AMD64 = v
	}
	if resolved.SHA256ARM64 == "" {
		v, ok := ann[annotationKeyARM64]
		if !ok || !sha256HexRe.MatchString(v) {
			return AgentdBinaryPins{}, fmt.Errorf("agentd pins: no valid %s annotation and no --agentd-binary-sha256-arm64 override", annotationKeyARM64)
		}
		resolved.SHA256ARM64 = v
	}
	return resolved, nil
}

// fetchIndexAnnotations is the production remoteIndexFetcher: anonymous
// registry read of the index annotations for a digest-pinned reference.
func fetchIndexAnnotations(imageRef string) (ociAnnotations, error) {
	if !strings.Contains(imageRef, "@") {
		return nil, fmt.Errorf("agentd pins: reference %q is not digest-pinned — annotation resolution requires an immutable digest", imageRef)
	}
	ref, err := name.ParseReference(imageRef)
	if err != nil {
		return nil, fmt.Errorf("parsing %q: %w", imageRef, err)
	}
	idx, err := remote.Index(ref)
	if err != nil {
		return nil, fmt.Errorf("fetching index: %w", err)
	}
	manifest, err := idx.IndexManifest()
	if err != nil {
		return nil, fmt.Errorf("reading index manifest: %w", err)
	}
	return manifest.Annotations, nil
}

// ResolvePinsWithCache is the startup entrypoint used by controller
// main: resolves any unset binary pins from the image's index
// annotations, backed by the ConfigMap cache for registry outages.
// Explicit flags always win. Runs before the manager starts (raw
// client on the ambient kubeconfig) so a broken pin fails fast.
func ResolvePinsWithCache(ctx context.Context, image, flagAMD64, flagARM64 string) (AgentdBinaryPins, error) {
	if flagAMD64 != "" && flagARM64 != "" {
		return AgentdBinaryPins{SHA256AMD64: flagAMD64, SHA256ARM64: flagARM64}, nil
	}
	cfg, err := ctrlconfig.GetConfig()
	if err != nil {
		return AgentdBinaryPins{}, fmt.Errorf("agentd pins: loading kubeconfig for cache access: %w", err)
	}
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		return AgentdBinaryPins{}, fmt.Errorf("agentd pins: scheme setup: %w", err)
	}
	c, err := client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		return AgentdBinaryPins{}, fmt.Errorf("agentd pins: client setup: %w", err)
	}
	ns := os.Getenv("POD_NAMESPACE")
	if ns == "" {
		ns = "llmsafespaces"
	}
	res := &cachedPinResolver{Client: c, Namespace: ns, fetch: fetchIndexAnnotations}
	pins, err := res.Resolve(ctx, image)
	if err != nil {
		return AgentdBinaryPins{}, err
	}
	if flagAMD64 != "" {
		pins.SHA256AMD64 = flagAMD64
	}
	if flagARM64 != "" {
		pins.SHA256ARM64 = flagARM64
	}
	return pins, nil
}
