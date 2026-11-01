package servicenetworking_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/parallelworks/squall/gcp/servicenetworking"
)

const connections = "/v1/services/servicenetworking.googleapis.com/connections"

func do(t *testing.T, s *servicenetworking.Server, method, path, body string) (int, map[string]any) {
	t.Helper()
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), method, path, strings.NewReader(body)))
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("%s %s: %v in %q", method, path, err, rec.Body)
	}
	return rec.Code, out
}

func TestRequestsTheAPIRejects(t *testing.T) {
	s := servicenetworking.New()
	for _, tc := range []struct {
		name, method, path, body string
		want                     int
	}{
		{"an unknown parameter", http.MethodGet, connections + "?network=projects/p/global/networks/n&pageSize=1", "", http.StatusBadRequest},
		{"another service", http.MethodGet, "/v1/services/other.googleapis.com/connections?network=projects/p/global/networks/n", "", http.StatusNotFound},
		{"another connection", http.MethodPatch, connections + "/other", `{}`, http.StatusNotFound},
		{"an unknown field", http.MethodPost, connections, `{"network":"projects/p/global/networks/n","reservedPeeringRanges":["r"],"x":1}`, http.StatusBadRequest},
		{"no ranges", http.MethodPost, connections, `{"network":"projects/p/global/networks/n"}`, http.StatusBadRequest},
		{"a repeated range", http.MethodPost, connections, `{"network":"projects/p/global/networks/n","reservedPeeringRanges":["r","r"]}`, http.StatusBadRequest},
		{"a bad update mask", http.MethodPatch, connections + "/-?updateMask=network", `{"network":"projects/p/global/networks/n","reservedPeeringRanges":["r"]}`, http.StatusBadRequest},
		{"a patch of no connection", http.MethodPatch, connections + "/-", `{"network":"projects/p/global/networks/n","reservedPeeringRanges":["r"]}`, http.StatusNotFound},
		{"an unknown operation", http.MethodGet, "/v1/operations/nope", "", http.StatusNotFound},
	} {
		if code, body := do(t, s, tc.method, tc.path, tc.body); code != tc.want {
			t.Errorf("%s = %d %v, want %d", tc.name, code, body, tc.want)
		}
	}
}

func TestOperationsCanBeRead(t *testing.T) {
	s := servicenetworking.New()
	code, op := do(t, s, http.MethodPost, connections, `{"network":"projects/p/global/networks/n","reservedPeeringRanges":["r"]}`)
	if code != http.StatusOK || op["done"] != true {
		t.Fatalf("create = %d %v", code, op)
	}
	code, got := do(t, s, http.MethodGet, "/v1/"+op["name"].(string), "")
	if code != http.StatusOK || got["response"].(map[string]any)["network"] != "projects/p/global/networks/n" {
		t.Fatalf("get = %d %v", code, got)
	}
	code, again := do(t, s, http.MethodPost, connections, `{"network":"projects/p/global/networks/n","reservedPeeringRanges":["r"]}`)
	if code != http.StatusOK || again["error"].(map[string]any)["code"] != float64(9) {
		t.Fatalf("a second create = %d %v, want an operation that ends in FAILED_PRECONDITION", code, again)
	}
}
