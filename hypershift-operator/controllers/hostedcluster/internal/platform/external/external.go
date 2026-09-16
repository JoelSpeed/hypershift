// Package external implements the Platform interface for platforms whose provider
// behavior lives outside HyperShift.
//
// Almost every method is a no-op by design. The integrator runs its own Cluster API
// provider as a fleet singleton alongside the HyperShift Operator, so HyperShift has no
// provider Deployment to reconcile, no cloud credentials to copy, and no KMS backend to
// wire up. What remains is ReconcileCAPIInfraCR, which instantiates the integrator's
// template into the control plane namespace; that lands in a later change, so for now
// this behaves exactly like the None platform and an External HostedCluster reaches a
// running control plane with no infrastructure and no workers.
//
// OrphanDeleter is deliberately not implemented: HyperShift cannot reason about whether
// third-party infrastructure it has never seen has been orphaned.
package external

import (
	"context"

	hyperv1 "github.com/openshift/hypershift/api/hypershift/v1beta1"
	"github.com/openshift/hypershift/support/upsert"

	appsv1 "k8s.io/api/apps/v1"
	rbacv1 "k8s.io/api/rbac/v1"

	"sigs.k8s.io/controller-runtime/pkg/client"
)

type External struct{}

func (p External) ReconcileCAPIInfraCR(ctx context.Context, c client.Client, createOrUpdate upsert.CreateOrUpdateFN,
	hcluster *hyperv1.HostedCluster,
	controlPlaneNamespace string, apiEndpoint hyperv1.APIEndpoint) (client.Object, error) {
	return nil, nil
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
