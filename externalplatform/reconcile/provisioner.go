// Package reconcile turns the External platform contract into an interface with two
// methods.
//
// The contract asks an integrator to hold a finalizer, declare a platform before reporting
// ready, name a Cluster API infrastructure object, keep a Ready condition current, and tear
// everything down in the right order. None of that is provider work, all of it has a wrong
// way to do it that fails quietly, and every integrator would otherwise write it again.
// Implement Provisioner, hand it to a Reconciler, and the ordering is not yours to get
// wrong.
//
// An integrator that wants to drive the contract directly can use the contract package on
// its own; this package is built on top of it and adds no facts of its own.
package reconcile

import (
	"context"
	"time"

	hyperv1 "github.com/openshift/hypershift/api/hypershift/v1beta1"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"sigs.k8s.io/controller-runtime/pkg/client"
)

// Provisioner is the provider-specific half of an External platform integration.
//
// Both methods are called repeatedly and must be idempotent: the reconciler calls them
// again on every requeue, on every watch event, and after every restart, and it has no way
// to tell a first call from a hundredth.
type Provisioner interface {
	// Platform declares what this platform is, once per integration rather than once per
	// cluster. Almost every implementation returns two constants.
	//
	// name is propagated verbatim to the guest's
	// Infrastructure.status.platformStatus.external.platformName, which is where everything
	// running in the cluster looks to identify its platform.
	//
	// cloudControllerManager decides the kubelet's cloud provider. Returning External means
	// every node boots with --cloud-provider=external and is tainted
	// node.cloudprovider.kubernetes.io/uninitialized, and the integrator's cloud controller
	// manager must remove that taint or the cluster will have zero usable nodes behind a
	// control plane that looks healthy. Returning None means neither.
	//
	// The reconciler publishes this before the object ever reports ready, so an integrator
	// cannot trip HyperShift's block-until-declared rule by accident. It is recorded once
	// and never re-read, so changing it for an existing cluster does nothing except raise a
	// degraded condition.
	Platform() (name string, cloudControllerManager hyperv1.ExternalCloudControllerManagerState)

	// Provision is called until it reports done. Report progress through Result's reason
	// and message: HyperShift mirrors the message verbatim onto the HostedCluster, where it
	// is the only provider-specific detail an operator will ever see.
	//
	// Returning an error requeues with backoff and surfaces the error as the message, so a
	// transient cloud failure needs no special handling. A condition the provider cannot
	// recover from on its own is better reported as not-done with an explanatory message
	// than as a permanent error, because the message is what reaches the user.
	Provision(ctx context.Context, request *Request) (ProvisionResult, error)

	// Deprovision is called while the hosted cluster object is being deleted, and the
	// reconciler removes the finalizer only once it reports done.
	//
	// By the time this is called, the Cluster API Cluster and the integrator's own
	// infrastructure object are already gone, and the control plane namespace, the guest
	// kubeconfig and the integrator's own RoleBinding are all still there. That ordering is
	// HyperShift's to maintain and is the reason this hook exists rather than a bare
	// finalizer.
	Deprovision(ctx context.Context, request *Request) (DeprovisionResult, error)
}

// Request is everything the reconciler has already resolved by the time a Provisioner is
// called.
type Request struct {
	// HostedClusterObject is the object HyperShift instantiated from the integrator's
	// template, in the control plane namespace. Its spec is the integrator's own type. A
	// Provisioner may read it freely but should write to it only through the contract
	// package, and only via the Result types below where they cover the field.
	HostedClusterObject *unstructured.Unstructured

	// HostedControlPlane is the control plane this object belongs to, read this reconcile.
	HostedControlPlane *hyperv1.HostedControlPlane

	// ControlPlaneEndpoint is where the guest Kubernetes API server is published.
	// HyperShift does not create the hosted cluster object until it knows this, so it is
	// always populated.
	ControlPlaneEndpoint hyperv1.APIEndpoint

	// ManagementClient reaches the management cluster. It is scoped by the RBAC HyperShift
	// minted for the registered provider ServiceAccount, which is namespaced to the control
	// plane namespaces of HostedClusters that named this integrator's API group.
	ManagementClient client.Client

	// GuestClient reaches the guest cluster over the service network, the same way the
	// control plane's own components do, so it does not depend on the external endpoint
	// being published or reachable.
	//
	// Nil unless the Reconciler was built with GuestClient enabled, and nil on Deprovision
	// once the guest is gone. Check it.
	GuestClient client.Client
}

// ProvisionResult is what a Provisioner reports back.
type ProvisionResult struct {
	// Infrastructure names the Cluster API infrastructure object the provider created in
	// the control plane namespace. HyperShift creates no Cluster API Cluster until all
	// three values are present, and the Cluster's spec.infrastructureRef is immutable
	// afterwards, so report it as soon as the object exists rather than once it is ready.
	//
	// Zero until the provider has created it.
	Infrastructure InfrastructureReference

	// Done reports that provisioning is complete and the cluster can proceed. It is what
	// becomes Ready=True on the hosted cluster object and ExternalInfrastructureReady=True
	// on the HostedCluster.
	Done bool

	// Reason is a CamelCase machine-readable reason, defaulted when empty.
	Reason string

	// Message is the human-readable detail, mirrored verbatim onto the HostedCluster.
	// Write it for an operator who cannot see the provider's logs.
	Message string

	// RequeueAfter asks for another reconcile after a fixed delay, for polling a cloud
	// operation that has no watchable representation. Zero means the reconciler decides.
	RequeueAfter time.Duration
}

// DeprovisionResult is what a Provisioner reports back while tearing down.
type DeprovisionResult struct {
	// Done reports that teardown is complete. The reconciler then removes its finalizer,
	// which is what lets the HostedCluster finish deleting, so do not report it until the
	// provider has actually released everything it holds.
	Done bool

	// Reason and Message describe progress, and are surfaced the same way as during
	// provisioning. A teardown that is stuck is the case an operator is most likely to be
	// looking at by hand, so say what it is waiting for.
	Reason  string
	Message string

	// RequeueAfter asks for another reconcile after a fixed delay. Zero means the
	// reconciler decides.
	RequeueAfter time.Duration
}

// InfrastructureReference names the Cluster API infrastructure object, carrying no version
// to match Cluster API's own ContractVersionedObjectReference. HyperShift resolves the
// served version from the management cluster.
type InfrastructureReference struct {
	APIGroup string
	Kind     string
	Name     string
}

// IsZero reports whether the provider has named an infrastructure object yet.
func (r InfrastructureReference) IsZero() bool {
	return r.APIGroup == "" && r.Kind == "" && r.Name == ""
}
