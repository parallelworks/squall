package arm_test

import (
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/compute/armcompute/v8"

	"github.com/parallelworks/squall/access"
	"github.com/parallelworks/squall/azure/arm"
)

func TestPolicyRefusesAnActionItDoesNotGrant(t *testing.T) {
	policy := access.Allow("Microsoft.Resources/subscriptions/resourceGroups/write")
	c := setup(t, arm.WithPolicy(policy))
	c.createGroup(t, group, nil)

	_, err := c.groups.Get(t.Context(), group, nil)
	if code(err) != "AuthorizationFailed" || status(err) != http.StatusForbidden {
		t.Fatalf("get = %v, want 403 AuthorizationFailed", err)
	}
	if !strings.Contains(err.Error(), "'Microsoft.Resources/subscriptions/resourceGroups/read'") {
		t.Errorf("error = %v, want it to name the action", err)
	}
}

func TestPolicyChecksResourcesAWriteLinksTo(t *testing.T) {
	policy := access.Allow(
		"Microsoft.Resources/subscriptions/resourceGroups/write",
		"Microsoft.Network/networkInterfaces/write",
		"Microsoft.Network/networkInterfaces/read",
		"Microsoft.Network/networkSecurityGroups/write",
	)
	c := setup(t, arm.WithPolicy(policy))
	c.createGroup(t, group, nil)

	p, err := c.nics.BeginCreateOrUpdate(t.Context(), group, "nic-1", nicBody(nil, nil), nil)
	_, err = wait(t, p, err)
	if code(err) != "LinkedAuthorizationFailed" || status(err) != http.StatusForbidden {
		t.Fatalf("create = %v, want 403 LinkedAuthorizationFailed", err)
	}
	if !strings.Contains(err.Error(), "'Microsoft.Network/virtualNetworks/subnets/join/action' on the linked scope(s) '"+subnet+"'") {
		t.Errorf("error = %v, want it to name the action and the subnet", err)
	}
	denials := policy.Denials()
	if len(denials) != 1 || !slices.Equal(denials[0].Missing, []string{"Microsoft.Network/virtualNetworks/subnets/join/action"}) {
		t.Errorf("Denials() = %+v", denials)
	}
}

func TestPolicyRecordsWhatAllowedCallsNeeded(t *testing.T) {
	policy := access.Allow(
		"Microsoft.Resources/subscriptions/resourceGroups/write",
		"Microsoft.Network/networkInterfaces/*",
		"Microsoft.Network/virtualNetworks/subnets/join/action",
		"Microsoft.Network/publicIPAddresses/join/action",
	)
	c := setup(t, arm.WithPolicy(policy))
	c.createGroup(t, group, nil)

	p, err := c.nics.BeginCreateOrUpdate(t.Context(), group, "nic-1", nicBody(nil, nil), nil)
	if _, err := wait(t, p, err); err != nil {
		t.Fatal(err)
	}
	if got := policy.Unused(); !slices.Equal(got, []string{"Microsoft.Network/publicIPAddresses/join/action"}) {
		t.Errorf("Unused() = %v", got)
	}
	want := []string{
		"Microsoft.Network/networkInterfaces/write",
		"Microsoft.Network/virtualNetworks/subnets/join/action",
		"Microsoft.Resources/subscriptions/resourceGroups/write",
	}
	if got := policy.Used(); !slices.Equal(got, want) {
		t.Errorf("Used() = %v, want %v", got, want)
	}
}

func TestPolicyChecksComputeActions(t *testing.T) {
	policy := access.Allow(
		"Microsoft.Resources/subscriptions/resourceGroups/write",
		"Microsoft.Network/networkInterfaces/*",
		"Microsoft.Network/virtualNetworks/subnets/join/action",
		"Microsoft.Compute/virtualMachines/write",
	)
	c := setup(t, arm.WithPolicy(policy))
	c.createGroup(t, group, nil)
	nic := c.createNIC(t, group, "nic", nil)
	c.createVM(t, "vm", nic)
	for action, call := range map[string]func() error{
		"Microsoft.Compute/virtualMachines/powerOff/action": func() error {
			_, err := c.vms.BeginPowerOff(t.Context(), group, "vm", nil)
			return err
		},
		"Microsoft.Compute/skus/read": func() error {
			_, err := c.skus.NewListPager(nil).NextPage(t.Context())
			return err
		},
		"Microsoft.Compute/galleries/images/versions/write": func() error {
			_, err := c.versions.BeginUpdate(t.Context(), group, "gallery", "image", "1.0.0", armcompute.GalleryImageVersionUpdate{}, nil)
			return err
		},
	} {
		if err := call(); code(err) != "AuthorizationFailed" || !strings.Contains(err.Error(), "'"+action+"'") {
			t.Errorf("%s = %v", action, err)
		}
	}
}
