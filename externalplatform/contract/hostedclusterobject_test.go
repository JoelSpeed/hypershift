package contract

import (
	"testing"

	hyperv1 "github.com/openshift/hypershift/api/hypershift/v1beta1"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func hostedClusterObject() *unstructured.Unstructured {
	object := &unstructured.Unstructured{}
	object.SetAPIVersion("example.io/v1alpha1")
	object.SetKind("FooHostedCluster")
	object.SetNamespace("clusters-example")
	object.SetName("example")
	return object
}

func TestControlPlaneEndpoint(t *testing.T) {
	object := hostedClusterObject()
	if err := unstructured.SetNestedMap(object.Object, map[string]any{
		"host": "api.example.hypershift.local",
		"port": int64(6443),
	}, "spec", "controlPlaneEndpoint"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	endpoint, err := ControlPlaneEndpoint(object)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if expected := (hyperv1.APIEndpoint{Host: "api.example.hypershift.local", Port: 6443}); endpoint != expected {
		t.Errorf("expected endpoint %v, got %v", expected, endpoint)
	}
}

func TestControlPlaneEndpointIsZeroBeforeHyperShiftReportsOne(t *testing.T) {
	endpoint, err := ControlPlaneEndpoint(hostedClusterObject())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if (endpoint != hyperv1.APIEndpoint{}) {
		t.Errorf("expected a zero endpoint, got %v", endpoint)
	}
}

func TestSetPlatform(t *testing.T) {
	for _, testCase := range []struct {
		name         string
		platformName string
		state        hyperv1.ExternalCloudControllerManagerState
		expectError  bool
	}{
		{
			name:         "accepts an external cloud controller manager",
			platformName: "ExampleCloud",
			state:        hyperv1.ExternalCloudControllerManager,
		},
		{
			name:         "accepts no cloud controller manager",
			platformName: "ExampleCloud",
			state:        hyperv1.NoCloudControllerManager,
		},
		{
			name:         "rejects an empty platform name",
			platformName: "",
			state:        hyperv1.ExternalCloudControllerManager,
			expectError:  true,
		},
		{
			name:         "rejects an unset state, which would otherwise be recorded as a guess",
			platformName: "ExampleCloud",
			state:        "",
			expectError:  true,
		},
		{
			name:         "rejects a misspelled state",
			platformName: "ExampleCloud",
			state:        "external",
			expectError:  true,
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			object := hostedClusterObject()
			err := SetPlatform(object, testCase.platformName, testCase.state)
			if testCase.expectError {
				if err == nil {
					t.Fatal("expected an error, got none")
				}
				if _, found, _ := unstructured.NestedMap(object.Object, "status", "platform"); found {
					t.Error("expected nothing to be written when the declaration is rejected")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}

			platform, err := Platform(object)
			if err != nil {
				t.Fatalf("unexpected error reading the declaration back: %v", err)
			}
			if platform == nil {
				t.Fatal("expected a declaration, got none")
			}
			if platform.Name != testCase.platformName {
				t.Errorf("expected platform name %q, got %q", testCase.platformName, platform.Name)
			}
			if platform.CloudControllerManager.State != testCase.state {
				t.Errorf("expected state %q, got %q", testCase.state, platform.CloudControllerManager.State)
			}
		})
	}
}

func TestPlatformIsNilBeforeTheIntegratorDeclares(t *testing.T) {
	platform, err := Platform(hostedClusterObject())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if platform != nil {
		t.Errorf("expected no declaration, got %v", platform)
	}
}

func TestPlatformRejectsAHalfWrittenDeclaration(t *testing.T) {
	for _, testCase := range []struct {
		name     string
		platform map[string]any
	}{
		{
			name:     "a name with no cloud controller manager state",
			platform: map[string]any{"name": "ExampleCloud"},
		},
		{
			name: "a cloud controller manager state with no name",
			platform: map[string]any{
				"cloudControllerManager": map[string]any{"state": "External"},
			},
		},
		{
			name: "an unrecognised cloud controller manager state",
			platform: map[string]any{
				"name":                   "ExampleCloud",
				"cloudControllerManager": map[string]any{"state": "Internal"},
			},
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			object := hostedClusterObject()
			if err := unstructured.SetNestedMap(object.Object, testCase.platform, "status", "platform"); err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if _, err := Platform(object); err == nil {
				t.Error("expected an error, got none")
			}
		})
	}
}

func TestSetInfrastructure(t *testing.T) {
	object := hostedClusterObject()
	if err := SetInfrastructure(object, "infrastructure.cluster.x-k8s.io", "FooCluster", "example"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	apiGroup, kind, name, err := Infrastructure(object)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if apiGroup != "infrastructure.cluster.x-k8s.io" || kind != "FooCluster" || name != "example" {
		t.Errorf("expected infrastructure.cluster.x-k8s.io/FooCluster/example, got %s/%s/%s", apiGroup, kind, name)
	}
}

func TestSetInfrastructureRejectsAPartialReference(t *testing.T) {
	// HyperShift points the Cluster API Cluster's immutable spec.infrastructureRef at what is
	// named here, so a reference that is missing a part has to fail before it is written
	// rather than resolve to something wrong once.
	for _, testCase := range []struct{ name, apiGroup, kind, objectName string }{
		{name: "no api group", kind: "FooCluster", objectName: "example"},
		{name: "no kind", apiGroup: "infrastructure.cluster.x-k8s.io", objectName: "example"},
		{name: "no name", apiGroup: "infrastructure.cluster.x-k8s.io", kind: "FooCluster"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			if err := SetInfrastructure(hostedClusterObject(), testCase.apiGroup, testCase.kind, testCase.objectName); err == nil {
				t.Error("expected an error, got none")
			}
		})
	}
}

func TestInfrastructureIsEmptyBeforeTheIntegratorReportsOne(t *testing.T) {
	apiGroup, kind, name, err := Infrastructure(hostedClusterObject())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if apiGroup != "" || kind != "" || name != "" {
		t.Errorf("expected empty values, got %s/%s/%s", apiGroup, kind, name)
	}
}

func TestSetReady(t *testing.T) {
	object := hostedClusterObject()
	if err := SetReady(object, metav1.ConditionFalse, "Provisioning", "Waiting for the load balancer"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	condition, err := Ready(object)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if condition == nil {
		t.Fatal("expected a condition, got none")
	}
	if condition.Status != metav1.ConditionFalse {
		t.Errorf("expected status False, got %q", condition.Status)
	}
	if condition.Reason != "Provisioning" {
		t.Errorf("expected reason Provisioning, got %q", condition.Reason)
	}
	if condition.Message != "Waiting for the load balancer" {
		t.Errorf("expected the message to be preserved verbatim, got %q", condition.Message)
	}
}

func TestSetReadyDoesNotDuplicateTheCondition(t *testing.T) {
	object := hostedClusterObject()
	for i := range 3 {
		if err := SetReady(object, metav1.ConditionFalse, "Provisioning", "still going"); err != nil {
			t.Fatalf("unexpected error on reconcile %d: %v", i, err)
		}
	}

	conditions, _, err := unstructured.NestedSlice(object.Object, "status", "conditions")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(conditions) != 1 {
		t.Errorf("expected exactly one condition, got %d", len(conditions))
	}
}

func TestSetReadyHoldsLastTransitionTimeWhileTheStatusIsUnchanged(t *testing.T) {
	// HyperShift reports how long it has been waiting on the provider from this timestamp, so
	// a provider that reconciles every thirty seconds must not keep resetting the clock.
	object := hostedClusterObject()
	if err := SetReady(object, metav1.ConditionFalse, "Provisioning", "first"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	first := lastTransitionTime(t, object)

	if err := SetReady(object, metav1.ConditionFalse, "StillProvisioning", "second"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if held := lastTransitionTime(t, object); held != first {
		t.Errorf("expected lastTransitionTime to be held at %q, got %q", first, held)
	}

	// Backdated rather than waited on, so the assertion does not depend on two calls landing
	// in different microseconds.
	backdate(t, object, "2020-01-01T00:00:00.000000Z")
	if err := SetReady(object, metav1.ConditionTrue, "AsExpected", "done"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if moved := lastTransitionTime(t, object); moved == "2020-01-01T00:00:00.000000Z" {
		t.Error("expected lastTransitionTime to move when the status changed")
	}
}

func TestSetReadyLeavesOtherConditionsAlone(t *testing.T) {
	object := hostedClusterObject()
	if err := unstructured.SetNestedSlice(object.Object, []any{
		map[string]any{"type": "ProviderSpecific", "status": "True", "reason": "AsExpected", "message": "mine"},
	}, "status", "conditions"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if err := SetReady(object, metav1.ConditionTrue, "AsExpected", "done"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	conditions, _, err := unstructured.NestedSlice(object.Object, "status", "conditions")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(conditions) != 2 {
		t.Fatalf("expected two conditions, got %d", len(conditions))
	}
	if conditionType, _, _ := unstructured.NestedString(conditions[0].(map[string]any), "type"); conditionType != "ProviderSpecific" {
		t.Errorf("expected the integrator's own condition to survive, got %q", conditionType)
	}
}

func TestReadyIsNilBeforeTheIntegratorSetsOne(t *testing.T) {
	condition, err := Ready(hostedClusterObject())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if condition != nil {
		t.Errorf("expected no condition, got %v", condition)
	}
}

func backdate(t *testing.T, object *unstructured.Unstructured, when string) {
	t.Helper()
	conditions, _, err := unstructured.NestedSlice(object.Object, "status", "conditions")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	for _, item := range conditions {
		entry := item.(map[string]any)
		if conditionType, _, _ := unstructured.NestedString(entry, "type"); conditionType != ReadyConditionType {
			continue
		}
		entry["lastTransitionTime"] = when
	}
	if err := unstructured.SetNestedSlice(object.Object, conditions, "status", "conditions"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func lastTransitionTime(t *testing.T, object *unstructured.Unstructured) string {
	t.Helper()
	conditions, _, err := unstructured.NestedSlice(object.Object, "status", "conditions")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	for _, item := range conditions {
		entry := item.(map[string]any)
		if conditionType, _, _ := unstructured.NestedString(entry, "type"); conditionType != ReadyConditionType {
			continue
		}
		value, _, _ := unstructured.NestedString(entry, "lastTransitionTime")
		return value
	}
	t.Fatal("no Ready condition found")
	return ""
}

func TestAnExplicitlyNullStatusIsTreatedAsAnEmptyOne(t *testing.T) {
	// An object whose status subresource has never been written can come back carrying a
	// literal null, and the nested accessors report that as a type error rather than as an
	// absent value. Every accessor here has to survive the very first reconcile.
	object := hostedClusterObject()
	object.Object["status"] = nil

	if err := SetPlatform(object, "ExampleCloud", hyperv1.ExternalCloudControllerManager); err != nil {
		t.Fatalf("unexpected error from SetPlatform: %v", err)
	}
	if err := SetInfrastructure(object, "infrastructure.cluster.x-k8s.io", "FooCluster", "example"); err != nil {
		t.Fatalf("unexpected error from SetInfrastructure: %v", err)
	}
	if err := SetReady(object, metav1.ConditionTrue, "AsExpected", "Everything is up"); err != nil {
		t.Fatalf("unexpected error from SetReady: %v", err)
	}

	platform, err := Platform(object)
	if err != nil {
		t.Fatalf("unexpected error from Platform: %v", err)
	}
	if platform == nil || platform.Name != "ExampleCloud" {
		t.Errorf("expected the declaration to be readable back, got %v", platform)
	}
	condition, err := Ready(object)
	if err != nil {
		t.Fatalf("unexpected error from Ready: %v", err)
	}
	if condition == nil || condition.Status != metav1.ConditionTrue {
		t.Errorf("expected Ready=True, got %v", condition)
	}
}

func TestReadersTolerateANullStatus(t *testing.T) {
	object := hostedClusterObject()
	object.Object["status"] = nil

	platform, err := Platform(object)
	if err != nil {
		t.Fatalf("unexpected error from Platform: %v", err)
	}
	if platform != nil {
		t.Errorf("expected no declaration, got %v", platform)
	}
	apiGroup, kind, name, err := Infrastructure(object)
	if err != nil {
		t.Fatalf("unexpected error from Infrastructure: %v", err)
	}
	if apiGroup != "" || kind != "" || name != "" {
		t.Errorf("expected no infrastructure object, got %s/%s/%s", apiGroup, kind, name)
	}
	condition, err := Ready(object)
	if err != nil {
		t.Fatalf("unexpected error from Ready: %v", err)
	}
	if condition != nil {
		t.Errorf("expected no condition, got %v", condition)
	}
}
