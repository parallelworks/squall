package compute

import (
	"cmp"
	"encoding/json"
	"net/http"
	"net/netip"
	"slices"
	"strconv"
	"strings"
)

const (
	minSubnetBits = 8
	maxSubnetBits = 29
)

// Subnetwork is a regional subnetwork of a VPC network, in the API's JSON shape.
type Subnetwork struct {
	Kind                    string           `json:"kind"`
	ID                      uint64           `json:"id,string"`
	CreationTimestamp       string           `json:"creationTimestamp"`
	Name                    string           `json:"name"`
	Description             string           `json:"description,omitzero"`
	Network                 string           `json:"network"`
	IPCidrRange             string           `json:"ipCidrRange"`
	GatewayAddress          string           `json:"gatewayAddress"`
	Region                  string           `json:"region"`
	SecondaryIPRanges       []SecondaryRange `json:"secondaryIpRanges,omitzero"`
	PrivateIPGoogleAccess   bool             `json:"privateIpGoogleAccess"`
	PrivateIPv6GoogleAccess string           `json:"privateIpv6GoogleAccess,omitzero"`
	Purpose                 string           `json:"purpose,omitzero"`
	StackType               string           `json:"stackType,omitzero"`
	LogConfig               json.RawMessage  `json:"logConfig,omitzero"`
	Fingerprint             string           `json:"fingerprint"`
	SelfLink                string           `json:"selfLink"`
}

// SecondaryRange is a subnetwork's secondary IP range, such as one for alias IPs.
type SecondaryRange struct {
	RangeName   string `json:"rangeName"`
	IPCidrRange string `json:"ipCidrRange"`
}

func (sn *Subnetwork) clone() Subnetwork {
	c := *sn
	c.SecondaryIPRanges = slices.Clone(sn.SecondaryIPRanges)
	c.LogConfig = slices.Clone(sn.LogConfig)
	return c
}

var subnetworkFields = fieldSet{names: []string{"name", "description", "id", "network", "ipCidrRange"}}

func (sn *Subnetwork) filterValue(field string) string {
	switch field {
	case "name":
		return sn.Name
	case "description":
		return sn.Description
	case "id":
		return strconv.FormatUint(sn.ID, 10)
	case "network":
		return sn.Network
	case "ipCidrRange":
		return sn.IPCidrRange
	}
	return ""
}

func subnetworkPath(region, name string) string { return region + "/subnetworks/" + name }

// subnetRange parses an IPv4 CIDR range as a subnetwork takes it, with no host bits set.
func subnetRange(field, cidr string) (netip.Prefix, *apiError) {
	p, err := netip.ParsePrefix(cidr)
	if err != nil || !p.Addr().Is4() || p.Masked() != p || p.Bits() < minSubnetBits || p.Bits() > maxSubnetBits {
		return netip.Prefix{}, invalid("Invalid value for field '%s': '%s'. Invalid IPCidrRange: %s. It must be an IPv4 CIDR range with no host bits set, from /%d to /%d.", field, cidr, cidr, minSubnetBits, maxSubnetBits)
	}
	return p, nil
}

// ranges are every range sn holds, primary first.
func (sn *Subnetwork) ranges() []netip.Prefix {
	out := []netip.Prefix{netip.MustParsePrefix(sn.IPCidrRange)}
	for _, r := range sn.SecondaryIPRanges {
		out = append(out, netip.MustParsePrefix(r.IPCidrRange))
	}
	return out
}

// validateSubnetwork checks sn and returns its ranges, primary first.
func validateSubnetwork(sn *Subnetwork) ([]netip.Prefix, *apiError) {
	if !validName(sn.Name) {
		return nil, invalid("Invalid value for field 'resource.name': '%s'. Must be a match of regex '(?:[a-z](?:[-a-z0-9]{0,61}[a-z0-9])?)'", sn.Name)
	}
	if sn.IPCidrRange == "" {
		return nil, invalid("Invalid value for field 'resource.ipCidrRange': ''. The IP CIDR range must be specified.")
	}
	primary, err := subnetRange("resource.ipCidrRange", sn.IPCidrRange)
	if err != nil {
		return nil, err
	}
	if p := sn.Purpose; p != "" && p != "PRIVATE" {
		return nil, invalid("Invalid value for field 'resource.purpose': '%s'. squall: only PRIVATE subnetworks are supported.", p)
	}
	if t := sn.StackType; t != "" && t != "IPV4_ONLY" {
		return nil, invalid("Invalid value for field 'resource.stackType': '%s'. squall: only IPV4_ONLY subnetworks are supported.", t)
	}
	if a := sn.PrivateIPv6GoogleAccess; a != "" && a != "DISABLE_GOOGLE_ACCESS" && a != "ENABLE_OUTBOUND_VM_ACCESS_TO_GOOGLE" && a != "ENABLE_BIDIRECTIONAL_ACCESS_TO_GOOGLE" {
		return nil, invalid("Invalid value for field 'resource.privateIpv6GoogleAccess': '%s'.", a)
	}
	ranges := []netip.Prefix{primary}
	var names []string
	for i, r := range sn.SecondaryIPRanges {
		if !validName(r.RangeName) {
			return nil, invalid("Invalid value for field 'resource.secondaryIpRanges[%d].rangeName': '%s'. Must be a match of regex '(?:[a-z](?:[-a-z0-9]{0,61}[a-z0-9])?)'", i, r.RangeName)
		}
		if slices.Contains(names, r.RangeName) {
			return nil, invalid("Invalid value for field 'resource.secondaryIpRanges[%d].rangeName': '%s'. Secondary range names must be unique within a subnetwork.", i, r.RangeName)
		}
		p, err := subnetRange("resource.secondaryIpRanges["+strconv.Itoa(i)+"].ipCidrRange", r.IPCidrRange)
		if err != nil {
			return nil, err
		}
		for j, other := range ranges {
			if p.Overlaps(other) {
				with := sn.IPCidrRange
				if j > 0 {
					with = sn.SecondaryIPRanges[j-1].IPCidrRange
				}
				return nil, invalid("Invalid IPCidrRange: %s conflicts with %s in subnetwork '%s'.", r.IPCidrRange, with, sn.Name)
			}
		}
		names = append(names, r.RangeName)
		ranges = append(ranges, p)
	}
	return ranges, nil
}

// rangeConflict is the error for a range that overlaps another on network,
// or nil. The caller holds s.mu.
func (s *Server) rangeConflict(network string, ranges []netip.Prefix) *apiError {
	var conflict *apiError
	each(s.subnetworks, func(sn *Subnetwork) {
		if conflict != nil || sn.Network != network {
			return
		}
		for _, theirs := range sn.ranges() {
			for _, ours := range ranges {
				if ours.Overlaps(theirs) {
					conflict = invalid("Invalid IPCidrRange: %s conflicts with existing subnetwork '%s' in region '%s'.", ours, sn.Name, sn.Region[strings.LastIndexByte(sn.Region, '/')+1:])
					return
				}
			}
		}
	})
	if conflict != nil {
		return conflict
	}
	each(s.globalAddresses, func(a *Address) {
		if conflict != nil || a.Network != network || a.Purpose != purposePeering {
			return
		}
		theirs := netip.PrefixFrom(netip.MustParseAddr(a.Address), a.PrefixLength)
		for _, ours := range ranges {
			if ours.Overlaps(theirs) {
				conflict = invalid("Invalid IPCidrRange: %s conflicts with the allocated IP range '%s' (%s).", ours, a.Name, theirs)
				return
			}
		}
	})
	return conflict
}

func (s *Server) insertSubnetwork(r *http.Request, opFault *apiError) (any, *apiError) {
	var want Subnetwork
	if err := decodeStrict(r, &want); err != nil {
		return nil, err
	}
	ranges, err := validateSubnetwork(&want)
	if err != nil {
		return nil, err
	}
	reqID, err := requestID(r)
	if err != nil {
		return nil, err
	}
	project, region := r.PathValue("project"), regionPath(r)
	path := subnetworkPath(region, want.Name)
	s.mu.Lock()
	defer s.mu.Unlock()
	if op, ok := s.requests[reqID]; ok {
		return encoded(op), nil
	}
	if _, ok := lookupIn(s.subnetworks, region, want.Name); ok {
		return nil, alreadyExists(path)
	}
	n, err := s.resolveNetwork(project, want.Network)
	if err != nil {
		return nil, err
	}
	if err := s.rangeConflict(n.SelfLink, ranges); err != nil {
		return nil, err
	}
	if opFault != nil {
		return encoded(s.startOperation(r, reqID, "insert", path, 0, opFault, nil)), nil
	}
	sn := want.clone()
	sn.Kind, sn.ID, sn.CreationTimestamp = "compute#subnetwork", s.newID(), timestamp(s.now())
	sn.Network, sn.Region, sn.SelfLink = n.SelfLink, selfLinkBase+region, selfLinkBase+path
	sn.GatewayAddress = ranges[0].Addr().Next().String()
	sn.Purpose, sn.StackType = cmp.Or(sn.Purpose, "PRIVATE"), cmp.Or(sn.StackType, "IPV4_ONLY")
	sn.PrivateIPv6GoogleAccess = cmp.Or(sn.PrivateIPv6GoogleAccess, "DISABLE_GOOGLE_ACCESS")
	sn.Fingerprint = hash([]any{sn.ID, sn.IPCidrRange})
	scoped(s.subnetworks, region).put(sn.Name, &sn)
	return encoded(s.startOperation(r, reqID, "insert", path, sn.ID, nil, nil)), nil
}

func (s *Server) getSubnetwork(r *http.Request, _ *apiError) (any, *apiError) {
	region, name := regionPath(r), r.PathValue("name")
	s.mu.Lock()
	defer s.mu.Unlock()
	sn, ok := lookupIn(s.subnetworks, region, name)
	if !ok {
		return nil, notFound(subnetworkPath(region, name))
	}
	return encoded(sn), nil
}

// subnetworkUser is a resource that keeps sn from being deleted, or "". The caller holds s.mu.
func (s *Server) subnetworkUser(sn *Subnetwork) string {
	user := ""
	each(s.instances, func(inst *Instance) {
		for _, nic := range inst.NetworkInterfaces {
			if user == "" && nic.Subnetwork == sn.SelfLink {
				user = strings.TrimPrefix(inst.SelfLink, selfLinkBase)
			}
		}
	})
	each(s.addresses, func(a *Address) {
		if user == "" && a.Subnetwork != "" && selfLinkBase+relPath(a.Subnetwork, projectOf(a.SelfLink[len(selfLinkBase):])) == sn.SelfLink {
			user = strings.TrimPrefix(a.SelfLink, selfLinkBase)
		}
	})
	return user
}

func (s *Server) deleteSubnetwork(r *http.Request, opFault *apiError) (any, *apiError) {
	reqID, err := requestID(r)
	if err != nil {
		return nil, err
	}
	region, name := regionPath(r), r.PathValue("name")
	path := subnetworkPath(region, name)
	s.mu.Lock()
	defer s.mu.Unlock()
	if op, ok := s.requests[reqID]; ok {
		return encoded(op), nil
	}
	sn, ok := lookupIn(s.subnetworks, region, name)
	if !ok {
		return nil, notFound(path)
	}
	if user := s.subnetworkUser(sn); user != "" {
		return nil, inUse("subnetwork", path, user)
	}
	if opFault == nil {
		s.subnetworks[region].remove(name)
	}
	return encoded(s.startOperation(r, reqID, "delete", path, sn.ID, opFault, nil)), nil
}

func (s *Server) listSubnetworks(r *http.Request, _ *apiError) (any, *apiError) {
	q, err := parseListQuery(r, subnetworkFields)
	if err != nil {
		return nil, err
	}
	region := regionPath(r)
	s.mu.Lock()
	defer s.mu.Unlock()
	return listScope(s.subnetworks[region], q, "compute#subnetworkList", region+"/subnetworks", nil), nil
}

func (s *Server) aggregatedSubnetworks(r *http.Request, _ *apiError) (any, *apiError) {
	q, err := parseListQuery(r, subnetworkFields)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return listAggregated(s.subnetworks, r.PathValue("project"), q, "compute#subnetworkAggregatedList", "subnetworks", nil), nil
}

// Subnetworks returns every subnetwork the server holds, ordered by selfLink.
func (s *Server) Subnetworks() []Subnetwork {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []Subnetwork
	each(s.subnetworks, func(sn *Subnetwork) { out = append(out, sn.clone()) })
	return out
}
