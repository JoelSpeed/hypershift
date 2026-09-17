package external

import (
	"context"
	"fmt"

	hyperv1 "github.com/openshift/hypershift/api/hypershift/v1beta1"
	"github.com/openshift/hypershift/support/externalplatform"
	"github.com/openshift/hypershift/support/k8sutil"
	"github.com/openshift/hypershift/support/upsert"

	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"sigs.k8s.io/controller-runtime/pkg/client"
)

// ProviderRBACName is the name of the Role and RoleBinding HyperShift mints in each control
// plane namespace for the registered integrator.
const ProviderRBACName = "external-platform-provider"

// ReconcileProviderRBAC grants the registered integrator access to this HostedCluster's
// control plane namespace, and nothing else.
//
// HyperShift mints the binding rather than letting the integrator create it, and that is
// the whole isolation story: the binding is namespaced and only appears in namespaces whose
// HostedCluster named this integrator's API group, so one partner cannot read another
// partner's control plane namespaces. An integrator that could bind itself could.
//
// What is deliberately not granted is as much of the contract as what is: no secrets beyond
// the two the provider is named, so no etcd encryption keys and no Kubernetes API server
// signing keys; nothing outside the control plane namespace; and no write access to the
// HostedControlPlane, whose status HyperShift owns.
func ReconcileProviderRBAC(ctx context.Context, c client.Client, createOrUpdate upsert.CreateOrUpdateFN,
	hcluster *hyperv1.HostedCluster, controlPlaneNamespace string, provider externalplatform.Provider) error {

	role := &rbacv1.Role{ObjectMeta: metav1.ObjectMeta{Namespace: controlPlaneNamespace, Name: ProviderRBACName}}
	if _, err := createOrUpdate(ctx, c, role, func() error {
		setProviderRBACMetadata(role, hcluster)
		role.Rules = providerPolicyRules(provider)
		return nil
	}); err != nil {
		return fmt.Errorf("failed to reconcile external platform provider Role: %w", err)
	}

	roleBinding := &rbacv1.RoleBinding{ObjectMeta: metav1.ObjectMeta{Namespace: controlPlaneNamespace, Name: ProviderRBACName}}
	if _, err := createOrUpdate(ctx, c, roleBinding, func() error {
		setProviderRBACMetadata(roleBinding, hcluster)
		roleBinding.RoleRef = rbacv1.RoleRef{
			APIGroup: rbacv1.GroupName,
			Kind:     "Role",
			Name:     role.Name,
		}
		roleBinding.Subjects = []rbacv1.Subject{{
			Kind:      "ServiceAccount",
			Name:      provider.ServiceAccount.Name,
			Namespace: provider.ServiceAccount.Namespace,
		}}
		return nil
	}); err != nil {
		return fmt.Errorf("failed to reconcile external platform provider RoleBinding: %w", err)
	}

	return nil
}

func providerPolicyRules(provider externalplatform.Provider) []rbacv1.PolicyRule {
	return []rbacv1.PolicyRule{
		{
			// The integrator's own types: the hosted cluster object HyperShift
			// instantiated, and whatever Cluster API infrastructure objects and machine
			// templates it defines alongside it.
			APIGroups: []string{provider.APIGroup},
			Resources: []string{rbacv1.ResourceAll},
			Verbs:     []string{rbacv1.VerbAll},
		},
		{
			// Cluster API objects, so the integrator's provider can act as a Cluster API
			// provider: reconcile its infrastructure object against the Cluster, and set
			// providerID and addresses on Machines.
			APIGroups: []string{"cluster.x-k8s.io", "infrastructure.cluster.x-k8s.io"},
			Resources: []string{rbacv1.ResourceAll},
			Verbs:     []string{rbacv1.VerbAll},
		},
		{
			// ControlPlaneComponent is the contract's health and version channel. It gates
			// the HostedControlPlane's Available condition and its version completion, so
			// the integrator needs both the object and its status.
			APIGroups: []string{hyperv1.GroupVersion.Group},
			Resources: []string{"controlplanecomponents", "controlplanecomponents/status"},
			Verbs:     []string{rbacv1.VerbAll},
		},
		{
			// Read-only: the HostedControlPlane is the integrator's view of what it is
			// provisioning for. Its status is HyperShift's to write.
			APIGroups: []string{hyperv1.GroupVersion.Group},
			Resources: []string{"hostedcontrolplanes"},
			Verbs:     []string{"get", "list", "watch"},
		},
		{
			// Named rather than blanket, so this does not become read access to the etcd
			// encryption key or the Kubernetes API server signing keys that live in the
			// same namespace. The kubeconfig is how the integrator reaches the guest
			// cluster; the pull secret is how its own workloads pull images.
			//
			// The service network kubeconfig rather than the admin one, because the
			// integrator's controller runs on the management cluster and so reaches the
			// Kubernetes API server the same way the control plane's own components do,
			// without depending on the external endpoint being published.
			APIGroups:     []string{corev1GroupName},
			Resources:     []string{"secrets"},
			ResourceNames: []string{"service-network-admin-kubeconfig", "pull-secret"},
			// list and watch cannot be restricted by resource name, so this is get only.
			// A controller that wants to react to these builds a single-object informer or
			// polls, which is what the library in the externalplatform module does.
			Verbs: []string{"get"},
		},
		{
			// Whatever the integrator runs per HostedCluster in this namespace: a cloud
			// controller manager, a CSI driver, its own webhook.
			APIGroups: []string{"apps"},
			Resources: []string{"deployments", "statefulsets"},
			Verbs:     []string{rbacv1.VerbAll},
		},
		{
			APIGroups: []string{corev1GroupName},
			Resources: []string{"services", "serviceaccounts", "configmaps"},
			Verbs:     []string{rbacv1.VerbAll},
		},
		{
			APIGroups: []string{corev1GroupName},
			Resources: []string{"pods", "pods/log", "events"},
			Verbs:     []string{"get", "list", "watch"},
		},
	}
}

// corev1GroupName is the empty string the core API group is spelled as in RBAC. Named so
// that the empty string in the rules above is obviously deliberate.
const corev1GroupName = ""

func setProviderRBACMetadata(obj client.Object, hcluster *hyperv1.HostedCluster) {
	annotations := obj.GetAnnotations()
	if annotations == nil {
		annotations = map[string]string{}
	}
	annotations[k8sutil.HostedClusterAnnotation] = client.ObjectKeyFromObject(hcluster).String()
	obj.SetAnnotations(annotations)
}
