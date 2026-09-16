package hostedcontrolplane

import (
	"context"
	"fmt"

	hyperv1 "github.com/openshift/hypershift/api/hypershift/v1beta1"
	"github.com/openshift/hypershift/support/externalplatform"
	"github.com/openshift/hypershift/support/statuspatching"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"sigs.k8s.io/controller-runtime/pkg/client"
)

// reconcileExternalPlatformStatus records the platform declaration the integrator published
// on the hosted cluster object onto the HostedControlPlane, the first time it is observed.
//
// The declaration says what the platform is called and whether a cloud controller manager
// runs for it. Both values reach the guest cluster's Infrastructure, which reaches the
// machine config server, which feeds the NodePool configuration hash, so reading them
// afresh on every reconcile would let an integrator roll every node in the cluster onto a
// configuration that disagrees with the one it was installed with. HyperShift therefore
// records the first observation and reports any later disagreement instead of acting on it.
//
// Recording it on the HostedControlPlane rather than resolving it where it is needed keeps
// the rest of the control plane free of the integrator's API group: the machine config
// server, the hosted cluster config operator and the ingress defaults all read the recorded
// value, and only this function talks to the integrator. The HostedCluster picks it up for
// free, because the HostedCluster controller already copies the whole platform status over.
func (r *HostedControlPlaneReconciler) reconcileExternalPlatformStatus(ctx context.Context, hcp *hyperv1.HostedControlPlane) error {
	if hcp.Spec.Platform.Type != hyperv1.ExternalPlatform {
		return nil
	}
	if hcp.Spec.Platform.External.HostedClusterTemplate.Resource == "" {
		// The union rule makes this unreachable through the API server, but the HCP is
		// written by a controller and there is nothing to resolve without the reference.
		return nil
	}

	condition := metav1.Condition{
		Type:               string(hyperv1.ValidExternalPlatformDeclaration),
		Status:             metav1.ConditionFalse,
		Reason:             hyperv1.WaitingOnExternalProviderReason,
		ObservedGeneration: hcp.Generation,
	}

	declaration, err := r.externalPlatformDeclaration(ctx, hcp)
	switch {
	case err != nil:
		// An unreadable or half-published declaration is the integrator's to fix, and is
		// reported rather than returned: retrying cannot make a malformed status well
		// formed, and failing the whole reconcile would take the control plane down with it.
		condition.Reason = hyperv1.ExternalPlatformDeclarationInvalidReason
		condition.Message = err.Error()
	case declaration == nil:
		condition.Message = fmt.Sprintf("The provider has not declared the platform on %s %s/%s yet",
			hcp.Spec.Platform.External.HostedClusterTemplate.Resource, hcp.Namespace, hcp.Name)
	default:
		condition.Status = metav1.ConditionTrue
		condition.Reason = hyperv1.AsExpectedReason
		condition.Message = fmt.Sprintf("The provider declared platform %q with cloudControllerManager.state %q",
			declaration.Name, declaration.CloudControllerManager.State)
	}

	if recorded := recordedExternalPlatformDeclaration(hcp); recorded != nil && declaration != nil && *recorded != *declaration {
		// Deliberately not an error and deliberately not applied. The cluster is already
		// installed against the recorded value; adopting the new one would roll every node
		// onto a configuration the cluster was never installed with.
		condition.Status = metav1.ConditionFalse
		condition.Reason = hyperv1.ExternalPlatformDeclarationChangedReason
		condition.Message = fmt.Sprintf("The provider changed its declaration from platform %q with cloudControllerManager.state %q to platform %q with cloudControllerManager.state %q after it was recorded; the recorded declaration continues to be used",
			recorded.Name, recorded.CloudControllerManager.State,
			declaration.Name, declaration.CloudControllerManager.State)
	}

	return statuspatching.PatchStatus(ctx, r.Client, hcp, func() error {
		meta.SetStatusCondition(&hcp.Status.Conditions, condition)
		// Recomputed from the refetched object rather than from the copy read above: another
		// writer may have recorded a declaration in the meantime, and that first one wins.
		if declaration != nil && recordedExternalPlatformDeclaration(hcp) == nil {
			if hcp.Status.Platform == nil {
				hcp.Status.Platform = &hyperv1.PlatformStatus{}
			}
			hcp.Status.Platform.External = *declaration
		}
		return nil
	})
}

// externalPlatformDeclaration reads the integrator's declaration off the hosted cluster
// object in the control plane namespace.
//
// A nil declaration with a nil error means there is nothing to record yet, which covers the
// whole of the normal startup sequence: the integrator's custom resource definitions may not
// be installed, the HyperShift Operator may not have instantiated the object, and the
// integrator may not have reached it.
func (r *HostedControlPlaneReconciler) externalPlatformDeclaration(ctx context.Context, hcp *hyperv1.HostedControlPlane) (*hyperv1.ExternalPlatformStatus, error) {
	// The Control Plane Operator's client does not cache unstructured objects, so this read
	// does not start an informer on a custom resource definition that may not exist.
	hostedClusterObject, err := externalplatform.GetHostedClusterObject(ctx, r.Client,
		hcp.Spec.Platform.External.HostedClusterTemplate,
		client.ObjectKey{Namespace: hcp.Namespace, Name: hcp.Name})
	if err != nil {
		if apierrors.IsNotFound(err) || meta.IsNoMatchError(err) {
			return nil, nil
		}
		return nil, err
	}
	return externalplatform.Declaration(hostedClusterObject)
}

// recordedExternalPlatformDeclaration returns the declaration already recorded on the
// HostedControlPlane, or nil if none has been.
func recordedExternalPlatformDeclaration(hcp *hyperv1.HostedControlPlane) *hyperv1.ExternalPlatformStatus {
	if hcp.Status.Platform == nil || hcp.Status.Platform.External == (hyperv1.ExternalPlatformStatus{}) {
		return nil
	}
	return &hcp.Status.Platform.External
}
