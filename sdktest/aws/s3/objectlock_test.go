package s3_test

import (
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awss3 "github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"

	"github.com/parallelworks/squall/access"
	"github.com/parallelworks/squall/aws/s3"
	"github.com/parallelworks/squall/sdktest/internal/sdkerr"
)

// clock is a settable clock for a server, safe to move while it serves.
type clock struct{ nanos atomic.Int64 }

func newClock() *clock {
	c := &clock{}
	c.nanos.Store(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC).UnixNano())
	return c
}

func (c *clock) now() time.Time                  { return time.Unix(0, c.nanos.Load()).UTC() }
func (c *clock) advance(d time.Duration)         { c.nanos.Add(int64(d)) }
func (c *clock) after(d time.Duration) time.Time { return c.now().Add(d) }

func createLocked(t *testing.T, c *awss3.Client, name string) {
	t.Helper()
	if _, err := c.CreateBucket(t.Context(), &awss3.CreateBucketInput{Bucket: aws.String(name), ObjectLockEnabledForBucket: aws.Bool(true)}); err != nil {
		t.Fatalf("create %s: %v", name, err)
	}
}

func deleteVersion(t *testing.T, c *awss3.Client, bucket, key, id string, bypass bool) error {
	t.Helper()
	in := &awss3.DeleteObjectInput{Bucket: aws.String(bucket), Key: aws.String(key), VersionId: aws.String(id)}
	if bypass {
		in.BypassGovernanceRetention = aws.Bool(true)
	}
	_, err := c.DeleteObject(t.Context(), in)
	return err
}

func isLocked(err error) bool {
	return sdkerr.AWSCode(err) == "AccessDenied" && sdkerr.AWSStatus(err) == http.StatusForbidden && strings.Contains(err.Error(), "object lock")
}

func TestObjectLockBucket(t *testing.T) {
	s := s3.New()
	c := newClient(t, s, "us-east-1")
	createLocked(t, c, "vault")
	v, err := c.GetBucketVersioning(t.Context(), &awss3.GetBucketVersioningInput{Bucket: aws.String("vault")})
	if err != nil || v.Status != types.BucketVersioningStatusEnabled {
		t.Fatalf("versioning = %+v, %v", v, err)
	}
	if _, err := c.PutBucketVersioning(t.Context(), &awss3.PutBucketVersioningInput{
		Bucket:                  aws.String("vault"),
		VersioningConfiguration: &types.VersioningConfiguration{Status: types.BucketVersioningStatusSuspended},
	}); sdkerr.AWSCode(err) != "InvalidBucketState" || sdkerr.AWSStatus(err) != http.StatusConflict {
		t.Fatalf("suspend = %v, want 409 InvalidBucketState", err)
	}
	got, err := c.GetObjectLockConfiguration(t.Context(), &awss3.GetObjectLockConfigurationInput{Bucket: aws.String("vault")})
	if err != nil || got.ObjectLockConfiguration.ObjectLockEnabled != types.ObjectLockEnabledEnabled || got.ObjectLockConfiguration.Rule != nil {
		t.Fatalf("lock configuration = %+v, %v", got, err)
	}
	if _, err := c.PutObjectLockConfiguration(t.Context(), &awss3.PutObjectLockConfigurationInput{
		Bucket: aws.String("vault"),
		ObjectLockConfiguration: &types.ObjectLockConfiguration{
			ObjectLockEnabled: types.ObjectLockEnabledEnabled,
			Rule:              &types.ObjectLockRule{DefaultRetention: &types.DefaultRetention{Mode: types.ObjectLockRetentionModeGovernance, Days: aws.Int32(3)}},
		},
	}); err != nil {
		t.Fatal(err)
	}
	got, err = c.GetObjectLockConfiguration(t.Context(), &awss3.GetObjectLockConfigurationInput{Bucket: aws.String("vault")})
	if err != nil || got.ObjectLockConfiguration.Rule.DefaultRetention.Mode != types.ObjectLockRetentionModeGovernance ||
		aws.ToInt32(got.ObjectLockConfiguration.Rule.DefaultRetention.Days) != 3 {
		t.Fatalf("lock configuration = %+v, %v", got, err)
	}
	for _, d := range []*types.DefaultRetention{
		{Mode: types.ObjectLockRetentionModeGovernance, Days: aws.Int32(1), Years: aws.Int32(1)},
		{Mode: types.ObjectLockRetentionModeGovernance},
		{Days: aws.Int32(1)},
	} {
		if _, err := c.PutObjectLockConfiguration(t.Context(), &awss3.PutObjectLockConfigurationInput{
			Bucket:                  aws.String("vault"),
			ObjectLockConfiguration: &types.ObjectLockConfiguration{ObjectLockEnabled: types.ObjectLockEnabledEnabled, Rule: &types.ObjectLockRule{DefaultRetention: d}},
		}); sdkerr.AWSCode(err) != "MalformedXML" {
			t.Errorf("default retention %+v = %v, want MalformedXML", d, err)
		}
	}
	if _, err := c.PutObjectLockConfiguration(t.Context(), &awss3.PutObjectLockConfigurationInput{
		Bucket: aws.String("vault"),
		ObjectLockConfiguration: &types.ObjectLockConfiguration{
			ObjectLockEnabled: types.ObjectLockEnabledEnabled,
			Rule:              &types.ObjectLockRule{DefaultRetention: &types.DefaultRetention{Mode: types.ObjectLockRetentionModeGovernance, Days: aws.Int32(0)}},
		},
	}); sdkerr.AWSCode(err) != "InvalidArgument" {
		t.Errorf("zero days = %v, want InvalidArgument", err)
	}

	create(t, c, "plain")
	if _, err := c.GetObjectLockConfiguration(t.Context(), &awss3.GetObjectLockConfigurationInput{Bucket: aws.String("plain")}); sdkerr.AWSCode(err) != "ObjectLockConfigurationNotFoundError" || sdkerr.AWSStatus(err) != http.StatusNotFound {
		t.Fatalf("plain lock configuration = %v, want 404 ObjectLockConfigurationNotFoundError", err)
	}
	enable := &awss3.PutObjectLockConfigurationInput{
		Bucket:                  aws.String("plain"),
		ObjectLockConfiguration: &types.ObjectLockConfiguration{ObjectLockEnabled: types.ObjectLockEnabledEnabled},
	}
	if _, err := c.PutObjectLockConfiguration(t.Context(), enable); sdkerr.AWSCode(err) != "InvalidBucketState" {
		t.Fatalf("enabling without versioning = %v, want InvalidBucketState", err)
	}
	if _, err := c.PutObject(t.Context(), &awss3.PutObjectInput{
		Bucket: aws.String("plain"), Key: aws.String("k"), Body: strings.NewReader("x"), ObjectLockLegalHoldStatus: types.ObjectLockLegalHoldStatusOn,
	}); sdkerr.AWSCode(err) != "InvalidRequest" {
		t.Fatalf("a legal hold outside Object Lock = %v, want InvalidRequest", err)
	}
	if _, err := c.PutBucketVersioning(t.Context(), &awss3.PutBucketVersioningInput{
		Bucket:                  aws.String("plain"),
		VersioningConfiguration: &types.VersioningConfiguration{Status: types.BucketVersioningStatusEnabled},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.PutObjectLockConfiguration(t.Context(), enable); err != nil {
		t.Fatalf("enabling on a versioned bucket = %v", err)
	}
	if b, _ := s.LookupBucket("plain"); !b.ObjectLock {
		t.Fatalf("bucket = %+v", b)
	}
}

func TestLockedVersionsRefuseDeletes(t *testing.T) {
	clk := newClock()
	s := s3.New(s3.WithClock(clk.now))
	c := newClient(t, s, "us-east-1")
	createLocked(t, c, "vault")

	compliance, err := c.PutObject(t.Context(), &awss3.PutObjectInput{
		Bucket: aws.String("vault"), Key: aws.String("c"), Body: strings.NewReader("c"),
		ObjectLockMode: types.ObjectLockModeCompliance, ObjectLockRetainUntilDate: aws.Time(clk.after(48 * time.Hour)),
	})
	if err != nil {
		t.Fatal(err)
	}
	governance, err := c.PutObject(t.Context(), &awss3.PutObjectInput{
		Bucket: aws.String("vault"), Key: aws.String("g"), Body: strings.NewReader("g"),
		ObjectLockMode: types.ObjectLockModeGovernance, ObjectLockRetainUntilDate: aws.Time(clk.after(48 * time.Hour)),
	})
	if err != nil {
		t.Fatal(err)
	}
	held, err := c.PutObject(t.Context(), &awss3.PutObjectInput{
		Bucket: aws.String("vault"), Key: aws.String("h"), Body: strings.NewReader("h"), ObjectLockLegalHoldStatus: types.ObjectLockLegalHoldStatusOn,
	})
	if err != nil {
		t.Fatal(err)
	}

	for _, key := range []string{"c", "g", "h"} {
		out, err := c.DeleteObject(t.Context(), &awss3.DeleteObjectInput{Bucket: aws.String("vault"), Key: aws.String(key)})
		if err != nil || !aws.ToBool(out.DeleteMarker) {
			t.Fatalf("a simple delete of %s = %+v, %v, want a delete marker", key, out, err)
		}
	}
	if err := deleteVersion(t, c, "vault", "c", aws.ToString(compliance.VersionId), true); !isLocked(err) {
		t.Fatalf("delete a compliance version with bypass = %v, want AccessDenied for the lock", err)
	}
	if err := deleteVersion(t, c, "vault", "g", aws.ToString(governance.VersionId), false); !isLocked(err) {
		t.Fatalf("delete a governance version = %v, want AccessDenied for the lock", err)
	}
	if err := deleteVersion(t, c, "vault", "h", aws.ToString(held.VersionId), true); !isLocked(err) {
		t.Fatalf("delete a held version with bypass = %v, want AccessDenied for the lock", err)
	}
	if err := deleteVersion(t, c, "vault", "g", aws.ToString(governance.VersionId), true); err != nil {
		t.Fatalf("delete a governance version with bypass = %v", err)
	}

	out, err := c.DeleteObjects(t.Context(), &awss3.DeleteObjectsInput{
		Bucket: aws.String("vault"),
		Delete: &types.Delete{Objects: []types.ObjectIdentifier{
			{Key: aws.String("c"), VersionId: compliance.VersionId},
			{Key: aws.String("h"), VersionId: held.VersionId},
			{Key: aws.String("g")},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Errors) != 2 || aws.ToString(out.Errors[0].Code) != "AccessDenied" || aws.ToString(out.Errors[1].Key) != "h" || len(out.Deleted) != 1 {
		t.Fatalf("DeleteObjects = errors %+v, deleted %+v", out.Errors, out.Deleted)
	}
	if _, err := c.DeleteBucket(t.Context(), &awss3.DeleteBucketInput{Bucket: aws.String("vault")}); sdkerr.AWSCode(err) != "BucketNotEmpty" {
		t.Fatalf("delete the bucket = %v, want BucketNotEmpty", err)
	}

	clk.advance(49 * time.Hour)
	if err := deleteVersion(t, c, "vault", "c", aws.ToString(compliance.VersionId), false); err != nil {
		t.Fatalf("delete an expired compliance version = %v", err)
	}
	if err := deleteVersion(t, c, "vault", "h", aws.ToString(held.VersionId), false); !isLocked(err) {
		t.Fatalf("delete a held version = %v, want AccessDenied for the lock", err)
	}
	if _, err := c.PutObjectLegalHold(t.Context(), &awss3.PutObjectLegalHoldInput{
		Bucket: aws.String("vault"), Key: aws.String("h"), VersionId: held.VersionId, LegalHold: &types.ObjectLockLegalHold{Status: types.ObjectLockLegalHoldStatusOff},
	}); err != nil {
		t.Fatal(err)
	}
	if err := deleteVersion(t, c, "vault", "h", aws.ToString(held.VersionId), false); err != nil {
		t.Fatalf("delete a released version = %v", err)
	}
}

func TestRetentionAndLegalHold(t *testing.T) {
	clk := newClock()
	s := s3.New(s3.WithClock(clk.now))
	c := newClient(t, s, "us-east-1")
	createLocked(t, c, "vault")
	put(t, c, "vault", "k", strings.NewReader("k"))

	if _, err := c.GetObjectRetention(t.Context(), &awss3.GetObjectRetentionInput{Bucket: aws.String("vault"), Key: aws.String("k")}); sdkerr.AWSCode(err) != "NoSuchObjectLockConfiguration" {
		t.Fatalf("retention of an unlocked version = %v, want NoSuchObjectLockConfiguration", err)
	}
	retain := func(mode types.ObjectLockRetentionMode, until time.Time, bypass bool) error {
		in := &awss3.PutObjectRetentionInput{Bucket: aws.String("vault"), Key: aws.String("k"), Retention: &types.ObjectLockRetention{Mode: mode, RetainUntilDate: aws.Time(until)}}
		if bypass {
			in.BypassGovernanceRetention = aws.Bool(true)
		}
		_, err := c.PutObjectRetention(t.Context(), in)
		return err
	}
	if err := retain(types.ObjectLockRetentionModeGovernance, clk.after(-time.Hour), false); sdkerr.AWSCode(err) != "InvalidArgument" {
		t.Fatalf("retention in the past = %v, want InvalidArgument", err)
	}
	if err := retain(types.ObjectLockRetentionModeGovernance, clk.after(24*time.Hour), false); err != nil {
		t.Fatal(err)
	}
	if err := retain(types.ObjectLockRetentionModeGovernance, clk.after(48*time.Hour), false); err != nil {
		t.Fatalf("extend = %v", err)
	}
	if err := retain(types.ObjectLockRetentionModeGovernance, clk.after(time.Hour), false); !isLocked(err) {
		t.Fatalf("shorten = %v, want AccessDenied for the lock", err)
	}
	if err := retain(types.ObjectLockRetentionModeCompliance, clk.after(time.Hour), true); err != nil {
		t.Fatalf("shorten with bypass = %v", err)
	}
	got, err := c.GetObjectRetention(t.Context(), &awss3.GetObjectRetentionInput{Bucket: aws.String("vault"), Key: aws.String("k")})
	if err != nil || got.Retention.Mode != types.ObjectLockRetentionModeCompliance || !got.Retention.RetainUntilDate.Equal(clk.after(time.Hour)) {
		t.Fatalf("retention = %+v, %v", got, err)
	}
	if err := retain(types.ObjectLockRetentionModeGovernance, clk.after(2*time.Hour), true); !isLocked(err) {
		t.Fatalf("weaken compliance with bypass = %v, want AccessDenied for the lock", err)
	}

	if _, err := c.GetObjectLegalHold(t.Context(), &awss3.GetObjectLegalHoldInput{Bucket: aws.String("vault"), Key: aws.String("k")}); sdkerr.AWSCode(err) != "NoSuchObjectLockConfiguration" {
		t.Fatalf("legal hold never set = %v, want NoSuchObjectLockConfiguration", err)
	}
	if err := s.SetLegalHold("vault", "k", "", true); err != nil {
		t.Fatal(err)
	}
	hold, err := c.GetObjectLegalHold(t.Context(), &awss3.GetObjectLegalHoldInput{Bucket: aws.String("vault"), Key: aws.String("k")})
	if err != nil || hold.LegalHold.Status != types.ObjectLockLegalHoldStatusOn {
		t.Fatalf("legal hold = %+v, %v", hold, err)
	}
	versions := s.ObjectVersions("vault")
	if len(versions) != 1 || versions[0].RetentionMode != "COMPLIANCE" || !versions[0].LegalHold {
		t.Fatalf("versions = %+v", versions)
	}
	if _, err := c.GetObjectRetention(t.Context(), &awss3.GetObjectRetentionInput{Bucket: aws.String("vault"), Key: aws.String("k"), VersionId: aws.String(strings.Repeat("0", 30) + "ff")}); sdkerr.AWSCode(err) != "NoSuchVersion" {
		t.Fatalf("retention of a missing version = %v, want NoSuchVersion", err)
	}
}

func TestDefaultRetentionLocksNewVersions(t *testing.T) {
	clk := newClock()
	s := s3.New(s3.WithClock(clk.now))
	if _, err := s.PutBucket(s3.Bucket{
		Name: "vault", Region: "us-east-1", Versioning: "Enabled", ObjectLock: true,
		DefaultRetention: &s3.DefaultRetention{Mode: "COMPLIANCE", Days: 1},
	}); err != nil {
		t.Fatal(err)
	}
	c := newClient(t, s, "us-east-1")
	put(t, c, "vault", "k", strings.NewReader("k"))
	id := startUpload(t, c, "vault", "big")
	part := uploadPart(t, c, "vault", "big", id, 1, []byte("x"))
	if _, err := complete(t, c, "vault", "big", id, part); err != nil {
		t.Fatal(err)
	}
	for _, v := range s.ObjectVersions("vault") {
		if v.RetentionMode != "COMPLIANCE" || !v.RetainUntil.Equal(clk.after(24*time.Hour)) {
			t.Errorf("version = %+v, want a day's compliance retention", v)
		}
		if err := deleteVersion(t, c, "vault", v.Key, v.VersionID, true); !isLocked(err) {
			t.Errorf("delete %s = %v, want AccessDenied for the lock", v.Key, err)
		}
	}
	clk.advance(24 * time.Hour)
	for _, v := range s.ObjectVersions("vault") {
		if err := deleteVersion(t, c, "vault", v.Key, v.VersionID, false); err != nil {
			t.Errorf("delete %s after a day = %v", v.Key, err)
		}
	}
	if _, err := c.DeleteBucket(t.Context(), &awss3.DeleteBucketInput{Bucket: aws.String("vault")}); err != nil {
		t.Fatal(err)
	}
}

func TestBypassNeedsItsPermission(t *testing.T) {
	clk := newClock()
	policy := access.Allow("s3:CreateBucket", "s3:PutBucketObjectLockConfiguration", "s3:PutBucketVersioning", "s3:PutObject", "s3:DeleteObjectVersion")
	s := s3.New(s3.WithClock(clk.now), s3.WithPolicy(policy))
	c := newClient(t, s, "us-east-1")
	createLocked(t, c, "vault")
	out := put(t, c, "vault", "k", strings.NewReader("k"))
	if err := s.SetRetention("vault", "k", aws.ToString(out.VersionId), "GOVERNANCE", clk.after(time.Hour)); err != nil {
		t.Fatal(err)
	}
	err := deleteVersion(t, c, "vault", "k", aws.ToString(out.VersionId), true)
	if sdkerr.AWSCode(err) != "AccessDenied" || !strings.Contains(err.Error(), "s3:BypassGovernanceRetention") {
		t.Fatalf("bypass without the permission = %v, want AccessDenied naming s3:BypassGovernanceRetention", err)
	}
	if _, err := c.PutObject(t.Context(), &awss3.PutObjectInput{
		Bucket: aws.String("vault"), Key: aws.String("k"), Body: strings.NewReader("k"), ObjectLockLegalHoldStatus: types.ObjectLockLegalHoldStatusOn,
	}); sdkerr.AWSCode(err) != "AccessDenied" || !strings.Contains(err.Error(), "s3:PutObjectLegalHold") {
		t.Fatalf("a legal hold without its permission = %v", err)
	}
}

func TestRequestPayment(t *testing.T) {
	c := newClient(t, s3.New(), "us-east-1")
	create(t, c, "data")
	got, err := c.GetBucketRequestPayment(t.Context(), &awss3.GetBucketRequestPaymentInput{Bucket: aws.String("data")})
	if err != nil || got.Payer != types.PayerBucketOwner {
		t.Fatalf("payer = %+v, %v", got, err)
	}
	if _, err := c.PutBucketRequestPayment(t.Context(), &awss3.PutBucketRequestPaymentInput{
		Bucket: aws.String("data"), RequestPaymentConfiguration: &types.RequestPaymentConfiguration{Payer: types.PayerRequester},
	}); err != nil {
		t.Fatal(err)
	}
	got, err = c.GetBucketRequestPayment(t.Context(), &awss3.GetBucketRequestPaymentInput{Bucket: aws.String("data")})
	if err != nil || got.Payer != types.PayerRequester {
		t.Fatalf("payer = %+v, %v", got, err)
	}
	if _, err := c.PutBucketRequestPayment(t.Context(), &awss3.PutBucketRequestPaymentInput{
		Bucket: aws.String("data"), RequestPaymentConfiguration: &types.RequestPaymentConfiguration{Payer: "Nobody"},
	}); sdkerr.AWSCode(err) != "MalformedXML" {
		t.Fatalf("payer Nobody = %v, want MalformedXML", err)
	}
	put(t, c, "data", "k", strings.NewReader("k"))
}

func TestRetentionReadBackIsNotAShortening(t *testing.T) {
	s := s3.New(s3.WithClock(func() time.Time { return time.Date(2026, 1, 1, 0, 0, 0, 123456789, time.UTC) }))
	if _, err := s.PutBucket(s3.Bucket{
		Name: "vault", Region: "us-east-1", Versioning: "Enabled", ObjectLock: true,
		DefaultRetention: &s3.DefaultRetention{Mode: "COMPLIANCE", Days: 1},
	}); err != nil {
		t.Fatal(err)
	}
	c := newClient(t, s, "us-east-1")
	put(t, c, "vault", "k", strings.NewReader("k"))
	got, err := c.GetObjectRetention(t.Context(), &awss3.GetObjectRetentionInput{Bucket: aws.String("vault"), Key: aws.String("k")})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.PutObjectRetention(t.Context(), &awss3.PutObjectRetentionInput{
		Bucket: aws.String("vault"), Key: aws.String("k"), Retention: got.Retention,
	}); err != nil {
		t.Fatalf("putting back the retention read = %v", err)
	}
	if v := s.ObjectVersions("vault"); !v[0].RetainUntil.Equal(time.Date(2026, 1, 2, 0, 0, 0, 123000000, time.UTC)) {
		t.Fatalf("RetainUntil = %v, want it to the millisecond", v[0].RetainUntil)
	}
}

func TestLockedUploadPartsNeedAChecksum(t *testing.T) {
	s := s3.New()
	url := serve(t, s)
	c := clientAt(url, "us-east-1")
	createLocked(t, c, "vault")
	out, err := c.CreateMultipartUpload(t.Context(), &awss3.CreateMultipartUploadInput{
		Bucket: aws.String("vault"), Key: aws.String("k"), ObjectLockLegalHoldStatus: types.ObjectLockLegalHoldStatusOn,
		ObjectLockMode: types.ObjectLockModeGovernance, ObjectLockRetainUntilDate: aws.Time(time.Now().Add(time.Hour)),
	})
	if err != nil {
		t.Fatal(err)
	}
	bare := clientAt(url, "us-east-1", func(o *awss3.Options) { o.RequestChecksumCalculation = aws.RequestChecksumCalculationWhenRequired })
	if _, err := bare.UploadPart(t.Context(), &awss3.UploadPartInput{
		Bucket: aws.String("vault"), Key: aws.String("k"), UploadId: out.UploadId, PartNumber: aws.Int32(1), Body: strings.NewReader("x"),
	}); sdkerr.AWSCode(err) != "InvalidRequest" || !strings.Contains(err.Error(), "Put Part requests") {
		t.Fatalf("a part without a checksum = %v, want InvalidRequest for Put Part requests", err)
	}
	uploadPart(t, c, "vault", "k", aws.ToString(out.UploadId), 1, []byte("x"))
}
