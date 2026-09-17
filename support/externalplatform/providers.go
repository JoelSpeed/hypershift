package externalplatform

import (
	"fmt"
	"sort"
	"strings"

	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation"
)

// ProviderFlagFormat documents the value the install flag and the operator flag both take.
const ProviderFlagFormat = "<apiGroup>=<namespace>/<serviceAccountName>"

// Provider is an integrator that a cluster administrator has registered with the HyperShift
// Operator at install time.
//
// Registration exists because installing a partner's custom resource definitions is not by
// itself consent to let that partner into other tenants' control plane namespaces. Only a
// registered integrator's ServiceAccount is granted access, and only in the namespaces of
// HostedClusters that named its API group, so one partner cannot reach another's clusters.
type Provider struct {
	// APIGroup is the group the integrator's hosted cluster template belongs to, and is
	// what a HostedCluster names in spec.platform.external.hostedClusterTemplate.apiGroup.
	// The group rather than the platform name because the platform name is not known until
	// the integrator declares it, long after the access decision has to be made.
	APIGroup string

	// ServiceAccount is the identity the integrator's controller runs as. It lives in the
	// integrator's own namespace, not in any control plane namespace.
	ServiceAccount types.NamespacedName
}

// Providers is the set of registered integrators, keyed by API group.
type Providers map[string]Provider

// ParseProviders parses the repeated flag values into a registry, rejecting duplicates so
// that a typo produces an install failure rather than a silently ignored registration.
func ParseProviders(values []string) (Providers, error) {
	providers := Providers{}
	for _, value := range values {
		provider, err := ParseProvider(value)
		if err != nil {
			return nil, err
		}
		if existing, ok := providers[provider.APIGroup]; ok {
			return nil, fmt.Errorf("API group %q is registered twice, to %s and to %s", provider.APIGroup, existing.ServiceAccount, provider.ServiceAccount)
		}
		providers[provider.APIGroup] = provider
	}
	return providers, nil
}

// ParseProvider parses a single "<apiGroup>=<namespace>/<serviceAccountName>" value.
func ParseProvider(value string) (Provider, error) {
	malformed := func() (Provider, error) {
		return Provider{}, fmt.Errorf("external platform provider %q must be of the form %s", value, ProviderFlagFormat)
	}

	apiGroup, serviceAccount, found := strings.Cut(value, "=")
	if !found {
		return malformed()
	}
	namespace, name, found := strings.Cut(serviceAccount, "/")
	if !found {
		return malformed()
	}
	if apiGroup == "" || namespace == "" || name == "" {
		return malformed()
	}
	for _, part := range []struct{ field, value string }{
		{"API group", apiGroup},
		{"namespace", namespace},
		{"service account name", name},
	} {
		// The API server would reject these too, but only once the operator tried to mint
		// the RoleBinding, which is a long way from the administrator who mistyped them.
		if errs := validation.IsDNS1123Subdomain(part.value); len(errs) > 0 {
			return Provider{}, fmt.Errorf("external platform provider %q has an invalid %s %q: %s", value, part.field, part.value, strings.Join(errs, ", "))
		}
	}

	return Provider{
		APIGroup:       apiGroup,
		ServiceAccount: types.NamespacedName{Namespace: namespace, Name: name},
	}, nil
}

// APIGroups returns the registered API groups in a stable order, so that the generated RBAC
// does not churn between renders.
func (p Providers) APIGroups() []string {
	groups := make([]string, 0, len(p))
	for group := range p {
		groups = append(groups, group)
	}
	sort.Strings(groups)
	return groups
}
