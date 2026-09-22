package v1alpha1

import (
	hyperv1 "github.com/openshift/hypershift/api/hypershift/v1beta1"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// AWSClusterConfig is where in AWS to build the cluster.
//
// This is the part a user writes, and the only part of either type that is specific to this
// integration rather than to the contract. It is defined once and embedded in both the
// template and the instance, because HyperShift copies it from one to the other verbatim.
//
// Bring-your-own network only: this example does not create VPCs, because an example that
// did would need real credentials and a real account to be worth reading. Cluster API
// Provider AWS will create a network if the AWSCluster asks it to, so adding that here is a
// field and a branch.
type AWSClusterConfig struct {
	// region is the AWS region to build the cluster in.
	//
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=63
	// +required
	Region string `json:"region,omitempty"`

	// vpcID is an existing VPC to place the cluster's nodes in.
	//
	// +kubebuilder:validation:Pattern=`^vpc-[0-9a-f]{8,17}$`
	// +required
	VPCID string `json:"vpcID,omitempty"`

	// subnetIDs are the subnets nodes may be placed in. One per availability zone the
	// cluster should span.
	//
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=10
	// +kubebuilder:validation:items:Pattern=`^subnet-[0-9a-f]{8,17}$`
	// +required
	SubnetIDs []string `json:"subnetIDs,omitempty"`

	// securityGroupID is an existing security group to attach to nodes. Cluster API
	// Provider AWS creates one if this is empty.
	//
	// +kubebuilder:validation:Pattern=`^sg-[0-9a-f]{8,17}$`
	// +optional
	SecurityGroupID string `json:"securityGroupID,omitempty"`

	// tags are applied to every AWS resource created for the cluster.
	//
	// +kubebuilder:validation:MaxProperties=25
	// +optional
	Tags map[string]string `json:"tags,omitempty"`
}

// AWSHostedClusterSpec is the instance's spec: the user's configuration, instantiated
// verbatim from the template, plus the one field HyperShift writes.
type AWSHostedClusterSpec struct {
	AWSClusterConfig `json:",inline"`

	// controlPlaneEndpoint is where the guest Kubernetes API server is published.
	//
	// Written by HyperShift rather than copied from the template, and the one part of the
	// spec it keeps reconciling. A structural schema prunes fields it does not declare, so
	// dropping this field silently discards the endpoint and the provider has nowhere to
	// point the cluster's nodes.
	//
	// +optional
	ControlPlaneEndpoint *hyperv1.APIEndpoint `json:"controlPlaneEndpoint,omitempty"`
}

// AWSHostedClusterStatus is written by the provider and read by HyperShift.
//
// Every field here is part of the External platform contract rather than of this
// integration, so this is the block an integrator copies unchanged. The provider does not
// write it directly: the reconciler does, through the contract package, which is what keeps
// the ordering rules the contract depends on.
type AWSHostedClusterStatus struct {
	// platform is the integration's one-time declaration of what platform this is and
	// whether it runs a cloud controller manager. HyperShift blocks node bootstrap until it
	// is present, and ignores changes once it has observed it.
	//
	// The type is HyperShift's own, so this block cannot drift from what HyperShift reads.
	//
	// +optional
	Platform *hyperv1.ExternalPlatformStatus `json:"platform,omitempty"`

	// infrastructure is the Cluster API infrastructure object the provider stood up.
	// HyperShift points the Cluster API Cluster's spec.infrastructureRef at it, and that
	// field is immutable, so this one is in practice immutable too.
	//
	// +optional
	Infrastructure *InfrastructureReference `json:"infrastructure,omitempty"`

	// conditions is the provider's report on this object. Ready is the only condition the
	// contract asks for, and its message is mirrored verbatim onto the HostedCluster, where
	// it is the only provider-specific detail a cluster's owner ever sees.
	//
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// InfrastructureReference names the Cluster API infrastructure object for this cluster.
//
// It is deliberately not a corev1.TypedLocalObjectReference or a Cluster API
// ContractVersionedObjectReference: the contract defines these three fields and nothing
// else, and borrowing a type whose shape can change independently would be a way for this
// object to stop matching what HyperShift reads.
type InfrastructureReference struct {
	// apiGroup is the API group of the infrastructure object, for example
	// infrastructure.cluster.x-k8s.io.
	//
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	// +required
	APIGroup string `json:"apiGroup,omitempty"`

	// kind is the kind of the infrastructure object, for example AWSCluster.
	//
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	// +required
	Kind string `json:"kind,omitempty"`

	// name is the name of the infrastructure object, which is always in the control plane
	// namespace alongside this object.
	//
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	// +required
	Name string `json:"name,omitempty"`
}

// AWSHostedCluster is instantiated by HyperShift into the control plane namespace, with the
// template's spec copied verbatim.
//
// It is where the two sides meet: HyperShift creates it and watches its status, the provider
// reconciles it and writes that status.
//
// +kubebuilder:object:root=true
// +kubebuilder:resource:scope=Namespaced
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].status`
// +kubebuilder:printcolumn:name="Region",type=string,JSONPath=`.spec.region`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
type AWSHostedCluster struct {
	metav1.TypeMeta `json:",inline"`
	// metadata is the standard object metadata.
	//
	// +optional
	metav1.ObjectMeta `json:"metadata,omitempty"`

	// spec is where in AWS to build the cluster.
	//
	// +required
	Spec AWSHostedClusterSpec `json:"spec,omitzero"`

	// status is the provider's report, and the contract's half of this object.
	//
	// +optional
	Status AWSHostedClusterStatus `json:"status,omitempty,omitzero"`
}

// AWSHostedClusterList contains a list of AWSHostedCluster.
//
// +kubebuilder:object:root=true
type AWSHostedClusterList struct {
	metav1.TypeMeta `json:",inline"`
	// metadata is the standard list metadata.
	//
	// +optional
	metav1.ListMeta `json:"metadata,omitempty"`

	// items is the list of AWSHostedClusters.
	Items []AWSHostedCluster `json:"items"`
}

// AWSHostedClusterTemplate is written by the user, in the HostedCluster's namespace, and
// named by that HostedCluster's spec.platform.external.hostedClusterTemplate.
//
// The label on this type is load-bearing rather than decorative: HyperShift reads it to
// detect version skew between itself and the integrator before the skew turns into silent
// misbehaviour, and rejects a HostedCluster whose template does not carry it.
//
// +kubebuilder:object:root=true
// +kubebuilder:resource:scope=Namespaced
// +kubebuilder:metadata:labels=hypershift.openshift.io/external-platform-contract=v1alpha1
type AWSHostedClusterTemplate struct {
	metav1.TypeMeta `json:",inline"`
	// metadata is the standard object metadata.
	//
	// +optional
	metav1.ObjectMeta `json:"metadata,omitempty"`

	// spec holds the template HyperShift instantiates.
	//
	// +required
	Spec AWSHostedClusterTemplateSpec `json:"spec,omitzero"`
}

// AWSHostedClusterTemplateSpec wraps the templated object, matching the template-and-instance
// shape Cluster API uses. HyperShift reads spec.template.spec and nothing else.
type AWSHostedClusterTemplateSpec struct {
	// template is the object to instantiate.
	//
	// +required
	Template AWSHostedClusterTemplateResource `json:"template,omitzero"`
}

// AWSHostedClusterTemplateResource is the templated object.
//
// Its spec is AWSClusterConfig rather than AWSHostedClusterSpec, because controlPlaneEndpoint
// is HyperShift's to write on the instance and is not something a user templates.
type AWSHostedClusterTemplateResource struct {
	// spec is copied verbatim into the instantiated AWSHostedCluster's spec.
	//
	// +required
	Spec AWSClusterConfig `json:"spec,omitzero"`
}

// AWSHostedClusterTemplateList contains a list of AWSHostedClusterTemplate.
//
// +kubebuilder:object:root=true
type AWSHostedClusterTemplateList struct {
	metav1.TypeMeta `json:",inline"`
	// metadata is the standard list metadata.
	//
	// +optional
	metav1.ListMeta `json:"metadata,omitempty"`

	// items is the list of AWSHostedClusterTemplates.
	Items []AWSHostedClusterTemplate `json:"items"`
}
