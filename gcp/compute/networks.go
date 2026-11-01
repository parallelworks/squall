package compute

import (
	"cmp"
	"fmt"
	"hash/fnv"
	"net/http"
	"slices"
	"strconv"
	"strings"
)

const (
	defaultMTU = 1460
	minMTU     = 1300
	maxMTU     = 8896
)

// Network is a VPC network, in the API's JSON shape.
type Network struct {
	Kind                                  string           `json:"kind"`
	ID                                    uint64           `json:"id,string"`
	CreationTimestamp                     string           `json:"creationTimestamp"`
	Name                                  string           `json:"name"`
	Description                           string           `json:"description,omitzero"`
	AutoCreateSubnetworks                 *bool            `json:"autoCreateSubnetworks,omitzero"`
	IPv4Range                             string           `json:"IPv4Range,omitzero"`
	Subnetworks                           []string         `json:"subnetworks,omitzero"`
	Peerings                              []NetworkPeering `json:"peerings,omitzero"`
	RoutingConfig                         *RoutingConfig   `json:"routingConfig,omitzero"`
	MTU                                   int              `json:"mtu,omitzero"`
	NetworkFirewallPolicyEnforcementOrder string           `json:"networkFirewallPolicyEnforcementOrder,omitzero"`
	NetworkProfile                        string           `json:"networkProfile,omitzero"`
	SelfLink                              string           `json:"selfLink"`
	SelfLinkWithID                        string           `json:"selfLinkWithId"`
}

// RoutingConfig is how a network's routers share routes.
type RoutingConfig struct {
	RoutingMode              string `json:"routingMode,omitzero"`
	BgpBestPathSelectionMode string `json:"bgpBestPathSelectionMode,omitzero"`
}

// NetworkPeering is a peering of a network with another, such as the one a
// private services access connection makes.
type NetworkPeering struct {
	Name                           string `json:"name"`
	Network                        string `json:"network"`
	State                          string `json:"state"`
	StateDetails                   string `json:"stateDetails,omitzero"`
	AutoCreateRoutes               bool   `json:"autoCreateRoutes"`
	ExchangeSubnetRoutes           bool   `json:"exchangeSubnetRoutes"`
	ExportCustomRoutes             bool   `json:"exportCustomRoutes"`
	ImportCustomRoutes             bool   `json:"importCustomRoutes"`
	ExportSubnetRoutesWithPublicIP bool   `json:"exportSubnetRoutesWithPublicIp"`
	ImportSubnetRoutesWithPublicIP bool   `json:"importSubnetRoutesWithPublicIp"`
	StackType                      string `json:"stackType,omitzero"`
}

func (n *Network) clone() Network {
	c := *n
	c.Subnetworks = slices.Clone(n.Subnetworks)
	c.Peerings = slices.Clone(n.Peerings)
	if n.RoutingConfig != nil {
		c.RoutingConfig = new(*n.RoutingConfig)
	}
	if n.AutoCreateSubnetworks != nil {
		c.AutoCreateSubnetworks = new(*n.AutoCreateSubnetworks)
	}
	return c
}

var networkFields = fieldSet{names: []string{"name", "description", "id"}}

func (n *Network) filterValue(field string) string {
	switch field {
	case "name":
		return n.Name
	case "description":
		return n.Description
	case "id":
		return strconv.FormatUint(n.ID, 10)
	}
	return ""
}

func networkPath(global, name string) string { return global + "/networks/" + name }

// networkLink is the selfLink of the network ref names, a bare name or a
// URL, in project, and whether ref is well formed.
func networkLink(project, ref string) (string, bool) {
	if !strings.Contains(ref, "/") {
		ref = "global/networks/" + ref
	}
	p := relPath(ref, project)
	owner, name, ok := strings.Cut(p, "/global/networks/")
	if !ok || !validName(name) || !strings.HasPrefix(owner, "projects/") || strings.Contains(owner[len("projects/"):], "/") {
		return "", false
	}
	return selfLinkBase + p, true
}

// resolveNetwork returns the network a resource's network field, ref, names
// in project, which must exist. The caller holds s.mu.
func (s *Server) resolveNetwork(project, ref string) (*Network, *apiError) {
	const field = "resource.network"
	if ref == "" {
		return nil, invalid("Invalid value for field '%s': ''. The network must be specified.", field)
	}
	link, ok := networkLink(project, ref)
	if !ok {
		return nil, invalid("Invalid value for field '%s': '%s'. The URL is malformed.", field, ref)
	}
	path := strings.TrimPrefix(link, selfLinkBase)
	global, name, _ := strings.Cut(path, "/networks/")
	n, ok := lookupIn(s.networks, global, name)
	if !ok {
		return nil, notFound(path)
	}
	return n, nil
}

// lookupNetwork returns the network r names by name or by ID. The caller holds s.mu.
func (s *Server) lookupNetwork(global, name string) (*Network, bool) {
	if n, ok := lookupIn(s.networks, global, name); ok {
		return n, true
	}
	if id, err := strconv.ParseUint(name, 10, 64); err == nil && s.networks[global] != nil {
		for _, n := range s.networks[global].byName {
			if n.ID == id {
				return n, true
			}
		}
	}
	return nil, false
}

func validateNetwork(project string, n *Network) *apiError {
	if !validName(n.Name) {
		return invalid("Invalid value for field 'resource.name': '%s'. Must be a match of regex '(?:[a-z](?:[-a-z0-9]{0,61}[a-z0-9])?)'", n.Name)
	}
	if n.IPv4Range != "" {
		return invalid("Invalid value for field 'resource.IPv4Range': '%s'. squall: legacy networks are not supported.", n.IPv4Range)
	}
	switch {
	case n.AutoCreateSubnetworks == nil:
		return invalid("Invalid value for field 'resource.autoCreateSubnetworks': ''. squall: only custom mode networks, with autoCreateSubnetworks false, are supported.")
	case *n.AutoCreateSubnetworks:
		return invalid("Invalid value for field 'resource.autoCreateSubnetworks': 'true'. squall: auto mode networks are not supported.")
	}
	if n.MTU != 0 && (n.MTU < minMTU || n.MTU > maxMTU) {
		return invalid("Invalid value for field 'resource.mtu': '%d'. Must be between %d and %d.", n.MTU, minMTU, maxMTU)
	}
	rc := cmp.Or(n.RoutingConfig, &RoutingConfig{})
	if m := rc.RoutingMode; m != "" && m != "REGIONAL" && m != "GLOBAL" {
		return invalid("Invalid value for field 'resource.routingConfig.routingMode': '%s'.", m)
	}
	if m := rc.BgpBestPathSelectionMode; m != "" && m != "LEGACY" && m != "STANDARD" {
		return invalid("Invalid value for field 'resource.routingConfig.bgpBestPathSelectionMode': '%s'.", m)
	}
	if o := n.NetworkFirewallPolicyEnforcementOrder; o != "" && o != "AFTER_CLASSIC_FIREWALL" && o != "BEFORE_CLASSIC_FIREWALL" {
		return invalid("Invalid value for field 'resource.networkFirewallPolicyEnforcementOrder': '%s'.", o)
	}
	if n.NetworkProfile != "" {
		n.NetworkProfile = selfLinkBase + relPath(n.NetworkProfile, project)
	}
	return nil
}

func (s *Server) insertNetwork(r *http.Request, opFault *apiError) (any, *apiError) {
	var want Network
	if err := decodeStrict(r, &want); err != nil {
		return nil, err
	}
	project := r.PathValue("project")
	if err := validateNetwork(project, &want); err != nil {
		return nil, err
	}
	reqID, err := requestID(r)
	if err != nil {
		return nil, err
	}
	global := scopeOf(r)
	path := networkPath(global, want.Name)
	s.mu.Lock()
	defer s.mu.Unlock()
	if op, ok := s.requests[reqID]; ok {
		return encoded(op), nil
	}
	if _, ok := lookupIn(s.networks, global, want.Name); ok {
		return nil, alreadyExists(path)
	}
	if opFault != nil {
		return encoded(s.startOperation(r, reqID, "insert", path, 0, opFault, nil)), nil
	}
	rc := cmp.Or(want.RoutingConfig, &RoutingConfig{})
	n := &Network{
		Kind: "compute#network", ID: s.newID(), CreationTimestamp: timestamp(s.now()),
		Name: want.Name, Description: want.Description, AutoCreateSubnetworks: new(false),
		RoutingConfig: &RoutingConfig{
			RoutingMode:              cmp.Or(rc.RoutingMode, "REGIONAL"),
			BgpBestPathSelectionMode: cmp.Or(rc.BgpBestPathSelectionMode, "LEGACY"),
		},
		MTU:                                   cmp.Or(want.MTU, defaultMTU),
		NetworkFirewallPolicyEnforcementOrder: cmp.Or(want.NetworkFirewallPolicyEnforcementOrder, "AFTER_CLASSIC_FIREWALL"),
		NetworkProfile:                        want.NetworkProfile,
		SelfLink:                              selfLinkBase + path,
	}
	n.SelfLinkWithID = selfLinkBase + networkPath(global, strconv.FormatUint(n.ID, 10))
	scoped(s.networks, global).put(n.Name, n)
	s.addDefaultRoute(global, n)
	return encoded(s.startOperation(r, reqID, "insert", path, n.ID, nil, nil)), nil
}

// addDefaultRoute adds the route to the internet the API creates with every
// network, which goes when the network does. The caller holds s.mu.
func (s *Server) addDefaultRoute(global string, n *Network) {
	h := fnv.New64a()
	fmt.Fprint(h, n.SelfLink, n.ID)
	name := fmt.Sprintf("default-route-%016x", h.Sum64())
	scoped(s.routes, global).put(name, &Route{
		Kind: "compute#route", ID: s.newID(), CreationTimestamp: n.CreationTimestamp,
		Name: name, Description: "Default route to the Internet.", Network: n.SelfLink,
		DestRange: "0.0.0.0/0", Priority: new(uint32(1000)),
		NextHopGateway: selfLinkBase + global + "/gateways/default-internet-gateway",
		RouteType:      "STATIC", SelfLink: selfLinkBase + routePath(global, name), auto: true,
	})
}

// view is n as a read returns it, with the subnetworks in it. The caller holds s.mu.
func (s *Server) view(n *Network) Network {
	c := n.clone()
	c.Subnetworks = nil
	each(s.subnetworks, func(sn *Subnetwork) {
		if sn.Network == n.SelfLink {
			c.Subnetworks = append(c.Subnetworks, sn.SelfLink)
		}
	})
	return c
}

func (s *Server) getNetwork(r *http.Request, _ *apiError) (any, *apiError) {
	global, name := scopeOf(r), r.PathValue("name")
	s.mu.Lock()
	defer s.mu.Unlock()
	n, ok := s.lookupNetwork(global, name)
	if !ok {
		return nil, notFound(networkPath(global, name))
	}
	return encoded(s.view(n)), nil
}

// networkUser is a resource that keeps n from being deleted, or "". The caller holds s.mu.
func (s *Server) networkUser(global string, n *Network) string {
	var users []string
	use := func(selfLink, network string) {
		if network == n.SelfLink {
			users = append(users, strings.TrimPrefix(selfLink, selfLinkBase))
		}
	}
	each(s.subnetworks, func(sn *Subnetwork) { use(sn.SelfLink, sn.Network) })
	each(s.routers, func(rt *Router) { use(rt.SelfLink, rt.Network) })
	each(s.firewalls, func(f *Firewall) { use(f.SelfLink, f.Network) })
	each(s.routes, func(rt *Route) {
		if !rt.auto {
			use(rt.SelfLink, rt.Network)
		}
	})
	each(s.globalAddresses, func(a *Address) { use(a.SelfLink, a.Network) })
	each(s.instances, func(inst *Instance) {
		for _, nic := range inst.NetworkInterfaces {
			use(inst.SelfLink, nic.Network)
		}
	})
	for _, p := range n.Peerings {
		users = append(users, networkPath(global, n.Name)+"/peerings/"+p.Name)
	}
	if len(users) == 0 {
		return ""
	}
	return users[0]
}

func (s *Server) deleteNetwork(r *http.Request, opFault *apiError) (any, *apiError) {
	reqID, err := requestID(r)
	if err != nil {
		return nil, err
	}
	global, name := scopeOf(r), r.PathValue("name")
	s.mu.Lock()
	defer s.mu.Unlock()
	if op, ok := s.requests[reqID]; ok {
		return encoded(op), nil
	}
	n, ok := s.lookupNetwork(global, name)
	if !ok {
		return nil, notFound(networkPath(global, name))
	}
	path := networkPath(global, n.Name)
	if user := s.networkUser(global, n); user != "" {
		return nil, inUse("network", path, user)
	}
	if opFault == nil {
		s.networks[global].remove(n.Name)
		if routes := s.routes[global]; routes != nil {
			for _, name := range slices.Clone(routes.names) {
				if rt := routes.byName[name]; rt.auto && rt.Network == n.SelfLink {
					routes.remove(name)
				}
			}
		}
	}
	return encoded(s.startOperation(r, reqID, "delete", path, n.ID, opFault, nil)), nil
}

func (s *Server) listNetworks(r *http.Request, _ *apiError) (any, *apiError) {
	q, err := parseListQuery(r, networkFields)
	if err != nil {
		return nil, err
	}
	global := scopeOf(r)
	s.mu.Lock()
	defer s.mu.Unlock()
	views := &collection[*Network]{byName: map[string]*Network{}}
	if c := s.networks[global]; c != nil {
		for _, name := range c.names {
			v := s.view(c.byName[name])
			views.put(name, &v)
		}
	}
	return listScope(views, q, "compute#networkList", global+"/networks", nil), nil
}

// Networks returns every network the server holds, ordered by selfLink.
func (s *Server) Networks() []Network {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []Network
	each(s.networks, func(n *Network) { out = append(out, s.view(n)) })
	return out
}

// LookupNetwork returns the named network in project.
func (s *Server) LookupNetwork(project, name string) (Network, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	n, ok := lookupIn(s.networks, "projects/"+project+"/global", name)
	if !ok {
		return Network{}, false
	}
	return s.view(n), true
}

// AddPeering adds p to the named network in project, as a peering made by
// another service does, such as a private services access connection. A
// network with a peering cannot be deleted. It fails if the network does not
// exist or already has a peering of p's name.
func (s *Server) AddPeering(project, network string, p NetworkPeering) error {
	global := "projects/" + project + "/global"
	s.mu.Lock()
	defer s.mu.Unlock()
	n, ok := lookupIn(s.networks, global, network)
	if !ok {
		return notFound(networkPath(global, network))
	}
	if slices.ContainsFunc(n.Peerings, func(q NetworkPeering) bool { return q.Name == p.Name }) {
		return alreadyExists(networkPath(global, network) + "/peerings/" + p.Name)
	}
	n.Peerings = append(n.Peerings, p)
	return nil
}

// RemovePeering removes the named peering from the named network in project,
// and reports whether it was there.
func (s *Server) RemovePeering(project, network, name string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	n, ok := lookupIn(s.networks, "projects/"+project+"/global", network)
	if !ok {
		return false
	}
	before := len(n.Peerings)
	n.Peerings = slices.DeleteFunc(n.Peerings, func(q NetworkPeering) bool { return q.Name == name })
	return len(n.Peerings) != before
}
