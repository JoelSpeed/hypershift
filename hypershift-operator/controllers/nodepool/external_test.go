package nodepool

import (
	"encoding/json"
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
	externalMachineTemplateGroup    = "infrastructure.cluster.x-k8s.io"
	externalMachineTemplateResource = "foomachinetemplates"
	externalMachineTemplateKind     = "FooMachineTemplate"
)

var externalMachineTemplateTestGVK = schema.GroupVersionKind{
	Group:   externalMachineTemplateGroup,
	Version: "v1beta1",
	Kind:    externalMachineTemplateKind,
}

func externalTestNodePool() *hyperv1.NodePool {
	return &hyperv1.NodePool{
		ObjectMeta: metav1.ObjectMeta{Name: "test", Namespace: "clusters"},
		Spec: hyperv1.NodePoolSpec{
			Platform: hyperv1.NodePoolPlatform{
				Type: hyperv1.ExternalPlatform,
				External: hyperv1.ExternalNodePoolPlatform{
					MachineTemplate: hyperv1.ExternalTemplateReference{
						APIGroup: externalMachineTemplateGroup,
						Resource: externalMachineTemplateResource,
						Name:     "my-machines",
					},
				},
			},
		},
	}
}

// externalSourceTemplate is the object the user wrote and the NodePool points at. It lives
// in the NodePool's own namespace, not the control plane namespace.
func externalSourceTemplate(instanceType string) *unstructured.Unstructured {
	source := &unstructured.Unstructured{}
	source.SetGroupVersionKind(externalMachineTemplateTestGVK)
	source.SetNamespace("clusters")
	source.SetName("my-machines")
	source.Object["spec"] = map[string]any{
		"template": map[string]any{
			"spec": map[string]any{
				"instanceType": instanceType,
			},
		},
	}
	return source
}

func externalTestNodePoolScheme() *runtime.Scheme {
	scheme := runtime.NewScheme()
	scheme.AddKnownTypeWithName(externalMachineTemplateTestGVK, &unstructured.Unstructured{})
	listGVK := externalMachineTemplateTestGVK
	listGVK.Kind += "List"
	scheme.AddKnownTypeWithName(listGVK, &unstructured.UnstructuredList{})
	return scheme
}

// externalTestRESTMapper maps the integrator's machine template the way a management
// cluster with its CRD installed would. Passing no group versions leaves the mapper unable
// to serve the integrator's kinds, which is how an uninstalled CRD presents.
func externalTestRESTMapper(installed bool) meta.RESTMapper {
	mapper := meta.NewDefaultRESTMapper([]schema.GroupVersion{externalMachineTemplateTestGVK.GroupVersion()})
	if installed {
		mapper.Add(externalMachineTemplateTestGVK, meta.RESTScopeNamespace)
	}
	return mapper
}

func externalTestCAPI(nodePool *hyperv1.NodePool, uncachedClient client.Client) *CAPI {
	return &CAPI{
		Token: &Token{
			ConfigGenerator: &ConfigGenerator{
				Client:                uncachedClient,
				nodePool:              nodePool,
				controlplaneNamespace: "clusters-example",
			},
		},
		capiClusterName: "example-abcde",
		uncachedClient:  uncachedClient,
	}
}

func externalTestClient(installed bool, objects ...client.Object) client.Client {
	return fake.NewClientBuilder().
		WithScheme(externalTestNodePoolScheme()).
		WithRESTMapper(externalTestRESTMapper(installed)).
		WithObjects(objects...).
		Build()
}

// expectedExternalTemplateName is the name the in-tree platforms would produce for the same
// spec, and is computed here the same way so the test fails if the External path ever stops
// hashing what they hash.
func expectedExternalTemplateName(t *testing.T, nodePool *hyperv1.NodePool, spec map[string]any) string {
	t.Helper()
	specJSON, err := json.Marshal(spec)
	if err != nil {
		t.Fatalf("failed to marshal spec: %v", err)
	}
	return generateMachineTemplateName(nodePool, specJSON)
}

func TestExternalMachineTemplate(t *testing.T) {
	t.Parallel()

	t.Run("When the referenced template exists, it should instantiate it into the control plane namespace", func(t *testing.T) {
		t.Parallel()
		g := NewWithT(t)

		nodePool := externalTestNodePool()
		source := externalSourceTemplate("m5.large")
		capi := externalTestCAPI(nodePool, externalTestClient(true, source))

		// Through machineTemplateBuilders rather than externalMachineTemplate directly, so
		// the namespace and the NodePool annotation the shared code adds are covered too.
		template, err := capi.machineTemplateBuilders(t.Context())
		g.Expect(err).ToNot(HaveOccurred())

		g.Expect(template.GetObjectKind().GroupVersionKind()).To(Equal(externalMachineTemplateTestGVK))
		g.Expect(template.GetNamespace()).To(Equal("clusters-example"))
		g.Expect(template.GetAnnotations()).To(HaveKeyWithValue(nodePoolAnnotation, "clusters/test"))

		spec, _, err := unstructured.NestedMap(source.Object, "spec")
		g.Expect(err).ToNot(HaveOccurred())
		g.Expect(template.GetName()).To(Equal(expectedExternalTemplateName(t, nodePool, spec)))

		instantiated, ok := template.(*unstructured.Unstructured)
		g.Expect(ok).To(BeTrue())
		g.Expect(instantiated.Object["spec"]).To(Equal(spec))
	})

	t.Run("When only the referenced template's metadata changes, it should keep the same name", func(t *testing.T) {
		t.Parallel()
		g := NewWithT(t)

		nodePool := externalTestNodePool()

		source := externalSourceTemplate("m5.large")
		before, err := externalTestCAPI(nodePool, externalTestClient(true, source)).machineTemplateBuilders(t.Context())
		g.Expect(err).ToNot(HaveOccurred())

		// Everything a write can touch without touching the spec. A rolling upgrade here
		// would replace every node in the pool for nothing.
		touched := externalSourceTemplate("m5.large")
		touched.SetLabels(map[string]string{"example.io/owner": "someone"})
		touched.SetAnnotations(map[string]string{"example.io/note": "retagged"})
		touched.SetResourceVersion("99999")
		touched.SetGeneration(17)
		touched.Object["status"] = map[string]any{"capacity": map[string]any{"cpu": "4"}}

		after, err := externalTestCAPI(nodePool, externalTestClient(true, touched)).machineTemplateBuilders(t.Context())
		g.Expect(err).ToNot(HaveOccurred())

		g.Expect(after.GetName()).To(Equal(before.GetName()))
	})

	t.Run("When the referenced template's spec changes, it should produce a new name", func(t *testing.T) {
		t.Parallel()
		g := NewWithT(t)

		nodePool := externalTestNodePool()

		before, err := externalTestCAPI(nodePool, externalTestClient(true, externalSourceTemplate("m5.large"))).machineTemplateBuilders(t.Context())
		g.Expect(err).ToNot(HaveOccurred())

		after, err := externalTestCAPI(nodePool, externalTestClient(true, externalSourceTemplate("m5.xlarge"))).machineTemplateBuilders(t.Context())
		g.Expect(err).ToNot(HaveOccurred())

		g.Expect(after.GetName()).ToNot(Equal(before.GetName()))
	})

	t.Run("When the referenced template does not exist, it should return an error", func(t *testing.T) {
		t.Parallel()
		g := NewWithT(t)

		capi := externalTestCAPI(externalTestNodePool(), externalTestClient(true))
		_, err := capi.machineTemplateBuilders(t.Context())
		g.Expect(err).To(HaveOccurred())
	})

	t.Run("When the referenced template has no spec, it should return an error", func(t *testing.T) {
		t.Parallel()
		g := NewWithT(t)

		source := externalSourceTemplate("m5.large")
		delete(source.Object, "spec")

		capi := externalTestCAPI(externalTestNodePool(), externalTestClient(true, source))
		_, err := capi.machineTemplateBuilders(t.Context())
		g.Expect(err).To(HaveOccurred())
	})

	t.Run("When the integrator's CRD is not installed, it should return an error", func(t *testing.T) {
		t.Parallel()
		g := NewWithT(t)

		capi := externalTestCAPI(externalTestNodePool(), externalTestClient(false))
		_, err := capi.machineTemplateBuilders(t.Context())
		g.Expect(err).To(HaveOccurred())
	})

	t.Run("When no uncached client is configured, it should return an error", func(t *testing.T) {
		t.Parallel()
		g := NewWithT(t)

		nodePool := externalTestNodePool()
		capi := externalTestCAPI(nodePool, externalTestClient(true, externalSourceTemplate("m5.large")))
		// A cached read of an integrator defined kind would start an informer on a CRD that
		// may not exist, so failing loudly is the only safe behavior.
		capi.uncachedClient = nil

		_, err := capi.machineTemplateBuilders(t.Context())
		g.Expect(err).To(HaveOccurred())
	})
}

func TestListMachineTemplatesExternal(t *testing.T) {
	t.Parallel()

	nodePool := externalTestNodePool()
	nodePoolKey := client.ObjectKeyFromObject(nodePool).String()

	instantiated := func(namespace, name string, annotations map[string]string) *unstructured.Unstructured {
		obj := &unstructured.Unstructured{}
		obj.SetGroupVersionKind(externalMachineTemplateTestGVK)
		obj.SetNamespace(namespace)
		obj.SetName(name)
		obj.SetAnnotations(annotations)
		return obj
	}

	t.Run("When templates exist, it should return only this NodePool's, in the control plane namespace", func(t *testing.T) {
		t.Parallel()
		g := NewWithT(t)

		capi := externalTestCAPI(nodePool, externalTestClient(true,
			instantiated("clusters-example", "mine", map[string]string{nodePoolAnnotation: nodePoolKey}),
			instantiated("clusters-example", "someone-elses", map[string]string{nodePoolAnnotation: "clusters/other"}),
			instantiated("clusters-example", "unannotated", nil),
			// The template the user wrote. It shares the kind, and a copied annotation is
			// all that would stand between it and deletion if the list were cluster wide.
			instantiated("clusters", "my-machines", map[string]string{nodePoolAnnotation: nodePoolKey}),
		))

		templates, err := capi.listMachineTemplates()
		g.Expect(err).ToNot(HaveOccurred())
		g.Expect(templates).To(HaveLen(1))
		g.Expect(templates[0].GetName()).To(Equal("mine"))
		g.Expect(templates[0].GetNamespace()).To(Equal("clusters-example"))
	})

	t.Run("When the integrator's CRD is not installed, it should report nothing rather than fail", func(t *testing.T) {
		t.Parallel()
		g := NewWithT(t)

		// The only caller is NodePool deletion. Uninstalling the integrator must not leave
		// NodePools undeletable.
		capi := externalTestCAPI(nodePool, externalTestClient(false))

		templates, err := capi.listMachineTemplates()
		g.Expect(err).ToNot(HaveOccurred())
		g.Expect(templates).To(BeEmpty())
	})
}

func TestMachineTemplateAPIVersion(t *testing.T) {
	t.Parallel()

	testCases := []struct {
		name        string
		group       string
		kind        string
		installed   bool
		expected    string
		expectError bool
	}{
		{
			name:      "When the kind is compiled in, it should come from the scheme",
			group:     "infrastructure.cluster.x-k8s.io",
			kind:      "AWSMachineTemplate",
			installed: false,
			expected:  "infrastructure.cluster.x-k8s.io/v1beta2",
		},
		{
			name:      "When the kind is the integrator's, it should come from the management cluster",
			group:     externalMachineTemplateGroup,
			kind:      externalMachineTemplateKind,
			installed: true,
			expected:  "infrastructure.cluster.x-k8s.io/v1beta1",
		},
		{
			name:        "When the kind resolves nowhere, it should return an error",
			group:       externalMachineTemplateGroup,
			kind:        "NotInstalledMachineTemplate",
			installed:   true,
			expectError: true,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			g := NewWithT(t)

			capi := externalTestCAPI(externalTestNodePool(), externalTestClient(tc.installed))
			apiVersion, err := capi.machineTemplateAPIVersion(tc.group, tc.kind)
			if tc.expectError {
				g.Expect(err).To(HaveOccurred())
				return
			}
			g.Expect(err).ToNot(HaveOccurred())
			g.Expect(apiVersion).To(Equal(tc.expected))
		})
	}
}
