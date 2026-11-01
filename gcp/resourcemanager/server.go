// Package resourcemanager is a stand-in for the Cloud Resource Manager API
// (cloudresourcemanager.googleapis.com v3), for testing code that looks up
// projects and reads or changes their IAM policies through the official
// client libraries.
//
// [Server] is an [http.Handler] that keeps projects and their IAM policies in
// memory and answers the REST calls the Go client library makes. Serve it
// with [net/http/httptest] and point a client at it with
// resourcemanager.NewProjectsRESTClient, option.WithEndpoint(srv.URL) and
// option.WithoutAuthentication(). A [fault.Injector] makes chosen calls fail,
// including after they took effect, so tests can check that code which
// retries converges.
//
// It covers projects: get, getIamPolicy, setIamPolicy and
// testIamPermissions. Other methods answer 404. Where it differs from Cloud
// Resource Manager:
//
//   - Projects are not created through the API: a test seeds them with
//     [WithProject] or [Server.PutProject], and they are always ACTIVE. A
//     project is named projects/{number} and can be asked for by number or
//     by ID. A project the server does not hold answers 403
//     PERMISSION_DENIED, as the API answers for a project that does not
//     exist or that the caller cannot see.
//   - A policy holds bindings, each with a role, members and an optional
//     condition. Audit configs are not supported and are rejected with 400.
//     A policy reads back as version 3 when a binding has a condition and
//     version 1 otherwise; setting a conditional binding needs version 3,
//     and reading a policy that has one needs requestedPolicyVersion 3.
//     Bindings are kept as they were set, without merging repeated roles.
//     Roles must be predefined (roles/...) or custom roles of a project or
//     organization, and members must have one of the API's member types;
//     neither is looked up. Setting a policy with a stale etag answers 409
//     ABORTED; one with no etag replaces the policy unconditionally.
//   - testIamPermissions answers with the permissions in the request that
//     the server's [access.Policy] grants, or all of them without
//     [WithPolicy], not those the project's own policy grants. It rejects a
//     wildcard or more than 100 permissions with 400, as the API does, but
//     does not check that a permission applies to projects.
//   - Request fields are named in lowerCamelCase, as the client sends them;
//     unknown fields are rejected with 400. The discovery document's
//     standard parameters, such as fields, are accepted and ignored, and
//     enums are written as numbers with $alt=json;enum-encoding=int, as the
//     client asks. Error messages are the stand-in's own, except the API's
//     permission and concurrent change messages.
//
// Fault method names are the API's method IDs, such as
// "cloudresourcemanager.projects.get". A call's resource is the project as
// the request names it, such as "projects/my-project" or "projects/123".
// [fault.FailOperation] fails a call as [fault.Reject] does, since none of
// these methods starts an operation.
//
// [WithPolicy] grants the caller a set of IAM permissions and refuses a call
// that needs one it lacks with 403 PERMISSION_DENIED, before any fault:
// resourcemanager.projects.get, resourcemanager.projects.getIamPolicy and
// resourcemanager.projects.setIamPolicy. testIamPermissions needs none.
//
// A lost response aborts the handler with [http.ErrAbortHandler] after
// sending the response header, so the [http.Server] serving it closes the
// connection. Calling ServeHTTP directly propagates that panic.
package resourcemanager

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"io"
	"maps"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/parallelworks/squall/access"
	"github.com/parallelworks/squall/fault"
	"github.com/parallelworks/squall/internal/stub"
)

const (
	maxBody = 1 << 20
	service = "cloudresourcemanager.googleapis.com"
)

// Project is a project, in the API's JSON shape.
type Project struct {
	// Name is projects/{number}.
	Name        string            `json:"name"`
	Parent      string            `json:"parent,omitzero"`
	ProjectID   string            `json:"projectId"`
	State       string            `json:"state"`
	DisplayName string            `json:"displayName,omitzero"`
	CreateTime  string            `json:"createTime"`
	Etag        string            `json:"etag"`
	Labels      map[string]string `json:"labels,omitzero"`
}

// Server is a stand-in for the Cloud Resource Manager API, safe for
// concurrent use. Create one with [New].
type Server struct {
	mux    *http.ServeMux
	faults *fault.Injector
	policy *access.Policy
	now    func() time.Time
	seeds  []Project

	mu       sync.Mutex
	nextEtag uint64
	projects map[string]*project
	// numbers maps project numbers to IDs.
	numbers map[string]string
}

type project struct {
	Project
	bindings []Binding
	etag     []byte
}

// Option configures a [Server].
type Option func(*Server)

// WithFaults makes the server ask in which calls to fail.
func WithFaults(in *fault.Injector) Option {
	return func(s *Server) { s.faults = in }
}

// WithPolicy makes the server refuse calls that need a permission p lacks,
// records what each call needed, and answers testIamPermissions with what p
// grants. Without it every call is allowed.
func WithPolicy(p *access.Policy) Option {
	return func(s *Server) { s.policy = p }
}

// WithClock sets the clock for project creation times. The default is
// time.Now.
func WithClock(now func() time.Time) Option {
	return func(s *Server) { s.now = now }
}

// WithProject seeds an active project with ID id and number number, its
// display name its ID. [New] panics if [Server.PutProject] would refuse it.
func WithProject(id string, number int64) Option {
	return func(s *Server) {
		s.seeds = append(s.seeds, Project{Name: "projects/" + strconv.FormatInt(number, 10), ProjectID: id, DisplayName: id})
	}
}

// New returns a server holding the projects its options seed.
func New(opts ...Option) *Server {
	s := &Server{
		mux:      http.NewServeMux(),
		now:      time.Now,
		projects: map[string]*project{},
		numbers:  map[string]string{},
	}
	for _, opt := range opts {
		opt(s)
	}
	for _, p := range s.seeds {
		if err := s.PutProject(p); err != nil {
			panic("squall: " + err.Error())
		}
	}
	s.registerRoutes()
	return s
}

// ServeHTTP answers a Cloud Resource Manager REST call.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mux.ServeHTTP(w, r)
}

// PutProject adds p, or replaces the project with its ID and number, keeping
// that project's IAM policy. p needs ProjectID and Name, projects/{number};
// Parent, DisplayName and Labels are optional, and the server sets State,
// CreateTime unless p has one, and Etag. It refuses an ID or number that is
// not valid, or that another project holds, and a parent that is not
// organizations/{id} or folders/{id}.
func (s *Server) PutProject(p Project) error {
	number, ok := strings.CutPrefix(p.Name, "projects/")
	if n, err := strconv.ParseInt(number, 10, 64); !ok || err != nil || n <= 0 || strconv.FormatInt(n, 10) != number {
		return fmt.Errorf("project name %q is not projects/{number}", p.Name)
	}
	if !validProjectID(p.ProjectID) {
		return fmt.Errorf("project ID %q is not 6 to 30 lowercase letters, digits or hyphens, starting with a letter and not ending with a hyphen", p.ProjectID)
	}
	if p.Parent != "" && !validParent(p.Parent) {
		return fmt.Errorf("parent %q is not organizations/{id} or folders/{id}", p.Parent)
	}
	if p.State != "" && p.State != "ACTIVE" {
		return fmt.Errorf("project state %q is not ACTIVE", p.State)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if id, ok := s.numbers[number]; ok && id != p.ProjectID {
		return fmt.Errorf("project number %s belongs to %s", number, id)
	}
	old, held := s.projects[p.ProjectID]
	if held && old.Name != p.Name {
		return fmt.Errorf("project %s is %s", p.ProjectID, old.Name)
	}
	p.State = "ACTIVE"
	if p.CreateTime == "" {
		p.CreateTime = s.now().UTC().Format(time.RFC3339Nano)
	}
	p.Labels = maps.Clone(p.Labels)
	stored := &project{Project: p}
	if held {
		stored.bindings, stored.etag = old.bindings, old.etag
	} else {
		stored.etag = s.newEtag()
	}
	stored.Etag = projectEtag(stored)
	s.projects[p.ProjectID] = stored
	s.numbers[number] = p.ProjectID
	return nil
}

func validProjectID(id string) bool {
	if len(id) < 6 || len(id) > 30 || id[0] < 'a' || id[0] > 'z' || id[len(id)-1] == '-' {
		return false
	}
	return !strings.ContainsFunc(id, func(c rune) bool {
		return (c < 'a' || c > 'z') && (c < '0' || c > '9') && c != '-'
	})
}

func validParent(parent string) bool {
	for _, kind := range []string{"organizations/", "folders/"} {
		if id, ok := strings.CutPrefix(parent, kind); ok {
			n, err := strconv.ParseUint(id, 10, 64)
			return err == nil && n > 0 && strconv.FormatUint(n, 10) == id
		}
	}
	return false
}

func projectEtag(p *project) string {
	h := fnv.New64a()
	_ = json.NewEncoder(h).Encode(p.Project)
	return fmt.Sprintf("W/%q", fmt.Sprintf("%016x", h.Sum64()))
}

// newEtag returns a policy etag no earlier policy had. The caller holds s.mu.
func (s *Server) newEtag() []byte {
	s.nextEtag++
	return binary.BigEndian.AppendUint64([]byte{0x07, 0x06}, s.nextEtag)
}

// lookup finds a project by ID or number. The caller holds s.mu.
func (s *Server) lookup(ref string) (*project, bool) {
	if id, ok := s.numbers[ref]; ok {
		ref = id
	}
	p, ok := s.projects[ref]
	return p, ok
}

// LookupProject returns the project with ID or number ref.
func (s *Server) LookupProject(ref string) (Project, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.lookup(ref)
	if !ok {
		return Project{}, false
	}
	return p.clone(), true
}

// Projects returns every project the server holds, ordered by ID.
func (s *Server) Projects() []Project {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Project, 0, len(s.projects))
	for _, id := range slices.Sorted(maps.Keys(s.projects)) {
		out = append(out, s.projects[id].clone())
	}
	return out
}

func (p *project) clone() Project {
	out := p.Project
	out.Labels = maps.Clone(p.Labels)
	return out
}

const (
	methodGet     = "cloudresourcemanager.projects.get"
	methodGetIAM  = "cloudresourcemanager.projects.getIamPolicy"
	methodSetIAM  = "cloudresourcemanager.projects.setIamPolicy"
	methodTestIAM = "cloudresourcemanager.projects.testIamPermissions"
)

// permissions are from each method's "IAM Permissions" in the REST reference.
var permissions = map[string]string{
	methodGet:    "resourcemanager.projects.get",
	methodGetIAM: "resourcemanager.projects.getIamPolicy",
	methodSetIAM: "resourcemanager.projects.setIamPolicy",
}

// commonParams are the discovery document's standard parameters, and the $ spellings the client sends.
var commonParams = []string{
	"$.xgafv", "access_token", "alt", "callback", "fields", "key", "oauth_token", "prettyPrint", "quotaUser", "upload_protocol", "uploadType",
	"$alt", "$fields", "$prettyPrint",
}

// call is one request to a project.
type call struct {
	*http.Request
	ref      string
	intEnums bool
}

func (c *call) resource() string { return "projects/" + c.ref }

type handler func(c *call) (any, *apiError)

func (s *Server) registerRoutes() {
	verbs := map[string]struct {
		method string
		h      handler
	}{
		"getIamPolicy":       {methodGetIAM, s.getIAMPolicy},
		"setIamPolicy":       {methodSetIAM, s.setIAMPolicy},
		"testIamPermissions": {methodTestIAM, s.testIAMPermissions},
	}
	s.mux.HandleFunc("GET /v3/projects/{project}", func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.PathValue("project"), ":") {
			writeError(w, unimplemented(r))
			return
		}
		s.serve(w, r, r.PathValue("project"), methodGet, s.get)
	})
	s.mux.HandleFunc("POST /v3/projects/{project}", func(w http.ResponseWriter, r *http.Request) {
		ref, verb, _ := strings.Cut(r.PathValue("project"), ":")
		v, ok := verbs[verb]
		if !ok || ref == "" {
			writeError(w, unimplemented(r))
			return
		}
		s.serve(w, r, ref, v.method, v.h)
	})
	s.mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		writeError(w, unimplemented(r))
	})
}

func (s *Server) serve(w http.ResponseWriter, r *http.Request, ref, method string, h handler) {
	c := &call{Request: r, ref: ref}
	if err := c.checkParams(); err != nil {
		writeError(w, err)
		return
	}
	if s.policy != nil && permissions[method] != "" {
		if missing := s.policy.Check(access.Call{Method: method, Resource: c.resource(), Needs: []string{permissions[method]}}); missing != nil {
			writeError(w, permissionDenied(missing[0], c.resource()))
			return
		}
	}
	f, _ := s.faults.Decide(fault.Call{Method: method, Resource: c.resource()})
	reject := f.Mode == fault.ModeReject || f.Mode == fault.ModeFailOperation
	stub.Serve(w, r, f, reject, "application/json; charset=UTF-8", func() { writeError(w, faultError(f)) }, func() (func(), bool) {
		body, err := h(c)
		if err != nil {
			if err == notHeld {
				err = permissionDenied(permissions[method], c.resource())
			}
			return func() { writeError(w, err) }, false
		}
		return func() { writeJSON(w, http.StatusOK, body) }, true
	})
}

// checkParams rejects a parameter the method does not take, as the API does.
func (c *call) checkParams() *apiError {
	q := c.URL.Query()
	for _, name := range slices.Sorted(maps.Keys(q)) {
		if !slices.Contains(commonParams, name) {
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

// notHeld is the error for a project the server does not hold; serve gives it the method's permission.
var notHeld = &apiError{Status: http.StatusForbidden, Message: "not held"}

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

func (s *Server) get(c *call) (any, *apiError) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.lookup(c.ref)
	if !ok {
		return nil, notHeld
	}
	out := p.clone()
	if !c.intEnums {
		return out, nil
	}
	type plain Project
	return struct {
		plain
		State int `json:"state"`
	}{plain(out), 1}, nil
}
