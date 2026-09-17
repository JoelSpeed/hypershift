package component

import (
	"context"
	"testing"

	hyperv1 "github.com/openshift/hypershift/api/hypershift/v1beta1"

	configv1 "github.com/openshift/api/config/v1"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/utils/ptr"

	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

const (
	testNamespace = "clusters-example"
	testVersion   = "4.21.0"
)

func testScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	scheme := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{
		hyperv1.AddToScheme,
		appsv1.AddToScheme,
		corev1.AddToScheme,
	} {
		if err := add(scheme); err != nil {
			t.Fatalf("unexpected error building the scheme: %v", err)
		}
	}
	return scheme
}

func testHostedControlPlane() *hyperv1.HostedControlPlane {
	hcp := &hyperv1.HostedControlPlane{
		ObjectMeta: metav1.ObjectMeta{
			Namespace:  testNamespace,
			Name:       "example",
			UID:        "hcp-uid",
			Generation: 7,
		},
	}
	hcp.Status.ControlPlaneVersion.Desired = configv1.Release{Version: testVersion}
	return hcp
}

// rolledOutDeployment is a Deployment whose status says the rollout finished: every replica
// counter agrees with the spec and the generation has been observed.
func rolledOutDeployment() *appsv1.Deployment {
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Namespace:  testNamespace,
			Name:       "example-cloud-controller-manager",
			Generation: 3,
		},
		Spec: appsv1.DeploymentSpec{Replicas: ptr.To[int32](2)},
		Status: appsv1.DeploymentStatus{
			ObservedGeneration: 3,
			Replicas:           2,
			ReadyReplicas:      2,
			AvailableReplicas:  2,
			UpdatedReplicas:    2,
			Conditions: []appsv1.DeploymentCondition{{
				Type:   appsv1.DeploymentAvailable,
				Status: corev1.ConditionTrue,
			}},
		},
	}
}

func readComponent(t *testing.T, c client.Client, name string) *hyperv1.ControlPlaneComponent {
	t.Helper()
	component := &hyperv1.ControlPlaneComponent{}
	if err := c.Get(context.Background(), client.ObjectKey{Namespace: testNamespace, Name: name}, component); err != nil {
		t.Fatalf("unexpected error reading the control plane component: %v", err)
	}
	return component
}

func TestReport(t *testing.T) {
	hcp := testHostedControlPlane()
	deployment := rolledOutDeployment()
	c := fake.NewClientBuilder().
		WithScheme(testScheme(t)).
		WithObjects(hcp, deployment).
		WithStatusSubresource(&hyperv1.ControlPlaneComponent{}).
		Build()

	if err := Report(context.Background(), c, hcp, deployment); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	component := readComponent(t, c, deployment.Name)
	if !meta.IsStatusConditionTrue(component.Status.Conditions, string(hyperv1.ControlPlaneComponentAvailable)) {
		t.Error("expected Available=True")
	}
	if !meta.IsStatusConditionTrue(component.Status.Conditions, string(hyperv1.ControlPlaneComponentRolloutComplete)) {
		t.Error("expected RolloutComplete=True")
	}
	if component.Status.Version != testVersion {
		t.Errorf("expected version %q, got %q", testVersion, component.Status.Version)
	}
	if component.Status.ObservedGeneration != hcp.Generation {
		t.Errorf("expected observedGeneration %d, got %d", hcp.Generation, component.Status.ObservedGeneration)
	}
	if len(component.Status.Resources) != 1 || component.Status.Resources[0].Name != deployment.Name {
		t.Errorf("expected the deployment to be listed as a resource, got %v", component.Status.Resources)
	}
}

func TestReportOwnsTheComponentFromTheHostedControlPlane(t *testing.T) {
	// Nothing else garbage-collects a ControlPlaneComponent, and one that outlives its
	// control plane holds the namespace open with nobody reconciling it.
	hcp := testHostedControlPlane()
	deployment := rolledOutDeployment()
	c := fake.NewClientBuilder().
		WithScheme(testScheme(t)).
		WithObjects(hcp, deployment).
		WithStatusSubresource(&hyperv1.ControlPlaneComponent{}).
		Build()

	if err := Report(context.Background(), c, hcp, deployment); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	owners := readComponent(t, c, deployment.Name).OwnerReferences
	if len(owners) != 1 {
		t.Fatalf("expected exactly one owner reference, got %v", owners)
	}
	if owners[0].Kind != "HostedControlPlane" || owners[0].Name != hcp.Name || owners[0].UID != hcp.UID {
		t.Errorf("expected the hosted control plane to own the component, got %v", owners[0])
	}
}

func TestReportWithholdsTheVersionUntilTheRolloutCompletes(t *testing.T) {
	// HyperShift calls a control plane upgrade complete once every component reports the
	// target version, so reporting it mid-rollout says the upgrade finished while the old
	// pods are still serving.
	hcp := testHostedControlPlane()
	deployment := rolledOutDeployment()
	deployment.Status.UpdatedReplicas = 1
	deployment.Status.UnavailableReplicas = 1

	c := fake.NewClientBuilder().
		WithScheme(testScheme(t)).
		WithObjects(hcp, deployment).
		WithStatusSubresource(&hyperv1.ControlPlaneComponent{}).
		Build()

	if err := Report(context.Background(), c, hcp, deployment); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	component := readComponent(t, c, deployment.Name)
	if meta.IsStatusConditionTrue(component.Status.Conditions, string(hyperv1.ControlPlaneComponentRolloutComplete)) {
		t.Error("expected RolloutComplete=False while replicas are still updating")
	}
	if component.Status.Version != "" {
		t.Errorf("expected no version while the rollout is incomplete, got %q", component.Status.Version)
	}
}

func TestReportHoldsTheLastVersionRatherThanBlankingIt(t *testing.T) {
	// A blank version on any component in the namespace blocks version rollout completion
	// for the whole cluster, so an upgrade that starts a new rollout must not clear the
	// version the component was previously at.
	hcp := testHostedControlPlane()
	deployment := rolledOutDeployment()
	c := fake.NewClientBuilder().
		WithScheme(testScheme(t)).
		WithObjects(hcp, deployment).
		WithStatusSubresource(&hyperv1.ControlPlaneComponent{}).
		Build()

	if err := Report(context.Background(), c, hcp, deployment); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	hcp.Status.ControlPlaneVersion.Desired = configv1.Release{Version: "4.22.0"}
	deployment.Status.UpdatedReplicas = 1
	deployment.Status.UnavailableReplicas = 1
	if err := Report(context.Background(), c, hcp, deployment); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if version := readComponent(t, c, deployment.Name).Status.Version; version != testVersion {
		t.Errorf("expected the previous version %q to be held during the rollout, got %q", testVersion, version)
	}
}

func TestReportIsNotAvailableWhenTheDeploymentIsNot(t *testing.T) {
	hcp := testHostedControlPlane()
	deployment := rolledOutDeployment()
	deployment.Status.Conditions = []appsv1.DeploymentCondition{{
		Type:    appsv1.DeploymentAvailable,
		Status:  corev1.ConditionFalse,
		Message: "Deployment does not have minimum availability",
	}}

	c := fake.NewClientBuilder().
		WithScheme(testScheme(t)).
		WithObjects(hcp, deployment).
		WithStatusSubresource(&hyperv1.ControlPlaneComponent{}).
		Build()

	if err := Report(context.Background(), c, hcp, deployment); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	condition := meta.FindStatusCondition(readComponent(t, c, deployment.Name).Status.Conditions, string(hyperv1.ControlPlaneComponentAvailable))
	if condition == nil || condition.Status != metav1.ConditionFalse {
		t.Fatalf("expected Available=False, got %v", condition)
	}
	if condition.Message == "" {
		t.Error("expected the deployment's own message to be carried through")
	}
}

func TestReportFailsWhenTheControlPlaneHasNoVersionYet(t *testing.T) {
	// Early in a cluster's life the control plane operator has not published a version.
	// Publishing a component with a blank one would block rollout completion silently, so
	// this is an error the integrator requeues on instead.
	hcp := testHostedControlPlane()
	hcp.Status.ControlPlaneVersion.Desired = configv1.Release{}
	deployment := rolledOutDeployment()

	c := fake.NewClientBuilder().
		WithScheme(testScheme(t)).
		WithObjects(hcp, deployment).
		WithStatusSubresource(&hyperv1.ControlPlaneComponent{}).
		Build()

	if err := Report(context.Background(), c, hcp, deployment); err == nil {
		t.Error("expected an error, got none")
	}
}

func TestReportRejectsADeploymentOutsideTheControlPlaneNamespace(t *testing.T) {
	hcp := testHostedControlPlane()
	deployment := rolledOutDeployment()
	deployment.Namespace = "somewhere-else"

	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(hcp).Build()
	if err := Report(context.Background(), c, hcp, deployment); err == nil {
		t.Error("expected an error, got none")
	}
}

func TestReportIsIdempotent(t *testing.T) {
	hcp := testHostedControlPlane()
	deployment := rolledOutDeployment()
	c := fake.NewClientBuilder().
		WithScheme(testScheme(t)).
		WithObjects(hcp, deployment).
		WithStatusSubresource(&hyperv1.ControlPlaneComponent{}).
		Build()

	for i := range 3 {
		if err := Report(context.Background(), c, hcp, deployment); err != nil {
			t.Fatalf("unexpected error on reconcile %d: %v", i, err)
		}
	}

	components := &hyperv1.ControlPlaneComponentList{}
	if err := c.List(context.Background(), components, client.InNamespace(testNamespace)); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(components.Items) != 1 {
		t.Errorf("expected exactly one control plane component, got %d", len(components.Items))
	}
	if conditions := components.Items[0].Status.Conditions; len(conditions) != 2 {
		t.Errorf("expected exactly two conditions, got %d", len(conditions))
	}
}

func TestRemove(t *testing.T) {
	hcp := testHostedControlPlane()
	deployment := rolledOutDeployment()
	c := fake.NewClientBuilder().
		WithScheme(testScheme(t)).
		WithObjects(hcp, deployment).
		WithStatusSubresource(&hyperv1.ControlPlaneComponent{}).
		Build()

	if err := Report(context.Background(), c, hcp, deployment); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if err := Remove(context.Background(), c, testNamespace, deployment.Name); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	err := c.Get(context.Background(), client.ObjectKey{Namespace: testNamespace, Name: deployment.Name}, &hyperv1.ControlPlaneComponent{})
	if !apierrors.IsNotFound(err) {
		t.Errorf("expected the component to be gone, got %v", err)
	}
}

func TestRemoveIsNotAnErrorWhenTheComponentIsAlreadyGone(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).Build()
	if err := Remove(context.Background(), c, testNamespace, "never-existed"); err != nil {
		t.Errorf("unexpected error: %v", err)
	}
}
