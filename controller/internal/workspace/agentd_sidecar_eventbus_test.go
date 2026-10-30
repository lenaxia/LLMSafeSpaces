// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package workspace

// agentd_sidecar_eventbus_test.go — the design-0072 rollout pins for
// the controller side: the reconciler's bus config composes into every
// agentd sidecar's env as AGENTD_EVENTBUS_ADDR (a credentialess plain
// value) + AGENTD_EVENTBUS_PASSWORD (a secretKeyRef to the platform
// credentials secret — the password never renders into a pod spec).

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	v1 "github.com/lenaxia/llmsafespaces/pkg/apis/llmsafespaces/v1"
)

func eventbusTestWorkspace() *v1.Workspace {
	return &v1.Workspace{ObjectMeta: metav1.ObjectMeta{Name: "ws-bus-test"}}
}

func TestAgentdSidecarEventbusEnv_ComposesWhenConfigured(t *testing.T) {
	r := &WorkspaceReconciler{
		EventbusAddr:               "llmsafespaces-valkey.llmsafespaces.svc.cluster.local:6379",
		EventbusPasswordSecretName: "llmsafespaces-credentials",
		EventbusPasswordSecretKey:  "redis-password",
	}
	c := r.buildAgentdSidecarContainer(eventbusTestWorkspace(), "pw")
	var addr, password *corev1.EnvVar
	for i := range c.Env {
		switch c.Env[i].Name {
		case "AGENTD_EVENTBUS_ADDR":
			addr = &c.Env[i]
		case "AGENTD_EVENTBUS_PASSWORD":
			password = &c.Env[i]
		}
	}
	require.NotNil(t, addr, "AGENTD_EVENTBUS_ADDR must ride the sidecar when the bus is configured")
	assert.Equal(t, r.EventbusAddr, addr.Value, "the address is the plain configured value — credentials NEVER ride it")
	require.NotNil(t, password, "AGENTD_EVENTBUS_PASSWORD must ride the sidecar")
	require.NotNil(t, password.ValueFrom, "the password must be a secretKeyRef, never a plain value")
	require.NotNil(t, password.ValueFrom.SecretKeyRef)
	assert.Equal(t, "llmsafespaces-credentials", password.ValueFrom.SecretKeyRef.Name)
	assert.Equal(t, "redis-password", password.ValueFrom.SecretKeyRef.Key)
}

func TestAgentdSidecarEventbusEnv_AddrOnlyWithoutSecret(t *testing.T) {
	r := &WorkspaceReconciler{EventbusAddr: "some-redis:6379"}
	c := r.buildAgentdSidecarContainer(eventbusTestWorkspace(), "pw")
	for _, e := range c.Env {
		assert.NotEqual(t, "AGENTD_EVENTBUS_PASSWORD", e.Name,
			"no secret configured: no password env (an authless Redis is legal)")
		addr := ""
		if e.Name == "AGENTD_EVENTBUS_ADDR" {
			addr = e.Value
		}
		if addr != "" {
			assert.Equal(t, "some-redis:6379", addr)
		}
	}
}

func TestAgentdSidecarEventbusEnv_AbsentWhenUnconfigured(t *testing.T) {
	r := &WorkspaceReconciler{}
	c := r.buildAgentdSidecarContainer(eventbusTestWorkspace(), "pw")
	for _, e := range c.Env {
		assert.NotContains(t, e.Name, "AGENTD_EVENTBUS",
			"unconfigured bus: no eventbus env at all — agentd stays dormant (the pre-0072 posture)")
	}
}

// TestPodMainContainerEventbusEnv_ComposesWhenConfigured pins the
// SINGLE-CONTAINER topology leg (review round 1, finding 2a): the main
// container runs the agentd server process (the notifier + bus
// consumers), so it must carry the same eventbusEnv the sidecar gets
// (#1332 both-modes convention) — ADDR plain, PASSWORD secretKeyRef.
func TestPodMainContainerEventbusEnv_ComposesWhenConfigured(t *testing.T) {
	pwSecret := makePasswordSecret("ws-bus-main", "default")
	ws := makeWorkspace("ws-bus-main", "default", v1.WorkspacePhaseCreating)
	ws.Status.PVCName = "pvc-" + ws.Name
	ws.Spec.Runtime = "base"
	r := reconcilerFor(t, pwSecret, makeBoundPVC(ws.Status.PVCName, "default", ws.UID), makeRuntimeEnv("base"))
	r.EventbusAddr = "llmsafespaces-valkey.llmsafespaces.svc.cluster.local:6379"
	r.EventbusPasswordSecretName = "llmsafespaces-credentials"
	r.EventbusPasswordSecretKey = "redis-password"

	pod, err := r.buildPod(context.Background(), ws)
	require.NoError(t, err)

	var main *corev1.Container
	for i := range pod.Spec.Containers {
		if pod.Spec.Containers[i].Name == "workspace" {
			main = &pod.Spec.Containers[i]
			break
		}
	}
	require.NotNil(t, main, "the main workspace container must exist")

	addr, hasAddr := envVar(main, "AGENTD_EVENTBUS_ADDR")
	require.True(t, hasAddr, "the MAIN container must carry AGENTD_EVENTBUS_ADDR (single-container mode runs the bus consumers here)")
	assert.Equal(t, r.EventbusAddr, addr.Value, "the address is the plain configured value — credentials NEVER ride it")
	pw, hasPw := envVar(main, "AGENTD_EVENTBUS_PASSWORD")
	require.True(t, hasPw, "the MAIN container must carry AGENTD_EVENTBUS_PASSWORD")
	require.NotNil(t, pw.ValueFrom, "the password must be a secretKeyRef, never a plain value")
	require.NotNil(t, pw.ValueFrom.SecretKeyRef)
	assert.Equal(t, "llmsafespaces-credentials", pw.ValueFrom.SecretKeyRef.Name)
	assert.Equal(t, "redis-password", pw.ValueFrom.SecretKeyRef.Key)
}

// TestPodMainContainerEventbusEnv_AbsentWhenUnconfigured: the dormant
// default holds at the FULL pod level — no container, main or sidecar,
// carries any eventbus env.
func TestPodMainContainerEventbusEnv_AbsentWhenUnconfigured(t *testing.T) {
	pwSecret := makePasswordSecret("ws-bus-off", "default")
	ws := makeWorkspace("ws-bus-off", "default", v1.WorkspacePhaseCreating)
	ws.Status.PVCName = "pvc-" + ws.Name
	ws.Spec.Runtime = "base"
	r := reconcilerFor(t, pwSecret, makeBoundPVC(ws.Status.PVCName, "default", ws.UID), makeRuntimeEnv("base"))

	pod, err := r.buildPod(context.Background(), ws)
	require.NoError(t, err)
	for i := range pod.Spec.Containers {
		for _, e := range pod.Spec.Containers[i].Env {
			assert.NotContains(t, e.Name, "AGENTD_EVENTBUS",
				"unconfigured bus: container %q must carry no eventbus env", pod.Spec.Containers[i].Name)
		}
	}
}
