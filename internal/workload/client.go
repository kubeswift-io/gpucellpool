// Package workload owns access to the INNER (workload) cluster: one cached,
// scoped client per kubeconfig Secret, plus the Node operations the reconciler
// needs.
//
// The cache exists for a specific reason: N pools pointing at one workload
// cluster must share one connection set. Never a controller-runtime manager per
// pool, never a goroutine per cell.
package workload

import (
	"context"
	"errors"
	"fmt"
	"net"
	"sync"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"

	cellsv1alpha1 "github.com/kubeswift-io/gpucellpool/api/v1alpha1"
)

// DefaultKubeconfigKey matches Cluster API's convention, which is also what
// capi-kubeswift publishes.
const DefaultKubeconfigKey = "value"

// ClientCache hands out clients for workload clusters, keyed by the credential
// Secret. An entry is invalidated when the Secret's resourceVersion changes, so
// credential rotation is picked up on the next reconcile with no restart.
type ClientCache struct {
	mu      sync.Mutex
	entries map[string]cacheEntry

	// newClient is injectable so tests do not need a real API server.
	newClient func(*rest.Config) (kubernetes.Interface, error)
}

type cacheEntry struct {
	resourceVersion string
	client          kubernetes.Interface
}

// NewClientCache returns an empty cache.
func NewClientCache() *ClientCache {
	return &ClientCache{
		entries: map[string]cacheEntry{},
		newClient: func(cfg *rest.Config) (kubernetes.Interface, error) {
			return kubernetes.NewForConfig(cfg)
		},
	}
}

// For returns a client for the workload cluster described by secret. key is the
// cache key, normally "<namespace>/<name>" of the Secret.
func (c *ClientCache) For(key string, secret *corev1.Secret, dataKey string) (kubernetes.Interface, error) {
	if dataKey == "" {
		dataKey = DefaultKubeconfigKey
	}
	raw, ok := secret.Data[dataKey]
	if !ok || len(raw) == 0 {
		return nil, fmt.Errorf("secret %s has no %q key holding a kubeconfig", key, dataKey)
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	if e, ok := c.entries[key]; ok && e.resourceVersion == secret.ResourceVersion {
		return e.client, nil
	}

	cfg, err := clientcmd.RESTConfigFromKubeConfig(raw)
	if err != nil {
		return nil, fmt.Errorf("parsing kubeconfig from secret %s: %w", key, err)
	}
	cs, err := c.newClient(cfg)
	if err != nil {
		return nil, fmt.Errorf("building workload client for %s: %w", key, err)
	}

	// Replacing the entry drops the previous client; nothing else holds it.
	c.entries[key] = cacheEntry{resourceVersion: secret.ResourceVersion, client: cs}
	return cs, nil
}

// Forget drops a cached client (pool deleted, or credential known bad).
func (c *ClientCache) Forget(key string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.entries, key)
}

// Len reports how many clusters are cached.
func (c *ClientCache) Len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.entries)
}

// ClassifyError maps a workload-cluster failure onto a condition reason.
//
// The distinction is the whole point: "your credential expired", "you removed my
// Node patch rights" and "the cluster is unreachable" are three different tickets,
// and collapsing them into one reason makes the pool's status useless at 02:00.
func ClassifyError(err error) string {
	switch {
	case err == nil:
		return cellsv1alpha1.ReasonConnected
	case apierrors.IsUnauthorized(err):
		return cellsv1alpha1.ReasonCredentialInvalid
	case apierrors.IsForbidden(err):
		return cellsv1alpha1.ReasonForbidden
	case apierrors.IsTimeout(err), apierrors.IsServerTimeout(err), apierrors.IsServiceUnavailable(err):
		return cellsv1alpha1.ReasonUnreachable
	}
	var netErr net.Error
	if errors.As(err, &netErr) {
		return cellsv1alpha1.ReasonUnreachable
	}
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		return cellsv1alpha1.ReasonUnreachable
	}
	return cellsv1alpha1.ReasonUnreachable
}

// Probe checks the workload cluster answers at all. It is deliberately a cheap,
// read-only call on a resource the observer role always has: a failure here is
// what freezes every destructive path in the reconciler.
func Probe(ctx context.Context, cs kubernetes.Interface) error {
	_, err := cs.Discovery().ServerVersion()
	return err
}
