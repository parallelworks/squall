package arm

import (
	"encoding/binary"
	"fmt"
	"net/http"
	"net/netip"
	"slices"
	"strings"
)

// pool is an IPv4 range addresses are taken from in turn, leaving out the
// addresses reserved at its start and end.
type pool struct {
	prefix      netip.Prefix
	first, last netip.Addr
	size        int
	next        netip.Addr
}

func newPool(p netip.Prefix, head, tail uint32) *pool {
	p = p.Masked()
	if !p.IsValid() || !p.Addr().Is4() {
		panic(fmt.Sprintf("squall: %s is not an IPv4 prefix", p))
	}
	hosts := ^uint32(0) >> p.Bits()
	if hosts < head+tail {
		panic(fmt.Sprintf("squall: %s leaves no address to allocate", p))
	}
	base := binary.BigEndian.Uint32(p.Addr().AsSlice())
	last := addr4(base + hosts - tail)
	return &pool{prefix: p, first: addr4(base + head), last: last, size: int(hosts-head-tail) + 1, next: last}
}

func addr4(v uint32) netip.Addr {
	return netip.AddrFrom4([4]byte(binary.BigEndian.AppendUint32(nil, v)))
}

func (p *pool) contains(a netip.Addr) bool {
	return a.Is4() && a.Compare(p.first) >= 0 && a.Compare(p.last) <= 0
}

// take returns the next address after the last one taken that used does not
// hold, wrapping around, or false when every address is used.
func (p *pool) take(used func(netip.Addr) bool) (netip.Addr, bool) {
	for range p.size {
		if p.next == p.last {
			p.next = p.first
		} else {
			p.next = p.next.Next()
		}
		if !used(p.next) {
			return p.next, true
		}
	}
	return netip.Addr{}, false
}

// WithPrivateRange sets the range private addresses come from, in every
// subnet, as a subnet with that prefix would give them: without its first
// four addresses and its last. The default is 10.0.0.0/8. It panics on a
// prefix that is not IPv4 or leaves no address.
func WithPrivateRange(p netip.Prefix) Option {
	return func(s *Server) { s.private = newPool(p, 4, 1) }
}

// WithPublicRange sets the range public addresses come from, without its
// first address. The default is 100.64.0.0/10. It panics on a prefix that is
// not IPv4 or leaves no address.
func WithPublicRange(p netip.Prefix) Option {
	return func(s *Server) { s.public = newPool(p, 1, 0) }
}

// checkFree fails a change that needs a private address in each of subnets,
// which may repeat, and public more public addresses, when the ranges cannot
// hold them. Addresses held by except and what it holds do not count as used.
// The caller holds s.mu.
func (s *Server) checkFree(subnets []string, public int, except string) *apiError {
	need := map[string]int{}
	for _, sn := range subnets {
		need[key(sn)]++
	}
	slices.SortFunc(subnets, func(a, b string) int { return strings.Compare(key(a), key(b)) })
	for _, sn := range subnets {
		if need[key(sn)] > s.privateFree(sn, except) {
			return newError(http.StatusBadRequest, "SubnetIsFull",
				"Subnet %s with address prefix %s does not have enough capacity for %d IP addresses.",
				sn[strings.LastIndex(sn, "/")+1:], s.private.prefix, need[key(sn)])
		}
	}
	if public > 0 && public > s.publicFree() {
		return newError(http.StatusBadRequest, "PublicIPCountLimitReached",
			"Cannot create more than %d public IP addresses for this subscription in this region.", s.public.size)
	}
	return nil
}

// privateFree counts the free addresses of the private range in subnet,
// counting those held by except and what it holds as free. The caller holds s.mu.
func (s *Server) privateFree(subnet, except string) int {
	free := s.private.size
	prefix := key(except) + "/"
	for a, holder := range s.privateIPs[key(subnet)] {
		if s.private.contains(a) && (except == "" || !strings.HasPrefix(key(holder), prefix)) {
			free--
		}
	}
	return free
}

// publicFree counts the free addresses of the public range. The caller holds s.mu.
func (s *Server) publicFree() int {
	free := s.public.size
	for a := range s.publicIPs {
		if s.public.contains(a) {
			free--
		}
	}
	return free
}

// allocPrivate takes the next free private address in subnet for holder,
// which [Server.checkFree] has made sure there is. The caller holds s.mu.
func (s *Server) allocPrivate(subnet, holder string) netip.Addr {
	used := s.privateIPs[key(subnet)]
	a, ok := s.private.take(func(a netip.Addr) bool { return used[a] != "" })
	if !ok {
		panic("squall: allocating a private address in a full subnet")
	}
	s.takePrivate(subnet, a, holder)
	return a
}

// allocPublic takes the next free public address for holder, which
// [Server.checkFree] has made sure there is. The caller holds s.mu.
func (s *Server) allocPublic(holder string) netip.Addr {
	a, ok := s.public.take(func(a netip.Addr) bool { return s.publicIPs[a] != "" })
	if !ok {
		panic("squall: allocating a public address from a full range")
	}
	s.publicIPs[a] = holder
	return a
}

// heldAddress is an address a resource takes in a subnet of its own, rather
// than through a network interface.
type heldAddress struct {
	subnet  string
	address netip.Addr
}

// holdAddress returns how to apply p to res, which takes an address in subnet,
// named by the property member. A new one takes the next free address; an
// existing one keeps its own and cannot move. The caller holds s.mu.
func (s *Server) holdAddress(res *resource, p *parsed, subnet, member string) (func(), *apiError) {
	if res.held != nil {
		if key(subnet) != key(res.held.subnet) {
			return nil, changeNotAllowed(member)
		}
		return func() { s.update(res, p) }, nil
	}
	if err := s.checkFree([]string{subnet}, 0, ""); err != nil {
		return nil, err
	}
	return func() {
		res.held = &heldAddress{subnet: subnet, address: s.allocPrivate(subnet, res.id)}
		s.update(res, p)
	}, nil
}
