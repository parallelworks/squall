package compute

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strconv"
	"strings"
)

// Router is a Cloud Router, in the API's JSON shape. Its Cloud NAT
// gateways are its nats. BGP settings, peers and interfaces keep the JSON
// the caller sent.
type Router struct {
	Kind                        string          `json:"kind"`
	ID                          uint64          `json:"id,string"`
	CreationTimestamp           string          `json:"creationTimestamp"`
	Name                        string          `json:"name"`
	Description                 string          `json:"description,omitzero"`
	Region                      string          `json:"region"`
	Network                     string          `json:"network"`
	Nats                        []RouterNat     `json:"nats,omitzero"`
	Bgp                         json.RawMessage `json:"bgp,omitzero"`
	BgpPeers                    json.RawMessage `json:"bgpPeers,omitzero"`
	Interfaces                  json.RawMessage `json:"interfaces,omitzero"`
	EncryptedInterconnectRouter bool            `json:"encryptedInterconnectRouter,omitzero"`
	SelfLink                    string          `json:"selfLink"`
}

// RouterNat is a Cloud NAT gateway on a router.
type RouterNat struct {
	Name                             string                `json:"name"`
	Type                             string                `json:"type,omitzero"`
	SourceSubnetworkIPRangesToNat    string                `json:"sourceSubnetworkIpRangesToNat"`
	Subnetworks                      []RouterNatSubnetwork `json:"subnetworks,omitzero"`
	NatIPAllocateOption              string                `json:"natIpAllocateOption,omitzero"`
	NatIPs                           []string              `json:"natIps,omitzero"`
	DrainNatIPs                      []string              `json:"drainNatIps,omitzero"`
	LogConfig                        *RouterNatLogConfig   `json:"logConfig,omitzero"`
	MinPortsPerVM                    int                   `json:"minPortsPerVm,omitzero"`
	MaxPortsPerVM                    int                   `json:"maxPortsPerVm,omitzero"`
	EnableDynamicPortAllocation      bool                  `json:"enableDynamicPortAllocation,omitzero"`
	EnableEndpointIndependentMapping *bool                 `json:"enableEndpointIndependentMapping,omitzero"`
	EndpointTypes                    []string              `json:"endpointTypes,omitzero"`
	UDPIdleTimeoutSec                int                   `json:"udpIdleTimeoutSec,omitzero"`
	ICMPIdleTimeoutSec               int                   `json:"icmpIdleTimeoutSec,omitzero"`
	TCPEstablishedIdleTimeoutSec     int                   `json:"tcpEstablishedIdleTimeoutSec,omitzero"`
	TCPTransitoryIdleTimeoutSec      int                   `json:"tcpTransitoryIdleTimeoutSec,omitzero"`
	TCPTimeWaitTimeoutSec            int                   `json:"tcpTimeWaitTimeoutSec,omitzero"`
	AutoNetworkTier                  string                `json:"autoNetworkTier,omitzero"`
	Rules                            json.RawMessage       `json:"rules,omitzero"`
}

// RouterNatSubnetwork is a subnetwork a NAT serves, when it serves a list of them.
type RouterNatSubnetwork struct {
	Name                  string   `json:"name"`
	SourceIPRangesToNat   []string `json:"sourceIpRangesToNat,omitzero"`
	SecondaryIPRangeNames []string `json:"secondaryIpRangeNames,omitzero"`
}

// RouterNatLogConfig is what a NAT logs.
type RouterNatLogConfig struct {
	Enable bool   `json:"enable"`
	Filter string `json:"filter,omitzero"`
}

func (rt *Router) clone() Router { return jsonClone(rt) }

var routerFields = fieldSet{names: []string{"name", "description", "id", "network"}}

func (rt *Router) filterValue(field string) string {
	switch field {
	case "name":
		return rt.Name
	case "description":
		return rt.Description
	case "id":
		return strconv.FormatUint(rt.ID, 10)
	case "network":
		return rt.Network
	}
	return ""
}

func routerPath(region, name string) string { return region + "/routers/" + name }

var (
	natSourceRanges  = []string{"ALL_SUBNETWORKS_ALL_IP_RANGES", "ALL_SUBNETWORKS_ALL_PRIMARY_IP_RANGES", "LIST_OF_SUBNETWORKS"}
	natAllocations   = []string{"AUTO_ONLY", "MANUAL_ONLY"}
	natLogFilters    = []string{"ERRORS_ONLY", "TRANSLATIONS_ONLY", "ALL"}
	natSubnetSources = []string{"ALL_IP_RANGES", "PRIMARY_IP_RANGE", "LIST_OF_SECONDARY_IP_RANGES"}
)

// configureNats checks a router's NATs in region on network, and returns
// them with the links they hold made full. The caller holds s.mu.
func (s *Server) configureNats(region, network string, nats []RouterNat) ([]RouterNat, *apiError) {
	project := projectOf(region)
	out := make([]RouterNat, 0, len(nats))
	var names []string
	for i, nat := range nats {
		f := fmt.Sprintf("resource.nats[%d]", i)
		if !validName(nat.Name) {
			return nil, invalid("Invalid value for field '%s.name': '%s'. Must be a match of regex '(?:[a-z](?:[-a-z0-9]{0,61}[a-z0-9])?)'", f, nat.Name)
		}
		if slices.Contains(names, nat.Name) {
			return nil, invalid("Invalid value for field '%s.name': '%s'. NAT names must be unique within a router.", f, nat.Name)
		}
		names = append(names, nat.Name)
		if t := nat.Type; t != "" && t != "PUBLIC" {
			return nil, invalid("Invalid value for field '%s.type': '%s'. squall: only PUBLIC NATs are supported.", f, t)
		}
		if !slices.Contains(natSourceRanges, nat.SourceSubnetworkIPRangesToNat) {
			return nil, invalid("Invalid value for field '%s.sourceSubnetworkIpRangesToNat': '%s'. Must be one of %v.", f, nat.SourceSubnetworkIPRangesToNat, natSourceRanges)
		}
		if (nat.SourceSubnetworkIPRangesToNat == "LIST_OF_SUBNETWORKS") != (len(nat.Subnetworks) > 0) {
			return nil, invalid("Invalid value for field '%s.subnetworks': subnetworks must be listed when, and only when, sourceSubnetworkIpRangesToNat is LIST_OF_SUBNETWORKS.", f)
		}
		nat.Subnetworks = slices.Clone(nat.Subnetworks)
		for j := range nat.Subnetworks {
			sub := &nat.Subnetworks[j]
			sf := fmt.Sprintf("%s.subnetworks[%d]", f, j)
			ref := sub.Name
			if !strings.Contains(ref, "/") {
				ref = subnetworkPath(region, ref)
			}
			inRegion, name, ok := strings.Cut(relPath(ref, project), "/subnetworks/")
			sn, found := lookupIn(s.subnetworks, inRegion, name)
			if !ok || inRegion != region {
				return nil, invalid("Invalid value for field '%s.name': '%s'. The subnetwork must be in region '%s'.", sf, sub.Name, region)
			}
			if !found || sn.Network != network {
				return nil, missingRef(sf+".name", sub.Name, "subnetwork")
			}
			sub.Name = sn.SelfLink
			for _, src := range sub.SourceIPRangesToNat {
				if !slices.Contains(natSubnetSources, src) {
					return nil, invalid("Invalid value for field '%s.sourceIpRangesToNat': '%s'.", sf, src)
				}
			}
		}
		if !slices.Contains(natAllocations, nat.NatIPAllocateOption) {
			return nil, invalid("Invalid value for field '%s.natIpAllocateOption': '%s'. Must be one of %v.", f, nat.NatIPAllocateOption, natAllocations)
		}
		if (nat.NatIPAllocateOption == "MANUAL_ONLY") != (len(nat.NatIPs) > 0) {
			return nil, invalid("Invalid value for field '%s.natIps': NAT IPs must be given when, and only when, natIpAllocateOption is MANUAL_ONLY.", f)
		}
		nat.NatIPs = slices.Clone(nat.NatIPs)
		for j, ref := range nat.NatIPs {
			if !strings.Contains(ref, "/") {
				ref = addressPath(region, ref)
			}
			inRegion, name, ok := strings.Cut(relPath(ref, project), "/addresses/")
			if _, found := lookupIn(s.addresses, inRegion, name); !ok || inRegion != region || !found {
				return nil, missingRef(fmt.Sprintf("%s.natIps[%d]", f, j), nat.NatIPs[j], "address")
			}
			nat.NatIPs[j] = selfLinkBase + addressPath(inRegion, name)
		}
		if lc := nat.LogConfig; lc != nil && lc.Filter != "" && !slices.Contains(natLogFilters, lc.Filter) {
			return nil, invalid("Invalid value for field '%s.logConfig.filter': '%s'. Must be one of %v.", f, lc.Filter, natLogFilters)
		}
		if nat.LogConfig != nil {
			nat.LogConfig = new(*nat.LogConfig)
		}
		out = append(out, nat)
	}
	return out, nil
}

func (s *Server) insertRouter(r *http.Request, opFault *apiError) (any, *apiError) {
	var want Router
	if err := decodeStrict(r, &want); err != nil {
		return nil, err
	}
	if !validName(want.Name) {
		return nil, invalid("Invalid value for field 'resource.name': '%s'. Must be a match of regex '(?:[a-z](?:[-a-z0-9]{0,61}[a-z0-9])?)'", want.Name)
	}
	reqID, err := requestID(r)
	if err != nil {
		return nil, err
	}
	project, region := r.PathValue("project"), regionPath(r)
	path := routerPath(region, want.Name)
	s.mu.Lock()
	defer s.mu.Unlock()
	if op, ok := s.requests[reqID]; ok {
		return encoded(op), nil
	}
	if _, ok := lookupIn(s.routers, region, want.Name); ok {
		return nil, alreadyExists(path)
	}
	n, err := s.resolveNetwork(project, want.Network)
	if err != nil {
		return nil, err
	}
	nats, err := s.configureNats(region, n.SelfLink, want.Nats)
	if err != nil {
		return nil, err
	}
	if opFault != nil {
		return encoded(s.startOperation(r, reqID, "insert", path, 0, opFault, nil)), nil
	}
	rt := want.clone()
	rt.Kind, rt.ID, rt.CreationTimestamp = "compute#router", s.newID(), timestamp(s.now())
	rt.Network, rt.Region, rt.SelfLink, rt.Nats = n.SelfLink, selfLinkBase+region, selfLinkBase+path, nats
	scoped(s.routers, region).put(rt.Name, &rt)
	return encoded(s.startOperation(r, reqID, "insert", path, rt.ID, nil, nil)), nil
}

// patchRouter applies a JSON merge patch, as the API's patch does: the
// fields the body holds replace the router's, a list such as nats whole.
func (s *Server) patchRouter(r *http.Request, opFault *apiError) (any, *apiError) {
	body, rerr := io.ReadAll(io.LimitReader(r.Body, maxBody))
	if rerr != nil {
		return nil, invalid("reading the request body: %v", rerr)
	}
	var patch Router
	if err := unmarshalStrict(body, &patch); err != nil {
		return nil, err
	}
	var present map[string]json.RawMessage
	if err := json.Unmarshal(body, &present); err != nil {
		return nil, invalid("Invalid JSON payload received: %v", err)
	}
	reqID, err := requestID(r)
	if err != nil {
		return nil, err
	}
	project, region, name := r.PathValue("project"), regionPath(r), r.PathValue("name")
	path := routerPath(region, name)
	s.mu.Lock()
	defer s.mu.Unlock()
	if op, ok := s.requests[reqID]; ok {
		return encoded(op), nil
	}
	rt, ok := lookupIn(s.routers, region, name)
	if !ok {
		return nil, notFound(path)
	}
	if _, ok := present["name"]; ok && patch.Name != rt.Name {
		return nil, invalid("Invalid value for field 'resource.name': '%s'. A router's name cannot be changed.", patch.Name)
	}
	if _, ok := present["network"]; ok {
		if link, ok := networkLink(project, patch.Network); !ok || link != rt.Network {
			return nil, invalid("Invalid value for field 'resource.network': '%s'. A router's network cannot be changed.", patch.Network)
		}
	}
	next := rt.clone()
	if _, ok := present["nats"]; ok {
		nats, err := s.configureNats(region, rt.Network, patch.Nats)
		if err != nil {
			return nil, err
		}
		next.Nats = nats
	}
	if _, ok := present["description"]; ok {
		next.Description = patch.Description
	}
	for _, raw := range []struct {
		key   string
		field *json.RawMessage
	}{{"bgp", &next.Bgp}, {"bgpPeers", &next.BgpPeers}, {"interfaces", &next.Interfaces}} {
		if v, ok := present[raw.key]; ok {
			*raw.field = nil
			if string(v) != "null" {
				*raw.field = v
			}
		}
	}
	if _, ok := present["encryptedInterconnectRouter"]; ok {
		next.EncryptedInterconnectRouter = patch.EncryptedInterconnectRouter
	}
	if opFault == nil {
		*rt = next
	}
	return encoded(s.startOperation(r, reqID, "patch", path, rt.ID, opFault, nil)), nil
}

func (s *Server) getRouter(r *http.Request, _ *apiError) (any, *apiError) {
	region, name := regionPath(r), r.PathValue("name")
	s.mu.Lock()
	defer s.mu.Unlock()
	rt, ok := lookupIn(s.routers, region, name)
	if !ok {
		return nil, notFound(routerPath(region, name))
	}
	return encoded(rt), nil
}

func (s *Server) deleteRouter(r *http.Request, opFault *apiError) (any, *apiError) {
	reqID, err := requestID(r)
	if err != nil {
		return nil, err
	}
	region, name := regionPath(r), r.PathValue("name")
	path := routerPath(region, name)
	s.mu.Lock()
	defer s.mu.Unlock()
	if op, ok := s.requests[reqID]; ok {
		return encoded(op), nil
	}
	rt, ok := lookupIn(s.routers, region, name)
	if !ok {
		return nil, notFound(path)
	}
	if opFault == nil {
		s.routers[region].remove(name)
	}
	return encoded(s.startOperation(r, reqID, "delete", path, rt.ID, opFault, nil)), nil
}

func (s *Server) listRouters(r *http.Request, _ *apiError) (any, *apiError) {
	q, err := parseListQuery(r, routerFields)
	if err != nil {
		return nil, err
	}
	region := regionPath(r)
	s.mu.Lock()
	defer s.mu.Unlock()
	return listScope(s.routers[region], q, "compute#routerList", region+"/routers", nil), nil
}

func (s *Server) aggregatedRouters(r *http.Request, _ *apiError) (any, *apiError) {
	q, err := parseListQuery(r, routerFields)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return listAggregated(s.routers, r.PathValue("project"), q, "compute#routerAggregatedList", "routers", nil), nil
}

// Routers returns every router the server holds, ordered by selfLink.
func (s *Server) Routers() []Router {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []Router
	each(s.routers, func(rt *Router) { out = append(out, rt.clone()) })
	return out
}
