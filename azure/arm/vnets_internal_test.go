package arm

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func serve(t *testing.T, s *Server, method, path, body string) map[string]any {
	t.Helper()
	req := httptest.NewRequestWithContext(t.Context(), method, path+"?api-version=2024-05-01", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer token")
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil || rec.Code >= http.StatusBadRequest {
		t.Fatalf("%s %s = %d %s", method, path, rec.Code, rec.Body)
	}
	return out
}

func TestVNetRoundTripLeavesSubnetsUnmodified(t *testing.T) {
	created := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	now := created
	s := New(WithClock(func() time.Time { return now }))
	const group = "/subscriptions/s/resourceGroups/rg"
	const vnet = group + "/providers/Microsoft.Network/virtualNetworks/vnet"
	serve(t, s, http.MethodPut, group, `{"location":"eastus"}`)
	serve(t, s, http.MethodPut, vnet, `{"location":"eastus","properties":{"addressSpace":{"addressPrefixes":["10.1.0.0/16"]},"subnets":[
		{"name":"plain","properties":{"addressPrefix":"10.1.0.0/24"}},
		{"name":"delegated","properties":{"addressPrefix":"10.1.1.0/24",
			"delegations":[{"name":"netapp","properties":{"serviceName":"Microsoft.Netapp/volumes"}}],
			"serviceEndpoints":[{"service":"Microsoft.Storage"}]}}]}}`)
	modified := func(name string) time.Time {
		t.Helper()
		s.mu.Lock()
		defer s.mu.Unlock()
		return s.resources[key(vnet+"/subnets/"+name)].modified
	}
	for range 2 {
		now = now.Add(time.Hour)
		serve(t, s, http.MethodPut, vnet, string(encode(serve(t, s, http.MethodGet, vnet, ""))))
		for _, name := range []string{"plain", "delegated"} {
			if got := modified(name); !got.Equal(created) {
				t.Fatalf("subnet %s after a VNet PUT of its own GET was modified at %v, want %v", name, got, created)
			}
		}
	}
	got := serve(t, s, http.MethodGet, vnet, "")
	subnets, _ := got["properties"].(map[string]any)["subnets"].([]any)
	for _, sn := range subnets {
		if sn := sn.(map[string]any); sn["name"] == "plain" {
			sn["properties"].(map[string]any)["privateEndpointNetworkPolicies"] = "Enabled"
		}
	}
	serve(t, s, http.MethodPut, vnet, string(encode(got)))
	if !modified("plain").Equal(now) || !modified("delegated").Equal(created) {
		t.Fatalf("after changing plain, plain was modified at %v and delegated at %v", modified("plain"), modified("delegated"))
	}
}
