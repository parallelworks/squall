package ec2_test

import (
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsec2 "github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/ec2/types"

	"github.com/parallelworks/squall/access"
	"github.com/parallelworks/squall/aws/ec2"
	"github.com/parallelworks/squall/sdktest/internal/sdkerr"
)

func attach(t *testing.T, s *ec2.Server, c *awsec2.Client, instance, device string) string {
	t.Helper()
	vol := createVolume(t, c, 10)
	s.Settle()
	if _, err := c.AttachVolume(t.Context(), &awsec2.AttachVolumeInput{Device: aws.String(device), InstanceId: aws.String(instance), VolumeId: aws.String(vol)}); err != nil {
		t.Fatal(err)
	}
	return vol
}

func setDeleteOnTermination(t *testing.T, c *awsec2.Client, instance string, mappings ...types.InstanceBlockDeviceMappingSpecification) error {
	t.Helper()
	_, err := c.ModifyInstanceAttribute(t.Context(), &awsec2.ModifyInstanceAttributeInput{InstanceId: aws.String(instance), BlockDeviceMappings: mappings})
	return err
}

func mapping(device, volume string, deleteOnTermination *bool) types.InstanceBlockDeviceMappingSpecification {
	ebs := &types.EbsInstanceBlockDeviceSpecification{DeleteOnTermination: deleteOnTermination}
	if volume != "" {
		ebs.VolumeId = aws.String(volume)
	}
	return types.InstanceBlockDeviceMappingSpecification{DeviceName: aws.String(device), Ebs: ebs}
}

func TestModifyDeleteOnTerminationOfAttachedVolumes(t *testing.T) {
	s := ec2.New()
	c := newClient(t, s)
	id := ids(run(t, c, runInput(putImage(t, s, ec2.Image{}))))[0]
	s.Settle()
	goes, stays := attach(t, s, c, id, "/dev/sdf"), attach(t, s, c, id, "/dev/sdg")

	if err := setDeleteOnTermination(t, c, id, mapping("/dev/sdf", goes, aws.Bool(true))); err != nil {
		t.Fatalf("on a running instance, right after attaching: %v", err)
	}
	if err := setDeleteOnTermination(t, c, id, mapping("/dev/sdg", "", aws.Bool(false)), mapping("/dev/xvda", "", aws.Bool(false))); err != nil {
		t.Fatalf("by device alone: %v", err)
	}
	vols, err := c.DescribeVolumes(t.Context(), &awsec2.DescribeVolumesInput{VolumeIds: []string{goes, stays}})
	if err != nil {
		t.Fatal(err)
	}
	for _, v := range vols.Volumes {
		want := aws.ToString(v.VolumeId) == goes
		if got := aws.ToBool(v.Attachments[0].DeleteOnTermination); got != want {
			t.Errorf("volume %s DeleteOnTermination = %v, want %v", aws.ToString(v.VolumeId), got, want)
		}
	}
	devices := map[string]bool{}
	for _, d := range describeInstance(t, c, id).BlockDeviceMappings {
		devices[aws.ToString(d.DeviceName)] = aws.ToBool(d.Ebs.DeleteOnTermination)
	}
	if !devices["/dev/sdf"] || devices["/dev/sdg"] || devices["/dev/xvda"] || len(devices) != 3 {
		t.Fatalf("instance block devices = %v", devices)
	}
	var root string
	for _, v := range s.Volumes() {
		if v.Device == "/dev/xvda" {
			root = v.VolumeID
		}
		if v.DeleteOnTermination != (v.VolumeID == goes) {
			t.Errorf("Volumes() %s DeleteOnTermination = %v", v.VolumeID, v.DeleteOnTermination)
		}
	}

	if err := setDeleteOnTermination(t, c, id, mapping("/dev/sdg", "", nil)); err != nil {
		t.Fatal(err)
	}
	if err := setDeleteOnTermination(t, c, id, mapping("/dev/sdg", "", aws.Bool(false))); err != nil {
		t.Fatal(err)
	}

	if _, err := c.TerminateInstances(t.Context(), &awsec2.TerminateInstancesInput{InstanceIds: []string{id}}); err != nil {
		t.Fatal(err)
	}
	s.Settle()
	left := map[string]string{}
	for _, v := range s.Volumes() {
		left[v.VolumeID] = v.State
	}
	if _, ok := left[goes]; ok || left[stays] != "available" || left[root] != "available" || len(left) != 2 {
		t.Fatalf("volumes after termination = %v; want %s and %s available, %s deleted", left, stays, root, goes)
	}
}

func TestDeleteOnTerminationDefaultsToTrue(t *testing.T) {
	s := ec2.New()
	c := newClient(t, s)
	id := ids(run(t, c, runInput(putImage(t, s, ec2.Image{}))))[0]
	s.Settle()
	vol := attach(t, s, c, id, "/dev/sdf")
	if err := setDeleteOnTermination(t, c, id, mapping("/dev/sdf", "", nil)); err != nil {
		t.Fatal(err)
	}
	vols, err := c.DescribeVolumes(t.Context(), &awsec2.DescribeVolumesInput{VolumeIds: []string{vol}})
	if err != nil || !aws.ToBool(vols.Volumes[0].Attachments[0].DeleteOnTermination) {
		t.Fatalf("volume = %+v, %v", vols, err)
	}
}

func TestModifyDeleteOnTerminationErrors(t *testing.T) {
	s := ec2.New()
	c := newClient(t, s)
	image := putImage(t, s, ec2.Image{})
	id := ids(run(t, c, runInput(image)))[0]
	s.Settle()
	vol := attach(t, s, c, id, "/dev/sdf")
	other := attach(t, s, c, ids(run(t, c, runInput(image)))[0], "/dev/sdf")
	for _, tc := range []struct {
		name string
		call func() error
		code string
	}{
		{"a device with nothing attached", func() error {
			return setDeleteOnTermination(t, c, id, mapping("/dev/sdz", "", aws.Bool(true)))
		}, "InvalidBlockDeviceMapping"},
		{"another instance's volume", func() error {
			return setDeleteOnTermination(t, c, id, mapping("/dev/sdf", other, aws.Bool(true)))
		}, "InvalidBlockDeviceMapping"},
		{"one good and one unknown device", func() error {
			return setDeleteOnTermination(t, c, id, mapping("/dev/sdf", vol, aws.Bool(true)), mapping("/dev/sdz", "", aws.Bool(true)))
		}, "InvalidBlockDeviceMapping"},
		{"with the instance type", func() error {
			_, err := c.ModifyInstanceAttribute(t.Context(), &awsec2.ModifyInstanceAttributeInput{
				InstanceId: aws.String(id), InstanceType: &types.AttributeValue{Value: aws.String("m5.large")},
				BlockDeviceMappings: []types.InstanceBlockDeviceMappingSpecification{mapping("/dev/sdf", "", aws.Bool(true))},
			})
			return err
		}, "InvalidParameterCombination"},
		{"an unknown instance", func() error {
			return setDeleteOnTermination(t, c, "i-0000000000000dead", mapping("/dev/sdf", "", aws.Bool(true)))
		}, "InvalidInstanceID.NotFound"},
	} {
		if err := tc.call(); sdkerr.AWSCode(err) != tc.code {
			t.Errorf("%s = %v, want %s", tc.name, err, tc.code)
		}
	}
	for _, v := range s.Volumes() {
		if v.VolumeID == vol && v.DeleteOnTermination {
			t.Fatal("a failed modify changed the volume")
		}
	}
}

func TestModifyDeleteOnTerminationNeedsItsPermission(t *testing.T) {
	policy := access.Allow("ec2:RunInstances", "ec2:CreateTags", "ec2:CreateVolume", "ec2:AttachVolume")
	s := ec2.New(ec2.WithPolicy(policy))
	c := newClient(t, s)
	id := ids(run(t, c, runInput(putImage(t, s, ec2.Image{}))))[0]
	s.Settle()
	attach(t, s, c, id, "/dev/sdf")
	if err := setDeleteOnTermination(t, c, id, mapping("/dev/sdf", "", aws.Bool(true))); sdkerr.AWSCode(err) != "UnauthorizedOperation" {
		t.Fatalf("without ec2:ModifyInstanceAttribute = %v, want UnauthorizedOperation", err)
	}
	if denials := policy.Denials(); len(denials) != 1 || denials[0].Method != "ec2.ModifyInstanceAttribute" {
		t.Fatalf("Denials() = %+v", denials)
	}
}
