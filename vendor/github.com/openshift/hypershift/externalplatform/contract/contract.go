// Package contract is the External platform contract, expressed as code.
//
// Everything here is a fact about the handoff between HyperShift and an integrator: what
// HyperShift writes onto the hosted cluster object, what it reads back off it, and how the
// names of the integrator's types relate to each other. Both sides import this package, so
// they cannot drift into disagreeing about what the contract says.
//
// The prose version lives at docs/content/reference/external-platform-contract.md. Where the
// two disagree, this package is what runs.
package contract

import (
	"fmt"
	"strings"

	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// Version is the contract version this build implements. HyperShift supports the current
// version and the one before it, so an integrator has a full release to move.
//
// Bumping it is a deliberate, breaking act: it requires a deprecation window of two OpenShift
// minor versions and a published compatibility matrix, because the integrators on the other
// side of the contract are released independently of HyperShift.
const Version = "v1alpha1"

const (
	// VersionLabel records, on the integrator's hosted cluster template custom resource
	// definition, which contract version the integrator implements. HyperShift compares it
	// against Version to detect skew before the skew turns into silent misbehaviour.
	VersionLabel = "hypershift.openshift.io/external-platform-contract"

	// PlatformGroupLabel records, on the instantiated hosted cluster object, the API group
	// of the integrator that owns it. The group rather than the platform name, because the
	// name is only known once the integrator has declared it and this label is set at
	// create time.
	PlatformGroupLabel = "hypershift.openshift.io/external-platform-group"
)

const (
	// templateResourceSuffix is the suffix the API requires on the plural resource name of
	// a template, and which is stripped to derive the resource of the instantiated object:
	// foohostedclustertemplates -> foohostedclusters.
	templateResourceSuffix = "templates"

	// instanceResourceSuffix replaces templateResourceSuffix. Both resources are plural.
	instanceResourceSuffix = "s"
)

// ReadyConditionType is the single condition the contract asks the integrator to set on the
// hosted cluster object. Its message is mirrored verbatim onto the HostedCluster, so it is
// the integrator's user-facing error channel.
const ReadyConditionType = "Ready"

// InstanceResource derives the plural resource of the instantiated hosted cluster object
// from the plural resource of the template: foohostedclustertemplates yields
// foohostedclusters.
//
// A resource that does not end in templates cannot be instantiated, and saying so here means
// the user sees a message about the reference they wrote rather than a not-found on a type
// they never named.
func InstanceResource(templateResource string) (string, error) {
	if !strings.HasSuffix(templateResource, templateResourceSuffix) {
		return "", fmt.Errorf("hosted cluster template resource %q must end in %q", templateResource, templateResourceSuffix)
	}
	return strings.TrimSuffix(templateResource, templateResourceSuffix) + instanceResourceSuffix, nil
}

// InstanceGVK resolves the GVK of the instantiated hosted cluster object from the API group
// and plural resource of the template.
func InstanceGVK(mapper meta.RESTMapper, apiGroup, templateResource string) (schema.GroupVersionKind, error) {
	instanceResource, err := InstanceResource(templateResource)
	if err != nil {
		return schema.GroupVersionKind{}, err
	}
	return KindFor(mapper, apiGroup, instanceResource)
}

// KindFor resolves a group and plural resource to the GVK of its preferred served version.
//
// Resource rather than kind throughout the contract: a resource is what a client resolves
// against without a discovery round-trip, it is unambiguous where a kind served by more than
// one resource is not, and it is the vocabulary RBAC is written in, so an administrator's
// registration grant and a user-supplied reference can be compared directly.
func KindFor(mapper meta.RESTMapper, apiGroup, resource string) (schema.GroupVersionKind, error) {
	gvk, err := mapper.KindFor(schema.GroupVersionResource{Group: apiGroup, Resource: resource})
	if err != nil {
		return schema.GroupVersionKind{}, fmt.Errorf("failed to resolve %s.%s: %w", resource, apiGroup, err)
	}
	return gvk, nil
}
