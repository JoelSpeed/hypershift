package contract

import (
	"testing"
)

func TestInstanceResource(t *testing.T) {
	for _, testCase := range []struct {
		name             string
		templateResource string
		expected         string
		expectError      bool
	}{
		{
			name:             "strips templates from a well formed resource",
			templateResource: "foohostedclustertemplates",
			expected:         "foohostedclusters",
		},
		{
			name:             "leaves the rest of the resource alone",
			templateResource: "examplecloudhostedclustertemplates",
			expected:         "examplecloudhostedclusters",
		},
		{
			name:             "rejects a resource that is not a template",
			templateResource: "foohostedclusters",
			expectError:      true,
		},
		{
			name:             "rejects the singular form",
			templateResource: "foohostedclustertemplate",
			expectError:      true,
		},
		{
			name:             "rejects an empty resource",
			templateResource: "",
			expectError:      true,
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			actual, err := InstanceResource(testCase.templateResource)
			if testCase.expectError {
				if err == nil {
					t.Fatalf("expected an error, got resource %q", actual)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if actual != testCase.expected {
				t.Errorf("expected resource %q, got %q", testCase.expected, actual)
			}
		})
	}
}
