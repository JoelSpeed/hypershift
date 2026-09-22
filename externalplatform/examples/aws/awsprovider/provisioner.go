// Package awsprovider is a worked External platform integration for AWS.
//
// It exists to be read. Every in-tree HyperShift platform is thousands of lines spread over
// twenty switch statements; this is what the same platform looks like on the other side of
// the contract, and it is one file of provider logic plus a cloud controller manager.
//
// The design is the one an integrator should copy:
//
//   - The integrator's own type, AWSHostedCluster, carries the inputs a HyperShift user
//     supplies. It is not a Cluster API type and does not pretend to be.
//   - The Cluster API infrastructure object is Cluster API Provider AWS's own AWSCluster.
//     The contract asks for a Cluster API infrastructure object, and CAPA already publishes
//     a good one; inventing a second would mean reimplementing its controller too.
//   - The provisioner translates one into the other and reports the result. That translation
//     is the whole integration.
//
// The AWSCluster is created as unstructured rather than by importing CAPA's types, so that
// this example, and an integrator who copies it, does not take a dependency on CAPA's module
// to set six fields. An integrator that already vendors CAPA should use the typed API.
package awsprovider

import (
	"context"
	"fmt"

	"github.com/openshift/hypershift/externalplatform/contract"
	awsv1alpha1 "github.com/openshift/hypershift/externalplatform/examples/aws/api/v1alpha1"
	"github.com/openshift/hypershift/externalplatform/reconcile"

	hyperv1 "github.com/openshift/hypershift/api/hypershift/v1beta1"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
)

const (
	// PlatformName is propagated verbatim to the guest cluster's Infrastructure at
	// status.platformStatus.external.platformName. It is what everything running in the
	// cluster reads to identify the platform it is on.
	//
	// Deliberately not "AWS": a guest that claims to be the in-tree AWS platform would have
	// operators in it looking for in-tree AWS behaviour that an external integration does not
	// provide. The External platform is a different platform that happens to run on AWS.
	PlatformName = "ExampleAWS"

	// TemplateResource is what a HostedCluster's platform.external.hostedClusterTemplate
	// names. HyperShift strips the templates suffix to find awshostedclusters.
	TemplateResource = "awshostedclustertemplates"
)

var (
	// APIGroup is this integrator's own group, and the key an administrator registers when
	// granting this provider access to control plane namespaces. It is taken from the API
	// package rather than spelled again, so that it cannot say one thing here and another
	// in the custom resource definitions generated from those types.
	APIGroup = awsv1alpha1.GroupVersion.Group

	// Version is the served version of this integrator's types.
	Version = awsv1alpha1.GroupVersion.Version

	// HostedClusterObjectGVK is the GVK HyperShift instantiates from the integrator's template.
	HostedClusterObjectGVK = awsv1alpha1.GroupVersion.WithKind("AWSHostedCluster")

	// capaAWSClusterGVK is the Cluster API infrastructure object this integration stands up.
	capaAWSClusterGVK = schema.GroupVersionKind{
		Group:   "infrastructure.cluster.x-k8s.io",
		Version: "v1beta2",
		Kind:    "AWSCluster",
	}
)

// AddToScheme registers everything this integration reads or writes.
//
// The integration's own types are registered typed, because they are this repository's to
// define and a provider that can decode its own spec into a struct is easier to read than one
// that walks maps. Cluster API Provider AWS's are registered as unstructured, so that this
// binary does not vendor CAPA to set six fields. That is a choice about dependencies rather
// than about correctness: the client treats a registered unstructured type exactly as it
// treats a typed one.
//
// Registering the integration's types typed does not change how the reconciler reads them. It
// asks for the object as unstructured, and the client takes the group, version and kind off
// the object itself in that case rather than looking them up in the scheme.
func AddToScheme(scheme *runtime.Scheme) error {
	for _, add := range []func(*runtime.Scheme) error{
		corev1.AddToScheme,
		appsv1.AddToScheme,
		// HostedControlPlane and ControlPlaneComponent, the only two HyperShift types an
		// integrator touches.
		hyperv1.AddToScheme,
		// AWSHostedCluster and AWSHostedClusterTemplate.
		awsv1alpha1.AddToScheme,
	} {
		if err := add(scheme); err != nil {
			return fmt.Errorf("failed to build the scheme: %w", err)
		}
	}
	scheme.AddKnownTypeWithName(capaAWSClusterGVK, &unstructured.Unstructured{})
	scheme.AddKnownTypeWithName(capaAWSClusterGVK.GroupVersion().WithKind(capaAWSClusterGVK.Kind+"List"), &unstructured.UnstructuredList{})
	metav1.AddToGroupVersion(scheme, capaAWSClusterGVK.GroupVersion())
	return nil
}

// Provisioner implements the External platform contract for AWS.
//
// It is stateless. Everything it needs comes in on the Request, which is what lets one
// instance serve every HostedCluster on a management cluster.
type Provisioner struct {
	// CloudControllerManagerImage is the AWS cloud controller manager deployed into each
	// control plane namespace. Empty disables the deployment, which is only useful for
	// testing: a cluster that declares an external cloud controller manager and never runs
	// one has no schedulable nodes.
	CloudControllerManagerImage string
}

var _ reconcile.Provisioner = &Provisioner{}

// Platform declares what this integration is, once for every cluster it serves.
//
// External rather than None because AWS nodes need their provider ID, addresses, zone labels
// and route table entries set by something, and the AWS cloud controller manager is what does
// that. Declaring External is also a promise: kubelets start with --cloud-provider=external
// and nodes join tainted node.cloudprovider.kubernetes.io/uninitialized, so if the cloud
// controller manager below never runs, the cluster has a healthy control plane and no
// schedulable nodes.
func (p *Provisioner) Platform() (string, hyperv1.ExternalCloudControllerManagerState) {
	return PlatformName, hyperv1.ExternalCloudControllerManager
}

// Provision translates the integrator's object into an AWSCluster and reports on it.
func (p *Provisioner) Provision(ctx context.Context, request *reconcile.Request) (reconcile.ProvisionResult, error) {
	spec, err := readSpec(request.HostedClusterObject)
	if err != nil {
		// A spec this controller cannot read is not going to become readable on a retry, so
		// report it where the cluster's owner sees it rather than failing in a loop.
		return reconcile.ProvisionResult{
			Reason:  "InvalidConfiguration",
			Message: err.Error(),
		}, nil
	}

	awsCluster := &unstructured.Unstructured{}
	awsCluster.SetGroupVersionKind(capaAWSClusterGVK)
	awsCluster.SetNamespace(request.HostedControlPlane.Namespace)
	awsCluster.SetName(request.HostedControlPlane.Name)

	// The infrastructure reference is reported whether or not the object is ready, and from
	// the moment it exists. HyperShift creates no Cluster API Cluster until it is named, and
	// a Cluster's spec.infrastructureRef is immutable afterwards, so naming it late only
	// delays the Cluster and naming it differently later is not possible at all.
	infrastructure := reconcile.InfrastructureReference{
		APIGroup: capaAWSClusterGVK.Group,
		Kind:     capaAWSClusterGVK.Kind,
		Name:     awsCluster.GetName(),
	}

	if _, err := controllerutil.CreateOrUpdate(ctx, request.ManagementClient, awsCluster, func() error {
		return mutateAWSCluster(awsCluster, spec, request)
	}); err != nil {
		return reconcile.ProvisionResult{Infrastructure: infrastructure}, fmt.Errorf("failed to reconcile the AWSCluster: %w", err)
	}

	if p.CloudControllerManagerImage != "" {
		if err := p.reconcileCloudControllerManager(ctx, request); err != nil {
			return reconcile.ProvisionResult{Infrastructure: infrastructure}, fmt.Errorf("failed to reconcile the cloud controller manager: %w", err)
		}
	}

	ready, message := awsClusterReady(awsCluster)
	if !ready {
		return reconcile.ProvisionResult{
			Infrastructure: infrastructure,
			Reason:         "WaitingForAWSCluster",
			Message:        fmt.Sprintf("Waiting for the AWSCluster to be provisioned: %s", message),
		}, nil
	}

	return reconcile.ProvisionResult{
		Infrastructure: infrastructure,
		Done:           true,
		Message:        fmt.Sprintf("AWS infrastructure in %s is ready", spec.Region),
	}, nil
}

// Deprovision deletes the AWSCluster and waits for CAPA to finish with it.
//
// Nothing here deletes the cloud controller manager: it lives in the control plane namespace,
// which HyperShift deletes once this reports done. Deleting it early would only remove the
// thing keeping existing nodes usable while the rest of the cluster tears down.
func (p *Provisioner) Deprovision(ctx context.Context, request *reconcile.Request) (reconcile.DeprovisionResult, error) {
	awsCluster := &unstructured.Unstructured{}
	awsCluster.SetGroupVersionKind(capaAWSClusterGVK)
	awsCluster.SetNamespace(request.HostedControlPlane.Namespace)
	awsCluster.SetName(request.HostedControlPlane.Name)

	err := request.ManagementClient.Get(ctx, client.ObjectKeyFromObject(awsCluster), awsCluster)
	switch {
	case apierrors.IsNotFound(err):
		// CAPA has released it, so everything it owned in AWS is gone.
		return reconcile.DeprovisionResult{Done: true}, nil
	case err != nil:
		return reconcile.DeprovisionResult{}, fmt.Errorf("failed to read the AWSCluster: %w", err)
	}

	if awsCluster.GetDeletionTimestamp().IsZero() {
		if err := request.ManagementClient.Delete(ctx, awsCluster); err != nil && !apierrors.IsNotFound(err) {
			return reconcile.DeprovisionResult{}, fmt.Errorf("failed to delete the AWSCluster: %w", err)
		}
	}

	// Not done. Reporting done here would release the finalizer on the hosted cluster object,
	// and HyperShift would delete the control plane namespace out from under CAPA while it
	// was still tearing down load balancers.
	return reconcile.DeprovisionResult{
		Reason:  "WaitingForAWSCluster",
		Message: "Waiting for Cluster API Provider AWS to finish destroying the AWSCluster",
	}, nil
}

// readSpec decodes the integrator's own object into the integrator's own type.
//
// The reconciler hands the object over as unstructured, because the contract is defined in
// terms of fields rather than of any integrator's Go types, and this is where an integration
// converts to the types it actually wrote. Everything below this line works on a struct.
//
// The required fields are checked again here even though the custom resource definition
// already rejects an object without them. A provider is not the only thing that writes these
// objects during a cluster's life, and one that assumes its schema was enforced fails by
// provisioning something wrong rather than by saying so.
func readSpec(hostedClusterObject *unstructured.Unstructured) (awsv1alpha1.AWSHostedClusterSpec, error) {
	typed := &awsv1alpha1.AWSHostedCluster{}
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(hostedClusterObject.Object, typed); err != nil {
		return awsv1alpha1.AWSHostedClusterSpec{}, fmt.Errorf("failed to read the spec: %w", err)
	}
	spec := typed.Spec

	switch {
	case spec.Region == "":
		return spec, fmt.Errorf("spec.region is required")
	case spec.VPCID == "":
		return spec, fmt.Errorf("spec.vpcID is required")
	case len(spec.SubnetIDs) == 0:
		return spec, fmt.Errorf("spec.subnetIDs must name at least one subnet")
	}
	return spec, nil
}

// mutateAWSCluster writes the CAPA object's spec.
//
// Only the fields this integration owns are written, so that a field CAPA or an administrator
// set is not reverted on the next reconcile.
func mutateAWSCluster(awsCluster *unstructured.Unstructured, spec awsv1alpha1.AWSHostedClusterSpec, request *reconcile.Request) error {
	labels := awsCluster.GetLabels()
	if labels == nil {
		labels = map[string]string{}
	}
	// Cluster API's own label, which is what makes the object show up in a kubectl get
	// filtered by cluster and what CAPA's controllers expect to find.
	labels["cluster.x-k8s.io/cluster-name"] = request.HostedControlPlane.Name
	awsCluster.SetLabels(labels)

	if err := unstructured.SetNestedField(awsCluster.Object, spec.Region, "spec", "region"); err != nil {
		return err
	}

	// The endpoint HyperShift published. CAPA would otherwise create a load balancer for a
	// control plane it does not run: the API server is already served by the management
	// cluster, and this is where it is.
	if err := unstructured.SetNestedMap(awsCluster.Object, map[string]any{
		"host": request.ControlPlaneEndpoint.Host,
		"port": int64(request.ControlPlaneEndpoint.Port),
	}, "spec", "controlPlaneEndpoint"); err != nil {
		return err
	}
	if err := unstructured.SetNestedField(awsCluster.Object, "none", "spec", "controlPlaneLoadBalancer", "loadBalancerType"); err != nil {
		return err
	}

	network := map[string]any{
		"vpc": map[string]any{"id": spec.VPCID},
	}
	subnets := make([]any, 0, len(spec.SubnetIDs))
	for _, subnetID := range spec.SubnetIDs {
		subnets = append(subnets, map[string]any{"id": subnetID})
	}
	network["subnets"] = subnets
	if spec.SecurityGroupID != "" {
		network["securityGroupOverrides"] = map[string]any{"node": spec.SecurityGroupID}
	}
	if err := unstructured.SetNestedMap(awsCluster.Object, network, "spec", "network"); err != nil {
		return err
	}

	if len(spec.Tags) > 0 {
		tags := make(map[string]any, len(spec.Tags))
		for key, value := range spec.Tags {
			tags[key] = value
		}
		if err := unstructured.SetNestedMap(awsCluster.Object, tags, "spec", "additionalTags"); err != nil {
			return err
		}
	}
	return nil
}

// awsClusterReady reads CAPA's own readiness, in both the shapes Cluster API's infrastructure
// contract has used, and returns whatever CAPA said about why it is not ready.
//
// The message matters more than it looks: HyperShift mirrors this integration's Ready message
// verbatim onto the HostedCluster, so CAPA's account of a missing subnet is the only thing a
// cluster's owner will ever see about it.
func awsClusterReady(awsCluster *unstructured.Unstructured) (bool, string) {
	if provisioned, found, _ := unstructured.NestedBool(awsCluster.Object, "status", "initialization", "provisioned"); found && provisioned {
		return true, ""
	}
	if ready, found, _ := unstructured.NestedBool(awsCluster.Object, "status", "ready"); found && ready {
		return true, ""
	}

	conditions, _, _ := unstructured.NestedSlice(awsCluster.Object, "status", "conditions")
	for _, item := range conditions {
		entry, ok := item.(map[string]any)
		if !ok {
			continue
		}
		if conditionType, _, _ := unstructured.NestedString(entry, "type"); conditionType != contract.ReadyConditionType {
			continue
		}
		if status, _, _ := unstructured.NestedString(entry, "status"); status == string(metav1.ConditionTrue) {
			continue
		}
		message, _, _ := unstructured.NestedString(entry, "message")
		reason, _, _ := unstructured.NestedString(entry, "reason")
		if message != "" {
			return false, message
		}
		if reason != "" {
			return false, reason
		}
	}
	return false, "Cluster API Provider AWS has not reported on it yet"
}
