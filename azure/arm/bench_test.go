package arm_test

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/parallelworks/squall/azure/arm"
)

func serve(b *testing.B, s *arm.Server, method, path, body string) *httptest.ResponseRecorder {
	b.Helper()
	req := httptest.NewRequestWithContext(b.Context(), method, path+"?api-version=2024-01-01", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer token")
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	return rec
}

func BenchmarkPutGetDelete(b *testing.B) {
	s := arm.New()
	serve(b, s, http.MethodPut, groupPath, `{"location":"eastus"}`)
	i := 0
	for b.Loop() {
		path := fmt.Sprintf("%s/providers/Microsoft.Network/networkInterfaces/nic-%d", groupPath, i)
		i++
		for _, call := range [][2]string{
			{http.MethodPut, `{"location":"eastus","properties":{"ipConfigurations":[{"name":"ip","properties":{"subnet":{"id":"s"}}}]}}`},
			{http.MethodGet, ""},
			{http.MethodDelete, ""},
		} {
			if rec := serve(b, s, call[0], path, call[1]); rec.Code >= 300 {
				b.Fatalf("%s %s = %d %s", call[0], path, rec.Code, rec.Body)
			}
		}
	}
}

func BenchmarkListGroupsFiltered(b *testing.B) {
	s := arm.New()
	for i := range 1000 {
		serve(b, s, http.MethodPut, fmt.Sprintf("/subscriptions/s/resourceGroups/rg-%d", i),
			fmt.Sprintf(`{"location":"eastus","tags":{"session":"%d"}}`, i%10))
	}
	req := httptest.NewRequestWithContext(b.Context(), http.MethodGet,
		"/subscriptions/s/resourcegroups?api-version=2024-01-01&%24filter=tagName+eq+%27session%27+and+tagValue+eq+%273%27", nil)
	req.Header.Set("Authorization", "Bearer token")
	for b.Loop() {
		rec := httptest.NewRecorder()
		s.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			b.Fatal(rec.Body)
		}
	}
}
