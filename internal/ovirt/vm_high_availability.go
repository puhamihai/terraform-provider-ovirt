package ovirt

import (
	"fmt"

	"github.com/hashicorp/terraform-plugin-sdk/v2/diag"
	"github.com/hashicorp/terraform-plugin-sdk/v2/helper/schema"
	ovirtsdk4 "github.com/ovirt/go-ovirt"
	ovirtclient "github.com/ovirt/go-ovirt-client/v3"
)

// applyVMHighAvailability sets the high availability configuration on a VM using the raw oVirt SDK,
// as go-ovirt-client v3 does not expose high availability on its VM parameters.
func (p *provider) applyVMHighAvailability(
	data *schema.ResourceData,
	vmID string,
	diags diag.Diagnostics,
) diag.Diagnostics {
	legacyClient, ok := p.client.(ovirtclient.ClientWithLegacySupport)
	if !ok {
		return append(diags, diag.Diagnostic{
			Severity: diag.Warning,
			Summary:  "High availability not applied",
			Detail:   "The configured client does not expose the underlying oVirt SDK connection (mock mode?), skipping high_availability.",
		})
	}
	haBuilder := ovirtsdk4.NewHighAvailabilityBuilder().Enabled(data.Get("high_availability").(bool))
	if priority := data.Get("high_availability_priority").(int); priority != 0 {
		haBuilder.Priority(int64(priority))
	}
	sdkVM, err := ovirtsdk4.NewVmBuilder().HighAvailabilityBuilder(haBuilder).Build()
	if err != nil {
		return append(diags, errorToDiag("build high availability VM update", err))
	}
	_, err = legacyClient.GetSDKClient().SystemService().VmsService().VmService(vmID).Update().Vm(sdkVM).Send()
	if err != nil {
		return append(diags, errorToDiag(fmt.Sprintf("set high availability on VM %s", vmID), err))
	}
	return diags
}

// readVMHighAvailability reads the high availability configuration of a VM back into the Terraform
// state. To avoid an extra engine call and spurious diffs for users that do not use high
// availability, it only runs when the state carries a high availability configuration.
func (p *provider) readVMHighAvailability(data *schema.ResourceData, diags diag.Diagnostics) diag.Diagnostics {
	priorityConfigured := data.Get("high_availability_priority").(int) != 0
	if !data.Get("high_availability").(bool) && !priorityConfigured {
		return diags
	}
	legacyClient, ok := p.client.(ovirtclient.ClientWithLegacySupport)
	if !ok {
		return diags
	}
	resp, err := legacyClient.GetSDKClient().SystemService().VmsService().VmService(data.Id()).Get().Send()
	if err != nil {
		return append(diags, errorToDiag(fmt.Sprintf("read high availability of VM %s", data.Id()), err))
	}
	sdkVM, ok := resp.Vm()
	if !ok {
		return diags
	}
	ha, ok := sdkVM.HighAvailability()
	if !ok {
		return diags
	}
	if enabled, ok := ha.Enabled(); ok {
		diags = setResourceField(data, "high_availability", enabled, diags)
	}
	if priority, ok := ha.Priority(); ok && priorityConfigured {
		diags = setResourceField(data, "high_availability_priority", int(priority), diags)
	}
	return diags
}
