package ec2_test

import (
	"net/http"
	"net/netip"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsec2 "github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/ec2/types"

	"github.com/parallelworks/squall/access"
	"github.com/parallelworks/squall/aws/ec2"
	"github.com/parallelworks/squall/fault"
	"github.com/parallelworks/squall/sdktest/internal/sdkerr"
)

func optionalTags(resource types.ResourceType, kv ...string) []types.TagSpecification {
	if len(kv) == 0 {
		return nil
	}
	return tagSpec(resource, kv...)
}

func createVPC(t *testing.T, c *awsec2.Client, cidr string, kv ...string) string {
	t.Helper()
	out, err := c.CreateVpc(t.Context(), &awsec2.CreateVpcInput{
		CidrBlock: aws.String(cidr), TagSpecifications: optionalTags(types.ResourceTypeVpc, kv...),
	})
	if err != nil {
		t.Fatalf("create vpc: %v", err)
	}
	return aws.ToString(out.Vpc.VpcId)
}

func createSubnet(t *testing.T, c *awsec2.Client, vpc, cidr, zone string, kv ...string) string {
	t.Helper()
	in := &awsec2.CreateSubnetInput{
		VpcId: aws.String(vpc), CidrBlock: aws.String(cidr), TagSpecifications: optionalTags(types.ResourceTypeSubnet, kv...),
	}
	if zone != "" {
		in.AvailabilityZone = aws.String(zone)
	}
	out, err := c.CreateSubnet(t.Context(), in)
	if err != nil {
		t.Fatalf("create subnet: %v", err)
	}
	return aws.ToString(out.Subnet.SubnetId)
}

func createAttachedGateway(t *testing.T, c *awsec2.Client, vpc string) string {
	t.Helper()
	out, err := c.CreateInternetGateway(t.Context(), &awsec2.CreateInternetGatewayInput{})
	if err != nil {
		t.Fatalf("create internet gateway: %v", err)
	}
	id := aws.ToString(out.InternetGateway.InternetGatewayId)
	if _, err := c.AttachInternetGateway(t.Context(), &awsec2.AttachInternetGatewayInput{InternetGatewayId: aws.String(id), VpcId: aws.String(vpc)}); err != nil {
		t.Fatalf("attach internet gateway: %v", err)
	}
	return id
}

func createNAT(t *testing.T, c *awsec2.Client, subnet, allocation string, kv ...string) types.NatGateway {
	t.Helper()
	out, err := c.CreateNatGateway(t.Context(), &awsec2.CreateNatGatewayInput{
		SubnetId: aws.String(subnet), AllocationId: aws.String(allocation), TagSpecifications: optionalTags(types.ResourceTypeNatgateway, kv...),
	})
	if err != nil {
		t.Fatalf("create nat gateway: %v", err)
	}
	return *out.NatGateway
}

func describeNAT(t *testing.T, c *awsec2.Client, id string) types.NatGateway {
	t.Helper()
	out, err := c.DescribeNatGateways(t.Context(), &awsec2.DescribeNatGatewaysInput{NatGatewayIds: []string{id}})
	if err != nil || len(out.NatGateways) != 1 {
		t.Fatalf("describe nat gateway %s = %v, %v", id, out, err)
	}
	return out.NatGateways[0]
}

func describeRouteTables(t *testing.T, c *awsec2.Client, filters ...types.Filter) []types.RouteTable {
	t.Helper()
	out, err := c.DescribeRouteTables(t.Context(), &awsec2.DescribeRouteTablesInput{Filters: filters})
	if err != nil {
		t.Fatalf("describe route tables: %v", err)
	}
	return out.RouteTables
}

func seedTransitGateway(t *testing.T, s *ec2.Server) string {
	t.Helper()
	tgw, err := s.PutTransitGateway(region, ec2.TransitGateway{Description: "shared"})
	if err != nil {
		t.Fatal(err)
	}
	return tgw.TransitGatewayID
}

func wantCode(t *testing.T, what string, err error, code string) {
	t.Helper()
	if sdkerr.AWSCode(err) != code || sdkerr.AWSStatus(err) != http.StatusBadRequest {
		t.Errorf("%s = %v, want 400 %s", what, err, code)
	}
}

func TestVPCNetworkCreateAndTeardown(t *testing.T) {
	s := ec2.New()
	c := newClient(t, s)
	ctx := t.Context()

	created, err := c.CreateVpc(ctx, &awsec2.CreateVpcInput{
		CidrBlock: aws.String("10.20.0.0/16"), TagSpecifications: tagSpec(types.ResourceTypeVpc, "Name", "net", "session", "s1"),
	})
	if err != nil {
		t.Fatal(err)
	}
	vpc := aws.ToString(created.Vpc.VpcId)
	if !strings.HasPrefix(vpc, "vpc-") || created.Vpc.State != types.VpcStatePending || tags(created.Vpc.Tags)["Name"] != "net" {
		t.Fatalf("create vpc = %+v", created.Vpc)
	}
	if err := awsec2.NewVpcAvailableWaiter(c).Wait(ctx, &awsec2.DescribeVpcsInput{VpcIds: []string{vpc}}, time.Second); err != nil {
		t.Fatalf("wait for vpc: %v", err)
	}
	for _, in := range []*awsec2.ModifyVpcAttributeInput{
		{VpcId: aws.String(vpc), EnableDnsSupport: &types.AttributeBooleanValue{Value: aws.Bool(true)}},
		{VpcId: aws.String(vpc), EnableDnsHostnames: &types.AttributeBooleanValue{Value: aws.Bool(true)}},
	} {
		if _, err := c.ModifyVpcAttribute(ctx, in); err != nil {
			t.Fatalf("modify vpc attribute: %v", err)
		}
	}
	attr, err := c.DescribeVpcAttribute(ctx, &awsec2.DescribeVpcAttributeInput{VpcId: aws.String(vpc), Attribute: types.VpcAttributeNameEnableDnsHostnames})
	if err != nil || !aws.ToBool(attr.EnableDnsHostnames.Value) {
		t.Fatalf("describe vpc attribute = %+v, %v", attr, err)
	}

	public := createSubnet(t, c, vpc, "10.20.0.0/24", region+"a", "tier", "public")
	private := createSubnet(t, c, vpc, "10.20.1.0/24", region+"b", "tier", "private")
	subnets, err := c.DescribeSubnets(ctx, &awsec2.DescribeSubnetsInput{Filters: []types.Filter{filter("vpc-id", vpc)}})
	if err != nil || len(subnets.Subnets) != 2 {
		t.Fatalf("describe subnets = %+v, %v", subnets, err)
	}
	if sub := subnets.Subnets[0]; aws.ToString(sub.AvailabilityZoneId) != "use1-az1" || aws.ToInt32(sub.AvailableIpAddressCount) != 251 {
		t.Errorf("subnet = %+v", sub)
	}

	igw := createAttachedGateway(t, c, vpc)
	eip := allocate(t, c)
	nat := createNAT(t, c, public, aws.ToString(eip.AllocationId), "Name", "egress")
	if nat.State != types.NatGatewayStatePending || aws.ToString(nat.VpcId) != vpc {
		t.Fatalf("create nat = %+v", nat)
	}
	if err := awsec2.NewNatGatewayAvailableWaiter(c).Wait(ctx, &awsec2.DescribeNatGatewaysInput{NatGatewayIds: []string{aws.ToString(nat.NatGatewayId)}}, time.Second); err != nil {
		t.Fatalf("wait for nat gateway: %v", err)
	}
	nat = describeNAT(t, c, aws.ToString(nat.NatGatewayId))
	natAddress := nat.NatGatewayAddresses[0]
	if aws.ToString(natAddress.PublicIp) != aws.ToString(eip.PublicIp) || !netip.MustParsePrefix("10.20.0.0/24").Contains(netip.MustParseAddr(aws.ToString(natAddress.PrivateIp))) {
		t.Errorf("nat address = %+v", natAddress)
	}
	addresses, err := describeAddresses(t, c, &awsec2.DescribeAddressesInput{AllocationIds: []string{aws.ToString(eip.AllocationId)}})
	if err != nil || aws.ToString(addresses[0].NetworkInterfaceId) != aws.ToString(natAddress.NetworkInterfaceId) || aws.ToString(addresses[0].AssociationId) == "" {
		t.Errorf("the nat gateway's address = %+v, %v", addresses, err)
	}

	publicRT, err := c.CreateRouteTable(ctx, &awsec2.CreateRouteTableInput{VpcId: aws.String(vpc), TagSpecifications: tagSpec(types.ResourceTypeRouteTable, "tier", "public")})
	if err != nil {
		t.Fatal(err)
	}
	publicRTID := aws.ToString(publicRT.RouteTable.RouteTableId)
	if _, err := c.CreateRoute(ctx, &awsec2.CreateRouteInput{RouteTableId: aws.String(publicRTID), DestinationCidrBlock: aws.String("0.0.0.0/0"), GatewayId: aws.String(igw)}); err != nil {
		t.Fatal(err)
	}
	publicAssoc, err := c.AssociateRouteTable(ctx, &awsec2.AssociateRouteTableInput{RouteTableId: aws.String(publicRTID), SubnetId: aws.String(public)})
	if err != nil || publicAssoc.AssociationState.State != types.RouteTableAssociationStateCodeAssociated {
		t.Fatalf("associate route table = %+v, %v", publicAssoc, err)
	}
	privateRT, err := c.CreateRouteTable(ctx, &awsec2.CreateRouteTableInput{VpcId: aws.String(vpc)})
	if err != nil {
		t.Fatal(err)
	}
	privateRTID := aws.ToString(privateRT.RouteTable.RouteTableId)
	if _, err := c.CreateRoute(ctx, &awsec2.CreateRouteInput{RouteTableId: aws.String(privateRTID), DestinationCidrBlock: aws.String("0.0.0.0/0"), NatGatewayId: nat.NatGatewayId}); err != nil {
		t.Fatal(err)
	}
	privateAssoc, err := c.AssociateRouteTable(ctx, &awsec2.AssociateRouteTableInput{RouteTableId: aws.String(privateRTID), SubnetId: aws.String(private)})
	if err != nil {
		t.Fatal(err)
	}
	tables := describeRouteTables(t, c, filter("vpc-id", vpc))
	if len(tables) != 3 {
		t.Fatalf("route tables = %d, want the main table and two more", len(tables))
	}
	mains := describeRouteTables(t, c, filter("vpc-id", vpc), filter("association.main", "true"))
	if len(mains) != 1 || aws.ToString(mains[0].Routes[0].GatewayId) != "local" || aws.ToString(mains[0].Routes[0].DestinationCidrBlock) != "10.20.0.0/16" {
		t.Fatalf("main route table = %+v", mains)
	}
	byPublicSubnet := describeRouteTables(t, c, filter("association.subnet-id", public))
	if len(byPublicSubnet) != 1 || aws.ToString(byPublicSubnet[0].RouteTableId) != publicRTID {
		t.Fatalf("route table of the public subnet = %+v", byPublicSubnet)
	}
	if r := byPublicSubnet[0].Routes[1]; aws.ToString(r.GatewayId) != igw || r.State != types.RouteStateActive || r.Origin != types.RouteOriginCreateRoute {
		t.Errorf("internet route = %+v", r)
	}

	dhcp, err := c.CreateDhcpOptions(ctx, &awsec2.CreateDhcpOptionsInput{DhcpConfigurations: []types.NewDhcpConfiguration{
		{Key: aws.String("domain-name"), Values: []string{"corp.internal"}},
		{Key: aws.String("domain-name-servers"), Values: []string{"AmazonProvidedDNS"}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	dhcpID := aws.ToString(dhcp.DhcpOptions.DhcpOptionsId)
	if _, err := c.AssociateDhcpOptions(ctx, &awsec2.AssociateDhcpOptionsInput{DhcpOptionsId: aws.String(dhcpID), VpcId: aws.String(vpc)}); err != nil {
		t.Fatal(err)
	}
	vpcs, err := c.DescribeVpcs(ctx, &awsec2.DescribeVpcsInput{Filters: []types.Filter{filter("tag:session", "s1")}})
	if err != nil || len(vpcs.Vpcs) != 1 || aws.ToString(vpcs.Vpcs[0].DhcpOptionsId) != dhcpID || vpcs.Vpcs[0].State != types.VpcStateAvailable {
		t.Fatalf("describe vpcs = %+v, %v", vpcs, err)
	}

	tgw := seedTransitGateway(t, s)
	attachment, err := c.CreateTransitGatewayVpcAttachment(ctx, &awsec2.CreateTransitGatewayVpcAttachmentInput{
		TransitGatewayId: aws.String(tgw), VpcId: aws.String(vpc), SubnetIds: []string{private},
		Options:           &types.CreateTransitGatewayVpcAttachmentRequestOptions{DnsSupport: types.DnsSupportValueEnable},
		TagSpecifications: tagSpec(types.ResourceTypeTransitGatewayAttachment, "Name", "hub"),
	})
	if err != nil {
		t.Fatal(err)
	}
	attachmentID := aws.ToString(attachment.TransitGatewayVpcAttachment.TransitGatewayAttachmentId)
	if attachment.TransitGatewayVpcAttachment.State != types.TransitGatewayAttachmentStatePending ||
		!slices.Equal(attachment.TransitGatewayVpcAttachment.SubnetIds, []string{private}) {
		t.Fatalf("create attachment = %+v", attachment.TransitGatewayVpcAttachment)
	}
	vpcAttachments, err := c.DescribeTransitGatewayVpcAttachments(ctx, &awsec2.DescribeTransitGatewayVpcAttachmentsInput{
		Filters: []types.Filter{filter("vpc-id", vpc), filter("tag:Name", "hub")},
	})
	if err != nil || len(vpcAttachments.TransitGatewayVpcAttachments) != 1 ||
		vpcAttachments.TransitGatewayVpcAttachments[0].State != types.TransitGatewayAttachmentStateAvailable {
		t.Fatalf("describe vpc attachments = %+v, %v", vpcAttachments, err)
	}
	attachments, err := c.DescribeTransitGatewayAttachments(ctx, &awsec2.DescribeTransitGatewayAttachmentsInput{
		Filters: []types.Filter{filter("resource-id", vpc), filter("resource-type", "vpc")},
	})
	if err != nil || len(attachments.TransitGatewayAttachments) != 1 || aws.ToString(attachments.TransitGatewayAttachments[0].TransitGatewayId) != tgw {
		t.Fatalf("describe attachments = %+v, %v", attachments, err)
	}
	if _, err := c.CreateRoute(ctx, &awsec2.CreateRouteInput{RouteTableId: aws.String(privateRTID), DestinationCidrBlock: aws.String("172.16.0.0/12"), TransitGatewayId: aws.String(tgw)}); err != nil {
		t.Fatal(err)
	}

	deleting, err := c.DeleteTransitGatewayVpcAttachment(ctx, &awsec2.DeleteTransitGatewayVpcAttachmentInput{TransitGatewayAttachmentId: aws.String(attachmentID)})
	if err != nil || deleting.TransitGatewayVpcAttachment.State != types.TransitGatewayAttachmentStateDeleting {
		t.Fatalf("delete attachment = %+v, %v", deleting, err)
	}
	wantCode(t, "delete a subnet while an attachment in it is deleting", deleteSubnet(t, c, private), "DependencyViolation")
	gone, err := c.DescribeTransitGatewayVpcAttachments(ctx, &awsec2.DescribeTransitGatewayVpcAttachmentsInput{TransitGatewayAttachmentIds: []string{attachmentID}})
	if err != nil || gone.TransitGatewayVpcAttachments[0].State != types.TransitGatewayAttachmentStateDeleted {
		t.Fatalf("describe deleted attachment = %+v, %v", gone, err)
	}
	if _, err := c.DeleteNatGateway(ctx, &awsec2.DeleteNatGatewayInput{NatGatewayId: nat.NatGatewayId}); err != nil {
		t.Fatal(err)
	}
	if err := awsec2.NewNatGatewayDeletedWaiter(c).Wait(ctx, &awsec2.DescribeNatGatewaysInput{NatGatewayIds: []string{aws.ToString(nat.NatGatewayId)}}, time.Second); err != nil {
		t.Fatalf("wait for nat gateway deletion: %v", err)
	}
	for _, r := range describeRouteTables(t, c, filter("route-table-id", privateRTID))[0].Routes[1:] {
		if r.State != types.RouteStateBlackhole {
			t.Errorf("route to %s after its target went = %s, want blackhole", aws.ToString(r.DestinationCidrBlock), r.State)
		}
	}
	if _, err := c.ReleaseAddress(ctx, &awsec2.ReleaseAddressInput{AllocationId: eip.AllocationId}); err != nil {
		t.Fatalf("release the nat gateway's address once it is deleted: %v", err)
	}
	for _, assoc := range []*string{publicAssoc.AssociationId, privateAssoc.AssociationId} {
		if _, err := c.DisassociateRouteTable(ctx, &awsec2.DisassociateRouteTableInput{AssociationId: assoc}); err != nil {
			t.Fatal(err)
		}
	}
	for _, id := range []string{publicRTID, privateRTID} {
		if _, err := c.DeleteRouteTable(ctx, &awsec2.DeleteRouteTableInput{RouteTableId: aws.String(id)}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := c.DetachInternetGateway(ctx, &awsec2.DetachInternetGatewayInput{InternetGatewayId: aws.String(igw), VpcId: aws.String(vpc)}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.DeleteInternetGateway(ctx, &awsec2.DeleteInternetGatewayInput{InternetGatewayId: aws.String(igw)}); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{public, private} {
		if _, err := c.DeleteSubnet(ctx, &awsec2.DeleteSubnetInput{SubnetId: aws.String(id)}); err != nil {
			t.Fatalf("delete subnet %s: %v", id, err)
		}
	}
	if _, err := c.DeleteVpc(ctx, &awsec2.DeleteVpcInput{VpcId: aws.String(vpc)}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.DeleteDhcpOptions(ctx, &awsec2.DeleteDhcpOptionsInput{DhcpOptionsId: aws.String(dhcpID)}); err != nil {
		t.Fatal(err)
	}

	if len(s.VPCs()) != 0 || len(s.Subnets()) != 0 || len(s.InternetGateways()) != 0 || len(s.RouteTables()) != 0 || len(s.NetworkInterfaces()) != 0 {
		t.Errorf("left after teardown: %+v %+v %+v %+v %+v", s.VPCs(), s.Subnets(), s.InternetGateways(), s.RouteTables(), s.NetworkInterfaces())
	}
	if nats := s.NATGateways(); len(nats) != 1 || nats[0].State != "deleted" {
		t.Errorf("NATGateways() = %+v, want the deleted gateway", nats)
	}
	if got := s.TransitGatewayAttachments(); len(got) != 1 || got[0].State != "deleted" {
		t.Errorf("TransitGatewayAttachments() = %+v, want the deleted attachment", got)
	}
	if got := s.DHCPOptions(); len(got) != 1 || got[0].Configurations["domain-name"][0] != "ec2.internal" {
		t.Errorf("DHCPOptions() = %+v, want the region's default set", got)
	}
}

func TestVPCDependencyViolations(t *testing.T) {
	s := ec2.New()
	c := newClient(t, s)
	ctx := t.Context()
	vpc := createVPC(t, c, "10.0.0.0/16")
	sub := createSubnet(t, c, vpc, "10.0.0.0/24", "")
	wantCode(t, "delete a vpc with a subnet", deleteVPC(t, c, vpc), "DependencyViolation")

	ni, err := c.CreateNetworkInterface(ctx, &awsec2.CreateNetworkInterfaceInput{SubnetId: aws.String(sub)})
	if err != nil {
		t.Fatal(err)
	}
	if got := ni.NetworkInterface; aws.ToString(got.VpcId) != vpc || aws.ToString(got.PrivateIpAddress) != "10.0.0.4" || aws.ToString(got.AvailabilityZone) != region+"a" {
		t.Errorf("interface in a vpc subnet = %+v", got)
	}
	wantCode(t, "delete a subnet with an interface", deleteSubnet(t, c, sub), "DependencyViolation")
	if _, err := c.DeleteNetworkInterface(ctx, &awsec2.DeleteNetworkInterfaceInput{NetworkInterfaceId: ni.NetworkInterface.NetworkInterfaceId}); err != nil {
		t.Fatal(err)
	}

	igw := createAttachedGateway(t, c, vpc)
	_, err = c.DeleteInternetGateway(ctx, &awsec2.DeleteInternetGatewayInput{InternetGatewayId: aws.String(igw)})
	wantCode(t, "delete an attached internet gateway", err, "DependencyViolation")

	eip := allocate(t, c)
	nat := createNAT(t, c, sub, aws.ToString(eip.AllocationId))
	_, err = c.DetachInternetGateway(ctx, &awsec2.DetachInternetGatewayInput{InternetGatewayId: aws.String(igw), VpcId: aws.String(vpc)})
	wantCode(t, "detach a gateway while a nat gateway maps a public address", err, "DependencyViolation")
	wantCode(t, "delete a subnet with a nat gateway", deleteSubnet(t, c, sub), "DependencyViolation")
	_, err = c.ReleaseAddress(ctx, &awsec2.ReleaseAddressInput{AllocationId: eip.AllocationId})
	wantCode(t, "release a nat gateway's address", err, "InvalidIPAddress.InUse")
	if _, err := c.DeleteNatGateway(ctx, &awsec2.DeleteNatGatewayInput{NatGatewayId: nat.NatGatewayId}); err != nil {
		t.Fatal(err)
	}
	wantCode(t, "delete a subnet while its nat gateway is deleting", deleteSubnet(t, c, sub), "DependencyViolation")
	if got := describeNAT(t, c, aws.ToString(nat.NatGatewayId)); got.State != types.NatGatewayStateDeleted {
		t.Fatalf("nat gateway = %s, want deleted", got.State)
	}

	rt, err := c.CreateRouteTable(ctx, &awsec2.CreateRouteTableInput{VpcId: aws.String(vpc)})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.AssociateRouteTable(ctx, &awsec2.AssociateRouteTableInput{RouteTableId: rt.RouteTable.RouteTableId, SubnetId: aws.String(sub)}); err != nil {
		t.Fatal(err)
	}
	_, err = c.DeleteRouteTable(ctx, &awsec2.DeleteRouteTableInput{RouteTableId: rt.RouteTable.RouteTableId})
	wantCode(t, "delete an associated route table", err, "DependencyViolation")
	main := describeRouteTables(t, c, filter("vpc-id", vpc), filter("association.main", "true"))[0]
	_, err = c.DeleteRouteTable(ctx, &awsec2.DeleteRouteTableInput{RouteTableId: main.RouteTableId})
	wantCode(t, "delete the main route table", err, "DependencyViolation")
	_, err = c.DisassociateRouteTable(ctx, &awsec2.DisassociateRouteTableInput{AssociationId: main.Associations[0].RouteTableAssociationId})
	wantCode(t, "disassociate the main route table", err, "InvalidParameterValue")

	if err := deleteSubnet(t, c, sub); err != nil {
		t.Fatalf("delete the subnet once nothing is in it: %v", err)
	}
	if got := describeRouteTables(t, c, filter("route-table-id", aws.ToString(rt.RouteTable.RouteTableId))); len(got[0].Associations) != 0 {
		t.Errorf("deleting a subnet left its route table association: %+v", got[0].Associations)
	}
	wantCode(t, "delete a vpc with an attached gateway and a route table", deleteVPC(t, c, vpc), "DependencyViolation")
	if _, err := c.DetachInternetGateway(ctx, &awsec2.DetachInternetGatewayInput{InternetGatewayId: aws.String(igw), VpcId: aws.String(vpc)}); err != nil {
		t.Fatal(err)
	}
	wantCode(t, "delete a vpc with a route table", deleteVPC(t, c, vpc), "DependencyViolation")
	if _, err := c.DeleteRouteTable(ctx, &awsec2.DeleteRouteTableInput{RouteTableId: rt.RouteTable.RouteTableId}); err != nil {
		t.Fatal(err)
	}
	dopt := s.VPCs()[0].DHCPOptionsID
	_, err = c.DeleteDhcpOptions(ctx, &awsec2.DeleteDhcpOptionsInput{DhcpOptionsId: aws.String(dopt)})
	wantCode(t, "delete dhcp options a vpc uses", err, "DependencyViolation")
	if err := deleteVPC(t, c, vpc); err != nil {
		t.Fatalf("delete the empty vpc: %v", err)
	}
	if len(s.RouteTables()) != 0 {
		t.Errorf("deleting a vpc left route tables: %+v", s.RouteTables())
	}
}

func deleteVPC(t *testing.T, c *awsec2.Client, id string) error {
	t.Helper()
	_, err := c.DeleteVpc(t.Context(), &awsec2.DeleteVpcInput{VpcId: aws.String(id)})
	return err
}

func deleteSubnet(t *testing.T, c *awsec2.Client, id string) error {
	t.Helper()
	_, err := c.DeleteSubnet(t.Context(), &awsec2.DeleteSubnetInput{SubnetId: aws.String(id)})
	return err
}

func TestInstancesInAVPCSubnet(t *testing.T) {
	s := ec2.New()
	c := newClient(t, s)
	vpc := createVPC(t, c, "10.1.0.0/16")
	sub := createSubnet(t, c, vpc, "10.1.2.0/28", region+"c")
	image := putImage(t, s, ec2.Image{Name: "base"})

	in := runInput(image)
	in.SubnetId = aws.String(sub)
	inst := run(t, c, in)[0]
	if aws.ToString(inst.VpcId) != vpc || aws.ToString(inst.Placement.AvailabilityZone) != region+"c" || aws.ToString(inst.PrivateIpAddress) != "10.1.2.4" {
		t.Errorf("instance = vpc %s zone %s ip %s", aws.ToString(inst.VpcId), aws.ToString(inst.Placement.AvailabilityZone), aws.ToString(inst.PrivateIpAddress))
	}
	found, err := describeInstances(t, c, &awsec2.DescribeInstancesInput{Filters: []types.Filter{filter("vpc-id", vpc)}})
	if err != nil || len(found) != 1 {
		t.Errorf("instances by vpc-id = %v, %v", found, err)
	}
	wantCode(t, "delete a subnet with an instance", deleteSubnet(t, c, sub), "DependencyViolation")

	full := runInput(image)
	full.SubnetId, full.MinCount, full.MaxCount = aws.String(sub), aws.Int32(11), aws.Int32(11)
	_, err = c.RunInstances(t.Context(), full)
	wantCode(t, "launch more instances than the subnet has addresses", err, "InsufficientFreeAddressesInSubnet")
	outside := runInput(image)
	outside.SubnetId, outside.PrivateIpAddress = aws.String(sub), aws.String("10.1.3.4")
	_, err = c.RunInstances(t.Context(), outside)
	wantCode(t, "launch with an address outside the subnet", err, "InvalidParameterValue")

	if _, err := c.TerminateInstances(t.Context(), &awsec2.TerminateInstancesInput{InstanceIds: []string{aws.ToString(inst.InstanceId)}}); err != nil {
		t.Fatal(err)
	}
	describeInstance(t, c, aws.ToString(inst.InstanceId))
	if err := deleteSubnet(t, c, sub); err != nil {
		t.Fatalf("delete the subnet once its instance terminated: %v", err)
	}
}

func TestAddressNeedsAnInternetGateway(t *testing.T) {
	s := ec2.New()
	c := newClient(t, s)
	vpc := createVPC(t, c, "10.0.0.0/16")
	sub := createSubnet(t, c, vpc, "10.0.0.0/24", "")
	ni, err := c.CreateNetworkInterface(t.Context(), &awsec2.CreateNetworkInterfaceInput{SubnetId: aws.String(sub)})
	if err != nil {
		t.Fatal(err)
	}
	eip := aws.ToString(allocate(t, c).AllocationId)
	_, err = associate(t, c, eip, aws.ToString(ni.NetworkInterface.NetworkInterfaceId), false)
	wantCode(t, "associate an address in a vpc with no gateway", err, "Gateway.NotAttached")
	createAttachedGateway(t, c, vpc)
	if _, err := associate(t, c, eip, aws.ToString(ni.NetworkInterface.NetworkInterfaceId), false); err != nil {
		t.Fatal(err)
	}
}

func TestNATGatewayFailures(t *testing.T) {
	s := ec2.New()
	c := newClient(t, s)
	vpc := createVPC(t, c, "10.0.0.0/16")
	sub := createSubnet(t, c, vpc, "10.0.0.0/24", "")
	eip := aws.ToString(allocate(t, c).AllocationId)

	noGateway := createNAT(t, c, sub, eip)
	if got := describeNAT(t, c, aws.ToString(noGateway.NatGatewayId)); got.State != types.NatGatewayStateFailed || aws.ToString(got.FailureCode) != "Gateway.NotAttached" {
		t.Errorf("nat gateway without an internet gateway = %s %s", got.State, aws.ToString(got.FailureCode))
	}
	err := awsec2.NewNatGatewayAvailableWaiter(c).Wait(t.Context(), &awsec2.DescribeNatGatewaysInput{NatGatewayIds: []string{aws.ToString(noGateway.NatGatewayId)}}, time.Second)
	if err == nil {
		t.Error("the available waiter returned for a failed nat gateway")
	}

	createAttachedGateway(t, c, vpc)
	first := createNAT(t, c, sub, eip)
	second := createNAT(t, c, sub, eip)
	if got := describeNAT(t, c, aws.ToString(first.NatGatewayId)); got.State != types.NatGatewayStateAvailable {
		t.Errorf("first nat gateway = %s", got.State)
	}
	if got := describeNAT(t, c, aws.ToString(second.NatGatewayId)); got.State != types.NatGatewayStateFailed || aws.ToString(got.FailureCode) != "Resource.AlreadyAssociated" {
		t.Errorf("nat gateway on an associated address = %s %s", got.State, aws.ToString(got.FailureCode))
	}

	_, err = c.CreateNatGateway(t.Context(), &awsec2.CreateNatGatewayInput{SubnetId: aws.String("subnet-0000000000000dead"), AllocationId: aws.String(eip)})
	wantCode(t, "nat gateway in an unknown subnet", err, "InvalidSubnetID.NotFound")
	_, err = c.CreateNatGateway(t.Context(), &awsec2.CreateNatGatewayInput{SubnetId: aws.String(sub), AllocationId: aws.String("eipalloc-0000000000000dead")})
	wantCode(t, "nat gateway with an unknown address", err, "InvalidAllocationID.NotFound")
	_, err = c.DeleteNatGateway(t.Context(), &awsec2.DeleteNatGatewayInput{NatGatewayId: aws.String("nat-0000000000000dead")})
	wantCode(t, "delete an unknown nat gateway", err, "NatGatewayNotFound")

	private, err := c.CreateNatGateway(t.Context(), &awsec2.CreateNatGatewayInput{SubnetId: aws.String(sub), ConnectivityType: types.ConnectivityTypePrivate})
	if err != nil || private.NatGateway.ConnectivityType != types.ConnectivityTypePrivate || aws.ToString(private.NatGateway.NatGatewayAddresses[0].PublicIp) != "" {
		t.Errorf("private nat gateway = %+v, %v", private, err)
	}
}

func TestVPCErrors(t *testing.T) {
	s := ec2.New()
	c := newClient(t, s)
	ctx := t.Context()
	vpc := createVPC(t, c, "10.0.0.0/16")
	other := createVPC(t, c, "10.9.0.0/16")
	sub := createSubnet(t, c, vpc, "10.0.0.0/24", "")
	otherSub := createSubnet(t, c, other, "10.9.0.0/24", "")
	igw := createAttachedGateway(t, c, vpc)
	rt, err := c.CreateRouteTable(ctx, &awsec2.CreateRouteTableInput{VpcId: aws.String(vpc)})
	if err != nil {
		t.Fatal(err)
	}
	rtID := rt.RouteTable.RouteTableId
	if _, err := c.AssociateRouteTable(ctx, &awsec2.AssociateRouteTableInput{RouteTableId: rtID, SubnetId: aws.String(sub)}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.CreateRoute(ctx, &awsec2.CreateRouteInput{RouteTableId: rtID, DestinationCidrBlock: aws.String("0.0.0.0/0"), GatewayId: aws.String(igw)}); err != nil {
		t.Fatal(err)
	}
	tgw := seedTransitGateway(t, s)
	if _, err := c.CreateTransitGatewayVpcAttachment(ctx, &awsec2.CreateTransitGatewayVpcAttachmentInput{
		TransitGatewayId: aws.String(tgw), VpcId: aws.String(vpc), SubnetIds: []string{sub},
	}); err != nil {
		t.Fatal(err)
	}
	unknownVPC, unknownSubnet := aws.String("vpc-0000000000000dead"), aws.String("subnet-0000000000000dead")

	for _, tc := range []struct {
		name string
		code string
		call func() error
	}{
		{"a vpc smaller than /28", "InvalidVpc.Range", func() error {
			_, err := c.CreateVpc(ctx, &awsec2.CreateVpcInput{CidrBlock: aws.String("10.0.0.0/29")})
			return err
		}},
		{"a vpc with no cidr", "MissingParameter", func() error {
			_, err := c.CreateVpc(ctx, &awsec2.CreateVpcInput{})
			return err
		}},
		{"describe an unknown vpc", "InvalidVpcID.NotFound", func() error {
			_, err := c.DescribeVpcs(ctx, &awsec2.DescribeVpcsInput{VpcIds: []string{*unknownVPC}})
			return err
		}},
		{"both vpc attributes at once", "InvalidParameterCombination", func() error {
			_, err := c.ModifyVpcAttribute(ctx, &awsec2.ModifyVpcAttributeInput{
				VpcId: aws.String(vpc), EnableDnsSupport: &types.AttributeBooleanValue{Value: aws.Bool(true)},
				EnableDnsHostnames: &types.AttributeBooleanValue{Value: aws.Bool(true)},
			})
			return err
		}},
		{"a subnet in an unknown vpc", "InvalidVpcID.NotFound", func() error {
			_, err := c.CreateSubnet(ctx, &awsec2.CreateSubnetInput{VpcId: unknownVPC, CidrBlock: aws.String("10.0.1.0/24")})
			return err
		}},
		{"a subnet outside its vpc", "InvalidSubnet.Range", func() error {
			_, err := c.CreateSubnet(ctx, &awsec2.CreateSubnetInput{VpcId: aws.String(vpc), CidrBlock: aws.String("10.1.0.0/24")})
			return err
		}},
		{"an overlapping subnet", "InvalidSubnet.Conflict", func() error {
			_, err := c.CreateSubnet(ctx, &awsec2.CreateSubnetInput{VpcId: aws.String(vpc), CidrBlock: aws.String("10.0.0.128/25")})
			return err
		}},
		{"a subnet in an unknown zone", "InvalidParameterValue", func() error {
			_, err := c.CreateSubnet(ctx, &awsec2.CreateSubnetInput{VpcId: aws.String(vpc), CidrBlock: aws.String("10.0.1.0/24"), AvailabilityZone: aws.String(region + "z")})
			return err
		}},
		{"describe an unknown subnet", "InvalidSubnetID.NotFound", func() error {
			_, err := c.DescribeSubnets(ctx, &awsec2.DescribeSubnetsInput{SubnetIds: []string{*unknownSubnet}})
			return err
		}},
		{"delete an unknown subnet", "InvalidSubnetID.NotFound", func() error { return deleteSubnet(t, c, *unknownSubnet) }},
		{"attach an attached gateway", "Resource.AlreadyAssociated", func() error {
			_, err := c.AttachInternetGateway(ctx, &awsec2.AttachInternetGatewayInput{InternetGatewayId: aws.String(igw), VpcId: aws.String(other)})
			return err
		}},
		{"detach a gateway from a vpc it is not attached to", "Gateway.NotAttached", func() error {
			_, err := c.DetachInternetGateway(ctx, &awsec2.DetachInternetGatewayInput{InternetGatewayId: aws.String(igw), VpcId: aws.String(other)})
			return err
		}},
		{"attach an unknown gateway", "InvalidInternetGatewayID.NotFound", func() error {
			_, err := c.AttachInternetGateway(ctx, &awsec2.AttachInternetGatewayInput{InternetGatewayId: aws.String("igw-0000000000000dead"), VpcId: aws.String(vpc)})
			return err
		}},
		{"associate an associated subnet", "Resource.AlreadyAssociated", func() error {
			_, err := c.AssociateRouteTable(ctx, &awsec2.AssociateRouteTableInput{RouteTableId: rtID, SubnetId: aws.String(sub)})
			return err
		}},
		{"associate a subnet of another vpc", "InvalidParameterValue", func() error {
			_, err := c.AssociateRouteTable(ctx, &awsec2.AssociateRouteTableInput{RouteTableId: rtID, SubnetId: aws.String(otherSub)})
			return err
		}},
		{"disassociate an unknown association", "InvalidAssociationID.NotFound", func() error {
			_, err := c.DisassociateRouteTable(ctx, &awsec2.DisassociateRouteTableInput{AssociationId: aws.String("rtbassoc-0000000000000dead")})
			return err
		}},
		{"a route that exists", "RouteAlreadyExists", func() error {
			_, err := c.CreateRoute(ctx, &awsec2.CreateRouteInput{RouteTableId: rtID, DestinationCidrBlock: aws.String("0.0.0.0/0"), GatewayId: aws.String(igw)})
			return err
		}},
		{"a route to an unknown gateway", "InvalidGatewayID.NotFound", func() error {
			_, err := c.CreateRoute(ctx, &awsec2.CreateRouteInput{RouteTableId: rtID, DestinationCidrBlock: aws.String("1.0.0.0/8"), GatewayId: aws.String("igw-0000000000000dead")})
			return err
		}},
		{"a route to an unknown nat gateway", "NatGatewayNotFound", func() error {
			_, err := c.CreateRoute(ctx, &awsec2.CreateRouteInput{RouteTableId: rtID, DestinationCidrBlock: aws.String("1.0.0.0/8"), NatGatewayId: aws.String("nat-0000000000000dead")})
			return err
		}},
		{"a route inside the vpc", "InvalidParameterValue", func() error {
			_, err := c.CreateRoute(ctx, &awsec2.CreateRouteInput{RouteTableId: rtID, DestinationCidrBlock: aws.String("10.0.5.0/24"), GatewayId: aws.String(igw)})
			return err
		}},
		{"a route in an unknown table", "InvalidRouteTableID.NotFound", func() error {
			_, err := c.CreateRoute(ctx, &awsec2.CreateRouteInput{RouteTableId: aws.String("rtb-0000000000000dead"), DestinationCidrBlock: aws.String("1.0.0.0/8"), GatewayId: aws.String(igw)})
			return err
		}},
		{"a route table in an unknown vpc", "InvalidVpcID.NotFound", func() error {
			_, err := c.CreateRouteTable(ctx, &awsec2.CreateRouteTableInput{VpcId: unknownVPC})
			return err
		}},
		{"an unknown dhcp option", "InvalidParameterValue", func() error {
			_, err := c.CreateDhcpOptions(ctx, &awsec2.CreateDhcpOptionsInput{DhcpConfigurations: []types.NewDhcpConfiguration{{Key: aws.String("router"), Values: []string{"10.0.0.1"}}}})
			return err
		}},
		{"associate unknown dhcp options", "InvalidDhcpOptionID.NotFound", func() error {
			_, err := c.AssociateDhcpOptions(ctx, &awsec2.AssociateDhcpOptionsInput{DhcpOptionsId: aws.String("dopt-0000000000000dead"), VpcId: aws.String(vpc)})
			return err
		}},
		{"attach to an unknown transit gateway", "InvalidTransitGatewayID.NotFound", func() error {
			_, err := c.CreateTransitGatewayVpcAttachment(ctx, &awsec2.CreateTransitGatewayVpcAttachmentInput{
				TransitGatewayId: aws.String("tgw-0000000000000dead"), VpcId: aws.String(other), SubnetIds: []string{otherSub},
			})
			return err
		}},
		{"attach a vpc twice", "DuplicateTransitGatewayAttachment", func() error {
			_, err := c.CreateTransitGatewayVpcAttachment(ctx, &awsec2.CreateTransitGatewayVpcAttachmentInput{
				TransitGatewayId: aws.String(tgw), VpcId: aws.String(vpc), SubnetIds: []string{sub},
			})
			return err
		}},
		{"attach a subnet of another vpc", "InvalidParameterValue", func() error {
			_, err := c.CreateTransitGatewayVpcAttachment(ctx, &awsec2.CreateTransitGatewayVpcAttachmentInput{
				TransitGatewayId: aws.String(tgw), VpcId: aws.String(other), SubnetIds: []string{sub},
			})
			return err
		}},
		{"delete an unknown attachment", "InvalidTransitGatewayAttachmentID.NotFound", func() error {
			_, err := c.DeleteTransitGatewayVpcAttachment(ctx, &awsec2.DeleteTransitGatewayVpcAttachmentInput{TransitGatewayAttachmentId: aws.String("tgw-attach-0000000000000dead")})
			return err
		}},
		{"an unsupported filter", "InvalidParameterValue", func() error {
			_, err := c.DescribeSubnets(ctx, &awsec2.DescribeSubnetsInput{Filters: []types.Filter{filter("ipv6-cidr-block-association.ipv6-cidr-block", "::/0")}})
			return err
		}},
	} {
		t.Run(tc.name, func(t *testing.T) { wantCode(t, tc.name, tc.call(), tc.code) })
	}
}

func TestVPCFilters(t *testing.T) {
	s := ec2.New()
	c := newClient(t, s)
	ctx := t.Context()
	a := createVPC(t, c, "10.0.0.0/16", "env", "a")
	b := createVPC(t, c, "10.1.0.0/16", "env", "b", "keep", "")
	createSubnet(t, c, a, "10.0.0.0/24", region+"a", "Name", "a1")
	createSubnet(t, c, a, "10.0.1.0/24", region+"b", "Name", "a2")
	createSubnet(t, c, b, "10.1.0.0/24", region+"a", "Name", "b1")
	igw := createAttachedGateway(t, c, b)

	for _, tc := range []struct {
		filters []types.Filter
		want    int
	}{
		{[]types.Filter{filter("vpc-id", a)}, 2},
		{[]types.Filter{filter("availability-zone", region+"a")}, 2},
		{[]types.Filter{filter("tag:Name", "a*")}, 2},
		{[]types.Filter{filter("vpc-id", a), filter("tag:Name", "*2")}, 1},
		{[]types.Filter{filter("cidr-block", "10.1.0.0/24")}, 1},
		{[]types.Filter{filter("state", "available")}, 3},
	} {
		out, err := c.DescribeSubnets(ctx, &awsec2.DescribeSubnetsInput{Filters: tc.filters})
		if err != nil || len(out.Subnets) != tc.want {
			t.Errorf("subnets matching %v = %d, %v; want %d", tc.filters, len(out.Subnets), err, tc.want)
		}
	}
	vpcs, err := c.DescribeVpcs(ctx, &awsec2.DescribeVpcsInput{Filters: []types.Filter{filter("tag-key", "keep")}})
	if err != nil || len(vpcs.Vpcs) != 1 || aws.ToString(vpcs.Vpcs[0].VpcId) != b {
		t.Errorf("vpcs with tag key keep = %+v, %v", vpcs, err)
	}
	gateways, err := c.DescribeInternetGateways(ctx, &awsec2.DescribeInternetGatewaysInput{Filters: []types.Filter{filter("attachment.vpc-id", b)}})
	if err != nil || len(gateways.InternetGateways) != 1 || aws.ToString(gateways.InternetGateways[0].InternetGatewayId) != igw {
		t.Errorf("gateways attached to b = %+v, %v", gateways, err)
	}
	if gateways, err = c.DescribeInternetGateways(ctx, &awsec2.DescribeInternetGatewaysInput{Filters: []types.Filter{filter("attachment.vpc-id", a)}}); err != nil || len(gateways.InternetGateways) != 0 {
		t.Errorf("gateways attached to a = %+v, %v", gateways, err)
	}

	pages := awsec2.NewDescribeSubnetsPaginator(c, &awsec2.DescribeSubnetsInput{MaxResults: aws.Int32(5)})
	var seen int
	for pages.HasMorePages() {
		page, err := pages.NextPage(ctx)
		if err != nil {
			t.Fatal(err)
		}
		seen += len(page.Subnets)
	}
	if seen != 3 {
		t.Errorf("paged through %d subnets, want 3", seen)
	}
}

func TestAvailabilityZones(t *testing.T) {
	c := newClient(t, ec2.New(ec2.WithAvailabilityZones(region, region+"a", region+"d")))
	out, err := c.DescribeAvailabilityZones(t.Context(), &awsec2.DescribeAvailabilityZonesInput{})
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, z := range out.AvailabilityZones {
		got = append(got, aws.ToString(z.ZoneName)+"="+aws.ToString(z.ZoneId))
		if z.State != types.AvailabilityZoneStateAvailable || aws.ToString(z.RegionName) != region {
			t.Errorf("zone = %+v", z)
		}
	}
	if want := []string{region + "a=use1-az1", region + "d=use1-az2"}; !slices.Equal(got, want) {
		t.Errorf("zones = %v, want %v", got, want)
	}
	byID, err := c.DescribeAvailabilityZones(t.Context(), &awsec2.DescribeAvailabilityZonesInput{ZoneIds: []string{"use1-az2"}})
	if err != nil || len(byID.AvailabilityZones) != 1 || aws.ToString(byID.AvailabilityZones[0].ZoneName) != region+"d" {
		t.Errorf("zone use1-az2 = %+v, %v", byID, err)
	}
	vpc := createVPC(t, c, "10.0.0.0/16")
	sub, err := c.CreateSubnet(t.Context(), &awsec2.CreateSubnetInput{VpcId: aws.String(vpc), CidrBlock: aws.String("10.0.0.0/24"), AvailabilityZoneId: aws.String("use1-az2")})
	if err != nil || aws.ToString(sub.Subnet.AvailabilityZone) != region+"d" {
		t.Errorf("subnet by zone ID = %+v, %v", sub, err)
	}
	_, err = c.CreateSubnet(t.Context(), &awsec2.CreateSubnetInput{VpcId: aws.String(vpc), CidrBlock: aws.String("10.0.1.0/24"), AvailabilityZone: aws.String(region + "b")})
	wantCode(t, "a subnet in a zone the region does not have", err, "InvalidParameterValue")
}

func TestVPCNetworkFaults(t *testing.T) {
	faults := fault.New().
		Add(fault.Rule{Method: "ec2.CreateVpc", Nth: 1, Fault: fault.LoseResponse()}).
		Add(fault.Rule{Method: "ec2.CreateNatGateway", Nth: 1, Fault: fault.FailAfter(http.StatusInternalServerError)}).
		Add(fault.Rule{Method: "ec2.DeleteSubnet", Nth: 1, Fault: fault.Reject(http.StatusBadRequest, "DependencyViolation")})
	s := ec2.New(ec2.WithFaults(faults))
	c := newClient(t, s)
	ctx := t.Context()

	if _, err := c.CreateVpc(ctx, &awsec2.CreateVpcInput{CidrBlock: aws.String("10.0.0.0/16")}); err == nil {
		t.Fatal("create vpc with a lost response succeeded")
	}
	vpcs := s.VPCs()
	if len(vpcs) != 1 || vpcs[0].CIDRBlock != "10.0.0.0/16" || !vpcs[0].EnableDNSSupport || vpcs[0].EnableDNSHostnames {
		t.Fatalf("VPCs() after a lost response = %+v", vpcs)
	}
	vpc := vpcs[0].VPCID
	sub := createSubnet(t, c, vpc, "10.0.0.0/24", "")
	createAttachedGateway(t, c, vpc)
	eip := allocate(t, c)

	retrying := clientAt(serve(t, s), region, withRetries(3))
	out, err := retrying.CreateNatGateway(ctx, &awsec2.CreateNatGatewayInput{SubnetId: aws.String(sub), AllocationId: eip.AllocationId})
	if err != nil {
		t.Fatalf("create nat gateway with a retry: %v", err)
	}
	if nats := s.NATGateways(); len(nats) != 1 || nats[0].NATGatewayID != aws.ToString(out.NatGateway.NatGatewayId) {
		t.Fatalf("NATGateways() = %+v, want the one the retry with the same client token found", nats)
	}

	injected := faults.Injected()
	if len(injected) != 2 || injected[0].Call.Resource != region+"/"+vpc || injected[1].Call.Resource != region+"/"+aws.ToString(out.NatGateway.NatGatewayId) {
		t.Errorf("injected = %v", injected)
	}

	err = deleteSubnet(t, c, sub)
	wantCode(t, "delete a subnet the fault rejects", err, "DependencyViolation")
	if len(s.Subnets()) != 1 {
		t.Errorf("a rejected delete deleted the subnet")
	}
}

func TestNATGatewaysSettleOnlyWhenTold(t *testing.T) {
	s := ec2.New(ec2.WithSettleAfter(-1))
	c := newClient(t, s)
	vpc := createVPC(t, c, "10.0.0.0/16")
	sub := createSubnet(t, c, vpc, "10.0.0.0/24", "")
	createAttachedGateway(t, c, vpc)
	nat := createNAT(t, c, sub, aws.ToString(allocate(t, c).AllocationId))
	if got := describeNAT(t, c, aws.ToString(nat.NatGatewayId)); got.State != types.NatGatewayStatePending || got.NatGatewayAddresses[0].Status != types.NatGatewayAddressStatusAssociating {
		t.Fatalf("nat gateway = %s %s, want pending and associating", got.State, got.NatGatewayAddresses[0].Status)
	}
	s.Settle()
	if got := describeNAT(t, c, aws.ToString(nat.NatGatewayId)); got.State != types.NatGatewayStateAvailable {
		t.Fatalf("nat gateway after Settle = %s", got.State)
	}
}

func TestVPCPolicyNeedsCreateTagsForPluralTagSpecifications(t *testing.T) {
	s := ec2.New(ec2.WithPolicy(access.Allow("ec2:CreateVpc", "ec2:CreateSubnet", "ec2:CreateTransitGatewayVpcAttachment")))
	c := newClient(t, s)
	vpc := createVPC(t, c, "10.0.0.0/16")
	sub := createSubnet(t, c, vpc, "10.0.0.0/24", "")
	tgw := seedTransitGateway(t, s)
	_, err := c.CreateTransitGatewayVpcAttachment(t.Context(), &awsec2.CreateTransitGatewayVpcAttachmentInput{
		TransitGatewayId: aws.String(tgw), VpcId: aws.String(vpc), SubnetIds: []string{sub},
		TagSpecifications: tagSpec(types.ResourceTypeTransitGatewayAttachment, "Name", "hub"),
	})
	if sdkerr.AWSCode(err) != "UnauthorizedOperation" || !strings.Contains(err.Error(), "ec2:CreateTags") {
		t.Fatalf("tagged attachment = %v, want UnauthorizedOperation naming ec2:CreateTags", err)
	}
}

func TestTagsOnNetworkResources(t *testing.T) {
	s := ec2.New()
	c := newClient(t, s)
	vpc := createVPC(t, c, "10.0.0.0/16")
	sub := createSubnet(t, c, vpc, "10.0.0.0/24", "")
	igw := createAttachedGateway(t, c, vpc)
	tgw := seedTransitGateway(t, s)
	ids := []string{vpc, sub, igw, tgw, aws.ToString(describeRouteTables(t, c)[0].RouteTableId)}
	if _, err := c.CreateTags(t.Context(), &awsec2.CreateTagsInput{Resources: ids, Tags: []types.Tag{{Key: aws.String("owner"), Value: aws.String("team")}}}); err != nil {
		t.Fatal(err)
	}
	if tags := s.VPCs()[0].Tags; tags["owner"] != "team" {
		t.Errorf("vpc tags = %v", tags)
	}
	if tags := s.TransitGateways()[0].Tags; tags["owner"] != "team" {
		t.Errorf("transit gateway tags = %v", tags)
	}
	_, err := c.CreateTags(t.Context(), &awsec2.CreateTagsInput{Resources: []string{"subnet-0000000000000dead"}, Tags: []types.Tag{{Key: aws.String("k"), Value: aws.String("v")}}})
	wantCode(t, "tag an unknown subnet", err, "InvalidSubnetID.NotFound")
}
