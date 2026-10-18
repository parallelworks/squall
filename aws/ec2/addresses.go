package ec2

import (
	"cmp"
	"maps"
	"net/netip"
	"slices"
)

const domainVPC = "vpc"

// Address is an Elastic IP address.
type Address struct {
	// Region is where the address is allocated. The server sets it.
	Region       string
	AllocationID string
	PublicIP     string
	// Domain is always "vpc".
	Domain string
	// AssociationID, NetworkInterfaceID and PrivateIPAddress are set while the
	// address is associated with a network interface.
	AssociationID      string
	NetworkInterfaceID string
	PrivateIPAddress   string
	Tags               map[string]string
}

func (a *Address) clone() Address {
	c := *a
	c.Tags = maps.Clone(a.Tags)
	return c
}

func (a *Address) disassociate() {
	a.AssociationID, a.NetworkInterfaceID, a.PrivateIPAddress = "", "", ""
}

type tagXML struct {
	Key   string `xml:"key"`
	Value string `xml:"value"`
}

func tagSet(tags map[string]string) []tagXML {
	out := make([]tagXML, 0, len(tags))
	for _, k := range slices.Sorted(maps.Keys(tags)) {
		out = append(out, tagXML{k, tags[k]})
	}
	return out
}

type addressXML struct {
	PublicIP           string   `xml:"publicIp"`
	AllocationID       string   `xml:"allocationId"`
	Domain             string   `xml:"domain"`
	AssociationID      string   `xml:"associationId,omitempty"`
	InstanceID         string   `xml:"instanceId,omitempty"`
	NetworkInterfaceID string   `xml:"networkInterfaceId,omitempty"`
	PrivateIPAddress   string   `xml:"privateIpAddress,omitempty"`
	Tags               []tagXML `xml:"tagSet>item,omitempty"`
	PublicIPv4Pool     string   `xml:"publicIpv4Pool"`
	NetworkBorderGroup string   `xml:"networkBorderGroup"`
}

func (a *Address) xml(instanceID string) addressXML {
	return addressXML{
		PublicIP:           a.PublicIP,
		AllocationID:       a.AllocationID,
		Domain:             a.Domain,
		AssociationID:      a.AssociationID,
		InstanceID:         instanceID,
		NetworkInterfaceID: a.NetworkInterfaceID,
		PrivateIPAddress:   a.PrivateIPAddress,
		Tags:               tagSet(a.Tags),
		PublicIPv4Pool:     "amazon",
		NetworkBorderGroup: a.Region,
	}
}

// one is the filter values of a field that may be empty.
func one(v string) []string {
	if v == "" {
		return nil
	}
	return []string{v}
}

// association renders a as an interface's association, or nothing for no
// address. An address with no allocation ID is one EC2 assigned an instance.
func (a *Address) association() *associationXML {
	if a == nil {
		return nil
	}
	out := &associationXML{PublicIP: a.PublicIP, AllocationID: a.AllocationID, AssociationID: a.AssociationID}
	if a.AllocationID == "" {
		out.IPOwnerID = "amazon"
	}
	return out
}

// publicAddresses returns the public address of each interface that has one,
// by interface ID. The caller holds s.mu.
func (rs *regionState) publicAddresses() map[string]*Address {
	out := map[string]*Address{}
	for _, inst := range rs.instances {
		if inst.PublicIPAddress != "" && inst.NetworkInterfaceID != "" {
			out[inst.NetworkInterfaceID] = &Address{PublicIP: inst.PublicIPAddress}
		}
	}
	for _, a := range rs.addresses {
		if a.NetworkInterfaceID != "" {
			out[a.NetworkInterfaceID] = a
		}
	}
	return out
}

// addressFields returns the values of a's DescribeAddresses filters, where
// the address's interface is attached to instanceID.
func addressFields(a *Address, instanceID string) func(string) ([]string, bool) {
	return func(name string) ([]string, bool) {
		switch name {
		case "allocation-id":
			return one(a.AllocationID), true
		case "association-id":
			return one(a.AssociationID), true
		case "domain":
			return one(a.Domain), true
		case "instance-id":
			return one(instanceID), true
		case "network-border-group":
			return one(a.Region), true
		case "network-interface-id":
			return one(a.NetworkInterfaceID), true
		case "private-ip-address":
			return one(a.PrivateIPAddress), true
		case "public-ip":
			return one(a.PublicIP), true
		}
		return tagFilter(a.Tags, name)
	}
}

// instanceOf returns the instance a's interface is attached to. The caller holds s.mu.
func (rs *regionState) instanceOf(a *Address) string {
	if ni, ok := rs.interfaces[a.NetworkInterfaceID]; ok {
		return ni.InstanceID
	}
	return ""
}

// addressOn returns the address associated with the interface id. The caller holds s.mu.
func (rs *regionState) addressOn(id string) *Address {
	for _, a := range rs.addresses {
		if a.NetworkInterfaceID == id {
			return a
		}
	}
	return nil
}

// addressWithIP returns the address with the public IP ip. The caller holds s.mu.
func (rs *regionState) addressWithIP(ip string) *Address {
	for _, a := range rs.addresses {
		if a.PublicIP == ip {
			return a
		}
	}
	return nil
}

// associate associates a with ni, disassociating whatever address ni had.
// The caller holds s.mu.
func (s *Server) associate(rs *regionState, a *Address, ni *NetworkInterface, id string) {
	for _, other := range rs.addresses {
		if other != a && other.NetworkInterfaceID == ni.NetworkInterfaceID {
			other.disassociate()
		}
	}
	a.NetworkInterfaceID = ni.NetworkInterfaceID
	a.PrivateIPAddress = ni.PrivateIPAddress
	a.AssociationID = cmp.Or(id, unusedID(s, "eipassoc", rs.associations()))
	if inst, ok := rs.instances[ni.InstanceID]; ok {
		s.releasePublicIP(inst)
	}
}

func (rs *regionState) associations() map[string]*Address {
	associations := map[string]*Address{}
	for _, a := range rs.addresses {
		if a.AssociationID != "" {
			associations[a.AssociationID] = a
		}
	}
	return associations
}

// storeAddress fills in what a leaves empty and stores it in region,
// replacing an address with the same allocation ID. The caller holds s.mu.
func (s *Server) storeAddress(region string, a Address) (*Address, *apiError) {
	if a.Domain != "" && a.Domain != domainVPC {
		return nil, invalidValue("Invalid value '%s' for domain.", a.Domain)
	}
	if err := validateTags(a.Tags); err != nil {
		return nil, err
	}
	rs := s.region(region)
	if a.AllocationID == "" {
		a.AllocationID = unusedID(s, "eipalloc", rs.addresses)
	}
	if a.PublicIP != "" {
		var err *apiError
		if a.PublicIP, err = parsePublicIP(a.PublicIP); err != nil {
			return nil, err
		}
		if holder, taken := s.publicIPs[a.PublicIP]; taken && holder != region+"/"+a.AllocationID {
			return nil, clientError("InvalidIPAddress.InUse", "Address '%s' is in use.", a.PublicIP)
		}
	}
	var ni *NetworkInterface
	if a.NetworkInterfaceID != "" {
		var ok bool
		if ni, ok = rs.interfaces[a.NetworkInterfaceID]; !ok {
			return nil, interfaceNotFound(a.NetworkInterfaceID)
		}
		if holder, taken := rs.associations()[a.AssociationID]; a.AssociationID != "" && taken && holder.AllocationID != a.AllocationID {
			return nil, invalidValue("Association ID '%s' is in use.", a.AssociationID)
		}
	}
	if old, ok := rs.addresses[a.AllocationID]; ok {
		delete(s.publicIPs, old.PublicIP)
	}
	if a.PublicIP == "" {
		a.PublicIP = s.allocatePublic()
	}
	s.publicIPs[a.PublicIP] = region + "/" + a.AllocationID
	a.Region, a.Domain = region, domainVPC
	a.Tags = maps.Clone(a.Tags)
	stored := &a
	rs.addresses[a.AllocationID] = stored
	if ni == nil {
		stored.disassociate()
		return stored, nil
	}
	s.associate(rs, stored, ni, stored.AssociationID)
	return stored, nil
}

func parsePublicIP(s string) (string, *apiError) {
	ip, err := netip.ParseAddr(s)
	if err != nil || !ip.Is4() {
		return "", clientError("InvalidAddress.Malformed", "Invalid IPv4 address: %s", s)
	}
	return ip.String(), nil
}

// allocatePublic returns the next free public IP address. The caller holds s.mu.
func (s *Server) allocatePublic() string {
	ip := s.nextPublic
	for s.publicIPs[ip.String()] != "" {
		ip = ip.Next()
	}
	s.nextPublic = ip.Next()
	return ip.String()
}

type allocateAddressResponse struct {
	meta
	PublicIP           string `xml:"publicIp"`
	Domain             string `xml:"domain"`
	AllocationID       string `xml:"allocationId"`
	PublicIPv4Pool     string `xml:"publicIpv4Pool"`
	NetworkBorderGroup string `xml:"networkBorderGroup"`
}

func (s *Server) allocateTarget(c *call) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	c.id = unusedID(s, "eipalloc", s.lookupRegion(c.region).addresses)
	return c.region + "/" + c.id
}

func (s *Server) allocateAddress(c *call) (response, *apiError) {
	domain := c.p.get("Domain")
	tags, err := c.p.tags("elastic-ip")
	if err != nil {
		return nil, err
	}
	if err := c.p.done(); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.addressLimit > 0 && len(s.region(c.region).addresses) >= s.addressLimit {
		return nil, clientError("AddressLimitExceeded", "The maximum number of addresses has been reached.")
	}
	a, err := s.storeAddress(c.region, Address{AllocationID: c.id, Domain: domain, Tags: tags})
	if err != nil {
		return nil, err
	}
	return &allocateAddressResponse{
		PublicIP: a.PublicIP, Domain: a.Domain, AllocationID: a.AllocationID,
		PublicIPv4Pool: "amazon", NetworkBorderGroup: a.Region,
	}, nil
}

type describeAddressesResponse struct {
	meta
	Addresses []addressXML `xml:"addressesSet>item"`
}

func (s *Server) describeAddresses(c *call) (response, *apiError) {
	ids, ips := c.p.list("AllocationId"), c.p.list("PublicIp")
	for _, ip := range ips {
		if _, err := parsePublicIP(ip); err != nil {
			return nil, err
		}
	}
	filters, err := c.p.filters(addressFields(&Address{}, ""))
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
		if _, ok := rs.addresses[id]; !ok {
			return nil, allocationNotFound(id)
		}
	}
	for _, ip := range ips {
		if rs.addressWithIP(ip) == nil {
			return nil, clientError("InvalidAddress.NotFound", "Address '%s' not found.", ip)
		}
	}
	resp := &describeAddressesResponse{}
	for _, id := range slices.Sorted(maps.Keys(rs.addresses)) {
		a := rs.addresses[id]
		instance := rs.instanceOf(a)
		if len(ids) > 0 && !slices.Contains(ids, id) || len(ips) > 0 && !slices.Contains(ips, a.PublicIP) ||
			!matchFilters(filters, addressFields(a, instance)) {
			continue
		}
		resp.Addresses = append(resp.Addresses, a.xml(instance))
	}
	return resp, nil
}

func (s *Server) releaseAddress(c *call) (response, *apiError) {
	id, err := c.p.required("AllocationId")
	if err != nil {
		return nil, err
	}
	if err := c.p.done(); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	rs := s.lookupRegion(c.region)
	a, ok := rs.addresses[id]
	if !ok {
		return nil, allocationNotFound(id)
	}
	if a.AssociationID != "" {
		return nil, clientError("InvalidIPAddress.InUse", "Address '%s' is in use.", a.PublicIP)
	}
	delete(rs.addresses, id)
	delete(s.publicIPs, a.PublicIP)
	return &returnResponse{Return: true}, nil
}

type associateAddressResponse struct {
	meta
	Return        bool   `xml:"return"`
	AssociationID string `xml:"associationId"`
}

func (s *Server) associateAddress(c *call) (response, *apiError) {
	id, err := c.p.required("AllocationId")
	if err != nil {
		return nil, err
	}
	niID, instanceID := c.p.get("NetworkInterfaceId"), c.p.get("InstanceId")
	switch {
	case niID == "" && instanceID == "":
		return nil, missing("InstanceId")
	case niID != "" && instanceID != "":
		return nil, clientError("InvalidParameterCombination", "You may specify an instance ID or a network interface ID, but not both.")
	}
	reassociate, err := c.p.bool("AllowReassociation", true)
	if err != nil {
		return nil, err
	}
	if err := c.p.done(); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	rs := s.lookupRegion(c.region)
	a, ok := rs.addresses[id]
	if !ok {
		return nil, allocationNotFound(id)
	}
	if instanceID != "" {
		inst, err := rs.instance(instanceID)
		if err != nil {
			return nil, err
		}
		if inst.State != stateRunning && inst.State != stateStopped || inst.NetworkInterfaceID == "" {
			return nil, clientError("InvalidInstanceID", "The %s instance '%s' is not in a valid state for this operation.", inst.State, instanceID)
		}
		niID = inst.NetworkInterfaceID
	}
	ni, ok := rs.interfaces[niID]
	if !ok {
		return nil, interfaceNotFound(niID)
	}
	if a.NetworkInterfaceID != niID {
		if a.AssociationID != "" && !reassociate {
			return nil, clientError("Resource.AlreadyAssociated", "resource %s is already associated with associate-id %s", id, a.AssociationID)
		}
		s.associate(rs, a, ni, "")
	}
	return &associateAddressResponse{Return: true, AssociationID: a.AssociationID}, nil
}

func (s *Server) disassociateAddress(c *call) (response, *apiError) {
	id, err := c.p.required("AssociationId")
	if err != nil {
		return nil, err
	}
	if err := c.p.done(); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	rs := s.lookupRegion(c.region)
	for _, a := range rs.addresses {
		if a.AssociationID == id {
			ni := rs.interfaces[a.NetworkInterfaceID]
			a.disassociate()
			if ni != nil {
				if inst, ok := rs.instances[ni.InstanceID]; ok {
					s.assignPublicIP(rs, inst)
				}
			}
			return &returnResponse{Return: true}, nil
		}
	}
	return nil, clientError("InvalidAssociationID.NotFound", "The association ID '%s' does not exist", id)
}

// Addresses returns every address the server holds, ordered by region and
// allocation ID.
func (s *Server) Addresses() []Address {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []Address
	for _, region := range slices.Sorted(maps.Keys(s.regions)) {
		rs := s.regions[region]
		for _, id := range slices.Sorted(maps.Keys(rs.addresses)) {
			out = append(out, rs.addresses[id].clone())
		}
	}
	return out
}

// LookupAddress returns the address with allocationID in region.
func (s *Server) LookupAddress(region, allocationID string) (Address, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	a, ok := s.lookupRegion(region).addresses[allocationID]
	if !ok {
		return Address{}, false
	}
	return a.clone(), true
}

// PutAddress stores a in region as if it had been allocated, replacing an
// address with the same allocation ID, and returns it as stored. It fills in
// what a leaves empty, such as the allocation ID and public IP address. When
// a names a network interface, the address is associated with it. Use it to
// set up state, such as an address another owner already holds.
func (s *Server) PutAddress(region string, a Address) (Address, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	stored, err := s.storeAddress(region, a)
	if err != nil {
		return Address{}, err
	}
	return stored.clone(), nil
}
