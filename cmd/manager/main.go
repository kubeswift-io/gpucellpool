// Command manager runs the gpucellpool controller.
//
// It reconciles GPUCellPool objects in the infrastructure ("outer") cluster and
// reads the workload ("inner") cluster through per-pool scoped clients. It never
// imports KubeSwift Go packages: SwiftGuest and SwiftSeedProfile are reached with
// unstructured clients and hard-coded GroupVersionKinds.
package main

import (
	"context"
	"crypto/tls"
	"flag"
	"fmt"
	"os"
	"time"

	apiextensionsclient "k8s.io/apiextensions-apiserver/pkg/client/clientset/clientset"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	cellsv1alpha1 "github.com/kubeswift-io/gpucellpool/api/v1alpha1"
	"github.com/kubeswift-io/gpucellpool/internal/controller"
	"github.com/kubeswift-io/gpucellpool/internal/crdcheck"
	webhookv1alpha1 "github.com/kubeswift-io/gpucellpool/internal/webhook/v1alpha1"
	"github.com/kubeswift-io/gpucellpool/internal/workload"
)

var (
	scheme   = runtime.NewScheme()
	setupLog = ctrl.Log.WithName("setup")
)

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(cellsv1alpha1.AddToScheme(scheme))
}

func main() {
	var metricsAddr, probeAddr string
	var enableLeaderElection, secureMetrics, enableWebhooks bool
	flag.StringVar(&metricsAddr, "metrics-bind-address", "0", "Metrics endpoint address; 0 disables it.")
	flag.StringVar(&probeAddr, "health-probe-bind-address", ":8081", "Health probe endpoint address.")
	flag.BoolVar(&enableLeaderElection, "leader-elect", false, "Enable leader election for controller manager.")
	flag.BoolVar(&secureMetrics, "metrics-secure", true, "Serve metrics over HTTPS.")
	flag.BoolVar(&enableWebhooks, "enable-webhooks", true,
		"Serve the validating webhook. Requires serving certs; disable for local runs against a cluster with no webhook configuration.")
	opts := zap.Options{Development: false}
	opts.BindFlags(flag.CommandLine)
	flag.Parse()

	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))

	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme: scheme,
		Metrics: metricsserver.Options{
			BindAddress:   metricsAddr,
			SecureServing: secureMetrics,
			TLSOpts: []func(*tls.Config){func(c *tls.Config) {
				c.MinVersion = tls.VersionTLS12
			}},
		},
		HealthProbeBindAddress: probeAddr,
		LeaderElection:         enableLeaderElection,
		LeaderElectionID:       "gpucellpool.cells.kubeswift.io",
	})
	if err != nil {
		setupLog.Error(err, "unable to create manager")
		os.Exit(1)
	}

	if err := (&controller.GPUCellPoolReconciler{
		Client:   mgr.GetClient(),
		Scheme:   mgr.GetScheme(),
		Recorder: mgr.GetEventRecorder("gpucellpool"),
		Clients:  workload.NewClientCache(),
		Clock:    time.Now,
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to set up the GPUCellPool controller")
		os.Exit(1)
	}

	if enableWebhooks {
		// The guestTemplate denylist is a security control (a cell's launcher pod
		// is privileged in the infrastructure cluster), so the webhook fails
		// closed and is on by default.
		if err := webhookv1alpha1.SetupWebhookWithManager(mgr); err != nil {
			setupLog.Error(err, "unable to set up the GPUCellPool webhook")
			os.Exit(1)
		}
	}

	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		setupLog.Error(err, "unable to set up health check")
		os.Exit(1)
	}
	if err := mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		setupLog.Error(err, "unable to set up ready check")
		os.Exit(1)
	}

	// Say so if the cluster is serving an older CRD than this binary was built
	// against. `helm upgrade` never updates a chart's crds/, and the apiserver then
	// silently drops every field the old schema lacks — the operator writes them,
	// they vanish, and nothing reports a problem. Not fatal: the rest of the
	// operator still works, and refusing to start would be a worse answer than
	// naming the gap.
	if err := reportCRDDrift(context.Background(), mgr.GetConfig()); err != nil {
		setupLog.Info("could not compare the served CRD schema", "error", err.Error())
	}

	setupLog.Info("starting manager")
	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		setupLog.Error(err, "problem running manager")
		os.Exit(1)
	}
}

func reportCRDDrift(ctx context.Context, cfg *rest.Config) error {
	cs, err := apiextensionsclient.NewForConfig(cfg)
	if err != nil {
		return err
	}
	results, err := crdcheck.Verify(ctx, cs.ApiextensionsV1().CustomResourceDefinitions())
	if err != nil {
		return err
	}
	for _, r := range results {
		switch {
		case !r.Checked:
			// At default verbosity, not V(1): a check that quietly does not run is
			// the same silent failure it exists to catch. On an upgraded release the
			// commonest cause is the operator's ClusterRole predating the
			// apiextensions grant, which reads as Forbidden here.
			setupLog.Info("CRD schema NOT compared — a stale schema would go unnoticed",
				"crd", r.Name, "reason", r.Err.Error(), "needs", "apiextensions.k8s.io/customresourcedefinitions: get")
		case len(r.Missing) > 0:
			// A real error value, not nil: a nil error logs a stacktrace pointing at
			// this function, which reads as a crash rather than the configuration
			// problem it is.
			setupLog.Error(fmt.Errorf("%d field(s) dropped by a stale CRD", len(r.Missing)),
				"the cluster is serving an OLDER CRD than this operator was built against; "+
					"the apiserver will SILENTLY DROP the fields below, so features that depend on them "+
					"will appear to be accepted and then do nothing. helm upgrade does not update CRDs — apply it yourself",
				"crd", r.Name, "missingFields", r.Missing, "fix", crdcheck.FixCommand(r.File))
		default:
			setupLog.Info("CRD schema matches this build", "crd", r.Name)
		}
	}
	return nil
}
