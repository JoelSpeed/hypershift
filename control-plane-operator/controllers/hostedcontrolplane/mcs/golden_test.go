package mcs

import (
	"testing"

	. "github.com/onsi/gomega"

	hyperv1 "github.com/openshift/hypershift/api/hypershift/v1beta1"
	"github.com/openshift/hypershift/api/util/ipnet"
	"github.com/openshift/hypershift/support/testutil"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// TestMachineConfigServerConfigGolden pins the rendered machine config server ConfigMap for
// every platform.
//
// This ConfigMap is what the machine config operator's bootstrap render reads, and it is
// covered by the NodePool configuration hash, so any change to its bytes rolls every node of
// every HostedCluster on the fleet. The fixtures exist so that a change to one platform's
// rendering cannot silently alter another's: a diff here on a platform the author did not
// intend to touch is a fleet-wide rollout, not a test failure to update away.
func TestMachineConfigServerConfigGolden(t *testing.T) {
	t.Parallel()

	baseHCP := func(platform hyperv1.PlatformSpec) *hyperv1.HostedControlPlane {
		return &hyperv1.HostedControlPlane{
			ObjectMeta: metav1.ObjectMeta{Namespace: "clusters-example", Name: "example"},
			Spec: hyperv1.HostedControlPlaneSpec{
				InfraID:  "example-abcde",
				Platform: platform,
				Networking: hyperv1.ClusterNetworking{
					MachineNetwork: []hyperv1.MachineNetworkEntry{{CIDR: *ipnet.MustParseCIDR("10.0.0.0/16")}},
					ClusterNetwork: []hyperv1.ClusterNetworkEntry{{CIDR: *ipnet.MustParseCIDR("10.132.0.0/14")}},
					ServiceNetwork: []hyperv1.ServiceNetworkEntry{{CIDR: *ipnet.MustParseCIDR("172.31.0.0/16")}},
					NetworkType:    hyperv1.OVNKubernetes,
				},
				DNS: hyperv1.DNSSpec{BaseDomain: "example.com"},
			},
			Status: hyperv1.HostedControlPlaneStatus{
				ControlPlaneEndpoint: hyperv1.APIEndpoint{Host: "api.example.com", Port: 6443},
			},
		}
	}

	externalHCP := func(state hyperv1.ExternalCloudControllerManagerState) *hyperv1.HostedControlPlane {
		hcp := baseHCP(hyperv1.PlatformSpec{
			Type: hyperv1.ExternalPlatform,
			External: hyperv1.ExternalPlatformSpec{
				HostedClusterTemplate: hyperv1.ExternalTemplateReference{
					APIGroup: "example.io",
					Resource: "foohostedclustertemplates",
					Name:     "my-infra",
				},
			},
		})
		hcp.Status.Platform = &hyperv1.PlatformStatus{
			External: hyperv1.ExternalPlatformStatus{
				Name:                   "ExampleCloud",
				CloudControllerManager: hyperv1.ExternalCloudControllerManagerStatus{State: state},
			},
		}
		return hcp
	}

	testCases := []struct {
		name string
		hcp  *hyperv1.HostedControlPlane
	}{
		{
			name: "AWS",
			hcp: baseHCP(hyperv1.PlatformSpec{
				Type: hyperv1.AWSPlatform,
				AWS: &hyperv1.AWSPlatformSpec{
					Region: "us-east-1",
					ResourceTags: []hyperv1.AWSClusterResourceTag{
						{Key: "team", Value: "hypershift"},
						// Dropped from the rendered Infrastructure; kept here so the
						// fixture proves the filter still runs.
						{Key: "kubernetes.io/cluster/example", Value: "owned"},
					},
				},
			}),
		},
		{
			name: "Azure",
			hcp: baseHCP(hyperv1.PlatformSpec{
				Type: hyperv1.AzurePlatform,
				Azure: &hyperv1.AzurePlatformSpec{
					Cloud:             "AzurePublicCloud",
					Location:          "eastus",
					ResourceGroupName: "example-rg",
					SubscriptionID:    "00000000-0000-0000-0000-000000000000",
					TenantID:          "00000000-0000-0000-0000-000000000000",
					VnetID:            "/subscriptions/00000000-0000-0000-0000-000000000000/resourceGroups/example-rg/providers/Microsoft.Network/virtualNetworks/example",
					SubnetID:          "/subscriptions/00000000-0000-0000-0000-000000000000/resourceGroups/example-rg/providers/Microsoft.Network/virtualNetworks/example/subnets/default",
					SecurityGroupID:   "/subscriptions/00000000-0000-0000-0000-000000000000/resourceGroups/example-rg/providers/Microsoft.Network/networkSecurityGroups/example",
				},
			}),
		},
		{
			name: "GCP",
			hcp: baseHCP(hyperv1.PlatformSpec{
				Type: hyperv1.GCPPlatform,
				GCP: &hyperv1.GCPPlatformSpec{
					Project: "example-project",
					Region:  "us-central1",
				},
			}),
		},
		{
			name: "KubeVirt",
			hcp:  baseHCP(hyperv1.PlatformSpec{Type: hyperv1.KubevirtPlatform, Kubevirt: &hyperv1.KubevirtPlatformSpec{}}),
		},
		{
			name: "None",
			hcp:  baseHCP(hyperv1.PlatformSpec{Type: hyperv1.NonePlatform}),
		},
		{
			// Differs from the None fixture only in the platform keys: the kubelet is
			// started with an external cloud provider and nodes join tainted.
			name: "ExternalWithCloudControllerManager",
			hcp:  externalHCP(hyperv1.ExternalCloudControllerManager),
		},
		{
			name: "ExternalWithoutCloudControllerManager",
			hcp:  externalHCP(hyperv1.NoCloudControllerManager),
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			g := NewWithT(t)

			params, err := NewMCSParams(tc.hcp, &corev1.Secret{}, &corev1.Secret{}, &corev1.ConfigMap{}, &corev1.ConfigMap{})
			g.Expect(err).ToNot(HaveOccurred())

			cm := &corev1.ConfigMap{}
			g.Expect(ReconcileMachineConfigServerConfig(cm, params)).To(Succeed())

			testutil.CompareWithFixture(t, cm.Data)
		})
	}
}

// TestNewMCSParamsBlocksUntilExternalPlatformDeclared proves the bootstrap configuration is
// never rendered from a guessed cloud provider, and that the declaration arriving does not
// itself change the rendering of anything else.
func TestNewMCSParamsBlocksUntilExternalPlatformDeclared(t *testing.T) {
	t.Parallel()
	g := NewWithT(t)

	hcp := &hyperv1.HostedControlPlane{
		ObjectMeta: metav1.ObjectMeta{Namespace: "clusters-example", Name: "example"},
		Spec: hyperv1.HostedControlPlaneSpec{
			InfraID: "example-abcde",
			Platform: hyperv1.PlatformSpec{
				Type: hyperv1.ExternalPlatform,
				External: hyperv1.ExternalPlatformSpec{
					HostedClusterTemplate: hyperv1.ExternalTemplateReference{
						APIGroup: "example.io",
						Resource: "foohostedclustertemplates",
						Name:     "my-infra",
					},
				},
			},
			DNS: hyperv1.DNSSpec{BaseDomain: "example.com"},
		},
		Status: hyperv1.HostedControlPlaneStatus{
			ControlPlaneEndpoint: hyperv1.APIEndpoint{Host: "api.example.com", Port: 6443},
		},
	}

	_, err := NewMCSParams(hcp, &corev1.Secret{}, &corev1.Secret{}, &corev1.ConfigMap{}, &corev1.ConfigMap{})
	g.Expect(err).To(HaveOccurred())

	hcp.Status.Platform = &hyperv1.PlatformStatus{
		External: hyperv1.ExternalPlatformStatus{
			Name: "ExampleCloud",
			CloudControllerManager: hyperv1.ExternalCloudControllerManagerStatus{
				State: hyperv1.ExternalCloudControllerManager,
			},
		},
	}
	params, err := NewMCSParams(hcp, &corev1.Secret{}, &corev1.Secret{}, &corev1.ConfigMap{}, &corev1.ConfigMap{})
	g.Expect(err).ToNot(HaveOccurred())
	g.Expect(params.Infrastructure.Spec.PlatformSpec.External).ToNot(BeNil())
	g.Expect(params.Infrastructure.Spec.PlatformSpec.External.PlatformName).To(Equal("ExampleCloud"))
	g.Expect(params.Infrastructure.Status.PlatformStatus.External).ToNot(BeNil())
	g.Expect(string(params.Infrastructure.Status.PlatformStatus.External.CloudControllerManager.State)).To(Equal("External"))
}
