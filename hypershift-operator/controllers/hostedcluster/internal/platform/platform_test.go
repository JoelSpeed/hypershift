package platform

import (
	"testing"

	. "github.com/onsi/gomega"

	hyperv1 "github.com/openshift/hypershift/api/hypershift/v1beta1"
	"github.com/openshift/hypershift/hypershift-operator/controllers/hostedcluster/internal/platform/external"

	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestGetPlatform(t *testing.T) {
	t.Parallel()
	uncachedClient := fake.NewClientBuilder().Build()

	testCases := []struct {
		name         string
		platformType hyperv1.PlatformType
		options      []Option
		expected     func(client.Client) Platform
		expectError  bool
	}{
		{
			name:         "When the platform is External, it should return the external platform",
			platformType: hyperv1.ExternalPlatform,
			options:      []Option{WithUncachedClient(uncachedClient)},
			// The uncached client has to reach the platform: the External platform reads
			// integrator-defined types, and going through the operator's cache would start
			// an informer on a CRD that may not be installed.
			expected: func(c client.Client) Platform { return external.New(c) },
		},
		{
			name:         "When the platform is unknown, it should return an error",
			platformType: hyperv1.PlatformType("NotAPlatform"),
			expectError:  true,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			g := NewWithT(t)

			hcluster := &hyperv1.HostedCluster{
				Spec: hyperv1.HostedClusterSpec{
					Platform: hyperv1.PlatformSpec{Type: tc.platformType},
				},
			}

			// A nil pull secret keeps GetPlatform off the payload image lookup path, which
			// needs a release provider. The External platform never takes that path at all:
			// its Cluster API provider is released by the integrator, not in the payload.
			platform, err := GetPlatform(t.Context(), hcluster, nil, "", nil, tc.options...)
			if tc.expectError {
				g.Expect(err).To(HaveOccurred())
				g.Expect(platform).To(BeNil())
				return
			}

			g.Expect(err).ToNot(HaveOccurred())
			g.Expect(platform).To(Equal(tc.expected(uncachedClient)))
		})
	}
}

// TestExternalCAPIProviderDeploymentSpec pins the behavior that lets an External
// HostedCluster reach a running control plane with no provider of HyperShift's own: a nil
// DeploymentSpec is how reconcileCAPIProvider is told this platform has no provider to
// deploy. Returning an empty spec instead would have it reconcile a broken Deployment.
func TestExternalCAPIProviderDeploymentSpec(t *testing.T) {
	t.Parallel()
	g := NewWithT(t)

	spec, err := external.External{}.CAPIProviderDeploymentSpec(&hyperv1.HostedCluster{}, &hyperv1.HostedControlPlane{})
	g.Expect(err).ToNot(HaveOccurred())
	g.Expect(spec).To(BeNil())
}
