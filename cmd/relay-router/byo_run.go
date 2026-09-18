// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"sync"
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
const byoMintAuthKeyName = secrets.RelayMintKeyName

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

// mintSecretReader is the Secret-read surface the mint auth needs (narrow
// so tests can use the client-go fake).
type mintSecretReader interface {
	Get(ctx context.Context, name string, opts metav1.GetOptions) (*corev1.Secret, error)
}

// mintSecrets adapts a CoreV1 Secrets getter to mintSecretReader.
type mintSecrets struct {
	client kubernetes.Interface
	ns     string
}

func (m mintSecrets) Get(ctx context.Context, name string, opts metav1.GetOptions) (*corev1.Secret, error) {
	return m.client.CoreV1().Secrets(m.ns).Get(ctx, name, opts)
}

type byoMintAuth struct {
	client    mintSecretReader
	namespace string
	ttl       time.Duration
	clock     func() time.Time

	mu   sync.Mutex
	key  string
	age  time.Time
	once bool
}

func newByoMintAuth(client mintSecretReader, namespace string) *byoMintAuth {
	return &byoMintAuth{client: client, namespace: namespace, ttl: time.Minute, clock: time.Now}
}

func (a *byoMintAuth) current(ctx context.Context) (string, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.once && a.clock().Sub(a.age) < a.ttl {
		return a.key, nil
	}
	sec, err := a.client.Get(ctx, byoMintAuthKeyName, metav1.GetOptions{})
	if err != nil {
		if a.once {
			return a.key, nil // cached last-known-good through outages
		}
		return "", err
	}
	key := string(sec.Data[secrets.RelayMintKeyDataKey])
	if key == "" {
		return "", errors.New("mint key secret has empty mint-key data")
	}
	a.key, a.age, a.once = key, a.clock(), true
	return key, nil
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
	mintAuth := newByoMintAuth(mintSecrets{client: clientset, ns: cfg.namespace}, cfg.namespace)

	factory := informers.NewSharedInformerFactoryWithOptions(clientset, cfg.informerResync,
		informers.WithNamespace(cfg.namespace))
	secretInformer := factory.Core().V1().Secrets().Informer()
	_, _ = secretInformer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    func(obj any) { byoWatchAdd(ctx, obj, keys, cacheStore) },
		UpdateFunc: func(_, obj any) { byoWatchAdd(ctx, obj, keys, cacheStore) },
		DeleteFunc: func(obj any) { byoWatchDelete(ctx, obj, keys, cacheStore) },
	})
	factory.Start(ctx.Done())
	if !cache.WaitForCacheSync(ctx.Done(), secretInformer.HasSynced) {
		return errors.New("byo-router: informer cache sync failed")
	}
	log.Printf("byo-router: informer synced, cached envelopes=%d", cacheStore.Len())

	server, err := buildBYOServer(cfg, keys, cacheStore, redactor, newLazyMinter(mintAuth), defaultRouterClient())
	if err != nil {
		return fmt.Errorf("byo-router: building server: %w", err)
	}

	return serveBYO(ctx, cfg, server)
}

// serveBYO runs the HTTP server until ctx is canceled, then drains
// in-flight streams within the grace bound (#1078: a cap, not a delay).
// The signal→drain WIRING lives here so a test can drive ctx-cancel the
// way SIGTERM does.
func serveBYO(ctx context.Context, cfg byoRunConfig, server *byoServer) error {
	var lc net.ListenConfig
	ln, err := lc.Listen(ctx, "tcp", cfg.listenAddr)
	if err != nil {
		return fmt.Errorf("byo-router: listen %s: %w", cfg.listenAddr, err)
	}
	return serveBYOOn(ctx, cfg, server, ln)
}

// serveBYOOn serves on the given listener until ctx is canceled, then
// drains in-flight streams within the grace bound. The listener is
// injected so the drain wiring is testable against the production path
// (TestDrainWiringThroughServeBYO).
func serveBYOOn(ctx context.Context, cfg byoRunConfig, server *byoServer, ln net.Listener) error {
	httpServer := &http.Server{
		Handler:           server.handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}

	errCh := make(chan error, 1)
	go func() {
		log.Printf("byo-router: listening on %s (namespace=%s, retention=%s)", ln.Addr(), cfg.namespace, cfg.retention)
		errCh <- httpServer.Serve(ln)
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

// buildBYOServer assembles the server from its runtime dependencies.
// Construction-side pins (US-72.1 §4.9 amendment binding): the redactor is
// NON-NIL on the production path and the key manager's redaction hook is
// wired — pinned by TestBuildBYOServerPinsNonNilRedactor.
func buildBYOServer(cfg byoRunConfig, keys *byoKeyManager, cacheStore *byoEnvelopeCache,
	redactor *redact.Redactor, minter byoMintService, client *http.Client) (*byoServer, error) {
	if redactor == nil {
		return nil, errors.New("byo-router: a nil redactor on the production path violates design 0058 §4.9 (fail-open is test-adoption only)")
	}
	return &byoServer{
		cfg: byoServerConfig{
			listenAddr:   cfg.listenAddr,
			maxBodyBytes: cfg.maxBodyBytes,
			maxRespBytes: cfg.maxRespBytes,
		},
		minter:   minter,
		cache:    cacheStore,
		resolve:  resolveDispatcher{keys: keys},
		quota:    newByoWorkspaceQuota(cfg.quotaWindow, cfg.quotaRequests, cfg.quotaBytes),
		redactor: redactor,
		client:   client,
		metrics:  newByoMetrics().withCacheLen(cacheStore.Len),
	}, nil
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

func byoWatchDelete(ctx context.Context, obj any, keys *byoKeyManager, cacheStore *byoEnvelopeCache) {
	// Stale-watch tombstones wrap the deleted object; unwrap or the delete
	// is silently dropped (informers deliver DeletedFinalStateUnknown when
	// the watch bookkeeping outruns the delete confirmation).
	if tombstone, ok := obj.(cache.DeletedFinalStateUnknown); ok {
		obj = tombstone.Obj
	}
	sec, ok := obj.(*corev1.Secret)
	if !ok {
		return
	}
	if sec.Name == byoKeyPairSecretName {
		// Keypair-Secret loss on a RUNNING replica is the DR case: fall
		// back to create-or-adopt regeneration (the losing replica of a
		// simultaneous recovery adopts the winner — same tail as boot). A
		// deleted Secret never re-delivers on resync, so transient API
		// errors get a bounded retry — never a silent strand.
		go func() {
			for attempt := 0; attempt < 3; attempt++ {
				if err := keys.Bootstrap(ctx); err == nil {
					return
				} else {
					log.Printf("byo-router: keypair-secret loss recovery attempt %d failed: %v", attempt+1, err)
				}
				select {
				case <-ctx.Done():
					return
				case <-time.After(time.Duration(attempt+1) * 5 * time.Second):
				}
			}
			// Terminal: loud give-up — resolve stays fail-closed. A pod
			// restart re-enters the same recovery with a FRESH manager
			// (empty highwater): on a rotated lineage it re-bootstraps at
			// generation 1 until a surviving peer's assert-driven DR
			// converges the fleet forward (US-72.3's controller re-seal
			// is the eventual backstop, not code in this PR).
			log.Printf("byo-router: keypair-secret loss recovery EXHAUSTED after 3 attempts; giving up (fail-closed)")
		}()
		return
	}
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
