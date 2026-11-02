package serviceusage_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/parallelworks/squall/gcp/serviceusage"
)

const bq = "/v1/projects/p/services/bigquery.googleapis.com"

func do(t *testing.T, s *serviceusage.Server, method, path, body string) (int, map[string]any) {
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
	s := serviceusage.New(serviceusage.WithProject("p", 123))
	for _, tc := range []struct {
		name, method, path, body string
		want                     int
	}{
		{"an unknown parameter", http.MethodGet, bq + "?view=FULL", "", http.StatusBadRequest},
		{"another format", http.MethodGet, bq + "?alt=proto", "", http.StatusBadRequest},
		{"an unknown verb", http.MethodPost, bq + ":generateServiceIdentity", `{}`, http.StatusNotFound},
		{"a verb on get", http.MethodGet, bq + ":enable", "", http.StatusNotFound},
		{"a batch enable", http.MethodPost, "/v1/projects/p/services:batchEnable", `{}`, http.StatusNotFound},
		{"a folder", http.MethodGet, "/v1/folders/1/services/bigquery.googleapis.com", "", http.StatusNotFound},
		{"an unknown field", http.MethodPost, bq + ":enable", `{"x":1}`, http.StatusBadRequest},
		{"another name in the body", http.MethodPost, bq + ":enable", `{"name":"projects/p/services/compute.googleapis.com"}`, http.StatusBadRequest},
		{"an unsupported filter", http.MethodGet, "/v1/projects/p/services?filter=name:bigquery", "", http.StatusBadRequest},
		{"a page token", http.MethodGet, "/v1/projects/p/services?pageToken=x", "", http.StatusBadRequest},
		{"a large page", http.MethodGet, "/v1/projects/p/services?pageSize=201", "", http.StatusBadRequest},
		{"an unknown operation", http.MethodGet, "/v1/operations/nope", "", http.StatusNotFound},
		{"an unknown project's list", http.MethodGet, "/v1/projects/q/services", "", http.StatusForbidden},
		{"an unknown service", http.MethodGet, "/v1/projects/p/services/made-up.googleapis.com", "", http.StatusForbidden},
	} {
		if code, body := do(t, s, tc.method, tc.path, tc.body); code != tc.want {
			t.Errorf("%s = %d %v, want %d", tc.name, code, body, tc.want)
		}
	}
}

func TestOperationsCanBeRead(t *testing.T) {
	s := serviceusage.New(serviceusage.WithProject("p", 123))
	code, op := do(t, s, http.MethodPost, bq+":enable", ``)
	if code != http.StatusOK || op["done"] != true || !strings.HasPrefix(op["name"].(string), "operations/acat.p2-123-") {
		t.Fatalf("enable = %d %v", code, op)
	}
	code, got := do(t, s, http.MethodGet, "/v1/"+op["name"].(string), "")
	resp := got["response"].(map[string]any)
	if code != http.StatusOK || resp["@type"] != "type.googleapis.com/google.api.serviceusage.v1.EnableServiceResponse" || resp["service"].(map[string]any)["state"] != "ENABLED" {
		t.Fatalf("get = %d %v", code, got)
	}
	code, got = do(t, s, http.MethodGet, "/v1/operations/noop.DONE_OPERATION", "")
	if code != http.StatusOK || got["done"] != true {
		t.Errorf("get the no-op operation = %d %v", code, got)
	}
}

func TestEnumsAsNumbers(t *testing.T) {
	s := serviceusage.New(serviceusage.WithProject("p", 123))
	if err := s.EnableService("123", "bigquery.googleapis.com"); err != nil {
		t.Fatal(err)
	}
	if _, out := do(t, s, http.MethodGet, bq, ""); out["state"] != "ENABLED" {
		t.Errorf("state = %v, want ENABLED", out["state"])
	}
	if _, out := do(t, s, http.MethodGet, bq+"?%24alt=json%3Benum-encoding%3Dint", ""); out["state"] != 2.0 {
		t.Errorf("state with enum-encoding=int = %v, want 2", out["state"])
	}
	_, out := do(t, s, http.MethodGet, "/v1/projects/p/services?filter=state:DISABLED", "")
	for _, svc := range out["services"].([]any) {
		if svc.(map[string]any)["config"].(map[string]any)["name"] == "bigquery.googleapis.com" {
			t.Errorf("the enabled service is listed as disabled: %v", svc)
		}
	}
}

func TestSeedingRefusesUnknownNames(t *testing.T) {
	s := serviceusage.New(serviceusage.WithProject("p", 123))
	if err := s.EnableService("q", "bigquery.googleapis.com"); err == nil {
		t.Error("enabling in an unknown project succeeded")
	}
	if err := s.EnableService("p", "made-up.googleapis.com"); err == nil {
		t.Error("enabling an unknown service succeeded")
	}
	if _, ok := s.LookupService("q", "bigquery.googleapis.com"); ok {
		t.Error("LookupService found a service in an unknown project")
	}
	if got := s.EnabledServices("q"); got != nil {
		t.Errorf("EnabledServices of an unknown project = %v", got)
	}
}
