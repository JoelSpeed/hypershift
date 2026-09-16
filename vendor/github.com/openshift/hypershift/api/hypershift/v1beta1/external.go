package v1beta1

// ExternalPlatformSpec specifies configuration for clusters running on a platform that
// HyperShift has no built-in knowledge of. All provider behaviour is supplied by a
// controller owned and released by an integrator, which HyperShift drives through a
// published contract rather than through compiled-in code.
type ExternalPlatformSpec struct {
	// hostedClusterTemplate is a reference to an integrator-owned hosted cluster template
	// in the same namespace as this HostedCluster. HyperShift instantiates the template
	// into the control plane namespace, where the integrator's controller observes it and
	// begins provisioning.
	//
	// The referenced object implements a contract published by HyperShift. It is not a
	// Cluster API infrastructure template, even though it borrows the same
	// template-and-instance shape. Everything else about the platform, including its name
	// and whether it runs a cloud controller manager, is declared by the integrator on the
	// instantiated object's status rather than configured here.
	//
	// It is immutable. The template is read once, when the instantiated object is created,
	// so repointing it afterwards would silently have no effect.
	//
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="hostedClusterTemplate is immutable"
	// +required
	HostedClusterTemplate ExternalTemplateReference `json:"hostedClusterTemplate,omitzero"`
}

// ExternalTemplateReference names a template object in the local namespace by API group
// and resource.
//
// Resource rather than kind: a resource is what a client resolves against without a
// discovery round-trip, it is unambiguous where a kind served by more than one resource is
// not, and it is the vocabulary RBAC is written in, so an administrator's registration
// grant and a user-supplied reference can be compared directly.
type ExternalTemplateReference struct {
	// apiGroup is the API group of the referenced template, for example example.io.
	// The core group is not a valid value: the referenced object is always a custom
	// resource.
	//
	// +kubebuilder:validation:XValidation:rule=`self.matches('^[a-z0-9]([-a-z0-9]*[a-z0-9])?(\\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)*$')`,message="apiGroup must be a lowercase RFC 1123 subdomain"
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	// +required
	APIGroup string `json:"apiGroup,omitempty"`

	// resource is the lowercase plural resource name of the referenced template, for
	// example foohostedclustertemplates.
	//
	// HyperShift derives the resource of the object it creates by stripping the templates
	// suffix, so foohostedclustertemplates yields foohostedclusters. Integrators must
	// therefore name their custom resources conventionally; a resource that does not
	// resolve is reported as invalid configuration on the HostedCluster.
	//
	// +kubebuilder:validation:XValidation:rule=`self.matches('^[a-z][a-z0-9]*$')`,message="resource must be a lowercase alphanumeric plural resource name"
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=63
	// +required
	Resource string `json:"resource,omitempty"`

	// name is the name of the referenced template.
	//
	// +kubebuilder:validation:XValidation:rule=`self.matches('^[a-z0-9]([-a-z0-9]*[a-z0-9])?(\\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)*$')`,message="name must be a lowercase RFC 1123 subdomain"
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	// +required
	Name string `json:"name,omitempty"`
}

// ExternalNodePoolPlatform specifies the configuration used for NodePools on an external
// platform.
type ExternalNodePoolPlatform struct {
	// machineTemplate is a reference to a Cluster API infrastructure machine template in
	// the same namespace as this NodePool. HyperShift instantiates a copy of it per
	// configuration hash into the control plane namespace and points a MachineDeployment
	// at that copy, exactly as it does for the in-tree platforms.
	//
	// Unlike the HostedCluster's hostedClusterTemplate, this really is a Cluster API
	// object: the MachineDeployment resolves it directly, so interposing a HyperShift type
	// would buy nothing. Its apiGroup is therefore constrained to a Cluster API group,
	// where the HyperShift Operator already holds the access it needs.
	//
	// Editing the referenced template is the supported way to change instance shape or
	// image; it triggers a rolling upgrade, as editing an in-tree platform's NodePool
	// configuration does.
	//
	// +kubebuilder:validation:XValidation:rule="self.apiGroup.endsWith('.cluster.x-k8s.io')",message="machineTemplate.apiGroup must be a Cluster API group"
	// +required
	MachineTemplate ExternalTemplateReference `json:"machineTemplate,omitzero"`
}

// ExternalPlatformStatus records what the integrator declared this platform to be.
//
// The integrator publishes the declaration on the instantiated hosted cluster object, and
// HyperShift copies it here the first time it observes it. It is recorded rather than read
// afresh because both values reach the guest cluster's Infrastructure, which reaches the
// machine config server, which reaches the NodePool configuration hash: a later change
// would roll every node in the cluster onto a configuration that disagrees with the one
// the cluster was installed with. HyperShift therefore reports a subsequent change as a
// degraded condition instead of acting on it.
//
// Nothing here is user-configurable. The corresponding spec carries only a reference to
// the integrator's template, because a platform's identity and architecture are properties
// of the integration rather than of an individual cluster.
type ExternalPlatformStatus struct {
	// name is the platform's name as declared by the integrator, for example ExampleCloud.
	// It is propagated verbatim to the guest cluster's Infrastructure at
	// status.platformStatus.external.platformName, which is where everything running in
	// the cluster looks to identify the platform it is running on.
	//
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	// +required
	Name string `json:"name,omitempty"`

	// cloudControllerManager declares whether the integrator runs a cloud controller
	// manager for this platform.
	//
	// +required
	CloudControllerManager ExternalCloudControllerManagerStatus `json:"cloudControllerManager,omitzero"`
}

// ExternalCloudControllerManagerStatus declares whether a cloud controller manager runs
// for an external platform.
type ExternalCloudControllerManagerStatus struct {
	// state is either External or None.
	//
	// External means the integrator runs a cloud controller manager. Kubelets are started
	// with --cloud-provider=external, so nodes join tainted with
	// node.cloudprovider.kubernetes.io/uninitialized and the integrator's cloud controller
	// manager must remove that taint. A cluster whose cloud controller manager never
	// arrives has a healthy control plane and no schedulable nodes.
	//
	// None means no cloud controller manager runs. Nodes join untainted, and the
	// integrator's machine controller is responsible for node addresses and provider IDs.
	//
	// +kubebuilder:validation:Enum=External;None
	// +required
	State ExternalCloudControllerManagerState `json:"state,omitempty"`
}

// ExternalCloudControllerManagerState describes whether a cloud controller manager runs
// for an external platform.
type ExternalCloudControllerManagerState string

const (
	// ExternalCloudControllerManager means the integrator runs a cloud controller manager.
	ExternalCloudControllerManager ExternalCloudControllerManagerState = "External"

	// NoCloudControllerManager means no cloud controller manager runs.
	NoCloudControllerManager ExternalCloudControllerManagerState = "None"
)
