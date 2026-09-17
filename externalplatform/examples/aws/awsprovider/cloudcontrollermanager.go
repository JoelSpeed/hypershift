package awsprovider

import (
	"context"
	"fmt"

	"github.com/openshift/hypershift/externalplatform/component"
	"github.com/openshift/hypershift/externalplatform/contract"
	"github.com/openshift/hypershift/externalplatform/reconcile"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"

	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
)

// cloudControllerManagerName is both the Deployment's name and the ControlPlaneComponent's.
// Keeping them the same is what makes `kubectl get controlplanecomponents` in a control plane
// namespace read as a list of what is running there.
const cloudControllerManagerName = "example-aws-cloud-controller-manager"

// reconcileCloudControllerManager runs the AWS cloud controller manager in the control plane
// namespace and reports it to HyperShift.
//
// It runs here rather than in the guest for the same reason every other control plane
// component does: the guest's nodes are exactly what it is responsible for bringing up, so a
// cloud controller manager that needed a working node to start could never start the first
// one.
//
// Reporting it is not optional bookkeeping. Having declared an external cloud controller
// manager, this integration has made every node in the cluster depend on this Deployment, and
// a HostedCluster that reports Available=True while it is crash-looping is telling its owner
// something false.
func (p *Provisioner) reconcileCloudControllerManager(ctx context.Context, request *reconcile.Request) error {
	hcp := request.HostedControlPlane

	// The service account carries no Kubernetes permissions at all: the cloud controller manager
	// talks to the guest with the kubeconfig mounted below, not with its own token. It exists to
	// be the pod's AWS identity, which a real deployment annotates for IRSA or pod identity.
	serviceAccount := &corev1.ServiceAccount{
		ObjectMeta: metav1.ObjectMeta{Namespace: hcp.Namespace, Name: cloudControllerManagerName},
	}
	if _, err := controllerutil.CreateOrUpdate(ctx, request.ManagementClient, serviceAccount, func() error {
		return nil
	}); err != nil {
		return fmt.Errorf("failed to reconcile the cloud controller manager service account: %w", err)
	}

	deployment := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Namespace: hcp.Namespace, Name: cloudControllerManagerName},
	}
	if _, err := controllerutil.CreateOrUpdate(ctx, request.ManagementClient, deployment, func() error {
		return p.mutateCloudControllerManager(deployment, request)
	}); err != nil {
		return fmt.Errorf("failed to reconcile the cloud controller manager deployment: %w", err)
	}

	// The library does the part that is easy to get wrong: it withholds the version until the
	// rollout is actually complete, holds the previous one during an upgrade rather than
	// blanking it, and sets an owner reference so the component cannot outlive the control
	// plane and wedge it. See the package documentation for what each of those prevents.
	return component.Report(ctx, request.ManagementClient, hcp, deployment)
}

func (p *Provisioner) mutateCloudControllerManager(deployment *appsv1.Deployment, request *reconcile.Request) error {
	labels := map[string]string{"app": cloudControllerManagerName}

	deployment.Spec.Replicas = ptr.To[int32](1)
	deployment.Spec.Selector = &metav1.LabelSelector{MatchLabels: labels}
	deployment.Spec.Template.ObjectMeta.Labels = labels
	deployment.Spec.Template.Spec.ServiceAccountName = cloudControllerManagerName
	deployment.Spec.Template.Spec.Volumes = []corev1.Volume{{
		Name: "guest-kubeconfig",
		VolumeSource: corev1.VolumeSource{
			Secret: &corev1.SecretVolumeSource{
				SecretName:  contract.GuestKubeconfigSecretName,
				DefaultMode: ptr.To[int32](0640),
			},
		},
	}}
	deployment.Spec.Template.Spec.Containers = []corev1.Container{{
		Name:  "cloud-controller-manager",
		Image: p.CloudControllerManagerImage,
		Command: []string{
			"/bin/aws-cloud-controller-manager",
			// Against the guest, over the service network, which is reachable before the
			// cluster's external endpoint is.
			fmt.Sprintf("--kubeconfig=/etc/guest-kubeconfig/%s", contract.GuestKubeconfigSecretKey),
			"--cloud-provider=aws",
			// The management cluster runs its own leader elections; this component is a
			// single replica scoped to one hosted cluster and has nothing to elect against.
			"--leader-elect=false",
			// Node lifecycle only. Routes and services are the guest cluster's own operators'
			// business, and a second controller writing them fights them.
			"--controllers=cloud-node,cloud-node-lifecycle",
			fmt.Sprintf("--cluster-name=%s", request.HostedControlPlane.Spec.InfraID),
		},
		VolumeMounts: []corev1.VolumeMount{{
			Name:      "guest-kubeconfig",
			MountPath: "/etc/guest-kubeconfig",
			ReadOnly:  true,
		}},
		Resources: corev1.ResourceRequirements{
			Requests: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse("75m"),
				corev1.ResourceMemory: resource.MustParse("60Mi"),
			},
		},
	}}
	return nil
}
