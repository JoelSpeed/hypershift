package globalconfig

import (
	"bytes"
	"fmt"
	"text/template"

	hyperv1 "github.com/openshift/hypershift/api/hypershift/v1beta1"
	"github.com/openshift/hypershift/support/netutil"

	configv1 "github.com/openshift/api/config/v1"
)

// Abbreviated version of the installer's InstallConfig type
// Bare minimum required to support MCS
type InstallConfig struct {
	MachineCIDRs []string
	Platform     string
	Region       string
	ProjectID    string

	// PlatformName and CloudControllerManager are the External platform's, and mirror the
	// declaration recorded on the HostedControlPlane.
	PlatformName           string
	CloudControllerManager string
}

func NewInstallConfig(hcp *hyperv1.HostedControlPlane) *InstallConfig {
	cfg := &InstallConfig{
		MachineCIDRs: netutil.MachineCIDRs(hcp.Spec.Networking.MachineNetwork),
		Platform:     string(hcp.Spec.Platform.Type),
	}
	switch hcp.Spec.Platform.Type {
	case hyperv1.AWSPlatform:
		cfg.Region = hcp.Spec.Platform.AWS.Region
	case hyperv1.GCPPlatform:
		cfg.Region = hcp.Spec.Platform.GCP.Region
		cfg.ProjectID = hcp.Spec.Platform.GCP.Project
	case hyperv1.ExternalPlatform:
		declaration := externalPlatformDeclaration(hcp)
		cfg.PlatformName = declaration.Name
		// The installer spells "no cloud controller manager" as the empty string rather
		// than as None, and this document is read by installer-derived code.
		if declaration.CloudControllerManager.State == hyperv1.ExternalCloudControllerManager {
			cfg.CloudControllerManager = string(configv1.CloudControllerManagerExternal)
		}
	}
	return cfg
}

// TODO (csrwng): replace with installconfig type if importing the type from the installer
// becomes a viable option. Currently it requires vendoring a lot of unrelated
// libraries.
const installConfigTemplateString = `apiVersion: v1
controlPlane:
  replicas: 1
networking:
  machineNetwork:
{{- range .MachineCIDRs }}
  - cidr: {{ . }}
{{- end }}
platform:
{{- if eq .Platform "AWS" }}
  aws:
    region: {{ .Region }}
{{- else if eq .Platform "GCP" }}
  gcp:
    projectID: {{ .ProjectID }}
    region: {{ .Region }}
{{- else if eq .Platform "External" }}
  external:
    platformName: {{ .PlatformName }}
{{- if .CloudControllerManager }}
    cloudControllerManager: {{ .CloudControllerManager }}
{{- end }}
{{- else }}
  none: {}
{{- end }}
`

var installConfigTemplate = template.Must(template.New("install-config").Parse(installConfigTemplateString))

func (c *InstallConfig) String() string {
	out := &bytes.Buffer{}
	if err := installConfigTemplate.Execute(out, c); err != nil {
		panic(fmt.Sprintf("unexpected error executing install-config template: %v", err))
	}
	return out.String()
}
