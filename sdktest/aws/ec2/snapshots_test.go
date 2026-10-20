package ec2_test

import (
	"slices"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsec2 "github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/ec2/types"

	"github.com/parallelworks/squall/aws/ec2"
	"github.com/parallelworks/squall/fault"
)

func createVolume(t *testing.T, c *awsec2.Client, size int32) string {
	t.Helper()
	out, err := c.CreateVolume(t.Context(), &awsec2.CreateVolumeInput{AvailabilityZone: aws.String(region + "a"), Size: aws.Int32(size)})
	if err != nil {
		t.Fatal(err)
	}
	return aws.ToString(out.VolumeId)
}

func createSnapshot(t *testing.T, c *awsec2.Client, volume string, kv ...string) string {
	t.Helper()
	out, err := c.CreateSnapshot(t.Context(), &awsec2.CreateSnapshotInput{
		VolumeId: aws.String(volume), TagSpecifications: tagSpec(types.ResourceTypeSnapshot, kv...),
	})
	if err != nil {
		t.Fatal(err)
	}
	return aws.ToString(out.SnapshotId)
}

func describeSnapshots(t *testing.T, c *awsec2.Client, in *awsec2.DescribeSnapshotsInput) ([]string, error) {
	t.Helper()
	out, err := c.DescribeSnapshots(t.Context(), in)
	if err != nil {
		return nil, err
	}
	var got []string
	for _, snap := range out.Snapshots {
		got = append(got, aws.ToString(snap.SnapshotId))
	}
	return got, nil
}

func registerInput(name, snapshot string) *awsec2.RegisterImageInput {
	return &awsec2.RegisterImageInput{
		Name: aws.String(name), RootDeviceName: aws.String("/dev/xvda"), Architecture: types.ArchitectureValuesX8664,
		VirtualizationType: aws.String("hvm"), EnaSupport: aws.Bool(true),
		BlockDeviceMappings: []types.BlockDeviceMapping{{
			DeviceName: aws.String("/dev/xvda"), Ebs: &types.EbsBlockDevice{SnapshotId: aws.String(snapshot), VolumeType: types.VolumeTypeGp3},
		}},
	}
}

func TestSnapshots(t *testing.T) {
	s := ec2.New(ec2.WithSettleAfter(1))
	c := newClient(t, s)
	vol := createVolume(t, c, 30)
	out, err := c.CreateSnapshot(t.Context(), &awsec2.CreateSnapshotInput{
		VolumeId: aws.String(vol), Description: aws.String("nightly"), TagSpecifications: tagSpec(types.ResourceTypeSnapshot, "session", "s"),
	})
	if err != nil {
		t.Fatal(err)
	}
	snap := aws.ToString(out.SnapshotId)
	if out.State != types.SnapshotStatePending || aws.ToInt32(out.VolumeSize) != 30 || aws.ToString(out.VolumeId) != vol ||
		aws.ToString(out.Description) != "nightly" || tags(out.Tags)["session"] != "s" || out.StartTime == nil {
		t.Fatalf("created = %+v", out)
	}
	other := createSnapshot(t, c, vol)
	completed := func(o *awsec2.SnapshotCompletedWaiterOptions) {
		o.MinDelay, o.MaxDelay = time.Millisecond, 5*time.Millisecond
	}
	if err := awsec2.NewSnapshotCompletedWaiter(c, completed).Wait(t.Context(), &awsec2.DescribeSnapshotsInput{SnapshotIds: []string{snap}}, 5*time.Second); err != nil {
		t.Fatalf("waiting for the snapshot: %v", err)
	}
	for _, tc := range []struct {
		name string
		in   *awsec2.DescribeSnapshotsInput
		want []string
	}{
		{"by tag", &awsec2.DescribeSnapshotsInput{Filters: []types.Filter{filter("tag:session", "s")}}, []string{snap}},
		{"by tag key", &awsec2.DescribeSnapshotsInput{Filters: []types.Filter{filter("tag-key", "sess*")}}, []string{snap}},
		{"by ID", &awsec2.DescribeSnapshotsInput{SnapshotIds: []string{other}}, []string{other}},
		{"by snapshot-id", &awsec2.DescribeSnapshotsInput{Filters: []types.Filter{filter("snapshot-id", snap)}}, []string{snap}},
		{"by volume", &awsec2.DescribeSnapshotsInput{Filters: []types.Filter{filter("volume-id", vol)}}, []string{snap, other}},
		{"completed", &awsec2.DescribeSnapshotsInput{Filters: []types.Filter{filter("status", "completed")}}, []string{snap, other}},
		{"owned by self", &awsec2.DescribeSnapshotsInput{OwnerIds: []string{"self"}, Filters: []types.Filter{filter("tag:session", "s")}}, []string{snap}},
		{"owned by another account", &awsec2.DescribeSnapshotsInput{OwnerIds: []string{"123456789012"}}, nil},
	} {
		got, err := describeSnapshots(t, c, tc.in)
		if err != nil || !slices.Equal(got, tc.want) {
			t.Errorf("%s = %v, %v; want %v", tc.name, got, err, tc.want)
		}
	}
	described, err := c.DescribeSnapshots(t.Context(), &awsec2.DescribeSnapshotsInput{SnapshotIds: []string{snap}})
	if err != nil || described.Snapshots[0].State != types.SnapshotStateCompleted || aws.ToString(described.Snapshots[0].Progress) != "100%" {
		t.Fatalf("described = %+v, %v", described, err)
	}

	reg, err := c.RegisterImage(t.Context(), func() *awsec2.RegisterImageInput {
		in := registerInput("golden-1", snap)
		in.TagSpecifications = tagSpec(types.ResourceTypeImage, "session", "s")
		return in
	}())
	if err != nil {
		t.Fatal(err)
	}
	image := aws.ToString(reg.ImageId)
	images, err := c.DescribeImages(t.Context(), &awsec2.DescribeImagesInput{Owners: []string{"self"}, Filters: []types.Filter{filter("block-device-mapping.snapshot-id", snap)}})
	if err != nil || len(images.Images) != 1 {
		t.Fatalf("images by snapshot = %+v, %v", images, err)
	}
	img := images.Images[0]
	if aws.ToString(img.ImageId) != image || aws.ToString(img.Name) != "golden-1" || img.State != types.ImageStateAvailable ||
		aws.ToInt32(img.BlockDeviceMappings[0].Ebs.VolumeSize) != 30 || tags(img.Tags)["session"] != "s" {
		t.Fatalf("image = %+v", img)
	}
	available := func(o *awsec2.ImageAvailableWaiterOptions) {
		o.MinDelay, o.MaxDelay = time.Millisecond, 5*time.Millisecond
	}
	if err := awsec2.NewImageAvailableWaiter(c, available).Wait(t.Context(), &awsec2.DescribeImagesInput{ImageIds: []string{image}}, 5*time.Second); err != nil {
		t.Fatalf("waiting for the image: %v", err)
	}
	id := ids(run(t, c, runInput(image)))[0]
	s.Settle()
	if root := describeInstance(t, c, id).BlockDeviceMappings; len(root) != 1 {
		t.Fatalf("instance from the image has devices %+v", root)
	}
	if v, err := c.DescribeVolumes(t.Context(), &awsec2.DescribeVolumesInput{Filters: []types.Filter{filter("attachment.instance-id", id)}}); err != nil ||
		len(v.Volumes) != 1 || aws.ToString(v.Volumes[0].SnapshotId) != snap || aws.ToInt32(v.Volumes[0].Size) != 30 {
		t.Fatalf("root volume = %+v, %v", v, err)
	}

	for _, tc := range []struct {
		name string
		call func() error
		code string
	}{
		{"delete a snapshot an image uses", func() error {
			_, err := c.DeleteSnapshot(t.Context(), &awsec2.DeleteSnapshotInput{SnapshotId: aws.String(snap)})
			return err
		}, "InvalidSnapshot.InUse"},
		{"register a name in use", func() error {
			_, err := c.RegisterImage(t.Context(), registerInput("golden-1", other))
			return err
		}, "InvalidAMIName.Duplicate"},
		{"register an unknown snapshot", func() error {
			_, err := c.RegisterImage(t.Context(), registerInput("golden-2", "snap-0000000000000dead"))
			return err
		}, "InvalidSnapshot.NotFound"},
		{"register a malformed name", func() error {
			_, err := c.RegisterImage(t.Context(), registerInput("x", other))
			return err
		}, "InvalidAMIName.Malformed"},
		{"register a root volume smaller than its snapshot", func() error {
			in := registerInput("golden-2", other)
			in.BlockDeviceMappings[0].Ebs.VolumeSize = aws.Int32(10)
			_, err := c.RegisterImage(t.Context(), in)
			return err
		}, "InvalidBlockDeviceMapping"},
		{"register without a root snapshot", func() error {
			in := registerInput("golden-2", other)
			in.RootDeviceName = aws.String("/dev/sda1")
			_, err := c.RegisterImage(t.Context(), in)
			return err
		}, "InvalidBlockDeviceMapping"},
		{"register an instance store image", func() error {
			in := registerInput("golden-2", other)
			in.ImageLocation = aws.String("bucket/manifest.xml")
			_, err := c.RegisterImage(t.Context(), in)
			return err
		}, "UnknownParameter"},
		{"describe an unknown snapshot", func() error {
			_, err := c.DescribeSnapshots(t.Context(), &awsec2.DescribeSnapshotsInput{SnapshotIds: []string{"snap-0000000000000dead"}})
			return err
		}, "InvalidSnapshot.NotFound"},
		{"describe a malformed snapshot ID", func() error {
			_, err := c.DescribeSnapshots(t.Context(), &awsec2.DescribeSnapshotsInput{SnapshotIds: []string{"snapshot-1"}})
			return err
		}, "InvalidSnapshotID.Malformed"},
		{"snapshot an unknown volume", func() error {
			_, err := c.CreateSnapshot(t.Context(), &awsec2.CreateSnapshotInput{VolumeId: aws.String("vol-0000000000000dead")})
			return err
		}, "InvalidVolume.NotFound"},
		{"filter on an unsupported name", func() error {
			_, err := c.DescribeSnapshots(t.Context(), &awsec2.DescribeSnapshotsInput{Filters: []types.Filter{filter("owner-alias", "amazon")}})
			return err
		}, "InvalidParameterValue"},
	} {
		if err := tc.call(); code(err) != tc.code {
			t.Errorf("%s = %v, want %s", tc.name, err, tc.code)
		}
	}

	if _, err := c.DeregisterImage(t.Context(), &awsec2.DeregisterImageInput{ImageId: aws.String(image)}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.DeregisterImage(t.Context(), &awsec2.DeregisterImageInput{ImageId: aws.String(image)}); code(err) != "InvalidAMIID.NotFound" {
		t.Fatalf("deregistering it again = %v, want InvalidAMIID.NotFound", err)
	}
	if _, err := c.DeleteSnapshot(t.Context(), &awsec2.DeleteSnapshotInput{SnapshotId: aws.String(snap)}); err != nil {
		t.Fatalf("deleting the snapshot once its image is gone: %v", err)
	}
	if _, err := c.DeleteSnapshot(t.Context(), &awsec2.DeleteSnapshotInput{SnapshotId: aws.String(snap)}); code(err) != "InvalidSnapshot.NotFound" {
		t.Fatalf("deleting it again = %v, want InvalidSnapshot.NotFound", err)
	}
	if got := s.Snapshots(); len(got) != 1 || got[0].SnapshotID != other {
		t.Fatalf("snapshots = %+v", got)
	}
}

func TestVolumeFromASnapshotTakesItsSize(t *testing.T) {
	s := ec2.New()
	c := newClient(t, s)
	snap := createSnapshot(t, c, createVolume(t, c, 40))
	out, err := c.CreateVolume(t.Context(), &awsec2.CreateVolumeInput{AvailabilityZone: aws.String(region + "b"), SnapshotId: aws.String(snap)})
	if err != nil || aws.ToInt32(out.Size) != 40 || aws.ToString(out.SnapshotId) != snap || aws.ToInt32(out.Iops) != 120 {
		t.Fatalf("volume = %+v, %v", out, err)
	}
	if _, err := c.CreateTags(t.Context(), &awsec2.CreateTagsInput{Resources: []string{snap}, Tags: []types.Tag{{Key: aws.String("k"), Value: aws.String("v")}}}); err != nil {
		t.Fatal(err)
	}
	if got, err := describeSnapshots(t, c, &awsec2.DescribeSnapshotsInput{Filters: []types.Filter{filter("tag:k", "v")}}); err != nil || !slices.Equal(got, []string{snap}) {
		t.Fatalf("snapshots tagged after = %v, %v", got, err)
	}
}

func TestDescribeSnapshotsPages(t *testing.T) {
	s := ec2.New()
	c := newClient(t, s)
	vol := createVolume(t, c, 1)
	var want []string
	for range 7 {
		want = append(want, createSnapshot(t, c, vol))
	}
	var got []string
	p := awsec2.NewDescribeSnapshotsPaginator(c, &awsec2.DescribeSnapshotsInput{MaxResults: aws.Int32(5)})
	for p.HasMorePages() {
		page, err := p.NextPage(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		for _, snap := range page.Snapshots {
			got = append(got, aws.ToString(snap.SnapshotId))
		}
	}
	if !slices.Equal(got, want) {
		t.Fatalf("pages = %v, want %v", got, want)
	}
}

func TestSnapshotFaults(t *testing.T) {
	faults := fault.New()
	s := ec2.New(ec2.WithFaults(faults))
	c := newClient(t, s)
	vol := createVolume(t, c, 8)
	faults.Add(
		fault.Rule{Method: "ec2.CreateSnapshot", Nth: 1, Fault: fault.LoseResponse()},
		fault.Rule{Method: "ec2.RegisterImage", Nth: 1, Fault: fault.FailAfter(500)},
		fault.Rule{Method: "ec2.DeleteSnapshot", Nth: 1, Fault: fault.Unavailable()},
	)
	if _, err := c.CreateSnapshot(t.Context(), &awsec2.CreateSnapshotInput{VolumeId: aws.String(vol), TagSpecifications: tagSpec(types.ResourceTypeSnapshot, "session", "s")}); err == nil {
		t.Fatal("a snapshot whose response is lost succeeded")
	}
	got, err := describeSnapshots(t, c, &awsec2.DescribeSnapshotsInput{Filters: []types.Filter{filter("tag:session", "s")}})
	if err != nil || len(got) != 1 {
		t.Fatalf("snapshots after a lost response = %v, %v; want the one taken", got, err)
	}
	snap := got[0]
	if _, err := c.RegisterImage(t.Context(), registerInput("golden-1", snap)); code(err) != "InternalError" {
		t.Fatalf("a register that fails after taking effect = %v, want InternalError", err)
	}
	if _, err := c.RegisterImage(t.Context(), registerInput("golden-1", snap)); code(err) != "InvalidAMIName.Duplicate" {
		t.Fatalf("registering again = %v, want InvalidAMIName.Duplicate", err)
	}
	if imgs := s.Images(); len(imgs) != 1 {
		t.Fatalf("images = %+v", imgs)
	}
	if _, err := c.DeregisterImage(t.Context(), &awsec2.DeregisterImageInput{ImageId: aws.String(s.Images()[0].ImageID)}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.DeleteSnapshot(t.Context(), &awsec2.DeleteSnapshotInput{SnapshotId: aws.String(snap)}); code(err) != "Unavailable" {
		t.Fatalf("a rejected delete = %v, want Unavailable", err)
	}
	if len(s.Snapshots()) != 1 {
		t.Fatal("a rejected delete took effect")
	}
	if _, err := c.DeleteSnapshot(t.Context(), &awsec2.DeleteSnapshotInput{SnapshotId: aws.String(snap)}); err != nil {
		t.Fatal(err)
	}
}

func TestAFaultRuleCanTargetOneSnapshot(t *testing.T) {
	faults := fault.New()
	s := ec2.New(ec2.WithFaults(faults))
	c := newClient(t, s)
	vol := createVolume(t, c, 8)
	keep, target := createSnapshot(t, c, vol), createSnapshot(t, c, vol)
	faults.Add(fault.Rule{Method: "ec2.DeleteSnapshot", Resource: region + "/" + target, Fault: fault.Unavailable()})
	if _, err := c.DeleteSnapshot(t.Context(), &awsec2.DeleteSnapshotInput{SnapshotId: aws.String(keep)}); err != nil {
		t.Fatalf("deleting another snapshot: %v", err)
	}
	if _, err := c.DeleteSnapshot(t.Context(), &awsec2.DeleteSnapshotInput{SnapshotId: aws.String(target)}); code(err) != "Unavailable" {
		t.Fatalf("deleting the targeted snapshot = %v, want Unavailable", err)
	}
}
