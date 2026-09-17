package external

import (
	"testing"
	"time"

	hyperv1 "github.com/openshift/hypershift/api/hypershift/v1beta1"
	"github.com/openshift/hypershift/support/upsert"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"

	capiv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/google/go-cmp/cmp"
)

const (
	testAPIGroup          = "example.io"
	testTemplateResource  = "foohostedclustertemplates"
	testTemplateKind      = "FooHostedClusterTemplate"
	testHostedClusterKind = "FooHostedCluster"
	testInfraGroup        = "infrastructure.cluster.x-k8s.io"
	testInfraKind         = "AWSCluster"
)

var (
	templateGVK      = schema.GroupVersionKind{Group: testAPIGroup, Version: "v1alpha1", Kind: testTemplateKind}
	hostedClusterGVK = schema.GroupVersionKind{Group: testAPIGroup, Version: "v1alpha1", Kind: testHostedClusterKind}
	infraGVK         = schema.GroupVersionKind{Group: testInfraGroup, Version: "v1beta2", Kind: testInfraKind}
)

// testRESTMapper maps the integrator's types the way a management cluster with the
// integrator's CRDs installed would.
func testRESTMapper() meta.RESTMapper {
	// The default group versions have to be populated for RESTMapping to resolve a
	// GroupKind that carries no version, which is the shape status.infrastructure uses.
	mapper := meta.NewDefaultRESTMapper([]schema.GroupVersion{templateGVK.GroupVersion(), infraGVK.GroupVersion()})
	for _, gvk := range []schema.GroupVersionKind{templateGVK, hostedClusterGVK, infraGVK} {
		mapper.Add(gvk, meta.RESTScopeNamespace)
	}
	return mapper
}

func testScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	scheme := runtime.NewScheme()
	for _, gvk := range []schema.GroupVersionKind{templateGVK, hostedClusterGVK, infraGVK} {
		scheme.AddKnownTypeWithName(gvk, &unstructured.Unstructured{})
		listGVK := gvk
		listGVK.Kind += "List"
		scheme.AddKnownTypeWithName(listGVK, &unstructured.UnstructuredList{})
	}
	return scheme
}

func testHostedCluster() *hyperv1.HostedCluster {
	return &hyperv1.HostedCluster{
		ObjectMeta: metav1.ObjectMeta{Name: "example", Namespace: "clusters"},
		Spec: hyperv1.HostedClusterSpec{
			InfraID: "example-abcde",
			Platform: hyperv1.PlatformSpec{
				Type: hyperv1.ExternalPlatform,
				External: hyperv1.ExternalPlatformSpec{
					HostedClusterTemplate: hyperv1.ExternalTemplateReference{
						APIGroup: testAPIGroup,
						Resource: testTemplateResource,
						Name:     "my-infra",
					},
				},
			},
		},
	}
}

func testTemplate(region string) *unstructured.Unstructured {
	template := &unstructured.Unstructured{}
	template.SetGroupVersionKind(templateGVK)
	template.SetNamespace("clusters")
	template.SetName("my-infra")
	template.Object["spec"] = map[string]any{
		"template": map[string]any{
			"spec": map[string]any{
				"region": region,
			},
		},
	}
	return template
}

func testHostedClusterObject() *unstructured.Unstructured {
	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(hostedClusterGVK)
	obj.SetNamespace("clusters-example")
	obj.SetName("example")
	return obj
}

func newFakeClient(t *testing.T, objects ...client.Object) client.Client {
	t.Helper()
	return fake.NewClientBuilder().
		WithScheme(testScheme(t)).
		WithRESTMapper(testRESTMapper()).
		WithObjects(objects...).
		Build()
}

func TestReconcileCAPIInfraCR(t *testing.T) {
	endpoint := hyperv1.APIEndpoint{Host: "api.example.com", Port: 6443}

	t.Run("When the control plane endpoint is unknown, it should do nothing", func(t *testing.T) {
		c := newFakeClient(t, testTemplate("us-east-1"))
		p := New(c)

		infraCR, err := p.ReconcileCAPIInfraCR(t.Context(), c, upsert.New(false).CreateOrUpdate, testHostedCluster(), "clusters-example", hyperv1.APIEndpoint{})
		if err != nil {
			t.Fatalf("expected no error, got %v", err)
		}
		if infraCR != nil {
			t.Errorf("expected no infra CR, got %v", infraCR)
		}

		created := &unstructured.Unstructured{}
		created.SetGroupVersionKind(hostedClusterGVK)
		if err := c.Get(t.Context(), client.ObjectKey{Namespace: "clusters-example", Name: "example"}, created); err == nil {
			t.Error("expected no hosted cluster object to have been created")
		}
	})

	t.Run("When no uncached client is configured, it should return an error", func(t *testing.T) {
		c := newFakeClient(t, testTemplate("us-east-1"))
		p := External{}

		if _, err := p.ReconcileCAPIInfraCR(t.Context(), c, upsert.New(false).CreateOrUpdate, testHostedCluster(), "clusters-example", endpoint); err == nil {
			t.Fatal("expected an error, got none")
		}
	})

	t.Run("When the template does not exist, it should return an error", func(t *testing.T) {
		c := newFakeClient(t)
		p := New(c)

		if _, err := p.ReconcileCAPIInfraCR(t.Context(), c, upsert.New(false).CreateOrUpdate, testHostedCluster(), "clusters-example", endpoint); err == nil {
			t.Fatal("expected an error, got none")
		}
	})

	t.Run("When the template exists, it should instantiate the hosted cluster object", func(t *testing.T) {
		c := newFakeClient(t, testTemplate("us-east-1"))
		p := New(c)

		infraCR, err := p.ReconcileCAPIInfraCR(t.Context(), c, upsert.New(false).CreateOrUpdate, testHostedCluster(), "clusters-example", endpoint)
		if err != nil {
			t.Fatalf("expected no error, got %v", err)
		}
		// The integrator has not reported a CAPI infrastructure object yet, so there is
		// nothing to point a CAPI Cluster at.
		if infraCR != nil {
			t.Errorf("expected no infra CR, got %v", infraCR)
		}

		created := &unstructured.Unstructured{}
		created.SetGroupVersionKind(hostedClusterGVK)
		if err := c.Get(t.Context(), client.ObjectKey{Namespace: "clusters-example", Name: "example"}, created); err != nil {
			t.Fatalf("expected the hosted cluster object to have been created: %v", err)
		}

		wantSpec := map[string]any{
			"region": "us-east-1",
			"controlPlaneEndpoint": map[string]any{
				"host": "api.example.com",
				"port": int64(6443),
			},
		}
		if diff := cmp.Diff(wantSpec, created.Object["spec"]); diff != "" {
			t.Errorf("unexpected spec (-want +got):\n%s", diff)
		}
		if got := created.GetLabels()[capiv1.ClusterNameLabel]; got != "example-abcde" {
			t.Errorf("expected the cluster name label to be the InfraID, got %q", got)
		}
		if got := created.GetLabels()[ExternalPlatformGroupLabel]; got != testAPIGroup {
			t.Errorf("expected the platform group label to be %q, got %q", testAPIGroup, got)
		}
	})

	t.Run("When the object already exists, it should update only the control plane endpoint", func(t *testing.T) {
		existing := testHostedClusterObject()
		existing.Object["spec"] = map[string]any{
			// A value the integrator resolved for itself. Copying the template over it
			// would silently re-provision.
			"region": "us-west-2",
			"controlPlaneEndpoint": map[string]any{
				"host": "old.example.com",
				"port": int64(443),
			},
		}
		c := newFakeClient(t, testTemplate("us-east-1"), existing)
		p := New(c)

		if _, err := p.ReconcileCAPIInfraCR(t.Context(), c, upsert.New(false).CreateOrUpdate, testHostedCluster(), "clusters-example", endpoint); err != nil {
			t.Fatalf("expected no error, got %v", err)
		}

		updated := &unstructured.Unstructured{}
		updated.SetGroupVersionKind(hostedClusterGVK)
		if err := c.Get(t.Context(), client.ObjectKey{Namespace: "clusters-example", Name: "example"}, updated); err != nil {
			t.Fatalf("failed to get the hosted cluster object: %v", err)
		}
		wantSpec := map[string]any{
			"region": "us-west-2",
			"controlPlaneEndpoint": map[string]any{
				"host": "api.example.com",
				"port": int64(6443),
			},
		}
		if diff := cmp.Diff(wantSpec, updated.Object["spec"]); diff != "" {
			t.Errorf("unexpected spec (-want +got):\n%s", diff)
		}
	})

	t.Run("When the provider reports an infrastructure object, it should return a stub of it", func(t *testing.T) {
		existing := testHostedClusterObject()
		existing.Object["status"] = map[string]any{
			"infrastructure": map[string]any{
				"apiGroup": testInfraGroup,
				"kind":     testInfraKind,
				"name":     "example-abcde",
			},
		}
		c := newFakeClient(t, testTemplate("us-east-1"), existing)
		p := New(c)

		infraCR, err := p.ReconcileCAPIInfraCR(t.Context(), c, upsert.New(false).CreateOrUpdate, testHostedCluster(), "clusters-example", endpoint)
		if err != nil {
			t.Fatalf("expected no error, got %v", err)
		}
		if infraCR == nil {
			t.Fatal("expected an infra CR, got nil")
		}
		if got := infraCR.GetObjectKind().GroupVersionKind(); got != infraGVK {
			t.Errorf("expected GVK %v, got %v", infraGVK, got)
		}
		if infraCR.GetName() != "example-abcde" || infraCR.GetNamespace() != "clusters-example" {
			t.Errorf("unexpected object key %s/%s", infraCR.GetNamespace(), infraCR.GetName())
		}

		// The stub must carry nothing but its identity: an apply of an empty spec over a
		// provisioned AWSCluster would destroy it.
		stub, ok := infraCR.(*unstructured.Unstructured)
		if !ok {
			t.Fatalf("expected an unstructured object, got %T", infraCR)
		}
		if _, found := stub.Object["spec"]; found {
			t.Error("expected the stub to carry no spec")
		}
		if _, found := stub.Object["status"]; found {
			t.Error("expected the stub to carry no status")
		}
	})

	t.Run("When the reported infrastructure kind is not installed, it should return an error", func(t *testing.T) {
		existing := testHostedClusterObject()
		existing.Object["status"] = map[string]any{
			"infrastructure": map[string]any{
				"apiGroup": testInfraGroup,
				"kind":     "NotInstalledCluster",
				"name":     "example-abcde",
			},
		}
		c := newFakeClient(t, testTemplate("us-east-1"), existing)
		p := New(c)

		if _, err := p.ReconcileCAPIInfraCR(t.Context(), c, upsert.New(false).CreateOrUpdate, testHostedCluster(), "clusters-example", endpoint); err == nil {
			t.Fatal("expected an error, got none")
		}
	})
}

func TestInfrastructureReadyCondition(t *testing.T) {
	testCases := []struct {
		name        string
		object      *unstructured.Unstructured
		waitingFor  time.Duration
		wantStatus  metav1.ConditionStatus
		wantReason  string
		wantMessage string
	}{
		{
			name:        "When the hosted cluster object does not exist, it should report not found",
			wantStatus:  metav1.ConditionFalse,
			wantReason:  hyperv1.ExternalInfrastructureNotFoundReason,
			wantMessage: "The FooHostedCluster clusters-example/example has not been created yet",
		},
		{
			name:        "When the provider has not reported a condition, it should report waiting",
			object:      testHostedClusterObject(),
			wantStatus:  metav1.ConditionFalse,
			wantReason:  hyperv1.WaitingOnExternalProviderReason,
			wantMessage: "The provider has not reported a Ready condition on FooHostedCluster clusters-example/example yet",
		},
		{
			name:        "When the provider reports not ready, it should mirror the provider message",
			object:      withReadyCondition(testHostedClusterObject(), metav1.ConditionFalse, "Provisioning", "waiting for the VPC to become available"),
			wantStatus:  metav1.ConditionFalse,
			wantReason:  hyperv1.WaitingOnExternalProviderReason,
			wantMessage: "waiting for the VPC to become available",
		},
		{
			name:        "When the provider has been unready for a long time, it should say how long",
			object:      withReadyCondition(testHostedClusterObject(), metav1.ConditionFalse, "Provisioning", "waiting for the VPC to become available"),
			waitingFor:  45 * time.Minute,
			wantStatus:  metav1.ConditionFalse,
			wantReason:  hyperv1.WaitingOnExternalProviderReason,
			wantMessage: "waiting for the VPC to become available (waiting on the provider for 45m0s)",
		},
		{
			name:        "When the provider reports ready, it should report ready",
			object:      withReadyCondition(testHostedClusterObject(), metav1.ConditionTrue, "AsExpected", "infrastructure is provisioned"),
			wantStatus:  metav1.ConditionTrue,
			wantReason:  hyperv1.AsExpectedReason,
			wantMessage: "infrastructure is provisioned",
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			var objects []client.Object
			if tc.object != nil {
				objects = append(objects, tc.object)
			}
			c := newFakeClient(t, objects...)

			condition, err := InfrastructureReadyCondition(t.Context(), c, testHostedCluster(), "clusters-example", tc.waitingFor)
			if err != nil {
				t.Fatalf("expected no error, got %v", err)
			}
			if condition.Type != string(hyperv1.ExternalInfrastructureReady) {
				t.Errorf("expected condition type %q, got %q", hyperv1.ExternalInfrastructureReady, condition.Type)
			}
			if condition.Status != tc.wantStatus {
				t.Errorf("expected status %q, got %q", tc.wantStatus, condition.Status)
			}
			if condition.Reason != tc.wantReason {
				t.Errorf("expected reason %q, got %q", tc.wantReason, condition.Reason)
			}
			if condition.Message != tc.wantMessage {
				t.Errorf("expected message %q, got %q", tc.wantMessage, condition.Message)
			}
		})
	}
}

func withReadyCondition(obj *unstructured.Unstructured, status metav1.ConditionStatus, reason, message string) *unstructured.Unstructured {
	obj.Object["status"] = map[string]any{
		"conditions": []any{
			map[string]any{
				"type":    "NotTheOneWeWant",
				"status":  "False",
				"reason":  "Ignored",
				"message": "this condition must not be mirrored",
			},
			map[string]any{
				"type":    readyConditionType,
				"status":  string(status),
				"reason":  reason,
				"message": message,
			},
		},
	}
	return obj
}

func TestDeleteHostedClusterObject(t *testing.T) {
	withFinalizer := func(obj *unstructured.Unstructured) *unstructured.Unstructured {
		obj.SetFinalizers([]string{"example.io/provider"})
		return obj
	}

	t.Run("when the object does not exist, it reports nothing to wait for", func(t *testing.T) {
		c := newFakeClient(t)
		exists, err := DeleteHostedClusterObject(t.Context(), c, testHostedCluster(), "clusters-example", false)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if exists {
			t.Errorf("expected the object to be reported gone")
		}
	})

	t.Run("when the custom resource definition is not installed, it reports nothing to wait for", func(t *testing.T) {
		// An uninstalled CRD took every object of that type with it. Erroring here would
		// make the HostedCluster undeletable for a reason nobody can act on.
		c := fake.NewClientBuilder().
			WithScheme(testScheme(t)).
			WithRESTMapper(meta.NewDefaultRESTMapper(nil)).
			Build()
		exists, err := DeleteHostedClusterObject(t.Context(), c, testHostedCluster(), "clusters-example", false)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if exists {
			t.Errorf("expected the object to be reported gone")
		}
	})

	t.Run("when the object exists, it is deleted and still reported present", func(t *testing.T) {
		c := newFakeClient(t, withFinalizer(testHostedClusterObject()))
		exists, err := DeleteHostedClusterObject(t.Context(), c, testHostedCluster(), "clusters-example", false)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !exists {
			t.Fatalf("expected the object to be reported present while the provider tears down")
		}

		got := &unstructured.Unstructured{}
		got.SetGroupVersionKind(hostedClusterGVK)
		if err := c.Get(t.Context(), client.ObjectKey{Namespace: "clusters-example", Name: "example"}, got); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got.GetDeletionTimestamp().IsZero() {
			t.Errorf("expected the object to be marked for deletion")
		}
	})

	t.Run("while the provider holds its finalizer, it keeps reporting present without forcing", func(t *testing.T) {
		c := newFakeClient(t, withFinalizer(testHostedClusterObject()))
		hc := testHostedCluster()
		for range 2 {
			exists, err := DeleteHostedClusterObject(t.Context(), c, hc, "clusters-example", false)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if !exists {
				t.Fatalf("expected the object to be reported present")
			}
		}

		got := &unstructured.Unstructured{}
		got.SetGroupVersionKind(hostedClusterGVK)
		if err := c.Get(t.Context(), client.ObjectKey{Namespace: "clusters-example", Name: "example"}, got); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if diff := cmp.Diff([]string{"example.io/provider"}, got.GetFinalizers()); diff != "" {
			t.Errorf("finalizers differ: %s", diff)
		}
	})

	t.Run("when forced, the provider's finalizer is stripped and the object goes away", func(t *testing.T) {
		c := newFakeClient(t, withFinalizer(testHostedClusterObject()))
		hc := testHostedCluster()

		// The first pass only issues the delete: force never skips asking the provider
		// first, it only stops waiting for an answer.
		if _, err := DeleteHostedClusterObject(t.Context(), c, hc, "clusters-example", true); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if _, err := DeleteHostedClusterObject(t.Context(), c, hc, "clusters-example", true); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		exists, err := DeleteHostedClusterObject(t.Context(), c, hc, "clusters-example", true)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if exists {
			t.Errorf("expected the object to be gone once its finalizer was stripped")
		}
	})
}
