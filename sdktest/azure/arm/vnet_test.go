package arm_test

import (
	"maps"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/network/armnetwork/v9"

	"github.com/parallelworks/squall/access"
	"github.com/parallelworks/squall/azure/arm"
)

func (c *clients) createVNet(t *testing.T, name string, subnets map[string]string) armnetwork.VirtualNetwork {
	t.Helper()
	body := armnetwork.VirtualNetwork{
		Location: new(location),
		Properties: &armnetwork.VirtualNetworkPropertiesFormat{
			AddressSpace: &armnetwork.AddressSpace{AddressPrefixes: []*string{new("10.1.0.0/16")}},
		},
	}
	for _, sn := range slices.Sorted(maps.Keys(subnets)) {
		body.Properties.Subnets = append(body.Properties.Subnets, &armnetwork.Subnet{
			Name: new(sn), Properties: &armnetwork.SubnetPropertiesFormat{AddressPrefix: new(subnets[sn])},
		})
	}
	p, err := c.vnets.BeginCreateOrUpdate(t.Context(), group, name, body, nil)
	res, err := wait(t, p, err)
	if err != nil {
		t.Fatal(err)
	}
	return res.VirtualNetwork
}

func (c *clients) putSubnet(t *testing.T, name string, props *armnetwork.SubnetPropertiesFormat) (armnetwork.Subnet, error) {
	t.Helper()
	p, err := c.subnets.BeginCreateOrUpdate(t.Context(), group, "vnet", name, armnetwork.Subnet{Properties: props}, nil)
	res, err := wait(t, p, err)
	return res.Subnet, err
}

func (c *clients) createNAT(t *testing.T, name string, pips ...*string) (armnetwork.NatGateway, error) {
	t.Helper()
	body := armnetwork.NatGateway{
		Location:   new(location),
		SKU:        &armnetwork.NatGatewaySKU{Name: new(armnetwork.NatGatewaySKUNameStandard)},
		Properties: &armnetwork.NatGatewayPropertiesFormat{},
	}
	for _, pip := range pips {
		body.Properties.PublicIPAddresses = append(body.Properties.PublicIPAddresses, &armnetwork.SubResource{ID: pip})
	}
	p, err := c.nats.BeginCreateOrUpdate(t.Context(), group, name, body, nil)
	res, err := wait(t, p, err)
	return res.NatGateway, err
}

func TestVirtualNetworksSubnetsAndNATGateways(t *testing.T) {
	c := setup(t)
	c.createGroup(t, group, nil)
	vnet := c.createVNet(t, "vnet", map[string]string{"default": "10.1.0.0/24"})
	if len(vnet.Properties.Subnets) != 1 || *vnet.Properties.Subnets[0].Properties.AddressPrefix != "10.1.0.0/24" ||
		!strings.HasSuffix(*vnet.Properties.Subnets[0].ID, "/virtualNetworks/vnet/subnets/default") || *vnet.Properties.EnableDdosProtection {
		t.Fatalf("vnet = %+v", vnet.Properties)
	}

	anf, err := c.putSubnet(t, "anf", &armnetwork.SubnetPropertiesFormat{
		AddressPrefix: new("10.1.1.0/28"),
		Delegations: []*armnetwork.Delegation{{
			Name: new("netapp"), Properties: &armnetwork.ServiceDelegationPropertiesFormat{ServiceName: new("Microsoft.Netapp/volumes")},
		}},
		ServiceEndpoints: []*armnetwork.ServiceEndpointPropertiesFormat{{Service: new("Microsoft.Storage")}},
	})
	if err != nil {
		t.Fatal(err)
	}
	d := anf.Properties.Delegations[0]
	if *anf.Type != "Microsoft.Network/virtualNetworks/subnets" || *d.Properties.ServiceName != "Microsoft.Netapp/volumes" ||
		*d.ID != *anf.ID+"/delegations/netapp" || *anf.Properties.ServiceEndpoints[0].ProvisioningState != armnetwork.ProvisioningStateSucceeded ||
		*anf.Properties.PrivateEndpointNetworkPolicies != armnetwork.VirtualNetworkPrivateEndpointNetworkPoliciesDisabled {
		t.Fatalf("subnet = %+v", anf.Properties)
	}

	pip := c.createPIP(t, "nat-ip")
	nat, err := c.createNAT(t, "nat", pip.ID)
	if err != nil {
		t.Fatal(err)
	}
	if *nat.SKU.Name != armnetwork.NatGatewaySKUNameStandard || *nat.Properties.IdleTimeoutInMinutes != 4 || len(nat.Properties.Subnets) != 0 {
		t.Fatalf("nat = %+v", nat)
	}
	gotPIP, err := c.pips.Get(t.Context(), group, "nat-ip", nil)
	if err != nil || gotPIP.Properties.NatGateway == nil || *gotPIP.Properties.NatGateway.ID != *nat.ID {
		t.Fatalf("public IP = %+v, %v", gotPIP.Properties, err)
	}

	got, err := c.subnets.Get(t.Context(), group, "vnet", "default", nil)
	if err != nil {
		t.Fatal(err)
	}
	got.Properties.NatGateway = &armnetwork.SubResource{ID: nat.ID}
	if _, err := c.putSubnet(t, "default", got.Properties); err != nil {
		t.Fatal(err)
	}
	gotNAT, err := c.nats.Get(t.Context(), group, "nat", nil)
	if err != nil || len(gotNAT.Properties.Subnets) != 1 || *gotNAT.Properties.Subnets[0].ID != *got.ID {
		t.Fatalf("nat subnets = %+v, %v", gotNAT.Properties, err)
	}
	snapshot := c.server.VirtualNetworks()
	if len(snapshot) != 1 || len(snapshot[0].Subnets) != 2 || snapshot[0].Subnets[1].NATGateway != *nat.ID ||
		!slices.Equal(snapshot[0].Subnets[0].Delegations, []string{"Microsoft.Netapp/volumes"}) {
		t.Fatalf("VirtualNetworks() = %+v", snapshot)
	}
	if nats := c.server.NATGateways(); len(nats) != 1 || !slices.Equal(nats[0].Subnets, []string{*got.ID}) {
		t.Fatalf("NATGateways() = %+v", nats)
	}

	p, err := c.nats.BeginDelete(t.Context(), group, "nat", nil)
	if _, err = wait(t, p, err); code(err) != "InUseNatGatewayCannotBeDeleted" || status(err) != http.StatusBadRequest {
		t.Fatalf("deleting a NAT gateway in use = %v", err)
	}

	nic := armnetwork.Interface{
		Location: new(location),
		Properties: &armnetwork.InterfacePropertiesFormat{IPConfigurations: []*armnetwork.InterfaceIPConfiguration{{
			Name: new("ipconfig1"), Properties: &armnetwork.InterfaceIPConfigurationPropertiesFormat{Subnet: &armnetwork.Subnet{ID: got.ID}},
		}}},
	}
	np, err := c.nics.BeginCreateOrUpdate(t.Context(), group, "nic", nic, nil)
	created, err := wait(t, np, err)
	if err != nil || *created.Properties.IPConfigurations[0].Properties.PrivateIPAddress != "10.1.0.4" {
		t.Fatalf("a NIC in the subnet = %+v, %v", created, err)
	}
	sp, err := c.subnets.BeginDelete(t.Context(), group, "vnet", "default", nil)
	if _, err = wait(t, sp, err); code(err) != "InUseSubnetCannotBeDeleted" || !strings.Contains(err.Error(), "/networkInterfaces/nic/ipConfigurations/ipconfig1") {
		t.Fatalf("deleting a subnet in use = %v", err)
	}

	p2, err := c.groups.BeginDelete(t.Context(), group, nil)
	if _, err = wait(t, p2, err); err != nil {
		t.Fatal(err)
	}
	if left := c.server.Resources(); len(left) != 0 {
		t.Fatalf("resources left = %+v", left)
	}
}

func TestSubnetUpdateDetachesItsNATGateway(t *testing.T) {
	c := setup(t)
	c.createGroup(t, group, nil)
	c.createVNet(t, "vnet", nil)
	nat, err := c.createNAT(t, "nat")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.putSubnet(t, "default", &armnetwork.SubnetPropertiesFormat{AddressPrefix: new("10.1.0.0/24"), NatGateway: &armnetwork.SubResource{ID: nat.ID}}); err != nil {
		t.Fatal(err)
	}
	sn, err := c.putSubnet(t, "default", &armnetwork.SubnetPropertiesFormat{AddressPrefix: new("10.1.0.0/24")})
	if err != nil || sn.Properties.NatGateway != nil {
		t.Fatalf("subnet = %+v, %v", sn.Properties, err)
	}
	p, err := c.nats.BeginDelete(t.Context(), group, "nat", nil)
	if _, err = wait(t, p, err); err != nil {
		t.Fatalf("deleting the detached NAT gateway = %v", err)
	}
	// A virtual network PUT replaces its subnets, deleting those it leaves out.
	c.createVNet(t, "vnet", nil)
	if _, err := c.subnets.Get(t.Context(), group, "vnet", "default", nil); code(err) != "ResourceNotFound" {
		t.Fatalf("a subnet the vnet left out = %v", err)
	}
}

func TestSubnetPrefixesAreChecked(t *testing.T) {
	c := setup(t)
	c.createGroup(t, group, nil)
	c.createVNet(t, "vnet", map[string]string{"default": "10.1.0.0/24"})
	for prefix, want := range map[string]string{
		"10.2.0.0/24":   "NetcfgSubnetRangeOutsideVnet",
		"10.1.0.128/25": "NetcfgSubnetRangesOverlap",
		"10.1.1.0/30":   "NetcfgInvalidSubnet",
		"10.1.1.1/24":   "InvalidCIDRNotation",
	} {
		if _, err := c.putSubnet(t, "other", &armnetwork.SubnetPropertiesFormat{AddressPrefix: new(prefix)}); code(err) != want || status(err) != http.StatusBadRequest {
			t.Errorf("subnet %s = %v, want %s", prefix, err, want)
		}
	}
	if _, err := c.putSubnet(t, "other", &armnetwork.SubnetPropertiesFormat{AddressPrefix: new("10.1.1.0/29")}); err != nil {
		t.Errorf("a /29 subnet = %v", err)
	}
	if _, err := c.subnets.BeginCreateOrUpdate(t.Context(), group, "missing", "default", armnetwork.Subnet{
		Properties: &armnetwork.SubnetPropertiesFormat{AddressPrefix: new("10.1.0.0/24")},
	}, nil); code(err) != "ParentResourceNotFound" {
		t.Errorf("a subnet of a missing vnet = %v", err)
	}
	if _, err := c.putSubnet(t, "nat", &armnetwork.SubnetPropertiesFormat{
		AddressPrefix: new("10.1.2.0/24"), NatGateway: &armnetwork.SubResource{ID: new(strings.Replace(vnet, "virtualNetworks/vnet", "natGateways/none", 1))},
	}); code(err) != "InvalidResourceReference" {
		t.Errorf("a subnet with a missing NAT gateway = %v", err)
	}
}

func TestPolicyChecksWhatSubnetsAndNATGatewaysLinkTo(t *testing.T) {
	policy := access.Allow(
		"Microsoft.Resources/subscriptions/resourceGroups/write",
		"Microsoft.Network/virtualNetworks/write",
		"Microsoft.Network/virtualNetworks/subnets/write",
		"Microsoft.Network/natGateways/write",
		"Microsoft.Network/publicIPAddresses/write",
	)
	c := setup(t, arm.WithPolicy(policy))
	c.createGroup(t, group, nil)
	c.createVNet(t, "vnet", nil)
	pip := c.createPIP(t, "nat-ip")
	if _, err := c.createNAT(t, "nat", pip.ID); code(err) != "LinkedAuthorizationFailed" ||
		!strings.Contains(err.Error(), "'Microsoft.Network/publicIPAddresses/join/action' on the linked scope(s) '"+*pip.ID+"'") {
		t.Fatalf("a NAT gateway without join on its public IP = %v", err)
	}
	nat, err := c.createNAT(t, "nat")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.putSubnet(t, "default", &armnetwork.SubnetPropertiesFormat{AddressPrefix: new("10.1.0.0/24"), NatGateway: &armnetwork.SubResource{ID: nat.ID}}); code(err) != "LinkedAuthorizationFailed" ||
		!strings.Contains(err.Error(), "'Microsoft.Network/natGateways/join/action' on the linked scope(s) '"+*nat.ID+"'") {
		t.Fatalf("a subnet without join on its NAT gateway = %v", err)
	}
}
