package compute_test

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/parallelworks/squall/gcp/compute"
)

func BenchmarkInsertGetDelete(b *testing.B) {
	s := compute.New()
	const base = "/compute/v1/projects/p/regions/us-east1/addresses"
	do := func(method, path, body string) {
		rec := httptest.NewRecorder()
		s.ServeHTTP(rec, httptest.NewRequestWithContext(b.Context(), method, path, strings.NewReader(body)))
		if rec.Code != http.StatusOK {
			b.Fatalf("%s %s = %d %s", method, path, rec.Code, rec.Body)
		}
	}
	i := 0
	for b.Loop() {
		name := fmt.Sprintf("ip-%d", i)
		i++
		do(http.MethodPost, base, `{"name":"`+name+`"}`)
		do(http.MethodGet, base+"/"+name, "")
		do(http.MethodDelete, base+"/"+name, "")
	}
}

func BenchmarkListFiltered(b *testing.B) {
	s := compute.New()
	for i := range 1000 {
		labels := map[string]string{"session-id": strconv.Itoa(i % 10)}
		if _, err := s.PutAddress("p", "us-east1", compute.Address{Name: fmt.Sprintf("ip-%d", i), Labels: labels}); err != nil {
			b.Fatal(err)
		}
	}
	req := httptest.NewRequestWithContext(b.Context(), http.MethodGet, "/compute/v1/projects/p/regions/us-east1/addresses?filter=labels.session-id%3D3", nil)
	for b.Loop() {
		rec := httptest.NewRecorder()
		s.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			b.Fatal(rec.Body)
		}
	}
}
