package s3_test

import (
	"bytes"
	"cmp"
	"encoding/xml"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/parallelworks/squall/aws/s3"
)

const authorization = "AWS4-HMAC-SHA256 Credential=AKID/20260101/us-east-1/s3/aws4_request, SignedHeaders=host, Signature=0"

// do sends one request straight to s and returns the status and body.
func do(t *testing.T, s *s3.Server, method, target, auth, body string) (int, http.Header, string) {
	t.Helper()
	req := httptest.NewRequestWithContext(t.Context(), method, target, strings.NewReader(body))
	if auth != "" {
		req.Header.Set("Authorization", auth)
	}
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	return rec.Code, rec.Header(), rec.Body.String()
}

func TestResponsesUseTheS3Shape(t *testing.T) {
	s := s3.New()
	if code, h, body := do(t, s, http.MethodPut, "/shape", authorization, ""); code != http.StatusOK || h.Get("Location") != "/shape" ||
		h.Get("x-amz-request-id") == "" || body != "" {
		t.Fatalf("create = %d %v %q", code, h, body)
	}
	code, _, body := do(t, s, http.MethodGet, "/", authorization, "")
	for _, want := range []string{
		`<ListAllMyBucketsResult xmlns="http://s3.amazonaws.com/doc/2006-03-01/">`,
		"<Owner><ID>", "<Buckets><Bucket><Name>shape</Name><CreationDate>",
	} {
		if code != http.StatusOK || !strings.Contains(body, want) {
			t.Fatalf("list = %d %s, want %s", code, body, want)
		}
	}
	code, _, body = do(t, s, http.MethodGet, "/missing?tagging", authorization, "")
	if code != http.StatusNotFound || !strings.Contains(body,
		"<Error><Code>NoSuchBucket</Code><Message>The specified bucket does not exist</Message><BucketName>missing</BucketName><RequestId>") {
		t.Fatalf("missing = %d %s", code, body)
	}
	code, h, body := do(t, s, http.MethodHead, "/missing", authorization, "")
	if code != http.StatusNotFound || body != "" || h.Get("Content-Type") != "" {
		t.Fatalf("head = %d %v %q", code, h, body)
	}
}

func TestListObjectsV2(t *testing.T) {
	s := s3.New()
	if _, err := s.PutBucket(s3.Bucket{Name: "list", Region: "us-east-1", Versioning: "Enabled"}); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"b", "a b/1", "a b/2", "c/d/e", "é", "Z", "gone"} {
		if _, err := s.PutObject("list", key, []byte(key)); err != nil {
			t.Fatal(err)
		}
	}
	if code, _, body := do(t, s, http.MethodDelete, "/list/gone", authorization, ""); code != http.StatusNoContent {
		t.Fatalf("delete = %d %s", code, body)
	}
	code, _, body := do(t, s, http.MethodGet, "/list?list-type=2&delimiter=/&encoding-type=url&fetch-owner=true", authorization, "")
	for _, want := range []string{
		`<ListBucketResult xmlns="http://s3.amazonaws.com/doc/2006-03-01/"><Name>list</Name><Prefix></Prefix><Delimiter>/</Delimiter><MaxKeys>1000</MaxKeys><EncodingType>url</EncodingType><KeyCount>5</KeyCount><IsTruncated>false</IsTruncated>`,
		`<Contents><Key>Z</Key><LastModified>`, `<ETag>&#34;21c2e59531c8710156d34a3c30ac81d5&#34;</ETag><Size>1</Size><StorageClass>STANDARD</StorageClass><Owner><ID>`,
		`<Key>%C3%A9</Key>`, `<CommonPrefixes><Prefix>a+b/</Prefix></CommonPrefixes><CommonPrefixes><Prefix>c/</Prefix></CommonPrefixes>`,
	} {
		if code != http.StatusOK || !strings.Contains(body, want) {
			t.Fatalf("list = %d %s, want %s", code, body, want)
		}
	}
	if strings.Contains(body, "gone") || strings.Index(body, "<Key>Z</Key>") > strings.Index(body, "<Key>b</Key>") {
		t.Fatalf("list = %s, want binary order without deleted keys", body)
	}
	code, _, body = do(t, s, http.MethodGet, "/list?list-type=2&max-keys=1&start-after=a", authorization, "")
	if code != http.StatusOK || !strings.Contains(body, "<StartAfter>a</StartAfter><Contents><Key>a b/1</Key>") ||
		!strings.Contains(body, "<IsTruncated>true</IsTruncated><NextContinuationToken>") || strings.Contains(body, "<Owner>") {
		t.Fatalf("a page = %d %s", code, body)
	}
	for _, tc := range []struct {
		name, query string
		status      int
		code        string
	}{
		{"list-type 1", "list-type=1", http.StatusBadRequest, "InvalidArgument"},
		{"negative max-keys", "list-type=2&max-keys=-1", http.StatusBadRequest, "InvalidArgument"},
		{"non-numeric max-keys", "list-type=2&max-keys=ten", http.StatusBadRequest, "InvalidArgument"},
		{"a bad continuation token", "list-type=2&continuation-token=bm9wZQ", http.StatusBadRequest, "InvalidArgument"},
		{"an empty continuation token", "list-type=2&continuation-token=", http.StatusBadRequest, "InvalidArgument"},
		{"another encoding", "list-type=2&encoding-type=base64", http.StatusBadRequest, "InvalidArgument"},
		{"an unknown parameter", "list-type=2&marker=a", http.StatusBadRequest, "InvalidArgument"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if code, _, body := do(t, s, http.MethodGet, "/list?"+tc.query, authorization, ""); code != tc.status || !strings.Contains(body, "<Code>"+tc.code+"</Code>") {
				t.Fatalf("= %d %s, want %d %s", code, body, tc.status, tc.code)
			}
		})
	}
	if code, _, body := do(t, s, http.MethodGet, "/missing?list-type=2", authorization, ""); code != http.StatusNotFound || !strings.Contains(body, "<Code>NoSuchBucket</Code>") {
		t.Fatalf("missing = %d %s", code, body)
	}
}

func TestRequestsTheServerCannotRead(t *testing.T) {
	s := s3.New()
	for _, tc := range []struct {
		name, method, target, auth, body string
		status                           int
		code                             string
	}{
		{"no credentials", http.MethodGet, "/", "", "", http.StatusForbidden, "AccessDenied"},
		{"a malformed credential scope", http.MethodGet, "/", "AWS4-HMAC-SHA256 Credential=AKID/us-east-1", "", http.StatusBadRequest, "AuthorizationHeaderMalformed"},
		{"another signing algorithm", http.MethodGet, "/", "AWS AKID:sig", "", http.StatusBadRequest, "InvalidArgument"},
		{"an unimplemented operation", http.MethodGet, "/b?lifecycle", authorization, "", http.StatusNotFound, "NotImplemented"},
		{"an unknown parameter", http.MethodGet, "/?bogus=1", authorization, "", http.StatusBadRequest, "InvalidArgument"},
		{"malformed XML", http.MethodPut, "/b?tagging", authorization, "<Tagging>", http.StatusBadRequest, "MalformedXML"},
		{"an empty delete", http.MethodPost, "/b?delete", authorization, "<Delete></Delete>", http.StatusBadRequest, "MalformedXML"},
		{"a delete of an empty key", http.MethodPost, "/b?delete", authorization, "<Delete><Object><Key></Key></Object></Delete>", http.StatusBadRequest, "MalformedXML"},
		{"a bad versioning status", http.MethodPut, "/b?versioning", authorization, "<VersioningConfiguration><Status>On</Status></VersioningConfiguration>", http.StatusBadRequest, "MalformedXML"},
		{"a bad continuation token", http.MethodGet, "/?continuation-token=%25", authorization, "", http.StatusBadRequest, "InvalidArgument"},
		{"a malformed chunked body", http.MethodPut, "/b?tagging", authorization, "", http.StatusBadRequest, "IncompleteBody"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequestWithContext(t.Context(), tc.method, tc.target, strings.NewReader(tc.body))
			if tc.auth != "" {
				req.Header.Set("Authorization", tc.auth)
			}
			if tc.code == "IncompleteBody" {
				req.Header.Set("Content-Encoding", "aws-chunked")
				req.Body = http.NoBody
				req.Header.Set("X-Amz-Content-Sha256", "STREAMING-UNSIGNED-PAYLOAD-TRAILER")
			}
			rec := httptest.NewRecorder()
			s.ServeHTTP(rec, req)
			if rec.Code != tc.status || !strings.Contains(rec.Body.String(), "<Code>"+tc.code+"</Code>") {
				t.Fatalf("= %d %s, want %d %s", rec.Code, rec.Body, tc.status, tc.code)
			}
		})
	}
}

func TestChunkedBodies(t *testing.T) {
	s := s3.New()
	if _, err := s.PutBucket(s3.Bucket{Name: "chunks", Region: "us-east-1"}); err != nil {
		t.Fatal(err)
	}
	body := "5;chunk-signature=0\r\nhello\r\n6\r\n world\r\n0\r\nx-amz-checksum-crc32:AAAAAA==\r\n\r\n"
	req := httptest.NewRequestWithContext(t.Context(), http.MethodPut, "/chunks/greeting?x-id=PutObject", strings.NewReader(body))
	req.Header.Set("Authorization", authorization)
	req.Header.Set("Content-Encoding", "aws-chunked")
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || rec.Header().Get("ETag") != `"5eb63bbbe01eeed093cb22bb8f5acdc3"` {
		t.Fatalf("put = %d %v %s", rec.Code, rec.Header(), rec.Body)
	}
}

func TestPresignedCredentialsNameTheRegion(t *testing.T) {
	s := s3.New()
	target := "/presigned?X-Amz-Credential=AKID%2F20260101%2Fap-south-1%2Fs3%2Faws4_request&X-Amz-Signature=0"
	code, _, body := do(t, s, http.MethodPut, target, "", "<CreateBucketConfiguration><LocationConstraint>ap-south-1</LocationConstraint></CreateBucketConfiguration>")
	if code != http.StatusOK {
		t.Fatalf("a presigned create = %d %s", code, body)
	}
	if b, ok := s.LookupBucket("presigned"); !ok || b.Region != "ap-south-1" {
		t.Fatalf("bucket = %+v, %v", b, ok)
	}
}

func TestHostileBodies(t *testing.T) {
	s := s3.New()
	if _, err := s.PutBucket(s3.Bucket{Name: "hostile", Region: "us-east-1"}); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, target, body string
		chunked            bool
		code               string
	}{
		{"a chunk size near the integer limit", "/hostile/k", "7fffffffffffffff\r\nab", true, "IncompleteBody"},
		{"a chunk size with a 0x prefix", "/hostile/k", "0x5\r\nhello\r\n0\r\n\r\n", true, "IncompleteBody"},
		{"a chunk longer than the body", "/hostile/k", "10\r\nhello\r\n0\r\n\r\n", true, "IncompleteBody"},
		{"a tag set over 1 MiB", "/hostile?tagging", strings.Repeat(" ", 2<<20), false, "MaxMessageLengthExceeded"},
		{"an object over 16 MiB", "/hostile/k", strings.Repeat("x", 17<<20), false, "EntityTooLarge"},
		{"a key over 1024 bytes", "/hostile/" + strings.Repeat("k", 1025), "x", false, "KeyTooLongError"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequestWithContext(t.Context(), http.MethodPut, tc.target, strings.NewReader(tc.body))
			req.Header.Set("Authorization", authorization)
			if tc.chunked {
				req.Header.Set("Content-Encoding", "aws-chunked")
			}
			rec := httptest.NewRecorder()
			s.ServeHTTP(rec, req)
			if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "<Code>"+tc.code+"</Code>") {
				t.Fatalf("= %d %s, want 400 %s", rec.Code, rec.Body, tc.code)
			}
		})
	}
	if _, err := s.PutBucket(s3.Bucket{Name: "hostile", Region: "us-east-1", Versioning: "Enabled"}); err != nil {
		t.Fatal(err)
	}
	long := strings.Repeat("k", 1025)
	code, _, body := do(t, s, http.MethodPost, "/hostile?delete", authorization, "<Delete><Object><Key>"+long+"</Key></Object></Delete>")
	if code != http.StatusOK || !strings.Contains(body, "<Code>KeyTooLongError</Code>") || strings.Contains(body, "<Deleted>") {
		t.Fatalf("a delete of a long key = %d %s", code, body)
	}
	for _, tag := range []string{"a\tb", "a\nb"} {
		code, _, body := do(t, s, http.MethodPut, "/hostile?tagging", authorization, "<Tagging><TagSet><Tag><Key>"+tag+"</Key><Value>v</Value></Tag></TagSet></Tagging>")
		if code != http.StatusBadRequest || !strings.Contains(body, "<Code>InvalidTag</Code>") {
			t.Fatalf("a tag key %q = %d %s", tag, code, body)
		}
	}
	if got := s.ObjectVersions("hostile"); len(got) != 0 {
		t.Fatalf("versions = %+v", got)
	}
}

func TestMultipartAndLockShapes(t *testing.T) {
	s := s3.New()
	if _, err := s.PutBucket(s3.Bucket{Name: "vault", Region: "us-east-1", Versioning: "Enabled", ObjectLock: true}); err != nil {
		t.Fatal(err)
	}
	code, _, body := do(t, s, http.MethodPost, "/vault/a%20b?uploads", authorization, "")
	if code != http.StatusOK || !strings.Contains(body,
		`<InitiateMultipartUploadResult xmlns="http://s3.amazonaws.com/doc/2006-03-01/"><Bucket>vault</Bucket><Key>a b</Key><UploadId>`) {
		t.Fatalf("create upload = %d %s", code, body)
	}
	if code, _, body := do(t, s, http.MethodGet, "/vault?uploads&max-uploads=5000", authorization, ""); code != http.StatusOK || !strings.Contains(body, "<MaxUploads>1000</MaxUploads>") {
		t.Fatalf("max-uploads 5000 = %d %s, want a page of 1000", code, body)
	}
	code, _, body = do(t, s, http.MethodGet, "/vault?uploads&encoding-type=url", authorization, "")
	if code != http.StatusOK || !strings.Contains(body, `<KeyMarker></KeyMarker><UploadIdMarker></UploadIdMarker><NextKeyMarker>a+b</NextKeyMarker><NextUploadIdMarker>`) || !strings.Contains(body, `</NextUploadIdMarker><Prefix></Prefix><MaxUploads>1000</MaxUploads>`+
		`<EncodingType>url</EncodingType><IsTruncated>false</IsTruncated><Upload><Key>a+b</Key>`) ||
		!strings.Contains(body, `<StorageClass>STANDARD</StorageClass><Initiated>`) {
		t.Fatalf("list uploads = %d %s", code, body)
	}
	lockConfig := "<ObjectLockConfiguration><ObjectLockEnabled>Enabled</ObjectLockEnabled></ObjectLockConfiguration>"
	for _, tc := range []struct {
		name, method, target string
		header               map[string]string
		body                 string
		status               int
		code                 string
	}{
		{"a retention without a checksum", http.MethodPut, "/vault/k", map[string]string{
			"x-amz-object-lock-mode": "GOVERNANCE", "x-amz-object-lock-retain-until-date": "2099-01-01T00:00:00Z",
		}, "", http.StatusBadRequest, "InvalidRequest"},
		{"a mode without a date", http.MethodPut, "/vault/k", map[string]string{"x-amz-object-lock-mode": "GOVERNANCE"}, "", http.StatusBadRequest, "InvalidArgument"},
		{"an unknown mode", http.MethodPut, "/vault/k", map[string]string{
			"x-amz-object-lock-mode": "STRICT", "x-amz-object-lock-retain-until-date": "2099-01-01T00:00:00Z",
		}, "", http.StatusBadRequest, "InvalidArgument"},
		{"a date in the past", http.MethodPut, "/vault/k", map[string]string{
			"x-amz-object-lock-mode": "GOVERNANCE", "x-amz-object-lock-retain-until-date": "2000-01-01T00:00:00Z", "Content-MD5": "x",
		}, "", http.StatusBadRequest, "InvalidArgument"},
		{"a bad legal hold", http.MethodPut, "/vault/k", map[string]string{"x-amz-object-lock-legal-hold": "on"}, "", http.StatusBadRequest, "InvalidArgument"},
		{"an event hold", http.MethodPut, "/vault/k", map[string]string{"x-amz-object-lock-event-hold": "ON"}, "", http.StatusBadRequest, "InvalidArgument"},
		{"a part without an upload", http.MethodPut, "/vault/k?partNumber=1", nil, "", http.StatusBadRequest, "InvalidArgument"},
		{"a part of an unknown upload", http.MethodPut, "/vault/k?partNumber=1&uploadId=nope", nil, "", http.StatusNotFound, "NoSuchUpload"},
		{"part zero", http.MethodPut, "/vault/k?partNumber=0&uploadId=nope", nil, "", http.StatusBadRequest, "InvalidArgument"},
		{"a negative max-uploads", http.MethodGet, "/vault?uploads&max-uploads=-1", nil, "", http.StatusBadRequest, "InvalidArgument"},
		{"max-uploads 0", http.MethodGet, "/vault?uploads&max-uploads=0", nil, "", http.StatusBadRequest, "InvalidArgument"},
		{"max-parts 0", http.MethodGet, "/vault/k?uploadId=x&max-parts=0", nil, "", http.StatusBadRequest, "InvalidArgument"},
		{"a retention with only a checksum type", http.MethodPut, "/vault/k", map[string]string{
			"x-amz-object-lock-mode": "GOVERNANCE", "x-amz-object-lock-retain-until-date": "2099-01-01T00:00:00Z",
			"x-amz-checksum-type": "FULL_OBJECT", "x-amz-checksum-algorithm": "CRC32", "x-amz-sdk-checksum-algorithm": "CRC32",
		}, "", http.StatusBadRequest, "InvalidRequest"},
		{
			"a versioning configuration under another root", http.MethodPut, "/vault?versioning", nil,
			"<Status><Status>Enabled</Status></Status>", http.StatusBadRequest, "MalformedXML",
		},
		{
			"a request payment configuration under another root", http.MethodPut, "/vault?requestPayment",
			map[string]string{"Content-MD5": "x"},
			"<Payment><Payer>Requester</Payer></Payment>", http.StatusBadRequest, "MalformedXML",
		},
		{
			"a request payment configuration without a checksum", http.MethodPut, "/vault?requestPayment", nil,
			"<RequestPaymentConfiguration><Payer>Requester</Payer></RequestPaymentConfiguration>", http.StatusBadRequest, "InvalidRequest",
		},
		{"retention of a missing key", http.MethodGet, "/vault/missing?retention", nil, "", http.StatusNotFound, "NoSuchKey"},
		{"a lock configuration without a checksum", http.MethodPut, "/vault?object-lock", nil, lockConfig, http.StatusBadRequest, "InvalidRequest"},
		{
			"a lock configuration under another root", http.MethodPut, "/vault?object-lock",
			map[string]string{"Content-MD5": "x"},
			"<Nope><ObjectLockEnabled>Enabled</ObjectLockEnabled></Nope>", http.StatusBadRequest, "MalformedXML",
		},
		{
			"a retention without a checksum", http.MethodPut, "/vault/missing?retention", nil,
			"<Retention><Mode>GOVERNANCE</Mode><RetainUntilDate>2099-01-01T00:00:00Z</RetainUntilDate></Retention>", http.StatusBadRequest, "InvalidRequest",
		},
		{
			"a retention under another root", http.MethodPut, "/vault/missing?retention",
			map[string]string{"Content-MD5": "x"},
			"<LegalHold><Status>ON</Status></LegalHold>", http.StatusBadRequest, "MalformedXML",
		},
		{"a legal hold without a checksum", http.MethodPut, "/vault/missing?legal-hold", nil, "<LegalHold><Status>ON</Status></LegalHold>", http.StatusBadRequest, "InvalidRequest"},
		{
			"a legal hold under another root", http.MethodPut, "/vault/missing?legal-hold",
			map[string]string{"Content-MD5": "x"},
			"<Retention><Status>ON</Status></Retention>", http.StatusBadRequest, "MalformedXML",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequestWithContext(t.Context(), tc.method, tc.target, strings.NewReader(cmp.Or(tc.body, "x")))
			req.Header.Set("Authorization", authorization)
			for k, v := range tc.header {
				req.Header.Set(k, v)
			}
			rec := httptest.NewRecorder()
			s.ServeHTTP(rec, req)
			if rec.Code != tc.status || !strings.Contains(rec.Body.String(), "<Code>"+tc.code+"</Code>") {
				t.Fatalf("= %d %s, want %d %s", rec.Code, rec.Body, tc.status, tc.code)
			}
		})
	}
	req := httptest.NewRequestWithContext(t.Context(), http.MethodPut, "/vault?object-lock", strings.NewReader(
		"<ObjectLockConfiguration><ObjectLockEnabled>Enabled</ObjectLockEnabled><Rule><DefaultRetention><Mode>GOVERNANCE</Mode><Years>1</Years></DefaultRetention></Rule></ObjectLockConfiguration>"))
	req.Header.Set("Authorization", authorization)
	req.Header.Set("Content-MD5", "x")
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("put lock configuration = %d %s", rec.Code, rec.Body)
	}
	code, _, body = do(t, s, http.MethodGet, "/vault?object-lock", authorization, "")
	if code != http.StatusOK || !strings.Contains(body,
		`<ObjectLockConfiguration xmlns="http://s3.amazonaws.com/doc/2006-03-01/"><ObjectLockEnabled>Enabled</ObjectLockEnabled><Rule><DefaultRetention><Mode>GOVERNANCE</Mode><Years>1</Years></DefaultRetention></Rule></ObjectLockConfiguration>`) {
		t.Fatalf("get lock configuration = %d %s", code, body)
	}
	if code, _, body = do(t, s, http.MethodPut, "/vault/k", authorization, "x"); code != http.StatusBadRequest || !strings.Contains(body, "<Code>InvalidRequest</Code>") {
		t.Fatalf("a put under default retention without a checksum = %d %s", code, body)
	}
}

func TestListMultipartUploadsNextMarkers(t *testing.T) {
	s := s3.New()
	if _, err := s.PutBucket(s3.Bucket{Name: "markers", Region: "us-east-1"}); err != nil {
		t.Fatal(err)
	}
	var sample string
	for _, key := range []string{"a/1", "sample.jpg", "videos/1"} {
		up, err := s.CreateMultipartUpload("markers", key)
		if err != nil {
			t.Fatal(err)
		}
		if key == "sample.jpg" {
			sample = up.UploadID
		}
	}
	code, _, body := do(t, s, http.MethodGet, "/markers?uploads&delimiter=/", authorization, "")
	if want := "<NextKeyMarker>sample.jpg</NextKeyMarker><NextUploadIdMarker>" + sample + "</NextUploadIdMarker>"; code != http.StatusOK ||
		!strings.Contains(body, want) || !strings.Contains(body, "<IsTruncated>false</IsTruncated>") {
		t.Fatalf("an untruncated page = %d %s, want %s", code, body, want)
	}
	code, _, body = do(t, s, http.MethodGet, "/markers?uploads&delimiter=/&max-uploads=1", authorization, "")
	if want := "<NextKeyMarker>a/</NextKeyMarker><NextUploadIdMarker></NextUploadIdMarker>"; code != http.StatusOK ||
		!strings.Contains(body, want) || !strings.Contains(body, "<IsTruncated>true</IsTruncated>") {
		t.Fatalf("a page ending on a prefix = %d %s, want %s", code, body, want)
	}
	code, _, body = do(t, s, http.MethodGet, "/markers?uploads&delimiter=/&key-marker=a/1", authorization, "")
	if code != http.StatusOK || strings.Contains(body, "<Prefix>a/</Prefix>") || !strings.Contains(body, "<Prefix>videos/</Prefix>") {
		t.Fatalf("after a key inside a/ = %d %s, want videos/ and not a/", code, body)
	}
}

func TestBucketConfigurationsUseTheS3Namespace(t *testing.T) {
	s := s3.New()
	if _, err := s.PutBucket(s3.Bucket{Name: "namespaced", Region: "us-east-1", Versioning: "Enabled", Tags: map[string]string{"k": "v"}}); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ query, root string }{
		{"versioning", "VersioningConfiguration"},
		{"requestPayment", "RequestPaymentConfiguration"},
		{"tagging", "Tagging"},
	} {
		code, _, body := do(t, s, http.MethodGet, "/namespaced?"+tc.query, authorization, "")
		if want := "<" + tc.root + ` xmlns="http://s3.amazonaws.com/doc/2006-03-01/">`; code != http.StatusOK || !strings.Contains(body, want) {
			t.Errorf("%s = %d %s, want %s", tc.query, code, body, want)
		}
	}
}

func TestRetainUntilUnderAMillisecondAheadIsInThePast(t *testing.T) {
	s := s3.New(s3.WithClock(func() time.Time { return time.Date(2026, 1, 1, 0, 0, 0, 500_000, time.UTC) }))
	if _, err := s.PutBucket(s3.Bucket{Name: "vault", Region: "us-east-1", Versioning: "Enabled", ObjectLock: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.PutObject("vault", "k", []byte("k")); err != nil {
		t.Fatal(err)
	}
	const soon = "2026-01-01T00:00:00.0009Z"
	req := httptest.NewRequestWithContext(t.Context(), http.MethodPut, "/vault/k", strings.NewReader("k"))
	req.Header.Set("Authorization", authorization)
	req.Header.Set("Content-MD5", "x")
	req.Header.Set("x-amz-object-lock-mode", "COMPLIANCE")
	req.Header.Set("x-amz-object-lock-retain-until-date", soon)
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "<Code>InvalidArgument</Code>") {
		t.Fatalf("put = %d %s, want 400 InvalidArgument", rec.Code, rec.Body)
	}
	req = httptest.NewRequestWithContext(t.Context(), http.MethodPut, "/vault/k?retention", strings.NewReader(
		"<Retention><Mode>COMPLIANCE</Mode><RetainUntilDate>"+soon+"</RetainUntilDate></Retention>"))
	req.Header.Set("Authorization", authorization)
	req.Header.Set("Content-MD5", "x")
	rec = httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "<Code>InvalidArgument</Code>") {
		t.Fatalf("put retention = %d %s, want 400 InvalidArgument", rec.Code, rec.Body)
	}
}

func FuzzServeHTTP(f *testing.F) {
	for _, seed := range []struct{ method, target, header, body string }{
		{http.MethodPut, "/fuzz/k", "Content-Encoding: aws-chunked", "5;chunk-signature=0\r\nhello\r\n0\r\n\r\n"},
		{http.MethodGet, "/fuzz?versions&key-marker=k&version-id-marker=null&max-keys=1", "", ""},
		{http.MethodGet, "/fuzz?list-type=2&delimiter=/&max-keys=1&continuation-token=cGs&encoding-type=url", "", ""},
		{http.MethodPost, "/fuzz?delete", "", "<Delete><Object><Key>k</Key><VersionId>null</VersionId></Object></Delete>"},
		{http.MethodPut, "/fuzz?tagging", "X-Amz-Content-Sha256: STREAMING-UNSIGNED-PAYLOAD-TRAILER", "0\r\n\r\n"},
		{http.MethodGet, "/?max-buckets=1&continuation-token=YQ&bucket-region=us-east-1", "", ""},
		{http.MethodPut, "/other", "", "<CreateBucketConfiguration><LocationConstraint>eu-west-1</LocationConstraint></CreateBucketConfiguration>"},
		{http.MethodGet, "/fuzz?uploads&delimiter=/&key-marker=a/&upload-id-marker=0&max-uploads=1&encoding-type=url", "", ""},
		{http.MethodPost, "/fuzz/k?uploadId=0", "", "<CompleteMultipartUpload><Part><PartNumber>1</PartNumber><ETag>x</ETag></Part></CompleteMultipartUpload>"},
		{http.MethodPut, "/fuzz/k?retention", "", "<Retention><Mode>COMPLIANCE</Mode><RetainUntilDate>2099-01-01T00:00:00Z</RetainUntilDate></Retention>"},
		{http.MethodPut, "/fuzz?object-lock", "", "<ObjectLockConfiguration><ObjectLockEnabled>Enabled</ObjectLockEnabled></ObjectLockConfiguration>"},
	} {
		f.Add(seed.method, seed.target, seed.header, []byte(seed.body))
	}
	f.Fuzz(func(t *testing.T, method, target, header string, body []byte) {
		req, err := http.NewRequestWithContext(t.Context(), method, "http://s3.test"+target, bytes.NewReader(body))
		if err != nil {
			return
		}
		req.Header.Set("Authorization", authorization)
		if name, value, ok := strings.Cut(header, ":"); ok && validHeaderName(name) {
			req.Header.Set(name, strings.TrimSpace(value))
		}
		s := s3.New()
		if _, err := s.PutBucket(s3.Bucket{Name: "fuzz", Region: "us-east-1", Versioning: "Enabled"}); err != nil {
			t.Fatal(err)
		}
		if _, err := s.PutObject("fuzz", "k", []byte("v")); err != nil {
			t.Fatal(err)
		}
		rec := httptest.NewRecorder()
		s.ServeHTTP(rec, req)
		if rec.Code < 200 || rec.Code > 599 {
			t.Fatalf("%s %s = %d", method, target, rec.Code)
		}
		if rec.Body.Len() > 0 && !wellFormed(rec.Body.Bytes()) {
			t.Fatalf("%s %s = %d %q, not XML", method, target, rec.Code, rec.Body)
		}
	})
}

func validHeaderName(name string) bool {
	return name != "" && strings.Trim(name, "-0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz") == ""
}

func wellFormed(b []byte) bool {
	dec := xml.NewDecoder(bytes.NewReader(b))
	for {
		_, err := dec.Token()
		if errors.Is(err, io.EOF) {
			return true
		}
		if err != nil {
			return false
		}
	}
}
