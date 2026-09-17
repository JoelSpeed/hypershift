// Package external implements the Platform interface for platforms whose provider
// behavior lives outside HyperShift.
//
// Most methods are no-ops by design. The integrator runs its own Cluster API provider as
// a fleet singleton alongside the HyperShift Operator, so HyperShift has no provider
// Deployment to reconcile, no cloud credentials to copy, and no KMS backend to wire up.
// What remains is ReconcileCAPIInfraCR, which instantiates the integrator's hosted
// cluster template into the control plane namespace and then waits for the integrator to
// report which Cluster API infrastructure object it stood up.
//
// The hosted cluster object is not a Cluster API InfraCluster, and Cluster API never
// resolves it. It is HyperShift's handoff contract: HyperShift writes the desired state
// into it, the integrator provisions, and the integrator reports back on
// status.infrastructure. Only then does HyperShift create the CAPI Cluster, pointing
// spec.infrastructureRef at the object the integrator named. That field is immutable in
// Cluster API, which is why HyperShift cannot create the Cluster up front and guess.
//
// OrphanDeleter is deliberately not implemented: HyperShift cannot reason about whether
// third-party infrastructure it has never seen has been orphaned.
package external

import (
	"context"
	"fmt"
	"time"

	hyperv1 "github.com/openshift/hypershift/api/hypershift/v1beta1"
	"github.com/openshift/hypershift/externalplatform/contract"
	"github.com/openshift/hypershift/support/externalplatform"
	"github.com/openshift/hypershift/support/upsert"

	appsv1 "k8s.io/api/apps/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"

	capiv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	// ExternalPlatformGroupLabel records, on the instantiated hosted cluster object, the
	// API group of the integrator that owns it. Aliased from the contract module so that
	// the key an integrator matches on and the key HyperShift writes are the same string.
	ExternalPlatformGroupLabel = contract.PlatformGroupLabel

	// readyConditionType is the single condition the contract asks the integrator to set
	// on the hosted cluster object. Its message is mirrored verbatim onto the
	// HostedCluster, so it is the integrator's user-facing error channel.
	readyConditionType = contract.ReadyConditionType

	// externalProviderWarningThreshold is how long HyperShift waits before the condition
	// message starts naming the elapsed time. An integrator controller that was never
	// installed produces exactly the same condition as one that is mid-provision, so
	// without this an operator has no way to tell a slow cloud from a missing controller.
	externalProviderWarningThreshold = 30 * time.Minute
)

// External implements the Platform interface for the External platform.
type External struct {
	// uncachedClient talks to the API server directly rather than through the operator's
	// cache. This is not an optimisation, it is a correctness requirement: the HyperShift
	// Operator's client caches unstructured objects, so a read of an integrator-defined
	// GVK through it would start an informer that LIST/WATCHes a CRD which may not be
	// installed, retrying forever and wedging reconciliation for every HostedCluster on
	// the management cluster.
	uncachedClient client.Client
}

// New returns an External platform backed by the given uncached client.
func New(uncachedClient client.Client) *External {
	return &External{uncachedClient: uncachedClient}
}

// ReconcileCAPIInfraCR instantiates the integrator's hosted cluster object and then
// returns a reference to the Cluster API infrastructure object the integrator reported on
// it. Returning nil skips creation of the CAPI Cluster, which is the normal state until
// the integrator has finished provisioning.
//
// The client the caller passes is deliberately ignored in favor of p.uncachedClient:
// every object this method touches has a type the integrator defined, and the caller's
// client is the operator's cached one.
func (p External) ReconcileCAPIInfraCR(ctx context.Context, c client.Client, createOrUpdate upsert.CreateOrUpdateFN,
	hcluster *hyperv1.HostedCluster,
	controlPlaneNamespace string, apiEndpoint hyperv1.APIEndpoint) (client.Object, error) {
	if apiEndpoint.Host == "" {
		// Same guard as the Agent platform. The endpoint is part of the spec HyperShift
		// hands the integrator, and the spec is written on create only, so creating the
		// object before the endpoint is known would permanently hand over an empty one.
		return nil, nil
	}
	if p.uncachedClient == nil {
		return nil, fmt.Errorf("the External platform requires an uncached client, none was configured")
	}

	hostedClusterObject, err := p.reconcileHostedClusterObject(ctx, createOrUpdate, hcluster, controlPlaneNamespace, apiEndpoint)
	if err != nil {
		return nil, err
	}

	return p.capiInfrastructureReference(hostedClusterObject, controlPlaneNamespace)
}

// reconcileHostedClusterObject instantiates the template the HostedCluster references into
// the control plane namespace.
func (p External) reconcileHostedClusterObject(ctx context.Context, createOrUpdate upsert.CreateOrUpdateFN,
	hcluster *hyperv1.HostedCluster, controlPlaneNamespace string, apiEndpoint hyperv1.APIEndpoint) (*unstructured.Unstructured, error) {
	ref := hcluster.Spec.Platform.External.HostedClusterTemplate
	mapper := p.uncachedClient.RESTMapper()

	templateGVK, err := externalplatform.KindFor(mapper, ref.APIGroup, ref.Resource)
	if err != nil {
		return nil, err
	}
	instanceGVK, err := externalplatform.HostedClusterObjectGVK(mapper, ref)
	if err != nil {
		return nil, err
	}

	template := &unstructured.Unstructured{}
	template.SetGroupVersionKind(templateGVK)
	if err := p.uncachedClient.Get(ctx, client.ObjectKey{Namespace: hcluster.Namespace, Name: ref.Name}, template); err != nil {
		return nil, fmt.Errorf("failed to get hosted cluster template %s %s/%s: %w", templateGVK.Kind, hcluster.Namespace, ref.Name, err)
	}
	templateSpec, found, err := unstructured.NestedMap(template.Object, "spec", "template", "spec")
	if err != nil {
		return nil, fmt.Errorf("failed to read spec.template.spec from hosted cluster template %s %s/%s: %w", templateGVK.Kind, hcluster.Namespace, ref.Name, err)
	}
	if !found {
		return nil, fmt.Errorf("hosted cluster template %s %s/%s has no spec.template.spec", templateGVK.Kind, hcluster.Namespace, ref.Name)
	}

	hostedClusterObject := &unstructured.Unstructured{}
	hostedClusterObject.SetGroupVersionKind(instanceGVK)
	hostedClusterObject.SetNamespace(controlPlaneNamespace)
	hostedClusterObject.SetName(hcluster.Name)
	if _, err := createOrUpdate(ctx, p.uncachedClient, hostedClusterObject, func() error {
		// The spec is the integrator's to own once it exists. Copying the template over
		// it on every reconcile would fight the integrator's own defaulting and would
		// silently re-provision when someone edits the template, so it is written on
		// create only, matching how reconcileCAPICluster treats the CAPI Cluster.
		if hostedClusterObject.GetResourceVersion() == "" {
			hostedClusterObject.Object["spec"] = templateSpec
		}

		// The endpoint is the exception: it is not known at the time the user writes the
		// template, it is HyperShift's to report, and it can legitimately change.
		if err := unstructured.SetNestedField(hostedClusterObject.Object, apiEndpoint.Host, "spec", "controlPlaneEndpoint", "host"); err != nil {
			return fmt.Errorf("failed to set spec.controlPlaneEndpoint.host: %w", err)
		}
		if err := unstructured.SetNestedField(hostedClusterObject.Object, int64(apiEndpoint.Port), "spec", "controlPlaneEndpoint", "port"); err != nil {
			return fmt.Errorf("failed to set spec.controlPlaneEndpoint.port: %w", err)
		}

		labels := hostedClusterObject.GetLabels()
		if labels == nil {
			labels = map[string]string{}
		}
		// InfraID rather than the HostedCluster name: this is the name of the CAPI Cluster
		// HyperShift will create, so the integrator can use the label to correlate its own
		// CAPI objects with this one.
		labels[capiv1.ClusterNameLabel] = hcluster.Spec.InfraID
		labels[ExternalPlatformGroupLabel] = ref.APIGroup
		hostedClusterObject.SetLabels(labels)

		return nil
	}); err != nil {
		return nil, fmt.Errorf("failed to reconcile hosted cluster object %s %s/%s: %w", instanceGVK.Kind, controlPlaneNamespace, hcluster.Name, err)
	}

	return hostedClusterObject, nil
}

// capiInfrastructureReference turns the integrator's status.infrastructure into a stub
// that reconcileCAPICluster can read a group, kind and name off.
//
// The returned object is deliberately a stub carrying nothing but its identity. HyperShift
// must never create or mutate the integrator's CAPI infrastructure object: an apply of an
// empty spec would destroy a provisioned AWSCluster. The only caller writes it into
// Cluster.spec.infrastructureRef and nothing else, and reconcileCAPICluster only ever
// reads GVK and name from it.
func (p External) capiInfrastructureReference(hostedClusterObject *unstructured.Unstructured, controlPlaneNamespace string) (client.Object, error) {
	apiGroup, _, err := unstructured.NestedString(hostedClusterObject.Object, "status", "infrastructure", "apiGroup")
	if err != nil {
		return nil, fmt.Errorf("failed to read status.infrastructure.apiGroup: %w", err)
	}
	kind, _, err := unstructured.NestedString(hostedClusterObject.Object, "status", "infrastructure", "kind")
	if err != nil {
		return nil, fmt.Errorf("failed to read status.infrastructure.kind: %w", err)
	}
	name, _, err := unstructured.NestedString(hostedClusterObject.Object, "status", "infrastructure", "name")
	if err != nil {
		return nil, fmt.Errorf("failed to read status.infrastructure.name: %w", err)
	}
	if apiGroup == "" || kind == "" || name == "" {
		// The integrator has not finished. This is a normal intermediate state, not an
		// error: no CAPI Cluster is created until the reference appears.
		return nil, nil
	}

	// status.infrastructure carries no version, matching CAPI's own
	// ContractVersionedObjectReference. Resolve the served version from the management
	// cluster so the stub is a well formed object; this also fails loudly when the CRD
	// the integrator named is not installed.
	mapping, err := p.uncachedClient.RESTMapper().RESTMapping(schema.GroupKind{Group: apiGroup, Kind: kind})
	if err != nil {
		return nil, fmt.Errorf("failed to resolve the Cluster API infrastructure object %s.%s named in status.infrastructure: %w", kind, apiGroup, err)
	}

	stub := &unstructured.Unstructured{}
	stub.SetGroupVersionKind(mapping.GroupVersionKind)
	stub.SetNamespace(controlPlaneNamespace)
	stub.SetName(name)
	return stub, nil
}

// InfrastructureReadyCondition reports the integrator's progress for the given
// HostedCluster as a condition suitable for HostedCluster.status.
//
// waitingFor is how long the HostedCluster has been waiting; it is used only to make the
// message actionable, because a missing integrator controller is otherwise indistinguishable
// from HyperShift hanging.
func InfrastructureReadyCondition(ctx context.Context, uncachedClient client.Client, hcluster *hyperv1.HostedCluster, controlPlaneNamespace string, waitingFor time.Duration) (metav1.Condition, error) {
	condition := metav1.Condition{
		Type:               string(hyperv1.ExternalInfrastructureReady),
		Status:             metav1.ConditionFalse,
		Reason:             hyperv1.ExternalInfrastructureNotFoundReason,
		ObservedGeneration: hcluster.Generation,
	}

	ref := hcluster.Spec.Platform.External.HostedClusterTemplate
	gvk, err := externalplatform.HostedClusterObjectGVK(uncachedClient.RESTMapper(), ref)
	if err != nil {
		if meta.IsNoMatchError(err) {
			condition.Message = fmt.Sprintf("The %s custom resource definition is not installed on the management cluster", err.Error())
			return condition, nil
		}
		return condition, err
	}

	hostedClusterObject := &unstructured.Unstructured{}
	hostedClusterObject.SetGroupVersionKind(gvk)
	if err := uncachedClient.Get(ctx, client.ObjectKey{Namespace: controlPlaneNamespace, Name: hcluster.Name}, hostedClusterObject); err != nil {
		if !apierrors.IsNotFound(err) {
			return condition, fmt.Errorf("failed to get hosted cluster object %s %s/%s: %w", gvk.Kind, controlPlaneNamespace, hcluster.Name, err)
		}
		condition.Message = fmt.Sprintf("The %s %s/%s has not been created yet", gvk.Kind, controlPlaneNamespace, hcluster.Name)
		return condition, nil
	}

	condition.Reason = hyperv1.WaitingOnExternalProviderReason
	ready, err := readyCondition(hostedClusterObject)
	if err != nil {
		return condition, err
	}
	switch {
	case ready == nil:
		condition.Message = fmt.Sprintf("The provider has not reported a Ready condition on %s %s/%s yet", gvk.Kind, controlPlaneNamespace, hcluster.Name)
	case ready.Status == metav1.ConditionTrue:
		condition.Status = metav1.ConditionTrue
		condition.Reason = hyperv1.AsExpectedReason
		condition.Message = ready.Message
		return condition, nil
	default:
		// Mirrored verbatim: this is the integrator's user-facing error channel, and
		// paraphrasing it would lose the only provider-specific detail available.
		condition.Message = ready.Message
	}

	if waitingFor >= externalProviderWarningThreshold {
		condition.Message = fmt.Sprintf("%s (waiting on the provider for %s)", condition.Message, waitingFor.Round(time.Minute))
	}
	return condition, nil
}

// readyCondition extracts the contract's single Ready condition from the hosted cluster
// object.
func readyCondition(hostedClusterObject *unstructured.Unstructured) (*metav1.Condition, error) {
	raw, found, err := unstructured.NestedSlice(hostedClusterObject.Object, "status", "conditions")
	if err != nil {
		return nil, fmt.Errorf("failed to read status.conditions: %w", err)
	}
	if !found {
		return nil, nil
	}
	for _, item := range raw {
		entry, ok := item.(map[string]any)
		if !ok {
			continue
		}
		conditionType, _, _ := unstructured.NestedString(entry, "type")
		if conditionType != readyConditionType {
			continue
		}
		status, _, _ := unstructured.NestedString(entry, "status")
		reason, _, _ := unstructured.NestedString(entry, "reason")
		message, _, _ := unstructured.NestedString(entry, "message")
		return &metav1.Condition{
			Type:    conditionType,
			Status:  metav1.ConditionStatus(status),
			Reason:  reason,
			Message: message,
		}, nil
	}
	return nil, nil
}

// DeleteHostedClusterObject deletes the hosted cluster object HyperShift instantiated and
// reports whether it is still present. It is called on every pass of the HostedCluster
// teardown until it reports false.
//
// Deleting this object is what tells the integrator to tear down. It has to happen while
// the control plane namespace still exists, because the guest kubeconfig and the
// integrator's RoleBinding both live there and the provider needs them to finish; deleting
// the namespace first would strand it. It also has to happen after the Cluster API Cluster
// is gone, so that machines and the Cluster API infrastructure object are torn down before
// whatever they were built on.
//
// force strips the integrator's finalizers instead of waiting for it to remove them. That
// almost certainly leaks provisioned infrastructure, so it happens only when an
// administrator has asked for it on the HostedCluster, and it is logged at every step.
func DeleteHostedClusterObject(ctx context.Context, uncachedClient client.Client, hcluster *hyperv1.HostedCluster, controlPlaneNamespace string, force bool) (bool, error) {
	log := ctrl.LoggerFrom(ctx)

	gvk, err := externalplatform.HostedClusterObjectGVK(uncachedClient.RESTMapper(), hcluster.Spec.Platform.External.HostedClusterTemplate)
	if err != nil {
		if meta.IsNoMatchError(err) {
			// The integrator's custom resource definition has been uninstalled, which took
			// every object of that type with it. There is nothing left to wait for, and
			// erroring here would make the HostedCluster undeletable for a reason the
			// administrator cannot act on.
			return false, nil
		}
		return false, err
	}

	hostedClusterObject := &unstructured.Unstructured{}
	hostedClusterObject.SetGroupVersionKind(gvk)
	if err := uncachedClient.Get(ctx, client.ObjectKey{Namespace: controlPlaneNamespace, Name: hcluster.Name}, hostedClusterObject); err != nil {
		if apierrors.IsNotFound(err) {
			return false, nil
		}
		return false, fmt.Errorf("failed to get hosted cluster object %s %s/%s: %w", gvk.Kind, controlPlaneNamespace, hcluster.Name, err)
	}

	if hostedClusterObject.GetDeletionTimestamp().IsZero() {
		if err := uncachedClient.Delete(ctx, hostedClusterObject); err != nil && !apierrors.IsNotFound(err) {
			return false, fmt.Errorf("failed to delete hosted cluster object %s %s/%s: %w", gvk.Kind, controlPlaneNamespace, hcluster.Name, err)
		}
		// Report it as still present even on a successful delete: the integrator's
		// finalizer means the object outlives the call, and the next pass re-reads it.
		return true, nil
	}

	if force && len(hostedClusterObject.GetFinalizers()) > 0 {
		log.Info("Force-removing the provider's finalizers from the hosted cluster object. Infrastructure the provider created is very likely to be leaked and must be cleaned up by hand",
			"annotation", hyperv1.ForceExternalCleanupAnnotation,
			"kind", gvk.Kind, "namespace", controlPlaneNamespace, "name", hcluster.Name,
			"finalizers", hostedClusterObject.GetFinalizers())
		hostedClusterObject.SetFinalizers(nil)
		if err := uncachedClient.Update(ctx, hostedClusterObject); err != nil && !apierrors.IsNotFound(err) {
			return false, fmt.Errorf("failed to remove finalizers from hosted cluster object %s %s/%s: %w", gvk.Kind, controlPlaneNamespace, hcluster.Name, err)
		}
		return true, nil
	}

	return true, nil
}

// CAPIProviderDeploymentSpec returns nil because the integrator deploys and owns its own
// Cluster API provider. A nil spec is the existing signal for "this platform has no
// provider", shared with the None platform.
func (p External) CAPIProviderDeploymentSpec(hcluster *hyperv1.HostedCluster, _ *hyperv1.HostedControlPlane) (*appsv1.DeploymentSpec, error) {
	return nil, nil
}

func (p External) ReconcileCredentials(ctx context.Context, c client.Client, createOrUpdate upsert.CreateOrUpdateFN,
	hcluster *hyperv1.HostedCluster,
	controlPlaneNamespace string) error {
	return nil
}

func (External) ReconcileSecretEncryption(ctx context.Context, c client.Client, createOrUpdate upsert.CreateOrUpdateFN,
	hcluster *hyperv1.HostedCluster,
	controlPlaneNamespace string) error {
	return nil
}

func (External) CAPIProviderPolicyRules() []rbacv1.PolicyRule {
	return nil
}

func (External) DeleteCredentials(ctx context.Context, c client.Client, hcluster *hyperv1.HostedCluster, controlPlaneNamespace string) error {
	return nil
}
