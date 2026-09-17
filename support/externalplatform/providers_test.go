package externalplatform

import (
	"testing"

	. "github.com/onsi/gomega"

	"k8s.io/apimachinery/pkg/types"
)

func TestParseProvider(t *testing.T) {
	testCases := []struct {
		name        string
		value       string
		expected    Provider
		expectError bool
	}{
		{
			name:  "When the registration is well formed, it should parse into a provider",
			value: "example.io=example-system/example-provider",
			expected: Provider{
				APIGroup:       "example.io",
				ServiceAccount: types.NamespacedName{Namespace: "example-system", Name: "example-provider"},
			},
		},
		{
			name:  "When the API group has several labels, it should keep the whole group",
			value: "infrastructure.cluster.x-k8s.io=capi-system/capa-controller-manager",
			expected: Provider{
				APIGroup:       "infrastructure.cluster.x-k8s.io",
				ServiceAccount: types.NamespacedName{Namespace: "capi-system", Name: "capa-controller-manager"},
			},
		},
		{
			name:        "When there is no separator between the group and the service account, it should be rejected",
			value:       "example.io",
			expectError: true,
		},
		{
			name:        "When the service account has no namespace, it should be rejected",
			value:       "example.io=example-provider",
			expectError: true,
		},
		{
			name:        "When the API group is empty, it should be rejected",
			value:       "=example-system/example-provider",
			expectError: true,
		},
		{
			name:        "When the namespace is empty, it should be rejected",
			value:       "example.io=/example-provider",
			expectError: true,
		},
		{
			name:        "When the service account name is empty, it should be rejected",
			value:       "example.io=example-system/",
			expectError: true,
		},
		{
			// Caught here rather than by the API server when the RoleBinding is minted,
			// which would be a long way from the administrator who mistyped it.
			name:        "When the API group is not a DNS subdomain, it should be rejected",
			value:       "Example.IO=example-system/example-provider",
			expectError: true,
		},
		{
			name:        "When the namespace is not a DNS subdomain, it should be rejected",
			value:       "example.io=Example System/example-provider",
			expectError: true,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			g := NewWithT(t)

			provider, err := ParseProvider(tc.value)
			if tc.expectError {
				g.Expect(err).To(HaveOccurred())
				return
			}
			g.Expect(err).ToNot(HaveOccurred())
			g.Expect(provider).To(Equal(tc.expected))
		})
	}
}

func TestParseProviders(t *testing.T) {
	g := NewWithT(t)

	providers, err := ParseProviders([]string{
		"zebra.io=zebra-system/zebra-provider",
		"example.io=example-system/example-provider",
	})
	g.Expect(err).ToNot(HaveOccurred())
	g.Expect(providers).To(HaveLen(2))
	g.Expect(providers["example.io"].ServiceAccount.Name).To(Equal("example-provider"))

	// Sorted regardless of the order the flags were given in, so that the generated
	// ClusterRole and the operator's arguments do not churn between renders.
	g.Expect(providers.APIGroups()).To(Equal([]string{"example.io", "zebra.io"}))

	g.Expect(ParseProviders(nil)).To(BeEmpty())
}

func TestParseProvidersRejectsADuplicateAPIGroup(t *testing.T) {
	g := NewWithT(t)

	// Silently keeping the last one would leave an administrator believing they had
	// registered two integrators when only one is admitted.
	_, err := ParseProviders([]string{
		"example.io=first-system/first-provider",
		"example.io=second-system/second-provider",
	})
	g.Expect(err).To(MatchError(ContainSubstring("registered twice")))
}
