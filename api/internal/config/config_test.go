// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package config

import (
	"os"
	"testing"
	"time"
)

func TestLoadConfig(t *testing.T) {
	// Create a temporary config file
	content := `
server:
  host: "127.0.0.1"
  port: 8080
  shutdownTimeout: 30s

kubernetes:
  configPath: "/path/to/kubeconfig"
  inCluster: false
  namespace: "test-namespace"
  podName: "test-pod"
  leaderElection:
    enabled: true
    leaseDuration: 15s
    renewDeadline: 10s
    retryPeriod: 2s

database:
  host: "localhost"
  port: 5432
  user: "testuser"
  password: "testpass"
  database: "testdb"
  sslMode: "disable"
  maxOpenConns: 10
  maxIdleConns: 5
  connMaxLifetime: 5m

redis:
  host: "localhost"
  port: 6379
  password: "testpass"
  db: 0
  poolSize: 10

auth:
  jwtSecret: "test-secret"
  tokenDuration: 24h
  apiKeyPrefix: "lsp_"

logging:
  level: "debug"
  development: true
  encoding: "console"

rateLimiting:
  enabled: true
  limits:
    default:
      requests: 100
      window: 1h
`
	tmpfile, err := os.CreateTemp("", "config-*.yaml")
	if err != nil {
		t.Fatalf("Failed to create temp file: %v", err)
	}
	defer os.Remove(tmpfile.Name())

	if _, err := tmpfile.Write([]byte(content)); err != nil {
		t.Fatalf("Failed to write to temp file: %v", err)
	}
	if err := tmpfile.Close(); err != nil {
		t.Fatalf("Failed to close temp file: %v", err)
	}

	// Test loading from file
	cfg, err := Load(tmpfile.Name())
	if err != nil {
		t.Fatalf("Failed to load config: %v", err)
	}

	// Verify loaded values
	if cfg.Server.Host != "127.0.0.1" {
		t.Errorf("Expected Server.Host to be '127.0.0.1', got '%s'", cfg.Server.Host)
	}
	if cfg.Server.Port != 8080 {
		t.Errorf("Expected Server.Port to be 8080, got %d", cfg.Server.Port)
	}
	if cfg.Server.ShutdownTimeout != 30*time.Second {
		t.Errorf("Expected Server.ShutdownTimeout to be 30s, got %v", cfg.Server.ShutdownTimeout)
	}

	if cfg.Kubernetes.Namespace != "test-namespace" {
		t.Errorf("Expected Kubernetes.Namespace to be 'test-namespace', got '%s'", cfg.Kubernetes.Namespace)
	}

	if cfg.Database.Host != "localhost" {
		t.Errorf("Expected Database.Host to be 'localhost', got '%s'", cfg.Database.Host)
	}
	if cfg.Database.Password != "testpass" {
		t.Errorf("Expected Database.Password to be 'testpass', got '%s'", cfg.Database.Password)
	}

	// Test environment variable override
	os.Setenv("LLMSAFESPACES_DATABASE_PASSWORD", "envpass")
	defer os.Unsetenv("LLMSAFESPACES_DATABASE_PASSWORD")

	cfg, err = Load(tmpfile.Name())
	if err != nil {
		t.Fatalf("Failed to load config with env vars: %v", err)
	}

	if cfg.Database.Password != "envpass" {
		t.Errorf("Expected Database.Password to be overridden to 'envpass', got '%s'", cfg.Database.Password)
	}
}

func TestLoadConfigError(t *testing.T) {
	// Test with non-existent file
	_, err := Load("non-existent-file.yaml")
	if err == nil {
		t.Error("Expected error when loading non-existent file, got nil")
	}
}

// ---- Epic 34 US-34.1: RememberMeDuration and CookieName config tests ----

func writeMinimalConfig(t *testing.T, authExtra string) string {
	t.Helper()
	content := `
server:
  host: "127.0.0.1"
  port: 8080
  shutdownTimeout: 30s
kubernetes:
  configPath: "/path/to/kubeconfig"
  inCluster: false
  namespace: "test-namespace"
  podName: "test-pod"
  leaderElection:
    enabled: true
    leaseDuration: 15s
    renewDeadline: 10s
    retryPeriod: 2s
database:
  host: "localhost"
  port: 5432
  user: "testuser"
  password: "testpass"
  database: "testdb"
  sslMode: "disable"
  maxOpenConns: 10
  maxIdleConns: 5
  connMaxLifetime: 5m
redis:
  host: "localhost"
  port: 6379
  password: ""
  db: 0
  poolSize: 10
auth:
  jwtSecret: "test-secret"
  tokenDuration: 24h
  apiKeyPrefix: "lsp_"
` + authExtra + `
logging:
  level: "debug"
  development: true
  encoding: "console"
rateLimiting:
  enabled: true
  limits:
    default:
      requests: 100
      window: 1h
`
	f, err := os.CreateTemp("", "config-epic34-*.yaml")
	if err != nil {
		t.Fatalf("CreateTemp: %v", err)
	}
	if _, err := f.WriteString(content); err != nil {
		t.Fatalf("WriteString: %v", err)
	}
	f.Close()
	t.Cleanup(func() { os.Remove(f.Name()) })
	return f.Name()
}

func TestConfig_RememberMeDuration_DefaultFromYAML(t *testing.T) {
	path := writeMinimalConfig(t, "  rememberMeDuration: 720h\n  cookieName: lsp_session\n")
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Auth.RememberMeDuration != 720*time.Hour {
		t.Errorf("expected RememberMeDuration=720h, got %v", cfg.Auth.RememberMeDuration)
	}
}

func TestConfig_CookieName_DefaultFromYAML(t *testing.T) {
	path := writeMinimalConfig(t, "  cookieName: lsp_session\n")
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Auth.CookieName != "lsp_session" {
		t.Errorf("expected CookieName=lsp_session, got %q", cfg.Auth.CookieName)
	}
}

func TestConfig_RememberMeDuration_EnvOverride(t *testing.T) {
	t.Setenv("LLMSAFESPACES_AUTH_REMEMBEREDURATION", "168h")
	path := writeMinimalConfig(t, "  rememberMeDuration: 720h\n")
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Auth.RememberMeDuration != 168*time.Hour {
		t.Errorf("expected RememberMeDuration=168h from env, got %v", cfg.Auth.RememberMeDuration)
	}
}

func TestConfig_RememberMeDuration_InvalidEnvIgnored(t *testing.T) {
	t.Setenv("LLMSAFESPACES_AUTH_REMEMBEREDURATION", "not-a-duration")
	path := writeMinimalConfig(t, "  rememberMeDuration: 720h\n")
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Auth.RememberMeDuration != 720*time.Hour {
		t.Errorf("invalid env value should be ignored; expected 720h from YAML, got %v", cfg.Auth.RememberMeDuration)
	}
}

func TestConfig_RememberMeDuration_ZeroEnvIgnored(t *testing.T) {
	t.Setenv("LLMSAFESPACES_AUTH_REMEMBEREDURATION", "0")
	path := writeMinimalConfig(t, "  rememberMeDuration: 720h\n")
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Auth.RememberMeDuration != 720*time.Hour {
		t.Errorf("zero env value should be ignored (d > 0 guard); expected 720h from YAML, got %v", cfg.Auth.RememberMeDuration)
	}
}

func TestConfig_ProxyRequestBuffer_EnvOverrides(t *testing.T) {
	t.Setenv("LLMSAFESPACES_PROXY_REQUESTBUFFERSIZEPERWORKSPACE", "7")
	t.Setenv("LLMSAFESPACES_PROXY_REQUESTBUFFERTIMEOUTSECONDS", "45")
	path := writeMinimalConfig(t, "")
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Proxy.RequestBufferSizePerWorkspace != 7 {
		t.Errorf("expected RequestBufferSizePerWorkspace=7 from env, got %d", cfg.Proxy.RequestBufferSizePerWorkspace)
	}
	if cfg.Proxy.RequestBufferTimeoutSeconds != 45 {
		t.Errorf("expected RequestBufferTimeoutSeconds=45 from env, got %d", cfg.Proxy.RequestBufferTimeoutSeconds)
	}
}

func TestConfig_ProxyRequestBuffer_InvalidEnvIgnored(t *testing.T) {
	t.Setenv("LLMSAFESPACES_PROXY_REQUESTBUFFERSIZEPERWORKSPACE", "not-a-number")
	t.Setenv("LLMSAFESPACES_PROXY_REQUESTBUFFERTIMEOUTSECONDS", "-3")
	path := writeMinimalConfig(t, "")
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Proxy.RequestBufferSizePerWorkspace != 0 {
		t.Errorf("invalid size env should be ignored; expected 0, got %d", cfg.Proxy.RequestBufferSizePerWorkspace)
	}
	if cfg.Proxy.RequestBufferTimeoutSeconds != 0 {
		t.Errorf("non-positive timeout env should be ignored; expected 0, got %d", cfg.Proxy.RequestBufferTimeoutSeconds)
	}
}

// TestConfig_Turnstile_DisabledIsDefault: with no Turnstile env vars
// set, Load() succeeds and Enabled=false — the SDK entry point is a
// no-op unless the operator opts in.
func TestConfig_Turnstile_DisabledIsDefault(t *testing.T) {
	path := writeMinimalConfig(t, "")
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Turnstile.Enabled {
		t.Fatal("expected Turnstile.Enabled=false by default")
	}
}

// TestConfig_Turnstile_EnabledWithSecretIsValid: standard prod
// configuration — enabled + secret + default verify URL.
func TestConfig_Turnstile_EnabledWithSecretIsValid(t *testing.T) {
	t.Setenv("LLMSAFESPACES_TURNSTILE_ENABLED", "true")
	t.Setenv("LLMSAFESPACES_TURNSTILE_SECRETKEY", "0xtest_secret")
	path := writeMinimalConfig(t, "")
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load returned error: %v", err)
	}
	if !cfg.Turnstile.Enabled {
		t.Error("expected Turnstile.Enabled=true from env")
	}
	if cfg.Turnstile.SecretKey != "0xtest_secret" {
		t.Errorf("expected SecretKey='0xtest_secret', got %q", cfg.Turnstile.SecretKey)
	}
	if cfg.Turnstile.VerifyURL != "https://challenges.cloudflare.com/turnstile/v0/siteverify" {
		t.Errorf("expected default VerifyURL, got %q", cfg.Turnstile.VerifyURL)
	}
}

// TestConfig_Turnstile_EnabledWithoutSecretFailsClosed: the whole
// point of the fail-closed guard is to refuse to start with an
// unusable Turnstile config. Load() must return an error, not silently
// leave Enabled=true with an empty secret.
func TestConfig_Turnstile_EnabledWithoutSecretFailsClosed(t *testing.T) {
	t.Setenv("LLMSAFESPACES_TURNSTILE_ENABLED", "true")
	// Deliberately do NOT set LLMSAFESPACES_TURNSTILE_SECRETKEY.
	path := writeMinimalConfig(t, "")
	cfg, err := Load(path)
	if err == nil {
		t.Fatalf("expected Load to fail-closed with enabled+empty-secret; got cfg=%+v", cfg)
	}
	if !containsAny(err.Error(), "turnstile.enabled", "secretKey", "empty") {
		t.Errorf("error message should mention turnstile+secret; got: %v", err)
	}
}

// TestConfig_Turnstile_VerifyURLOverride: operators can point at a
// non-default endpoint (e.g. a test server) via env.
func TestConfig_Turnstile_VerifyURLOverride(t *testing.T) {
	t.Setenv("LLMSAFESPACES_TURNSTILE_ENABLED", "true")
	t.Setenv("LLMSAFESPACES_TURNSTILE_SECRETKEY", "0xtest_secret")
	t.Setenv("LLMSAFESPACES_TURNSTILE_VERIFYURL", "http://127.0.0.1:9999/verify")
	path := writeMinimalConfig(t, "")
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Turnstile.VerifyURL != "http://127.0.0.1:9999/verify" {
		t.Errorf("expected env-override VerifyURL, got %q", cfg.Turnstile.VerifyURL)
	}
}

func containsAny(s string, subs ...string) bool {
	for _, sub := range subs {
		if len(sub) == 0 {
			continue
		}
		if len(s) >= len(sub) {
			for i := 0; i+len(sub) <= len(s); i++ {
				if s[i:i+len(sub)] == sub {
					return true
				}
			}
		}
	}
	return false
}
