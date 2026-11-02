// Package serviceusage is a stand-in for the Service Usage API
// (serviceusage.googleapis.com v1), for testing code that enables the APIs a
// project needs through the official client libraries.
//
// [Server] is an [http.Handler] that keeps which services each project has
// enabled, and the long-running operations that change them, in memory and
// answers the REST calls the Go client library makes. Serve it with
// [net/http/httptest] and point a client at it with
// serviceusage.NewRESTClient, option.WithEndpoint(srv.URL) and
// option.WithoutAuthentication(). A [fault.Injector] makes chosen calls fail,
// including after they took effect, so tests can check that code which
// retries converges.
//
// It covers:
//
//   - services: enable, disable, get, list;
//   - operations: get.
//
// Other methods, such as batchEnable and batchGet, answer 404. Where it
// differs from Service Usage:
//
//   - Projects are not created through the API: a test seeds them with
//     [WithProject], and a project starts with no service enabled, where a
//     new project has some enabled by default. A service is named
//     projects/{project}/services/{service}, where {project} is the project
//     number or ID, and the server names it by number, as the API does. A
//     project the server does not hold answers 403 PERMISSION_DENIED, as the
//     API answers for a project that does not exist or that the caller cannot
//     see. Folders and organizations are not supported and answer 404.
//   - The services a project can enable are the server's catalog: the
//     services squall stands in for and a few common ones, and those
//     [WithService] adds. Another service answers 403 PERMISSION_DENIED with
//     reason SERVICE_CONFIG_NOT_FOUND_OR_PERMISSION_DENIED, as the API does
//     for a service that does not exist. A service's config holds only its
//     name and title.
//   - Changes finish before the call that makes them returns, so the
//     operation it answers with is already done. Enabling a service that is
//     already enabled succeeds with the done operation
//     operations/noop.DONE_OPERATION, as the API does. Disabling a service
//     that is not enabled is refused with 400 FAILED_PRECONDITION, as the API
//     documents. Dependencies between services are not modeled: enabling one
//     enables no other, and disableDependentServices and
//     checkIfServiceHasUsage are accepted and ignored.
//   - list takes the filters state:ENABLED and state:DISABLED, or none, and
//     answers in a single page.
//   - Request fields are named in lowerCamelCase, as the client sends them;
//     unknown fields are rejected with 400. The discovery document's standard
//     parameters are accepted and ignored, and states are written as numbers
//     with $alt=json;enum-encoding=int. Error messages are the stand-in's
//     own, except those quoted here.
//
// Fault method names are the API's method IDs, such as
// "serviceusage.services.enable". A call's resource is its path below /v1/
// without its verb, such as "projects/my-project/services/bigquery.googleapis.com",
// or the operation's name. [fault.FailOperation] ends an enable or disable's
// operation in an error without the change taking effect.
//
// [WithPolicy] grants the caller a set of IAM permissions and refuses a call
// that needs one it lacks with 403 PERMISSION_DENIED, before any fault:
// serviceusage.services.enable, serviceusage.services.disable,
// serviceusage.services.get, serviceusage.services.list and
// serviceusage.operations.get.
//
// A lost response aborts the handler with [http.ErrAbortHandler] after
// sending the response header, so the [http.Server] serving it closes the
// connection. Calling ServeHTTP directly propagates that panic.
package serviceusage

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"

	"github.com/parallelworks/squall/access"
	"github.com/parallelworks/squall/fault"
	"github.com/parallelworks/squall/internal/stub"
)

const (
	maxBody       = 1 << 20
	maxOperations = 10_000

	service       = "serviceusage.googleapis.com"
	noopOperation = "operations/noop.DONE_OPERATION"

	stateDisabled = "DISABLED"
	stateEnabled  = "ENABLED"
)

// Service is a service's state in a project, in the API's JSON shape.
type Service struct {
	// Name is projects/{number}/services/{service}.
	Name string `json:"name"`
	// Parent is projects/{number}.
	Parent string        `json:"parent"`
	Config ServiceConfig `json:"config"`
	// State is ENABLED or DISABLED.
	State string `json:"state"`
}

// ServiceConfig is the part of a service's configuration the server keeps.
type ServiceConfig struct {
	Name  string `json:"name"`
	Title string `json:"title,omitzero"`
}

// Server is a stand-in for the Service Usage API, safe for concurrent use.
// Create one with [New].
type Server struct {
	mux    *http.ServeMux
	faults *fault.Injector
	policy *access.Policy
	// catalog maps the services a project can enable to their titles.
	catalog map[string]string
	// numbers maps project numbers to IDs, and ids IDs to numbers.
	numbers, ids map[string]string

	mu         sync.Mutex
	nextID     uint64
	enabled    map[string]map[string]bool
	operations map[string]*operation
	opOrder    []string
	opNext     int
}

// defaultCatalog holds the services squall stands in for and a few common ones.
var defaultCatalog = map[string]string{
	"bigquery.googleapis.com":             "BigQuery API",
	"cloudresourcemanager.googleapis.com": "Cloud Resource Manager API",
	"compute.googleapis.com":              "Compute Engine API",
	"container.googleapis.com":            "Kubernetes Engine API",
	"iam.googleapis.com":                  "Identity and Access Management (IAM) API",
	"logging.googleapis.com":              "Cloud Logging API",
	"lustre.googleapis.com":               "Google Cloud Managed Lustre API",
	"monitoring.googleapis.com":           "Cloud Monitoring API",
	"servicenetworking.googleapis.com":    "Service Networking API",
	"serviceusage.googleapis.com":         "Service Usage API",
	"storage.googleapis.com":              "Cloud Storage API",
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

// WithProject seeds a project with ID id and number number, with no service
// enabled.
func WithProject(id string, number int64) Option {
	return func(s *Server) {
		n := strconv.FormatInt(number, 10)
		s.numbers[n], s.ids[id] = id, n
	}
}

// WithService adds the service name, such as example.googleapis.com, with
// title title to the services a project can enable.
func WithService(name, title string) Option {
	return func(s *Server) { s.catalog[name] = title }
}

// New returns a server holding the projects its options seed.
func New(opts ...Option) *Server {
	s := &Server{
		mux:        http.NewServeMux(),
		catalog:    maps.Clone(defaultCatalog),
		numbers:    map[string]string{},
		ids:        map[string]string{},
		enabled:    map[string]map[string]bool{},
		operations: map[string]*operation{},
	}
	for _, opt := range opts {
		opt(s)
	}
	s.registerRoutes()
	return s
}

// ServeHTTP answers a Service Usage REST call.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mux.ServeHTTP(w, r)
}

// project resolves ref, a project number or ID, to the project's ID and number.
func (s *Server) project(ref string) (id, number string, ok bool) {
	if id, ok := s.numbers[ref]; ok {
		return id, ref, true
	}
	number, ok = s.ids[ref]
	return ref, number, ok
}

// EnableService enables the service name in project, its number or ID, as
// a test's setup.
func (s *Server) EnableService(project, name string) error {
	id, _, ok := s.project(project)
	if !ok {
		return fmt.Errorf("no project %s", project)
	}
	if _, ok := s.catalog[name]; !ok {
		return fmt.Errorf("no service %s", name)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.enable(id, name)
	return nil
}

// enable marks name enabled in the project with ID id. The caller holds s.mu.
func (s *Server) enable(id, name string) {
	if s.enabled[id] == nil {
		s.enabled[id] = map[string]bool{}
	}
	s.enabled[id][name] = true
}

// EnabledServices returns the names of the services enabled in project, its
// number or ID, in order.
func (s *Server) EnabledServices(project string) []string {
	id, _, ok := s.project(project)
	if !ok {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Sorted(maps.Keys(s.enabled[id]))
}

// LookupService returns the service name in project, its number or ID, and
// false for a project the server does not hold or a service it does not
// know.
func (s *Server) LookupService(project, name string) (Service, bool) {
	id, number, ok := s.project(project)
	if _, known := s.catalog[name]; !ok || !known {
		return Service{}, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.service(id, number, name), true
}

// service is name's state in a project. The caller holds s.mu.
func (s *Server) service(id, number, name string) Service {
	state := stateDisabled
	if s.enabled[id][name] {
		state = stateEnabled
	}
	parent := "projects/" + number
	return Service{Name: parent + "/services/" + name, Parent: parent, Config: ServiceConfig{Name: name, Title: s.catalog[name]}, State: state}
}

const (
	methodEnable  = "serviceusage.services.enable"
	methodDisable = "serviceusage.services.disable"
	methodGet     = "serviceusage.services.get"
	methodList    = "serviceusage.services.list"
	methodOpGet   = "serviceusage.operations.get"
)

// permissions are from the Service Usage access control reference.
var permissions = map[string]string{
	methodEnable:  "serviceusage.services.enable",
	methodDisable: "serviceusage.services.disable",
	methodGet:     "serviceusage.services.get",
	methodList:    "serviceusage.services.list",
	methodOpGet:   "serviceusage.operations.get",
}

// commonParams are the discovery document's standard parameters, and the $ spellings the client sends.
var commonParams = []string{
	"$.xgafv", "access_token", "alt", "callback", "fields", "key", "oauth_token", "prettyPrint", "quotaUser", "upload_protocol", "uploadType",
	"$alt", "$fields", "$prettyPrint",
}

// call is one request.
type call struct {
	*http.Request
	// project is the project as the request names it, and name the service.
	project, name string
	intEnums      bool
}

// denied is what the call answers when the caller may not make it, or names a project the server does not hold.
func (c *call) denied(method string) *apiError {
	perm := permissions[method]
	switch method {
	case methodEnable:
		return permissionDenied(perm, "enable service ["+c.name+"]")
	case methodDisable:
		return permissionDenied(perm, "disable service ["+c.name+"]")
	case methodGet:
		return permissionDenied(perm, "get service ["+c.name+"]")
	case methodList:
		return permissionDenied(perm, "list services for consumer container [projects/"+c.project+"]")
	}
	return permissionDenied(perm, "get operation ["+c.PathValue("operation")+"]")
}

// handler answers one method; a non-nil opFault fails the operation it
// starts without the call taking effect.
type handler func(c *call, opFault *apiError) (any, *apiError)

func (s *Server) registerRoutes() {
	const services = "/v1/projects/{project}/services"
	s.mux.HandleFunc("GET "+services, func(w http.ResponseWriter, r *http.Request) {
		s.serve(w, r, "", methodList, s.list, "filter", "pageSize", "pageToken")
	})
	s.mux.HandleFunc("GET "+services+"/{service}", func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.PathValue("service"), ":") {
			writeError(w, unimplemented(r))
			return
		}
		s.serve(w, r, r.PathValue("service"), methodGet, s.get)
	})
	s.mux.HandleFunc("POST "+services+"/{service}", func(w http.ResponseWriter, r *http.Request) {
		name, verb, _ := strings.Cut(r.PathValue("service"), ":")
		switch verb {
		case "enable":
			s.serve(w, r, name, methodEnable, s.enableService)
		case "disable":
			s.serve(w, r, name, methodDisable, s.disableService)
		default:
			writeError(w, unimplemented(r))
		}
	})
	s.mux.HandleFunc("GET /v1/operations/{operation...}", func(w http.ResponseWriter, r *http.Request) {
		s.serve(w, r, "", methodOpGet, s.getOperation)
	})
	s.mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		writeError(w, unimplemented(r))
	})
}

func (s *Server) serve(w http.ResponseWriter, r *http.Request, name, method string, h handler, params ...string) {
	if name == "" && method != methodList && method != methodOpGet {
		writeError(w, unimplemented(r))
		return
	}
	c := &call{Request: r, project: r.PathValue("project"), name: name}
	if err := c.checkParams(params); err != nil {
		writeError(w, err)
		return
	}
	resource := strings.TrimPrefix(r.URL.Path, "/v1/")
	if i := strings.LastIndex(resource, ":"); i > strings.LastIndex(resource, "/") {
		resource = resource[:i]
	}
	if s.policy != nil {
		if missing := s.policy.Check(access.Call{Method: method, Resource: resource, Needs: []string{permissions[method]}}); missing != nil {
			writeError(w, c.denied(method))
			return
		}
	}
	f, _ := s.faults.Decide(fault.Call{Method: method, Resource: resource})
	startsOperation := method == methodEnable || method == methodDisable
	reject := f.Mode == fault.ModeReject || f.Mode == fault.ModeFailOperation && !startsOperation
	stub.Serve(w, r, f, reject, "application/json; charset=UTF-8", func() { writeError(w, faultError(f)) }, func() (func(), bool) {
		var opFault *apiError
		if f.Mode == fault.ModeFailOperation {
			opFault = faultError(f)
		}
		body, err := h(c, opFault)
		if err == errNotHeld {
			err = c.denied(method)
		}
		if err != nil {
			return func() { writeError(w, err) }, false
		}
		return func() { writeJSON(w, http.StatusOK, body) }, true
	})
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
		alt := q.Get(name)
		format, opts, _ := strings.Cut(alt, ";")
		if alt != "" && format != "json" {
			return invalid("squall: %s=%s is not supported", name, alt)
		}
		switch opts {
		case "":
		case "enum-encoding=int":
			c.intEnums = true
		default:
			return invalid("squall: %s=%s is not supported", name, alt)
		}
	}
	return nil
}

// errNotHeld is the error for a project the server does not hold; serve gives it the method's message.
var errNotHeld = &apiError{Status: http.StatusForbidden, Message: "not held"}

func readJSON(r *http.Request, v any) *apiError {
	body, err := io.ReadAll(io.LimitReader(r.Body, maxBody+1))
	if err != nil {
		return invalid("reading the request body: %v", err)
	}
	if len(body) > maxBody {
		return invalid("squall: bodies are limited to %d bytes", maxBody)
	}
	if strings.TrimSpace(string(body)) == "" {
		return nil
	}
	dec := json.NewDecoder(strings.NewReader(string(body)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return invalid("Invalid JSON payload received. %v", err)
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return invalid("Invalid JSON payload received. Data after the top-level value.")
	}
	return nil
}

// target resolves the call's project and service. The caller holds s.mu.
func (s *Server) target(c *call) (id, number string, err *apiError) {
	id, number, ok := s.project(c.project)
	if !ok {
		return "", "", errNotHeld
	}
	if _, ok := s.catalog[c.name]; !ok {
		return "", "", unknownService(c.name)
	}
	return id, number, nil
}

// checkName refuses a body naming another service than the path.
func (c *call) checkName(name string) *apiError {
	if name != "" && name != "projects/"+c.project+"/services/"+c.name {
		return invalid("Invalid value for name: %q does not match the request path.", name)
	}
	return nil
}

func (s *Server) enableService(c *call, opFault *apiError) (any, *apiError) {
	var req struct {
		Name string `json:"name"`
	}
	if err := readJSON(c.Request, &req); err != nil {
		return nil, err
	}
	if err := c.checkName(req.Name); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	id, number, err := s.target(c)
	if err != nil {
		return nil, err
	}
	if opFault == nil && s.enabled[id][c.name] {
		svc := s.service(id, number, c.name)
		return (&operation{Name: noopOperation, Done: true, kind: enableResponse, svc: &svc}).wire(c.intEnums), nil
	}
	if opFault == nil {
		s.enable(id, c.name)
	}
	return s.startOperation(id, number, c.name, enableResponse, opFault).wire(c.intEnums), nil
}

func (s *Server) disableService(c *call, opFault *apiError) (any, *apiError) {
	var req struct {
		Name                     string          `json:"name"`
		DisableDependentServices bool            `json:"disableDependentServices"`
		CheckIfServiceHasUsage   json.RawMessage `json:"checkIfServiceHasUsage"`
	}
	if err := readJSON(c.Request, &req); err != nil {
		return nil, err
	}
	if err := c.checkName(req.Name); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	id, number, err := s.target(c)
	if err != nil {
		return nil, err
	}
	if !s.enabled[id][c.name] {
		return nil, failedPrecondition("Service %s is not enabled in projects/%s.", c.name, number)
	}
	if opFault == nil {
		delete(s.enabled[id], c.name)
	}
	return s.startOperation(id, number, c.name, disableResponse, opFault).wire(c.intEnums), nil
}

func (s *Server) get(c *call, _ *apiError) (any, *apiError) {
	s.mu.Lock()
	defer s.mu.Unlock()
	id, number, err := s.target(c)
	if err != nil {
		return nil, err
	}
	return wireService(s.service(id, number, c.name), c.intEnums), nil
}

func (s *Server) list(c *call, _ *apiError) (any, *apiError) {
	q := c.URL.Query()
	var want string
	switch filter := strings.Join(strings.Fields(q.Get("filter")), ""); filter {
	case "":
	case "state:ENABLED":
		want = stateEnabled
	case "state:DISABLED":
		want = stateDisabled
	default:
		return nil, invalid("squall: filter %q is not supported; use state:ENABLED or state:DISABLED", q.Get("filter"))
	}
	if v := q.Get("pageSize"); v != "" {
		if n, err := strconv.Atoi(v); err != nil || n < 0 || n > 200 {
			return nil, invalid("Invalid value for page_size: %q. It must be between 0 and 200.", v)
		}
	}
	if q.Get("pageToken") != "" {
		return nil, invalid("Invalid value for page_token: %q.", q.Get("pageToken"))
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	id, number, ok := s.project(c.project)
	if !ok {
		return nil, errNotHeld
	}
	var out struct {
		Services []any `json:"services,omitzero"`
	}
	for _, name := range slices.Sorted(maps.Keys(s.catalog)) {
		if svc := s.service(id, number, name); want == "" || svc.State == want {
			out.Services = append(out.Services, wireService(svc, c.intEnums))
		}
	}
	return out, nil
}

// wireService writes svc's state as a number when the client asks for enums that way.
func wireService(svc Service, intEnums bool) any {
	if !intEnums {
		return svc
	}
	type plain Service
	state := 1
	if svc.State == stateEnabled {
		state = 2
	}
	return struct {
		plain
		State int `json:"state"`
	}{plain(svc), state}
}
