package v1alpha1

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	"sigs.k8s.io/controller-runtime/pkg/webhook"

	cellsv1alpha1 "github.com/kubeswift-io/gpucellpool/api/v1alpha1"
)

// This suite exercises the webhook the way a cluster does: a real apiserver, the
// generated ValidatingWebhookConfiguration, real serving certificates, and
// rejections coming back as apiserver errors on ordinary client calls.
//
// The validation LOGIC is unit-tested in gpucellpool_validator_test.go. What is
// only provable here is the wiring: that the marker's path matches the served
// route, that the certificate plumbing works, and that admission actually blocks.
var (
	webhookEnv    *envtest.Environment
	webhookClient client.Client
	webhookScheme = runtime.NewScheme()
	cancelMgr     context.CancelFunc
)

func TestMain(m *testing.M) {
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		fmt.Fprintln(os.Stderr, "KUBEBUILDER_ASSETS is unset; skipping the webhook envtest suite (run: make test)")
		os.Exit(m.Run())
	}
	if err := setup(); err != nil {
		fmt.Fprintf(os.Stderr, "webhook suite setup: %v\n", err)
		teardown()
		os.Exit(1)
	}
	code := m.Run()
	teardown()
	os.Exit(code)
}

func setup() error {
	if err := cellsv1alpha1.AddToScheme(webhookScheme); err != nil {
		return err
	}
	if err := corev1.AddToScheme(webhookScheme); err != nil {
		return err
	}

	root, err := filepath.Abs("../../..")
	if err != nil {
		return err
	}

	webhookEnv = &envtest.Environment{
		CRDDirectoryPaths:     []string{filepath.Join(root, "config", "crd", "bases")},
		ErrorIfCRDPathMissing: true,
		WebhookInstallOptions: envtest.WebhookInstallOptions{
			// The generated manifest, so a marker/path mismatch fails here.
			Paths: []string{filepath.Join(root, "config", "webhook")},
		},
	}
	cfg, err := webhookEnv.Start()
	if err != nil {
		return err
	}

	webhookClient, err = client.New(cfg, client.Options{Scheme: webhookScheme})
	if err != nil {
		return err
	}

	opts := webhookEnv.WebhookInstallOptions
	mgr, err := ctrl.NewManager(cfg, ctrl.Options{
		Scheme:  webhookScheme,
		Metrics: metricsserver.Options{BindAddress: "0"},
		WebhookServer: webhook.NewServer(webhook.Options{
			Host:    opts.LocalServingHost,
			Port:    opts.LocalServingPort,
			CertDir: opts.LocalServingCertDir,
		}),
	})
	if err != nil {
		return err
	}
	if err := SetupWebhookWithManager(mgr); err != nil {
		return err
	}

	var ctx context.Context
	ctx, cancelMgr = context.WithCancel(context.Background())
	go func() {
		if err := mgr.Start(ctx); err != nil {
			fmt.Fprintf(os.Stderr, "manager: %v\n", err)
		}
	}()

	// Wait for the serving endpoint, or admission fails for the wrong reason.
	addr := net.JoinHostPort(opts.LocalServingHost, fmt.Sprint(opts.LocalServingPort))
	deadline := time.Now().Add(30 * time.Second)
	for {
		conn, dialErr := tls.Dial("tcp", addr, &tls.Config{InsecureSkipVerify: true}) //nolint:gosec // test dial
		if dialErr == nil {
			_ = conn.Close()
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("webhook server never became dialable at %s: %w", addr, dialErr)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

func teardown() {
	if cancelMgr != nil {
		cancelMgr()
	}
	if webhookEnv != nil {
		_ = webhookEnv.Stop()
	}
}

func requireWebhookEnv(t *testing.T) {
	t.Helper()
	if webhookClient == nil {
		t.Skip("webhook envtest not available (KUBEBUILDER_ASSETS unset)")
	}
}

func poolInNamespace(t *testing.T, name string) *cellsv1alpha1.GPUCellPool {
	t.Helper()
	name = strings.ToLower(name)
	ns := "wh-" + name
	err := webhookClient.Create(context.Background(), &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})
	// Do not swallow this: an invalid namespace name makes every Create below
	// fail with "namespace not found", which reads exactly like a webhook that
	// did not fire.
	if err != nil && !apierrors.IsAlreadyExists(err) {
		t.Fatalf("create namespace %q: %v", ns, err)
	}
	p := validPool()
	p.Namespace = ns
	p.Name = name
	return p
}

func TestAdmissionAcceptsAValidPool(t *testing.T) {
	requireWebhookEnv(t)
	p := poolInNamespace(t, "valid")
	if err := webhookClient.Create(context.Background(), p); err != nil {
		t.Fatalf("apiserver rejected a valid pool: %v", err)
	}
	t.Cleanup(func() { _ = webhookClient.Delete(context.Background(), p) })
}

func TestAdmissionRejectsThroughTheRealPath(t *testing.T) {
	requireWebhookEnv(t)
	cases := map[string]struct {
		mutate func(*cellsv1alpha1.GPUCellPool)
		want   string
	}{
		// The security-relevant one: the guestTemplate denylist is what stops a
		// pool asking for things a privileged launcher pod would then do.
		"denied kernelRef": {
			func(p *cellsv1alpha1.GPUCellPool) {
				p.Spec.Cell.GuestTemplate = runtime.RawExtension{Raw: []byte(
					`{"guestClassRef":{"name":"c"},"imageRef":{"name":"i"},"kernelRef":{"name":"k"}}`)}
			}, "disk boot"},
		"operator-owned field": {
			func(p *cellsv1alpha1.GPUCellPool) {
				p.Spec.Cell.GuestTemplate = runtime.RawExtension{Raw: []byte(
					`{"guestClassRef":{"name":"c"},"imageRef":{"name":"i"},"nodeName":"boba"}`)}
			}, "set by the operator"},
		"two claim references": {
			func(p *cellsv1alpha1.GPUCellPool) { p.Spec.Cell.GPU.DRA.ResourceClaimName = "shared" },
			"exactly one"},
		"missing join secret": {
			func(p *cellsv1alpha1.GPUCellPool) { p.Spec.Bootstrap.JoinSecretRef = nil },
			"joinSecretRef"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			p := poolInNamespace(t, strings.ReplaceAll(name, " ", "-"))
			c.mutate(p)
			err := webhookClient.Create(context.Background(), p)
			if err == nil {
				_ = webhookClient.Delete(context.Background(), p)
				t.Fatalf("apiserver accepted an invalid pool (expected %q)", c.want)
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Errorf("apiserver error does not carry our message %q: %v", c.want, err)
			}
		})
	}
}

func TestAdmissionBlocksScaleDownWhenTheWorkloadClusterIsUnreachable(t *testing.T) {
	requireWebhookEnv(t)
	ctx := context.Background()

	p := poolInNamespace(t, "scaledown")
	p.Spec.Replicas = 3
	if err := webhookClient.Create(ctx, p); err != nil {
		t.Fatalf("create: %v", err)
	}
	t.Cleanup(func() { _ = webhookClient.Delete(ctx, p) })

	// Record the state the rule keys on, through the status subresource.
	p.Status.Conditions = []metav1.Condition{{
		Type: cellsv1alpha1.ConditionWorkloadClusterReachable, Status: metav1.ConditionFalse,
		Reason: cellsv1alpha1.ReasonUnreachable, LastTransitionTime: metav1.Now(),
		ObservedGeneration: p.Generation,
	}}
	if err := webhookClient.Status().Update(ctx, p); err != nil {
		t.Fatalf("status update: %v", err)
	}

	fresh := &cellsv1alpha1.GPUCellPool{}
	if err := webhookClient.Get(ctx, types.NamespacedName{Namespace: p.Namespace, Name: p.Name}, fresh); err != nil {
		t.Fatalf("get: %v", err)
	}

	// Down destroys cells we cannot drain: rejected.
	fresh.Spec.Replicas = 1
	if err := webhookClient.Update(ctx, fresh); err == nil {
		t.Error("apiserver allowed a scale-down while the workload cluster was unreachable")
	} else if !strings.Contains(err.Error(), "cannot scale down") {
		t.Errorf("unexpected rejection message: %v", err)
	}

	// Up destroys nothing: allowed even while unreachable.
	if err := webhookClient.Get(ctx, types.NamespacedName{Namespace: p.Namespace, Name: p.Name}, fresh); err != nil {
		t.Fatalf("get: %v", err)
	}
	fresh.Spec.Replicas = 5
	if err := webhookClient.Update(ctx, fresh); err != nil {
		t.Errorf("apiserver blocked a scale-up: %v", err)
	}
}

func TestAdmissionAllowsDeletion(t *testing.T) {
	requireWebhookEnv(t)
	ctx := context.Background()
	p := poolInNamespace(t, "deletable")
	if err := webhookClient.Create(ctx, p); err != nil {
		t.Fatalf("create: %v", err)
	}
	// Teardown safety is the reconciler's drain gate, not admission's business.
	if err := webhookClient.Delete(ctx, p); err != nil {
		t.Errorf("admission blocked a deletion: %v", err)
	}
}
