package arm_test

import (
	"net/http"
	"strings"
	"testing"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore/runtime"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/compute/armcompute/v8"

	"github.com/parallelworks/squall/access"
	"github.com/parallelworks/squall/azure/arm"
	"github.com/parallelworks/squall/fault"
)

const (
	// region is where cross-region copies go.
	region        = "westus"
	computePrefix = "/subscriptions/" + subscription + "/resourceGroups/" + group + "/providers/Microsoft.Compute/"
)

func diskBody(loc string, size int32) armcompute.Disk {
	return armcompute.Disk{
		Location: new(loc),
		SKU:      &armcompute.DiskSKU{Name: new(armcompute.DiskStorageAccountTypesPremiumLRS)},
		Properties: &armcompute.DiskProperties{
			CreationData: &armcompute.CreationData{CreateOption: new(armcompute.DiskCreateOptionEmpty)},
			DiskSizeGB:   new(size),
		},
	}
}

// restoreBody is a disk in loc copied from the snapshot source.
func restoreBody(loc, source string) armcompute.Disk {
	return armcompute.Disk{
		Location: new(loc),
		Properties: &armcompute.DiskProperties{
			CreationData: &armcompute.CreationData{CreateOption: new(armcompute.DiskCreateOptionCopy), SourceResourceID: new(source)},
		},
	}
}

func snapshotBody(loc string, option armcompute.DiskCreateOption, source string, incremental bool) armcompute.Snapshot {
	return armcompute.Snapshot{
		Location: new(loc),
		SKU:      &armcompute.SnapshotSKU{Name: new(armcompute.SnapshotStorageAccountTypesStandardLRS)},
		Properties: &armcompute.SnapshotProperties{
			CreationData: &armcompute.CreationData{CreateOption: new(option), SourceResourceID: new(source)},
			Incremental:  new(incremental),
		},
	}
}

func (c *clients) putDisk(t *testing.T, name string, body armcompute.Disk) (armcompute.Disk, error) {
	t.Helper()
	p, err := c.disks.BeginCreateOrUpdate(t.Context(), group, name, body, nil)
	res, err := wait(t, p, err)
	return res.Disk, err
}

func (c *clients) putSnapshot(t *testing.T, name string, body armcompute.Snapshot) (armcompute.Snapshot, error) {
	t.Helper()
	p, err := c.snapshots.BeginCreateOrUpdate(t.Context(), group, name, body, nil)
	res, err := wait(t, p, err)
	return res.Snapshot, err
}

func (c *clients) completion(t *testing.T, name string) float32 {
	t.Helper()
	got, err := c.snapshots.Get(t.Context(), group, name, nil)
	if err != nil {
		t.Fatal(err)
	}
	return *got.Properties.CompletionPercent
}

// settled finishes the operation a Begin call started and polls it to the end.
func settled[T any](t *testing.T, c *clients, p *runtime.Poller[T], err error) (T, error) {
	t.Helper()
	if err != nil {
		var zero T
		return zero, err
	}
	c.server.Settle()
	return p.PollUntilDone(t.Context(), nil)
}

func TestDisksAndSnapshots(t *testing.T) {
	c := setup(t)
	c.createGroup(t, group, nil)
	disk, err := c.putDisk(t, "data", diskBody(location, 10))
	if err != nil {
		t.Fatal(err)
	}
	if *disk.ID != computePrefix+"disks/data" || *disk.Properties.DiskState != armcompute.DiskStateUnattached || *disk.SKU.Tier != "Premium" ||
		*disk.Properties.ProvisioningState != "Succeeded" || *disk.Properties.DiskSizeBytes != 10<<30 || disk.Properties.UniqueID == nil {
		t.Fatalf("disk = %+v %+v", disk.SKU, disk.Properties)
	}
	if c.polls() != 0 {
		t.Errorf("polled %d times, want 0", c.polls())
	}
	snap, err := c.putSnapshot(t, "snap", snapshotBody(location, armcompute.DiskCreateOptionCopy, *disk.ID, true))
	props := snap.Properties
	if err != nil || *props.CreationData.SourceUniqueID != *disk.Properties.UniqueID || *props.DiskSizeGB != 10 || !*props.Incremental ||
		props.IncrementalSnapshotFamilyID == nil || props.CompletionPercent != nil || *props.SnapshotAccessState != armcompute.SnapshotAccessStateAvailable ||
		*snap.SKU.Tier != "Standard" {
		t.Fatalf("snapshot = %+v, %v", props, err)
	}
	if _, err := c.putSnapshot(t, "x", snapshotBody(location, armcompute.DiskCreateOptionCopy, computePrefix+"disks/none", false)); code(err) != "NotFound" || status(err) != http.StatusNotFound {
		t.Fatalf("a snapshot of a missing disk = %v", err)
	}
	if _, err := c.putSnapshot(t, "x", snapshotBody(region, armcompute.DiskCreateOptionCopy, *disk.ID, true)); code(err) != "NotFound" {
		t.Fatalf("a snapshot of a disk in another region = %v", err)
	}
	full, err := c.putSnapshot(t, "full", snapshotBody(location, armcompute.DiskCreateOptionCopy, *disk.ID, false))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.putSnapshot(t, "x", snapshotBody(region, armcompute.DiskCreateOptionCopyStart, *full.ID, true)); code(err) != "InvalidParameter" {
		t.Fatalf("copying a full snapshot = %v", err)
	}
	if _, err := c.putSnapshot(t, "x", snapshotBody(region, armcompute.DiskCreateOptionCopyStart, *snap.ID, false)); code(err) != "InvalidParameter" {
		t.Fatalf("a copy that is not incremental = %v", err)
	}

	// A copy's operation ends before its background copy does.
	copied, err := c.putSnapshot(t, "copy", snapshotBody(region, armcompute.DiskCreateOptionCopyStart, *snap.ID, true))
	if err != nil || *copied.Location != region || *copied.Properties.ProvisioningState != "Succeeded" || *copied.Properties.CompletionPercent != 0 ||
		*copied.Properties.SnapshotAccessState != armcompute.SnapshotAccessStatePending {
		t.Fatalf("copy = %+v, %v", copied.Properties, err)
	}
	if _, err := c.putDisk(t, "restored", restoreBody(region, *copied.ID)); code(err) != "Conflict" || status(err) != http.StatusConflict ||
		!strings.Contains(err.Error(), "copy is still in progress") {
		t.Fatalf("a disk from a copy in progress = %v", err)
	}
	if _, err := c.putSnapshot(t, "copy-2", snapshotBody(region, armcompute.DiskCreateOptionCopyStart, *snap.ID, true)); code(err) != "Conflict" {
		t.Fatalf("a second copy of the disk's snapshots at once = %v", err)
	}
	for _, want := range []float32{50, 100} {
		if got := c.completion(t, "copy"); got != want {
			t.Fatalf("completionPercent = %v, want %v", got, want)
		}
	}
	snaps := c.server.Snapshots()
	if len(snaps) != 3 || snaps[0].ID != *copied.ID || snaps[0].CompletionPercent != 100 || !snaps[0].CopyStart || snaps[0].Source != *snap.ID ||
		snaps[2].CompletionPercent != 100 || snaps[2].Source != *disk.ID {
		t.Fatalf("Snapshots() = %+v", snaps)
	}
	restored, err := c.putDisk(t, "restored", restoreBody(region, *copied.ID))
	if err != nil || *restored.Properties.DiskSizeGB != 10 || *restored.Properties.CreationData.SourceResourceID != *copied.ID ||
		*restored.SKU.Name != armcompute.DiskStorageAccountTypesStandardLRS {
		t.Fatalf("disk from a complete copy = %+v, %v", restored.Properties, err)
	}
	if disks := c.server.Disks(); len(disks) != 2 || disks[1].Source != *copied.ID || disks[1].Location != region || disks[0].SizeGB != 10 {
		t.Fatalf("Disks() = %+v", disks)
	}
	page, err := c.snapshots.NewListByResourceGroupPager(group, nil).NextPage(t.Context())
	if err != nil || len(page.Value) != 3 {
		t.Fatalf("list snapshots = %d, %v", len(page.Value), err)
	}

	dp, err := c.disks.BeginDelete(t.Context(), group, "restored", nil)
	if _, err = wait(t, dp, err); err != nil {
		t.Fatal(err)
	}
	if _, err := c.disks.Get(t.Context(), group, "restored", nil); code(err) != "ResourceNotFound" {
		t.Fatalf("get of a deleted disk = %v", err)
	}
	gp, err := c.groups.BeginDelete(t.Context(), group, nil)
	if _, err = wait(t, gp, err); err != nil {
		t.Fatal(err)
	}
	if left := c.server.Resources(); len(left) != 0 {
		t.Fatalf("resources left = %+v", left)
	}
}

func TestVirtualMachineOSDisks(t *testing.T) {
	c := setup(t)
	c.createGroup(t, group, nil)
	vm := c.createVM(t, "vm-1", c.createNIC(t, group, "nic-1", nil))
	osDisk := vm.Properties.StorageProfile.OSDisk
	disk, err := c.disks.Get(t.Context(), group, *osDisk.Name, nil)
	if err != nil || *disk.ID != *osDisk.ManagedDisk.ID || *disk.Properties.DiskState != armcompute.DiskStateAttached || *disk.ManagedBy != *vm.ID ||
		*disk.Properties.OSType != armcompute.OperatingSystemTypesLinux || *disk.SKU.Name != armcompute.DiskStorageAccountTypesPremiumLRS ||
		*disk.Properties.CreationData.CreateOption != armcompute.DiskCreateOptionFromImage {
		t.Fatalf("OS disk = %+v, %v", disk, err)
	}
	dp, err := c.disks.BeginDelete(t.Context(), group, *osDisk.Name, nil)
	if _, err = wait(t, dp, err); code(err) != "OperationNotAllowed" || status(err) != http.StatusConflict || !strings.Contains(err.Error(), "is attached to VM "+*vm.ID) {
		t.Fatalf("deleting an attached disk = %v", err)
	}
	if _, err := c.putSnapshot(t, "os", snapshotBody(location, armcompute.DiskCreateOptionCopy, *disk.ID, true)); err != nil {
		t.Fatalf("a snapshot of an attached disk = %v", err)
	}
	pp, err := c.vms.BeginDeallocate(t.Context(), group, "vm-1", nil)
	if _, err = wait(t, pp, err); err != nil {
		t.Fatal(err)
	}
	if disks := c.server.Disks(); len(disks) != 1 || disks[0].State != "Reserved" || disks[0].VirtualMachine != *vm.ID {
		t.Fatalf("Disks() of a deallocated VM = %+v", disks)
	}
	vp, err := c.vms.BeginDelete(t.Context(), group, "vm-1", nil)
	if _, err = wait(t, vp, err); err != nil {
		t.Fatal(err)
	}
	if _, err := c.disks.Get(t.Context(), group, *osDisk.Name, nil); code(err) != "ResourceNotFound" {
		t.Fatalf("an OS disk whose deleteOption is Delete, after its VM = %v", err)
	}

	body := vmBody(c.createNIC(t, group, "nic-2", nil).ID)
	body.Properties.StorageProfile.OSDisk.DeleteOption = new(armcompute.DiskDeleteOptionTypesDetach)
	cp, err := c.vms.BeginCreateOrUpdate(t.Context(), group, "vm-2", body, nil)
	if _, err = wait(t, cp, err); err != nil {
		t.Fatal(err)
	}
	vp, err = c.vms.BeginDelete(t.Context(), group, "vm-2", nil)
	if _, err = wait(t, vp, err); err != nil {
		t.Fatal(err)
	}
	if disks := c.server.Disks(); len(disks) != 1 || disks[0].State != "Unattached" || disks[0].VirtualMachine != "" {
		t.Fatalf("Disks() after deleting a VM that detaches its OS disk = %+v", disks)
	}
}

func TestCopyStartCompletesOverSettles(t *testing.T) {
	c := setup(t, arm.WithSettleAfter(-1))
	c.createGroup(t, group, nil)
	c.server.Settle()
	p, err := c.disks.BeginCreateOrUpdate(t.Context(), group, "data", diskBody(location, 10), nil)
	if _, err = settled(t, c, p, err); err != nil {
		t.Fatal(err)
	}
	sp, err := c.snapshots.BeginCreateOrUpdate(t.Context(), group, "snap", snapshotBody(location, armcompute.DiskCreateOptionCopy, computePrefix+"disks/data", true), nil)
	snap, err := settled(t, c, sp, err)
	if err != nil {
		t.Fatal(err)
	}
	cp, err := c.snapshots.BeginCreateOrUpdate(t.Context(), group, "copy", snapshotBody(region, armcompute.DiskCreateOptionCopyStart, *snap.ID, true), nil)
	if err != nil {
		t.Fatal(err)
	}
	pollOnce(t, cp)
	if got, err := c.snapshots.Get(t.Context(), group, "copy", nil); err != nil || *got.Properties.ProvisioningState != "Updating" {
		t.Fatalf("a copy being created = %+v, %v", got.Properties, err)
	}
	c.server.Settle()
	copied, err := cp.PollUntilDone(t.Context(), nil)
	if err != nil || *copied.Properties.CompletionPercent != 0 {
		t.Fatalf("copy = %+v, %v", copied.Properties, err)
	}
	// Reads do not advance a copy when only Settle finishes changes.
	if got := c.completion(t, "copy"); got != 0 {
		t.Fatalf("completionPercent after a read = %v", got)
	}
	for _, want := range []float32{50, 100} {
		c.server.Settle()
		if got := c.completion(t, "copy"); got != want {
			t.Fatalf("completionPercent = %v, want %v", got, want)
		}
	}

	// Deleting the source of a copy in progress ends the copy.
	cp, err = c.snapshots.BeginCreateOrUpdate(t.Context(), group, "copy-2", snapshotBody(region, armcompute.DiskCreateOptionCopyStart, *snap.ID, true), nil)
	if _, err = settled(t, c, cp, err); err != nil {
		t.Fatal(err)
	}
	dp, err := c.snapshots.BeginDelete(t.Context(), group, "snap", nil)
	if _, err = settled(t, c, dp, err); err != nil {
		t.Fatal(err)
	}
	c.server.Settle()
	got, err := c.snapshots.Get(t.Context(), group, "copy-2", nil)
	if err != nil || *got.Properties.CompletionPercent != 0 || got.Properties.CopyCompletionError == nil ||
		*got.Properties.CopyCompletionError.ErrorCode != armcompute.CopyCompletionErrorReasonCopySourceNotFound {
		t.Fatalf("a copy whose source was deleted = %+v, %v", got.Properties, err)
	}
	if _, err := c.putDisk(t, "restored", restoreBody(region, *got.ID)); code(err) != "Conflict" {
		t.Fatalf("a disk from a failed copy = %v", err)
	}
}

func versionBody(source string) armcompute.GalleryImageVersion {
	return armcompute.GalleryImageVersion{
		Location: new(location),
		Properties: &armcompute.GalleryImageVersionProperties{
			StorageProfile: &armcompute.GalleryImageVersionStorageProfile{
				OSDiskImage: &armcompute.GalleryOSDiskImage{Source: &armcompute.GalleryDiskImageSource{ID: new(source)}},
			},
			PublishingProfile: &armcompute.GalleryImageVersionPublishingProfile{
				TargetRegions: []*armcompute.TargetRegion{{Name: new(location)}},
			},
		},
	}
}

func imageBody(osType armcompute.OperatingSystemTypes) armcompute.GalleryImage {
	return armcompute.GalleryImage{
		Location: new(location),
		Properties: &armcompute.GalleryImageProperties{
			OSType: new(osType), OSState: new(armcompute.OperatingSystemStateTypesGeneralized),
			HyperVGeneration: new(armcompute.HyperVGenerationV2),
			Identifier:       &armcompute.GalleryImageIdentifier{Publisher: new("squall"), Offer: new("test"), SKU: new("v2")},
		},
	}
}

func TestGalleryImageVersions(t *testing.T) {
	c := setup(t, arm.WithSettleAfter(-1))
	c.createGroup(t, group, nil)
	c.server.Settle()
	gp, err := c.galleries.BeginCreateOrUpdate(t.Context(), group, "gallery", armcompute.Gallery{Location: new(location)}, nil)
	gallery, err := settled(t, c, gp, err)
	if err != nil || *gallery.Properties.Identifier.UniqueName != subscription+"-GALLERY" {
		t.Fatalf("gallery = %+v, %v", gallery.Properties, err)
	}
	ip, err := c.images.BeginCreateOrUpdate(t.Context(), group, "gallery", "image", imageBody(armcompute.OperatingSystemTypesLinux), nil)
	image, err := settled(t, c, ip, err)
	if err != nil || *image.Properties.Architecture != armcompute.ArchitectureX64 || *image.Type != "Microsoft.Compute/galleries/images" {
		t.Fatalf("image = %+v, %v", image.Properties, err)
	}
	if _, err := c.images.BeginCreateOrUpdate(t.Context(), group, "gallery", "image", imageBody(armcompute.OperatingSystemTypesWindows), nil); code(err) != "PropertyChangeNotAllowed" {
		t.Fatalf("changing an image's osType = %v", err)
	}

	// The source disk is in another region, and its snapshot is copied to the gallery's.
	p, err := c.disks.BeginCreateOrUpdate(t.Context(), group, "data", diskBody(region, 10), nil)
	if _, err = settled(t, c, p, err); err != nil {
		t.Fatal(err)
	}
	sp, err := c.snapshots.BeginCreateOrUpdate(t.Context(), group, "snap", snapshotBody(region, armcompute.DiskCreateOptionCopy, computePrefix+"disks/data", true), nil)
	snap, err := settled(t, c, sp, err)
	if err != nil {
		t.Fatal(err)
	}
	sp, err = c.snapshots.BeginCreateOrUpdate(t.Context(), group, "copy", snapshotBody(location, armcompute.DiskCreateOptionCopyStart, *snap.ID, true), nil)
	copied, err := settled(t, c, sp, err)
	if err != nil {
		t.Fatal(err)
	}

	// A version from a copy in progress fails, and resuming its poller reads why.
	vp, err := c.versions.BeginCreateOrUpdate(t.Context(), group, "gallery", "image", "1.0.0", versionBody(*copied.ID), nil)
	if err != nil {
		t.Fatal(err)
	}
	token, err := vp.ResumeToken()
	if err != nil {
		t.Fatal(err)
	}
	c.server.Settle()
	resumed, err := c.versions.BeginCreateOrUpdate(t.Context(), group, "gallery", "image", "1.0.0", armcompute.GalleryImageVersion{},
		&armcompute.GalleryImageVersionsClientBeginCreateOrUpdateOptions{ResumeToken: token})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := resumed.PollUntilDone(t.Context(), nil); code(err) != "Conflict" || !strings.Contains(err.Error(), "copy is still in progress") {
		t.Fatalf("a version from a copy in progress = %v", err)
	}
	expand := &armcompute.GalleryImageVersionsClientGetOptions{Expand: new(armcompute.ReplicationStatusTypesReplicationStatus)}
	failed, err := c.versions.Get(t.Context(), group, "gallery", "image", "1.0.0", expand)
	if err != nil || *failed.Properties.ProvisioningState != "Failed" ||
		*failed.Properties.ReplicationStatus.AggregatedState != armcompute.AggregatedReplicationStateFailed ||
		*failed.Properties.ReplicationStatus.Summary[0].State != armcompute.ReplicationStateFailed ||
		!strings.Contains(*failed.Properties.ReplicationStatus.Summary[0].Details, "copy is still in progress") {
		t.Fatalf("failed version = %+v, %v", failed.Properties, err)
	}

	c.server.Settle()
	if got := c.completion(t, "copy"); got != 100 {
		t.Fatalf("completionPercent = %v", got)
	}
	vp, err = c.versions.BeginCreateOrUpdate(t.Context(), group, "gallery", "image", "1.0.1", versionBody(*copied.ID), nil)
	if err != nil {
		t.Fatal(err)
	}
	building, err := c.versions.Get(t.Context(), group, "gallery", "image", "1.0.1", expand)
	if err != nil || *building.Properties.ProvisioningState != "Creating" ||
		*building.Properties.ReplicationStatus.AggregatedState != armcompute.AggregatedReplicationStateInProgress ||
		*building.Properties.ReplicationStatus.Summary[0].State != armcompute.ReplicationStateReplicating {
		t.Fatalf("version being built = %+v, %v", building.Properties, err)
	}
	c.server.Settle()
	if _, err := vp.PollUntilDone(t.Context(), nil); err != nil {
		t.Fatal(err)
	}
	built, err := c.versions.Get(t.Context(), group, "gallery", "image", "1.0.1", expand)
	if err != nil || *built.Properties.ProvisioningState != "Succeeded" ||
		*built.Properties.ReplicationStatus.AggregatedState != armcompute.AggregatedReplicationStateCompleted ||
		*built.Properties.ReplicationStatus.Summary[0].Progress != 100 || *built.Properties.StorageProfile.OSDiskImage.SizeInGB != 10 ||
		*built.Properties.PublishingProfile.ReplicaCount != 1 ||
		*built.Properties.PublishingProfile.TargetRegions[0].StorageAccountType != armcompute.StorageAccountTypeStandardLRS {
		t.Fatalf("built version = %+v, %v", built.Properties, err)
	}
	if plain, err := c.versions.Get(t.Context(), group, "gallery", "image", "1.0.1", nil); err != nil || plain.Properties.ReplicationStatus != nil {
		t.Fatalf("version without $expand = %+v, %v", plain.Properties, err)
	}
	versions := c.server.GalleryImageVersions()
	if len(versions) != 2 || versions[0].ProvisioningState != "Failed" || !strings.Contains(versions[0].Error, "copy is still in progress") ||
		versions[1].ProvisioningState != "Succeeded" || versions[1].Sources[0] != *copied.ID {
		t.Fatalf("GalleryImageVersions() = %+v", versions)
	}

	for name, source := range map[string]string{
		"a missing snapshot":           computePrefix + "snapshots/none",
		"a snapshot in another region": *snap.ID,
	} {
		if _, err := c.versions.BeginCreateOrUpdate(t.Context(), group, "gallery", "image", "1.0.2", versionBody(source), nil); code(err) != "InvalidParameter" ||
			status(err) != http.StatusBadRequest || !strings.Contains(err.Error(), "is not found") {
			t.Errorf("a version from %s = %v", name, err)
		}
	}
	if _, err := c.versions.BeginCreateOrUpdate(t.Context(), group, "gallery", "image", "latest", versionBody(*copied.ID), nil); code(err) != "InvalidParameter" {
		t.Fatalf("a version named latest = %v", err)
	}
	if _, err := c.images.BeginDelete(t.Context(), group, "gallery", "image", nil); code(err) != "CannotDeleteResource" || status(err) != http.StatusConflict {
		t.Fatalf("deleting an image that holds versions = %v", err)
	}
	if _, err := c.galleries.BeginDelete(t.Context(), group, "gallery", nil); code(err) != "CannotDeleteResource" {
		t.Fatalf("deleting a gallery that holds images = %v", err)
	}
	page, err := c.versions.NewListByGalleryImagePager(group, "gallery", "image", nil).NextPage(t.Context())
	if err != nil || len(page.Value) != 2 {
		t.Fatalf("list versions = %d, %v", len(page.Value), err)
	}

	dp, err := c.groups.BeginDelete(t.Context(), group, nil)
	if _, err = settled(t, c, dp, err); err != nil {
		t.Fatal(err)
	}
	if left := c.server.Resources(); len(left) != 0 {
		t.Fatalf("resources left = %+v", left)
	}
}

func TestSnapshotWriteFailsAsItsOperation(t *testing.T) {
	faults := fault.New().Add(fault.Rule{Method: "Microsoft.Compute/snapshots/write", Fault: fault.FailOperation(http.StatusConflict)})
	c := setup(t, arm.WithFaults(faults))
	c.createGroup(t, group, nil)
	disk, err := c.putDisk(t, "data", diskBody(location, 10))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.putSnapshot(t, "snap", snapshotBody(location, armcompute.DiskCreateOptionCopy, *disk.ID, true)); code(err) != "Conflict" {
		t.Fatalf("a failed snapshot = %v", err)
	}
	if snaps := c.server.Snapshots(); len(snaps) != 0 {
		t.Fatalf("Snapshots() after a failed write = %+v", snaps)
	}
}

func TestPolicyChecksSnapshotSources(t *testing.T) {
	actions := []string{
		"Microsoft.Resources/subscriptions/resourceGroups/write",
		"Microsoft.Compute/disks/write",
		"Microsoft.Compute/snapshots/write",
	}
	c := setup(t, arm.WithPolicy(access.Allow(actions...)))
	c.createGroup(t, group, nil)
	disk, err := c.putDisk(t, "data", diskBody(location, 10))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.putSnapshot(t, "snap", snapshotBody(location, armcompute.DiskCreateOptionCopy, *disk.ID, true)); code(err) != "LinkedAuthorizationFailed" ||
		!strings.Contains(err.Error(), "'Microsoft.Compute/disks/beginGetAccess/action' on the linked scope(s) '"+*disk.ID+"'") {
		t.Fatalf("a snapshot without access to its disk = %v", err)
	}
	if _, err := c.disks.Get(t.Context(), group, "data", nil); code(err) != "AuthorizationFailed" || !strings.Contains(err.Error(), "Microsoft.Compute/disks/read") {
		t.Fatalf("reading a disk without disks/read = %v", err)
	}

	c = setup(t, arm.WithPolicy(access.Allow(append(actions, "Microsoft.Compute/disks/beginGetAccess/action")...)))
	c.createGroup(t, group, nil)
	if disk, err = c.putDisk(t, "data", diskBody(location, 10)); err != nil {
		t.Fatal(err)
	}
	if _, err := c.putSnapshot(t, "snap", snapshotBody(location, armcompute.DiskCreateOptionCopy, *disk.ID, true)); err != nil {
		t.Fatal(err)
	}
	if _, err := c.galleries.BeginCreateOrUpdate(t.Context(), group, "gallery", armcompute.Gallery{Location: new(location)}, nil); code(err) != "AuthorizationFailed" ||
		!strings.Contains(err.Error(), "Microsoft.Compute/galleries/write") {
		t.Fatalf("a gallery without galleries/write = %v", err)
	}
}

func TestCopyStepsSetsHowLongACopyRuns(t *testing.T) {
	c := setup(t, arm.WithSettleAfter(-1), arm.WithCopySteps(3))
	c.createGroup(t, group, nil)
	c.server.Settle()
	p, err := c.disks.BeginCreateOrUpdate(t.Context(), group, "data", diskBody(location, 10), nil)
	if _, err = settled(t, c, p, err); err != nil {
		t.Fatal(err)
	}
	sp, err := c.snapshots.BeginCreateOrUpdate(t.Context(), group, "snap", snapshotBody(location, armcompute.DiskCreateOptionCopy, computePrefix+"disks/data", true), nil)
	snap, err := settled(t, c, sp, err)
	if err != nil {
		t.Fatal(err)
	}
	cp, err := c.snapshots.BeginCreateOrUpdate(t.Context(), group, "copy", snapshotBody(region, armcompute.DiskCreateOptionCopyStart, *snap.ID, true), nil)
	if _, err = settled(t, c, cp, err); err != nil {
		t.Fatal(err)
	}
	for _, want := range []float32{34, 68, 100} {
		c.server.Settle()
		if got := c.completion(t, "copy"); got != want {
			t.Fatalf("completionPercent = %v, want %v", got, want)
		}
	}
}
