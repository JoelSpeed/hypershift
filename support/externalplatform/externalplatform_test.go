package externalplatform

import (
	"testing"

	. "github.com/onsi/gomega"

	hyperv1 "github.com/openshift/hypershift/api/hypershift/v1beta1"

	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

const (
	testAPIGroup          = "example.io"
	testTemplateResource  = "foohostedclustertemplates"
	testTemplateKind      = "FooHostedClusterTemplate"
	testHostedClusterKind = "FooHostedCluster"
)

var (
	templateGVK      = schema.GroupVersionKind{Group: testAPIGroup, Version: "v1alpha1", Kind: testTemplateKind}
	hostedClusterGVK = schema.GroupVersionKind{Group: testAPIGroup, Version: "v1alpha1", Kind: testHostedClusterKind}
)

func testRef() hyperv1.ExternalTemplateReference {
	return hyperv1.ExternalTemplateReference{
		APIGroup: testAPIGroup,
		Resource: testTemplateResource,
		Name:     "my-infra",
	}
}

// testRESTMapper maps the integrator's types the way a management cluster with the
// integrator's CRDs installed would.
func testRESTMapper() meta.RESTMapper {
	mapper := meta.NewDefaultRESTMapper([]schema.GroupVersion{templateGVK.GroupVersion()})
	for _, gvk := range []schema.GroupVersionKind{templateGVK, hostedClusterGVK} {
		mapper.Add(gvk, meta.RESTScopeNamespace)
	}
	return mapper
}

func testClient(objects ...client.Object) client.Client {
	scheme := runtime.NewScheme()
	for _, gvk := range []schema.GroupVersionKind{templateGVK, hostedClusterGVK} {
		scheme.AddKnownTypeWithName(gvk, &unstructured.Unstructured{})
		listGVK := gvk
		listGVK.Kind += "List"
		scheme.AddKnownTypeWithName(listGVK, &unstructured.UnstructuredList{})
	}
	return fake.NewClientBuilder().
		WithScheme(scheme).
		WithRESTMapper(testRESTMapper()).
		WithObjects(objects...).
		Build()
}

func hostedClusterObject(platform map[string]any) *unstructured.Unstructured {
	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(hostedClusterGVK)
	obj.SetNamespace("clusters-example")
	obj.SetName("example")
	if platform != nil {
		obj.Object["status"] = map[string]any{"platform": platform}
	}
	return obj
}

func TestHostedClusterObjectGVK(t *testing.T) {
	t.Parallel()

	testCases := []struct {
		name      string
		resource  string
		want      schema.GroupVersionKind
		expectErr bool
	}{
		{
			name:     "When the resource is a template, it should strip the suffix",
			resource: testTemplateResource,
			want:     hostedClusterGVK,
		},
		{
			name:      "When the resource is not a template, it should return an error",
			resource:  "foohostedclusters",
			expectErr: true,
		},
		{
			name:      "When the derived resource is not installed, it should return an error",
			resource:  "barhostedclustertemplates",
			expectErr: true,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			g := NewWithT(t)

			ref := testRef()
			ref.Resource = tc.resource
			got, err := HostedClusterObjectGVK(testRESTMapper(), ref)
			if tc.expectErr {
				g.Expect(err).To(HaveOccurred())
				return
			}
			g.Expect(err).ToNot(HaveOccurred())
			g.Expect(got).To(Equal(tc.want))
		})
	}
}

func TestGetHostedClusterObject(t *testing.T) {
	t.Parallel()

	key := client.ObjectKey{Namespace: "clusters-example", Name: "example"}

	t.Run("When the object exists, it should be returned with the instantiated kind", func(t *testing.T) {
		t.Parallel()
		g := NewWithT(t)

		got, err := GetHostedClusterObject(t.Context(), testClient(hostedClusterObject(nil)), testRef(), key)
		g.Expect(err).ToNot(HaveOccurred())
		g.Expect(got.GroupVersionKind()).To(Equal(hostedClusterGVK))
		g.Expect(got.GetName()).To(Equal("example"))
	})

	t.Run("When the object does not exist, it should return an error", func(t *testing.T) {
		t.Parallel()
		g := NewWithT(t)

		_, err := GetHostedClusterObject(t.Context(), testClient(), testRef(), key)
		g.Expect(err).To(HaveOccurred())
	})

	t.Run("When the resource does not resolve, it should return an error", func(t *testing.T) {
		t.Parallel()
		g := NewWithT(t)

		ref := testRef()
		ref.Resource = "barhostedclustertemplates"
		_, err := GetHostedClusterObject(t.Context(), testClient(hostedClusterObject(nil)), ref, key)
		g.Expect(err).To(HaveOccurred())
	})
}

func TestDeclaration(t *testing.T) {
	t.Parallel()

	testCases := []struct {
		name      string
		platform  map[string]any
		want      *hyperv1.ExternalPlatformStatus
		expectErr bool
	}{
		{
			name: "When the integrator declares a cloud controller manager, it should be read",
			platform: map[string]any{
				"name":                   "ExampleCloud",
				"cloudControllerManager": map[string]any{"state": "External"},
			},
			want: &hyperv1.ExternalPlatformStatus{
				Name: "ExampleCloud",
				CloudControllerManager: hyperv1.ExternalCloudControllerManagerStatus{
					State: hyperv1.ExternalCloudControllerManager,
				},
			},
		},
		{
			name: "When the integrator declares no cloud controller manager, it should be read",
			platform: map[string]any{
				"name":                   "ExampleCloud",
				"cloudControllerManager": map[string]any{"state": "None"},
			},
			want: &hyperv1.ExternalPlatformStatus{
				Name: "ExampleCloud",
				CloudControllerManager: hyperv1.ExternalCloudControllerManagerStatus{
					State: hyperv1.NoCloudControllerManager,
				},
			},
		},
		{
			// The normal state early in provisioning, and the one thing here that must not
			// look like a failure.
			name:     "When nothing has been declared, it should report nothing",
			platform: nil,
		},
		{
			name:     "When the declaration is an empty block, it should report nothing",
			platform: map[string]any{},
		},
		{
			name:      "When only the name has been declared, it should return an error",
			platform:  map[string]any{"name": "ExampleCloud"},
			expectErr: true,
		},
		{
			name:      "When only the cloud controller manager has been declared, it should return an error",
			platform:  map[string]any{"cloudControllerManager": map[string]any{"state": "External"}},
			expectErr: true,
		},
		{
			name: "When the cloud controller manager state is not a contract value, it should return an error",
			platform: map[string]any{
				"name":                   "ExampleCloud",
				"cloudControllerManager": map[string]any{"state": "Internal"},
			},
			expectErr: true,
		},
		{
			name: "When the declaration is not the shape the contract asks for, it should return an error",
			platform: map[string]any{
				"name":                   "ExampleCloud",
				"cloudControllerManager": "External",
			},
			expectErr: true,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			g := NewWithT(t)

			got, err := Declaration(hostedClusterObject(tc.platform))
			if tc.expectErr {
				g.Expect(err).To(HaveOccurred())
				return
			}
			g.Expect(err).ToNot(HaveOccurred())
			g.Expect(got).To(Equal(tc.want))
		})
	}
}
