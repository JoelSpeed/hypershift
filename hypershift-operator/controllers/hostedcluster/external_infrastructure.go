package hostedcluster

import (
	"context"
	"fmt"
	"time"

	hyperv1 "github.com/openshift/hypershift/api/hypershift/v1beta1"
	"github.com/openshift/hypershift/hypershift-operator/controllers/hostedcluster/internal/platform/external"
	"github.com/openshift/hypershift/support/statuspatching"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// externalInfrastructureRequeue is how often an External HostedCluster is re-reconciled
// while the integrator is still provisioning.
//
// A periodic requeue rather than a watch, deliberately. Watching the integrator's objects
// would mean starting an informer on a group and kind chosen by whoever wrote the
// HostedCluster, which lets a tenant make the operator watch arbitrary resource types.
// Infrastructure provisioning takes minutes, so a minute of reporting latency costs
// nothing.
const externalInfrastructureRequeue = 60 * time.Second

// reconcileExternalInfrastructureStatus reports the integrator's progress on the
// HostedCluster and asks for a requeue while it is still working.
//
// It re-reads the hosted cluster object rather than reusing what the core HCP chain saw,
// so that the condition is still reported when that chain failed for an unrelated reason.
// Being able to see why an External cluster is stuck matters most exactly when something
// else is also broken.
func (r *HostedClusterReconciler) reconcileExternalInfrastructureStatus(
	ctx context.Context, hcluster *hyperv1.HostedCluster, controlPlaneNamespace string,
) (*time.Duration, error) {
	// How long the cluster has been waiting, taken from the condition's own transition
	// time. A condition that has been False since the HostedCluster was created has a
	// transition time of when it first went False, which is what we want.
	var waitingFor time.Duration
	if existing := meta.FindStatusCondition(hcluster.Status.Conditions, string(hyperv1.ExternalInfrastructureReady)); existing != nil && existing.Status != metav1.ConditionTrue {
		waitingFor = r.Clock.Since(existing.LastTransitionTime.Time)
	}

	condition, err := external.InfrastructureReadyCondition(ctx, r.UncachedClient, hcluster, controlPlaneNamespace, waitingFor)
	if err != nil {
		return nil, err
	}
	if err := statuspatching.PatchStatusCondition(ctx, r.Client, hcluster, &hcluster.Status.Conditions, condition); err != nil {
		return nil, fmt.Errorf("failed to set the %s condition: %w", hyperv1.ExternalInfrastructureReady, err)
	}

	if condition.Status == metav1.ConditionTrue {
		return nil, nil
	}
	requeue := externalInfrastructureRequeue
	return &requeue, nil
}
