package external

import (
	"context"
	"fmt"

	hyperv1 "github.com/openshift/hypershift/api/hypershift/v1beta1"
	"github.com/openshift/hypershift/cmd/cluster/core"

	crclient "sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

const (
	NodePortServicePublishingStrategy     = "NodePort"
	LoadBalancerServicePublishingStrategy = "LoadBalancer"
)

type RawCreateOptions struct {
	ServicePublishingStrategy string
	APIServerAddress          string

	HostedClusterTemplateAPIGroup string
	HostedClusterTemplateResource string
	HostedClusterTemplateName     string

	MachineTemplateAPIGroup string
	MachineTemplateResource string
	MachineTemplateName     string
}

func DefaultOptions() *RawCreateOptions {
	return &RawCreateOptions{
		// NodePort rather than LoadBalancer, matching None and Agent: nothing about an
		// external platform implies the management cluster can provision load balancers.
		ServicePublishingStrategy: NodePortServicePublishingStrategy,
	}
}

// validatedCreateOptions is a private wrapper that enforces a call of Validate() before Complete() can be invoked.
type validatedCreateOptions struct {
	*RawCreateOptions
}

type ValidatedCreateOptions struct {
	// Embed a private pointer that cannot be instantiated outside of this package.
	*validatedCreateOptions
}

func (o *RawCreateOptions) Validate(ctx context.Context, opts *core.CreateOptions) (core.PlatformCompleter, error) {
	if o.ServicePublishingStrategy != NodePortServicePublishingStrategy && o.ServicePublishingStrategy != LoadBalancerServicePublishingStrategy {
		return nil, fmt.Errorf("service publishing strategy %s is not supported, supported options: %s, %s", o.ServicePublishingStrategy, NodePortServicePublishingStrategy, LoadBalancerServicePublishingStrategy)
	}
	if o.ServicePublishingStrategy != NodePortServicePublishingStrategy && o.APIServerAddress != "" {
		return nil, fmt.Errorf("--api-server-address is supported only for NodePort service publishing strategy, service publishing strategy %s is used", o.ServicePublishingStrategy)
	}
	if o.APIServerAddress == "" && o.ServicePublishingStrategy == NodePortServicePublishingStrategy && !opts.Render {
		var err error
		if o.APIServerAddress, err = core.GetAPIServerAddressByNode(ctx, opts.Log, opts.Kubeconfig); err != nil {
			return nil, err
		}
	}

	// The machine template flags are all-or-nothing. A NodePool without one would be
	// rejected by the API server, so a partially specified reference has to fail here
	// rather than produce an object that cannot be created.
	machineTemplateSet := o.MachineTemplateAPIGroup != "" || o.MachineTemplateResource != "" || o.MachineTemplateName != ""
	machineTemplateComplete := o.MachineTemplateAPIGroup != "" && o.MachineTemplateResource != "" && o.MachineTemplateName != ""
	if machineTemplateSet && !machineTemplateComplete {
		return nil, fmt.Errorf("--machine-template-api-group, --machine-template-resource and --machine-template-name must be set together")
	}

	return &ValidatedCreateOptions{
		validatedCreateOptions: &validatedCreateOptions{
			RawCreateOptions: o,
		},
	}, nil
}

// completedCreateOptions is a private wrapper that enforces a call of Complete() before cluster creation can be invoked.
type completedCreateOptions struct {
	*ValidatedCreateOptions

	externalDNSDomain string
}

type CreateOptions struct {
	// Embed a private pointer that cannot be instantiated outside of this package.
	*completedCreateOptions
}

func (o *ValidatedCreateOptions) Complete(ctx context.Context, opts *core.CreateOptions) (core.Platform, error) {
	return &CreateOptions{
		completedCreateOptions: &completedCreateOptions{
			ValidatedCreateOptions: o,
			externalDNSDomain:      opts.ExternalDNSDomain,
		},
	}, nil
}

func (o *CreateOptions) ApplyPlatformSpecifics(cluster *hyperv1.HostedCluster) error {
	if cluster.Spec.DNS.BaseDomain == "" {
		cluster.Spec.DNS.BaseDomain = "example.com"
	}
	cluster.Spec.Platform = hyperv1.PlatformSpec{
		Type: hyperv1.ExternalPlatform,
		External: hyperv1.ExternalPlatformSpec{
			HostedClusterTemplate: hyperv1.ExternalTemplateReference{
				APIGroup: o.HostedClusterTemplateAPIGroup,
				Resource: o.HostedClusterTemplateResource,
				Name:     o.HostedClusterTemplateName,
			},
		},
	}

	switch o.ServicePublishingStrategy {
	case NodePortServicePublishingStrategy:
		cluster.Spec.Services = core.GetServicePublishingStrategyMappingByAPIServerAddress(o.APIServerAddress, cluster.Spec.Networking.NetworkType)
	case LoadBalancerServicePublishingStrategy:
		cluster.Spec.Services = core.GetIngressServicePublishingStrategyMapping(cluster.Spec.Networking.NetworkType, o.externalDNSDomain != "", false)
	default:
		return fmt.Errorf("service publishing strategy %s is not supported", o.ServicePublishingStrategy)
	}

	return nil
}

// GenerateNodePools returns no NodePools unless a machine template was named. Which
// machine template a NodePool should use is a property of the integrator's provider that
// HyperShift has no way to guess, and a NodePool without one cannot be created, so the
// default is a cluster with no workers rather than an object that fails to apply.
func (o *CreateOptions) GenerateNodePools(defaultNodePool core.DefaultNodePoolConstructor) []*hyperv1.NodePool {
	if o.MachineTemplateName == "" {
		return nil
	}

	nodePool := defaultNodePool(hyperv1.ExternalPlatform, "")
	nodePool.Spec.Platform.External = hyperv1.ExternalNodePoolPlatform{
		MachineTemplate: hyperv1.ExternalTemplateReference{
			APIGroup: o.MachineTemplateAPIGroup,
			Resource: o.MachineTemplateResource,
			Name:     o.MachineTemplateName,
		},
	}
	if nodePool.Spec.Management.UpgradeType == "" {
		nodePool.Spec.Management.UpgradeType = hyperv1.UpgradeTypeReplace
	}
	return []*hyperv1.NodePool{nodePool}
}

func (o *CreateOptions) GenerateResources() ([]crclient.Object, error) {
	return nil, nil
}

var _ core.Platform = (*CreateOptions)(nil)

func BindOptions(opts *RawCreateOptions, flags *pflag.FlagSet) {
	flags.StringVar(&opts.ServicePublishingStrategy, "service-publishing-strategy", opts.ServicePublishingStrategy, fmt.Sprintf("Define how to expose the cluster services. Supported options: %s (Use LoadBalancer and Route to expose services), %s (Select a random node to expose service access through NodePort)", LoadBalancerServicePublishingStrategy, NodePortServicePublishingStrategy))
	flags.StringVar(&opts.APIServerAddress, "api-server-address", opts.APIServerAddress, "The IP address to be used for the hosted cluster's Kubernetes API communication. Only used with NodePort service publishing strategy. Requires management cluster connectivity if left unset.")

	flags.StringVar(&opts.HostedClusterTemplateAPIGroup, "hosted-cluster-template-api-group", opts.HostedClusterTemplateAPIGroup, "The API group of the integrator's hosted cluster template, e.g. example.io")
	flags.StringVar(&opts.HostedClusterTemplateResource, "hosted-cluster-template-resource", opts.HostedClusterTemplateResource, "The plural resource name of the integrator's hosted cluster template, e.g. foohostedclustertemplates")
	flags.StringVar(&opts.HostedClusterTemplateName, "hosted-cluster-template-name", opts.HostedClusterTemplateName, "The name of the integrator's hosted cluster template, which must exist in the same namespace as the HostedCluster")

	flags.StringVar(&opts.MachineTemplateAPIGroup, "machine-template-api-group", opts.MachineTemplateAPIGroup, "The Cluster API group of the machine template for the generated NodePool, e.g. infrastructure.cluster.x-k8s.io")
	flags.StringVar(&opts.MachineTemplateResource, "machine-template-resource", opts.MachineTemplateResource, "The plural resource name of the machine template for the generated NodePool, e.g. foomachinetemplates")
	flags.StringVar(&opts.MachineTemplateName, "machine-template-name", opts.MachineTemplateName, "The name of the machine template for the generated NodePool. If unset, no NodePool is generated.")
}

func NewCreateCommand(opts *core.RawCreateOptions) *cobra.Command {
	cmd := &cobra.Command{
		Use:          "external",
		Short:        "Creates basic functional HostedCluster resources on an external platform",
		SilenceUsage: true,
	}

	externalOpts := DefaultOptions()
	BindOptions(externalOpts, cmd.Flags())
	_ = cmd.MarkFlagRequired("hosted-cluster-template-api-group")
	_ = cmd.MarkFlagRequired("hosted-cluster-template-resource")
	_ = cmd.MarkFlagRequired("hosted-cluster-template-name")
	_ = cmd.MarkPersistentFlagRequired("pull-secret")

	cmd.RunE = func(cmd *cobra.Command, args []string) error {
		ctx := cmd.Context()
		if opts.Timeout > 0 {
			var cancel context.CancelFunc
			ctx, cancel = context.WithTimeout(ctx, opts.Timeout)
			defer cancel()
		}

		if err := core.CreateCluster(ctx, opts, externalOpts); err != nil {
			opts.Log.Error(err, "Failed to create cluster")
			return err
		}
		return nil
	}

	return cmd
}
