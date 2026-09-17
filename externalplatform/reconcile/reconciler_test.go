package reconcile

import (
	"context"
	"fmt"
	"testing"

	hyperv1 "github.com/openshift/hypershift/api/hypershift/v1beta1"
	"github.com/openshift/hypershift/externalplatform/contract"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/utils/ptr"

	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
)

const (
	testNamespace = "clusters-example"
	testName      = "example"
	testFinalizer = "example.io/external-platform"
)

var testGVK = schema.GroupVersionKind{Group: "example.io", Version: "v1alpha1", Kind: "FooHostedCluster"}

// fakeProvisioner records what it was asked to do and returns what the test told it to.
type fakeProvisioner struct {
	provisionResult   ProvisionResult
	provisionError    error
	deprovisionResult DeprovisionResult
	deprovisionError  error

	provisionCalls   int
	deprovisionCalls int
	sawGuestClient   bool
}

func (p *fakeProvisioner) Platform() (string, hyperv1.ExternalCloudControllerManagerState) {
	return "ExampleCloud", hyperv1.ExternalCloudControllerManager
}

func (p *fakeProvisioner) Provision(_ context.Context, request *Request) (ProvisionResult, error) {
	p.provisionCalls++
	p.sawGuestClient = request.GuestClient != nil
	return p.provisionResult, p.provisionError
}

func (p *fakeProvisioner) Deprovision(_ context.Context, _ *Request) (DeprovisionResult, error) {
	p.deprovisionCalls++
	return p.deprovisionResult, p.deprovisionError
}

func testScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := hyperv1.AddToScheme(scheme); err != nil {
		t.Fatalf("unexpected error building the scheme: %v", err)
	}
	scheme.AddKnownTypeWithName(testGVK, &unstructured.Unstructured{})
	scheme.AddKnownTypeWithName(testGVK.GroupVersion().WithKind(testGVK.Kind+"List"), &unstructured.UnstructuredList{})
	metav1.AddToGroupVersion(scheme, testGVK.GroupVersion())
	return scheme
}

func testHostedClusterObject() *unstructured.Unstructured {
	object := &unstructured.Unstructured{}
	object.SetGroupVersionKind(testGVK)
	object.SetNamespace(testNamespace)
	object.SetName(testName)
	if err := unstructured.SetNestedMap(object.Object, map[string]any{
		"host": "api.example.hypershift.local",
		"port": int64(6443),
	}, "spec", "controlPlaneEndpoint"); err != nil {
		panic(err)
	}
	return object
}

func testHostedControlPlane() *hyperv1.HostedControlPlane {
	return &hyperv1.HostedControlPlane{
		ObjectMeta: metav1.ObjectMeta{Namespace: testNamespace, Name: testName},
	}
}

type fixture struct {
	reconciler  *Reconciler
	provisioner *fakeProvisioner
	client      client.Client
}

func newFixture(t *testing.T, provisioner *fakeProvisioner, objects ...client.Object) *fixture {
	t.Helper()
	// The hosted cluster object has a status subresource, as the contract requires: every
	// field HyperShift reads off it lives under status, and a provider that could write
	// spec would be able to undo HyperShift's own controlPlaneEndpoint.
	hostedClusterObject := &unstructured.Unstructured{}
	hostedClusterObject.SetGroupVersionKind(testGVK)
	c := fake.NewClientBuilder().
		WithScheme(testScheme(t)).
		WithObjects(objects...).
		WithStatusSubresource(hostedClusterObject).
		Build()
	return &fixture{
		reconciler: &Reconciler{
			Client:                 c,
			Provisioner:            provisioner,
			HostedClusterObjectGVK: testGVK,
			FinalizerName:          testFinalizer,
		},
		provisioner: provisioner,
		client:      c,
	}
}

func (f *fixture) reconcile(t *testing.T) ctrl.Result {
	t.Helper()
	result, err := f.reconciler.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: client.ObjectKey{Namespace: testNamespace, Name: testName},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	return result
}

func (f *fixture) read(t *testing.T) *unstructured.Unstructured {
	t.Helper()
	object := &unstructured.Unstructured{}
	object.SetGroupVersionKind(testGVK)
	if err := f.client.Get(context.Background(), client.ObjectKey{Namespace: testNamespace, Name: testName}, object); err != nil {
		t.Fatalf("unexpected error reading the hosted cluster object: %v", err)
	}
	return object
}

func TestReconcileDeclaresThePlatformBeforeReportingReady(t *testing.T) {
	// HyperShift refuses to render the guest Infrastructure until the declaration is
	// present, so a provider that reported ready first would leave the cluster unable to
	// boot a node with nothing obviously wrong.
	f := newFixture(t, &fakeProvisioner{
		provisionResult: ProvisionResult{Done: false, Message: "Creating the load balancer"},
	}, testHostedClusterObject(), testHostedControlPlane())

	f.reconcile(t)

	platform, err := contract.Platform(f.read(t))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if platform == nil {
		t.Fatal("expected the platform to be declared on the very first reconcile")
	}
	if platform.Name != "ExampleCloud" {
		t.Errorf("expected platform name ExampleCloud, got %q", platform.Name)
	}
	if platform.CloudControllerManager.State != hyperv1.ExternalCloudControllerManager {
		t.Errorf("expected an external cloud controller manager, got %q", platform.CloudControllerManager.State)
	}

	condition, err := contract.Ready(f.read(t))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if condition == nil || condition.Status != metav1.ConditionFalse {
		t.Fatalf("expected Ready=False while provisioning, got %v", condition)
	}
	if condition.Message != "Creating the load balancer" {
		t.Errorf("expected the provider's message verbatim, got %q", condition.Message)
	}
}

func TestReconcileTakesTheFinalizerBeforeProvisioning(t *testing.T) {
	// A provider that crashed after creating a load balancer but before taking the
	// finalizer would leak it with nothing left to say so.
	provisioner := &fakeProvisioner{provisionResult: ProvisionResult{Done: true}}
	f := newFixture(t, provisioner, testHostedClusterObject(), testHostedControlPlane())

	f.reconcile(t)

	if !controllerutil.ContainsFinalizer(f.read(t), testFinalizer) {
		t.Error("expected the finalizer to be held")
	}
	if provisioner.provisionCalls != 1 {
		t.Errorf("expected exactly one provision call, got %d", provisioner.provisionCalls)
	}
}

func TestReconcileRecordsTheInfrastructureObject(t *testing.T) {
	f := newFixture(t, &fakeProvisioner{
		provisionResult: ProvisionResult{
			Infrastructure: InfrastructureReference{
				APIGroup: "infrastructure.cluster.x-k8s.io",
				Kind:     "FooCluster",
				Name:     testName,
			},
			// Not done: HyperShift needs the reference as soon as the object exists,
			// because the Cluster it feeds has an immutable infrastructureRef.
			Done: false,
		},
	}, testHostedClusterObject(), testHostedControlPlane())

	f.reconcile(t)

	apiGroup, kind, name, err := contract.Infrastructure(f.read(t))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if apiGroup != "infrastructure.cluster.x-k8s.io" || kind != "FooCluster" || name != testName {
		t.Errorf("expected the infrastructure object to be named, got %s/%s/%s", apiGroup, kind, name)
	}
}

func TestReconcileReportsReady(t *testing.T) {
	f := newFixture(t, &fakeProvisioner{
		provisionResult: ProvisionResult{Done: true, Reason: "AsExpected", Message: "Everything is up"},
	}, testHostedClusterObject(), testHostedControlPlane())

	f.reconcile(t)

	condition, err := contract.Ready(f.read(t))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if condition == nil || condition.Status != metav1.ConditionTrue {
		t.Fatalf("expected Ready=True, got %v", condition)
	}
	if condition.Message != "Everything is up" {
		t.Errorf("expected the provider's message verbatim, got %q", condition.Message)
	}
}

func TestReconcileReportsAProvisioningError(t *testing.T) {
	// A provider stuck in a retry loop has to say so somewhere the cluster's owner can see,
	// not only in the provider's own logs.
	provisioner := &fakeProvisioner{provisionError: fmt.Errorf("the cloud said no")}
	f := newFixture(t, provisioner, testHostedClusterObject(), testHostedControlPlane())

	if _, err := f.reconciler.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: client.ObjectKey{Namespace: testNamespace, Name: testName},
	}); err == nil {
		t.Fatal("expected the error to be returned so the reconcile backs off")
	}

	condition, err := contract.Ready(f.read(t))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if condition == nil || condition.Status != metav1.ConditionFalse {
		t.Fatalf("expected Ready=False, got %v", condition)
	}
	if condition.Message != "the cloud said no" {
		t.Errorf("expected the error as the message, got %q", condition.Message)
	}
}

func TestReconcileDoesNothingWhileTheControlPlaneIsPaused(t *testing.T) {
	// An operator pausing a control plane is debugging it by hand, and provisioning against
	// it would fight whatever they are doing.
	hcp := testHostedControlPlane()
	hcp.Spec.PausedUntil = ptr.To("true")

	provisioner := &fakeProvisioner{provisionResult: ProvisionResult{Done: true}}
	f := newFixture(t, provisioner, testHostedClusterObject(), hcp)

	f.reconcile(t)

	if provisioner.provisionCalls != 0 {
		t.Errorf("expected no provision call, got %d", provisioner.provisionCalls)
	}
	if controllerutil.ContainsFinalizer(f.read(t), testFinalizer) {
		t.Error("expected no finalizer to be taken while paused")
	}
}

func TestReconcileRequeuesWithoutProvisioningWhenTheGuestIsUnreachable(t *testing.T) {
	provisioner := &fakeProvisioner{provisionResult: ProvisionResult{Done: true}}
	f := newFixture(t, provisioner, testHostedClusterObject(), testHostedControlPlane())
	f.reconciler.WithGuestClient = true

	result := f.reconcile(t)

	if result.RequeueAfter == 0 {
		t.Error("expected a requeue while the guest kubeconfig does not exist")
	}
	if provisioner.provisionCalls != 0 {
		t.Errorf("expected no provision call, got %d", provisioner.provisionCalls)
	}
	condition, err := contract.Ready(f.read(t))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if condition == nil || condition.Reason != "WaitingForGuestCluster" {
		t.Errorf("expected the wait to be reported, got %v", condition)
	}
}

func TestReconcileHoldsTheFinalizerUntilDeprovisioningIsDone(t *testing.T) {
	object := testHostedClusterObject()
	object.SetFinalizers([]string{testFinalizer})
	object.SetDeletionTimestamp(ptr.To(metav1.Now()))

	provisioner := &fakeProvisioner{
		deprovisionResult: DeprovisionResult{Done: false, Message: "Waiting for the load balancer to drain"},
	}
	f := newFixture(t, provisioner, object, testHostedControlPlane())

	f.reconcile(t)

	if provisioner.deprovisionCalls != 1 {
		t.Errorf("expected exactly one deprovision call, got %d", provisioner.deprovisionCalls)
	}
	if !controllerutil.ContainsFinalizer(f.read(t), testFinalizer) {
		t.Error("expected the finalizer to be held while teardown is incomplete")
	}
}

func TestReconcileReleasesTheFinalizerWhenDeprovisioningIsDone(t *testing.T) {
	object := testHostedClusterObject()
	object.SetFinalizers([]string{testFinalizer})
	object.SetDeletionTimestamp(ptr.To(metav1.Now()))

	provisioner := &fakeProvisioner{deprovisionResult: DeprovisionResult{Done: true}}
	f := newFixture(t, provisioner, object, testHostedControlPlane())

	f.reconcile(t)

	// The fake client deletes an object once its last finalizer comes off, which is exactly
	// what the real one does and what lets the HostedCluster finish deleting.
	err := f.client.Get(context.Background(), client.ObjectKey{Namespace: testNamespace, Name: testName}, testHostedClusterObject())
	if err == nil {
		t.Error("expected the hosted cluster object to be gone once the finalizer was released")
	}
}

func TestReconcileDoesNotProvisionWhileDeleting(t *testing.T) {
	object := testHostedClusterObject()
	object.SetFinalizers([]string{testFinalizer})
	object.SetDeletionTimestamp(ptr.To(metav1.Now()))

	provisioner := &fakeProvisioner{deprovisionResult: DeprovisionResult{Done: false}}
	f := newFixture(t, provisioner, object, testHostedControlPlane())

	f.reconcile(t)

	if provisioner.provisionCalls != 0 {
		t.Errorf("expected no provision call while deleting, got %d", provisioner.provisionCalls)
	}
}

func TestReconcileIgnoresAnObjectThatIsAlreadyGone(t *testing.T) {
	provisioner := &fakeProvisioner{}
	f := newFixture(t, provisioner, testHostedControlPlane())

	f.reconcile(t)

	if provisioner.provisionCalls != 0 || provisioner.deprovisionCalls != 0 {
		t.Error("expected nothing to be called for an object that does not exist")
	}
}

func TestReconcileIsIdempotent(t *testing.T) {
	provisioner := &fakeProvisioner{provisionResult: ProvisionResult{Done: true}}
	f := newFixture(t, provisioner, testHostedClusterObject(), testHostedControlPlane())

	for i := range 3 {
		if _, err := f.reconciler.Reconcile(context.Background(), ctrl.Request{
			NamespacedName: client.ObjectKey{Namespace: testNamespace, Name: testName},
		}); err != nil {
			t.Fatalf("unexpected error on reconcile %d: %v", i, err)
		}
	}

	object := f.read(t)
	if finalizers := object.GetFinalizers(); len(finalizers) != 1 {
		t.Errorf("expected exactly one finalizer, got %v", finalizers)
	}
	conditions, _, err := unstructured.NestedSlice(object.Object, "status", "conditions")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(conditions) != 1 {
		t.Errorf("expected exactly one condition, got %d", len(conditions))
	}
}

func TestSetupWithManagerRequiresAProvisionerAndAKind(t *testing.T) {
	for _, testCase := range []struct {
		name       string
		reconciler *Reconciler
	}{
		{name: "no provisioner", reconciler: &Reconciler{HostedClusterObjectGVK: testGVK}},
		{name: "no kind", reconciler: &Reconciler{Provisioner: &fakeProvisioner{}}},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			testCase.reconciler.Client = fake.NewClientBuilder().WithScheme(testScheme(t)).Build()
			if err := testCase.reconciler.SetupWithManager(nil); err == nil {
				t.Error("expected an error, got none")
			}
		})
	}
}

func TestFinalizerDefaultsToTheIntegratorsOwnGroup(t *testing.T) {
	reconciler := &Reconciler{
		Client:                 fake.NewClientBuilder().WithScheme(testScheme(t)).Build(),
		Provisioner:            &fakeProvisioner{},
		HostedClusterObjectGVK: testGVK,
	}
	if err := reconciler.applyDefaults(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if expected := "example.io/external-platform"; reconciler.FinalizerName != expected {
		t.Errorf("expected the finalizer to default to %q, got %q", expected, reconciler.FinalizerName)
	}
}
