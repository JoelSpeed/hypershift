//go:build envtest

// Package integration runs the conformance suite against the AWS example, on a real API
// server.
//
// This is the only test in the repository that exercises all three pieces at once: the
// contract library, the conformance suite, and an integration written against them. The unit
// tests either side of it prove that each behaves as intended in isolation, which is exactly
// the property that skew destroys.
//
// It is also the worked answer to "how do I test my integration", so it is written the way an
// integrator's own CI should be: start the provider, stand in for the cloud, call
// conformance.Run, assert nothing.
//
//	KUBEBUILDER_ASSETS=$(setup-envtest use -p path 1.35.0) go test -tags envtest ./examples/aws/integration/...
package integration

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/openshift/hypershift/externalplatform/conformance"
	"github.com/openshift/hypershift/externalplatform/examples/aws/awsprovider"
	"github.com/openshift/hypershift/externalplatform/reconcile"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/apimachinery/pkg/util/yaml"

	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
)

// capaFinalizer stands in for the finalizer Cluster API Provider AWS holds while it owns
// anything in the account. Without it the AWSCluster vanishes the instant it is deleted, and
// the test would not prove that teardown waits.
const capaFinalizer = "awscluster.infrastructure.cluster.x-k8s.io/test"

// crdPaths are the custom resource definitions the API server needs: the integration's own,
// a stand-in for Cluster API Provider AWS's, and HyperShift's two.
//
// The HostedControlPlane definition is the TechPreviewNoUpgrade one, because that is where
// platform: External is served from while the feature gate is TechPreview. Reading them out
// of the tree rather than copying them is deliberate: a copy would go stale silently, and a
// stale HostedControlPlane definition is exactly the skew this test exists to catch.
var crdPaths = []string{
	"../manifests/01-crds.yaml",
	"testdata/awscluster-crd.yaml",
	"../../../../cmd/install/assets/crds/hypershift-operator/zz_generated.crd-manifests/hostedcontrolplanes-Hypershift-TechPreviewNoUpgrade.crd.yaml",
	"../../../../cmd/install/assets/crds/hypershift-operator/zz_generated.crd-manifests/controlplanecomponents.crd.yaml",
}

func TestConformance(t *testing.T) {
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		t.Skip("KUBEBUILDER_ASSETS is unset; run `setup-envtest use -p path` and export it")
	}
	logf.SetLogger(zap.New(zap.UseDevMode(true), zap.WriteTo(os.Stderr)))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	crds, err := readCRDs()
	if err != nil {
		t.Fatalf("unexpected error reading the custom resource definitions: %v", err)
	}

	environment := &envtest.Environment{CRDs: crds}
	config, err := environment.Start()
	if err != nil {
		t.Fatalf("unexpected error starting the API server: %v", err)
	}
	defer func() {
		if err := environment.Stop(); err != nil {
			t.Errorf("unexpected error stopping the API server: %v", err)
		}
	}()

	scheme := runtime.NewScheme()
	if err := awsprovider.AddToScheme(scheme); err != nil {
		t.Fatalf("unexpected error building the scheme: %v", err)
	}
	// Not something the provider needs; the conformance suite reads the integrator's custom
	// resource definitions to check the contract version label and the status subresource.
	if err := apiextensionsv1.AddToScheme(scheme); err != nil {
		t.Fatalf("unexpected error building the scheme: %v", err)
	}

	manager, err := ctrl.NewManager(config, ctrl.Options{
		Scheme:  scheme,
		Client:  reconcile.ClientOptions(),
		Metrics: metricsserver.Options{BindAddress: "0"},
	})
	if err != nil {
		t.Fatalf("unexpected error building the manager: %v", err)
	}

	// The real provisioner, wired exactly as the binary wires it. No cloud controller manager
	// image, because a Deployment that no kubelet will ever run would sit there not rolling
	// out; the unit tests cover that half.
	if err := (&reconcile.Reconciler{
		Provisioner:            &awsprovider.Provisioner{},
		HostedClusterObjectGVK: awsprovider.HostedClusterObjectGVK,
	}).SetupWithManager(manager); err != nil {
		t.Fatalf("unexpected error setting up the reconciler: %v", err)
	}

	managerDone := make(chan error, 1)
	go func() { managerDone <- manager.Start(ctx) }()
	defer func() {
		cancel()
		if err := <-managerDone; err != nil {
			t.Errorf("the manager exited with an error: %v", err)
		}
	}()

	if !manager.GetCache().WaitForCacheSync(ctx) {
		t.Fatal("the manager's cache never synced")
	}

	c, err := client.New(config, client.Options{Scheme: scheme})
	if err != nil {
		t.Fatalf("unexpected error building a client: %v", err)
	}

	stopAWS := stubCloudProviderAWS(ctx, t, c)
	defer stopAWS()

	conformance.Run(t, ctx, conformance.Options{
		Client:           c,
		APIGroup:         awsprovider.APIGroup,
		TemplateResource: awsprovider.TemplateResource,
		Spec: map[string]any{
			"region":    "eu-west-1",
			"vpcID":     "vpc-0123456789abcdef0",
			"subnetIDs": []any{"subnet-0123456789abcdef0"},
		},
		// Nothing here waits on a cloud, so the suite's minute-scale defaults would only
		// make a failure slow to report.
		Timeout:   90 * time.Second,
		Interval:  200 * time.Millisecond,
		SettleFor: 5 * time.Second,
	})
}

// stubCloudProviderAWS plays Cluster API Provider AWS: it takes a finalizer on every
// AWSCluster, reports it provisioned, and releases the finalizer when it is deleted.
//
// That is the entire surface the example depends on, which is the point. The provisioner is
// not tested against AWS here; it is tested against the shape of a Cluster API infrastructure
// provider, and that shape is four lines.
func stubCloudProviderAWS(ctx context.Context, t *testing.T, c client.Client) func() {
	t.Helper()

	// Its own context, so that stopping the stub does not depend on the test's having been
	// cancelled first. The defer that stops it runs before the one that cancels the test.
	ctx, cancel := context.WithCancel(ctx)

	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		_ = wait.PollUntilContextCancel(ctx, 100*time.Millisecond, true, func(ctx context.Context) (bool, error) {
			list := &unstructured.UnstructuredList{}
			list.SetAPIVersion("infrastructure.cluster.x-k8s.io/v1beta2")
			list.SetKind("AWSClusterList")
			if err := c.List(ctx, list); err != nil {
				t.Logf("stub provider failed to list AWSClusters: %v", err)
				return false, nil
			}
			for i := range list.Items {
				if err := reconcileStubAWSCluster(ctx, c, &list.Items[i]); err != nil && !apierrors.IsConflict(err) && !apierrors.IsNotFound(err) {
					t.Logf("stub provider failed on %s: %v", list.Items[i].GetName(), err)
				}
			}
			// Never done: the stub runs until the context is cancelled.
			return false, nil
		})
	}()

	return func() {
		cancel()
		<-stopped
	}
}

func reconcileStubAWSCluster(ctx context.Context, c client.Client, awsCluster *unstructured.Unstructured) error {
	if !awsCluster.GetDeletionTimestamp().IsZero() {
		finalizers := []string{}
		for _, finalizer := range awsCluster.GetFinalizers() {
			if finalizer != capaFinalizer {
				finalizers = append(finalizers, finalizer)
			}
		}
		if len(finalizers) == len(awsCluster.GetFinalizers()) {
			return nil
		}
		awsCluster.SetFinalizers(finalizers)
		return c.Update(ctx, awsCluster)
	}

	if !hasFinalizer(awsCluster) {
		awsCluster.SetFinalizers(append(awsCluster.GetFinalizers(), capaFinalizer))
		return c.Update(ctx, awsCluster)
	}

	if provisioned, _, _ := unstructured.NestedBool(awsCluster.Object, "status", "initialization", "provisioned"); provisioned {
		return nil
	}
	if err := unstructured.SetNestedField(awsCluster.Object, true, "status", "initialization", "provisioned"); err != nil {
		return err
	}
	return c.Status().Update(ctx, awsCluster)
}

func hasFinalizer(awsCluster *unstructured.Unstructured) bool {
	for _, finalizer := range awsCluster.GetFinalizers() {
		if finalizer == capaFinalizer {
			return true
		}
	}
	return false
}

func readCRDs() ([]*apiextensionsv1.CustomResourceDefinition, error) {
	var crds []*apiextensionsv1.CustomResourceDefinition
	for _, path := range crdPaths {
		file, err := os.Open(filepath.Clean(path))
		if err != nil {
			return nil, fmt.Errorf("failed to open %s: %w", path, err)
		}
		decoder := yaml.NewYAMLOrJSONDecoder(file, 4096)
		for {
			crd := &apiextensionsv1.CustomResourceDefinition{}
			if err := decoder.Decode(crd); err != nil {
				break
			}
			if crd.Name == "" {
				continue
			}
			crds = append(crds, crd)
		}
		if err := file.Close(); err != nil {
			return nil, err
		}
	}
	return crds, nil
}
