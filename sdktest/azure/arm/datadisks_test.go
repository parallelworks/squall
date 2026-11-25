package arm_test

import (
	"net/http"
	"testing"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/compute/armcompute/v8"

	"github.com/parallelworks/squall/azure/arm"
	"github.com/parallelworks/squall/fault"
)

const dataGroup = "data"

func (c *clients) dataDisk(t *testing.T, name string) string {
	t.Helper()
	p, err := c.disks.BeginCreateOrUpdate(t.Context(), dataGroup, name, diskBody(location, 16), nil)
	res, err := wait(t, p, err)
	if err != nil {
		t.Fatal(err)
	}
	return *res.ID
}

func attach(lun int32, id string) *armcompute.DataDisk {
	return &armcompute.DataDisk{
		Lun: new(lun), CreateOption: new(armcompute.DiskCreateOptionTypesAttach), ManagedDisk: &armcompute.ManagedDiskParameters{ID: new(id)},
	}
}

func (c *clients) updateDisks(t *testing.T, vm string, disks []*armcompute.DataDisk) (armcompute.VirtualMachine, error) {
	t.Helper()
	got, err := c.vms.Get(t.Context(), group, vm, nil)
	if err != nil {
		t.Fatal(err)
	}
	got.Properties.StorageProfile.DataDisks = disks
	p, err := c.vms.BeginUpdate(t.Context(), group, vm, armcompute.VirtualMachineUpdate{
		Properties: &armcompute.VirtualMachineProperties{StorageProfile: got.Properties.StorageProfile},
	}, nil)
	res, err := wait(t, p, err)
	return res.VirtualMachine, err
}

func (c *clients) holder(t *testing.T, id string) string {
	t.Helper()
	for _, d := range c.server.Disks() {
		if d.ID == id {
			return d.VirtualMachine
		}
	}
	t.Fatalf("no disk %s", id)
	return ""
}

func TestVirtualMachineDataDisks(t *testing.T) {
	var now time.Time
	c := setup(t, arm.WithClock(func() time.Time { return now }))
	now = time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	c.createGroup(t, group, nil)
	c.createGroup(t, dataGroup, nil)
	scratch, logs := c.dataDisk(t, "scratch"), c.dataDisk(t, "logs")
	body := vmBody(c.createNIC(t, group, "nic-1", nil).ID)
	body.Properties.StorageProfile.DataDisks = []*armcompute.DataDisk{attach(0, scratch)}
	p, err := c.vms.BeginCreateOrUpdate(t.Context(), group, "vm-1", body, nil)
	created, err := wait(t, p, err)
	if err != nil {
		t.Fatal(err)
	}
	listed := created.Properties.StorageProfile.DataDisks
	if len(listed) != 1 || *listed[0].Name != "scratch" || *listed[0].Lun != 0 || *listed[0].DiskSizeGB != 16 || *listed[0].ToBeDetached ||
		*listed[0].DeleteOption != armcompute.DiskDeleteOptionTypesDetach || *listed[0].ManagedDisk.ID != scratch {
		t.Fatalf("data disks = %+v", listed)
	}
	disk, err := c.disks.Get(t.Context(), dataGroup, "scratch", nil)
	if err != nil || *disk.ManagedBy != *created.ID || *disk.Properties.DiskState != armcompute.DiskStateAttached ||
		!disk.Properties.LastOwnershipUpdateTime.Equal(now) {
		t.Fatalf("attached disk = %+v, %v", disk, err)
	}
	dp, err := c.disks.BeginDelete(t.Context(), dataGroup, "scratch", nil)
	if _, err = wait(t, dp, err); code(err) != "OperationNotAllowed" || status(err) != http.StatusConflict {
		t.Fatalf("deleting an attached data disk = %v", err)
	}
	other := vmBody(c.createNIC(t, group, "nic-2", nil).ID)
	other.Properties.StorageProfile.DataDisks = []*armcompute.DataDisk{attach(0, scratch)}
	if _, err := c.vms.BeginCreateOrUpdate(t.Context(), group, "vm-2", other, nil); code(err) != "OperationNotAllowed" || status(err) != http.StatusConflict {
		t.Fatalf("attaching a disk another VM holds = %v", err)
	}

	now = now.Add(time.Hour)
	updated, err := c.updateDisks(t, "vm-1", append(listed, attach(1, logs)))
	if err != nil || len(updated.Properties.StorageProfile.DataDisks) != 2 || c.holder(t, logs) != *created.ID {
		t.Fatalf("attaching with PATCH = %+v, %v", updated.Properties.StorageProfile.DataDisks, err)
	}
	now = now.Add(time.Hour)
	updated, err = c.updateDisks(t, "vm-1", updated.Properties.StorageProfile.DataDisks[1:])
	if err != nil || len(updated.Properties.StorageProfile.DataDisks) != 1 || c.holder(t, scratch) != "" || c.holder(t, logs) != *created.ID {
		t.Fatalf("detaching by leaving the disk out = %+v, %v", updated.Properties.StorageProfile.DataDisks, err)
	}
	disk, err = c.disks.Get(t.Context(), dataGroup, "scratch", nil)
	if err != nil || disk.ManagedBy != nil || *disk.Properties.DiskState != armcompute.DiskStateUnattached || !disk.Properties.LastOwnershipUpdateTime.Equal(now) {
		t.Fatalf("detached disk = %+v, %v", disk, err)
	}
	marked := updated.Properties.StorageProfile.DataDisks
	marked[0].ToBeDetached = new(true)
	updated, err = c.updateDisks(t, "vm-1", marked)
	if err != nil || len(updated.Properties.StorageProfile.DataDisks) != 0 || c.holder(t, logs) != "" {
		t.Fatalf("detaching with toBeDetached = %+v, %v", updated.Properties.StorageProfile.DataDisks, err)
	}

	for name, tc := range map[string]struct {
		disks []*armcompute.DataDisk
		code  string
	}{
		"two disks at one lun": {[]*armcompute.DataDisk{attach(2, scratch), attach(2, logs)}, "InvalidParameter"},
		"a lun past 63":        {[]*armcompute.DataDisk{attach(64, scratch)}, "InvalidParameter"},
		"no lun":               {[]*armcompute.DataDisk{{CreateOption: new(armcompute.DiskCreateOptionTypesAttach), ManagedDisk: &armcompute.ManagedDiskParameters{ID: new(scratch)}}}, "InvalidParameter"},
		"an empty disk":        {[]*armcompute.DataDisk{{Lun: new(int32(0)), CreateOption: new(armcompute.DiskCreateOptionTypesEmpty), DiskSizeGB: new(int32(8))}}, "BadRequest"},
	} {
		if _, err := c.updateDisks(t, "vm-1", tc.disks); code(err) != tc.code || status(err) != http.StatusBadRequest {
			t.Errorf("%s = %v, want %s", name, err, tc.code)
		}
	}
	if c.holder(t, scratch) != "" || c.holder(t, logs) != "" {
		t.Fatal("a refused update attached a disk")
	}
}

func TestVirtualMachineUpdate(t *testing.T) {
	c := setup(t, arm.WithSettleAfter(1))
	c.createGroup(t, group, nil)
	c.createGroup(t, dataGroup, nil)
	scratch := c.dataDisk(t, "scratch")
	body := vmBody(c.createNIC(t, group, "nic-1", nil).ID)
	body.Properties.StorageProfile.DataDisks = []*armcompute.DataDisk{attach(0, scratch)}
	p, err := c.vms.BeginCreateOrUpdate(t.Context(), group, "vm-1", body, nil)
	if _, err = wait(t, p, err); err != nil {
		t.Fatal(err)
	}
	up, err := c.vms.BeginUpdate(t.Context(), group, "vm-1", armcompute.VirtualMachineUpdate{Tags: map[string]*string{"team": new("a")}}, nil)
	if err != nil || up.Done() {
		t.Fatalf("an update in progress = %v", err)
	}
	updated, err := up.PollUntilDone(t.Context(), nil)
	if err != nil || *updated.Tags["team"] != "a" || len(updated.Properties.StorageProfile.DataDisks) != 1 || *updated.Properties.ProvisioningState != "Succeeded" {
		t.Fatalf("a tags update = %+v, %v", updated, err)
	}
	if c.holder(t, scratch) != *updated.ID {
		t.Fatal("an update that leaves out dataDisks detached one")
	}

	body.Properties.StorageProfile.DataDisks = nil
	p, err = c.vms.BeginCreateOrUpdate(t.Context(), group, "vm-1", body, nil)
	put, err := wait(t, p, err)
	if err != nil || len(put.Properties.StorageProfile.DataDisks) != 1 || c.holder(t, scratch) != *updated.ID {
		t.Fatalf("a PUT that leaves out dataDisks = %+v, %v", put.Properties.StorageProfile.DataDisks, err)
	}

	resize := armcompute.VirtualMachineUpdate{Properties: &armcompute.VirtualMachineProperties{
		HardwareProfile: &armcompute.HardwareProfile{VMSize: new(armcompute.VirtualMachineSizeTypes("Standard_D4s_v5"))},
	}}
	if _, err := c.vms.BeginUpdate(t.Context(), group, "vm-1", resize, nil); code(err) != "BadRequest" {
		t.Fatalf("a resize with PATCH = %v", err)
	}
	renamed := armcompute.VirtualMachineUpdate{Properties: &armcompute.VirtualMachineProperties{StorageProfile: &armcompute.StorageProfile{
		OSDisk: &armcompute.OSDisk{Name: new("other")},
	}}}
	if _, err := c.vms.BeginUpdate(t.Context(), group, "vm-1", renamed, nil); code(err) != "PropertyChangeNotAllowed" || status(err) != http.StatusConflict {
		t.Fatalf("renaming the OS disk with PATCH = %v", err)
	}
	if _, err := c.vms.BeginUpdate(t.Context(), group, "missing", armcompute.VirtualMachineUpdate{Tags: map[string]*string{}}, nil); code(err) != "ResourceNotFound" {
		t.Fatalf("updating a missing VM = %v", err)
	}
}

func TestVirtualMachineUpdateFaults(t *testing.T) {
	faults := fault.New()
	c := setupRetrying(t, arm.WithFaults(faults))
	c.createGroup(t, group, nil)
	c.createGroup(t, dataGroup, nil)
	scratch := c.dataDisk(t, "scratch")
	body := vmBody(c.createNIC(t, group, "nic-1", nil).ID)
	body.Properties.StorageProfile.DataDisks = []*armcompute.DataDisk{attach(0, scratch)}
	p, err := c.vms.BeginCreateOrUpdate(t.Context(), group, "vm-1", body, nil)
	vm, err := wait(t, p, err)
	if err != nil {
		t.Fatal(err)
	}
	failed := fault.FailOperation(http.StatusConflict)
	failed.Reason = "OperationNotAllowed"
	faults.Add(fault.Rule{Method: "Microsoft.Compute/virtualMachines/write", Times: 1, Fault: failed})
	if _, err := c.updateDisks(t, "vm-1", []*armcompute.DataDisk{}); code(err) != "OperationNotAllowed" || c.holder(t, scratch) != *vm.ID {
		t.Fatalf("a detach whose operation failed = %v, held by %q", err, c.holder(t, scratch))
	}
	refused := fault.FailAfter(http.StatusConflict)
	refused.Reason = "Conflict"
	faults.Add(fault.Rule{Method: "Microsoft.Compute/virtualMachines/write", Times: 1, Fault: refused})
	if _, err := c.updateDisks(t, "vm-1", []*armcompute.DataDisk{}); code(err) != "Conflict" || c.holder(t, scratch) != "" {
		t.Fatalf("a detach that failed after taking effect = %v, held by %q", err, c.holder(t, scratch))
	}
}

func TestDeletingDataDisksWithTheirVM(t *testing.T) {
	c := setup(t)
	c.createGroup(t, group, nil)
	c.createGroup(t, dataGroup, nil)
	kept, gone := c.dataDisk(t, "kept"), c.dataDisk(t, "gone")
	body := vmBody(c.createNIC(t, group, "nic-1", nil).ID)
	deleted := attach(1, gone)
	deleted.DeleteOption = new(armcompute.DiskDeleteOptionTypesDelete)
	body.Properties.StorageProfile.DataDisks = []*armcompute.DataDisk{attach(0, kept), deleted}
	p, err := c.vms.BeginCreateOrUpdate(t.Context(), group, "vm-1", body, nil)
	if _, err = wait(t, p, err); err != nil {
		t.Fatal(err)
	}
	gp, err := c.groups.BeginDelete(t.Context(), dataGroup, nil)
	if _, err = wait(t, gp, err); code(err) != "ResourceGroupDeletionBlocked" {
		t.Fatalf("deleting a group whose disks a VM elsewhere holds = %v", err)
	}
	if len(c.server.Disks()) != 3 {
		t.Fatalf("Disks() after a blocked group delete = %+v", c.server.Disks())
	}
	vp, err := c.vms.BeginDelete(t.Context(), group, "vm-1", nil)
	if _, err = wait(t, vp, err); err != nil {
		t.Fatal(err)
	}
	if disks := c.server.Disks(); len(disks) != 1 || disks[0].ID != kept || disks[0].State != "Unattached" {
		t.Fatalf("Disks() after deleting the VM = %+v", disks)
	}
	gp, err = c.groups.BeginDelete(t.Context(), dataGroup, nil)
	if _, err = wait(t, gp, err); err != nil {
		t.Fatalf("deleting the group once its disks are free = %v", err)
	}
}
