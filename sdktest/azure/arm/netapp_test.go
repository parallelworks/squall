package arm_test

import (
	"fmt"
	"net/http"
	"net/netip"
	"strings"
	"testing"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/netapp/armnetapp/v6"

	"github.com/parallelworks/squall/access"
	"github.com/parallelworks/squall/azure/arm"
	"github.com/parallelworks/squall/fault"
)

const tib = int64(1) << 40

// createVolume creates the NetApp account acct, its pool pool and the pool's volume vol in subnet.
func (c *clients) createVolume(t *testing.T, acct, pool, vol string) (armnetapp.Volume, error) {
	t.Helper()
	ap, err := c.netapps.BeginCreateOrUpdate(t.Context(), group, acct, armnetapp.Account{
		Location: new(location), Properties: &armnetapp.AccountProperties{},
	}, nil)
	if _, err = wait(t, ap, err); err != nil {
		return armnetapp.Volume{}, err
	}
	pp, err := c.pools.BeginCreateOrUpdate(t.Context(), group, acct, pool, armnetapp.CapacityPool{
		Location: new(location),
		Properties: &armnetapp.PoolProperties{
			ServiceLevel: new(armnetapp.ServiceLevelPremium), Size: new(4 * tib), QosType: new(armnetapp.QosTypeAuto),
		},
	}, nil)
	if _, err = wait(t, pp, err); err != nil {
		return armnetapp.Volume{}, err
	}
	vp, err := c.volumes.BeginCreateOrUpdate(t.Context(), group, acct, pool, vol, armnetapp.Volume{
		Location: new(location),
		Properties: &armnetapp.VolumeProperties{
			CreationToken: new(vol), SubnetID: new(subnet), UsageThreshold: new(tib), ServiceLevel: new(armnetapp.ServiceLevelPremium),
			NetworkFeatures: new(armnetapp.NetworkFeaturesStandard), ProtocolTypes: []*string{new("NFSv4.1")},
			ExportPolicy: &armnetapp.VolumePropertiesExportPolicy{Rules: []*armnetapp.ExportPolicyRule{{
				AllowedClients: new("10.0.0.0/16"), Nfsv41: new(true), Nfsv3: new(false), Cifs: new(false), RuleIndex: new(int32(1)),
			}}},
		},
	}, nil)
	res, err := wait(t, vp, err)
	return res.Volume, err
}

func TestNetAppVolumes(t *testing.T) {
	c := setup(t, arm.WithSettleAfter(1))
	c.createGroup(t, group, nil)
	vol, err := c.createVolume(t, "acct", "pool", "vol")
	if err != nil {
		t.Fatal(err)
	}
	if *vol.Name != "acct/pool/vol" || *vol.Properties.ProvisioningState != "Succeeded" || len(vol.Properties.MountTargets) != 1 {
		t.Fatalf("volume = %+v", vol.Properties)
	}
	target := vol.Properties.MountTargets[0]
	address, perr := netip.ParseAddr(*target.IPAddress)
	if perr != nil || !netip.MustParsePrefix("10.0.0.0/8").Contains(address) || *target.FileSystemID != *vol.Properties.FileSystemID {
		t.Fatalf("mount target = %+v", target)
	}
	if got := c.server.Volumes(); len(got) != 1 || got[0].ID != *vol.ID || got[0].MountAddress != address.String() {
		t.Fatalf("Volumes = %+v", got)
	}
	polled := false
	for _, r := range *c.requests {
		if strings.Contains(r, "/providers/Microsoft.NetApp/locations/"+location+"/operationResults/") {
			polled = true
		}
	}
	if !polled {
		t.Fatalf("the creates were not polled through NetApp's operation results: %v", *c.requests)
	}
	pool, err := c.pools.Get(t.Context(), group, "acct", "pool", nil)
	if err != nil || *pool.Name != "acct/pool" || pool.Properties.PoolID == nil {
		t.Fatalf("pool = %+v, %v", pool, err)
	}

	dp, err := c.pools.BeginDelete(t.Context(), group, "acct", "pool", nil)
	if err == nil {
		_, err = dp.PollUntilDone(t.Context(), nil)
	}
	if code(err) != "CannotDeleteResource" || status(err) != http.StatusConflict {
		t.Fatalf("deleting a pool that holds a volume = %v", err)
	}
	vp, err := c.volumes.BeginDelete(t.Context(), group, "acct", "pool", "vol", nil)
	if err != nil {
		t.Fatal(err)
	}
	pollOnce(t, vp)
	if _, err := vp.PollUntilDone(t.Context(), nil); err != nil {
		t.Fatal(err)
	}
	if _, err := c.volumes.Get(t.Context(), group, "acct", "pool", "vol", nil); code(err) != "ResourceNotFound" {
		t.Fatalf("a deleted volume = %v", err)
	}
	if got := c.server.Volumes(); len(got) != 0 {
		t.Fatalf("Volumes after the delete = %+v", got)
	}
}

func TestNetAppCreateInProgress(t *testing.T) {
	c := setup(t, arm.WithSettleAfter(-1))
	c.createGroup(t, group, nil)
	p, err := c.netapps.BeginCreateOrUpdate(t.Context(), group, "acct", armnetapp.Account{Location: new(location)}, nil)
	if err != nil {
		t.Fatal(err)
	}
	pollOnce(t, p)
	got, err := c.netapps.Get(t.Context(), group, "acct", nil)
	if err != nil || *got.Properties.ProvisioningState != "Creating" {
		t.Fatalf("an account being created = %+v, %v", got, err)
	}
	c.server.Settle()
	if _, err := p.PollUntilDone(t.Context(), nil); err != nil {
		t.Fatal(err)
	}
}

func TestNetAppGroupDeletion(t *testing.T) {
	c := setup(t)
	c.createGroup(t, group, nil)
	if _, err := c.createVolume(t, "acct", "pool", "vol"); err != nil {
		t.Fatal(err)
	}
	p, err := c.groups.BeginDelete(t.Context(), group, nil)
	if _, err = wait(t, p, err); err != nil {
		t.Fatal(err)
	}
	if got := c.server.Resources(); len(got) != 0 {
		t.Fatalf("resources after the group's deletion = %+v", got)
	}
}

func TestNetAppPollingNeedsOperationResults(t *testing.T) {
	writes := []string{
		"Microsoft.Resources/subscriptions/resourceGroups/write", "Microsoft.NetApp/netAppAccounts/write",
		"Microsoft.NetApp/netAppAccounts/read",
	}
	c := setup(t, arm.WithSettleAfter(1), arm.WithPolicy(access.Allow(writes...)))
	c.createGroup(t, group, nil)
	p, err := c.netapps.BeginCreateOrUpdate(t.Context(), group, "acct", armnetapp.Account{Location: new(location)}, nil)
	const poll = "Microsoft.NetApp/locations/operationresults/read"
	if _, err = wait(t, p, err); code(err) != "AuthorizationFailed" || !strings.Contains(err.Error(), poll) {
		t.Fatalf("polling without %s = %v", poll, err)
	}
}

func checkNetAppSystemData(t *testing.T, what string, sd *armnetapp.SystemData, created, modified time.Time) {
	t.Helper()
	const caller = "00000000-0000-0000-0000-000000000000"
	app := armnetapp.CreatedByTypeApplication
	if sd == nil || sd.CreatedAt == nil || !sd.CreatedAt.Equal(created) || sd.LastModifiedAt == nil || !sd.LastModifiedAt.Equal(modified) ||
		sd.CreatedBy == nil || *sd.CreatedBy != caller || sd.CreatedByType == nil || *sd.CreatedByType != app ||
		sd.LastModifiedBy == nil || *sd.LastModifiedBy != caller || sd.LastModifiedByType == nil || *sd.LastModifiedByType != app {
		t.Fatalf("%s systemData = %+v, want created at %v and last modified at %v", what, sd, created, modified)
	}
}

func TestNetAppSystemData(t *testing.T) {
	created := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	now := created
	faults := fault.New().Add(fault.Rule{
		Method: "Microsoft.NetApp/netAppAccounts/capacityPools/volumes/write", Nth: 3,
		Fault: fault.Fault{Mode: fault.ModeFailOperation, Status: http.StatusConflict, Reason: "Conflict"},
	})
	c := setup(t, arm.WithClock(func() time.Time { return now }), arm.WithFaults(faults))
	c.createGroup(t, group, nil)
	check := func(acctModified, poolModified, volModified time.Time) {
		t.Helper()
		acct, err := c.netapps.Get(t.Context(), group, "acct", nil)
		if err != nil {
			t.Fatal(err)
		}
		checkNetAppSystemData(t, "account", acct.SystemData, created, acctModified)
		pool, err := c.pools.Get(t.Context(), group, "acct", "pool", nil)
		if err != nil {
			t.Fatal(err)
		}
		checkNetAppSystemData(t, "pool", pool.SystemData, created, poolModified)
		vol, err := c.volumes.Get(t.Context(), group, "acct", "pool", "vol", nil)
		if err != nil {
			t.Fatal(err)
		}
		checkNetAppSystemData(t, "volume", vol.SystemData, created, volModified)
		accts, err := c.netapps.NewListPager(group, nil).NextPage(t.Context())
		if err != nil || len(accts.Value) != 1 {
			t.Fatalf("listed accounts = %+v, %v", accts.Value, err)
		}
		checkNetAppSystemData(t, "listed account", accts.Value[0].SystemData, created, acctModified)
		pools, err := c.pools.NewListPager(group, "acct", nil).NextPage(t.Context())
		if err != nil || len(pools.Value) != 1 {
			t.Fatalf("listed pools = %+v, %v", pools.Value, err)
		}
		checkNetAppSystemData(t, "listed pool", pools.Value[0].SystemData, created, poolModified)
		vols, err := c.volumes.NewListPager(group, "acct", "pool", nil).NextPage(t.Context())
		if err != nil || len(vols.Value) != 1 {
			t.Fatalf("listed volumes = %+v, %v", vols.Value, err)
		}
		checkNetAppSystemData(t, "listed volume", vols.Value[0].SystemData, created, volModified)
	}
	vol, err := c.createVolume(t, "acct", "pool", "vol")
	if err != nil {
		t.Fatal(err)
	}
	checkNetAppSystemData(t, "new volume", vol.SystemData, created, created)
	check(created, created, created)

	updated := created.Add(time.Hour)
	now = updated
	if _, err := c.createVolume(t, "acct", "pool", "vol"); err != nil {
		t.Fatal(err)
	}
	check(updated, updated, updated)

	now = updated.Add(time.Hour)
	if _, err := c.createVolume(t, "acct", "pool", "vol"); code(err) != "Conflict" {
		t.Fatalf("a failed volume update = %v", err)
	}
	check(now, now, updated)
}

func (c *clients) createVolumeSnapshot(t *testing.T, name string) (armnetapp.Snapshot, error) {
	t.Helper()
	p, err := c.volSnaps.BeginCreate(t.Context(), group, "acct", "pool", "vol", name, armnetapp.Snapshot{Location: new(location)}, nil)
	res, err := wait(t, p, err)
	return res.Snapshot, err
}

func TestNetAppSnapshots(t *testing.T) {
	created := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	now := created
	c := setup(t, arm.WithSettleAfter(1), arm.WithClock(func() time.Time { return now }))
	c.createGroup(t, group, nil)
	vol, err := c.createVolume(t, "acct", "pool", "vol")
	if err != nil {
		t.Fatal(err)
	}
	snap, err := c.createVolumeSnapshot(t, "snap")
	if err != nil {
		t.Fatal(err)
	}
	if *snap.ID != *vol.ID+"/snapshots/snap" || *snap.Name != "acct/pool/vol/snap" ||
		*snap.Type != "Microsoft.NetApp/netAppAccounts/capacityPools/volumes/snapshots" || *snap.Location != location {
		t.Fatalf("snapshot = %+v", snap)
	}
	props := snap.Properties
	if props == nil || *props.ProvisioningState != "Succeeded" || props.Created == nil || !props.Created.Equal(created) ||
		props.SnapshotID == nil || len(*props.SnapshotID) != 36 {
		t.Fatalf("snapshot properties = %+v", props)
	}
	checkNetAppSystemData(t, "new snapshot", snap.SystemData, created, created)

	now = created.Add(time.Hour)
	again, err := c.createVolumeSnapshot(t, "snap")
	if err != nil || *again.Properties.SnapshotID != *props.SnapshotID {
		t.Fatalf("creating the snapshot again = %+v, %v", again, err)
	}
	checkNetAppSystemData(t, "snapshot created again", again.SystemData, created, created)

	earlier := created.Add(-time.Hour)
	id, ok := c.server.SnapshotVolume(*vol.ID, "a-old", earlier)
	if !ok || id != *vol.ID+"/snapshots/a-old" {
		t.Fatalf("SnapshotVolume = %q, %v", id, ok)
	}
	if _, ok := c.server.SnapshotVolume(*vol.ID, "a-old", earlier); ok {
		t.Fatal("SnapshotVolume took a name a snapshot has")
	}
	if _, ok := c.server.SnapshotVolume(*vol.ID+"x", "other", earlier); ok {
		t.Fatal("SnapshotVolume snapshotted a missing volume")
	}
	got, err := c.volSnaps.Get(t.Context(), group, "acct", "pool", "vol", "a-old", nil)
	if err != nil || !got.Properties.Created.Equal(earlier) {
		t.Fatalf("a snapshot SnapshotVolume took = %+v, %v", got, err)
	}
	checkNetAppSystemData(t, "snapshot SnapshotVolume took", got.SystemData, earlier, earlier)
	page, err := c.volSnaps.NewListPager(group, "acct", "pool", "vol", nil).NextPage(t.Context())
	if err != nil || len(page.Value) != 2 || *page.Value[0].Name != "acct/pool/vol/a-old" || *page.Value[1].Name != "acct/pool/vol/snap" {
		t.Fatalf("listed snapshots = %+v, %v", page.Value, err)
	}

	dp, err := c.volSnaps.BeginDelete(t.Context(), group, "acct", "pool", "vol", "a-old", nil)
	if _, err = wait(t, dp, err); err != nil {
		t.Fatal(err)
	}
	if _, err := c.volSnaps.Get(t.Context(), group, "acct", "pool", "vol", "a-old", nil); code(err) != "ResourceNotFound" {
		t.Fatalf("a deleted snapshot = %v", err)
	}
	dp, err = c.volSnaps.BeginDelete(t.Context(), group, "acct", "pool", "vol", "a-old", nil)
	if _, err = wait(t, dp, err); err != nil {
		t.Fatalf("deleting a missing snapshot = %v", err)
	}
}

func TestNetAppSnapshotInputs(t *testing.T) {
	c := setup(t)
	c.createGroup(t, group, nil)
	if _, err := c.createVolumeSnapshot(t, "snap"); code(err) != "ParentResourceNotFound" || status(err) != http.StatusNotFound {
		t.Fatalf("a snapshot of a missing volume = %v", err)
	}
	if _, err := c.createVolume(t, "acct", "pool", "vol"); err != nil {
		t.Fatal(err)
	}
	p, err := c.volSnaps.BeginCreate(t.Context(), group, "acct", "pool", "vol", "snap", armnetapp.Snapshot{Location: new(location)}, nil)
	if _, err = wait(t, p, err); err != nil {
		t.Fatal(err)
	}
	if _, err := c.volSnaps.Get(t.Context(), group, "acct", "pool", "other", "snap", nil); code(err) != "ResourceNotFound" {
		t.Fatalf("a snapshot of another volume = %v", err)
	}
}

func TestNetAppVolumeDeletionTakesSnapshots(t *testing.T) {
	for _, force := range []*bool{nil, new(false), new(true)} {
		name := "default"
		if force != nil {
			name = map[bool]string{false: "forceDelete=false", true: "forceDelete=true"}[*force]
		}
		t.Run(name, func(t *testing.T) {
			c := setup(t)
			c.createGroup(t, group, nil)
			vol, err := c.createVolume(t, "acct", "pool", "vol")
			if err != nil {
				t.Fatal(err)
			}
			if _, err := c.createVolumeSnapshot(t, "snap"); err != nil {
				t.Fatal(err)
			}
			if _, ok := c.server.SnapshotVolume(*vol.ID, "other", time.Now()); !ok {
				t.Fatal("SnapshotVolume failed")
			}
			p, err := c.volumes.BeginDelete(t.Context(), group, "acct", "pool", "vol", &armnetapp.VolumesClientBeginDeleteOptions{ForceDelete: force})
			if _, err = wait(t, p, err); err != nil {
				t.Fatal(err)
			}
			for _, r := range c.server.Resources() {
				if strings.Contains(r.ID, "/volumes/") {
					t.Fatalf("left after the volume's deletion: %s", r.ID)
				}
			}
			if _, err := c.volumes.Get(t.Context(), group, "acct", "pool", "vol", nil); code(err) != "ResourceNotFound" {
				t.Fatalf("a deleted volume = %v", err)
			}
			if _, err := c.createVolume(t, "acct", "pool", "vol"); err != nil {
				t.Fatal(err)
			}
			if page, err := c.volSnaps.NewListPager(group, "acct", "pool", "vol", nil).NextPage(t.Context()); err != nil || len(page.Value) != 0 {
				t.Fatalf("snapshots of a volume made again = %+v, %v", page.Value, err)
			}
		})
	}
}

func TestNetAppSnapshotLimit(t *testing.T) {
	c := setup(t)
	c.createGroup(t, group, nil)
	vol, err := c.createVolume(t, "acct", "pool", "vol")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.createVolumeSnapshot(t, "snap"); err != nil {
		t.Fatal(err)
	}
	at := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	for i := range 254 {
		if _, ok := c.server.SnapshotVolume(*vol.ID, fmt.Sprintf("s%03d", i), at); !ok {
			t.Fatalf("SnapshotVolume %d failed", i)
		}
	}
	if _, ok := c.server.SnapshotVolume(*vol.ID, "one-more", at); ok {
		t.Fatal("SnapshotVolume took a 256th snapshot")
	}
	if _, err := c.createVolumeSnapshot(t, "one-more"); code(err) != "SnapshotLimitReached" || status(err) != http.StatusConflict {
		t.Fatalf("a 256th snapshot = %v", err)
	}
	if _, err := c.createVolumeSnapshot(t, "snap"); err != nil {
		t.Fatalf("creating an existing snapshot at the limit = %v", err)
	}
	dp, err := c.volSnaps.BeginDelete(t.Context(), group, "acct", "pool", "vol", "s000", nil)
	if _, err = wait(t, dp, err); err != nil {
		t.Fatal(err)
	}
	if _, err := c.createVolumeSnapshot(t, "one-more"); err != nil {
		t.Fatalf("a snapshot after one is deleted = %v", err)
	}
}

func TestNetAppSnapshotCreatedAgainFinishesAtOnce(t *testing.T) {
	c := setup(t, arm.WithSettleAfter(5))
	c.createGroup(t, group, nil)
	if _, err := c.createVolume(t, "acct", "pool", "vol"); err != nil {
		t.Fatal(err)
	}
	if _, err := c.createVolumeSnapshot(t, "snap"); err != nil {
		t.Fatal(err)
	}
	p, err := c.volSnaps.BeginCreate(t.Context(), group, "acct", "pool", "vol", "snap", armnetapp.Snapshot{Location: new(location)}, nil)
	if err != nil || !p.Done() {
		t.Fatalf("creating an existing snapshot = done %v, %v", p != nil && p.Done(), err)
	}
	got, err := c.volSnaps.Get(t.Context(), group, "acct", "pool", "vol", "snap", nil)
	if err != nil || *got.Properties.ProvisioningState != "Succeeded" {
		t.Fatalf("a snapshot created again = %+v, %v", got.Properties, err)
	}
}

func TestNetAppSnapshotVolumeSettlesTheVolume(t *testing.T) {
	c := setup(t, arm.WithSettleAfter(-1))
	c.createGroup(t, group, nil)
	ap, err := c.netapps.BeginCreateOrUpdate(t.Context(), group, "acct", armnetapp.Account{Location: new(location)}, nil)
	if err != nil {
		t.Fatal(err)
	}
	c.server.Settle()
	if _, err := ap.PollUntilDone(t.Context(), nil); err != nil {
		t.Fatal(err)
	}
	pp, err := c.pools.BeginCreateOrUpdate(t.Context(), group, "acct", "pool", armnetapp.CapacityPool{
		Location:   new(location),
		Properties: &armnetapp.PoolProperties{ServiceLevel: new(armnetapp.ServiceLevelPremium), Size: new(4 * tib)},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	c.server.Settle()
	if _, err := pp.PollUntilDone(t.Context(), nil); err != nil {
		t.Fatal(err)
	}
	vp, err := c.volumes.BeginCreateOrUpdate(t.Context(), group, "acct", "pool", "vol", armnetapp.Volume{
		Location:   new(location),
		Properties: &armnetapp.VolumeProperties{CreationToken: new("vol"), SubnetID: new(subnet), UsageThreshold: new(tib)},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	vols := c.server.Resources()
	id := ""
	for _, r := range vols {
		if strings.HasSuffix(r.ID, "/volumes/vol") {
			id = r.ID
			if r.ProvisioningState != "Creating" {
				t.Fatalf("the volume = %+v, want it Creating", r)
			}
		}
	}
	if _, ok := c.server.SnapshotVolume(id, "snap", time.Now()); !ok {
		t.Fatal("SnapshotVolume failed on a volume being created")
	}
	got, err := c.volumes.Get(t.Context(), group, "acct", "pool", "vol", nil)
	if err != nil || *got.Properties.ProvisioningState != "Succeeded" {
		t.Fatalf("the volume after SnapshotVolume = %+v, %v", got.Properties, err)
	}
	if _, err := vp.PollUntilDone(t.Context(), nil); err != nil {
		t.Fatal(err)
	}
}

func TestNetAppVolumeDeletionFinishesSnapshotCreates(t *testing.T) {
	c := setup(t, arm.WithSettleAfter(5))
	c.createGroup(t, group, nil)
	if _, err := c.createVolume(t, "acct", "pool", "vol"); err != nil {
		t.Fatal(err)
	}
	if _, err := c.volSnaps.BeginCreate(t.Context(), group, "acct", "pool", "vol", "snap", armnetapp.Snapshot{Location: new(location)}, nil); err != nil {
		t.Fatal(err)
	}
	dp, err := c.volumes.BeginDelete(t.Context(), group, "acct", "pool", "vol", nil)
	if err != nil {
		t.Fatal(err)
	}
	got, err := c.volSnaps.Get(t.Context(), group, "acct", "pool", "vol", "snap", nil)
	if err != nil || *got.Properties.ProvisioningState != "Succeeded" {
		t.Fatalf("a snapshot whose create was in progress when its volume's delete began = %+v, %v", got.Properties, err)
	}
	if _, err := dp.PollUntilDone(t.Context(), nil); err != nil {
		t.Fatal(err)
	}
	if _, err := c.volSnaps.Get(t.Context(), group, "acct", "pool", "vol", "snap", nil); code(err) != "ResourceNotFound" {
		t.Fatalf("the snapshot after its volume's deletion = %v", err)
	}
}
