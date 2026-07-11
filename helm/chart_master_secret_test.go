// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package chart_test

// Helm template tests for US-50.1: the master KEK is delivered as a read-only
// file mount by default (not an env var), with a legacy env deliveryMethod opt-in.
//
// These run `helm template` (same path as operators + the Makefile target) and
// assert structural invariants on the rendered api Deployment. They skip when
// helm is not on $PATH (see helmTemplate). Run with:
//
//	go test ./helm/...

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// apiDeploymentDoc returns the rendered api Deployment doc.
func apiDeploymentDoc(t *testing.T, docs []map[string]any) map[string]any {
	t.Helper()
	for _, d := range findByKind(docs, "Deployment") {
		// The fullname helper renders <release>-llmsafespaces-api (or just
		// <release>-api when the release name already contains the chart name);
		// match by the "-api" suffix so the test is robust to fullname policy.
		if name := metaName(d); strings.HasSuffix(name, "-api") {
			return d
		}
	}
	require.Fail(t, "api Deployment not rendered", "no Deployment ending in -api")
	return nil
}

// podSpec navigates to spec.template.spec of a Deployment.
func podSpec(t *testing.T, dep map[string]any) map[string]any {
	t.Helper()
	spec, _ := dep["spec"].(map[string]any)
	require.NotNil(t, spec, "Deployment has no spec")
	tmpl, _ := spec["template"].(map[string]any)
	require.NotNil(t, tmpl, "Deployment has no spec.template")
	ps, _ := tmpl["spec"].(map[string]any)
	require.NotNil(t, ps, "Deployment has no spec.template.spec")
	return ps
}

// apiContainer returns the first container named "api" from the Deployment.
func apiContainer(t *testing.T, dep map[string]any) map[string]any {
	t.Helper()
	ps := podSpec(t, dep)
	conts, _ := ps["containers"].([]any)
	require.NotEmpty(t, conts, "no containers rendered")
	for _, c := range conts {
		cm, _ := c.(map[string]any)
		if cm["name"] == "api" {
			return cm
		}
	}
	require.Fail(t, "no container named 'api' rendered")
	return nil
}

// envValue returns the value of a container env var, or "" if absent.
func envValue(container map[string]any, name string) (string, bool) {
	envs, _ := container["env"].([]any)
	for _, e := range envs {
		em, _ := e.(map[string]any)
		if em["name"] == name {
			// Plain value
			if v, ok := em["value"].(string); ok {
				return v, true
			}
			// valueFrom (secretKeyRef) — return a marker so callers can detect presence.
			return "<valueFrom>", true
		}
	}
	return "", false
}

// hasVolume reports whether the pod spec has a volume of the given name.
func hasVolume(ps map[string]any, name string) bool {
	vols, _ := ps["volumes"].([]any)
	for _, v := range vols {
		vm, _ := v.(map[string]any)
		if vm["name"] == name {
			return true
		}
	}
	return false
}

// volumeMountPath returns the mountPath for a named volumeMount, or "" if absent.
func volumeMountPath(container map[string]any, name string) (string, bool) {
	vms, _ := container["volumeMounts"].([]any)
	for _, v := range vms {
		vm, _ := v.(map[string]any)
		if vm["name"] == name {
			mp, _ := vm["mountPath"].(string)
			return mp, true
		}
	}
	return "", false
}

// TestUS501_DefaultRender_KEKViaFileMount_NotEnvVar asserts the default Helm
// install delivers the master KEK as a file mount, not an env var.
func TestUS501_DefaultRender_KEKViaFileMount_NotEnvVar(t *testing.T) {
	docs := helmTemplate(t, "")
	dep := apiDeploymentDoc(t, docs)
	container := apiContainer(t, dep)

	// No LLMSAFESPACES_MASTER_SECRET env var (the raw value).
	if _, ok := envValue(container, "LLMSAFESPACES_MASTER_SECRET"); ok {
		t.Error("default render must NOT set the LLMSAFESPACES_MASTER_SECRET env var (file mount is the default)")
	}

	// The file-path env var points at the mount.
	filePath, ok := envValue(container, "LLMSAFESPACES_MASTER_SECRET_FILE")
	require.True(t, ok, "default render must set LLMSAFESPACES_MASTER_SECRET_FILE")
	assert.Equal(t, "/var/run/secrets/llmsafespaces/master-secret", filePath,
		"default file mount path should be /var/run/secrets/llmsafespaces/master-secret")

	// The master-secret volume + mount exist.
	ps := podSpec(t, dep)
	require.True(t, hasVolume(ps, "master-secret"), "default render must define the master-secret volume")
	mp, ok := volumeMountPath(container, "master-secret")
	require.True(t, ok, "default render must mount the master-secret volume")
	assert.Equal(t, "/var/run/secrets/llmsafespaces/master-secret", mp)
}

// TestUS501_DefaultRender_MasterSecretVolumeMode0440 asserts the secret volume
// uses defaultMode 0440 (read-only, group-readable).
func TestUS501_DefaultRender_MasterSecretVolumeMode0440(t *testing.T) {
	docs := helmTemplate(t, "")
	dep := apiDeploymentDoc(t, docs)
	ps := podSpec(t, dep)

	vols, _ := ps["volumes"].([]any)
	var msVol map[string]any
	for _, v := range vols {
		vm, _ := v.(map[string]any)
		if vm["name"] == "master-secret" {
			msVol = vm
			break
		}
	}
	require.NotNil(t, msVol, "master-secret volume must exist")

	sec, _ := msVol["secret"].(map[string]any)
	require.NotNil(t, sec, "master-secret volume must be a secret source")
	// defaultMode is rendered as a decimal number by Helm/YAML; assert it's 0o440 (288).
	mode, ok := sec["defaultMode"]
	require.True(t, ok, "master-secret volume must set defaultMode")
	var modeInt int
	switch m := mode.(type) {
	case int:
		modeInt = m
	case int64:
		modeInt = int(m)
	case float64:
		modeInt = int(m)
	default:
		t.Fatalf("unexpected defaultMode type %T: %v", mode, mode)
	}
	assert.Equal(t, 0o440, modeInt, "master-secret volume defaultMode must be 0440")
}

// TestUS501_LegacyEnvDeliveryMethod_RestoresEnvVar asserts that setting
// masterSecret.deliveryMethod=env restores the legacy env-var block and omits
// the volume.
func TestUS501_LegacyEnvDeliveryMethod_RestoresEnvVar(t *testing.T) {
	docs := helmTemplate(t, "masterSecret:\n  deliveryMethod: env\n")
	dep := apiDeploymentDoc(t, docs)
	container := apiContainer(t, dep)

	// The legacy env var is present (as a valueFrom secretKeyRef).
	_, ok := envValue(container, "LLMSAFESPACES_MASTER_SECRET")
	require.True(t, ok, "deliveryMethod=env must restore the LLMSAFESPACES_MASTER_SECRET env var")

	// No file-path env var and no master-secret volume.
	_, ok = envValue(container, "LLMSAFESPACES_MASTER_SECRET_FILE")
	assert.False(t, ok, "deliveryMethod=env must not set the file-path env var")
	ps := podSpec(t, dep)
	assert.False(t, hasVolume(ps, "master-secret"), "deliveryMethod=env must not mount the master-secret volume")
}

// TestUS501_FileMountPathOverride asserts a custom fileMountPath is respected.
func TestUS501_FileMountPathOverride(t *testing.T) {
	docs := helmTemplate(t, "masterSecret:\n  fileMountPath: /var/run/llmsafespaces/kek\n")
	dep := apiDeploymentDoc(t, docs)
	container := apiContainer(t, dep)

	filePath, ok := envValue(container, "LLMSAFESPACES_MASTER_SECRET_FILE")
	require.True(t, ok)
	assert.Equal(t, "/var/run/llmsafespaces/kek", filePath, "custom fileMountPath must propagate to env + mount")

	mp, ok := volumeMountPath(container, "master-secret")
	require.True(t, ok)
	assert.Equal(t, "/var/run/llmsafespaces/kek", mp)
}

// TestMasterSecret_MountPathNotNestedInOtherVolume guards against the exact
// failure mode where master-secret's mountPath sits *inside* another volume's
// mountPath (regression: chart_master_secret_test path was previously
// "/etc/llmsafespaces/master-secret", inside the config volume's
// "/etc/llmsafespaces"). runc rejects nested subPath secret-over-configmap
// mounts with "not a directory" on some kernel/runtime combos, surfacing as
// CrashLoopBackOff at first deploy.
//
// The invariant: no other volumeMount's mountPath may be a strict ancestor
// directory of the master-secret mountPath.
func TestMasterSecret_MountPathNotNestedInOtherVolume(t *testing.T) {
	docs := helmTemplate(t, "")
	dep := apiDeploymentDoc(t, docs)
	container := apiContainer(t, dep)

	msMount, ok := volumeMountPath(container, "master-secret")
	require.True(t, ok, "master-secret volumeMount must be present in default render")

	vms, _ := container["volumeMounts"].([]any)
	for _, v := range vms {
		vm, _ := v.(map[string]any)
		name, _ := vm["name"].(string)
		if name == "master-secret" {
			continue
		}
		other, _ := vm["mountPath"].(string)
		if other == "" {
			continue
		}
		// Ancestor iff msMount == other or starts with other+"/"
		ancestor := msMount == other || strings.HasPrefix(msMount, strings.TrimRight(other, "/")+"/")
		assert.Falsef(t, ancestor,
			"master-secret mountPath %q is nested inside volumeMount %q (path %q); "+
				"runc rejects nested secret-over-configmap subPath mounts. Choose a "+
				"sibling location like /var/run/secrets/llmsafespaces/master-secret.",
			msMount, name, other)
	}
}
