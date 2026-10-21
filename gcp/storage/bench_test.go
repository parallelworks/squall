package storage_test

import (
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/parallelworks/squall/gcp/storage"
)

func BenchmarkUploadListDelete(b *testing.B) {
	s := storage.New()
	if _, err := s.PutBucket(storage.Bucket{Name: "bench", Project: "p"}); err != nil {
		b.Fatal(err)
	}
	for i := range 1000 {
		if _, err := s.PutObject("bench", "key-"+strconv.Itoa(i), []byte("data")); err != nil {
			b.Fatal(err)
		}
	}
	do := func(method, target, body string) {
		req := httptest.NewRequestWithContext(b.Context(), method, target, strings.NewReader(body))
		rec := httptest.NewRecorder()
		s.ServeHTTP(rec, req)
		if rec.Code >= http.StatusBadRequest {
			b.Fatalf("%s %s = %d %s", method, target, rec.Code, rec.Body)
		}
	}
	for b.Loop() {
		do(http.MethodPost, "/upload/storage/v1/b/bench/o?uploadType=media&name=new", "data")
		do(http.MethodGet, "/storage/v1/b/bench/o?maxResults=100&prefix=key-1", "")
		do(http.MethodDelete, "/storage/v1/b/bench/o/new", "")
	}
}
