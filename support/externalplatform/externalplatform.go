// Package externalplatform holds the parts of the External platform contract that more
// than one HyperShift operator needs.
//
// The hosted cluster object the integrator owns is read by two operators for two different
// reasons: the HyperShift Operator instantiates it and reports its readiness on the
// HostedCluster, and the Control Plane Operator reads the declaration the integrator
// publishes on it. Keeping the resource stripping and the status decoding here means the
// two cannot drift into disagreeing about what the contract says.
package externalplatform

import (
	"context"
	"fmt"
	"strings"

	hyperv1 "github.com/openshift/hypershift/api/hypershift/v1beta1"

	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	// templateResourceSuffix is the suffix the API requires on the resource name of a
	// template, and which is stripped to derive the resource of the instantiated object:
	// foohostedclustertemplates -> foohostedclusters.
	templateResourceSuffix = "templates"

	// instanceResourceSuffix replaces templateResourceSuffix. Both resources are plural.
	instanceResourceSuffix = "s"
)

// HostedClusterObjectGVK resolves the GVK of the instantiated hosted cluster object from
// the template reference, by stripping the templates suffix off the resource:
// foohostedclustertemplates -> foohostedclusters.
func HostedClusterObjectGVK(mapper meta.RESTMapper, ref hyperv1.ExternalTemplateReference) (schema.GroupVersionKind, error) {
	if !strings.HasSuffix(ref.Resource, templateResourceSuffix) {
		return schema.GroupVersionKind{}, fmt.Errorf("hosted cluster template resource %q must end in %q", ref.Resource, templateResourceSuffix)
	}
	instanceResource := strings.TrimSuffix(ref.Resource, templateResourceSuffix) + instanceResourceSuffix
	return KindFor(mapper, ref.APIGroup, instanceResource)
}

// KindFor resolves a group and plural resource to the GVK of its preferred served version.
func KindFor(mapper meta.RESTMapper, apiGroup, resource string) (schema.GroupVersionKind, error) {
	gvk, err := mapper.KindFor(schema.GroupVersionResource{Group: apiGroup, Resource: resource})
	if err != nil {
		return schema.GroupVersionKind{}, fmt.Errorf("failed to resolve %s.%s: %w", resource, apiGroup, err)
	}
	return gvk, nil
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
	platform, found, err := unstructured.NestedMap(hostedClusterObject.Object, "status", "platform")
	if err != nil {
		return nil, fmt.Errorf("failed to read status.platform: %w", err)
	}
	if !found || len(platform) == 0 {
		return nil, nil
	}

	name, _, err := unstructured.NestedString(platform, "name")
	if err != nil {
		return nil, fmt.Errorf("failed to read status.platform.name: %w", err)
	}
	if name == "" {
		return nil, fmt.Errorf("status.platform.name is not set")
	}

	state, _, err := unstructured.NestedString(platform, "cloudControllerManager", "state")
	if err != nil {
		return nil, fmt.Errorf("failed to read status.platform.cloudControllerManager.state: %w", err)
	}
	switch hyperv1.ExternalCloudControllerManagerState(state) {
	case hyperv1.ExternalCloudControllerManager, hyperv1.NoCloudControllerManager:
	default:
		// Checked here rather than left to the API server so that a typo surfaces as a
		// message naming the offending value, instead of as a rejected status patch on an
		// unrelated field.
		return nil, fmt.Errorf("status.platform.cloudControllerManager.state must be %q or %q, got %q",
			hyperv1.ExternalCloudControllerManager, hyperv1.NoCloudControllerManager, state)
	}

	return &hyperv1.ExternalPlatformStatus{
		Name: name,
		CloudControllerManager: hyperv1.ExternalCloudControllerManagerStatus{
			State: hyperv1.ExternalCloudControllerManagerState(state),
		},
	}, nil
}
