package s3_test

import (
	"bytes"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	awshttp "github.com/aws/aws-sdk-go-v2/aws/transport/http"
	"github.com/aws/aws-sdk-go-v2/credentials"
	awss3 "github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"

	"github.com/parallelworks/squall/aws/s3"
	"github.com/parallelworks/squall/fault"
	"github.com/parallelworks/squall/sdktest/internal/sdkerr"
	"github.com/parallelworks/squall/sdktest/internal/sdkhttp"
)

func serve(t *testing.T, s *s3.Server) string {
	t.Helper()
	srv := httptest.NewServer(s)
	t.Cleanup(srv.Close)
	return srv.URL
}

// clientAt returns a client for url in region that does not retry, so each call is one request.
func clientAt(url, region string, opts ...func(*awss3.Options)) *awss3.Client {
	return awss3.New(awss3.Options{
		Region:           region,
		BaseEndpoint:     aws.String(url),
		Credentials:      credentials.NewStaticCredentialsProvider("AKID", "SECRET", ""),
		HTTPClient:       sdkhttp.CopyBodies(awshttp.NewBuildableClient()),
		RetryMaxAttempts: 1,
		// config.LoadDefaultConfig sets this, and s3.New alone does not.
		RequestChecksumCalculation: aws.RequestChecksumCalculationWhenSupported,
	}, opts...)
}

func newClient(t *testing.T, s *s3.Server, region string) *awss3.Client {
	t.Helper()
	return clientAt(serve(t, s), region)
}

func create(t *testing.T, c *awss3.Client, name string) {
	t.Helper()
	in := &awss3.CreateBucketInput{Bucket: aws.String(name), ACL: types.BucketCannedACLPrivate}
	if region := c.Options().Region; region != "us-east-1" {
		in.CreateBucketConfiguration = &types.CreateBucketConfiguration{LocationConstraint: types.BucketLocationConstraint(region)}
	}
	if _, err := c.CreateBucket(t.Context(), in); err != nil {
		t.Fatalf("create %s: %v", name, err)
	}
}

func tag(t *testing.T, c *awss3.Client, name string, kv ...string) error {
	t.Helper()
	set := []types.Tag{}
	for i := 0; i < len(kv); i += 2 {
		set = append(set, types.Tag{Key: aws.String(kv[i]), Value: aws.String(kv[i+1])})
	}
	_, err := c.PutBucketTagging(t.Context(), &awss3.PutBucketTaggingInput{Bucket: aws.String(name), Tagging: &types.Tagging{TagSet: set}})
	return err
}

func TestCreateTagAndRead(t *testing.T) {
	s := s3.New()
	url := serve(t, s)
	east, west := clientAt(url, "us-east-1"), clientAt(url, "us-west-2")
	create(t, east, "east-bucket")
	create(t, west, "west-bucket")
	if err := tag(t, west, "west-bucket", "Owner", "abc", "Name", "data"); err != nil {
		t.Fatalf("tag: %v", err)
	}

	head, err := west.HeadBucket(t.Context(), &awss3.HeadBucketInput{Bucket: aws.String("west-bucket")})
	if err != nil || aws.ToString(head.BucketRegion) != "us-west-2" {
		t.Fatalf("head = %+v, %v", head, err)
	}
	tags, err := west.GetBucketTagging(t.Context(), &awss3.GetBucketTaggingInput{Bucket: aws.String("west-bucket")})
	if err != nil {
		t.Fatalf("get tags: %v", err)
	}
	got := map[string]string{}
	for _, tg := range tags.TagSet {
		got[aws.ToString(tg.Key)] = aws.ToString(tg.Value)
	}
	if len(got) != 2 || got["Owner"] != "abc" || got["Name"] != "data" {
		t.Fatalf("tags = %v", got)
	}
	if _, err := east.GetBucketTagging(t.Context(), &awss3.GetBucketTaggingInput{Bucket: aws.String("east-bucket")}); sdkerr.AWSCode(err) != "NoSuchTagSet" || sdkerr.AWSStatus(err) != http.StatusNotFound {
		t.Fatalf("untagged get = %v", err)
	}
	if err := tag(t, west, "west-bucket"); err != nil {
		t.Fatalf("empty tag set: %v", err)
	}
	if _, err := west.GetBucketTagging(t.Context(), &awss3.GetBucketTaggingInput{Bucket: aws.String("west-bucket")}); sdkerr.AWSCode(err) != "NoSuchTagSet" {
		t.Fatalf("get after an empty tag set = %v", err)
	}
	if b, ok := s.LookupBucket("west-bucket"); !ok || b.Region != "us-west-2" || b.Tags != nil || b.CreationDate.IsZero() {
		t.Fatalf("lookup = %+v, %v", b, ok)
	}
}

func TestCreateBucketErrors(t *testing.T) {
	s := s3.New()
	if err := s.PutForeignBucket("taken-name", "us-east-1"); err != nil {
		t.Fatal(err)
	}
	url := serve(t, s)
	east, west := clientAt(url, "us-east-1"), clientAt(url, "us-west-2")
	create(t, east, "mine-east")
	create(t, west, "mine-west")
	for _, tc := range []struct {
		name   string
		client *awss3.Client
		in     *awss3.CreateBucketInput
		status int
		code   string
	}{
		{"another account's name", east, &awss3.CreateBucketInput{Bucket: aws.String("taken-name")}, http.StatusConflict, "BucketAlreadyExists"},
		{"an owned name outside us-east-1", west, &awss3.CreateBucketInput{
			Bucket:                    aws.String("mine-west"),
			CreateBucketConfiguration: &types.CreateBucketConfiguration{LocationConstraint: "us-west-2"},
		}, http.StatusConflict, "BucketAlreadyOwnedByYou"},
		{"an invalid name", east, &awss3.CreateBucketInput{Bucket: aws.String("Bad_Name")}, http.StatusBadRequest, "InvalidBucketName"},
		{"no constraint outside us-east-1", west, &awss3.CreateBucketInput{Bucket: aws.String("new-west")}, http.StatusBadRequest, "IllegalLocationConstraintException"},
		{"a constraint for another region", west, &awss3.CreateBucketInput{
			Bucket:                    aws.String("new-west"),
			CreateBucketConfiguration: &types.CreateBucketConfiguration{LocationConstraint: "eu-west-1"},
		}, http.StatusBadRequest, "IllegalLocationConstraintException"},
		{"us-east-1 as a constraint", east, &awss3.CreateBucketInput{
			Bucket:                    aws.String("new-east"),
			CreateBucketConfiguration: &types.CreateBucketConfiguration{LocationConstraint: "us-east-1"},
		}, http.StatusBadRequest, "InvalidLocationConstraint"},
		{"a public ACL", east, &awss3.CreateBucketInput{Bucket: aws.String("new-east"), ACL: types.BucketCannedACLPublicRead}, http.StatusBadRequest, "InvalidBucketAclWithObjectOwnership"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := tc.client.CreateBucket(t.Context(), tc.in)
			if sdkerr.AWSStatus(err) != tc.status || sdkerr.AWSCode(err) != tc.code {
				t.Fatalf("create = %v, want %d %s", err, tc.status, tc.code)
			}
		})
	}
	if _, err := east.CreateBucket(t.Context(), &awss3.CreateBucketInput{Bucket: aws.String("mine-east")}); err != nil {
		t.Fatalf("creating an owned us-east-1 bucket again: %v", err)
	}
	if got := len(s.Buckets()); got != 2 {
		t.Fatalf("buckets = %d", got)
	}
}

func TestBucketsElsewhereAndOtherAccounts(t *testing.T) {
	s := s3.New()
	if err := s.PutForeignBucket("theirs", "us-west-2"); err != nil {
		t.Fatal(err)
	}
	url := serve(t, s)
	east, west := clientAt(url, "us-east-1"), clientAt(url, "us-west-2")
	create(t, east, "mine")

	for _, tc := range []struct {
		name, bucket string
		client       *awss3.Client
		head         string
		other        string
	}{
		{"a missing bucket", "nobody", east, "NotFound", "NoSuchBucket"},
		{"another account's bucket", "theirs", west, "Forbidden", "AccessDenied"},
		{"another account's bucket elsewhere", "theirs", east, "MovedPermanently", "PermanentRedirect"},
		{"the account's bucket elsewhere", "mine", west, "MovedPermanently", "PermanentRedirect"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := tc.client.HeadBucket(t.Context(), &awss3.HeadBucketInput{Bucket: aws.String(tc.bucket)})
			if sdkerr.AWSCode(err) != tc.head {
				t.Fatalf("head = %v, want %s", err, tc.head)
			}
			bucket := aws.String(tc.bucket)
			for name, call := range map[string]func() error{
				"tagging": func() error {
					_, err := tc.client.GetBucketTagging(t.Context(), &awss3.GetBucketTaggingInput{Bucket: bucket})
					return err
				},
				"versions": func() error {
					_, err := tc.client.ListObjectVersions(t.Context(), &awss3.ListObjectVersionsInput{Bucket: bucket})
					return err
				},
				"delete": func() error {
					_, err := tc.client.DeleteBucket(t.Context(), &awss3.DeleteBucketInput{Bucket: bucket})
					return err
				},
			} {
				if err := call(); sdkerr.AWSCode(err) != tc.other {
					t.Errorf("%s = %v, want %s", name, err, tc.other)
				}
			}
		})
	}
	_, err := west.HeadBucket(t.Context(), &awss3.HeadBucketInput{Bucket: aws.String("theirs")})
	if re, ok := errors.AsType[*awshttp.ResponseError](err); !ok || re.HTTPStatusCode() != http.StatusForbidden ||
		re.Response.Header.Get("x-amz-bucket-region") != "us-west-2" {
		t.Fatalf("denied head = %v", err)
	}
	_, err = west.GetBucketTagging(t.Context(), &awss3.GetBucketTaggingInput{Bucket: aws.String("mine")})
	if re, ok := errors.AsType[*awshttp.ResponseError](err); !ok || re.HTTPStatusCode() != http.StatusMovedPermanently ||
		re.Response.Header.Get("x-amz-bucket-region") != "us-east-1" {
		t.Fatalf("redirect = %v", err)
	}
	if _, ok := s.LookupBucket("mine"); !ok {
		t.Fatal("a redirected delete removed the bucket")
	}
}

func TestListBuckets(t *testing.T) {
	s := s3.New()
	url := serve(t, s)
	east, west := clientAt(url, "us-east-1"), clientAt(url, "us-west-2")
	for _, name := range []string{"app-a", "app-b", "logs"} {
		create(t, east, name)
	}
	create(t, west, "app-west")
	if err := s.PutForeignBucket("app-foreign", "us-east-1"); err != nil {
		t.Fatal(err)
	}

	names := func(in *awss3.ListBucketsInput) ([]string, []string, int) {
		t.Helper()
		var names, regions []string
		pages := 0
		for p := awss3.NewListBucketsPaginator(west, in); p.HasMorePages(); {
			out, err := p.NextPage(t.Context())
			if err != nil {
				t.Fatalf("list: %v", err)
			}
			pages++
			for _, b := range out.Buckets {
				names = append(names, aws.ToString(b.Name))
				regions = append(regions, aws.ToString(b.BucketRegion))
				if b.CreationDate == nil {
					t.Fatalf("bucket %s has no creation date", aws.ToString(b.Name))
				}
			}
		}
		return names, regions, pages
	}
	if got, regions, pages := names(&awss3.ListBucketsInput{}); !slices.Equal(got, []string{"app-a", "app-b", "app-west", "logs"}) || pages != 1 || regions[0] != "" {
		t.Fatalf("all = %v %v in %d pages", got, regions, pages)
	}
	got, regions, pages := names(&awss3.ListBucketsInput{MaxBuckets: aws.Int32(1)})
	if !slices.Equal(got, []string{"app-a", "app-b", "app-west", "logs"}) || pages != 4 ||
		!slices.Equal(regions, []string{"us-east-1", "us-east-1", "us-west-2", "us-east-1"}) {
		t.Fatalf("paged = %v %v in %d pages", got, regions, pages)
	}
	if got, regions, _ := names(&awss3.ListBucketsInput{Prefix: aws.String("app-")}); !slices.Equal(got, []string{"app-a", "app-b", "app-west"}) ||
		!slices.Equal(regions, []string{"us-east-1", "us-east-1", "us-west-2"}) {
		t.Fatalf("prefixed = %v %v", got, regions)
	}
	if got, _, _ := names(&awss3.ListBucketsInput{Prefix: aws.String("app-"), BucketRegion: aws.String("us-west-2")}); !slices.Equal(got, []string{"app-west"}) {
		t.Fatalf("filtered = %v", got)
	}
	if _, err := west.ListBuckets(t.Context(), &awss3.ListBucketsInput{BucketRegion: aws.String("us-east-1")}); sdkerr.AWSCode(err) != "InvalidArgument" {
		t.Fatalf("another region's buckets = %v", err)
	}
	if out, err := east.ListBuckets(t.Context(), &awss3.ListBucketsInput{BucketRegion: aws.String("us-east-1")}); err != nil || len(out.Buckets) != 3 {
		t.Fatalf("us-east-1 buckets = %+v, %v", out, err)
	}
	if _, err := west.ListBuckets(t.Context(), &awss3.ListBucketsInput{MaxBuckets: aws.Int32(0)}); sdkerr.AWSCode(err) != "InvalidArgument" {
		t.Fatalf("max-buckets 0 = %v", err)
	}
}

func TestTagRules(t *testing.T) {
	s := s3.New()
	c := newClient(t, s, "us-east-1")
	create(t, c, "tagged")
	many := make([]string, 0, 102)
	for i := range 51 {
		many = append(many, strings.Repeat("k", i+1), "v")
	}
	for name, kv := range map[string][]string{
		"an empty key":      {"", "v"},
		"a long key":        {strings.Repeat("k", 129), "v"},
		"a long value":      {"k", strings.Repeat("v", 257)},
		"a duplicate key":   {"k", "1", "k", "2"},
		"an aws: prefix":    {"aws:owner", "v"},
		"a disallowed rune": {"k", "a,b"},
		"too many tags":     many,
	} {
		if err := tag(t, c, "tagged", kv...); sdkerr.AWSCode(err) != "InvalidTag" {
			t.Errorf("%s = %v, want InvalidTag", name, err)
		}
	}
	if err := tag(t, c, "tagged", "Owner Name", "a+b=c.d_e:f/g@h", "empty", ""); err != nil {
		t.Fatalf("valid tags: %v", err)
	}
	if b, _ := s.LookupBucket("tagged"); len(b.Tags) != 2 || b.Tags["empty"] != "" {
		t.Fatalf("tags = %v", b.Tags)
	}
	if _, err := c.DeleteBucketTagging(t.Context(), &awss3.DeleteBucketTaggingInput{Bucket: aws.String("tagged")}); err != nil {
		t.Fatal(err)
	}
	if b, _ := s.LookupBucket("tagged"); b.Tags != nil {
		t.Fatalf("tags after delete = %v", b.Tags)
	}
}

// empty removes every version and delete marker, deleting each page before asking for the next.
func empty(t *testing.T, c *awss3.Client, bucket string, pageSize int32) int {
	t.Helper()
	removed := 0
	p := awss3.NewListObjectVersionsPaginator(c, &awss3.ListObjectVersionsInput{Bucket: aws.String(bucket), MaxKeys: aws.Int32(pageSize)})
	for p.HasMorePages() {
		page, err := p.NextPage(t.Context())
		if err != nil {
			t.Fatalf("list versions: %v", err)
		}
		var ids []types.ObjectIdentifier
		for _, v := range page.Versions {
			ids = append(ids, types.ObjectIdentifier{Key: v.Key, VersionId: v.VersionId})
		}
		for _, m := range page.DeleteMarkers {
			ids = append(ids, types.ObjectIdentifier{Key: m.Key, VersionId: m.VersionId})
		}
		if len(ids) == 0 {
			continue
		}
		out, err := c.DeleteObjects(t.Context(), &awss3.DeleteObjectsInput{Bucket: aws.String(bucket), Delete: &types.Delete{Objects: ids}})
		if err != nil || len(out.Errors) > 0 {
			t.Fatalf("delete objects = %+v, %v", out, err)
		}
		removed += len(out.Deleted)
	}
	return removed
}

func put(t *testing.T, c *awss3.Client, bucket, key string, body io.Reader) *awss3.PutObjectOutput {
	t.Helper()
	out, err := c.PutObject(t.Context(), &awss3.PutObjectInput{Bucket: aws.String(bucket), Key: aws.String(key), Body: body})
	if err != nil {
		t.Fatalf("put %s: %v", key, err)
	}
	return out
}

func TestEmptyAVersionedBucketThenDeleteIt(t *testing.T) {
	s := s3.New()
	c := newClient(t, s, "us-east-1")
	create(t, c, "versioned")
	if _, err := c.PutBucketVersioning(t.Context(), &awss3.PutBucketVersioningInput{
		Bucket:                  aws.String("versioned"),
		VersioningConfiguration: &types.VersioningConfiguration{Status: types.BucketVersioningStatusEnabled},
	}); err != nil {
		t.Fatal(err)
	}
	first := put(t, c, "versioned", "dir/a.txt", strings.NewReader("one"))
	put(t, c, "versioned", "dir/a.txt", bytes.NewReader([]byte("two")))
	put(t, c, "versioned", "b.txt", strings.NewReader("a third body"))
	if aws.ToString(first.ETag) != `"f97c5d29941bfb1b2fdab0874906ab82"` || len(aws.ToString(first.VersionId)) != 32 {
		t.Fatalf("put = etag %s version %s", aws.ToString(first.ETag), aws.ToString(first.VersionId))
	}
	del, err := c.DeleteObject(t.Context(), &awss3.DeleteObjectInput{Bucket: aws.String("versioned"), Key: aws.String("b.txt")})
	if err != nil || !aws.ToBool(del.DeleteMarker) || aws.ToString(del.VersionId) == "" {
		t.Fatalf("delete = %+v, %v", del, err)
	}

	versions := s.ObjectVersions("versioned")
	if len(versions) != 4 || versions[0].Key != "b.txt" || !versions[0].DeleteMarker || !versions[0].IsLatest ||
		versions[1].Size != int64(len("a third body")) || versions[1].IsLatest || versions[3].VersionID != aws.ToString(first.VersionId) {
		t.Fatalf("versions = %+v", versions)
	}
	if _, err := c.DeleteBucket(t.Context(), &awss3.DeleteBucketInput{Bucket: aws.String("versioned")}); sdkerr.AWSCode(err) != "BucketNotEmpty" || sdkerr.AWSStatus(err) != http.StatusConflict {
		t.Fatalf("delete a bucket with versions = %v", err)
	}
	if n := empty(t, c, "versioned", 1); n != 4 {
		t.Fatalf("emptied %d versions, want 4", n)
	}
	if _, err := c.DeleteBucket(t.Context(), &awss3.DeleteBucketInput{Bucket: aws.String("versioned")}); err != nil {
		t.Fatalf("delete the empty bucket: %v", err)
	}
	if _, err := c.DeleteBucket(t.Context(), &awss3.DeleteBucketInput{Bucket: aws.String("versioned")}); sdkerr.AWSCode(err) != "NoSuchBucket" || sdkerr.AWSStatus(err) != http.StatusNotFound {
		t.Fatalf("delete again = %v", err)
	}
}

func TestUnversionedBucketsHoldNullVersions(t *testing.T) {
	s := s3.New()
	c := newClient(t, s, "us-east-1")
	create(t, c, "plain")
	for range 3 {
		put(t, c, "plain", "k", strings.NewReader("data"))
	}
	put(t, c, "plain", "other", strings.NewReader(""))
	out, err := c.ListObjectVersions(t.Context(), &awss3.ListObjectVersionsInput{Bucket: aws.String("plain")})
	if err != nil || len(out.Versions) != 2 || aws.ToString(out.Versions[0].VersionId) != "null" || !aws.ToBool(out.Versions[0].IsLatest) ||
		aws.ToString(out.Versions[0].Key) != "k" || aws.ToInt64(out.Versions[0].Size) != 4 || len(out.DeleteMarkers) != 0 {
		t.Fatalf("versions = %+v, %v", out, err)
	}
	if _, err := c.DeleteObject(t.Context(), &awss3.DeleteObjectInput{Bucket: aws.String("plain"), Key: aws.String("k")}); err != nil {
		t.Fatal(err)
	}
	if got := s.ObjectVersions("plain"); len(got) != 1 || got[0].Key != "other" {
		t.Fatalf("after delete = %+v", got)
	}
	if n := empty(t, c, "plain", 1000); n != 1 {
		t.Fatalf("emptied %d", n)
	}
	v, err := c.GetBucketVersioning(t.Context(), &awss3.GetBucketVersioningInput{Bucket: aws.String("plain")})
	if err != nil || v.Status != "" {
		t.Fatalf("versioning = %+v, %v", v, err)
	}
}

func TestSuspendedVersioningReplacesTheNullVersion(t *testing.T) {
	s := s3.New()
	if _, err := s.PutBucket(s3.Bucket{Name: "suspended", Region: "us-east-1", Versioning: "Enabled"}); err != nil {
		t.Fatal(err)
	}
	c := newClient(t, s, "us-east-1")
	put(t, c, "suspended", "k", strings.NewReader("versioned"))
	if _, err := c.PutBucketVersioning(t.Context(), &awss3.PutBucketVersioningInput{
		Bucket:                  aws.String("suspended"),
		VersioningConfiguration: &types.VersioningConfiguration{Status: types.BucketVersioningStatusSuspended},
	}); err != nil {
		t.Fatal(err)
	}
	put(t, c, "suspended", "k", strings.NewReader("null 1"))
	put(t, c, "suspended", "k", strings.NewReader("null 2"))
	if _, err := c.DeleteObject(t.Context(), &awss3.DeleteObjectInput{Bucket: aws.String("suspended"), Key: aws.String("k")}); err != nil {
		t.Fatal(err)
	}
	got := s.ObjectVersions("suspended")
	if len(got) != 2 || !got[0].DeleteMarker || got[0].VersionID != "null" || got[1].VersionID == "null" || got[1].DeleteMarker {
		t.Fatalf("versions = %+v", got)
	}
	v, err := c.GetBucketVersioning(t.Context(), &awss3.GetBucketVersioningInput{Bucket: aws.String("suspended")})
	if err != nil || v.Status != types.BucketVersioningStatusSuspended {
		t.Fatalf("versioning = %+v, %v", v, err)
	}
}

func TestAPageAfterADeletedNullVersion(t *testing.T) {
	s := s3.New()
	if _, err := s.PutBucket(s3.Bucket{Name: "mixed", Region: "us-east-1", Versioning: "Enabled"}); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if _, err := s.PutObject("mixed", "k", []byte("versioned")); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.PutBucket(s3.Bucket{Name: "mixed", Region: "us-east-1", Versioning: "Suspended"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.PutObject("mixed", "k", []byte("null")); err != nil {
		t.Fatal(err)
	}
	c := newClient(t, s, "us-east-1")
	if n := empty(t, c, "mixed", 1); n != 3 {
		t.Fatalf("emptied %d versions page by page, want 3", n)
	}
}

func TestListObjectVersionsPagesAndRejects(t *testing.T) {
	s := s3.New()
	if _, err := s.PutBucket(s3.Bucket{Name: "pages", Region: "us-east-1", Versioning: "Enabled"}); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"a", "a", "a", "b", "c/1", "c/2"} {
		if _, err := s.PutObject("pages", key, []byte(key)); err != nil {
			t.Fatal(err)
		}
	}
	c := newClient(t, s, "us-east-1")
	var keys []string
	for p := awss3.NewListObjectVersionsPaginator(c, &awss3.ListObjectVersionsInput{Bucket: aws.String("pages"), MaxKeys: aws.Int32(2)}); p.HasMorePages(); {
		page, err := p.NextPage(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		for _, v := range page.Versions {
			keys = append(keys, aws.ToString(v.Key))
		}
	}
	if !slices.Equal(keys, []string{"a", "a", "a", "b", "c/1", "c/2"}) {
		t.Fatalf("keys = %v", keys)
	}
	out, err := c.ListObjectVersions(t.Context(), &awss3.ListObjectVersionsInput{Bucket: aws.String("pages"), Prefix: aws.String("c/")})
	if err != nil || len(out.Versions) != 2 {
		t.Fatalf("prefix = %+v, %v", out, err)
	}
	if _, err := c.ListObjectVersions(t.Context(), &awss3.ListObjectVersionsInput{Bucket: aws.String("pages"), Delimiter: aws.String("/")}); sdkerr.AWSCode(err) != "InvalidArgument" {
		t.Fatalf("delimiter = %v", err)
	}
	if _, err := c.ListObjectVersions(t.Context(), &awss3.ListObjectVersionsInput{Bucket: aws.String("pages"), VersionIdMarker: aws.String("null")}); sdkerr.AWSCode(err) != "InvalidArgument" {
		t.Fatalf("version marker without key marker = %v", err)
	}
	if _, err := c.DeleteObject(t.Context(), &awss3.DeleteObjectInput{Bucket: aws.String("pages"), Key: aws.String("a"), VersionId: aws.String("bogus")}); sdkerr.AWSCode(err) != "InvalidArgument" {
		t.Fatalf("delete a malformed version = %v", err)
	}
	if _, err := c.PutObject(t.Context(), &awss3.PutObjectInput{Bucket: aws.String("pages"), Key: aws.String(strings.Repeat("k", 1025)), Body: strings.NewReader("x")}); sdkerr.AWSCode(err) != "KeyTooLongError" {
		t.Fatalf("a long key = %v", err)
	}
}

func TestPolicyCORSAndUnimplemented(t *testing.T) {
	s := s3.New()
	c := newClient(t, s, "us-east-1")
	create(t, c, "web")
	policy := `{"Version":"2012-10-17","Statement":[]}`
	if _, err := c.PutBucketPolicy(t.Context(), &awss3.PutBucketPolicyInput{Bucket: aws.String("web"), Policy: aws.String(policy)}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.PutBucketPolicy(t.Context(), &awss3.PutBucketPolicyInput{Bucket: aws.String("web"), Policy: aws.String("deny all")}); sdkerr.AWSCode(err) != "MalformedPolicy" {
		t.Fatalf("bad policy = %v", err)
	}
	rule := types.CORSRule{AllowedMethods: []string{"GET", "PUT"}, AllowedOrigins: []string{"*"}, MaxAgeSeconds: aws.Int32(3600)}
	if _, err := c.PutBucketCors(t.Context(), &awss3.PutBucketCorsInput{Bucket: aws.String("web"), CORSConfiguration: &types.CORSConfiguration{CORSRules: []types.CORSRule{rule}}}); err != nil {
		t.Fatal(err)
	}
	rule.AllowedMethods = []string{"PATCH"}
	if _, err := c.PutBucketCors(t.Context(), &awss3.PutBucketCorsInput{Bucket: aws.String("web"), CORSConfiguration: &types.CORSConfiguration{CORSRules: []types.CORSRule{rule}}}); sdkerr.AWSCode(err) != "InvalidRequest" {
		t.Fatalf("bad CORS = %v", err)
	}
	if b, _ := s.LookupBucket("web"); b.Policy != policy || !strings.Contains(b.CORS, "<AllowedMethod>PUT</AllowedMethod>") {
		t.Fatalf("bucket = %+v", b)
	}
	if _, err := c.GetObject(t.Context(), &awss3.GetObjectInput{Bucket: aws.String("web"), Key: aws.String("k")}); sdkerr.AWSCode(err) != "NotImplemented" || sdkerr.AWSStatus(err) != http.StatusNotFound {
		t.Fatalf("get object = %v", err)
	}
	if _, err := c.GetBucketLocation(t.Context(), &awss3.GetBucketLocationInput{Bucket: aws.String("web")}); sdkerr.AWSCode(err) != "NotImplemented" {
		t.Fatalf("get location = %v", err)
	}
}

func TestFaults(t *testing.T) {
	faults := fault.New().Add(
		fault.Rule{Method: "s3.CreateBucket", Nth: 1, Fault: fault.LoseResponse()},
		fault.Rule{Method: "s3.HeadBucket", Nth: 1, Fault: fault.LoseResponse()},
		fault.Rule{Method: "s3.GetBucketTagging", Resource: "locked", Fault: fault.Reject(http.StatusForbidden, "AccessDenied")},
		fault.Rule{Method: "s3.DeleteObjects", Nth: 1, Fault: fault.FailAfter(http.StatusInternalServerError)},
		fault.Rule{Method: "s3.ListBuckets", Nth: 1, Fault: fault.RateLimited()},
	)
	s := s3.New(s3.WithFaults(faults))
	c := newClient(t, s, "eu-west-1")
	in := &awss3.CreateBucketInput{Bucket: aws.String("lost"), CreateBucketConfiguration: &types.CreateBucketConfiguration{LocationConstraint: "eu-west-1"}}
	if _, err := c.CreateBucket(t.Context(), in); err == nil {
		t.Fatal("a lost create succeeded")
	}
	if _, ok := s.LookupBucket("lost"); !ok {
		t.Fatal("a lost create did not take effect")
	}
	if _, err := c.CreateBucket(t.Context(), in); sdkerr.AWSCode(err) != "BucketAlreadyOwnedByYou" {
		t.Fatalf("retried create = %v", err)
	}
	if _, err := c.HeadBucket(t.Context(), &awss3.HeadBucketInput{Bucket: aws.String("nothing")}); err == nil || sdkerr.AWSCode(err) == "NotFound" {
		t.Fatalf("a lost head = %v, want a transport error", err)
	}

	create(t, c, "locked")
	if _, err := c.GetBucketTagging(t.Context(), &awss3.GetBucketTaggingInput{Bucket: aws.String("locked")}); sdkerr.AWSCode(err) != "AccessDenied" || sdkerr.AWSStatus(err) != http.StatusForbidden {
		t.Fatalf("denied tagging = %v", err)
	}
	if _, err := s.PutObject("locked", "k", []byte("x")); err != nil {
		t.Fatal(err)
	}
	_, err := c.DeleteObjects(t.Context(), &awss3.DeleteObjectsInput{Bucket: aws.String("locked"), Delete: &types.Delete{Objects: []types.ObjectIdentifier{{Key: aws.String("k")}}}})
	if sdkerr.AWSCode(err) != "InternalError" || len(s.ObjectVersions("locked")) != 0 {
		t.Fatalf("delete objects failing after = %v, left %v", err, s.ObjectVersions("locked"))
	}
	if _, err := c.ListBuckets(t.Context(), &awss3.ListBucketsInput{}); sdkerr.AWSCode(err) != "SlowDown" || sdkerr.AWSStatus(err) != http.StatusServiceUnavailable {
		t.Fatalf("throttled list = %v", err)
	}
	if got := faults.Injected(); len(got) != 5 || got[0].Call.Resource != "lost" || got[3].Call.Resource != "locked" {
		t.Fatalf("injected = %v", got)
	}
}

// sweep empties and deletes every bucket, stopping at the first error.
func sweep(t *testing.T, c *awss3.Client) error {
	t.Helper()
	for p := awss3.NewListBucketsPaginator(c, &awss3.ListBucketsInput{}); p.HasMorePages(); {
		page, err := p.NextPage(t.Context())
		if err != nil {
			return err
		}
		for _, b := range page.Buckets {
			versions, err := c.ListObjectVersions(t.Context(), &awss3.ListObjectVersionsInput{Bucket: b.Name})
			if err != nil {
				return err
			}
			var ids []types.ObjectIdentifier
			for _, v := range versions.Versions {
				ids = append(ids, types.ObjectIdentifier{Key: v.Key, VersionId: v.VersionId})
			}
			if len(ids) > 0 {
				if _, err := c.DeleteObjects(t.Context(), &awss3.DeleteObjectsInput{Bucket: b.Name, Delete: &types.Delete{Objects: ids}}); err != nil {
					return err
				}
			}
			if _, err := c.DeleteBucket(t.Context(), &awss3.DeleteBucketInput{Bucket: b.Name}); err != nil && sdkerr.AWSCode(err) != "NoSuchBucket" {
				return err
			}
		}
	}
	return nil
}

func TestSweepsConvergeThroughFaults(t *testing.T) {
	for seed := range uint64(20) {
		faults := fault.New(fault.Seed(seed)).Random(0.3, fault.LoseResponse(), fault.Unavailable(), fault.FailAfter(http.StatusInternalServerError))
		s := s3.New(s3.WithFaults(faults))
		c := newClient(t, s, "us-east-1")
		for _, key := range []string{"a", "b", "c"} {
			if _, err := s.PutBucket(s3.Bucket{Name: "bucket-" + key, Region: "us-east-1", Tags: map[string]string{"Owner": "s1"}}); err != nil {
				t.Fatal(err)
			}
			if _, err := s.PutObject("bucket-"+key, key, []byte(key)); err != nil {
				t.Fatal(err)
			}
		}
		for range 100 {
			if sweep(t, c) == nil {
				break
			}
		}
		if got := s.Buckets(); len(got) != 0 {
			t.Fatalf("seed %d left %v after %v", seed, got, faults.Injected())
		}
	}
}

func TestChunkedUploadsOverTLS(t *testing.T) {
	s := s3.New()
	srv := httptest.NewTLSServer(s)
	t.Cleanup(srv.Close)
	tlsConfig := srv.Client().Transport.(*http.Transport).TLSClientConfig
	httpClient := awshttp.NewBuildableClient().WithTransportOptions(func(tr *http.Transport) { tr.TLSClientConfig = tlsConfig })
	c := clientAt(srv.URL, "us-east-1", func(o *awss3.Options) { o.HTTPClient = httpClient })
	create(t, c, "chunked")
	body := strings.Repeat("0123456789", 10_000)
	var sent http.Header
	_, err := c.PutObject(t.Context(), &awss3.PutObjectInput{Bucket: aws.String("chunked"), Key: aws.String("big"), Body: strings.NewReader(body)},
		func(o *awss3.Options) {
			o.HTTPClient = headerRecorder{httpClient, &sent}
		})
	if err != nil {
		t.Fatalf("put: %v", err)
	}
	if !strings.Contains(sent.Get("Content-Encoding"), "aws-chunked") {
		t.Fatalf("the SDK sent %v, not an aws-chunked body", sent)
	}
	if got := s.ObjectVersions("chunked"); len(got) != 1 || got[0].Size != int64(len(body)) {
		t.Fatalf("versions = %+v", got)
	}
}

type headerRecorder struct {
	next sdkhttp.Doer
	sent *http.Header
}

func (h headerRecorder) Do(req *http.Request) (*http.Response, error) {
	*h.sent = req.Header.Clone()
	return h.next.Do(req)
}
