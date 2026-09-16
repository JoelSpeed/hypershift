package hostedcluster

import (
	"testing"
	"time"

	hyperv1 "github.com/openshift/hypershift/api/hypershift/v1beta1"
	"github.com/openshift/hypershift/support/api"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	clocktesting "k8s.io/utils/clock/testing"

	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

var externalHostedClusterGVK = schema.GroupVersionKind{Group: "example.io", Version: "v1alpha1", Kind: "FooHostedCluster"}

func externalTestHostedCluster() *hyperv1.HostedCluster {
	return &hyperv1.HostedCluster{
		ObjectMeta: metav1.ObjectMeta{Name: "example", Namespace: "clusters"},
		Spec: hyperv1.HostedClusterSpec{
			Platform: hyperv1.PlatformSpec{
				Type: hyperv1.ExternalPlatform,
				External: hyperv1.ExternalPlatformSpec{
					HostedClusterTemplate: hyperv1.ExternalTemplateReference{
						APIGroup: "example.io",
						Resource: "foohostedclustertemplates",
						Name:     "my-infra",
					},
				},
			},
		},
	}
}

// externalTestUncachedClient stands in for the direct API server client, with the
// integrator's CRDs installed.
func externalTestUncachedClient(objects ...client.Object) client.Client {
	scheme := runtime.NewScheme()
	scheme.AddKnownTypeWithName(externalHostedClusterGVK, &unstructured.Unstructured{})
	listGVK := externalHostedClusterGVK
	listGVK.Kind += "List"
	scheme.AddKnownTypeWithName(listGVK, &unstructured.UnstructuredList{})

	mapper := meta.NewDefaultRESTMapper([]schema.GroupVersion{externalHostedClusterGVK.GroupVersion()})
	mapper.Add(externalHostedClusterGVK, meta.RESTScopeNamespace)
	mapper.Add(schema.GroupVersionKind{
		Group:   externalHostedClusterGVK.Group,
		Version: externalHostedClusterGVK.Version,
		Kind:    "FooHostedClusterTemplate",
	}, meta.RESTScopeNamespace)

	return fake.NewClientBuilder().WithScheme(scheme).WithRESTMapper(mapper).WithObjects(objects...).Build()
}

func TestReconcileExternalInfrastructureStatus(t *testing.T) {
	notReady := &unstructured.Unstructured{}
	notReady.SetGroupVersionKind(externalHostedClusterGVK)
	notReady.SetNamespace("clusters-example")
	notReady.SetName("example")
	notReady.Object["status"] = map[string]any{
		"conditions": []any{
			map[string]any{"type": "Ready", "status": "False", "reason": "Provisioning", "message": "creating the network"},
		},
	}

	ready := notReady.DeepCopy()
	ready.Object["status"] = map[string]any{
		"conditions": []any{
			map[string]any{"type": "Ready", "status": "True", "reason": "AsExpected", "message": "infrastructure is provisioned"},
		},
	}

	testCases := []struct {
		name            string
		object          *unstructured.Unstructured
		expectedStatus  metav1.ConditionStatus
		expectedReason  string
		expectedRequeue bool
	}{
		{
			name:            "When the provider has not created the object, it should report not found and requeue",
			expectedStatus:  metav1.ConditionFalse,
			expectedReason:  hyperv1.ExternalInfrastructureNotFoundReason,
			expectedRequeue: true,
		},
		{
			name:            "When the provider is still working, it should report waiting and requeue",
			object:          notReady,
			expectedStatus:  metav1.ConditionFalse,
			expectedReason:  hyperv1.WaitingOnExternalProviderReason,
			expectedRequeue: true,
		},
		{
			// No requeue: once the provider is done the CAPI Cluster exists and the normal
			// watches take over, so polling the integrator forever would be pure load.
			name:           "When the provider is done, it should report ready and stop requeueing",
			object:         ready,
			expectedStatus: metav1.ConditionTrue,
			expectedReason: hyperv1.AsExpectedReason,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			hcluster := externalTestHostedCluster()
			var integratorObjects []client.Object
			if tc.object != nil {
				integratorObjects = append(integratorObjects, tc.object)
			}

			r := &HostedClusterReconciler{
				Client: fake.NewClientBuilder().
					WithScheme(api.Scheme).
					WithObjects(hcluster).
					WithStatusSubresource(&hyperv1.HostedCluster{}).
					Build(),
				UncachedClient: externalTestUncachedClient(integratorObjects...),
				Clock:          clocktesting.NewFakeClock(time.Now()),
			}

			requeue, err := r.reconcileExternalInfrastructureStatus(t.Context(), hcluster, "clusters-example")
			if err != nil {
				t.Fatalf("expected no error, got %v", err)
			}
			if tc.expectedRequeue != (requeue != nil) {
				t.Errorf("expected requeue %v, got %v", tc.expectedRequeue, requeue)
			}
			if requeue != nil && *requeue != externalInfrastructureRequeue {
				t.Errorf("expected a requeue of %s, got %s", externalInfrastructureRequeue, *requeue)
			}

			persisted := &hyperv1.HostedCluster{}
			if err := r.Client.Get(t.Context(), client.ObjectKeyFromObject(hcluster), persisted); err != nil {
				t.Fatalf("failed to get the hosted cluster: %v", err)
			}
			condition := meta.FindStatusCondition(persisted.Status.Conditions, string(hyperv1.ExternalInfrastructureReady))
			if condition == nil {
				t.Fatalf("expected the %s condition to have been persisted", hyperv1.ExternalInfrastructureReady)
			}
			if condition.Status != tc.expectedStatus {
				t.Errorf("expected status %q, got %q", tc.expectedStatus, condition.Status)
			}
			if condition.Reason != tc.expectedReason {
				t.Errorf("expected reason %q, got %q", tc.expectedReason, condition.Reason)
			}
		})
	}
}
