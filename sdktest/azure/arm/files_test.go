package arm_test

import (
	"net/http"
	"net/netip"
	"strings"
	"testing"

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
