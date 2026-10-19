package arm_test

import (
	"net/http"
	"strings"
	"testing"

	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/compute/armcompute/v8"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/network/armnetwork/v9"

	"github.com/parallelworks/squall/azure/arm"
	"github.com/parallelworks/squall/fault"
)

func vmBody(nicIDs ...*string) armcompute.VirtualMachine {
	var nics []*armcompute.NetworkInterfaceReference
	for i, id := range nicIDs {
		nics = append(nics, &armcompute.NetworkInterfaceReference{
			ID: id,
			Properties: &armcompute.NetworkInterfaceReferenceProperties{
				Primary: new(i == 0), DeleteOption: new(armcompute.DeleteOptionsDelete),
			},
		})
	}
	return armcompute.VirtualMachine{
		Location: new(location),
		Tags:     map[string]*string{"session": new("s")},
		Properties: &armcompute.VirtualMachineProperties{
			HardwareProfile: &armcompute.HardwareProfile{VMSize: new(armcompute.VirtualMachineSizeTypes("Standard_D2s_v5"))},
			StorageProfile: &armcompute.StorageProfile{
				ImageReference: &armcompute.ImageReference{
					Publisher: new("Canonical"), Offer: new("ubuntu-24_04-lts"), SKU: new("server"), Version: new("latest"),
				},
				OSDisk: &armcompute.OSDisk{
					CreateOption: new(armcompute.DiskCreateOptionTypesFromImage),
					ManagedDisk:  &armcompute.ManagedDiskParameters{StorageAccountType: new(armcompute.StorageAccountTypesPremiumLRS)},
					DeleteOption: new(armcompute.DiskDeleteOptionTypesDelete),
				},
			},
			OSProfile: &armcompute.OSProfile{
				AdminUsername: new("azureuser"), AdminPassword: new("secret"), CustomData: new("Y2xvdWQtaW5pdA=="),
			},
			NetworkProfile: &armcompute.NetworkProfile{NetworkInterfaces: nics},
		},
	}
}

func (c *clients) createVM(t *testing.T, name string, nic armnetwork.Interface) armcompute.VirtualMachine {
	t.Helper()
	p, err := c.vms.BeginCreateOrUpdate(t.Context(), group, name, vmBody(nic.ID), nil)
	res, err := wait(t, p, err)
	if err != nil {
		t.Fatal(err)
	}
	return res.VirtualMachine
}

func powerState(statuses []*armcompute.InstanceViewStatus) string {
	for _, s := range statuses {
		if p, ok := strings.CutPrefix(*s.Code, "PowerState/"); ok {
			return p
		}
	}
	return ""
}

func TestVirtualMachines(t *testing.T) {
	c := setup(t)
	c.createGroup(t, group, nil)
	nic := c.createNIC(t, group, "vm-1-nic", nil)
	vm := c.createVM(t, "vm-1", nic)
	props := vm.Properties
	disk := props.StorageProfile.OSDisk
	if *props.ProvisioningState != "Succeeded" || *props.VMID == "" || props.TimeCreated == nil ||
		!strings.HasPrefix(*disk.Name, "vm-1_OsDisk_1_") || *disk.OSType != armcompute.OperatingSystemTypesLinux ||
		*disk.ManagedDisk.ID != "/subscriptions/"+subscription+"/resourceGroups/RG-1/providers/Microsoft.Compute/disks/"+*disk.Name ||
		*props.NetworkProfile.NetworkInterfaces[0].ID != *nic.ID || *props.OSProfile.ComputerName != "vm-1" ||
		props.OSProfile.AdminPassword != nil || props.OSProfile.CustomData != nil || props.InstanceView != nil {
		t.Fatalf("vm = %+v", props)
	}
	if c.polls() != 0 {
		t.Errorf("polled %d times, want 0", c.polls())
	}
	got, err := c.vms.Get(t.Context(), group, "VM-1", &armcompute.VirtualMachinesClientGetOptions{Expand: new(armcompute.InstanceViewTypesInstanceView)})
	if err != nil || powerState(got.Properties.InstanceView.Statuses) != "running" || *got.Properties.InstanceView.ComputerName != "vm-1" {
		t.Fatalf("get with instance view = %+v, %v", got.Properties.InstanceView, err)
	}
	gotNIC, err := c.nics.Get(t.Context(), group, "vm-1-nic", nil)
	if err != nil || *gotNIC.Properties.VirtualMachine.ID != *vm.ID || !*gotNIC.Properties.Primary || gotNIC.Properties.MacAddress == nil {
		t.Fatalf("attached nic = %+v, %v", gotNIC.Properties, err)
	}
	if _, err := c.nics.BeginDelete(t.Context(), group, "vm-1-nic", nil); code(err) != "NicInUse" {
		t.Fatalf("deleting an attached nic = %v", err)
	}
	other := c.createNIC(t, group, "other-nic", nil)
	if _, err := c.vms.BeginCreateOrUpdate(t.Context(), group, "vm-2", vmBody(nic.ID), nil); code(err) != "NicInUse" {
		t.Fatalf("a VM with another's nic = %v", err)
	}
	missing := new(strings.Replace(*nic.ID, "vm-1-nic", "nope", 1))
	if _, err := c.vms.BeginCreateOrUpdate(t.Context(), group, "vm-2", vmBody(missing), nil); code(err) != "InvalidParameter" {
		t.Fatalf("a VM with a missing nic = %v", err)
	}
	noSize := vmBody(other.ID)
	noSize.Properties.HardwareProfile = nil
	if _, err := c.vms.BeginCreateOrUpdate(t.Context(), group, "vm-2", noSize, nil); code(err) != "InvalidParameter" {
		t.Fatalf("a VM with no size = %v", err)
	}

	view, err := c.vms.InstanceView(t.Context(), group, "vm-1", nil)
	if err != nil || powerState(view.Statuses) != "running" {
		t.Fatalf("instance view = %+v, %v", view, err)
	}
	dp, err := c.vms.BeginDeallocate(t.Context(), group, "vm-1", nil)
	if _, err = wait(t, dp, err); err != nil {
		t.Fatal(err)
	}
	if view, _ := c.vms.InstanceView(t.Context(), group, "vm-1", nil); powerState(view.Statuses) != "deallocated" {
		t.Fatalf("after deallocate = %s", powerState(view.Statuses))
	}
	sp, err := c.vms.BeginStart(t.Context(), group, "vm-1", nil)
	if _, err = wait(t, sp, err); err != nil {
		t.Fatal(err)
	}
	if got := c.server.VirtualMachines(); len(got) != 1 || got[0].PowerState != "running" || got[0].Size != "Standard_D2s_v5" {
		t.Fatalf("VirtualMachines = %+v", got)
	}

	// An update keeps what Azure assigned.
	update := vmBody(nic.ID, other.ID)
	update.Properties.OSProfile = nil
	up, err := c.vms.BeginCreateOrUpdate(t.Context(), group, "vm-1", update, nil)
	updated, err := wait(t, up, err)
	if err != nil || *updated.Properties.VMID != *vm.Properties.VMID || *updated.Properties.StorageProfile.OSDisk.Name != *disk.Name ||
		*updated.Properties.OSProfile.ComputerName != "vm-1" || len(updated.Properties.NetworkProfile.NetworkInterfaces) != 2 {
		t.Fatalf("update = %+v, %v", updated.Properties, err)
	}
	renamed := vmBody(nic.ID)
	renamed.Properties.StorageProfile.OSDisk.Name = new("other")
	if _, err := c.vms.BeginCreateOrUpdate(t.Context(), group, "vm-1", renamed, nil); code(err) != "PropertyChangeNotAllowed" || status(err) != http.StatusConflict {
		t.Fatalf("renaming the OS disk = %v", err)
	}

	pager := c.vms.NewListPager(group, &armcompute.VirtualMachinesClientListOptions{Expand: new(armcompute.ExpandTypeForListVMsInstanceView)})
	page, err := pager.NextPage(t.Context())
	if err != nil || len(page.Value) != 1 || page.Value[0].Properties.InstanceView == nil {
		t.Fatalf("list = %+v, %v", page.Value, err)
	}

	del, err := c.vms.BeginDelete(t.Context(), group, "vm-1", nil)
	if _, err = wait(t, del, err); err != nil {
		t.Fatal(err)
	}
	if _, err := c.vms.Get(t.Context(), group, "vm-1", nil); code(err) != "ResourceNotFound" {
		t.Fatalf("get after delete = %v", err)
	}
	// Both interfaces were marked to delete with the VM.
	if got := c.server.NetworkInterfaces(); len(got) != 0 {
		t.Fatalf("interfaces after delete = %+v", got)
	}
	if _, err := c.vms.BeginDeallocate(t.Context(), group, "vm-1", nil); code(err) != "ResourceNotFound" {
		t.Fatalf("deallocating a deleted VM = %v", err)
	}
}

func TestVirtualMachineDeleteDetachesInterfaces(t *testing.T) {
	c := setup(t)
	c.createGroup(t, group, nil)
	nic := c.createNIC(t, group, "nic", nil)
	body := vmBody(nic.ID)
	body.Properties.NetworkProfile.NetworkInterfaces[0].Properties.DeleteOption = nil
	p, err := c.vms.BeginCreateOrUpdate(t.Context(), group, "vm", body, nil)
	if _, err = wait(t, p, err); err != nil {
		t.Fatal(err)
	}
	d, err := c.vms.BeginDelete(t.Context(), group, "vm", nil)
	if _, err = wait(t, d, err); err != nil {
		t.Fatal(err)
	}
	got, err := c.nics.Get(t.Context(), group, "nic", nil)
	if err != nil || got.Properties.VirtualMachine != nil {
		t.Fatalf("detached nic = %+v, %v", got.Properties, err)
	}
}

func TestVirtualMachinePollersWithSettle(t *testing.T) {
	c := setup(t, arm.WithSettleAfter(1))
	c.createGroup(t, group, nil)
	nic := c.createNIC(t, group, "nic", nil)
	c.server.Settle()
	*c.requests = nil
	p, err := c.vms.BeginCreateOrUpdate(t.Context(), group, "vm", vmBody(nic.ID), nil)
	if err != nil || p.Done() {
		t.Fatalf("begin = done %v, %v", p.Done(), err)
	}
	got, err := c.vms.Get(t.Context(), group, "vm", &armcompute.VirtualMachinesClientGetOptions{Expand: new(armcompute.InstanceViewTypesInstanceView)})
	if err != nil || *got.Properties.ProvisioningState != "Creating" || powerState(got.Properties.InstanceView.Statuses) != "starting" {
		t.Fatalf("vm in progress = %+v, %v", got.Properties, err)
	}
	res, err := p.PollUntilDone(t.Context(), nil)
	if err != nil || *res.Properties.ProvisioningState != "Succeeded" {
		t.Fatalf("result = %+v, %v", res.Properties, err)
	}
	if c.polls() != 1 {
		t.Errorf("polled %d times, want 1", c.polls())
	}

	d, err := c.vms.BeginDeallocate(t.Context(), group, "vm", nil)
	if err != nil || d.Done() {
		t.Fatalf("deallocate = done %v, %v", d.Done(), err)
	}
	view, err := c.vms.InstanceView(t.Context(), group, "vm", nil)
	if err != nil || powerState(view.Statuses) != "deallocating" || !strings.Contains(*view.Statuses[0].Code, "updating") {
		t.Fatalf("deallocating = %+v, %v", view.Statuses, err)
	}
	if _, err := d.PollUntilDone(t.Context(), nil); err != nil {
		t.Fatal(err)
	}
	if got := c.server.VirtualMachines(); got[0].PowerState != "deallocated" {
		t.Fatalf("after deallocate = %+v", got)
	}

	del, err := c.vms.BeginDelete(t.Context(), group, "vm", nil)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := c.vms.Get(t.Context(), group, "vm", nil); err != nil || *got.Properties.ProvisioningState != "Deleting" {
		t.Fatalf("vm being deleted = %+v, %v", got.Properties, err)
	}
	// A change to a resource with another in progress finishes that one first.
	if _, err := c.vms.BeginStart(t.Context(), group, "vm", nil); code(err) != "ResourceNotFound" {
		t.Fatalf("starting a VM being deleted = %v", err)
	}
	if _, err := del.PollUntilDone(t.Context(), nil); err != nil {
		t.Fatal(err)
	}
}

func TestVirtualMachineFailedOperation(t *testing.T) {
	faults := fault.New().Add(fault.Rule{
		Method: "Microsoft.Compute/virtualMachines/write", Nth: 1,
		Fault: fault.Fault{Mode: fault.ModeFailOperation, Status: http.StatusConflict, Reason: "AllocationFailed"},
	}, fault.Rule{
		Method: "Microsoft.Compute/virtualMachines/start/action", Fault: fault.FailOperation(http.StatusInternalServerError),
	})
	c := setup(t, arm.WithFaults(faults))
	c.createGroup(t, group, nil)
	nic := c.createNIC(t, group, "nic", nil)
	p, err := c.vms.BeginCreateOrUpdate(t.Context(), group, "vm", vmBody(nic.ID), nil)
	if err != nil || p.Done() {
		t.Fatalf("begin = done %v, %v", p.Done(), err)
	}
	if _, err := p.PollUntilDone(t.Context(), nil); code(err) != "AllocationFailed" {
		t.Fatalf("failed create = %v", err)
	}
	if got := c.server.VirtualMachines(); len(got) != 0 {
		t.Fatalf("a failed create took effect: %+v", got)
	}
	vm := c.createVM(t, "vm", nic)
	sp, err := c.vms.BeginStart(t.Context(), group, "vm", nil)
	if _, err = wait(t, sp, err); code(err) != "InternalServerError" {
		t.Fatalf("failed start = %v", err)
	}
	if got := c.server.VirtualMachines(); got[0].ID != *vm.ID || got[0].ProvisioningState != "Succeeded" {
		t.Fatalf("after a failed start = %+v", got)
	}
	if got := faults.Injected(); len(got) != 2 || got[0].Call.Resource != "/subscriptions/"+subscription+"/resourceGroups/rg-1/providers/Microsoft.Compute/virtualMachines/vm" {
		t.Fatalf("injected = %v", got)
	}
}

func TestVirtualMachineLostResponseRetries(t *testing.T) {
	faults := fault.New().Add(
		fault.Rule{Method: "Microsoft.Compute/virtualMachines/write", Nth: 1, Fault: fault.LoseResponse()},
		fault.Rule{Method: "Microsoft.Compute/virtualMachines/delete", Nth: 1, Fault: fault.LoseResponse()},
		fault.Rule{Method: "Microsoft.Compute/virtualMachines/read", Nth: 1, Fault: fault.Unavailable()},
	)
	c := setupRetrying(t, arm.WithFaults(faults))
	c.createGroup(t, group, nil)
	nic := c.createNIC(t, group, "nic", nil)
	p, err := c.vms.BeginCreateOrUpdate(t.Context(), group, "vm", vmBody(nic.ID), nil)
	res, err := wait(t, p, err)
	if err != nil || *res.Properties.ProvisioningState != "Succeeded" {
		t.Fatalf("create with a lost response = %+v, %v", res, err)
	}
	if _, err := c.vms.Get(t.Context(), group, "vm", nil); err != nil {
		t.Fatalf("get after a 503 = %v", err)
	}
	d, err := c.vms.BeginDelete(t.Context(), group, "vm", nil)
	if _, err = wait(t, d, err); err != nil {
		t.Fatalf("delete with a lost response = %v", err)
	}
	if got := c.server.VirtualMachines(); len(got) != 0 {
		t.Fatalf("vms = %+v", got)
	}
	if got := len(faults.Injected()); got != 3 {
		t.Fatalf("injected %d faults, want 3", got)
	}
}
