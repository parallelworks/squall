package s3_test

import (
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	awss3 "github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"

	"github.com/parallelworks/squall/access"
	"github.com/parallelworks/squall/aws/s3"
	"github.com/parallelworks/squall/sdktest/internal/sdkerr"
)

func TestPolicyRefusesAnOperationByItsAction(t *testing.T) {
	policy := access.Allow("s3:CreateBucket")
	c := newClient(t, s3.New(s3.WithPolicy(policy)), "us-east-1")
	create(t, c, "data")

	_, err := c.ListObjectVersions(t.Context(), &awss3.ListObjectVersionsInput{Bucket: aws.String("data")})
	if sdkerr.AWSCode(err) != "AccessDenied" || sdkerr.AWSStatus(err) != http.StatusForbidden {
		t.Fatalf("list versions = %v, want 403 AccessDenied", err)
	}
	if !strings.Contains(err.Error(), "s3:ListBucketVersions") {
		t.Errorf("error = %v, want it to name s3:ListBucketVersions", err)
	}
}

func TestPolicyChecksDeleteObjectsKeyByKey(t *testing.T) {
	policy := access.Allow("s3:CreateBucket", "s3:PutBucketVersioning", "s3:PutObject", "s3:DeleteObject")
	c := newClient(t, s3.New(s3.WithPolicy(policy)), "us-east-1")
	create(t, c, "data")
	if _, err := c.PutBucketVersioning(t.Context(), &awss3.PutBucketVersioningInput{
		Bucket:                  aws.String("data"),
		VersioningConfiguration: &types.VersioningConfiguration{Status: types.BucketVersioningStatusEnabled},
	}); err != nil {
		t.Fatal(err)
	}
	a := put(t, c, "data", "a", strings.NewReader("a"))
	put(t, c, "data", "b", strings.NewReader("b"))

	out, err := c.DeleteObjects(t.Context(), &awss3.DeleteObjectsInput{
		Bucket: aws.String("data"),
		Delete: &types.Delete{Objects: []types.ObjectIdentifier{
			{Key: aws.String("a"), VersionId: a.VersionId},
			{Key: aws.String("b")},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Errors) != 1 || aws.ToString(out.Errors[0].Key) != "a" || aws.ToString(out.Errors[0].Code) != "AccessDenied" {
		t.Errorf("Errors = %+v, want AccessDenied for a's version", out.Errors)
	}
	if len(out.Deleted) != 1 || aws.ToString(out.Deleted[0].Key) != "b" {
		t.Errorf("Deleted = %+v, want b", out.Deleted)
	}
	denials := policy.Denials()
	if len(denials) != 1 || denials[0].Resource != "data/a" || !slices.Equal(denials[0].Missing, []string{"s3:DeleteObjectVersion"}) {
		t.Errorf("Denials() = %+v", denials)
	}
}
