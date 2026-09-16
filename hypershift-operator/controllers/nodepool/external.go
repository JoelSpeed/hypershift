package nodepool

import (
	"context"
	"fmt"

	hyperv1 "github.com/openshift/hypershift/api/hypershift/v1beta1"
	"github.com/openshift/hypershift/support/api"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"sigs.k8s.io/controller-runtime/pkg/client"
)

// externalMachineTemplate instantiates the Cluster API machine template the NodePool
// references into the control plane namespace.
//
// Unlike the HostedCluster's hosted cluster template, this one really is a Cluster API
// object: the MachineDeployment resolves it by group and kind, and CAPI's machine
// infrastructure contract applies to it in full. HyperShift is doing nothing here that it
// does not already do for the in-tree platforms; it is only copying a spec it did not
// author rather than building one.
func (c *CAPI) externalMachineTemplate(ctx context.Context, templateNameGenerator func(spec any) (string, error)) (client.Object, error) {
	ref := c.nodePool.Spec.Platform.External.MachineTemplate

	gvk, err := c.externalMachineTemplateGVK()
	if err != nil {
		return nil, err
	}

	source := &unstructured.Unstructured{}
	source.SetGroupVersionKind(gvk)
	if err := c.uncachedClient.Get(ctx, client.ObjectKey{Namespace: c.nodePool.Namespace, Name: ref.Name}, source); err != nil {
		return nil, fmt.Errorf("failed to get machine template %s %s/%s: %w", gvk.Kind, c.nodePool.Namespace, ref.Name, err)
	}

	// NestedMap deep copies, so the name is hashed over, and the object is built from,
	// a value that cannot alias the one that was read.
	spec, found, err := unstructured.NestedMap(source.Object, "spec")
	if err != nil {
		return nil, fmt.Errorf("failed to read spec from machine template %s %s/%s: %w", gvk.Kind, c.nodePool.Namespace, ref.Name, err)
	}
	if !found {
		return nil, fmt.Errorf("machine template %s %s/%s has no spec", gvk.Kind, c.nodePool.Namespace, ref.Name)
	}

	// The spec alone, exactly as the in-tree platforms hash their own machine template
	// spec. Nothing from metadata takes part: resourceVersion and generation change on
	// writes that leave the spec untouched, and hashing either of them would roll every
	// node in the pool for a no-op edit. Go marshals map[string]any with sorted keys, so
	// the hash is stable across reads.
	templateName, err := templateNameGenerator(spec)
	if err != nil {
		return nil, fmt.Errorf("failed to generate template name: %w", err)
	}

	template := &unstructured.Unstructured{}
	template.SetGroupVersionKind(gvk)
	template.SetName(templateName)
	template.Object["spec"] = spec
	return template, nil
}

// externalMachineTemplateGVK resolves the kind of the NodePool's machine template
// reference against the management cluster.
//
// The resource is used as given. The templates suffix is stripped for the HostedCluster's
// hosted cluster object, where HyperShift instantiates a template into an instance of a
// different kind, but a machine template instantiates into another machine template:
// MachineDeployment.spec.template.spec.infrastructureRef names a template, not a machine.
func (c *CAPI) externalMachineTemplateGVK() (schema.GroupVersionKind, error) {
	ref := c.nodePool.Spec.Platform.External.MachineTemplate
	if c.uncachedClient == nil {
		return schema.GroupVersionKind{}, fmt.Errorf("the External platform requires an uncached client, none was configured")
	}
	gvk, err := c.uncachedClient.RESTMapper().KindFor(schema.GroupVersionResource{Group: ref.APIGroup, Resource: ref.Resource})
	if err != nil {
		return schema.GroupVersionKind{}, fmt.Errorf("failed to resolve %s.%s: %w", ref.Resource, ref.APIGroup, err)
	}
	return gvk, nil
}

// machineTemplateClient returns the client to use for reads of the NodePool's machine
// template kind.
//
// Every in-tree platform's kind is compiled into the operator and watched already, so the
// cache is both correct and cheaper. The External platform's kind is the integrator's, and
// a cached read of it would start an informer on a CRD that may not be installed.
func (c *CAPI) machineTemplateClient() (client.Client, error) {
	if c.nodePool.Spec.Platform.Type != hyperv1.ExternalPlatform {
		return c.Client, nil
	}
	if c.uncachedClient == nil {
		return nil, fmt.Errorf("the External platform requires an uncached client, none was configured")
	}
	return c.uncachedClient, nil
}

// machineTemplateAPIVersion reconstructs the version that a v1beta2
// ContractVersionedObjectReference deliberately omits.
//
// The scheme is consulted first: it needs no API call, and it keeps the in-tree platforms
// pinned to the version HyperShift compiled against. It cannot answer for the External
// platform, whose machine template kinds are the integrator's and are not registered, so
// fall back to what the management cluster is actually serving.
func (c *CAPI) machineTemplateAPIVersion(group, kind string) (string, error) {
	groupKind := schema.GroupKind{Group: group, Kind: kind}
	if versions := api.Scheme.VersionsForGroupKind(groupKind); len(versions) > 0 {
		return schema.GroupVersion{Group: group, Version: versions[0].Version}.String(), nil
	}

	if c.uncachedClient == nil {
		return "", fmt.Errorf("no versions registered for GroupKind %s/%s", group, kind)
	}
	mapping, err := c.uncachedClient.RESTMapper().RESTMapping(groupKind)
	if err != nil {
		return "", fmt.Errorf("failed to resolve a version for GroupKind %s/%s: %w", group, kind, err)
	}
	return mapping.GroupVersionKind.GroupVersion().String(), nil
}
