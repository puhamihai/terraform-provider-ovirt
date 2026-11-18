package ovirt

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/hashicorp/terraform-plugin-sdk/v2/diag"
	"github.com/hashicorp/terraform-plugin-sdk/v2/helper/schema"
	ovirtsdk4 "github.com/ovirt/go-ovirt"
	ovirtclient "github.com/ovirt/go-ovirt-client/v3"
)

const (
	// vmUnlockPollInterval is how often the VM status is polled while waiting for the VM to leave
	// the image_locked status, for example while a cloned template disk is still being copied.
	vmUnlockPollInterval = 5 * time.Second
	// vmLockRetryInitialBackoff and vmLockRetryMaxBackoff bound the exponential backoff used when
	// the engine rejects a VM update because another operation holds the VM lock.
	vmLockRetryInitialBackoff = 2 * time.Second
	vmLockRetryMaxBackoff     = 30 * time.Second
)

// vmHighAvailabilityUpdater sends a high availability update for a single VM to the engine.
type vmHighAvailabilityUpdater func(vmID string, vm *ovirtsdk4.Vm) error

// highAvailabilityUpdater returns the function used to send high availability updates. go-ovirt-client
// v3 does not expose high availability on its VM parameters, so the update goes through the raw oVirt
// SDK. The second return value is false when no SDK connection is available (mock mode).
func (p *provider) highAvailabilityUpdater() (vmHighAvailabilityUpdater, bool) {
	if p.vmHAUpdater != nil {
		return p.vmHAUpdater, true
	}
	legacyClient, ok := p.client.(ovirtclient.ClientWithLegacySupport)
	if !ok {
		return nil, false
	}
	return func(vmID string, vm *ovirtsdk4.Vm) error {
		_, err := legacyClient.GetSDKClient().SystemService().VmsService().VmService(vmID).Update().Vm(vm).Send()
		return err
	}, true
}

// applyVMHighAvailability sets the high availability configuration on a VM. Updates rejected because
// the VM is locked by another operation are retried with backoff until ctx expires.
func (p *provider) applyVMHighAvailability(
	ctx context.Context,
	data *schema.ResourceData,
	vmID string,
	diags diag.Diagnostics,
) diag.Diagnostics {
	update, ok := p.highAvailabilityUpdater()
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
	err = retryWhileVMLocked(ctx, vmLockRetryInitialBackoff, vmLockRetryMaxBackoff, func() error {
		return update(vmID, sdkVM)
	})
	if err != nil {
		return append(diags, errorToDiag(fmt.Sprintf("set high availability on VM %s", vmID), err))
	}
	return diags
}

// isVMLockedError reports whether err is the engine refusing a VM change because another operation
// (such as a disk copy after a clone) currently holds the VM lock. The raw SDK reports these as
// plain errors, so the HTTP status and fault text are matched in the message.
func isVMLockedError(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	if !strings.Contains(msg, `HTTP response code is "409"`) {
		return false
	}
	return strings.Contains(msg, "Related operation is currently in progress") ||
		strings.Contains(msg, "VM is locked")
}

// retryWhileVMLocked calls fn until it succeeds, fails with an error other than a VM lock conflict, or
// ctx expires. The wait between attempts starts at initialBackoff and doubles up to maxBackoff.
func retryWhileVMLocked(ctx context.Context, initialBackoff, maxBackoff time.Duration, fn func() error) error {
	wait := initialBackoff
	for {
		err := fn()
		if !isVMLockedError(err) {
			return err
		}
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			return fmt.Errorf("VM still locked by another operation when the timeout expired: %w", err)
		case <-timer.C:
		}
		wait *= 2
		if wait > maxBackoff {
			wait = maxBackoff
		}
	}
}

// waitForVMUnlocked polls the VM until it leaves the image_locked status, returning the latest VM.
// The wait ends with an error when ctx expires.
func waitForVMUnlocked(
	ctx context.Context,
	client ovirtclient.Client,
	id ovirtclient.VMID,
	interval time.Duration,
) (ovirtclient.VM, error) {
	for {
		vm, err := client.GetVM(id)
		if err != nil {
			return nil, err
		}
		if vm.Status() != ovirtclient.VMStatusImageLocked {
			return vm, nil
		}
		timer := time.NewTimer(interval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return vm, fmt.Errorf(
				"timed out waiting for VM %s to leave status %s (%w)",
				id,
				ovirtclient.VMStatusImageLocked,
				ctx.Err(),
			)
		case <-timer.C:
		}
	}
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
