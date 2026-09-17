package contract

import (
	"fmt"

	hyperv1 "github.com/openshift/hypershift/api/hypershift/v1beta1"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// normalizeStatus drops an explicitly null status.
//
// An object whose status subresource has never been written can come back carrying
// "status": null rather than no status at all, and every nested accessor below reports that
// as a type error rather than as an empty object. Dropping the key changes nothing that is
// serialized and makes the first write to status behave like every subsequent one.
func normalizeStatus(hostedClusterObject *unstructured.Unstructured) {
	if hostedClusterObject.Object == nil {
		hostedClusterObject.Object = map[string]any{}
		return
	}
	if status, found := hostedClusterObject.Object["status"]; found && status == nil {
		delete(hostedClusterObject.Object, "status")
	}
}

// ControlPlaneEndpoint returns the endpoint HyperShift wrote onto the hosted cluster object.
//
// It is the one part of the spec HyperShift keeps reconciling, because it is not knowable
// when the user writes the template and it can legitimately change. An integrator that
// provisions a load balancer or a DNS record points it here.
//
// A zero endpoint means HyperShift has not reported one yet. That cannot normally happen,
// because the object is not created until the endpoint is known, but an integrator should
// requeue rather than provision against an empty host.
func ControlPlaneEndpoint(hostedClusterObject *unstructured.Unstructured) (hyperv1.APIEndpoint, error) {
	host, _, err := unstructured.NestedString(hostedClusterObject.Object, "spec", "controlPlaneEndpoint", "host")
	if err != nil {
		return hyperv1.APIEndpoint{}, fmt.Errorf("failed to read spec.controlPlaneEndpoint.host: %w", err)
	}
	port, _, err := unstructured.NestedInt64(hostedClusterObject.Object, "spec", "controlPlaneEndpoint", "port")
	if err != nil {
		return hyperv1.APIEndpoint{}, fmt.Errorf("failed to read spec.controlPlaneEndpoint.port: %w", err)
	}
	return hyperv1.APIEndpoint{Host: host, Port: int32(port)}, nil
}

// SetPlatform publishes the integrator's platform declaration.
//
// Both values have to be set together and before the object first reports ready. HyperShift
// will not render the guest cluster's Infrastructure until the declaration is present,
// because guessing would bake the wrong cloud provider into every node that boots, and the
// NodePool configuration hash would then change when the real value arrived, rolling the
// whole pool.
//
// HyperShift records the declaration the first time it observes it and never re-reads it. A
// later change is reported as a degraded condition and otherwise ignored, because acting on
// it would roll every node in the cluster onto a configuration that disagrees with the one
// the cluster was installed with. In practice an integrator hardcodes both values per
// platform, which is why this takes them rather than reading them from anywhere.
func SetPlatform(hostedClusterObject *unstructured.Unstructured, name string, state hyperv1.ExternalCloudControllerManagerState) error {
	normalizeStatus(hostedClusterObject)

	if name == "" {
		return fmt.Errorf("the platform name must not be empty")
	}
	switch state {
	case hyperv1.ExternalCloudControllerManager, hyperv1.NoCloudControllerManager:
	default:
		return fmt.Errorf("the cloud controller manager state must be %q or %q, got %q",
			hyperv1.ExternalCloudControllerManager, hyperv1.NoCloudControllerManager, state)
	}

	if err := unstructured.SetNestedField(hostedClusterObject.Object, name, "status", "platform", "name"); err != nil {
		return fmt.Errorf("failed to set status.platform.name: %w", err)
	}
	if err := unstructured.SetNestedField(hostedClusterObject.Object, string(state), "status", "platform", "cloudControllerManager", "state"); err != nil {
		return fmt.Errorf("failed to set status.platform.cloudControllerManager.state: %w", err)
	}
	return nil
}

// Platform reads the integrator's declaration back.
//
// A nil return with a nil error means the integrator has not declared anything yet, which is
// the normal state early in provisioning. An incomplete or invalid block is an error rather
// than a nil: the contract asks for both values together, and recording half of one would
// pin a value the integrator never agreed to.
func Platform(hostedClusterObject *unstructured.Unstructured) (*hyperv1.ExternalPlatformStatus, error) {
	normalizeStatus(hostedClusterObject)

	platform, found, err := unstructured.NestedMap(hostedClusterObject.Object, "status", "platform")
	if err != nil {
		return nil, fmt.Errorf("failed to read status.platform: %w", err)
	}
	if !found || len(platform) == 0 {
		return nil, nil
	}

	name, _, err := unstructured.NestedString(platform, "name")
	if err != nil {
		return nil, fmt.Errorf("failed to read status.platform.name: %w", err)
	}
	if name == "" {
		return nil, fmt.Errorf("status.platform.name is not set")
	}

	state, _, err := unstructured.NestedString(platform, "cloudControllerManager", "state")
	if err != nil {
		return nil, fmt.Errorf("failed to read status.platform.cloudControllerManager.state: %w", err)
	}
	switch hyperv1.ExternalCloudControllerManagerState(state) {
	case hyperv1.ExternalCloudControllerManager, hyperv1.NoCloudControllerManager:
	default:
		// Checked here rather than left to the API server so that a typo surfaces as a
		// message naming the offending value, instead of as a rejected status patch on an
		// unrelated field.
		return nil, fmt.Errorf("status.platform.cloudControllerManager.state must be %q or %q, got %q",
			hyperv1.ExternalCloudControllerManager, hyperv1.NoCloudControllerManager, state)
	}

	return &hyperv1.ExternalPlatformStatus{
		Name: name,
		CloudControllerManager: hyperv1.ExternalCloudControllerManagerStatus{
			State: hyperv1.ExternalCloudControllerManagerState(state),
		},
	}, nil
}

// SetInfrastructure names the Cluster API infrastructure object the integrator stood up.
//
// HyperShift creates no Cluster API Cluster until all three values are present, and then
// points the Cluster's spec.infrastructureRef at what is named here. That field is immutable
// in Cluster API, which is why HyperShift cannot create the Cluster up front and guess, and
// why this cannot usefully be changed afterwards.
//
// No version, matching Cluster API's own ContractVersionedObjectReference. HyperShift
// resolves the served version from the management cluster.
func SetInfrastructure(hostedClusterObject *unstructured.Unstructured, apiGroup, kind, name string) error {
	normalizeStatus(hostedClusterObject)

	for _, field := range []struct{ key, value string }{
		{"apiGroup", apiGroup},
		{"kind", kind},
		{"name", name},
	} {
		if field.value == "" {
			return fmt.Errorf("status.infrastructure.%s must not be empty", field.key)
		}
		if err := unstructured.SetNestedField(hostedClusterObject.Object, field.value, "status", "infrastructure", field.key); err != nil {
			return fmt.Errorf("failed to set status.infrastructure.%s: %w", field.key, err)
		}
	}
	return nil
}

// Infrastructure reads back the Cluster API infrastructure object the integrator named.
// Empty values mean the integrator has not reported one yet.
func Infrastructure(hostedClusterObject *unstructured.Unstructured) (apiGroup, kind, name string, err error) {
	normalizeStatus(hostedClusterObject)

	for _, field := range []struct {
		key  string
		into *string
	}{
		{"apiGroup", &apiGroup},
		{"kind", &kind},
		{"name", &name},
	} {
		value, _, readErr := unstructured.NestedString(hostedClusterObject.Object, "status", "infrastructure", field.key)
		if readErr != nil {
			return "", "", "", fmt.Errorf("failed to read status.infrastructure.%s: %w", field.key, readErr)
		}
		*field.into = value
	}
	return apiGroup, kind, name, nil
}

// SetReady sets the contract's Ready condition.
//
// The message is mirrored verbatim onto the HostedCluster, so write it for an operator who
// cannot see the provider's logs and who has no other source of provider-specific detail.
//
// lastTransitionTime is preserved when the status is unchanged, so that HyperShift's
// "waiting on the provider for N minutes" reflects how long the provider has actually been
// in this state rather than how long ago it last reconciled.
func SetReady(hostedClusterObject *unstructured.Unstructured, status metav1.ConditionStatus, reason, message string) error {
	normalizeStatus(hostedClusterObject)

	conditions, _, err := unstructured.NestedSlice(hostedClusterObject.Object, "status", "conditions")
	if err != nil {
		return fmt.Errorf("failed to read status.conditions: %w", err)
	}

	now := metav1.Now().UTC().Format(metav1.RFC3339Micro)
	condition := map[string]any{
		"type":               ReadyConditionType,
		"status":             string(status),
		"reason":             reason,
		"message":            message,
		"lastTransitionTime": now,
	}

	for i, item := range conditions {
		entry, ok := item.(map[string]any)
		if !ok {
			continue
		}
		if conditionType, _, _ := unstructured.NestedString(entry, "type"); conditionType != ReadyConditionType {
			continue
		}
		if existingStatus, _, _ := unstructured.NestedString(entry, "status"); existingStatus == string(status) {
			if existing, found, _ := unstructured.NestedString(entry, "lastTransitionTime"); found && existing != "" {
				condition["lastTransitionTime"] = existing
			}
		}
		conditions[i] = condition
		return setConditions(hostedClusterObject, conditions)
	}

	return setConditions(hostedClusterObject, append(conditions, condition))
}

// Ready returns the contract's Ready condition, or nil if the integrator has not set one.
func Ready(hostedClusterObject *unstructured.Unstructured) (*metav1.Condition, error) {
	normalizeStatus(hostedClusterObject)

	conditions, found, err := unstructured.NestedSlice(hostedClusterObject.Object, "status", "conditions")
	if err != nil {
		return nil, fmt.Errorf("failed to read status.conditions: %w", err)
	}
	if !found {
		return nil, nil
	}
	for _, item := range conditions {
		entry, ok := item.(map[string]any)
		if !ok {
			continue
		}
		conditionType, _, _ := unstructured.NestedString(entry, "type")
		if conditionType != ReadyConditionType {
			continue
		}
		status, _, _ := unstructured.NestedString(entry, "status")
		reason, _, _ := unstructured.NestedString(entry, "reason")
		message, _, _ := unstructured.NestedString(entry, "message")
		return &metav1.Condition{
			Type:    conditionType,
			Status:  metav1.ConditionStatus(status),
			Reason:  reason,
			Message: message,
		}, nil
	}
	return nil, nil
}

func setConditions(hostedClusterObject *unstructured.Unstructured, conditions []any) error {
	if err := unstructured.SetNestedSlice(hostedClusterObject.Object, conditions, "status", "conditions"); err != nil {
		return fmt.Errorf("failed to set status.conditions: %w", err)
	}
	return nil
}
