package awsprovider

import (
	"context"
	"strings"
	"testing"

	hyperv1 "github.com/openshift/hypershift/api/hypershift/v1beta1"
	"github.com/openshift/hypershift/externalplatform/reconcile"

	configv1 "github.com/openshift/api/config/v1"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

const (
	testNamespace = "clusters-example"
	testName      = "example"
)

func testScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	scheme := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{
		corev1.AddToScheme,
		appsv1.AddToScheme,
		hyperv1.AddToScheme,
	} {
		if err := add(scheme); err != nil {
			t.Fatalf("unexpected error building the scheme: %v", err)
		}
	}
	for _, gvk := range []schema.GroupVersionKind{HostedClusterObjectGVK, capaAWSClusterGVK} {
		scheme.AddKnownTypeWithName(gvk, &unstructured.Unstructured{})
		scheme.AddKnownTypeWithName(gvk.GroupVersion().WithKind(gvk.Kind+"List"), &unstructured.UnstructuredList{})
		metav1.AddToGroupVersion(scheme, gvk.GroupVersion())
	}
	return scheme
}

func testHostedClusterObject(spec map[string]any) *unstructured.Unstructured {
	object := &unstructured.Unstructured{}
	object.SetGroupVersionKind(HostedClusterObjectGVK)
	object.SetNamespace(testNamespace)
	object.SetName(testName)
	object.Object["spec"] = spec
	return object
}

func validSpec() map[string]any {
	return map[string]any{
		"region":          "eu-west-1",
		"vpcID":           "vpc-0123456789abcdef0",
		"subnetIDs":       []any{"subnet-aaa", "subnet-bbb"},
		"securityGroupID": "sg-0123456789abcdef0",
		"tags":            map[string]any{"owner": "platform-team"},
	}
}

func testRequest(t *testing.T, hostedClusterObject *unstructured.Unstructured, objects ...client.Object) (*reconcile.Request, client.Client) {
	t.Helper()

	awsCluster := &unstructured.Unstructured{}
	awsCluster.SetGroupVersionKind(capaAWSClusterGVK)

	c := fake.NewClientBuilder().
		WithScheme(testScheme(t)).
		WithObjects(objects...).
		WithStatusSubresource(awsCluster, &hyperv1.ControlPlaneComponent{}).
		Build()

	return &reconcile.Request{
		HostedClusterObject: hostedClusterObject,
		HostedControlPlane: &hyperv1.HostedControlPlane{
			ObjectMeta: metav1.ObjectMeta{Namespace: testNamespace, Name: testName, UID: "hcp-uid", Generation: 3},
			Spec:       hyperv1.HostedControlPlaneSpec{InfraID: "example-xyz12"},
			Status: hyperv1.HostedControlPlaneStatus{
				ControlPlaneVersion: hyperv1.ControlPlaneVersionStatus{
					Desired: configv1.Release{Version: "4.21.0"},
				},
			},
		},
		ControlPlaneEndpoint: hyperv1.APIEndpoint{Host: "api.example.hypershift.local", Port: 6443},
		ManagementClient:     c,
	}, c
}

func readAWSCluster(t *testing.T, c client.Client) *unstructured.Unstructured {
	t.Helper()
	awsCluster := &unstructured.Unstructured{}
	awsCluster.SetGroupVersionKind(capaAWSClusterGVK)
	if err := c.Get(context.Background(), client.ObjectKey{Namespace: testNamespace, Name: testName}, awsCluster); err != nil {
		t.Fatalf("unexpected error reading the AWSCluster: %v", err)
	}
	return awsCluster
}

func TestPlatformDeclaresAnExternalCloudControllerManager(t *testing.T) {
	// Declaring External is what taints every node uninitialized, so it has to agree with the
	// cloud controller manager this provisioner actually deploys.
	name, state := (&Provisioner{}).Platform()
	if name != PlatformName {
		t.Errorf("expected platform name %q, got %q", PlatformName, name)
	}
	if state != hyperv1.ExternalCloudControllerManager {
		t.Errorf("expected an external cloud controller manager, got %q", state)
	}
}

func TestProvisionTranslatesTheSpecIntoAnAWSCluster(t *testing.T) {
	request, c := testRequest(t, testHostedClusterObject(validSpec()))

	if _, err := (&Provisioner{}).Provision(context.Background(), request); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	awsCluster := readAWSCluster(t, c)
	if region, _, _ := unstructured.NestedString(awsCluster.Object, "spec", "region"); region != "eu-west-1" {
		t.Errorf("expected the region to be copied, got %q", region)
	}
	if vpc, _, _ := unstructured.NestedString(awsCluster.Object, "spec", "network", "vpc", "id"); vpc != "vpc-0123456789abcdef0" {
		t.Errorf("expected the VPC to be copied, got %q", vpc)
	}
	subnets, _, _ := unstructured.NestedSlice(awsCluster.Object, "spec", "network", "subnets")
	if len(subnets) != 2 {
		t.Errorf("expected two subnets, got %v", subnets)
	}
	if tag, _, _ := unstructured.NestedString(awsCluster.Object, "spec", "additionalTags", "owner"); tag != "platform-team" {
		t.Errorf("expected the tags to be copied, got %q", tag)
	}
	if name := awsCluster.GetLabels()["cluster.x-k8s.io/cluster-name"]; name != testName {
		t.Errorf("expected the Cluster API cluster name label, got %q", name)
	}
}

func TestProvisionPublishesTheControlPlaneEndpointAndDisablesCAPAsLoadBalancer(t *testing.T) {
	// The API server is served by the management cluster. A load balancer created here would
	// be an empty one that nothing ever reaches, billed monthly.
	request, c := testRequest(t, testHostedClusterObject(validSpec()))

	if _, err := (&Provisioner{}).Provision(context.Background(), request); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	awsCluster := readAWSCluster(t, c)
	host, _, _ := unstructured.NestedString(awsCluster.Object, "spec", "controlPlaneEndpoint", "host")
	port, _, _ := unstructured.NestedInt64(awsCluster.Object, "spec", "controlPlaneEndpoint", "port")
	if host != "api.example.hypershift.local" || port != 6443 {
		t.Errorf("expected HyperShift's endpoint to be published, got %s:%d", host, port)
	}
	if kind, _, _ := unstructured.NestedString(awsCluster.Object, "spec", "controlPlaneLoadBalancer", "loadBalancerType"); kind != "none" {
		t.Errorf("expected the control plane load balancer to be disabled, got %q", kind)
	}
}

func TestProvisionNamesTheInfrastructureObjectBeforeItIsReady(t *testing.T) {
	// HyperShift creates no Cluster API Cluster until the object is named, and a Cluster's
	// spec.infrastructureRef is immutable, so waiting for readiness would only delay it.
	request, _ := testRequest(t, testHostedClusterObject(validSpec()))

	result, err := (&Provisioner{}).Provision(context.Background(), request)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Done {
		t.Error("expected the provisioner to wait for Cluster API Provider AWS")
	}
	expected := reconcile.InfrastructureReference{
		APIGroup: "infrastructure.cluster.x-k8s.io",
		Kind:     "AWSCluster",
		Name:     testName,
	}
	if result.Infrastructure != expected {
		t.Errorf("expected the infrastructure object to be named %+v, got %+v", expected, result.Infrastructure)
	}
}

func TestProvisionIsDoneWhenClusterAPIProviderAWSReportsProvisioned(t *testing.T) {
	for _, testCase := range []struct {
		name   string
		status map[string]any
	}{
		{name: "v1beta2 initialization", status: map[string]any{"initialization": map[string]any{"provisioned": true}}},
		{name: "v1beta1 ready", status: map[string]any{"ready": true}},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			awsCluster := &unstructured.Unstructured{}
			awsCluster.SetGroupVersionKind(capaAWSClusterGVK)
			awsCluster.SetNamespace(testNamespace)
			awsCluster.SetName(testName)
			awsCluster.Object["status"] = testCase.status

			request, _ := testRequest(t, testHostedClusterObject(validSpec()), awsCluster)

			result, err := (&Provisioner{}).Provision(context.Background(), request)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if !result.Done {
				t.Errorf("expected provisioning to be done, got %+v", result)
			}
		})
	}
}

func TestProvisionMirrorsClusterAPIProviderAWSsOwnMessage(t *testing.T) {
	// HyperShift copies this integration's message verbatim onto the HostedCluster, so CAPA's
	// account of the failure is the only thing a cluster's owner will ever see about it.
	awsCluster := &unstructured.Unstructured{}
	awsCluster.SetGroupVersionKind(capaAWSClusterGVK)
	awsCluster.SetNamespace(testNamespace)
	awsCluster.SetName(testName)
	awsCluster.Object["status"] = map[string]any{
		"conditions": []any{map[string]any{
			"type":    "Ready",
			"status":  "False",
			"reason":  "SubnetsNotFound",
			"message": "subnet subnet-aaa does not exist in vpc-0123456789abcdef0",
		}},
	}

	request, _ := testRequest(t, testHostedClusterObject(validSpec()), awsCluster)

	result, err := (&Provisioner{}).Provision(context.Background(), request)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(result.Message, "subnet subnet-aaa does not exist") {
		t.Errorf("expected Cluster API Provider AWS's message to be passed through, got %q", result.Message)
	}
}

func TestProvisionReportsAnUnreadableSpecRatherThanFailingInALoop(t *testing.T) {
	request, _ := testRequest(t, testHostedClusterObject(map[string]any{"vpcID": "vpc-0123456789abcdef0"}))

	result, err := (&Provisioner{}).Provision(context.Background(), request)
	if err != nil {
		t.Fatalf("expected the invalid spec to be reported rather than returned, got %v", err)
	}
	if result.Done {
		t.Error("expected provisioning not to be done")
	}
	if !strings.Contains(result.Message, "spec.region is required") {
		t.Errorf("expected the message to name the missing field, got %q", result.Message)
	}
}

func TestProvisionRunsAndReportsTheCloudControllerManager(t *testing.T) {
	request, c := testRequest(t, testHostedClusterObject(validSpec()))

	if _, err := (&Provisioner{
		CloudControllerManagerImage: "registry.example.com/aws-cloud-controller-manager:v1.32.0",
	}).Provision(context.Background(), request); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	serviceAccount := &corev1.ServiceAccount{}
	if err := c.Get(context.Background(), client.ObjectKey{Namespace: testNamespace, Name: cloudControllerManagerName}, serviceAccount); err != nil {
		t.Fatalf("expected a service account for the pod's AWS identity: %v", err)
	}

	deployment := &appsv1.Deployment{}
	if err := c.Get(context.Background(), client.ObjectKey{Namespace: testNamespace, Name: cloudControllerManagerName}, deployment); err != nil {
		t.Fatalf("expected the cloud controller manager to be deployed: %v", err)
	}
	if image := deployment.Spec.Template.Spec.Containers[0].Image; image != "registry.example.com/aws-cloud-controller-manager:v1.32.0" {
		t.Errorf("expected the configured image, got %q", image)
	}
	if volume := deployment.Spec.Template.Spec.Volumes[0].Secret.SecretName; volume != "service-network-admin-kubeconfig" {
		t.Errorf("expected the guest kubeconfig to be mounted, got %q", volume)
	}

	// Reported to HyperShift, because having declared an external cloud controller manager
	// this integration has made every node in the cluster depend on that Deployment.
	component := &hyperv1.ControlPlaneComponent{}
	if err := c.Get(context.Background(), client.ObjectKey{Namespace: testNamespace, Name: cloudControllerManagerName}, component); err != nil {
		t.Fatalf("expected a control plane component to be published: %v", err)
	}
	if component.Status.Version != "" {
		t.Errorf("expected no version while the rollout is incomplete, got %q", component.Status.Version)
	}
	if len(component.OwnerReferences) != 1 {
		t.Errorf("expected the component to be owned by the hosted control plane, got %v", component.OwnerReferences)
	}
}

func TestProvisionSkipsTheCloudControllerManagerWhenNoImageIsConfigured(t *testing.T) {
	request, c := testRequest(t, testHostedClusterObject(validSpec()))

	if _, err := (&Provisioner{}).Provision(context.Background(), request); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	deployment := &appsv1.Deployment{}
	err := c.Get(context.Background(), client.ObjectKey{Namespace: testNamespace, Name: cloudControllerManagerName}, deployment)
	if !apierrors.IsNotFound(err) {
		t.Errorf("expected no deployment, got %v", err)
	}
}

func TestProvisionIsIdempotent(t *testing.T) {
	request, c := testRequest(t, testHostedClusterObject(validSpec()))
	provisioner := &Provisioner{CloudControllerManagerImage: "registry.example.com/ccm:v1"}

	for i := range 3 {
		if _, err := provisioner.Provision(context.Background(), request); err != nil {
			t.Fatalf("unexpected error on pass %d: %v", i, err)
		}
	}

	awsClusters := &unstructured.UnstructuredList{}
	awsClusters.SetGroupVersionKind(capaAWSClusterGVK.GroupVersion().WithKind(capaAWSClusterGVK.Kind + "List"))
	if err := c.List(context.Background(), awsClusters, client.InNamespace(testNamespace)); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(awsClusters.Items) != 1 {
		t.Errorf("expected exactly one AWSCluster, got %d", len(awsClusters.Items))
	}
}

func TestDeprovisionDeletesTheAWSClusterAndWaitsForIt(t *testing.T) {
	// Reporting done here would release the finalizer and let HyperShift delete the control
	// plane namespace out from under Cluster API Provider AWS mid-teardown.
	awsCluster := &unstructured.Unstructured{}
	awsCluster.SetGroupVersionKind(capaAWSClusterGVK)
	awsCluster.SetNamespace(testNamespace)
	awsCluster.SetName(testName)
	awsCluster.SetFinalizers([]string{"awscluster.infrastructure.cluster.x-k8s.io"})

	request, c := testRequest(t, testHostedClusterObject(validSpec()), awsCluster)

	result, err := (&Provisioner{}).Deprovision(context.Background(), request)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Done {
		t.Error("expected teardown to wait for Cluster API Provider AWS")
	}
	if deletionTimestamp := readAWSCluster(t, c).GetDeletionTimestamp(); deletionTimestamp.IsZero() {
		t.Error("expected the AWSCluster to have been deleted")
	}
}

func TestDeprovisionIsDoneOnceTheAWSClusterIsGone(t *testing.T) {
	request, _ := testRequest(t, testHostedClusterObject(validSpec()))

	result, err := (&Provisioner{}).Deprovision(context.Background(), request)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.Done {
		t.Error("expected teardown to be done once the AWSCluster no longer exists")
	}
}

func TestDeprovisionLeavesTheCloudControllerManagerRunning(t *testing.T) {
	// It lives in the control plane namespace, which HyperShift deletes once teardown is
	// done. Deleting it early only removes what is keeping existing nodes usable while the
	// rest of the cluster tears down.
	request, c := testRequest(t, testHostedClusterObject(validSpec()))
	provisioner := &Provisioner{CloudControllerManagerImage: "registry.example.com/ccm:v1"}

	if _, err := provisioner.Provision(context.Background(), request); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if _, err := provisioner.Deprovision(context.Background(), request); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	deployment := &appsv1.Deployment{}
	if err := c.Get(context.Background(), client.ObjectKey{Namespace: testNamespace, Name: cloudControllerManagerName}, deployment); err != nil {
		t.Errorf("expected the cloud controller manager to still be running: %v", err)
	}
	if replicas := deployment.Spec.Replicas; replicas == nil || *replicas != 1 {
		t.Errorf("expected one replica, got %v", replicas)
	}
}
