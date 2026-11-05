package s3_test

import (
	"bytes"
	"crypto/md5"
	"encoding/hex"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	awss3 "github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"

	"github.com/parallelworks/squall/access"
	"github.com/parallelworks/squall/aws/s3"
	"github.com/parallelworks/squall/fault"
	"github.com/parallelworks/squall/sdktest/internal/sdkerr"
)

func startUpload(t *testing.T, c *awss3.Client, bucket, key string) string {
	t.Helper()
	out, err := c.CreateMultipartUpload(t.Context(), &awss3.CreateMultipartUploadInput{Bucket: aws.String(bucket), Key: aws.String(key)})
	if err != nil {
		t.Fatalf("create upload of %s: %v", key, err)
	}
	if aws.ToString(out.Bucket) != bucket || aws.ToString(out.Key) != key || aws.ToString(out.UploadId) == "" {
		t.Fatalf("create upload = %+v", out)
	}
	return aws.ToString(out.UploadId)
}

func uploadPart(t *testing.T, c *awss3.Client, bucket, key, id string, n int32, data []byte) types.CompletedPart {
	t.Helper()
	out, err := c.UploadPart(t.Context(), &awss3.UploadPartInput{
		Bucket: aws.String(bucket), Key: aws.String(key), UploadId: aws.String(id), PartNumber: aws.Int32(n), Body: bytes.NewReader(data),
	})
	if err != nil {
		t.Fatalf("upload part %d: %v", n, err)
	}
	return types.CompletedPart{PartNumber: aws.Int32(n), ETag: out.ETag}
}

func complete(t *testing.T, c *awss3.Client, bucket, key, id string, parts ...types.CompletedPart) (*awss3.CompleteMultipartUploadOutput, error) {
	t.Helper()
	return c.CompleteMultipartUpload(t.Context(), &awss3.CompleteMultipartUploadInput{
		Bucket: aws.String(bucket), Key: aws.String(key), UploadId: aws.String(id),
		MultipartUpload: &types.CompletedMultipartUpload{Parts: parts},
	})
}

func TestMultipartUploadCompletes(t *testing.T) {
	s := s3.New()
	c := newClient(t, s, "us-west-2")
	create(t, c, "data")
	if _, err := c.PutBucketVersioning(t.Context(), &awss3.PutBucketVersioningInput{
		Bucket:                  aws.String("data"),
		VersioningConfiguration: &types.VersioningConfiguration{Status: types.BucketVersioningStatusEnabled},
	}); err != nil {
		t.Fatal(err)
	}
	id := startUpload(t, c, "data", "big")
	first, last := bytes.Repeat([]byte("a"), 5<<20), []byte("tail")
	p1 := uploadPart(t, c, "data", "big", id, 1, first)
	uploadPart(t, c, "data", "big", id, 2, []byte("replaced"))
	p2 := uploadPart(t, c, "data", "big", id, 2, last)

	ups := s.MultipartUploads("data")
	if len(ups) != 1 || len(ups[0].Parts) != 2 || ups[0].Parts[0].Size != 5<<20 || ups[0].Parts[1].Size != 4 {
		t.Fatalf("MultipartUploads() = %+v", ups)
	}
	parts, err := c.ListParts(t.Context(), &awss3.ListPartsInput{Bucket: aws.String("data"), Key: aws.String("big"), UploadId: aws.String(id), MaxParts: aws.Int32(1)})
	if err != nil {
		t.Fatal(err)
	}
	if len(parts.Parts) != 1 || aws.ToInt32(parts.Parts[0].PartNumber) != 1 || !aws.ToBool(parts.IsTruncated) || aws.ToString(parts.NextPartNumberMarker) != "1" {
		t.Fatalf("ListParts = %+v", parts)
	}

	out, err := complete(t, c, "data", "big", id, p1, p2)
	if err != nil {
		t.Fatal(err)
	}
	sum1, sum2 := md5.Sum(first), md5.Sum(last)
	whole := md5.Sum(append(sum1[:], sum2[:]...))
	if want := fmt.Sprintf(`"%s-2"`, hex.EncodeToString(whole[:])); aws.ToString(out.ETag) != want || aws.ToString(out.VersionId) == "" {
		t.Fatalf("complete = %s %s, want ETag %s and a version", aws.ToString(out.ETag), aws.ToString(out.VersionId), want)
	}
	versions := s.ObjectVersions("data")
	if len(versions) != 1 || versions[0].Size != 5<<20+4 || versions[0].VersionID != aws.ToString(out.VersionId) {
		t.Fatalf("versions = %+v", versions)
	}
	if ups := s.MultipartUploads("data"); len(ups) != 0 {
		t.Fatalf("uploads after completing = %+v", ups)
	}
	if _, err := complete(t, c, "data", "big", id, p1, p2); sdkerr.AWSCode(err) != "NoSuchUpload" || sdkerr.AWSStatus(err) != http.StatusNotFound {
		t.Fatalf("completing again = %v, want 404 NoSuchUpload", err)
	}
}

func TestMultipartUploadErrors(t *testing.T) {
	s := s3.New()
	c := newClient(t, s, "us-east-1")
	create(t, c, "data")
	id := startUpload(t, c, "data", "k")
	small := uploadPart(t, c, "data", "k", id, 1, []byte("small"))
	other := uploadPart(t, c, "data", "k", id, 2, []byte("other"))

	for _, tc := range []struct {
		name  string
		parts []types.CompletedPart
		code  string
	}{
		{"parts out of order", []types.CompletedPart{other, small}, "InvalidPartOrder"},
		{"a part repeated", []types.CompletedPart{small, small}, "InvalidPartOrder"},
		{"a part never uploaded", []types.CompletedPart{small, {PartNumber: aws.Int32(3), ETag: other.ETag}}, "InvalidPart"},
		{"a part's ETag wrong", []types.CompletedPart{{PartNumber: aws.Int32(1), ETag: other.ETag}}, "InvalidPart"},
		{"a part under 5 MiB before the last", []types.CompletedPart{small, other}, "EntityTooSmall"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := complete(t, c, "data", "k", id, tc.parts...); sdkerr.AWSCode(err) != tc.code || sdkerr.AWSStatus(err) != http.StatusBadRequest {
				t.Fatalf("complete = %v, want 400 %s", err, tc.code)
			}
		})
	}
	if _, err := c.UploadPart(t.Context(), &awss3.UploadPartInput{
		Bucket: aws.String("data"), Key: aws.String("k"), UploadId: aws.String(id), PartNumber: aws.Int32(10_001), Body: strings.NewReader("x"),
	}); sdkerr.AWSCode(err) != "InvalidArgument" {
		t.Fatalf("part 10001 = %v, want InvalidArgument", err)
	}
	if _, err := c.UploadPart(t.Context(), &awss3.UploadPartInput{
		Bucket: aws.String("data"), Key: aws.String("other-key"), UploadId: aws.String(id), PartNumber: aws.Int32(1), Body: strings.NewReader("x"),
	}); sdkerr.AWSCode(err) != "NoSuchUpload" {
		t.Fatalf("a part for another key = %v, want NoSuchUpload", err)
	}
	if _, err := complete(t, c, "data", "k", id, small); err != nil {
		t.Fatalf("one small part = %v", err)
	}

	id = startUpload(t, c, "data", "k")
	if _, err := c.AbortMultipartUpload(t.Context(), &awss3.AbortMultipartUploadInput{Bucket: aws.String("data"), Key: aws.String("k"), UploadId: aws.String(id)}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.AbortMultipartUpload(t.Context(), &awss3.AbortMultipartUploadInput{Bucket: aws.String("data"), Key: aws.String("k"), UploadId: aws.String(id)}); sdkerr.AWSCode(err) != "NoSuchUpload" {
		t.Fatalf("aborting again = %v, want NoSuchUpload", err)
	}
	if _, err := c.ListParts(t.Context(), &awss3.ListPartsInput{Bucket: aws.String("data"), Key: aws.String("k"), UploadId: aws.String(id)}); sdkerr.AWSCode(err) != "NoSuchUpload" {
		t.Fatalf("parts of an aborted upload = %v, want NoSuchUpload", err)
	}
}

func TestListMultipartUploadsPagesByKeyAndInitiation(t *testing.T) {
	s := s3.New()
	c := newClient(t, s, "us-east-1")
	create(t, c, "data")
	var want []string
	for _, key := range []string{"b", "a", "b", "photos/1", "photos/2", "videos/1", "c"} {
		up, err := s.CreateMultipartUpload("data", key)
		if err != nil {
			t.Fatal(err)
		}
		want = append(want, key+" "+up.UploadID)
	}
	slices.SortStableFunc(want, func(x, y string) int { return strings.Compare(strings.Fields(x)[0], strings.Fields(y)[0]) })

	var got []string
	pages := 0
	p := awss3.NewListMultipartUploadsPaginator(c, &awss3.ListMultipartUploadsInput{Bucket: aws.String("data")}, func(o *awss3.ListMultipartUploadsPaginatorOptions) { o.Limit = 2 })
	for p.HasMorePages() {
		page, err := p.NextPage(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		pages++
		for _, u := range page.Uploads {
			got = append(got, aws.ToString(u.Key)+" "+aws.ToString(u.UploadId))
			if u.Initiated == nil || aws.ToString(u.Owner.ID) == "" {
				t.Errorf("upload = %+v", u)
			}
		}
	}
	if !slices.Equal(got, want) || pages != 4 {
		t.Fatalf("uploads in %d pages = %v, want %v", pages, got, want)
	}

	var keys, prefixes []string
	p = awss3.NewListMultipartUploadsPaginator(c, &awss3.ListMultipartUploadsInput{Bucket: aws.String("data"), Delimiter: aws.String("/")}, func(o *awss3.ListMultipartUploadsPaginatorOptions) { o.Limit = 1 })
	for p.HasMorePages() {
		page, err := p.NextPage(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		for _, u := range page.Uploads {
			keys = append(keys, aws.ToString(u.Key))
		}
		for _, cp := range page.CommonPrefixes {
			prefixes = append(prefixes, aws.ToString(cp.Prefix))
		}
	}
	if !slices.Equal(keys, []string{"a", "b", "b", "c"}) || !slices.Equal(prefixes, []string{"photos/", "videos/"}) {
		t.Fatalf("by delimiter = %v and %v", keys, prefixes)
	}

	out, err := c.ListMultipartUploads(t.Context(), &awss3.ListMultipartUploadsInput{Bucket: aws.String("data"), Prefix: aws.String("photos/")})
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Uploads) != 2 || aws.ToString(out.Prefix) != "photos/" || aws.ToBool(out.IsTruncated) {
		t.Fatalf("by prefix = %+v", out)
	}
}

func TestBucketWithOnlyUploadsDeletes(t *testing.T) {
	s := s3.New()
	c := newClient(t, s, "us-east-1")
	create(t, c, "data")
	startUpload(t, c, "data", "k")
	if _, err := c.DeleteBucket(t.Context(), &awss3.DeleteBucketInput{Bucket: aws.String("data")}); err != nil {
		t.Fatalf("delete a bucket with an upload = %v", err)
	}
	if ups := s.MultipartUploads("data"); ups != nil {
		t.Fatalf("uploads = %+v", ups)
	}
}

func TestLostCompletionLeavesTheObject(t *testing.T) {
	faults := fault.New().Add(fault.Rule{Method: "s3.CompleteMultipartUpload", Nth: 1, Fault: fault.LoseResponse()})
	s := s3.New(s3.WithFaults(faults))
	c := newClient(t, s, "us-east-1")
	create(t, c, "data")
	id := startUpload(t, c, "data", "k")
	part := uploadPart(t, c, "data", "k", id, 1, []byte("x"))
	if _, err := complete(t, c, "data", "k", id, part); err == nil {
		t.Fatal("complete with a lost response succeeded")
	}
	if _, err := complete(t, c, "data", "k", id, part); sdkerr.AWSCode(err) != "NoSuchUpload" {
		t.Fatalf("retry = %v, want NoSuchUpload", err)
	}
	if v := s.ObjectVersions("data"); len(v) != 1 || v[0].Key != "k" {
		t.Fatalf("versions = %+v", v)
	}
}

func TestMultipartPermissions(t *testing.T) {
	policy := access.Allow("s3:CreateBucket", "s3:PutObject", "s3:ListBucketMultipartUploads")
	s := s3.New(s3.WithPolicy(policy))
	c := newClient(t, s, "us-east-1")
	create(t, c, "data")
	id := startUpload(t, c, "data", "k")
	if _, err := c.ListMultipartUploads(t.Context(), &awss3.ListMultipartUploadsInput{Bucket: aws.String("data")}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.AbortMultipartUpload(t.Context(), &awss3.AbortMultipartUploadInput{Bucket: aws.String("data"), Key: aws.String("k"), UploadId: aws.String(id)}); sdkerr.AWSCode(err) != "AccessDenied" {
		t.Fatalf("abort = %v, want AccessDenied", err)
	}
	if _, err := c.ListParts(t.Context(), &awss3.ListPartsInput{Bucket: aws.String("data"), Key: aws.String("k"), UploadId: aws.String(id)}); !strings.Contains(fmt.Sprint(err), "s3:ListMultipartUploadParts") {
		t.Fatalf("list parts = %v, want it to name s3:ListMultipartUploadParts", err)
	}
}
