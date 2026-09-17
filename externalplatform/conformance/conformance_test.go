package conformance

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	hyperv1 "github.com/openshift/hypershift/api/hypershift/v1beta1"
	"github.com/openshift/hypershift/externalplatform/contract"

	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
)

const (
	testAPIGroup         = "example.io"
	testVersion          = "v1alpha1"
	testTemplateResource = "foohostedclustertemplates"
	testInstanceResource = "foohostedclusters"
	testFinalizer        = "example.io/external-platform"
)

var (
	testTemplateGVK = schema.GroupVersionKind{Group: testAPIGroup, Version: testVersion, Kind: "FooHostedClusterTemplate"}
	testInstanceGVK = schema.GroupVersionKind{Group: testAPIGroup, Version: testVersion, Kind: "FooHostedCluster"}
)

func testScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	scheme := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{
		corev1.AddToScheme,
		apiextensionsv1.AddToScheme,
		hyperv1.AddToScheme,
	} {
		if err := add(scheme); err != nil {
			t.Fatalf("unexpected error building the scheme: %v", err)
		}
	}
	for _, gvk := range []schema.GroupVersionKind{testTemplateGVK, testInstanceGVK} {
		scheme.AddKnownTypeWithName(gvk, &unstructured.Unstructured{})
		scheme.AddKnownTypeWithName(gvk.GroupVersion().WithKind(gvk.Kind+"List"), &unstructured.UnstructuredList{})
	}
	metav1.AddToGroupVersion(scheme, testInstanceGVK.GroupVersion())
	return scheme
}

func testRESTMapper() meta.RESTMapper {
	mapper := meta.NewDefaultRESTMapper([]schema.GroupVersion{testInstanceGVK.GroupVersion()})
	mapper.Add(testTemplateGVK, meta.RESTScopeNamespace)
	mapper.Add(testInstanceGVK, meta.RESTScopeNamespace)
	return mapper
}

// customResourceDefinition builds a compliant pair of definitions, which each test then
// breaks in exactly one way.
func customResourceDefinitions() []client.Object {
	definition := func(resource, kind, contractVersion string, statusSubresource bool) *apiextensionsv1.CustomResourceDefinition {
		crd := &apiextensionsv1.CustomResourceDefinition{
			ObjectMeta: metav1.ObjectMeta{Name: resource + "." + testAPIGroup},
			Spec: apiextensionsv1.CustomResourceDefinitionSpec{
				Group: testAPIGroup,
				Names: apiextensionsv1.CustomResourceDefinitionNames{Plural: resource, Kind: kind},
				Versions: []apiextensionsv1.CustomResourceDefinitionVersion{{
					Name:   testVersion,
					Served: true,
				}},
			},
		}
		if contractVersion != "" {
			crd.Labels = map[string]string{contract.VersionLabel: contractVersion}
		}
		if statusSubresource {
			crd.Spec.Versions[0].Subresources = &apiextensionsv1.CustomResourceSubresources{
				Status: &apiextensionsv1.CustomResourceSubresourceStatus{},
			}
		}
		return crd
	}
	return []client.Object{
		definition(testTemplateResource, testTemplateGVK.Kind, contract.Version, false),
		definition(testInstanceResource, testInstanceGVK.Kind, contract.Version, true),
	}
}

// provisionedObject is what a compliant provider leaves behind: declared, named, finalized
// and ready.
func provisionedObject() *unstructured.Unstructured {
	object := &unstructured.Unstructured{}
	object.SetGroupVersionKind(testInstanceGVK)
	object.SetNamespace("clusters-conformance")
	object.SetName("conformance")
	object.SetFinalizers([]string{testFinalizer})
	if err := contract.SetPlatform(object, "ExampleCloud", hyperv1.ExternalCloudControllerManager); err != nil {
		panic(err)
	}
	if err := contract.SetInfrastructure(object, "infrastructure.cluster.x-k8s.io", "FooCluster", "conformance"); err != nil {
		panic(err)
	}
	if err := contract.SetReady(object, metav1.ConditionTrue, "AsExpected", "Infrastructure is ready"); err != nil {
		panic(err)
	}
	return object
}

func newSuite(t *testing.T, objects ...client.Object) *Suite {
	t.Helper()

	instance := &unstructured.Unstructured{}
	instance.SetGroupVersionKind(testInstanceGVK)

	c := fake.NewClientBuilder().
		WithScheme(testScheme(t)).
		WithRESTMapper(testRESTMapper()).
		WithObjects(objects...).
		WithStatusSubresource(instance).
		Build()

	suite, err := New(Options{
		Client:           c,
		APIGroup:         testAPIGroup,
		TemplateResource: testTemplateResource,
		Spec:             map[string]any{"region": "eu-west-1"},
		// Short enough that a failing check is a fast test, long enough that a compliant one
		// is not a flake.
		Timeout:   2 * time.Second,
		Interval:  5 * time.Millisecond,
		SettleFor: 50 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("unexpected error building the suite: %v", err)
	}
	return suite
}

func expectError(t *testing.T, err error, substring string) {
	t.Helper()
	if err == nil {
		t.Fatalf("expected an error mentioning %q, got none", substring)
	}
	if !strings.Contains(err.Error(), substring) {
		t.Errorf("expected an error mentioning %q, got %q", substring, err.Error())
	}
}

func TestCheckTypesAcceptsCompliantDefinitions(t *testing.T) {
	suite := newSuite(t, customResourceDefinitions()...)
	if err := suite.CheckTypes(context.Background()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestCheckTypesRequiresTheContractVersionLabel(t *testing.T) {
	definitions := customResourceDefinitions()
	definitions[0].SetLabels(nil)

	suite := newSuite(t, definitions...)
	expectError(t, suite.CheckTypes(context.Background()), contract.VersionLabel)
}

func TestCheckTypesRejectsASkewedContractVersion(t *testing.T) {
	definitions := customResourceDefinitions()
	definitions[0].SetLabels(map[string]string{contract.VersionLabel: "v1beta7"})

	suite := newSuite(t, definitions...)
	expectError(t, suite.CheckTypes(context.Background()), "v1beta7")
}

func TestCheckTypesRequiresAStatusSubresourceOnTheInstantiatedType(t *testing.T) {
	// Everything the contract asks the integrator to publish lives under status, and a type
	// without the subresource lets the provider overwrite the endpoint HyperShift wrote.
	definitions := customResourceDefinitions()
	definitions[1].(*apiextensionsv1.CustomResourceDefinition).Spec.Versions[0].Subresources = nil

	suite := newSuite(t, definitions...)
	expectError(t, suite.CheckTypes(context.Background()), "status subresource")
}

func TestSetupCreatesWhatHyperShiftWould(t *testing.T) {
	suite := newSuite(t)
	if err := suite.Setup(context.Background()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	object, err := suite.HostedClusterObject(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	endpoint, err := contract.ControlPlaneEndpoint(object)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if endpoint.Host == "" || endpoint.Port == 0 {
		t.Errorf("expected an endpoint to be published, got %v", endpoint)
	}
	if region, _, _ := unstructured.NestedString(object.Object, "spec", "region"); region != "eu-west-1" {
		t.Errorf("expected the integrator's own spec to be copied, got %q", region)
	}
	if object.GetLabels()[contract.PlatformGroupLabel] != testAPIGroup {
		t.Errorf("expected the platform group label, got %v", object.GetLabels())
	}

	hcp := &hyperv1.HostedControlPlane{}
	if err := suite.Client.Get(context.Background(), client.ObjectKey{Namespace: suite.Namespace, Name: suite.Name}, hcp); err != nil {
		t.Errorf("expected a hosted control plane to be created: %v", err)
	}
}

func TestCheckProvisioningAcceptsACompliantProvider(t *testing.T) {
	suite := newSuite(t, provisionedObject())
	if err := suite.CheckProvisioning(context.Background()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestCheckProvisioningCatchesReadyBeforeDeclared(t *testing.T) {
	// The failure this suite exists for: the cluster looks provisioned and no node can boot.
	object := provisionedObject()
	unstructured.RemoveNestedField(object.Object, "status", "platform")

	suite := newSuite(t, object)
	expectError(t, suite.CheckProvisioning(context.Background()), "status.platform")
}

func TestCheckProvisioningCatchesAMissingInfrastructureReference(t *testing.T) {
	object := provisionedObject()
	unstructured.RemoveNestedField(object.Object, "status", "infrastructure")

	suite := newSuite(t, object)
	expectError(t, suite.CheckProvisioning(context.Background()), "status.infrastructure")
}

func TestCheckProvisioningCatchesAMissingFinalizer(t *testing.T) {
	object := provisionedObject()
	object.SetFinalizers(nil)

	suite := newSuite(t, object)
	expectError(t, suite.CheckProvisioning(context.Background()), "no finalizer")
}

func TestCheckProvisioningCatchesAConditionWithNoReason(t *testing.T) {
	object := provisionedObject()
	if err := contract.SetReady(object, metav1.ConditionFalse, "", "Working on it"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	suite := newSuite(t, object)
	expectError(t, suite.CheckProvisioning(context.Background()), "no reason")
}

func TestCheckProvisioningTimesOutOnAProviderThatNeverReports(t *testing.T) {
	object := provisionedObject()
	unstructured.RemoveNestedField(object.Object, "status", "conditions")

	suite := newSuite(t, object)
	expectError(t, suite.CheckProvisioning(context.Background()), "timed out")
}

func TestCheckSettledAcceptsAProviderThatHasStopped(t *testing.T) {
	suite := newSuite(t, provisionedObject())
	if err := suite.CheckSettled(context.Background()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestCheckSettledCatchesAMovingLastTransitionTime(t *testing.T) {
	// A provider that rewrites lastTransitionTime every pass makes a cluster that has been
	// stuck for an hour look like it just got there.
	suite := newSuite(t, provisionedObject())

	stop := rewriteReadyContinuously(t, suite)
	defer stop()

	expectError(t, suite.CheckSettled(context.Background()), "lastTransitionTime moved")
}

func TestCheckSettledCatchesAChangedDeclaration(t *testing.T) {
	suite := newSuite(t, provisionedObject())

	object, err := suite.HostedClusterObject(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	go func() {
		time.Sleep(10 * time.Millisecond)
		if err := contract.SetPlatform(object, "SomethingElse", hyperv1.NoCloudControllerManager); err != nil {
			return
		}
		_ = suite.Client.Status().Update(context.Background(), object)
	}()

	expectError(t, suite.CheckSettled(context.Background()), "immutable once observed")
}

func TestCheckDeprovisioningAcceptsAProviderThatReleasesItsFinalizer(t *testing.T) {
	suite := newSuite(t, provisionedObject())

	stop := releaseFinalizerOnDeletion(t, suite)
	defer stop()

	if err := suite.CheckDeprovisioning(context.Background()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestCheckDeprovisioningCatchesAStuckFinalizer(t *testing.T) {
	// The worst failure in the contract: the HostedCluster cannot be deleted without an
	// administrator stripping a finalizer by hand.
	suite := newSuite(t, provisionedObject())
	expectError(t, suite.CheckDeprovisioning(context.Background()), "timed out")
}

func TestNewRejectsATemplateThatCannotBeResolved(t *testing.T) {
	_, err := New(Options{
		Client: fake.NewClientBuilder().WithScheme(testScheme(t)).WithRESTMapper(testRESTMapper()).Build(),

		APIGroup:         testAPIGroup,
		TemplateResource: "foohostedclusters",
	})
	expectError(t, err, "must end in \"templates\"")
}

func TestRunReportsTheFirstFailingCheck(t *testing.T) {
	recorder := &recordingT{}
	Run(recorder, context.Background(), Options{
		Client: fake.NewClientBuilder().
			WithScheme(testScheme(t)).
			WithRESTMapper(testRESTMapper()).
			Build(),
		APIGroup:         testAPIGroup,
		TemplateResource: testTemplateResource,
		Timeout:          100 * time.Millisecond,
		Interval:         5 * time.Millisecond,
	})

	if recorder.fatal == "" {
		t.Fatal("expected the run to fail with no custom resource definitions installed")
	}
	if !strings.Contains(recorder.fatal, "Types") {
		t.Errorf("expected the Types check to be named as the failure, got %q", recorder.fatal)
	}
}

// recordingT is a TestingT that records rather than fails, so that the suite's own failure
// paths can be tested.
type recordingT struct {
	fatal string
	errs  []string
}

func (r *recordingT) Helper()                         {}
func (r *recordingT) Logf(format string, args ...any) {}
func (r *recordingT) Errorf(format string, args ...any) {
	r.errs = append(r.errs, sprintf(format, args...))
}
func (r *recordingT) Fatalf(format string, args ...any) {
	if r.fatal == "" {
		r.fatal = sprintf(format, args...)
	}
}

func sprintf(format string, args ...any) string {
	return fmt.Sprintf(format, args...)
}

// rewriteReadyContinuously simulates a provider that reconciles in a loop and rewrites its
// condition every time.
func rewriteReadyContinuously(t *testing.T, suite *Suite) func() {
	t.Helper()
	done := make(chan struct{})
	go func() {
		for {
			select {
			case <-done:
				return
			case <-time.After(5 * time.Millisecond):
			}
			object, err := suite.HostedClusterObject(context.Background())
			if err != nil {
				continue
			}
			conditions, _, _ := unstructured.NestedSlice(object.Object, "status", "conditions")
			for i, item := range conditions {
				entry, ok := item.(map[string]any)
				if !ok {
					continue
				}
				entry["lastTransitionTime"] = metav1.Now().UTC().Add(time.Duration(i+1) * time.Hour).Format(time.RFC3339)
				conditions[i] = entry
			}
			_ = unstructured.SetNestedSlice(object.Object, conditions, "status", "conditions")
			_ = suite.Client.Status().Update(context.Background(), object)
		}
	}()
	return func() { close(done) }
}

// releaseFinalizerOnDeletion simulates a provider that tears down promptly.
func releaseFinalizerOnDeletion(t *testing.T, suite *Suite) func() {
	t.Helper()
	done := make(chan struct{})
	go func() {
		for {
			select {
			case <-done:
				return
			case <-time.After(5 * time.Millisecond):
			}
			object, err := suite.HostedClusterObject(context.Background())
			if err != nil || object.GetDeletionTimestamp().IsZero() {
				continue
			}
			if controllerutil.RemoveFinalizer(object, testFinalizer) {
				_ = suite.Client.Update(context.Background(), object)
			}
		}
	}()
	return func() { close(done) }
}
