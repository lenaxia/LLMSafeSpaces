// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/viper"

	k8sconfig "github.com/lenaxia/llmsafespaces/pkg/config"
)

// DefaultJWTIssuer / DefaultJWTAudience are the iss/aud claims minted on
// every JWT and validated on every parse, when the operator hasn't set
// them explicitly. Defaults keep out-of-the-box deploys working; operators
// running multiple LLMSafeSpaces instances that should not accept each
// other's tokens set their own values via Helm or env.
const (
	DefaultJWTIssuer   = "llmsafespaces"
	DefaultJWTAudience = "llmsafespaces"
)

// Config represents the application configuration
type Config struct {
	Server struct {
		Host            string        `mapstructure:"host"`
		Port            int           `mapstructure:"port"`
		ShutdownTimeout time.Duration `mapstructure:"shutdownTimeout"`
		// InferenceRelayURL is the self-hosted relay fleet URL (Epic 42)
		// the controller uses for free-tier inference IP distribution. When
		// set, ListModels remaps free-tier opencode models to providerID=opencode-relay.
		// Empty (the chart default) means direct-to-Zen mode and no remap.
		InferenceRelayURL string `mapstructure:"inferenceRelayURL"`
	} `mapstructure:"server"`

	// Use the shared Kubernetes config
	Kubernetes k8sconfig.KubernetesConfig `mapstructure:"kubernetes"`

	Database struct {
		Host            string        `mapstructure:"host"`
		Port            int           `mapstructure:"port"`
		User            string        `mapstructure:"user"`
		Password        string        `mapstructure:"password"`
		Database        string        `mapstructure:"database"`
		SSLMode         string        `mapstructure:"sslMode"`
		MaxOpenConns    int           `mapstructure:"maxOpenConns"`
		MaxIdleConns    int           `mapstructure:"maxIdleConns"`
		ConnMaxLifetime time.Duration `mapstructure:"connMaxLifetime"`
	} `mapstructure:"database"`

	Redis struct {
		Host     string `mapstructure:"host"`
		Port     int    `mapstructure:"port"`
		Password string `mapstructure:"password"`
		DB       int    `mapstructure:"db"`
		PoolSize int    `mapstructure:"poolSize"`
	} `mapstructure:"redis"`

	Auth struct {
		JWTSecret string `mapstructure:"jwtSecret"`
		// JWTPreviousSecrets is the list of previous JWT signing keys
		// retained for VALIDATION ONLY. Tokens signed with any entry
		// here are still accepted; new tokens are always signed with
		// JWTSecret. Operators rotate by:
		//   1. Move current JWTSecret to head of JWTPreviousSecrets.
		//   2. Set JWTSecret to a fresh random string.
		//   3. Restart API; old sessions stay valid until they
		//      expire (TokenDuration), at which point the entry can
		//      be removed.
		// Closes F1.7.5 (Epic 17). Set via env
		// LLMSAFESPACES_AUTH_JWTPREVIOUSSECRETS as a comma-separated
		// list, OR via the YAML key `jwtPreviousSecrets: [...]`.
		JWTPreviousSecrets []string `mapstructure:"jwtPreviousSecrets"`
		// JWTIssuer is the iss claim minted on every token and validated
		// on every parse. Default "llmsafespaces". Set when deploying
		// multiple LLMSafeSpaces instances that should not accept each
		// other's tokens. Set via env LLMSAFESPACES_AUTH_JWTISSUER.
		JWTIssuer string `mapstructure:"jwtIssuer"`
		// JWTAudience is the aud claim minted on every token and validated
		// on every parse. Default "llmsafespaces". Same deployment-shape
		// rationale as JWTIssuer. Set via env
		// LLMSAFESPACES_AUTH_JWTAUDIENCE.
		JWTAudience         string        `mapstructure:"jwtAudience"`
		TokenDuration       time.Duration `mapstructure:"tokenDuration"`
		APIKeyPrefix        string        `mapstructure:"apiKeyPrefix"`
		CookieName          string        `mapstructure:"cookieName"`
		RememberMeDuration  time.Duration `mapstructure:"rememberMeDuration"`
		RegistrationEnabled bool          `mapstructure:"registrationEnabled"`
		LockoutEnabled      bool          `mapstructure:"lockoutEnabled"`
		LockoutAttempts     int           `mapstructure:"lockoutAttempts"`
		LockoutDuration     time.Duration `mapstructure:"lockoutDuration"`
		APIKeyDEKTTL        time.Duration `mapstructure:"apiKeyDEKTTL"`
	} `mapstructure:"auth"`

	Security struct {
		AllowedOrigins       []string `mapstructure:"allowedOrigins"`
		AllowCredentials     bool     `mapstructure:"allowCredentials"`
		RootKeyProvider      string   `mapstructure:"rootKeyProvider"`
		SealedKeyPath        string   `mapstructure:"sealedKeyPath"`
		PassphrasePath       string   `mapstructure:"passphrasePath"`
		SkipMasterKeyWarning bool     `mapstructure:"skipMasterKeyWarning"`
		// KMS holds cloud KMS provider configuration (Epic 57 US-57.1).
		// When RootKeyProvider is "aws-kms", KMS.AWS.KeyArns must contain
		// ARNs for each purpose (providerCredentials, orgCredentials,
		// masterKek). See design/stories/epic-57-rce-resistance-hardening/
		// README.md D4 for the three-key model.
		KMS KMSConfig `mapstructure:"kms"`
	} `mapstructure:"security"`

	Logging struct {
		Level       string `mapstructure:"level"`
		Development bool   `mapstructure:"development"`
		Encoding    string `mapstructure:"encoding"`
	} `mapstructure:"logging"`

	RateLimiting struct {
		Enabled       bool          `mapstructure:"enabled"`
		DefaultLimit  int           `mapstructure:"defaultLimit"`
		DefaultWindow time.Duration `mapstructure:"defaultWindow"`
		BurstSize     int           `mapstructure:"burstSize"`
		Strategy      string        `mapstructure:"strategy"`
	} `mapstructure:"rateLimiting"`

	Proxy struct {
		RequestBufferSizePerWorkspace int `mapstructure:"requestBufferSizePerWorkspace"`
		RequestBufferTimeoutSeconds   int `mapstructure:"requestBufferTimeoutSeconds"`
	} `mapstructure:"proxy"`

	// Billing holds Stripe configuration for org subscriptions (Epic 43).
	// When SecretKey is empty, a NoopCheckoutProvider is used and the webhook
	// endpoint rejects all deliveries — development/test mode.
	Billing struct {
		SecretKey          string            `mapstructure:"secretKey"`
		WebhookSecret      string            `mapstructure:"webhookSecret"`
		CheckoutSuccessURL string            `mapstructure:"checkoutSuccessUrl"`
		CheckoutCancelURL  string            `mapstructure:"checkoutCancelUrl"`
		PortalReturnURL    string            `mapstructure:"portalReturnUrl"`
		PlanPrices         map[string]string `mapstructure:"planPrices"`
		Meters             map[string]string `mapstructure:"meters"`
	} `mapstructure:"billing"`

	// Email holds outbound email configuration (US-43.2 invitations). When
	// Provider is empty or "noop", NoopProvider logs to stderr — no AWS
	// dependency. "ses" requires AWS credentials via IRSA or env.
	Email struct {
		Provider    string `mapstructure:"provider"`
		SESRegion   string `mapstructure:"sesRegion"`
		FromAddress string `mapstructure:"fromAddress"`
		BaseURL     string `mapstructure:"baseUrl"`
	} `mapstructure:"email"`

	// OIDC holds SSO login wiring (US-43.10, D17). RedirectBaseURL is the
	// origin the IdP redirects back to after authentication; the full callback
	// is {RedirectBaseURL}/api/v1/auth/sso/:orgSlug/callback. When empty the
	// start endpoint derives it from the incoming request. FrontendRedirectURL
	// is where the browser lands after a successful or failed SSO callback.
	OIDC struct {
		RedirectBaseURL     string `mapstructure:"redirectBaseUrl"`
		FrontendRedirectURL string `mapstructure:"frontendRedirectUrl"`
		// StateCookieName is the signed PKCE/state cookie name.
		StateCookieName string `mapstructure:"stateCookieName"`
	} `mapstructure:"oidc"`

	// OrgSubdomainRouting holds Epic 54 (US-54.1) email-led login discovery
	// config. When BaseDomain is non-empty, POST /auth/lookup redirects found
	// users to https://<orgSlug>.<baseDomain>. When empty (subdomain routing
	// disabled — the default), the lookup falls back to the direct SSO start
	// URL (/api/v1/auth/sso/<slug>/start), which works regardless of chart
	// config. CookieDomain is the value set on the lsp_session cookie's
	// Domain attribute so the session survives root→subdomain redirects; it
	// is consumed by the auth cookie setter (US-54.3 wires it through Helm).
	OrgSubdomainRouting struct {
		BaseDomain   string `mapstructure:"baseDomain"`
		CookieDomain string `mapstructure:"cookieDomain"`
	} `mapstructure:"orgSubdomainRouting"`

	// Turnstile is Cloudflare's CAPTCHA. When Enabled, the /register
	// middleware validates the client-supplied cf-turnstile-response
	// token against SecretKey using VerifyURL. When Enabled but SecretKey
	// is empty, /register 500s at startup — fail-closed, don't run in
	// a state where the operator thinks Turnstile is on but it isn't.
	//
	// When Enabled is false, the middleware is a no-op and /register
	// accepts requests without a token. All chart-side wiring flows
	// through Enabled+SecretKey+SiteKey; there's no partial-config state.
	//
	// Wired via ops-prod cluster-config → chart values → env:
	//   LLMSAFESPACES_TURNSTILE_ENABLED     ("true" | unset)
	//   LLMSAFESPACES_TURNSTILE_SECRETKEY   (secretKeyRef; never in ConfigMap)
	//   LLMSAFESPACES_TURNSTILE_VERIFYURL   (defaults to Cloudflare production)
	Turnstile struct {
		Enabled   bool   `mapstructure:"enabled"`
		SecretKey string `mapstructure:"secretKey"`
		VerifyURL string `mapstructure:"verifyURL"`
	} `mapstructure:"turnstile"`

	// Workspace holds Helm-managed workspace defaults. Currently only
	// DefaultStorageClass is exposed here: when non-empty the API pins the
	// `workspace.defaultStorageClass` instance setting via SetHelmOverrides
	// so admins cannot override it via the settings UI (Tier 1). When
	// empty, the setting stays admin-mutable (Tier 2) with its DB-backed
	// value (which itself defaults to "" — meaning "use cluster default SC").
	//
	// This pathway exists so operators running LLMSafeSpaces on clusters
	// with dedicated low-durability StorageClasses (e.g. Longhorn 2-replica
	// pools) can declare that choice in the Helm chart rather than having
	// to remember to set it in the admin UI after every install/re-install.
	//
	// Wired via: values.yaml `workspace.defaultStorageClass` → API
	// ConfigMap `workspace.defaultStorageClass` → this field → app.go
	// SetHelmOverrides → workspace service Create path.
	Workspace struct {
		DefaultStorageClass string `mapstructure:"defaultStorageClass"`
	} `mapstructure:"workspace"`

	// Terminal holds the WebSocket terminal proxy's security config.
	//
	// AllowedOrigins governs the gorilla/websocket Upgrader's CheckOrigin:
	//   - Empty (default): same-origin only. Browser requests whose Origin
	//     does not match the API's own Host are rejected at upgrade.
	//     Non-browser clients (no Origin) are accepted; they authenticate
	//     via the single-use ticket, not cookies.
	//   - Contains "*": all origins accepted (the historical behavior).
	//     Operators who really want this must opt in explicitly.
	//   - Otherwise: same-origin requests plus anything in the list.
	//
	// Wired via: values.yaml `terminal.allowedOrigins` → API ConfigMap
	// `terminal.allowedOrigins` → this field → app.go → NewTerminalHandler.
	// Default empty so an out-of-the-box install is fail-closed against
	// cross-site WebSocket hijacking (G35).
	Terminal struct {
		AllowedOrigins []string `mapstructure:"allowedOrigins"`
	} `mapstructure:"terminal"`
}

// KMSConfig holds cloud KMS provider configuration for the master KEK
// (Epic 57 US-57.1/57.3). When Security.RootKeyProvider is "aws-kms",
// KMS.AWS must be fully configured. When "gcp-kms", KMS.GCP.
// Otherwise this struct is ignored.
type KMSConfig struct {
	AWS AWSKMSConfig `mapstructure:"aws"`
	GCP GCPKMSConfig `mapstructure:"gcp"`
}

// AWSKMSConfig holds AWS KMS-specific provider configuration.
// CredentialsFile is the path to an AWS shared-credentials file mounted
// from a K8s Secret (D2: file-mount, not IRSA — narrower trust surface).
// Region is the AWS region of the configured keys. KeyArns maps purpose
// strings to KMS key ARNs; exactly three purposes are expected (D4):
// "providerCredentials", "orgCredentials", "masterKek".
type AWSKMSConfig struct {
	Region          string            `mapstructure:"region"`
	CredentialsFile string            `mapstructure:"credentialsFile"`
	KeyArns         map[string]string `mapstructure:"keyArns"`
}

// GCPKMSConfig holds GCP KMS-specific provider configuration (US-57.3).
// Parallel to AWSKMSConfig. CredentialsFile is the path to a service-account
// JSON file mounted from a K8s Secret. KeyNames maps purpose strings to
// GCP KMS key resource names.
type GCPKMSConfig struct {
	CredentialsFile string            `mapstructure:"credentialsFile"`
	KeyNames        map[string]string `mapstructure:"keyNames"`
}

// Load loads configuration from file and environment variables
//
//nolint:gocyclo // grandfathered; tracked for incremental reduction per .golangci.yml
func Load(path string) (*Config, error) {
	var config Config

	// Set up viper
	v := viper.New()
	v.SetConfigType("yaml")

	// Read config file
	if path != "" {
		v.SetConfigFile(path)
	} else {
		// Look for config in default locations
		v.AddConfigPath("./config")
		v.AddConfigPath(".")
		v.SetConfigName("config")
	}

	// Read config file
	if err := v.ReadInConfig(); err != nil {
		return nil, fmt.Errorf("failed to read config file: %w", err)
	}

	// Set up environment variable overrides.
	// Replace underscores with dots so LLMSAFESPACES_KUBERNETES_INCLUSTER maps
	// to the nested viper key kubernetes.incluster (matches struct tag inCluster).
	v.SetEnvPrefix("LLMSAFESPACES")
	v.SetEnvKeyReplacer(strings.NewReplacer(".", "_"))
	v.AutomaticEnv()
	// Explicit bindings for nested keys that AutomaticEnv misses because
	// viper only replaces dots→underscores in env names, not the reverse.
	_ = v.BindEnv("kubernetes.inCluster", "LLMSAFESPACES_KUBERNETES_INCLUSTER")
	_ = v.BindEnv("kubernetes.configPath", "LLMSAFESPACES_KUBERNETES_CONFIGPATH")
	_ = v.BindEnv("server.inferenceRelayURL", "LLMSAFESPACES_SERVER_INFERENCERELAYURL")

	// Epic 57 US-57.1: KMS nested-key bindings.
	bindKMSEnvVars(v)

	// Unmarshal config
	if err := v.Unmarshal(&config); err != nil {
		return nil, fmt.Errorf("failed to unmarshal config: %w", err)
	}

	// Apply manual env-var overrides for fields that viper's AutomaticEnv
	// misses (nested keys, comma-separated lists, typed values). Extracted
	// to keep Load()'s cyclomatic complexity under the linter threshold.
	applyEnvOverrides(&config)

	// JWT iss/aud defaults + Turnstile env overrides + CORS validation:
	// all extracted to helpers for the same reason.
	applyAuthDefaults(&config)
	if err := applyTurnstileEnv(&config); err != nil {
		return nil, err
	}
	if err := validateSecurity(&config); err != nil {
		return nil, err
	}

	return &config, nil
}

// applyEnvOverrides applies manual os.Getenv overrides for config fields
// that viper's AutomaticEnv doesn't reliably reach (nested struct keys,
// comma-separated lists, typed values like durations and ints). Each block
// follows the same pattern: read env, parse if needed, assign if non-empty.
//
// This function is extracted from Load() to keep the latter's cyclomatic
// complexity under the project linter threshold (65). Every if-statement
// here would otherwise contribute to Load's complexity score.
func applyEnvOverrides(config *Config) {
	if v := os.Getenv("LLMSAFESPACES_DATABASE_PASSWORD"); v != "" {
		config.Database.Password = v
	}
	if v := os.Getenv("LLMSAFESPACES_REDIS_PASSWORD"); v != "" {
		config.Redis.Password = v
	}
	if v := os.Getenv("LLMSAFESPACES_AUTH_JWTSECRET"); v != "" {
		config.Auth.JWTSecret = v
	}

	// F1.7.5: comma-separated list of previous JWT secrets for
	// rotation-during-grace-period validation.
	if envPrev := os.Getenv("LLMSAFESPACES_AUTH_JWTPREVIOUSSECRETS"); envPrev != "" {
		var out []string
		for _, p := range strings.Split(envPrev, ",") {
			p = strings.TrimSpace(p)
			if p != "" {
				out = append(out, p)
			}
		}
		if len(out) > 0 {
			config.Auth.JWTPreviousSecrets = out
		}
	}

	if v := os.Getenv("LLMSAFESPACES_AUTH_LOCKOUTENABLED"); v == "true" {
		config.Auth.LockoutEnabled = true
	}
	if v := os.Getenv("LLMSAFESPACES_AUTH_LOCKOUTATTEMPTS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			config.Auth.LockoutAttempts = n
		}
	}
	if v := os.Getenv("LLMSAFESPACES_AUTH_LOCKOUTDURATION"); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			config.Auth.LockoutDuration = d
		}
	}
	if v := os.Getenv("LLMSAFESPACES_AUTH_REMEMBEREDURATION"); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d > 0 {
			config.Auth.RememberMeDuration = d
		}
	}

	if v := os.Getenv("LLMSAFESPACES_SECURITY_ALLOWEDORIGINS"); v != "" {
		config.Security.AllowedOrigins = strings.Split(v, ",")
	}
	if v := os.Getenv("LLMSAFESPACES_SECURITY_ALLOWCREDENTIALS"); v == "true" {
		config.Security.AllowCredentials = true
	}
	if v := os.Getenv("LLMSAFESPACES_SECURITY_ROOTKEYPROVIDER"); v != "" {
		config.Security.RootKeyProvider = v
	}
	if v := os.Getenv("LLMSAFESPACES_SECURITY_SEALEDKEYPATH"); v != "" {
		config.Security.SealedKeyPath = v
	}
	if v := os.Getenv("LLMSAFESPACES_SECURITY_PASSPHRASEPATH"); v != "" {
		config.Security.PassphrasePath = v
	}
	if v := os.Getenv("LLMSAFESPACES_SECURITY_SKIPMASTERKEYWARNING"); v == "true" {
		config.Security.SkipMasterKeyWarning = true
	}

	if v := os.Getenv("LLMSAFESPACES_RATELIMITING_ENABLED"); v == "true" {
		config.RateLimiting.Enabled = true
	}
	if v := os.Getenv("LLMSAFESPACES_RATELIMITING_DEFAULTLIMIT"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			config.RateLimiting.DefaultLimit = n
		}
	}
	if v := os.Getenv("LLMSAFESPACES_RATELIMITING_DEFAULTWINDOW"); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			config.RateLimiting.DefaultWindow = d
		}
	}
	if v := os.Getenv("LLMSAFESPACES_RATELIMITING_BURSTSIZE"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			config.RateLimiting.BurstSize = n
		}
	}

	if v := os.Getenv("LLMSAFESPACES_PROXY_REQUESTBUFFERSIZEPERWORKSPACE"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			config.Proxy.RequestBufferSizePerWorkspace = n
		}
	}
	if v := os.Getenv("LLMSAFESPACES_PROXY_REQUESTBUFFERTIMEOUTSECONDS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			config.Proxy.RequestBufferTimeoutSeconds = n
		}
	}

	if v := os.Getenv("LLMSAFESPACES_BILLING_SECRETKEY"); v != "" {
		config.Billing.SecretKey = v
	}
	if v := os.Getenv("LLMSAFESPACES_BILLING_WEBHOOKSECRET"); v != "" {
		config.Billing.WebhookSecret = v
	}
	if v := os.Getenv("LLMSAFESPACES_BILLING_CHECKOUTSUCCESSURL"); v != "" {
		config.Billing.CheckoutSuccessURL = v
	}
	if v := os.Getenv("LLMSAFESPACES_BILLING_CHECKOUTCANCELURL"); v != "" {
		config.Billing.CheckoutCancelURL = v
	}
	if v := os.Getenv("LLMSAFESPACES_BILLING_PORTALRETURNURL"); v != "" {
		config.Billing.PortalReturnURL = v
	}
	if v := os.Getenv("LLMSAFESPACES_EMAIL_PROVIDER"); v != "" {
		config.Email.Provider = v
	}
	if v := os.Getenv("LLMSAFESPACES_EMAIL_SESREGION"); v != "" {
		config.Email.SESRegion = v
	}
	if v := os.Getenv("LLMSAFESPACES_EMAIL_FROMADDRESS"); v != "" {
		config.Email.FromAddress = v
	}
	if v := os.Getenv("LLMSAFESPACES_EMAIL_BASEURL"); v != "" {
		config.Email.BaseURL = v
	}
	if v := os.Getenv("LLMSAFESPACES_OIDC_REDIRECTBASEURL"); v != "" {
		config.OIDC.RedirectBaseURL = v
	}
	if v := os.Getenv("LLMSAFESPACES_OIDC_FRONTENDREDIRECTURL"); v != "" {
		config.OIDC.FrontendRedirectURL = v
	}
	if v := os.Getenv("LLMSAFESPACES_OIDC_STATECOOKIENAME"); v != "" {
		config.OIDC.StateCookieName = v
	}
	for _, envKey := range []string{
		"LLMSAFESPACES_BILLING_PLANPRICES_TEAM",
		"LLMSAFESPACES_BILLING_PLANPRICES_BUSINESS",
		"LLMSAFESPACES_BILLING_PLANPRICES_ENTERPRISE",
		"LLMSAFESPACES_BILLING_PLANPRICES_PRO",
	} {
		if v := os.Getenv(envKey); v != "" {
			plan := strings.ToLower(strings.TrimPrefix(envKey, "LLMSAFESPACES_BILLING_PLANPRICES_"))
			if config.Billing.PlanPrices == nil {
				config.Billing.PlanPrices = make(map[string]string)
			}
			config.Billing.PlanPrices[plan] = v
		}
	}
	for _, envKey := range []string{
		"LLMSAFESPACES_BILLING_METERS_LLM_TOKENS",
		"LLMSAFESPACES_BILLING_METERS_COMPUTE_SECONDS",
	} {
		if v := os.Getenv(envKey); v != "" {
			meter := strings.ToLower(strings.TrimPrefix(envKey, "LLMSAFESPACES_BILLING_METERS_"))
			if config.Billing.Meters == nil {
				config.Billing.Meters = make(map[string]string)
			}
			config.Billing.Meters[meter] = v
		}
	}

	// Pod identity for leader election. Set via the Downward API in the
	// chart (metadata.name → LLMSAFESPACES_KUBERNETES_PODNAME). Without
	// this, leader election panics with "Lock identity is empty".
	if envPodName := os.Getenv("LLMSAFESPACES_KUBERNETES_PODNAME"); envPodName != "" {
		config.Kubernetes.PodName = envPodName
	}

	// Defensive fallback: if PodName is still empty but leader election is
	// enabled, fall back to os.Hostname() (the pod's hostname matches its
	// name in Kubernetes by default). Better than panicking.
	if config.Kubernetes.LeaderElection.Enabled && config.Kubernetes.PodName == "" {
		if hn, err := os.Hostname(); err == nil && hn != "" {
			config.Kubernetes.PodName = hn
		}
	}
}

// applyAuthDefaults populates Auth.JWTIssuer / Auth.JWTAudience with the
// "llmsafespaces" defaults when the operator hasn't set them. Production
// deploys go through Load(); tests construct Service directly via New(),
// which also calls this.
func applyAuthDefaults(config *Config) {
	if config.Auth.JWTIssuer == "" {
		config.Auth.JWTIssuer = DefaultJWTIssuer
	}
	if config.Auth.JWTAudience == "" {
		config.Auth.JWTAudience = DefaultJWTAudience
	}
}

// validateSecurity enforces startup-time invariants on SecurityConfig.
//
// Today: AllowedOrigins=["*"] + AllowCredentials=true is forbidden. The CORS
// spec (Fetch §3.2.1) forbids this combination because it would let any
// website read authenticated responses from this API in a victim's browser.
// Browsers reject the combo client-side, but relying on browser enforcement
// is not a security posture — a misconfigured deploy would silently produce
// broken CORS responses with no server-side signal. Fail-closed at boot
// instead: refuse to start, log the cause, operator fixes the chart values.
//
// Mirrors the Turnstile fail-closed guard pattern (applyTurnstileEnv).
var errCORSWildcardWithCredentials = fmt.Errorf("config: security.allowedOrigins=\"*\" is incompatible with security.allowCredentials=true; use an explicit origin list when allowCredentials is true")

func validateSecurity(config *Config) error {
	if !config.Security.AllowCredentials {
		return nil
	}
	for _, o := range config.Security.AllowedOrigins {
		if o == "*" {
			return errCORSWildcardWithCredentials
		}
	}
	return nil
}

// applyTurnstileEnv applies LLMSAFESPACES_TURNSTILE_* env overrides to
// config.Turnstile and returns an error when Enabled=true and SecretKey
// is empty (fail-closed guard).
//
// The env layer is separate from the YAML/viper unmarshal path because
// the secret is loaded via a K8s Secret + valueFrom.secretKeyRef in the
// chart, and viper's AutomaticEnv doesn't pick up nested keys reliably
// when they contain dots.
func applyTurnstileEnv(config *Config) error {
	if v := os.Getenv("LLMSAFESPACES_TURNSTILE_ENABLED"); v == "true" {
		config.Turnstile.Enabled = true
	}
	if v := os.Getenv("LLMSAFESPACES_TURNSTILE_SECRETKEY"); v != "" {
		config.Turnstile.SecretKey = v
	}
	if v := os.Getenv("LLMSAFESPACES_TURNSTILE_VERIFYURL"); v != "" {
		config.Turnstile.VerifyURL = v
	}
	// Sensible default for the verify URL so operators don't have to
	// set it in the common case. Cloudflare's Turnstile production
	// endpoint has been stable since GA (2023-09).
	if config.Turnstile.Enabled && config.Turnstile.VerifyURL == "" {
		config.Turnstile.VerifyURL = "https://challenges.cloudflare.com/turnstile/v0/siteverify"
	}
	// Fail-closed guard: refuse to start with Enabled=true and an empty
	// secret. Ships an obvious log line at boot rather than a subtle
	// "every /register succeeds without CAPTCHA" bypass.
	if config.Turnstile.Enabled && config.Turnstile.SecretKey == "" {
		return fmt.Errorf("config: turnstile.enabled=true but turnstile.secretKey is empty; set LLMSAFESPACES_TURNSTILE_SECRETKEY or disable")
	}
	return nil
}

// bindKMSEnvVars registers explicit Viper env-var bindings for the KMS
// nested config keys (Epic 57 US-57.1). Viper's AutomaticEnv only
// replaces dots→underscores in env names, not the reverse, so map keys
// like keyArns.providerCredentials need explicit binding.
func bindKMSEnvVars(v *viper.Viper) {
	_ = v.BindEnv("security.kms.aws.region", "LLMSAFESPACES_SECURITY_KMS_AWS_REGION")
	_ = v.BindEnv("security.kms.aws.credentialsFile", "LLMSAFESPACES_SECURITY_KMS_AWS_CREDENTIALSFILE")
	_ = v.BindEnv("security.kms.aws.keyArns.providerCredentials", "LLMSAFESPACES_SECURITY_KMS_AWS_KEYARNS_PROVIDERCREDENTIALS")
	_ = v.BindEnv("security.kms.aws.keyArns.orgCredentials", "LLMSAFESPACES_SECURITY_KMS_AWS_KEYARNS_ORGCREDENTIALS")
	_ = v.BindEnv("security.kms.aws.keyArns.masterKek", "LLMSAFESPACES_SECURITY_KMS_AWS_KEYARNS_MASTERKEK")
	_ = v.BindEnv("security.kms.gcp.credentialsFile", "LLMSAFESPACES_SECURITY_KMS_GCP_CREDENTIALSFILE")
	_ = v.BindEnv("security.kms.gcp.keyNames.providerCredentials", "LLMSAFESPACES_SECURITY_KMS_GCP_KEYNAMES_PROVIDERCREDENTIALS")
	_ = v.BindEnv("security.kms.gcp.keyNames.orgCredentials", "LLMSAFESPACES_SECURITY_KMS_GCP_KEYNAMES_ORGCREDENTIALS")
	_ = v.BindEnv("security.kms.gcp.keyNames.masterKek", "LLMSAFESPACES_SECURITY_KMS_GCP_KEYNAMES_MASTERKEK")
}
