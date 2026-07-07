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

// Config represents the application configuration
type Config struct {
	Server struct {
		Host            string        `mapstructure:"host"`
		Port            int           `mapstructure:"port"`
		ShutdownTimeout time.Duration `mapstructure:"shutdownTimeout"`
		// InferenceRelayURL is the CF Worker URL for free-tier inference relay (Epic 26).
		// When set, ListModels remaps free-tier opencode models to providerID=opencode-relay.
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
		JWTPreviousSecrets  []string      `mapstructure:"jwtPreviousSecrets"`
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
}

// Load loads configuration from file and environment variables
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

	// Unmarshal config
	if err := v.Unmarshal(&config); err != nil {
		return nil, fmt.Errorf("failed to unmarshal config: %w", err)
	}

	// Override with environment variables for sensitive data
	if envDBPassword := os.Getenv("LLMSAFESPACES_DATABASE_PASSWORD"); envDBPassword != "" {
		config.Database.Password = envDBPassword
	}

	if envRedisPassword := os.Getenv("LLMSAFESPACES_REDIS_PASSWORD"); envRedisPassword != "" {
		config.Redis.Password = envRedisPassword
	}

	if envJWTSecret := os.Getenv("LLMSAFESPACES_AUTH_JWTSECRET"); envJWTSecret != "" {
		config.Auth.JWTSecret = envJWTSecret
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

	// Turnstile env overrides + fail-closed guard. Extracted to a
	// helper to keep Load()'s cyclomatic complexity below the project
	// linter threshold; see applyTurnstileEnv below.
	if err := applyTurnstileEnv(&config); err != nil {
		return nil, err
	}

	return &config, nil
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
