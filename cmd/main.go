package main

import (
	"flag"

	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	_ "k8s.io/client-go/plugin/pkg/client/auth"
	"k8s.io/klog/v2"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	inferencev1alpha1 "github.com/llm-d/llm-d-resiliency-operator/apis/inference/v1alpha1"
	"github.com/llm-d/llm-d-resiliency-operator/internal/controller/enginefault"
	"github.com/llm-d/llm-d-resiliency-operator/internal/controller/recoveryrequest"
)

var scheme = runtime.NewScheme()

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(inferencev1alpha1.AddToScheme(scheme))
}

func main() {
	klog.InitFlags(nil)
	ctrl.SetLogger(klog.Background())
	defer klog.Flush()

	var metricsAddr string
	var enableLeaderElection bool
	var probeAddr string
	var podNamespace string
	var podLabelKey string
	var engineOptions enginefault.Options
	engineOptions.BindFlags(flag.CommandLine)
	flag.StringVar(&metricsAddr, "metrics-bind-address", ":8080", "The address the metric endpoint binds to.")
	flag.StringVar(&probeAddr, "health-probe-bind-address", ":8081", "The address the probe endpoint binds to.")
	flag.BoolVar(&enableLeaderElection, "leader-elect", false,
		"Enable leader election for controller manager. "+
			"Enabling this will ensure only one active controller manager.")
	flag.StringVar(&podNamespace, "pod-namespace", "default", "The namespace of the pod to label.")
	flag.StringVar(&podLabelKey, "pod-label-key", "llm-d.ai/inference-serving", "The label key to match the pod.")
	flag.Parse()
	if engineOptions.Enabled && !enableLeaderElection {
		klog.Fatal("vLLM FT requires --leader-elect")
	}

	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme: scheme,
		Metrics: metricsserver.Options{
			BindAddress: metricsAddr,
		},
		HealthProbeBindAddress: probeAddr,
		LeaderElection:         enableLeaderElection,
		LeaderElectionID:       "llm-d-resiliency-operator-leader",
	})
	if err != nil {
		klog.Fatalf("unable to start manager: %v", err)
	}

	if err = (&recoveryrequest.Reconciler{
		Client:       mgr.GetClient(),
		Scheme:       mgr.GetScheme(),
		PodNamespace: podNamespace,
		PodLabelKey:  podLabelKey,
	}).SetupWithManager(mgr); err != nil {
		klog.Fatalf("unable to create controller: %v", err)
	}

	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		klog.Fatalf("unable to set up health check: %v", err)
	}
	if err := engineOptions.Setup(mgr); err != nil {
		klog.Fatalf("unable to configure vLLM FT: %v", err)
	}
	if err := mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		klog.Fatalf("unable to set up ready check: %v", err)
	}

	klog.Info("starting manager")
	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		klog.Fatalf("problem running manager: %v", err)
	}
}
