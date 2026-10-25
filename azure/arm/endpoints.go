package arm

import (
	"maps"
	"net/http"
	"strings"
)

var peKind = &kind{
	typ: "Microsoft.Network/privateEndpoints", plural: "privateEndpoints", typeName: "PrivateEndpoint",
	top: []string{"extendedLocation"},
	props: []string{
		"applicationSecurityGroups", "customDnsConfigs", "customNetworkInterfaceName", "ipConfigurations", "ipVersionType",
		"manualPrivateLinkServiceConnections", "privateLinkServiceConnections", "subnet",
	},
	readOnly: []string{"networkInterfaces", "provisioningState", "resourceGuid"},
	located:  true, etag: weakETag, opName: "PrivateEndpoint",
}

// peState is what a private endpoint holds beyond its body.
type peState struct {
	// nic is the ID of the network interface Azure made for it.
	nic string
}

// preparePE validates p as the new body of the private endpoint res, and
// returns how to apply it. A new one gets a network interface in its subnet,
// named customNetworkInterfaceName or as Azure names it, without the
// endpoint's tags, as Azure makes it. The caller holds s.mu.
func (s *Server) preparePE(res *resource, p *parsed) (func(), *apiError) {
	subnet := refID(p.props, "subnet")
	if subnet == "" {
		return nil, badRequest("Private endpoint %s must reference a subnet.", res.id)
	}
	auto, err := objects(p.props, "privateLinkServiceConnections")
	if err != nil {
		return nil, err
	}
	manual, err := objects(p.props, "manualPrivateLinkServiceConnections")
	if err != nil {
		return nil, err
	}
	if len(auto)+len(manual) != 1 {
		return nil, unsupported("a private endpoint needs exactly one private link service connection")
	}
	if res.pe != nil {
		if key(subnet) != key(res.pe.subnet(s)) {
			return nil, changeNotAllowed("subnet")
		}
		return func() { s.update(res, p) }, nil
	}
	nicName := str(p.props, "customNetworkInterfaceName")
	if nicName == "" {
		nicName = res.name + ".nic." + res.guid
	}
	nicID := res.id[:strings.LastIndex(res.id, "/providers/")] + "/providers/" + nicKind.typ + "/" + nicName
	if s.resources[key(nicID)] != nil {
		return nil, unsupported("network interface %s already exists", nicID)
	}
	if err := s.checkFree([]string{subnet}, 0, ""); err != nil {
		return nil, err
	}
	return func() {
		nic := &resource{
			kind: nicKind, id: nicID, name: nicName, sub: res.sub, group: res.group, loc: res.loc,
			top: map[string]any{}, props: map[string]any{"privateEndpoint": ref(res.id)},
			state: stateSucceeded, created: s.now(), guid: s.newUUID(),
		}
		s.touch(nic)
		ic := &ipConfig{name: "privateEndpointIpConfig." + s.newUUID(), subnet: subnet, primary: true, props: map[string]any{}}
		ic.address = s.allocPrivate(subnet, nicID+"/ipConfigurations/"+ic.name)
		nic.nic = &nicState{configs: []*ipConfig{ic}}
		s.resources[key(nicID)] = nic
		res.pe = &peState{nic: nicID}
		s.update(res, p)
	}, nil
}

// subnet returns the subnet of the endpoint's network interface. The caller holds s.mu.
func (pe *peState) subnet(s *Server) string {
	if nic := s.resources[key(pe.nic)]; nic != nil && nic.nic != nil && len(nic.nic.configs) > 0 {
		return nic.nic.configs[0].subnet
	}
	return ""
}

// renderPE adds what Azure reports for a private endpoint: its network
// interface, and its connection approved when automatic and pending when manual.
func renderPE(res *resource, props map[string]any) {
	if res.pe != nil {
		props["networkInterfaces"] = []any{ref(res.pe.nic)}
	}
	for member, state := range map[string]map[string]any{
		"privateLinkServiceConnections":       {"status": "Approved", "description": "Auto-Approved", "actionsRequired": "None"},
		"manualPrivateLinkServiceConnections": {"status": "Pending", "description": "Awaiting approval", "actionsRequired": "None"},
	} {
		conns, _ := objects(res.props, member)
		if conns == nil {
			continue
		}
		out := make([]any, 0, len(conns))
		for _, conn := range conns {
			c := maps.Clone(conn)
			cp, _ := conn["properties"].(map[string]any)
			cp = maps.Clone(cp)
			if cp == nil {
				cp = map[string]any{}
			}
			cp["privateLinkServiceConnectionState"] = state
			cp["provisioningState"] = res.state
			c["id"], c["etag"], c["type"] = res.id+"/"+member+"/"+str(conn, "name"), res.etag, peKind.typ+"/"+member
			c["properties"] = cp
			out = append(out, c)
		}
		props[member] = out
	}
	if _, ok := props["customDnsConfigs"]; !ok {
		props["customDnsConfigs"] = []any{}
	}
}

// approvalAction stands for the action that approves a private endpoint's
// connection to the resource an ID names, which depends on the resource's type.
const approvalAction = "private endpoint connection approval"

// connectionApproval returns the privateEndpointConnectionsApproval action on
// the resource id names, such as
// Microsoft.Storage/storageAccounts/privateEndpointConnectionsApproval/action.
func connectionApproval(id string) string {
	_, rest, ok := strings.Cut(strings.ToLower(id), "/providers/")
	if !ok {
		return ""
	}
	// The original spelling of the namespace and types, for the message.
	rest = id[len(id)-len(rest):]
	parts := strings.Split(rest, "/")
	typ := []string{parts[0]}
	for i := 1; i < len(parts); i += 2 {
		typ = append(typ, parts[i])
	}
	return strings.Join(typ, "/") + "/privateEndpointConnectionsApproval/action"
}

// patchTags replaces the tags of the network resource of kind k, as the
// network resource providers' UpdateTags does.
func (s *Server) patchTags(k *kind) handler {
	return func(c *call) (*reply, *apiError) {
		body, err := readObject(c.r)
		if err != nil {
			return nil, err
		}
		if err := checkMembers(body, "TagsObject", "", "tags"); err != nil {
			return nil, err
		}
		tags, err := tagsOf(body)
		if err != nil {
			return nil, err
		}
		s.mu.Lock()
		defer s.mu.Unlock()
		if err := s.writableGroup(c); err != nil {
			return nil, err
		}
		res, err := s.claim(resourceID(c, k.typ, c.val("name")), "")
		if err != nil {
			return nil, err
		}
		if res == nil || res.kind != k {
			return nil, resourceNotFound(k.typ, c.val("name"), c.val("rg"))
		}
		res.tags = tags
		s.touch(res)
		return &reply{status: http.StatusOK, body: encode(s.renderNetwork(res))}, nil
	}
}
