package v1beta1

import (
	"encoding/json"
	"strings"
	"testing"
)

// platformSpecNMinus1 represents the N-1 version of PlatformSpec: one that has no
// knowledge of the External platform at all. Only the fields exercised by these
// tests are modelled.
type platformSpecNMinus1 struct {
	Type PlatformType     `json:"type,omitempty"` //nolint:kubeapilinter // test-only N-1 compat struct
	AWS  *AWSPlatformSpec `json:"aws,omitempty"`  //nolint:kubeapilinter // test-only N-1 compat struct
}

// nodePoolPlatformNMinus1 is the NodePool equivalent of platformSpecNMinus1.
type nodePoolPlatformNMinus1 struct {
	Type PlatformType         `json:"type,omitempty"` //nolint:kubeapilinter // test-only N-1 compat struct
	AWS  *AWSNodePoolPlatform `json:"aws,omitempty"`  //nolint:kubeapilinter // test-only N-1 compat struct
}

// TestPlatformSpecExternalOmittedWhenUnset is the load-bearing compatibility test for
// adding External to the platform union. ExternalPlatformSpec is a non-pointer struct,
// so it only stays absent from the wire because of omitzero. If it ever serialized as
// an empty object, every existing non-External HostedCluster written by new code would
// carry an `external` key and be rejected by the union CEL rule.
func TestPlatformSpecExternalOmittedWhenUnset(t *testing.T) {
	tests := []struct {
		name    string
		current PlatformSpec
	}{
		{
			name:    "When the platform is None it should omit external",
			current: PlatformSpec{Type: NonePlatform},
		},
		{
			name:    "When the platform is AWS it should omit external",
			current: PlatformSpec{Type: AWSPlatform, AWS: &AWSPlatformSpec{Region: "us-east-1"}},
		},
		{
			name:    "When the platform spec is empty it should omit external",
			current: PlatformSpec{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			data, err := json.Marshal(tt.current)
			if err != nil {
				t.Fatalf("failed to marshal current struct: %v", err)
			}
			if strings.Contains(string(data), `"external"`) {
				t.Errorf("external should be omitted when unset, got %s", string(data))
			}
		})
	}
}

func TestNodePoolPlatformExternalOmittedWhenUnset(t *testing.T) {
	tests := []struct {
		name    string
		current NodePoolPlatform
	}{
		{
			name:    "When the platform is None it should omit external",
			current: NodePoolPlatform{Type: NonePlatform},
		},
		{
			name:    "When the platform is AWS it should omit external",
			current: NodePoolPlatform{Type: AWSPlatform, AWS: &AWSNodePoolPlatform{InstanceType: "m5.large"}},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			data, err := json.Marshal(tt.current)
			if err != nil {
				t.Fatalf("failed to marshal current struct: %v", err)
			}
			if strings.Contains(string(data), `"external"`) {
				t.Errorf("external should be omitted when unset, got %s", string(data))
			}
		})
	}
}

// TestExternalPlatformSpecSerializationCompatibility verifies that an External
// HostedCluster written by new code still deserializes cleanly in N-1 code (which
// simply drops the unknown key), and that N-1 output round-trips back into the
// current type.
func TestExternalPlatformSpecSerializationCompatibility(t *testing.T) {
	current := PlatformSpec{
		Type: ExternalPlatform,
		External: ExternalPlatformSpec{
			HostedClusterTemplate: ExternalTemplateReference{
				APIGroup: "example.io",
				Resource: "foohostedclustertemplates",
				Name:     "my-infra",
			},
		},
	}

	data, err := json.Marshal(current)
	if err != nil {
		t.Fatalf("failed to marshal current struct: %v", err)
	}
	if !strings.Contains(string(data), `"external":{"hostedClusterTemplate":{"apiGroup":"example.io","resource":"foohostedclustertemplates","name":"my-infra"}}`) {
		t.Errorf("unexpected JSON output: %s", string(data))
	}

	var nMinus1 platformSpecNMinus1
	if err := json.Unmarshal(data, &nMinus1); err != nil {
		t.Fatalf("N-1 failed to unmarshal JSON from N: %v", err)
	}
	if nMinus1.Type != ExternalPlatform {
		t.Errorf("N-1 type mismatch: got %q, want %q", nMinus1.Type, ExternalPlatform)
	}

	nMinus1Data, err := json.Marshal(platformSpecNMinus1{Type: NonePlatform})
	if err != nil {
		t.Fatalf("failed to marshal N-1 struct: %v", err)
	}
	var roundTripped PlatformSpec
	if err := json.Unmarshal(nMinus1Data, &roundTripped); err != nil {
		t.Fatalf("N failed to unmarshal JSON from N-1: %v", err)
	}
	if roundTripped.External != (ExternalPlatformSpec{}) {
		t.Errorf("external should be the zero value after N-1 round-trip, got %+v", roundTripped.External)
	}
}

func TestExternalNodePoolPlatformSerializationCompatibility(t *testing.T) {
	current := NodePoolPlatform{
		Type: ExternalPlatform,
		External: ExternalNodePoolPlatform{
			MachineTemplate: ExternalTemplateReference{
				APIGroup: "infrastructure.cluster.x-k8s.io",
				Resource: "foomachinetemplates",
				Name:     "my-machines",
			},
		},
	}

	data, err := json.Marshal(current)
	if err != nil {
		t.Fatalf("failed to marshal current struct: %v", err)
	}
	if !strings.Contains(string(data), `"external":{"machineTemplate":{"apiGroup":"infrastructure.cluster.x-k8s.io","resource":"foomachinetemplates","name":"my-machines"}}`) {
		t.Errorf("unexpected JSON output: %s", string(data))
	}

	var nMinus1 nodePoolPlatformNMinus1
	if err := json.Unmarshal(data, &nMinus1); err != nil {
		t.Fatalf("N-1 failed to unmarshal JSON from N: %v", err)
	}
	if nMinus1.Type != ExternalPlatform {
		t.Errorf("N-1 type mismatch: got %q, want %q", nMinus1.Type, ExternalPlatform)
	}

	nMinus1Data, err := json.Marshal(nodePoolPlatformNMinus1{Type: NonePlatform})
	if err != nil {
		t.Fatalf("failed to marshal N-1 struct: %v", err)
	}
	var roundTripped NodePoolPlatform
	if err := json.Unmarshal(nMinus1Data, &roundTripped); err != nil {
		t.Fatalf("N failed to unmarshal JSON from N-1: %v", err)
	}
	if roundTripped.External != (ExternalNodePoolPlatform{}) {
		t.Errorf("external should be the zero value after N-1 round-trip, got %+v", roundTripped.External)
	}
}
