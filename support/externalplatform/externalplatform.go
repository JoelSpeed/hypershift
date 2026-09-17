// Package externalplatform adapts the External platform contract to HyperShift's own types.
//
// The contract itself lives in the github.com/openshift/hypershift/externalplatform module,
// which integrators import, and this package is the HyperShift side of it: the same
// resource stripping and status decoding, expressed in terms of ExternalTemplateReference
// and a controller-runtime client rather than of bare strings and unstructured objects.
// Delegating rather than restating is the point, because a HyperShift that disagreed with
// the module about what the contract says would be a HyperShift that no integrator could
// pass conformance against.
package externalplatform

import (
	"context"
	"fmt"

	hyperv1 "github.com/openshift/hypershift/api/hypershift/v1beta1"
	"github.com/openshift/hypershift/externalplatform/contract"

	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"sigs.k8s.io/controller-runtime/pkg/client"
)

// HostedClusterObjectGVK resolves the GVK of the instantiated hosted cluster object from
// the template reference, by stripping the templates suffix off the resource:
// foohostedclustertemplates -> foohostedclusters.
func HostedClusterObjectGVK(mapper meta.RESTMapper, ref hyperv1.ExternalTemplateReference) (schema.GroupVersionKind, error) {
	return contract.InstanceGVK(mapper, ref.APIGroup, ref.Resource)
}

// KindFor resolves a group and plural resource to the GVK of its preferred served version.
func KindFor(mapper meta.RESTMapper, apiGroup, resource string) (schema.GroupVersionKind, error) {
	return contract.KindFor(mapper, apiGroup, resource)
}

// GetHostedClusterObject returns the instantiated hosted cluster object living at the given
// key, which is the control plane namespace and the HostedCluster's name.
//
// The client must not be a caching one. Every object this package touches has a type the
// integrator defined, so a cached read would start an informer LIST/WATCHing a custom
// resource definition that may not be installed.
func GetHostedClusterObject(ctx context.Context, c client.Client, ref hyperv1.ExternalTemplateReference, key client.ObjectKey) (*unstructured.Unstructured, error) {
	gvk, err := HostedClusterObjectGVK(c.RESTMapper(), ref)
	if err != nil {
		return nil, err
	}

	hostedClusterObject := &unstructured.Unstructured{}
	hostedClusterObject.SetGroupVersionKind(gvk)
	if err := c.Get(ctx, key, hostedClusterObject); err != nil {
		return nil, fmt.Errorf("failed to get hosted cluster object %s %s: %w", gvk.Kind, key, err)
	}
	return hostedClusterObject, nil
}

// Declaration reads the integrator's status.platform block off the hosted cluster object.
//
// A nil return with a nil error means the integrator has not declared anything yet, which
// is the normal state early in provisioning. An incomplete or invalid block is an error
// rather than a nil: the contract asks for both values to be published together, and
// recording half of one would pin a value the integrator never agreed to.
func Declaration(hostedClusterObject *unstructured.Unstructured) (*hyperv1.ExternalPlatformStatus, error) {
	return contract.Platform(hostedClusterObject)
}
