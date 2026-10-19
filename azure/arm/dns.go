package arm

import (
	"cmp"
	"encoding/json"
	"net/http"
	"net/netip"
	"slices"
	"strconv"
	"strings"
)

var (
	zoneKind = &kind{
		typ: "Microsoft.Network/privateDnsZones", plural: "privateDnsZones", typeName: "PrivateZone",
		readOnly: []string{
			"internalId", "maxNumberOfRecordSets", "maxNumberOfVirtualNetworkLinks",
			"maxNumberOfVirtualNetworkLinksWithRegistration", "numberOfRecordSets", "numberOfVirtualNetworkLinks",
			"numberOfVirtualNetworkLinksWithRegistration", "provisioningState",
		},
		located: true, etag: plainETag,
	}
	linkKind = &kind{
		typ: "Microsoft.Network/privateDnsZones/virtualNetworkLinks", plural: "virtualNetworkLinks", typeName: "VirtualNetworkLink",
		props:    []string{"registrationEnabled", "resolutionPolicy", "virtualNetwork"},
		readOnly: []string{"provisioningState", "virtualNetworkLinkState"},
		located:  true, etag: plainETag,
	}
	recordKind = &kind{
		typ: "Microsoft.Network/privateDnsZones/A", plural: "A", typeName: "RecordSet",
		props: []string{
			"aRecords", "metadata", "ttl", "aaaaRecords", "cnameRecord", "mxRecords", "ptrRecords", "soaRecord",
			"srvRecords", "txtRecords",
		},
		readOnly: []string{"fqdn", "isAutoRegistered"},
		etag:     plainETag,
	}
)

func zoneID(c *call) string { return resourceID(c, zoneKind.typ, c.val("zone")) }

func linkID(c *call) string { return zoneID(c) + "/virtualNetworkLinks/" + c.val("name") }

func recordID(c *call) string { return zoneID(c) + "/A/" + c.val("name") }

func dnsPaths(c *call, _ string) opPaths { return dnsOps(c) }

func (s *Server) putZone(c *call) (*reply, *apiError) {
	return s.put(putSpec{
		kind: zoneKind, id: zoneID, paths: dnsPaths, render: s.renderZone,
		prepare: func(res *resource, p *parsed) (func(), *apiError) {
			if p.location != "global" {
				return nil, badRequest("The location '%s' is not valid for a private DNS zone; it must be 'global'.", p.location)
			}
			return func() { s.update(res, p) }, nil
		},
	})(c)
}

func (s *Server) putLink(c *call) (*reply, *apiError) {
	return s.put(putSpec{
		kind: linkKind, id: linkID, parent: zoneID, paths: dnsPaths, render: s.renderLink,
		prepare: func(res *resource, p *parsed) (func(), *apiError) {
			if p.location != "global" {
				return nil, badRequest("The location '%s' is not valid for a virtual network link; it must be 'global'.", p.location)
			}
			vnet := refID(p.props, "virtualNetwork")
			if vnet == "" {
				return nil, badRequest("The virtual network link %s must reference a virtual network.", res.id)
			}
			zone := key(res.id[:strings.LastIndex(key(res.id), "/virtualnetworklinks/")])
			for _, other := range s.resources {
				if other.kind == linkKind && other != res && strings.HasPrefix(key(other.id), zone+"/") &&
					key(refID(other.props, "virtualNetwork")) == key(vnet) {
					return nil, newError(http.StatusConflict, "Conflict",
						"A virtual network cannot be linked to the same private DNS zone more than once. Virtual network %s is already linked by %s.", vnet, other.id)
				}
			}
			return func() { s.update(res, p) }, nil
		},
	})(c)
}

// zoneName returns the zone of a link or record set ID.
func zoneName(id string) string {
	const marker = "/privatednszones/"
	rest := id[strings.Index(key(id), marker)+len(marker):]
	if i := strings.IndexByte(rest, '/'); i >= 0 {
		rest = rest[:i]
	}
	return rest
}

func (s *Server) getZone(c *call) (*reply, *apiError) {
	return s.getDNS(c, zoneKind, zoneID(c), c.val("zone"), s.renderZone)
}

func (s *Server) getLink(c *call) (*reply, *apiError) {
	return s.getDNS(c, linkKind, linkID(c), c.val("zone")+"/"+c.val("name"), s.renderLink)
}

func (s *Server) getRecord(c *call) (*reply, *apiError) {
	return s.getDNS(c, recordKind, recordID(c), c.val("zone")+"/A/"+c.val("name"), renderRecord)
}

func (s *Server) getDNS(c *call, k *kind, id, name string, render func(*resource) map[string]any) (*reply, *apiError) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.readableGroup(c); err != nil {
		return nil, err
	}
	res := s.read(id)
	if res == nil || res.kind != k {
		return nil, resourceNotFound(zoneKind.typ, name, c.val("rg"))
	}
	return ok200(render(res)), nil
}

// zoneChildren counts the links (with registration) and record sets in the zone id. The caller holds s.mu.
func (s *Server) zoneChildren(id string) (links, registering, records int) {
	prefix := key(id) + "/"
	for k, res := range s.resources {
		if !strings.HasPrefix(k, prefix) {
			continue
		}
		switch res.kind {
		case linkKind:
			links++
			if v, _ := boolean(res.props, "registrationEnabled"); v {
				registering++
			}
		case recordKind:
			records++
		}
	}
	return links, registering, records
}

func (s *Server) renderZone(res *resource) map[string]any {
	m, props := envelope(res)
	links, registering, records := s.zoneChildren(res.id)
	props["internalId"] = res.guid
	props["maxNumberOfRecordSets"] = 25000
	props["maxNumberOfVirtualNetworkLinks"] = 1000
	props["maxNumberOfVirtualNetworkLinksWithRegistration"] = 100
	props["numberOfRecordSets"] = records + 1
	props["numberOfVirtualNetworkLinks"] = links
	props["numberOfVirtualNetworkLinksWithRegistration"] = registering
	return m
}

func (s *Server) renderLink(res *resource) map[string]any {
	m, props := envelope(res)
	if _, ok := props["registrationEnabled"]; !ok {
		props["registrationEnabled"] = false
	}
	props["virtualNetworkLinkState"] = "Completed"
	if res.state != stateSucceeded {
		props["virtualNetworkLinkState"] = "InProgress"
	}
	return m
}

func renderRecord(res *resource) map[string]any {
	m, props := envelope(res)
	delete(props, "provisioningState")
	zone := zoneName(res.id)
	props["fqdn"] = zone + "."
	if res.name != "@" {
		props["fqdn"] = res.name + "." + zone + "."
	}
	props["isAutoRegistered"] = false
	if _, ok := props["aRecords"]; !ok {
		props["aRecords"] = []any{}
	}
	return m
}

func (s *Server) putRecord(c *call) (*reply, *apiError) {
	p, err := parseBody(c, recordKind)
	if err != nil {
		return nil, err
	}
	for _, name := range recordKind.props {
		if _, ok := p.props[name]; ok && name != "aRecords" && name != "metadata" && name != "ttl" {
			return nil, badRequest("An A record set cannot have %s.", name)
		}
	}
	records, err := objects(p.props, "aRecords")
	if err != nil {
		return nil, err
	}
	for i, r := range records {
		if err := checkMembers(r, "ARecord", "properties.aRecords["+strconv.Itoa(i)+"]", "ipv4Address"); err != nil {
			return nil, err
		}
		if a, perr := netip.ParseAddr(str(r, "ipv4Address")); perr != nil || !a.Is4() {
			return nil, badRequest("The A record value '%s' is not a valid IPv4 address.", str(r, "ipv4Address"))
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.writableGroup(c); err != nil {
		return nil, err
	}
	if s.settled(zoneID(c)) == nil {
		return nil, parentNotFound(c.val("zone"))
	}
	id := recordID(c)
	res := s.settled(id)
	if err := checkPrecondition(c.r, res); err != nil {
		return nil, err
	}
	status := http.StatusOK
	if res == nil {
		res, status = s.newResource(c, recordKind, id, p), http.StatusCreated
		s.resources[key(id)] = res
	} else {
		s.update(res, p)
	}
	return &reply{status: status, body: encode(renderRecord(res))}, nil
}

func (s *Server) deleteRecord(c *call) (*reply, *apiError) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.writableGroup(c); err != nil {
		return nil, err
	}
	res := s.settled(recordID(c))
	if err := checkPrecondition(c.r, res); err != nil {
		return nil, err
	}
	if res == nil {
		return &reply{status: http.StatusNoContent}, nil
	}
	s.remove(res)
	return &reply{status: http.StatusOK}, nil
}

// RecordSet is a snapshot of a private DNS A record set.
type RecordSet struct {
	ID string
	// FQDN is the record set's fully qualified name, ending in a dot.
	FQDN      string
	TTL       int64
	Addresses []string
}

// RecordSets returns the private DNS A record sets the server holds, ordered by ID.
func (s *Server) RecordSets() []RecordSet {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []RecordSet
	for _, res := range s.resources {
		if res.kind != recordKind {
			continue
		}
		props := renderRecord(res)["properties"].(map[string]any)
		r := RecordSet{ID: res.id, FQDN: props["fqdn"].(string)}
		if ttl, ok := res.props["ttl"].(json.Number); ok {
			r.TTL, _ = ttl.Int64()
		}
		records, _ := objects(res.props, "aRecords")
		for _, a := range records {
			r.Addresses = append(r.Addresses, str(a, "ipv4Address"))
		}
		out = append(out, r)
	}
	slices.SortFunc(out, func(a, b RecordSet) int { return cmp.Compare(a.ID, b.ID) })
	return out
}
