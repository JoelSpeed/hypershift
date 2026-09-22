// Package v1alpha1 is the AWS reference integration's own API.
//
// Two types, and the relationship between their names is the contract: HyperShift takes the
// template resource a HostedCluster names, strips the "templates" suffix to get the instance
// resource, and instantiates the instance in the control plane namespace. Rename either one
// and HyperShift will not find the other.
//
// The inputs a user supplies are defined once, in AWSClusterConfig, which both types embed.
// That is the reason this package exists rather than a hand-written pair of custom resource
// definitions: HyperShift copies the template's spec.template.spec into the instance's spec
// verbatim, so the two schemas have to agree exactly, and a structural schema may not use
// $ref to share them. Written by hand they are the same fields typed out twice, and nothing
// fails loudly when a change lands in only one of them. Defined here and generated, they
// cannot disagree.
//
// Neither type is a Cluster API type. The Cluster API infrastructure object for this
// integration is Cluster API Provider AWS's own AWSCluster, which the provider creates.
//
// +kubebuilder:object:generate=true
// +groupName=aws.example.hypershift.openshift.io
package v1alpha1

import (
	"k8s.io/apimachinery/pkg/runtime/schema"

	"sigs.k8s.io/controller-runtime/pkg/scheme"
)

var (
	// GroupVersion is this integrator's own group and version. It is the single source of
	// truth for the group: the provider derives its constants from it, and controller-gen
	// derives the custom resource definitions from the types registered against it.
	GroupVersion = schema.GroupVersion{Group: "aws.example.hypershift.openshift.io", Version: "v1alpha1"}

	// SchemeBuilder registers this integration's types with a scheme.
	SchemeBuilder = &scheme.Builder{GroupVersion: GroupVersion}

	// AddToScheme adds this integration's types to a scheme.
	AddToScheme = SchemeBuilder.AddToScheme
)

func init() {
	SchemeBuilder.Register(
		&AWSHostedCluster{}, &AWSHostedClusterList{},
		&AWSHostedClusterTemplate{}, &AWSHostedClusterTemplateList{},
	)
}
