// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package workspace

import (
	"context"
	"errors"

	"sigs.k8s.io/controller-runtime/pkg/client"
)

// errClusterReaderUnwired is returned by the direct-reader seam when
// APIReader was never wired. Every cluster-scoped lookup on the
// reconcile path fails with it instead of silently falling back to the
// cached client — a fallback would reintroduce the #1551 wedge the
// moment the informer is the broken thing.
var errClusterReaderUnwired = errors.New(
	"WorkspaceReconciler.APIReader is not wired — route mgr.GetAPIReader() in SetupControllers " +
		"(cluster-scoped reconcile reads must bypass the cache; #1587)")

// unwiredReader is the APIReader stand-in handed to lookups when the
// field is nil: it fails every read with errClusterReaderUnwired. The
// refusal is lazy by construction — reads that never happen (explicit
// image references) never fail — and SetupWithManager refuses a nil
// APIReader outright, so production cannot boot unwired; this guards
// direct constructions (tests, future binaries).
type unwiredReader struct{}

func (unwiredReader) Get(context.Context, client.ObjectKey, client.Object, ...client.GetOption) error {
	return errClusterReaderUnwired
}

func (unwiredReader) List(context.Context, client.ObjectList, ...client.ListOption) error {
	return errClusterReaderUnwired
}

// directReader returns the reader cluster-scoped reconcile-path lookups
// (RuntimeEnvironment resolution, StorageClass WFFC detection) must use
// (#1587): a cached Get of an unwatched cluster-scoped type lazily
// starts an informer that blocks forever when RBAC withholds it — one
// reconcile silently wedges a worker (#1551). Direct reads fail fast
// (an RBAC gap 403s immediately) and requeue through the normal
// recovery path. Design 0058 §4.3 precedent (RelayStagingConfig.APIReader).
func (r *WorkspaceReconciler) directReader() client.Reader {
	if r.APIReader != nil {
		return r.APIReader
	}
	return unwiredReader{}
}
