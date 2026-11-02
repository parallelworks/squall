// Package bigquery is a stand-in for the BigQuery API
// (bigquery.googleapis.com v2), for testing code that creates and deletes
// datasets through the official client libraries.
//
// [Server] is an [http.Handler] that keeps datasets in memory and answers the
// REST calls the Go client library makes. Serve it with [net/http/httptest]
// and point a client at it with bigquery.NewClient,
// option.WithEndpoint(srv.URL + "/bigquery/v2/") and
// option.WithoutAuthentication(). A [fault.Injector] makes chosen calls fail,
// including after they took effect, so tests can check that code which
// retries converges.
//
// It covers datasets: insert, get, list, patch and delete. Other methods,
// tables and jobs among them, answer 404. Where it differs from BigQuery:
//
//   - A dataset holds a friendly name, a description, labels and a location,
//     which defaults to US, as the API's does when the request names none.
//     Locations are not checked, and access entries, default table
//     expirations, encryption, collation, time travel, storage billing and
//     the API's other dataset fields are rejected with 400. A dataset's
//     access list reads back empty, where the API fills in default entries.
//   - Dataset IDs are letters, digits and underscores, up to 1024 characters,
//     and labels follow the API's rules: at most 64, keys of 1 to 63
//     lowercase letters, digits, underscores or dashes starting with a
//     letter, and values of up to 63 of the same. A dataset whose ID starts
//     with an underscore is hidden from a list unless it asks for all.
//   - Projects are not checked: any project ID in a request names a project
//     with no datasets until one is created.
//   - Tables are not served. [Server.AddTable] puts a table in a dataset so
//     a test can check that deleting it is refused with 400 resourceInUse
//     unless deleteContents is true, as the API does.
//   - A patch changes the friendly name, the description and labels, where a
//     null label value removes the label, and honors If-Match with 412.
//   - List takes no filter, and pages with maxResults and pageToken.
//   - [WithServiceUsage] refuses every call in a project where
//     bigquery.googleapis.com is not enabled in a [serviceusage.Server] with
//     403 accessNotConfigured, as the API does. The stand-in checks the
//     project the request names, where the API checks the project the
//     caller's credentials are billed to, which is usually the same. Without
//     WithServiceUsage the API is always enabled.
//   - Errors have the API's shape and reasons. Their messages are the API's
//     where it documents them or clients match on them, such as "Not found:
//     Dataset my-project:my_dataset", and the stand-in's own otherwise.
//
// Fault method names are the API's method IDs, such as
// "bigquery.datasets.insert". A call's resource is the dataset, such as
// "projects/my-project/datasets/my_dataset", the one being created for an
// insert, or "projects/my-project/datasets" for a list.
// [fault.FailOperation] fails a call as [fault.Reject] does, since none of
// these methods starts an operation.
//
// [WithPolicy] grants the caller a set of IAM permissions and refuses a call
// that needs one it lacks with 403 accessDenied, before any fault:
// bigquery.datasets.create, bigquery.datasets.get, bigquery.datasets.update
// and bigquery.datasets.delete. A delete with deleteContents of a dataset
// that holds tables also needs bigquery.tables.delete. A list needs none,
// as the API answers it with the datasets the caller can read.
//
// A lost response aborts the handler with [http.ErrAbortHandler] after
// sending the response header, so the [http.Server] serving it closes the
// connection. Calling ServeHTTP directly propagates that panic.
package bigquery

import (
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"io"
	"maps"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/parallelworks/squall/access"
	"github.com/parallelworks/squall/fault"
	"github.com/parallelworks/squall/gcp/serviceusage"
	"github.com/parallelworks/squall/internal/stub"
)

const (
	maxBody = 1 << 20
	apiBase = "https://bigquery.googleapis.com/bigquery/v2/"
	service = "bigquery.googleapis.com"
)

// Dataset is a dataset, in the API's JSON shape.
type Dataset struct {
	Kind string `json:"kind"`
	Etag string `json:"etag"`
	// ID is {project}:{dataset}.
	ID               string            `json:"id"`
	SelfLink         string            `json:"selfLink"`
	DatasetReference DatasetReference  `json:"datasetReference"`
	FriendlyName     string            `json:"friendlyName,omitzero"`
	Description      string            `json:"description,omitzero"`
	Labels           map[string]string `json:"labels,omitzero"`
	// CreationTime and LastModifiedTime are milliseconds since the epoch.
	CreationTime     int64  `json:"creationTime,string"`
	LastModifiedTime int64  `json:"lastModifiedTime,string"`
	Location         string `json:"location"`
	Type             string `json:"type"`
}

// DatasetReference names a dataset.
type DatasetReference struct {
	DatasetID string `json:"datasetId"`
	ProjectID string `json:"projectId"`
}

// Server is a stand-in for the BigQuery API, safe for concurrent use.
// Create one with [New].
type Server struct {
	mux    *http.ServeMux
	faults *fault.Injector
	policy *access.Policy
	now    func() time.Time
	usage  *serviceusage.Server

	mu       sync.Mutex
	datasets map[datasetKey]*dataset
}

type datasetKey struct{ project, id string }

type dataset struct {
	Dataset
	tables map[string]bool
}

// Option configures a [Server].
type Option func(*Server)

// WithFaults makes the server ask in which calls to fail.
func WithFaults(in *fault.Injector) Option {
	return func(s *Server) { s.faults = in }
}

// WithPolicy makes the server refuse calls that need a permission p lacks,
// and records what each call needed. Without it every call is allowed.
func WithPolicy(p *access.Policy) Option {
	return func(s *Server) { s.policy = p }
}

// WithClock sets the clock for creation and modification times. The default
// is time.Now.
func WithClock(now func() time.Time) Option {
	return func(s *Server) { s.now = now }
}

// WithServiceUsage refuses calls in a project where u does not have
// bigquery.googleapis.com enabled.
func WithServiceUsage(u *serviceusage.Server) Option {
	return func(s *Server) { s.usage = u }
}

// New returns an empty server.
func New(opts ...Option) *Server {
	s := &Server{
		mux:      http.NewServeMux(),
		now:      time.Now,
		datasets: map[datasetKey]*dataset{},
	}
	for _, opt := range opts {
		opt(s)
	}
	s.registerRoutes()
	return s
}

// ServeHTTP answers a BigQuery REST call.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mux.ServeHTTP(w, r)
}

// Datasets returns every dataset the server holds, ordered by project and ID.
func (s *Server) Datasets() []Dataset {
	s.mu.Lock()
	defer s.mu.Unlock()
	keys := slices.SortedFunc(maps.Keys(s.datasets), func(a, b datasetKey) int {
		return strings.Compare(a.project+":"+a.id, b.project+":"+b.id)
	})
	out := make([]Dataset, 0, len(keys))
	for _, k := range keys {
		out = append(out, s.datasets[k].clone())
	}
	return out
}

// LookupDataset returns the dataset id in project.
func (s *Server) LookupDataset(project, id string) (Dataset, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	d, ok := s.datasets[datasetKey{project, id}]
	if !ok {
		return Dataset{}, false
	}
	return d.clone(), true
}

// AddTable puts a table named table in the dataset id in project, which
// must exist.
func (s *Server) AddTable(project, id, table string) error {
	if table == "" {
		return errors.New("a table needs a name")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	d, ok := s.datasets[datasetKey{project, id}]
	if !ok {
		return fmt.Errorf("no dataset %s:%s", project, id)
	}
	d.tables[table] = true
	return nil
}

func (d *dataset) clone() Dataset {
	out := d.Dataset
	out.Labels = maps.Clone(d.Labels)
	return out
}

func (d *dataset) touch(now time.Time) {
	d.LastModifiedTime = now.UnixMilli()
	h := fnv.New64a()
	_ = json.NewEncoder(h).Encode(d.Dataset)
	d.Etag = fmt.Sprintf("%016x", h.Sum64())
}

const (
	methodInsert = "bigquery.datasets.insert"
	methodGet    = "bigquery.datasets.get"
	methodList   = "bigquery.datasets.list"
	methodPatch  = "bigquery.datasets.patch"
	methodDelete = "bigquery.datasets.delete"
)

// permissions are from each method's "Authorization" in the REST reference.
var permissions = map[string]string{
	methodInsert: "bigquery.datasets.create",
	methodGet:    "bigquery.datasets.get",
	methodPatch:  "bigquery.datasets.update",
	methodDelete: "bigquery.datasets.delete",
}

// commonParams are the discovery document's standard parameters, and the $ spellings the client sends.
var commonParams = []string{
	"$.xgafv", "access_token", "alt", "callback", "fields", "key", "oauth_token", "prettyPrint", "quotaUser", "upload_protocol", "uploadType", "userIp",
	"$alt", "$fields", "$prettyPrint",
}

// call is one request about a project's datasets.
type call struct {
	*http.Request
	project, dataset string
	method           string
	body             []byte
}

func (c *call) resource() string {
	if c.dataset == "" {
		return "projects/" + c.project + "/datasets"
	}
	return "projects/" + c.project + "/datasets/" + c.dataset
}

type handler func(c *call) (any, *apiError)

func (s *Server) registerRoutes() {
	const (
		datasets = "/bigquery/v2/projects/{project}/datasets"
		dataset  = datasets + "/{dataset}"
	)
	s.route("POST "+datasets, methodInsert, s.insert, "accessPolicyVersion")
	s.route("GET "+datasets, methodList, s.list, "all", "filter", "maxResults", "pageToken")
	s.route("GET "+dataset, methodGet, s.get, "accessPolicyVersion", "datasetView")
	s.route("PATCH "+dataset, methodPatch, s.patch, "accessPolicyVersion", "updateMode")
	s.route("DELETE "+dataset, methodDelete, s.delete, "deleteContents")
	s.mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		writeError(w, unimplemented(r))
	})
}

func (s *Server) route(pattern, method string, h handler, params ...string) {
	s.mux.HandleFunc(pattern, func(w http.ResponseWriter, r *http.Request) {
		c := &call{Request: r, project: r.PathValue("project"), dataset: r.PathValue("dataset"), method: method}
		if strings.Contains(c.dataset, ":") {
			writeError(w, unimplemented(r))
			return
		}
		if err := c.checkParams(params); err != nil {
			writeError(w, err)
			return
		}
		if err := c.readBody(); err != nil {
			writeError(w, err)
			return
		}
		if method == methodInsert {
			var ref struct {
				DatasetReference *DatasetReference `json:"datasetReference"`
			}
			if json.Unmarshal(c.body, &ref) == nil && ref.DatasetReference != nil {
				c.dataset = ref.DatasetReference.DatasetID
			}
		}
		if err := s.checkEnabled(c.project); err != nil {
			writeError(w, err)
			return
		}
		if s.policy != nil && permissions[method] != "" {
			if missing := s.policy.Check(access.Call{Method: method, Resource: c.resource(), Needs: []string{permissions[method]}}); missing != nil {
				writeError(w, c.denied(missing[0]))
				return
			}
		}
		f, _ := s.faults.Decide(fault.Call{Method: method, Resource: c.resource()})
		reject := f.Mode == fault.ModeReject || f.Mode == fault.ModeFailOperation
		stub.Serve(w, r, f, reject, "application/json; charset=UTF-8", func() { writeError(w, faultError(f)) }, func() (func(), bool) {
			body, err := h(c)
			if err != nil {
				return func() { writeError(w, err) }, false
			}
			if body == nil {
				return func() { w.WriteHeader(http.StatusNoContent) }, true
			}
			return func() { writeJSON(w, http.StatusOK, body) }, true
		})
	})
}

// denied refuses the call for lacking perm.
func (c *call) denied(perm string) *apiError {
	if c.method == methodInsert {
		return accessDenied(perm, c.project, "")
	}
	return accessDenied(perm, c.project, c.dataset)
}

// checkEnabled refuses a call in a project where the API is not enabled.
func (s *Server) checkEnabled(project string) *apiError {
	if s.usage == nil {
		return nil
	}
	svc, ok := s.usage.LookupService(project, service)
	if ok && svc.State == "ENABLED" {
		return nil
	}
	if ok {
		project = strings.TrimPrefix(svc.Parent, "projects/")
	}
	return notEnabled(project)
}

// checkParams rejects a parameter the method does not take, as the API does.
func (c *call) checkParams(params []string) *apiError {
	q := c.URL.Query()
	for _, name := range slices.Sorted(maps.Keys(q)) {
		if !slices.Contains(commonParams, name) && !slices.Contains(params, name) {
			return invalid("Invalid JSON payload received. Unknown name %q: Cannot bind query parameter. Field %q could not be found in request message.", name, name)
		}
	}
	for _, name := range []string{"$alt", "alt"} {
		if alt := q.Get(name); alt != "" && alt != "json" {
			return invalid("squall: %s=%s is not supported", name, alt)
		}
	}
	return nil
}

func (c *call) readBody() *apiError {
	if c.Method != http.MethodPost && c.Method != http.MethodPatch {
		return nil
	}
	body, err := io.ReadAll(io.LimitReader(c.Body, maxBody+1))
	if err != nil {
		return invalid("reading the request body: %v", err)
	}
	if len(body) > maxBody {
		return invalid("squall: bodies are limited to %d bytes", maxBody)
	}
	c.body = body
	return nil
}

// decode reads the call's body into v, refusing fields v does not have.
func (c *call) decode(v any) *apiError {
	dec := json.NewDecoder(strings.NewReader(string(c.body)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		if field, ok := strings.CutPrefix(err.Error(), "json: unknown field "); ok {
			return invalid("squall: dataset field %s is not supported", field)
		}
		return invalid("Invalid JSON payload received. %v", err)
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return invalid("Invalid JSON payload received. Data after the top-level value.")
	}
	return nil
}
