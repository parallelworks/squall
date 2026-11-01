package ec2

import (
	"maps"
	"slices"
)

var dhcpKeys = []string{
	"domain-name", "domain-name-servers", "ntp-servers", "netbios-name-servers", "netbios-node-type",
	"ipv6-address-preferred-lease-time",
}

// DHCPOptions is a DHCP options set.
type DHCPOptions struct {
	// Region is where the set is. The server sets it.
	Region        string
	DHCPOptionsID string
	// Configurations holds each option's values, by option name, such as
	// "domain-name-servers".
	Configurations map[string][]string
	Tags           map[string]string
}

func (d *DHCPOptions) clone() DHCPOptions {
	c := *d
	c.Configurations = maps.Clone(d.Configurations)
	for k, v := range c.Configurations {
		c.Configurations[k] = slices.Clone(v)
	}
	c.Tags = maps.Clone(d.Tags)
	return c
}

type dhcpValueXML struct {
	Value string `xml:"value"`
}

type dhcpConfigurationXML struct {
	Key    string         `xml:"key"`
	Values []dhcpValueXML `xml:"valueSet>item"`
}

type dhcpOptionsXML struct {
	DHCPOptionsID  string                 `xml:"dhcpOptionsId"`
	Configurations []dhcpConfigurationXML `xml:"dhcpConfigurationSet>item"`
	Tags           []tagXML               `xml:"tagSet>item,omitempty"`
}

func (d *DHCPOptions) xml() dhcpOptionsXML {
	out := dhcpOptionsXML{DHCPOptionsID: d.DHCPOptionsID, Tags: tagSet(d.Tags)}
	for _, k := range slices.Sorted(maps.Keys(d.Configurations)) {
		conf := dhcpConfigurationXML{Key: k}
		for _, v := range d.Configurations[k] {
			conf.Values = append(conf.Values, dhcpValueXML{v})
		}
		out.Configurations = append(out.Configurations, conf)
	}
	return out
}

func dhcpOptionsFields(_ *regionState, d *DHCPOptions) func(string) ([]string, bool) {
	return func(name string) ([]string, bool) {
		switch name {
		case "dhcp-options-id":
			return one(d.DHCPOptionsID), true
		case "key":
			return slices.Sorted(maps.Keys(d.Configurations)), true
		case "value":
			var out []string
			for _, k := range slices.Sorted(maps.Keys(d.Configurations)) {
				out = append(out, d.Configurations[k]...)
			}
			return out, true
		}
		return tagFilter(d.Tags, name)
	}
}

func dhcpOptionsNotFound(id string) *apiError {
	return clientError("InvalidDhcpOptionID.NotFound", "The dhcpOption ID '%s' does not exist", id)
}

// defaultDHCPOptions returns the ID of region's default DHCP options set,
// which new VPCs use, creating it with the first VPC as EC2 has it before
// any. The caller holds s.mu.
func (s *Server) defaultDHCPOptions(rs *regionState, region string) string {
	if _, ok := rs.dhcpOptions[rs.defaultDHCP]; ok {
		return rs.defaultDHCP
	}
	domain := region + ".compute.internal"
	if region == "us-east-1" {
		domain = "ec2.internal"
	}
	d := &DHCPOptions{
		Region: region, DHCPOptionsID: unusedID(s, "dopt", rs.dhcpOptions),
		Configurations: map[string][]string{"domain-name": {domain}, "domain-name-servers": {"AmazonProvidedDNS"}},
	}
	rs.dhcpOptions[d.DHCPOptionsID] = d
	rs.defaultDHCP = d.DHCPOptionsID
	return d.DHCPOptionsID
}

type createDHCPOptionsResponse struct {
	meta
	DHCPOptions dhcpOptionsXML `xml:"dhcpOptions"`
}

func (s *Server) createDHCPOptions(c *call) (response, *apiError) {
	configurations := map[string][]string{}
	for _, n := range c.p.indexes("DhcpConfiguration") {
		key := c.p.get("DhcpConfiguration." + n + ".Key")
		if !slices.Contains(dhcpKeys, key) {
			return nil, invalidValue("Value (%s) for parameter name is invalid. Unknown DHCP option", key)
		}
		values := c.p.list("DhcpConfiguration." + n + ".Value")
		if len(values) == 0 {
			return nil, invalidValue("Value for parameter %s is missing", key)
		}
		configurations[key] = append(configurations[key], values...)
	}
	if len(configurations) == 0 {
		return nil, missing("DhcpConfiguration")
	}
	tags, err := c.p.tags("dhcp-options")
	if err != nil {
		return nil, err
	}
	if err := c.p.done(); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	d := &DHCPOptions{Region: c.region, DHCPOptionsID: c.id, Configurations: configurations, Tags: tags}
	s.region(c.region).dhcpOptions[d.DHCPOptionsID] = d
	return &createDHCPOptionsResponse{DHCPOptions: d.xml()}, nil
}

type describeDHCPOptionsResponse struct {
	meta
	DHCPOptions []dhcpOptionsXML `xml:"dhcpOptionsSet>item"`
	NextToken   string           `xml:"nextToken,omitempty"`
}

func (s *Server) describeDHCPOptions(c *call) (response, *apiError) {
	sets, next, err := describe(s, c, lister[*DHCPOptions]{
		ids: "DhcpOptionsId", prefix: "dopt", of: dhcpOptionsOf,
		empty: &DHCPOptions{}, fields: dhcpOptionsFields, notFound: dhcpOptionsNotFound,
	}, func(_ *regionState, d *DHCPOptions) dhcpOptionsXML { return d.xml() })
	if err != nil {
		return nil, err
	}
	return &describeDHCPOptionsResponse{DHCPOptions: sets, NextToken: next}, nil
}

func (s *Server) associateDHCPOptions(c *call) (response, *apiError) {
	id, err := c.p.required("DhcpOptionsId")
	if err != nil {
		return nil, err
	}
	vpcID, err := c.p.required("VpcId")
	if err != nil {
		return nil, err
	}
	if err := c.p.done(); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	rs := s.lookupRegion(c.region)
	if _, ok := rs.dhcpOptions[id]; !ok && id != dhcpDefault {
		return nil, dhcpOptionsNotFound(id)
	}
	v, err := rs.vpc(vpcID)
	if err != nil {
		return nil, err
	}
	v.DHCPOptionsID = id
	return &returnResponse{Return: true}, nil
}

func (s *Server) deleteDHCPOptions(c *call) (response, *apiError) {
	id, err := c.p.required("DhcpOptionsId")
	if err != nil {
		return nil, err
	}
	if err := c.p.done(); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	rs := s.lookupRegion(c.region)
	if _, ok := rs.dhcpOptions[id]; !ok {
		return nil, dhcpOptionsNotFound(id)
	}
	for _, v := range rs.vpcs {
		if v.DHCPOptionsID == id {
			return nil, dependencyViolation("dhcpOptions", id)
		}
	}
	delete(rs.dhcpOptions, id)
	return &returnResponse{Return: true}, nil
}

// DHCPOptions returns every DHCP options set the server holds, ordered by
// region and ID. A region's default set appears with its first VPC.
func (s *Server) DHCPOptions() []DHCPOptions {
	return collect(s, dhcpOptionsOf, (*DHCPOptions).clone)
}
