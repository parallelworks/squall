package compute_test

import (
	"errors"
	"net/http"
	"net/netip"
	"slices"
	"strconv"
	"strings"
	"testing"

	"cloud.google.com/go/compute/apiv1/computepb"
	"google.golang.org/api/googleapi"
	"google.golang.org/api/iterator"

	"github.com/parallelworks/squall/access"
	"github.com/parallelworks/squall/fault"
	"github.com/parallelworks/squall/gcp/compute"
	"github.com/parallelworks/squall/sdktest/internal/sdkerr"
)

const netLink = "https://www.googleapis.com/compute/v1/projects/p/global/networks/vpc"

func (c *clients) insertNetwork(t *testing.T, name string) error {
	t.Helper()
	op, err := c.networks.Insert(t.Context(), &computepb.InsertNetworkRequest{
		Project: project,
		NetworkResource: &computepb.Network{
			Name: new(name), Description: new("network-id=n1"), AutoCreateSubnetworks: new(false),
			RoutingConfig: &computepb.NetworkRoutingConfig{RoutingMode: new("GLOBAL")},
		},
	})
	if err != nil {
		return err
	}
	return op.Wait(t.Context())
}

func (c *clients) insertSubnet(t *testing.T, name, cidr string) error {
	t.Helper()
	op, err := c.subnets.Insert(t.Context(), &computepb.InsertSubnetworkRequest{
		Project: project, Region: region,
		SubnetworkResource: &computepb.Subnetwork{
			Name: new(name), IpCidrRange: new(cidr), Network: new(netLink), PrivateIpGoogleAccess: new(true),
			SecondaryIpRanges: []*computepb.SubnetworkSecondaryRange{{RangeName: new("pods"), IpCidrRange: new("10.200.0.0/16")}},
		},
	})
	if err != nil {
		return err
	}
	return op.Wait(t.Context())
}

func (c *clients) insertPeeringRange(t *testing.T, name string) error {
	t.Helper()
	op, err := c.globalIPs.Insert(t.Context(), &computepb.InsertGlobalAddressRequest{
		Project: project,
		AddressResource: &computepb.Address{
			Name: new(name), Purpose: new("VPC_PEERING"), AddressType: new("INTERNAL"),
			PrefixLength: new(int32(18)), Network: new(netLink),
		},
	})
	if err != nil {
		return err
	}
	return op.Wait(t.Context())
}

func (c *clients) insertRouter(t *testing.T, name string) error {
	t.Helper()
	op, err := c.routers.Insert(t.Context(), &computepb.InsertRouterRequest{
		Project: project, Region: region,
		RouterResource: &computepb.Router{Name: new(name), Network: new(netLink)},
	})
	if err != nil {
		return err
	}
	return op.Wait(t.Context())
}

func (c *clients) patchNat(t *testing.T, router, nat string) error {
	t.Helper()
	op, err := c.routers.Patch(t.Context(), &computepb.PatchRouterRequest{
		Project: project, Region: region, Router: router,
		RouterResource: &computepb.Router{Nats: []*computepb.RouterNat{{
			Name:                          new(nat),
			SourceSubnetworkIpRangesToNat: new("ALL_SUBNETWORKS_ALL_IP_RANGES"),
			NatIpAllocateOption:           new("AUTO_ONLY"),
			LogConfig:                     &computepb.RouterNatLogConfig{Enable: new(false), Filter: new("ERRORS_ONLY")},
		}}},
	})
	if err != nil {
		return err
	}
	return op.Wait(t.Context())
}

func (c *clients) insertFirewall(t *testing.T, name, network string) error {
	t.Helper()
	op, err := c.firewalls.Insert(t.Context(), &computepb.InsertFirewallRequest{
		Project: project,
		FirewallResource: &computepb.Firewall{
			Name: new(name), Network: new(network),
			Allowed:      []*computepb.Allowed{{IPProtocol: new("tcp"), Ports: []string{"22", "8000-8080"}}},
			SourceRanges: []string{"10.0.0.0/20"}, TargetTags: []string{"ssh"},
		},
	})
	if err != nil {
		return err
	}
	return op.Wait(t.Context())
}

func (c *clients) deleteNetwork(t *testing.T, name string) error {
	t.Helper()
	op, err := c.networks.Delete(t.Context(), &computepb.DeleteNetworkRequest{Project: project, Network: name})
	if err != nil {
		return err
	}
	return op.Wait(t.Context())
}

// networkRoutes are the names of the routes on the network vpc.
func (c *clients) networkRoutes(t *testing.T) []string {
	t.Helper()
	it := c.routes.List(t.Context(), &computepb.ListRoutesRequest{Project: project, Filter: new(`network = "` + netLink + `"`)})
	var out []string
	for {
		rt, err := it.Next()
		if errors.Is(err, iterator.Done) {
			return out
		}
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, rt.GetName())
	}
}

func TestNetworkInsertGetAndDefaultRoute(t *testing.T) {
	c := setup(t)
	if err := c.insertNetwork(t, "vpc"); err != nil {
		t.Fatal(err)
	}
	if err := c.insertNetwork(t, "vpc"); sdkerr.GoogleCode(err) != http.StatusConflict || apiReason(err) != "alreadyExists" {
		t.Fatalf("inserting it again = %v, want HTTP 409 alreadyExists", err)
	}
	n, err := c.networks.Get(t.Context(), &computepb.GetNetworkRequest{Project: project, Network: "vpc"})
	if err != nil || n.GetSelfLink() != netLink || n.GetAutoCreateSubnetworks() || n.GetMtu() != 1460 ||
		n.GetRoutingConfig().GetRoutingMode() != "GLOBAL" || n.GetDescription() != "network-id=n1" {
		t.Fatalf("get = %v, %v", n, err)
	}
	byID, err := c.networks.Get(t.Context(), &computepb.GetNetworkRequest{Project: project, Network: strconv.FormatUint(n.GetId(), 10)})
	if err != nil || byID.GetName() != "vpc" {
		t.Fatalf("get by ID = %v, %v", byID, err)
	}
	routes := c.networkRoutes(t)
	if len(routes) != 1 || !strings.HasPrefix(routes[0], "default-route-") {
		t.Fatalf("routes on the network = %v, want its default route", routes)
	}
	rt, err := c.routes.Get(t.Context(), &computepb.GetRouteRequest{Project: project, Route: routes[0]})
	if err != nil || rt.GetDestRange() != "0.0.0.0/0" || !strings.HasSuffix(rt.GetNextHopGateway(), "/global/gateways/default-internet-gateway") {
		t.Fatalf("default route = %v, %v", rt, err)
	}

	for _, bad := range []*computepb.Network{
		{Name: new("auto"), AutoCreateSubnetworks: new(true)},
		{Name: new("legacy")},
		{Name: new("Bad_Name"), AutoCreateSubnetworks: new(false)},
		{Name: new("jumbo"), AutoCreateSubnetworks: new(false), Mtu: new(int32(9000))},
	} {
		_, err := c.networks.Insert(t.Context(), &computepb.InsertNetworkRequest{Project: project, NetworkResource: bad})
		if sdkerr.GoogleCode(err) != http.StatusBadRequest {
			t.Errorf("insert %v = %v, want HTTP 400", bad, err)
		}
	}

	if err := c.deleteNetwork(t, "vpc"); err != nil {
		t.Fatal(err)
	}
	if got := c.server.Routes(); len(got) != 0 {
		t.Fatalf("routes after the network went = %v, want its default route gone with it", got)
	}
	if _, err := c.networks.Get(t.Context(), &computepb.GetNetworkRequest{Project: project, Network: "vpc"}); sdkerr.GoogleCode(err) != http.StatusNotFound {
		t.Fatalf("get after delete = %v, want HTTP 404", err)
	}
	if err := c.deleteNetwork(t, "vpc"); sdkerr.GoogleCode(err) != http.StatusNotFound || apiReason(err) != "notFound" {
		t.Fatalf("deleting it again = %v, want HTTP 404 notFound", err)
	}
}

func TestSubnetworks(t *testing.T) {
	c := setup(t)
	if err := c.insertSubnet(t, "sub", "10.0.0.0/20"); sdkerr.GoogleCode(err) != http.StatusNotFound {
		t.Fatalf("a subnetwork of a missing network = %v, want HTTP 404", err)
	}
	if err := c.insertNetwork(t, "vpc"); err != nil {
		t.Fatal(err)
	}
	if err := c.insertSubnet(t, "sub", "10.0.0.0/20"); err != nil {
		t.Fatal(err)
	}
	sn, err := c.subnets.Get(t.Context(), &computepb.GetSubnetworkRequest{Project: project, Region: region, Subnetwork: "sub"})
	if err != nil || sn.GetNetwork() != netLink || sn.GetGatewayAddress() != "10.0.0.1" || !sn.GetPrivateIpGoogleAccess() ||
		len(sn.GetSecondaryIpRanges()) != 1 || sn.GetRegion() != "https://www.googleapis.com/compute/v1/projects/p/regions/us-east1" {
		t.Fatalf("get = %v, %v", sn, err)
	}
	if err := c.insertSubnet(t, "sub", "10.1.0.0/20"); sdkerr.GoogleCode(err) != http.StatusConflict {
		t.Fatalf("a name in use = %v, want HTTP 409", err)
	}
	for _, cidr := range []string{"10.0.8.0/24", "10.0.0.1/20", "not-a-cidr", "10.0.0.0/30"} {
		if err := c.insertSubnet(t, "other", cidr); sdkerr.GoogleCode(err) != http.StatusBadRequest {
			t.Errorf("a subnetwork with %s = %v, want HTTP 400", cidr, err)
		}
	}
	n, _ := c.server.LookupNetwork(project, "vpc")
	if !slices.Equal(n.Subnetworks, []string{sn.GetSelfLink()}) {
		t.Errorf("network subnetworks = %v", n.Subnetworks)
	}

	it := c.subnets.AggregatedList(t.Context(), &computepb.AggregatedListSubnetworksRequest{Project: project, Filter: new(`network = "` + netLink + `"`)})
	found := 0
	for {
		pair, err := it.Next()
		if errors.Is(err, iterator.Done) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		found += len(pair.Value.GetSubnetworks())
	}
	if found != 1 {
		t.Fatalf("aggregated subnetworks on the network = %d, want 1", found)
	}

	op, err := c.subnets.Delete(t.Context(), &computepb.DeleteSubnetworkRequest{Project: project, Region: region, Subnetwork: "sub"})
	if err == nil {
		err = op.Wait(t.Context())
	}
	if err != nil || len(c.server.Subnetworks()) != 0 {
		t.Fatalf("delete = %v, subnetworks %v", err, c.server.Subnetworks())
	}
}

func TestRouterWithCloudNat(t *testing.T) {
	c := setup(t)
	if err := c.insertNetwork(t, "vpc"); err != nil {
		t.Fatal(err)
	}
	if err := c.insertRouter(t, "nat-router"); err != nil {
		t.Fatal(err)
	}
	if err := c.patchNat(t, "nat-router", "nat"); err != nil {
		t.Fatal(err)
	}
	rt, err := c.routers.Get(t.Context(), &computepb.GetRouterRequest{Project: project, Region: region, Router: "nat-router"})
	if err != nil || rt.GetNetwork() != netLink || len(rt.GetNats()) != 1 || rt.GetNats()[0].GetNatIpAllocateOption() != "AUTO_ONLY" ||
		rt.GetNats()[0].GetLogConfig().GetFilter() != "ERRORS_ONLY" {
		t.Fatalf("get = %v, %v", rt, err)
	}
	op, err := c.routers.Patch(t.Context(), &computepb.PatchRouterRequest{
		Project: project, Region: region, Router: "nat-router",
		RouterResource: &computepb.Router{Nats: []*computepb.RouterNat{{Name: new("nat"), SourceSubnetworkIpRangesToNat: new("ALL_SUBNETWORKS_ALL_IP_RANGES"), NatIpAllocateOption: new("MANUAL_ONLY")}}},
	})
	if sdkerr.GoogleCode(err) != http.StatusBadRequest {
		t.Fatalf("a MANUAL_ONLY NAT without IPs = %v, %v, want HTTP 400", op, err)
	}

	it := c.routers.AggregatedList(t.Context(), &computepb.AggregatedListRoutersRequest{Project: project, Filter: new(`network = "` + netLink + `"`)})
	pair, err := it.Next()
	if err != nil || pair.Key != "regions/us-east1" || len(pair.Value.GetRouters()) != 1 {
		t.Fatalf("aggregated routers = %v, %v", pair, err)
	}

	op, err = c.routers.Delete(t.Context(), &computepb.DeleteRouterRequest{Project: project, Region: region, Router: "nat-router"})
	if err == nil {
		err = op.Wait(t.Context())
	}
	if err != nil || len(c.server.Routers()) != 0 {
		t.Fatalf("delete = %v, routers %v", err, c.server.Routers())
	}
	if err := c.insertRouter(t, "nat-router"); err != nil {
		t.Fatal(err)
	}
	if _, err := c.routers.Get(t.Context(), &computepb.GetRouterRequest{Project: project, Region: region, Router: "missing"}); sdkerr.GoogleCode(err) != http.StatusNotFound {
		t.Fatalf("get of a missing router = %v, want HTTP 404", err)
	}
}

func TestPeeringRangeIsAllocatedClearOfSubnetworks(t *testing.T) {
	c := setup(t)
	if err := c.insertNetwork(t, "vpc"); err != nil {
		t.Fatal(err)
	}
	if err := c.insertSubnet(t, "sub", "10.0.0.0/20"); err != nil {
		t.Fatal(err)
	}
	if err := c.insertPeeringRange(t, "psa"); err != nil {
		t.Fatal(err)
	}
	a, err := c.globalIPs.Get(t.Context(), &computepb.GetGlobalAddressRequest{Project: project, Address: "psa"})
	if err != nil || a.GetPurpose() != "VPC_PEERING" || a.GetPrefixLength() != 18 || a.GetNetwork() != netLink || a.GetRegion() != "" {
		t.Fatalf("get = %v, %v", a, err)
	}
	got := netip.PrefixFrom(netip.MustParseAddr(a.GetAddress()), 18)
	for _, used := range []string{"10.0.0.0/20", "10.200.0.0/16"} {
		if got.Overlaps(netip.MustParsePrefix(used)) {
			t.Fatalf("allocated %s overlaps the subnetwork's %s", got, used)
		}
	}
	if err := c.insertSubnet(t, "inside", got.String()); sdkerr.GoogleCode(err) != http.StatusBadRequest {
		t.Fatalf("a subnetwork inside the allocated range = %v, want HTTP 400", err)
	}
	if _, err := c.globalIPs.SetLabels(t.Context(), &computepb.SetLabelsGlobalAddressRequest{
		Project: project, Resource: "psa",
		GlobalSetLabelsRequestResource: &computepb.GlobalSetLabelsRequest{LabelFingerprint: a.LabelFingerprint, Labels: map[string]string{"owner": "n1"}},
	}); err != nil {
		t.Fatal(err)
	}
	if got := c.server.GlobalAddresses(); len(got) != 1 || got[0].Labels["owner"] != "n1" {
		t.Fatalf("GlobalAddresses() = %+v", got)
	}
}

func TestDeletingANetworkInUseIsRefused(t *testing.T) {
	c := setup(t)
	if err := c.insertNetwork(t, "vpc"); err != nil {
		t.Fatal(err)
	}
	if err := c.insertFirewall(t, "allow-ssh", "missing"); sdkerr.GoogleCode(err) != http.StatusNotFound {
		t.Fatalf("a firewall on a missing network = %v, want HTTP 404", err)
	}
	for _, step := range []func() error{
		func() error { return c.insertSubnet(t, "sub", "10.0.0.0/20") },
		func() error { return c.insertRouter(t, "nat-router") },
		func() error { return c.insertFirewall(t, "allow-ssh", netLink) },
		func() error { return c.insertPeeringRange(t, "psa") },
	} {
		if err := step(); err != nil {
			t.Fatal(err)
		}
	}
	refusedBy := func(user string) {
		t.Helper()
		err := c.deleteNetwork(t, "vpc")
		gerr, ok := errors.AsType[*googleapi.Error](err)
		if !ok || gerr.Code != http.StatusBadRequest || apiReason(err) != "resourceInUseByAnotherResource" || !strings.Contains(gerr.Message, "'"+user+"'") {
			t.Fatalf("delete = %v, want HTTP 400 resourceInUseByAnotherResource naming %s", err, user)
		}
	}
	refusedBy("projects/p/regions/us-east1/subnetworks/sub")
	if op, err := c.subnets.Delete(t.Context(), &computepb.DeleteSubnetworkRequest{Project: project, Region: region, Subnetwork: "sub"}); err != nil || op.Wait(t.Context()) != nil {
		t.Fatal(err)
	}
	refusedBy("projects/p/regions/us-east1/routers/nat-router")
	if op, err := c.routers.Delete(t.Context(), &computepb.DeleteRouterRequest{Project: project, Region: region, Router: "nat-router"}); err != nil || op.Wait(t.Context()) != nil {
		t.Fatal(err)
	}
	refusedBy("projects/p/global/firewalls/allow-ssh")
	if op, err := c.firewalls.Delete(t.Context(), &computepb.DeleteFirewallRequest{Project: project, Firewall: "allow-ssh"}); err != nil || op.Wait(t.Context()) != nil {
		t.Fatal(err)
	}
	refusedBy("projects/p/global/addresses/psa")
	if op, err := c.globalIPs.Delete(t.Context(), &computepb.DeleteGlobalAddressRequest{Project: project, Address: "psa"}); err != nil || op.Wait(t.Context()) != nil {
		t.Fatal(err)
	}
	if err := c.deleteNetwork(t, "vpc"); err != nil {
		t.Fatal(err)
	}
	if n := c.server.Networks(); len(n) != 0 {
		t.Fatalf("Networks() = %v", n)
	}
}

func TestDeletingASubnetworkAnInstanceUsesIsRefused(t *testing.T) {
	c := setup(t)
	if err := c.insertNetwork(t, "vpc"); err != nil {
		t.Fatal(err)
	}
	if err := c.insertSubnet(t, "sub", "10.0.0.0/20"); err != nil {
		t.Fatal(err)
	}
	if err := c.insert(t, single("vm", "s")); err != nil {
		t.Fatal(err)
	}
	_, err := c.subnets.Delete(t.Context(), &computepb.DeleteSubnetworkRequest{Project: project, Region: region, Subnetwork: "sub"})
	gerr, ok := errors.AsType[*googleapi.Error](err)
	if !ok || gerr.Code != http.StatusBadRequest || apiReason(err) != "resourceInUseByAnotherResource" ||
		gerr.Message != "The subnetwork resource 'projects/p/regions/us-east1/subnetworks/sub' is already being used by 'projects/p/zones/us-east1-b/instances/vm'" {
		t.Fatalf("delete = %v, want HTTP 400 resourceInUseByAnotherResource naming the instance", err)
	}
}

func TestFirewallsListByNetwork(t *testing.T) {
	c := setup(t)
	if err := c.insertNetwork(t, "vpc"); err != nil {
		t.Fatal(err)
	}
	if err := c.insertNetwork(t, "other"); err != nil {
		t.Fatal(err)
	}
	for _, f := range [][2]string{{"a", netLink}, {"b", "global/networks/other"}, {"c", "vpc"}} {
		if err := c.insertFirewall(t, f[0], f[1]); err != nil {
			t.Fatal(err)
		}
	}
	it := c.firewalls.List(t.Context(), &computepb.ListFirewallsRequest{Project: project, Filter: new(`network = "` + netLink + `"`)})
	var names []string
	for {
		f, err := it.Next()
		if errors.Is(err, iterator.Done) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		names = append(names, f.GetName())
	}
	if !slices.Equal(names, []string{"a", "c"}) {
		t.Fatalf("firewalls on vpc = %v", names)
	}
	f, err := c.firewalls.Get(t.Context(), &computepb.GetFirewallRequest{Project: project, Firewall: "a"})
	if err != nil || f.GetDirection() != "INGRESS" || f.GetPriority() != 1000 || f.GetAllowed()[0].GetIPProtocol() != "tcp" || f.GetAllowed()[0].GetPorts()[1] != "8000-8080" {
		t.Fatalf("get = %v, %v", f, err)
	}
	if err := c.insertFirewall(t, "a", netLink); sdkerr.GoogleCode(err) != http.StatusConflict {
		t.Fatalf("a name in use = %v, want HTTP 409", err)
	}
}

func TestNetworkFaults(t *testing.T) {
	faults := fault.New().Add(
		fault.Rule{Method: "compute.networks.insert", Resource: "projects/p/global/networks/vpc", Nth: 1, Fault: fault.LoseResponse()},
		fault.Rule{Method: "compute.routers.patch", Nth: 1, Fault: fault.FailOperation(http.StatusServiceUnavailable)},
	)
	c := setup(t, compute.WithFaults(faults))
	if err := c.insertNetwork(t, "vpc"); err == nil || sdkerr.GoogleCode(err) != 0 {
		t.Fatalf("insert = %v, want a transport error", err)
	}
	if _, ok := c.server.LookupNetwork(project, "vpc"); !ok {
		t.Fatal("the insert whose response was lost did not take effect")
	}
	if err := c.insertRouter(t, "nat-router"); err != nil {
		t.Fatal(err)
	}
	if err := c.patchNat(t, "nat-router", "nat"); err == nil {
		t.Fatal("a patch whose operation failed succeeded")
	}
	if rt := c.server.Routers(); len(rt[0].Nats) != 0 {
		t.Fatalf("a failed patch changed the router: %+v", rt[0].Nats)
	}
	if err := c.patchNat(t, "nat-router", "nat"); err != nil {
		t.Fatal(err)
	}
	if got := faults.Injected(); len(got) != 2 {
		t.Fatalf("Injected = %v", got)
	}
}

func TestNetworkPermissions(t *testing.T) {
	policy := access.Allow("compute.networks.get", "compute.globalOperations.get")
	c := setup(t, compute.WithPolicy(policy))
	err := c.insertNetwork(t, "vpc")
	gerr, ok := errors.AsType[*googleapi.Error](err)
	if !ok || gerr.Code != http.StatusForbidden || !strings.Contains(gerr.Message, "Required 'compute.networks.create' permission for 'projects/p/global/networks/vpc'") {
		t.Fatalf("insert = %v, want 403 naming compute.networks.create", err)
	}
}
