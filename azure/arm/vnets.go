package arm

import (
	"cmp"
	"fmt"
	"maps"
	"net/http"
	"net/netip"
	"slices"
	"strconv"
	"strings"
)

var (
	vnetKind = &kind{
		typ: "Microsoft.Network/virtualNetworks", plural: "virtualNetworks", typeName: "VirtualNetwork",
		top: []string{"extendedLocation"},
		props: []string{
			"addressSpace", "bgpCommunities", "ddosProtectionPlan", "dhcpOptions", "enableDdosProtection", "enableVmProtection",
			"encryption", "flowTimeoutInMinutes", "ipAllocations", "privateEndpointVNetPolicies", "subnets", "virtualNetworkPeerings",
		},
		readOnly: []string{"defaultPublicNatGateway", "flowLogs", "provisioningState", "resourceGuid"},
		located:  true, etag: weakETag, opName: "VirtualNetwork",
	}
	subnetKind = &kind{
		typ: "Microsoft.Network/virtualNetworks/subnets", plural: "subnets", typeName: "Subnet",
		props: []string{
			"addressPrefix", "addressPrefixes", "applicationGatewayIPConfigurations", "defaultOutboundAccess", "delegations",
			"ipAllocations", "ipamPoolPrefixAllocations", "natGateway", "networkSecurityGroup", "privateEndpointNetworkPolicies",
			"privateLinkServiceNetworkPolicies", "routeTable", "serviceEndpointPolicies", "serviceEndpoints", "serviceGateway",
			"sharingScope",
		},
		readOnly: []string{
			"ipConfigurationProfiles", "ipConfigurations", "privateEndpoints", "provisioningState", "purpose",
			"resourceNavigationLinks", "serviceAssociationLinks",
		},
		etag: weakETag, opName: "Subnet",
	}
	natKind = &kind{
		typ: "Microsoft.Network/natGateways", plural: "natGateways", typeName: "NatGateway",
		top: []string{"sku", "zones"},
		props: []string{
			"idleTimeoutInMinutes", "publicIpAddresses", "publicIpAddressesV6", "publicIpPrefixes", "publicIpPrefixesV6",
			"serviceGateway", "sourceVirtualNetwork",
		},
		readOnly: []string{"provisioningState", "resourceGuid", "subnets"},
		located:  true, etag: weakETag, opName: "NatGateway",
	}
)

const delegationType = "Microsoft.Network/virtualNetworks/subnets/delegations"

// vnetState is what a virtual network holds beyond its body.
type vnetState struct {
	space []netip.Prefix
}

// subnetState is what a subnet holds beyond its body.
type subnetState struct {
	prefixes []netip.Prefix
	// pool gives the addresses of what joins the subnet, from its first prefix.
	pool     *pool
	nat, nsg string
}

func vnetID(c *call) string { return resourceID(c, vnetKind.typ, c.val("vnet")) }

// networkID is the ID of the network resource of kind k a call names.
func networkID(c *call, k *kind) string {
	if k == subnetKind {
		return vnetID(c) + "/subnets/" + c.val("name")
	}
	return resourceID(c, k.typ, c.val("name"))
}

// parentVNet is the ID of the virtual network that holds the subnet id.
func parentVNet(subnet string) string {
	return subnet[:strings.LastIndex(key(subnet), "/subnets/")]
}

// subnetsOf returns the subnets of the virtual network vnet, ordered by name. The caller holds s.mu.
func (s *Server) subnetsOf(vnet *resource) []*resource {
	prefix := key(vnet.id) + "/subnets/"
	var out []*resource
	for _, res := range s.resources {
		if res.kind == subnetKind && strings.HasPrefix(key(res.id), prefix) {
			out = append(out, res)
		}
	}
	slices.SortFunc(out, func(a, b *resource) int { return cmp.Compare(key(a.name), key(b.name)) })
	return out
}

// subnetsUsing returns the IDs of the subnets whose NAT gateway or network
// security group, as field picks, is id. The caller holds s.mu.
func (s *Server) subnetsUsing(id string, field func(*subnetState) string) []string {
	var out []string
	for _, res := range s.resources {
		if res.subnet != nil && key(field(res.subnet)) == key(id) {
			out = append(out, res.id)
		}
	}
	slices.Sort(out)
	return out
}

func subnetNAT(st *subnetState) string { return st.nat }

func subnetNSG(st *subnetState) string { return st.nsg }

// subnetUser returns what holds an address in the subnet id, or "". The caller holds s.mu.
func (s *Server) subnetUser(id string) string {
	holders := slices.Sorted(maps.Values(s.privateIPs[key(id)]))
	if len(holders) == 0 {
		return ""
	}
	return holders[0]
}

// subnetPool returns the range addresses in subnet come from: the subnet's
// own, when the server holds it, or the private range. The caller holds s.mu.
func (s *Server) subnetPool(subnet string) *pool {
	if res := s.resources[key(subnet)]; res != nil && res.subnet != nil {
		return res.subnet.pool
	}
	return s.private
}

// strs returns m[key] as an array of strings.
func strs(m map[string]any, key string) ([]string, *apiError) {
	v, ok := m[key]
	if !ok || v == nil {
		return nil, nil
	}
	arr, ok := v.([]any)
	if !ok {
		return nil, invalidContent(key, fmt.Errorf("expected an array, not %T", v))
	}
	out := make([]string, 0, len(arr))
	for i, e := range arr {
		s, ok := e.(string)
		if !ok {
			return nil, invalidContent(fmt.Sprintf("%s[%d]", key, i), fmt.Errorf("expected a string, not %T", e))
		}
		out = append(out, s)
	}
	return out, nil
}

// parsePrefixes reads the IPv4 prefixes of the resource id.
func parsePrefixes(values []string, id string) ([]netip.Prefix, *apiError) {
	out := make([]netip.Prefix, 0, len(values))
	for _, v := range values {
		p, err := netip.ParsePrefix(v)
		if err != nil || p != p.Masked() {
			return nil, newError(http.StatusBadRequest, "InvalidCIDRNotation",
				"The address prefix %s in resource %s has an invalid CIDR notation.", v, id)
		}
		if !p.Addr().Is4() {
			return nil, unsupported("address prefix %s is not IPv4", v)
		}
		out = append(out, p)
	}
	return out, nil
}

func within(p netip.Prefix, space []netip.Prefix) bool {
	for _, sp := range space {
		if sp.Bits() <= p.Bits() && sp.Contains(p.Addr()) {
			return true
		}
	}
	return false
}

func overlap(a, b []netip.Prefix) bool {
	for _, x := range a {
		if slices.ContainsFunc(b, x.Overlaps) {
			return true
		}
	}
	return false
}

func subnetOverlaps(name, vnet string) *apiError {
	return newError(http.StatusBadRequest, "NetcfgSubnetRangesOverlap",
		"Subnet '%s' is not valid because its IP address range overlaps with that of an existing subnet in virtual network '%s'.", name, vnet)
}

func subnetInUse(name, user string) *apiError {
	return newError(http.StatusBadRequest, "InUseSubnetCannotBeDeleted",
		"Subnet %s is in use by %s and cannot be deleted. In order to delete the subnet, delete all the resources within the subnet. See aka.ms/deletesubnet.",
		name, user)
}

// parseSubnet validates props as the body of the subnet id named name in the
// virtual network vnet, whose address space is space, and returns what the
// subnet holds. It takes natGateway and networkSecurityGroup out of props.
// The caller holds s.mu.
func (s *Server) parseSubnet(vnet *resource, space []netip.Prefix, id, name string, props map[string]any) (*subnetState, *apiError) {
	values, err := strs(props, "addressPrefixes")
	if err != nil {
		return nil, err
	}
	if len(values) == 0 {
		if v := str(props, "addressPrefix"); v != "" {
			values = []string{v}
		}
	}
	if len(values) == 0 {
		return nil, unsupported("subnet %s needs addressPrefix or addressPrefixes", id)
	}
	prefixes, err := parsePrefixes(values, id)
	if err != nil {
		return nil, err
	}
	for _, p := range prefixes {
		if p.Bits() > 29 {
			return nil, newError(http.StatusBadRequest, "NetcfgInvalidSubnet", "Subnet '%s' is not valid in virtual network '%s'.", name, vnet.name)
		}
		if !within(p, space) {
			return nil, newError(http.StatusBadRequest, "NetcfgSubnetRangeOutsideVnet",
				"Subnet '%s' is not valid because its IP address range is outside the IP address range of virtual network '%s'.", name, vnet.name)
		}
	}
	st := &subnetState{prefixes: prefixes, nat: refID(props, "natGateway"), nsg: refID(props, "networkSecurityGroup")}
	delete(props, "natGateway")
	delete(props, "networkSecurityGroup")
	for _, r := range []struct {
		id string
		k  *kind
	}{{st.nat, natKind}, {st.nsg, nsgKind}} {
		if r.id == "" {
			continue
		}
		if target := s.settled(r.id); target == nil || target.kind != r.k || target.loc != vnet.loc {
			return nil, invalidReference(r.id, id)
		}
	}
	if _, err := objects(props, "delegations"); err != nil {
		return nil, err
	}
	if _, err := objects(props, "serviceEndpoints"); err != nil {
		return nil, err
	}
	st.pool = newPool(prefixes[0], 4, 1)
	if old := s.resources[key(id)]; old != nil && old.subnet != nil {
		if old.subnet.pool.prefix == st.pool.prefix {
			st.pool = old.subnet.pool
		}
		if !slices.Equal(old.subnet.prefixes, prefixes) && s.subnetUser(id) != "" {
			return nil, newError(http.StatusBadRequest, "InUseSubnetCannotBeUpdated", "Subnet %s is in use and cannot be updated.", name)
		}
	}
	return st, nil
}

// prepareSubnet validates p as the new body of the subnet res, and returns
// how to apply it. The caller holds s.mu.
func (s *Server) prepareSubnet(res *resource, p *parsed) (func(), *apiError) {
	vnet := s.resources[key(parentVNet(res.id))]
	st, err := s.parseSubnet(vnet, vnet.vnet.space, res.id, res.name, p.props)
	if err != nil {
		return nil, err
	}
	for _, other := range s.subnetsOf(vnet) {
		if other != res && overlap(st.prefixes, other.subnet.prefixes) {
			return nil, subnetOverlaps(res.name, vnet.name)
		}
	}
	return func() {
		res.subnet = st
		s.update(res, p)
	}, nil
}

// prepareVNet validates p as the new body of the virtual network res, and
// returns how to apply it. The subnets p lists replace those res has: the
// ones it leaves out are deleted. The caller holds s.mu.
func (s *Server) prepareVNet(res *resource, p *parsed) (func(), *apiError) {
	addressSpace, err := object(p.props, "addressSpace")
	if err != nil {
		return nil, err
	}
	values, err := strs(addressSpace, "addressPrefixes")
	if err != nil {
		return nil, err
	}
	space, err := parsePrefixes(values, res.id)
	if err != nil {
		return nil, err
	}
	raw, err := objects(p.props, "subnets")
	if err != nil {
		return nil, err
	}
	delete(p.props, "subnets")
	type planned struct {
		name  string
		res   *resource
		props map[string]any
		st    *subnetState
	}
	plan := make([]planned, 0, len(raw))
	listed := map[string]bool{}
	for i, rs := range raw {
		path := "properties.subnets[" + strconv.Itoa(i) + "]"
		if err := checkMembers(rs, subnetKind.typeName, path, "id", "name", "etag", "type", "properties"); err != nil {
			return nil, err
		}
		props, err := object(rs, "properties")
		if err != nil {
			return nil, err
		}
		if err := checkMembers(props, "SubnetPropertiesFormat", path+".properties", slices.Concat(subnetKind.props, subnetKind.readOnly)...); err != nil {
			return nil, err
		}
		name := str(rs, "name")
		if name == "" {
			return nil, badRequest("Subnet %d of virtual network %s has no name.", i, res.id)
		}
		id := res.id + "/subnets/" + name
		if listed[key(id)] {
			return nil, unsupported("subnet %s is listed more than once", id)
		}
		listed[key(id)] = true
		recorded := map[string]any{}
		for k, v := range props {
			if slices.Contains(subnetKind.props, k) && v != nil {
				recorded[k] = v
			}
		}
		st, err := s.parseSubnet(res, space, id, name, recorded)
		if err != nil {
			return nil, err
		}
		for _, other := range plan {
			if overlap(st.prefixes, other.st.prefixes) {
				return nil, subnetOverlaps(name, res.name)
			}
		}
		plan = append(plan, planned{name: name, res: s.settled(id), props: recorded, st: st})
	}
	var gone []*resource
	if res.vnet != nil {
		for _, sn := range s.subnetsOf(res) {
			if listed[key(sn.id)] {
				continue
			}
			if user := s.subnetUser(sn.id); user != "" {
				return nil, subnetInUse(sn.name, user)
			}
			gone = append(gone, sn)
		}
	}
	return func() {
		for _, sn := range gone {
			s.remove(sn)
		}
		for _, pl := range plan {
			sn := pl.res
			if sn == nil {
				id := res.id + "/subnets/" + pl.name
				sn = &resource{
					kind: subnetKind, id: id, name: pl.name, sub: res.sub, group: res.group,
					state: stateSucceeded, created: s.now(), guid: s.newUUID(), top: map[string]any{},
				}
				s.resources[key(id)] = sn
			}
			sn.subnet, sn.props = pl.st, pl.props
			s.touch(sn)
		}
		res.vnet = &vnetState{space: space}
		s.update(res, p)
	}, nil
}

// prepareNAT validates p as the new body of the NAT gateway res, and returns
// how to apply it. The caller holds s.mu.
func (s *Server) prepareNAT(res *resource, p *parsed) (func(), *apiError) {
	if sku, _ := p.top["sku"].(map[string]any); sku != nil {
		if name := str(sku, "name"); name != "" && !strings.EqualFold(name, "Standard") {
			return nil, unsupported("NAT gateway SKU %s is not supported; use Standard", name)
		}
		sku = maps.Clone(sku)
		sku["name"], sku["tier"] = "Standard", "Regional"
		p.top["sku"] = sku
	} else {
		p.top["sku"] = map[string]any{"name": "Standard", "tier": "Regional"}
	}
	pips, err := objects(p.props, "publicIpAddresses")
	if err != nil {
		return nil, err
	}
	for _, pip := range pips {
		id := str(pip, "id")
		if target := s.settled(id); target == nil || target.kind != pipKind || target.loc != res.loc {
			return nil, invalidReference(id, res.id)
		}
		if user := s.pipUser(id); user != "" && key(user) != key(res.id) {
			return nil, newError(http.StatusBadRequest, "PublicIPAddressInUse",
				"Resource %s is referencing public IP address %s that is already allocated to resource %s.", res.id, id, user)
		}
	}
	return func() { s.update(res, p) }, nil
}

// natPIPs returns the public IP addresses the NAT gateway res uses.
func natPIPs(res *resource) []string {
	pips, _ := objects(res.props, "publicIpAddresses")
	out := make([]string, 0, len(pips))
	for _, pip := range pips {
		if id := str(pip, "id"); id != "" {
			out = append(out, id)
		}
	}
	return out
}

// pipNAT returns the NAT gateway that uses the public IP address id, or "". The caller holds s.mu.
func (s *Server) pipNAT(id string) string {
	for _, res := range s.resources {
		if res.kind == natKind && slices.ContainsFunc(natPIPs(res), func(pip string) bool { return key(pip) == key(id) }) {
			return res.id
		}
	}
	return ""
}

// renderSubnet renders the subnet res as Azure answers it, with its
// delegations and service endpoints, and the defaults Azure fills in.
func renderSubnet(res *resource) map[string]any {
	m, props := envelope(res)
	if st := res.subnet; st != nil {
		if st.nat != "" {
			props["natGateway"] = ref(st.nat)
		}
		if st.nsg != "" {
			props["networkSecurityGroup"] = ref(st.nsg)
		}
	}
	delegations, _ := objects(res.props, "delegations")
	out := make([]any, 0, len(delegations))
	for _, d := range delegations {
		d = maps.Clone(d)
		dp, _ := d["properties"].(map[string]any)
		dp = maps.Clone(dp)
		if dp == nil {
			dp = map[string]any{}
		}
		if _, ok := dp["actions"]; !ok {
			dp["actions"] = []any{}
		}
		dp["provisioningState"] = res.state
		d["id"], d["etag"], d["type"], d["properties"] = res.id+"/delegations/"+str(d, "name"), res.etag, delegationType, dp
		out = append(out, d)
	}
	props["delegations"] = out
	if endpoints, _ := objects(res.props, "serviceEndpoints"); endpoints != nil {
		out := make([]any, 0, len(endpoints))
		for _, e := range endpoints {
			e = maps.Clone(e)
			e["provisioningState"] = res.state
			out = append(out, e)
		}
		props["serviceEndpoints"] = out
	}
	for name, value := range map[string]string{"privateEndpointNetworkPolicies": "Disabled", "privateLinkServiceNetworkPolicies": "Enabled"} {
		if _, ok := props[name]; !ok {
			props[name] = value
		}
	}
	return m
}

// renderVNet adds what Azure reports for the virtual network res: its subnets and defaults.
func (s *Server) renderVNet(res *resource, props map[string]any) {
	subnets := s.subnetsOf(res)
	out := make([]any, 0, len(subnets))
	for _, sn := range subnets {
		out = append(out, renderSubnet(sn))
	}
	props["subnets"] = out
	if _, ok := props["virtualNetworkPeerings"]; !ok {
		props["virtualNetworkPeerings"] = []any{}
	}
	if _, ok := props["enableDdosProtection"]; !ok {
		props["enableDdosProtection"] = false
	}
	if _, ok := props["privateEndpointVNetPolicies"]; !ok {
		props["privateEndpointVNetPolicies"] = "Disabled"
	}
}

// renderNAT adds what Azure reports for the NAT gateway res: the subnets that use it and defaults.
func (s *Server) renderNAT(res *resource, props map[string]any) {
	if _, ok := props["idleTimeoutInMinutes"]; !ok {
		props["idleTimeoutInMinutes"] = 4
	}
	if users := s.subnetsUsing(res.id, subnetNAT); len(users) > 0 {
		refs := make([]any, 0, len(users))
		for _, u := range users {
			refs = append(refs, ref(u))
		}
		props["subnets"] = refs
	}
}

// listNetwork lists the network resources of kind k in the call's group.
func (s *Server) listNetwork(k *kind) handler {
	return func(c *call) (*reply, *apiError) {
		s.mu.Lock()
		defer s.mu.Unlock()
		if err := s.readableGroup(c); err != nil {
			return nil, err
		}
		return ok(listBody(s.listed(c, k), func(res *resource) any { return s.renderNetwork(res) })), nil
	}
}

// listNetworkInSubscription lists the network resources of kind k in the
// call's subscription.
func (s *Server) listNetworkInSubscription(k *kind) handler {
	return func(c *call) (*reply, *apiError) {
		s.mu.Lock()
		defer s.mu.Unlock()
		return ok(listBody(s.listedInSubscription(c, k), func(res *resource) any { return s.renderNetwork(res) })), nil
	}
}

// listSubnets lists the subnets of the call's virtual network.
func (s *Server) listSubnets(c *call) (*reply, *apiError) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.readableGroup(c); err != nil {
		return nil, err
	}
	vnet := s.read(vnetID(c))
	if vnet == nil || vnet.kind != vnetKind {
		return nil, resourceNotFound(vnetKind.typ, c.val("vnet"), c.val("rg"))
	}
	return ok(listBody(s.subnetsOf(vnet), func(res *resource) any { return renderSubnet(res) })), nil
}

// VirtualNetwork is a snapshot of a virtual network.
type VirtualNetwork struct {
	ID              string
	AddressPrefixes []string
	Subnets         []Subnet
	Tags            map[string]string
}

// Subnet is a snapshot of a subnet.
type Subnet struct {
	ID              string
	AddressPrefixes []string
	// NATGateway and NetworkSecurityGroup are the IDs of those the subnet uses, if any.
	NATGateway, NetworkSecurityGroup string
	// Delegations are the services the subnet is delegated to, such as Microsoft.Netapp/volumes.
	Delegations []string
	// ServiceEndpoints are the services of its service endpoints, such as Microsoft.Storage.
	ServiceEndpoints []string
}

// VirtualNetworks returns the virtual networks the server holds, ordered by
// ID, each with its subnets ordered by name.
func (s *Server) VirtualNetworks() []VirtualNetwork {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []VirtualNetwork
	for _, res := range s.resources {
		if res.vnet == nil {
			continue
		}
		v := VirtualNetwork{ID: res.id, AddressPrefixes: prefixStrings(res.vnet.space), Tags: maps.Clone(res.tags)}
		for _, sn := range s.subnetsOf(res) {
			v.Subnets = append(v.Subnets, snapshotSubnet(sn))
		}
		out = append(out, v)
	}
	slices.SortFunc(out, func(a, b VirtualNetwork) int { return cmp.Compare(a.ID, b.ID) })
	return out
}

func snapshotSubnet(res *resource) Subnet {
	sn := Subnet{ID: res.id, AddressPrefixes: prefixStrings(res.subnet.prefixes), NATGateway: res.subnet.nat, NetworkSecurityGroup: res.subnet.nsg}
	delegations, _ := objects(res.props, "delegations")
	for _, d := range delegations {
		dp, _ := d["properties"].(map[string]any)
		sn.Delegations = append(sn.Delegations, str(dp, "serviceName"))
	}
	endpoints, _ := objects(res.props, "serviceEndpoints")
	for _, e := range endpoints {
		sn.ServiceEndpoints = append(sn.ServiceEndpoints, str(e, "service"))
	}
	return sn
}

func prefixStrings(prefixes []netip.Prefix) []string {
	out := make([]string, 0, len(prefixes))
	for _, p := range prefixes {
		out = append(out, p.String())
	}
	return out
}

// NATGateway is a snapshot of a NAT gateway.
type NATGateway struct {
	ID string
	// PublicIPAddresses are the IDs of the public IP addresses it uses.
	PublicIPAddresses []string
	// Subnets are the IDs of the subnets that use it, ordered.
	Subnets []string
	Tags    map[string]string
}

// NATGateways returns the NAT gateways the server holds, ordered by ID.
func (s *Server) NATGateways() []NATGateway {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []NATGateway
	for _, res := range s.resources {
		if res.kind == natKind {
			out = append(out, NATGateway{ID: res.id, PublicIPAddresses: natPIPs(res), Subnets: s.subnetsUsing(res.id, subnetNAT), Tags: maps.Clone(res.tags)})
		}
	}
	slices.SortFunc(out, func(a, b NATGateway) int { return cmp.Compare(a.ID, b.ID) })
	return out
}
