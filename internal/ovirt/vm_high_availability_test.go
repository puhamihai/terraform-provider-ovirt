package ovirt

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"testing"
	"time"

	"github.com/hashicorp/go-cty/cty"
	"github.com/hashicorp/terraform-plugin-sdk/v2/helper/resource"
	"github.com/hashicorp/terraform-plugin-sdk/v2/helper/schema"
	ovirtsdk4 "github.com/ovirt/go-ovirt"
	ovirtclient "github.com/ovirt/go-ovirt-client/v3"
)

// errVMLocked mimics the error the raw oVirt SDK returns when a VM is edited while another
// operation, such as the disk copy of a clone, still holds the VM lock.
var errVMLocked = errors.New( //nolint:staticcheck // mirrors the engine error text verbatim
	`Fault reason is "Operation Failed". Fault detail is "[Cannot edit VM. Related operation is currently ` +
		`in progress. Please try again later.]". HTTP response code is "409". HTTP response message is "409 Conflict"`,
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

func TestRetryWhileVMLocked(t *testing.T) {
	t.Parallel()
	otherConflict := errors.New(
		`Fault reason is "Operation Failed". Fault detail is "[Cannot edit VM. Some other reason.]". ` +
			`HTTP response code is "409". HTTP response message is "409 Conflict"`,
	)
	testCases := []struct {
		name          string
		results       []error
		expectedCalls int
		expectErr     bool
	}{
		{"locked once, then succeeds", []error{errVMLocked, nil}, 2, false},
		{"succeeds immediately", []error{nil}, 1, false},
		{"other error is not retried", []error{errors.New("boom"), nil}, 1, true},
		{"conflict without a lock is not retried", []error{otherConflict, nil}, 1, true},
	}
	for _, tc := range testCases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			calls := 0
			err := retryWhileVMLocked(context.Background(), time.Millisecond, time.Millisecond, func() error {
				result := tc.results[calls]
				calls++
				return result
			})
			if calls != tc.expectedCalls {
				t.Fatalf("expected %d calls, got %d", tc.expectedCalls, calls)
			}
			if tc.expectErr != (err != nil) {
				t.Fatalf("expected error: %t, got: %v", tc.expectErr, err)
			}
		})
	}

	t.Run("gives up when the context expires", func(t *testing.T) {
		t.Parallel()
		ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
		defer cancel()
		calls := 0
		err := retryWhileVMLocked(ctx, time.Millisecond, 5*time.Millisecond, func() error {
			calls++
			return errVMLocked
		})
		if err == nil {
			t.Fatalf("expected an error once the context expired")
		}
		if !isVMLockedError(err) {
			t.Fatalf("expected the returned error to keep the lock error, got: %v", err)
		}
		if calls < 2 {
			t.Fatalf("expected the update to be retried before giving up, got %d calls", calls)
		}
	})
}

// newHighAvailabilityTestProvider returns a provider backed by the mock client, with high
// availability updates sent to update instead of an engine.
func newHighAvailabilityTestProvider(t *testing.T, update vmHighAvailabilityUpdater) *provider {
	p := newProvider(newTestLogger(t)).(*provider)
	p.client = p.testHelper.GetClient()
	p.vmHAUpdater = update
	return p
}

func newHighAvailabilityVMData(t *testing.T, p *provider) *schema.ResourceData {
	return schema.TestResourceDataRaw(t, vmSchema, map[string]interface{}{
		"cluster_id":                 string(p.testHelper.GetClusterID()),
		"template_id":                string(p.testHelper.GetBlankTemplateID()),
		"name":                       p.testHelper.GenerateTestResourceName(t),
		"high_availability":          true,
		"high_availability_priority": 50,
	})
}

func removeTestVM(t *testing.T, p *provider, id string) {
	t.Cleanup(func() {
		if id == "" {
			return
		}
		if err := p.client.WithContext(context.Background()).RemoveVM(ovirtclient.VMID(id)); err != nil {
			t.Logf("failed to remove test VM %s: %v", id, err)
		}
	})
}

// TestVMCreateRetriesHighAvailabilityWhileLocked verifies that create retries the high availability
// update when the engine reports the VM as locked, and sends the configured values.
func TestVMCreateRetriesHighAvailabilityWhileLocked(t *testing.T) {
	t.Parallel()
	calls := 0
	p := newHighAvailabilityTestProvider(t, func(_ string, vm *ovirtsdk4.Vm) error {
		calls++
		ha, ok := vm.HighAvailability()
		if !ok {
			t.Errorf("high availability missing from the VM update")
		} else {
			if enabled, _ := ha.Enabled(); !enabled {
				t.Errorf("expected high availability to be enabled in the VM update")
			}
			if priority, _ := ha.Priority(); priority != 50 {
				t.Errorf("expected high availability priority 50 in the VM update, got %d", priority)
			}
		}
		if calls == 1 {
			return errVMLocked
		}
		return nil
	})
	data := newHighAvailabilityVMData(t, p)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()

	diags := p.vmCreate(ctx, data, nil)
	removeTestVM(t, p, data.Id())

	if diags.HasError() {
		t.Fatalf("expected create to succeed after the lock cleared, got: %v", diags)
	}
	if calls != 2 {
		t.Fatalf("expected the high availability update to be sent twice, got %d", calls)
	}
	if data.Id() == "" {
		t.Fatalf("expected the VM ID to be set")
	}
}

// TestVMCreateKeepsIDWhenHighAvailabilityFails verifies that a failure after the VM was created still
// leaves the ID in state, so Terraform tracks the VM as tainted instead of orphaning it.
func TestVMCreateKeepsIDWhenHighAvailabilityFails(t *testing.T) {
	t.Parallel()
	p := newHighAvailabilityTestProvider(t, func(_ string, _ *ovirtsdk4.Vm) error {
		return errors.New("engine rejected the update")
	})
	data := newHighAvailabilityVMData(t, p)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()

	diags := p.vmCreate(ctx, data, nil)
	removeTestVM(t, p, data.Id())

	if !diags.HasError() {
		t.Fatalf("expected create to fail when the high availability update fails")
	}
	if data.Id() == "" {
		t.Fatalf("expected the VM ID to be set after a failure that follows VM creation")
	}
	if _, err := p.client.WithContext(ctx).GetVM(ovirtclient.VMID(data.Id())); err != nil {
		t.Fatalf("expected the VM recorded in state to exist, got: %v", err)
	}
}
