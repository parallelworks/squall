package arm_test

import (
	"bytes"
	"encoding/json"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/parallelworks/squall/azure/arm"
	"github.com/parallelworks/squall/fault"
)

// rest calls a data plane that has no Go client library in use, as its REST
// reference documents the calls.
type rest struct {
	t        *testing.T
	base     string
	key      string
	version  string
	header   http.Header
	lastETag string
}

func (r *rest) do(method, path string, body any) (int, map[string]any) {
	r.t.Helper()
	var reader io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			r.t.Fatal(err)
		}
		reader = bytes.NewReader(b)
	}
	sep := "?"
	if strings.Contains(path, "?") {
		sep = "&"
	}
	if r.version != "" {
		path += sep + "api-version=" + r.version
	}
	req, err := http.NewRequestWithContext(r.t.Context(), method, r.base+path, reader)
	if err != nil {
		r.t.Fatal(err)
	}
	if r.key != "" {
		req.Header.Set("api-key", r.key)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	maps.Copy(req.Header, r.header)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		r.t.Fatal(err)
	}
	defer resp.Body.Close()
	r.lastETag = resp.Header.Get("ETag")
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		r.t.Fatal(err)
	}
	var out map[string]any
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &out); err != nil {
			r.t.Fatalf("%s %s answered %d %q: %v", method, path, resp.StatusCode, raw, err)
		}
	}
	return resp.StatusCode, out
}

func errorMessage(body map[string]any) string {
	e, _ := body["error"].(map[string]any)
	m, _ := e["message"].(string)
	return m
}

// searchService creates a search service and returns a client of its data
// plane that sends its secondary admin key.
func (c *clients) searchService(t *testing.T, name string) *rest {
	t.Helper()
	c.createGroup(t, group, nil)
	p, err := c.beginSearch(t, name)
	if _, err := wait(t, p, err); err != nil {
		t.Fatal(err)
	}
	keys, err := c.searchKeys.Get(t.Context(), group, name, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(c.server.Search())
	t.Cleanup(srv.Close)
	return &rest{t: t, base: srv.URL + "/" + name, key: *keys.SecondaryKey, version: "2024-07-01"}
}

func docsIndex(name string, extra ...map[string]any) map[string]any {
	fields := []any{
		map[string]any{"name": "id", "type": "Edm.String", "key": true},
		map[string]any{"name": "title", "type": "Edm.String", "searchable": true},
		map[string]any{"name": "secret", "type": "Edm.String", "retrievable": false},
	}
	for _, f := range extra {
		fields = append(fields, f)
	}
	return map[string]any{"name": name, "fields": fields, "corsOptions": map[string]any{"allowedOrigins": []any{"*"}}}
}

func TestSearchIndexes(t *testing.T) {
	c := setup(t)
	api := c.searchService(t, "find")
	status, created := api.do("PUT", "/indexes/docs", docsIndex("docs"))
	if status != http.StatusCreated || created["name"] != "docs" || created["@odata.etag"] == nil || api.lastETag != created["@odata.etag"] {
		t.Fatalf("create = %d %v, ETag %q", status, created, api.lastETag)
	}
	etag := api.lastETag
	if status, got := api.do("GET", "/indexes('docs')", nil); status != http.StatusOK || got["corsOptions"] == nil {
		t.Fatalf("get by key = %d %v", status, got)
	}
	update := docsIndex("docs", map[string]any{"name": "url", "type": "Edm.String"})
	update["@odata.etag"] = etag
	if status, got := api.do("PUT", "/indexes/docs", update); status != http.StatusOK || len(got["fields"].([]any)) != 4 || api.lastETag == etag {
		t.Fatalf("update = %d %v", status, got)
	}
	if status, got := api.do("PUT", "/indexes/docs", map[string]any{"fields": docsIndex("docs")["fields"].([]any)[:1]}); status != http.StatusBadRequest {
		t.Fatalf("removing fields = %d %v", status, got)
	}
	if status, got := api.do("PUT", "/indexes/docs", map[string]any{"name": "other", "fields": []any{}}); status != http.StatusBadRequest {
		t.Fatalf("a body naming another index = %d %v", status, got)
	}
	if status, got := api.do("PUT", "/indexes/two", map[string]any{"fields": []any{map[string]any{"name": "n", "type": "Edm.Int32", "key": true}}}); status != http.StatusBadRequest {
		t.Fatalf("a key that is not a string = %d %v", status, got)
	}
	if status, got := api.do("PUT", "/indexes/two", map[string]any{"fields": []any{}, "colour": "red"}); status != http.StatusBadRequest ||
		!strings.Contains(errorMessage(got), "'colour' does not exist") {
		t.Fatalf("an unknown member = %d %v", status, got)
	}
	if status, _ := api.do("PUT", "/indexes/Upper", docsIndex("Upper")); status != http.StatusBadRequest {
		t.Fatalf("an upper-case index name = %d", status)
	}

	api.header = http.Header{"If-None-Match": {"*"}}
	if status, _ := api.do("PUT", "/indexes/docs", docsIndex("docs")); status != http.StatusPreconditionFailed {
		t.Fatalf("If-None-Match * on an index = %d", status)
	}
	api.header = http.Header{"If-Match": {etag}}
	if status, _ := api.do("DELETE", "/indexes/docs", nil); status != http.StatusPreconditionFailed {
		t.Fatalf("a delete with a stale If-Match = %d", status)
	}
	api.header = http.Header{"Prefer": {"return=minimal"}}
	if status, body := api.do("PUT", "/indexes/minimal", docsIndex("minimal")); status != http.StatusNoContent || body != nil {
		t.Fatalf("return=minimal = %d %v", status, body)
	}
	api.header = nil

	if status, _ := api.do("DELETE", "/indexes/docs", nil); status != http.StatusNoContent {
		t.Fatalf("delete = %d", status)
	}
	if status, got := api.do("GET", "/indexes/docs", nil); status != http.StatusNotFound || errorMessage(got) != "No index with the name 'docs' was found in a service named 'find'." {
		t.Fatalf("a deleted index = %d %v", status, got)
	}
	if status, _ := api.do("DELETE", "/indexes('docs')", nil); status != http.StatusNotFound {
		t.Fatalf("deleting a missing index = %d", status)
	}
}

func TestSearchDataPlaneAuthentication(t *testing.T) {
	c := setup(t)
	api := c.searchService(t, "find")
	keys, err := c.searchKeys.Get(t.Context(), group, "find", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	api.key = *keys.PrimaryKey
	if status, _ := api.do("PUT", "/indexes/docs", docsIndex("docs")); status != http.StatusCreated {
		t.Fatalf("the primary key = %d", status)
	}
	api.key = "wrong"
	if status, _ := api.do("GET", "/indexes/docs", nil); status != http.StatusForbidden {
		t.Fatalf("a wrong key = %d", status)
	}
	api.key = ""
	if status, _ := api.do("GET", "/indexes/docs", nil); status != http.StatusForbidden {
		t.Fatalf("no key = %d", status)
	}
	api.key, api.version = *keys.PrimaryKey, ""
	if status, got := api.do("GET", "/indexes/docs", nil); status != http.StatusBadRequest || !strings.Contains(errorMessage(got), "api-version") {
		t.Fatalf("no api-version = %d %v", status, got)
	}
	api.version = "2024-07-01"
	other := &rest{t: t, base: strings.TrimSuffix(api.base, "/find") + "/lost", key: api.key, version: "2024-07-01"}
	if status, _ := other.do("GET", "/indexes/docs", nil); status != http.StatusNotFound {
		t.Fatalf("a service that does not exist = %d", status)
	}

	// The service's host name names it too.
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, strings.TrimSuffix(api.base, "/find")+"/indexes/docs?api-version=2024-07-01", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Host = "find.search.windows.net"
	req.Header.Set("api-key", api.key)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("addressed by host name = %d", resp.StatusCode)
	}

	if _, err := c.search.Delete(t.Context(), group, "find", nil, nil); err != nil {
		t.Fatal(err)
	}
	if status, _ := api.do("GET", "/indexes/docs", nil); status != http.StatusNotFound {
		t.Fatalf("a deleted service = %d", status)
	}
	c.server.Settle()
	p, err := c.beginSearch(t, "find")
	if _, err := wait(t, p, err); err != nil {
		t.Fatal(err)
	}
	keys, err = c.searchKeys.Get(t.Context(), group, "find", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	api.key = *keys.PrimaryKey
	if status, _ := api.do("GET", "/indexes/docs", nil); status != http.StatusNotFound {
		t.Fatalf("an index of a service deleted and made again = %d", status)
	}
}

func dataSource(name string) map[string]any {
	return map[string]any{
		"name": name, "type": "azureblob", "credentials": map[string]any{"connectionString": "DefaultEndpointsProtocol=https;AccountName=a;AccountKey=k"},
		"container": map[string]any{"name": "docs"},
	}
}

func lastRun(t *testing.T, api *rest, indexer string) (string, map[string]any) {
	t.Helper()
	status, got := api.do("GET", "/indexers('"+indexer+"')/search.status", nil)
	if status != http.StatusOK || got["status"] != "running" {
		t.Fatalf("status = %d %v", status, got)
	}
	last, _ := got["lastResult"].(map[string]any)
	s, _ := last["status"].(string)
	return s, got
}

func TestSearchDataSourcesAndIndexers(t *testing.T) {
	c := setup(t, arm.WithSettleAfter(1))
	api := c.searchService(t, "find")
	if status, got := api.do("PUT", "/datasources/blobs", dataSource("blobs")); status != http.StatusCreated ||
		got["credentials"].(map[string]any)["connectionString"] != nil {
		t.Fatalf("create data source = %d %v", status, got)
	}
	keep := dataSource("blobs")
	keep["credentials"] = map[string]any{"connectionString": "<unchanged>"}
	if status, _ := api.do("PUT", "/datasources('blobs')", keep); status != http.StatusOK {
		t.Fatalf("an update keeping the connection string = %d", status)
	}
	if status, _ := api.do("PUT", "/datasources/other", map[string]any{"type": "azureblob", "container": map[string]any{"name": "x"}}); status != http.StatusBadRequest {
		t.Fatalf("a data source without credentials = %d", status)
	}
	if status, _ := api.do("PUT", "/datasources/other", map[string]any{"type": "ftp"}); status != http.StatusBadRequest {
		t.Fatalf("an unknown data source type = %d", status)
	}
	indexer := map[string]any{"dataSourceName": "blobs", "targetIndexName": "docs"}
	if status, got := api.do("PUT", "/indexers/nightly", indexer); status != http.StatusBadRequest || !strings.Contains(errorMessage(got), "target index 'docs'") {
		t.Fatalf("an indexer of a missing index = %d %v", status, got)
	}
	api.do("PUT", "/indexes/docs", docsIndex("docs"))
	if status, got := api.do("PUT", "/indexers/nightly", indexer); status != http.StatusCreated || got["dataSourceName"] != "blobs" {
		t.Fatalf("create indexer = %d %v", status, got)
	}
	if run, _ := lastRun(t, api, "nightly"); run != "inProgress" {
		t.Fatalf("a new indexer's run = %q", run)
	}
	if status, got := api.do("POST", "/indexers('nightly')/search.run", nil); status != http.StatusConflict ||
		errorMessage(got) != "Another indexer invocation is currently in progress; concurrent invocations are not allowed." {
		t.Fatalf("a run while one is in progress = %d %v", status, got)
	}
	run, got := lastRun(t, api, "nightly")
	if run != "success" || got["lastResult"].(map[string]any)["itemsProcessed"] != 0.0 {
		t.Fatalf("a run after its reads = %q %v", run, got)
	}
	if status, _ := api.do("POST", "/indexers/nightly/search.run", nil); status != http.StatusAccepted {
		t.Fatalf("run = %d", status)
	}
	c.server.Settle()
	if run, got := lastRun(t, api, "nightly"); run != "success" || len(got["executionHistory"].([]any)) != 2 {
		t.Fatalf("a settled run = %q %v", run, got)
	}
	disabled := map[string]any{"dataSourceName": "blobs", "targetIndexName": "docs", "disabled": true}
	api.do("PUT", "/indexers/off", disabled)
	if _, got := lastRun(t, api, "off"); got["lastResult"] != nil {
		t.Fatalf("a disabled indexer ran: %v", got)
	}
	if status, _ := api.do("PUT", "/indexers/skilled", map[string]any{"dataSourceName": "blobs", "targetIndexName": "docs", "skillsetName": "s"}); status != http.StatusBadRequest {
		t.Fatalf("an indexer with a skillset = %d", status)
	}
	if status, _ := api.do("GET", "/indexers('nightly')", nil); status != http.StatusOK {
		t.Fatalf("get indexer = %d", status)
	}
	for _, path := range []string{"/indexers('nightly')", "/datasources/blobs"} {
		if status, _ := api.do("DELETE", path, nil); status != http.StatusNoContent {
			t.Fatalf("delete %s = %d", path, status)
		}
		if status, _ := api.do("DELETE", path, nil); status != http.StatusNotFound {
			t.Fatalf("delete %s again = %d", path, status)
		}
	}
	if status, got := api.do("GET", "/indexers/nightly/search.status", nil); status != http.StatusNotFound ||
		errorMessage(got) != "No indexer with the name 'nightly' was found in a service named 'find'." {
		t.Fatalf("status of a deleted indexer = %d %v", status, got)
	}
}

func TestSearchDocuments(t *testing.T) {
	c := setup(t)
	api := c.searchService(t, "find")
	api.do("PUT", "/indexes/docs", docsIndex("docs"))
	batch := map[string]any{"value": []any{
		map[string]any{"@search.action": "upload", "id": "b", "title": "guide.pdf", "secret": "s"},
		map[string]any{"id": "a", "title": "notes.txt"},
		map[string]any{"@search.action": "upload", "id": "c", "title": "guide.pdf"},
	}}
	if status, got := api.do("POST", "/indexes/docs/docs/index", batch); status != http.StatusOK || len(got["value"].([]any)) != 3 ||
		got["value"].([]any)[0].(map[string]any)["statusCode"] != 201.0 {
		t.Fatalf("index = %d %v", status, got)
	}
	status, got := api.do("POST", "/indexes('docs')/docs/search.index", map[string]any{"value": []any{
		map[string]any{"@search.action": "merge", "id": "a", "title": "renamed.txt"},
		map[string]any{"@search.action": "merge", "id": "z", "title": "x"},
		map[string]any{"@search.action": "delete", "id": "c"},
		map[string]any{"@search.action": "mergeOrUpload", "id": "d", "title": "new.md"},
	}})
	results, _ := got["value"].([]any)
	codes := []float64{}
	for _, r := range results {
		codes = append(codes, r.(map[string]any)["statusCode"].(float64))
	}
	if status != http.StatusMultiStatus || !slices.Equal(codes, []float64{200, 404, 200, 201}) {
		t.Fatalf("a batch with a failure = %d %v", status, got)
	}
	status, got = api.do("GET", "/indexes('docs')/docs?$select=title&$count=true", nil)
	var titles []any
	for _, hit := range got["value"].([]any) {
		h := hit.(map[string]any)
		if len(h) != 2 || h["@search.score"] != 1.0 {
			t.Fatalf("a hit = %v", h)
		}
		titles = append(titles, h["title"])
	}
	if status != http.StatusOK || got["@odata.count"] != 3.0 || !slices.Equal(titles, []any{"renamed.txt", "guide.pdf", "new.md"}) {
		t.Fatalf("search = %d %v", status, got)
	}
	if _, got := api.do("GET", "/indexes/docs/docs?search=*&$top=1&$skip=1", nil); len(got["value"].([]any)) != 1 ||
		got["value"].([]any)[0].(map[string]any)["id"] != "b" || got["value"].([]any)[0].(map[string]any)["secret"] != nil {
		t.Fatalf("a page = %v", got)
	}
	for _, query := range []string{"?search=guide", "?$filter=title eq 'x'", "?$select=colour", "?$top=-1"} {
		if status, _ := api.do("GET", "/indexes/docs/docs"+strings.ReplaceAll(query, " ", "%20"), nil); status != http.StatusBadRequest {
			t.Fatalf("search %s = %d", query, status)
		}
	}
	for _, body := range []map[string]any{
		{"value": []any{map[string]any{"id": "e", "colour": "red"}}},
		{"value": []any{map[string]any{"title": "keyless"}}},
	} {
		if status, _ := api.do("POST", "/indexes/docs/docs/index", body); status != http.StatusBadRequest {
			t.Fatalf("index %v = %d", body, status)
		}
	}
	api.do("DELETE", "/indexes/docs", nil)
	api.do("PUT", "/indexes/docs", docsIndex("docs"))
	if _, got := api.do("GET", "/indexes/docs/docs", nil); len(got["value"].([]any)) != 0 {
		t.Fatalf("an index made again kept documents: %v", got)
	}
}

func TestSearchDataPlaneFaults(t *testing.T) {
	faults := fault.New()
	c := setup(t, arm.WithFaults(faults))
	api := c.searchService(t, "find")
	api.do("PUT", "/indexes/docs", docsIndex("docs"))
	api.do("PUT", "/datasources/blobs", dataSource("blobs"))
	api.do("PUT", "/indexers/nightly", map[string]any{"dataSourceName": "blobs", "targetIndexName": "docs"})
	faults.Add(
		fault.Rule{Method: "Indexers_Run", Fault: fault.FailOperation(http.StatusInternalServerError).WithMessage("The data source is unreachable.")},
		fault.Rule{Method: "Indexes_Delete", Resource: "find/indexes/docs", Times: 1, Fault: fault.Reject(http.StatusInternalServerError, "")},
		fault.Rule{Method: "Documents_SearchGet", Times: 1, Fault: fault.Unavailable().WithMessage("busy")},
	)
	if status, _ := api.do("POST", "/indexers/nightly/search.run", nil); status != http.StatusAccepted {
		t.Fatalf("a run whose execution fails = %d", status)
	}
	run, got := lastRun(t, api, "nightly")
	if last := got["lastResult"].(map[string]any); run != "transientFailure" || last["errorMessage"] != "The data source is unreachable." {
		t.Fatalf("a failed run = %v", got)
	}
	if status, got := api.do("GET", "/indexes/docs/docs", nil); status != http.StatusServiceUnavailable || errorMessage(got) != "busy" {
		t.Fatalf("a faulted search = %d %v", status, got)
	}
	if status, _ := api.do("DELETE", "/indexes/docs", nil); status != http.StatusInternalServerError {
		t.Fatalf("a faulted delete = %d", status)
	}
	if status, _ := api.do("DELETE", "/indexes/docs", nil); status != http.StatusNoContent {
		t.Fatalf("a delete after the fault = %d", status)
	}
}
