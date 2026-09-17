package reconcile

import (
	"context"
	"fmt"
	"strconv"
	"time"

	hyperv1 "github.com/openshift/hypershift/api/hypershift/v1beta1"
	"github.com/openshift/hypershift/externalplatform/contract"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/tools/clientcmd"

	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

const (
	// defaultRequeue is how often provisioning is retried when the provider reports
	// progress but no error and asks for nothing in particular. Cloud provisioning happens
	// on the order of minutes, and HyperShift's own reconcile of the hosted cluster object
	// polls at a comparable rate.
	defaultRequeue = 30 * time.Second
)

// ClientOptions is the client configuration an integrator's manager must be built with.
//
// HyperShift grants a registered integrator a namespaced Role in each control plane namespace
// it is entitled to, and nothing cluster-wide. A controller-runtime cache, by contrast, backs
// every read with a cluster-wide list and watch, so a single cached Get on a Secret asks for
// permission to read every Secret on the management cluster and fails with a Forbidden that
// names a namespace the provider never touched.
//
// Disabling the cache for the two types read by key turns those into direct Gets, which the
// namespaced grant permits. The hosted cluster objects the controller watches are unaffected:
// they are the integrator's own types, which it does hold cluster-wide access to.
//
//	ctrl.NewManager(config, ctrl.Options{Client: reconcile.ClientOptions(), ...})
func ClientOptions() client.Options {
	return client.Options{
		Cache: &client.CacheOptions{
			DisableFor: []client.Object{
				&corev1.Secret{},
				&hyperv1.HostedControlPlane{},
			},
		},
	}
}

// Reconciler drives a Provisioner through the External platform contract.
//
// Build one per integration, register it with SetupWithManager, and it watches the hosted
// cluster objects HyperShift instantiates from the integrator's template.
type Reconciler struct {
	// Client reaches the management cluster.
	Client client.Client

	// Provisioner does the provider work.
	Provisioner Provisioner

	// HostedClusterObjectGVK is the GVK of the object HyperShift instantiates from the
	// integrator's template, which is the template's own kind with the Template suffix
	// stripped: FooHostedClusterTemplate yields FooHostedCluster.
	HostedClusterObjectGVK schema.GroupVersionKind

	// FinalizerName is the finalizer held on the hosted cluster object while the provider
	// owns anything. Defaults to <apiGroup>/external-platform.
	//
	// This finalizer is what makes deletion ordering work, and also what makes a broken
	// provider controller able to make a HostedCluster undeletable. An administrator can
	// override that with the hypershift.openshift.io/force-external-cleanup annotation,
	// which strips it and very likely leaks whatever was provisioned.
	FinalizerName string

	// WithGuestClient populates Request.GuestClient from the service network kubeconfig.
	// Off by default, because building it needs the guest API server to be reachable and a
	// provider that does not touch the guest should not be blocked on that.
	WithGuestClient bool
}

// SetupWithManager registers the reconciler on the hosted cluster object's GVK.
//
// The manager's client must not cache the integrator's own types if the integrator's custom
// resource definitions might not be installed, which for a provider watching its own types
// is not a risk it has.
func (r *Reconciler) SetupWithManager(mgr ctrl.Manager) error {
	if r.Client == nil {
		r.Client = mgr.GetClient()
	}
	if err := r.applyDefaults(); err != nil {
		return err
	}

	hostedClusterObject := &unstructured.Unstructured{}
	hostedClusterObject.SetGroupVersionKind(r.HostedClusterObjectGVK)

	return ctrl.NewControllerManagedBy(mgr).
		For(hostedClusterObject).
		Named("externalplatform").
		Complete(r)
}

// applyDefaults fills in what the integrator did not set and rejects what it cannot guess.
func (r *Reconciler) applyDefaults() error {
	if r.Provisioner == nil {
		return fmt.Errorf("a Provisioner is required")
	}
	if r.HostedClusterObjectGVK.Empty() {
		return fmt.Errorf("a HostedClusterObjectGVK is required")
	}
	if r.FinalizerName == "" {
		r.FinalizerName = r.HostedClusterObjectGVK.Group + "/external-platform"
	}
	return nil
}

func (r *Reconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	hostedClusterObject := &unstructured.Unstructured{}
	hostedClusterObject.SetGroupVersionKind(r.HostedClusterObjectGVK)
	if err := r.Client.Get(ctx, req.NamespacedName, hostedClusterObject); err != nil {
		// Gone means HyperShift deleted it and the finalizer has already come off, so there
		// is nothing left to tear down and nothing to report it on.
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// Named after the HostedCluster, in the control plane namespace, so the HostedControlPlane
	// shares its name. This is the only lookup the provider needs to find the control plane.
	hcp := &hyperv1.HostedControlPlane{}
	if err := r.Client.Get(ctx, client.ObjectKey{Namespace: req.Namespace, Name: req.Name}, hcp); err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to get hosted control plane %s: %w", req.NamespacedName, err)
	}

	if paused, until := isPaused(hcp.Spec.PausedUntil); paused {
		// An operator pausing a control plane is debugging it. Provisioning against it
		// would fight whatever they are doing by hand.
		logger.Info("Reconciliation is paused for this hosted control plane")
		return ctrl.Result{RequeueAfter: until}, nil
	}

	endpoint, err := contract.ControlPlaneEndpoint(hostedClusterObject)
	if err != nil {
		return ctrl.Result{}, err
	}

	request := &Request{
		HostedClusterObject:  hostedClusterObject,
		HostedControlPlane:   hcp,
		ControlPlaneEndpoint: endpoint,
		ManagementClient:     r.Client,
	}

	if !hostedClusterObject.GetDeletionTimestamp().IsZero() {
		return r.deprovision(ctx, request)
	}
	return r.provision(ctx, request)
}

func (r *Reconciler) provision(ctx context.Context, request *Request) (ctrl.Result, error) {
	hostedClusterObject := request.HostedClusterObject

	// Before any provider work, so that a provider that crashes mid-provision has still
	// left something behind to tear down.
	if controllerutil.AddFinalizer(hostedClusterObject, r.FinalizerName) {
		if err := r.Client.Update(ctx, hostedClusterObject); err != nil {
			return ctrl.Result{}, fmt.Errorf("failed to add the finalizer: %w", err)
		}
	}

	original := hostedClusterObject.DeepCopy()

	// First, and unconditionally. HyperShift refuses to render the guest Infrastructure
	// until this is present, because guessing would bake the wrong cloud provider into
	// every node that boots, and nodes cannot boot before the control plane knows its
	// platform. Writing it here rather than leaving it to the Provisioner means an
	// integrator cannot report ready without it by accident.
	name, cloudControllerManager := r.Provisioner.Platform()
	if err := contract.SetPlatform(hostedClusterObject, name, cloudControllerManager); err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to declare the platform: %w", err)
	}

	if r.WithGuestClient {
		guestClient, err := r.guestClient(ctx, request.HostedControlPlane.Namespace)
		if err != nil {
			// Not fatal, and not silent: the guest API server comes up after the hosted
			// cluster object does, so this is expected early and alarming late.
			return r.report(ctx, original, hostedClusterObject, metav1.ConditionFalse,
				"WaitingForGuestCluster", fmt.Sprintf("Waiting for the guest cluster to become reachable: %v", err), defaultRequeue)
		}
		request.GuestClient = guestClient
	}

	result, err := r.Provisioner.Provision(ctx, request)
	if err != nil {
		// Reported as well as returned, so that a provider stuck in a retry loop says so on
		// the HostedCluster rather than only in its own logs.
		if reportErr := r.reportOnly(ctx, original, hostedClusterObject, metav1.ConditionFalse,
			reasonOr(result.Reason, "ProvisioningFailed"), err.Error()); reportErr != nil {
			return ctrl.Result{}, fmt.Errorf("%w (and failed to report it: %v)", err, reportErr)
		}
		return ctrl.Result{}, err
	}

	// As soon as the object exists, not once it is ready: HyperShift creates no Cluster API
	// Cluster until this is named, and a Cluster's spec.infrastructureRef is immutable, so
	// naming it late only delays the Cluster.
	if !result.Infrastructure.IsZero() {
		if err := contract.SetInfrastructure(hostedClusterObject, result.Infrastructure.APIGroup, result.Infrastructure.Kind, result.Infrastructure.Name); err != nil {
			return ctrl.Result{}, fmt.Errorf("failed to record the infrastructure object: %w", err)
		}
	}

	if result.Done {
		return r.report(ctx, original, hostedClusterObject, metav1.ConditionTrue,
			reasonOr(result.Reason, hyperv1.AsExpectedReason), messageOr(result.Message, "Infrastructure is ready"), result.RequeueAfter)
	}
	return r.report(ctx, original, hostedClusterObject, metav1.ConditionFalse,
		reasonOr(result.Reason, "Provisioning"), messageOr(result.Message, "Provisioning infrastructure"),
		requeueOr(result.RequeueAfter, defaultRequeue))
}

func (r *Reconciler) deprovision(ctx context.Context, request *Request) (ctrl.Result, error) {
	hostedClusterObject := request.HostedClusterObject
	if !controllerutil.ContainsFinalizer(hostedClusterObject, r.FinalizerName) {
		// Either teardown already finished, or an administrator used the force-cleanup
		// annotation to strip the finalizer. Either way there is nothing to hold up.
		return ctrl.Result{}, nil
	}

	original := hostedClusterObject.DeepCopy()

	if r.WithGuestClient {
		// Best effort only. By this point the guest may already be unreachable, and a
		// provider that cannot finish teardown without it would make the HostedCluster
		// undeletable, which is a far worse failure than a leaked guest-side object.
		if guestClient, err := r.guestClient(ctx, request.HostedControlPlane.Namespace); err == nil {
			request.GuestClient = guestClient
		}
	}

	result, err := r.Provisioner.Deprovision(ctx, request)
	if err != nil {
		if reportErr := r.reportOnly(ctx, original, hostedClusterObject, metav1.ConditionFalse,
			reasonOr(result.Reason, "DeprovisioningFailed"), err.Error()); reportErr != nil {
			return ctrl.Result{}, fmt.Errorf("%w (and failed to report it: %v)", err, reportErr)
		}
		return ctrl.Result{}, err
	}

	if !result.Done {
		return r.report(ctx, original, hostedClusterObject, metav1.ConditionFalse,
			reasonOr(result.Reason, "Deprovisioning"), messageOr(result.Message, "Destroying infrastructure"),
			requeueOr(result.RequeueAfter, defaultRequeue))
	}

	// Last, and only now. Removing the finalizer is what lets the HostedCluster finish
	// deleting, and there is no way back from it: HyperShift deletes the control plane
	// namespace next, taking the guest kubeconfig and the provider's own RoleBinding with it.
	if controllerutil.RemoveFinalizer(hostedClusterObject, r.FinalizerName) {
		if err := r.Client.Update(ctx, hostedClusterObject); err != nil {
			return ctrl.Result{}, fmt.Errorf("failed to remove the finalizer: %w", err)
		}
	}
	return ctrl.Result{}, nil
}

// report writes the Ready condition and patches the status, returning the result the
// reconcile should end on.
func (r *Reconciler) report(ctx context.Context, original, hostedClusterObject *unstructured.Unstructured, status metav1.ConditionStatus, reason, message string, requeueAfter time.Duration) (ctrl.Result, error) {
	if err := r.reportOnly(ctx, original, hostedClusterObject, status, reason, message); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{RequeueAfter: requeueAfter}, nil
}

func (r *Reconciler) reportOnly(ctx context.Context, original, hostedClusterObject *unstructured.Unstructured, status metav1.ConditionStatus, reason, message string) error {
	if err := contract.SetReady(hostedClusterObject, status, reason, message); err != nil {
		return err
	}
	// Patched rather than updated, so that a provider losing a conflict with HyperShift's
	// own writes to spec.controlPlaneEndpoint does not also lose the status it just wrote.
	if err := r.Client.Status().Patch(ctx, hostedClusterObject, client.MergeFrom(original)); err != nil {
		return fmt.Errorf("failed to patch the hosted cluster object status: %w", err)
	}
	return nil
}

// guestClient builds a client for the guest cluster from the service network kubeconfig.
//
// Built per reconcile rather than cached, because the kubeconfig's credentials rotate and a
// cached client would keep using the old ones until the provider restarted.
func (r *Reconciler) guestClient(ctx context.Context, controlPlaneNamespace string) (client.Client, error) {
	secret := &corev1.Secret{}
	if err := r.Client.Get(ctx, client.ObjectKey{Namespace: controlPlaneNamespace, Name: contract.GuestKubeconfigSecretName}, secret); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, fmt.Errorf("the guest kubeconfig secret %s/%s does not exist yet", controlPlaneNamespace, contract.GuestKubeconfigSecretName)
		}
		return nil, fmt.Errorf("failed to get the guest kubeconfig secret: %w", err)
	}

	kubeconfig, ok := secret.Data[contract.GuestKubeconfigSecretKey]
	if !ok {
		return nil, fmt.Errorf("the guest kubeconfig secret %s/%s has no %q key", controlPlaneNamespace, contract.GuestKubeconfigSecretName, contract.GuestKubeconfigSecretKey)
	}

	restConfig, err := clientcmd.RESTConfigFromKubeConfig(kubeconfig)
	if err != nil {
		return nil, fmt.Errorf("failed to build a rest config from the guest kubeconfig: %w", err)
	}
	guestClient, err := client.New(restConfig, client.Options{})
	if err != nil {
		return nil, fmt.Errorf("failed to build a client for the guest cluster: %w", err)
	}
	return guestClient, nil
}

// isPaused mirrors HyperShift's own handling of pausedUntil, which is either a boolean or
// an RFC3339 timestamp. An unparseable value is treated as not paused, because a typo that
// silently froze a cluster would be worse than one that did nothing.
func isPaused(pausedUntil *string) (bool, time.Duration) {
	if pausedUntil == nil {
		return false, 0
	}
	if paused, err := strconv.ParseBool(*pausedUntil); err == nil {
		return paused, 0
	}
	if until, err := time.Parse(time.RFC3339, *pausedUntil); err == nil {
		return time.Now().Before(until), time.Until(until)
	}
	return false, 0
}

func reasonOr(reason, fallback string) string {
	if reason == "" {
		return fallback
	}
	return reason
}

func messageOr(message, fallback string) string {
	if message == "" {
		return fallback
	}
	return message
}

func requeueOr(requeueAfter, fallback time.Duration) time.Duration {
	if requeueAfter == 0 {
		return fallback
	}
	return requeueAfter
}
