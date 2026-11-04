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

func BenchmarkListGroupResources(b *testing.B) {
	s := arm.New()
	serve(b, s, http.MethodPut, groupPath, `{"location":"eastus"}`)
	for i := range 200 {
		serve(b, s, http.MethodPut, fmt.Sprintf("%s/providers/Microsoft.Network/networkSecurityGroups/nsg-%d", groupPath, i),
			fmt.Sprintf(`{"location":"eastus","tags":{"session":"%d"}}`, i%10))
	}
	for _, bc := range [][2]string{{"all", ""}, {"filtered", "&%24filter=tagName+eq+%27session%27+and+tagValue+eq+%273%27"}} {
		b.Run(bc[0], func(b *testing.B) {
			req := httptest.NewRequestWithContext(b.Context(), http.MethodGet, groupPath+"/resources?api-version=2025-04-01"+bc[1], nil)
			req.Header.Set("Authorization", "Bearer token")
			for b.Loop() {
				rec := httptest.NewRecorder()
				s.ServeHTTP(rec, req)
				if rec.Code != http.StatusOK {
					b.Fatal(rec.Body)
				}
			}
		})
	}
}

func blobServe(b *testing.B, h http.Handler, method, target, body string) *httptest.ResponseRecorder {
	b.Helper()
	req := httptest.NewRequestWithContext(b.Context(), method, target, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer token")
	req.Header.Set("x-ms-blob-type", "BlockBlob")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func blobStorage(b *testing.B, props string) (*arm.Server, http.Handler) {
	b.Helper()
	s := arm.New()
	serve(b, s, http.MethodPut, groupPath, `{"location":"eastus"}`)
	account := groupPath + "/providers/Microsoft.Storage/storageAccounts/acct"
	serve(b, s, http.MethodPut, account, `{"location":"eastus","sku":{"name":"Standard_LRS"},"kind":"StorageV2"}`)
	serve(b, s, http.MethodPut, account+"/blobServices/default", `{"properties":`+props+`}`)
	serve(b, s, http.MethodPut, account+"/blobServices/default/containers/data", `{}`)
	return s, s.Blob()
}

func BenchmarkPutListDeleteBlob(b *testing.B) {
	_, h := blobStorage(b, `{"isVersioningEnabled":true,"deleteRetentionPolicy":{"enabled":true,"days":7}}`)
	i := 0
	for b.Loop() {
		name := fmt.Sprintf("/acct/data/blob-%d", i)
		i++
		for _, call := range [][2]string{
			{http.MethodPut, name},
			{http.MethodGet, "/acct/data?restype=container&comp=list&include=versions,deleted&maxresults=10&prefix=" + name[len("/acct/data/"):]},
			{http.MethodDelete, name},
		} {
			if rec := blobServe(b, h, call[0], call[1], "data"); rec.Code >= 300 {
				b.Fatalf("%s %s = %d %s", call[0], call[1], rec.Code, rec.Body)
			}
		}
	}
}

func BenchmarkListBlobs(b *testing.B) {
	_, h := blobStorage(b, `{"isVersioningEnabled":true}`)
	for i := range 1000 {
		blobServe(b, h, http.MethodPut, fmt.Sprintf("/acct/data/dir-%d/blob-%d", i%10, i), "data")
	}
	for _, bc := range [][2]string{{"page", "&maxresults=100"}, {"versions", "&include=versions,deleted,snapshots,metadata"}, {"delimiter", "&delimiter=/"}} {
		b.Run(bc[0], func(b *testing.B) {
			for b.Loop() {
				if rec := blobServe(b, h, http.MethodGet, "/acct/data?restype=container&comp=list"+bc[1], ""); rec.Code != http.StatusOK {
					b.Fatal(rec.Body)
				}
			}
		})
	}
}
