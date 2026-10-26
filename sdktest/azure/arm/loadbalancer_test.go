package arm_test

import (
	"net/http"
	"strings"
	"testing"

	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/compute/armcompute/v8"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/network/armnetwork/v9"

	"github.com/parallelworks/squall/access"
	"github.com/parallelworks/squall/azure/arm"
)

const lbID = "/subscriptions/" + subscription + "/resourceGroups/" + group + "/providers/Microsoft.Network/loadBalancers/lb"

// lbBody is a public load balancer with one frontend on each of pips, a
// pool and an outbound rule that gives each instance in the pool ports
// through every frontend.
func lbBody(ports int32, pips ...*string) armnetwork.LoadBalancer {
	body := armnetwork.LoadBalancer{
		Location: new(location),
		SKU:      &armnetwork.LoadBalancerSKU{Name: new(armnetwork.LoadBalancerSKUNameStandard)},
		Properties: &armnetwork.LoadBalancerPropertiesFormat{
			BackendAddressPools: []*armnetwork.BackendAddressPool{{Name: new("pool")}},
			OutboundRules: []*armnetwork.OutboundRule{{
				Name: new("outbound"),
				Properties: &armnetwork.OutboundRulePropertiesFormat{
					AllocatedOutboundPorts: new(ports),
					Protocol:               new(armnetwork.LoadBalancerOutboundRuleProtocolAll),
					BackendAddressPool:     &armnetwork.SubResource{ID: new(lbID + "/backendAddressPools/pool")},
				},
			}},
		},
	}
	rule := body.Properties.OutboundRules[0].Properties
	for i, pip := range pips {
		name := "frontend" + string(rune('a'+i))
		body.Properties.FrontendIPConfigurations = append(body.Properties.FrontendIPConfigurations, &armnetwork.FrontendIPConfiguration{
			Name: new(name), Properties: &armnetwork.FrontendIPConfigurationPropertiesFormat{PublicIPAddress: &armnetwork.PublicIPAddress{ID: pip}},
		})
		rule.FrontendIPConfigurations = append(rule.FrontendIPConfigurations, &armnetwork.SubResource{ID: new(lbID + "/frontendIPConfigurations/" + name)})
	}
	return body
}

func (c *clients) putLB(t *testing.T, body armnetwork.LoadBalancer) (armnetwork.LoadBalancer, error) {
	t.Helper()
	p, err := c.lbs.BeginCreateOrUpdate(t.Context(), group, "lb", body, nil)
	res, err := wait(t, p, err)
	return res.LoadBalancer, err
}

// pooledScaleSet is a scale set of capacity VMs whose primary IP
// configuration joins the backend address pool pool.
func pooledScaleSet(capacity int64, pool string) armcompute.VirtualMachineScaleSet {
	body := scaleSetBody(new(capacity))
	ip := body.Properties.VirtualMachineProfile.NetworkProfile.NetworkInterfaceConfigurations[0].Properties.IPConfigurations[0]
	ip.Properties.LoadBalancerBackendAddressPools = []*armcompute.SubResource{{ID: new(pool)}}
	return body
}

func (c *clients) putScaleSet(t *testing.T, body armcompute.VirtualMachineScaleSet) error {
	t.Helper()
	p, err := c.sets.BeginCreateOrUpdate(t.Context(), group, "set", body, nil)
	_, err = wait(t, p, err)
	return err
}

func TestLoadBalancerOutboundRulesLimitTheScaleSetsInTheirPool(t *testing.T) {
	c := setup(t)
	c.createGroup(t, group, nil)
	pip := c.createPIP(t, "lb-ip")
	lb, err := c.putLB(t, lbBody(16_000, pip.ID))
	if err != nil {
		t.Fatal(err)
	}
	rule := lb.Properties.OutboundRules[0].Properties
	if *lb.SKU.Name != armnetwork.LoadBalancerSKUNameStandard || *lb.SKU.Tier != armnetwork.LoadBalancerSKUTierRegional ||
		*rule.AllocatedOutboundPorts != 16_000 || *rule.IdleTimeoutInMinutes != 4 || *rule.BackendAddressPool.ID != lbID+"/backendAddressPools/pool" ||
		*lb.Properties.FrontendIPConfigurations[0].Properties.PublicIPAddress.ID != *pip.ID {
		t.Fatalf("load balancer = %+v", lb.Properties)
	}
	gotPIP, err := c.pips.Get(t.Context(), group, "lb-ip", nil)
	if err != nil || gotPIP.Properties.IPConfiguration == nil || *gotPIP.Properties.IPConfiguration.ID != lbID+"/frontendIPConfigurations/frontenda" {
		t.Fatalf("public IP = %+v, %v", gotPIP.Properties, err)
	}
	if _, err := c.putLB(t, lbBody(1_004, pip.ID)); code(err) != "SpecifiedAllocatedOutboundPortsForOutboundRuleIsNotAMultipleOfEight" || status(err) != http.StatusBadRequest {
		t.Fatalf("ports not a multiple of 8 = %v", err)
	}

	pool := lbID + "/backendAddressPools/pool"
	if err := c.putScaleSet(t, pooledScaleSet(4, pool)); err != nil {
		t.Fatal(err)
	}
	got, err := c.lbs.Get(t.Context(), group, "lb", nil)
	if err != nil || len(got.Properties.BackendAddressPools[0].Properties.BackendIPConfigurations) != 4 ||
		!strings.HasSuffix(*got.Properties.BackendAddressPools[0].Properties.BackendIPConfigurations[0].ID, "/virtualMachineScaleSets/set/virtualMachines/0/networkInterfaces/nic/ipConfigurations/ip") {
		t.Fatalf("pool members = %+v, %v", got.Properties, err)
	}
	snapshot := c.server.LoadBalancers()
	if len(snapshot) != 1 || len(snapshot[0].BackendPools[0].Members) != 4 || snapshot[0].OutboundRules[0].AllocatedOutboundPorts != 16_000 ||
		snapshot[0].OutboundRules[0].PublicIPAddresses[0] != *pip.ID {
		t.Fatalf("LoadBalancers() = %+v", snapshot)
	}

	// Four VMs take 64,000 ports, all one public IP address gives; a fifth needs more.
	const tooMany = "SpecifiedAllocatedOutboundPortsForOutboundRuleExceedsTotalNumberOfAvailablePorts"
	p, err := c.sets.BeginUpdate(t.Context(), group, "set", armcompute.VirtualMachineScaleSetUpdate{SKU: &armcompute.SKU{Capacity: new(int64(5))}}, nil)
	if _, err = wait(t, p, err); code(err) != tooMany || !strings.Contains(err.Error(), "exceeds total number of available ports 12800") {
		t.Fatalf("scaling out past the ports = %v", err)
	}
	if err := c.putScaleSet(t, pooledScaleSet(5, pool)); code(err) != tooMany {
		t.Fatalf("putting the scale set past the ports = %v", err)
	}
	if sets := c.server.ScaleSets(); sets[0].Capacity != 4 {
		t.Fatalf("capacity after the refusals = %d", sets[0].Capacity)
	}
	if _, err := c.putLB(t, lbBody(16_008, pip.ID)); code(err) != tooMany {
		t.Fatalf("raising the ports past what the pool allows = %v", err)
	}
	second := c.createPIP(t, "lb-ip-2")
	if _, err := c.putLB(t, lbBody(16_000, pip.ID, second.ID)); err != nil {
		t.Fatal(err)
	}
	p, err = c.sets.BeginUpdate(t.Context(), group, "set", armcompute.VirtualMachineScaleSetUpdate{SKU: &armcompute.SKU{Capacity: new(int64(8))}}, nil)
	if _, err = wait(t, p, err); err != nil {
		t.Fatalf("scaling out with a second public IP address = %v", err)
	}

	dp, err := c.lbs.BeginDelete(t.Context(), group, "lb", nil)
	if _, err = wait(t, dp, err); code(err) != "LoadBalancerInUseByVirtualMachineScaleSet" || status(err) != http.StatusBadRequest {
		t.Fatalf("deleting a load balancer in use = %v", err)
	}
	pp, err := c.pips.BeginDelete(t.Context(), group, "lb-ip", nil)
	if _, err = wait(t, pp, err); code(err) != "PublicIPAddressCannotBeDeleted" {
		t.Fatalf("deleting a frontend's public IP address = %v", err)
	}
	if _, err := c.createNAT(t, "nat", pip.ID); code(err) != "PublicIPAddressInUse" {
		t.Fatalf("a NAT gateway taking a frontend's public IP address = %v", err)
	}

	gp, err := c.groups.BeginDelete(t.Context(), group, nil)
	if _, err = wait(t, gp, err); err != nil {
		t.Fatal(err)
	}
	if left := c.server.Resources(); len(left) != 0 {
		t.Fatalf("resources left = %+v", left)
	}
}

func TestScaleSetPoolReferencesAreChecked(t *testing.T) {
	c := setup(t)
	c.createGroup(t, group, nil)
	pip := c.createPIP(t, "lb-ip")
	if _, err := c.putLB(t, lbBody(0, pip.ID)); err != nil {
		t.Fatal(err)
	}
	if err := c.putScaleSet(t, pooledScaleSet(1, lbID+"/backendAddressPools/none")); code(err) != "InvalidResourceReference" || status(err) != http.StatusBadRequest {
		t.Fatalf("a scale set joining a missing pool = %v", err)
	}
	// A rule with the default allocation does not limit the pool.
	if err := c.putScaleSet(t, pooledScaleSet(80, lbID+"/backendAddressPools/pool")); err != nil {
		t.Fatal(err)
	}
	lb, err := c.lbs.Get(t.Context(), group, "lb", nil)
	if err != nil || *lb.Properties.OutboundRules[0].Properties.AllocatedOutboundPorts != 512 {
		t.Fatalf("default allocation for 80 instances = %+v, %v", lb.Properties.OutboundRules[0].Properties, err)
	}

	nic := nicBody(nil, nil)
	ip := nic.Properties.IPConfigurations[0].Properties
	ip.LoadBalancerBackendAddressPools = []*armnetwork.BackendAddressPool{{ID: new(lbID + "/backendAddressPools/none")}}
	p, err := c.nics.BeginCreateOrUpdate(t.Context(), group, "nic", nic, nil)
	if _, err = wait(t, p, err); code(err) != "InvalidResourceReference" {
		t.Fatalf("an interface joining a missing pool = %v", err)
	}
	ip.LoadBalancerBackendAddressPools[0].ID = new(lbID + "/backendAddressPools/pool")
	p, err = c.nics.BeginCreateOrUpdate(t.Context(), group, "nic", nic, nil)
	if _, err = wait(t, p, err); err != nil {
		t.Fatal(err)
	}
	if members := c.server.LoadBalancers()[0].BackendPools[0].Members; len(members) != 81 || !strings.HasSuffix(members[80], "/networkInterfaces/nic/ipConfigurations/ipconfig1") {
		t.Fatalf("pool members = %v", members)
	}
	if err := c.putScaleSet(t, pooledScaleSet(80, lbID+"/backendAddressPools/pool")); err != nil {
		t.Fatal(err)
	}
	body := lbBody(0, new(lbID))
	body.Properties.FrontendIPConfigurations[0].Properties.PublicIPAddress.ID = lb.Properties.FrontendIPConfigurations[0].Properties.PublicIPAddress.ID
	body.Properties.BackendAddressPools, body.Properties.OutboundRules = nil, nil
	if _, err := c.putLB(t, body); code(err) != "InUseBackendAddressPoolCannotBeDeleted" {
		t.Fatalf("dropping a pool in use = %v", err)
	}
}

func TestPolicyChecksWhatLoadBalancersLinkTo(t *testing.T) {
	actions := []string{
		"Microsoft.Resources/subscriptions/resourceGroups/write",
		"Microsoft.Network/publicIPAddresses/write",
		"Microsoft.Network/loadBalancers/write",
		"Microsoft.Compute/virtualMachineScaleSets/write",
		"Microsoft.Network/virtualNetworks/subnets/join/action",
	}
	c := setup(t, arm.WithPolicy(access.Allow(actions...)))
	c.createGroup(t, group, nil)
	pip := c.createPIP(t, "lb-ip")
	if _, err := c.putLB(t, lbBody(1_024, pip.ID)); code(err) != "LinkedAuthorizationFailed" ||
		!strings.Contains(err.Error(), "'Microsoft.Network/publicIPAddresses/join/action' on the linked scope(s) '"+*pip.ID+"'") {
		t.Fatalf("a load balancer without join on its public IP address = %v", err)
	}

	c = setup(t, arm.WithPolicy(access.Allow(append(actions, "Microsoft.Network/publicIPAddresses/join/action")...)))
	c.createGroup(t, group, nil)
	pip = c.createPIP(t, "lb-ip")
	if _, err := c.putLB(t, lbBody(1_024, pip.ID)); err != nil {
		t.Fatal(err)
	}
	pool := lbID + "/backendAddressPools/pool"
	if err := c.putScaleSet(t, pooledScaleSet(1, pool)); code(err) != "LinkedAuthorizationFailed" ||
		!strings.Contains(err.Error(), "'Microsoft.Network/loadBalancers/backendAddressPools/join/action' on the linked scope(s) '"+pool+"'") {
		t.Fatalf("a scale set without join on its pool = %v", err)
	}
}
