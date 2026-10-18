package ec2_test

import (
	"encoding/base64"
	"net/http"
	"slices"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsec2 "github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/ec2/types"

	"github.com/parallelworks/squall/aws/ec2"
	"github.com/parallelworks/squall/fault"
)

func putImage(t *testing.T, s *ec2.Server, img ec2.Image) string {
	t.Helper()
	stored, err := s.PutImage(region, img)
	if err != nil {
		t.Fatal(err)
	}
	return stored.ImageID
}

func runInput(image string, kv ...string) *awsec2.RunInstancesInput {
	return &awsec2.RunInstancesInput{
		ImageId: aws.String(image), InstanceType: types.InstanceTypeT3Micro, MinCount: aws.Int32(1), MaxCount: aws.Int32(1),
		SubnetId: aws.String(subnet), TagSpecifications: tagSpec(types.ResourceTypeInstance, kv...),
	}
}

func run(t *testing.T, c *awsec2.Client, in *awsec2.RunInstancesInput) []types.Instance {
	t.Helper()
	out, err := c.RunInstances(t.Context(), in)
	if err != nil {
		t.Fatalf("run instances: %v", err)
	}
	return out.Instances
}

func describeInstances(t *testing.T, c *awsec2.Client, in *awsec2.DescribeInstancesInput) ([]types.Instance, error) {
	t.Helper()
	out, err := c.DescribeInstances(t.Context(), in)
	if err != nil {
		return nil, err
	}
	var all []types.Instance
	for _, r := range out.Reservations {
		all = append(all, r.Instances...)
	}
	return all, nil
}

func describeInstance(t *testing.T, c *awsec2.Client, id string) types.Instance {
	t.Helper()
	got, err := describeInstances(t, c, &awsec2.DescribeInstancesInput{InstanceIds: []string{id}})
	if err != nil || len(got) != 1 {
		t.Fatalf("describe %s = %v, %v", id, got, err)
	}
	return got[0]
}

func ids(insts []types.Instance) []string {
	var out []string
	for _, inst := range insts {
		out = append(out, aws.ToString(inst.InstanceId))
	}
	return out
}

func TestRunAndDescribeInstances(t *testing.T) {
	s := ec2.New()
	c := newClient(t, s)
	image := putImage(t, s, ec2.Image{Name: "base"})
	if _, err := c.CreatePlacementGroup(t.Context(), &awsec2.CreatePlacementGroupInput{
		GroupName: aws.String("pack"), Strategy: types.PlacementStrategyCluster,
	}); err != nil {
		t.Fatal(err)
	}
	in := runInput(image, "Name", "web")
	in.TagSpecifications = append(in.TagSpecifications, tagSpec(types.ResourceTypeVolume, "disk", "root")...)
	in.TagSpecifications = append(in.TagSpecifications, tagSpec(types.ResourceTypeNetworkInterface, "nic", "primary")...)
	in.PrivateIpAddress = aws.String("10.1.2.3")
	in.SecurityGroupIds = []string{"sg-1"}
	in.Placement = &types.Placement{AvailabilityZone: aws.String(region + "b"), GroupName: aws.String("pack")}
	in.UserData = aws.String(base64.StdEncoding.EncodeToString([]byte("#!/bin/sh\necho hi\n")))
	in.KeyName = aws.String("key")
	launched := run(t, c, in)
	if len(launched) != 1 {
		t.Fatalf("launched %d instances", len(launched))
	}
	first := launched[0]
	id := aws.ToString(first.InstanceId)
	if first.State.Name != types.InstanceStateNamePending || aws.ToInt32(first.State.Code) != 0 ||
		aws.ToString(first.PrivateIpAddress) != "10.1.2.3" || aws.ToString(first.SubnetId) != subnet {
		t.Fatalf("launched %+v", first)
	}
	inst := describeInstance(t, c, id)
	if inst.State.Name != types.InstanceStateNameRunning || aws.ToInt32(inst.State.Code) != 16 {
		t.Fatalf("state after a describe = %+v, want running", inst.State)
	}
	if aws.ToString(inst.ImageId) != image || inst.InstanceType != types.InstanceTypeT3Micro || tags(inst.Tags)["Name"] != "web" ||
		aws.ToString(inst.Placement.AvailabilityZone) != region+"b" || aws.ToString(inst.Placement.GroupName) != "pack" ||
		aws.ToString(inst.KeyName) != "key" || inst.Architecture != types.ArchitectureValuesX8664 ||
		aws.ToString(inst.RootDeviceName) != "/dev/xvda" || inst.LaunchTime == nil || aws.ToString(inst.PrivateDnsName) != "ip-10-1-2-3.ec2.internal" {
		t.Fatalf("instance = %+v", inst)
	}
	if len(inst.SecurityGroups) != 1 || aws.ToString(inst.SecurityGroups[0].GroupId) != "sg-1" {
		t.Fatalf("security groups = %+v", inst.SecurityGroups)
	}
	if len(inst.NetworkInterfaces) != 1 {
		t.Fatalf("interfaces = %+v", inst.NetworkInterfaces)
	}
	nic := inst.NetworkInterfaces[0]
	if aws.ToInt32(nic.Attachment.DeviceIndex) != 0 || !aws.ToBool(nic.Attachment.DeleteOnTermination) || aws.ToString(nic.PrivateIpAddress) != "10.1.2.3" {
		t.Fatalf("interface = %+v", nic)
	}
	enis, err := describeInterfaces(t, c, &awsec2.DescribeNetworkInterfacesInput{NetworkInterfaceIds: []string{aws.ToString(nic.NetworkInterfaceId)}})
	if err != nil || enis[0].Status != types.NetworkInterfaceStatusInUse || aws.ToString(enis[0].Attachment.InstanceId) != id ||
		tags(enis[0].TagSet)["nic"] != "primary" || aws.ToString(enis[0].AvailabilityZone) != region+"b" {
		t.Fatalf("interface = %+v, %v", enis, err)
	}
	if len(inst.BlockDeviceMappings) != 1 || aws.ToString(inst.BlockDeviceMappings[0].DeviceName) != "/dev/xvda" ||
		inst.BlockDeviceMappings[0].Ebs.Status != types.AttachmentStatusAttached {
		t.Fatalf("block devices = %+v", inst.BlockDeviceMappings)
	}
	vols, err := c.DescribeVolumes(t.Context(), &awsec2.DescribeVolumesInput{Filters: []types.Filter{filter("attachment.instance-id", id)}})
	if err != nil || len(vols.Volumes) != 1 {
		t.Fatalf("root volumes = %+v, %v", vols, err)
	}
	if v := vols.Volumes[0]; v.State != types.VolumeStateInUse || aws.ToInt32(v.Size) != 8 || v.VolumeType != types.VolumeTypeGp3 ||
		tags(v.Tags)["disk"] != "root" || aws.ToString(v.AvailabilityZone) != region+"b" {
		t.Fatalf("root volume = %+v", v)
	}
	got, ok := s.LookupInstance(region, id)
	if !ok || got.UserData != "#!/bin/sh\necho hi\n" || got.PlacementGroup != "pack" {
		t.Fatalf("LookupInstance = %+v, %v", got, ok)
	}
}

func TestRunManyInstances(t *testing.T) {
	s := ec2.New()
	c := newClient(t, s)
	in := runInput(putImage(t, s, ec2.Image{}))
	in.MinCount, in.MaxCount = aws.Int32(2), aws.Int32(3)
	out, err := c.RunInstances(t.Context(), in)
	if err != nil || len(out.Instances) != 3 {
		t.Fatalf("run = %+v, %v", out, err)
	}
	seen := map[string]bool{}
	for i, inst := range out.Instances {
		if aws.ToInt32(inst.AmiLaunchIndex) != int32(i) || seen[aws.ToString(inst.PrivateIpAddress)] {
			t.Fatalf("instance %d = %+v", i, inst)
		}
		seen[aws.ToString(inst.PrivateIpAddress)] = true
	}
	all, err := c.DescribeInstances(t.Context(), &awsec2.DescribeInstancesInput{})
	if err != nil || len(all.Reservations) != 1 || aws.ToString(all.Reservations[0].ReservationId) != aws.ToString(out.ReservationId) ||
		len(all.Reservations[0].Instances) != 3 {
		t.Fatalf("describe = %+v, %v", all, err)
	}
	if got := len(s.Volumes()); got != 3 {
		t.Fatalf("%d volumes, want a root volume each", got)
	}
}

func TestInstanceWaiters(t *testing.T) {
	s := ec2.New(ec2.WithSettleAfter(2))
	c := newClient(t, s)
	id := ids(run(t, c, runInput(putImage(t, s, ec2.Image{}))))[0]
	in := &awsec2.DescribeInstancesInput{InstanceIds: []string{id}}
	for range 2 {
		if got := describeInstance(t, c, id); got.State.Name != types.InstanceStateNamePending {
			t.Fatalf("state = %s, want pending for two describes", got.State.Name)
		}
	}
	running := awsec2.NewInstanceRunningWaiter(c, func(o *awsec2.InstanceRunningWaiterOptions) {
		o.MinDelay, o.MaxDelay = time.Millisecond, 5*time.Millisecond
	})
	if err := running.Wait(t.Context(), in, 5*time.Second); err != nil {
		t.Fatalf("waiting to run: %v", err)
	}
	stop, err := c.StopInstances(t.Context(), &awsec2.StopInstancesInput{InstanceIds: []string{id}, Force: aws.Bool(true)})
	if err != nil {
		t.Fatal(err)
	}
	if ch := stop.StoppingInstances[0]; ch.CurrentState.Name != types.InstanceStateNameStopping || ch.PreviousState.Name != types.InstanceStateNameRunning ||
		aws.ToInt32(ch.CurrentState.Code) != 64 {
		t.Fatalf("stop = %+v", ch)
	}
	stopped := awsec2.NewInstanceStoppedWaiter(c, func(o *awsec2.InstanceStoppedWaiterOptions) {
		o.MinDelay, o.MaxDelay = time.Millisecond, 5*time.Millisecond
	})
	if err := stopped.Wait(t.Context(), in, 5*time.Second); err != nil {
		t.Fatalf("waiting to stop: %v", err)
	}
	start, err := c.StartInstances(t.Context(), &awsec2.StartInstancesInput{InstanceIds: []string{id}})
	if err != nil {
		t.Fatal(err)
	}
	if ch := start.StartingInstances[0]; ch.CurrentState.Name != types.InstanceStateNamePending || ch.PreviousState.Name != types.InstanceStateNameStopped {
		t.Fatalf("start = %+v", ch)
	}
	if err := running.Wait(t.Context(), in, 5*time.Second); err != nil {
		t.Fatalf("waiting to run again: %v", err)
	}
	term, err := c.TerminateInstances(t.Context(), &awsec2.TerminateInstancesInput{InstanceIds: []string{id}})
	if err != nil {
		t.Fatal(err)
	}
	if ch := term.TerminatingInstances[0]; ch.CurrentState.Name != types.InstanceStateNameShuttingDown || ch.PreviousState.Name != types.InstanceStateNameRunning {
		t.Fatalf("terminate = %+v", ch)
	}
	terminated := awsec2.NewInstanceTerminatedWaiter(c, func(o *awsec2.InstanceTerminatedWaiterOptions) {
		o.MinDelay, o.MaxDelay = time.Millisecond, 5*time.Millisecond
	})
	if err := terminated.Wait(t.Context(), in, 5*time.Second); err != nil {
		t.Fatalf("waiting to terminate: %v", err)
	}
	inst := describeInstance(t, c, id)
	if len(inst.NetworkInterfaces) != 0 || inst.PrivateIpAddress != nil || len(inst.BlockDeviceMappings) != 0 {
		t.Fatalf("a terminated instance = %+v, want no interface or volumes", inst)
	}
	if n, v := len(s.NetworkInterfaces()), len(s.Volumes()); n != 0 || v != 0 {
		t.Fatalf("%d interfaces and %d volumes left, want those deleted on termination gone", n, v)
	}
	again, err := c.TerminateInstances(t.Context(), &awsec2.TerminateInstancesInput{InstanceIds: []string{id}})
	if err != nil || again.TerminatingInstances[0].CurrentState.Name != types.InstanceStateNameTerminated ||
		again.TerminatingInstances[0].PreviousState.Name != types.InstanceStateNameTerminated {
		t.Fatalf("terminating again = %+v, %v", again, err)
	}
	if err := s.ForgetInstance(region, id); err != nil {
		t.Fatal(err)
	}
	if _, err := describeInstances(t, c, in); code(err) != "InvalidInstanceID.NotFound" {
		t.Fatalf("describing a forgotten instance = %v", err)
	}
}

func TestSettle(t *testing.T) {
	s := ec2.New(ec2.WithSettleAfter(-1))
	c := newClient(t, s)
	id := ids(run(t, c, runInput(putImage(t, s, ec2.Image{}))))[0]
	for range 3 {
		if got := describeInstance(t, c, id); got.State.Name != types.InstanceStateNamePending {
			t.Fatalf("state = %s, want pending until Settle", got.State.Name)
		}
	}
	if _, err := c.StopInstances(t.Context(), &awsec2.StopInstancesInput{InstanceIds: []string{id}}); code(err) != "IncorrectInstanceState" {
		t.Fatalf("stopping a pending instance = %v", err)
	}
	if err := s.ForgetInstance(region, id); err == nil {
		t.Fatal("ForgetInstance removed an instance that is not terminated")
	}
	s.Settle()
	if got := describeInstance(t, c, id); got.State.Name != types.InstanceStateNameRunning {
		t.Fatalf("state after Settle = %s", got.State.Name)
	}
}

func TestInstanceStateErrors(t *testing.T) {
	s := ec2.New()
	c := newClient(t, s)
	image := putImage(t, s, ec2.Image{})
	stopping := ids(run(t, c, runInput(image)))[0]
	gone := ids(run(t, c, runInput(image)))[0]
	describeInstance(t, c, stopping)
	if _, err := c.StopInstances(t.Context(), &awsec2.StopInstancesInput{InstanceIds: []string{stopping}}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.TerminateInstances(t.Context(), &awsec2.TerminateInstancesInput{InstanceIds: []string{gone}}); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name string
		call func() error
		code string
	}{
		{"start a stopping instance", func() error {
			_, err := c.StartInstances(t.Context(), &awsec2.StartInstancesInput{InstanceIds: []string{stopping}})
			return err
		}, "IncorrectInstanceState"},
		{"start a terminating instance", func() error {
			_, err := c.StartInstances(t.Context(), &awsec2.StartInstancesInput{InstanceIds: []string{gone}})
			return err
		}, "IncorrectInstanceState"},
		{"stop a terminating instance", func() error {
			_, err := c.StopInstances(t.Context(), &awsec2.StopInstancesInput{InstanceIds: []string{gone}})
			return err
		}, "IncorrectInstanceState"},
		{"change the type of a terminating instance", func() error {
			_, err := c.ModifyInstanceAttribute(t.Context(), &awsec2.ModifyInstanceAttributeInput{
				InstanceId: aws.String(gone), InstanceType: &types.AttributeValue{Value: aws.String("t3.small")},
			})
			return err
		}, "IncorrectInstanceState"},
		{"stop an unknown instance", func() error {
			_, err := c.StopInstances(t.Context(), &awsec2.StopInstancesInput{InstanceIds: []string{stopping, "i-0000000000000dead"}})
			return err
		}, "InvalidInstanceID.NotFound"},
		{"terminate a malformed ID", func() error {
			_, err := c.TerminateInstances(t.Context(), &awsec2.TerminateInstancesInput{InstanceIds: []string{"instance-1"}})
			return err
		}, "InvalidInstanceID.Malformed"},
		{"describe unknown instances", func() error {
			_, err := describeInstances(t, c, &awsec2.DescribeInstancesInput{InstanceIds: []string{"i-12345678", "i-0000000000000dead"}})
			return err
		}, "InvalidInstanceID.NotFound"},
		{"describe a malformed ID", func() error {
			_, err := describeInstances(t, c, &awsec2.DescribeInstancesInput{InstanceIds: []string{"i-xyz"}})
			return err
		}, "InvalidInstanceID.Malformed"},
		{"describe with an unknown filter", func() error {
			_, err := describeInstances(t, c, &awsec2.DescribeInstancesInput{Filters: []types.Filter{filter("hypervisor", "xen")}})
			return err
		}, "InvalidParameterValue"},
		{"page through named instances", func() error {
			_, err := describeInstances(t, c, &awsec2.DescribeInstancesInput{InstanceIds: []string{gone}, MaxResults: aws.Int32(5)})
			return err
		}, "InvalidParameterCombination"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := tc.call(); code(err) != tc.code {
				t.Fatalf("= %v, want %s", err, tc.code)
			}
		})
	}
	if got := describeInstance(t, c, stopping); got.State.Name != types.InstanceStateNameStopped {
		t.Fatalf("a failed stop of two instances changed the first: %s", got.State.Name)
	}
	again, err := c.StopInstances(t.Context(), &awsec2.StopInstancesInput{InstanceIds: []string{stopping}})
	if err != nil || again.StoppingInstances[0].CurrentState.Name != types.InstanceStateNameStopped {
		t.Fatalf("stopping a stopped instance = %+v, %v", again, err)
	}
	if _, err := c.ModifyInstanceAttribute(t.Context(), &awsec2.ModifyInstanceAttributeInput{
		InstanceId: aws.String(stopping), InstanceType: &types.AttributeValue{Value: aws.String("m5.large")},
	}); err != nil {
		t.Fatalf("changing the type of a stopped instance: %v", err)
	}
	if got := describeInstance(t, c, stopping); got.InstanceType != types.InstanceTypeM5Large {
		t.Fatalf("type = %s", got.InstanceType)
	}
	for _, bad := range []string{"t4g.micro", "x9.huge"} {
		if _, err := c.ModifyInstanceAttribute(t.Context(), &awsec2.ModifyInstanceAttributeInput{
			InstanceId: aws.String(stopping), InstanceType: &types.AttributeValue{Value: aws.String(bad)},
		}); code(err) != "InvalidParameterValue" {
			t.Errorf("changing the type to %s = %v", bad, err)
		}
	}
}

func TestRunInstancesErrors(t *testing.T) {
	s := ec2.New()
	c := newClient(t, s)
	image := putImage(t, s, ec2.Image{})
	held := createInterface(t, c)
	for _, tc := range []struct {
		name   string
		change func(*awsec2.RunInstancesInput)
		code   string
	}{
		{"an unknown image", func(in *awsec2.RunInstancesInput) { in.ImageId = aws.String("ami-0000000000000dead") }, "InvalidAMIID.NotFound"},
		{"a malformed image ID", func(in *awsec2.RunInstancesInput) { in.ImageId = aws.String("image-1") }, "InvalidAMIID.Malformed"},
		{"an unknown instance type", func(in *awsec2.RunInstancesInput) { in.InstanceType = "x9.huge" }, "InvalidParameterValue"},
		{"an instance type of another architecture", func(in *awsec2.RunInstancesInput) { in.InstanceType = types.InstanceTypeT4gMicro }, "InvalidParameterValue"},
		{"no subnet", func(in *awsec2.RunInstancesInput) { in.SubnetId = nil }, "VPCIdNotSpecified"},
		{"more than the maximum", func(in *awsec2.RunInstancesInput) { in.MinCount = aws.Int32(2) }, "InvalidParameterValue"},
		{"an unknown placement group", func(in *awsec2.RunInstancesInput) {
			in.Placement = &types.Placement{GroupName: aws.String("missing")}
		}, "InvalidPlacementGroup.Unknown"},
		{"a zone in another region", func(in *awsec2.RunInstancesInput) {
			in.Placement = &types.Placement{AvailabilityZone: aws.String("eu-west-1a")}
		}, "InvalidParameterValue"},
		{"a private IP address for many instances", func(in *awsec2.RunInstancesInput) {
			in.MaxCount, in.PrivateIpAddress = aws.Int32(2), aws.String("10.9.9.9")
		}, "InvalidParameterCombination"},
		{"a private IP address in use", func(in *awsec2.RunInstancesInput) { in.PrivateIpAddress = held.PrivateIpAddress }, "InvalidIPAddress.InUse"},
		{"an interface and a subnet", func(in *awsec2.RunInstancesInput) {
			in.NetworkInterfaces = []types.InstanceNetworkInterfaceSpecification{{DeviceIndex: aws.Int32(0), SubnetId: aws.String(subnet)}}
		}, "InvalidParameterCombination"},
		{"an interface in use", func(in *awsec2.RunInstancesInput) {
			in.SubnetId = nil
			in.NetworkInterfaces = []types.InstanceNetworkInterfaceSpecification{{DeviceIndex: aws.Int32(0), NetworkInterfaceId: aws.String("eni-in-use")}}
		}, "InvalidNetworkInterface.InUse"},
		{"bad user data", func(in *awsec2.RunInstancesInput) { in.UserData = aws.String("not base64!") }, "InvalidParameterValue"},
		{"a root volume smaller than the image's", func(in *awsec2.RunInstancesInput) {
			in.BlockDeviceMappings = []types.BlockDeviceMapping{{DeviceName: aws.String("/dev/xvda"), Ebs: &types.EbsBlockDevice{VolumeSize: aws.Int32(4)}}}
		}, "InvalidBlockDeviceMapping"},
		{"an unsupported parameter", func(in *awsec2.RunInstancesInput) { in.DisableApiTermination = aws.Bool(true) }, "UnknownParameter"},
		{"tags for a resource it does not create", func(in *awsec2.RunInstancesInput) {
			in.TagSpecifications = tagSpec(types.ResourceTypeElasticIp, "k", "v")
		}, "InvalidParameterValue"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := s.PutNetworkInterface(region, ec2.NetworkInterface{NetworkInterfaceID: "eni-in-use", SubnetID: subnet, InstanceID: "i-other"}); err != nil {
				t.Fatal(err)
			}
			in := runInput(image)
			tc.change(in)
			if _, err := c.RunInstances(t.Context(), in); code(err) != tc.code {
				t.Fatalf("= %v, want %s", err, tc.code)
			}
			if got := s.Instances(); len(got) != 0 {
				t.Fatalf("a failed launch left %+v", got)
			}
		})
	}
	if got := len(s.NetworkInterfaces()); got != 2 {
		t.Fatalf("%d interfaces, want only the two the test made", got)
	}
}

func TestRunInstancesClientToken(t *testing.T) {
	s := ec2.New()
	c := newClient(t, s)
	in := runInput(putImage(t, s, ec2.Image{}))
	in.ClientToken = aws.String("launch-1")
	first, err := c.RunInstances(t.Context(), in)
	if err != nil {
		t.Fatal(err)
	}
	again, err := c.RunInstances(t.Context(), in)
	if err != nil || aws.ToString(again.ReservationId) != aws.ToString(first.ReservationId) ||
		!slices.Equal(ids(again.Instances), ids(first.Instances)) {
		t.Fatalf("repeating the token = %+v, %v; want the first reservation", again, err)
	}
	inst := describeInstance(t, c, ids(first.Instances)[0])
	if aws.ToString(inst.ClientToken) != "launch-1" {
		t.Fatalf("client token = %q", aws.ToString(inst.ClientToken))
	}
	in.InstanceType = types.InstanceTypeT3Small
	if _, err := c.RunInstances(t.Context(), in); code(err) != "IdempotentParameterMismatch" {
		t.Fatalf("the token with other parameters = %v, want IdempotentParameterMismatch", err)
	}
	if got := len(s.Instances()); got != 1 {
		t.Fatalf("%d instances, want 1", got)
	}
	got, err := describeInstances(t, c, &awsec2.DescribeInstancesInput{Filters: []types.Filter{filter("client-token", "launch-1")}})
	if err != nil || len(got) != 1 {
		t.Fatalf("by client token = %v, %v", got, err)
	}
}

func TestRunInstancesReplaysAcrossALostResponse(t *testing.T) {
	faults := fault.New().Add(fault.Rule{Method: "ec2.RunInstances", Nth: 1, Fault: fault.LoseResponse()})
	s := ec2.New(ec2.WithFaults(faults))
	c := newClient(t, s)
	in := runInput(putImage(t, s, ec2.Image{}))
	in.ClientToken = aws.String("launch-1")
	if _, err := c.RunInstances(t.Context(), in); err == nil || code(err) != "" {
		t.Fatalf("the first run = %v, want a transport error", err)
	}
	out, err := c.RunInstances(t.Context(), in)
	if err != nil {
		t.Fatalf("retrying with the token: %v", err)
	}
	got := s.Instances()
	if len(got) != 1 || got[0].InstanceID != ids(out.Instances)[0] {
		t.Fatalf("instances = %+v, want only the one the retry returned", got)
	}
	if inj := faults.Injected(); inj[0].Call.Resource != region+"/"+got[0].InstanceID {
		t.Fatalf("the fault named %s, want the instance it created", inj[0].Call.Resource)
	}
}

// launchOnce is the pattern the stand-in exists to test: run with one client
// token until it succeeds, however the attempts fail.
func launchOnce(t *testing.T, c *awsec2.Client, image string) (string, error) {
	t.Helper()
	in := runInput(image)
	in.ClientToken = aws.String("launch-once")
	var last error
	for range 20 {
		out, err := c.RunInstances(t.Context(), in)
		if err == nil {
			return ids(out.Instances)[0], nil
		}
		last = err
	}
	return "", last
}

func TestRunInstancesConvergesUnderRandomFaults(t *testing.T) {
	for seed := range uint64(30) {
		faults := fault.New(fault.Seed(seed)).Random(0.4,
			fault.Unavailable(), fault.LoseResponse(), fault.FailAfter(http.StatusInternalServerError))
		s := ec2.New(ec2.WithFaults(faults))
		c := newClient(t, s)
		id, err := launchOnce(t, c, putImage(t, s, ec2.Image{}))
		if err != nil {
			t.Fatalf("seed %d: %v\ninjected: %v", seed, err, faults.Injected())
		}
		if got := s.Instances(); len(got) != 1 || got[0].InstanceID != id || len(s.Volumes()) != 1 || len(s.NetworkInterfaces()) != 1 {
			t.Fatalf("seed %d: instances = %+v, want only %s with one volume and interface\ninjected: %v", seed, got, id, faults.Injected())
		}
	}
}

func TestInstanceFilters(t *testing.T) {
	s := ec2.New(ec2.WithSettleAfter(-1))
	c := newClient(t, s)
	image := putImage(t, s, ec2.Image{})
	if _, err := c.CreatePlacementGroup(t.Context(), &awsec2.CreatePlacementGroupInput{GroupName: aws.String("pack"), Strategy: types.PlacementStrategySpread}); err != nil {
		t.Fatal(err)
	}
	web := runInput(image, "role", "web", "session", "s1")
	web.Placement = &types.Placement{GroupName: aws.String("pack")}
	webID := ids(run(t, c, web))[0]
	db := runInput(image, "role", "db")
	db.SubnetId = aws.String("subnet-other")
	db.PrivateIpAddress = aws.String("10.200.0.9")
	db.ClientToken = aws.String("db-1")
	dbID := ids(run(t, c, db))[0]
	s.Settle()
	if _, err := c.StopInstances(t.Context(), &awsec2.StopInstancesInput{InstanceIds: []string{dbID}}); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		filters []types.Filter
		want    []string
	}{
		{[]types.Filter{filter("instance-state-name", "running")}, []string{webID}},
		{[]types.Filter{filter("instance-state-name", "pending", "stopping")}, []string{dbID}},
		{[]types.Filter{filter("instance-state-code", "16")}, []string{webID}},
		{[]types.Filter{filter("tag:role", "web", "db")}, []string{webID, dbID}},
		{[]types.Filter{filter("tag:role", "w*")}, []string{webID}},
		{[]types.Filter{filter("tag-key", "session")}, []string{webID}},
		{[]types.Filter{filter("client-token", "db-1")}, []string{dbID}},
		{[]types.Filter{filter("instance-id", dbID)}, []string{dbID}},
		{[]types.Filter{filter("subnet-id", "subnet-other")}, []string{dbID}},
		{[]types.Filter{filter("private-ip-address", "10.200.0.9")}, []string{dbID}},
		{[]types.Filter{filter("placement-group-name", "pack")}, []string{webID}},
		{[]types.Filter{filter("image-id", image), filter("tag:role", "db")}, []string{dbID}},
		{[]types.Filter{filter("instance-type", "t3.micro")}, []string{webID, dbID}},
		{[]types.Filter{filter("tag:role", "cache")}, nil},
	} {
		got, err := describeInstances(t, c, &awsec2.DescribeInstancesInput{Filters: tc.filters})
		if err != nil {
			t.Fatalf("%s: %v", describe(tc.filters), err)
		}
		if !slices.Equal(ids(got), tc.want) {
			t.Errorf("%s = %v, want %v", describe(tc.filters), ids(got), tc.want)
		}
	}
}

func TestDescribeInstancesPages(t *testing.T) {
	s := ec2.New()
	c := newClient(t, s)
	in := runInput(putImage(t, s, ec2.Image{}))
	in.MaxCount = aws.Int32(12)
	want := ids(run(t, c, in))
	var got []string
	pages := 0
	p := awsec2.NewDescribeInstancesPaginator(c, &awsec2.DescribeInstancesInput{MaxResults: aws.Int32(5)})
	for p.HasMorePages() {
		page, err := p.NextPage(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		pages++
		for _, r := range page.Reservations {
			got = append(got, ids(r.Instances)...)
		}
	}
	if pages != 3 || !slices.Equal(got, want) {
		t.Fatalf("%d pages of %v, want 3 of %v", pages, got, want)
	}
}

func TestExistingInterfaceAndPublicIPs(t *testing.T) {
	s := ec2.New()
	c := newClient(t, s)
	image := putImage(t, s, ec2.Image{})
	eni := aws.ToString(createInterface(t, c).NetworkInterfaceId)
	in := runInput(image)
	in.SubnetId = nil
	in.NetworkInterfaces = []types.InstanceNetworkInterfaceSpecification{{DeviceIndex: aws.Int32(0), NetworkInterfaceId: aws.String(eni)}}
	attachedID := ids(run(t, c, in))[0]
	if got := describeInstance(t, c, attachedID); aws.ToString(got.NetworkInterfaces[0].NetworkInterfaceId) != eni ||
		aws.ToBool(got.NetworkInterfaces[0].Attachment.DeleteOnTermination) || aws.ToString(got.SubnetId) != subnet {
		t.Fatalf("instance = %+v, want it on %s", got, eni)
	}

	public := runInput(image)
	public.SubnetId = nil
	public.NetworkInterfaces = []types.InstanceNetworkInterfaceSpecification{{
		DeviceIndex: aws.Int32(0), SubnetId: aws.String(subnet), AssociatePublicIpAddress: aws.Bool(true), Groups: []string{"sg-2"},
	}}
	publicID := ids(run(t, c, public))[0]
	inst := describeInstance(t, c, publicID)
	ephemeral := aws.ToString(inst.PublicIpAddress)
	if ephemeral == "" || aws.ToString(inst.NetworkInterfaces[0].Association.IpOwnerId) != "amazon" {
		t.Fatalf("instance = %+v, want a public IP address", inst)
	}
	byIP, err := describeInstances(t, c, &awsec2.DescribeInstancesInput{Filters: []types.Filter{filter("ip-address", ephemeral)}})
	if err != nil || !slices.Equal(ids(byIP), []string{publicID}) {
		t.Fatalf("by ip-address = %v, %v", ids(byIP), err)
	}

	alloc := allocate(t, c)
	out, err := c.AssociateAddress(t.Context(), &awsec2.AssociateAddressInput{AllocationId: alloc.AllocationId, InstanceId: aws.String(publicID)})
	if err != nil {
		t.Fatal(err)
	}
	if got := describeInstance(t, c, publicID); aws.ToString(got.PublicIpAddress) != aws.ToString(alloc.PublicIp) {
		t.Fatalf("public IP with an Elastic IP = %s, want %s", aws.ToString(got.PublicIpAddress), aws.ToString(alloc.PublicIp))
	}
	addrs, err := describeAddresses(t, c, &awsec2.DescribeAddressesInput{Filters: []types.Filter{filter("instance-id", publicID)}})
	if err != nil || len(addrs) != 1 {
		t.Fatalf("addresses by instance = %v, %v", addrs, err)
	}
	if _, err := c.DisassociateAddress(t.Context(), &awsec2.DisassociateAddressInput{AssociationId: out.AssociationId}); err != nil {
		t.Fatal(err)
	}
	if got := describeInstance(t, c, publicID); got.PublicIpAddress == nil || aws.ToString(got.PublicIpAddress) == aws.ToString(alloc.PublicIp) {
		t.Fatalf("public IP after disassociating = %v, want a new one EC2 assigned", got.PublicIpAddress)
	}
	if _, err := c.StopInstances(t.Context(), &awsec2.StopInstancesInput{InstanceIds: []string{publicID}}); err != nil {
		t.Fatal(err)
	}
	if got := describeInstance(t, c, publicID); got.PublicIpAddress != nil {
		t.Fatalf("a stopped instance kept public IP %s", aws.ToString(got.PublicIpAddress))
	}
	if _, err := c.StartInstances(t.Context(), &awsec2.StartInstancesInput{InstanceIds: []string{publicID}}); err != nil {
		t.Fatal(err)
	}
	if got := describeInstance(t, c, publicID); got.PublicIpAddress == nil {
		t.Fatal("a restarted instance has no public IP")
	}

	if _, err := c.TerminateInstances(t.Context(), &awsec2.TerminateInstancesInput{InstanceIds: []string{attachedID, publicID}}); err != nil {
		t.Fatal(err)
	}
	s.Settle()
	enis := s.NetworkInterfaces()
	if len(enis) != 1 || enis[0].NetworkInterfaceID != eni || enis[0].Status != "available" {
		t.Fatalf("interfaces after termination = %+v, want only %s, available", enis, eni)
	}
}

func TestAssociateAddressWithInstanceErrors(t *testing.T) {
	s := ec2.New(ec2.WithSettleAfter(-1))
	c := newClient(t, s)
	id := ids(run(t, c, runInput(putImage(t, s, ec2.Image{}))))[0]
	alloc := allocate(t, c).AllocationId
	for _, tc := range []struct {
		name string
		in   *awsec2.AssociateAddressInput
		code string
	}{
		{"a pending instance", &awsec2.AssociateAddressInput{AllocationId: alloc, InstanceId: aws.String(id)}, "InvalidInstanceID"},
		{"an unknown instance", &awsec2.AssociateAddressInput{AllocationId: alloc, InstanceId: aws.String("i-0000000000000dead")}, "InvalidInstanceID.NotFound"},
		{"no target", &awsec2.AssociateAddressInput{AllocationId: alloc}, "MissingParameter"},
		{"two targets", &awsec2.AssociateAddressInput{AllocationId: alloc, InstanceId: aws.String(id), NetworkInterfaceId: aws.String("eni-1")}, "InvalidParameterCombination"},
	} {
		if _, err := c.AssociateAddress(t.Context(), tc.in); code(err) != tc.code {
			t.Errorf("%s = %v, want %s", tc.name, err, tc.code)
		}
	}
}

func TestImages(t *testing.T) {
	s := ec2.New()
	c := newClient(t, s)
	mine := putImage(t, s, ec2.Image{Name: "base-1", Tags: map[string]string{"os": "linux"}})
	arm := putImage(t, s, ec2.Image{Name: "base-arm", Architecture: "arm64", RootDeviceName: "/dev/sda1", BlockDeviceMappings: []ec2.BlockDeviceMapping{
		{DeviceName: "/dev/sda1", SnapshotID: "snap-1", VolumeSize: 20, VolumeType: "gp2", DeleteOnTermination: true},
		{DeviceName: "/dev/sdb", VolumeSize: 100, VolumeType: "st1"},
	}})
	public := putImage(t, s, ec2.Image{Name: "vendor", OwnerID: "137112412989", OwnerAlias: "amazon", Public: true})
	describeImages := func(in *awsec2.DescribeImagesInput) ([]string, error) {
		out, err := c.DescribeImages(t.Context(), in)
		if err != nil {
			return nil, err
		}
		var got []string
		for _, img := range out.Images {
			got = append(got, aws.ToString(img.ImageId))
		}
		return got, nil
	}
	for _, tc := range []struct {
		name string
		in   *awsec2.DescribeImagesInput
		want []string
	}{
		{"all", &awsec2.DescribeImagesInput{}, []string{mine, arm, public}},
		{"by ID", &awsec2.DescribeImagesInput{ImageIds: []string{arm}}, []string{arm}},
		{"owned by self", &awsec2.DescribeImagesInput{Owners: []string{"self"}}, []string{mine, arm}},
		{"owned by amazon", &awsec2.DescribeImagesInput{Owners: []string{"amazon"}}, []string{public}},
		{"by name", &awsec2.DescribeImagesInput{Filters: []types.Filter{filter("name", "base-*")}}, []string{mine, arm}},
		{"by architecture", &awsec2.DescribeImagesInput{Filters: []types.Filter{filter("architecture", "arm64")}}, []string{arm}},
		{"by tag", &awsec2.DescribeImagesInput{Filters: []types.Filter{filter("tag:os", "linux")}}, []string{mine}},
		{"public", &awsec2.DescribeImagesInput{Filters: []types.Filter{filter("is-public", "true")}}, []string{public}},
	} {
		got, err := describeImages(tc.in)
		if err != nil || !slices.Equal(got, tc.want) {
			t.Errorf("%s = %v, %v; want %v", tc.name, got, err, tc.want)
		}
	}
	out, err := c.DescribeImages(t.Context(), &awsec2.DescribeImagesInput{ImageIds: []string{arm}})
	if err != nil {
		t.Fatal(err)
	}
	if img := out.Images[0]; img.Architecture != types.ArchitectureValuesArm64 || aws.ToString(img.RootDeviceName) != "/dev/sda1" ||
		len(img.BlockDeviceMappings) != 2 || aws.ToInt32(img.BlockDeviceMappings[0].Ebs.VolumeSize) != 20 || img.State != types.ImageStateAvailable {
		t.Fatalf("image = %+v", img)
	}
	if _, err := describeImages(&awsec2.DescribeImagesInput{ImageIds: []string{mine, "ami-0000000000000dead"}}); code(err) != "InvalidAMIID.NotFound" {
		t.Fatalf("an unknown image = %v", err)
	}
	if _, err := describeImages(&awsec2.DescribeImagesInput{ImageIds: []string{"image-1"}}); code(err) != "InvalidAMIID.Malformed" {
		t.Fatalf("a malformed image ID = %v", err)
	}

	in := runInput(arm)
	in.InstanceType = types.InstanceTypeT4gMicro
	in.BlockDeviceMappings = []types.BlockDeviceMapping{
		{DeviceName: aws.String("/dev/sda1"), Ebs: &types.EbsBlockDevice{VolumeSize: aws.Int32(50), VolumeType: types.VolumeTypeGp3}},
		{DeviceName: aws.String("/dev/sdc"), Ebs: &types.EbsBlockDevice{VolumeSize: aws.Int32(10), DeleteOnTermination: aws.Bool(false)}},
	}
	id := ids(run(t, c, in))[0]
	vols, err := c.DescribeVolumes(t.Context(), &awsec2.DescribeVolumesInput{Filters: []types.Filter{filter("attachment.instance-id", id)}})
	if err != nil || len(vols.Volumes) != 3 {
		t.Fatalf("volumes = %+v, %v", vols, err)
	}
	sizes := map[string]int32{}
	for _, v := range vols.Volumes {
		sizes[aws.ToString(v.Attachments[0].Device)] = aws.ToInt32(v.Size)
	}
	if sizes["/dev/sda1"] != 50 || sizes["/dev/sdb"] != 100 || sizes["/dev/sdc"] != 10 {
		t.Fatalf("volume sizes = %v", sizes)
	}
	if _, err := c.TerminateInstances(t.Context(), &awsec2.TerminateInstancesInput{InstanceIds: []string{id}}); err != nil {
		t.Fatal(err)
	}
	s.Settle()
	left := s.Volumes()
	if len(left) != 2 || left[0].State != "available" || left[1].State != "available" {
		t.Fatalf("volumes after termination = %+v, want the two not deleted on termination, available", left)
	}
}

func TestInstanceTypes(t *testing.T) {
	s := ec2.New()
	c := newClient(t, s)
	if err := s.PutInstanceType(ec2.InstanceType{InstanceType: "p5.48xlarge", VCPUs: 192, MemoryMiB: 2097152}); err != nil {
		t.Fatal(err)
	}
	out, err := c.DescribeInstanceTypes(t.Context(), &awsec2.DescribeInstanceTypesInput{InstanceTypes: []types.InstanceType{"p5.48xlarge", types.InstanceTypeT4gMicro}})
	if err != nil || len(out.InstanceTypes) != 2 {
		t.Fatalf("describe = %+v, %v", out, err)
	}
	if got := out.InstanceTypes[0]; got.InstanceType != "p5.48xlarge" || aws.ToInt32(got.VCpuInfo.DefaultVCpus) != 192 ||
		aws.ToInt64(got.MemoryInfo.SizeInMiB) != 2097152 || !slices.Equal(got.ProcessorInfo.SupportedArchitectures, []types.ArchitectureType{"x86_64"}) {
		t.Fatalf("p5 = %+v", got)
	}
	arm, err := c.DescribeInstanceTypes(t.Context(), &awsec2.DescribeInstanceTypesInput{Filters: []types.Filter{filter("processor-info.supported-architecture", "arm64")}})
	if err != nil || len(arm.InstanceTypes) != 2 {
		t.Fatalf("arm types = %+v, %v", arm, err)
	}
	if _, err := c.DescribeInstanceTypes(t.Context(), &awsec2.DescribeInstanceTypesInput{InstanceTypes: []types.InstanceType{"x9.huge"}}); code(err) != "InvalidInstanceType" {
		t.Fatalf("an unknown type = %v", err)
	}
	var all []types.InstanceType
	p := awsec2.NewDescribeInstanceTypesPaginator(c, &awsec2.DescribeInstanceTypesInput{MaxResults: aws.Int32(5)})
	for p.HasMorePages() {
		page, err := p.NextPage(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		for _, it := range page.InstanceTypes {
			all = append(all, it.InstanceType)
		}
	}
	if len(all) < 10 || !slices.Contains(all, "p5.48xlarge") {
		t.Fatalf("paged types = %v", all)
	}
}

func TestPlacementGroups(t *testing.T) {
	s := ec2.New()
	c := newClient(t, s)
	created, err := c.CreatePlacementGroup(t.Context(), &awsec2.CreatePlacementGroupInput{
		GroupName: aws.String("pack"), Strategy: types.PlacementStrategyPartition, PartitionCount: aws.Int32(3),
		TagSpecifications: tagSpec(types.ResourceTypePlacementGroup, "session", "s1"),
	})
	if err != nil {
		t.Fatal(err)
	}
	pg := created.PlacementGroup
	if aws.ToString(pg.GroupName) != "pack" || aws.ToString(pg.GroupId) == "" || pg.State != types.PlacementGroupStateAvailable ||
		aws.ToInt32(pg.PartitionCount) != 3 || tags(pg.Tags)["session"] != "s1" {
		t.Fatalf("created = %+v", pg)
	}
	if _, err := c.CreatePlacementGroup(t.Context(), &awsec2.CreatePlacementGroupInput{GroupName: aws.String("spread"), Strategy: types.PlacementStrategySpread}); err != nil {
		t.Fatal(err)
	}
	names := func(in *awsec2.DescribePlacementGroupsInput) ([]string, error) {
		out, err := c.DescribePlacementGroups(t.Context(), in)
		if err != nil {
			return nil, err
		}
		var got []string
		for _, g := range out.PlacementGroups {
			got = append(got, aws.ToString(g.GroupName))
		}
		return got, nil
	}
	for _, tc := range []struct {
		name string
		in   *awsec2.DescribePlacementGroupsInput
		want []string
	}{
		{"all", &awsec2.DescribePlacementGroupsInput{}, []string{"pack", "spread"}},
		{"by name", &awsec2.DescribePlacementGroupsInput{GroupNames: []string{"spread"}}, []string{"spread"}},
		{"by ID", &awsec2.DescribePlacementGroupsInput{GroupIds: []string{aws.ToString(pg.GroupId)}}, []string{"pack"}},
		{"by name filter", &awsec2.DescribePlacementGroupsInput{Filters: []types.Filter{filter("group-name", "sp*")}}, []string{"spread"}},
		{"by tag", &awsec2.DescribePlacementGroupsInput{Filters: []types.Filter{filter("tag:session", "s1")}}, []string{"pack"}},
	} {
		got, err := names(tc.in)
		if err != nil || !slices.Equal(got, tc.want) {
			t.Errorf("%s = %v, %v; want %v", tc.name, got, err, tc.want)
		}
	}
	in := runInput(putImage(t, s, ec2.Image{}))
	in.Placement = &types.Placement{GroupId: pg.GroupId}
	id := ids(run(t, c, in))[0]
	for _, tc := range []struct {
		name string
		call func() error
		code string
	}{
		{"create a duplicate", func() error {
			_, err := c.CreatePlacementGroup(t.Context(), &awsec2.CreatePlacementGroupInput{GroupName: aws.String("pack"), Strategy: types.PlacementStrategyCluster})
			return err
		}, "InvalidPlacementGroup.Duplicate"},
		{"create with a bad strategy", func() error {
			_, err := c.CreatePlacementGroup(t.Context(), &awsec2.CreatePlacementGroupInput{GroupName: aws.String("x"), Strategy: "line"})
			return err
		}, "InvalidParameterValue"},
		{"describe an unknown group", func() error {
			_, err := names(&awsec2.DescribePlacementGroupsInput{GroupNames: []string{"missing"}})
			return err
		}, "InvalidPlacementGroup.Unknown"},
		{"delete an unknown group", func() error {
			_, err := c.DeletePlacementGroup(t.Context(), &awsec2.DeletePlacementGroupInput{GroupName: aws.String("missing")})
			return err
		}, "InvalidPlacementGroup.Unknown"},
		{"delete a group in use", func() error {
			_, err := c.DeletePlacementGroup(t.Context(), &awsec2.DeletePlacementGroupInput{GroupName: aws.String("pack")})
			return err
		}, "InvalidPlacementGroup.InUse"},
	} {
		if err := tc.call(); code(err) != tc.code {
			t.Errorf("%s = %v, want %s", tc.name, err, tc.code)
		}
	}
	if got := describeInstance(t, c, id); aws.ToString(got.Placement.GroupName) != "pack" || aws.ToString(got.Placement.GroupId) != aws.ToString(pg.GroupId) {
		t.Fatalf("placement = %+v", got.Placement)
	}
	if _, err := c.TerminateInstances(t.Context(), &awsec2.TerminateInstancesInput{InstanceIds: []string{id}}); err != nil {
		t.Fatal(err)
	}
	s.Settle()
	if _, err := c.DeletePlacementGroup(t.Context(), &awsec2.DeletePlacementGroupInput{GroupName: aws.String("pack")}); err != nil {
		t.Fatalf("deleting a group whose instances terminated: %v", err)
	}
	if got := s.PlacementGroups(); len(got) != 1 || got[0].GroupName != "spread" || got[0].SpreadLevel != "rack" {
		t.Fatalf("groups = %+v", got)
	}
}

func TestVolumes(t *testing.T) {
	s := ec2.New(ec2.WithSettleAfter(1))
	c := newClient(t, s)
	created, err := c.CreateVolume(t.Context(), &awsec2.CreateVolumeInput{
		AvailabilityZone: aws.String(region + "a"), Size: aws.Int32(100), VolumeType: types.VolumeTypeGp3,
		TagSpecifications: tagSpec(types.ResourceTypeVolume, "data", "yes"), ClientToken: aws.String("vol-1"),
	})
	if err != nil {
		t.Fatal(err)
	}
	vol := aws.ToString(created.VolumeId)
	if created.State != types.VolumeStateCreating || aws.ToInt32(created.Iops) != 3000 || aws.ToInt32(created.Throughput) != 125 {
		t.Fatalf("created = %+v", created)
	}
	again, err := c.CreateVolume(t.Context(), &awsec2.CreateVolumeInput{
		AvailabilityZone: aws.String(region + "a"), Size: aws.Int32(100), VolumeType: types.VolumeTypeGp3,
		TagSpecifications: tagSpec(types.ResourceTypeVolume, "data", "yes"), ClientToken: aws.String("vol-1"),
	})
	if err != nil || aws.ToString(again.VolumeId) != vol {
		t.Fatalf("repeating the token = %+v, %v", again, err)
	}
	if _, err := c.CreateVolume(t.Context(), &awsec2.CreateVolumeInput{AvailabilityZone: aws.String(region + "a"), Size: aws.Int32(5), ClientToken: aws.String("vol-1")}); code(err) != "IdempotentParameterMismatch" {
		t.Fatalf("the token with other parameters = %v", err)
	}
	waitFast := func(o *awsec2.VolumeAvailableWaiterOptions) {
		o.MinDelay, o.MaxDelay = time.Millisecond, 5*time.Millisecond
	}
	if err := awsec2.NewVolumeAvailableWaiter(c, waitFast).Wait(t.Context(), &awsec2.DescribeVolumesInput{VolumeIds: []string{vol}}, 5*time.Second); err != nil {
		t.Fatalf("waiting for the volume: %v", err)
	}
	id := ids(run(t, c, runInput(putImage(t, s, ec2.Image{}))))[0]
	if _, err := c.AttachVolume(t.Context(), &awsec2.AttachVolumeInput{Device: aws.String("/dev/sdf"), InstanceId: aws.String(id), VolumeId: aws.String(vol)}); code(err) != "IncorrectInstanceState" {
		t.Fatalf("attaching to a pending instance = %v", err)
	}
	s.Settle()
	att, err := c.AttachVolume(t.Context(), &awsec2.AttachVolumeInput{Device: aws.String("/dev/sdf"), InstanceId: aws.String(id), VolumeId: aws.String(vol)})
	if err != nil || att.State != types.VolumeAttachmentStateAttaching {
		t.Fatalf("attach = %+v, %v", att, err)
	}
	inUse := func(o *awsec2.VolumeInUseWaiterOptions) {
		o.MinDelay, o.MaxDelay = time.Millisecond, 5*time.Millisecond
	}
	if err := awsec2.NewVolumeInUseWaiter(c, inUse).Wait(t.Context(), &awsec2.DescribeVolumesInput{VolumeIds: []string{vol}}, 5*time.Second); err != nil {
		t.Fatalf("waiting for the attachment: %v", err)
	}
	for _, tc := range []struct {
		filters []types.Filter
		want    int
	}{
		{[]types.Filter{filter("attachment.instance-id", id)}, 2},
		{[]types.Filter{filter("attachment.instance-id", id), filter("attachment.device", "/dev/sdf")}, 1},
		{[]types.Filter{filter("status", "in-use")}, 2},
		{[]types.Filter{filter("tag:data", "yes")}, 1},
		{[]types.Filter{filter("tag-key", "data"), filter("status", "available")}, 0},
		{[]types.Filter{filter("attachment.delete-on-termination", "true")}, 1},
	} {
		out, err := c.DescribeVolumes(t.Context(), &awsec2.DescribeVolumesInput{Filters: tc.filters})
		if err != nil || len(out.Volumes) != tc.want {
			t.Errorf("%s = %d, %v; want %d", describe(tc.filters), len(out.Volumes), err, tc.want)
		}
	}
	root := ""
	for _, v := range s.Volumes() {
		if v.Device == "/dev/xvda" {
			root = v.VolumeID
		}
	}
	for _, tc := range []struct {
		name string
		call func() error
		code string
	}{
		{"delete an attached volume", func() error {
			_, err := c.DeleteVolume(t.Context(), &awsec2.DeleteVolumeInput{VolumeId: aws.String(vol)})
			return err
		}, "VolumeInUse"},
		{"attach an attached volume", func() error {
			_, err := c.AttachVolume(t.Context(), &awsec2.AttachVolumeInput{Device: aws.String("/dev/sdg"), InstanceId: aws.String(id), VolumeId: aws.String(vol)})
			return err
		}, "VolumeInUse"},
		{"detach the root volume of a running instance", func() error {
			_, err := c.DetachVolume(t.Context(), &awsec2.DetachVolumeInput{VolumeId: aws.String(root)})
			return err
		}, "OperationNotPermitted"},
		{"describe an unknown volume", func() error {
			_, err := c.DescribeVolumes(t.Context(), &awsec2.DescribeVolumesInput{VolumeIds: []string{"vol-0000000000000dead"}})
			return err
		}, "InvalidVolume.NotFound"},
		{"create a volume in another region's zone", func() error {
			_, err := c.CreateVolume(t.Context(), &awsec2.CreateVolumeInput{AvailabilityZone: aws.String("eu-west-1a"), Size: aws.Int32(1)})
			return err
		}, "InvalidParameterValue"},
		{"create a volume without a size", func() error {
			_, err := c.CreateVolume(t.Context(), &awsec2.CreateVolumeInput{AvailabilityZone: aws.String(region + "a")})
			return err
		}, "MissingParameter"},
	} {
		if err := tc.call(); code(err) != tc.code {
			t.Errorf("%s = %v, want %s", tc.name, err, tc.code)
		}
	}
	det, err := c.DetachVolume(t.Context(), &awsec2.DetachVolumeInput{VolumeId: aws.String(vol)})
	if err != nil || det.State != types.VolumeAttachmentStateDetaching {
		t.Fatalf("detach = %+v, %v", det, err)
	}
	if err := awsec2.NewVolumeAvailableWaiter(c, waitFast).Wait(t.Context(), &awsec2.DescribeVolumesInput{VolumeIds: []string{vol}}, 5*time.Second); err != nil {
		t.Fatalf("waiting for the detachment: %v", err)
	}
	if _, err := c.DeleteVolume(t.Context(), &awsec2.DeleteVolumeInput{VolumeId: aws.String(vol)}); err != nil {
		t.Fatal(err)
	}
	deleted := func(o *awsec2.VolumeDeletedWaiterOptions) {
		o.MinDelay, o.MaxDelay = time.Millisecond, 5*time.Millisecond
	}
	if err := awsec2.NewVolumeDeletedWaiter(c, deleted).Wait(t.Context(), &awsec2.DescribeVolumesInput{VolumeIds: []string{vol}}, 5*time.Second); err != nil {
		t.Fatalf("waiting for deletion: %v", err)
	}
}

func TestDescribeVolumesPages(t *testing.T) {
	s := ec2.New()
	c := newClient(t, s)
	var want []string
	for range 7 {
		out, err := c.CreateVolume(t.Context(), &awsec2.CreateVolumeInput{AvailabilityZone: aws.String(region + "a"), Size: aws.Int32(1)})
		if err != nil {
			t.Fatal(err)
		}
		want = append(want, aws.ToString(out.VolumeId))
	}
	var got []string
	p := awsec2.NewDescribeVolumesPaginator(c, &awsec2.DescribeVolumesInput{MaxResults: aws.Int32(5)})
	for p.HasMorePages() {
		page, err := p.NextPage(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		for _, v := range page.Volumes {
			got = append(got, aws.ToString(v.VolumeId))
		}
	}
	if !slices.Equal(got, want) {
		t.Fatalf("pages = %v, want %v", got, want)
	}
}

func TestCreateAndDeleteTags(t *testing.T) {
	s := ec2.New()
	c := newClient(t, s)
	id := ids(run(t, c, runInput(putImage(t, s, ec2.Image{}), "keep", "1")))[0]
	vol := s.Volumes()[0].VolumeID
	eni := aws.ToString(createInterface(t, c).NetworkInterfaceId)
	alloc := aws.ToString(allocate(t, c).AllocationId)
	newTags := []types.Tag{{Key: aws.String("session"), Value: aws.String("s1")}, {Key: aws.String("role"), Value: aws.String("web")}}
	if _, err := c.CreateTags(t.Context(), &awsec2.CreateTagsInput{Resources: []string{id, vol, eni, alloc}, Tags: newTags}); err != nil {
		t.Fatal(err)
	}
	inst := describeInstance(t, c, id)
	if got := tags(inst.Tags); got["keep"] != "1" || got["session"] != "s1" || got["role"] != "web" {
		t.Fatalf("instance tags = %v", got)
	}
	if got := s.Volumes()[0].Tags; got["session"] != "s1" {
		t.Fatalf("volume tags = %v", got)
	}
	if a, _ := s.LookupAddress(region, alloc); a.Tags["role"] != "web" {
		t.Fatalf("address tags = %v", a.Tags)
	}
	if _, err := c.DeleteTags(t.Context(), &awsec2.DeleteTagsInput{Resources: []string{id}, Tags: []types.Tag{
		{Key: aws.String("session"), Value: aws.String("other")}, {Key: aws.String("role")},
	}}); err != nil {
		t.Fatal(err)
	}
	if got := tags(describeInstance(t, c, id).Tags); got["session"] != "s1" || got["role"] != "" || len(got) != 2 {
		t.Fatalf("tags after a delete by key and a delete by another value = %v", got)
	}
	if _, err := c.DeleteTags(t.Context(), &awsec2.DeleteTagsInput{Resources: []string{eni}}); err != nil {
		t.Fatal(err)
	}
	if ni, _ := s.LookupNetworkInterface(region, eni); len(ni.Tags) != 0 {
		t.Fatalf("interface tags after deleting all = %v", ni.Tags)
	}
	for _, tc := range []struct {
		resource string
		code     string
	}{
		{"i-0000000000000dead", "InvalidInstanceID.NotFound"},
		{"vol-0000000000000dead", "InvalidVolume.NotFound"},
		{"eni-0000000000000dead", "InvalidNetworkInterfaceID.NotFound"},
		{"snapshot-1", "InvalidID"},
	} {
		_, err := c.CreateTags(t.Context(), &awsec2.CreateTagsInput{Resources: []string{id, tc.resource}, Tags: []types.Tag{{Key: aws.String("x"), Value: aws.String("y")}}})
		if code(err) != tc.code {
			t.Errorf("tagging %s = %v, want %s", tc.resource, err, tc.code)
		}
	}
	if got := tags(describeInstance(t, c, id).Tags); got["x"] != "" {
		t.Fatalf("a failed CreateTags tagged the instance: %v", got)
	}
}

func TestInstanceFaults(t *testing.T) {
	s := ec2.New()
	c := newClient(t, s)
	image := putImage(t, s, ec2.Image{})
	keep := ids(run(t, c, runInput(image)))[0]
	target := ids(run(t, c, runInput(image)))[0]
	faults := fault.New().Add(
		fault.Rule{Method: "ec2.TerminateInstances", Resource: region + "/" + target, Fault: fault.Unavailable()},
		fault.Rule{Method: "ec2.DescribeInstances", Nth: 1, Fault: fault.Unavailable()},
	)
	s = ec2.New(ec2.WithFaults(faults))
	c = newClient(t, s)
	image = putImage(t, s, ec2.Image{})
	run(t, c, runInput(image))
	run(t, c, runInput(image))
	if _, err := describeInstances(t, c, &awsec2.DescribeInstancesInput{}); code(err) != "Unavailable" {
		t.Fatalf("the first describe = %v, want Unavailable", err)
	}
	if got, _ := s.LookupInstance(region, keep); got.State != "pending" {
		t.Fatalf("a rejected describe settled the instance: %s", got.State)
	}
	if _, err := c.TerminateInstances(t.Context(), &awsec2.TerminateInstancesInput{InstanceIds: []string{keep}}); err != nil {
		t.Fatalf("terminating another instance: %v", err)
	}
	if _, err := c.TerminateInstances(t.Context(), &awsec2.TerminateInstancesInput{InstanceIds: []string{target}}); code(err) != "Unavailable" {
		t.Fatalf("terminating the targeted instance = %v, want Unavailable", err)
	}
}
