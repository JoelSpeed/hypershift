// Package component publishes a ControlPlaneComponent for a workload an integrator runs in
// a control plane namespace.
//
// Any workload an integrator runs there, a cloud controller manager or a CSI controller,
// has to publish one. HyperShift aggregates every ControlPlaneComponent in the namespace
// into the HostedControlPlane's Available condition and into its version rollout, with no
// allowlist and no registry filter, so an integrator's workload is part of both whether or
// not it says anything.
//
// Both of those aggregations have a failure mode that is silent, cluster-wide, and very
// hard to attribute to the component that caused it. This package exists so that an
// integrator gets them right without having to have read HyperShift's aggregation code:
//
//   - A blank or stale status.version permanently blocks the control plane's version
//     rollout. Report sets it from the HostedControlPlane and, exactly as HyperShift's own
//     components do, only once the rollout is complete.
//   - Nothing garbage-collects a ControlPlaneComponent that the integrator stops managing,
//     and Available=True on every item in the namespace is what gates the cluster. Report
//     sets an owner reference to the HostedControlPlane so the object dies with the control
//     plane, and Remove exists for a component that goes away before the control plane does.
package component

import (
	"context"
	"fmt"

	hyperv1 "github.com/openshift/hypershift/api/hypershift/v1beta1"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"

	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
)

// Report publishes the ControlPlaneComponent for a Deployment the integrator manages.
//
// The component takes the Deployment's name and namespace, so one call per workload per
// reconcile is the whole integration. It is safe to call before the Deployment has any
// status, and safe to call repeatedly.
//
// The HostedControlPlane must be the one in the same namespace, and must have been read
// this reconcile: the version and the owner reference both come off it.
func Report(ctx context.Context, c client.Client, hcp *hyperv1.HostedControlPlane, deployment *appsv1.Deployment) error {
	if deployment.Namespace != hcp.Namespace {
		return fmt.Errorf("deployment %s/%s is not in the control plane namespace %s", deployment.Namespace, deployment.Name, hcp.Namespace)
	}

	available, availableReason, availableMessage := deploymentAvailable(deployment)
	rolledOut, rolloutReason, rolloutMessage := deploymentRolledOut(deployment)

	component := &hyperv1.ControlPlaneComponent{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: deployment.Namespace,
			Name:      deployment.Name,
		},
	}
	if _, err := controllerutil.CreateOrUpdate(ctx, c, component, func() error {
		// An owner reference rather than a finalizer or a cleanup pass, because the case
		// that matters is the control plane being deleted while the integrator's controller
		// is down. A ControlPlaneComponent that outlived its control plane would be an
		// object nobody is reconciling in a namespace that is trying to go away.
		return controllerutil.SetControllerReference(hcp, component, c.Scheme())
	}); err != nil {
		return fmt.Errorf("failed to reconcile control plane component %s/%s: %w", component.Namespace, component.Name, err)
	}

	meta.SetStatusCondition(&component.Status.Conditions, metav1.Condition{
		Type:    string(hyperv1.ControlPlaneComponentAvailable),
		Status:  available,
		Reason:  availableReason,
		Message: availableMessage,
	})
	meta.SetStatusCondition(&component.Status.Conditions, metav1.Condition{
		Type:    string(hyperv1.ControlPlaneComponentRolloutComplete),
		Status:  rolledOut,
		Reason:  rolloutReason,
		Message: rolloutMessage,
	})

	// Only on a complete rollout, matching HyperShift's own components. Reporting the target
	// version while the rollout is still going would tell the control plane the upgrade had
	// finished when the old pods are still serving.
	if rolledOut == metav1.ConditionTrue {
		version := ControlPlaneVersion(hcp)
		if version == "" {
			// The control plane operator has not published a version yet, which happens on
			// the first reconciles of a new cluster. Leaving the previous value alone is
			// the safe move: an empty one blocks rollout completion for the whole cluster.
			return fmt.Errorf("the hosted control plane %s/%s has not reported a control plane version yet", hcp.Namespace, hcp.Name)
		}
		component.Status.Version = version
	}

	component.Status.ObservedGeneration = hcp.Generation
	component.Status.Resources = []hyperv1.ComponentResource{{
		Kind:  "Deployment",
		Group: appsv1.GroupName,
		Name:  deployment.Name,
	}}

	if err := c.Status().Update(ctx, component); err != nil {
		return fmt.Errorf("failed to update control plane component %s/%s status: %w", component.Namespace, component.Name, err)
	}
	return nil
}

// Remove deletes a ControlPlaneComponent the integrator no longer manages.
//
// Call this when a workload is removed from a running control plane, for instance because
// the integrator stopped shipping it. Nothing else will: HyperShift requires Available=True
// on every ControlPlaneComponent in the namespace, so one that is left behind holds the
// whole cluster Available=False for as long as the control plane lives.
//
// Deleting an object that is already gone is not an error.
func Remove(ctx context.Context, c client.Client, namespace, name string) error {
	component := &hyperv1.ControlPlaneComponent{
		ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: name},
	}
	if err := c.Delete(ctx, component); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("failed to delete control plane component %s/%s: %w", namespace, name, err)
	}
	return nil
}

// ControlPlaneVersion returns the release version the control plane is reconciling towards,
// which is the version a ControlPlaneComponent has to report before HyperShift will call a
// version rollout complete. It is empty until the control plane operator has published one.
func ControlPlaneVersion(hcp *hyperv1.HostedControlPlane) string {
	return hcp.Status.ControlPlaneVersion.Desired.Version
}

func deploymentAvailable(deployment *appsv1.Deployment) (metav1.ConditionStatus, string, string) {
	for _, condition := range deployment.Status.Conditions {
		if condition.Type != appsv1.DeploymentAvailable {
			continue
		}
		if condition.Status == corev1.ConditionTrue {
			return metav1.ConditionTrue, hyperv1.AsExpectedReason, fmt.Sprintf("Deployment %s is available", deployment.Name)
		}
		return metav1.ConditionFalse, hyperv1.WaitingForAvailableReason, fmt.Sprintf("Deployment %s is not available: %s", deployment.Name, condition.Message)
	}
	return metav1.ConditionFalse, hyperv1.NotFoundReason, fmt.Sprintf("%s Deployment Available condition not found", deployment.Name)
}

// deploymentRolledOut mirrors HyperShift's own readiness check rather than reading the
// Progressing condition, which stays True for ten minutes after a rollout stalls.
func deploymentRolledOut(deployment *appsv1.Deployment) (metav1.ConditionStatus, string, string) {
	replicas := ptr.Deref(deployment.Spec.Replicas, 0)
	if replicas != deployment.Status.AvailableReplicas ||
		replicas != deployment.Status.ReadyReplicas ||
		replicas != deployment.Status.UpdatedReplicas ||
		replicas != deployment.Status.Replicas ||
		deployment.Status.UnavailableReplicas != 0 ||
		deployment.Generation != deployment.Status.ObservedGeneration {
		return metav1.ConditionFalse, "WaitingForRolloutComplete",
			fmt.Sprintf("Waiting for deployment %s rollout to finish: %d out of %d new replicas have been updated",
				deployment.Name, deployment.Status.UpdatedReplicas, replicas)
	}
	return metav1.ConditionTrue, hyperv1.AsExpectedReason, fmt.Sprintf("Deployment %s successfully rolled out", deployment.Name)
}
