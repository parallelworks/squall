package route53

import (
	"encoding/xml"
	"maps"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	timeFormat = "2006-01-02T15:04:05.000Z"

	maxZonesPage = 100
	maxTags      = 50
	maxTagChange = 10
)

// HostedZone is a private hosted zone.
type HostedZone struct {
	// ID is the zone's ID without the "/hostedzone/" prefix.
	ID string
	// Name is the zone's domain name as Route 53 returns it, with a trailing dot.
	Name            string
	CallerReference string
	Comment         string
	// VPCs are the VPCs the zone is associated with, in the order they were associated.
	VPCs []VPC
	// Tags is nil while the zone has none.
	Tags map[string]string
}

// VPC names a VPC by its region and ID.
type VPC struct {
	Region string
	ID     string
}

type zone struct {
	HostedZone
	records map[recordKey]*RecordSet
}

func (z *zone) clone() HostedZone {
	c := z.HostedZone
	c.VPCs = slices.Clone(z.VPCs)
	c.Tags = maps.Clone(z.Tags)
	return c
}

type change struct {
	id          string
	submittedAt time.Time
	comment     string
}

type hostedZoneXML struct {
	ID                     string              `xml:"Id"`
	Name                   string              `xml:"Name"`
	CallerReference        string              `xml:"CallerReference"`
	Config                 hostedZoneConfigXML `xml:"Config"`
	ResourceRecordSetCount int                 `xml:"ResourceRecordSetCount"`
}

type hostedZoneConfigXML struct {
	Comment     string `xml:"Comment,omitempty"`
	PrivateZone bool   `xml:"PrivateZone"`
}

type vpcXML struct {
	VPCRegion string `xml:"VPCRegion"`
	VPCId     string `xml:"VPCId"`
}

type changeInfoXML struct {
	ID          string `xml:"Id"`
	Status      string `xml:"Status"`
	SubmittedAt string `xml:"SubmittedAt"`
	Comment     string `xml:"Comment,omitempty"`
}

func (z *zone) xml() hostedZoneXML {
	return hostedZoneXML{
		ID: "/hostedzone/" + z.ID, Name: z.Name, CallerReference: z.CallerReference,
		Config:                 hostedZoneConfigXML{Comment: z.Comment, PrivateZone: true},
		ResourceRecordSetCount: len(z.records),
	}
}

func (z *zone) vpcsXML() []vpcXML {
	out := make([]vpcXML, 0, len(z.VPCs))
	for _, v := range z.VPCs {
		out = append(out, vpcXML{VPCRegion: v.Region, VPCId: v.ID})
	}
	return out
}

// lookup returns the zone with id. The caller holds s.mu.
func (s *Server) lookup(id string) (*zone, *apiError) {
	if z, ok := s.zones[id]; ok {
		return z, nil
	}
	return nil, noSuchHostedZone(id)
}

// record keeps a change for GetChange and returns its ChangeInfo. The caller holds s.mu.
func (s *Server) record(comment string) changeInfoXML {
	c := change{id: s.newID("C"), submittedAt: s.now().UTC(), comment: comment}
	s.changes[c.id] = c
	s.changeIDs = append(s.changeIDs, c.id)
	if len(s.changeIDs) > maxChanges {
		delete(s.changes, s.changeIDs[0])
		s.changeIDs = slices.Delete(s.changeIDs, 0, 1)
	}
	return c.xml()
}

func (c change) xml() changeInfoXML {
	return changeInfoXML{ID: "/change/" + c.id, Status: "INSYNC", SubmittedAt: c.submittedAt.Format(timeFormat), Comment: c.comment}
}

// vpc checks a VPC from a request. The caller holds s.mu.
func (s *Server) vpc(v *vpcXML) (VPC, *apiError) {
	if v == nil || v.VPCId == "" {
		return VPC{}, invalidInput("Invalid request: a VPC with a VPCId and VPCRegion is required")
	}
	if v.VPCRegion == "" {
		return VPC{}, invalidInput("Invalid request: the VPC %s needs a VPCRegion", v.VPCId)
	}
	if s.vpcs != nil && !s.vpcs(v.VPCRegion, v.VPCId) {
		return VPC{}, invalidVPCID(v.VPCId)
	}
	return VPC{Region: v.VPCRegion, ID: v.VPCId}, nil
}

// associated returns the other zone named name that v is associated with. The caller holds s.mu.
func (s *Server) associated(name string, v VPC) (*zone, bool) {
	for _, z := range s.zones {
		if z.Name == name && slices.Contains(z.VPCs, v) {
			return z, true
		}
	}
	return nil, false
}

type createHostedZoneRequest struct {
	Name             string  `xml:"Name"`
	VPC              *vpcXML `xml:"VPC"`
	CallerReference  string  `xml:"CallerReference"`
	HostedZoneConfig *struct {
		Comment     string `xml:"Comment"`
		PrivateZone bool   `xml:"PrivateZone"`
	} `xml:"HostedZoneConfig"`
	DelegationSetID string `xml:"DelegationSetId"`
}

// createTarget names the zone a CreateHostedZone creates.
func createTarget(c *call) string {
	var in createHostedZoneRequest
	//nolint:gosec // the body is bounded by maxBody
	if xml.Unmarshal(c.body, &in) != nil {
		return ""
	}
	if name, ok := canonicalName(in.Name); ok {
		return name
	}
	return in.Name
}

func (s *Server) createHostedZone(c *call) (*reply, *apiError) {
	var in createHostedZoneRequest
	if err := decodeXML(c.body, &in); err != nil {
		return nil, err
	}
	if in.CallerReference == "" || utf8.RuneCountInString(in.CallerReference) > 128 {
		return nil, invalidInput("Invalid request: CallerReference must be 1 to 128 characters")
	}
	name, ok := canonicalName(in.Name)
	if !ok {
		return nil, invalidDomainName(in.Name)
	}
	if in.DelegationSetID != "" {
		return nil, invalidInput("squall: delegation sets are not supported")
	}
	if in.VPC == nil {
		return nil, invalidInput("squall: only private hosted zones, created with a VPC, are supported")
	}
	comment := ""
	if in.HostedZoneConfig != nil {
		comment = in.HostedZoneConfig.Comment
	}
	if utf8.RuneCountInString(comment) > 256 {
		return nil, invalidInput("Invalid request: a comment is up to 256 characters")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	v, err := s.vpc(in.VPC)
	if err != nil {
		return nil, err
	}
	if _, ok := s.references[in.CallerReference]; ok {
		return nil, clientError(http.StatusConflict, "HostedZoneAlreadyExists",
			"A hosted zone has already been created with the specified caller reference.")
	}
	if other, ok := s.associated(name, v); ok {
		return nil, conflictingDomain(v, other.ID)
	}
	z := &zone{
		ID: s.newID("Z"), Name: name, CallerReference: in.CallerReference, Comment: comment, VPCs: []VPC{v},
		records: map[recordKey]*RecordSet{},
	}
	for _, rs := range defaultRecordSets(name) {
		z.records[rs.key()] = rs
	}
	s.zones[z.ID] = z
	s.references[in.CallerReference] = z.ID
	location := "https://" + c.r.Host + prefix + "hostedzone/" + z.ID
	if c.r.TLS == nil {
		location = "http://" + c.r.Host + prefix + "hostedzone/" + z.ID
	}
	return &reply{
		status: http.StatusCreated,
		header: map[string]string{"Location": location},
		body: struct {
			XMLName    xml.Name      `xml:"https://route53.amazonaws.com/doc/2013-04-01/ CreateHostedZoneResponse"`
			HostedZone hostedZoneXML `xml:"HostedZone"`
			ChangeInfo changeInfoXML `xml:"ChangeInfo"`
			VPC        vpcXML        `xml:"VPC"`
		}{HostedZone: z.xml(), ChangeInfo: s.record(""), VPC: vpcXML{VPCRegion: v.Region, VPCId: v.ID}},
	}, nil
}

func (s *Server) getHostedZone(c *call) (*reply, *apiError) {
	s.mu.Lock()
	defer s.mu.Unlock()
	z, err := s.lookup(c.id)
	if err != nil {
		return nil, err
	}
	return &reply{body: struct {
		XMLName    xml.Name      `xml:"https://route53.amazonaws.com/doc/2013-04-01/ GetHostedZoneResponse"`
		HostedZone hostedZoneXML `xml:"HostedZone"`
		VPCs       []vpcXML      `xml:"VPCs>VPC"`
	}{HostedZone: z.xml(), VPCs: z.vpcsXML()}}, nil
}

// sortedZones returns the zones in the order ListHostedZonesByName lists them. The caller holds s.mu.
func (s *Server) sortedZones() []*zone {
	return slices.SortedFunc(maps.Values(s.zones), func(a, b *zone) int {
		if c := compareNames(a.Name, b.Name); c != 0 {
			return c
		}
		return strings.Compare(a.ID, b.ID)
	})
}

// maxItems reads the maxitems parameter, which defaults to and is capped at limit.
func maxItems(c *call, limit int) (int, *apiError) {
	v := c.query.Get("maxitems")
	if v == "" {
		return limit, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 1 {
		return 0, invalidInput("Invalid request: maxitems must be a positive integer, not %q", v)
	}
	return min(n, limit), nil
}

func (s *Server) listHostedZonesByName(c *call) (*reply, *apiError) {
	limit, err := maxItems(c, maxZonesPage)
	if err != nil {
		return nil, err
	}
	dnsName, zoneID := c.query.Get("dnsname"), c.query.Get("hostedzoneid")
	if zoneID != "" && dnsName == "" {
		return nil, invalidInput("Invalid request: hostedzoneid requires dnsname")
	}
	start := ""
	if dnsName != "" {
		var ok bool
		if start, ok = canonicalName(dnsName); !ok {
			return nil, invalidDomainName(dnsName)
		}
	}
	type page struct {
		XMLName          xml.Name        `xml:"https://route53.amazonaws.com/doc/2013-04-01/ ListHostedZonesByNameResponse"`
		HostedZones      []hostedZoneXML `xml:"HostedZones>HostedZone"`
		DNSName          string          `xml:"DNSName,omitempty"`
		HostedZoneID     string          `xml:"HostedZoneId,omitempty"`
		IsTruncated      bool            `xml:"IsTruncated"`
		NextDNSName      string          `xml:"NextDNSName,omitempty"`
		NextHostedZoneID string          `xml:"NextHostedZoneId,omitempty"`
		MaxItems         string          `xml:"MaxItems"`
	}
	out := page{DNSName: dnsName, HostedZoneID: zoneID, MaxItems: strconv.Itoa(limit), HostedZones: []hostedZoneXML{}}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, z := range s.sortedZones() {
		if start != "" {
			c := compareNames(z.Name, start)
			if c < 0 || c == 0 && zoneID != "" && z.ID < zoneID {
				continue
			}
		}
		if len(out.HostedZones) == limit {
			out.IsTruncated, out.NextDNSName, out.NextHostedZoneID = true, z.Name, z.ID
			break
		}
		out.HostedZones = append(out.HostedZones, z.xml())
	}
	return &reply{body: out}, nil
}

func (s *Server) deleteHostedZone(c *call) (*reply, *apiError) {
	s.mu.Lock()
	defer s.mu.Unlock()
	z, err := s.lookup(c.id)
	if err != nil {
		return nil, err
	}
	for k := range z.records {
		if k.name != z.Name || k.typ != "SOA" && k.typ != "NS" {
			return nil, clientError(http.StatusBadRequest, "HostedZoneNotEmpty",
				"The specified hosted zone contains non-required resource record sets and so cannot be deleted.")
		}
	}
	delete(s.zones, z.ID)
	return &reply{body: struct {
		XMLName    xml.Name      `xml:"https://route53.amazonaws.com/doc/2013-04-01/ DeleteHostedZoneResponse"`
		ChangeInfo changeInfoXML `xml:"ChangeInfo"`
	}{ChangeInfo: s.record("")}}, nil
}

type vpcRequest struct {
	VPC     *vpcXML `xml:"VPC"`
	Comment string  `xml:"Comment"`
}

type changeInfoResponse struct {
	XMLName    xml.Name
	ChangeInfo changeInfoXML `xml:"ChangeInfo"`
}

func changeInfoReply(op string, info changeInfoXML) *reply {
	return &reply{body: changeInfoResponse{XMLName: xml.Name{Space: namespace, Local: op + "Response"}, ChangeInfo: info}}
}

func (s *Server) associateVPC(c *call) (*reply, *apiError) {
	var in vpcRequest
	if err := decodeXML(c.body, &in); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	z, err := s.lookup(c.id)
	if err != nil {
		return nil, err
	}
	v, err := s.vpc(in.VPC)
	if err != nil {
		return nil, err
	}
	if other, ok := s.associated(z.Name, v); ok {
		return nil, conflictingDomain(v, other.ID)
	}
	z.VPCs = append(z.VPCs, v)
	return changeInfoReply("AssociateVPCWithHostedZone", s.record(in.Comment)), nil
}

func (s *Server) disassociateVPC(c *call) (*reply, *apiError) {
	var in vpcRequest
	if err := decodeXML(c.body, &in); err != nil {
		return nil, err
	}
	if in.VPC == nil || in.VPC.VPCId == "" || in.VPC.VPCRegion == "" {
		return nil, invalidInput("Invalid request: a VPC with a VPCId and VPCRegion is required")
	}
	v := VPC{Region: in.VPC.VPCRegion, ID: in.VPC.VPCId}
	s.mu.Lock()
	defer s.mu.Unlock()
	z, err := s.lookup(c.id)
	if err != nil {
		return nil, err
	}
	i := slices.Index(z.VPCs, v)
	if i < 0 {
		return nil, clientError(http.StatusNotFound, "VPCAssociationNotFound",
			"The specified VPC %s and hosted zone %s are not currently associated.", v.ID, z.ID)
	}
	if len(z.VPCs) == 1 {
		return nil, clientError(http.StatusBadRequest, "LastVPCAssociation",
			"The VPC %s is the last VPC associated with the private hosted zone %s and cannot be disassociated.", v.ID, z.ID)
	}
	z.VPCs = slices.Delete(z.VPCs, i, i+1)
	return changeInfoReply("DisassociateVPCFromHostedZone", s.record(in.Comment)), nil
}

func (s *Server) getChange(c *call) (*reply, *apiError) {
	s.mu.Lock()
	defer s.mu.Unlock()
	ch, ok := s.changes[c.id]
	if !ok {
		return nil, clientError(http.StatusNotFound, "NoSuchChange", "A change with the specified change ID does not exist: %s", c.id)
	}
	return changeInfoReply("GetChange", ch.xml()), nil
}

type tagXML struct {
	Key   string `xml:"Key"`
	Value string `xml:"Value"`
}

// taggedZone returns the hosted zone a tags call names. The caller holds s.mu.
func (s *Server) taggedZone(c *call) (*zone, *apiError) {
	switch c.resourceType {
	case "hostedzone":
		return s.lookup(c.id)
	case "healthcheck":
		return nil, clientError(http.StatusNotFound, "NoSuchHealthCheck", "No health check exists with the specified ID %s.", c.id)
	}
	return nil, invalidInput("Invalid request: %q is not a resource type, which must be healthcheck or hostedzone", c.resourceType)
}

func (s *Server) changeTags(c *call) (*reply, *apiError) {
	var in struct {
		AddTags       []tagXML `xml:"AddTags>Tag"`
		RemoveTagKeys []string `xml:"RemoveTagKeys>Key"`
	}
	if err := decodeXML(c.body, &in); err != nil {
		return nil, err
	}
	if len(in.AddTags) > maxTagChange || len(in.RemoveTagKeys) > maxTagChange {
		return nil, invalidInput("Invalid request: a call adds and removes up to %d tags each", maxTagChange)
	}
	for _, t := range in.AddTags {
		if n := utf8.RuneCountInString(t.Key); n < 1 || n > 128 || utf8.RuneCountInString(t.Value) > 256 {
			return nil, invalidInput("Invalid request: tag keys are 1 to 128 characters and values up to 256")
		}
		if strings.HasPrefix(strings.ToLower(t.Key), "aws:") {
			return nil, invalidInput("Invalid request: tag keys may not start with aws:")
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	z, err := s.taggedZone(c)
	if err != nil {
		return nil, err
	}
	tags := maps.Clone(z.Tags)
	if tags == nil {
		tags = map[string]string{}
	}
	for _, t := range in.AddTags {
		tags[t.Key] = t.Value
	}
	for _, k := range in.RemoveTagKeys {
		delete(tags, k)
	}
	if len(tags) > maxTags {
		return nil, invalidInput("Invalid request: a resource has up to %d tags", maxTags)
	}
	z.Tags = tags
	if len(tags) == 0 {
		z.Tags = nil
	}
	return &reply{body: struct {
		XMLName xml.Name `xml:"https://route53.amazonaws.com/doc/2013-04-01/ ChangeTagsForResourceResponse"`
	}{}}, nil
}

func (s *Server) listTags(c *call) (*reply, *apiError) {
	s.mu.Lock()
	defer s.mu.Unlock()
	z, err := s.taggedZone(c)
	if err != nil {
		return nil, err
	}
	tags := []tagXML{}
	for _, k := range slices.Sorted(maps.Keys(z.Tags)) {
		tags = append(tags, tagXML{Key: k, Value: z.Tags[k]})
	}
	type tagSet struct {
		ResourceType string   `xml:"ResourceType"`
		ResourceID   string   `xml:"ResourceId"`
		Tags         []tagXML `xml:"Tags>Tag"`
	}
	return &reply{body: struct {
		XMLName        xml.Name `xml:"https://route53.amazonaws.com/doc/2013-04-01/ ListTagsForResourceResponse"`
		ResourceTagSet tagSet   `xml:"ResourceTagSet"`
	}{ResourceTagSet: tagSet{ResourceType: c.resourceType, ResourceID: z.ID, Tags: tags}}}, nil
}

// HostedZones returns every hosted zone, in the order ListHostedZonesByName lists them.
func (s *Server) HostedZones() []HostedZone {
	s.mu.Lock()
	defer s.mu.Unlock()
	zones := s.sortedZones()
	out := make([]HostedZone, 0, len(zones))
	for _, z := range zones {
		out = append(out, z.clone())
	}
	return out
}

// LookupHostedZone returns the hosted zone with id, which may carry the "/hostedzone/" prefix.
func (s *Server) LookupHostedZone(id string) (HostedZone, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	z, ok := s.zones[strings.TrimPrefix(id, "/hostedzone/")]
	if !ok {
		return HostedZone{}, false
	}
	return z.clone(), true
}
