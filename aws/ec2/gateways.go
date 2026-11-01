package ec2

import (
	"maps"
	"time"
)

const (
	stateDeleting = "deleting"
	stateDeleted  = "deleted"
	stateFailed   = "failed"

	connectivityPublic  = "public"
	connectivityPrivate = "private"
	interfaceTypeNAT    = "nat_gateway"
)

// InternetGateway is an internet gateway.
type InternetGateway struct {
	// Region is where the gateway is. The server sets it.
	Region            string
	InternetGatewayID string
	// VPCID is the VPC the gateway is attached to, if any.
	VPCID string
	Tags  map[string]string
}

func (igw *InternetGateway) clone() InternetGateway {
	c := *igw
	c.Tags = maps.Clone(igw.Tags)
	return c
}

type gatewayAttachmentXML struct {
	VPCID string `xml:"vpcId"`
	State string `xml:"state"`
}

type internetGatewayXML struct {
	InternetGatewayID string                 `xml:"internetGatewayId"`
	Attachments       []gatewayAttachmentXML `xml:"attachmentSet>item"`
	Tags              []tagXML               `xml:"tagSet>item,omitempty"`
}

// xml renders igw. An attached gateway's attachment reads "available", as in EC2.
func (igw *InternetGateway) xml() internetGatewayXML {
	out := internetGatewayXML{InternetGatewayID: igw.InternetGatewayID, Tags: tagSet(igw.Tags)}
	if igw.VPCID != "" {
		out.Attachments = []gatewayAttachmentXML{{igw.VPCID, statusAvailable}}
	}
	return out
}

func internetGatewayFields(_ *regionState, igw *InternetGateway) func(string) ([]string, bool) {
	return func(name string) ([]string, bool) {
		switch name {
		case "internet-gateway-id":
			return one(igw.InternetGatewayID), true
		case "attachment.vpc-id":
			return one(igw.VPCID), true
		case "attachment.state":
			if igw.VPCID == "" {
				return nil, true
			}
			return []string{statusAvailable}, true
		}
		return tagFilter(igw.Tags, name)
	}
}

func internetGatewayNotFound(id string) *apiError {
	return clientError("InvalidInternetGatewayID.NotFound", "The internetGateway ID '%s' does not exist", id)
}

// internetGateway returns the internet gateway with id. The caller holds s.mu.
func (rs *regionState) internetGateway(id string) (*InternetGateway, *apiError) {
	if igw, ok := rs.internetGateways[id]; ok {
		return igw, nil
	}
	return nil, internetGatewayNotFound(id)
}

// gatewayOf returns the internet gateway attached to the VPC with id. The caller holds s.mu.
func (rs *regionState) gatewayOf(vpcID string) *InternetGateway {
	for _, igw := range rs.internetGateways {
		if igw.VPCID == vpcID {
			return igw
		}
	}
	return nil
}

type createInternetGatewayResponse struct {
	meta
	InternetGateway internetGatewayXML `xml:"internetGateway"`
}

func (s *Server) createInternetGateway(c *call) (response, *apiError) {
	tags, err := c.p.tags("internet-gateway")
	if err != nil {
		return nil, err
	}
	if err := c.p.done(); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	igw := &InternetGateway{Region: c.region, InternetGatewayID: c.id, Tags: tags}
	s.region(c.region).internetGateways[igw.InternetGatewayID] = igw
	return &createInternetGatewayResponse{InternetGateway: igw.xml()}, nil
}

// readGatewayAndVPC reads the InternetGatewayId and VpcId of an attach or detach call.
func readGatewayAndVPC(p *params) (string, string, *apiError) {
	id, err := p.required("InternetGatewayId")
	if err != nil {
		return "", "", err
	}
	vpcID, err := p.required("VpcId")
	if err != nil {
		return "", "", err
	}
	return id, vpcID, p.done()
}

func (s *Server) attachInternetGateway(c *call) (response, *apiError) {
	id, vpcID, err := readGatewayAndVPC(c.p)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	rs := s.lookupRegion(c.region)
	igw, err := rs.internetGateway(id)
	if err != nil {
		return nil, err
	}
	if _, err := rs.vpc(vpcID); err != nil {
		return nil, err
	}
	if igw.VPCID != "" {
		return nil, clientError("Resource.AlreadyAssociated", "resource %s is already attached to network %s", id, igw.VPCID)
	}
	if rs.gatewayOf(vpcID) != nil {
		return nil, clientError("Resource.AlreadyAssociated", "Network %s already has an internet gateway attached", vpcID)
	}
	igw.VPCID = vpcID
	return &returnResponse{Return: true}, nil
}

func (s *Server) detachInternetGateway(c *call) (response, *apiError) {
	id, vpcID, err := readGatewayAndVPC(c.p)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	rs := s.lookupRegion(c.region)
	igw, err := rs.internetGateway(id)
	if err != nil {
		return nil, err
	}
	if _, err := rs.vpc(vpcID); err != nil {
		return nil, err
	}
	if igw.VPCID != vpcID {
		return nil, clientError("Gateway.NotAttached", "resource %s is not attached to network %s", id, vpcID)
	}
	if rs.hasPublicAddresses(vpcID) {
		return nil, clientError("DependencyViolation",
			"Network %s has some mapped public address(es). Please unmap those public address(es) before detaching the gateway.", vpcID)
	}
	igw.VPCID = ""
	return &returnResponse{Return: true}, nil
}

// hasPublicAddresses reports whether an interface in the VPC with id has a
// public address, an Elastic IP address or one EC2 assigned. The caller holds s.mu.
func (rs *regionState) hasPublicAddresses(vpcID string) bool {
	public := rs.publicAddresses()
	for id, ni := range rs.interfaces {
		if sub, ok := rs.subnets[ni.SubnetID]; ok && sub.VPCID == vpcID && public[id] != nil {
			return true
		}
	}
	return false
}

type describeInternetGatewaysResponse struct {
	meta
	InternetGateways []internetGatewayXML `xml:"internetGatewaySet>item"`
	NextToken        string               `xml:"nextToken,omitempty"`
}

func (s *Server) describeInternetGateways(c *call) (response, *apiError) {
	gateways, next, err := describe(s, c, lister[*InternetGateway]{
		ids: "InternetGatewayId", prefix: "igw", of: internetGatewaysOf,
		empty: &InternetGateway{}, fields: internetGatewayFields, notFound: internetGatewayNotFound,
	}, func(_ *regionState, igw *InternetGateway) internetGatewayXML { return igw.xml() })
	if err != nil {
		return nil, err
	}
	return &describeInternetGatewaysResponse{InternetGateways: gateways, NextToken: next}, nil
}

func (s *Server) deleteInternetGateway(c *call) (response, *apiError) {
	id, err := c.p.required("InternetGatewayId")
	if err != nil {
		return nil, err
	}
	if err := c.p.done(); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	rs := s.lookupRegion(c.region)
	igw, err := rs.internetGateway(id)
	if err != nil {
		return nil, err
	}
	if igw.VPCID != "" {
		return nil, dependencyViolation("internetGateway", id)
	}
	delete(rs.internetGateways, id)
	return &returnResponse{Return: true}, nil
}

// NATGateway is a NAT gateway.
type NATGateway struct {
	// Region is where the gateway is. The server sets it.
	Region       string
	NATGatewayID string
	SubnetID     string
	VPCID        string
	// ConnectivityType is "public" or "private".
	ConnectivityType string
	// State is "pending", "available", "deleting", "deleted" or "failed".
	State string
	// AllocationID is the Elastic IP address of a public gateway.
	AllocationID string
	// NetworkInterfaceID, PrivateIPAddress and PublicIP are the gateway's
	// interface and addresses, until it is deleted.
	NetworkInterfaceID string
	PrivateIPAddress   string
	PublicIP           string
	// FailureCode and FailureMessage say why a failed gateway failed.
	FailureCode    string
	FailureMessage string
	CreateTime     time.Time
	DeleteTime     time.Time
	Tags           map[string]string

	wait        int
	failCode    string
	failMessage string
}

func (nat *NATGateway) clone() NATGateway {
	c := *nat
	c.Tags = maps.Clone(nat.Tags)
	return c
}

func (nat *NATGateway) transitional() bool {
	return nat.State == statePending || nat.State == stateDeleting
}

// gone reports whether the gateway no longer routes traffic.
func (nat *NATGateway) gone() bool {
	return nat.State == stateDeleting || nat.State == stateDeleted || nat.State == stateFailed
}

// settleNAT completes the gateway's creation or deletion: a pending gateway
// becomes available or fails, and a deleting one lets go of its interface
// and address. The caller holds s.mu.
func (s *Server) settleNAT(rs *regionState, nat *NATGateway) {
	switch nat.State {
	case statePending:
		nat.State = statusAvailable
		if nat.failCode != "" {
			nat.State, nat.FailureCode, nat.FailureMessage = stateFailed, nat.failCode, nat.failMessage
		}
	case stateDeleting:
		nat.State = stateDeleted
		if ni, ok := rs.interfaces[nat.NetworkInterfaceID]; ok {
			s.removeInterface(rs, ni)
		}
		nat.NetworkInterfaceID, nat.PrivateIPAddress, nat.PublicIP = "", "", ""
	}
	nat.wait = 0
}

// advanceNAT counts a describe of nat, settling it once enough have seen it
// in a transitional state. The caller holds s.mu.
func (s *Server) advanceNAT(rs *regionState, nat *NATGateway) {
	if !nat.transitional() || s.settleAfter < 0 {
		return
	}
	if nat.wait > 0 {
		nat.wait--
		return
	}
	s.settleNAT(rs, nat)
}

type natGatewayAddressXML struct {
	AllocationID       string `xml:"allocationId,omitempty"`
	NetworkInterfaceID string `xml:"networkInterfaceId,omitempty"`
	PrivateIP          string `xml:"privateIp,omitempty"`
	PublicIP           string `xml:"publicIp,omitempty"`
	AssociationID      string `xml:"associationId,omitempty"`
	IsPrimary          bool   `xml:"isPrimary"`
	Status             string `xml:"status"`
}

type natGatewayXML struct {
	NATGatewayID     string                 `xml:"natGatewayId"`
	SubnetID         string                 `xml:"subnetId"`
	VPCID            string                 `xml:"vpcId"`
	State            string                 `xml:"state"`
	ConnectivityType string                 `xml:"connectivityType"`
	CreateTime       string                 `xml:"createTime"`
	DeleteTime       string                 `xml:"deleteTime,omitempty"`
	FailureCode      string                 `xml:"failureCode,omitempty"`
	FailureMessage   string                 `xml:"failureMessage,omitempty"`
	Addresses        []natGatewayAddressXML `xml:"natGatewayAddressSet>item"`
	Tags             []tagXML               `xml:"tagSet>item,omitempty"`
}

var natAddressStatus = map[string]string{
	statePending: "associating", statusAvailable: "succeeded", stateDeleting: "disassociating", stateDeleted: "succeeded", stateFailed: stateFailed,
}

func (rs *regionState) natGatewayXML(nat *NATGateway) natGatewayXML {
	out := natGatewayXML{
		NATGatewayID: nat.NATGatewayID, SubnetID: nat.SubnetID, VPCID: nat.VPCID, State: nat.State,
		ConnectivityType: nat.ConnectivityType, CreateTime: nat.CreateTime.UTC().Format(timeFormat),
		FailureCode: nat.FailureCode, FailureMessage: nat.FailureMessage, Tags: tagSet(nat.Tags),
	}
	if !nat.DeleteTime.IsZero() {
		out.DeleteTime = nat.DeleteTime.UTC().Format(timeFormat)
	}
	address := natGatewayAddressXML{
		AllocationID: nat.AllocationID, NetworkInterfaceID: nat.NetworkInterfaceID, PrivateIP: nat.PrivateIPAddress,
		PublicIP: nat.PublicIP, IsPrimary: true, Status: natAddressStatus[nat.State],
	}
	if a := rs.addressOn(nat.NetworkInterfaceID); a != nil && nat.NetworkInterfaceID != "" {
		address.AssociationID = a.AssociationID
	}
	out.Addresses = []natGatewayAddressXML{address}
	return out
}

func natGatewayFields(_ *regionState, nat *NATGateway) func(string) ([]string, bool) {
	return func(name string) ([]string, bool) {
		switch name {
		case "nat-gateway-id":
			return one(nat.NATGatewayID), true
		case "subnet-id":
			return one(nat.SubnetID), true
		case "vpc-id":
			return one(nat.VPCID), true
		case "state":
			return one(nat.State), true
		}
		return tagFilter(nat.Tags, name)
	}
}

func natGatewayNotFound(id string) *apiError {
	return clientError("NatGatewayNotFound", "The NAT gateway '%s' was not found", id)
}

// natGateway returns the NAT gateway with id. The caller holds s.mu.
func (rs *regionState) natGateway(id string) (*NATGateway, *apiError) {
	if nat, ok := rs.natGateways[id]; ok {
		return nat, nil
	}
	return nil, natGatewayNotFound(id)
}

type createNATGatewayResponse struct {
	meta
	NATGateway  natGatewayXML `xml:"natGateway"`
	ClientToken string        `xml:"clientToken,omitempty"`
}

func (s *Server) createNATGateway(c *call) (response, *apiError) {
	subnetID, err := c.p.required("SubnetId")
	if err != nil {
		return nil, err
	}
	connectivity, err := c.p.enum("ConnectivityType", connectivityPublic, connectivityPublic, connectivityPrivate)
	if err != nil {
		return nil, err
	}
	allocationID, privateIP := c.p.get("AllocationId"), c.p.get("PrivateIpAddress")
	switch {
	case connectivity == connectivityPublic && allocationID == "":
		return nil, missing("AllocationId")
	case connectivity == connectivityPrivate && allocationID != "":
		return nil, clientError("InvalidParameterCombination", "An Elastic IP address cannot be specified for a private NAT gateway.")
	}
	tags, err := c.p.tags("natgateway")
	if err != nil {
		return nil, err
	}
	token := c.p.get("ClientToken")
	if err := c.p.done(); err != nil {
		return nil, err
	}
	fingerprint := c.p.fingerprint()
	s.mu.Lock()
	defer s.mu.Unlock()
	t, err := s.replay(c.region, "natgateways", token, fingerprint)
	if err != nil {
		return nil, err
	}
	if t != nil {
		rs := s.lookupRegion(c.region)
		if nat, ok := rs.natGateways[t.id]; ok {
			return &createNATGatewayResponse{NATGateway: rs.natGatewayXML(nat), ClientToken: token}, nil
		}
		return &createNATGatewayResponse{NATGateway: t.created.(natGatewayXML), ClientToken: token}, nil
	}
	rs := s.lookupRegion(c.region)
	sub, err := rs.subnet(subnetID)
	if err != nil {
		return nil, err
	}
	var a *Address
	if allocationID != "" {
		var ok bool
		if a, ok = rs.addresses[allocationID]; !ok {
			return nil, allocationNotFound(allocationID)
		}
	}
	nat := &NATGateway{
		Region: c.region, NATGatewayID: c.id, SubnetID: sub.SubnetID, VPCID: sub.VPCID, ConnectivityType: connectivity,
		State: statePending, AllocationID: allocationID, CreateTime: time.Now(), Tags: tags, wait: max(s.settleAfter, 0),
	}
	switch {
	case a != nil && a.AssociationID != "":
		nat.failCode, nat.failMessage = "Resource.AlreadyAssociated", "Elastic IP address ["+allocationID+"] is already associated"
	case connectivity == connectivityPublic && rs.gatewayOf(sub.VPCID) == nil:
		nat.failCode, nat.failMessage = "Gateway.NotAttached", "Network "+sub.VPCID+" has no Internet gateway attached"
	default:
		ni, err := s.storeInterface(c.region, NetworkInterface{
			SubnetID: sub.SubnetID, Description: "Interface for NAT Gateway " + nat.NATGatewayID,
			Status: statusInUse, PrivateIPAddress: privateIP,
		})
		if err != nil {
			return nil, err
		}
		ni.InterfaceType = interfaceTypeNAT
		nat.NetworkInterfaceID, nat.PrivateIPAddress = ni.NetworkInterfaceID, ni.PrivateIPAddress
		if a != nil {
			s.associate(rs, a, ni, "")
			nat.PublicIP = a.PublicIP
		}
	}
	rs = s.region(c.region)
	rs.natGateways[nat.NATGatewayID] = nat
	created := rs.natGatewayXML(nat)
	s.remember(c.region, "natgateways", token, fingerprint, nat.NATGatewayID, created)
	return &createNATGatewayResponse{NATGateway: created, ClientToken: token}, nil
}

type describeNATGatewaysResponse struct {
	meta
	NATGateways []natGatewayXML `xml:"natGatewaySet>item"`
	NextToken   string          `xml:"nextToken,omitempty"`
}

func (s *Server) describeNATGateways(c *call) (response, *apiError) {
	gateways, next, err := describe(s, c, lister[*NATGateway]{
		ids: "NatGatewayId", prefix: "nat", of: natGatewaysOf,
		empty: &NATGateway{}, fields: natGatewayFields, notFound: natGatewayNotFound,
		advance: s.advanceNAT,
	}, (*regionState).natGatewayXML)
	if err != nil {
		return nil, err
	}
	return &describeNATGatewaysResponse{NATGateways: gateways, NextToken: next}, nil
}

type deleteNATGatewayResponse struct {
	meta
	NATGatewayID string `xml:"natGatewayId"`
}

func (s *Server) deleteNATGateway(c *call) (response, *apiError) {
	id, err := c.p.required("NatGatewayId")
	if err != nil {
		return nil, err
	}
	if err := c.p.done(); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	nat, err := s.lookupRegion(c.region).natGateway(id)
	if err != nil {
		return nil, err
	}
	if nat.State != stateDeleting && nat.State != stateDeleted {
		nat.State, nat.DeleteTime, nat.wait = stateDeleting, time.Now(), max(s.settleAfter, 0)
	}
	return &deleteNATGatewayResponse{NATGatewayID: id}, nil
}

// InternetGateways returns every internet gateway the server holds, ordered
// by region and ID.
func (s *Server) InternetGateways() []InternetGateway {
	return collect(s, internetGatewaysOf, (*InternetGateway).clone)
}

// NATGateways returns every NAT gateway the server holds, deleted ones
// included, ordered by region and ID.
func (s *Server) NATGateways() []NATGateway {
	return collect(s, natGatewaysOf, (*NATGateway).clone)
}
