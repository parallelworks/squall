package s3_test

import (
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/parallelworks/squall/aws/s3"
)

func BenchmarkPutListDeleteVersions(b *testing.B) {
	s := s3.New()
	if _, err := s.PutBucket(s3.Bucket{Name: "bench", Region: "us-east-1", Versioning: "Enabled"}); err != nil {
		b.Fatal(err)
	}
	for i := range 1000 {
		if _, err := s.PutObject("bench", "key-"+strconv.Itoa(i), []byte("data")); err != nil {
			b.Fatal(err)
		}
	}
	do := func(method, target, body string) {
		req := httptest.NewRequestWithContext(b.Context(), method, target, strings.NewReader(body))
		req.Header.Set("Authorization", authorization)
		rec := httptest.NewRecorder()
		s.ServeHTTP(rec, req)
		if rec.Code >= http.StatusBadRequest {
			b.Fatalf("%s %s = %d %s", method, target, rec.Code, rec.Body)
		}
	}
	for b.Loop() {
		do(http.MethodPut, "/bench/new", "data")
		do(http.MethodGet, "/bench?versions&max-keys=100&prefix=key-1", "")
		do(http.MethodPost, "/bench?delete", "<Delete><Object><Key>new</Key></Object></Delete>")
	}
}
