package globalconfig

import (
	"fmt"
	"net"
	"strconv"
	"strings"

	hyperv1 "github.com/openshift/hypershift/api/hypershift/v1beta1"
	"github.com/openshift/hypershift/control-plane-operator/controllers/hostedcontrolplane/cloud/openstack"
	"github.com/openshift/hypershift/support/netutil"

	configv1 "github.com/openshift/api/config/v1"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func httpsURL(host string, port int32) string {
	return fmt.Sprintf("https://%s", net.JoinHostPort(host, strconv.Itoa(int(port))))
}

func InfrastructureConfig() *configv1.Infrastructure {
	infra := &configv1.Infrastructure{
		ObjectMeta: metav1.ObjectMeta{
			Name: "cluster",
		},
	}
	return infra
}

func ReconcileInfrastructure(infra *configv1.Infrastructure, hcp *hyperv1.HostedControlPlane) {

	platformType := hcp.Spec.Platform.Type

	apiServerAddress := hcp.Status.ControlPlaneEndpoint.Host
	apiServerPort := hcp.Status.ControlPlaneEndpoint.Port

	infra.Spec.PlatformSpec.Type = configv1.PlatformType(platformType)
	infra.Status.APIServerInternalURL = httpsURL(apiServerAddress, apiServerPort)
	if netutil.IsPrivateHCP(hcp) {
		infra.Status.APIServerInternalURL = httpsURL(fmt.Sprintf("api.%s.hypershift.local", hcp.Name), apiServerPort)
	}

	infra.Status.APIServerURL = httpsURL(apiServerAddress, apiServerPort)
	if len(hcp.Spec.KubeAPIServerDNSName) > 0 {
		infra.Status.APIServerURL = httpsURL(hcp.Spec.KubeAPIServerDNSName, apiServerPort)
	}
	infra.Status.EtcdDiscoveryDomain = BaseDomain(hcp)
	infra.Status.InfrastructureName = hcp.Spec.InfraID
	infra.Status.ControlPlaneTopology = configv1.ExternalTopologyMode
	infra.Status.Platform = configv1.PlatformType(platformType)
	if infra.Status.PlatformStatus == nil {
		infra.Status.PlatformStatus = &configv1.PlatformStatus{}
	}
	infra.Status.PlatformStatus.Type = configv1.PlatformType(platformType)

	switch hcp.Spec.InfrastructureAvailabilityPolicy {
	case hyperv1.HighlyAvailable:
		infra.Status.InfrastructureTopology = configv1.HighlyAvailableTopologyMode
	default:
		infra.Status.InfrastructureTopology = configv1.SingleReplicaTopologyMode
	}

	switch platformType {
	case hyperv1.AWSPlatform:
		if infra.Spec.PlatformSpec.AWS == nil {
			infra.Spec.PlatformSpec.AWS = &configv1.AWSPlatformSpec{}
		}
		if infra.Status.PlatformStatus.AWS == nil {
			infra.Status.PlatformStatus.AWS = &configv1.AWSPlatformStatus{}
		}
		infra.Status.PlatformStatus.AWS.Region = hcp.Spec.Platform.AWS.Region
		tags := []configv1.AWSResourceTag{}
		for _, tag := range hcp.Spec.Platform.AWS.ResourceTags {
			// This breaks the AWS CSI driver as it ends up being used there as an extra tag
			// which makes it fail to start with "Invalid extra tags: Tag key prefix 'kubernetes.io' is reserved".
			if strings.HasPrefix(tag.Key, "kubernetes.io") {
				continue
			}
			tags = append(tags, configv1.AWSResourceTag{
				Key:   tag.Key,
				Value: tag.Value,
			})
		}
		infra.Status.PlatformStatus.AWS.ResourceTags = tags
	case hyperv1.AzurePlatform:
		infra.Spec.CloudConfig.Name = "cloud.conf"
		if infra.Status.PlatformStatus.Azure == nil {
			infra.Status.PlatformStatus.Azure = &configv1.AzurePlatformStatus{}
		}
		cloudName := configv1.AzureCloudEnvironment(hcp.Spec.Platform.Azure.Cloud)
		if cloudName == "" {
			cloudName = configv1.AzurePublicCloud
		}
		infra.Status.PlatformStatus.Azure.CloudName = cloudName
		infra.Status.PlatformStatus.Azure.ResourceGroupName = hcp.Spec.Platform.Azure.ResourceGroupName
	case hyperv1.PowerVSPlatform:
		infra.Status.PlatformStatus.PowerVS = &configv1.PowerVSPlatformStatus{
			Region:         hcp.Spec.Platform.PowerVS.Region,
			Zone:           hcp.Spec.Platform.PowerVS.Zone,
			CISInstanceCRN: hcp.Spec.Platform.PowerVS.CISInstanceCRN,
			ResourceGroup:  hcp.Spec.Platform.PowerVS.ResourceGroup,
		}
	case hyperv1.OpenStackPlatform:
		infra.Spec.PlatformSpec.OpenStack = &configv1.OpenStackPlatformSpec{}
		// This ConfigMap is populated by the local ignition provider and given to MCO
		infra.Spec.CloudConfig.Name = "cloud-provider-config"
		infra.Spec.CloudConfig.Key = openstack.CloudConfigKey
		infra.Status.PlatformStatus.OpenStack = &configv1.OpenStackPlatformStatus{
			CloudName:            "openstack",
			LoadBalancer:         &configv1.OpenStackPlatformLoadBalancer{Type: configv1.LoadBalancerTypeUserManaged},
			APIServerInternalIPs: []string{},
			IngressIPs:           []string{},
		}
	case hyperv1.GCPPlatform:
		if infra.Status.PlatformStatus.GCP == nil {
			infra.Status.PlatformStatus.GCP = &configv1.GCPPlatformStatus{}
		}
		infra.Status.PlatformStatus.GCP.ProjectID = hcp.Spec.Platform.GCP.Project
		infra.Status.PlatformStatus.GCP.Region = hcp.Spec.Platform.GCP.Region
		var labels []configv1.GCPResourceLabel
		for _, label := range hcp.Spec.Platform.GCP.ResourceLabels {
			// Skip labels with reserved prefixes to avoid breaking downstream components.
			if strings.HasPrefix(label.Key, "kubernetes-io") {
				continue
			}
			value := ""
			if label.Value != nil {
				value = *label.Value
			}
			labels = append(labels, configv1.GCPResourceLabel{
				Key:   label.Key,
				Value: value,
			})
		}
		infra.Status.PlatformStatus.GCP.ResourceLabels = labels
	case hyperv1.ExternalPlatform:
		// Both values come from the declaration the integrator published and HyperShift
		// recorded, not from the spec: a platform's name and whether it runs a cloud
		// controller manager are properties of the integration rather than of a cluster.
		//
		// This is the only place the cloud provider the kubelet runs with is decided.
		// cloudControllerManager.state of External makes the machine config operator start
		// kubelets with --cloud-provider=external, so nodes join tainted uninitialized and
		// stay unschedulable until the integrator's cloud controller manager removes the
		// taint. Callers on the bootstrap path must therefore refuse to render before the
		// declaration has been recorded, rather than render a guess and correct it later.
		declaration := externalPlatformDeclaration(hcp)
		infra.Spec.PlatformSpec.External = &configv1.ExternalPlatformSpec{
			PlatformName: declaration.Name,
		}
		infra.Status.PlatformStatus.External = &configv1.ExternalPlatformStatus{
			CloudControllerManager: configv1.CloudControllerManagerStatus{
				State: configv1.CloudControllerManagerState(declaration.CloudControllerManager.State),
			},
		}
	}
}

// externalPlatformDeclaration returns the declaration recorded on the HostedControlPlane,
// or the zero value if none has been. Callers that must not act on the zero value check
// HasExternalPlatformDeclaration first.
func externalPlatformDeclaration(hcp *hyperv1.HostedControlPlane) hyperv1.ExternalPlatformStatus {
	if hcp.Status.Platform == nil {
		return hyperv1.ExternalPlatformStatus{}
	}
	return hcp.Status.Platform.External
}

// HasExternalPlatformDeclaration reports whether the integrator's platform declaration has
// been recorded on the HostedControlPlane.
//
// Anything that renders guest configuration destined for a node must gate on this. The
// declaration decides the kubelet's cloud provider, which is baked into the bootstrap
// configuration and covered by the NodePool configuration hash, so rendering a default and
// correcting it once the declaration arrives would boot nodes against the wrong cloud
// provider and then roll every one of them.
func HasExternalPlatformDeclaration(hcp *hyperv1.HostedControlPlane) bool {
	return externalPlatformDeclaration(hcp) != hyperv1.ExternalPlatformStatus{}
}
