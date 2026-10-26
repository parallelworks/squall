package arm

import (
	"cmp"
	"fmt"
	"hash/fnv"
	"maps"
	"net/http"
	"net/netip"
	"slices"
	"strconv"
	"strings"
)

var (
	nsgKind = &kind{
		typ: "Microsoft.Network/networkSecurityGroups", plural: "networkSecurityGroups", typeName: "NetworkSecurityGroup",
		props:    []string{"securityRules", "flushConnection"},
		readOnly: []string{"defaultSecurityRules", "flowLogs", "networkInterfaces", "provisioningState", "resourceGuid", "subnets"},
		located:  true, etag: weakETag, opName: "NetworkSecurityGroup",
	}
	nicKind = &kind{
		typ: "Microsoft.Network/networkInterfaces", plural: "networkInterfaces", typeName: "NetworkInterface",
		top: []string{"extendedLocation"},
		props: []string{
			"auxiliaryMode", "auxiliarySku", "disableTcpStateTracking", "dnsSettings", "enableAcceleratedNetworking",
			"enableIPForwarding", "ipConfigurations", "migrationPhase", "networkSecurityGroup", "nicType", "workloadType",
		},
		readOnly: []string{
			"defaultOutboundConnectivityEnabled", "dscpConfiguration", "hostedWorkloads", "macAddress", "primary",
			"privateEndpoint", "privateLinkService", "provisioningState", "resourceGuid", "tapConfigurations",
			"virtualMachine", "vnetEncryptionSupported",
		},
		located: true, etag: weakETag, opName: "Nic",
	}
	pipKind = &kind{
		typ: "Microsoft.Network/publicIPAddresses", plural: "publicIPAddresses", typeName: "PublicIPAddress",
		top: []string{"sku", "zones", "extendedLocation"},
		props: []string{
			"ddosSettings", "deleteOption", "dnsSettings", "ipTags", "idleTimeoutInMinutes", "publicIPAddressVersion",
			"publicIPAllocationMethod",
		},
		readOnly: []string{
			"ipAddress", "ipConfiguration", "linkedPublicIPAddress", "migrationPhase", "natGateway", "provisioningState",
			"publicIPPrefix", "resourceGuid", "servicePublicIPAddress",
		},
		located: true, etag: weakETag, opName: "PublicIpAddress",
	}
)

const ipConfigType = "Microsoft.Network/networkInterfaces/ipConfigurations"

type nicState struct {
	configs []*ipConfig
	nsg     string
	// vm is the ID of the VM the interface is attached to, if any.
	vm string
}

type ipConfig struct {
	name    string
	subnet  string
	address netip.Addr
	static  bool
	primary bool
	pip     string
	// pools are the load balancer backend address pools it is in.
	pools []string
	props map[string]any
}

type pipState struct {
	address netip.Addr
}

// putSpec describes the create or update of a resource with a
// long-running operation.
type putSpec struct {
	kind *kind
	id   func(c *call) string
	// parent, when set, is the ID of the resource that must hold the new one.
	parent func(c *call) string
	paths  func(c *call, location string) opPaths
	// precondition checks the If-Match and If-None-Match headers against the
	// resource, which is nil when it does not exist. Nil is checkPrecondition.
	precondition func(r *http.Request, existing *resource) *apiError
	// prepare validates p as the new body of res, which is not yet stored when
	// new, and returns how to apply it. Nil records p as it is.
	prepare func(res *resource, p *parsed) (func(), *apiError)
	render  func(res *resource) map[string]any
	// acceptedBody, when set, answers a change in progress with 202, this
	// body and a Location URL as well, as private DNS and Storage do.
	acceptedBody func() []byte
	// createdStatus answers a create that finishes at once, if not 201.
	createdStatus int
	// bodyPolled answers a change in progress with no polling header, so the
	// SDK polls the resource itself until its provisioningState ends, as
	// Cognitive Services deployments and Search services are; paths is unused.
	bodyPolled bool
	// sync finishes the change before answering, whatever [WithSettleAfter]
	// says, as Application Insights components do; paths is unused.
	sync bool
}

// polled adds to rep, which answers with the body-polled resource res, the
// retry-after-ms polling URLs carry while res changes.
func polled(res *resource, rep *reply) *reply {
	if res.state != stateSucceeded {
		rep.header = http.Header{"Retry-After-Ms": {retryAfterMS}}
	}
	return rep
}

// fail applies err to the call: an async error becomes the error the call's
// operation ends in, unless a fault already chose one, and any other fails
// the call, which fail returns.
func (c *call) fail(err *apiError) *apiError {
	if !err.async {
		return err
	}
	if c.opFault == nil {
		c.opFault = err
	}
	return nil
}

// put creates or updates a resource as spec describes.
func (s *Server) put(spec putSpec) handler {
	return func(c *call) (*reply, *apiError) {
		p, err := parseBody(c, spec.kind)
		if err != nil {
			return nil, err
		}
		s.mu.Lock()
		defer s.mu.Unlock()
		if err := s.writableGroup(c); err != nil {
			return nil, err
		}
		if spec.parent != nil {
			if parent := spec.parent(c); s.settled(parent) == nil {
				return nil, parentNotFound(spec.kind, parent)
			}
		}
		id := spec.id(c)
		existing, err := s.claim(id, "")
		if err != nil {
			return nil, err
		}
		precondition := spec.precondition
		if precondition == nil {
			precondition = checkPrecondition
		}
		if err := precondition(c.r, existing); err != nil {
			if err = c.fail(err); err != nil {
				return nil, err
			}
		}
		if existing != nil && existing.loc != p.location {
			return nil, wrongLocation(existing.name, existing.loc, p.location, c.val("rg"))
		}
		res, status, state := existing, http.StatusOK, stateUpdating
		if res == nil {
			res, status, state = s.newResource(c, spec.kind, id, p), cmp.Or(spec.createdStatus, http.StatusCreated), stateCreating
		}
		commit := func() { s.update(res, p) }
		if spec.prepare != nil {
			var perr *apiError
			if commit, perr = spec.prepare(res, p); perr != nil {
				if perr = c.fail(perr); perr != nil {
					return nil, perr
				}
			}
		}
		render := func() []byte { return encode(spec.render(res)) }
		answer := func(op *operation, render func() []byte) *reply {
			if spec.bodyPolled {
				return polled(res, &reply{status: status, body: render()})
			}
			if spec.acceptedBody != nil && op.status != opSucceeded {
				return accepted(c, op, status, http.StatusAccepted, true, spec.acceptedBody)
			}
			return accepted(c, op, status, status, false, render)
		}
		var paths opPaths
		if spec.paths != nil {
			paths = spec.paths(c, p.location)
		}
		if c.opFault != nil {
			op := s.begin(c, paths, 0, nil)
			op.target, op.state = res.id, state
			return answer(op, func() []byte { return withState(res, state, render) }), nil
		}
		commit()
		s.resources[key(id)] = res
		if spec.sync {
			return &reply{status: status, body: render()}, nil
		}
		op := s.change(c, res, paths, 0, state, nil)
		op.result = render
		return answer(op, render), nil
	}
}

// putNetwork creates or updates a network resource of kind k.
func (s *Server) putNetwork(k *kind) handler {
	spec := putSpec{
		kind:   k,
		id:     func(c *call) string { return networkID(c, k) },
		paths:  networkOps,
		render: s.renderNetwork,
	}
	switch k {
	case vnetKind:
		spec.prepare = s.prepareVNet
	case subnetKind:
		spec.parent = vnetID
		spec.paths = func(c *call, _ string) opPaths { return networkOps(c, s.resources[key(vnetID(c))].loc) }
		spec.prepare = s.prepareSubnet
	case natKind:
		spec.prepare = s.prepareNAT
	case lbKind:
		spec.prepare = s.prepareLB
	case nicKind:
		spec.prepare = s.prepareNIC
	case pipKind:
		spec.prepare = s.preparePIP
	case peKind:
		spec.prepare = s.preparePE
	}
	return s.put(spec)
}

// prepareNIC validates p as the new body of the interface res, and returns
// how to apply it. The caller holds s.mu.
func (s *Server) prepareNIC(res *resource, p *parsed) (func(), *apiError) {
	raw, err := objects(p.props, "ipConfigurations")
	if err != nil {
		return nil, err
	}
	delete(p.props, "ipConfigurations")
	if len(raw) == 0 {
		return nil, newError(http.StatusBadRequest, "InvalidIPConfigCount", "Network interface %s must have one or more IP configurations.", res.name)
	}
	nsg := refID(p.props, "networkSecurityGroup")
	delete(p.props, "networkSecurityGroup")
	if nsg != "" {
		if target := s.settled(nsg); target == nil || target.kind != nsgKind || target.loc != res.loc {
			return nil, invalidReference(nsg, res.id)
		}
	}
	var old map[string]*ipConfig
	if res.nic != nil {
		old = map[string]*ipConfig{}
		for _, ic := range res.nic.configs {
			old[key(ic.name)] = ic
		}
	}
	configs := make([]*ipConfig, 0, len(raw))
	for i, rc := range raw {
		ic, err := s.parseIPConfig(res, rc, i, len(raw))
		if err != nil {
			return nil, err
		}
		configs = append(configs, ic)
	}
	// Reserve every address before taking any, so a conflict leaves nothing taken.
	taken := map[string]bool{}
	for _, ic := range configs {
		if !ic.static {
			continue
		}
		holder := s.privateIPs[key(ic.subnet)][ic.address]
		k := key(ic.subnet) + " " + ic.address.String()
		if holder != "" && !strings.HasPrefix(key(holder), key(res.id)+"/") || taken[k] {
			return nil, newError(http.StatusBadRequest, "PrivateIPAddressIsAllocated",
				"IP configuration %s/ipConfigurations/%s is using the private IP address %s which is already allocated to resource %s.",
				res.id, ic.name, ic.address, holder)
		}
		taken[k] = true
	}
	var subnets []string
	for _, ic := range configs {
		if !ic.static || s.subnetPool(ic.subnet).contains(ic.address) {
			subnets = append(subnets, ic.subnet)
		}
	}
	if err := s.checkFree(subnets, 0, res.id); err != nil {
		return nil, err
	}
	if err := s.checkJoin(res.id, res.loc, nicPools(configs)); err != nil {
		return nil, err
	}
	return func() {
		if res.nic != nil {
			s.releaseNIC(res)
		}
		holder := func(ic *ipConfig) string { return res.id + "/ipConfigurations/" + ic.name }
		// Static addresses go first, so a dynamic one cannot take an address a later configuration claims.
		for _, ic := range configs {
			if ic.static {
				s.takePrivate(ic.subnet, ic.address, holder(ic))
			}
		}
		for _, ic := range configs {
			prev := old[key(ic.name)]
			switch {
			case ic.static:
			case prev != nil && key(prev.subnet) == key(ic.subnet) && s.privateIPs[key(ic.subnet)][prev.address] == "":
				ic.address = prev.address
				s.takePrivate(ic.subnet, ic.address, holder(ic))
			default:
				ic.address = s.allocPrivate(ic.subnet, holder(ic))
			}
		}
		vm := ""
		if res.nic != nil {
			vm = res.nic.vm
		}
		res.nic = &nicState{configs: configs, nsg: nsg, vm: vm}
		s.update(res, p)
	}, nil
}

// parseIPConfig reads the ith of n IP configurations of the interface res.
// The caller holds s.mu.
func (s *Server) parseIPConfig(res *resource, rc map[string]any, i, n int) (*ipConfig, *apiError) {
	path := "properties.ipConfigurations[" + strconv.Itoa(i) + "]"
	if err := checkMembers(rc, "NetworkInterfaceIPConfiguration", path, "id", "name", "etag", "type", "properties"); err != nil {
		return nil, err
	}
	props, err := object(rc, "properties")
	if err != nil {
		return nil, err
	}
	if err := checkMembers(props, "NetworkInterfaceIPConfigurationPropertiesFormat", path+".properties",
		"applicationGatewayBackendAddressPools", "applicationSecurityGroups", "gatewayLoadBalancer",
		"loadBalancerBackendAddressPools", "loadBalancerInboundNatRules", "primary", "privateIPAddress",
		"privateIPAddressPrefixLength", "privateIPAddressVersion", "privateIPAllocationMethod",
		"privateLinkConnectionProperties", "provisioningState", "publicIPAddress", "subnet", "virtualNetworkTaps"); err != nil {
		return nil, err
	}
	ic := &ipConfig{name: str(rc, "name"), subnet: refID(props, "subnet"), pip: refID(props, "publicIPAddress")}
	if ic.name == "" {
		return nil, badRequest("IP configuration %d of network interface %s has no name.", i, res.id)
	}
	if ic.subnet == "" {
		return nil, badRequest("IP configuration %s of network interface %s must reference a subnet.", ic.name, res.id)
	}
	if v := str(props, "privateIPAddressVersion"); v != "" && !strings.EqualFold(v, "IPv4") {
		return nil, unsupported("privateIPAddressVersion %s is not supported", v)
	}
	ic.primary, _ = boolean(props, "primary")
	if n == 1 {
		ic.primary = true
	}
	switch method := str(props, "privateIPAllocationMethod"); {
	case strings.EqualFold(method, "Static"):
		a, perr := netip.ParseAddr(str(props, "privateIPAddress"))
		if perr != nil || !a.Is4() {
			return nil, badRequest("IP configuration %s of network interface %s has a static allocation with no valid privateIPAddress.", ic.name, res.id)
		}
		ic.address, ic.static = a, true
	case method != "" && !strings.EqualFold(method, "Dynamic"):
		return nil, badRequest("The privateIPAllocationMethod '%s' is not valid.", method)
	}
	if ic.pip != "" {
		target := s.settled(ic.pip)
		if target == nil || target.kind != pipKind || target.loc != res.loc {
			return nil, invalidReference(ic.pip, res.id)
		}
		if user := s.pipUser(ic.pip); user != "" && !strings.HasPrefix(key(user), key(res.id)+"/") {
			return nil, newError(http.StatusBadRequest, "PublicIPAddressInUse",
				"Resource %s/ipConfigurations/%s is referencing public IP address %s that is already allocated to resource %s.",
				res.id, ic.name, ic.pip, user)
		}
	}
	if ic.pools, err = refIDs(props, "loadBalancerBackendAddressPools"); err != nil {
		return nil, err
	}
	if err := s.checkPoolRefs(ic.pools, res.id, res.loc); err != nil {
		return nil, err
	}
	ic.props = map[string]any{}
	for k, v := range props {
		switch k {
		case "primary", "privateIPAddress", "privateIPAddressVersion", "privateIPAllocationMethod",
			"privateLinkConnectionProperties", "provisioningState", "publicIPAddress", "subnet":
		default:
			ic.props[k] = v
		}
	}
	return ic, nil
}

// preparePIP validates p as the new body of the public IP address res, and
// returns how to apply it. The caller holds s.mu.
func (s *Server) preparePIP(res *resource, p *parsed) (func(), *apiError) {
	if v := str(p.props, "publicIPAddressVersion"); v != "" && !strings.EqualFold(v, "IPv4") {
		return nil, unsupported("publicIPAddressVersion %s is not supported", v)
	}
	if sku, _ := p.top["sku"].(map[string]any); sku != nil {
		if name := str(sku, "name"); name != "" && !strings.EqualFold(name, "Standard") {
			return nil, unsupported("public IP address SKU %s is not supported; use Standard", name)
		}
	} else {
		p.top["sku"] = map[string]any{"name": "Standard", "tier": "Regional"}
	}
	if res.pip == nil {
		if err := s.checkFree(nil, 1, ""); err != nil {
			return nil, err
		}
	}
	return func() {
		if res.pip == nil {
			res.pip = &pipState{address: s.allocPublic(key(res.id))}
		}
		s.update(res, p)
	}, nil
}

func (s *Server) getResource(k *kind) handler {
	return func(c *call) (*reply, *apiError) {
		s.mu.Lock()
		defer s.mu.Unlock()
		if err := s.readableGroup(c); err != nil {
			return nil, err
		}
		typ, name := k.typ, c.val("name")
		if k == subnetKind {
			if vnet := s.read(vnetID(c)); vnet == nil || vnet.kind != vnetKind {
				return nil, resourceNotFound(vnetKind.typ, c.val("vnet"), c.val("rg"))
			}
			typ, name = vnetKind.typ, c.val("vnet")+"/subnets/"+name
		}
		res := s.read(networkID(c, k))
		if res == nil || res.kind != k {
			return nil, resourceNotFound(typ, name, c.val("rg"))
		}
		return ok200(s.renderNetwork(res)), nil
	}
}

func (s *Server) deleteNetwork(k *kind) handler {
	return func(c *call) (*reply, *apiError) {
		s.mu.Lock()
		defer s.mu.Unlock()
		if err := s.writableGroup(c); err != nil {
			return nil, err
		}
		res, err := s.claim(networkID(c, k), "")
		if err != nil {
			return nil, err
		}
		if res == nil || res.kind != k {
			return &reply{status: http.StatusNoContent}, nil
		}
		if err := s.checkUnused(res); err != nil {
			return nil, err
		}
		location := res.loc
		if k == subnetKind {
			location = s.resources[key(parentVNet(res.id))].loc
		}
		op := s.change(c, res, networkOps(c, location), http.StatusNoContent, stateDeleting, func() { s.remove(res) })
		return accepted(c, op, http.StatusOK, http.StatusAccepted, true, noBody), nil
	}
}

// checkUnused fails the deletion of a network resource another uses. The caller holds s.mu.
func (s *Server) checkUnused(res *resource) *apiError {
	switch res.kind {
	case nsgKind:
		if users := slices.Concat(s.nsgUsers(res.id), s.subnetsUsing(res.id, subnetNSG)); len(users) > 0 {
			return newError(http.StatusBadRequest, "InUseNetworkSecurityGroupCannotBeDeleted",
				"Network security group %s cannot be deleted because it is in use by the following resources: %s. In order to delete the Network security group, remove the association with the resource(s). To learn how to do this, see aka.ms/deletensg.",
				res.id, strings.Join(users, ", "))
		}
	case pipKind:
		if user := s.pipUser(res.id); user != "" {
			return newError(http.StatusBadRequest, "PublicIPAddressCannotBeDeleted",
				"Public IP address %s can not be deleted since it is still allocated to resource %s. In order to delete the public IP, disassociate/detach the Public IP address from the resource.  To learn how to do this, see aka.ms/deletepublicip.",
				res.id, user)
		}
	case nicKind:
		if res.nic.vm != "" {
			return nicInUse(res.id, res.nic.vm)
		}
	case subnetKind:
		if user := s.subnetUser(res.id); user != "" {
			return subnetInUse(res.name, user)
		}
	case vnetKind:
		for _, sn := range s.subnetsOf(res) {
			if user := s.subnetUser(sn.id); user != "" {
				return subnetInUse(sn.name, user)
			}
		}
	case natKind:
		if users := s.subnetsUsing(res.id, subnetNAT); len(users) > 0 {
			return newError(http.StatusBadRequest, "InUseNatGatewayCannotBeDeleted",
				"Nat Gateway %s cannot be deleted because it is in use by the following resources: %s. In order to delete the Nat Gateway, remove the association with the resource(s).",
				res.id, strings.Join(users, ", "))
		}
	case lbKind:
		return s.lbInUse(res)
	case netappAccountKind, poolKind:
		return s.netappInUse(res)
	}
	return nil
}

// nicInUse fails a call on the network interface nic, as Azure names it in
// the call's answer, because the VM vm uses it.
func nicInUse(nic, vm string) *apiError {
	return newError(http.StatusBadRequest, "NicInUse",
		"Network Interface %s is used by existing resource %s. In order to delete the network interface, it must be dissociated from the resource. To learn more, see aka.ms/deletenic.",
		nic, vm)
}

// nsgUsers lists the interfaces and scale sets that use the security group
// id. The caller holds s.mu.
func (s *Server) nsgUsers(id string) []string {
	var users []string
	for _, res := range s.resources {
		switch {
		case res.nic != nil && key(res.nic.nsg) == key(id):
			users = append(users, res.id)
		case res.scale != nil:
			for _, cfg := range res.scale.nics {
				if key(cfg.nsg) == key(id) {
					for _, inst := range res.scale.sorted() {
						users = append(users, inst.id(res)+"/networkInterfaces/"+cfg.name)
					}
				}
			}
		}
	}
	slices.Sort(users)
	return users
}

// pipUser returns the IP configuration, NAT gateway or load balancer frontend
// IP configuration that uses the public IP address id, or "". The caller
// holds s.mu.
func (s *Server) pipUser(id string) string {
	for _, res := range s.resources {
		if res.nic == nil {
			continue
		}
		for _, ic := range res.nic.configs {
			if key(ic.pip) == key(id) {
				return res.id + "/ipConfigurations/" + ic.name
			}
		}
	}
	return cmp.Or(s.pipNAT(id), s.pipLB(id))
}

func (s *Server) renderNetwork(res *resource) map[string]any {
	if res.kind == subnetKind {
		return renderSubnet(res)
	}
	m, props := envelope(res)
	props["resourceGuid"] = res.guid
	switch res.kind {
	case nsgKind:
		rules, _ := objects(res.props, "securityRules")
		out := make([]any, 0, len(rules))
		for _, r := range rules {
			rule := maps.Clone(r)
			name := str(r, "name")
			rule["id"], rule["etag"], rule["type"] = res.id+"/securityRules/"+name, res.etag, res.kind.typ+"/securityRules"
			rp, _ := r["properties"].(map[string]any)
			rp = maps.Clone(rp)
			if rp == nil {
				rp = map[string]any{}
			}
			rp["provisioningState"] = res.state
			rule["properties"] = rp
			out = append(out, rule)
		}
		props["securityRules"] = out
		var nics []any
		for _, u := range s.nsgUsers(res.id) {
			nics = append(nics, ref(u))
		}
		if nics != nil {
			props["networkInterfaces"] = nics
		}
		var subnets []any
		for _, u := range s.subnetsUsing(res.id, subnetNSG) {
			subnets = append(subnets, ref(u))
		}
		if subnets != nil {
			props["subnets"] = subnets
		}
	case nicKind:
		if res.nic != nil {
			s.renderNIC(res, props)
		}
	case pipKind:
		var address netip.Addr
		if res.pip != nil {
			address = res.pip.address
		}
		renderPublicIP(res.loc, address, props)
		if nat := s.pipNAT(res.id); nat != "" {
			props["natGateway"] = ref(nat)
		} else if user := s.pipUser(res.id); user != "" {
			props["ipConfiguration"] = ref(user)
		}
	case peKind:
		renderPE(res, props)
	case vnetKind:
		s.renderVNet(res, props)
	case natKind:
		s.renderNAT(res, props)
	case lbKind:
		s.renderLB(res, props)
	}
	return m
}

func (s *Server) renderNIC(res *resource, props map[string]any) {
	configs := make([]any, 0, len(res.nic.configs))
	for _, ic := range res.nic.configs {
		configs = append(configs, map[string]any{
			"id": res.id + "/ipConfigurations/" + ic.name, "name": ic.name, "etag": res.etag, "type": ipConfigType,
			"properties": renderIPConfig(ic, res.state),
		})
	}
	props["ipConfigurations"] = configs
	if res.nic.nsg != "" {
		props["networkSecurityGroup"] = ref(res.nic.nsg)
	}
	if vm := res.nic.vm; vm != "" {
		props["virtualMachine"] = ref(vm)
		props["primary"] = s.primaryNIC(vm) == key(res.id)
		props["macAddress"] = macAddress(res.guid)
	}
	for _, name := range []string{"enableAcceleratedNetworking", "enableIPForwarding"} {
		if _, ok := props[name]; !ok {
			props[name] = false
		}
	}
	if _, ok := props["nicType"]; !ok {
		props["nicType"] = "Standard"
	}
}

func renderIPConfig(ic *ipConfig, state string) map[string]any {
	p := maps.Clone(ic.props)
	if p == nil {
		p = map[string]any{}
	}
	p["privateIPAddress"] = ic.address.String()
	p["privateIPAllocationMethod"] = "Dynamic"
	if ic.static {
		p["privateIPAllocationMethod"] = "Static"
	}
	p["privateIPAddressVersion"] = "IPv4"
	p["primary"] = ic.primary
	p["provisioningState"] = state
	p["subnet"] = ref(ic.subnet)
	if ic.pip != "" {
		p["publicIPAddress"] = ref(ic.pip)
	}
	return p
}

// renderPublicIP adds the properties Azure computes for a public IP address.
func renderPublicIP(location string, address netip.Addr, props map[string]any) {
	if address.IsValid() {
		props["ipAddress"] = address.String()
	}
	props["publicIPAddressVersion"] = "IPv4"
	if _, ok := props["publicIPAllocationMethod"]; !ok {
		props["publicIPAllocationMethod"] = "Static"
	}
	if _, ok := props["idleTimeoutInMinutes"]; !ok {
		props["idleTimeoutInMinutes"] = 4
	}
	if dns, _ := props["dnsSettings"].(map[string]any); dns != nil {
		if label := str(dns, "domainNameLabel"); label != "" {
			dns = maps.Clone(dns)
			dns["fqdn"] = label + "." + location + ".cloudapp.azure.com"
			props["dnsSettings"] = dns
		}
	}
}

// macAddress derives a stable MAC address in Azure's 00-0D-3A range from a GUID.
func macAddress(guid string) string {
	h := fnv.New32a()
	_, _ = h.Write([]byte(guid))
	b := h.Sum(nil)
	return fmt.Sprintf("00-0D-3A-%02X-%02X-%02X", b[1], b[2], b[3])
}

func (s *Server) takePrivate(subnet string, a netip.Addr, holder string) {
	used := s.privateIPs[key(subnet)]
	if used == nil {
		used = map[netip.Addr]string{}
		s.privateIPs[key(subnet)] = used
	}
	used[a] = holder
}

func (s *Server) releasePrivate(subnet string, a netip.Addr) {
	delete(s.privateIPs[key(subnet)], a)
}

// releaseNIC frees the addresses of the interface res. The caller holds s.mu.
func (s *Server) releaseNIC(res *resource) {
	for _, ic := range res.nic.configs {
		s.releasePrivate(ic.subnet, ic.address)
	}
}

// NetworkInterface is a snapshot of a network interface.
type NetworkInterface struct {
	ID string
	// PrivateIPAddresses are the addresses of its IP configurations, in order.
	PrivateIPAddresses []string
	// VirtualMachine is the ID of the VM it is attached to, if any.
	VirtualMachine string
	Tags           map[string]string
}

// NetworkInterfaces returns the network interfaces the server holds, not
// counting those of scale set VMs, ordered by ID.
func (s *Server) NetworkInterfaces() []NetworkInterface {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []NetworkInterface
	for _, res := range s.resources {
		if res.nic == nil {
			continue
		}
		n := NetworkInterface{ID: res.id, VirtualMachine: res.nic.vm, Tags: maps.Clone(res.tags)}
		for _, ic := range res.nic.configs {
			n.PrivateIPAddresses = append(n.PrivateIPAddresses, ic.address.String())
		}
		out = append(out, n)
	}
	slices.SortFunc(out, func(a, b NetworkInterface) int { return cmp.Compare(a.ID, b.ID) })
	return out
}

// PublicIPAddress is a snapshot of a public IP address.
type PublicIPAddress struct {
	ID        string
	IPAddress string
	Tags      map[string]string
}

// PublicIPAddresses returns the public IP addresses the server holds, not
// counting those of scale set VMs, ordered by ID.
func (s *Server) PublicIPAddresses() []PublicIPAddress {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []PublicIPAddress
	for _, res := range s.resources {
		if res.pip != nil {
			out = append(out, PublicIPAddress{ID: res.id, IPAddress: res.pip.address.String(), Tags: maps.Clone(res.tags)})
		}
	}
	slices.SortFunc(out, func(a, b PublicIPAddress) int { return cmp.Compare(a.ID, b.ID) })
	return out
}
