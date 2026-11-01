package resourcemanager_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/parallelworks/squall/gcp/resourcemanager"
)

func do(t *testing.T, s *resourcemanager.Server, method, path, body string) (int, map[string]any) {
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
	s := resourcemanager.New(resourcemanager.WithProject("my-project", 123))
	const p = "/v3/projects/my-project"
	cond := `{"role":"roles/viewer","members":["user:a@example.com"],"condition":{"expression":"true"}}`
	for _, tc := range []struct {
		name, method, path, body string
		want                     int
	}{
		{"an unknown parameter", http.MethodGet, p + "?view=FULL", "", http.StatusBadRequest},
		{"another format", http.MethodGet, p + "?alt=proto", "", http.StatusBadRequest},
		{"an unknown verb", http.MethodPost, p + ":move", `{}`, http.StatusNotFound},
		{"a verb on get", http.MethodGet, p + ":getIamPolicy", "", http.StatusNotFound},
		{"a list", http.MethodGet, "/v3/projects", "", http.StatusNotFound},
		{"an unknown field", http.MethodPost, p + ":getIamPolicy", `{"x":1}`, http.StatusBadRequest},
		{"a bad requested version", http.MethodPost, p + ":getIamPolicy", `{"options":{"requestedPolicyVersion":2}}`, http.StatusBadRequest},
		{"no policy", http.MethodPost, p + ":setIamPolicy", `{}`, http.StatusBadRequest},
		{"audit configs", http.MethodPost, p + ":setIamPolicy", `{"policy":{"auditConfigs":[{"service":"allServices"}]}}`, http.StatusBadRequest},
		{"a bad role", http.MethodPost, p + ":setIamPolicy", `{"policy":{"bindings":[{"role":"viewer","members":["user:a@example.com"]}]}}`, http.StatusBadRequest},
		{"a bad member", http.MethodPost, p + ":setIamPolicy", `{"policy":{"bindings":[{"role":"roles/viewer","members":["a@example.com"]}]}}`, http.StatusBadRequest},
		{"no members", http.MethodPost, p + ":setIamPolicy", `{"policy":{"bindings":[{"role":"roles/viewer","members":[]}]}}`, http.StatusBadRequest},
		{"a condition below version 3", http.MethodPost, p + ":setIamPolicy", `{"policy":{"bindings":[` + cond + `]}}`, http.StatusBadRequest},
		{"a bad mask", http.MethodPost, p + ":setIamPolicy", `{"policy":{},"updateMask":"version"}`, http.StatusBadRequest},
		{"a bad etag", http.MethodPost, p + ":setIamPolicy", `{"policy":{"etag":"!!"}}`, http.StatusBadRequest},
		{"a stale etag", http.MethodPost, p + ":setIamPolicy", `{"policy":{"etag":"AAAA"}}`, http.StatusConflict},
		{"a wildcard permission", http.MethodPost, p + ":testIamPermissions", `{"permissions":["resourcemanager.*"]}`, http.StatusBadRequest},
		{"too many permissions", http.MethodPost, p + ":testIamPermissions", `{"permissions":[` + strings.Repeat(`"a.b.c",`, 100) + `"a.b.c"]}`, http.StatusBadRequest},
		{"another project", http.MethodGet, "/v3/projects/other-project", "", http.StatusForbidden},
		{"another project's policy", http.MethodPost, "/v3/projects/456:testIamPermissions", `{}`, http.StatusForbidden},
	} {
		if code, body := do(t, s, tc.method, tc.path, tc.body); code != tc.want {
			t.Errorf("%s = %d %v, want %d", tc.name, code, body, tc.want)
		}
	}
}

func TestConditionalPolicyNeedsVersion3(t *testing.T) {
	s := resourcemanager.New(resourcemanager.WithProject("my-project", 123))
	const p = "/v3/projects/123"
	set := `{"policy":{"version":3,"bindings":[{"role":"roles/viewer","members":["user:a@example.com"],"condition":{"title":"t","expression":"request.time < timestamp('2030-01-01T00:00:00Z')"}}]}}`
	if code, body := do(t, s, http.MethodPost, p+":setIamPolicy", set); code != http.StatusOK || body["version"] != 3.0 {
		t.Fatalf("set = %d %v", code, body)
	}
	if code, body := do(t, s, http.MethodPost, p+":getIamPolicy", `{}`); code != http.StatusBadRequest {
		t.Errorf("get without version 3 = %d %v, want 400", code, body)
	}
	if code, body := do(t, s, http.MethodPost, p+":getIamPolicy", `{"options":{"requestedPolicyVersion":3}}`); code != http.StatusOK || body["version"] != 3.0 {
		t.Errorf("get with version 3 = %d %v", code, body)
	}
}

func TestEnumsAsNumbers(t *testing.T) {
	s := resourcemanager.New(resourcemanager.WithProject("my-project", 123))
	if _, out := do(t, s, http.MethodGet, "/v3/projects/my-project", ""); out["state"] != "ACTIVE" {
		t.Errorf("state = %v, want ACTIVE", out["state"])
	}
	if _, out := do(t, s, http.MethodGet, "/v3/projects/my-project?%24alt=json%3Benum-encoding%3Dint", ""); out["state"] != 1.0 {
		t.Errorf("state with enum-encoding=int = %v, want 1", out["state"])
	}
}

func TestPutProjectRefusesConflicts(t *testing.T) {
	s := resourcemanager.New(resourcemanager.WithProject("my-project", 123))
	for _, p := range []resourcemanager.Project{
		{Name: "projects/123", ProjectID: "other-project"},
		{Name: "projects/456", ProjectID: "my-project"},
		{Name: "projects/0123", ProjectID: "other-project"},
		{Name: "projects/456", ProjectID: "Other"},
		{Name: "projects/456", ProjectID: "other-project-"},
		{Name: "projects/456", ProjectID: "other-project", Parent: "orgs/1"},
		{Name: "projects/456", ProjectID: "other-project", State: "DELETE_REQUESTED"},
	} {
		if err := s.PutProject(p); err == nil {
			t.Errorf("PutProject(%+v) = nil, want an error", p)
		}
	}
	if err := s.PutProject(resourcemanager.Project{Name: "projects/123", ProjectID: "my-project", Parent: "folders/7", DisplayName: "Mine"}); err != nil {
		t.Fatal(err)
	}
	got := s.Projects()
	if len(got) != 1 || got[0].Parent != "folders/7" || got[0].DisplayName != "Mine" || got[0].State != "ACTIVE" {
		t.Errorf("Projects() = %+v", got)
	}
	defer func() {
		if recover() == nil {
			t.Error("New did not panic on a project it cannot hold")
		}
	}()
	resourcemanager.New(resourcemanager.WithProject("x", 1))
}
