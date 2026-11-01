package ec2_test

import (
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsec2 "github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/ec2/types"

	"github.com/parallelworks/squall/access"
	"github.com/parallelworks/squall/aws/ec2"
	"github.com/parallelworks/squall/fault"
	"github.com/parallelworks/squall/sdktest/internal/sdkerr"
)

func createGroup(t *testing.T, c *awsec2.Client, vpc, name string, kv ...string) string {
	t.Helper()
	out, err := c.CreateSecurityGroup(t.Context(), &awsec2.CreateSecurityGroupInput{
		GroupName: aws.String(name), Description: aws.String(name + " servers"), VpcId: aws.String(vpc),
		TagSpecifications: optionalTags(types.ResourceTypeSecurityGroup, kv...),
	})
	if err != nil {
		t.Fatalf("create security group %s: %v", name, err)
	}
	return aws.ToString(out.GroupId)
}

func describeGroups(t *testing.T, c *awsec2.Client, in *awsec2.DescribeSecurityGroupsInput) []types.SecurityGroup {
	t.Helper()
	out, err := c.DescribeSecurityGroups(t.Context(), in)
	if err != nil {
		t.Fatalf("describe security groups: %v", err)
	}
	return out.SecurityGroups
}

func describeGroup(t *testing.T, c *awsec2.Client, id string) types.SecurityGroup {
	t.Helper()
	groups := describeGroups(t, c, &awsec2.DescribeSecurityGroupsInput{GroupIds: []string{id}})
	if len(groups) != 1 {
		t.Fatalf("describe security group %s = %+v", id, groups)
	}
	return groups[0]
}

func describeRules(t *testing.T, c *awsec2.Client, filters ...types.Filter) []types.SecurityGroupRule {
	t.Helper()
	out, err := c.DescribeSecurityGroupRules(t.Context(), &awsec2.DescribeSecurityGroupRulesInput{Filters: filters})
	if err != nil {
		t.Fatalf("describe security group rules: %v", err)
	}
	return out.SecurityGroupRules
}

func tcp(port int32, cidr, description string) types.IpPermission {
	return types.IpPermission{
		IpProtocol: aws.String("tcp"), FromPort: aws.Int32(port), ToPort: aws.Int32(port),
		IpRanges: []types.IpRange{{CidrIp: aws.String(cidr), Description: aws.String(description)}},
	}
}

func fromGroup(group string) types.IpPermission {
	return types.IpPermission{IpProtocol: aws.String("-1"), UserIdGroupPairs: []types.UserIdGroupPair{{GroupId: aws.String(group)}}}
}

func deleteGroup(t *testing.T, c *awsec2.Client, id string) error {
	t.Helper()
	_, err := c.DeleteSecurityGroup(t.Context(), &awsec2.DeleteSecurityGroupInput{GroupId: aws.String(id)})
	return err
}

func TestSecurityGroupPairNamingItsVPCAndAccount(t *testing.T) {
	c := newClient(t, ec2.New())
	vpc := createVPC(t, c, "10.0.0.0/16")
	web, db := createGroup(t, c, vpc, "web"), createGroup(t, c, vpc, "db")
	_, err := c.AuthorizeSecurityGroupIngress(t.Context(), &awsec2.AuthorizeSecurityGroupIngressInput{
		GroupId: aws.String(db),
		IpPermissions: []types.IpPermission{{
			IpProtocol:       aws.String("-1"),
			UserIdGroupPairs: []types.UserIdGroupPair{{GroupId: aws.String(web), VpcId: aws.String(vpc), UserId: aws.String("123456789012")}},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	pairs := describeGroup(t, c, db).IpPermissions
	if len(pairs) != 1 || len(pairs[0].UserIdGroupPairs) != 1 || aws.ToString(pairs[0].UserIdGroupPairs[0].GroupId) != web {
		t.Errorf("db ingress = %+v, want all traffic from web", pairs)
	}
}

func TestSecurityGroupLifecycle(t *testing.T) {
	s := ec2.New()
	c := newClient(t, s)
	ctx := t.Context()
	vpc := createVPC(t, c, "10.0.0.0/16")

	defaults := describeGroups(t, c, &awsec2.DescribeSecurityGroupsInput{Filters: []types.Filter{filter("vpc-id", vpc), filter("group-name", "default")}})
	if len(defaults) != 1 {
		t.Fatalf("default groups = %+v", defaults)
	}
	def := defaults[0]
	defID := aws.ToString(def.GroupId)
	if len(def.IpPermissions) != 1 || aws.ToString(def.IpPermissions[0].IpProtocol) != "-1" ||
		len(def.IpPermissions[0].UserIdGroupPairs) != 1 || aws.ToString(def.IpPermissions[0].UserIdGroupPairs[0].GroupId) != defID {
		t.Errorf("default group ingress = %+v, want all traffic from itself", def.IpPermissions)
	}
	if len(def.IpPermissionsEgress) != 1 || aws.ToString(def.IpPermissionsEgress[0].IpRanges[0].CidrIp) != "0.0.0.0/0" {
		t.Errorf("default group egress = %+v, want all traffic to anywhere", def.IpPermissionsEgress)
	}

	created, err := c.CreateSecurityGroup(ctx, &awsec2.CreateSecurityGroupInput{
		GroupName: aws.String("web"), Description: aws.String("web servers"), VpcId: aws.String(vpc),
		TagSpecifications: tagSpec(types.ResourceTypeSecurityGroup, "Name", "web", "session", "s1"),
	})
	if err != nil {
		t.Fatal(err)
	}
	web := aws.ToString(created.GroupId)
	if !strings.HasPrefix(web, "sg-") || !strings.HasSuffix(aws.ToString(created.SecurityGroupArn), "security-group/"+web) || tags(created.Tags)["Name"] != "web" {
		t.Fatalf("create security group = %+v", created)
	}
	if g := describeGroup(t, c, web); aws.ToString(g.GroupName) != "web" || aws.ToString(g.Description) != "web servers" || aws.ToString(g.VpcId) != vpc ||
		len(g.IpPermissions) != 0 || len(g.IpPermissionsEgress) != 1 {
		t.Fatalf("new group = %+v, want no ingress and the allow-all egress rule", g)
	}

	authorized, err := c.AuthorizeSecurityGroupIngress(ctx, &awsec2.AuthorizeSecurityGroupIngressInput{
		GroupId: aws.String(web),
		IpPermissions: []types.IpPermission{
			tcp(22, "10.0.0.0/8", "ssh"),
			{
				IpProtocol: aws.String("tcp"), FromPort: aws.Int32(443), ToPort: aws.Int32(443),
				IpRanges:   []types.IpRange{{CidrIp: aws.String("0.0.0.0/0")}},
				Ipv6Ranges: []types.Ipv6Range{{CidrIpv6: aws.String("::/0"), Description: aws.String("https v6")}},
			},
			fromGroup(defID),
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !aws.ToBool(authorized.Return) || len(authorized.SecurityGroupRules) != 4 {
		t.Fatalf("authorize ingress = %+v", authorized)
	}
	byPeer := map[string]types.SecurityGroupRule{}
	for _, r := range authorized.SecurityGroupRules {
		if !strings.HasPrefix(aws.ToString(r.SecurityGroupRuleId), "sgr-") || aws.ToString(r.GroupId) != web || aws.ToBool(r.IsEgress) {
			t.Errorf("rule = %+v", r)
		}
		peer := aws.ToString(r.CidrIpv4) + aws.ToString(r.CidrIpv6)
		if r.ReferencedGroupInfo != nil {
			peer = aws.ToString(r.ReferencedGroupInfo.GroupId)
		}
		byPeer[peer] = r
	}
	if r := byPeer["10.0.0.0/8"]; aws.ToString(r.Description) != "ssh" || aws.ToInt32(r.FromPort) != 22 || aws.ToString(r.IpProtocol) != "tcp" {
		t.Errorf("ssh rule = %+v", r)
	}
	if r := byPeer[defID]; aws.ToString(r.IpProtocol) != "-1" || aws.ToInt32(r.FromPort) != -1 {
		t.Errorf("rule from the default group = %+v", r)
	}

	g := describeGroup(t, c, web)
	if len(g.IpPermissions) != 3 {
		t.Fatalf("ingress permissions = %+v, want one per protocol and port range", g.IpPermissions)
	}
	https := g.IpPermissions[1]
	if aws.ToInt32(https.FromPort) != 443 || len(https.IpRanges) != 1 || len(https.Ipv6Ranges) != 1 || aws.ToString(https.Ipv6Ranges[0].Description) != "https v6" {
		t.Errorf("https permission = %+v", https)
	}

	if _, err := c.AuthorizeSecurityGroupEgress(ctx, &awsec2.AuthorizeSecurityGroupEgressInput{
		GroupId: aws.String(web), IpPermissions: []types.IpPermission{tcp(5432, "10.0.1.0/24", "database")},
	}); err != nil {
		t.Fatal(err)
	}
	rules := describeRules(t, c, filter("group-id", web))
	if len(rules) != 6 {
		t.Fatalf("rules of the group = %d, want 6", len(rules))
	}
	one := describeRules(t, c, filter("security-group-rule-id", aws.ToString(byPeer["::/0"].SecurityGroupRuleId)))
	if len(one) != 1 || aws.ToString(one[0].CidrIpv6) != "::/0" {
		t.Errorf("rule by ID = %+v", one)
	}
	var paged int
	for pages := awsec2.NewDescribeSecurityGroupRulesPaginator(c, &awsec2.DescribeSecurityGroupRulesInput{MaxResults: aws.Int32(5)}); pages.HasMorePages(); {
		page, err := pages.NextPage(ctx)
		if err != nil {
			t.Fatal(err)
		}
		paged += len(page.SecurityGroupRules)
	}
	if paged != 8 {
		t.Errorf("paged through %d rules, want 8 with the default group's", paged)
	}

	revoked, err := c.RevokeSecurityGroupIngress(ctx, &awsec2.RevokeSecurityGroupIngressInput{
		GroupId: aws.String(web), IpPermissions: []types.IpPermission{tcp(22, "10.0.0.0/8", "")},
	})
	if err != nil || len(revoked.RevokedSecurityGroupRules) != 1 || aws.ToString(revoked.RevokedSecurityGroupRules[0].SecurityGroupRuleId) != aws.ToString(byPeer["10.0.0.0/8"].SecurityGroupRuleId) {
		t.Fatalf("revoke by permission = %+v, %v", revoked, err)
	}
	if _, err := c.RevokeSecurityGroupIngress(ctx, &awsec2.RevokeSecurityGroupIngressInput{
		GroupId: aws.String(web), SecurityGroupRuleIds: []string{aws.ToString(byPeer["::/0"].SecurityGroupRuleId)},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.RevokeSecurityGroupEgress(ctx, &awsec2.RevokeSecurityGroupEgressInput{
		GroupId: aws.String(web), IpPermissions: []types.IpPermission{{IpProtocol: aws.String("-1"), IpRanges: []types.IpRange{{CidrIp: aws.String("0.0.0.0/0")}}}},
	}); err != nil {
		t.Fatal(err)
	}
	held := s.SecurityGroups()
	i := slices.IndexFunc(held, func(g ec2.SecurityGroup) bool { return g.GroupID == web })
	if i < 0 || len(held[i].Rules) != 3 || held[i].Tags["session"] != "s1" {
		t.Fatalf("SecurityGroups() = %+v", held)
	}
	for _, r := range held[i].Rules {
		if r.CIDRIPv6 == "::/0" || r.CIDRIPv4 == "10.0.0.0/8" || r.Egress && r.CIDRIPv4 == "0.0.0.0/0" {
			t.Errorf("revoked rule %+v is still held", r)
		}
	}

	wantCode(t, "delete a vpc with a security group", deleteVPC(t, c, vpc), "DependencyViolation")
	wantCode(t, "delete the default group", deleteGroup(t, c, defID), "CannotDelete")
	if err := deleteGroup(t, c, web); err != nil {
		t.Fatal(err)
	}
	if err := deleteVPC(t, c, vpc); err != nil {
		t.Fatalf("delete the vpc with only its default group: %v", err)
	}
	if got := s.SecurityGroups(); len(got) != 0 {
		t.Errorf("deleting a vpc left security groups: %+v", got)
	}
}

func TestDescribeSecurityGroups(t *testing.T) {
	s := ec2.New()
	c := newClient(t, s)
	ctx := t.Context()
	a := createVPC(t, c, "10.0.0.0/16")
	b := createVPC(t, c, "10.1.0.0/16")
	web := createGroup(t, c, a, "web", "tier", "front")
	createGroup(t, c, a, "db", "tier", "back", "keep", "")
	createGroup(t, c, b, "web")

	for _, tc := range []struct {
		name string
		in   *awsec2.DescribeSecurityGroupsInput
		want int
	}{
		{"all", &awsec2.DescribeSecurityGroupsInput{}, 5},
		{"by vpc", &awsec2.DescribeSecurityGroupsInput{Filters: []types.Filter{filter("vpc-id", a)}}, 3},
		{"by name filter", &awsec2.DescribeSecurityGroupsInput{Filters: []types.Filter{filter("group-name", "w*")}}, 2},
		{"by name filter in a vpc", &awsec2.DescribeSecurityGroupsInput{Filters: []types.Filter{filter("group-name", "web"), filter("vpc-id", b)}}, 1},
		{"by id filter", &awsec2.DescribeSecurityGroupsInput{Filters: []types.Filter{filter("group-id", web)}}, 1},
		{"by tag", &awsec2.DescribeSecurityGroupsInput{Filters: []types.Filter{filter("tag:tier", "front", "back")}}, 2},
		{"by tag key", &awsec2.DescribeSecurityGroupsInput{Filters: []types.Filter{filter("tag-key", "keep")}}, 1},
		{"by id", &awsec2.DescribeSecurityGroupsInput{GroupIds: []string{web}}, 1},
		{"by name", &awsec2.DescribeSecurityGroupsInput{GroupNames: []string{"db"}}, 1},
	} {
		if got := describeGroups(t, c, tc.in); len(got) != tc.want {
			t.Errorf("%s = %d groups, want %d", tc.name, len(got), tc.want)
		}
	}

	var seen []string
	for pages := awsec2.NewDescribeSecurityGroupsPaginator(c, &awsec2.DescribeSecurityGroupsInput{MaxResults: aws.Int32(5)}); pages.HasMorePages(); {
		page, err := pages.NextPage(ctx)
		if err != nil {
			t.Fatal(err)
		}
		for _, g := range page.SecurityGroups {
			seen = append(seen, aws.ToString(g.GroupId))
		}
	}
	all := s.SecurityGroups()
	if len(seen) != len(all) {
		t.Errorf("paged through %d groups, want %d", len(seen), len(all))
	}
}

func TestSecurityGroupsOfInterfacesAndInstances(t *testing.T) {
	s := ec2.New()
	c := newClient(t, s)
	ctx := t.Context()
	vpc := createVPC(t, c, "10.0.0.0/16")
	sub := createSubnet(t, c, vpc, "10.0.0.0/24", "")
	web := createGroup(t, c, vpc, "web")
	app := createGroup(t, c, vpc, "app")
	def := aws.ToString(describeGroups(t, c, &awsec2.DescribeSecurityGroupsInput{Filters: []types.Filter{filter("vpc-id", vpc), filter("group-name", "default")}})[0].GroupId)

	plain, err := c.CreateNetworkInterface(ctx, &awsec2.CreateNetworkInterfaceInput{SubnetId: aws.String(sub)})
	if err != nil {
		t.Fatal(err)
	}
	if g := plain.NetworkInterface.Groups; len(g) != 1 || aws.ToString(g[0].GroupId) != def {
		t.Errorf("groups of an interface created with none = %+v, want the vpc's default group", g)
	}
	ni, err := c.CreateNetworkInterface(ctx, &awsec2.CreateNetworkInterfaceInput{SubnetId: aws.String(sub), Groups: []string{web}})
	if err != nil {
		t.Fatal(err)
	}
	wantCode(t, "delete a group an interface uses", deleteGroup(t, c, web), "DependencyViolation")
	if _, err := c.DeleteNetworkInterface(ctx, &awsec2.DeleteNetworkInterfaceInput{NetworkInterfaceId: ni.NetworkInterface.NetworkInterfaceId}); err != nil {
		t.Fatal(err)
	}

	image := putImage(t, s, ec2.Image{Name: "base"})
	in := runInput(image)
	in.SubnetId, in.SecurityGroupIds = aws.String(sub), []string{app}
	inst := run(t, c, in)[0]
	if len(inst.SecurityGroups) != 1 || aws.ToString(inst.SecurityGroups[0].GroupId) != app {
		t.Errorf("instance groups = %+v", inst.SecurityGroups)
	}
	wantCode(t, "delete a group an instance uses", deleteGroup(t, c, app), "DependencyViolation")
	if _, err := c.TerminateInstances(ctx, &awsec2.TerminateInstancesInput{InstanceIds: []string{aws.ToString(inst.InstanceId)}}); err != nil {
		t.Fatal(err)
	}
	describeInstance(t, c, aws.ToString(inst.InstanceId))

	if _, err := c.AuthorizeSecurityGroupIngress(ctx, &awsec2.AuthorizeSecurityGroupIngressInput{GroupId: aws.String(web), IpPermissions: []types.IpPermission{fromGroup(app)}}); err != nil {
		t.Fatal(err)
	}
	wantCode(t, "delete a group another group's rule refers to", deleteGroup(t, c, app), "DependencyViolation")
	if err := deleteGroup(t, c, web); err != nil {
		t.Fatal(err)
	}
	if err := deleteGroup(t, c, app); err != nil {
		t.Fatalf("delete a group once nothing uses it: %v", err)
	}

	untracked, err := c.CreateNetworkInterface(ctx, &awsec2.CreateNetworkInterfaceInput{SubnetId: aws.String(subnet), Groups: []string{"sg-1"}})
	if err != nil || len(untracked.NetworkInterface.Groups) != 1 {
		t.Errorf("interface in a subnet the server does not hold = %+v, %v", untracked, err)
	}
}

func TestSecurityGroupErrors(t *testing.T) {
	s := ec2.New()
	c := newClient(t, s)
	ctx := t.Context()
	vpc := createVPC(t, c, "10.0.0.0/16")
	other := createVPC(t, c, "10.9.0.0/16")
	sub := createSubnet(t, c, vpc, "10.0.0.0/24", "")
	web := createGroup(t, c, vpc, "web")
	foreign := createGroup(t, c, other, "foreign")
	if _, err := c.AuthorizeSecurityGroupIngress(ctx, &awsec2.AuthorizeSecurityGroupIngressInput{
		GroupId: aws.String(web), IpPermissions: []types.IpPermission{tcp(22, "10.0.0.0/8", "ssh")},
	}); err != nil {
		t.Fatal(err)
	}
	image := putImage(t, s, ec2.Image{Name: "base"})
	unknown := "sg-0000000000000dead"

	for _, tc := range []struct {
		name string
		code string
		call func() error
	}{
		{"a name already in the vpc", "InvalidGroup.Duplicate", func() error {
			_, err := c.CreateSecurityGroup(ctx, &awsec2.CreateSecurityGroupInput{GroupName: aws.String("web"), Description: aws.String("again"), VpcId: aws.String(vpc)})
			return err
		}},
		{"the reserved name", "InvalidGroup.Reserved", func() error {
			_, err := c.CreateSecurityGroup(ctx, &awsec2.CreateSecurityGroupInput{GroupName: aws.String("default"), Description: aws.String("d"), VpcId: aws.String(vpc)})
			return err
		}},
		{"a group in an unknown vpc", "InvalidVpcID.NotFound", func() error {
			_, err := c.CreateSecurityGroup(ctx, &awsec2.CreateSecurityGroupInput{GroupName: aws.String("x"), Description: aws.String("x"), VpcId: aws.String("vpc-0000000000000dead")})
			return err
		}},
		{"a group with no vpc", "VPCIdNotSpecified", func() error {
			_, err := c.CreateSecurityGroup(ctx, &awsec2.CreateSecurityGroupInput{GroupName: aws.String("x"), Description: aws.String("x")})
			return err
		}},
		{"describe an unknown group", "InvalidGroup.NotFound", func() error {
			_, err := c.DescribeSecurityGroups(ctx, &awsec2.DescribeSecurityGroupsInput{GroupIds: []string{unknown}})
			return err
		}},
		{"describe an unknown group name", "InvalidGroup.NotFound", func() error {
			_, err := c.DescribeSecurityGroups(ctx, &awsec2.DescribeSecurityGroupsInput{GroupNames: []string{"missing"}})
			return err
		}},
		{"describe an unknown rule", "InvalidSecurityGroupRuleId.NotFound", func() error {
			_, err := c.DescribeSecurityGroupRules(ctx, &awsec2.DescribeSecurityGroupRulesInput{SecurityGroupRuleIds: []string{"sgr-0000000000000dead"}})
			return err
		}},
		{"an unsupported rule filter", "InvalidParameterValue", func() error {
			_, err := c.DescribeSecurityGroupRules(ctx, &awsec2.DescribeSecurityGroupRulesInput{Filters: []types.Filter{filter("tag:Name", "x")}})
			return err
		}},
		{"a rule that exists", "InvalidPermission.Duplicate", func() error {
			_, err := c.AuthorizeSecurityGroupIngress(ctx, &awsec2.AuthorizeSecurityGroupIngressInput{GroupId: aws.String(web), IpPermissions: []types.IpPermission{tcp(22, "10.0.0.0/8", "other words")}})
			return err
		}},
		{"a rule on an unknown group", "InvalidGroup.NotFound", func() error {
			_, err := c.AuthorizeSecurityGroupIngress(ctx, &awsec2.AuthorizeSecurityGroupIngressInput{GroupId: aws.String(unknown), IpPermissions: []types.IpPermission{tcp(22, "10.0.0.0/8", "")}})
			return err
		}},
		{"a rule from a group of another vpc", "InvalidParameterValue", func() error {
			_, err := c.AuthorizeSecurityGroupIngress(ctx, &awsec2.AuthorizeSecurityGroupIngressInput{GroupId: aws.String(web), IpPermissions: []types.IpPermission{fromGroup(foreign)}})
			return err
		}},
		{"a rule from an unknown group", "InvalidGroup.NotFound", func() error {
			_, err := c.AuthorizeSecurityGroupIngress(ctx, &awsec2.AuthorizeSecurityGroupIngressInput{GroupId: aws.String(web), IpPermissions: []types.IpPermission{fromGroup(unknown)}})
			return err
		}},
		{"a reversed port range", "InvalidParameterValue", func() error {
			_, err := c.AuthorizeSecurityGroupIngress(ctx, &awsec2.AuthorizeSecurityGroupIngressInput{GroupId: aws.String(web), IpPermissions: []types.IpPermission{{
				IpProtocol: aws.String("tcp"), FromPort: aws.Int32(90), ToPort: aws.Int32(80), IpRanges: []types.IpRange{{CidrIp: aws.String("10.0.0.0/8")}},
			}}})
			return err
		}},
		{"a malformed cidr", "InvalidParameterValue", func() error {
			_, err := c.AuthorizeSecurityGroupIngress(ctx, &awsec2.AuthorizeSecurityGroupIngressInput{GroupId: aws.String(web), IpPermissions: []types.IpPermission{tcp(80, "10.0.0.0", "")}})
			return err
		}},
		{"revoke a rule that does not exist", "InvalidPermission.NotFound", func() error {
			_, err := c.RevokeSecurityGroupIngress(ctx, &awsec2.RevokeSecurityGroupIngressInput{GroupId: aws.String(web), IpPermissions: []types.IpPermission{tcp(23, "10.0.0.0/8", "")}})
			return err
		}},
		{"revoke an unknown rule ID", "InvalidSecurityGroupRuleId.NotFound", func() error {
			_, err := c.RevokeSecurityGroupIngress(ctx, &awsec2.RevokeSecurityGroupIngressInput{GroupId: aws.String(web), SecurityGroupRuleIds: []string{"sgr-0000000000000dead"}})
			return err
		}},
		{"delete an unknown group", "InvalidGroup.NotFound", func() error { return deleteGroup(t, c, unknown) }},
		{"an interface with an unknown group", "InvalidGroup.NotFound", func() error {
			_, err := c.CreateNetworkInterface(ctx, &awsec2.CreateNetworkInterfaceInput{SubnetId: aws.String(sub), Groups: []string{unknown}})
			return err
		}},
		{"an interface with a group of another vpc", "InvalidParameter", func() error {
			_, err := c.CreateNetworkInterface(ctx, &awsec2.CreateNetworkInterfaceInput{SubnetId: aws.String(sub), Groups: []string{foreign}})
			return err
		}},
		{"an instance with an unknown group", "InvalidGroup.NotFound", func() error {
			in := runInput(image)
			in.SubnetId, in.SecurityGroupIds = aws.String(sub), []string{unknown}
			_, err := c.RunInstances(ctx, in)
			return err
		}},
	} {
		t.Run(tc.name, func(t *testing.T) { wantCode(t, tc.name, tc.call(), tc.code) })
	}
	if len(s.NetworkInterfaces()) != 0 || len(s.Instances()) != 0 {
		t.Errorf("a refused launch left interfaces or instances behind")
	}
}

func TestSecurityGroupFaults(t *testing.T) {
	faults := fault.New().
		Add(fault.Rule{Method: "ec2.CreateSecurityGroup", Nth: 1, Fault: fault.LoseResponse()}).
		Add(fault.Rule{Method: "ec2.DeleteSecurityGroup", Nth: 1, Fault: fault.Reject(http.StatusBadRequest, "DependencyViolation")})
	s := ec2.New(ec2.WithFaults(faults))
	c := newClient(t, s)
	vpc := createVPC(t, c, "10.0.0.0/16")

	if _, err := c.CreateSecurityGroup(t.Context(), &awsec2.CreateSecurityGroupInput{GroupName: aws.String("web"), Description: aws.String("web"), VpcId: aws.String(vpc)}); err == nil {
		t.Fatal("create security group with a lost response succeeded")
	}
	i := slices.IndexFunc(s.SecurityGroups(), func(g ec2.SecurityGroup) bool { return g.GroupName == "web" })
	if i < 0 {
		t.Fatalf("SecurityGroups() after a lost response = %+v", s.SecurityGroups())
	}
	web := s.SecurityGroups()[i]
	if len(web.Rules) != 1 || !web.Rules[0].Egress || web.Rules[0].CIDRIPv4 != "0.0.0.0/0" {
		t.Errorf("rules of the new group = %+v", web.Rules)
	}
	if injected := faults.Injected(); len(injected) != 1 || injected[0].Call.Resource != region+"/"+web.GroupID {
		t.Errorf("injected = %v", injected)
	}
	wantCode(t, "delete a group the fault rejects", deleteGroup(t, c, web.GroupID), "DependencyViolation")
	if len(s.SecurityGroups()) != 2 {
		t.Errorf("a rejected delete deleted the group")
	}
}

func TestSecurityGroupPolicy(t *testing.T) {
	s := ec2.New(ec2.WithPolicy(access.Allow("ec2:CreateVpc", "ec2:CreateSecurityGroup")))
	c := newClient(t, s)
	vpc := createVPC(t, c, "10.0.0.0/16")
	web := createGroup(t, c, vpc, "web")
	_, err := c.CreateSecurityGroup(t.Context(), &awsec2.CreateSecurityGroupInput{
		GroupName: aws.String("tagged"), Description: aws.String("tagged"), VpcId: aws.String(vpc),
		TagSpecifications: tagSpec(types.ResourceTypeSecurityGroup, "Name", "tagged"),
	})
	if sdkerr.AWSCode(err) != "UnauthorizedOperation" || !strings.Contains(err.Error(), "ec2:CreateTags") {
		t.Errorf("tagged group = %v, want UnauthorizedOperation naming ec2:CreateTags", err)
	}
	_, err = c.AuthorizeSecurityGroupIngress(t.Context(), &awsec2.AuthorizeSecurityGroupIngressInput{
		GroupId: aws.String(web), IpPermissions: []types.IpPermission{tcp(22, "10.0.0.0/8", "")},
	})
	if sdkerr.AWSCode(err) != "UnauthorizedOperation" || !strings.Contains(err.Error(), "ec2:AuthorizeSecurityGroupIngress") {
		t.Errorf("authorize ingress = %v, want UnauthorizedOperation naming ec2:AuthorizeSecurityGroupIngress", err)
	}
}
