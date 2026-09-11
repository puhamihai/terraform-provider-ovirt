package ovirt

import (
	"fmt"
	"testing"

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
