package compute

import (
	"cmp"
	"encoding/binary"
	"net/http"
	"net/netip"
)

const (
	purposePeering = "VPC_PEERING"

	minPeeringBits = 8
	maxPeeringBits = 29
)

// peeringPool is where ranges for private services access are allocated
// when the caller names no address.
var peeringPool = netip.MustParsePrefix("10.0.0.0/8")

var globalAddressFields = fieldSet{names: []string{"name", "description", "status", "address", "addressType", "id", "purpose", "network"}, labels: true}

// validateGlobal checks the fields of a global address a caller sets: an
// external address, or an internal range for private services access.
func validateGlobal(a *Address) *apiError {
	if err := validate(a); err != nil {
		return err
	}
	a.AddressType = cmp.Or(a.AddressType, addressExternal)
	switch {
	case a.AddressType == addressExternal && a.Purpose == "":
		if a.Network != "" || a.PrefixLength != 0 {
			return invalid("Invalid value for field 'resource.network': '%s'. An external address takes no network or prefix length.", a.Network)
		}
		return nil
	case a.AddressType == addressInternal && a.Purpose == purposePeering:
	default:
		return invalid("Invalid value for field 'resource.purpose': '%s'. squall: global addresses are external, or internal with purpose VPC_PEERING.", a.Purpose)
	}
	if a.Network == "" {
		return invalid("Invalid value for field 'resource.network': ''. An address with purpose VPC_PEERING needs a network.")
	}
	if a.PrefixLength < minPeeringBits || a.PrefixLength > maxPeeringBits {
		return invalid("Invalid value for field 'resource.prefixLength': '%d'. Must be between %d and %d.", a.PrefixLength, minPeeringBits, maxPeeringBits)
	}
	if a.Address != "" {
		ip := netip.MustParseAddr(a.Address)
		if p := netip.PrefixFrom(ip, a.PrefixLength); !ip.Is4() || p.Masked() != p {
			return invalid("Invalid value for field 'resource.address': '%s'. It must be the first address of a /%d range.", a.Address, a.PrefixLength)
		}
	}
	return nil
}

// peeringRange returns the range a for a VPC_PEERING address on network
// asks for, or the first free one of its size. The caller holds s.mu.
func (s *Server) peeringRange(network string, a *Address) (netip.Prefix, *apiError) {
	if a.Address != "" {
		p := netip.PrefixFrom(netip.MustParseAddr(a.Address), a.PrefixLength)
		return p, s.rangeConflict(network, []netip.Prefix{p})
	}
	size := uint32(1) << (32 - a.PrefixLength)
	start := peeringPool.Addr().As4()
	base := binary.BigEndian.Uint32(start[:])
	for off := uint64(0); off+uint64(size) <= 1<<(32-peeringPool.Bits()); off += uint64(size) {
		var ip [4]byte
		binary.BigEndian.PutUint32(ip[:], base+uint32(off))
		p := netip.PrefixFrom(netip.AddrFrom4(ip), a.PrefixLength)
		if s.rangeConflict(network, []netip.Prefix{p}) == nil {
			return p, nil
		}
	}
	return netip.Prefix{}, invalid("Invalid value for field 'resource.prefixLength': '%d'. No free /%d range is left in %s on the network.", a.PrefixLength, a.PrefixLength, peeringPool)
}

func (s *Server) insertGlobalAddress(r *http.Request, opFault *apiError) (any, *apiError) {
	var want Address
	if err := decodeStrict(r, &want); err != nil {
		return nil, err
	}
	if err := validateGlobal(&want); err != nil {
		return nil, err
	}
	reqID, err := requestID(r)
	if err != nil {
		return nil, err
	}
	project, global := r.PathValue("project"), scopeOf(r)
	path := addressPath(global, want.Name)
	s.mu.Lock()
	defer s.mu.Unlock()
	if op, ok := s.requests[reqID]; ok {
		return encoded(op), nil
	}
	if _, ok := lookupIn(s.globalAddresses, global, want.Name); ok {
		return nil, alreadyExists(path)
	}
	a := want.clone()
	a.ID, a.CreationTimestamp, a.Status, a.Users, a.Region = 0, "", "", nil, ""
	if a.Purpose == purposePeering {
		n, err := s.resolveNetwork(project, a.Network)
		if err != nil {
			return nil, err
		}
		p, err := s.peeringRange(n.SelfLink, &a)
		if err != nil {
			return nil, err
		}
		a.Network, a.Address = n.SelfLink, p.Addr().String()
	} else if a.Address != "" {
		a.Address = netip.MustParseAddr(a.Address).String()
		if _, taken := s.ipHolders[ipKey(global, addressExternal, a.Address)]; taken {
			return nil, invalid("IP address '%s' is already being used by another resource.", a.Address)
		}
	}
	if opFault != nil {
		return encoded(s.startOperation(r, reqID, "insert", path, 0, opFault, nil)), nil
	}
	a.Kind, a.ID, a.CreationTimestamp, a.Status = "compute#address", s.newID(), timestamp(s.now()), "RESERVED"
	a.SelfLink, a.LabelFingerprint = selfLinkBase+path, fingerprint(a.Labels)
	if a.AddressType == addressExternal {
		a.NetworkTier = cmp.Or(a.NetworkTier, "PREMIUM")
		if a.Address == "" {
			a.Address = s.allocate(global, addressExternal)
		}
		s.ipHolders[ipKey(global, addressExternal, a.Address)] = path
	}
	scoped(s.globalAddresses, global).put(a.Name, &a)
	return encoded(s.startOperation(r, reqID, "insert", path, a.ID, nil, nil)), nil
}

// GlobalAddresses returns every global address the server holds, ordered by selfLink.
func (s *Server) GlobalAddresses() []Address {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []Address
	each(s.globalAddresses, func(a *Address) { out = append(out, a.clone()) })
	return out
}

// LookupGlobalAddress returns the named global address in project.
func (s *Server) LookupGlobalAddress(project, name string) (Address, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	a, ok := lookupIn(s.globalAddresses, "projects/"+project+"/global", name)
	if !ok {
		return Address{}, false
	}
	return a.clone(), true
}
