// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package chart_test

// Regression tests for the optional DB bootstrap hook (issue #423).
//
// On a green-field install against a stock Postgres, the chart's pre-install
// migration Job fails because the llmsafespaces role and database do not
// exist yet, and the chart provides no mechanism to create them. Every new
// operator hits this on first install:
//
//	FATAL: database "llmsafespaces" does not exist
//	FATAL: role "llmsafespaces" does not exist
//
// Because the migration Job is a pre-install hook with
// hook-delete-policy: before-hook-creation,hook-succeeded, the chart never
// proceeds past this point. The only recovery is helm uninstall + manual DB
// provisioning + helm install again.
//
// Fix: an opt-in db-init Helm hook Job that runs as the Postgres superuser
// and creates the role + database idempotently before migrations run. It is
// disabled by default so operators with externally-managed Postgres (where a
// DBA already created the role+DB) are not affected.
//
// Hook ordering: db-init runs at hook-weight -10 (before migrations at -5),
// after the credentials Secret (-15). It connects via a separate superuser
// Secret (BYO) so the app role password is never exposed as a superuser.
//
// These tests assert the bootstrap Job exists when enabled, is correctly
// hook-ordered, runs idempotent SQL, and is absent by default.

import (
	"os/exec"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// findDBInitJob returns the rendered db-init Job, or nil if not present.
// Named <release>-llmsafespaces-db-init.
func findDBInitJob(t *testing.T, docs []map[string]any) map[string]any {
	t.Helper()
	for _, j := range findByKind(docs, "Job") {
		if strings.HasSuffix(metaName(j), "-db-init") {
			return j
		}
	}
	return nil
}

// TestDBInit_DefaultDisabled verifies no db-init Job renders with default
// values. Operators with externally-managed Postgres must not be forced
// into the superuser bootstrap path.
func TestDBInit_DefaultDisabled(t *testing.T) {
	docs := helmTemplate(t, "")
	assert.Nil(t, findDBInitJob(t, docs),
		"db-init Job must NOT render by default (dbInit.enabled defaults to false)")
}

// TestDBInit_EnabledRendersPreInstallHook verifies the bootstrap Job
// renders as a pre-install/pre-upgrade hook when enabled, ordered before
// the migration Job so the role+DB exist before migrations connect.
func TestDBInit_EnabledRendersPreInstallHook(t *testing.T) {
	docs := helmTemplate(t, "dbInit:\n  enabled: true\n  superuserSecret:\n    name: pg-superuser\n")
	job := findDBInitJob(t, docs)
	require.NotNil(t, job, "db-init Job must render when dbInit.enabled=true")

	meta, _ := job["metadata"].(map[string]any)
	ann, _ := meta["annotations"].(map[string]any)

	// Must be a pre-install AND pre-upgrade hook so role/DB are reconciled
	// on every chart upgrade too (password rotation, schema re-init).
	hook, _ := ann["helm.sh/hook"].(string)
	assert.Contains(t, hook, "pre-install",
		"db-init must be a pre-install hook (got: %q)", hook)
	assert.Contains(t, hook, "pre-upgrade",
		"db-init must be a pre-upgrade hook so role/DB are reconciled on upgrade (got: %q)", hook)

	// Must run BEFORE the migration Job (-5) so the role/DB exist when the
	// migrator connects. Must run AFTER the credentials Secret (-15) so the
	// app-user password it sets is already materialized.
	weight, _ := ann["helm.sh/hook-weight"].(string)
	assert.Equal(t, "-10", weight,
		"db-init hook-weight must be -10 (before migrations -5, after Secret -15); got %q", weight)

	// Succeeded hooks are cleaned up but re-created before next install so
	// the bootstrap is re-runnable across upgrades.
	del, _ := ann["helm.sh/hook-delete-policy"].(string)
	assert.Contains(t, del, "before-hook-creation",
		"db-init must delete before-hook-creation so re-runs are clean (got %q)", del)
	assert.Contains(t, del, "hook-succeeded",
		"db-init must delete on hook-succeeded (got %q)", del)
}

// TestDBInit_UsesSuperuserSecret verifies the bootstrap Job connects as the
// Postgres superuser via a SEPARATE secret, never the app credentials
// secret. The app role has no CREATE DATABASE privilege and is the very
// role being created.
func TestDBInit_UsesSuperuserSecret(t *testing.T) {
	docs := helmTemplate(t, "dbInit:\n  enabled: true\n  superuserSecret:\n    name: pg-superuser\n")
	job := findDBInitJob(t, docs)
	require.NotNil(t, job)

	env := dbInitContainerEnv(t, job, "db-init")
	// PGUSER / PGPASSWORD must come from the named superuser Secret.
	requireSuperuserEnv(t, env, "PGUSER", "pg-superuser", "username")
	requireSuperuserEnv(t, env, "PGPASSWORD", "pg-superuser", "password")

	// The app-user password (APP_PASSWORD) is read from the chart's own
	// credentials Secret so the role is created with the same password the
	// migrator and API will use.
	var foundAppPwd bool
	credsSecret := "test-release-llmsafespaces-credentials"
	for _, e := range env {
		em, ok := e.(map[string]any)
		if !ok {
			continue
		}
		if name, _ := em["name"].(string); name == "APP_PASSWORD" {
			foundAppPwd = true
			ref, _ := em["valueFrom"].(map[string]any)
			skr, _ := ref["secretKeyRef"].(map[string]any)
			assert.Equal(t, credsSecret, skr["name"],
				"APP_PASSWORD must come from the chart credentials Secret %q", credsSecret)
			assert.Equal(t, "postgres-password", skr["key"],
				"APP_PASSWORD must read the postgres-password key")
		}
	}
	assert.True(t, foundAppPwd,
		"db-init must receive APP_PASSWORD (the role's password) from the credentials Secret")
}

// TestDBInit_RunsIdempotentCreateSQL verifies the bootstrap Job runs SQL
// that is idempotent (safe across re-runs / upgrades) — i.e. checks for
// existence before creating, rather than blind CREATE that errors on the
// second run.
func TestDBInit_RunsIdempotentCreateSQL(t *testing.T) {
	docs := helmTemplate(t, "dbInit:\n  enabled: true\n  superuserSecret:\n    name: pg-superuser\n")
	job := findDBInitJob(t, docs)
	require.NotNil(t, job)

	script := joinContainerCommand(t, job, "db-init")

	// Role creation must be guarded by an existence check.
	assert.Contains(t, script, "pg_roles",
		"db-init SQL must check pg_roles before CREATE ROLE (idempotency)")
	assert.Contains(t, script, "CREATE ROLE",
		"db-init SQL must create the application role")

	// Database creation must be guarded by an existence check.
	assert.Contains(t, script, "pg_database",
		"db-init SQL must check pg_database before CREATE DATABASE (idempotency)")
	assert.Contains(t, script, "CREATE DATABASE",
		"db-init SQL must create the application database")

	// The database must be owned by the application role so the migrator
	// (which connects as the app role) has DDL privileges.
	assert.Contains(t, script, "OWNER",
		"CREATE DATABASE must set OWNER to the application role")

	// set -o pipefail so a failing psql in the existence-check pipeline is
	// surfaced rather than masked by grep's exit code.
	assert.Contains(t, script, "pipefail",
		"db-init shell must set pipefail so psql connection errors are not masked by grep")
	// SQL single-quote escaping so a ' in the password cannot break the
	// statement (the password comes from a Secret the chart does not control).
	assert.Contains(t, script, "sed",
		"db-init shell must SQL-escape interpolated values (sed-based single-quote doubling)")
}

// TestDBInit_PostgresNetworkPolicyAllowsHook verifies that enabling dbInit
// extends the chart's Postgres ingress NetworkPolicy to permit the db-init
// pod. Without this rule the datastore NetworkPolicy (default-deny ingress)
// silently blocks the hook pod and the install stalls — the exact failure
// mode the hook exists to prevent. This is the critical regression: it
// catches the original #438 review finding.
func TestDBInit_PostgresNetworkPolicyAllowsHook(t *testing.T) {
	// When enabled: the postgres-ingress NetworkPolicy MUST include a rule
	// allowing component=db-init.
	docs := helmTemplate(t, "dbInit:\n  enabled: true\n  superuserSecret:\n    name: pg-superuser\n")
	rule := findIngressRuleForComponent(t, docs, "test-release-llmsafespaces-postgres-ingress", "db-init")
	require.NotNil(t, rule,
		"postgres-ingress NetworkPolicy must include a rule for component=db-init when dbInit.enabled=true")
	ports, _ := rule["ports"].([]any)
	require.NotEmpty(t, ports, "db-init rule must allow port 5432")
	pm, _ := ports[0].(map[string]any)
	assert.EqualValues(t, 5432, pm["port"],
		"db-init ingress rule must allow TCP 5432")

	// When disabled: the db-init rule MUST be absent (and the migrate rule
	// still present so migrations themselves are not broken).
	docsDisabled := helmTemplate(t, "")
	ruleDisabled := findIngressRuleForComponent(t, docsDisabled, "test-release-llmsafespaces-postgres-ingress", "db-init")
	require.Nil(t, ruleDisabled,
		"postgres-ingress NetworkPolicy must NOT include a db-init rule when dbInit.enabled=false")
	migrateRule := findIngressRuleForComponent(t, docsDisabled, "test-release-llmsafespaces-postgres-ingress", "migrate")
	require.NotNil(t, migrateRule,
		"postgres-ingress NetworkPolicy must still allow component=migrate when dbInit is disabled")
}

// TestDBInit_SetsSSLMode verifies the db-init pod propagates the configured
// sslMode via PGSSLMODE. Without it, an operator setting sslMode=require
// would see the hook use psql's default (prefer) and fail against a
// Postgres that rejects non-SSL connections — the same install-blocking
// class as the role/DB bootstrap bug. Mirrors how the migration Job threads
// DB_SSLMODE.
func TestDBInit_SetsSSLMode(t *testing.T) {
	docs := helmTemplate(t, "dbInit:\n  enabled: true\n  superuserSecret:\n    name: pg-superuser\npostgresql:\n  sslMode: require\n")
	job := findDBInitJob(t, docs)
	require.NotNil(t, job)

	env := dbInitContainerEnv(t, job, "db-init")
	var found bool
	for _, e := range env {
		em, ok := e.(map[string]any)
		if !ok {
			continue
		}
		if name, _ := em["name"].(string); name == "PGSSLMODE" {
			found = true
			assert.Equal(t, "require", em["value"],
				"PGSSLMODE must read from postgresql.sslMode")
		}
	}
	assert.True(t, found, "db-init container must define PGSSLMODE env var")
}

// findIngressRuleForComponent searches a named NetworkPolicy for an ingress
// rule whose from-podSelector matches app.kubernetes.io/component=<comp>.
// Returns nil if no such rule exists.
func findIngressRuleForComponent(t *testing.T, docs []map[string]any, npName, comp string) map[string]any {
	t.Helper()
	for _, d := range findByKind(docs, "NetworkPolicy") {
		if metaName(d) != npName {
			continue
		}
		spec, _ := d["spec"].(map[string]any)
		ingress, _ := spec["ingress"].([]any)
		for _, r := range ingress {
			rm, ok := r.(map[string]any)
			if !ok {
				continue
			}
			from, _ := rm["from"].([]any)
			for _, f := range from {
				fm, ok := f.(map[string]any)
				if !ok {
					continue
				}
				podSel, _ := fm["podSelector"].(map[string]any)
				labels, _ := podSel["matchLabels"].(map[string]any)
				if labels["app.kubernetes.io/component"] == comp {
					return rm
				}
			}
		}
	}
	return nil
}

// TestDBInit_RequiresSuperuserSecretName verifies that enabling dbInit
// without a superuser Secret name fails the render (template error) rather
// than silently rendering a Job that points at an empty Secret — that
// would just move the footgun one step later.
func TestDBInit_RequiresSuperuserSecretName(t *testing.T) {
	// helm template returns non-zero on a required() failure; the test
	// helper helmTemplate asserts cmd.Run succeeds, so we invoke helm
	// directly here and assert it fails.
	if _, err := execLookHelm(); err != nil {
		t.Skip("helm not on PATH; skipping chart render test")
	}
	dir := t.TempDir()
	valuesPath := dir + "/values.yaml"
	require.NoError(t, writeFile(valuesPath,
		"dbInit:\n  enabled: true\n  superuserSecret:\n    name: \"\"\n"))
	out, err := helmTemplateRaw(t, valuesPath)
	require.Error(t, err,
		"helm template must fail when dbInit.enabled=true but superuserSecret.name is empty")
	assert.Contains(t, string(out), "superuserSecret.name",
		"failure message must point at the missing superuserSecret.name value")
}

// execLookHelm returns the helm path and whether it is on PATH.
func execLookHelm() (string, error) {
	return exec.LookPath("helm")
}

// helmTemplateRaw runs helm template with the given values file and returns
// the combined stdout+stderr and the run error. Unlike helmTemplate(), it
// does NOT assert success — callers use it to assert a render FAILURE.
func helmTemplateRaw(t *testing.T, valuesPath string) ([]byte, error) {
	t.Helper()
	args := []string{"template", "test-release", chartDir(t), "-n", "test-ns", "-f", valuesPath}
	cmd := exec.Command("helm", args...)
	out, err := cmd.CombinedOutput()
	return out, err
}

// NOTE: dbInitContainerEnv / dbInitFindContainer mirror the container-parsing
// helpers in chart_migration_job_test.go (PR #437). If #437 merges first,
// this file should drop these and reuse the shared helpers; the symbols
// cannot coexist in the same package. Kept local here so this PR is
// independently mergeable regardless of merge order.

func dbInitFindContainer(t *testing.T, job map[string]any, name string) map[string]any {
	t.Helper()
	spec, _ := job["spec"].(map[string]any)
	tmpl, _ := spec["template"].(map[string]any)
	tspec, _ := tmpl["spec"].(map[string]any)
	containers, _ := tspec["containers"].([]any)
	for _, c := range containers {
		cm, ok := c.(map[string]any)
		if !ok {
			continue
		}
		if cn, _ := cm["name"].(string); cn == name {
			return cm
		}
	}
	require.FailNow(t, "container %q not found in Job", name)
	return nil
}

func dbInitContainerEnv(t *testing.T, job map[string]any, name string) []any {
	t.Helper()
	c := dbInitFindContainer(t, job, name)
	env, ok := c["env"].([]any)
	require.True(t, ok, "container %q must have env", name)
	return env
}

// requireSuperuserEnv asserts an env var is sourced from the named
// superuser Secret key via secretKeyRef.
func requireSuperuserEnv(t *testing.T, env []any, varName, secretName, key string) {
	t.Helper()
	for _, e := range env {
		em, ok := e.(map[string]any)
		if !ok {
			continue
		}
		if name, _ := em["name"].(string); name == varName {
			ref, _ := em["valueFrom"].(map[string]any)
			skr, _ := ref["secretKeyRef"].(map[string]any)
			assert.Equal(t, secretName, skr["name"],
				"%s must reference superuser Secret %q", varName, secretName)
			assert.Equal(t, key, skr["key"],
				"%s must read key %q from the superuser Secret", varName, key)
			return
		}
	}
	require.FailNow(t, "db-init container must define %s env var", varName)
}

// joinContainerCommand reconstructs the shell script passed to a container
// whose command is ["/bin/sh","-c"] with the script in args[0].
func joinContainerCommand(t *testing.T, job map[string]any, name string) string {
	t.Helper()
	c := dbInitFindContainer(t, job, name)
	args, _ := c["args"].([]any)
	require.NotEmpty(t, args, "container %q must have args (the SQL script)", name)
	parts := make([]string, 0, len(args))
	for _, a := range args {
		if s, ok := a.(string); ok {
			parts = append(parts, s)
		}
	}
	return strings.Join(parts, "\n")
}
