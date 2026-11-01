package ec2

import (
	"maps"
	"slices"
	"strings"
	"time"
)

const (
	optionEnable  = "enable"
	optionDisable = "disable"
)

// TransitGateway is a transit gateway. The server does not create them:
// [Server.PutTransitGateway] sets one up for VPCs to attach to.
type TransitGateway struct {
	// Region is where the gateway is. The server sets it.
	Region           string
	TransitGatewayID string
	Description      string
	// State is "available" unless set.
	State string
	Tags  map[string]string
}

func (tgw *TransitGateway) clone() TransitGateway {
	c := *tgw
	c.Tags = maps.Clone(tgw.Tags)
	return c
}

// TransitGatewayAttachment attaches a VPC to a transit gateway.
type TransitGatewayAttachment struct {
	// Region is where the attachment is. The server sets it.
	Region                     string
	TransitGatewayAttachmentID string
	TransitGatewayID           string
	VPCID                      string
	SubnetIDs                  []string
	// State is "pending", "available", "deleting" or "deleted".
	State string
	// Options are "enable" or "disable", as created.
	DNSSupport                      string
	IPv6Support                     string
	ApplianceModeSupport            string
	SecurityGroupReferencingSupport string
	CreationTime                    time.Time
	Tags                            map[string]string

	wait int
}

func (a *TransitGatewayAttachment) clone() TransitGatewayAttachment {
	c := *a
	c.SubnetIDs = slices.Clone(a.SubnetIDs)
	c.Tags = maps.Clone(a.Tags)
	return c
}

// settle completes the attachment's creation or deletion.
func (a *TransitGatewayAttachment) settle() {
	switch a.State {
	case statePending:
		a.State = statusAvailable
	case stateDeleting:
		a.State = stateDeleted
	}
	a.wait = 0
}

// advanceAttachment counts a describe of a, settling it once enough have seen
// it in a transitional state. The caller holds s.mu.
func (s *Server) advanceAttachment(_ *regionState, a *TransitGatewayAttachment) {
	if a.State != statePending && a.State != stateDeleting || s.settleAfter < 0 {
		return
	}
	if a.wait > 0 {
		a.wait--
		return
	}
	a.settle()
}

type tgwVPCAttachmentOptionsXML struct {
	DNSSupport                      string `xml:"dnsSupport"`
	IPv6Support                     string `xml:"ipv6Support"`
	ApplianceModeSupport            string `xml:"applianceModeSupport"`
	SecurityGroupReferencingSupport string `xml:"securityGroupReferencingSupport"`
}

type tgwVPCAttachmentXML struct {
	TransitGatewayAttachmentID string                     `xml:"transitGatewayAttachmentId"`
	TransitGatewayID           string                     `xml:"transitGatewayId"`
	VPCID                      string                     `xml:"vpcId"`
	State                      string                     `xml:"state"`
	SubnetIDs                  []string                   `xml:"subnetIds>item"`
	CreationTime               string                     `xml:"creationTime"`
	Options                    tgwVPCAttachmentOptionsXML `xml:"options"`
	Tags                       []tagXML                   `xml:"tagSet>item,omitempty"`
}

func (a *TransitGatewayAttachment) vpcXML() tgwVPCAttachmentXML {
	return tgwVPCAttachmentXML{
		TransitGatewayAttachmentID: a.TransitGatewayAttachmentID, TransitGatewayID: a.TransitGatewayID, VPCID: a.VPCID,
		State: a.State, SubnetIDs: a.SubnetIDs, CreationTime: a.CreationTime.UTC().Format(timeFormat),
		Options: tgwVPCAttachmentOptionsXML{a.DNSSupport, a.IPv6Support, a.ApplianceModeSupport, a.SecurityGroupReferencingSupport},
		Tags:    tagSet(a.Tags),
	}
}

type tgwAttachmentXML struct {
	TransitGatewayAttachmentID string   `xml:"transitGatewayAttachmentId"`
	TransitGatewayID           string   `xml:"transitGatewayId"`
	ResourceType               string   `xml:"resourceType"`
	ResourceID                 string   `xml:"resourceId"`
	State                      string   `xml:"state"`
	CreationTime               string   `xml:"creationTime"`
	Tags                       []tagXML `xml:"tagSet>item,omitempty"`
}

func (a *TransitGatewayAttachment) xml() tgwAttachmentXML {
	return tgwAttachmentXML{
		TransitGatewayAttachmentID: a.TransitGatewayAttachmentID, TransitGatewayID: a.TransitGatewayID, ResourceType: "vpc",
		ResourceID: a.VPCID, State: a.State, CreationTime: a.CreationTime.UTC().Format(timeFormat), Tags: tagSet(a.Tags),
	}
}

func attachmentFields(_ *regionState, a *TransitGatewayAttachment) func(string) ([]string, bool) {
	return func(name string) ([]string, bool) {
		switch name {
		case "transit-gateway-attachment-id":
			return one(a.TransitGatewayAttachmentID), true
		case "transit-gateway-id":
			return one(a.TransitGatewayID), true
		case "vpc-id":
			return one(a.VPCID), true
		case "state":
			return one(a.State), true
		}
		return tagFilter(a.Tags, name)
	}
}

// genericAttachmentFields are the filters of DescribeTransitGatewayAttachments,
// which name the VPC as the attached resource.
func genericAttachmentFields(rs *regionState, a *TransitGatewayAttachment) func(string) ([]string, bool) {
	fields := attachmentFields(rs, a)
	return func(name string) ([]string, bool) {
		switch name {
		case "resource-type":
			return []string{"vpc"}, true
		case "resource-id":
			return one(a.VPCID), true
		case "vpc-id":
			return nil, false
		}
		return fields(name)
	}
}

func transitGatewayNotFound(id string) *apiError {
	return clientError("InvalidTransitGatewayID.NotFound", "Transit Gateway %s was deleted or does not exist.", id)
}

func attachmentNotFound(id string) *apiError {
	return clientError("InvalidTransitGatewayAttachmentID.NotFound", "Transit Gateway Attachment %s was deleted or does not exist.", id)
}

// checkTransitGateway fails unless the server holds the transit gateway with
// id. The caller holds s.mu.
func (rs *regionState) checkTransitGateway(id string) *apiError {
	if tgw, ok := rs.transitGateways[id]; ok && tgw.State != stateDeleted {
		return nil
	}
	return transitGatewayNotFound(id)
}

// transitGatewayAttached reports whether the VPC with vpcID has an attachment
// to the transit gateway with id that carries traffic. The caller holds s.mu.
func (rs *regionState) transitGatewayAttached(id, vpcID string) bool {
	for _, a := range rs.transitGatewayAttachments {
		if a.TransitGatewayID == id && a.VPCID == vpcID && (a.State == statePending || a.State == statusAvailable) {
			return true
		}
	}
	return false
}

type tgwVPCAttachmentResponse struct {
	meta
	Attachment tgwVPCAttachmentXML `xml:"transitGatewayVpcAttachment"`
}

func (s *Server) createTransitGatewayVPCAttachment(c *call) (response, *apiError) {
	tgwID, err := c.p.required("TransitGatewayId")
	if err != nil {
		return nil, err
	}
	vpcID, err := c.p.required("VpcId")
	if err != nil {
		return nil, err
	}
	subnetIDs := c.p.list("SubnetIds")
	if len(subnetIDs) == 0 {
		return nil, missing("SubnetIds")
	}
	a := &TransitGatewayAttachment{
		Region: c.region, TransitGatewayAttachmentID: c.id, TransitGatewayID: tgwID, VPCID: vpcID, SubnetIDs: subnetIDs,
		State: statePending, CreationTime: time.Now(), wait: max(s.settleAfter, 0),
	}
	for _, o := range []struct {
		param  string
		value  *string
		absent string
	}{
		{"Options.DnsSupport", &a.DNSSupport, optionEnable},
		{"Options.Ipv6Support", &a.IPv6Support, optionDisable},
		{"Options.ApplianceModeSupport", &a.ApplianceModeSupport, optionDisable},
		{"Options.SecurityGroupReferencingSupport", &a.SecurityGroupReferencingSupport, optionDisable},
	} {
		if *o.value, err = c.p.enum(o.param, o.absent, optionEnable, optionDisable); err != nil {
			return nil, err
		}
	}
	specs, err := c.p.tagSpecsIn("TagSpecifications", "transit-gateway-attachment")
	if err != nil {
		return nil, err
	}
	a.Tags = specs["transit-gateway-attachment"]
	if err := c.p.done(); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	rs := s.lookupRegion(c.region)
	if err := rs.checkTransitGateway(tgwID); err != nil {
		return nil, err
	}
	if _, err := rs.vpc(vpcID); err != nil {
		return nil, err
	}
	zones := map[string]bool{}
	for _, id := range subnetIDs {
		sub, err := rs.subnet(id)
		if err != nil {
			return nil, err
		}
		if sub.VPCID != vpcID {
			return nil, invalidValue("Subnet %s does not belong to %s", id, vpcID)
		}
		if zones[sub.AvailabilityZone] {
			return nil, clientError("DuplicateSubnetsInSameZone", "Duplicate Subnets for same AZ")
		}
		zones[sub.AvailabilityZone] = true
	}
	for _, other := range rs.transitGatewayAttachments {
		if other.TransitGatewayID == tgwID && other.VPCID == vpcID && other.State != stateDeleted {
			return nil, clientError("DuplicateTransitGatewayAttachment", "%s has non-deleted Transit Gateway Attachments with same VPC ID.", tgwID)
		}
	}
	rs.transitGatewayAttachments[a.TransitGatewayAttachmentID] = a
	return &tgwVPCAttachmentResponse{Attachment: a.vpcXML()}, nil
}

func attachmentLister() lister[*TransitGatewayAttachment] {
	return lister[*TransitGatewayAttachment]{
		ids: "TransitGatewayAttachmentIds", prefix: "tgw-attach",
		of:    attachmentsOf,
		empty: &TransitGatewayAttachment{}, fields: attachmentFields, notFound: attachmentNotFound,
	}
}

type describeTGWVPCAttachmentsResponse struct {
	meta
	Attachments []tgwVPCAttachmentXML `xml:"transitGatewayVpcAttachments>item"`
	NextToken   string                `xml:"nextToken,omitempty"`
}

func (s *Server) describeTransitGatewayVPCAttachments(c *call) (response, *apiError) {
	l := attachmentLister()
	l.advance = s.advanceAttachment
	attachments, next, err := describe(s, c, l, func(_ *regionState, a *TransitGatewayAttachment) tgwVPCAttachmentXML { return a.vpcXML() })
	if err != nil {
		return nil, err
	}
	return &describeTGWVPCAttachmentsResponse{Attachments: attachments, NextToken: next}, nil
}

type describeTGWAttachmentsResponse struct {
	meta
	Attachments []tgwAttachmentXML `xml:"transitGatewayAttachments>item"`
	NextToken   string             `xml:"nextToken,omitempty"`
}

func (s *Server) describeTransitGatewayAttachments(c *call) (response, *apiError) {
	l := attachmentLister()
	l.fields, l.advance = genericAttachmentFields, s.advanceAttachment
	attachments, next, err := describe(s, c, l, func(_ *regionState, a *TransitGatewayAttachment) tgwAttachmentXML { return a.xml() })
	if err != nil {
		return nil, err
	}
	return &describeTGWAttachmentsResponse{Attachments: attachments, NextToken: next}, nil
}

func (s *Server) deleteTransitGatewayVPCAttachment(c *call) (response, *apiError) {
	id, err := c.p.required("TransitGatewayAttachmentId")
	if err != nil {
		return nil, err
	}
	if err := c.p.done(); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	a, ok := s.lookupRegion(c.region).transitGatewayAttachments[id]
	if !ok {
		return nil, attachmentNotFound(id)
	}
	if a.State != stateDeleting && a.State != stateDeleted {
		a.State, a.wait = stateDeleting, max(s.settleAfter, 0)
	}
	return &tgwVPCAttachmentResponse{Attachment: a.vpcXML()}, nil
}

// PutTransitGateway stores tgw in region, replacing a gateway with the same
// ID, and returns it as stored. It fills in what tgw leaves empty, such as the
// ID and state. Use it to set up a gateway for VPCs to attach to, as one
// another account shares would be.
func (s *Server) PutTransitGateway(region string, tgw TransitGateway) (TransitGateway, error) {
	if tgw.TransitGatewayID != "" && !strings.HasPrefix(tgw.TransitGatewayID, "tgw-") {
		return TransitGateway{}, invalidValue("Invalid transit gateway ID '%s'.", tgw.TransitGatewayID)
	}
	if err := validateTags(tgw.Tags); err != nil {
		return TransitGateway{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	rs := s.region(region)
	if tgw.TransitGatewayID == "" {
		tgw.TransitGatewayID = unusedID(s, "tgw", rs.transitGateways)
	}
	if tgw.State == "" {
		tgw.State = statusAvailable
	}
	tgw.Region = region
	stored := tgw.clone()
	rs.transitGateways[tgw.TransitGatewayID] = &stored
	return stored.clone(), nil
}

// TransitGateways returns every transit gateway the server holds, ordered by
// region and ID.
func (s *Server) TransitGateways() []TransitGateway {
	return collect(s, transitGatewaysOf, (*TransitGateway).clone)
}

// TransitGatewayAttachments returns every transit gateway attachment the
// server holds, deleted ones included, ordered by region and ID.
func (s *Server) TransitGatewayAttachments() []TransitGatewayAttachment {
	return collect(s, attachmentsOf,
		(*TransitGatewayAttachment).clone)
}
