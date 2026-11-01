package ec2

import (
	"maps"
	"net/netip"
	"slices"
	"strconv"
	"strings"
)

const (
	gatewayLocal       = "local"
	originRouteTable   = "CreateRouteTable"
	originRoute        = "CreateRoute"
	routeActive        = "active"
	routeBlackhole     = "blackhole"
	associationCurrent = "associated"
)

// RouteTable is a VPC's route table.
type RouteTable struct {
	// Region is where the route table is. The server sets it.
	Region       string
	RouteTableID string
	VPCID        string
	// Routes start with the local route to the VPC's CIDR block.
	Routes []Route
	// Associations holds the main association, for a VPC's main route
	// table, and one for each subnet associated with the table.
	Associations []RouteTableAssociation
	Tags         map[string]string
}

// Route is a route in a route table. It has one target.
type Route struct {
	DestinationCIDRBlock string
	// GatewayID is "local" for the local route, or an internet gateway.
	GatewayID        string
	NATGatewayID     string
	TransitGatewayID string
	// Origin is "CreateRouteTable" for the local route and "CreateRoute" for
	// the others.
	Origin string
}

// RouteTableAssociation associates a route table with a subnet, or makes
// it its VPC's main route table.
type RouteTableAssociation struct {
	AssociationID string
	SubnetID      string
	Main          bool
}

func (rt *RouteTable) clone() RouteTable {
	c := *rt
	c.Routes = slices.Clone(rt.Routes)
	c.Associations = slices.Clone(rt.Associations)
	c.Tags = maps.Clone(rt.Tags)
	return c
}

func (rt *RouteTable) main() bool {
	return slices.ContainsFunc(rt.Associations, func(a RouteTableAssociation) bool { return a.Main })
}

// newRouteTable stores a route table for v with only the local route. The
// caller holds s.mu.
func (s *Server) newRouteTable(rs *regionState, v *VPC, id string, tags map[string]string) *RouteTable {
	if id == "" {
		id = unusedID(s, "rtb", rs.routeTables)
	}
	rt := &RouteTable{
		Region: v.Region, RouteTableID: id, VPCID: v.VPCID,
		Routes: []Route{{DestinationCIDRBlock: v.CIDRBlock, GatewayID: gatewayLocal, Origin: originRouteTable}},
		Tags:   tags,
	}
	rs.routeTables[id] = rt
	return rt
}

// newMainRouteTable stores v's main route table. The caller holds s.mu.
func (s *Server) newMainRouteTable(rs *regionState, v *VPC) {
	rt := s.newRouteTable(rs, v, "", nil)
	rt.Associations = []RouteTableAssociation{{AssociationID: s.newID("rtbassoc"), Main: true}}
}

// routeState is "active" while the route's target can carry traffic for the
// route table's VPC, and "blackhole" once it cannot. The caller holds s.mu.
func (rs *regionState) routeState(rt *RouteTable, r Route) string {
	ok := true
	switch {
	case r.GatewayID == gatewayLocal:
	case r.GatewayID != "":
		igw, found := rs.internetGateways[r.GatewayID]
		ok = found && igw.VPCID == rt.VPCID
	case r.NATGatewayID != "":
		nat, found := rs.natGateways[r.NATGatewayID]
		ok = found && !nat.gone()
	case r.TransitGatewayID != "":
		ok = rs.transitGatewayAttached(r.TransitGatewayID, rt.VPCID)
	}
	if ok {
		return routeActive
	}
	return routeBlackhole
}

type routeXML struct {
	DestinationCIDRBlock string `xml:"destinationCidrBlock"`
	GatewayID            string `xml:"gatewayId,omitempty"`
	NATGatewayID         string `xml:"natGatewayId,omitempty"`
	TransitGatewayID     string `xml:"transitGatewayId,omitempty"`
	State                string `xml:"state"`
	Origin               string `xml:"origin"`
}

type associationStateXML struct {
	State string `xml:"state"`
}

type routeTableAssociationXML struct {
	AssociationID    string              `xml:"routeTableAssociationId"`
	RouteTableID     string              `xml:"routeTableId"`
	SubnetID         string              `xml:"subnetId,omitempty"`
	Main             bool                `xml:"main"`
	AssociationState associationStateXML `xml:"associationState"`
}

type routeTableXML struct {
	RouteTableID string                     `xml:"routeTableId"`
	VPCID        string                     `xml:"vpcId"`
	Routes       []routeXML                 `xml:"routeSet>item"`
	Associations []routeTableAssociationXML `xml:"associationSet>item"`
	Tags         []tagXML                   `xml:"tagSet>item,omitempty"`
}

func (rs *regionState) routeTableXML(rt *RouteTable) routeTableXML {
	out := routeTableXML{RouteTableID: rt.RouteTableID, VPCID: rt.VPCID, Tags: tagSet(rt.Tags)}
	for _, r := range rt.Routes {
		out.Routes = append(out.Routes, routeXML{
			DestinationCIDRBlock: r.DestinationCIDRBlock, GatewayID: r.GatewayID, NATGatewayID: r.NATGatewayID,
			TransitGatewayID: r.TransitGatewayID, State: rs.routeState(rt, r), Origin: r.Origin,
		})
	}
	for _, a := range rt.Associations {
		out.Associations = append(out.Associations, routeTableAssociationXML{
			AssociationID: a.AssociationID, RouteTableID: rt.RouteTableID, SubnetID: a.SubnetID, Main: a.Main,
			AssociationState: associationStateXML{associationCurrent},
		})
	}
	return out
}

func routeTableFields(rs *regionState, rt *RouteTable) func(string) ([]string, bool) {
	routes := func(value func(Route) string) []string {
		var out []string
		for _, r := range rt.Routes {
			if v := value(r); v != "" {
				out = append(out, v)
			}
		}
		return out
	}
	associations := func(value func(RouteTableAssociation) string) []string {
		var out []string
		for _, a := range rt.Associations {
			if v := value(a); v != "" {
				out = append(out, v)
			}
		}
		return out
	}
	return func(name string) ([]string, bool) {
		switch name {
		case "route-table-id":
			return one(rt.RouteTableID), true
		case "vpc-id":
			return one(rt.VPCID), true
		case "association.subnet-id":
			return associations(func(a RouteTableAssociation) string { return a.SubnetID }), true
		case "association.route-table-association-id":
			return associations(func(a RouteTableAssociation) string { return a.AssociationID }), true
		case "association.route-table-id":
			return associations(func(RouteTableAssociation) string { return rt.RouteTableID }), true
		case "association.main":
			return associations(func(a RouteTableAssociation) string { return strconv.FormatBool(a.Main) }), true
		case "route.destination-cidr-block":
			return routes(func(r Route) string { return r.DestinationCIDRBlock }), true
		case "route.gateway-id":
			return routes(func(r Route) string { return r.GatewayID }), true
		case "route.nat-gateway-id":
			return routes(func(r Route) string { return r.NATGatewayID }), true
		case "route.transit-gateway-id":
			return routes(func(r Route) string { return r.TransitGatewayID }), true
		case "route.origin":
			return routes(func(r Route) string { return r.Origin }), true
		case "route.state":
			return routes(func(r Route) string { return rs.routeState(rt, r) }), true
		}
		return tagFilter(rt.Tags, name)
	}
}

func routeTableNotFound(id string) *apiError {
	return clientError("InvalidRouteTableID.NotFound", "The routeTable ID '%s' does not exist", id)
}

// routeTable returns the route table with id. The caller holds s.mu.
func (rs *regionState) routeTable(id string) (*RouteTable, *apiError) {
	if rt, ok := rs.routeTables[id]; ok {
		return rt, nil
	}
	return nil, routeTableNotFound(id)
}

type createRouteTableResponse struct {
	meta
	RouteTable  routeTableXML `xml:"routeTable"`
	ClientToken string        `xml:"clientToken,omitempty"`
}

func (s *Server) createRouteTable(c *call) (response, *apiError) {
	vpcID, err := c.p.required("VpcId")
	if err != nil {
		return nil, err
	}
	tags, err := c.p.tags("route-table")
	if err != nil {
		return nil, err
	}
	token := c.p.get("ClientToken")
	if err := c.p.done(); err != nil {
		return nil, err
	}
	fingerprint := c.p.fingerprint()
	s.mu.Lock()
	defer s.mu.Unlock()
	rs := s.lookupRegion(c.region)
	t, err := s.replay(c.region, "routetables", token, fingerprint)
	if err != nil {
		return nil, err
	}
	if t != nil {
		if rt, ok := rs.routeTables[t.id]; ok {
			return &createRouteTableResponse{RouteTable: rs.routeTableXML(rt), ClientToken: token}, nil
		}
		return &createRouteTableResponse{RouteTable: t.created.(routeTableXML), ClientToken: token}, nil
	}
	v, err := rs.vpc(vpcID)
	if err != nil {
		return nil, err
	}
	rt := s.newRouteTable(rs, v, c.id, tags)
	created := rs.routeTableXML(rt)
	s.remember(c.region, "routetables", token, fingerprint, rt.RouteTableID, created)
	return &createRouteTableResponse{RouteTable: created, ClientToken: token}, nil
}

type describeRouteTablesResponse struct {
	meta
	RouteTables []routeTableXML `xml:"routeTableSet>item"`
	NextToken   string          `xml:"nextToken,omitempty"`
}

func (s *Server) describeRouteTables(c *call) (response, *apiError) {
	tables, next, err := describe(s, c, lister[*RouteTable]{
		ids: "RouteTableId", prefix: "rtb", of: routeTablesOf,
		empty: &RouteTable{}, fields: routeTableFields, notFound: routeTableNotFound,
	}, (*regionState).routeTableXML)
	if err != nil {
		return nil, err
	}
	return &describeRouteTablesResponse{RouteTables: tables, NextToken: next}, nil
}

func (s *Server) deleteRouteTable(c *call) (response, *apiError) {
	id, err := c.p.required("RouteTableId")
	if err != nil {
		return nil, err
	}
	if err := c.p.done(); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	rs := s.lookupRegion(c.region)
	rt, err := rs.routeTable(id)
	if err != nil {
		return nil, err
	}
	if len(rt.Associations) > 0 {
		return nil, dependencyViolation("routeTable", id)
	}
	delete(rs.routeTables, id)
	return &returnResponse{Return: true}, nil
}

type associateRouteTableResponse struct {
	meta
	AssociationID    string              `xml:"associationId"`
	AssociationState associationStateXML `xml:"associationState"`
}

func (s *Server) associateRouteTable(c *call) (response, *apiError) {
	id, err := c.p.required("RouteTableId")
	if err != nil {
		return nil, err
	}
	subnetID, err := c.p.required("SubnetId")
	if err != nil {
		return nil, err
	}
	if err := c.p.done(); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	rs := s.lookupRegion(c.region)
	rt, err := rs.routeTable(id)
	if err != nil {
		return nil, err
	}
	sub, err := rs.subnet(subnetID)
	if err != nil {
		return nil, err
	}
	if sub.VPCID != rt.VPCID {
		return nil, invalidValue("Route table %s and subnet %s belong to different networks", id, subnetID)
	}
	for _, other := range rs.routeTables {
		if slices.ContainsFunc(other.Associations, func(a RouteTableAssociation) bool { return a.SubnetID == subnetID }) {
			return nil, clientError("Resource.AlreadyAssociated", "the specified association for route table %s conflicts with an existing association", id)
		}
	}
	a := RouteTableAssociation{AssociationID: s.newID("rtbassoc"), SubnetID: subnetID}
	rt.Associations = append(rt.Associations, a)
	return &associateRouteTableResponse{AssociationID: a.AssociationID, AssociationState: associationStateXML{associationCurrent}}, nil
}

func (s *Server) disassociateRouteTable(c *call) (response, *apiError) {
	id, err := c.p.required("AssociationId")
	if err != nil {
		return nil, err
	}
	if err := c.p.done(); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, rt := range s.lookupRegion(c.region).routeTables {
		i := slices.IndexFunc(rt.Associations, func(a RouteTableAssociation) bool { return a.AssociationID == id })
		if i < 0 {
			continue
		}
		if rt.Associations[i].Main {
			return nil, invalidValue("Cannot disassociate the main route table association %s", id)
		}
		rt.Associations = slices.Delete(rt.Associations, i, i+1)
		return &returnResponse{Return: true}, nil
	}
	return nil, clientError("InvalidAssociationID.NotFound", "The association ID '%s' does not exist", id)
}

func (s *Server) createRoute(c *call) (response, *apiError) {
	id, err := c.p.required("RouteTableId")
	if err != nil {
		return nil, err
	}
	destination, err := c.p.required("DestinationCidrBlock")
	if err != nil {
		return nil, err
	}
	prefix, perr := netip.ParsePrefix(destination)
	if perr != nil || !prefix.Addr().Is4() {
		return nil, invalidValue("Value (%s) for parameter destinationCidrBlock is invalid. This is not a valid CIDR block.", destination)
	}
	r := Route{
		DestinationCIDRBlock: prefix.Masked().String(), GatewayID: c.p.get("GatewayId"), NATGatewayID: c.p.get("NatGatewayId"),
		TransitGatewayID: c.p.get("TransitGatewayId"), Origin: originRoute,
	}
	targets := 0
	for _, t := range []string{r.GatewayID, r.NATGatewayID, r.TransitGatewayID} {
		if t != "" {
			targets++
		}
	}
	if targets != 1 {
		return nil, clientError("InvalidParameterCombination", "The request must contain exactly one of gatewayId, natGatewayId or transitGatewayId")
	}
	if err := c.p.done(); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	rs := s.lookupRegion(c.region)
	rt, err := rs.routeTable(id)
	if err != nil {
		return nil, err
	}
	if err := rs.checkRouteTarget(rt, r); err != nil {
		return nil, err
	}
	for _, have := range rt.Routes {
		if have.DestinationCIDRBlock == r.DestinationCIDRBlock {
			return nil, clientError("RouteAlreadyExists", "The route identified by %s already exists.", r.DestinationCIDRBlock)
		}
	}
	if v, ok := rs.vpcs[rt.VPCID]; ok && contains(netip.MustParsePrefix(v.CIDRBlock), prefix.Masked()) {
		return nil, invalidValue("The destination CIDR block %s is equal to or more specific than one of this VPC's CIDR blocks. This route can target only an interface or an instance.", destination)
	}
	rt.Routes = append(rt.Routes, r)
	return &returnResponse{Return: true}, nil
}

// checkRouteTarget checks that r's target exists and can take traffic for
// rt's VPC. The caller holds s.mu.
func (rs *regionState) checkRouteTarget(rt *RouteTable, r Route) *apiError {
	switch {
	case r.GatewayID != "":
		igw, ok := rs.internetGateways[r.GatewayID]
		if !ok || !strings.HasPrefix(r.GatewayID, "igw-") {
			return clientError("InvalidGatewayID.NotFound", "The gateway ID '%s' does not exist", r.GatewayID)
		}
		if igw.VPCID != rt.VPCID {
			return invalidValue("route table %s and network gateway %s belong to different networks", rt.RouteTableID, r.GatewayID)
		}
	case r.NATGatewayID != "":
		nat, ok := rs.natGateways[r.NATGatewayID]
		if !ok || nat.gone() {
			return natGatewayNotFound(r.NATGatewayID)
		}
		if nat.VPCID != rt.VPCID {
			return invalidValue("route table %s and NAT gateway %s belong to different networks", rt.RouteTableID, r.NATGatewayID)
		}
	case r.TransitGatewayID != "":
		return rs.checkTransitGateway(r.TransitGatewayID)
	}
	return nil
}

// RouteTables returns every route table the server holds, ordered by region
// and ID.
func (s *Server) RouteTables() []RouteTable {
	return collect(s, routeTablesOf, (*RouteTable).clone)
}
