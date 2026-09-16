package external

import (
	"context"

	hyperv1 "github.com/openshift/hypershift/api/hypershift/v1beta1"
	"github.com/openshift/hypershift/cmd/nodepool/core"

	crclient "sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/spf13/cobra"
)

type ExternalPlatformCreateOptions struct {
	MachineTemplateAPIGroup string
	MachineTemplateResource string
	MachineTemplateName     string
}

func NewExternalPlatformCreateOptions(_ *cobra.Command) *ExternalPlatformCreateOptions {
	return &ExternalPlatformCreateOptions{
		MachineTemplateAPIGroup: "infrastructure.cluster.x-k8s.io",
	}
}

func NewCreateCommand(coreOpts *core.CreateNodePoolOptions) *cobra.Command {
	cmd, platformOpts := newCreateCommandWithOpts()
	cmd.RunE = coreOpts.CreateRunFunc(platformOpts)
	return cmd
}

func newCreateCommandWithOpts() (*cobra.Command, *ExternalPlatformCreateOptions) {
	cmd := &cobra.Command{
		Use:          "external",
		Short:        "Creates basic functional NodePool resources for an external platform",
		SilenceUsage: true,
	}

	platformOpts := NewExternalPlatformCreateOptions(cmd)
	cmd.Flags().StringVar(&platformOpts.MachineTemplateAPIGroup, "machine-template-api-group", platformOpts.MachineTemplateAPIGroup, "The Cluster API group of the machine template, which must be a .cluster.x-k8s.io group")
	cmd.Flags().StringVar(&platformOpts.MachineTemplateResource, "machine-template-resource", platformOpts.MachineTemplateResource, "The plural resource name of the machine template, e.g. foomachinetemplates")
	cmd.Flags().StringVar(&platformOpts.MachineTemplateName, "machine-template-name", platformOpts.MachineTemplateName, "The name of the machine template, which must exist in the same namespace as the NodePool")
	_ = cmd.MarkFlagRequired("machine-template-resource")
	_ = cmd.MarkFlagRequired("machine-template-name")

	return cmd, platformOpts
}

func (o *ExternalPlatformCreateOptions) UpdateNodePool(_ context.Context, nodePool *hyperv1.NodePool, _ *hyperv1.HostedCluster, _ crclient.Client) error {
	nodePool.Spec.Platform.External = hyperv1.ExternalNodePoolPlatform{
		MachineTemplate: hyperv1.ExternalTemplateReference{
			APIGroup: o.MachineTemplateAPIGroup,
			Resource: o.MachineTemplateResource,
			Name:     o.MachineTemplateName,
		},
	}
	return nil
}

func (o *ExternalPlatformCreateOptions) Type() hyperv1.PlatformType {
	return hyperv1.ExternalPlatform
}
