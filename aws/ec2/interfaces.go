package ec2

import (
	"encoding/binary"
	"errors"
	"fmt"
	"maps"
	"net/netip"
	"slices"
	"strings"
)

const (
	statusAvailable = "available"
	statusInUse     = "in-use"

	minResults = 5
	maxResults = 1000
)

// NetworkInterface is an elastic network interface.
type NetworkInterface struct {
	// Region is where the interface is. The server sets it.
	Region             string
	NetworkInterfaceID string
	SubnetID           string
	AvailabilityZone   string
	Description        string
	// Status is "available", or "in-use" while the interface is attached.
	Status           string
	MACAddress       string
	PrivateIPAddress string
	SecurityGroupIDs []string
	Tags             map[string]string
	// AttachmentID and InstanceID are set while the interface is attached.
	AttachmentID string
	InstanceID   string
	// DeleteOnTermination is whether terminating the instance deletes the interface.
	DeleteOnTermination bool
}

func (ni *NetworkInterface) clone() NetworkInterface {
	c := *ni
	c.SecurityGroupIDs = slices.Clone(ni.SecurityGroupIDs)
	c.Tags = maps.Clone(ni.Tags)
	return c
}

type interfaceXML struct {
	NetworkInterfaceID string          `xml:"networkInterfaceId"`
	SubnetID           string          `xml:"subnetId"`
	AvailabilityZone   string          `xml:"availabilityZone"`
	Description        string          `xml:"description"`
	RequesterManaged   bool            `xml:"requesterManaged"`
	Status             string          `xml:"status"`
	MACAddress         string          `xml:"macAddress"`
	PrivateIPAddress   string          `xml:"privateIpAddress"`
	SourceDestCheck    bool            `xml:"sourceDestCheck"`
	Groups             []groupXML      `xml:"groupSet>item,omitempty"`
	Attachment         *attachmentXML  `xml:"attachment,omitempty"`
	Association        *associationXML `xml:"association,omitempty"`
	Tags               []tagXML        `xml:"tagSet>item,omitempty"`
	PrivateIPAddresses []privateIPXML  `xml:"privateIpAddressesSet>item"`
	InterfaceType      string          `xml:"interfaceType"`
}

type groupXML struct {
	GroupID string `xml:"groupId"`
}

type attachmentXML struct {
	AttachmentID        string `xml:"attachmentId"`
	InstanceID          string `xml:"instanceId,omitempty"`
	DeviceIndex         int    `xml:"deviceIndex"`
	Status              string `xml:"status"`
	DeleteOnTermination bool   `xml:"deleteOnTermination"`
}

type associationXML struct {
	PublicIP      string `xml:"publicIp"`
	IPOwnerID     string `xml:"ipOwnerId,omitempty"`
	AllocationID  string `xml:"allocationId,omitempty"`
	AssociationID string `xml:"associationId,omitempty"`
}

type privateIPXML struct {
	PrivateIPAddress string          `xml:"privateIpAddress"`
	Primary          bool            `xml:"primary"`
	Association      *associationXML `xml:"association,omitempty"`
}

// xml renders ni with the address a associated with it, if any.
func (ni *NetworkInterface) xml(a *Address) interfaceXML {
	out := interfaceXML{
		NetworkInterfaceID: ni.NetworkInterfaceID,
		SubnetID:           ni.SubnetID,
		AvailabilityZone:   ni.AvailabilityZone,
		Description:        ni.Description,
		Status:             ni.Status,
		MACAddress:         ni.MACAddress,
		PrivateIPAddress:   ni.PrivateIPAddress,
		SourceDestCheck:    true,
		Tags:               tagSet(ni.Tags),
		InterfaceType:      "interface",
	}
	for _, g := range ni.SecurityGroupIDs {
		out.Groups = append(out.Groups, groupXML{g})
	}
	if ni.InstanceID != "" {
		out.Attachment = &attachmentXML{AttachmentID: ni.AttachmentID, InstanceID: ni.InstanceID, Status: "attached", DeleteOnTermination: ni.DeleteOnTermination}
	}
	out.Association = a.association()

	out.PrivateIPAddresses = []privateIPXML{{PrivateIPAddress: ni.PrivateIPAddress, Primary: true, Association: out.Association}}
	return out
}

// interfaceFields returns the values of ni's DescribeNetworkInterfaces
// filters, where a is the address associated with it, if any.
func interfaceFields(ni *NetworkInterface, a *Address) func(string) ([]string, bool) {
	var assoc Address
	if a != nil {
		assoc = *a
	}
	return func(name string) ([]string, bool) {
		switch name {
		case "association.allocation-id":
			return one(assoc.AllocationID), true
		case "association.association-id":
			return one(assoc.AssociationID), true
		case "association.public-ip":
			return one(assoc.PublicIP), true
		case "attachment.attachment-id":
			return one(ni.AttachmentID), true
		case "attachment.instance-id":
			return one(ni.InstanceID), true
		case "availability-zone":
			return one(ni.AvailabilityZone), true
		case "description":
			return one(ni.Description), true
		case "group-id":
			return ni.SecurityGroupIDs, true
		case "mac-address":
			return one(ni.MACAddress), true
		case "network-interface-id":
			return one(ni.NetworkInterfaceID), true
		case "private-ip-address", "addresses.private-ip-address":
			return one(ni.PrivateIPAddress), true
		case "status":
			return one(ni.Status), true
		case "subnet-id":
			return one(ni.SubnetID), true
		}
		return tagFilter(ni.Tags, name)
	}
}

func privateKey(region, subnet, ip string) string { return region + "/" + subnet + "/" + ip }

// storeInterface fills in what ni leaves empty and stores it in region,
// replacing an interface with the same ID. The caller holds s.mu.
func (s *Server) storeInterface(region string, ni NetworkInterface) (*NetworkInterface, *apiError) {
	if ni.SubnetID == "" {
		return nil, missing("SubnetId")
	}
	if err := validateTags(ni.Tags); err != nil {
		return nil, err
	}
	switch ni.Status {
	case "":
		ni.Status = statusAvailable
		if ni.InstanceID != "" {
			ni.Status = statusInUse
		}
	case statusAvailable:
		if ni.InstanceID != "" {
			return nil, invalidValue("A network interface attached to instance '%s' cannot be available.", ni.InstanceID)
		}
	case statusInUse:
	default:
		return nil, invalidValue("Invalid network interface status '%s'.", ni.Status)
	}
	if ni.NetworkInterfaceID != "" && !strings.HasPrefix(ni.NetworkInterfaceID, "eni-") {
		return nil, invalidValue("Invalid network interface ID '%s'.", ni.NetworkInterfaceID)
	}
	rs := s.region(region)
	if ni.NetworkInterfaceID == "" {
		ni.NetworkInterfaceID = unusedID(s, "eni", rs.interfaces)
	}
	if ni.PrivateIPAddress != "" {
		ip, err := netip.ParseAddr(ni.PrivateIPAddress)
		if err != nil || !ip.Is4() {
			return nil, invalidValue("Invalid private IP address '%s'.", ni.PrivateIPAddress)
		}
		ni.PrivateIPAddress = ip.String()
		if holder, taken := s.privateIPs[privateKey(region, ni.SubnetID, ni.PrivateIPAddress)]; taken && holder != ni.NetworkInterfaceID {
			return nil, clientError("InvalidIPAddress.InUse", "The specified address %s is already in use.", ni.PrivateIPAddress)
		}
	}
	if old, ok := rs.interfaces[ni.NetworkInterfaceID]; ok {
		delete(s.privateIPs, privateKey(region, old.SubnetID, old.PrivateIPAddress))
	}
	if ni.PrivateIPAddress == "" {
		ni.PrivateIPAddress = s.allocatePrivate(region, ni.SubnetID)
	}
	s.privateIPs[privateKey(region, ni.SubnetID, ni.PrivateIPAddress)] = ni.NetworkInterfaceID
	ni.Region = region
	if ni.AvailabilityZone == "" {
		ni.AvailabilityZone = region + "a"
	}
	if ni.MACAddress == "" {
		ni.MACAddress = s.newMAC()
	}
	if ni.Status == statusInUse && ni.InstanceID != "" && ni.AttachmentID == "" {
		ni.AttachmentID = s.newID("eni-attach")
	}
	if ni.InstanceID == "" {
		ni.AttachmentID, ni.DeleteOnTermination = "", false
	}
	stored := ni.clone()
	rs.interfaces[ni.NetworkInterfaceID] = &stored
	if a := rs.addressOn(ni.NetworkInterfaceID); a != nil {
		a.PrivateIPAddress = ni.PrivateIPAddress
	}
	return &stored, nil
}

// allocatePrivate returns the next private IP address free in subnet. The caller holds s.mu.
func (s *Server) allocatePrivate(region, subnet string) string {
	ip := s.nextLocal
	for s.privateIPs[privateKey(region, subnet, ip.String())] != "" {
		ip = ip.Next()
	}
	s.nextLocal = ip.Next()
	return ip.String()
}

// newMAC returns a fresh, locally administered MAC address. The caller holds s.mu.
func (s *Server) newMAC() string {
	s.nextID++
	var b [8]byte
	binary.BigEndian.PutUint64(b[:], s.nextID)
	return fmt.Sprintf("02:%02x:%02x:%02x:%02x:%02x", b[3], b[4], b[5], b[6], b[7])
}

type createNetworkInterfaceResponse struct {
	meta
	NetworkInterface interfaceXML `xml:"networkInterface"`
	ClientToken      string       `xml:"clientToken,omitempty"`
}

func (s *Server) createInterfaceTarget(c *call) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if id, ok := s.tokenTarget(c, "interfaces"); ok {
		return c.region + "/" + id
	}
	c.id = unusedID(s, "eni", s.lookupRegion(c.region).interfaces)
	return c.region + "/" + c.id
}

func (s *Server) createNetworkInterface(c *call) (response, *apiError) {
	subnet, err := c.p.required("SubnetId")
	if err != nil {
		return nil, err
	}
	want := NetworkInterface{
		NetworkInterfaceID: c.id,
		SubnetID:           subnet,
		Description:        c.p.get("Description"),
		PrivateIPAddress:   c.p.get("PrivateIpAddress"),
		SecurityGroupIDs:   c.p.list("SecurityGroupId"),
	}
	if want.Tags, err = c.p.tags("network-interface"); err != nil {
		return nil, err
	}
	token := c.p.get("ClientToken")
	if err := c.p.done(); err != nil {
		return nil, err
	}
	fingerprint := c.p.fingerprint()
	s.mu.Lock()
	defer s.mu.Unlock()
	t, err := s.replay(c.region, "interfaces", token, fingerprint)
	if err != nil {
		return nil, err
	}
	if t != nil {
		created := t.created.(NetworkInterface)
		return &createNetworkInterfaceResponse{NetworkInterface: created.xml(nil), ClientToken: token}, nil
	}
	ni, err := s.storeInterface(c.region, want)
	if err != nil {
		return nil, err
	}
	s.remember(c.region, "interfaces", token, fingerprint, ni.NetworkInterfaceID, ni.clone())
	return &createNetworkInterfaceResponse{NetworkInterface: ni.xml(nil), ClientToken: token}, nil
}

type describeNetworkInterfacesResponse struct {
	meta
	NetworkInterfaces []interfaceXML `xml:"networkInterfaceSet>item"`
	NextToken         string         `xml:"nextToken,omitempty"`
}

func (s *Server) describeNetworkInterfaces(c *call) (response, *apiError) {
	ids := c.p.list("NetworkInterfaceId")
	filters, err := c.p.filters(interfaceFields(&NetworkInterface{}, nil))
	if err != nil {
		return nil, err
	}
	limit, after, err := c.p.page("NetworkInterfaceIds", len(ids) > 0, maxResults, "eni")
	if err != nil {
		return nil, err
	}
	if err := c.p.done(); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	rs := s.lookupRegion(c.region)
	for _, id := range ids {
		if _, ok := rs.interfaces[id]; !ok {
			return nil, interfaceNotFound(id)
		}
	}
	associated := rs.publicAddresses()
	var matched []*NetworkInterface
	for _, id := range slices.Sorted(maps.Keys(rs.interfaces)) {
		ni := rs.interfaces[id]
		if len(ids) > 0 && !slices.Contains(ids, id) || !matchFilters(filters, interfaceFields(ni, associated[id])) {
			continue
		}
		matched = append(matched, ni)
	}
	page, next := paginate(matched, func(ni *NetworkInterface) string { return ni.NetworkInterfaceID }, after, limit)
	resp := &describeNetworkInterfacesResponse{NextToken: next}
	for _, ni := range page {
		resp.NetworkInterfaces = append(resp.NetworkInterfaces, ni.xml(associated[ni.NetworkInterfaceID]))
	}
	return resp, nil
}

func (s *Server) deleteNetworkInterface(c *call) (response, *apiError) {
	id, err := c.p.required("NetworkInterfaceId")
	if err != nil {
		return nil, err
	}
	if err := c.p.done(); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	rs := s.lookupRegion(c.region)
	ni, ok := rs.interfaces[id]
	if !ok {
		return nil, interfaceNotFound(id)
	}
	if ni.Status == statusInUse {
		return nil, clientError("InvalidNetworkInterface.InUse", "The network interface '%s' is currently in use.", id)
	}
	s.removeInterface(rs, ni)
	return &returnResponse{Return: true}, nil
}

// removeInterface deletes ni, disassociating its address. The caller holds s.mu.
func (s *Server) removeInterface(rs *regionState, ni *NetworkInterface) {
	if a := rs.addressOn(ni.NetworkInterfaceID); a != nil {
		a.disassociate()
	}
	delete(rs.interfaces, ni.NetworkInterfaceID)
	delete(s.privateIPs, privateKey(ni.Region, ni.SubnetID, ni.PrivateIPAddress))
}

// NetworkInterfaces returns every network interface the server holds,
// ordered by region and ID.
func (s *Server) NetworkInterfaces() []NetworkInterface {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []NetworkInterface
	for _, region := range slices.Sorted(maps.Keys(s.regions)) {
		rs := s.regions[region]
		for _, id := range slices.Sorted(maps.Keys(rs.interfaces)) {
			out = append(out, rs.interfaces[id].clone())
		}
	}
	return out
}

// LookupNetworkInterface returns the network interface with id in region.
func (s *Server) LookupNetworkInterface(region, id string) (NetworkInterface, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	ni, ok := s.lookupRegion(region).interfaces[id]
	if !ok {
		return NetworkInterface{}, false
	}
	return ni.clone(), true
}

// PutNetworkInterface stores ni in region as if it had been created,
// replacing an interface with the same ID, and returns it as stored. It
// requires a subnet ID and fills in what ni leaves empty, such as the ID,
// private IP address and MAC address. An interface with an InstanceID is in
// use. Use it to set up state, such as an interface another owner holds.
func (s *Server) PutNetworkInterface(region string, ni NetworkInterface) (NetworkInterface, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	stored, err := s.storeInterface(region, ni)
	if err != nil {
		return NetworkInterface{}, err
	}
	return stored.clone(), nil
}

// ErrNotFound is returned by the server's methods for a resource it does not hold.
var ErrNotFound = errors.New("ec2: resource not found")

// AttachNetworkInterface marks the interface with id in region attached to
// instanceID, as attaching it to an instance would: its status becomes
// "in-use", so it cannot be deleted until [Server.DetachNetworkInterface].
func (s *Server) AttachNetworkInterface(region, id, instanceID string) error {
	if instanceID == "" {
		return errors.New("ec2: attaching a network interface needs an instance ID")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	ni, ok := s.lookupRegion(region).interfaces[id]
	if !ok {
		return fmt.Errorf("network interface %s in %s: %w", id, region, ErrNotFound)
	}
	ni.Status, ni.InstanceID, ni.AttachmentID = statusInUse, instanceID, s.newID("eni-attach")
	return nil
}

// DetachNetworkInterface marks the interface with id in region detached: its
// status becomes "available".
func (s *Server) DetachNetworkInterface(region, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	ni, ok := s.lookupRegion(region).interfaces[id]
	if !ok {
		return fmt.Errorf("network interface %s in %s: %w", id, region, ErrNotFound)
	}
	ni.Status, ni.InstanceID, ni.AttachmentID, ni.DeleteOnTermination = statusAvailable, "", "", false
	return nil
}
