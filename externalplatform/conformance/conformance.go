// Package conformance is the suite an integrator runs in its own CI to prove its controller
// implements the External platform contract.
//
// Version skew between HyperShift and an independently released integrator produces silent
// misbehaviour rather than a clean error: a provider that reports ready before declaring its
// platform yields a cluster that cannot boot a node, a provider that never sets the
// infrastructure reference yields a control plane with no Cluster API Cluster, and a provider
// that drops its finalizer early leaks whatever it provisioned. Every one of those looks like
// a HyperShift bug from the outside. This suite catches them in the integrator's CI instead,
// against nothing but an API server.
//
// The suite plays HyperShift's half of the contract. It creates the control plane namespace,
// the HostedControlPlane and the hosted cluster object HyperShift would have instantiated
// from the integrator's template, waits for the integrator's controller to do its work, and
// asserts the contract held at every step. It needs no HyperShift Operator, no control plane,
// and no cloud.
//
//	func TestConformance(t *testing.T) {
//		conformance.Run(t, context.Background(), conformance.Options{
//			Client:           managementClient, // envtest, or a real cluster
//			APIGroup:         "example.io",
//			TemplateResource: "foohostedclustertemplates",
//			Spec:             map[string]any{"region": "eu-west-1"},
//		})
//	}
//
// The client's scheme must contain the HyperShift API types and
// apiextensions.k8s.io/v1, and the cluster must have the integrator's own custom resource
// definitions and HyperShift's HostedControlPlane definition installed. The integrator's
// controller must be running against the same cluster.
package conformance

import (
	"context"
	"fmt"
	"time"

	hyperv1 "github.com/openshift/hypershift/api/hypershift/v1beta1"
	"github.com/openshift/hypershift/externalplatform/contract"

	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"sigs.k8s.io/controller-runtime/pkg/client"
)

// Options configures a run of the suite.
type Options struct {
	// Client reaches the management cluster the integrator's controller is watching, with
	// enough privilege to create namespaces, HostedControlPlanes and the integrator's own
	// types. Its RESTMapper resolves the integrator's resources, so it must be built against
	// a cluster that already has their custom resource definitions installed.
	Client client.Client

	// APIGroup and TemplateResource name the integrator's hosted cluster template exactly as
	// a HostedCluster would, so that a suite that passes proves the reference a user will
	// write also resolves.
	APIGroup         string
	TemplateResource string

	// Spec is the spec HyperShift would have copied out of the template. It is the
	// integrator's own type, so the suite cannot supply a meaningful one. Whatever a minimal
	// valid cluster needs goes here.
	Spec map[string]any

	// HostedControlPlane is created alongside the hosted cluster object, because a provider
	// reads it to find the release version and the paused state. Defaulted by
	// DefaultHostedControlPlane when nil; supply one when the default does not satisfy the
	// installed custom resource definition.
	HostedControlPlane *hyperv1.HostedControlPlane

	// Namespace and Name stand in for the control plane namespace and the HostedCluster name.
	// Defaulted to a name unlikely to collide with anything real.
	Namespace string
	Name      string

	// ControlPlaneEndpoint is what HyperShift would have written onto the object. Defaulted
	// to a plausible service network endpoint; nothing has to answer on it.
	ControlPlaneEndpoint hyperv1.APIEndpoint

	// Timeout bounds each wait. Defaulted to ten minutes, which is generous for a real cloud
	// and immaterial for a provider that does nothing.
	Timeout time.Duration

	// Interval is how often the suite polls. Defaulted to two seconds.
	Interval time.Duration

	// SettleFor is how long the suite watches a ready cluster to prove the provider has
	// actually settled rather than reconciling in a loop. Defaulted to thirty seconds.
	SettleFor time.Duration
}

// TestingT is the part of *testing.T the suite uses, so that this package does not import
// testing and register its flags in an integrator's binary. Ginkgo's GinkgoT() satisfies it
// too.
type TestingT interface {
	Helper()
	Logf(format string, args ...any)
	Errorf(format string, args ...any)
	Fatalf(format string, args ...any)
}

// Suite is a configured run. Use Run unless the checks need to be driven individually, which
// is worth doing only when an integrator is working through a failure.
type Suite struct {
	Options

	templateGVK schema.GroupVersionKind
	instanceGVK schema.GroupVersionKind
}

// New validates the options and resolves the integrator's types.
func New(opts Options) (*Suite, error) {
	if opts.Client == nil {
		return nil, fmt.Errorf("a Client is required")
	}
	// Checked here rather than left to fail inside a check, because the failure it produces
	// there is "no kind is registered for the type v1.CustomResourceDefinition", which reads
	// like a bug in the suite rather than a line missing from the caller's scheme.
	if !opts.Client.Scheme().Recognizes(apiextensionsv1.SchemeGroupVersion.WithKind("CustomResourceDefinition")) {
		return nil, fmt.Errorf("the Client's scheme must include k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1: the suite reads your custom resource definitions to check the contract version label and the status subresource")
	}
	if opts.APIGroup == "" {
		return nil, fmt.Errorf("an APIGroup is required")
	}
	if opts.TemplateResource == "" {
		return nil, fmt.Errorf("a TemplateResource is required")
	}
	if opts.Namespace == "" {
		opts.Namespace = "clusters-conformance"
	}
	if opts.Name == "" {
		opts.Name = "conformance"
	}
	if opts.ControlPlaneEndpoint == (hyperv1.APIEndpoint{}) {
		opts.ControlPlaneEndpoint = hyperv1.APIEndpoint{
			Host: fmt.Sprintf("kube-apiserver.%s.svc.cluster.local", opts.Namespace),
			Port: 6443,
		}
	}
	if opts.Timeout == 0 {
		opts.Timeout = 10 * time.Minute
	}
	if opts.Interval == 0 {
		opts.Interval = 2 * time.Second
	}
	if opts.SettleFor == 0 {
		opts.SettleFor = 30 * time.Second
	}
	if opts.HostedControlPlane == nil {
		opts.HostedControlPlane = DefaultHostedControlPlane(opts.Namespace, opts.Name)
	}

	suite := &Suite{Options: opts}

	mapper := opts.Client.RESTMapper()
	var err error
	if suite.templateGVK, err = contract.KindFor(mapper, opts.APIGroup, opts.TemplateResource); err != nil {
		return nil, err
	}
	if suite.instanceGVK, err = contract.InstanceGVK(mapper, opts.APIGroup, opts.TemplateResource); err != nil {
		return nil, err
	}
	return suite, nil
}

// Run executes every check in order and fails the test on the first one that does not hold.
//
// The checks are sequential rather than independent because each builds on the state the
// previous one left: there is no point asserting a declaration is stable before anything has
// declared one.
func Run(t TestingT, ctx context.Context, opts Options) {
	t.Helper()

	suite, err := New(opts)
	if err != nil {
		t.Fatalf("failed to configure the conformance suite: %v", err)
		return
	}

	defer func() {
		if err := suite.Cleanup(ctx); err != nil {
			t.Errorf("failed to clean up after the conformance suite: %v", err)
		}
	}()

	for _, check := range []struct {
		name string
		run  func(context.Context) error
	}{
		{"Types", suite.CheckTypes},
		{"Setup", suite.Setup},
		{"Provisioning", suite.CheckProvisioning},
		{"Settled", suite.CheckSettled},
		{"Deprovisioning", suite.CheckDeprovisioning},
	} {
		started := time.Now()
		if err := check.run(ctx); err != nil {
			t.Fatalf("conformance check %q failed after %s: %v", check.name, time.Since(started).Round(time.Second), err)
			return
		}
		t.Logf("conformance check %q passed in %s", check.name, time.Since(started).Round(time.Second))
	}
}

// DefaultHostedControlPlane builds the least HostedControlPlane that satisfies the type's own
// validation. It is not a functioning control plane and nothing reconciles it; it exists
// because a provider reads the release version and the paused state off one.
func DefaultHostedControlPlane(namespace, name string) *hyperv1.HostedControlPlane {
	return &hyperv1.HostedControlPlane{
		ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: name},
		Spec: hyperv1.HostedControlPlaneSpec{
			ReleaseImage: "quay.io/openshift-release-dev/ocp-release:conformance",
			PullSecret:   corev1.LocalObjectReference{Name: "pull-secret"},
			SSHKey:       corev1.LocalObjectReference{Name: "ssh-key"},
			IssuerURL:    "https://kubernetes.default.svc",
			InfraID:      name,
			DNS:          hyperv1.DNSSpec{BaseDomain: "conformance.hypershift.local"},
			Etcd: hyperv1.EtcdSpec{
				ManagementType: hyperv1.Managed,
				// Required alongside Managed, and rejected alongside Unmanaged.
				Managed: &hyperv1.ManagedEtcdSpec{
					Storage: hyperv1.ManagedEtcdStorageSpec{
						Type: hyperv1.PersistentVolumeEtcdStorage,
					},
				},
			},
			Platform: hyperv1.PlatformSpec{
				Type: hyperv1.ExternalPlatform,
				// The union discriminator's member. Nothing in the suite reads it, but the
				// API rejects a HostedControlPlane without it.
				External: hyperv1.ExternalPlatformSpec{
					HostedClusterTemplate: hyperv1.ExternalTemplateReference{
						APIGroup: "conformance.hypershift.openshift.io",
						Resource: "conformancehostedclustertemplates",
						Name:     name,
					},
				},
			},
			// Four is the API's minimum. They are never reconciled here; nothing in the
			// suite starts a control plane.
			Services: []hyperv1.ServicePublishingStrategyMapping{
				{Service: hyperv1.APIServer, ServicePublishingStrategy: hyperv1.ServicePublishingStrategy{Type: hyperv1.LoadBalancer}},
				{Service: hyperv1.OAuthServer, ServicePublishingStrategy: hyperv1.ServicePublishingStrategy{Type: hyperv1.Route}},
				{Service: hyperv1.Konnectivity, ServicePublishingStrategy: hyperv1.ServicePublishingStrategy{Type: hyperv1.Route}},
				{Service: hyperv1.Ignition, ServicePublishingStrategy: hyperv1.ServicePublishingStrategy{Type: hyperv1.Route}},
			},
		},
	}
}

// HostedClusterObject returns the object the suite manages, read fresh.
func (s *Suite) HostedClusterObject(ctx context.Context) (*unstructured.Unstructured, error) {
	object := &unstructured.Unstructured{}
	object.SetGroupVersionKind(s.instanceGVK)
	if err := s.Client.Get(ctx, client.ObjectKey{Namespace: s.Namespace, Name: s.Name}, object); err != nil {
		return nil, err
	}
	return object, nil
}

// Setup creates the namespace, the HostedControlPlane and the hosted cluster object, exactly
// as HyperShift would.
func (s *Suite) Setup(ctx context.Context) error {
	namespace := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: s.Namespace}}
	if err := s.Client.Create(ctx, namespace); err != nil && !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("failed to create the control plane namespace: %w", err)
	}

	hcp := s.HostedControlPlane.DeepCopy()
	hcp.Namespace, hcp.Name = s.Namespace, s.Name
	if err := s.Client.Create(ctx, hcp); err != nil && !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("failed to create the hosted control plane (supply Options.HostedControlPlane if the default does not validate): %w", err)
	}

	object := &unstructured.Unstructured{}
	object.SetGroupVersionKind(s.instanceGVK)
	object.SetNamespace(s.Namespace)
	object.SetName(s.Name)
	object.SetLabels(map[string]string{
		contract.PlatformGroupLabel:     s.APIGroup,
		"cluster.x-k8s.io/cluster-name": s.Name,
	})
	if s.Spec != nil {
		object.Object["spec"] = deepCopyMap(s.Spec)
	}
	// Written by HyperShift rather than copied from the template, and the one part of the
	// spec HyperShift keeps reconciling.
	if err := unstructured.SetNestedMap(object.Object, map[string]any{
		"host": s.ControlPlaneEndpoint.Host,
		"port": int64(s.ControlPlaneEndpoint.Port),
	}, "spec", "controlPlaneEndpoint"); err != nil {
		return fmt.Errorf("failed to set spec.controlPlaneEndpoint: %w", err)
	}

	if err := s.Client.Create(ctx, object); err != nil && !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("failed to create the hosted cluster object: %w", err)
	}
	return nil
}

// Cleanup removes what Setup created. Safe to call when Setup never ran, and safe to call
// twice.
func (s *Suite) Cleanup(ctx context.Context) error {
	object := &unstructured.Unstructured{}
	object.SetGroupVersionKind(s.instanceGVK)
	object.SetNamespace(s.Namespace)
	object.SetName(s.Name)
	if err := s.Client.Delete(ctx, object); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("failed to delete the hosted cluster object: %w", err)
	}

	hcp := &hyperv1.HostedControlPlane{ObjectMeta: metav1.ObjectMeta{Namespace: s.Namespace, Name: s.Name}}
	if err := s.Client.Delete(ctx, hcp); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("failed to delete the hosted control plane: %w", err)
	}
	return nil
}

// poll calls condition with a freshly read object until it reports true, the context is done,
// or the timeout expires. A not-found object is reported to the condition as nil, so that a
// check can wait for a deletion as well as for a status.
func (s *Suite) poll(ctx context.Context, what string, condition func(*unstructured.Unstructured) (bool, error)) error {
	ctx, cancel := context.WithTimeout(ctx, s.Timeout)
	defer cancel()

	for {
		object, err := s.HostedClusterObject(ctx)
		switch {
		case apierrors.IsNotFound(err):
			object = nil
		case err != nil:
			return fmt.Errorf("failed to read the hosted cluster object: %w", err)
		}

		done, err := condition(object)
		if err != nil {
			// Returned rather than retried: every condition below fails only on a contract
			// violation, which retrying would merely observe again more slowly.
			return err
		}
		if done {
			return nil
		}

		select {
		case <-ctx.Done():
			return fmt.Errorf("timed out after %s waiting for %s", s.Timeout, what)
		case <-time.After(s.Interval):
		}
	}
}

// deepCopyMap copies the caller's spec, so that a suite that is run twice against the same
// Options does not hand the second run a map the first one's provider mutated.
func deepCopyMap(in map[string]any) map[string]any {
	return runtime.DeepCopyJSON(in)
}
