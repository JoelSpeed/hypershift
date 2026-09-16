package external

import (
	"context"

	"github.com/openshift/hypershift/cmd/cluster/core"
	"github.com/openshift/hypershift/cmd/log"

	"github.com/spf13/cobra"
)

func NewDestroyCommand(opts *core.DestroyOptions) *cobra.Command {
	cmd := &cobra.Command{
		Use:          "external",
		Short:        "Destroys a HostedCluster on an external platform",
		SilenceUsage: true,
	}

	logger := log.Log
	cmd.RunE = func(cmd *cobra.Command, args []string) error {
		if err := DestroyCluster(cmd.Context(), opts); err != nil {
			logger.Error(err, "Failed to destroy cluster")
			return err
		}
		return nil
	}

	return cmd
}

// DestroyCluster deletes the HostedCluster and waits for it to go away. There is no
// infrastructure teardown step here: the integrator's controller owns the provider
// resources and tears them down under its own finalizer as the objects HyperShift created
// are deleted. Unlike the in-tree platforms, the CLI has no credentials with which to
// clean up after a provider that fails to do so.
func DestroyCluster(ctx context.Context, o *core.DestroyOptions) error {
	hostedCluster, err := core.GetCluster(ctx, o)
	if err != nil {
		return err
	}
	if hostedCluster != nil {
		o.InfraID = hostedCluster.Spec.InfraID
	}
	return core.DestroyCluster(ctx, hostedCluster, o, nil)
}
