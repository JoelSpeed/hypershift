package hostedcontrolplane

import (
	"testing"

	. "github.com/onsi/gomega"

	hyperv1 "github.com/openshift/hypershift/api/hypershift/v1beta1"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

const (
	externalTestAPIGroup = "example.io"
	externalTestResource = "foohostedclustertemplates"
)

var (
	externalTestTemplateGVK      = schema.GroupVersionKind{Group: externalTestAPIGroup, Version: "v1alpha1", Kind: "FooHostedClusterTemplate"}
	externalTestHostedClusterGVK = schema.GroupVersionKind{Group: externalTestAPIGroup, Version: "v1alpha1", Kind: "FooHostedCluster"}
)

func externalTestHCP(recorded *hyperv1.ExternalPlatformStatus) *hyperv1.HostedControlPlane {
	hcp := &hyperv1.HostedControlPlane{
		ObjectMeta: metav1.ObjectMeta{Namespace: "clusters-example", Name: "example"},
		Spec: hyperv1.HostedControlPlaneSpec{
			Platform: hyperv1.PlatformSpec{
				Type: hyperv1.ExternalPlatform,
				External: hyperv1.ExternalPlatformSpec{
					HostedClusterTemplate: hyperv1.ExternalTemplateReference{
						APIGroup: externalTestAPIGroup,
						Resource: externalTestResource,
						Name:     "my-infra",
					},
				},
			},
		},
	}
	if recorded != nil {
		hcp.Status.Platform = &hyperv1.PlatformStatus{External: *recorded}
	}
	return hcp
}

// externalTestHostedClusterObject builds the integrator's object with the given
// status.platform block, or without one when platform is nil.
func externalTestHostedClusterObject(platform map[string]any) *unstructured.Unstructured {
	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(externalTestHostedClusterGVK)
	obj.SetNamespace("clusters-example")
	obj.SetName("example")
	if platform != nil {
		obj.Object["status"] = map[string]any{"platform": platform}
	}
	return obj
}

func externalTestDeclaration(name string, state hyperv1.ExternalCloudControllerManagerState) map[string]any {
	return map[string]any{
		"name":                   name,
		"cloudControllerManager": map[string]any{"state": string(state)},
	}
}

// externalTestClient builds a client that knows the integrator's types, the way a
// management cluster with the integrator's custom resource definitions installed would.
// When registerCRDs is false the types are absent, which is what a Control Plane Operator
// sees before the integrator has been installed.
func externalTestClient(t *testing.T, registerCRDs bool, objects ...client.Object) client.Client {
	t.Helper()

	// A fresh scheme rather than api.Scheme: the integrator's types are registered on it
	// per test case, and api.Scheme is a process-wide global shared with every other test.
	testScheme := runtime.NewScheme()
	if err := hyperv1.AddToScheme(testScheme); err != nil {
		t.Fatalf("failed to build scheme: %v", err)
	}
	mapper := meta.NewDefaultRESTMapper([]schema.GroupVersion{externalTestTemplateGVK.GroupVersion()})
	if registerCRDs {
		for _, gvk := range []schema.GroupVersionKind{externalTestTemplateGVK, externalTestHostedClusterGVK} {
			testScheme.AddKnownTypeWithName(gvk, &unstructured.Unstructured{})
			listGVK := gvk
			listGVK.Kind += "List"
			testScheme.AddKnownTypeWithName(listGVK, &unstructured.UnstructuredList{})
			mapper.Add(gvk, meta.RESTScopeNamespace)
		}
	}

	return fake.NewClientBuilder().
		WithScheme(testScheme).
		WithRESTMapper(mapper).
		WithObjects(objects...).
		WithStatusSubresource(&hyperv1.HostedControlPlane{}).
		Build()
}

func TestReconcileExternalPlatformStatus(t *testing.T) {
	t.Parallel()

	externalDeclaration := &hyperv1.ExternalPlatformStatus{
		Name: "ExampleCloud",
		CloudControllerManager: hyperv1.ExternalCloudControllerManagerStatus{
			State: hyperv1.ExternalCloudControllerManager,
		},
	}
	noneDeclaration := &hyperv1.ExternalPlatformStatus{
		Name: "ExampleCloud",
		CloudControllerManager: hyperv1.ExternalCloudControllerManagerStatus{
			State: hyperv1.NoCloudControllerManager,
		},
	}

	testCases := []struct {
		name string
		// hcp is nil for the not-External case, which is built inline.
		hcp                 *hyperv1.HostedControlPlane
		registerCRDs        bool
		hostedClusterObject *unstructured.Unstructured
		expectRecorded      *hyperv1.ExternalPlatformStatus
		expectConditionSet  bool
		expectStatus        metav1.ConditionStatus
		expectReason        string
	}{
		{
			name: "When the platform is not External, it should do nothing",
			hcp: &hyperv1.HostedControlPlane{
				ObjectMeta: metav1.ObjectMeta{Namespace: "clusters-example", Name: "example"},
				Spec:       hyperv1.HostedControlPlaneSpec{Platform: hyperv1.PlatformSpec{Type: hyperv1.AWSPlatform}},
			},
		},
		{
			// The integrator has not been installed. Reported as waiting rather than as an
			// error: a control plane can come up long before its provider does.
			name:               "When the integrator's types are not installed, it should report waiting",
			hcp:                externalTestHCP(nil),
			expectConditionSet: true,
			expectStatus:       metav1.ConditionFalse,
			expectReason:       hyperv1.WaitingOnExternalProviderReason,
		},
		{
			name:               "When the hosted cluster object has not been created, it should report waiting",
			hcp:                externalTestHCP(nil),
			registerCRDs:       true,
			expectConditionSet: true,
			expectStatus:       metav1.ConditionFalse,
			expectReason:       hyperv1.WaitingOnExternalProviderReason,
		},
		{
			name:                "When nothing has been declared, it should report waiting",
			hcp:                 externalTestHCP(nil),
			registerCRDs:        true,
			hostedClusterObject: externalTestHostedClusterObject(nil),
			expectConditionSet:  true,
			expectStatus:        metav1.ConditionFalse,
			expectReason:        hyperv1.WaitingOnExternalProviderReason,
		},
		{
			name:                "When the declaration is first observed, it should be recorded",
			hcp:                 externalTestHCP(nil),
			registerCRDs:        true,
			hostedClusterObject: externalTestHostedClusterObject(externalTestDeclaration("ExampleCloud", hyperv1.ExternalCloudControllerManager)),
			expectRecorded:      externalDeclaration,
			expectConditionSet:  true,
			expectStatus:        metav1.ConditionTrue,
			expectReason:        hyperv1.AsExpectedReason,
		},
		{
			name:                "When a declaration without a cloud controller manager is observed, it should be recorded",
			hcp:                 externalTestHCP(nil),
			registerCRDs:        true,
			hostedClusterObject: externalTestHostedClusterObject(externalTestDeclaration("ExampleCloud", hyperv1.NoCloudControllerManager)),
			expectRecorded:      noneDeclaration,
			expectConditionSet:  true,
			expectStatus:        metav1.ConditionTrue,
			expectReason:        hyperv1.AsExpectedReason,
		},
		{
			name:                "When the declaration is unchanged, it should stay recorded",
			hcp:                 externalTestHCP(externalDeclaration),
			registerCRDs:        true,
			hostedClusterObject: externalTestHostedClusterObject(externalTestDeclaration("ExampleCloud", hyperv1.ExternalCloudControllerManager)),
			expectRecorded:      externalDeclaration,
			expectConditionSet:  true,
			expectStatus:        metav1.ConditionTrue,
			expectReason:        hyperv1.AsExpectedReason,
		},
		{
			// The rule the whole function exists for: adopting the new value would change
			// the kubelet's cloud provider and roll every node in the cluster.
			name:                "When the declaration changes after it was recorded, it should keep the recorded value and report it",
			hcp:                 externalTestHCP(externalDeclaration),
			registerCRDs:        true,
			hostedClusterObject: externalTestHostedClusterObject(externalTestDeclaration("ExampleCloud", hyperv1.NoCloudControllerManager)),
			expectRecorded:      externalDeclaration,
			expectConditionSet:  true,
			expectStatus:        metav1.ConditionFalse,
			expectReason:        hyperv1.ExternalPlatformDeclarationChangedReason,
		},
		{
			name:                "When the platform name changes after it was recorded, it should keep the recorded value and report it",
			hcp:                 externalTestHCP(externalDeclaration),
			registerCRDs:        true,
			hostedClusterObject: externalTestHostedClusterObject(externalTestDeclaration("OtherCloud", hyperv1.ExternalCloudControllerManager)),
			expectRecorded:      externalDeclaration,
			expectConditionSet:  true,
			expectStatus:        metav1.ConditionFalse,
			expectReason:        hyperv1.ExternalPlatformDeclarationChangedReason,
		},
		{
			name:                "When the declaration is malformed, it should report it as invalid rather than fail",
			hcp:                 externalTestHCP(nil),
			registerCRDs:        true,
			hostedClusterObject: externalTestHostedClusterObject(map[string]any{"name": "ExampleCloud"}),
			expectConditionSet:  true,
			expectStatus:        metav1.ConditionFalse,
			expectReason:        hyperv1.ExternalPlatformDeclarationInvalidReason,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			g := NewWithT(t)

			objects := []client.Object{tc.hcp}
			if tc.hostedClusterObject != nil {
				objects = append(objects, tc.hostedClusterObject)
			}
			c := externalTestClient(t, tc.registerCRDs, objects...)
			r := &HostedControlPlaneReconciler{Client: c}

			g.Expect(r.reconcileExternalPlatformStatus(t.Context(), tc.hcp)).To(Succeed())

			got := &hyperv1.HostedControlPlane{}
			g.Expect(c.Get(t.Context(), client.ObjectKeyFromObject(tc.hcp), got)).To(Succeed())

			g.Expect(recordedExternalPlatformDeclaration(got)).To(Equal(tc.expectRecorded))

			condition := meta.FindStatusCondition(got.Status.Conditions, string(hyperv1.ValidExternalPlatformDeclaration))
			if !tc.expectConditionSet {
				g.Expect(condition).To(BeNil())
				return
			}
			g.Expect(condition).ToNot(BeNil())
			g.Expect(condition.Status).To(Equal(tc.expectStatus))
			g.Expect(condition.Reason).To(Equal(tc.expectReason))
			g.Expect(condition.Message).ToNot(BeEmpty())
		})
	}
}
