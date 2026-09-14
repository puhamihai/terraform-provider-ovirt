package ovirt

import (
	"fmt"
	"regexp"
	"testing"

	"github.com/hashicorp/go-cty/cty"
	"github.com/hashicorp/terraform-plugin-sdk/v2/helper/resource"
)

func TestVMHighAvailability(t *testing.T) {
	t.Parallel()
	p := newProvider(newTestLogger(t))
	testHelper := p.getTestHelper()
	clusterID := testHelper.GetClusterID()

	config := fmt.Sprintf(
		`
provider "ovirt" {
	mock = true
}

data "ovirt_blank_template" "blank" {
}

resource "ovirt_vm" "test" {
	template_id                = data.ovirt_blank_template.blank.id
	cluster_id                 = "%s"
	name                       = "%s"
	high_availability          = true
	high_availability_priority = 50
}
`,
		clusterID,
		testHelper.GenerateTestResourceName(t),
	)

	resource.UnitTest(
		t, resource.TestCase{
			ProviderFactories: p.getProviderFactories(),
			Steps: []resource.TestStep{
				{
					// The mock client does not expose the underlying SDK connection, so the
					// high availability settings are skipped with a warning and only the
					// schema plumbing is verified here.
					Config: config,
					Check: resource.ComposeTestCheckFunc(
						resource.TestCheckResourceAttr("ovirt_vm.test", "high_availability", "true"),
						resource.TestCheckResourceAttr("ovirt_vm.test", "high_availability_priority", "50"),
					),
				},
				{
					Config:  config,
					Destroy: true,
				},
			},
		},
	)
}

func TestValidateHighAvailabilityPriority(t *testing.T) {
	t.Parallel()
	testCases := []struct {
		name      string
		input     interface{}
		expectErr bool
	}{
		{"minimum", 1, false},
		{"midrange", 50, false},
		{"maximum", 100, false},
		{"zero", 0, true},
		{"negative", -1, true},
		{"above maximum", 101, true},
		{"not an integer", "50", true},
	}

	for _, tc := range testCases {
		tc := tc
		t.Run(
			tc.name, func(t *testing.T) {
				t.Parallel()
				diags := validateHighAvailabilityPriority(tc.input, cty.Path{})
				if tc.expectErr && !diags.HasError() {
					t.Fatalf("expected an error for input %v, got none", tc.input)
				}
				if !tc.expectErr && diags.HasError() {
					t.Fatalf("expected no error for input %v, got %v", tc.input, diags)
				}
			},
		)
	}
}

// TestVMHighAvailabilityRejectsInvalidConfig verifies that invalid configurations are rejected
// during plan, before any call reaches the engine.
func TestVMHighAvailabilityRejectsInvalidConfig(t *testing.T) {
	t.Parallel()
	testCases := []struct {
		name          string
		attributes    string
		expectedError *regexp.Regexp
	}{
		{
			"priority above maximum",
			"high_availability = true\n\thigh_availability_priority = 101",
			regexp.MustCompile("between 1 and 100"),
		},
		{
			"priority without high availability",
			"high_availability_priority = 50",
			regexp.MustCompile("high_availability"),
		},
	}

	for _, tc := range testCases {
		tc := tc
		t.Run(
			tc.name, func(t *testing.T) {
				t.Parallel()
				p := newProvider(newTestLogger(t))
				testHelper := p.getTestHelper()
				resource.UnitTest(
					t, resource.TestCase{
						ProviderFactories: p.getProviderFactories(),
						Steps: []resource.TestStep{
							{
								Config: fmt.Sprintf(
									`
provider "ovirt" {
	mock = true
}

data "ovirt_blank_template" "blank" {
}

resource "ovirt_vm" "test" {
	template_id = data.ovirt_blank_template.blank.id
	cluster_id  = "%s"
	name        = "%s"
	%s
}
`,
									testHelper.GetClusterID(),
									testHelper.GenerateTestResourceName(t),
									tc.attributes,
								),
								ExpectError: tc.expectedError,
							},
						},
					},
				)
			},
		)
	}
}
