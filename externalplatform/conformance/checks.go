package conformance

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/openshift/hypershift/externalplatform/contract"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"sigs.k8s.io/controller-runtime/pkg/client"
)

// templateKindSuffix is the suffix the contract requires on the kind of a hosted cluster
// template, and which is stripped to derive the kind of the instantiated object.
const templateKindSuffix = "Template"

// CheckTypes asserts the integrator's types are named and labelled the way the contract
// requires, before anything is created.
//
// These are the failures that are hardest to diagnose from the outside: a kind that does not
// end in Template makes HyperShift reject the HostedCluster with a message about a type the
// user never named, and a missing contract version label makes HyperShift refuse a provider
// it cannot prove it agrees with.
func (s *Suite) CheckTypes(ctx context.Context) error {
	if !strings.HasSuffix(s.templateGVK.Kind, templateKindSuffix) {
		return fmt.Errorf("the hosted cluster template kind %q must end in %q", s.templateGVK.Kind, templateKindSuffix)
	}
	if expected := strings.TrimSuffix(s.templateGVK.Kind, templateKindSuffix); s.instanceGVK.Kind != expected {
		return fmt.Errorf("the template %q should instantiate a %q, but the API group serves %q for that resource",
			s.templateGVK.Kind, expected, s.instanceGVK.Kind)
	}
	if s.templateGVK.Version != s.instanceGVK.Version {
		// HyperShift instantiates into the template's own group and version. Two versions
		// would mean HyperShift writing a version the integrator does not serve as preferred.
		return fmt.Errorf("the template is served at %s and the instantiated object at %s; both must be the same version",
			s.templateGVK.Version, s.instanceGVK.Version)
	}

	definition := &apiextensionsv1.CustomResourceDefinition{}
	definitionName := fmt.Sprintf("%s.%s", s.TemplateResource, s.APIGroup)
	if err := s.Client.Get(ctx, client.ObjectKey{Name: definitionName}, definition); err != nil {
		return fmt.Errorf("failed to read the custom resource definition %s: %w", definitionName, err)
	}
	version, found := definition.Labels[contract.VersionLabel]
	if !found {
		return fmt.Errorf("the custom resource definition %s must carry the label %s declaring which contract version it implements",
			definitionName, contract.VersionLabel)
	}
	if version != contract.Version {
		// Not a warning. HyperShift supports the current version and the one before it, and
		// this suite is built from one specific version, so a suite that passed against a
		// version it does not implement would prove nothing.
		return fmt.Errorf("the custom resource definition %s declares contract version %q, but this suite is built from %q",
			definitionName, version, contract.Version)
	}

	instanceDefinition := &apiextensionsv1.CustomResourceDefinition{}
	instanceResource, err := contract.InstanceResource(s.TemplateResource)
	if err != nil {
		return err
	}
	instanceDefinitionName := fmt.Sprintf("%s.%s", instanceResource, s.APIGroup)
	if err := s.Client.Get(ctx, client.ObjectKey{Name: instanceDefinitionName}, instanceDefinition); err != nil {
		return fmt.Errorf("failed to read the custom resource definition %s: %w", instanceDefinitionName, err)
	}
	for _, served := range instanceDefinition.Spec.Versions {
		if served.Name != s.instanceGVK.Version {
			continue
		}
		if served.Subresources == nil || served.Subresources.Status == nil {
			// Everything the contract asks the integrator to publish lives under status. A
			// type without the subresource would have the provider and HyperShift writing the
			// same object, and the provider able to undo the endpoint HyperShift wrote.
			return fmt.Errorf("the custom resource definition %s must enable the status subresource for version %s",
				instanceDefinitionName, served.Name)
		}
	}
	return nil
}

// CheckProvisioning waits for the integrator's controller to finish, asserting the ordering
// the contract requires along the way.
//
// The ordering assertions are the point. A provider that eventually reaches the right state
// still breaks clusters if it reports ready before declaring its platform, because HyperShift
// will have rendered a machine config by then.
func (s *Suite) CheckProvisioning(ctx context.Context) error {
	var declaredBeforeReady bool

	err := s.poll(ctx, "the provider to report Ready=True", func(object *unstructured.Unstructured) (bool, error) {
		if object == nil {
			return false, fmt.Errorf("the hosted cluster object disappeared during provisioning")
		}

		platform, err := contract.Platform(object)
		if err != nil {
			return false, err
		}
		condition, err := contract.Ready(object)
		if err != nil {
			return false, err
		}
		if platform != nil {
			declaredBeforeReady = true
		}
		if condition == nil {
			return false, nil
		}
		if condition.Reason == "" {
			return false, fmt.Errorf("the Ready condition has no reason; it is the only machine-readable account of what the provider is doing")
		}
		if condition.Status != metav1.ConditionTrue {
			return false, nil
		}

		if !declaredBeforeReady {
			return false, fmt.Errorf("the object reported Ready=True without ever declaring status.platform; HyperShift will not render the guest cluster's Infrastructure until it is present, so nodes would never boot")
		}
		if len(object.GetFinalizers()) == 0 {
			return false, fmt.Errorf("the object reported Ready=True with no finalizer; whatever the provider created would leak when the cluster is deleted")
		}
		apiGroup, kind, name, err := contract.Infrastructure(object)
		if err != nil {
			return false, err
		}
		if apiGroup == "" || kind == "" || name == "" {
			return false, fmt.Errorf("the object reported Ready=True without naming a Cluster API infrastructure object in status.infrastructure; HyperShift creates no Cluster without it")
		}
		return true, nil
	})
	if err != nil {
		return err
	}
	return nil
}

// CheckSettled watches a ready cluster for a while and asserts the provider has stopped
// changing it.
//
// A provider that rewrites its status on every reconcile is not merely noisy: HyperShift
// reports how long the provider has been in its current state from lastTransitionTime, and a
// provider that moves it on every pass makes a cluster that has been stuck for an hour look
// like it just got there.
func (s *Suite) CheckSettled(ctx context.Context) error {
	object, err := s.HostedClusterObject(ctx)
	if err != nil {
		return fmt.Errorf("failed to read the hosted cluster object: %w", err)
	}
	platform, err := contract.Platform(object)
	if err != nil {
		return err
	}
	condition, err := contract.Ready(object)
	if err != nil {
		return err
	}
	if platform == nil || condition == nil {
		return fmt.Errorf("the hosted cluster object is not provisioned; run CheckProvisioning first")
	}
	apiGroup, kind, name, err := contract.Infrastructure(object)
	if err != nil {
		return err
	}

	deadline := time.Now().Add(s.SettleFor)
	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(s.Interval):
		}

		object, err := s.HostedClusterObject(ctx)
		if err != nil {
			return fmt.Errorf("failed to read the hosted cluster object: %w", err)
		}

		observedPlatform, err := contract.Platform(object)
		if err != nil {
			return err
		}
		if observedPlatform == nil || *observedPlatform != *platform {
			// HyperShift records the declaration once and never re-reads it, so a provider
			// that changes it has produced a cluster whose nodes disagree with its own status
			// and cannot be fixed without recreating it.
			return fmt.Errorf("the platform declaration changed from %+v to %+v after the object became ready; it is immutable once observed",
				*platform, observedPlatform)
		}

		observedCondition, err := contract.Ready(object)
		if err != nil {
			return err
		}
		if observedCondition == nil || observedCondition.Status != metav1.ConditionTrue {
			return fmt.Errorf("the Ready condition went from True to %v after provisioning finished", observedCondition)
		}
		if !observedCondition.LastTransitionTime.Equal(&condition.LastTransitionTime) {
			return fmt.Errorf("the Ready condition's lastTransitionTime moved from %s to %s without the status changing; preserve it when the status is unchanged",
				condition.LastTransitionTime, observedCondition.LastTransitionTime)
		}

		observedAPIGroup, observedKind, observedName, err := contract.Infrastructure(object)
		if err != nil {
			return err
		}
		if observedAPIGroup != apiGroup || observedKind != kind || observedName != name {
			// The Cluster API Cluster's spec.infrastructureRef is immutable, so HyperShift
			// cannot follow a provider that renames its infrastructure object.
			return fmt.Errorf("the infrastructure reference changed from %s/%s/%s to %s/%s/%s; it is immutable once HyperShift has created the Cluster",
				apiGroup, kind, name, observedAPIGroup, observedKind, observedName)
		}
	}
	return nil
}

// CheckDeprovisioning deletes the hosted cluster object and asserts the provider releases it.
//
// A provider whose teardown never completes makes the HostedCluster undeletable, which is the
// worst failure in the contract: it needs an administrator to strip a finalizer by hand and
// leaks whatever was provisioned. It is also the path least likely to be exercised by an
// integrator's own testing, which usually stops at a working cluster.
func (s *Suite) CheckDeprovisioning(ctx context.Context) error {
	object, err := s.HostedClusterObject(ctx)
	if err != nil {
		return fmt.Errorf("failed to read the hosted cluster object: %w", err)
	}
	if len(object.GetFinalizers()) == 0 {
		return fmt.Errorf("the hosted cluster object holds no finalizer, so deleting it would not wait for the provider")
	}

	if err := s.Client.Delete(ctx, object); err != nil {
		return fmt.Errorf("failed to delete the hosted cluster object: %w", err)
	}

	return s.poll(ctx, "the provider to finish tearing down and release its finalizer", func(object *unstructured.Unstructured) (bool, error) {
		if object == nil {
			return true, nil
		}
		if object.GetDeletionTimestamp().IsZero() {
			return false, fmt.Errorf("the hosted cluster object has no deletion timestamp after being deleted")
		}
		return false, nil
	})
}
