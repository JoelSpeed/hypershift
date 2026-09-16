package external

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/openshift/hypershift/cmd/cluster/core"
	"github.com/openshift/hypershift/support/certs"
	"github.com/openshift/hypershift/support/testutil"
	"github.com/openshift/hypershift/test/integration/framework"

	utilrand "k8s.io/apimachinery/pkg/util/rand"

	"github.com/spf13/pflag"
)

func TestCreateCluster(t *testing.T) {
	utilrand.Seed(1234567890)
	certs.UnsafeSeed(1234567890)
	ctx := framework.InterruptableContext(t.Context())
	tempDir := t.TempDir()

	pullSecretFile := filepath.Join(tempDir, "pull-secret.json")

	if err := os.WriteFile(pullSecretFile, []byte(`fake`), 0600); err != nil {
		t.Fatalf("failed to write pullSecret: %v", err)
	}

	for _, testCase := range []struct {
		name string
		args []string
	}{
		{
			name: "When only the hosted cluster template is provided, it should render a cluster with no NodePool",
			args: []string{
				// if we don't set this, the machine's IP is looked up, which isn't portable
				"--api-server-address=fakeAddress",
				"--render-sensitive",
				"--name=example",
				"--pull-secret=" + pullSecretFile,
				"--hosted-cluster-template-api-group=example.io",
				"--hosted-cluster-template-resource=foohostedclustertemplates",
				"--hosted-cluster-template-name=my-infra",
			},
		},
		{
			name: "When a machine template is also provided, it should render a NodePool referencing it",
			args: []string{
				"--api-server-address=fakeAddress",
				"--render-sensitive",
				"--name=example",
				"--pull-secret=" + pullSecretFile,
				"--hosted-cluster-template-api-group=example.io",
				"--hosted-cluster-template-resource=foohostedclustertemplates",
				"--hosted-cluster-template-name=my-infra",
				"--machine-template-api-group=infrastructure.cluster.x-k8s.io",
				"--machine-template-resource=foomachinetemplates",
				"--machine-template-name=my-machines",
			},
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			flags := pflag.NewFlagSet(testCase.name, pflag.ContinueOnError)
			coreOpts := core.DefaultOptions()
			core.BindDeveloperOptions(coreOpts, flags)
			externalOpts := DefaultOptions()
			BindOptions(externalOpts, flags)
			if err := flags.Parse(testCase.args); err != nil {
				t.Fatalf("failed to parse flags: %v", err)
			}

			tempDir := t.TempDir()
			manifestsFile := filepath.Join(tempDir, "manifests.yaml")
			coreOpts.Render = true
			coreOpts.RenderInto = manifestsFile

			if err := core.CreateCluster(ctx, coreOpts, externalOpts); err != nil {
				t.Fatalf("failed to create cluster: %v", err)
			}

			manifests, err := os.ReadFile(manifestsFile)
			if err != nil {
				t.Fatalf("failed to read manifests file: %v", err)
			}
			testutil.CompareWithFixture(t, manifests)
		})
	}
}

func TestRawCreateOptionsValidate(t *testing.T) {
	t.Parallel()

	pullSecretFile := filepath.Join(t.TempDir(), "pull-secret.json")
	if err := os.WriteFile(pullSecretFile, []byte(`fake`), 0600); err != nil {
		t.Fatalf("failed to write pullSecret: %v", err)
	}

	testCases := []struct {
		name        string
		opts        *RawCreateOptions
		expectError string
	}{
		{
			name: "When no machine template flags are set, it should validate successfully",
			opts: &RawCreateOptions{
				ServicePublishingStrategy: NodePortServicePublishingStrategy,
				APIServerAddress:          "fakeAddress",
			},
		},
		{
			name: "When all machine template flags are set, it should validate successfully",
			opts: &RawCreateOptions{
				ServicePublishingStrategy: NodePortServicePublishingStrategy,
				APIServerAddress:          "fakeAddress",
				MachineTemplateAPIGroup:   "infrastructure.cluster.x-k8s.io",
				MachineTemplateResource:   "foomachinetemplates",
				MachineTemplateName:       "my-machines",
			},
		},
		{
			name: "When only some machine template flags are set, it should return an error",
			opts: &RawCreateOptions{
				ServicePublishingStrategy: NodePortServicePublishingStrategy,
				APIServerAddress:          "fakeAddress",
				MachineTemplateName:       "my-machines",
			},
			expectError: "must be set together",
		},
		{
			name: "When the service publishing strategy is unknown, it should return an error",
			opts: &RawCreateOptions{
				ServicePublishingStrategy: "Nonsense",
			},
			expectError: "is not supported",
		},
		{
			name: "When an api server address is set for the LoadBalancer strategy, it should return an error",
			opts: &RawCreateOptions{
				ServicePublishingStrategy: LoadBalancerServicePublishingStrategy,
				APIServerAddress:          "fakeAddress",
			},
			expectError: "only for NodePort service publishing strategy",
		},
	}

	// core.CreateOptions cannot be built outside its own package, so go through the
	// public path to get one. Render is set so Validate never tries to look up the
	// management cluster's node addresses.
	coreRawOpts := core.DefaultOptions()
	coreRawOpts.Name = "example"
	coreRawOpts.PullSecretFile = pullSecretFile
	coreRawOpts.Render = true
	coreValidated, err := coreRawOpts.Validate(t.Context())
	if err != nil {
		t.Fatalf("failed to validate core options: %v", err)
	}
	coreOpts, err := coreValidated.Complete()
	if err != nil {
		t.Fatalf("failed to complete core options: %v", err)
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			_, err := tc.opts.Validate(t.Context(), coreOpts)
			if tc.expectError == "" {
				if err != nil {
					t.Fatalf("expected no error, got %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("expected an error containing %q, got none", tc.expectError)
			}
			if !strings.Contains(err.Error(), tc.expectError) {
				t.Fatalf("expected an error containing %q, got %v", tc.expectError, err)
			}
		})
	}
}
