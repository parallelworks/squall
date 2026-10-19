package arm

import (
	"cmp"
	"encoding/json"
	"fmt"
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
		located: true, location: "global", etag: plainETag,
	}
	linkKind = &kind{
		typ: "Microsoft.Network/privateDnsZones/virtualNetworkLinks", plural: "virtualNetworkLinks", typeName: "VirtualNetworkLink",
		props:    []string{"registrationEnabled", "resolutionPolicy", "virtualNetwork"},
		readOnly: []string{"provisioningState", "virtualNetworkLinkState"},
		located:  true, location: "global", etag: quotedETag,
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
	return s.put(putSpec{kind: zoneKind, id: zoneID, paths: dnsPaths, render: s.renderZone, emptyAccepted: true})(c)
}

func (s *Server) putLink(c *call) (*reply, *apiError) {
	return s.put(putSpec{
		kind: linkKind, id: linkID, parent: zoneID, paths: dnsPaths, render: s.renderLink, emptyAccepted: true,
		precondition: func(r *http.Request, existing *resource) *apiError {
			err := checkPrecondition(r, existing)
			if err != nil && existing != nil && r.Header.Get("If-None-Match") == "*" {
				err = newError(http.StatusPreconditionFailed, "PreconditionFailed",
					"The Virtual Network link %s for the Private DNS zone %s exists already and hence cannot be created again.", existing.name, zoneName(existing.id))
				err.async = true
			}
			return err
		},
		prepare: func(res *resource, p *parsed) (func(), *apiError) {
			vnet := refID(p.props, "virtualNetwork")
			if vnet == "" {
				return nil, badRequest("The virtual network link %s must reference a virtual network.", res.id)
			}
			zone := key(res.id[:strings.LastIndex(key(res.id), "/virtualnetworklinks/")])
			for _, other := range s.resources {
				if other.kind == linkKind && other != res && strings.HasPrefix(key(other.id), zone+"/") &&
					key(refID(other.props, "virtualNetwork")) == key(vnet) {
					name := zoneName(res.id)
					err := badRequest("A virtual network cannot be linked to multiple zones with overlapping namespaces. You tried to link virtual network with '%s' and '%s' zones.", name, name)
					err.async = true
					return nil, err
				}
			}
			return func() { s.update(res, p) }, nil
		},
	})(c)
}

// linkOperationPending turns away a change to the virtual network link res while op changes it.
func linkOperationPending(res *resource, op *operation) *apiError {
	e := newError(http.StatusConflict, "Conflict",
		"Another operation is pending for requested object. Operation group '/operations/groups/id/|virtualNetworkLinks|%s|%s|%s|%s' already has 1 operations like '/operations/type/UpsertVirtualNetworkLink/id/%s' queued.",
		res.sub, res.group, zoneName(res.id), res.name, op.id)
	e.bare = true
	return e
}

// recordPrecondition applies the If-Match and If-None-Match headers to a
// change of the record set name, as private DNS answers them.
func recordPrecondition(r *http.Request, existing *resource, name string) *apiError {
	if checkPrecondition(r, existing) == nil {
		return nil
	}
	e := newError(http.StatusPreconditionFailed, "PreconditionFailed", "The Record set %s has been modified (etag mismatch).", name)
	if existing != nil && r.Header.Get("If-None-Match") == "*" {
		e.Message = fmt.Sprintf("The Record set %s exists already and hence cannot be created again.", name)
	}
	e.bare = true
	return e
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
	return s.getDNS(c, linkKind, linkID(c), c.val("zone")+"/virtualNetworkLinks/"+c.val("name"), s.renderLink)
}

func (s *Server) getRecord(c *call) (*reply, *apiError) {
	return s.getDNS(c, recordKind, recordID(c), c.val("zone")+"/A/"+c.val("name"), renderRecord)
}

// getDNS answers a get of the private DNS resource id of kind k, named in
// errors by its path below Microsoft.Network/privateDnsZones.
func (s *Server) getDNS(c *call, k *kind, id, name string, render func(*resource) map[string]any) (*reply, *apiError) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.readableGroup(c); err != nil {
		return nil, err
	}
	res := s.read(id)
	if res != nil && res.kind == k {
		return ok200(render(res)), nil
	}
	if k == recordKind && s.read(zoneID(c)) != nil {
		e := newError(http.StatusNotFound, "NotFound", "The resource record '%s' does not exist in resource group '%s' of subscription '%s'.",
			c.val("name"), c.val("rg"), c.val("sub"))
		e.bare = true
		return nil, e
	}
	return nil, resourceNotFound(zoneKind.typ, name, c.val("rg"))
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
		return nil, parentNotFound(recordKind, zoneID(c))
	}
	id := recordID(c)
	res := s.settled(id)
	if err := recordPrecondition(c.r, res, c.val("name")); err != nil {
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
	if err := recordPrecondition(c.r, res, c.val("name")); err != nil {
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
