// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/lenaxia/llmsafespaces/pkg/redact"
	"github.com/lenaxia/llmsafespaces/pkg/secrets"
)

// byoMintAuthKeyName holds the controller-shared mint key (design §4.4 —
// the API/controller are the only mint callers). The Secret is created by
// the controller staging reconcile (US-72.3); the router reads it lazily
// and caches briefly so boot ordering never wedges and rotation converges.
const byoMintAuthKeyName = "llm-relay-mint-key"

type byoRunConfig struct {
	listenAddr     string
	namespace      string
	retention      time.Duration
	drainGrace     time.Duration
	quotaWindow    time.Duration
	quotaRequests  int64
	quotaBytes     int64
	maxBodyBytes   int64
	maxRespBytes   int64
	informerResync time.Duration
}

func loadByoRunConfig() byoRunConfig {
	return byoRunConfig{
		listenAddr:     getEnv("LISTEN_ADDR", ":8090"),
		namespace:      getEnv("BYO_NAMESPACE", "llm-relay"),
		retention:      getEnvDuration("BYO_PRIOR_KEY_RETENTION", byoDefaultRetention),
		drainGrace:     getEnvDuration("BYO_DRAIN_GRACE", 10*time.Minute),
		quotaWindow:    getEnvDuration("BYO_QUOTA_WINDOW", time.Minute),
		quotaRequests:  int64(getEnvInt("BYO_QUOTA_REQUESTS", 120)),
		quotaBytes:     int64(getEnvInt("BYO_QUOTA_BYTES", 200<<20)),
		maxBodyBytes:   int64(getEnvInt("BYO_MAX_BODY_BYTES", byoDefaultMaxBodyBytes)),
		maxRespBytes:   int64(getEnvInt("BYO_MAX_RESP_BYTES", byoDefaultMaxRespBytes)),
		informerResync: getEnvDuration("BYO_INFORMER_RESYNC", 5*time.Minute),
	}
}

// byoMintAuth resolves the mint key lazily from its Secret, caching for a
// bounded window so rotation converges without a restart.
type byoMintAuth struct {
	client    *kubernetes.Clientset
	namespace string
	ttl       time.Duration

	ch   chan struct{}
	key  string
	age  time.Time
	once bool
}

func newByoMintAuth(client *kubernetes.Clientset, namespace string) *byoMintAuth {
	return &byoMintAuth{client: client, namespace: namespace, ttl: time.Minute, ch: make(chan struct{}, 1)}
}

func (a *byoMintAuth) current(ctx context.Context) (string, error) {
	if a.once && time.Since(a.age) < a.ttl {
		return a.key, nil
	}
	sec, err := a.client.CoreV1().Secrets(a.namespace).Get(ctx, byoMintAuthKeyName, metav1.GetOptions{})
	if err != nil {
		if a.once {
			return a.key, nil // cached last-known-good through outages
		}
		return "", err
	}
	key := string(sec.Data["mint-key"])
	if key == "" {
		return "", errors.New("mint key secret has empty mint-key data")
	}
	a.key, a.age, a.once = key, time.Now(), true
	return a.key, nil
}

// runBYO is the BYO resolve router entrypoint: bootstrap the keypair
// machinery, start the staged-envelope informer (ciphertext only), and
// serve until SIGTERM, draining in-flight streams within the grace bound
// (the #1078 pattern: a cap, not a delay — terminationGracePeriodSeconds
// is sized to the longest legitimate stream).
func runBYO(ctx context.Context) error {
	cfg := loadByoRunConfig()

	restCfg, err := rest.InClusterConfig()
	if err != nil {
		kubeconfig := os.Getenv("KUBECONFIG")
		if kubeconfig == "" {
			return fmt.Errorf("byo-router: no in-cluster config and no KUBECONFIG: %w", err)
		}
		restCfg, err = clientcmd.BuildConfigFromFlags("", kubeconfig)
		if err != nil {
			return fmt.Errorf("byo-router: loading kubeconfig: %w", err)
		}
	}
	clientset, err := kubernetes.NewForConfig(restCfg)
	if err != nil {
		return fmt.Errorf("byo-router: building client: %w", err)
	}

	store := coreV1Secrets{client: clientset.CoreV1().Secrets(cfg.namespace)}
	redactor, err := redact.NewRedactor(nil)
	if err != nil {
		return fmt.Errorf("byo-router: redactor: %w", err)
	}
	hook := stagingHookFor(redactor)

	keys := newByoKeyManager(store, cfg.retention, hook)
	if err := keys.Bootstrap(ctx); err != nil {
		return fmt.Errorf("byo-router: keypair bootstrap: %w", err)
	}

	cacheStore := newByoEnvelopeCache()
	mintAuth := newByoMintAuth(clientset, cfg.namespace)

	factory := informers.NewSharedInformerFactoryWithOptions(clientset, cfg.informerResync,
		informers.WithNamespace(cfg.namespace))
	secretInformer := factory.Core().V1().Secrets().Informer()
	_, _ = secretInformer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    func(obj any) { byoWatchAdd(ctx, obj, keys, cacheStore) },
		UpdateFunc: func(_, obj any) { byoWatchAdd(ctx, obj, keys, cacheStore) },
		DeleteFunc: func(obj any) { byoWatchDelete(obj, cacheStore) },
	})
	factory.Start(ctx.Done())
	if !cache.WaitForCacheSync(ctx.Done(), secretInformer.HasSynced) {
		return errors.New("byo-router: informer cache sync failed")
	}
	log.Printf("byo-router: informer synced, cached envelopes=%d", cacheStore.Len())

	metrics := newByoMetrics().withCacheLen(cacheStore.Len)
	server := &byoServer{
		cfg: byoServerConfig{
			listenAddr:    cfg.listenAddr,
			namespace:     cfg.namespace,
			maxBodyBytes:  cfg.maxBodyBytes,
			maxRespBytes:  cfg.maxRespBytes,
			quotaWindow:   cfg.quotaWindow,
			quotaRequests: cfg.quotaRequests,
			quotaBytes:    cfg.quotaBytes,
			retention:     cfg.retention,
		},
		minter:   nil, // set below (Secret-held mint key)
		cache:    cacheStore,
		resolve:  resolveDispatcher{keys: keys},
		quota:    newByoWorkspaceQuota(cfg.quotaWindow, cfg.quotaRequests, cfg.quotaBytes),
		redactor: redactor,
		client:   defaultRouterClient(),
		metrics:  metrics,
	}
	server.minter = newLazyMinter(mintAuth)

	httpServer := &http.Server{
		Addr:              cfg.listenAddr,
		Handler:           server.handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}

	errCh := make(chan error, 1)
	go func() {
		log.Printf("byo-router: listening on %s (namespace=%s, retention=%s)", cfg.listenAddr, cfg.namespace, cfg.retention)
		errCh <- httpServer.ListenAndServe()
	}()

	select {
	case <-ctx.Done():
		log.Printf("byo-router: draining (grace %s)...", cfg.drainGrace)
		drainCtx, cancel := context.WithTimeout(context.Background(), cfg.drainGrace)
		defer cancel()
		if err := httpServer.Shutdown(drainCtx); err != nil { //nolint:contextcheck // the drain context must outlive the canceled parent (SIGTERM) — in-flight streams finish within the grace bound
			return fmt.Errorf("byo-router: drain: %w", err)
		}
		log.Println("byo-router: drained")
		return nil
	case err := <-errCh:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	}
}

// byoWatchAdd routes watch deliveries: the keypair Secret feeds the key
// machinery (assert on every load); labeled envelope Secrets feed the
// ciphertext cache. Everything else is ignored.
func byoWatchAdd(ctx context.Context, obj any, keys *byoKeyManager, cacheStore *byoEnvelopeCache) {
	sec, ok := obj.(*corev1.Secret)
	if !ok {
		return
	}
	if sec.Name == byoKeyPairSecretName {
		keys.ApplyWatchUpdate(ctx, sec)
		return
	}
	if sec.Labels[byoEnvWorkspaceLabel] != "" {
		cacheStore.Apply(sec)
	}
}

func byoWatchDelete(obj any, cacheStore *byoEnvelopeCache) {
	sec, ok := obj.(*corev1.Secret)
	if !ok {
		return
	}
	// Keypair-Secret loss is the DR case — the manager regenerates via
	// create-or-adopt on the next bootstrap/watch cycle; eviction here is
	// ciphertext-only.
	cacheStore.Evict(sec.Name)
}

// lazyMinter fronts the token minter with the Secret-held signing key.
type lazyMinter struct {
	auth *byoMintAuth
}

func newLazyMinter(auth *byoMintAuth) *lazyMinter { return &lazyMinter{auth: auth} }

func (l *lazyMinter) AuthKey(ctx context.Context) (string, error) { return l.auth.current(ctx) }

func (l *lazyMinter) Mint(payload byoTokenPayload) (string, error) {
	key, err := l.auth.current(context.Background())
	if err != nil {
		return "", fmt.Errorf("mint key unavailable: %w", err)
	}
	return newByoTokenMinter([]byte(key)).Mint(payload)
}

func (l *lazyMinter) Verify(token string) (byoTokenPayload, error) {
	key, err := l.auth.current(context.Background())
	if err != nil {
		return byoTokenPayload{}, fmt.Errorf("mint key unavailable: %w", err)
	}
	return newByoTokenMinter([]byte(key)).Verify(token)
}

// stagingHookFor adapts the redactor for staged-key registration.
func stagingHookFor(r *redact.Redactor) secrets.StagedKeyRedactor {
	return secrets.RedactStagedKeys{Redactor: r}
}
