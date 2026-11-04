package arm_test

import (
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/privatedns/armprivatedns"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/resources/armresources/v3"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/storage/armstorage"

	"github.com/parallelworks/squall/access"
	"github.com/parallelworks/squall/azure/arm"
)

func (c *clients) groupResources(t *testing.T, opts *armresources.ClientListByResourceGroupOptions) []*armresources.GenericResourceExpanded {
	t.Helper()
	var out []*armresources.GenericResourceExpanded
	pager := c.resources.NewListByResourceGroupPager(group, opts)
	for pager.More() {
		page, err := pager.NextPage(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, page.Value...)
	}
	return out
}

func names(resources []*armresources.GenericResourceExpanded) []string {
	var out []string
	for _, r := range resources {
		out = append(out, *r.Type+" "+*r.Name)
	}
	return out
}

func TestListGroupResources(t *testing.T) {
	c := setup(t)
	if _, err := c.resources.NewListByResourceGroupPager(group, nil).NextPage(t.Context()); code(err) != "ResourceGroupNotFound" || status(err) != http.StatusNotFound {
		t.Fatalf("a missing group = %v", err)
	}
	c.createGroup(t, group, nil)
	if got := c.groupResources(t, nil); len(got) != 0 {
		t.Fatalf("an empty group = %v", names(got))
	}
	c.createGroup(t, "other", nil)
	c.createNIC(t, "other", "elsewhere", nil)
	vnet := c.createVNet(t, "vnet", map[string]string{"default": "10.1.0.0/24"})
	c.createNSG(t, "nsg")
	c.createZone(t, "example.internal")
	c.putRecord(t, "example.internal", "www", "10.1.0.4")
	lp, err := c.links.BeginCreateOrUpdate(t.Context(), group, "example.internal", "link", armprivatedns.VirtualNetworkLink{
		Location:   new("global"),
		Properties: &armprivatedns.VirtualNetworkLinkProperties{VirtualNetwork: &armprivatedns.SubResource{ID: vnet.ID}, RegistrationEnabled: new(false)},
	}, nil)
	if _, err := wait(t, lp, err); err != nil {
		t.Fatal(err)
	}
	acct := c.createAccount(t, group, "store1")
	if _, err := c.containers.Create(t.Context(), group, "store1", "data", armstorage.BlobContainer{}, nil); err != nil {
		t.Fatal(err)
	}
	got := c.groupResources(t, nil)
	want := []string{
		"Microsoft.Network/networkSecurityGroups nsg",
		"Microsoft.Network/privateDnsZones example.internal",
		"Microsoft.Network/privateDnsZones/virtualNetworkLinks example.internal/link",
		"Microsoft.Network/virtualNetworks vnet",
		"Microsoft.Storage/storageAccounts store1",
	}
	if !slices.Equal(names(got), want) {
		t.Fatalf("resources = %v, want %v", names(got), want)
	}
	for _, r := range got {
		if !strings.HasPrefix(*r.ID, "/subscriptions/"+subscription+"/resourceGroups/"+group+"/providers/") || r.Location == nil {
			t.Errorf("resource = %+v", r)
		}
		if *r.ID == *acct.ID && (*r.Kind != "StorageV2" || *r.SKU.Name != "Standard_LRS" || *r.Tags["session-id"] != "s-1" || r.ProvisioningState != nil) {
			t.Errorf("storage account = %+v", r)
		}
	}
	expanded := c.groupResources(t, &armresources.ClientListByResourceGroupOptions{Expand: new("createdTime,provisioningState"), Filter: new("tagName eq 'session-id'")})
	if len(expanded) != 1 || *expanded[0].ID != *acct.ID || *expanded[0].ProvisioningState != "Succeeded" || expanded[0].CreatedTime == nil {
		t.Errorf("expanded and filtered = %v", names(expanded))
	}
	if _, err := c.resources.NewListByResourceGroupPager(group, &armresources.ClientListByResourceGroupOptions{Top: new(int32(1))}).NextPage(t.Context()); status(err) != http.StatusBadRequest {
		t.Errorf("$top = %v", err)
	}
}

func TestListGroupResourcesNeedsItsAction(t *testing.T) {
	const write = "Microsoft.Resources/subscriptions/resourceGroups/write"
	c := setup(t, arm.WithPolicy(access.Allow(write)))
	c.createGroup(t, group, nil)
	_, err := c.resources.NewListByResourceGroupPager(group, nil).NextPage(t.Context())
	if code(err) != "AuthorizationFailed" || status(err) != http.StatusForbidden ||
		!strings.Contains(err.Error(), "'Microsoft.Resources/subscriptions/resourceGroups/resources/read'") {
		t.Fatalf("list without resources/read = %v", err)
	}
	c = setup(t, arm.WithPolicy(access.Allow(write, "Microsoft.Resources/subscriptions/resourceGroups/resources/read")))
	c.createGroup(t, group, nil)
	if got := c.groupResources(t, nil); len(got) != 0 {
		t.Fatalf("list with resources/read = %v", names(got))
	}
}

func TestListGroupResourcesWhileDeleting(t *testing.T) {
	c := setup(t, arm.WithSettleAfter(1))
	c.createGroup(t, group, nil)
	c.createNSG(t, "keep")
	c.createNSG(t, "gone")
	c.server.Settle()
	if _, err := c.nsgs.BeginDelete(t.Context(), group, "gone", nil); err != nil {
		t.Fatal(err)
	}
	expand := &armresources.ClientListByResourceGroupOptions{Expand: new("provisioningState")}
	got := c.groupResources(t, expand)
	if len(got) != 2 || *got[0].Name != "gone" || *got[0].ProvisioningState != "Deleting" {
		t.Fatalf("a deletion in progress = %v", names(got))
	}
	if got := c.groupResources(t, expand); !slices.Equal(names(got), []string{"Microsoft.Network/networkSecurityGroups keep"}) {
		t.Fatalf("after the deletion settles = %v", names(got))
	}
}
