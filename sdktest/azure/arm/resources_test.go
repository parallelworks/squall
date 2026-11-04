package arm_test

import (
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/applicationinsights/armapplicationinsights"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/compute/armcompute/v8"
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
	byValue := c.groupResources(t, &armresources.ClientListByResourceGroupOptions{Filter: new("tagName eq 'session-id' and tagValue eq 's-1'")})
	if len(byValue) != 1 || *byValue[0].ID != *acct.ID || byValue[0].Tags != nil {
		t.Errorf("filtered by tag value, which leaves out tags = %+v", byValue)
	}
	if top := c.groupResources(t, &armresources.ClientListByResourceGroupOptions{Top: new(int32(2))}); !slices.Equal(names(top), want[:2]) {
		t.Errorf("$top = %v", names(top))
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

func TestListGroupResourcesShowsTopLevelMembers(t *testing.T) {
	c := setup(t)
	c.createGroup(t, group, nil)
	body := vmBody(c.createNIC(t, group, "nic", nil).ID)
	body.Plan = &armcompute.Plan{Name: new("server"), Publisher: new("publisher"), Product: new("product")}
	p, err := c.vms.BeginCreateOrUpdate(t.Context(), group, "vm", body, nil)
	vm, err := wait(t, p, err)
	if err != nil {
		t.Fatal(err)
	}
	wp, err := c.beginWorkspace(t, "ws")
	ws, err := wait(t, wp, err)
	if err != nil {
		t.Fatal(err)
	}
	byType := map[string]*armresources.GenericResourceExpanded{}
	for _, r := range c.groupResources(t, nil) {
		byType[*r.Type] = r
	}
	if r := byType["Microsoft.Compute/virtualMachines"]; r == nil || r.Plan == nil || *r.Plan.Name != "server" || *r.Plan.Publisher != "publisher" {
		t.Errorf("VM = %+v", r)
	}
	if r := byType["Microsoft.Compute/disks"]; r == nil || r.ManagedBy == nil || *r.ManagedBy != *vm.ID || r.SKU == nil || *r.SKU.Name != "Premium_LRS" {
		t.Errorf("OS disk = %+v", r)
	}
	if r := byType["Microsoft.MachineLearningServices/workspaces"]; r == nil || r.Identity == nil || *r.Identity.Type != "SystemAssigned" ||
		*r.Identity.PrincipalID != *ws.Identity.PrincipalID {
		t.Errorf("workspace = %+v", r)
	}
	if r := byType["Microsoft.Network/networkInterfaces"]; r == nil || r.ManagedBy != nil || r.Identity != nil || r.Plan != nil {
		t.Errorf("NIC = %+v", r)
	}
}

func TestListGroupResourcesWhileTheGroupDeletes(t *testing.T) {
	c := setup(t, arm.WithSettleAfter(1))
	c.createGroup(t, group, nil)
	c.createNSG(t, "nsg")
	c.server.Settle()
	if _, err := c.groups.BeginDelete(t.Context(), group, nil); err != nil {
		t.Fatal(err)
	}
	if got := c.groupResources(t, nil); !slices.Equal(names(got), []string{"Microsoft.Network/networkSecurityGroups nsg"}) {
		t.Fatalf("a group being deleted = %v", names(got))
	}
	if _, err := c.resources.NewListByResourceGroupPager(group, nil).NextPage(t.Context()); code(err) != "ResourceGroupNotFound" {
		t.Fatalf("after the deletion settles = %v", err)
	}
}

// generic is what a group's resource list and a GET by ID both show of a resource.
type generic struct {
	ID, Type, Location, ManagedBy, Kind *string
	ExtendedLocation                    *armresources.ExtendedLocation
	Identity                            *armresources.Identity
	Plan                                *armresources.Plan
	SKU                                 *armresources.SKU
	Tags                                map[string]*string
}

func TestListGroupResourcesMatchTheirGets(t *testing.T) {
	c := setup(t)
	c.createGroup(t, group, nil)
	prefix := "/subscriptions/" + subscription + "/resourceGroups/" + group + "/providers/"
	identity := prefix + "Microsoft.ManagedIdentity/userAssignedIdentities/id"
	body := vmBody(c.createNIC(t, group, "nic", nil).ID)
	body.Plan = &armcompute.Plan{Name: new("server"), Publisher: new("publisher"), Product: new("product")}
	body.Identity = &armcompute.VirtualMachineIdentity{
		Type:                   new(armcompute.ResourceIdentityTypeUserAssigned),
		UserAssignedIdentities: map[string]*armcompute.UserAssignedIdentitiesValue{identity: {}},
	}
	vp, err := c.vms.BeginCreateOrUpdate(t.Context(), group, "vm", body, nil)
	vm, err := wait(t, vp, err)
	if err != nil {
		t.Fatal(err)
	}
	win := c.createWindowsVM(t, "win")
	c.sendScript(t, "win", "hostname")
	c.createScaleSet(t, "set", 2)
	c.createAccount(t, group, "st1")
	pip := c.createPIP(t, "pip")
	lbIP := c.createPIP(t, "lb-ip")
	natIP := c.createPIP(t, "nat-ip")
	c.createNSG(t, "nsg")
	vnet := c.createVNet(t, "vnet", map[string]string{"default": "10.1.0.0/24"})
	c.createZone(t, "example.internal")
	c.createLogWorkspace(t, "logs")
	c.createCognitiveAccount(t, "ai", "ai-domain")
	disk, err := c.putDisk(t, "data", diskBody(location, 10))
	if err != nil {
		t.Fatal(err)
	}
	snap, err := c.putSnapshot(t, "snap", snapshotBody(location, armcompute.DiskCreateOptionCopy, *disk.ID, false))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.components.CreateOrUpdate(t.Context(), group, "insights", armapplicationinsights.Component{
		Location: new(location), Kind: new("web"),
		Properties: &armapplicationinsights.ComponentProperties{ApplicationType: new(armapplicationinsights.ApplicationTypeWeb)},
	}, nil); err != nil {
		t.Fatal(err)
	}
	for _, step := range []func() error{
		func() error { _, err := c.createNAT(t, "nat", natIP.ID); return err },
		func() error { _, err := c.putLB(t, lbBody(1_024, lbIP.ID)); return err },
		func() error {
			_, err := c.createPrivateEndpoint(t, prefix+"Microsoft.Storage/storageAccounts/st1")
			return err
		},
		func() error { _, err := c.createVolume(t, "netapp", "pool", "vol"); return err },
		func() error { p, err := c.beginVault(t, "kv1"); _, err = wait(t, p, err); return err },
		func() error { p, err := c.beginWorkspace(t, "ws"); _, err = wait(t, p, err); return err },
		func() error { p, err := c.beginSearch(t, "search"); _, err = wait(t, p, err); return err },
		func() error { p, err := c.beginFilesystem(t, "lustre"); _, err = wait(t, p, err); return err },
		func() error {
			p, err := c.links.BeginCreateOrUpdate(t.Context(), group, "example.internal", "link", armprivatedns.VirtualNetworkLink{
				Location:   new("global"),
				Properties: &armprivatedns.VirtualNetworkLinkProperties{VirtualNetwork: &armprivatedns.SubResource{ID: vnet.ID}, RegistrationEnabled: new(false)},
			}, nil)
			_, err = wait(t, p, err)
			return err
		},
		func() error {
			p, err := c.galleries.BeginCreateOrUpdate(t.Context(), group, "gallery", armcompute.Gallery{Location: new(location)}, nil)
			if _, err = wait(t, p, err); err != nil {
				return err
			}
			ip, err := c.images.BeginCreateOrUpdate(t.Context(), group, "gallery", "image", imageBody(armcompute.OperatingSystemTypesLinux), nil)
			if _, err = wait(t, ip, err); err != nil {
				return err
			}
			vp, err := c.versions.BeginCreateOrUpdate(t.Context(), group, "gallery", "image", "1.0.0", versionBody(*snap.ID), nil)
			_, err = wait(t, vp, err)
			return err
		},
	} {
		if err := step(); err != nil {
			t.Fatal(err)
		}
	}
	listed := c.groupResources(t, nil)
	var ids []string
	for _, r := range listed {
		ids = append(ids, *r.ID)
		got, err := c.resources.GetByID(t.Context(), *r.ID, "2024-01-01", nil)
		if err != nil {
			t.Fatalf("GET %s: %v", *r.ID, err)
		}
		fromList, _ := json.Marshal(generic{r.ID, r.Type, r.Location, r.ManagedBy, r.Kind, r.ExtendedLocation, r.Identity, r.Plan, r.SKU, r.Tags})
		g := got.GenericResource
		fromGet, _ := json.Marshal(generic{g.ID, g.Type, g.Location, g.ManagedBy, g.Kind, g.ExtendedLocation, g.Identity, g.Plan, g.SKU, g.Tags})
		if string(fromList) != string(fromGet) {
			t.Errorf("%s lists as %s, GET shows %s", *r.ID, fromList, fromGet)
		}
	}
	want := []string{
		prefix + "Microsoft.CognitiveServices/accounts/ai",
		prefix + "Microsoft.Compute/disks/data",
		*vm.Properties.StorageProfile.OSDisk.ManagedDisk.ID,
		*win.Properties.StorageProfile.OSDisk.ManagedDisk.ID,
		prefix + "Microsoft.Compute/galleries/gallery",
		prefix + "Microsoft.Compute/galleries/gallery/images/image",
		prefix + "Microsoft.Compute/galleries/gallery/images/image/versions/1.0.0",
		*snap.ID,
		*vm.ID,
		*win.ID,
		*win.ID + "/extensions/" + scriptExtension,
		prefix + "Microsoft.Compute/virtualMachineScaleSets/set",
		prefix + "microsoft.insights/components/insights",
		prefix + "Microsoft.KeyVault/vaults/kv1",
		prefix + "Microsoft.MachineLearningServices/workspaces/ws",
		prefix + "Microsoft.NetApp/netAppAccounts/netapp",
		prefix + "Microsoft.NetApp/netAppAccounts/netapp/capacityPools/pool",
		prefix + "Microsoft.NetApp/netAppAccounts/netapp/capacityPools/pool/volumes/vol",
		lbID,
		prefix + "Microsoft.Network/natGateways/nat",
		prefix + "Microsoft.Network/networkInterfaces/files1-nic",
		prefix + "Microsoft.Network/networkInterfaces/nic",
		prefix + "Microsoft.Network/networkInterfaces/win-nic",
		prefix + "Microsoft.Network/networkSecurityGroups/nsg",
		prefix + "Microsoft.Network/privateDnsZones/example.internal",
		prefix + "Microsoft.Network/privateDnsZones/example.internal/virtualNetworkLinks/link",
		prefix + "Microsoft.Network/privateEndpoints/files1-endpoint",
		*lbIP.ID,
		*natIP.ID,
		*pip.ID,
		*vnet.ID,
		prefix + "Microsoft.OperationalInsights/workspaces/logs",
		prefix + "Microsoft.Search/searchServices/search",
		prefix + "Microsoft.Storage/storageAccounts/st1",
		prefix + "Microsoft.StorageCache/amlFilesystems/lustre",
	}
	if !slices.Equal(ids, want) {
		t.Errorf("listed\n%s\nwant\n%s", strings.Join(ids, "\n"), strings.Join(want, "\n"))
	}
	for typ, check := range map[string]func(*armresources.GenericResourceExpanded) bool{
		"Microsoft.Compute/virtualMachineScaleSets": func(r *armresources.GenericResourceExpanded) bool {
			return r.SKU != nil && *r.SKU.Name != "" && *r.SKU.Tier == "Standard" && *r.SKU.Capacity == 2
		},
		"Microsoft.Compute/disks": func(r *armresources.GenericResourceExpanded) bool {
			return r.SKU != nil && *r.SKU.Name == "Premium_LRS" && *r.SKU.Tier == "Premium"
		},
		"Microsoft.Storage/storageAccounts": func(r *armresources.GenericResourceExpanded) bool {
			return r.SKU != nil && *r.SKU.Name == "Standard_LRS" && *r.SKU.Tier == "Standard" && *r.Kind == "StorageV2"
		},
		"Microsoft.Compute/virtualMachines": func(r *armresources.GenericResourceExpanded) bool {
			return r.Plan != nil && *r.Plan.Name == "server" && r.Identity != nil && *r.Identity.Type == "UserAssigned" &&
				r.Identity.UserAssignedIdentities[identity] != nil
		},
		"Microsoft.MachineLearningServices/workspaces": func(r *armresources.GenericResourceExpanded) bool {
			return r.Identity != nil && r.Identity.PrincipalID != nil && *r.Identity.PrincipalID != ""
		},
	} {
		i := slices.IndexFunc(listed, func(r *armresources.GenericResourceExpanded) bool { return *r.Type == typ })
		if i < 0 || !check(listed[i]) {
			t.Errorf("%s = %+v", typ, listed[max(i, 0)])
		}
	}
	expanded := c.groupResources(t, &armresources.ClientListByResourceGroupOptions{Expand: new("provisioningState")})
	for _, r := range expanded {
		got, err := c.resources.GetByID(t.Context(), *r.ID, "2024-01-01", nil)
		if err != nil {
			t.Fatal(err)
		}
		props, _ := got.Properties.(map[string]any)
		if want, _ := props["provisioningState"].(string); r.ProvisioningState == nil || *r.ProvisioningState != want {
			t.Errorf("%s lists provisioningState %v, GET shows %q", *r.ID, r.ProvisioningState, want)
		}
	}
}
