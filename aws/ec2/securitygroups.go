package ec2

import (
	"fmt"
	"maps"
	"net/netip"
	"slices"
	"strconv"
	"strings"
)

const (
	defaultGroupName = "default"
	protocolAll      = "-1"
	maxGroupName     = 255
)

var (
	protocolNumbers = map[string]string{"1": "icmp", "6": "tcp", "17": "udp", "58": "icmpv6"}
	namedProtocols  = []string{"tcp", "udp", "icmp", "icmpv6", protocolAll}
)

// SecurityGroup is a VPC security group.
type SecurityGroup struct {
	// Region is where the group is. The server sets it.
	Region      string
	GroupID     string
	GroupName   string
	Description string
	VPCID       string
	// Rules holds the group's ingress and egress rules in the order they
	// were added.
	Rules []SecurityGroupRule
	Tags  map[string]string
}

// SecurityGroupRule is one rule of a security group. It has one peer: an
// IPv4 CIDR block, an IPv6 CIDR block or a security group.
type SecurityGroupRule struct {
	RuleID string
	Egress bool
	// IPProtocol is "tcp", "udp", "icmp", "icmpv6", "-1" for all, or another
	// protocol number.
	IPProtocol string
	// FromPort and ToPort are the port range for tcp and udp, and the ICMP
	// type and code for icmp and icmpv6. They are -1 for all.
	FromPort, ToPort  int
	CIDRIPv4          string
	CIDRIPv6          string
	ReferencedGroupID string
	Description       string
}

func (g *SecurityGroup) clone() SecurityGroup {
	c := *g
	c.Rules = slices.Clone(g.Rules)
	c.Tags = maps.Clone(g.Tags)
	return c
}

func (g *SecurityGroup) isDefault() bool { return g.GroupName == defaultGroupName }

// same reports whether r and o allow the same traffic, whatever their IDs and
// descriptions.
func (r SecurityGroupRule) same(o SecurityGroupRule) bool {
	r.RuleID, r.Description, o.RuleID, o.Description = "", "", "", ""
	return r == o
}

func (r SecurityGroupRule) peer() string {
	switch {
	case r.CIDRIPv4 != "":
		return r.CIDRIPv4
	case r.CIDRIPv6 != "":
		return r.CIDRIPv6
	}
	return r.ReferencedGroupID
}

func (r SecurityGroupRule) String() string {
	return fmt.Sprintf("peer: %s, %s, from port: %d, to port: %d, ALLOW", r.peer(), strings.ToUpper(r.IPProtocol), r.FromPort, r.ToPort)
}

type userIDGroupPairXML struct {
	GroupID     string `xml:"groupId"`
	Description string `xml:"description,omitempty"`
}

type ipRangeXML struct {
	CIDRIP      string `xml:"cidrIp"`
	Description string `xml:"description,omitempty"`
}

type ipv6RangeXML struct {
	CIDRIPv6    string `xml:"cidrIpv6"`
	Description string `xml:"description,omitempty"`
}

type ipPermissionXML struct {
	IPProtocol string               `xml:"ipProtocol"`
	FromPort   *int                 `xml:"fromPort,omitempty"`
	ToPort     *int                 `xml:"toPort,omitempty"`
	Groups     []userIDGroupPairXML `xml:"groups>item,omitempty"`
	IPRanges   []ipRangeXML         `xml:"ipRanges>item,omitempty"`
	IPv6Ranges []ipv6RangeXML       `xml:"ipv6Ranges>item,omitempty"`
}

type securityGroupXML struct {
	GroupID          string            `xml:"groupId"`
	GroupName        string            `xml:"groupName"`
	Description      string            `xml:"groupDescription"`
	VPCID            string            `xml:"vpcId"`
	SecurityGroupArn string            `xml:"securityGroupArn"`
	Ingress          []ipPermissionXML `xml:"ipPermissions>item"`
	Egress           []ipPermissionXML `xml:"ipPermissionsEgress>item"`
	Tags             []tagXML          `xml:"tagSet>item,omitempty"`
}

func securityGroupArn(region, id string) string {
	return "arn:aws:ec2:" + region + "::security-group/" + id
}

func (g *SecurityGroup) xml() securityGroupXML {
	return securityGroupXML{
		GroupID: g.GroupID, GroupName: g.GroupName, Description: g.Description, VPCID: g.VPCID,
		SecurityGroupArn: securityGroupArn(g.Region, g.GroupID),
		Ingress:          permissions(g.Rules, false), Egress: permissions(g.Rules, true), Tags: tagSet(g.Tags),
	}
}

// permissions renders the ingress or egress rules as EC2 describes a group's
// permissions: one per protocol and port range, with each rule's peer under it.
func permissions(rules []SecurityGroupRule, egress bool) []ipPermissionXML {
	type key struct {
		protocol string
		from, to int
	}
	var order []key
	byKey := map[key]*ipPermissionXML{}
	for _, r := range rules {
		if r.Egress != egress {
			continue
		}
		k := key{r.IPProtocol, r.FromPort, r.ToPort}
		p, ok := byKey[k]
		if !ok {
			p = &ipPermissionXML{IPProtocol: r.IPProtocol}
			if r.IPProtocol != protocolAll {
				p.FromPort, p.ToPort = new(r.FromPort), new(r.ToPort)
			}
			byKey[k] = p
			order = append(order, k)
		}
		switch {
		case r.CIDRIPv4 != "":
			p.IPRanges = append(p.IPRanges, ipRangeXML{r.CIDRIPv4, r.Description})
		case r.CIDRIPv6 != "":
			p.IPv6Ranges = append(p.IPv6Ranges, ipv6RangeXML{r.CIDRIPv6, r.Description})
		default:
			p.Groups = append(p.Groups, userIDGroupPairXML{r.ReferencedGroupID, r.Description})
		}
	}
	out := make([]ipPermissionXML, 0, len(order))
	for _, k := range order {
		out = append(out, *byKey[k])
	}
	return out
}

type referencedGroupXML struct {
	GroupID string `xml:"groupId"`
}

type securityGroupRuleXML struct {
	RuleID          string              `xml:"securityGroupRuleId"`
	GroupID         string              `xml:"groupId"`
	IsEgress        bool                `xml:"isEgress"`
	IPProtocol      string              `xml:"ipProtocol"`
	FromPort        int                 `xml:"fromPort"`
	ToPort          int                 `xml:"toPort"`
	CIDRIPv4        string              `xml:"cidrIpv4,omitempty"`
	CIDRIPv6        string              `xml:"cidrIpv6,omitempty"`
	ReferencedGroup *referencedGroupXML `xml:"referencedGroupInfo,omitempty"`
	Description     string              `xml:"description,omitempty"`
	RuleArn         string              `xml:"securityGroupRuleArn"`
}

func (g *SecurityGroup) ruleXML(r SecurityGroupRule) securityGroupRuleXML {
	out := securityGroupRuleXML{
		RuleID: r.RuleID, GroupID: g.GroupID, IsEgress: r.Egress, IPProtocol: r.IPProtocol, FromPort: r.FromPort, ToPort: r.ToPort,
		CIDRIPv4: r.CIDRIPv4, CIDRIPv6: r.CIDRIPv6, Description: r.Description,
		RuleArn: "arn:aws:ec2:" + g.Region + "::security-group-rule/" + r.RuleID,
	}
	if r.ReferencedGroupID != "" {
		out.ReferencedGroup = &referencedGroupXML{r.ReferencedGroupID}
	}
	return out
}

type revokedRuleXML struct {
	RuleID            string `xml:"securityGroupRuleId"`
	GroupID           string `xml:"groupId"`
	IsEgress          bool   `xml:"isEgress"`
	IPProtocol        string `xml:"ipProtocol"`
	FromPort          int    `xml:"fromPort"`
	ToPort            int    `xml:"toPort"`
	CIDRIPv4          string `xml:"cidrIpv4,omitempty"`
	CIDRIPv6          string `xml:"cidrIpv6,omitempty"`
	ReferencedGroupID string `xml:"referencedGroupId,omitempty"`
	Description       string `xml:"description,omitempty"`
}

func securityGroupFields(_ *regionState, g *SecurityGroup) func(string) ([]string, bool) {
	return func(name string) ([]string, bool) {
		switch name {
		case "group-id":
			return one(g.GroupID), true
		case "group-name":
			return one(g.GroupName), true
		case "description":
			return one(g.Description), true
		case "vpc-id":
			return one(g.VPCID), true
		}
		return tagFilter(g.Tags, name)
	}
}

// groupRule is a rule with the group that holds it.
type groupRule struct {
	group *SecurityGroup
	rule  SecurityGroupRule
}

func securityGroupRulesOf(rs *regionState) map[string]groupRule {
	out := map[string]groupRule{}
	for _, g := range rs.securityGroups {
		for _, r := range g.Rules {
			out[r.RuleID] = groupRule{g, r}
		}
	}
	return out
}

func securityGroupRuleFields(_ *regionState, gr groupRule) func(string) ([]string, bool) {
	return func(name string) ([]string, bool) {
		switch name {
		case "group-id":
			return one(gr.group.GroupID), true
		case "security-group-rule-id":
			return one(gr.rule.RuleID), true
		}
		return nil, false
	}
}

func securityGroupNotFound(id string) *apiError {
	return clientError("InvalidGroup.NotFound", "The security group '%s' does not exist", id)
}

func ruleNotFound(id string) *apiError {
	return clientError("InvalidSecurityGroupRuleId.NotFound", "The security group rule ID '%s' does not exist", id)
}

func noDefaultVPC() *apiError {
	return clientError("VPCIdNotSpecified", "No default VPC for this user")
}

// securityGroup returns the security group with id. The caller holds s.mu.
func (rs *regionState) securityGroup(id string) (*SecurityGroup, *apiError) {
	if g, ok := rs.securityGroups[id]; ok {
		return g, nil
	}
	return nil, securityGroupNotFound(id)
}

// defaultGroup returns the ID of the default security group of the VPC with
// id. The caller holds s.mu.
func (rs *regionState) defaultGroup(vpcID string) string {
	for _, g := range rs.securityGroups {
		if g.VPCID == vpcID && g.isDefault() {
			return g.GroupID
		}
	}
	return ""
}

// groupsFor returns the security groups an interface in subnetID gets when
// a call names ids: ids as they are in a subnet the server does not hold, and
// otherwise ids checked against the subnet's VPC, or its default group for
// none. The caller holds s.mu.
func (rs *regionState) groupsFor(subnetID string, ids []string) ([]string, *apiError) {
	sub, ok := rs.subnets[subnetID]
	if !ok {
		return ids, nil
	}
	if len(ids) == 0 {
		return []string{rs.defaultGroup(sub.VPCID)}, nil
	}
	for _, id := range ids {
		g, err := rs.securityGroup(id)
		if err != nil {
			return nil, err
		}
		if g.VPCID != sub.VPCID {
			return nil, clientError("InvalidParameter", "Security group %s and subnet %s belong to different networks.", id, subnetID)
		}
	}
	return ids, nil
}

// newDefaultSecurityGroup stores v's default security group, which allows
// traffic from its own members and all outbound traffic. The caller holds s.mu.
func (s *Server) newDefaultSecurityGroup(rs *regionState, v *VPC) {
	g := &SecurityGroup{
		Region: v.Region, GroupID: unusedID(s, "sg", rs.securityGroups), GroupName: defaultGroupName,
		Description: "default VPC security group", VPCID: v.VPCID,
	}
	g.Rules = []SecurityGroupRule{
		{RuleID: s.newID("sgr"), IPProtocol: protocolAll, FromPort: -1, ToPort: -1, ReferencedGroupID: g.GroupID},
		s.allowAllEgress(),
	}
	rs.securityGroups[g.GroupID] = g
}

// allowAllEgress returns the rule a new group starts with. The caller holds s.mu.
func (s *Server) allowAllEgress() SecurityGroupRule {
	return SecurityGroupRule{RuleID: s.newID("sgr"), Egress: true, IPProtocol: protocolAll, FromPort: -1, ToPort: -1, CIDRIPv4: "0.0.0.0/0"}
}

// groupInUse reports whether a network interface uses the group with id or
// another group's rule refers to it. The caller holds s.mu.
func (rs *regionState) groupInUse(id string) bool {
	for _, ni := range rs.interfaces {
		if slices.Contains(ni.SecurityGroupIDs, id) {
			return true
		}
	}
	for _, g := range rs.securityGroups {
		if g.GroupID != id && slices.ContainsFunc(g.Rules, func(r SecurityGroupRule) bool { return r.ReferencedGroupID == id }) {
			return true
		}
	}
	return false
}

type createSecurityGroupResponse struct {
	meta
	GroupID          string   `xml:"groupId"`
	SecurityGroupArn string   `xml:"securityGroupArn"`
	Tags             []tagXML `xml:"tagSet>item,omitempty"`
}

func (s *Server) createSecurityGroup(c *call) (response, *apiError) {
	name, err := c.p.required("GroupName")
	if err != nil {
		return nil, err
	}
	description, err := c.p.required("GroupDescription")
	if err != nil {
		return nil, err
	}
	switch {
	case len(name) > maxGroupName:
		return nil, invalidValue("Value (%s) for parameter GroupName is invalid. Group names may be up to %d characters.", name, maxGroupName)
	case strings.HasPrefix(strings.ToLower(name), "sg-"):
		return nil, invalidValue("Value (%s) for parameter GroupName is invalid. Group names may not be in the format sg-*.", name)
	case name == defaultGroupName:
		return nil, clientError("InvalidGroup.Reserved", "The security group '%s' is reserved.", name)
	}
	vpcID := c.p.get("VpcId")
	tags, err := c.p.tags("security-group")
	if err != nil {
		return nil, err
	}
	if err := c.p.done(); err != nil {
		return nil, err
	}
	if vpcID == "" {
		return nil, noDefaultVPC()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	rs := s.lookupRegion(c.region)
	v, err := rs.vpc(vpcID)
	if err != nil {
		return nil, err
	}
	for _, g := range rs.securityGroups {
		if g.VPCID == vpcID && g.GroupName == name {
			return nil, clientError("InvalidGroup.Duplicate", "The security group '%s' already exists for VPC '%s'", name, vpcID)
		}
	}
	g := &SecurityGroup{
		Region: c.region, GroupID: c.id, GroupName: name, Description: description, VPCID: v.VPCID,
		Rules: []SecurityGroupRule{s.allowAllEgress()}, Tags: tags,
	}
	rs.securityGroups[g.GroupID] = g
	return &createSecurityGroupResponse{GroupID: g.GroupID, SecurityGroupArn: securityGroupArn(c.region, g.GroupID), Tags: tagSet(tags)}, nil
}

type describeSecurityGroupsResponse struct {
	meta
	SecurityGroups []securityGroupXML `xml:"securityGroupInfo>item"`
	NextToken      string             `xml:"nextToken,omitempty"`
}

func (s *Server) describeSecurityGroups(c *call) (response, *apiError) {
	l := lister[*SecurityGroup]{
		ids: "GroupId", prefix: "sg", of: securityGroupsOf,
		empty: &SecurityGroup{}, fields: securityGroupFields, notFound: securityGroupNotFound,
	}
	if names := c.p.list("GroupName"); len(names) > 0 {
		l.byName = func(rs *regionState) ([]string, *apiError) { return rs.groupsNamed(names) }
	}
	groups, next, err := describe(s, c, l, func(_ *regionState, g *SecurityGroup) securityGroupXML { return g.xml() })
	if err != nil {
		return nil, err
	}
	return &describeSecurityGroupsResponse{SecurityGroups: groups, NextToken: next}, nil
}

// groupsNamed returns the IDs of the groups with names, in any VPC. The
// caller holds s.mu.
func (rs *regionState) groupsNamed(names []string) ([]string, *apiError) {
	var ids []string
	for _, name := range names {
		found := false
		for id, g := range rs.securityGroups {
			if g.GroupName == name {
				ids, found = append(ids, id), true
			}
		}
		if !found {
			return nil, securityGroupNotFound(name)
		}
	}
	return ids, nil
}

type securityGroupRulesResponse struct {
	meta
	Return bool                   `xml:"return,omitempty"`
	Rules  []securityGroupRuleXML `xml:"securityGroupRuleSet>item"`
	Next   string                 `xml:"nextToken,omitempty"`
}

func (s *Server) describeSecurityGroupRules(c *call) (response, *apiError) {
	rules, next, err := describe(s, c, lister[groupRule]{
		ids: "SecurityGroupRuleId", prefix: "sgr", of: securityGroupRulesOf,
		empty: groupRule{group: &SecurityGroup{}}, fields: securityGroupRuleFields, notFound: ruleNotFound,
	}, func(_ *regionState, gr groupRule) securityGroupRuleXML { return gr.group.ruleXML(gr.rule) })
	if err != nil {
		return nil, err
	}
	return &securityGroupRulesResponse{Rules: rules, Next: next}, nil
}

// groupID reads the GroupId a rule or delete call acts on. EC2 takes a
// GroupName instead only in a default VPC, which the server has none of.
func groupID(p *params, byName bool) (string, *apiError) {
	if byName && p.get("GroupName") != "" {
		return "", noDefaultVPC()
	}
	return p.required("GroupId")
}

// readPermissions reads IpPermissions.N as the rules they make, one for each
// peer. The caller checks referenced groups.
func readPermissions(p *params, egress bool) ([]SecurityGroupRule, *apiError) {
	indexes := p.indexes("IpPermissions")
	if len(indexes) == 0 {
		return nil, missing("IpPermissions")
	}
	var out []SecurityGroupRule
	for _, n := range indexes {
		key := "IpPermissions." + n + "."
		base, err := readProtocol(p, key)
		if err != nil {
			return nil, err
		}
		base.Egress = egress
		before := len(out)
		for _, m := range p.indexes(key + "IpRanges") {
			r := base
			if r.CIDRIPv4, err = readCIDR(p, key+"IpRanges."+m+".CidrIp", true); err != nil {
				return nil, err
			}
			r.Description = p.get(key + "IpRanges." + m + ".Description")
			out = append(out, r)
		}
		for _, m := range p.indexes(key + "Ipv6Ranges") {
			r := base
			if r.CIDRIPv6, err = readCIDR(p, key+"Ipv6Ranges."+m+".CidrIpv6", false); err != nil {
				return nil, err
			}
			r.Description = p.get(key + "Ipv6Ranges." + m + ".Description")
			out = append(out, r)
		}
		for _, m := range p.indexes(key + "Groups") {
			r := base
			if r.ReferencedGroupID, err = p.required(key + "Groups." + m + ".GroupId"); err != nil {
				return nil, err
			}
			r.Description = p.get(key + "Groups." + m + ".Description")
			p.get(key + "Groups." + m + ".UserId")
			p.get(key + "Groups." + m + ".VpcId")
			out = append(out, r)
		}
		if len(out) == before {
			return nil, invalidValue("The permission %s needs at least one of IpRanges, Ipv6Ranges or Groups.", strings.TrimSuffix(key, "."))
		}
	}
	return out, nil
}

// readProtocol reads a permission's protocol and ports.
func readProtocol(p *params, key string) (SecurityGroupRule, *apiError) {
	protocol, err := p.required(key + "IpProtocol")
	if err != nil {
		return SecurityGroupRule{}, err
	}
	protocol = strings.ToLower(protocol)
	if name, ok := protocolNumbers[protocol]; ok {
		protocol = name
	}
	if !slices.Contains(namedProtocols, protocol) {
		if n, err := strconv.Atoi(protocol); err != nil || n < 0 || n > 255 {
			return SecurityGroupRule{}, invalidValue("Invalid value '%s' for IP protocol. Unknown protocol.", protocol)
		}
	}
	r := SecurityGroupRule{IPProtocol: protocol, FromPort: -1, ToPort: -1}
	from, hasFrom, err := port(p, key+"FromPort")
	if err != nil {
		return r, err
	}
	to, hasTo, err := port(p, key+"ToPort")
	if err != nil {
		return r, err
	}
	switch protocol {
	case "tcp", "udp":
		if !hasFrom || !hasTo || from < 0 || to > 65535 || from > to {
			return r, invalidValue("Invalid TCP/UDP port range (%d, %d).", from, to)
		}
		r.FromPort, r.ToPort = from, to
	case "icmp", "icmpv6":
		if hasFrom {
			r.FromPort = from
		}
		if hasTo {
			r.ToPort = to
		}
		if r.FromPort < -1 || r.FromPort > 255 || r.ToPort < -1 || r.ToPort > 255 {
			return r, invalidValue("Invalid ICMP type or code (%d, %d).", r.FromPort, r.ToPort)
		}
	}
	return r, nil
}

func port(p *params, key string) (int, bool, *apiError) {
	has := p.values.Has(key)
	n, err := p.int(key)
	return n, has, err
}

func readCIDR(p *params, key string, v4 bool) (string, *apiError) {
	cidr, err := p.required(key)
	if err != nil {
		return "", err
	}
	prefix, perr := netip.ParsePrefix(cidr)
	if perr != nil || prefix.Addr().Is4() != v4 {
		return "", invalidValue("CIDR block %s is malformed", cidr)
	}
	return prefix.Masked().String(), nil
}

// checkPeers checks that every group rules refer to is in g's VPC. The
// caller holds s.mu.
func (rs *regionState) checkPeers(g *SecurityGroup, rules []SecurityGroupRule) *apiError {
	for _, r := range rules {
		if r.ReferencedGroupID == "" {
			continue
		}
		peer, err := rs.securityGroup(r.ReferencedGroupID)
		if err != nil {
			return err
		}
		if peer.VPCID != g.VPCID {
			return invalidValue("Security groups %s and %s belong to different networks.", g.GroupID, peer.GroupID)
		}
	}
	return nil
}

func (s *Server) authorizeIngress(c *call) (response, *apiError) { return s.authorize(c, false) }

func (s *Server) authorizeEgress(c *call) (response, *apiError) { return s.authorize(c, true) }

func (s *Server) authorize(c *call, egress bool) (response, *apiError) {
	id, err := groupID(c.p, !egress)
	if err != nil {
		return nil, err
	}
	rules, err := readPermissions(c.p, egress)
	if err != nil {
		return nil, err
	}
	if err := c.p.done(); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	rs := s.lookupRegion(c.region)
	g, err := rs.securityGroup(id)
	if err != nil {
		return nil, err
	}
	if err := rs.checkPeers(g, rules); err != nil {
		return nil, err
	}
	for i, r := range rules {
		if slices.ContainsFunc(g.Rules, r.same) || slices.ContainsFunc(rules[:i], r.same) {
			return nil, clientError("InvalidPermission.Duplicate", "the specified rule \"%s\" already exists", r)
		}
	}
	resp := &securityGroupRulesResponse{Return: true}
	for _, r := range rules {
		r.RuleID = s.newID("sgr")
		g.Rules = append(g.Rules, r)
		resp.Rules = append(resp.Rules, g.ruleXML(r))
	}
	return resp, nil
}

type revokeResponse struct {
	meta
	Return  bool             `xml:"return"`
	Revoked []revokedRuleXML `xml:"revokedSecurityGroupRuleSet>item"`
}

func (s *Server) revokeIngress(c *call) (response, *apiError) { return s.revoke(c, false) }

func (s *Server) revokeEgress(c *call) (response, *apiError) { return s.revoke(c, true) }

func (s *Server) revoke(c *call, egress bool) (response, *apiError) {
	id, err := groupID(c.p, !egress)
	if err != nil {
		return nil, err
	}
	ruleIDs := c.p.list("SecurityGroupRuleId")
	var rules []SecurityGroupRule
	switch hasPermissions := len(c.p.indexes("IpPermissions")) > 0; {
	case hasPermissions && len(ruleIDs) > 0:
		return nil, clientError("InvalidParameterCombination", "The parameter IpPermissions cannot be used with the parameter SecurityGroupRuleId")
	case len(ruleIDs) == 0:
		if rules, err = readPermissions(c.p, egress); err != nil {
			return nil, err
		}
	}
	if err := c.p.done(); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	g, err := s.lookupRegion(c.region).securityGroup(id)
	if err != nil {
		return nil, err
	}
	var remove []int
	for _, ruleID := range ruleIDs {
		i := slices.IndexFunc(g.Rules, func(r SecurityGroupRule) bool { return r.RuleID == ruleID && r.Egress == egress })
		if i < 0 {
			return nil, ruleNotFound(ruleID)
		}
		if !slices.Contains(remove, i) {
			remove = append(remove, i)
		}
	}
	for _, want := range rules {
		i := slices.IndexFunc(g.Rules, want.same)
		if i < 0 {
			return nil, clientError("InvalidPermission.NotFound", "The specified rule does not exist in this security group.")
		}
		if !slices.Contains(remove, i) {
			remove = append(remove, i)
		}
	}
	resp := &revokeResponse{Return: true}
	for _, i := range remove {
		r := g.Rules[i]
		resp.Revoked = append(resp.Revoked, revokedRuleXML{
			RuleID: r.RuleID, GroupID: g.GroupID, IsEgress: r.Egress, IPProtocol: r.IPProtocol, FromPort: r.FromPort, ToPort: r.ToPort,
			CIDRIPv4: r.CIDRIPv4, CIDRIPv6: r.CIDRIPv6, ReferencedGroupID: r.ReferencedGroupID, Description: r.Description,
		})
	}
	g.Rules = slices.DeleteFunc(g.Rules, func(r SecurityGroupRule) bool {
		return slices.ContainsFunc(resp.Revoked, func(x revokedRuleXML) bool { return x.RuleID == r.RuleID })
	})
	return resp, nil
}

type deleteSecurityGroupResponse struct {
	meta
	Return  bool   `xml:"return"`
	GroupID string `xml:"groupId"`
}

func (s *Server) deleteSecurityGroup(c *call) (response, *apiError) {
	id, err := groupID(c.p, true)
	if err != nil {
		return nil, err
	}
	if err := c.p.done(); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	rs := s.lookupRegion(c.region)
	g, err := rs.securityGroup(id)
	if err != nil {
		return nil, err
	}
	if g.isDefault() {
		return nil, clientError("CannotDelete", "the specified group: \"%s\" name: \"%s\" cannot be deleted by a user", id, g.GroupName)
	}
	if rs.groupInUse(id) {
		return nil, clientError("DependencyViolation", "resource %s has a dependent object", id)
	}
	delete(rs.securityGroups, id)
	return &deleteSecurityGroupResponse{Return: true, GroupID: id}, nil
}

// SecurityGroups returns every security group the server holds, ordered by
// region and ID. Each VPC has its default group.
func (s *Server) SecurityGroups() []SecurityGroup {
	return collect(s, securityGroupsOf, (*SecurityGroup).clone)
}
