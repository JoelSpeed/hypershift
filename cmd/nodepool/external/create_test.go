package external

import (
	"testing"

	hyperv1 "github.com/openshift/hypershift/api/hypershift/v1beta1"
	"github.com/openshift/hypershift/cmd/nodepool/core"

	"github.com/google/go-cmp/cmp"
)

func TestNewCreateCommand(t *testing.T) {
	coreOpts := &core.CreateNodePoolOptions{}
	cmd := NewCreateCommand(coreOpts)

	if cmd.Use != "external" {
		t.Errorf("expected Use to be %q, got %q", "external", cmd.Use)
	}
	if cmd.RunE == nil {
		t.Error("expected RunE to be set")
	}

	// The API constrains the machine template to a Cluster API group, so the flag defaults
	// to the group every in-tree infrastructure provider uses rather than to nothing.
	flag := cmd.Flag("machine-template-api-group")
	if flag == nil {
		t.Fatal("expected machine-template-api-group flag to be registered")
	}
	if flag.DefValue != "infrastructure.cluster.x-k8s.io" {
		t.Errorf("expected machine-template-api-group default to be %q, got %q", "infrastructure.cluster.x-k8s.io", flag.DefValue)
	}
	for _, name := range []string{"machine-template-resource", "machine-template-name"} {
		if cmd.Flag(name) == nil {
			t.Errorf("expected %s flag to be registered", name)
		}
	}
}

func TestUpdateNodePool(t *testing.T) {
	t.Parallel()
	testCases := []struct {
		name     string
		opts     *ExternalPlatformCreateOptions
		expected hyperv1.ExternalNodePoolPlatform
	}{
		{
			name: "When a machine template is provided, it should set the machine template reference",
			opts: &ExternalPlatformCreateOptions{
				MachineTemplateAPIGroup: "infrastructure.cluster.x-k8s.io",
				MachineTemplateResource: "foomachinetemplates",
				MachineTemplateName:     "my-machines",
			},
			expected: hyperv1.ExternalNodePoolPlatform{
				MachineTemplate: hyperv1.ExternalTemplateReference{
					APIGroup: "infrastructure.cluster.x-k8s.io",
					Resource: "foomachinetemplates",
					Name:     "my-machines",
				},
			},
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			nodePool := &hyperv1.NodePool{
				Spec: hyperv1.NodePoolSpec{
					Arch: string(hyperv1.ArchitectureAMD64),
					Platform: hyperv1.NodePoolPlatform{
						Type: hyperv1.ExternalPlatform,
					},
				},
			}

			if err := tc.opts.UpdateNodePool(t.Context(), nodePool, nil, nil); err != nil {
				t.Fatalf("failed to update nodepool: %v", err)
			}
			if diff := cmp.Diff(tc.expected, nodePool.Spec.Platform.External); diff != "" {
				t.Errorf("unexpected machine template (-want +got):\n%s", diff)
			}
		})
	}
}

func TestType(t *testing.T) {
	t.Parallel()
	opts := &ExternalPlatformCreateOptions{}
	if opts.Type() != hyperv1.ExternalPlatform {
		t.Errorf("expected Type() to return %q, got %q", hyperv1.ExternalPlatform, opts.Type())
	}
}
