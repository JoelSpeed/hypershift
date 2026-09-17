// Command example-aws-external-platform is the controller half of the AWS reference
// integration.
//
// One instance serves every External HostedCluster on a management cluster that names this
// integration's API group. That is the topology the contract is designed around: an
// integrator deploys and releases this alongside the HyperShift Operator, and ships fixes
// without waiting for a HyperShift release.
//
//	example-aws-external-platform \
//	  --cloud-controller-manager-image=registry.k8s.io/provider-aws/cloud-controller-manager:v1.32.0
package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/openshift/hypershift/externalplatform/examples/aws/awsprovider"
	"github.com/openshift/hypershift/externalplatform/reconcile"

	hyperv1 "github.com/openshift/hypershift/api/hypershift/v1beta1"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"

	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
)

func main() {
	var (
		cloudControllerManagerImage string
		metricsAddress              string
		leaderElection              bool
	)
	flag.StringVar(&cloudControllerManagerImage, "cloud-controller-manager-image", "",
		"The AWS cloud controller manager image to run in each control plane namespace. Required in practice: this integration declares an external cloud controller manager, so without one no node in any hosted cluster becomes schedulable.")
	flag.StringVar(&metricsAddress, "metrics-bind-address", ":8080", "The address the metrics endpoint binds to.")
	flag.BoolVar(&leaderElection, "leader-elect", true, "Run leader election, so that two replicas do not provision the same cluster twice.")
	flag.Parse()

	ctrl.SetLogger(zap.New(zap.UseDevMode(false)))
	logger := ctrl.Log.WithName("setup")

	if err := run(cloudControllerManagerImage, metricsAddress, leaderElection); err != nil {
		logger.Error(err, "Exiting")
		os.Exit(1)
	}
}

func run(cloudControllerManagerImage, metricsAddress string, leaderElection bool) error {
	scheme := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{
		corev1.AddToScheme,
		appsv1.AddToScheme,
		// For HostedControlPlane and ControlPlaneComponent, which are the only two HyperShift
		// types an integrator touches.
		hyperv1.AddToScheme,
	} {
		if err := add(scheme); err != nil {
			return fmt.Errorf("failed to build the scheme: %w", err)
		}
	}
	// The integration's own type and Cluster API Provider AWS's, both handled as unstructured
	// so that this binary does not vendor either API.
	for _, gvk := range []schema.GroupVersionKind{
		awsprovider.HostedClusterObjectGVK,
		{Group: "infrastructure.cluster.x-k8s.io", Version: "v1beta2", Kind: "AWSCluster"},
	} {
		scheme.AddKnownTypeWithName(gvk, &unstructured.Unstructured{})
		scheme.AddKnownTypeWithName(gvk.GroupVersion().WithKind(gvk.Kind+"List"), &unstructured.UnstructuredList{})
		metav1.AddToGroupVersion(scheme, gvk.GroupVersion())
	}

	manager, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme: scheme,
		// Load-bearing, and the thing an integrator is most likely to leave out: without it
		// the cache tries to watch every Secret on the management cluster and this binary
		// cannot start. See the function's documentation.
		Client:                  reconcile.ClientOptions(),
		Metrics:                 metricsserver.Options{BindAddress: metricsAddress},
		LeaderElection:          leaderElection,
		LeaderElectionID:        "example-aws-external-platform.hypershift.openshift.io",
		LeaderElectionNamespace: os.Getenv("POD_NAMESPACE"),
	})
	if err != nil {
		return fmt.Errorf("failed to build the manager: %w", err)
	}

	// Everything the contract requires of the integrator, other than the provider work
	// itself, is in the reconciler. The provisioner below is the whole integration.
	if err := (&reconcile.Reconciler{
		Provisioner:            &awsprovider.Provisioner{CloudControllerManagerImage: cloudControllerManagerImage},
		HostedClusterObjectGVK: awsprovider.HostedClusterObjectGVK,
	}).SetupWithManager(manager); err != nil {
		return fmt.Errorf("failed to set up the controller: %w", err)
	}

	return manager.Start(ctrl.SetupSignalHandler())
}
