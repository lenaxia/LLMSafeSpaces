// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package workspace

import (
	"context"
	"fmt"

	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/util/retry"

	apierrors "github.com/lenaxia/llmsafespaces/api/internal/errors"
	agent "github.com/lenaxia/llmsafespaces/pkg/agent"
	v1 "github.com/lenaxia/llmsafespaces/pkg/apis/llmsafespaces/v1"
	"github.com/lenaxia/llmsafespaces/pkg/settings"
	"github.com/lenaxia/llmsafespaces/pkg/types"
)

// harness_binding.go — the API tier of the bind-once harness model
// (owner ruling 2026-11-16: workspaces do NOT switch harnesses — one
// harness per workspace, bound once, immutable forever; the
// 2026-11-15 prelim's switch-with-recreate design was REJECTED, so
// there is deliberately NO switch or recreate endpoint anywhere).
//
// The three bind entrances:
//
//   - CREATE with harness   — bound at create (buildWorkspaceCRD
//     writes spec.harness directly);
//   - CREATE without        — UNBOUND: the CRD carries the
//     AnnotationHarnessUnbound marker and no pod is built until …
//   - PUT /harness          — the explicit bind (bind-while-null), or
//   - first session         — POST /sessions/new {harness} binds
//     atomically with the session (the frontend picker's submit);
//     an omitted harness on an unbound workspace binds opencode
//     (the non-interactive default — programmatic callers and board
//     workers never present a choice).
//
// Every other harness-bearing write is a 409 harness_immutable
// (NewHarnessImmutableError); the validating webhook's admission
// denial is the CRD-tier twin.

// knownHarness reports whether h is a harness the platform knows.
func knownHarness(h string) bool {
	return h == string(v1.HarnessOpencode) || h == string(v1.HarnessPi)
}

// EnabledHarnesses returns the instance's enabled harness set (the
// picker's offer list): opencode ALWAYS (union-enforced — the
// platform's default harness cannot be withdrawn), plus pi when the
// instance lists it (settings key harness.enabled; v1 platform
// config per the ruling). Values outside the known set are ignored.
// A settings failure degrades to opencode-only (the pre-ruling
// offer), never to an error: reads must not 500 on a settings hiccup.
func (s *Service) EnabledHarnesses(ctx context.Context) []string {
	out := []string{string(v1.HarnessOpencode)}
	if s.instanceSettings == nil {
		return out
	}
	list, err := s.instanceSettings.GetStrings(ctx, settings.KeyHarnessEnabled.Name())
	if err != nil {
		s.logger.Warn("harness enabled-set read failed; serving opencode-only", "error", err.Error())
		return out
	}
	for _, h := range list {
		if h == string(v1.HarnessPi) {
			out = append(out, string(v1.HarnessPi))
			break // canonical order, no duplicates
		}
	}
	return out
}

// harnessEnabled reports whether h is bindable on this instance.
func (s *Service) harnessEnabled(ctx context.Context, h string) bool {
	for _, e := range s.EnabledHarnesses(ctx) {
		if e == h {
			return true
		}
	}
	return false
}

// validateHarnessForBind checks a requested bind value: non-empty,
// known, and enabled. Validation errors are 422-shaped.
func (s *Service) validateHarnessForBind(ctx context.Context, harness string) error {
	if harness == "" {
		return apierrors.NewValidationError(
			"harness is required: choose one of the instance's enabled harnesses",
			map[string]interface{}{"field": "harness", "harnessOptions": s.EnabledHarnesses(ctx)},
			fmt.Errorf("harness is empty"))
	}
	if !knownHarness(harness) {
		return apierrors.NewValidationError(
			fmt.Sprintf("unknown harness %q (known: opencode, pi)", harness),
			map[string]interface{}{"field": "harness"},
			fmt.Errorf("unknown harness %q", harness))
	}
	if !s.harnessEnabled(ctx, harness) {
		return apierrors.NewValidationError(
			fmt.Sprintf("harness %q is not enabled on this instance", harness),
			map[string]interface{}{"field": "harness", "harnessOptions": s.EnabledHarnesses(ctx)},
			fmt.Errorf("harness %q not in enabled set", harness))
	}
	return nil
}

// effectiveBoundHarness resolves a CRD's binding state to the harness
// the workspace effectively runs: spec.harness when bound; opencode
// for LEGACY marker-less empty-harness objects (pre-ruling creates
// under the old schema default — bound opencode in every observable
// way, and backfilled to explicit by the controller); "" only for a
// genuinely born-unbound workspace (marker present). The second
// return is true when the workspace is bound (non-empty effective
// value).
func effectiveBoundHarness(crd *v1.Workspace) (string, bool) {
	if crd.Spec.Harness != "" {
		return string(crd.Spec.Harness), true
	}
	if crd.Annotations[v1.AnnotationHarnessUnbound] == "true" {
		return "", false
	}
	// Legacy: absent harness = the old default, opencode.
	return string(v1.HarnessOpencode), true
}

// harnessPtr boxes a harness string for the DTOs (nil = unbound).
func harnessPtr(h string) *string {
	if h == "" {
		return nil
	}
	v := h
	return &v
}

// BindWorkspaceHarness services PUT /workspaces/:id/harness — the
// explicit bind-while-null (owner ruling 2026-11-16). Idempotent on
// the already-bound-same-value path (200, alreadyBound); any other
// value on a bound workspace is a 409 harness_immutable; a disabled
// or unknown harness is a 422. There is NO unbind and NO switch.
func (s *Service) BindWorkspaceHarness(ctx context.Context, userID, workspaceID, harness string) (*types.BindHarnessResult, error) {
	if err := s.verifyOwner(ctx, userID, workspaceID); err != nil {
		return nil, err
	}

	crd, err := func() (*v1.Workspace, error) {
		wsClient, wErr := s.workspaceCRDClient()
		if wErr != nil {
			return nil, wErr
		}
		return wsClient.Get(ctx, workspaceID, metav1.GetOptions{})
	}()
	if err != nil {
		if k8serrors.IsNotFound(err) {
			return nil, apierrors.NewNotFoundError("workspace", workspaceID, err)
		}
		return nil, apierrors.NewInternalError("workspace_get_failed", err)
	}

	current, bound := effectiveBoundHarness(crd)
	if bound {
		if harness == current {
			// Idempotent no-op — including on a harness the instance
			// has since disabled: bound workspaces ignore enablement
			// changes (immutability protects them).
			return &types.BindHarnessResult{
				Harness:        current,
				AlreadyBound:   true,
				HarnessOptions: s.EnabledHarnesses(ctx),
			}, nil
		}
		// Any other value — a different harness OR the empty unbind
		// attempt — is the loud 409 ("value -> anything else").
		return nil, apierrors.NewHarnessImmutableError(workspaceID, current, harness)
	}

	// Genuinely unbound (born-unbound marker): "" is the required-field
	// 422 here (nothing to be idempotent against); a value is validated
	// against the enabled set, then bound exactly once.
	if err := s.validateHarnessForBind(ctx, harness); err != nil {
		return nil, err
	}
	if err := s.bindUnboundWorkspace(ctx, workspaceID, harness); err != nil {
		return nil, err
	}
	s.logger.Info("workspace harness bound (explicit)", "workspaceID", workspaceID, "harness", harness)
	return &types.BindHarnessResult{
		Harness:        harness,
		HarnessOptions: s.EnabledHarnesses(ctx),
	}, nil
}

// bindUnboundWorkspace performs the one-time ""→value spec write on a
// born-unbound workspace: fresh-read + conflict-retry (the
// clearForceRecycleAnnotation shape), sets spec.harness, and REMOVES
// the born-unbound marker (the marker only ever marks the waiting
// state). A lost race (another writer bound first) re-reads as bound:
// same value → clean return; different value → 409 harness_immutable
// (the webhook denied the loser, which also surfaces here as a
// conflict-shaped retry exit — both converge on the re-read verdict).
func (s *Service) bindUnboundWorkspace(ctx context.Context, workspaceID, harness string) error {
	return retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		fresh, err := func() (*v1.Workspace, error) {
			wsClient, wErr := s.workspaceCRDClient()
			if wErr != nil {
				return nil, wErr
			}
			return wsClient.Get(ctx, workspaceID, metav1.GetOptions{})
		}()
		if err != nil {
			return apierrors.NewInternalError("workspace_get_failed", err)
		}
		current, bound := effectiveBoundHarness(fresh)
		if bound {
			if harness == current {
				return nil // idempotent: someone bound our exact value
			}
			return apierrors.NewHarnessImmutableError(workspaceID, current, harness)
		}
		if fresh.Spec.Harness == "" && fresh.Annotations[v1.AnnotationHarnessUnbound] != "true" {
			// A legacy object that lost its race with the controller's
			// backfill stamp read window: only opencode may be stamped.
			if harness != string(v1.HarnessOpencode) {
				return apierrors.NewHarnessImmutableError(workspaceID, string(v1.HarnessOpencode), harness)
			}
		}
		fresh.Spec.Harness = v1.HarnessType(harness)
		if fresh.Annotations == nil {
			fresh.Annotations = map[string]string{}
		}
		delete(fresh.Annotations, v1.AnnotationHarnessUnbound)
		wsClient, err := s.workspaceCRDClient()
		if err != nil {
			return apierrors.NewInternalError("workspace_client_failed", err)
		}
		if _, err := wsClient.Update(ctx, fresh); err != nil {
			if k8serrors.IsConflict(err) {
				return err // retry
			}
			if k8serrors.IsForbidden(err) || k8serrors.IsInvalid(err) {
				// The webhook denied us (admission denial surfaces as
				// Forbidden/Invalid): re-read once for the verdict.
				return s.rereadBindingVerdict(ctx, workspaceID, harness)
			}
			return apierrors.NewInternalError("harness_bind_failed", err)
		}
		// VERIFY (CRD-skew seam, owner report 2026-11-16): a structural
		// apiserver serving the pre-.15 Workspace CRD PRUNES spec.harness
		// and returns success — reporting a bind that never landed parks
		// the born-unbound workspace in Creating forever (no pod, no
		// error, no event). Read back; an unlanded bind is a LOUD
		// internal error naming the operator action, never a silent
		// no-op. (Also collapses the benign re-entrancy: a concurrent
		// same-value bind reads back bound and returns clean.)
		after, err := func() (*v1.Workspace, error) {
			wsClient, wErr := s.workspaceCRDClient()
			if wErr != nil {
				return nil, wErr
			}
			return wsClient.Get(ctx, workspaceID, metav1.GetOptions{})
		}()
		if err != nil {
			return apierrors.NewInternalError("workspace_get_failed", err)
		}
		if after.Spec.Harness == "" {
			return apierrors.NewInternalError("harness_bind_not_persisted", fmt.Errorf(
				"the harness bind for workspace %s was accepted but did not persist (spec.harness reads back empty) — the cluster's Workspace CRD predates 0.44.15 (helm does not upgrade crds/; the operator must `kubectl apply -f helm/crds/workspace.yaml`); retry the bind after the CRD lands",
				workspaceID,
			))
		}
		return nil
	})
}

// rereadBindingVerdict converts a webhook denial into the API's
// binding verdict: re-read; bound-same → nil (idempotent), bound-
// different → 409 harness_immutable, still unbound → the denial was
// about something else (surface the original class honestly).
func (s *Service) rereadBindingVerdict(ctx context.Context, workspaceID, harness string) error {
	fresh, err := func() (*v1.Workspace, error) {
		wsClient, wErr := s.workspaceCRDClient()
		if wErr != nil {
			return nil, wErr
		}
		return wsClient.Get(ctx, workspaceID, metav1.GetOptions{})
	}()
	if err != nil {
		return apierrors.NewInternalError("workspace_get_failed", err)
	}
	current, bound := effectiveBoundHarness(fresh)
	if bound {
		if harness == current {
			return nil
		}
		return apierrors.NewHarnessImmutableError(workspaceID, current, harness)
	}
	// Still unbound: the denial was not a binding race. Report as a
	// conflict with the webhook's shape (the message names the rule).
	return apierrors.NewHarnessImmutableError(workspaceID, "", harness)
}

// resolveSessionHarness settles the harness for a session-create on
// this workspace and performs the first-session bind when needed
// (owner ruling 2026-11-16: "bind atomically with the session" — the
// bind lands BEFORE any pod wait or session creation, so a session
// never exists on an unbound workspace; a later session-create
// failure leaves the chosen binding in place, which is the desired
// convergence: the user's picker choice persists for the retry).
//
//	requested == ""          → the bound harness (legacy: opencode;
//	                           born-unbound omitted: bind opencode —
//	                           the non-interactive default)
//	requested == bound       → proceed (match)
//	requested != bound       → 409 harness_immutable (bound case) or
//	                           422 not-enabled (unbound case)
func (s *Service) resolveSessionHarness(ctx context.Context, crd *v1.Workspace, workspaceID, requested string) (string, error) {
	current, bound := effectiveBoundHarness(crd)
	if bound {
		if requested != "" && requested != current {
			return "", apierrors.NewHarnessImmutableError(workspaceID, current, requested)
		}
		return current, nil
	}
	// Born-unbound: the picker's submit (or the non-interactive
	// default when omitted).
	target := requested
	if target == "" {
		target = string(v1.HarnessOpencode)
	}
	if err := s.validateHarnessForBind(ctx, target); err != nil {
		return "", err
	}
	if err := s.bindUnboundWorkspace(ctx, workspaceID, target); err != nil {
		return "", err
	}
	s.logger.Info("workspace harness bound (first session)", "workspaceID", workspaceID, "harness", target)
	return target, nil
}

// SetAgentAdapter wires the platform Agent Adapter (the harness
// router) for per-harness session creation (EnsureSession's pi arm;
// the opencode arm keeps its raw pod path unchanged). Optional — nil
// leaves EnsureSession opencode-only (pre-wiring shape).
func (s *Service) SetAgentAdapter(a agent.Adapter) { s.agentAdapter = a }
