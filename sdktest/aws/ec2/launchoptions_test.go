package ec2_test

import (
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsec2 "github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/ec2/types"

	"github.com/parallelworks/squall/aws/ec2"
)

func withLaunchOptions(in *awsec2.RunInstancesInput) {
	in.MetadataOptions = &types.InstanceMetadataOptionsRequest{
		HttpTokens: types.HttpTokensStateRequired, HttpEndpoint: types.InstanceMetadataEndpointStateEnabled,
		HttpPutResponseHopLimit: aws.Int32(2), InstanceMetadataTags: types.InstanceMetadataTagsStateEnabled,
	}
	in.PrivateDnsNameOptions = &types.PrivateDnsNameOptionsRequest{
		HostnameType: types.HostnameTypeResourceName, EnableResourceNameDnsARecord: aws.Bool(true),
		EnableResourceNameDnsAAAARecord: aws.Bool(false),
	}
	in.CpuOptions = &types.CpuOptionsRequest{CoreCount: aws.Int32(1), ThreadsPerCore: aws.Int32(1), AmdSevSnp: types.AmdSevSnpSpecificationDisabled}
	in.SubnetId = nil
	in.NetworkInterfaces = []types.InstanceNetworkInterfaceSpecification{{
		DeviceIndex: aws.Int32(0), NetworkCardIndex: aws.Int32(0), SubnetId: aws.String(subnet), InterfaceType: aws.String("efa"),
	}}
}

func TestRunInstancesLaunchOptions(t *testing.T) {
	s := ec2.New()
	c := newClient(t, s)
	in := runInput(putImage(t, s, ec2.Image{}))
	withLaunchOptions(in)
	launched := run(t, c, in)[0]
	if m := launched.MetadataOptions; m == nil || m.State != types.InstanceMetadataOptionsStatePending {
		t.Fatalf("launched metadata options = %+v", m)
	}
	id := aws.ToString(launched.InstanceId)
	inst := describeInstance(t, c, id)
	if m := inst.MetadataOptions; m == nil || m.State != types.InstanceMetadataOptionsStateApplied || m.HttpTokens != types.HttpTokensStateRequired ||
		m.HttpEndpoint != types.InstanceMetadataEndpointStateEnabled || aws.ToInt32(m.HttpPutResponseHopLimit) != 2 ||
		m.InstanceMetadataTags != types.InstanceMetadataTagsStateEnabled || m.HttpProtocolIpv6 != types.InstanceMetadataProtocolStateDisabled {
		t.Fatalf("metadata options = %+v", m)
	}
	if o := inst.PrivateDnsNameOptions; o == nil || o.HostnameType != types.HostnameTypeResourceName ||
		!aws.ToBool(o.EnableResourceNameDnsARecord) || aws.ToBool(o.EnableResourceNameDnsAAAARecord) {
		t.Fatalf("private DNS name options = %+v", o)
	}
	if got := aws.ToString(inst.PrivateDnsName); got != id+".ec2.internal" {
		t.Fatalf("private DNS name = %q, want the resource name", got)
	}
	if o := inst.CpuOptions; o == nil || aws.ToInt32(o.CoreCount) != 1 || aws.ToInt32(o.ThreadsPerCore) != 1 || o.AmdSevSnp != types.AmdSevSnpSpecificationDisabled {
		t.Fatalf("CPU options = %+v", o)
	}
	nic := inst.NetworkInterfaces[0]
	if aws.ToString(nic.InterfaceType) != "efa" || aws.ToInt32(nic.Attachment.NetworkCardIndex) != 0 {
		t.Fatalf("interface = %+v", nic)
	}
	enis, err := describeInterfaces(t, c, &awsec2.DescribeNetworkInterfacesInput{NetworkInterfaceIds: []string{aws.ToString(nic.NetworkInterfaceId)}})
	if err != nil || enis[0].InterfaceType != types.NetworkInterfaceType("efa") {
		t.Fatalf("described interface = %+v, %v", enis, err)
	}
	got, _ := s.LookupInstance(region, id)
	if got.MetadataOptions.HTTPTokens != "required" || got.PrivateDNSNameOptions.HostnameType != "resource-name" || got.CPUOptions.ThreadsPerCore != 1 {
		t.Fatalf("LookupInstance = %+v", got)
	}
}

func TestRunInstancesLaunchOptionDefaults(t *testing.T) {
	s := ec2.New()
	c := newClient(t, s)
	inst := describeInstance(t, c, aws.ToString(run(t, c, runInput(putImage(t, s, ec2.Image{})))[0].InstanceId))
	if m := inst.MetadataOptions; m == nil || m.HttpTokens != types.HttpTokensStateOptional || m.HttpEndpoint != types.InstanceMetadataEndpointStateEnabled ||
		aws.ToInt32(m.HttpPutResponseHopLimit) != 1 || m.InstanceMetadataTags != types.InstanceMetadataTagsStateDisabled {
		t.Fatalf("metadata options = %+v", m)
	}
	if o := inst.PrivateDnsNameOptions; o == nil || o.HostnameType != types.HostnameTypeIpName {
		t.Fatalf("private DNS name options = %+v", o)
	}
	if o := inst.CpuOptions; o == nil || aws.ToInt32(o.CoreCount) != 1 || aws.ToInt32(o.ThreadsPerCore) != 2 || o.AmdSevSnp != "" {
		t.Fatalf("t3.micro CPU options = %+v, want 1 core, 2 threads", o)
	}
	if got := aws.ToString(inst.NetworkInterfaces[0].InterfaceType); got != "interface" {
		t.Fatalf("interface type = %q", got)
	}
}

func TestRunInstancesLaunchOptionErrors(t *testing.T) {
	s := ec2.New()
	c := newClient(t, s)
	image := putImage(t, s, ec2.Image{})
	held := createInterface(t, c)
	for _, tc := range []struct {
		name   string
		change func(*awsec2.RunInstancesInput)
		code   string
	}{
		{"an unknown HttpTokens", func(in *awsec2.RunInstancesInput) { in.MetadataOptions.HttpTokens = "sometimes" }, "InvalidParameterValue"},
		{"an unknown HttpEndpoint", func(in *awsec2.RunInstancesInput) { in.MetadataOptions.HttpEndpoint = "on" }, "InvalidParameterValue"},
		{"an unknown InstanceMetadataTags", func(in *awsec2.RunInstancesInput) { in.MetadataOptions.InstanceMetadataTags = "yes" }, "InvalidParameterValue"},
		{"a hop limit of 0", func(in *awsec2.RunInstancesInput) { in.MetadataOptions.HttpPutResponseHopLimit = aws.Int32(0) }, "InvalidParameterValue"},
		{"a hop limit over 64", func(in *awsec2.RunInstancesInput) { in.MetadataOptions.HttpPutResponseHopLimit = aws.Int32(65) }, "InvalidParameterValue"},
		{"an unknown HostnameType", func(in *awsec2.RunInstancesInput) { in.PrivateDnsNameOptions.HostnameType = "fqdn" }, "InvalidParameterValue"},
		{"an unknown interface type", func(in *awsec2.RunInstancesInput) { in.NetworkInterfaces[0].InterfaceType = aws.String("trunk") }, "InvalidParameterValue"},
		{"another network card", func(in *awsec2.RunInstancesInput) { in.NetworkInterfaces[0].NetworkCardIndex = aws.Int32(1) }, "InvalidParameterValue"},
		{"an interface type with an existing interface", func(in *awsec2.RunInstancesInput) {
			in.NetworkInterfaces[0].SubnetId, in.NetworkInterfaces[0].NetworkInterfaceId = nil, held.NetworkInterfaceId
		}, "InvalidParameterCombination"},
		{"more threads per core than the type has", func(in *awsec2.RunInstancesInput) { in.CpuOptions.ThreadsPerCore = aws.Int32(3) }, "InvalidParameterValue"},
		{"more cores than the type has", func(in *awsec2.RunInstancesInput) { in.CpuOptions.CoreCount = aws.Int32(2) }, "InvalidParameterValue"},
		{"no cores", func(in *awsec2.RunInstancesInput) { in.CpuOptions.CoreCount = aws.Int32(0) }, "InvalidParameterValue"},
		{"an unknown AmdSevSnp", func(in *awsec2.RunInstancesInput) { in.CpuOptions.AmdSevSnp = "maybe" }, "InvalidParameterValue"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			in := runInput(image)
			withLaunchOptions(in)
			tc.change(in)
			if _, err := c.RunInstances(t.Context(), in); code(err) != tc.code {
				t.Fatalf("= %v, want %s", err, tc.code)
			}
			if got := s.Instances(); len(got) != 0 {
				t.Fatalf("a failed launch left %+v", got)
			}
		})
	}
}

func TestRunInstancesLaunchOptionsAreIdempotent(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*awsec2.RunInstancesInput)
	}{
		{"metadata options", func(in *awsec2.RunInstancesInput) { in.MetadataOptions.HttpTokens = types.HttpTokensStateOptional }},
		{"private DNS name options", func(in *awsec2.RunInstancesInput) { in.PrivateDnsNameOptions.HostnameType = types.HostnameTypeIpName }},
		{"interface type", func(in *awsec2.RunInstancesInput) { in.NetworkInterfaces[0].InterfaceType = aws.String("interface") }},
		{"network card index", func(in *awsec2.RunInstancesInput) { in.NetworkInterfaces[0].NetworkCardIndex = nil }},
		{"CPU options", func(in *awsec2.RunInstancesInput) { in.CpuOptions.ThreadsPerCore = aws.Int32(2) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := ec2.New()
			c := newClient(t, s)
			in := runInput(putImage(t, s, ec2.Image{}))
			withLaunchOptions(in)
			in.ClientToken = aws.String("launch-options")
			first := run(t, c, in)
			if again := run(t, c, in); ids(again)[0] != ids(first)[0] {
				t.Fatalf("the same launch again = %v, want %v", ids(again), ids(first))
			}
			tc.change(in)
			if _, err := c.RunInstances(t.Context(), in); code(err) != "IdempotentParameterMismatch" {
				t.Fatalf("the token with other %s = %v, want IdempotentParameterMismatch", tc.name, err)
			}
			if got := len(s.Instances()); got != 1 {
				t.Fatalf("%d instances, want 1", got)
			}
		})
	}
}
