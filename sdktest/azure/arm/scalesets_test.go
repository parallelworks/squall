package arm_test

import (
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/compute/armcompute/v8"

	"github.com/parallelworks/squall/azure/arm"
	"github.com/parallelworks/squall/fault"
)

func scaleSetBody(capacity *int64) armcompute.VirtualMachineScaleSet {
	return armcompute.VirtualMachineScaleSet{
		Location: new(location),
		Tags:     map[string]*string{"session": new("s")},
		SKU:      &armcompute.SKU{Name: new("Standard_D2s_v5"), Capacity: capacity},
		Properties: &armcompute.VirtualMachineScaleSetProperties{
			UpgradePolicy: &armcompute.UpgradePolicy{Mode: new(armcompute.UpgradeModeManual)},
			VirtualMachineProfile: &armcompute.VirtualMachineScaleSetVMProfile{
				OSProfile: &armcompute.VirtualMachineScaleSetOSProfile{
					ComputerNamePrefix: new("node"), AdminUsername: new("azureuser"), AdminPassword: new("secret"),
				},
				StorageProfile: &armcompute.VirtualMachineScaleSetStorageProfile{
					ImageReference: &armcompute.ImageReference{
						Publisher: new("Canonical"), Offer: new("ubuntu-24_04-lts"), SKU: new("server"), Version: new("latest"),
					},
				},
				NetworkProfile: &armcompute.VirtualMachineScaleSetNetworkProfile{
					NetworkInterfaceConfigurations: []*armcompute.VirtualMachineScaleSetNetworkConfiguration{{
						Name: new("nic"),
						Properties: &armcompute.VirtualMachineScaleSetNetworkConfigurationProperties{
							Primary: new(true),
							IPConfigurations: []*armcompute.VirtualMachineScaleSetIPConfiguration{{
								Name: new("ip"),
								Properties: &armcompute.VirtualMachineScaleSetIPConfigurationProperties{
									Subnet: &armcompute.APIEntityReference{ID: new(subnet)},
									PublicIPAddressConfiguration: &armcompute.VirtualMachineScaleSetPublicIPAddressConfiguration{
										Name:       new("pub"),
										Properties: &armcompute.VirtualMachineScaleSetPublicIPAddressConfigurationProperties{IdleTimeoutInMinutes: new(int32(15))},
									},
								},
							}},
						},
					}},
				},
			},
		},
	}
}

func (c *clients) createScaleSet(t *testing.T, name string, capacity int64) armcompute.VirtualMachineScaleSet {
	t.Helper()
	p, err := c.sets.BeginCreateOrUpdate(t.Context(), group, name, scaleSetBody(new(capacity)), nil)
	res, err := wait(t, p, err)
	if err != nil {
		t.Fatal(err)
	}
	return res.VirtualMachineScaleSet
}

func (c *clients) listScaleSetVMs(t *testing.T, name string, expand bool) []*armcompute.VirtualMachineScaleSetVM {
	t.Helper()
	var opts *armcompute.VirtualMachineScaleSetVMsClientListOptions
	if expand {
		opts = &armcompute.VirtualMachineScaleSetVMsClientListOptions{Expand: new("instanceView")}
	}
	var out []*armcompute.VirtualMachineScaleSetVM
	pager := c.setVMs.NewListPager(group, name, opts)
	for pager.More() {
		page, err := pager.NextPage(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, page.Value...)
	}
	return out
}

func instanceIDs(vms []*armcompute.VirtualMachineScaleSetVM) []string {
	var ids []string
	for _, vm := range vms {
		ids = append(ids, *vm.InstanceID)
	}
	return ids
}

func TestScaleSets(t *testing.T) {
	c := setup(t)
	c.createGroup(t, group, nil)
	set := c.createScaleSet(t, "set", 2)
	if *set.SKU.Capacity != 2 || *set.Properties.ProvisioningState != "Succeeded" || *set.Properties.UniqueID == "" ||
		*set.Properties.OrchestrationMode != armcompute.OrchestrationModeUniform ||
		set.Properties.VirtualMachineProfile.OSProfile.AdminPassword != nil {
		t.Fatalf("scale set = %+v", set.Properties)
	}
	vms := c.listScaleSetVMs(t, "set", true)
	if got := instanceIDs(vms); !slices.Equal(got, []string{"0", "1"}) {
		t.Fatalf("instances = %v", got)
	}
	vm := vms[1]
	if *vm.Name != "set_1" || *vm.ID != *set.ID+"/virtualMachines/1" || *vm.Properties.ProvisioningState != "Succeeded" ||
		vm.Properties.TimeCreated == nil || *vm.Properties.OSProfile.ComputerName != "node000001" ||
		!*vm.Properties.LatestModelApplied || powerState(vm.Properties.InstanceView.Statuses) != "running" ||
		*vm.Properties.NetworkProfile.NetworkInterfaces[0].ID != *vm.ID+"/networkInterfaces/nic" {
		t.Fatalf("instance = %+v", vm.Properties)
	}
	ip, err := c.nics.GetVirtualMachineScaleSetIPConfiguration(t.Context(), group, "set", "1", "nic", "ip", nil)
	if err != nil || *ip.Properties.PrivateIPAddress != "10.0.0.5" || *ip.Properties.Subnet.ID != subnet ||
		*ip.Properties.PublicIPAddress.ID != *vm.ID+"/networkInterfaces/nic/ipConfigurations/ip/publicIPAddresses/pub" {
		t.Fatalf("ip configuration = %+v, %v", ip, err)
	}
	pub, err := c.pips.GetVirtualMachineScaleSetPublicIPAddress(t.Context(), group, "set", "1", "nic", "ip", "pub", nil)
	if err != nil || *pub.Properties.IPAddress != "100.64.0.2" || *pub.Properties.IdleTimeoutInMinutes != 15 {
		t.Fatalf("public ip = %+v, %v", pub, err)
	}
	if _, err := c.nics.GetVirtualMachineScaleSetIPConfiguration(t.Context(), group, "set", "7", "nic", "ip", nil); status(err) != http.StatusNotFound {
		t.Fatalf("ip configuration of a missing instance = %v", err)
	}
	one, err := c.setVMs.Get(t.Context(), group, "set", "0", nil)
	if err != nil || *one.InstanceID != "0" || *one.Properties.OSProfile.ComputerName != "node000000" {
		t.Fatalf("get instance = %+v, %v", one, err)
	}
	if _, err := c.setVMs.Get(t.Context(), group, "set", "9", nil); code(err) != "NotFound" {
		t.Fatalf("get of a missing instance = %v", err)
	}
	if got, err := c.sets.Get(t.Context(), group, "SET", nil); err != nil || *got.SKU.Capacity != 2 || *got.Tags["session"] != "s" {
		t.Fatalf("get = %+v, %v", got, err)
	}

	// PATCH merges the capacity; the new VM takes the next instance ID.
	p, err := c.sets.BeginUpdate(t.Context(), group, "set", armcompute.VirtualMachineScaleSetUpdate{SKU: &armcompute.SKU{Capacity: new(int64(3))}}, nil)
	updated, err := wait(t, p, err)
	if err != nil || *updated.SKU.Capacity != 3 || *updated.SKU.Name != "Standard_D2s_v5" || *updated.Tags["session"] != "s" {
		t.Fatalf("patch = %+v, %v", updated.SKU, err)
	}
	dp, err := c.sets.BeginDeleteInstances(t.Context(), group, "set", armcompute.VirtualMachineScaleSetVMInstanceRequiredIDs{InstanceIDs: []*string{new("0")}}, nil)
	if _, err = wait(t, dp, err); err != nil {
		t.Fatal(err)
	}
	if got := instanceIDs(c.listScaleSetVMs(t, "set", false)); !slices.Equal(got, []string{"1", "2"}) {
		t.Fatalf("after deleting instance 0 = %v", got)
	}
	dp, err = c.sets.BeginDeleteInstances(t.Context(), group, "set", armcompute.VirtualMachineScaleSetVMInstanceRequiredIDs{InstanceIDs: []*string{new("0")}}, nil)
	if _, err = wait(t, dp, err); code(err) != "InvalidParameter" || status(err) != http.StatusBadRequest {
		t.Fatalf("deleting a missing instance = %v", err)
	}
	if _, err := c.sets.BeginCreateOrUpdate(t.Context(), group, "set", scaleSetBody(nil), nil); code(err) != "InvalidParameter" {
		t.Fatalf("a PUT with a sku and no capacity = %v", err)
	}
	// A PUT without a sku keeps the capacity, and a changed model marks the VMs out of date.
	body := scaleSetBody(nil)
	body.SKU = nil
	body.Properties.VirtualMachineProfile.StorageProfile.ImageReference.Version = new("24.04.202601010")
	up, err := c.sets.BeginCreateOrUpdate(t.Context(), group, "set", body, nil)
	put, err := wait(t, up, err)
	if err != nil || *put.SKU.Capacity != 2 {
		t.Fatalf("put without capacity = %+v, %v", put.SKU, err)
	}
	if vms := c.listScaleSetVMs(t, "set", false); *vms[0].Properties.LatestModelApplied {
		t.Fatalf("instance after a model change = %+v", vms[0].Properties)
	}
	// Lowering the capacity deletes the newest.
	up, err = c.sets.BeginCreateOrUpdate(t.Context(), group, "set", scaleSetBody(new(int64(1))), nil)
	if _, err = wait(t, up, err); err != nil {
		t.Fatal(err)
	}
	got := c.server.ScaleSets()
	if len(got) != 1 || got[0].Capacity != 1 || len(got[0].VMs) != 1 || got[0].VMs[0].InstanceID != "1" ||
		got[0].VMs[0].PublicIPAddresses[0] != "100.64.0.2" {
		t.Fatalf("ScaleSets = %+v", got)
	}

	page, err := c.sets.NewListPager(group, nil).NextPage(t.Context())
	if err != nil || len(page.Value) != 1 {
		t.Fatalf("list = %+v, %v", page.Value, err)
	}
	del, err := c.sets.BeginDelete(t.Context(), group, "set", nil)
	if _, err = wait(t, del, err); err != nil {
		t.Fatal(err)
	}
	if _, err := c.sets.Get(t.Context(), group, "set", nil); code(err) != "ResourceNotFound" {
		t.Fatalf("get after delete = %v", err)
	}
	if got := c.server.Resources(); len(got) != 0 {
		t.Fatalf("resources after delete = %+v", got)
	}
}

func TestScaleSetValidation(t *testing.T) {
	c := setup(t)
	c.createGroup(t, group, nil)
	flexible := scaleSetBody(new(int64(1)))
	flexible.Properties.OrchestrationMode = new(armcompute.OrchestrationModeFlexible)
	if _, err := c.sets.BeginCreateOrUpdate(t.Context(), group, "set", flexible, nil); code(err) != "BadRequest" {
		t.Fatalf("a Flexible scale set = %v", err)
	}
	noSKU := scaleSetBody(nil)
	noSKU.SKU = nil
	if _, err := c.sets.BeginCreateOrUpdate(t.Context(), group, "set", noSKU, nil); code(err) != "InvalidParameter" || !strings.Contains(err.Error(), "'sku'") {
		t.Fatalf("a new scale set with no sku = %v", err)
	}
	if _, err := c.sets.BeginUpdate(t.Context(), group, "nope", armcompute.VirtualMachineScaleSetUpdate{}, nil); code(err) != "ResourceNotFound" {
		t.Fatalf("patching a missing scale set = %v", err)
	}
	if _, err := c.sets.BeginUpdate(t.Context(), group, "nope", armcompute.VirtualMachineScaleSetUpdate{
		Properties: &armcompute.VirtualMachineScaleSetUpdateProperties{Overprovision: new(false)},
	}, nil); code(err) != "BadRequest" {
		t.Fatalf("patching properties = %v", err)
	}
}

func TestScaleSetCreatedWithoutCapacity(t *testing.T) {
	c := setup(t)
	c.createGroup(t, group, nil)
	p, err := c.sets.BeginCreateOrUpdate(t.Context(), group, "set", scaleSetBody(nil), nil)
	created, err := wait(t, p, err)
	if err != nil || *created.SKU.Capacity != 0 || *created.SKU.Name != "Standard_D2s_v5" || *created.Properties.ProvisioningState != "Succeeded" {
		t.Fatalf("a new scale set with no capacity = %+v, %v", created.SKU, err)
	}
	if vms := c.listScaleSetVMs(t, "set", false); len(vms) != 0 {
		t.Fatalf("VMs of a scale set created with no capacity = %v", instanceIDs(vms))
	}
	up, err := c.sets.BeginUpdate(t.Context(), group, "set", armcompute.VirtualMachineScaleSetUpdate{SKU: &armcompute.SKU{Capacity: new(int64(2))}}, nil)
	if _, err = wait(t, up, err); err != nil {
		t.Fatal(err)
	}
	if got := instanceIDs(c.listScaleSetVMs(t, "set", false)); !slices.Equal(got, []string{"0", "1"}) {
		t.Fatalf("after scaling out = %v", got)
	}
	if _, err := c.sets.BeginCreateOrUpdate(t.Context(), group, "set", scaleSetBody(nil), nil); code(err) != "InvalidParameter" || !strings.Contains(err.Error(), "'sku.capacity'") {
		t.Fatalf("a PUT to an existing scale set with a sku and no capacity = %v", err)
	}
}

func TestScaleSetCapacityWithSettle(t *testing.T) {
	c := setup(t, arm.WithSettleAfter(-1))
	c.createGroup(t, group, nil)
	p, err := c.sets.BeginCreateOrUpdate(t.Context(), group, "set", scaleSetBody(new(int64(2))), nil)
	if err != nil || p.Done() {
		t.Fatalf("begin = done %v, %v", p.Done(), err)
	}
	vms := c.listScaleSetVMs(t, "set", false)
	if len(vms) != 2 || *vms[0].Properties.ProvisioningState != "Creating" {
		t.Fatalf("instances in progress = %v", vms)
	}
	pollOnce(t, p)
	c.server.Settle()
	set, err := p.PollUntilDone(t.Context(), nil)
	if err != nil || *set.Properties.ProvisioningState != "Succeeded" {
		t.Fatalf("result = %+v, %v", set.Properties, err)
	}
	u, err := c.sets.BeginUpdate(t.Context(), group, "set", armcompute.VirtualMachineScaleSetUpdate{SKU: &armcompute.SKU{Capacity: new(int64(1))}}, nil)
	if err != nil || u.Done() {
		t.Fatalf("patch = done %v, %v", u.Done(), err)
	}
	vms = c.listScaleSetVMs(t, "set", false)
	if *vms[1].Properties.ProvisioningState != "Deleting" || *vms[0].Properties.ProvisioningState != "Succeeded" {
		t.Fatalf("instances while scaling in = %s %s", *vms[0].Properties.ProvisioningState, *vms[1].Properties.ProvisioningState)
	}
	got, _ := c.sets.Get(t.Context(), group, "set", nil)
	if *got.Properties.ProvisioningState != "Updating" {
		t.Fatalf("scale set while scaling in = %s", *got.Properties.ProvisioningState)
	}
	c.server.Settle()
	if _, err := u.PollUntilDone(t.Context(), nil); err != nil {
		t.Fatal(err)
	}
	if got := instanceIDs(c.listScaleSetVMs(t, "set", false)); !slices.Equal(got, []string{"0"}) {
		t.Fatalf("instances = %v", got)
	}
	d, err := c.sets.BeginDelete(t.Context(), group, "set", nil)
	if err != nil || d.Done() {
		t.Fatalf("delete = done %v, %v", d.Done(), err)
	}
	if vms := c.listScaleSetVMs(t, "set", false); *vms[0].Properties.ProvisioningState != "Deleting" {
		t.Fatalf("instance while deleting = %s", *vms[0].Properties.ProvisioningState)
	}
	if _, err := c.sets.BeginUpdate(t.Context(), group, "set", armcompute.VirtualMachineScaleSetUpdate{}, nil); code(err) != "OperationNotAllowed" {
		t.Fatalf("updating a scale set being deleted = %v", err)
	}
	c.server.Settle()
	if _, err := d.PollUntilDone(t.Context(), nil); err != nil {
		t.Fatal(err)
	}
	if got := c.server.ScaleSets(); len(got) != 0 {
		t.Fatalf("scale sets = %+v", got)
	}
}

func TestScaleSetLostResponses(t *testing.T) {
	faults := fault.New().Add(
		fault.Rule{Method: "Microsoft.Compute/virtualMachineScaleSets/write", Nth: 1, Fault: fault.LoseResponse()},
		fault.Rule{Method: "Microsoft.Compute/virtualMachineScaleSets/delete/action", Nth: 1, Fault: fault.LoseResponse()},
	)
	c := setupRetrying(t, arm.WithFaults(faults))
	c.createGroup(t, group, nil)
	set := c.createScaleSet(t, "set", 3)
	if *set.SKU.Capacity != 3 {
		t.Fatalf("capacity after a retried create = %d", *set.SKU.Capacity)
	}
	// The SDK does not retry a POST whose response was lost; a caller that
	// does finds the instance gone.
	ids := armcompute.VirtualMachineScaleSetVMInstanceRequiredIDs{InstanceIDs: []*string{new("2")}}
	if _, err := c.sets.BeginDeleteInstances(t.Context(), group, "set", ids, nil); err == nil {
		t.Fatal("deleteInstances with a lost response succeeded")
	}
	dp, err := c.sets.BeginDeleteInstances(t.Context(), group, "set", ids, nil)
	if _, err = wait(t, dp, err); code(err) != "InvalidParameter" {
		t.Fatalf("retried deleteInstances = %v", err)
	}
	if got := c.server.ScaleSets(); got[0].Capacity != 2 || len(got[0].VMs) != 2 {
		t.Fatalf("scale set = %+v", got)
	}
}

func TestScaleSetNSGInUse(t *testing.T) {
	c := setup(t)
	c.createGroup(t, group, nil)
	nsg := c.createNSG(t, "nsg")
	body := scaleSetBody(new(int64(1)))
	body.Properties.VirtualMachineProfile.NetworkProfile.NetworkInterfaceConfigurations[0].Properties.NetworkSecurityGroup = &armcompute.SubResource{ID: nsg.ID}
	p, err := c.sets.BeginCreateOrUpdate(t.Context(), group, "set", body, nil)
	if _, err = wait(t, p, err); err != nil {
		t.Fatal(err)
	}
	d, err := c.nsgs.BeginDelete(t.Context(), group, "nsg", nil)
	if _, err = wait(t, d, err); code(err) != "InUseNetworkSecurityGroupCannotBeDeleted" {
		t.Fatalf("deleting an NSG a scale set uses = %v", err)
	}
	got, err := c.nsgs.Get(t.Context(), group, "nsg", nil)
	if err != nil || len(got.Properties.NetworkInterfaces) != 1 {
		t.Fatalf("nsg users = %+v, %v", got.Properties.NetworkInterfaces, err)
	}
}

func TestScaleSetComputerNamesDoNotChange(t *testing.T) {
	c := setup(t)
	c.createGroup(t, group, nil)
	c.createScaleSet(t, "set", 1)
	renamed := scaleSetBody(nil)
	renamed.SKU = nil
	renamed.Properties.VirtualMachineProfile.OSProfile.ComputerNamePrefix = new("other")
	if _, err := c.sets.BeginCreateOrUpdate(t.Context(), group, "set", renamed, nil); code(err) != "PropertyChangeNotAllowed" || status(err) != http.StatusConflict {
		t.Fatalf("changing the computer name prefix = %v", err)
	}
	omitted := scaleSetBody(nil)
	omitted.SKU = nil
	omitted.Properties.VirtualMachineProfile.OSProfile.ComputerNamePrefix = nil
	p, err := c.sets.BeginCreateOrUpdate(t.Context(), group, "set", omitted, nil)
	if _, err = wait(t, p, err); err != nil {
		t.Fatal(err)
	}
	if vms := c.listScaleSetVMs(t, "set", false); len(vms) != 1 || *vms[0].Properties.OSProfile.ComputerName != "node000000" {
		t.Fatalf("VMs after a PUT without a prefix = %+v", vms)
	}
}
