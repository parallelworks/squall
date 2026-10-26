package arm_test

import (
	"net/netip"
	"strings"
	"testing"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore/runtime"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/storagecache/armstoragecache/v3"

	"github.com/parallelworks/squall/access"
	"github.com/parallelworks/squall/azure/arm"
)

func (c *clients) beginFilesystem(t *testing.T, name string) (*runtime.Poller[armstoragecache.AmlFilesystemsClientCreateOrUpdateResponse], error) {
	t.Helper()
	return c.lustre.BeginCreateOrUpdate(t.Context(), group, name, armstoragecache.AmlFilesystem{
		Location: new(location),
		SKU:      &armstoragecache.SKUName{Name: new("AMLFS-Durable-Premium-250")},
		Zones:    []*string{new("1")},
		Properties: &armstoragecache.AmlFilesystemProperties{
			StorageCapacityTiB: new(float32(16)),
			FilesystemSubnet:   new(subnet),
			MaintenanceWindow: &armstoragecache.AmlFilesystemPropertiesMaintenanceWindow{
				DayOfWeek: new(armstoragecache.MaintenanceDayOfWeekTypeFriday), TimeOfDayUTC: new("22:00"),
			},
		},
	}, nil)
}

func TestManagedLustre(t *testing.T) {
	c := setup(t, arm.WithSettleAfter(-1))
	c.createGroup(t, group, nil)
	if _, err := c.cache.CheckAmlFSSubnets(t.Context(), &armstoragecache.ManagementClientCheckAmlFSSubnetsOptions{
		AmlFilesystemSubnetInfo: &armstoragecache.AmlFilesystemSubnetInfo{
			FilesystemSubnet: new(subnet), Location: new(location), SKU: &armstoragecache.SKUName{Name: new("AMLFS-Durable-Premium-250")},
			StorageCapacityTiB: new(float32(16)),
		},
	}); err != nil {
		t.Fatal(err)
	}
	p, err := c.beginFilesystem(t, "fs")
	if err != nil {
		t.Fatal(err)
	}
	pollOnce(t, p)
	creating, err := c.lustre.Get(t.Context(), group, "fs", nil)
	if err != nil || *creating.Properties.ProvisioningState != armstoragecache.AmlFilesystemProvisioningStateTypeCreating ||
		*creating.Properties.ClientInfo.MgsAddress != "" || *creating.Properties.Health.State != armstoragecache.AmlFilesystemHealthStateTypeTransitioning {
		t.Fatalf("a file system being created = %+v, %v", creating.Properties, err)
	}
	c.server.Settle()
	res, err := p.PollUntilDone(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	props := res.Properties
	address, perr := netip.ParseAddr(*props.ClientInfo.MgsAddress)
	if perr != nil || !netip.MustParsePrefix("10.0.0.0/8").Contains(address) ||
		*props.Health.State != armstoragecache.AmlFilesystemHealthStateTypeAvailable || *props.ThroughputProvisionedMBps != 4000 ||
		!strings.HasPrefix(*props.ClientInfo.MountCommand, "mount -t lustre -o noatime,user_xattr "+address.String()+"@tcp0:/lustrefs") {
		t.Fatalf("file system = %+v, client info %+v", props, props.ClientInfo)
	}
	polled := false
	for _, r := range *c.requests {
		if strings.Contains(r, "/providers/Microsoft.StorageCache/locations/"+location+"/ascOperations/") {
			polled = true
		}
	}
	if !polled {
		t.Fatalf("the create was not polled through StorageCache's operations: %v", *c.requests)
	}

	dp, err := c.lustre.BeginDelete(t.Context(), group, "fs", nil)
	if err != nil {
		t.Fatal(err)
	}
	pollOnce(t, dp)
	c.server.Settle()
	if _, err := dp.PollUntilDone(t.Context(), nil); err != nil {
		t.Fatal(err)
	}
	if _, err := c.lustre.Get(t.Context(), group, "fs", nil); code(err) != "ResourceNotFound" {
		t.Fatalf("a deleted file system = %v", err)
	}
}

func TestManagedLustrePollingNeedsAscOperations(t *testing.T) {
	writes := []string{
		"Microsoft.Resources/subscriptions/resourceGroups/write", "Microsoft.StorageCache/amlFilesystems/write",
		"Microsoft.StorageCache/amlFilesystems/read",
	}
	c := setup(t, arm.WithSettleAfter(1), arm.WithPolicy(access.Allow(writes...)))
	c.createGroup(t, group, nil)
	p, err := c.beginFilesystem(t, "fs")
	const poll = "Microsoft.StorageCache/locations/ascOperations/read"
	if _, err = wait(t, p, err); code(err) != "AuthorizationFailed" || !strings.Contains(err.Error(), poll) {
		t.Fatalf("polling without %s = %v", poll, err)
	}
}
