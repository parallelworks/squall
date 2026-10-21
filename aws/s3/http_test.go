package s3_test

import (
	"bytes"
	"encoding/xml"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

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

func FuzzServeHTTP(f *testing.F) {
	for _, seed := range []struct{ method, target, header, body string }{
		{http.MethodPut, "/fuzz/k", "Content-Encoding: aws-chunked", "5;chunk-signature=0\r\nhello\r\n0\r\n\r\n"},
		{http.MethodGet, "/fuzz?versions&key-marker=k&version-id-marker=null&max-keys=1", "", ""},
		{http.MethodPost, "/fuzz?delete", "", "<Delete><Object><Key>k</Key><VersionId>null</VersionId></Object></Delete>"},
		{http.MethodPut, "/fuzz?tagging", "X-Amz-Content-Sha256: STREAMING-UNSIGNED-PAYLOAD-TRAILER", "0\r\n\r\n"},
		{http.MethodGet, "/?max-buckets=1&continuation-token=YQ&bucket-region=us-east-1", "", ""},
		{http.MethodPut, "/other", "", "<CreateBucketConfiguration><LocationConstraint>eu-west-1</LocationConstraint></CreateBucketConfiguration>"},
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
