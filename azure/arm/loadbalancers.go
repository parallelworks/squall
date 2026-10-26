package arm

import (
	"cmp"
	"encoding/json"
	"maps"
	"net/http"
	"slices"
	"strconv"
	"strings"
)

var lbKind = &kind{
	typ: "Microsoft.Network/loadBalancers", plural: "loadBalancers", typeName: "LoadBalancer",
	top: []string{"sku", "extendedLocation"},
	props: []string{
		"backendAddressPools", "frontendIPConfigurations", "inboundNatPools", "inboundNatRules", "loadBalancingRules", "mode",
		"outboundRules", "probes", "scope",
	},
	readOnly: []string{"provisioningState", "resourceGuid"},
	located:  true, etag: weakETag, opName: "LoadBalancer",
}

const (
	frontendType     = "Microsoft.Network/loadBalancers/frontendIPConfigurations"
	backendPoolType  = "Microsoft.Network/loadBalancers/backendAddressPools"
	outboundRuleType = "Microsoft.Network/loadBalancers/outboundRules"
	// portsPerIP are the SNAT ports each frontend public IP address gives.
	portsPerIP = 64_000
)

// lbState is what a load balancer holds beyond its body.
type lbState struct {
	frontends []*lbFrontend
	pools     []*lbPool
	rules     []*outboundRule
}

type lbFrontend struct {
	name, pip string
	zones     any
	props     map[string]any
}

type lbPool struct {
	name  string
	props map[string]any
}

type outboundRule struct {
	name      string
	pool      string
	frontends []string
	// ports is allocatedOutboundPorts; 0 asks for the default allocation.
	ports int
	props map[string]any
}

// childName reads the name of the ith element of a load balancer's array
// member, checking its members against known.
func childName(rc map[string]any, member, typeName string, i int, known ...string) (string, map[string]any, *apiError) {
	path := "properties." + member + "[" + strconv.Itoa(i) + "]"
	if err := checkMembers(rc, typeName, path, slices.Concat([]string{"id", "name", "etag", "type", "properties"}, known)...); err != nil {
		return "", nil, err
	}
	props, err := object(rc, "properties")
	if err != nil {
		return "", nil, err
	}
	if props == nil {
		props = map[string]any{}
	}
	name := str(rc, "name")
	if name == "" {
		return "", nil, unsupported("%s[%d] needs a name", member, i)
	}
	return name, props, nil
}

// recorded copies the members of props the server keeps as the caller sent them.
func recorded(props map[string]any, keep ...string) map[string]any {
	out := map[string]any{}
	for _, k := range keep {
		if v, ok := props[k]; ok && v != nil {
			out[k] = v
		}
	}
	return out
}

// refIDs returns the ids of the reference objects in the array m[key].
func refIDs(m map[string]any, key string) ([]string, *apiError) {
	refs, err := objects(m, key)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, r := range refs {
		if id := str(r, "id"); id != "" {
			out = append(out, id)
		}
	}
	return out, nil
}

// childRef resolves the reference id, which must name a child of plural in
// the load balancer lb, to the child's name, or fails as Azure answers a
// reference that does not resolve.
func childRef(lb, plural, id string, names map[string]bool) (string, *apiError) {
	prefix := key(lb) + "/" + strings.ToLower(plural) + "/"
	name, ok := strings.CutPrefix(key(id), prefix)
	if !ok || !names[name] {
		return "", invalidReference(id, lb)
	}
	return id[len(prefix):], nil
}

// prepareLB validates p as the new body of the load balancer res, and returns
// how to apply it. The caller holds s.mu.
func (s *Server) prepareLB(res *resource, p *parsed) (func(), *apiError) {
	if sku, _ := p.top["sku"].(map[string]any); sku != nil {
		if name := str(sku, "name"); name != "" && !strings.EqualFold(name, "Standard") {
			return nil, unsupported("load balancer SKU %s is not supported; use Standard", name)
		}
		if tier := str(sku, "tier"); tier != "" && !strings.EqualFold(tier, "Regional") {
			return nil, unsupported("load balancer tier %s is not supported; use Regional", tier)
		}
	}
	p.top["sku"] = map[string]any{"name": "Standard", "tier": "Regional"}
	for _, member := range []string{"inboundNatPools", "inboundNatRules", "loadBalancingRules", "probes"} {
		if items, err := objects(p.props, member); err != nil {
			return nil, err
		} else if len(items) > 0 {
			return nil, unsupported("load balancer %s are not supported", member)
		}
		delete(p.props, member)
	}
	st := &lbState{}
	frontends, pools := map[string]bool{}, map[string]bool{}
	raw, err := objects(p.props, "frontendIPConfigurations")
	if err != nil {
		return nil, err
	}
	pips := map[string]string{}
	for i, rc := range raw {
		name, props, err := childName(rc, "frontendIPConfigurations", "FrontendIPConfiguration", i, "zones")
		if err != nil {
			return nil, err
		}
		if err := checkMembers(props, "FrontendIPConfigurationPropertiesFormat", "properties.frontendIPConfigurations["+strconv.Itoa(i)+"].properties",
			"ddosSettings", "enableConnectionTracking", "gatewayLoadBalancer", "inboundNatPools", "inboundNatRules", "loadBalancingRules",
			"outboundRules", "privateIPAddress", "privateIPAddressVersion", "privateIPAllocationMethod", "provisioningState",
			"publicIPAddress", "publicIPPrefix", "subnet"); err != nil {
			return nil, err
		}
		for _, m := range []string{"gatewayLoadBalancer", "privateIPAddress", "publicIPPrefix", "subnet"} {
			if props[m] != nil {
				return nil, unsupported("frontend IP configuration %s: %s is not supported; give a publicIPAddress", name, m)
			}
		}
		if v := str(props, "privateIPAddressVersion"); v != "" && !strings.EqualFold(v, "IPv4") {
			return nil, unsupported("privateIPAddressVersion %s is not supported", v)
		}
		if frontends[key(name)] {
			return nil, unsupported("frontend IP configuration %s is listed more than once", name)
		}
		frontends[key(name)] = true
		fe := &lbFrontend{
			name: name, pip: refID(props, "publicIPAddress"), zones: rc["zones"],
			props: recorded(props, "ddosSettings", "enableConnectionTracking", "privateIPAddressVersion", "privateIPAllocationMethod"),
		}
		if fe.pip == "" {
			return nil, unsupported("frontend IP configuration %s needs a publicIPAddress", name)
		}
		if target := s.settled(fe.pip); target == nil || target.kind != pipKind || target.loc != res.loc {
			return nil, invalidReference(fe.pip, res.id)
		}
		user := s.pipUser(fe.pip)
		if user != "" && strings.HasPrefix(key(user), key(res.id)+"/") {
			user = ""
		}
		if user = cmp.Or(user, pips[key(fe.pip)]); user != "" {
			return nil, newError(http.StatusBadRequest, "PublicIPAddressInUse",
				"Resource %s/frontendIPConfigurations/%s is referencing public IP address %s that is already allocated to resource %s.",
				res.id, name, fe.pip, user)
		}
		pips[key(fe.pip)] = res.id + "/frontendIPConfigurations/" + name
		st.frontends = append(st.frontends, fe)
	}
	if raw, err = objects(p.props, "backendAddressPools"); err != nil {
		return nil, err
	}
	for i, rc := range raw {
		name, props, err := childName(rc, "backendAddressPools", "BackendAddressPool", i)
		if err != nil {
			return nil, err
		}
		if err := checkMembers(props, "BackendAddressPoolPropertiesFormat", "properties.backendAddressPools["+strconv.Itoa(i)+"].properties",
			"backendIPConfigurations", "drainPeriodInSeconds", "inboundNatRules", "loadBalancerBackendAddresses", "loadBalancingRules",
			"location", "outboundRule", "outboundRules", "provisioningState", "syncMode", "tunnelInterfaces", "virtualNetwork"); err != nil {
			return nil, err
		}
		if addrs, err := objects(props, "loadBalancerBackendAddresses"); err != nil {
			return nil, err
		} else if len(addrs) > 0 {
			return nil, unsupported("backend pool %s: loadBalancerBackendAddresses are not supported; join it from an IP configuration", name)
		}
		if pools[key(name)] {
			return nil, unsupported("backend address pool %s is listed more than once", name)
		}
		pools[key(name)] = true
		st.pools = append(st.pools, &lbPool{name: name, props: recorded(props, "drainPeriodInSeconds", "location", "syncMode", "tunnelInterfaces", "virtualNetwork")})
	}
	if raw, err = objects(p.props, "outboundRules"); err != nil {
		return nil, err
	}
	rules := map[string]bool{}
	for i, rc := range raw {
		name, props, err := childName(rc, "outboundRules", "OutboundRule", i)
		if err != nil {
			return nil, err
		}
		if err := checkMembers(props, "OutboundRulePropertiesFormat", "properties.outboundRules["+strconv.Itoa(i)+"].properties",
			"allocatedOutboundPorts", "backendAddressPool", "enableTcpReset", "frontendIPConfigurations", "idleTimeoutInMinutes",
			"protocol", "provisioningState"); err != nil {
			return nil, err
		}
		if rules[key(name)] {
			return nil, unsupported("outbound rule %s is listed more than once", name)
		}
		rules[key(name)] = true
		rule, err := parseOutboundRule(res.id, name, props, frontends, pools)
		if err != nil {
			return nil, err
		}
		st.rules = append(st.rules, rule)
	}
	for _, m := range []string{"backendAddressPools", "frontendIPConfigurations", "outboundRules"} {
		delete(p.props, m)
	}
	if res.lb != nil {
		for _, pool := range res.lb.pools {
			if pools[key(pool.name)] {
				continue
			}
			if users := s.poolUsers(res.id + "/backendAddressPools/" + pool.name); len(users) > 0 {
				return nil, poolInUse(res.id+"/backendAddressPools/"+pool.name, users)
			}
		}
	}
	for _, rule := range st.rules {
		if err := checkRulePorts(res.id, rule, s.poolMembers(res.id+"/backendAddressPools/"+rule.pool, "")); err != nil {
			return nil, err
		}
	}
	return func() {
		res.lb = st
		s.update(res, p)
	}, nil
}

// parseOutboundRule reads the outbound rule name of the load balancer lb,
// whose frontends and pools are named in frontends and pools.
func parseOutboundRule(lb, name string, props map[string]any, frontends, pools map[string]bool) (*outboundRule, *apiError) {
	rule := &outboundRule{name: name, props: recorded(props, "enableTcpReset", "idleTimeoutInMinutes", "protocol")}
	pool := refID(props, "backendAddressPool")
	fes, err := objects(props, "frontendIPConfigurations")
	if err != nil {
		return nil, err
	}
	if pool == "" || len(fes) == 0 || str(props, "protocol") == "" {
		return nil, unsupported("outbound rule %s needs backendAddressPool, frontendIPConfigurations and protocol", name)
	}
	if rule.pool, err = childRef(lb, "backendAddressPools", pool, pools); err != nil {
		return nil, err
	}
	for _, fe := range fes {
		n, err := childRef(lb, "frontendIPConfigurations", str(fe, "id"), frontends)
		if err != nil {
			return nil, err
		}
		rule.frontends = append(rule.frontends, n)
	}
	if v, ok := props["allocatedOutboundPorts"]; ok && v != nil {
		n, isNum := v.(json.Number)
		ports, convErr := n.Int64()
		if !isNum || convErr != nil || ports < 0 {
			return nil, badRequest("The value '%v' of allocatedOutboundPorts of outbound rule %s is not valid.", v, name)
		}
		rule.ports = int(ports)
	}
	id := lb + "/outboundRules/" + name
	switch {
	case rule.ports%8 != 0:
		return nil, newError(http.StatusBadRequest, "SpecifiedAllocatedOutboundPortsForOutboundRuleIsNotAMultipleOfEight",
			"Specified Allocated Outbound Ports %d for Outbound Rule %s is not a multiple of 8.", rule.ports, id)
	case rule.ports > portsPerIP:
		return nil, newError(http.StatusBadRequest, "SpecifiedAllocatedOutboundPortsForOutboundRuleExceedsTotalNumberOfAllowedPortsPerRule",
			"Specified Allocated Outbound Ports %d for Outbound Rule %s exceeds total number of allowed ports per rule %d.", rule.ports, id, portsPerIP)
	}
	return rule, nil
}

// checkRulePorts fails when the outbound rule of the load balancer lb cannot
// give each of members backend instances its ports.
func checkRulePorts(lb string, rule *outboundRule, members int) *apiError {
	if rule.ports == 0 || members == 0 || rule.ports*members <= portsPerIP*len(rule.frontends) {
		return nil
	}
	available := portsPerIP * len(rule.frontends) / members / 8 * 8
	return newError(http.StatusBadRequest, "SpecifiedAllocatedOutboundPortsForOutboundRuleExceedsTotalNumberOfAvailablePorts",
		"Specified Allocated Outbound Ports %d for Outbound Rule %s/outboundRules/%s exceeds total number of available ports %d. Reduce allocated ports or increase number of IP addresses for outbound rule.",
		rule.ports, lb, rule.name, available)
}

// splitPool splits the ID of a backend address pool into its load balancer's ID and its name.
func splitPool(id string) (lb, name string, ok bool) {
	i := strings.LastIndex(key(id), "/backendaddresspools/")
	if i < 0 {
		return "", "", false
	}
	return id[:i], id[i+len("/backendaddresspools/"):], true
}

// lbPoolOf returns the load balancer holding the backend address pool id, in
// location, and the pool's name, or nil. The caller holds s.mu.
func (s *Server) lbPoolOf(id, location string) (*resource, string) {
	lb, name, ok := splitPool(id)
	if !ok || strings.Contains(name, "/") {
		return nil, ""
	}
	res := s.settled(lb)
	if res == nil || res.lb == nil || res.loc != location {
		return nil, ""
	}
	for _, pool := range res.lb.pools {
		if strings.EqualFold(pool.name, name) {
			return res, pool.name
		}
	}
	return nil, ""
}

// checkPoolRefs fails when a backend address pool in pools, which by
// references, is not one a load balancer in location holds. The caller holds s.mu.
func (s *Server) checkPoolRefs(pools []string, by, location string) *apiError {
	for _, pool := range pools {
		if res, _ := s.lbPoolOf(pool, location); res == nil {
			return invalidReference(pool, by)
		}
	}
	return nil
}

// checkJoin fails a change that makes owner hold joined[pool] primary IP
// configurations in each backend address pool, keyed by key, when an
// outbound rule of the pool then cannot give each its ports. The caller holds s.mu.
func (s *Server) checkJoin(owner, location string, joined map[string]int) *apiError {
	for _, pool := range slices.Sorted(maps.Keys(joined)) {
		lb, name := s.lbPoolOf(pool, location)
		if lb == nil {
			continue
		}
		members := s.poolMembers(pool, owner) + joined[pool]
		for _, rule := range lb.lb.rules {
			if strings.EqualFold(rule.pool, name) {
				if err := checkRulePorts(lb.id, rule, members); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

func hasPool(pools []string, id string) bool {
	return slices.ContainsFunc(pools, func(p string) bool { return key(p) == key(id) })
}

// scalePools returns, for each backend address pool, keyed by key, how many
// primary IP configurations n VMs with the interfaces nics put in it.
func scalePools(nics []nicConfig, n int) map[string]int {
	out := map[string]int{}
	for _, cfg := range nics {
		for _, spec := range cfg.ips {
			if !spec.primary {
				continue
			}
			for _, pool := range spec.pools {
				out[key(pool)] += n
			}
		}
	}
	return out
}

// nicPools returns, for each backend address pool, keyed by key, how many
// primary IP configurations of configs are in it.
func nicPools(configs []*ipConfig) map[string]int {
	out := map[string]int{}
	for _, ic := range configs {
		if !ic.primary {
			continue
		}
		for _, pool := range ic.pools {
			out[key(pool)]++
		}
	}
	return out
}

// poolMembers counts the primary IP configurations in the backend address
// pool id, those of scale set VMs included, leaving out those of except. The
// caller holds s.mu.
func (s *Server) poolMembers(id, except string) int {
	n := 0
	for _, res := range s.resources {
		if except != "" && key(res.id) == key(except) {
			continue
		}
		switch {
		case res.nic != nil:
			n += nicPools(res.nic.configs)[key(id)]
		case res.scale != nil:
			n += scalePools(res.scale.nics, len(res.scale.instances))[key(id)]
		}
	}
	return n
}

// poolMemberIDs returns the IDs of the IP configurations in the backend
// address pool id, ordered by what holds them and a scale set's by instance
// ID. The caller holds s.mu.
func (s *Server) poolMemberIDs(id string) []string {
	var out []string
	holders := slices.SortedFunc(maps.Values(s.resources), func(a, b *resource) int { return cmp.Compare(key(a.id), key(b.id)) })
	for _, res := range holders {
		switch {
		case res.nic != nil:
			for _, ic := range res.nic.configs {
				if hasPool(ic.pools, id) {
					out = append(out, res.id+"/ipConfigurations/"+ic.name)
				}
			}
		case res.scale != nil:
			for _, inst := range res.scale.sorted() {
				for _, cfg := range res.scale.nics {
					for _, spec := range cfg.ips {
						if hasPool(spec.pools, id) {
							out = append(out, inst.id(res)+"/networkInterfaces/"+cfg.name+"/ipConfigurations/"+spec.name)
						}
					}
				}
			}
		}
	}
	return out
}

// poolUsers returns the scale sets and network interfaces whose IP
// configurations reference the backend address pool id, ordered. The caller holds s.mu.
func (s *Server) poolUsers(id string) []string {
	var out []string
	for _, res := range s.resources {
		switch {
		case res.nic != nil:
			if slices.ContainsFunc(res.nic.configs, func(ic *ipConfig) bool { return hasPool(ic.pools, id) }) {
				out = append(out, res.id)
			}
		case res.scale != nil:
			if len(scaleRefs(res.scale.nics, id)) > 0 {
				out = append(out, res.id)
			}
		}
	}
	slices.Sort(out)
	return out
}

// scaleRefs returns the IP configurations of nics that reference the backend address pool id.
func scaleRefs(nics []nicConfig, id string) []ipConfigSpec {
	var out []ipConfigSpec
	for _, cfg := range nics {
		for _, spec := range cfg.ips {
			if hasPool(spec.pools, id) {
				out = append(out, spec)
			}
		}
	}
	return out
}

func poolInUse(pool string, users []string) *apiError {
	return newError(http.StatusBadRequest, "InUseBackendAddressPoolCannotBeDeleted",
		"Backend address pool %s cannot be deleted because it is in use by the following resources: %s.", pool, strings.Join(users, ", "))
}

// lbInUse fails the deletion of the load balancer res while a scale set or
// interface uses one of its pools. The caller holds s.mu.
func (s *Server) lbInUse(res *resource) *apiError {
	if res.lb == nil {
		return nil
	}
	used := map[string][]string{}
	var users []string
	for _, pool := range res.lb.pools {
		for _, u := range s.poolUsers(res.id + "/backendAddressPools/" + pool.name) {
			if used[u] == nil {
				users = append(users, u)
			}
			used[u] = append(used[u], pool.name)
		}
	}
	slices.Sort(users)
	for _, u := range users {
		if r := s.resources[key(u)]; r != nil && r.scale != nil {
			return newError(http.StatusBadRequest, "LoadBalancerInUseByVirtualMachineScaleSet",
				"Cannot delete load balancer %s since its child resources %s are in use by virtual machine scale set %s.",
				res.id, strings.Join(used[u], ", "), u)
		}
	}
	for _, pool := range res.lb.pools {
		id := res.id + "/backendAddressPools/" + pool.name
		if users := s.poolUsers(id); len(users) > 0 {
			return poolInUse(id, users)
		}
	}
	return nil
}

// pipLB returns the load balancer frontend IP configuration that uses the
// public IP address id, or "". The caller holds s.mu.
func (s *Server) pipLB(id string) string {
	for _, res := range s.resources {
		if res.lb == nil {
			continue
		}
		for _, fe := range res.lb.frontends {
			if key(fe.pip) == key(id) {
				return res.id + "/frontendIPConfigurations/" + fe.name
			}
		}
	}
	return ""
}

// defaultPorts is the SNAT ports each of members backend instances gets from
// frontends frontend IP addresses with default port allocation, as Microsoft
// documents it.
func defaultPorts(members, frontends int) int {
	per := 32
	for _, step := range []struct{ upTo, ports int }{{50, 1024}, {100, 512}, {200, 256}, {400, 128}, {800, 64}} {
		if members <= step.upTo {
			per = step.ports
			break
		}
	}
	return min(per*frontends, 1024)
}

// renderLB adds what Azure reports for the load balancer res: its frontend IP
// configurations, backend address pools and outbound rules, with the
// references between them, and defaults.
func (s *Server) renderLB(res *resource, props map[string]any) {
	for _, m := range []string{"inboundNatPools", "inboundNatRules", "loadBalancingRules", "probes"} {
		props[m] = []any{}
	}
	st := res.lb
	if st == nil {
		st = &lbState{}
	}
	child := func(plural, name, typ string, p map[string]any) map[string]any {
		p["provisioningState"] = res.state
		return map[string]any{"id": res.id + "/" + plural + "/" + name, "name": name, "etag": res.etag, "type": typ, "properties": p}
	}
	rulesOf := func(match func(*outboundRule) bool) []any {
		var out []any
		for _, rule := range st.rules {
			if match(rule) {
				out = append(out, ref(res.id+"/outboundRules/"+rule.name))
			}
		}
		return out
	}
	frontends := make([]any, 0, len(st.frontends))
	for _, fe := range st.frontends {
		p := maps.Clone(fe.props)
		p["publicIPAddress"] = ref(fe.pip)
		if _, ok := p["privateIPAllocationMethod"]; !ok {
			p["privateIPAllocationMethod"] = "Dynamic"
		}
		if rules := rulesOf(func(r *outboundRule) bool {
			return slices.ContainsFunc(r.frontends, func(n string) bool { return strings.EqualFold(n, fe.name) })
		}); rules != nil {
			p["outboundRules"] = rules
		}
		m := child("frontendIPConfigurations", fe.name, frontendType, p)
		if fe.zones != nil {
			m["zones"] = fe.zones
		}
		frontends = append(frontends, m)
	}
	props["frontendIPConfigurations"] = frontends
	pools := make([]any, 0, len(st.pools))
	for _, pool := range st.pools {
		p := maps.Clone(pool.props)
		p["loadBalancerBackendAddresses"] = []any{}
		if members := s.poolMemberIDs(res.id + "/backendAddressPools/" + pool.name); len(members) > 0 {
			refs := make([]any, 0, len(members))
			for _, m := range members {
				refs = append(refs, ref(m))
			}
			p["backendIPConfigurations"] = refs
		}
		if rules := rulesOf(func(r *outboundRule) bool { return strings.EqualFold(r.pool, pool.name) }); rules != nil {
			p["outboundRules"] = rules
		}
		pools = append(pools, child("backendAddressPools", pool.name, backendPoolType, p))
	}
	props["backendAddressPools"] = pools
	rules := make([]any, 0, len(st.rules))
	for _, rule := range st.rules {
		p := maps.Clone(rule.props)
		p["backendAddressPool"] = ref(res.id + "/backendAddressPools/" + rule.pool)
		fes := make([]any, 0, len(rule.frontends))
		for _, fe := range rule.frontends {
			fes = append(fes, ref(res.id+"/frontendIPConfigurations/"+fe))
		}
		p["frontendIPConfigurations"] = fes
		p["allocatedOutboundPorts"] = rule.ports
		if rule.ports == 0 {
			p["allocatedOutboundPorts"] = defaultPorts(s.poolMembers(res.id+"/backendAddressPools/"+rule.pool, ""), len(rule.frontends))
		}
		if _, ok := p["idleTimeoutInMinutes"]; !ok {
			p["idleTimeoutInMinutes"] = 4
		}
		if _, ok := p["enableTcpReset"]; !ok {
			p["enableTcpReset"] = false
		}
		rules = append(rules, child("outboundRules", rule.name, outboundRuleType, p))
	}
	props["outboundRules"] = rules
}

// LoadBalancer is a snapshot of a load balancer.
type LoadBalancer struct {
	ID string
	// PublicIPAddresses are the IDs of its frontends' public IP addresses, in order.
	PublicIPAddresses []string
	BackendPools      []BackendPool
	OutboundRules     []OutboundRule
	Tags              map[string]string
}

// BackendPool is a snapshot of a load balancer's backend address pool.
type BackendPool struct {
	ID string
	// Members are the IDs of the IP configurations in it, those of scale set
	// VMs included, ordered by what holds them and a scale set's by instance ID.
	Members []string
}

// OutboundRule is a snapshot of a load balancer's outbound rule.
type OutboundRule struct {
	ID          string
	BackendPool string
	// PublicIPAddresses are the IDs of the public IP addresses of its frontends, in order.
	PublicIPAddresses []string
	// AllocatedOutboundPorts are the SNAT ports each backend instance gets, or
	// 0 for the default allocation.
	AllocatedOutboundPorts int
}

// LoadBalancers returns the load balancers the server holds, ordered by ID.
func (s *Server) LoadBalancers() []LoadBalancer {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []LoadBalancer
	for _, res := range s.resources {
		if res.lb == nil {
			continue
		}
		lb := LoadBalancer{ID: res.id, Tags: maps.Clone(res.tags)}
		pips := map[string]string{}
		for _, fe := range res.lb.frontends {
			lb.PublicIPAddresses = append(lb.PublicIPAddresses, fe.pip)
			pips[key(fe.name)] = fe.pip
		}
		for _, pool := range res.lb.pools {
			id := res.id + "/backendAddressPools/" + pool.name
			lb.BackendPools = append(lb.BackendPools, BackendPool{ID: id, Members: s.poolMemberIDs(id)})
		}
		for _, rule := range res.lb.rules {
			r := OutboundRule{ID: res.id + "/outboundRules/" + rule.name, BackendPool: res.id + "/backendAddressPools/" + rule.pool, AllocatedOutboundPorts: rule.ports}
			for _, fe := range rule.frontends {
				r.PublicIPAddresses = append(r.PublicIPAddresses, pips[key(fe)])
			}
			lb.OutboundRules = append(lb.OutboundRules, r)
		}
		out = append(out, lb)
	}
	slices.SortFunc(out, func(a, b LoadBalancer) int { return cmp.Compare(a.ID, b.ID) })
	return out
}
