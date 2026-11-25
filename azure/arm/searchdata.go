package arm

import (
	"bytes"
	"cmp"
	"encoding/json"
	"fmt"
	"maps"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/parallelworks/squall/fault"
	"github.com/parallelworks/squall/internal/stub"
)

// Search returns the data plane of the server's search services, an
// [http.Handler] to serve with [net/http/httptest]. It takes the service from
// a host name such as {service}.search.windows.net, or else from the first
// path segment, so a client reaches the service at {server URL}/{service}.
func (s *Server) Search() http.Handler { return http.HandlerFunc(s.serveSearch) }

// searchState is what a search service's data plane holds.
type searchState struct {
	indexes     map[string]*searchIndex
	dataSources map[string]*searchObject
	indexers    map[string]*indexer
}

// searchObject is an index, data source or indexer definition.
type searchObject struct {
	body map[string]any
	etag string
}

type searchIndex struct {
	searchObject
	key    string
	fields map[string]map[string]any
	docs   map[string]map[string]any
}

type indexer struct {
	searchObject
	// runs are the indexer's executions, newest first.
	runs []*indexerRun
}

// indexerRun is one execution of an indexer.
type indexerRun struct {
	status     string
	err        string
	start, end time.Time
	// wait is how many more status reads see the run in progress.
	wait int
}

const (
	runInProgress = "inProgress"
	runSuccess    = "success"
	runFailure    = "transientFailure"
	maxRuns       = 50
	maxIndexBatch = 1000
)

type searchCall struct {
	r       *http.Request
	query   url.Values
	service string
	// base is the URL the service is addressed at, for @odata.context.
	base       string
	collection string
	name       string
	rest       string
	op         *searchOp
	svc        *resource
	// opFault, when set, is the error a run must end in.
	opFault *apiError
}

func (c *searchCall) resource() string {
	return strings.ToLower(c.service) + "/" + c.collection + "/" + c.name
}

type searchOp struct {
	// name is the operation's ID in the REST API's specification, such as Indexes_CreateOrUpdate, which fault rules match.
	name string
	h    func(s *Server, c *searchCall, st *searchState) (*searchReply, *apiError)
}

type searchReply struct {
	status int
	etag   string
	body   map[string]any
}

var (
	putIndexOp        = &searchOp{"Indexes_CreateOrUpdate", (*Server).putSearchIndex}
	getIndexOp        = &searchOp{"Indexes_Get", (*Server).getSearchIndex}
	deleteIndexOp     = &searchOp{"Indexes_Delete", (*Server).deleteSearchIndex}
	putDataSourceOp   = &searchOp{"DataSources_CreateOrUpdate", (*Server).putDataSource}
	getDataSourceOp   = &searchOp{"DataSources_Get", (*Server).getDataSource}
	deleteDataSourceO = &searchOp{"DataSources_Delete", (*Server).deleteDataSource}
	putIndexerOp      = &searchOp{"Indexers_CreateOrUpdate", (*Server).putIndexer}
	getIndexerOp      = &searchOp{"Indexers_Get", (*Server).getIndexer}
	deleteIndexerOp   = &searchOp{"Indexers_Delete", (*Server).deleteIndexer}
	runIndexerOp      = &searchOp{"Indexers_Run", (*Server).runIndexer}
	indexerStatusOp   = &searchOp{"Indexers_GetStatus", (*Server).indexerStatus}
	indexDocumentsOp  = &searchOp{"Documents_Index", (*Server).indexDocuments}
	searchDocumentsOp = &searchOp{"Documents_SearchGet", (*Server).searchDocuments}
)

// searchOperation returns the operation c asks for, or nil if it is not served.
func searchOperation(c *searchCall) *searchOp {
	m := c.r.Method
	switch c.collection + " " + c.rest {
	case "indexes ":
		return pick(m, putIndexOp, getIndexOp, deleteIndexOp)
	case "datasources ":
		return pick(m, putDataSourceOp, getDataSourceOp, deleteDataSourceO)
	case "indexers ":
		return pick(m, putIndexerOp, getIndexerOp, deleteIndexerOp)
	case "indexers search.run":
		if m == http.MethodPost {
			return runIndexerOp
		}
	case "indexers search.status":
		if m == http.MethodGet {
			return indexerStatusOp
		}
	case "indexes docs/index", "indexes docs/search.index":
		if m == http.MethodPost {
			return indexDocumentsOp
		}
	case "indexes docs":
		if m == http.MethodGet {
			return searchDocumentsOp
		}
	}
	return nil
}

func pick(method string, put, get, del *searchOp) *searchOp {
	switch method {
	case http.MethodPut:
		return put
	case http.MethodGet:
		return get
	case http.MethodDelete:
		return del
	}
	return nil
}

// odataKey addresses an object by its key, as in indexers('name').
var odataKey = regexp.MustCompile(`^(indexes|datasources|indexers)\('([^']*)'\)$`)

// parseSearchRequest finds the service, collection, object and rest of the path r names.
func parseSearchRequest(r *http.Request) *searchCall {
	c := &searchCall{r: r, query: r.URL.Query()}
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	path := strings.TrimPrefix(r.URL.EscapedPath(), "/")
	host, _, err := net.SplitHostPort(r.Host)
	if err != nil {
		host = r.Host
	}
	if labels := strings.Split(host, "."); len(labels) > 3 && strings.EqualFold(labels[1], "search") {
		c.service, c.base = labels[0], scheme+"://"+r.Host
	} else {
		c.service, path, _ = strings.Cut(path, "/")
		c.base = scheme + "://" + r.Host + "/" + c.service
	}
	segments := strings.Split(path, "/")
	if m := odataKey.FindStringSubmatch(segments[0]); m != nil {
		c.collection, c.name = m[1], m[2]
		segments = segments[1:]
	} else if len(segments) > 1 && slices.Contains([]string{"indexes", "datasources", "indexers"}, segments[0]) {
		c.collection, c.name = segments[0], segments[1]
		segments = segments[2:]
	}
	c.name, _ = url.PathUnescape(c.name)
	c.rest = strings.Join(segments, "/")
	return c
}

func (s *Server) serveSearch(w http.ResponseWriter, r *http.Request) {
	c := parseSearchRequest(r)
	if c.collection != "" && c.name != "" {
		c.op = searchOperation(c)
	}
	if c.op == nil {
		s.writeSearchError(w, newError(http.StatusNotFound, "", "squall: %s %s is not served", r.Method, r.URL.RequestURI()))
		return
	}
	if c.query.Get("api-version") == "" {
		s.writeSearchError(w, newError(http.StatusBadRequest, "", "Invalid or missing api-version query string parameter."))
		return
	}
	if err := s.admitSearch(c); err != nil {
		s.writeSearchError(w, err)
		return
	}
	f, _ := s.faults.Decide(fault.Call{Method: c.op.name, Resource: c.resource()})
	reject := f.Mode == fault.ModeReject || f.Mode == fault.ModeFailOperation && c.op != runIndexerOp
	if f.Mode == fault.ModeFailOperation && !reject {
		c.opFault = searchFaultError(f)
	}
	stub.Serve(w, r, f, reject, "application/json", func() { s.writeSearchError(w, searchFaultError(f)) }, func() (func(), bool) {
		s.mu.Lock()
		rep, err := c.op.h(s, c, s.searchStateOf(c.svc))
		s.mu.Unlock()
		if err != nil {
			return func() { s.writeSearchError(w, err) }, false
		}
		return func() { s.writeSearchReply(w, rep) }, true
	})
}

// admitSearch finds the service c names and checks its admin key.
func (s *Server) admitSearch(c *searchCall) *apiError {
	s.mu.Lock()
	defer s.mu.Unlock()
	c.svc = s.searchServiceNamed(c.service)
	if c.svc == nil {
		return newError(http.StatusNotFound, "ResourceNotFound", "squall: no search service is named %s", c.service)
	}
	key := c.r.Header.Get("api-key")
	if key == "" && strings.HasPrefix(c.r.Header.Get("Authorization"), "Bearer ") {
		return unsupported("Microsoft Entra ID tokens are not supported; send an admin key in api-key")
	}
	if key != accessKey(c.svc, "primary", 52) && key != accessKey(c.svc, "secondary", 52) {
		return newError(http.StatusForbidden, "", "Authorization failed. The api-key is missing or invalid.")
	}
	return nil
}

// searchServiceNamed returns the search service named name, the one whose ID
// sorts first when several share it. The caller holds s.mu.
func (s *Server) searchServiceNamed(name string) *resource {
	var found *resource
	for _, res := range s.resources {
		if res.kind == searchKind && strings.EqualFold(res.name, name) && (found == nil || key(res.id) < key(found.id)) {
			found = res
		}
	}
	return found
}

// searchStateOf returns the data plane state of the service res. The caller holds s.mu.
func (s *Server) searchStateOf(res *resource) *searchState {
	if res.searchData == nil {
		res.searchData = &searchState{indexes: map[string]*searchIndex{}, dataSources: map[string]*searchObject{}, indexers: map[string]*indexer{}}
	}
	return res.searchData
}

// searchFaultError is the error a fault answers with. Search leaves the code
// of most errors empty, as the fault package's generic reasons become.
func searchFaultError(f fault.Fault) *apiError {
	status := cmp.Or(f.Status, http.StatusServiceUnavailable)
	code := f.Reason
	switch code {
	case "", "backendError":
		code = ""
	case "rateLimitExceeded":
		code, status = "", http.StatusServiceUnavailable
	}
	return &apiError{Status: status, Code: code, Message: f.ErrorMessage()}
}

func (s *Server) searchHeaders(h http.Header) {
	h.Set("request-id", fmt.Sprintf("%08x-0000-4000-8000-%012x", 0, s.requests.Add(1)))
	h.Set("OData-Version", "4.0")
}

func (s *Server) writeSearchError(w http.ResponseWriter, e *apiError) {
	s.searchHeaders(w.Header())
	writeJSON(w, e.Status, e.Header, encode(map[string]any{"error": map[string]any{"code": e.Code, "message": e.Message}}))
}

func (s *Server) writeSearchReply(w http.ResponseWriter, rep *searchReply) {
	h := w.Header()
	s.searchHeaders(h)
	if rep.etag != "" {
		h.Set("ETag", rep.etag)
	}
	if rep.body == nil {
		w.WriteHeader(rep.status)
		return
	}
	var buf bytes.Buffer
	if err := json.NewEncoder(&buf).Encode(rep.body); err != nil {
		panic(fmt.Sprintf("squall: encoding a response: %v", err))
	}
	h.Set("Content-Type", "application/json; odata.metadata=minimal; odata.streaming=true")
	w.WriteHeader(rep.status)
	_, _ = w.Write(buf.Bytes())
}

// searchETag returns a new ETag in the form Search gives them. The caller holds s.mu.
func (s *Server) searchETag() string {
	s.seq++
	return fmt.Sprintf(`"0x8D%013X"`, s.seq)
}

func (o *searchObject) render(c *searchCall, collection string) map[string]any {
	m := maps.Clone(o.body)
	m["@odata.context"] = c.base + "/$metadata#" + collection + "/$entity"
	m["@odata.etag"] = o.etag
	return m
}

// readSearchObject reads a definition from the request body, rejecting
// members not in known, and checks that the name it gives, if any, is the
// one in the URL.
func readSearchObject(c *searchCall, typeName string, known ...string) (map[string]any, *apiError) {
	body, err := readObject(c.r)
	if err != nil {
		return nil, newError(http.StatusBadRequest, "", "The request is invalid. Details: %s", err.Message)
	}
	for _, k := range slices.Sorted(maps.Keys(body)) {
		if strings.HasPrefix(k, "@odata.") {
			delete(body, k)
			continue
		}
		if k != "name" && !slices.Contains(known, k) {
			return nil, newError(http.StatusBadRequest, "",
				"The request is invalid. Details: The property '%s' does not exist on type '%s'. Make sure to only use property names that are defined by the type.", k, typeName)
		}
	}
	if name, ok := body["name"].(string); ok && name != c.name {
		return nil, newError(http.StatusBadRequest, "", "The %s name '%s' in the request body does not match the name '%s' in the URL.", typeName, name, c.name)
	}
	body["name"] = c.name
	return body, nil
}

// searchPrecondition applies If-Match and If-None-Match to a change of existing, which may be nil.
func searchPrecondition(r *http.Request, existing *searchObject) *apiError {
	m, none := r.Header.Get("If-Match"), r.Header.Get("If-None-Match")
	if m != "" && (existing == nil || m != "*" && m != existing.etag) || none == "*" && existing != nil {
		return newError(http.StatusPreconditionFailed, "",
			"The precondition given in one of the request headers evaluated to false. No change was made to the resource from this request.")
	}
	return nil
}

// putAnswer answers a create with 201 and an update with 200, or 204 when
// the caller prefers return=minimal.
func putAnswer(c *searchCall, created bool, o *searchObject, collection string) *searchReply {
	status := http.StatusOK
	if created {
		status = http.StatusCreated
	}
	if strings.Contains(c.r.Header.Get("Prefer"), "return=minimal") {
		return &searchReply{status: http.StatusNoContent, etag: o.etag}
	}
	return &searchReply{status: status, etag: o.etag, body: o.render(c, collection)}
}

func searchNotFound(what, name, service string) *apiError {
	return newError(http.StatusNotFound, "", "No %s with the name '%s' was found in a service named '%s'.", what, name, service)
}

var (
	indexNames  = regexp.MustCompile(`^[a-z0-9](?:-?[a-z0-9])*$`)
	objectNames = regexp.MustCompile(`^[A-Za-z0-9](?:[A-Za-z0-9_]|-[A-Za-z0-9_])*$`)
)

func checkSearchName(what, name string, pattern *regexp.Regexp) *apiError {
	if len(name) > 128 || !pattern.MatchString(name) {
		return newError(http.StatusBadRequest, "", "The %s name '%s' is invalid.", what, name)
	}
	return nil
}

func (s *Server) putSearchIndex(c *searchCall, st *searchState) (*searchReply, *apiError) {
	if err := checkSearchName("index", c.name, indexNames); err != nil {
		return nil, err
	}
	body, err := readSearchObject(c, "Index",
		"analyzers", "charFilters", "corsOptions", "defaultScoringProfile", "description", "encryptionKey", "fields", "normalizers",
		"scoringProfiles", "semantic", "similarity", "suggesters", "tokenFilters", "tokenizers", "vectorSearch")
	if err != nil {
		return nil, err
	}
	existing := st.indexes[c.name]
	var prior *searchObject
	if existing != nil {
		prior = &existing.searchObject
	}
	if err := searchPrecondition(c.r, prior); err != nil {
		return nil, err
	}
	fields, keyField, err := indexFields(body)
	if err != nil {
		return nil, err
	}
	if existing != nil {
		for name, old := range existing.fields {
			now := fields[name]
			if now == nil || str(now, "type") != str(old, "type") || name == existing.key && name != keyField {
				return nil, newError(http.StatusBadRequest, "", "Existing field '%s' cannot be removed or have its type or key changed.", name)
			}
		}
	}
	idx := existing
	if idx == nil {
		idx = &searchIndex{docs: map[string]map[string]any{}}
		st.indexes[c.name] = idx
	}
	idx.body, idx.etag, idx.key, idx.fields = body, s.searchETag(), keyField, fields
	return putAnswer(c, existing == nil, &idx.searchObject, "indexes"), nil
}

// indexFields reads an index's fields, by name, and the name of its key,
// which must be the one key field and of type Edm.String.
func indexFields(body map[string]any) (map[string]map[string]any, string, *apiError) {
	list, err := objects(body, "fields")
	if err != nil {
		return nil, "", newError(http.StatusBadRequest, "", "The request is invalid. Details: %s", err.Message)
	}
	fields := map[string]map[string]any{}
	keyField := ""
	for _, f := range list {
		name, typ := str(f, "name"), str(f, "type")
		if name == "" || typ == "" {
			return nil, "", newError(http.StatusBadRequest, "", "The request is invalid. Details: every field needs a name and a type.")
		}
		if fields[name] != nil {
			return nil, "", newError(http.StatusBadRequest, "", "The request is invalid. Details: the field name '%s' is used more than once.", name)
		}
		fields[name] = f
		if isKey, _ := boolean(f, "key"); isKey {
			if keyField != "" || typ != "Edm.String" {
				keyField = ""
				break
			}
			keyField = name
		}
	}
	if keyField == "" {
		return nil, "", newError(http.StatusBadRequest, "", "The request is invalid. Details: the index must have exactly one key field, of type Edm.String.")
	}
	return fields, keyField, nil
}

func (s *Server) getSearchIndex(c *searchCall, st *searchState) (*searchReply, *apiError) {
	idx := st.indexes[c.name]
	if idx == nil {
		return nil, searchNotFound("index", c.name, c.svc.name)
	}
	return &searchReply{status: http.StatusOK, etag: idx.etag, body: idx.render(c, "indexes")}, nil
}

// deleteSearchIndex deletes the index and its documents, answering 404 for
// one that does not exist, as Search does.
func (s *Server) deleteSearchIndex(c *searchCall, st *searchState) (*searchReply, *apiError) {
	idx := st.indexes[c.name]
	if idx == nil {
		return nil, searchNotFound("index", c.name, c.svc.name)
	}
	if err := searchPrecondition(c.r, &idx.searchObject); err != nil {
		return nil, err
	}
	delete(st.indexes, c.name)
	return &searchReply{status: http.StatusNoContent}, nil
}

var dataSourceTypes = []string{"adlsgen2", "azureblob", "azuresql", "azuretable", "cosmosdb", "mysql", "onelake"}

// unchangedSecret stands for a data source's connection string, which GET
// leaves out, in an update that keeps it.
var unchangedSecret = []string{"<unchanged>", "<redacted>"}

func (s *Server) putDataSource(c *searchCall, st *searchState) (*searchReply, *apiError) {
	if err := checkSearchName("data source", c.name, objectNames); err != nil {
		return nil, err
	}
	body, err := readSearchObject(c, "DataSource",
		"container", "credentials", "dataChangeDetectionPolicy", "dataDeletionDetectionPolicy", "description", "encryptionKey", "identity",
		"indexerPermissionOptions", "type")
	if err != nil {
		return nil, err
	}
	existing := st.dataSources[c.name]
	if err := searchPrecondition(c.r, existing); err != nil {
		return nil, err
	}
	if !slices.Contains(dataSourceTypes, str(body, "type")) {
		return nil, newError(http.StatusBadRequest, "", "The request is invalid. Details: dataSource.type must be one of %s.", strings.Join(dataSourceTypes, ", "))
	}
	creds, _ := body["credentials"].(map[string]any)
	conn := str(creds, "connectionString")
	if slices.Contains(unchangedSecret, conn) && existing != nil {
		conn = str(existing.body["credentials"].(map[string]any), "connectionString")
	}
	container, _ := body["container"].(map[string]any)
	if conn == "" || slices.Contains(unchangedSecret, conn) || str(container, "name") == "" {
		return nil, newError(http.StatusBadRequest, "", "The request is invalid. Details: a data source needs credentials.connectionString and container.name.")
	}
	creds = maps.Clone(creds)
	creds["connectionString"] = conn
	body["credentials"] = creds
	ds := existing
	if ds == nil {
		ds = &searchObject{}
		st.dataSources[c.name] = ds
	}
	ds.body, ds.etag = body, s.searchETag()
	view := dataSourceView{ds}.object()
	return putAnswer(c, existing == nil, &view, "datasources"), nil
}

// dataSourceView shows a data source without its connection string, as
// Search leaves it out of what it returns.
type dataSourceView struct{ *searchObject }

func (v dataSourceView) object() searchObject {
	body := maps.Clone(v.body)
	creds := maps.Clone(body["credentials"].(map[string]any))
	creds["connectionString"] = nil
	body["credentials"] = creds
	return searchObject{body: body, etag: v.etag}
}

func (s *Server) getDataSource(c *searchCall, st *searchState) (*searchReply, *apiError) {
	ds := st.dataSources[c.name]
	if ds == nil {
		return nil, searchNotFound("data source", c.name, c.svc.name)
	}
	view := dataSourceView{ds}.object()
	return &searchReply{status: http.StatusOK, etag: ds.etag, body: view.render(c, "datasources")}, nil
}

func (s *Server) deleteDataSource(c *searchCall, st *searchState) (*searchReply, *apiError) {
	ds := st.dataSources[c.name]
	if ds == nil {
		return nil, searchNotFound("data source", c.name, c.svc.name)
	}
	if err := searchPrecondition(c.r, ds); err != nil {
		return nil, err
	}
	delete(st.dataSources, c.name)
	return &searchReply{status: http.StatusNoContent}, nil
}

func (s *Server) putIndexer(c *searchCall, st *searchState) (*searchReply, *apiError) {
	if err := checkSearchName("indexer", c.name, objectNames); err != nil {
		return nil, err
	}
	body, err := readSearchObject(c, "Indexer",
		"cache", "dataSourceName", "description", "disabled", "encryptionKey", "fieldMappings", "outputFieldMappings", "parameters", "schedule",
		"skillsetName", "targetIndexName")
	if err != nil {
		return nil, err
	}
	existing := st.indexers[c.name]
	var prior *searchObject
	if existing != nil {
		prior = &existing.searchObject
	}
	if err := searchPrecondition(c.r, prior); err != nil {
		return nil, err
	}
	if str(body, "skillsetName") != "" {
		return nil, unsupported("skillsets are not supported")
	}
	source, target := str(body, "dataSourceName"), str(body, "targetIndexName")
	if source == "" || target == "" {
		return nil, newError(http.StatusBadRequest, "", "The request is invalid. Details: an indexer needs dataSourceName and targetIndexName.")
	}
	if st.dataSources[source] == nil {
		return nil, newError(http.StatusBadRequest, "", "This indexer refers to a data source '%s' that doesn't exist.", source)
	}
	if st.indexes[target] == nil {
		return nil, newError(http.StatusBadRequest, "", "This indexer refers to a target index '%s' that doesn't exist.", target)
	}
	ix := existing
	if ix == nil {
		ix = &indexer{}
		st.indexers[c.name] = ix
	}
	ix.body, ix.etag = body, s.searchETag()
	if disabled, _ := boolean(body, "disabled"); !disabled && existing == nil {
		s.startRun(ix, nil)
	}
	return putAnswer(c, existing == nil, &ix.searchObject, "indexers"), nil
}

func (s *Server) indexerNamed(c *searchCall, st *searchState) (*indexer, *apiError) {
	ix := st.indexers[c.name]
	if ix == nil {
		return nil, searchNotFound("indexer", c.name, c.svc.name)
	}
	return ix, nil
}

func (s *Server) getIndexer(c *searchCall, st *searchState) (*searchReply, *apiError) {
	ix, err := s.indexerNamed(c, st)
	if err != nil {
		return nil, err
	}
	return &searchReply{status: http.StatusOK, etag: ix.etag, body: ix.render(c, "indexers")}, nil
}

func (s *Server) deleteIndexer(c *searchCall, st *searchState) (*searchReply, *apiError) {
	ix, err := s.indexerNamed(c, st)
	if err != nil {
		return nil, err
	}
	if err := searchPrecondition(c.r, &ix.searchObject); err != nil {
		return nil, err
	}
	delete(st.indexers, c.name)
	return &searchReply{status: http.StatusNoContent}, nil
}

func (ix *indexer) running() bool { return len(ix.runs) > 0 && ix.runs[0].status == runInProgress }

// startRun starts an execution of ix, which ends in failure when set, at
// once or after the reads [WithSettleAfter] sets. The caller holds s.mu.
func (s *Server) startRun(ix *indexer, failure *apiError) {
	run := &indexerRun{status: runInProgress, start: s.now(), wait: max(s.settleAfter, 0)}
	ix.runs = append([]*indexerRun{run}, ix.runs[:min(len(ix.runs), maxRuns-1)]...)
	switch {
	case failure != nil:
		run.status, run.err, run.end = runFailure, failure.Message, s.now()
	case s.settleAfter == 0:
		s.finishRun(run)
	}
}

// finishRun ends run, if it is in progress, in success. The caller holds s.mu.
func (s *Server) finishRun(run *indexerRun) {
	if run.status == runInProgress {
		run.status, run.end = runSuccess, s.now()
	}
}

// runIndexer starts an execution of the indexer, which fails with 409 while
// one is in progress, as Search answers.
func (s *Server) runIndexer(c *searchCall, st *searchState) (*searchReply, *apiError) {
	ix, err := s.indexerNamed(c, st)
	if err != nil {
		return nil, err
	}
	if ix.running() {
		return nil, newError(http.StatusConflict, "", "Another indexer invocation is currently in progress; concurrent invocations are not allowed.")
	}
	s.startRun(ix, c.opFault)
	return &searchReply{status: http.StatusAccepted}, nil
}

// indexerStatus answers the indexer's executions, counting the read toward
// the one in progress.
func (s *Server) indexerStatus(c *searchCall, st *searchState) (*searchReply, *apiError) {
	ix, err := s.indexerNamed(c, st)
	if err != nil {
		return nil, err
	}
	if ix.running() && s.settleAfter >= 0 {
		if run := ix.runs[0]; run.wait == 0 {
			s.finishRun(run)
		} else {
			run.wait--
		}
	}
	history := make([]any, 0, len(ix.runs))
	for _, run := range ix.runs {
		history = append(history, run.render())
	}
	var last any
	if len(history) > 0 {
		last = history[0]
	}
	return &searchReply{status: http.StatusOK, body: map[string]any{
		"@odata.context": c.base + "/$metadata#Microsoft.Azure.Search.V2024_07_01.IndexerExecutionInfo",
		"name":           c.name, "status": "running", "lastResult": last, "executionHistory": history,
		"limits": map[string]any{"maxRunTime": "PT2H", "maxDocumentExtractionSize": 134217728, "maxDocumentContentCharactersToExtract": 4194304},
	}}, nil
}

func (run *indexerRun) render() map[string]any {
	m := map[string]any{
		"status": run.status, "statusDetail": nil, "errorMessage": nil, "startTime": timestamp(run.start), "endTime": nil,
		"itemsProcessed": 0, "itemsFailed": 0, "initialTrackingState": nil, "finalTrackingState": nil, "errors": []any{}, "warnings": []any{},
	}
	if run.err != "" {
		m["errorMessage"] = run.err
	}
	if !run.end.IsZero() {
		m["endTime"] = timestamp(run.end)
	}
	return m
}

// settleSearch ends the indexer executions in progress. The caller holds s.mu.
func (s *Server) settleSearch(st *searchState) {
	for _, ix := range st.indexers {
		if ix.running() {
			s.finishRun(ix.runs[0])
		}
	}
}

func (s *Server) indexOf(c *searchCall, st *searchState) (*searchIndex, *apiError) {
	idx := st.indexes[c.name]
	if idx == nil {
		return nil, searchNotFound("index", c.name, c.svc.name)
	}
	return idx, nil
}

// indexDocuments applies a batch of upload, merge, mergeOrUpload and delete
// actions, answering 207 when any of them fails.
func (s *Server) indexDocuments(c *searchCall, st *searchState) (*searchReply, *apiError) {
	idx, err := s.indexOf(c, st)
	if err != nil {
		return nil, err
	}
	body, err := readObject(c.r)
	if err != nil {
		return nil, newError(http.StatusBadRequest, "", "The request is invalid. Details: %s", err.Message)
	}
	actions, err := objects(body, "value")
	if err != nil {
		return nil, newError(http.StatusBadRequest, "", "The request is invalid. Details: %s", err.Message)
	}
	if len(actions) > maxIndexBatch {
		return nil, newError(http.StatusRequestEntityTooLarge, "", "The request is too large. A batch can hold at most %d actions.", maxIndexBatch)
	}
	for i, a := range actions {
		for k := range a {
			if k != "@search.action" && idx.fields[k] == nil {
				return nil, newError(http.StatusBadRequest, "",
					"The request is invalid. Details: The property '%s' does not exist on type 'search.documentFields'. Make sure to only use property names that are defined by the type.", k)
			}
		}
		if key, _ := a[idx.key].(string); key == "" {
			return nil, newError(http.StatusBadRequest, "", "The request is invalid. Details: actions : %d: Document key cannot be missing or empty.", i)
		}
		if !slices.Contains([]string{"", "upload", "merge", "mergeOrUpload", "delete"}, str(a, "@search.action")) {
			return nil, newError(http.StatusBadRequest, "", "The request is invalid. Details: actions : %d: unknown @search.action '%s'.", i, str(a, "@search.action"))
		}
	}
	status := http.StatusOK
	results := make([]any, 0, len(actions))
	for _, a := range actions {
		key := a[idx.key].(string)
		doc := maps.Clone(a)
		delete(doc, "@search.action")
		code, msg := http.StatusOK, ""
		existing := idx.docs[key]
		switch cmp.Or(str(a, "@search.action"), "upload") {
		case "upload":
			if existing == nil {
				code = http.StatusCreated
			}
			idx.docs[key] = doc
		case "merge", "mergeOrUpload":
			switch {
			case existing != nil:
				maps.Copy(existing, doc)
			case str(a, "@search.action") == "merge":
				code, msg = http.StatusNotFound, "Document not found."
			default:
				code, idx.docs[key] = http.StatusCreated, doc
			}
		case "delete":
			delete(idx.docs, key)
		}
		result := map[string]any{"key": key, "status": code < 300, "errorMessage": nil, "statusCode": code}
		if msg != "" {
			result["errorMessage"], status = msg, http.StatusMultiStatus
		}
		results = append(results, result)
	}
	return &searchReply{status: status, body: map[string]any{"value": results}}, nil
}

// searchParams are the query parameters a document search takes.
var searchParams = []string{"api-version", "search", "$select", "$top", "$skip", "$count"}

// searchDocuments lists the index's documents, in key order, for a search
// of * or none.
func (s *Server) searchDocuments(c *searchCall, st *searchState) (*searchReply, *apiError) {
	idx, err := s.indexOf(c, st)
	if err != nil {
		return nil, err
	}
	for k := range c.query {
		if !slices.Contains(searchParams, k) {
			return nil, unsupported("the search parameter %s is not supported", k)
		}
	}
	if q := c.query.Get("search"); q != "" && q != "*" {
		return nil, unsupported("full-text search is not supported; search for * or leave search out")
	}
	top, err := searchInt(c, "$top", 50)
	if err != nil {
		return nil, err
	}
	skip, err := searchInt(c, "$skip", 0)
	if err != nil {
		return nil, err
	}
	var selected []string
	if sel := c.query.Get("$select"); sel != "" && sel != "*" {
		for f := range strings.SplitSeq(sel, ",") {
			f = strings.TrimSpace(f)
			if idx.fields[f] == nil {
				return nil, newError(http.StatusBadRequest, "", "Invalid expression: Could not find a property named '%s' on type 'search.document'.\r\nParameter name: $select", f)
			}
			selected = append(selected, f)
		}
	}
	keys := slices.Sorted(maps.Keys(idx.docs))
	body := map[string]any{"@odata.context": c.base + "/indexes('" + c.name + "')/$metadata#docs(*)"}
	if c.query.Get("$count") == "true" {
		body["@odata.count"] = len(keys)
	}
	keys = keys[min(skip, len(keys)):]
	keys = keys[:min(top, len(keys))]
	value := make([]any, 0, len(keys))
	for _, k := range keys {
		doc := idx.docs[k]
		hit := map[string]any{"@search.score": 1.0}
		for name, field := range idx.fields {
			if retrievable, set := boolean(field, "retrievable"); set && !retrievable {
				continue
			}
			if selected != nil && !slices.Contains(selected, name) {
				continue
			}
			if v, ok := doc[name]; ok {
				hit[name] = v
			}
		}
		value = append(value, hit)
	}
	body["value"] = value
	return &searchReply{status: http.StatusOK, body: body}, nil
}

func searchInt(c *searchCall, name string, def int) (int, *apiError) {
	v := c.query.Get(name)
	if v == "" {
		return def, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 0 {
		return 0, newError(http.StatusBadRequest, "", "Invalid expression: %s must be a non-negative integer.", name)
	}
	return n, nil
}
