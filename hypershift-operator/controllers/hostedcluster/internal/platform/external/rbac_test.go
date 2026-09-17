package external

import (
	"slices"
	"testing"

	"github.com/openshift/hypershift/support/externalplatform"
	"github.com/openshift/hypershift/support/k8sutil"
	"github.com/openshift/hypershift/support/upsert"

	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"

	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/google/go-cmp/cmp"
)

func testProvider() externalplatform.Provider {
	return externalplatform.Provider{
		APIGroup:       testAPIGroup,
		ServiceAccount: types.NamespacedName{Namespace: "example-system", Name: "example-provider"},
	}
}

func newRBACFakeClient(t *testing.T) client.Client {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatalf("failed to build the scheme: %v", err)
	}
	return fake.NewClientBuilder().WithScheme(scheme).Build()
}

func TestReconcileProviderRBAC(t *testing.T) {
	t.Run("It should bind the registered provider's service account into the control plane namespace", func(t *testing.T) {
		c := newRBACFakeClient(t)
		if err := ReconcileProviderRBAC(t.Context(), c, upsert.New(false).CreateOrUpdate, testHostedCluster(), "clusters-example", testProvider()); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		roleBinding := &rbacv1.RoleBinding{}
		if err := c.Get(t.Context(), client.ObjectKey{Namespace: "clusters-example", Name: ProviderRBACName}, roleBinding); err != nil {
			t.Fatalf("failed to get the RoleBinding: %v", err)
		}

		expectedSubjects := []rbacv1.Subject{{
			Kind:      "ServiceAccount",
			Name:      "example-provider",
			Namespace: "example-system",
		}}
		if diff := cmp.Diff(expectedSubjects, roleBinding.Subjects); diff != "" {
			t.Errorf("unexpected subjects: %s", diff)
		}

		expectedRoleRef := rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "Role", Name: ProviderRBACName}
		if diff := cmp.Diff(expectedRoleRef, roleBinding.RoleRef); diff != "" {
			t.Errorf("unexpected roleRef: %s", diff)
		}

		// A namespaced Role rather than a ClusterRole is the whole isolation story: it is
		// what stops one integrator reading another integrator's control plane namespaces.
		if diff := cmp.Diff("clusters-example", roleBinding.Namespace); diff != "" {
			t.Errorf("unexpected namespace: %s", diff)
		}

		if got := roleBinding.Annotations[k8sutil.HostedClusterAnnotation]; got != "clusters/example" {
			t.Errorf("expected the RoleBinding to be annotated with its HostedCluster, got %q", got)
		}
	})

	t.Run("It should grant the registered provider's own API group and nothing else's", func(t *testing.T) {
		c := newRBACFakeClient(t)
		if err := ReconcileProviderRBAC(t.Context(), c, upsert.New(false).CreateOrUpdate, testHostedCluster(), "clusters-example", testProvider()); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		role := &rbacv1.Role{}
		if err := c.Get(t.Context(), client.ObjectKey{Namespace: "clusters-example", Name: ProviderRBACName}, role); err != nil {
			t.Fatalf("failed to get the Role: %v", err)
		}

		var granted []string
		for _, rule := range role.Rules {
			granted = append(granted, rule.APIGroups...)
		}
		if !slices.Contains(granted, testAPIGroup) {
			t.Errorf("expected the provider's own API group %q to be granted, got %v", testAPIGroup, granted)
		}
		if slices.Contains(granted, "other.io") {
			t.Errorf("expected no other integrator's API group to be granted, got %v", granted)
		}
	})

	t.Run("It should not grant blanket access to secrets", func(t *testing.T) {
		c := newRBACFakeClient(t)
		if err := ReconcileProviderRBAC(t.Context(), c, upsert.New(false).CreateOrUpdate, testHostedCluster(), "clusters-example", testProvider()); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		role := &rbacv1.Role{}
		if err := c.Get(t.Context(), client.ObjectKey{Namespace: "clusters-example", Name: ProviderRBACName}, role); err != nil {
			t.Fatalf("failed to get the Role: %v", err)
		}

		// The etcd encryption key and the Kubernetes API server signing keys live in this
		// same namespace, so every secrets rule has to be named. resourceNames cannot
		// restrict list or watch, which is why the rule is get only.
		for _, rule := range role.Rules {
			if !slices.Contains(rule.Resources, "secrets") {
				continue
			}
			if len(rule.ResourceNames) == 0 {
				t.Errorf("expected the secrets rule to name the secrets it grants, got %+v", rule)
			}
			if diff := cmp.Diff([]string{"get"}, rule.Verbs); diff != "" {
				t.Errorf("unexpected verbs on the secrets rule: %s", diff)
			}
		}
	})

	t.Run("It should not grant write access to the hosted control plane", func(t *testing.T) {
		c := newRBACFakeClient(t)
		if err := ReconcileProviderRBAC(t.Context(), c, upsert.New(false).CreateOrUpdate, testHostedCluster(), "clusters-example", testProvider()); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		role := &rbacv1.Role{}
		if err := c.Get(t.Context(), client.ObjectKey{Namespace: "clusters-example", Name: ProviderRBACName}, role); err != nil {
			t.Fatalf("failed to get the Role: %v", err)
		}

		// The HostedControlPlane's status is HyperShift's to write. The integrator reports
		// through its own hosted cluster object and through ControlPlaneComponent.
		for _, rule := range role.Rules {
			if !slices.Contains(rule.Resources, "hostedcontrolplanes") {
				continue
			}
			if diff := cmp.Diff([]string{"get", "list", "watch"}, rule.Verbs); diff != "" {
				t.Errorf("unexpected verbs on the hostedcontrolplanes rule: %s", diff)
			}
		}
	})

	t.Run("It should be idempotent across repeated reconciles", func(t *testing.T) {
		c := newRBACFakeClient(t)
		for range 2 {
			if err := ReconcileProviderRBAC(t.Context(), c, upsert.New(false).CreateOrUpdate, testHostedCluster(), "clusters-example", testProvider()); err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
		}

		roles := &rbacv1.RoleList{}
		if err := c.List(t.Context(), roles, client.InNamespace("clusters-example")); err != nil {
			t.Fatalf("failed to list Roles: %v", err)
		}
		if len(roles.Items) != 1 {
			t.Errorf("expected exactly one Role, got %d", len(roles.Items))
		}
	})
}
