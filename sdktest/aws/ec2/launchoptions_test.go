package ec2_test

import (
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsec2 "github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/ec2/types"

	"github.com/parallelworks/squall/aws/ec2"
	"github.com/parallelworks/squall/sdktest/internal/sdkerr"
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
	if err != nil || len(enis) != 1 || enis[0].InterfaceType != types.NetworkInterfaceType("efa") {
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
	in := runInput(putImage(t, s, ec2.Image{}))
	in.CpuOptions = &types.CpuOptionsRequest{AmdSevSnp: types.AmdSevSnpSpecificationEnabled}
	snp := describeInstance(t, c, aws.ToString(run(t, c, in)[0].InstanceId))
	if o := snp.CpuOptions; o == nil || aws.ToInt32(o.CoreCount) != 1 || aws.ToInt32(o.ThreadsPerCore) != 2 || o.AmdSevSnp != types.AmdSevSnpSpecificationEnabled {
		t.Fatalf("CPU options with only AmdSevSnp = %+v, want the defaults", o)
	}
}

func TestCPUOptionsFollowTheInstanceType(t *testing.T) {
	s := ec2.New()
	c := newClient(t, s)
	if err := s.PutInstanceType(ec2.InstanceType{InstanceType: "m7a.large", VCPUs: 2, MemoryMiB: 8192, ThreadsPerCore: 1}); err != nil {
		t.Fatal(err)
	}
	if err := s.PutInstanceType(ec2.InstanceType{InstanceType: "x9.odd", VCPUs: 3, ThreadsPerCore: 2}); err == nil {
		t.Fatal("PutInstanceType accepted 2 threads per core on 3 vCPUs")
	}
	out, err := c.DescribeInstanceTypes(t.Context(), &awsec2.DescribeInstanceTypesInput{InstanceTypes: []types.InstanceType{"m7a.large", types.InstanceTypeT3Micro}})
	if err != nil || len(out.InstanceTypes) != 2 {
		t.Fatalf("describe = %+v, %v", out, err)
	}
	for _, it := range out.InstanceTypes {
		cores, threads := int32(2), int32(1)
		if it.InstanceType == types.InstanceTypeT3Micro {
			cores, threads = 1, 2
		}
		v := it.VCpuInfo
		if aws.ToInt32(v.DefaultCores) != cores || aws.ToInt32(v.DefaultThreadsPerCore) != threads ||
			len(v.ValidCores) != int(cores) || len(v.ValidThreadsPerCore) != int(threads) {
			t.Fatalf("%s vCPU info = %+v, want %d cores of %d threads", it.InstanceType, v, cores, threads)
		}
	}
	image := putImage(t, s, ec2.Image{})
	launch := func(cpu *types.CpuOptionsRequest) (*awsec2.RunInstancesOutput, error) {
		in := runInput(image)
		in.InstanceType, in.CpuOptions = "m7a.large", cpu
		return c.RunInstances(t.Context(), in)
	}
	for _, cpu := range []*types.CpuOptionsRequest{nil, {CoreCount: aws.Int32(2), ThreadsPerCore: aws.Int32(1)}} {
		launched, err := launch(cpu)
		if err != nil {
			t.Fatalf("launching m7a.large with %+v: %v", cpu, err)
		}
		if o := describeInstance(t, c, aws.ToString(launched.Instances[0].InstanceId)).CpuOptions; o == nil ||
			aws.ToInt32(o.CoreCount) != 2 || aws.ToInt32(o.ThreadsPerCore) != 1 {
			t.Fatalf("m7a.large CPU options = %+v, want 2 cores, 1 thread", o)
		}
	}
	if _, err := launch(&types.CpuOptionsRequest{CoreCount: aws.Int32(1), ThreadsPerCore: aws.Int32(2)}); sdkerr.AWSCode(err) != "InvalidParameterValue" {
		t.Fatalf("2 threads per core on m7a.large = %v, want InvalidParameterValue", err)
	}
}

func TestChangingTheInstanceTypeResetsCPUOptions(t *testing.T) {
	s := ec2.New()
	c := newClient(t, s)
	in := runInput(putImage(t, s, ec2.Image{}))
	in.CpuOptions = &types.CpuOptionsRequest{CoreCount: aws.Int32(1), ThreadsPerCore: aws.Int32(1), AmdSevSnp: types.AmdSevSnpSpecificationEnabled}
	id := aws.ToString(run(t, c, in)[0].InstanceId)
	describeInstance(t, c, id)
	if _, err := c.StopInstances(t.Context(), &awsec2.StopInstancesInput{InstanceIds: []string{id}}); err != nil {
		t.Fatal(err)
	}
	describeInstance(t, c, id)
	if _, err := c.ModifyInstanceAttribute(t.Context(), &awsec2.ModifyInstanceAttributeInput{
		InstanceId: aws.String(id), InstanceType: &types.AttributeValue{Value: aws.String("m5.xlarge")},
	}); err != nil {
		t.Fatal(err)
	}
	if o := describeInstance(t, c, id).CpuOptions; o == nil || aws.ToInt32(o.CoreCount) != 2 || aws.ToInt32(o.ThreadsPerCore) != 2 || o.AmdSevSnp != types.AmdSevSnpSpecificationEnabled {
		t.Fatalf("CPU options after the change = %+v, want m5.xlarge's defaults with AMD SEV-SNP kept", o)
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
		{"a core count without threads per core", func(in *awsec2.RunInstancesInput) { in.CpuOptions.ThreadsPerCore = nil }, "MissingParameter"},
		{"threads per core without a core count", func(in *awsec2.RunInstancesInput) { in.CpuOptions.CoreCount = nil }, "MissingParameter"},
		{"an unknown AmdSevSnp", func(in *awsec2.RunInstancesInput) { in.CpuOptions.AmdSevSnp = "maybe" }, "InvalidParameterValue"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			in := runInput(image)
			withLaunchOptions(in)
			tc.change(in)
			if _, err := c.RunInstances(t.Context(), in); sdkerr.AWSCode(err) != tc.code {
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
			if _, err := c.RunInstances(t.Context(), in); sdkerr.AWSCode(err) != "IdempotentParameterMismatch" {
				t.Fatalf("the token with other %s = %v, want IdempotentParameterMismatch", tc.name, err)
			}
			if got := len(s.Instances()); got != 1 {
				t.Fatalf("%d instances, want 1", got)
			}
		})
	}
}

func TestLaunchOptionEdges(t *testing.T) {
	s := ec2.New(ec2.WithSettleAfter(-1))
	c := newClient(t, s)
	image := putImage(t, s, ec2.Image{})

	in := runInput(image)
	in.ClientToken = aws.String("card")
	in.SubnetId = nil
	in.NetworkInterfaces = []types.InstanceNetworkInterfaceSpecification{{DeviceIndex: aws.Int32(0), SubnetId: aws.String(subnet), NetworkCardIndex: aws.Int32(0)}}
	first := run(t, c, in)
	in.NetworkInterfaces[0].NetworkCardIndex = nil
	if again := run(t, c, in); ids(again)[0] != ids(first)[0] {
		t.Fatalf("leaving out network card index 0 = %v, want the same launch %v", ids(again), ids(first))
	}

	id := ids(first)[0]
	if got := describeInstance(t, c, id).MetadataOptions.State; got != types.InstanceMetadataOptionsStatePending {
		t.Fatalf("metadata state while launching = %s", got)
	}
	s.Settle()
	if _, err := c.StopInstances(t.Context(), &awsec2.StopInstancesInput{InstanceIds: []string{id}}); err != nil {
		t.Fatal(err)
	}
	s.Settle()
	if _, err := c.StartInstances(t.Context(), &awsec2.StartInstancesInput{InstanceIds: []string{id}}); err != nil {
		t.Fatal(err)
	}
	if got := describeInstance(t, c, id).MetadataOptions.State; got != types.InstanceMetadataOptionsStateApplied {
		t.Fatalf("metadata state while restarting = %s, want applied: nothing changed them", got)
	}

	nic, err := c.CreateNetworkInterface(t.Context(), &awsec2.CreateNetworkInterfaceInput{SubnetId: aws.String(subnet)})
	if err != nil {
		t.Fatal(err)
	}
	attach := func(interfaceType string) error {
		in := runInput(image)
		in.SubnetId = nil
		in.NetworkInterfaces = []types.InstanceNetworkInterfaceSpecification{{DeviceIndex: aws.Int32(0), NetworkInterfaceId: nic.NetworkInterface.NetworkInterfaceId, InterfaceType: aws.String(interfaceType)}}
		_, err := c.RunInstances(t.Context(), in)
		return err
	}
	if err := attach("efa"); sdkerr.AWSCode(err) != "InvalidParameterCombination" {
		t.Fatalf("efa on a plain interface = %v", err)
	}
	if err := attach("interface"); err != nil {
		t.Fatalf("the interface's own type = %v", err)
	}

	in = runInput(image)
	in.SubnetId = nil
	in.NetworkInterfaces = []types.InstanceNetworkInterfaceSpecification{{DeviceIndex: aws.Int32(0), SubnetId: aws.String(subnet), InterfaceType: aws.String("efa-only")}}
	if _, err := c.RunInstances(t.Context(), in); sdkerr.AWSCode(err) != "InvalidParameterValue" || !strings.Contains(err.Error(), "primary network interface") {
		t.Fatalf("efa-only primary interface = %v", err)
	}
}
