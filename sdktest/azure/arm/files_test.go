package arm_test

import (
	"net/http"
	"net/netip"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/network/armnetwork/v9"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/storage/armstorage"

	"github.com/parallelworks/squall/access"
	"github.com/parallelworks/squall/azure/arm"
)

func TestFileShares(t *testing.T) {
	c := setup(t)
	c.createGroup(t, group, nil)
	p, err := c.accounts.BeginCreate(t.Context(), group, "files1", armstorage.AccountCreateParameters{
		SKU: &armstorage.SKU{Name: new(armstorage.SKUNamePremiumLRS)}, Kind: new(armstorage.KindFileStorage), Location: new(location),
	}, nil)
	if _, err = wait(t, p, err); err != nil {
		t.Fatal(err)
	}
	cors := &armstorage.CorsRules{CorsRules: []*armstorage.CorsRule{{
		AllowedHeaders: []*string{new("*")}, AllowedOrigins: []*string{new("*")}, ExposedHeaders: []*string{new("Content-Disposition")},
		AllowedMethods: []*armstorage.CorsRuleAllowedMethodsItem{new(armstorage.CorsRuleAllowedMethodsItemGET)}, MaxAgeInSeconds: new(int32(3600)),
	}}}
	set, err := c.files.SetServiceProperties(t.Context(), group, "files1", armstorage.FileServiceProperties{
		FileServiceProperties: &armstorage.FileServicePropertiesProperties{Cors: cors},
	}, nil)
	if err != nil || len(set.FileServiceProperties.FileServiceProperties.Cors.CorsRules) != 1 || *set.Name != "default" {
		t.Fatalf("set file service = %+v, %v", set, err)
	}
	retention := &armstorage.DeleteRetentionPolicy{Enabled: new(true), Days: new(int32(7))}
	if _, err := c.files.SetServiceProperties(t.Context(), group, "files1", armstorage.FileServiceProperties{
		FileServiceProperties: &armstorage.FileServicePropertiesProperties{ShareDeleteRetentionPolicy: retention},
	}, nil); err != nil {
		t.Fatal(err)
	}
	got, err := c.files.GetServiceProperties(t.Context(), group, "files1", nil)
	if err != nil || len(got.FileServiceProperties.FileServiceProperties.Cors.CorsRules) != 0 ||
		!*got.FileServiceProperties.FileServiceProperties.ShareDeleteRetentionPolicy.Enabled {
		t.Fatalf("setting file service properties replaces them = %+v, %v", got, err)
	}

	if _, err := c.shares.Get(t.Context(), group, "files1", "data", nil); code(err) != "ShareNotFound" || status(err) != http.StatusNotFound {
		t.Fatalf("get of a missing share = %v", err)
	}
	nfs := armstorage.EnabledProtocolsNFS
	share := armstorage.FileShare{FileShareProperties: &armstorage.FileShareProperties{ShareQuota: new(int32(100)), EnabledProtocols: &nfs}}
	made, err := c.shares.Create(t.Context(), group, "files1", "data", share, nil)
	if err != nil || *made.Name != "data" || *made.FileShareProperties.ShareQuota != 100 || *made.FileShareProperties.EnabledProtocols != nfs {
		t.Fatalf("share = %+v, %v", made, err)
	}
	if _, err := c.shares.Create(t.Context(), group, "files1", "data", share, nil); code(err) != "ShareAlreadyExists" {
		t.Fatalf("creating a share twice = %v", err)
	}
	if _, err := c.shares.Create(t.Context(), group, "missing1", "data", share, nil); code(err) != "ParentResourceNotFound" {
		t.Fatalf("a share in a missing account = %v", err)
	}
	if _, err := c.shares.Delete(t.Context(), group, "files1", "data", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := c.shares.Get(t.Context(), group, "files1", "data", nil); code(err) != "ShareNotFound" {
		t.Fatalf("a deleted share = %v", err)
	}
}

func (c *clients) createPrivateEndpoint(t *testing.T, target string) (armnetwork.PrivateEndpoint, error) {
	t.Helper()
	p, err := c.endpoints.BeginCreateOrUpdate(t.Context(), group, "files1-endpoint", armnetwork.PrivateEndpoint{
		Location: new(location),
		Tags:     map[string]*string{"owner": new("a")},
		Properties: &armnetwork.PrivateEndpointProperties{
			Subnet:                     &armnetwork.Subnet{ID: new(subnet)},
			CustomNetworkInterfaceName: new("files1-nic"),
			PrivateLinkServiceConnections: []*armnetwork.PrivateLinkServiceConnection{{
				Name:       new("files1-link"),
				Properties: &armnetwork.PrivateLinkServiceConnectionProperties{PrivateLinkServiceID: new(target), GroupIDs: []*string{new("file")}},
			}},
		},
	}, nil)
	res, err := wait(t, p, err)
	return res.PrivateEndpoint, err
}

func TestPrivateEndpoints(t *testing.T) {
	c := setup(t, arm.WithSettleAfter(1))
	c.createGroup(t, group, nil)
	acct := c.createAccount(t, group, "files1")
	pe, err := c.createPrivateEndpoint(t, *acct.ID)
	if err != nil {
		t.Fatal(err)
	}
	nicID := "/subscriptions/" + subscription + "/resourceGroups/" + group + "/providers/Microsoft.Network/networkInterfaces/files1-nic"
	conn := pe.Properties.PrivateLinkServiceConnections[0].Properties
	if len(pe.Properties.NetworkInterfaces) != 1 || *pe.Properties.NetworkInterfaces[0].ID != nicID ||
		*conn.PrivateLinkServiceConnectionState.Status != "Approved" || *pe.Properties.ProvisioningState != armnetwork.ProvisioningStateSucceeded {
		t.Fatalf("private endpoint = %+v", pe.Properties)
	}
	nic, err := c.nics.Get(t.Context(), group, "files1-nic", nil)
	if err != nil {
		t.Fatal(err)
	}
	address, perr := netip.ParseAddr(*nic.Properties.IPConfigurations[0].Properties.PrivateIPAddress)
	if perr != nil || !netip.MustParsePrefix("10.0.0.0/8").Contains(address) || *nic.Properties.PrivateEndpoint.ID != *pe.ID || len(nic.Tags) != 0 {
		t.Fatalf("the endpoint's interface = %+v", nic.Interface)
	}
	tagged, err := c.nics.UpdateTags(t.Context(), group, "files1-nic", armnetwork.TagsObject{Tags: map[string]*string{"owner": new("a")}}, nil)
	if err != nil || *tagged.Tags["owner"] != "a" || *tagged.Properties.IPConfigurations[0].Properties.PrivateIPAddress != address.String() {
		t.Fatalf("tagging the interface = %+v, %v", tagged, err)
	}
	dp, err := c.endpoints.BeginDelete(t.Context(), group, "files1-endpoint", nil)
	if _, err = wait(t, dp, err); err != nil {
		t.Fatal(err)
	}
	if _, err := c.nics.Get(t.Context(), group, "files1-nic", nil); code(err) != "ResourceNotFound" {
		t.Fatalf("the interface once its endpoint is deleted = %v", err)
	}
}

func TestPrivateEndpointAutoApprovalNeedsTheTargetsApproval(t *testing.T) {
	writes := []string{
		"Microsoft.Resources/subscriptions/resourceGroups/write", "Microsoft.Storage/storageAccounts/write",
		"Microsoft.Network/privateEndpoints/write", "Microsoft.Network/virtualNetworks/subnets/join/action",
	}
	c := setup(t, arm.WithPolicy(access.Allow(writes...)))
	c.createGroup(t, group, nil)
	acct := c.createAccount(t, group, "files1")
	const approval = "Microsoft.Storage/storageAccounts/privateEndpointConnectionsApproval/action"
	if _, err := c.createPrivateEndpoint(t, *acct.ID); code(err) != "LinkedAuthorizationFailed" || !strings.Contains(err.Error(), approval) {
		t.Fatalf("an automatically approved endpoint without %s = %v", approval, err)
	}

	c = setup(t, arm.WithPolicy(access.Allow(append(writes, approval)...)))
	c.createGroup(t, group, nil)
	acct = c.createAccount(t, group, "files1")
	if _, err := c.createPrivateEndpoint(t, *acct.ID); err != nil {
		t.Fatal(err)
	}
}

// shareItems lists the account's shares with opts, as name, then
// "@snapshotTime" for a snapshot or " deleted {version}" for a deleted share.
func (c *clients) shareItems(t *testing.T, account string, opts *armstorage.FileSharesClientListOptions) ([]string, []*armstorage.FileShareItem) {
	t.Helper()
	var names []string
	var items []*armstorage.FileShareItem
	pager := c.shares.NewListPager(group, account, opts)
	for pager.More() {
		page, err := pager.NextPage(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		for _, it := range page.Value {
			s := *it.Name
			if p := it.Properties; p.SnapshotTime != nil {
				s += "@" + p.SnapshotTime.UTC().Format(time.RFC3339Nano)
			} else if p.Deleted != nil && *p.Deleted {
				s += " deleted " + *p.Version
			}
			names = append(names, s)
			items = append(items, it)
		}
	}
	return names, items
}

func TestFileShareSnapshots(t *testing.T) {
	now := time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC)
	c := setup(t, arm.WithClock(func() time.Time { return now }))
	c.createGroup(t, group, nil)
	c.createAccount(t, group, "files1")
	if _, err := c.files.SetServiceProperties(t.Context(), group, "files1", armstorage.FileServiceProperties{
		FileServiceProperties: &armstorage.FileServicePropertiesProperties{
			ShareDeleteRetentionPolicy: &armstorage.DeleteRetentionPolicy{Enabled: new(false)},
		},
	}, nil); err != nil {
		t.Fatal(err)
	}
	snapshots := &armstorage.FileSharesClientCreateOptions{Expand: new("snapshots")}
	if _, err := c.shares.Create(t.Context(), group, "files1", "data", armstorage.FileShare{}, snapshots); code(err) != "ShareNotFound" {
		t.Fatalf("a snapshot of a missing share = %v", err)
	}
	share := armstorage.FileShare{FileShareProperties: &armstorage.FileShareProperties{
		ShareQuota: new(int32(100)), Metadata: map[string]*string{"team": new("a")},
	}}
	if _, err := c.shares.Create(t.Context(), group, "files1", "data", share, nil); err != nil {
		t.Fatal(err)
	}
	id := "/subscriptions/" + subscription + "/resourceGroups/" + group + "/providers/Microsoft.Storage/storageAccounts/files1/fileServices/default/shares/data"
	c.server.SetShareUsage(id, 10)
	snap, err := c.shares.Create(t.Context(), group, "files1", "data", armstorage.FileShare{}, snapshots)
	if err != nil || *snap.ID != id || *snap.Name != "data" || !snap.FileShareProperties.SnapshotTime.Equal(now) ||
		*snap.FileShareProperties.ShareQuota != 100 || *snap.FileShareProperties.Metadata["team"] != "a" {
		t.Fatalf("snapshot = %s, %v", dump(snap), err)
	}
	c.server.SetShareUsage(id, 20)
	tagged := armstorage.FileShare{FileShareProperties: &armstorage.FileShareProperties{Metadata: map[string]*string{"kind": new("nightly")}}}
	second, err := c.shares.Create(t.Context(), group, "files1", "data", tagged, snapshots)
	if err != nil || !second.FileShareProperties.SnapshotTime.Equal(now.Add(100*time.Nanosecond)) ||
		second.FileShareProperties.Metadata["team"] != nil || *second.FileShareProperties.Metadata["kind"] != "nightly" {
		t.Fatalf("a second snapshot at the same time = %s, %v", dump(second), err)
	}
	quota := armstorage.FileShare{FileShareProperties: &armstorage.FileShareProperties{ShareQuota: new(int32(5))}}
	if _, err := c.shares.Create(t.Context(), group, "files1", "data", quota, snapshots); status(err) != http.StatusBadRequest {
		t.Fatalf("a snapshot with a quota = %v", err)
	}
	if _, err := c.shares.Create(t.Context(), group, "files1", "data", share, &armstorage.FileSharesClientCreateOptions{Expand: new("stats")}); status(err) != http.StatusBadRequest {
		t.Fatalf("a create with $expand=stats = %v", err)
	}

	stamp := snap.FileShareProperties.SnapshotTime.Format(time.RFC3339Nano)
	got, err := c.shares.Get(t.Context(), group, "files1", "data", &armstorage.FileSharesClientGetOptions{XMSSnapshot: &stamp, Expand: new("stats")})
	if err != nil || !got.FileShareProperties.SnapshotTime.Equal(now) || *got.FileShareProperties.ShareUsageBytes != 10 {
		t.Fatalf("the snapshot = %s, %v", dump(got), err)
	}
	if got, err := c.shares.Get(t.Context(), group, "files1", "data", &armstorage.FileSharesClientGetOptions{Expand: new("stats")}); err != nil ||
		got.FileShareProperties.SnapshotTime != nil || *got.FileShareProperties.ShareUsageBytes != 20 {
		t.Fatalf("the share = %s, %v", dump(got), err)
	}
	missing := now.Add(time.Hour).Format(time.RFC3339Nano)
	if _, err := c.shares.Get(t.Context(), group, "files1", "data", &armstorage.FileSharesClientGetOptions{XMSSnapshot: &missing}); code(err) != "ShareSnapshotNotFound" || status(err) != http.StatusNotFound {
		t.Fatalf("a missing snapshot = %v", err)
	}
	if _, err := c.shares.Get(t.Context(), group, "files1", "data", &armstorage.FileSharesClientGetOptions{Expand: new("snapshots")}); status(err) != http.StatusBadRequest {
		t.Fatalf("a get with $expand=snapshots = %v", err)
	}

	if _, err := c.shares.Create(t.Context(), group, "files1", "logs", armstorage.FileShare{}, nil); err != nil {
		t.Fatal(err)
	}
	old := now.Add(-time.Hour)
	if !c.server.SnapshotShare(id, old) || c.server.SnapshotShare(id, old) || c.server.SnapshotShare(id+"x", old) {
		t.Fatal("SnapshotShare takes one snapshot of an existing share per time")
	}
	at := func(t time.Time) string { return "data@" + t.Format(time.RFC3339Nano) }
	if names, _ := c.shareItems(t, "files1", nil); !slices.Equal(names, []string{"data", "logs"}) {
		t.Fatalf("shares = %v", names)
	}
	want := []string{"data", at(old), at(now), at(now.Add(100 * time.Nanosecond)), "logs"}
	names, items := c.shareItems(t, "files1", &armstorage.FileSharesClientListOptions{Expand: new("snapshots")})
	if !slices.Equal(names, want) || *items[1].ID != id {
		t.Fatalf("shares with snapshots = %v, %s", names, dump(items))
	}
	if names, _ := c.shareItems(t, "files1", &armstorage.FileSharesClientListOptions{Expand: new("snapshots"), Filter: new("lo")}); !slices.Equal(names, []string{"logs"}) {
		t.Fatalf("shares with a filter = %v", names)
	}
	if _, err := c.shares.NewListPager(group, "files1", &armstorage.FileSharesClientListOptions{Expand: new("stats")}).NextPage(t.Context()); status(err) != http.StatusBadRequest {
		t.Fatalf("a list with $expand=stats = %v", err)
	}

	if _, err := c.shares.Delete(t.Context(), group, "files1", "data", &armstorage.FileSharesClientDeleteOptions{XMSSnapshot: &stamp}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.shares.Get(t.Context(), group, "files1", "data", &armstorage.FileSharesClientGetOptions{XMSSnapshot: &stamp}); code(err) != "ShareSnapshotNotFound" {
		t.Fatalf("a deleted snapshot = %v", err)
	}
	if _, err := c.shares.Delete(t.Context(), group, "files1", "data", &armstorage.FileSharesClientDeleteOptions{Include: new("none")}); code(err) != "ShareHasSnapshots" || status(err) != http.StatusConflict {
		t.Fatalf("deleting a share with snapshots and $include=none = %v", err)
	}
	if _, err := c.shares.Delete(t.Context(), group, "files1", "data", &armstorage.FileSharesClientDeleteOptions{Include: new("all")}); status(err) != http.StatusBadRequest {
		t.Fatalf("deleting with an unknown $include = %v", err)
	}
	if _, err := c.shares.Delete(t.Context(), group, "files1", "logs", &armstorage.FileSharesClientDeleteOptions{Include: new("none")}); err != nil {
		t.Fatalf("deleting a share without snapshots and $include=none = %v", err)
	}
	if _, err := c.shares.Delete(t.Context(), group, "files1", "data", &armstorage.FileSharesClientDeleteOptions{Include: new("leased-snapshots")}); err != nil {
		t.Fatal(err)
	}
	if names, _ := c.shareItems(t, "files1", &armstorage.FileSharesClientListOptions{Expand: new("snapshots,deleted")}); len(names) != 0 {
		t.Fatalf("shares after deleting them with soft delete off = %v", names)
	}

	if _, err := c.shares.Create(t.Context(), group, "files1", "data", armstorage.FileShare{}, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := c.shares.Create(t.Context(), group, "files1", "data", armstorage.FileShare{}, snapshots); err != nil {
		t.Fatal(err)
	}
	if _, err := c.shares.Delete(t.Context(), group, "files1", "data", nil); err != nil {
		t.Fatalf("deleting a share with snapshots by default = %v", err)
	}
}

func TestFileShareSoftDelete(t *testing.T) {
	now := time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC)
	c := setup(t, arm.WithClock(func() time.Time { return now }))
	c.createGroup(t, group, nil)
	c.createAccount(t, group, "files1")
	if _, err := c.files.SetServiceProperties(t.Context(), group, "files1", armstorage.FileServiceProperties{
		FileServiceProperties: &armstorage.FileServicePropertiesProperties{
			ShareDeleteRetentionPolicy: &armstorage.DeleteRetentionPolicy{Enabled: new(true), Days: new(int32(3))},
		},
	}, nil); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"data", "logs"} {
		if _, err := c.shares.Create(t.Context(), group, "files1", name, armstorage.FileShare{}, nil); err != nil {
			t.Fatal(err)
		}
	}
	snapshots := &armstorage.FileSharesClientCreateOptions{Expand: new("snapshots")}
	if _, err := c.shares.Create(t.Context(), group, "files1", "data", armstorage.FileShare{}, snapshots); err != nil {
		t.Fatal(err)
	}
	if _, err := c.shares.Delete(t.Context(), group, "files1", "data", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := c.shares.Get(t.Context(), group, "files1", "data", nil); code(err) != "ShareNotFound" {
		t.Fatalf("a soft-deleted share = %v", err)
	}
	if names, _ := c.shareItems(t, "files1", &armstorage.FileSharesClientListOptions{Expand: new("snapshots")}); !slices.Equal(names, []string{"logs"}) {
		t.Fatalf("shares without deleted = %v", names)
	}
	names, items := c.shareItems(t, "files1", &armstorage.FileSharesClientListOptions{Expand: new("deleted")})
	if len(items) != 2 || names[1] != "logs" {
		t.Fatalf("shares with deleted = %v", names)
	}
	id := "/subscriptions/" + subscription + "/resourceGroups/" + group + "/providers/Microsoft.Storage/storageAccounts/files1/fileServices/default/shares/data"
	d := items[0].Properties
	version := *d.Version
	if !*d.Deleted || !d.DeletedTime.Equal(now) || *d.RemainingRetentionDays != 3 || version == "" || *items[0].ID != id+"_"+version {
		t.Fatalf("the deleted share = %s", dump(items[0]))
	}

	if _, err := c.shares.Create(t.Context(), group, "files1", "data", armstorage.FileShare{}, nil); err != nil {
		t.Fatalf("a share named as a deleted one = %v", err)
	}
	restore := armstorage.DeletedShare{DeletedShareName: new("data"), DeletedShareVersion: &version}
	if _, err := c.shares.Restore(t.Context(), group, "files1", "data", restore, nil); code(err) != "ShareAlreadyExists" {
		t.Fatalf("restoring over a share = %v", err)
	}
	if _, err := c.shares.Delete(t.Context(), group, "files1", "data", nil); err != nil {
		t.Fatal(err)
	}
	if names, _ := c.shareItems(t, "files1", &armstorage.FileSharesClientListOptions{Expand: new("deleted,snapshots")}); len(names) != 3 || !strings.HasPrefix(names[0], "data deleted ") || !strings.HasPrefix(names[1], "data deleted ") {
		t.Fatalf("shares deleted twice = %v", names)
	}
	if _, err := c.shares.Restore(t.Context(), group, "files1", "data", armstorage.DeletedShare{DeletedShareName: new("data"), DeletedShareVersion: new("FFFF")}, nil); code(err) != "ShareNotFound" {
		t.Fatalf("restoring a missing version = %v", err)
	}
	if _, err := c.shares.Restore(t.Context(), group, "files1", "other", restore, nil); status(err) != http.StatusBadRequest {
		t.Fatalf("restoring under another name = %v", err)
	}
	if _, err := c.shares.Restore(t.Context(), group, "files1", "data", restore, nil); err != nil {
		t.Fatal(err)
	}
	names, _ = c.shareItems(t, "files1", &armstorage.FileSharesClientListOptions{Expand: new("snapshots,deleted")})
	if len(names) != 4 || names[0] != "data" || !strings.HasPrefix(names[1], "data@") || !strings.HasPrefix(names[2], "data deleted ") {
		t.Fatalf("shares once one is restored = %v", names)
	}

	logs := strings.TrimSuffix(id, "data") + "logs"
	if _, ok := c.server.SoftDeleteShare(logs, now.Add(-48*time.Hour), 3); !ok {
		t.Fatal("SoftDeleteShare of an existing share")
	}
	if _, ok := c.server.SoftDeleteShare(logs, now, 3); ok {
		t.Fatal("SoftDeleteShare of a deleted share")
	}
	_, items = c.shareItems(t, "files1", &armstorage.FileSharesClientListOptions{Expand: new("deleted"), Filter: new("logs")})
	if len(items) != 1 || *items[0].Properties.RemainingRetentionDays != 1 {
		t.Fatalf("a share soft-deleted two days ago = %s", dump(items))
	}
	now = now.Add(25 * time.Hour)
	if names, _ := c.shareItems(t, "files1", &armstorage.FileSharesClientListOptions{Expand: new("deleted"), Filter: new("logs")}); len(names) != 0 {
		t.Fatalf("a share kept past its retention = %v", names)
	}

	if _, err := c.files.SetServiceProperties(t.Context(), group, "files1", armstorage.FileServiceProperties{
		FileServiceProperties: &armstorage.FileServicePropertiesProperties{
			ShareDeleteRetentionPolicy: &armstorage.DeleteRetentionPolicy{Enabled: new(false)},
		},
	}, nil); err != nil {
		t.Fatal(err)
	}
	if names, _ := c.shareItems(t, "files1", &armstorage.FileSharesClientListOptions{Expand: new("deleted")}); len(names) != 2 {
		t.Fatalf("shares deleted before soft delete was turned off = %v", names)
	}
	dp, err := c.accounts.Delete(t.Context(), group, "files1", nil)
	if err != nil {
		t.Fatalf("deleting an account with snapshots and deleted shares = %v, %v", dp, err)
	}
	c.createAccount(t, group, "files1")
	if names, _ := c.shareItems(t, "files1", &armstorage.FileSharesClientListOptions{Expand: new("deleted,snapshots")}); len(names) != 0 {
		t.Fatalf("shares of an account made again = %v", names)
	}
}

func TestFileShareSoftDeleteIsOnByDefault(t *testing.T) {
	c := setup(t)
	c.createGroup(t, group, nil)
	c.createAccount(t, group, "files1")
	got, err := c.files.GetServiceProperties(t.Context(), group, "files1", nil)
	if p := got.FileServiceProperties.FileServiceProperties.ShareDeleteRetentionPolicy; err != nil || p == nil || !*p.Enabled || *p.Days != 7 {
		t.Fatalf("a new account's file service = %s, %v", dump(got), err)
	}
	for _, name := range []string{"data", "logs"} {
		if _, err := c.shares.Create(t.Context(), group, "files1", name, armstorage.FileShare{}, nil); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := c.shares.Delete(t.Context(), group, "files1", "data", nil); err != nil {
		t.Fatal(err)
	}
	_, items := c.shareItems(t, "files1", &armstorage.FileSharesClientListOptions{Expand: new("deleted")})
	if len(items) != 2 || !*items[0].Properties.Deleted || *items[0].Properties.RemainingRetentionDays != 7 {
		t.Fatalf("a share deleted on a new account = %s", dump(items))
	}
	if _, err := c.files.SetServiceProperties(t.Context(), group, "files1", armstorage.FileServiceProperties{
		FileServiceProperties: &armstorage.FileServicePropertiesProperties{
			ShareDeleteRetentionPolicy: &armstorage.DeleteRetentionPolicy{Enabled: new(false)},
		},
	}, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := c.shares.Delete(t.Context(), group, "files1", "logs", nil); err != nil {
		t.Fatal(err)
	}
	if names, _ := c.shareItems(t, "files1", &armstorage.FileSharesClientListOptions{Expand: new("deleted")}); len(names) != 1 || !strings.HasPrefix(names[0], "data deleted ") {
		t.Fatalf("shares once soft delete is disabled = %v", names)
	}
}

func TestFileShareSnapshotTimes(t *testing.T) {
	now := time.Date(2026, 3, 4, 5, 6, 7, 123456789, time.UTC)
	c := setup(t, arm.WithClock(func() time.Time { return now }))
	c.createGroup(t, group, nil)
	c.createAccount(t, group, "files1")
	share, err := c.shares.Create(t.Context(), group, "files1", "data", armstorage.FileShare{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	snap, err := c.shares.Create(t.Context(), group, "files1", "data", armstorage.FileShare{}, &armstorage.FileSharesClientCreateOptions{Expand: new("snapshots")})
	want := time.Date(2026, 3, 4, 5, 6, 7, 123456700, time.UTC)
	if err != nil || !snap.FileShareProperties.SnapshotTime.Equal(want) {
		t.Fatalf("a snapshot taken at %v = %s, %v", now, dump(snap), err)
	}
	if !c.server.SnapshotShare(*share.ID, now.Add(time.Second)) {
		t.Fatal("SnapshotShare")
	}
	for _, stamp := range []string{"2026-03-04T05:06:07.1234567Z", "2026-03-04T05:06:08.1234567Z"} {
		if got, err := c.shares.Get(t.Context(), group, "files1", "data", &armstorage.FileSharesClientGetOptions{XMSSnapshot: &stamp}); err != nil || got.FileShareProperties.SnapshotTime == nil {
			t.Fatalf("the snapshot at %s = %s, %v", stamp, dump(got), err)
		}
		if _, err := c.shares.Delete(t.Context(), group, "files1", "data", &armstorage.FileSharesClientDeleteOptions{XMSSnapshot: &stamp}); err != nil {
			t.Fatal(err)
		}
		if _, err := c.shares.Get(t.Context(), group, "files1", "data", &armstorage.FileSharesClientGetOptions{XMSSnapshot: &stamp}); code(err) != "ShareSnapshotNotFound" {
			t.Fatalf("the snapshot at %s once deleted = %v", stamp, err)
		}
	}
}
