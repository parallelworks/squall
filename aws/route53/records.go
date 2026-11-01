package route53

import (
	"cmp"
	"encoding/xml"
	"fmt"
	"maps"
	"net/netip"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"
)

const maxRecordsPage = 300

// recordTypes are the record types Route 53 supports.
var recordTypes = []string{"A", "AAAA", "CAA", "CNAME", "DS", "HTTPS", "MX", "NAPTR", "NS", "PTR", "SOA", "SPF", "SRV", "SSHFP", "SVCB", "TLSA", "TXT"}

// RecordSet is a simple resource record set, or an alias.
type RecordSet struct {
	// Name is the record set's name as Route 53 returns it, with a trailing dot.
	Name string
	Type string
	// TTL is zero for an alias.
	TTL int64
	// Values are the records' values in the order they were sent, empty for an alias.
	Values []string
	// Alias is the alias target, nil for a set of records.
	Alias *AliasTarget
}

// AliasTarget is where an alias record set points.
type AliasTarget struct {
	HostedZoneID         string
	DNSName              string
	EvaluateTargetHealth bool
}

type recordKey struct{ name, typ string }

func (rs *RecordSet) key() recordKey { return recordKey{rs.Name, rs.Type} }

func (rs *RecordSet) clone() RecordSet {
	c := *rs
	c.Values = slices.Clone(rs.Values)
	if rs.Alias != nil {
		c.Alias = new(*rs.Alias)
	}
	return c
}

// matches reports whether rs has other's TTL, values and alias target, as a DELETE must name them.
func (rs *RecordSet) matches(other *RecordSet) bool {
	if rs.TTL != other.TTL || (rs.Alias == nil) != (other.Alias == nil) || rs.Alias != nil && *rs.Alias != *other.Alias {
		return false
	}
	a, b := slices.Clone(rs.Values), slices.Clone(other.Values)
	slices.Sort(a)
	slices.Sort(b)
	return slices.Equal(a, b)
}

// defaultRecordSets are the SOA and NS record sets Route 53 creates with a private zone.
func defaultRecordSets(zone string) []*RecordSet {
	return []*RecordSet{
		{Name: zone, Type: "NS", TTL: 172800, Values: []string{
			"ns-1536.awsdns-00.co.uk.", "ns-0.awsdns-00.com.", "ns-1024.awsdns-00.org.", "ns-512.awsdns-00.net.",
		}},
		{Name: zone, Type: "SOA", TTL: 900, Values: []string{"ns-1536.awsdns-00.co.uk. awsdns-hostmaster.amazon.com. 1 7200 900 1209600 86400"}},
	}
}

type resourceRecordXML struct {
	Value string `xml:"Value"`
}

type aliasTargetXML struct {
	HostedZoneID         string `xml:"HostedZoneId"`
	DNSName              string `xml:"DNSName"`
	EvaluateTargetHealth bool   `xml:"EvaluateTargetHealth"`
}

type recordSetXML struct {
	Name            string              `xml:"Name"`
	Type            string              `xml:"Type"`
	TTL             *int64              `xml:"TTL,omitempty"`
	ResourceRecords []resourceRecordXML `xml:"ResourceRecords>ResourceRecord,omitempty"`
	AliasTarget     *aliasTargetXML     `xml:"AliasTarget,omitempty"`
}

// changeRecordSetXML is a record set in a change, with the fields the server rejects.
type changeRecordSetXML struct {
	Name            string `xml:"Name"`
	Type            string `xml:"Type"`
	TTL             *int64 `xml:"TTL"`
	ResourceRecords *struct {
		Records []resourceRecordXML `xml:"ResourceRecord"`
	} `xml:"ResourceRecords"`
	AliasTarget             *aliasTargetXML `xml:"AliasTarget"`
	SetIdentifier           *string         `xml:"SetIdentifier"`
	Weight                  *string         `xml:"Weight"`
	Region                  *string         `xml:"Region"`
	GeoLocation             *struct{}       `xml:"GeoLocation"`
	Failover                *string         `xml:"Failover"`
	MultiValueAnswer        *string         `xml:"MultiValueAnswer"`
	HealthCheckID           *string         `xml:"HealthCheckId"`
	TrafficPolicyInstanceID *string         `xml:"TrafficPolicyInstanceId"`
	CidrRoutingConfig       *struct{}       `xml:"CidrRoutingConfig"`
	GeoProximityLocation    *struct{}       `xml:"GeoProximityLocation"`
}

func (rs *RecordSet) xml() recordSetXML {
	out := recordSetXML{Name: rs.Name, Type: rs.Type}
	if rs.Alias != nil {
		out.AliasTarget = &aliasTargetXML{HostedZoneID: rs.Alias.HostedZoneID, DNSName: rs.Alias.DNSName, EvaluateTargetHealth: rs.Alias.EvaluateTargetHealth}
		return out
	}
	out.TTL = new(rs.TTL)
	for _, v := range rs.Values {
		out.ResourceRecords = append(out.ResourceRecords, resourceRecordXML{Value: v})
	}
	return out
}

type changeXML struct {
	Action            string             `xml:"Action"`
	ResourceRecordSet changeRecordSetXML `xml:"ResourceRecordSet"`
}

// parsed is a change whose request is well formed; badName is set for a name that is not a domain name.
type parsed struct {
	action  string
	rs      *RecordSet
	badName string
}

// parseChange checks a change's form, which fails the whole request with InvalidInput.
func parseChange(ch changeXML) (parsed, *apiError) {
	in := ch.ResourceRecordSet
	if ch.Action != "CREATE" && ch.Action != "DELETE" && ch.Action != "UPSERT" {
		return parsed{}, invalidInput("Invalid request: %q is not an action, which must be CREATE, DELETE or UPSERT", ch.Action)
	}
	if !slices.Contains(recordTypes, in.Type) {
		return parsed{}, invalidInput("Invalid request: %q is not a record type", in.Type)
	}
	if in.SetIdentifier != nil || in.Weight != nil || in.Region != nil || in.GeoLocation != nil || in.Failover != nil ||
		in.MultiValueAnswer != nil || in.HealthCheckID != nil || in.TrafficPolicyInstanceID != nil ||
		in.CidrRoutingConfig != nil || in.GeoProximityLocation != nil {
		return parsed{}, invalidInput("squall: routing policies, health checks and traffic policy instances are not supported")
	}
	found := ""
	switch hasTTL, hasRecords := in.TTL != nil, in.ResourceRecords != nil; {
	case in.AliasTarget != nil && (hasTTL || hasRecords):
		found = "more than one"
	case in.AliasTarget == nil && (!hasTTL || !hasRecords):
		found = "none"
	}
	if found != "" {
		return parsed{}, invalidInput("Invalid request: Expected exactly one of [AliasTarget, all of [TTL, and ResourceRecords], or TrafficPolicyInstanceId], but found %s in Change with [Action=%s, Name=%s, Type=%s, SetIdentifier=null]",
			found, ch.Action, in.Name, in.Type)
	}
	name, ok := canonicalName(in.Name)
	if !ok {
		return parsed{action: ch.Action, badName: in.Name}, nil
	}
	rs := &RecordSet{Name: name, Type: in.Type}
	if in.AliasTarget != nil {
		target, ok := canonicalName(in.AliasTarget.DNSName)
		if !ok || in.AliasTarget.HostedZoneID == "" {
			return parsed{}, invalidInput("Invalid request: an AliasTarget needs a HostedZoneId and a DNSName")
		}
		rs.Alias = &AliasTarget{HostedZoneID: in.AliasTarget.HostedZoneID, DNSName: target, EvaluateTargetHealth: in.AliasTarget.EvaluateTargetHealth}
		return parsed{action: ch.Action, rs: rs}, nil
	}
	if *in.TTL < 0 || *in.TTL > 1<<31-1 {
		return parsed{}, invalidInput("Invalid request: TTL must be from 0 to 2147483647")
	}
	if len(in.ResourceRecords.Records) == 0 {
		return parsed{}, invalidInput("Invalid request: ResourceRecords needs at least one ResourceRecord")
	}
	rs.TTL = *in.TTL
	for _, r := range in.ResourceRecords.Records {
		if r.Value == "" || utf8.RuneCountInString(r.Value) > 4000 {
			return parsed{}, invalidInput("Invalid request: a resource record's value is 1 to 4000 characters")
		}
		rs.Values = append(rs.Values, r.Value)
	}
	return parsed{action: ch.Action, rs: rs}, nil
}

// valueProblem returns what is wrong with a record set's values, or "".
func valueProblem(rs *RecordSet) string {
	switch rs.Type {
	case "A", "AAAA":
		for _, v := range rs.Values {
			addr, err := netip.ParseAddr(v)
			if rs.Type == "A" && (err != nil || !addr.Is4()) {
				return fmt.Sprintf("Invalid Resource Record: 'FATAL problem: ARRDATAIllegalIPv4Address (Value is not a valid IPv4 address) encountered with '%s''", v)
			}
			if rs.Type == "AAAA" && (err != nil || !addr.Is6() || addr.Is4In6() || addr.Zone() != "") {
				return fmt.Sprintf("Invalid Resource Record: 'FATAL problem: AAAARRDATAIllegalIPv6Address (Value is not a valid IPv6 address) encountered with '%s''", v)
			}
		}
	case "CNAME":
		if len(rs.Values) > 1 {
			return fmt.Sprintf("RRSet of type CNAME with DNS name %s has more than one value", rs.Name)
		}
	}
	return ""
}

// conflict returns why a CNAME and other record sets cannot share rs's name in z, or "".
func conflict(records map[recordKey]*RecordSet, rs *RecordSet, zone string) string {
	if rs.Type == "CNAME" && rs.Name == zone {
		return fmt.Sprintf("RRSet of type CNAME with DNS name %s is not permitted at apex in zone %s", rs.Name, zone)
	}
	for k := range records {
		if k.name != rs.Name || k.typ == rs.Type {
			continue
		}
		if rs.Type == "CNAME" || k.typ == "CNAME" {
			return fmt.Sprintf("RRSet of type %s with DNS name %s is not permitted as it conflicts with other records with the same DNS name in zone %s", rs.Type, rs.Name, zone)
		}
	}
	return ""
}

func describe(rs *RecordSet) string {
	return fmt.Sprintf("[name='%s', type='%s']", rs.Name, rs.Type)
}

func (s *Server) changeRecordSets(c *call) (*reply, *apiError) {
	var in struct {
		Comment string      `xml:"ChangeBatch>Comment"`
		Changes []changeXML `xml:"ChangeBatch>Changes>Change"`
	}
	if err := decodeXML(c.body, &in); err != nil {
		return nil, err
	}
	if len(in.Changes) == 0 {
		return nil, invalidInput("Invalid request: a ChangeBatch needs at least one Change")
	}
	if utf8.RuneCountInString(in.Comment) > 256 {
		return nil, invalidInput("Invalid request: a comment is up to 256 characters")
	}
	changes := make([]parsed, 0, len(in.Changes))
	for _, ch := range in.Changes {
		p, err := parseChange(ch)
		if err != nil {
			return nil, err
		}
		changes = append(changes, p)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	z, err := s.lookup(c.id)
	if err != nil {
		return nil, err
	}
	records := maps.Clone(z.records)
	var messages []string
	for _, ch := range changes {
		if ch.badName != "" {
			messages = append(messages, fmt.Sprintf("Invalid Resource Record: 'FATAL problem: DomainNameInvalid (%s is not a valid domain name)'", ch.badName))
			continue
		}
		rs := ch.rs
		if !inZone(rs.Name, z.Name) {
			messages = append(messages, fmt.Sprintf("RRSet with DNS name %s is not permitted in zone %s", rs.Name, z.Name))
			continue
		}
		cur, exists := records[rs.key()]
		if ch.action == "DELETE" {
			switch {
			case !exists:
				messages = append(messages, fmt.Sprintf("Tried to delete resource record set %s but it was not found", describe(rs)))
			case !cur.matches(rs):
				messages = append(messages, fmt.Sprintf("Tried to delete resource record set %s but the values provided do not match the current values", describe(rs)))
			case rs.Name == z.Name && (rs.Type == "SOA" || rs.Type == "NS"):
				messages = append(messages, fmt.Sprintf("A HostedZone must contain at least one %s record for the zone itself.", rs.Type))
			default:
				delete(records, rs.key())
			}
			continue
		}
		if ch.action == "CREATE" && exists {
			messages = append(messages, fmt.Sprintf("Tried to create resource record set %s but it already exists", describe(rs)))
			continue
		}
		if problem := cmp.Or(valueProblem(rs), conflict(records, rs, z.Name)); problem != "" {
			messages = append(messages, problem)
			continue
		}
		records[rs.key()] = rs
	}
	if len(messages) > 0 {
		return nil, invalidChangeBatch(messages)
	}
	z.records = records
	return changeInfoReply("ChangeResourceRecordSets", s.record(in.Comment)), nil
}

// sortedRecords returns z's record sets in the order ListResourceRecordSets lists them.
func sortedRecords(z *zone) []*RecordSet {
	return slices.SortedFunc(maps.Values(z.records), compareRecords)
}

func compareRecords(a, b *RecordSet) int {
	if c := compareNames(a.Name, b.Name); c != 0 {
		return c
	}
	return strings.Compare(a.Type, b.Type)
}

func (s *Server) listRecordSets(c *call) (*reply, *apiError) {
	limit, err := maxItems(c, maxRecordsPage)
	if err != nil {
		return nil, err
	}
	startName, startType := c.query.Get("name"), c.query.Get("type")
	if c.query.Has("identifier") {
		return nil, invalidInput("squall: record sets have no set identifiers")
	}
	if startType != "" && startName == "" {
		return nil, invalidInput("Invalid request: type requires name")
	}
	if startType != "" && !slices.Contains(recordTypes, startType) {
		return nil, invalidInput("Invalid request: %q is not a record type", startType)
	}
	start := &RecordSet{Type: startType}
	if startName != "" {
		var ok bool
		if start.Name, ok = canonicalName(startName); !ok {
			return nil, invalidInput("Invalid request: %s is not a valid domain name", startName)
		}
	}
	type page struct {
		XMLName            xml.Name       `xml:"https://route53.amazonaws.com/doc/2013-04-01/ ListResourceRecordSetsResponse"`
		ResourceRecordSets []recordSetXML `xml:"ResourceRecordSets>ResourceRecordSet"`
		IsTruncated        bool           `xml:"IsTruncated"`
		NextRecordName     string         `xml:"NextRecordName,omitempty"`
		NextRecordType     string         `xml:"NextRecordType,omitempty"`
		MaxItems           string         `xml:"MaxItems"`
	}
	out := page{ResourceRecordSets: []recordSetXML{}, MaxItems: strconv.Itoa(limit)}
	s.mu.Lock()
	defer s.mu.Unlock()
	z, err := s.lookup(c.id)
	if err != nil {
		return nil, err
	}
	for _, rs := range sortedRecords(z) {
		if start.Name != "" && compareRecords(rs, start) < 0 {
			continue
		}
		if len(out.ResourceRecordSets) == limit {
			out.IsTruncated, out.NextRecordName, out.NextRecordType = true, rs.Name, rs.Type
			break
		}
		out.ResourceRecordSets = append(out.ResourceRecordSets, rs.xml())
	}
	return &reply{body: out}, nil
}

// RecordSets returns the record sets of the hosted zone with id, which may
// carry the "/hostedzone/" prefix, in the order ListResourceRecordSets lists
// them, or nil if there is no such zone.
func (s *Server) RecordSets(zoneID string) []RecordSet {
	s.mu.Lock()
	defer s.mu.Unlock()
	z, ok := s.zones[strings.TrimPrefix(zoneID, "/hostedzone/")]
	if !ok {
		return nil
	}
	sorted := sortedRecords(z)
	out := make([]RecordSet, 0, len(sorted))
	for _, rs := range sorted {
		out = append(out, rs.clone())
	}
	return out
}
