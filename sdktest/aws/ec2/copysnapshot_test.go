package ec2_test

import (
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsec2 "github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/ec2/types"

	"github.com/parallelworks/squall/access"
	"github.com/parallelworks/squall/aws/ec2"
	"github.com/parallelworks/squall/sdktest/internal/sdkerr"
)

const westRegion = "us-west-2"

func copySnapshot(t *testing.T, c *awsec2.Client, source string, kv ...string) (*awsec2.CopySnapshotOutput, error) {
	t.Helper()
	return c.CopySnapshot(t.Context(), &awsec2.CopySnapshotInput{
		SourceRegion: aws.String(region), SourceSnapshotId: aws.String(source), Description: aws.String("copy of " + source),
		TagSpecifications: tagSpec(types.ResourceTypeSnapshot, kv...),
	})
}

func eastAndWest(t *testing.T, s *ec2.Server) (east, west *awsec2.Client) {
	t.Helper()
	url := serve(t, s)
	return clientAt(url, region, withRetries(1)), clientAt(url, westRegion, withRetries(1))
}

func TestCopySnapshotAcrossRegions(t *testing.T) {
	s := ec2.New(ec2.WithSettleAfter(1))
	east, west := eastAndWest(t, s)
	source := createSnapshot(t, east, createVolume(t, east, 30))
	s.Settle()

	out, err := copySnapshot(t, west, source, "copy-of", source)
	if err != nil {
		t.Fatal(err)
	}
	cp := aws.ToString(out.SnapshotId)
	if !strings.HasPrefix(cp, "snap-") || cp == source || tags(out.Tags)["copy-of"] != source {
		t.Fatalf("copy = %+v", out)
	}
	described, err := west.DescribeSnapshots(t.Context(), &awsec2.DescribeSnapshotsInput{SnapshotIds: []string{cp}})
	if err != nil {
		t.Fatal(err)
	}
	got := described.Snapshots[0]
	if got.State != types.SnapshotStatePending || aws.ToString(got.Progress) != "0%" || aws.ToString(got.VolumeId) != "vol-ffffffff" ||
		aws.ToInt32(got.VolumeSize) != 30 || aws.ToString(got.Description) != "copy of "+source || tags(got.Tags)["copy-of"] != source {
		t.Fatalf("copy described = %+v", got)
	}
	completed := func(o *awsec2.SnapshotCompletedWaiterOptions) {
		o.MinDelay, o.MaxDelay = time.Millisecond, 5*time.Millisecond
	}
	if err := awsec2.NewSnapshotCompletedWaiter(west, completed).Wait(t.Context(), &awsec2.DescribeSnapshotsInput{SnapshotIds: []string{cp}}, 5*time.Second); err != nil {
		t.Fatalf("waiting for the copy: %v", err)
	}
	if found, err := describeSnapshots(t, west, &awsec2.DescribeSnapshotsInput{
		OwnerIds: []string{"self"}, Filters: []types.Filter{filter("tag:copy-of", source), filter("status", "completed")},
	}); err != nil || !slices.Equal(found, []string{cp}) {
		t.Fatalf("copies by tag = %v, %v", found, err)
	}
	if found, err := describeSnapshots(t, east, &awsec2.DescribeSnapshotsInput{Filters: []types.Filter{filter("tag:copy-of", source)}}); err != nil || len(found) != 0 {
		t.Fatalf("copies by tag in the source region = %v, %v; want none", found, err)
	}
	if _, err := west.DescribeSnapshots(t.Context(), &awsec2.DescribeSnapshotsInput{SnapshotIds: []string{source}}); sdkerr.AWSCode(err) != "InvalidSnapshot.NotFound" {
		t.Fatalf("the source in the destination region = %v, want InvalidSnapshot.NotFound", err)
	}

	byID := map[string]ec2.Snapshot{}
	for _, snap := range s.Snapshots() {
		byID[snap.SnapshotID] = snap
	}
	if src, c := byID[source], byID[cp]; src.Region != region || c.Region != westRegion || c.SourceRegion != region ||
		c.SourceSnapshotID != source || src.SourceSnapshotID != "" {
		t.Fatalf("snapshots = %+v", byID)
	}

	if _, err := west.RegisterImage(t.Context(), registerInput("golden-west", cp)); err != nil {
		t.Fatalf("registering an image from the copy: %v", err)
	}
	if _, err := east.DeleteSnapshot(t.Context(), &awsec2.DeleteSnapshotInput{SnapshotId: aws.String(source)}); err != nil {
		t.Fatalf("deleting the source: %v", err)
	}
	if found, err := describeSnapshots(t, west, &awsec2.DescribeSnapshotsInput{SnapshotIds: []string{cp}}); err != nil || len(found) != 1 {
		t.Fatalf("the copy after its source is deleted = %v, %v", found, err)
	}
}

func TestDeletingACopyLeavesTheSource(t *testing.T) {
	s := ec2.New()
	east, west := eastAndWest(t, s)
	source := createSnapshot(t, east, createVolume(t, east, 8))
	s.Settle()
	out, err := copySnapshot(t, west, source)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := west.DeleteSnapshot(t.Context(), &awsec2.DeleteSnapshotInput{SnapshotId: out.SnapshotId}); err != nil {
		t.Fatal(err)
	}
	if found, err := describeSnapshots(t, east, &awsec2.DescribeSnapshotsInput{SnapshotIds: []string{source}}); err != nil || len(found) != 1 {
		t.Fatalf("the source after its copy is deleted = %v, %v", found, err)
	}
}

func TestCopySnapshotErrors(t *testing.T) {
	s := ec2.New(ec2.WithSettleAfter(-1))
	east, west := eastAndWest(t, s)
	pending := createSnapshot(t, east, createVolume(t, east, 8))
	for _, tc := range []struct {
		name string
		call func() error
		code string
	}{
		{"copy a missing snapshot", func() error {
			_, err := copySnapshot(t, west, "snap-0000000000000dead")
			return err
		}, "InvalidSnapshot.NotFound"},
		{"copy from the wrong source region", func() error {
			_, err := west.CopySnapshot(t.Context(), &awsec2.CopySnapshotInput{SourceRegion: aws.String("eu-west-1"), SourceSnapshotId: aws.String(pending)})
			return err
		}, "InvalidSnapshot.NotFound"},
		{"copy a malformed snapshot ID", func() error {
			_, err := copySnapshot(t, west, "snapshot-1")
			return err
		}, "InvalidSnapshotID.Malformed"},
		{"copy a pending snapshot", func() error {
			_, err := copySnapshot(t, west, pending)
			return err
		}, "IncorrectState"},
		{"copy to an Outpost", func() error {
			_, err := west.CopySnapshot(t.Context(), &awsec2.CopySnapshotInput{
				SourceRegion: aws.String(region), SourceSnapshotId: aws.String(pending), DestinationOutpostArn: aws.String("arn:aws:outposts:us-west-2:123456789012:outpost/op-1"),
			})
			return err
		}, "UnknownParameter"},
	} {
		if err := tc.call(); sdkerr.AWSCode(err) != tc.code {
			t.Errorf("%s = %v, want %s", tc.name, err, tc.code)
		}
	}
	if got := s.Snapshots(); len(got) != 1 {
		t.Fatalf("snapshots after failed copies = %+v", got)
	}
}

func TestCopySnapshotNeedsItsPermission(t *testing.T) {
	policy := access.Allow("ec2:CreateVolume", "ec2:CreateSnapshot", "ec2:CreateTags")
	s := ec2.New(ec2.WithPolicy(policy))
	east, west := eastAndWest(t, s)
	source := createSnapshot(t, east, createVolume(t, east, 8))
	_, err := copySnapshot(t, west, source, "copy-of", source)
	if sdkerr.AWSCode(err) != "UnauthorizedOperation" || !strings.Contains(err.Error(), "ec2:CopySnapshot") {
		t.Fatalf("copy = %v, want UnauthorizedOperation naming ec2:CopySnapshot", err)
	}
	if got := s.Snapshots(); len(got) != 1 {
		t.Fatalf("a refused copy made a snapshot: %+v", got)
	}
	denials := policy.Denials()
	if len(denials) != 1 || denials[0].Method != "ec2.CopySnapshot" || !slices.Equal(denials[0].Missing, []string{"ec2:CopySnapshot"}) {
		t.Errorf("Denials() = %+v", denials)
	}
}
