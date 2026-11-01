package ec2

import (
	"cmp"
	"maps"
	"net/netip"
	"slices"
	"strconv"
	"strings"
)

const (
	tenancyDefault = "default"
	dhcpDefault    = "default"

	minPrefixBits = 16
	maxPrefixBits = 28
	// reservedAddresses is how many addresses EC2 keeps in each subnet: the
	// first four and the last.
	reservedAddresses = 5
)

// VPC is a virtual private cloud.
type VPC struct {
	// Region is where the VPC is. The server sets it.
	Region    string
	VPCID     string
	CIDRBlock string
	// CIDRAssociationID identifies the VPC's CIDR block association.
	CIDRAssociationID string
	// DHCPOptionsID is the DHCP options set associated with the VPC, or
	// "default" for none.
	DHCPOptionsID      string
	InstanceTenancy    string
	EnableDNSSupport   bool
	EnableDNSHostnames bool
	Tags               map[string]string
}

func (v *VPC) clone() VPC {
	c := *v
	c.Tags = maps.Clone(v.Tags)
	return c
}

type cidrStateXML struct {
	State string `xml:"state"`
}

type cidrAssociationXML struct {
	AssociationID string       `xml:"associationId"`
	CIDRBlock     string       `xml:"cidrBlock"`
	State         cidrStateXML `xml:"cidrBlockState"`
}

type vpcXML struct {
	VPCID            string               `xml:"vpcId"`
	State            string               `xml:"state"`
	CIDRBlock        string               `xml:"cidrBlock"`
	CIDRAssociations []cidrAssociationXML `xml:"cidrBlockAssociationSet>item"`
	DHCPOptionsID    string               `xml:"dhcpOptionsId"`
	InstanceTenancy  string               `xml:"instanceTenancy"`
	IsDefault        bool                 `xml:"isDefault"`
	Tags             []tagXML             `xml:"tagSet>item,omitempty"`
}

func (v *VPC) xml(state string) vpcXML {
	return vpcXML{
		VPCID: v.VPCID, State: state, CIDRBlock: v.CIDRBlock,
		CIDRAssociations: []cidrAssociationXML{{v.CIDRAssociationID, v.CIDRBlock, cidrStateXML{"associated"}}},
		DHCPOptionsID:    v.DHCPOptionsID, InstanceTenancy: v.InstanceTenancy, Tags: tagSet(v.Tags),
	}
}

func vpcFields(_ *regionState, v *VPC) func(string) ([]string, bool) {
	return func(name string) ([]string, bool) {
		switch name {
		case "vpc-id":
			return one(v.VPCID), true
		case "cidr", "cidr-block-association.cidr-block":
			return one(v.CIDRBlock), true
		case "cidr-block-association.association-id":
			return one(v.CIDRAssociationID), true
		case "cidr-block-association.state":
			return []string{"associated"}, true
		case "dhcp-options-id":
			return one(v.DHCPOptionsID), true
		case "instance-tenancy":
			return one(v.InstanceTenancy), true
		case "is-default":
			return []string{"false"}, true
		case "state":
			return []string{statusAvailable}, true
		}
		return tagFilter(v.Tags, name)
	}
}

func vpcNotFound(id string) *apiError {
	return clientError("InvalidVpcID.NotFound", "The vpc ID '%s' does not exist", id)
}

func dependencyViolation(kind, id string) *apiError {
	return clientError("DependencyViolation", "The %s '%s' has dependencies and cannot be deleted.", kind, id)
}

// vpc returns the VPC with id. The caller holds s.mu.
func (rs *regionState) vpc(id string) (*VPC, *apiError) {
	if v, ok := rs.vpcs[id]; ok {
		return v, nil
	}
	return nil, vpcNotFound(id)
}

// parseCIDR reads an IPv4 CIDR block of /16 to /28, the sizes EC2 takes for
// VPCs and subnets, and returns it with its host bits cleared. rangeCode is
// the code for a block of another size.
func parseCIDR(param, cidr, rangeCode string) (netip.Prefix, *apiError) {
	prefix, err := netip.ParsePrefix(cidr)
	if err != nil || !prefix.Addr().Is4() {
		return netip.Prefix{}, invalidValue("Value (%s) for parameter %s is invalid. This is not a valid CIDR block.", cidr, param)
	}
	if prefix.Bits() < minPrefixBits || prefix.Bits() > maxPrefixBits {
		return netip.Prefix{}, clientError(rangeCode, "The CIDR '%s' is invalid.", cidr)
	}
	return prefix.Masked(), nil
}

// contains reports whether outer holds all of inner.
func contains(outer, inner netip.Prefix) bool {
	return outer.Bits() <= inner.Bits() && outer.Contains(inner.Addr())
}

type createVPCResponse struct {
	meta
	VPC vpcXML `xml:"vpc"`
}

func (s *Server) createVPC(c *call) (response, *apiError) {
	cidr, err := c.p.required("CidrBlock")
	if err != nil {
		return nil, err
	}
	prefix, err := parseCIDR("cidrBlock", cidr, "InvalidVpc.Range")
	if err != nil {
		return nil, err
	}
	tenancy, err := c.p.enum("InstanceTenancy", tenancyDefault, tenancyDefault, "dedicated")
	if err != nil {
		return nil, err
	}
	tags, err := c.p.tags("vpc")
	if err != nil {
		return nil, err
	}
	if err := c.p.done(); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	rs := s.region(c.region)
	v := &VPC{
		Region: c.region, VPCID: c.id, CIDRBlock: prefix.String(), CIDRAssociationID: s.newID("vpc-cidr-assoc"),
		DHCPOptionsID: s.defaultDHCPOptions(rs, c.region), InstanceTenancy: tenancy, EnableDNSSupport: true, Tags: tags,
	}
	rs.vpcs[v.VPCID] = v
	s.newMainRouteTable(rs, v)
	s.newDefaultSecurityGroup(rs, v)
	return &createVPCResponse{VPC: v.xml(statePending)}, nil
}

type describeVPCsResponse struct {
	meta
	VPCs      []vpcXML `xml:"vpcSet>item"`
	NextToken string   `xml:"nextToken,omitempty"`
}

func (s *Server) describeVPCs(c *call) (response, *apiError) {
	vpcs, next, err := describe(s, c, lister[*VPC]{
		ids: "VpcId", prefix: "vpc", of: vpcsOf,
		empty: &VPC{}, fields: vpcFields, notFound: vpcNotFound,
	}, func(_ *regionState, v *VPC) vpcXML { return v.xml(statusAvailable) })
	if err != nil {
		return nil, err
	}
	return &describeVPCsResponse{VPCs: vpcs, NextToken: next}, nil
}

func (s *Server) modifyVPCAttribute(c *call) (response, *apiError) {
	id, err := c.p.required("VpcId")
	if err != nil {
		return nil, err
	}
	hasSupport, hasHostnames := c.p.values.Has("EnableDnsSupport.Value"), c.p.values.Has("EnableDnsHostnames.Value")
	support, err := c.p.bool("EnableDnsSupport.Value", false)
	if err != nil {
		return nil, err
	}
	hostnames, err := c.p.bool("EnableDnsHostnames.Value", false)
	if err != nil {
		return nil, err
	}
	switch {
	case hasSupport && hasHostnames:
		return nil, clientError("InvalidParameterCombination", "Fields for multiple attribute types specified: enableDnsSupport, enableDnsHostnames")
	case !hasSupport && !hasHostnames:
		return nil, missing("EnableDnsSupport")
	}
	if err := c.p.done(); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	v, err := s.lookupRegion(c.region).vpc(id)
	if err != nil {
		return nil, err
	}
	if hasSupport {
		v.EnableDNSSupport = support
	} else {
		v.EnableDNSHostnames = hostnames
	}
	return &returnResponse{Return: true}, nil
}

type attributeValueXML struct {
	Value bool `xml:"value"`
}

type describeVPCAttributeResponse struct {
	meta
	VPCID              string             `xml:"vpcId"`
	EnableDNSSupport   *attributeValueXML `xml:"enableDnsSupport,omitempty"`
	EnableDNSHostnames *attributeValueXML `xml:"enableDnsHostnames,omitempty"`
}

func (s *Server) describeVPCAttribute(c *call) (response, *apiError) {
	id, err := c.p.required("VpcId")
	if err != nil {
		return nil, err
	}
	if _, err := c.p.required("Attribute"); err != nil {
		return nil, err
	}
	attribute, err := c.p.enum("Attribute", "", "enableDnsSupport", "enableDnsHostnames")
	if err != nil {
		return nil, err
	}
	if err := c.p.done(); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	v, err := s.lookupRegion(c.region).vpc(id)
	if err != nil {
		return nil, err
	}
	resp := &describeVPCAttributeResponse{VPCID: id}
	if attribute == "enableDnsSupport" {
		resp.EnableDNSSupport = &attributeValueXML{v.EnableDNSSupport}
	} else {
		resp.EnableDNSHostnames = &attributeValueXML{v.EnableDNSHostnames}
	}
	return resp, nil
}

func (s *Server) deleteVPC(c *call) (response, *apiError) {
	id, err := c.p.required("VpcId")
	if err != nil {
		return nil, err
	}
	if err := c.p.done(); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	rs := s.lookupRegion(c.region)
	if _, err := rs.vpc(id); err != nil {
		return nil, err
	}
	if rs.vpcInUse(id) {
		return nil, dependencyViolation("vpc", id)
	}
	for rtbID, rt := range rs.routeTables {
		if rt.VPCID == id {
			delete(rs.routeTables, rtbID)
		}
	}
	delete(rs.securityGroups, rs.defaultGroup(id))
	delete(rs.vpcs, id)
	return &returnResponse{Return: true}, nil
}

// vpcInUse reports whether anything EC2 does not delete with the VPC with id
// is in it: a subnet, an attached internet gateway, a route table other
// than the main one or a security group other than the default one. The
// caller holds s.mu.
func (rs *regionState) vpcInUse(id string) bool {
	for _, sub := range rs.subnets {
		if sub.VPCID == id {
			return true
		}
	}
	for _, igw := range rs.internetGateways {
		if igw.VPCID == id {
			return true
		}
	}
	for _, rt := range rs.routeTables {
		if rt.VPCID == id && !rt.main() {
			return true
		}
	}
	for _, g := range rs.securityGroups {
		if g.VPCID == id && !g.isDefault() {
			return true
		}
	}
	return false
}

// Subnet is a subnet of a VPC.
type Subnet struct {
	// Region is where the subnet is. The server sets it.
	Region             string
	SubnetID           string
	VPCID              string
	CIDRBlock          string
	AvailabilityZone   string
	AvailabilityZoneID string
	Tags               map[string]string
}

func (sub *Subnet) clone() Subnet {
	c := *sub
	c.Tags = maps.Clone(sub.Tags)
	return c
}

func (sub *Subnet) prefix() netip.Prefix { return netip.MustParsePrefix(sub.CIDRBlock) }

// usable reports whether ip is one of the subnet's addresses EC2 does not
// keep for itself.
func (sub *Subnet) usable(ip netip.Addr) bool {
	p := sub.prefix()
	if !p.Contains(ip) {
		return false
	}
	offset := addrOffset(p.Addr(), ip)
	return offset >= reservedAddresses-1 && offset < 1<<(32-p.Bits())-1
}

func addrOffset(base, ip netip.Addr) uint32 {
	b, a := base.As4(), ip.As4()
	return (uint32(a[0])<<24 | uint32(a[1])<<16 | uint32(a[2])<<8 | uint32(a[3])) -
		(uint32(b[0])<<24 | uint32(b[1])<<16 | uint32(b[2])<<8 | uint32(b[3]))
}

// free returns how many of the subnet's addresses no interface holds. The
// caller holds s.mu.
func (rs *regionState) free(sub *Subnet) int {
	n := 1<<(32-sub.prefix().Bits()) - reservedAddresses
	for _, ni := range rs.interfaces {
		if ni.SubnetID == sub.SubnetID {
			n--
		}
	}
	return n
}

type subnetXML struct {
	SubnetID                string   `xml:"subnetId"`
	SubnetArn               string   `xml:"subnetArn"`
	State                   string   `xml:"state"`
	VPCID                   string   `xml:"vpcId"`
	CIDRBlock               string   `xml:"cidrBlock"`
	AvailableIPAddressCount int      `xml:"availableIpAddressCount"`
	AvailabilityZone        string   `xml:"availabilityZone"`
	AvailabilityZoneID      string   `xml:"availabilityZoneId"`
	DefaultForAZ            bool     `xml:"defaultForAz"`
	MapPublicIPOnLaunch     bool     `xml:"mapPublicIpOnLaunch"`
	Tags                    []tagXML `xml:"tagSet>item,omitempty"`
}

func (rs *regionState) subnetXML(sub *Subnet) subnetXML {
	return subnetXML{
		SubnetID: sub.SubnetID, SubnetArn: "arn:aws:ec2:" + sub.Region + "::subnet/" + sub.SubnetID, State: statusAvailable,
		VPCID: sub.VPCID, CIDRBlock: sub.CIDRBlock, AvailableIPAddressCount: rs.free(sub),
		AvailabilityZone: sub.AvailabilityZone, AvailabilityZoneID: sub.AvailabilityZoneID, Tags: tagSet(sub.Tags),
	}
}

func subnetFields(rs *regionState, sub *Subnet) func(string) ([]string, bool) {
	return func(name string) ([]string, bool) {
		switch name {
		case "subnet-id":
			return one(sub.SubnetID), true
		case "vpc-id":
			return one(sub.VPCID), true
		case "cidr-block", "cidr", "cidrBlock":
			return one(sub.CIDRBlock), true
		case "availability-zone", "availabilityZone":
			return one(sub.AvailabilityZone), true
		case "availability-zone-id", "availabilityZoneId":
			return one(sub.AvailabilityZoneID), true
		case "available-ip-address-count":
			if sub.CIDRBlock == "" {
				return nil, true
			}
			return []string{strconv.Itoa(rs.free(sub))}, true
		case "default-for-az", "defaultForAz", "map-public-ip-on-launch":
			return []string{"false"}, true
		case "state":
			return []string{statusAvailable}, true
		}
		return tagFilter(sub.Tags, name)
	}
}

func subnetNotFound(id string) *apiError {
	return clientError("InvalidSubnetID.NotFound", "The subnet ID '%s' does not exist", id)
}

// subnet returns the subnet with id. The caller holds s.mu.
func (rs *regionState) subnet(id string) (*Subnet, *apiError) {
	if sub, ok := rs.subnets[id]; ok {
		return sub, nil
	}
	return nil, subnetNotFound(id)
}

type createSubnetResponse struct {
	meta
	Subnet subnetXML `xml:"subnet"`
}

func (s *Server) createSubnet(c *call) (response, *apiError) {
	vpcID, err := c.p.required("VpcId")
	if err != nil {
		return nil, err
	}
	cidr, err := c.p.required("CidrBlock")
	if err != nil {
		return nil, err
	}
	prefix, err := parseCIDR("cidrBlock", cidr, "InvalidSubnet.Range")
	if err != nil {
		return nil, err
	}
	zone, zoneID := c.p.get("AvailabilityZone"), c.p.get("AvailabilityZoneId")
	if zone != "" && zoneID != "" {
		return nil, clientError("InvalidParameterCombination", "Only one of availabilityZone and availabilityZoneId may be specified.")
	}
	tags, err := c.p.tags("subnet")
	if err != nil {
		return nil, err
	}
	if err := c.p.done(); err != nil {
		return nil, err
	}
	zones := s.availabilityZones(c.region)
	z, ok := zones[0], true
	switch {
	case zone != "":
		z, ok = findZone(zones, func(z availabilityZone) bool { return z.name == zone })
	case zoneID != "":
		z, ok = findZone(zones, func(z availabilityZone) bool { return z.id == zoneID })
	}
	if !ok {
		names := make([]string, len(zones))
		for i, z := range zones {
			names[i] = z.name
		}
		return nil, invalidValue("Value (%s) for parameter availabilityZone is invalid. Subnets can currently only be created in the following availability zones: %s.",
			cmp.Or(zone, zoneID), strings.Join(names, ", "))
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	rs := s.lookupRegion(c.region)
	v, err := rs.vpc(vpcID)
	if err != nil {
		return nil, err
	}
	if !contains(netip.MustParsePrefix(v.CIDRBlock), prefix) {
		return nil, clientError("InvalidSubnet.Range", "The CIDR '%s' is invalid.", cidr)
	}
	for _, other := range rs.subnets {
		if other.VPCID == vpcID && other.prefix().Overlaps(prefix) {
			return nil, clientError("InvalidSubnet.Conflict", "The CIDR '%s' conflicts with another subnet", cidr)
		}
	}
	sub := &Subnet{
		Region: c.region, SubnetID: c.id, VPCID: vpcID, CIDRBlock: prefix.String(),
		AvailabilityZone: z.name, AvailabilityZoneID: z.id, Tags: tags,
	}
	rs.subnets[sub.SubnetID] = sub
	return &createSubnetResponse{Subnet: rs.subnetXML(sub)}, nil
}

type describeSubnetsResponse struct {
	meta
	Subnets   []subnetXML `xml:"subnetSet>item"`
	NextToken string      `xml:"nextToken,omitempty"`
}

func (s *Server) describeSubnets(c *call) (response, *apiError) {
	subnets, next, err := describe(s, c, lister[*Subnet]{
		ids: "SubnetId", prefix: "subnet", of: subnetsOf,
		empty: &Subnet{}, fields: subnetFields, notFound: subnetNotFound,
	}, (*regionState).subnetXML)
	if err != nil {
		return nil, err
	}
	return &describeSubnetsResponse{Subnets: subnets, NextToken: next}, nil
}

func (s *Server) deleteSubnet(c *call) (response, *apiError) {
	id, err := c.p.required("SubnetId")
	if err != nil {
		return nil, err
	}
	if err := c.p.done(); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	rs := s.lookupRegion(c.region)
	if _, err := rs.subnet(id); err != nil {
		return nil, err
	}
	if rs.subnetInUse(id) {
		return nil, dependencyViolation("subnet", id)
	}
	for _, rt := range rs.routeTables {
		rt.Associations = slices.DeleteFunc(rt.Associations, func(a RouteTableAssociation) bool { return a.SubnetID == id })
	}
	delete(rs.subnets, id)
	return &returnResponse{Return: true}, nil
}

// subnetInUse reports whether a network interface or a transit gateway
// attachment is in the subnet with id. A NAT gateway's interface counts until
// the gateway is deleted. The caller holds s.mu.
func (rs *regionState) subnetInUse(id string) bool {
	for _, ni := range rs.interfaces {
		if ni.SubnetID == id {
			return true
		}
	}
	for _, a := range rs.transitGatewayAttachments {
		if a.State != stateDeleted && slices.Contains(a.SubnetIDs, id) {
			return true
		}
	}
	return false
}

// availabilityZone is a zone of a region and its zone ID.
type availabilityZone struct{ name, id string }

func findZone(zones []availabilityZone, match func(availabilityZone) bool) (availabilityZone, bool) {
	if i := slices.IndexFunc(zones, match); i >= 0 {
		return zones[i], true
	}
	return availabilityZone{}, false
}

// WithAvailabilityZones sets the availability zones of region, which
// DescribeAvailabilityZones lists and CreateSubnet takes. Without it a region
// has three: the region's name with "a", "b" and "c" appended.
func WithAvailabilityZones(region string, names ...string) Option {
	return func(s *Server) {
		if s.zones == nil {
			s.zones = map[string][]string{}
		}
		s.zones[region] = slices.Clone(names)
	}
}

// availabilityZones returns region's zones. Zone IDs follow EC2's form, such
// as use1-az1, numbered in the order of the zones.
func (s *Server) availabilityZones(region string) []availabilityZone {
	names, ok := s.zones[region]
	if !ok {
		names = []string{region + "a", region + "b", region + "c"}
	}
	short := regionShortName(region)
	out := make([]availabilityZone, len(names))
	for i, name := range names {
		out[i] = availabilityZone{name, short + "-az" + strconv.Itoa(i+1)}
	}
	return out
}

var directionShortNames = map[string]string{"northeast": "ne", "northwest": "nw", "southeast": "se", "southwest": "sw"}

// regionShortName abbreviates a region as zone IDs do: us-east-1 is use1 and
// ap-southeast-2 is apse2.
func regionShortName(region string) string {
	parts := strings.Split(region, "-")
	if len(parts) < 3 {
		return region
	}
	var short strings.Builder
	short.WriteString(parts[0])
	for _, part := range parts[1 : len(parts)-1] {
		if abbr, ok := directionShortNames[part]; ok {
			short.WriteString(abbr)
		} else if part != "" {
			short.WriteString(part[:1])
		}
	}
	short.WriteString(parts[len(parts)-1])
	return short.String()
}

type availabilityZoneXML struct {
	ZoneName           string `xml:"zoneName"`
	ZoneState          string `xml:"zoneState"`
	RegionName         string `xml:"regionName"`
	ZoneID             string `xml:"zoneId"`
	ZoneType           string `xml:"zoneType"`
	OptInStatus        string `xml:"optInStatus"`
	GroupName          string `xml:"groupName"`
	NetworkBorderGroup string `xml:"networkBorderGroup"`
}

func zoneFields(region string, z availabilityZone) func(string) ([]string, bool) {
	return func(name string) ([]string, bool) {
		switch name {
		case "zone-name":
			return one(z.name), true
		case "zone-id":
			return one(z.id), true
		case "region-name", "group-name", "network-border-group":
			return one(region), true
		case "state":
			return []string{statusAvailable}, true
		case "zone-type":
			return []string{"availability-zone"}, true
		case "opt-in-status":
			return []string{"opt-in-not-required"}, true
		}
		return nil, false
	}
}

type describeAvailabilityZonesResponse struct {
	meta
	Zones []availabilityZoneXML `xml:"availabilityZoneInfo>item"`
}

func (s *Server) describeAvailabilityZones(c *call) (response, *apiError) {
	names, ids := c.p.list("ZoneName"), c.p.list("ZoneId")
	if _, err := c.p.bool("AllAvailabilityZones", false); err != nil {
		return nil, err
	}
	filters, err := c.p.filters(zoneFields(c.region, availabilityZone{}))
	if err != nil {
		return nil, err
	}
	if err := c.p.done(); err != nil {
		return nil, err
	}
	zones := s.availabilityZones(c.region)
	for _, name := range names {
		if _, ok := findZone(zones, func(z availabilityZone) bool { return z.name == name }); !ok {
			return nil, invalidValue("Invalid availability zone: [%s]", name)
		}
	}
	for _, id := range ids {
		if _, ok := findZone(zones, func(z availabilityZone) bool { return z.id == id }); !ok {
			return nil, invalidValue("Invalid availability zone ID: [%s]", id)
		}
	}
	resp := &describeAvailabilityZonesResponse{}
	for _, z := range zones {
		if (len(names) == 0 && len(ids) == 0 || slices.Contains(names, z.name) || slices.Contains(ids, z.id)) &&
			matchFilters(filters, zoneFields(c.region, z)) {
			resp.Zones = append(resp.Zones, availabilityZoneXML{
				ZoneName: z.name, ZoneState: statusAvailable, RegionName: c.region, ZoneID: z.id, ZoneType: "availability-zone",
				OptInStatus: "opt-in-not-required", GroupName: c.region, NetworkBorderGroup: c.region,
			})
		}
	}
	return resp, nil
}

// VPCs returns every VPC the server holds, ordered by region and ID.
func (s *Server) VPCs() []VPC { return collect(s, vpcsOf, (*VPC).clone) }

// Subnets returns every subnet the server holds, ordered by region and ID.
func (s *Server) Subnets() []Subnet {
	return collect(s, subnetsOf, (*Subnet).clone)
}
