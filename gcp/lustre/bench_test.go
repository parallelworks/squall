package lustre_test

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/parallelworks/squall/gcp/lustre"
)

func BenchmarkCreateGetDelete(b *testing.B) {
	s := lustre.New()
	do := func(method, path, body string) {
		rec := httptest.NewRecorder()
		s.ServeHTTP(rec, httptest.NewRequestWithContext(b.Context(), method, path, strings.NewReader(body)))
		if rec.Code != http.StatusOK {
			b.Fatalf("%s %s = %d %s", method, path, rec.Code, rec.Body)
		}
	}
	const create = `{"filesystem":"lfs","capacityGib":"9000","perUnitStorageThroughput":"1000","network":"projects/p/global/networks/n"}`
	i := 0
	for b.Loop() {
		id := fmt.Sprintf("fs-%d", i)
		i++
		do(http.MethodPost, instances+"?instanceId="+id, create)
		do(http.MethodGet, instances+"/"+id, "")
		do(http.MethodDelete, instances+"/"+id, "")
	}
}

func BenchmarkListFiltered(b *testing.B) {
	s := lustre.New()
	for i := range 1000 {
		in := lustre.Instance{
			Filesystem: "lfs", CapacityGib: 9000, PerUnitStorageThroughput: 1000, Network: "projects/p/global/networks/n",
			Labels: map[string]string{"session-id": strconv.Itoa(i % 10)},
		}
		if _, err := s.PutInstance("p", "us-central1-a", fmt.Sprintf("fs-%d", i), in); err != nil {
			b.Fatal(err)
		}
	}
	req := httptest.NewRequestWithContext(b.Context(), http.MethodGet, instances+"?filter=labels.session-id%3D3", nil)
	for b.Loop() {
		rec := httptest.NewRecorder()
		s.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			b.Fatal(rec.Body)
		}
	}
}
