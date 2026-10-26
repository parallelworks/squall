package arm_test

import (
	"net/http"
	"net/netip"
	"strings"
	"testing"

	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/netapp/armnetapp/v6"

	"github.com/parallelworks/squall/access"
	"github.com/parallelworks/squall/azure/arm"
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
