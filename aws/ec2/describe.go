package ec2

import (
	"maps"
	"slices"
	"strings"
)

// lister describes one kind of resource a region holds by ID.
type lister[V any] struct {
	// ids is the parameter that names resources, such as "VpcId".
	ids    string
	prefix string
	of     func(*regionState) map[string]V
	// empty is a resource with no values, for checking filter names.
	empty    V
	fields   func(*regionState, V) func(string) ([]string, bool)
	notFound func(id string) *apiError
	// advance, if set, counts a describe of a resource the call names or
	// that it covers by naming none.
	advance func(*regionState, V)
	// byName, if set, returns the IDs of the resources the call names
	// another way, such as by name.
	byName func(*regionState) ([]string, *apiError)
}

// describe answers a describe call for l's resources: the ones it names, or
// all, that match its filters, a page at a time, rendered by render.
func describe[V, T any](s *Server, c *call, l lister[V], render func(*regionState, V) T) ([]T, string, *apiError) {
	ids := c.p.list(l.ids)
	filters, err := c.p.filters(l.fields(&regionState{}, l.empty))
	if err != nil {
		return nil, "", err
	}
	plural := l.ids
	if !strings.HasSuffix(plural, "s") {
		plural += "s"
	}
	limit, after, err := c.p.page(plural, len(ids) > 0 || l.byName != nil, maxResults, l.prefix)
	if err != nil {
		return nil, "", err
	}
	if err := c.p.done(); err != nil {
		return nil, "", err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	rs := s.lookupRegion(c.region)
	held := l.of(rs)
	if l.byName != nil {
		named, err := l.byName(rs)
		if err != nil {
			return nil, "", err
		}
		ids = append(ids, named...)
	}
	for _, id := range ids {
		if _, ok := held[id]; !ok {
			return nil, "", l.notFound(id)
		}
	}
	var matched []string
	for _, id := range slices.Sorted(maps.Keys(held)) {
		if len(ids) > 0 && !slices.Contains(ids, id) {
			continue
		}
		if l.advance != nil {
			l.advance(rs, held[id])
		}
		if matchFilters(filters, l.fields(rs, held[id])) {
			matched = append(matched, id)
		}
	}
	page, next := paginate(matched, func(id string) string { return id }, after, limit)
	out := make([]T, 0, len(page))
	for _, id := range page {
		out = append(out, render(rs, held[id]))
	}
	return out, next, nil
}

// collect returns a copy of every resource of one kind the server holds,
// ordered by region and ID.
func collect[V, T any](s *Server, of func(*regionState) map[string]V, clone func(V) T) []T {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []T
	for _, region := range slices.Sorted(maps.Keys(s.regions)) {
		held := of(s.regions[region])
		for _, id := range slices.Sorted(maps.Keys(held)) {
			out = append(out, clone(held[id]))
		}
	}
	return out
}

// createTarget names the resource a create call makes: a fresh ID with
// prefix, or what an earlier call with the same client token for kind made.
func createTarget[V any](s *Server, prefix, kind string, of func(*regionState) map[string]V) func(*call) string {
	return func(c *call) string {
		s.mu.Lock()
		defer s.mu.Unlock()
		if id, ok := s.tokenTarget(c, kind); kind != "" && ok {
			return c.region + "/" + id
		}
		c.id = unusedID(s, prefix, of(s.lookupRegion(c.region)))
		return c.region + "/" + c.id
	}
}

func vpcsOf(rs *regionState) map[string]*VPC { return rs.vpcs }

func subnetsOf(rs *regionState) map[string]*Subnet { return rs.subnets }

func internetGatewaysOf(rs *regionState) map[string]*InternetGateway { return rs.internetGateways }

func natGatewaysOf(rs *regionState) map[string]*NATGateway { return rs.natGateways }

func routeTablesOf(rs *regionState) map[string]*RouteTable { return rs.routeTables }

func dhcpOptionsOf(rs *regionState) map[string]*DHCPOptions { return rs.dhcpOptions }

func transitGatewaysOf(rs *regionState) map[string]*TransitGateway { return rs.transitGateways }

func securityGroupsOf(rs *regionState) map[string]*SecurityGroup { return rs.securityGroups }

func attachmentsOf(rs *regionState) map[string]*TransitGatewayAttachment {
	return rs.transitGatewayAttachments
}
