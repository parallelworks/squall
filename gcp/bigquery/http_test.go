package bigquery_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/parallelworks/squall/gcp/bigquery"
)

const datasets = "/bigquery/v2/projects/p/datasets"

func do(t *testing.T, s *bigquery.Server, method, path, body string) (int, map[string]any) {
	t.Helper()
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), method, path, strings.NewReader(body)))
	var out map[string]any
	if rec.Body.Len() > 0 {
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatalf("%s %s: %v in %q", method, path, err, rec.Body)
		}
	}
	return rec.Code, out
}

func TestRequestsTheAPIRejects(t *testing.T) {
	s := bigquery.New()
	if code, body := do(t, s, http.MethodPost, datasets, `{"datasetReference":{"datasetId":"d"}}`); code != http.StatusOK {
		t.Fatalf("insert = %d %v", code, body)
	}
	for _, tc := range []struct {
		name, method, path, body string
		want                     int
	}{
		{"an unknown parameter", http.MethodGet, datasets + "/d?view=FULL", "", http.StatusBadRequest},
		{"another format", http.MethodGet, datasets + "/d?alt=media", "", http.StatusBadRequest},
		{"a bad dataset view", http.MethodGet, datasets + "/d?datasetView=ALL", "", http.StatusBadRequest},
		{"a table list", http.MethodGet, datasets + "/d/tables", "", http.StatusNotFound},
		{"an undelete", http.MethodPost, datasets + "/d:undelete", `{}`, http.StatusNotFound},
		{"no dataset reference", http.MethodPost, datasets, `{}`, http.StatusBadRequest},
		{"another project's reference", http.MethodPost, datasets, `{"datasetReference":{"datasetId":"e","projectId":"q"}}`, http.StatusBadRequest},
		{"an unknown field", http.MethodPost, datasets, `{"datasetReference":{"datasetId":"e"},"defaultTableExpirationMs":"3600000"}`, http.StatusBadRequest},
		{"trailing data", http.MethodPost, datasets, `{"datasetReference":{"datasetId":"e"}} {}`, http.StatusBadRequest},
		{"too many labels", http.MethodPost, datasets, `{"datasetReference":{"datasetId":"e"},"labels":{` + manyLabels(65) + `}}`, http.StatusBadRequest},
		{"a bad deleteContents", http.MethodDelete, datasets + "/d?deleteContents=yes", "", http.StatusBadRequest},
		{"a bad dataset ID on get", http.MethodGet, datasets + "/a-b", "", http.StatusBadRequest},
		{"a list filter", http.MethodGet, datasets + "?filter=labels.env", "", http.StatusBadRequest},
		{"a bad page token", http.MethodGet, datasets + "?pageToken=!", "", http.StatusBadRequest},
		{"a patch of an output field", http.MethodPatch, datasets + "/d", `{"location":"EU"}`, http.StatusBadRequest},
		{"a patch of no dataset", http.MethodPatch, datasets + "/e", `{}`, http.StatusNotFound},
		{"a patch of another dataset", http.MethodPatch, datasets + "/d", `{"datasetReference":{"datasetId":"e"}}`, http.StatusBadRequest},
	} {
		if code, body := do(t, s, tc.method, tc.path, tc.body); code != tc.want {
			t.Errorf("%s = %d %v, want %d", tc.name, code, body, tc.want)
		}
	}
}

func manyLabels(n int) string {
	labels := make([]string, n)
	for i := range n {
		labels[i] = `"k` + strings.Repeat("a", i) + `":"v"`
	}
	return strings.Join(labels, ",")
}

func TestErrorShape(t *testing.T) {
	s := bigquery.New()
	code, body := do(t, s, http.MethodGet, datasets+"/missing", "")
	e := body["error"].(map[string]any)
	reason := e["errors"].([]any)[0].(map[string]any)
	if code != http.StatusNotFound || e["code"] != 404.0 || e["status"] != "NOT_FOUND" || e["message"] != "Not found: Dataset p:missing" || reason["reason"] != "notFound" || reason["domain"] != "global" {
		t.Errorf("get = %d %v", code, body)
	}
}

func TestListPagesAndHidesDatasets(t *testing.T) {
	s := bigquery.New()
	for _, id := range []string{"c", "a", "_hidden", "b"} {
		if code, body := do(t, s, http.MethodPost, datasets, `{"datasetReference":{"datasetId":"`+id+`"}}`); code != http.StatusOK {
			t.Fatalf("insert %s = %d %v", id, code, body)
		}
	}
	var ids []string
	token := ""
	for range 5 {
		_, body := do(t, s, http.MethodGet, datasets+"?maxResults=2&pageToken="+token, "")
		for _, d := range body["datasets"].([]any) {
			ids = append(ids, d.(map[string]any)["datasetReference"].(map[string]any)["datasetId"].(string))
		}
		next, _ := body["nextPageToken"].(string)
		if next == "" {
			break
		}
		token = next
	}
	if strings.Join(ids, ",") != "a,b,c" {
		t.Errorf("pages = %v", ids)
	}
	if _, body := do(t, s, http.MethodGet, datasets+"?all=true", ""); len(body["datasets"].([]any)) != 4 {
		t.Errorf("list with all = %v", body)
	}
	if _, body := do(t, s, http.MethodGet, "/bigquery/v2/projects/other/datasets", ""); body["datasets"] != nil || body["kind"] != "bigquery#datasetList" {
		t.Errorf("list of another project = %v", body)
	}
}

func TestPatchRemovesNullLabels(t *testing.T) {
	s := bigquery.New()
	do(t, s, http.MethodPost, datasets, `{"datasetReference":{"datasetId":"d"},"labels":{"a":"1","b":"2"},"friendlyName":"D"}`)
	code, body := do(t, s, http.MethodPatch, datasets+"/d", `{"labels":{"a":null,"c":"3"},"description":"x"}`)
	labels, _ := body["labels"].(map[string]any)
	if code != http.StatusOK || len(labels) != 2 || labels["b"] != "2" || labels["c"] != "3" || body["friendlyName"] != "D" || body["description"] != "x" {
		t.Errorf("patch = %d %v", code, body)
	}
	if code, body := do(t, s, http.MethodPatch, datasets+"/d", `{"labels":{"Bad":"x"}}`); code != http.StatusBadRequest {
		t.Errorf("patch with a bad label = %d %v", code, body)
	}
}

func TestAddTableNeedsADataset(t *testing.T) {
	s := bigquery.New()
	if err := s.AddTable("p", "d", "t"); err == nil {
		t.Error("AddTable to no dataset succeeded")
	}
	do(t, s, http.MethodPost, datasets, `{"datasetReference":{"datasetId":"d"}}`)
	if err := s.AddTable("p", "d", ""); err == nil {
		t.Error("AddTable with no name succeeded")
	}
}
