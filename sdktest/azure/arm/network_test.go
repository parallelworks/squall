package arm_test

import (
	"net/http"
	"net/netip"
	"slices"
	"strings"
	"testing"

	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/network/armnetwork/v9"

	"github.com/parallelworks/squall/azure/arm"
	"github.com/parallelworks/squall/fault"
)

func nicBody(nsg, pip *string) armnetwork.Interface {
	ip := &armnetwork.InterfaceIPConfiguration{
		Name: new("ipconfig1"),
		Properties: &armnetwork.InterfaceIPConfigurationPropertiesFormat{
			Subnet:                    &armnetwork.Subnet{ID: new(subnet)},
			PrivateIPAllocationMethod: new(armnetwork.IPAllocationMethodDynamic),
		},
	}
	if pip != nil {
		ip.Properties.PublicIPAddress = &armnetwork.PublicIPAddress{ID: pip}
	}
	nic := armnetwork.Interface{
		Location:   new(location),
		Tags:       map[string]*string{"session": new("s")},
		Properties: &armnetwork.InterfacePropertiesFormat{IPConfigurations: []*armnetwork.InterfaceIPConfiguration{ip}},
	}
	if nsg != nil {
		nic.Properties.NetworkSecurityGroup = &armnetwork.SecurityGroup{ID: nsg}
	}
	return nic
}

func (c *clients) createNIC(t *testing.T, rg, name string, nsg *string) armnetwork.Interface {
	t.Helper()
	p, err := c.nics.BeginCreateOrUpdate(t.Context(), rg, name, nicBody(nsg, nil), nil)
	if err != nil {
		t.Fatal(err)
	}
	if p.Done() {
		res, err := p.Result(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		return res.Interface
	}
	got, err := c.nics.Get(t.Context(), rg, name, nil)
	if err != nil {
		t.Fatal(err)
	}
	return got.Interface
}

func (c *clients) createPIP(t *testing.T, name string) armnetwork.PublicIPAddress {
	t.Helper()
	p, err := c.pips.BeginCreateOrUpdate(t.Context(), group, name, armnetwork.PublicIPAddress{
		Location: new(location),
		Properties: &armnetwork.PublicIPAddressPropertiesFormat{
			DNSSettings: &armnetwork.PublicIPAddressDNSSettings{DomainNameLabel: new(name)},
		},
	}, nil)
	res, err := wait(t, p, err)
	if err != nil {
		t.Fatal(err)
	}
	return res.PublicIPAddress
}

func (c *clients) createNSG(t *testing.T, name string) armnetwork.SecurityGroup {
	t.Helper()
	p, err := c.nsgs.BeginCreateOrUpdate(t.Context(), group, name, armnetwork.SecurityGroup{
		Location: new(location),
		Properties: &armnetwork.SecurityGroupPropertiesFormat{SecurityRules: []*armnetwork.SecurityRule{{
			Name: new("ssh"),
			Properties: &armnetwork.SecurityRulePropertiesFormat{
				Priority: new(int32(100)), Direction: new(armnetwork.SecurityRuleDirectionInbound),
				Access: new(armnetwork.SecurityRuleAccessAllow), Protocol: new(armnetwork.SecurityRuleProtocolTCP),
				SourceAddressPrefix: new("*"), SourcePortRange: new("*"), DestinationAddressPrefix: new("*"),
				DestinationPortRange: new("22"),
			},
		}}},
	}, nil)
	res, err := wait(t, p, err)
	if err != nil {
		t.Fatal(err)
	}
	return res.SecurityGroup
}

func TestSecurityGroups(t *testing.T) {
	c := setup(t)
	c.createGroup(t, group, nil)
	nsg := c.createNSG(t, "nsg")
	rule := nsg.Properties.SecurityRules[0]
	if *nsg.Properties.ProvisioningState != armnetwork.ProvisioningStateSucceeded || *rule.Properties.DestinationPortRange != "22" ||
		!strings.HasSuffix(*rule.ID, "/networkSecurityGroups/nsg/securityRules/ssh") || nsg.Etag == nil || *nsg.Properties.ResourceGUID == "" {
		t.Fatalf("nsg = %+v", nsg)
	}
	nic := c.createNIC(t, group, "nic", nsg.ID)
	if *nic.Properties.NetworkSecurityGroup.ID != *nsg.ID {
		t.Fatalf("nic nsg = %v", nic.Properties.NetworkSecurityGroup.ID)
	}
	got, err := c.nsgs.Get(t.Context(), group, "NSG", nil)
	if err != nil || len(got.Properties.NetworkInterfaces) != 1 || *got.Properties.NetworkInterfaces[0].ID != *nic.ID {
		t.Fatalf("nsg users = %+v, %v", got.Properties, err)
	}
	p, err := c.nsgs.BeginDelete(t.Context(), group, "nsg", nil)
	if _, err = wait(t, p, err); code(err) != "InUseNetworkSecurityGroupCannotBeDeleted" || status(err) != http.StatusBadRequest {
		t.Fatalf("deleting an NSG in use = %v", err)
	}
	p2, err := c.nics.BeginDelete(t.Context(), group, "nic", nil)
	if _, err = wait(t, p2, err); err != nil {
		t.Fatal(err)
	}
	p, err = c.nsgs.BeginDelete(t.Context(), group, "nsg", nil)
	if _, err = wait(t, p, err); err != nil {
		t.Fatal(err)
	}
	if _, err := c.nsgs.Get(t.Context(), group, "nsg", nil); code(err) != "ResourceNotFound" || status(err) != http.StatusNotFound {
		t.Fatalf("get after delete = %v", err)
	}
	// Deleting what is not there succeeds, as ARM's DELETE is idempotent.
	p, err = c.nsgs.BeginDelete(t.Context(), group, "nsg", nil)
	if _, err = wait(t, p, err); err != nil {
		t.Fatalf("deleting it again = %v", err)
	}
	if _, err := c.nics.BeginCreateOrUpdate(t.Context(), group, "nic-2", nicBody(nsg.ID, nil), nil); code(err) != "InvalidResourceReference" {
		t.Fatalf("a NIC with a missing NSG = %v", err)
	}
}

func TestNetworkInterfacesAndPublicIPs(t *testing.T) {
	c := setup(t)
	c.createGroup(t, group, nil)
	pip := c.createPIP(t, "pip")
	if *pip.Properties.IPAddress != "100.64.0.1" || *pip.SKU.Name != armnetwork.PublicIPAddressSKUNameStandard ||
		*pip.Properties.PublicIPAllocationMethod != armnetwork.IPAllocationMethodStatic ||
		*pip.Properties.DNSSettings.Fqdn != "pip.eastus.cloudapp.azure.com" {
		t.Fatalf("pip = %+v", pip.Properties)
	}
	p, err := c.nics.BeginCreateOrUpdate(t.Context(), group, "nic", nicBody(nil, pip.ID), nil)
	created, err := wait(t, p, err)
	if err != nil {
		t.Fatal(err)
	}
	ip := created.Properties.IPConfigurations[0]
	if *ip.Properties.PrivateIPAddress != "10.0.0.4" || !*ip.Properties.Primary || *ip.Properties.Subnet.ID != subnet ||
		*ip.Properties.PublicIPAddress.ID != *pip.ID || *ip.ID != *created.ID+"/ipConfigurations/ipconfig1" ||
		created.Properties.VirtualMachine != nil || *created.Tags["session"] != "s" {
		t.Fatalf("nic = %+v, ip = %+v", created.Properties, ip.Properties)
	}
	gotPIP, err := c.pips.Get(t.Context(), group, "pip", nil)
	if err != nil || *gotPIP.Properties.IPConfiguration.ID != *ip.ID {
		t.Fatalf("pip ipConfiguration = %+v, %v", gotPIP.Properties.IPConfiguration, err)
	}
	// Updating keeps a dynamic address.
	p, err = c.nics.BeginCreateOrUpdate(t.Context(), group, "nic", nicBody(nil, pip.ID), nil)
	updated, err := wait(t, p, err)
	if err != nil || *updated.Properties.IPConfigurations[0].Properties.PrivateIPAddress != "10.0.0.4" {
		t.Fatalf("update = %+v, %v", updated.Properties, err)
	}
	if _, err := c.nics.BeginCreateOrUpdate(t.Context(), group, "nic-2", nicBody(nil, pip.ID), nil); code(err) != "PublicIPAddressInUse" {
		t.Fatalf("taking a public IP in use = %v", err)
	}
	pd, err := c.pips.BeginDelete(t.Context(), group, "pip", nil)
	if _, err = wait(t, pd, err); code(err) != "PublicIPAddressCannotBeDeleted" {
		t.Fatalf("deleting a public IP in use = %v", err)
	}
	static := nicBody(nil, nil)
	static.Properties.IPConfigurations[0].Properties.PrivateIPAllocationMethod = new(armnetwork.IPAllocationMethodStatic)
	static.Properties.IPConfigurations[0].Properties.PrivateIPAddress = new("10.0.0.4")
	if _, err := c.nics.BeginCreateOrUpdate(t.Context(), group, "nic-3", static, nil); code(err) != "PrivateIPAddressIsAllocated" {
		t.Fatalf("a static address in use = %v", err)
	}
	static.Properties.IPConfigurations[0].Properties.PrivateIPAddress = new("10.0.0.9")
	p, err = c.nics.BeginCreateOrUpdate(t.Context(), group, "nic-3", static, nil)
	got, err := wait(t, p, err)
	if err != nil || *got.Properties.IPConfigurations[0].Properties.PrivateIPAllocationMethod != armnetwork.IPAllocationMethodStatic {
		t.Fatalf("a static address = %+v, %v", got.Properties, err)
	}
	moved := nicBody(nil, nil)
	moved.Location = new("westus")
	if _, err := c.nics.BeginCreateOrUpdate(t.Context(), group, "nic", moved, nil); code(err) != "InvalidResourceLocation" || status(err) != http.StatusConflict {
		t.Fatalf("moving a NIC = %v", err)
	}
	if _, err := c.nics.Get(t.Context(), "nope", "nic", nil); code(err) != "ResourceGroupNotFound" {
		t.Fatalf("get in a missing group = %v", err)
	}
	if got := c.server.NetworkInterfaces(); len(got) != 2 || got[1].PrivateIPAddresses[0] != "10.0.0.9" {
		t.Fatalf("NetworkInterfaces = %+v", got)
	}
	if got := c.server.PublicIPAddresses(); len(got) != 1 || got[0].IPAddress != "100.64.0.1" {
		t.Fatalf("PublicIPAddresses = %+v", got)
	}
}

func TestNetworkInterfaceDynamicAddressAvoidsItsStaticOne(t *testing.T) {
	c := setup(t)
	c.createGroup(t, group, nil)
	body := nicBody(nil, nil)
	body.Properties.IPConfigurations[0].Properties.Primary = new(true)
	body.Properties.IPConfigurations = append(body.Properties.IPConfigurations, &armnetwork.InterfaceIPConfiguration{
		Name: new("ipconfig2"),
		Properties: &armnetwork.InterfaceIPConfigurationPropertiesFormat{
			Subnet:                    &armnetwork.Subnet{ID: new(subnet)},
			PrivateIPAllocationMethod: new(armnetwork.IPAllocationMethodStatic),
			PrivateIPAddress:          new("10.0.0.4"),
		},
	})
	p, err := c.nics.BeginCreateOrUpdate(t.Context(), group, "nic", body, nil)
	got, err := wait(t, p, err)
	if err != nil {
		t.Fatal(err)
	}
	dynamic, static := got.Properties.IPConfigurations[0].Properties, got.Properties.IPConfigurations[1].Properties
	if *static.PrivateIPAddress != "10.0.0.4" || *dynamic.PrivateIPAddress == "10.0.0.4" {
		t.Fatalf("addresses = %s and %s", *dynamic.PrivateIPAddress, *static.PrivateIPAddress)
	}
}

func TestNetworkPollersWithSettle(t *testing.T) {
	c := setup(t, arm.WithSettleAfter(2))
	c.createGroup(t, group, nil)
	p, err := c.pips.BeginCreateOrUpdate(t.Context(), group, "pip", armnetwork.PublicIPAddress{Location: new(location)}, nil)
	if err != nil || p.Done() {
		t.Fatalf("begin = done %v, %v", p.Done(), err)
	}
	res, err := p.PollUntilDone(t.Context(), nil)
	if err != nil || *res.Properties.ProvisioningState != armnetwork.ProvisioningStateSucceeded {
		t.Fatalf("result = %+v, %v", res.Properties, err)
	}
	if got := c.polls(); got != 3 {
		t.Errorf("polled %d times, want 3", got)
	}
	d, err := c.pips.BeginDelete(t.Context(), group, "pip", nil)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := c.pips.Get(t.Context(), group, "pip", nil); err != nil || *got.Properties.ProvisioningState != armnetwork.ProvisioningStateDeleting {
		t.Fatalf("pip being deleted = %+v, %v", got.Properties, err)
	}
	if _, err := d.PollUntilDone(t.Context(), nil); err != nil {
		t.Fatal(err)
	}
	if got := c.server.PublicIPAddresses(); len(got) != 0 {
		t.Fatalf("deleted pip = %+v", got)
	}
}

func TestNetworkFailedOperation(t *testing.T) {
	faults := fault.New().Add(fault.Rule{
		Method: "Microsoft.Network/networkInterfaces/write", Nth: 1, Fault: fault.Fault{Mode: fault.ModeFailOperation, Status: http.StatusBadRequest, Reason: "SubnetIsFull"},
	})
	c := setup(t, arm.WithFaults(faults))
	c.createGroup(t, group, nil)
	p, err := c.nics.BeginCreateOrUpdate(t.Context(), group, "nic", nicBody(nil, nil), nil)
	if _, err = wait(t, p, err); code(err) != "SubnetIsFull" {
		t.Fatalf("failed operation = %v", err)
	}
	if _, err := c.nics.Get(t.Context(), group, "nic", nil); code(err) != "ResourceNotFound" {
		t.Fatalf("a failed create took effect: %v", err)
	}
	if nic := c.createNIC(t, group, "nic", nil); *nic.Properties.IPConfigurations[0].Properties.PrivateIPAddress != "10.0.0.4" {
		t.Fatalf("the failed create took an address: %+v", nic.Properties.IPConfigurations[0].Properties)
	}
}

func TestNetworkChangeInProgressIsRetryable(t *testing.T) {
	c := setup(t, arm.WithSettleAfter(-1))
	c.createGroup(t, group, nil)
	body := armnetwork.SecurityGroup{Location: new(location)}
	if _, err := c.nsgs.BeginCreateOrUpdate(t.Context(), group, "nsg", body, nil); err != nil {
		t.Fatal(err)
	}
	_, err := c.nsgs.BeginCreateOrUpdate(t.Context(), group, "nsg", body, nil)
	if code(err) != "RetryableError" || status(err) != http.StatusTooManyRequests || !strings.Contains(err.Error(), "RetryableErrorDueToAnotherOperation") {
		t.Fatalf("a change while another runs = %v", err)
	}
	c.server.Settle()
	p, err := c.nsgs.BeginCreateOrUpdate(t.Context(), group, "nsg", body, nil)
	if err != nil || p.Done() {
		t.Fatalf("a change once the other finished = %v", err)
	}
}

func TestNetworkChangeInProgressConvergesWhenRetried(t *testing.T) {
	c := setupRetrying(t, arm.WithSettleAfter(1))
	c.createGroup(t, group, nil)
	if _, err := c.pips.BeginCreateOrUpdate(t.Context(), group, "pip", armnetwork.PublicIPAddress{Location: new(location)}, nil); err != nil {
		t.Fatal(err)
	}
	d, err := c.pips.BeginDelete(t.Context(), group, "pip", nil)
	if _, err = wait(t, d, err); err != nil {
		t.Fatalf("deleting a public IP being created = %v", err)
	}
}

func TestAddressRangesRunOut(t *testing.T) {
	c := setup(t, arm.WithPrivateRange(netip.MustParsePrefix("10.9.0.0/29")), arm.WithPublicRange(netip.MustParsePrefix("100.64.0.0/30")))
	c.createGroup(t, group, nil)
	var got []string
	for _, name := range []string{"nic-1", "nic-2", "nic-3"} {
		nic := c.createNIC(t, group, name, nil)
		got = append(got, *nic.Properties.IPConfigurations[0].Properties.PrivateIPAddress)
	}
	if want := []string{"10.9.0.4", "10.9.0.5", "10.9.0.6"}; !slices.Equal(got, want) {
		t.Fatalf("addresses = %v, want %v", got, want)
	}
	p, err := c.nics.BeginCreateOrUpdate(t.Context(), group, "nic-4", nicBody(nil, nil), nil)
	if _, err = wait(t, p, err); code(err) != "SubnetIsFull" || status(err) != http.StatusBadRequest {
		t.Fatalf("an interface in a full subnet = %v", err)
	}
	if _, err := c.nics.Get(t.Context(), group, "nic-4", nil); code(err) != "ResourceNotFound" {
		t.Fatalf("the interface that did not fit = %v", err)
	}
	if _, err := c.sets.BeginCreateOrUpdate(t.Context(), group, "set", scaleSetBody(new(int64(1))), nil); code(err) != "SubnetIsFull" {
		t.Fatalf("a scale set in a full subnet = %v", err)
	}
	other := nicBody(nil, nil)
	other.Properties.IPConfigurations[0].Properties.Subnet.ID = new(vnet + "/subnets/other")
	p, err = c.nics.BeginCreateOrUpdate(t.Context(), group, "nic-4", other, nil)
	if _, err = wait(t, p, err); err != nil {
		t.Fatalf("an interface in another subnet = %v", err)
	}
	dp, err := c.nics.BeginDelete(t.Context(), group, "nic-2", nil)
	if _, err = wait(t, dp, err); err != nil {
		t.Fatal(err)
	}
	if nic := c.createNIC(t, group, "nic-5", nil); *nic.Properties.IPConfigurations[0].Properties.PrivateIPAddress != "10.9.0.5" {
		t.Fatalf("a freed address = %s, want 10.9.0.5", *nic.Properties.IPConfigurations[0].Properties.PrivateIPAddress)
	}

	for _, name := range []string{"pip-1", "pip-2", "pip-3"} {
		c.createPIP(t, name)
	}
	pp, err := c.pips.BeginCreateOrUpdate(t.Context(), group, "pip-4", armnetwork.PublicIPAddress{Location: new(location)}, nil)
	if _, err = wait(t, pp, err); code(err) != "PublicIPCountLimitReached" || status(err) != http.StatusBadRequest {
		t.Fatalf("a public IP address past the range = %v", err)
	}
	if got := c.server.PublicIPAddresses(); len(got) != 3 || got[2].IPAddress != "100.64.0.3" {
		t.Fatalf("public addresses = %+v", got)
	}
}
