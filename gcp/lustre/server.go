// Package lustre is a stand-in for the Google Cloud Managed Lustre API
// (lustre.googleapis.com v1), for testing code that calls it through the
// official client libraries.
//
// [Server] is an [http.Handler] that keeps Managed Lustre instances and their
// long-running operations in memory and answers the REST calls the Go client
// library makes. Serve it with [net/http/httptest] and point a client at it
// with lustre.NewRESTClient, option.WithEndpoint(srv.URL) and
// option.WithoutAuthentication(). A [fault.Injector] makes chosen calls fail,
// including after they took effect, so tests can check that code which
// retries converges.
//
// It covers:
//
//   - instances: create, get, list, patch, delete;
//   - operations: get, list, delete, cancel.
//
// Other methods answer 404. Where it differs from Managed Lustre:
//
//   - Changes finish before the call that makes them returns, so the
//     operation it answers with is already done and the SDK's Wait returns
//     at once. [WithSettleAfter] holds them in progress, with the operation
//     not done, for more reads of the instance or its operation, or until
//     [Server.Settle]: a new instance is CREATING and a deleted one DELETING
//     until it goes. A patched one keeps its state, since the API deprecates
//     UPDATING. A change to an instance with another in progress finishes
//     that one first. The v1 API has no
//     FAILED state: a create that fails ends its operation in an error and
//     leaves no instance, which [fault.FailOperation] does.
//   - A call that repeats an earlier call's requestId in the same project,
//     for the same method, gets that call's operation back without taking
//     effect again. The server keeps the last 10,000 operations.
//   - Instance IDs, filesystem names, networks, labels, access rules and
//     capacity follow the API's documented rules: capacityGib must lie in
//     its throughput tier's range and be a multiple of the tier's step,
//     which grows above a threshold, and a Dynamic tier instance takes
//     perUnitStorageThroughput 0 or none. Locations are not checked, and
//     networks, KMS keys, placement policies and quotas are not looked up.
//     A mount point's address comes from 10.0.0.0/8 and is set once the
//     instance is ACTIVE. Access rule ranges are CIDR ranges or addresses
//     that must not overlap, and a maintenance policy has one weekly window
//     and at most one exclusion window. Options are stored as the API reads
//     them, so an enum sent by name or number reads back the same, as names
//     or, with enum-encoding=int, numbers. Fields the server does not know,
//     at any depth, are rejected with 400.
//   - A patch changes only labels, description, gkeSupportEnabled and
//     capacityGib, which may only grow and, from the smaller step, not past
//     its threshold. The fields a patch names, by its updateMask, by * for
//     all, or with no mask by those the body holds, may include output-only
//     fields, which are ignored, and others whose value is unchanged, as
//     AIP-203 directs; any other change, or a nested path, is rejected with
//     400. A patched instance returns to the state it was in.
//   - List filters follow AIP-160: field = value and field != value on name,
//     state, filesystem, network, description and labels.KEY, labels.KEY:*
//     and labels:KEY for a label's presence, joined by AND, OR, NOT, - and
//     parentheses, with OR binding tighter than AND. Lists are ordered by
//     name; orderBy takes name and name desc. Anything else, such as a
//     wildcard or a comparison with <, is rejected with 400 rather than
//     ignored. The location - lists every location.
//   - Canceling an operation records the request and changes nothing else,
//     which the API allows. The discovery document's standard parameters,
//     such as fields or callback, are accepted and ignored: responses are
//     always whole JSON. Other parameters a method does not take are
//     rejected with 400. Error messages are the stand-in's own.
//
// Fault method names are the API's method IDs, such as
// "lustre.projects.locations.instances.create". A call's resource is its
// path below /v1/, such as "projects/p/locations/us-central1-a/instances/fs";
// a create names the instance it creates, and a list the collection.
//
// [WithPolicy] grants the caller a set of IAM permissions and refuses a call
// that needs one it lacks with 403 PERMISSION_DENIED, before any fault, as
// each method's reference lists them: lustre.instances.create, get, list,
// update and delete, and lustre.operations.get, list, delete and cancel.
// Permissions are not checked against the resource they act on: a grant
// covers every project.
//
// A lost response aborts the handler with [http.ErrAbortHandler] after
// sending the response header, so the [http.Server] serving it closes the
// connection. Calling ServeHTTP directly propagates that panic.
package lustre

import (
	"bytes"
	"cmp"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/netip"
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
	maxList = 1000
)

// Server is a stand-in for the Managed Lustre API, safe for concurrent use. Create one with [New].
type Server struct {
	mux    *http.ServeMux
	faults *fault.Injector
	policy *access.Policy
	now    func() time.Time

	settleAfter int

	mu         sync.Mutex
	nextID     uint64
	nextIP     netip.Addr
	instances  map[string]*instance
	operations map[string]*operation
	opOrder    []string
	opNext     int
	requests   map[string]*operation
	pending    map[*operation]struct{}
}

// Option configures a [Server].
type Option func(*Server)

// WithFaults makes the server ask in which calls to fail.
func WithFaults(in *fault.Injector) Option {
	return func(s *Server) { s.faults = in }
}

// WithClock sets the clock for instance and operation times. The default is time.Now.
func WithClock(now func() time.Time) Option {
	return func(s *Server) { s.now = now }
}

// WithSettleAfter holds each change in progress for n reads of the instance or its operation (0, the default, finishes it at once; a negative n waits for [Server.Settle]).
func WithSettleAfter(n int) Option {
	return func(s *Server) { s.settleAfter = n }
}

// New returns an empty server.
func New(opts ...Option) *Server {
	s := &Server{
		mux:        http.NewServeMux(),
		now:        time.Now,
		nextID:     1,
		nextIP:     netip.MustParseAddr("10.0.0.2"),
		instances:  map[string]*instance{},
		operations: map[string]*operation{},
		requests:   map[string]*operation{},
		pending:    map[*operation]struct{}{},
	}
	for _, opt := range opts {
		opt(s)
	}
	s.registerRoutes()
	return s
}

// ServeHTTP answers a Managed Lustre REST call.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mux.ServeHTTP(w, r)
}

func (s *Server) registerRoutes() {
	const (
		location  = "/v1/projects/{project}/locations/{location}"
		instances = location + "/instances"
		instance  = instances + "/{instance}"
		ops       = location + "/operations"
		op        = ops + "/{operation}"
	)
	s.route("POST "+instances, "lustre.projects.locations.instances.create", "", s.createInstance, createTarget,
		"instanceId", "requestId")
	s.route("GET "+instances, "lustre.projects.locations.instances.list", "", s.listInstances, nil,
		"pageSize", "pageToken", "filter", "orderBy")
	s.route("GET "+instance, "lustre.projects.locations.instances.get", "", s.getInstance, nil)
	s.route("PATCH "+instance, "lustre.projects.locations.instances.patch", "", s.patchInstance, nil,
		"updateMask", "requestId")
	s.route("DELETE "+instance, "lustre.projects.locations.instances.delete", "", s.deleteInstance, nil,
		"requestId", "force")
	s.route("GET "+ops, "lustre.projects.locations.operations.list", "", s.listOperations, nil,
		"pageSize", "pageToken", "filter", "returnPartialSuccess")
	s.route("GET "+op, "lustre.projects.locations.operations.get", "", s.getOperation, nil)
	s.route("DELETE "+op, "lustre.projects.locations.operations.delete", "", s.deleteOperation, nil)
	s.route("POST "+op, "lustre.projects.locations.operations.cancel", ":cancel", s.cancelOperation, nil)
	s.mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		writeError(w, unimplemented(r))
	})
}

// handler answers one method; a non-nil opFault fails the operation it starts without the call taking effect.
type handler func(c *call, opFault *apiError) (any, *apiError)

type call struct {
	*http.Request
	resource string
	intEnums bool
}

// operationMethods start an operation, so only they can fault with [fault.ModeFailOperation].
var operationMethods = []string{
	"lustre.projects.locations.instances.create",
	"lustre.projects.locations.instances.patch",
	"lustre.projects.locations.instances.delete",
}

// commonParams are the discovery document's standard parameters, and the $ spellings the client sends.
var commonParams = []string{
	"$.xgafv", "access_token", "alt", "callback", "fields", "key", "oauth_token", "prettyPrint", "quotaUser", "upload_protocol", "uploadType",
	"$alt", "$fields", "$prettyPrint",
}

// route registers h; verb, such as ":cancel", is a suffix ServeMux cannot match on its own.
func (s *Server) route(pattern, method, verb string, h handler, target func(*call) string, params ...string) {
	startsOperation := slices.Contains(operationMethods, method)
	s.mux.HandleFunc(pattern, func(w http.ResponseWriter, r *http.Request) {
		path, ok := strings.CutSuffix(strings.TrimPrefix(r.URL.Path, "/v1/"), verb)
		if !ok || strings.Contains(path, ":") {
			writeError(w, unimplemented(r))
			return
		}
		c := &call{Request: r, resource: path}
		if err := c.checkParams(method, params); err != nil {
			writeError(w, err)
			return
		}
		resource := path
		if target != nil {
			resource = target(c)
		}
		if s.policy != nil {
			if missing := s.policy.Check(access.Call{Method: method, Resource: resource, Needs: basePermissions[method]}); missing != nil {
				writeError(w, permissionDenied(missing, iamResource(method, path)))
				return
			}
		}
		f, _ := s.faults.Decide(fault.Call{Method: method, Resource: resource})
		reject := f.Mode == fault.ModeReject || f.Mode == fault.ModeFailOperation && !startsOperation
		stub.Serve(w, r, f, reject, "application/json; charset=UTF-8", func() { writeError(w, faultError(f)) }, func() (func(), bool) {
			var opFault *apiError
			if f.Mode == fault.ModeFailOperation {
				opFault = faultError(f)
			}
			body, err := h(c, opFault)
			if err != nil {
				return func() { writeError(w, err) }, false
			}
			return func() { writeJSON(w, http.StatusOK, body) }, true
		})
	})
}

// checkParams rejects an unknown parameter as the API does, rather than ignoring it.
func (c *call) checkParams(method string, params []string) *apiError {
	q := c.URL.Query()
	for name := range q {
		if !slices.Contains(params, name) && !slices.Contains(commonParams, name) {
			return invalid("Invalid JSON payload received. Unknown name %q: Cannot bind query parameter. Field %q could not be found in request message.", name, name)
		}
	}
	for _, name := range []string{"$alt", "alt"} {
		alt := q.Get(name)
		format, opts, _ := strings.Cut(alt, ";")
		if alt != "" && format != "json" {
			return invalid("squall: %s=%s is not supported by %s", name, alt, method)
		}
		switch opts {
		case "":
		case "enum-encoding=int":
			c.intEnums = true
		default:
			return invalid("squall: %s=%s is not supported by %s", name, alt, method)
		}
	}
	return nil
}

// createTarget names the instance a create makes, so a fault rule can target one ID.
func createTarget(c *call) string {
	if id := c.URL.Query().Get("instanceId"); id != "" {
		return c.resource + "/" + id
	}
	return c.resource
}

func (c *call) parent() string {
	return "projects/" + c.PathValue("project") + "/locations/" + c.PathValue("location")
}

// requestKey scopes a requestId to its project and method, as compute does.
func (c *call) requestKey(method string) (string, *apiError) {
	id := c.URL.Query().Get("requestId")
	if id == "" {
		return "", nil
	}
	if !validUUID(id) {
		return "", invalid("Invalid value for request_id: %q. The request ID must be a valid UUID other than the zero UUID.", id)
	}
	return c.PathValue("project") + " " + method + " " + id, nil
}

func validUUID(id string) bool {
	if len(id) != 36 || id == "00000000-0000-0000-0000-000000000000" {
		return false
	}
	for i, ch := range []byte(id) {
		switch i {
		case 8, 13, 18, 23:
			if ch != '-' {
				return false
			}
		default:
			if !strings.ContainsRune("0123456789abcdefABCDEF", rune(ch)) {
				return false
			}
		}
	}
	return true
}

func decodeStrict(r *http.Request, v any) *apiError {
	body, err := io.ReadAll(io.LimitReader(r.Body, maxBody+1))
	if err != nil {
		return invalid("reading the request body: %v", err)
	}
	if len(body) > maxBody {
		return invalid("squall: bodies are limited to %d bytes", maxBody)
	}
	if len(bytes.TrimSpace(body)) == 0 {
		body = []byte("{}")
	}
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		if name, ok := strings.CutPrefix(err.Error(), "json: unknown field "); ok {
			return invalid("Invalid JSON payload received. Unknown name %s: Cannot find field.", name)
		}
		return invalid("Invalid JSON payload received. %v", err)
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return invalid("Invalid JSON payload received. Data after the top-level value.")
	}
	return nil
}

// pageOf pages by the last key returned, so a page survives deletes between calls.
func pageOf[T any](c *call, items []T, key func(T) string, desc bool) ([]T, string, *apiError) {
	q := c.URL.Query()
	limit := maxList
	if v := q.Get("pageSize"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			return nil, "", invalid("Invalid value for page_size: %q", v)
		}
		if n > 0 {
			limit = min(n, maxList)
		}
	}
	if token := q.Get("pageToken"); token != "" {
		after, err := base64.RawURLEncoding.DecodeString(token)
		if err != nil || len(after) == 0 {
			return nil, "", invalid("Invalid page token: %q", token)
		}
		i := 0
		for i < len(items) && !past(key(items[i]), string(after), desc) {
			i++
		}
		items = items[i:]
	}
	if len(items) <= limit {
		return items, "", nil
	}
	items = items[:limit]
	return items, base64.RawURLEncoding.EncodeToString([]byte(key(items[limit-1]))), nil
}

func past(k, after string, desc bool) bool {
	if desc {
		return k < after
	}
	return k > after
}

type apiError struct {
	Status   int
	Message  string
	Reason   string
	Domain   string
	Metadata map[string]string
}

func (e *apiError) Error() string { return fmt.Sprintf("%d %s", e.Status, e.Message) }

func invalid(format string, args ...any) *apiError {
	return &apiError{Status: http.StatusBadRequest, Message: fmt.Sprintf(format, args...)}
}

func notFound(resource string) *apiError {
	return &apiError{Status: http.StatusNotFound, Message: fmt.Sprintf("Resource '%s' was not found", resource)}
}

func alreadyExists(resource string) *apiError {
	return &apiError{Status: http.StatusConflict, Message: fmt.Sprintf("Resource '%s' already exists", resource)}
}

func unimplemented(r *http.Request) *apiError {
	return &apiError{Status: http.StatusNotFound, Message: "squall: " + r.Method + " " + r.URL.Path + " is not implemented"}
}

func faultError(f fault.Fault) *apiError {
	return &apiError{
		Status:  cmp.Or(f.Status, http.StatusServiceUnavailable),
		Message: fmt.Sprintf("squall: injected fault (%s)", f),
		Reason:  f.Reason,
		Domain:  "lustre.googleapis.com",
	}
}

// rpcCodes follow Google's HTTP mapping of google.rpc.Code.
var rpcCodes = map[int]struct {
	code int
	name string
}{
	http.StatusBadRequest:          {3, "INVALID_ARGUMENT"},
	http.StatusUnauthorized:        {16, "UNAUTHENTICATED"},
	http.StatusForbidden:           {7, "PERMISSION_DENIED"},
	http.StatusNotFound:            {5, "NOT_FOUND"},
	http.StatusConflict:            {6, "ALREADY_EXISTS"},
	http.StatusPreconditionFailed:  {9, "FAILED_PRECONDITION"},
	http.StatusTooManyRequests:     {8, "RESOURCE_EXHAUSTED"},
	499:                            {1, "CANCELLED"},
	http.StatusInternalServerError: {13, "INTERNAL"},
	http.StatusNotImplemented:      {12, "UNIMPLEMENTED"},
	http.StatusServiceUnavailable:  {14, "UNAVAILABLE"},
	http.StatusGatewayTimeout:      {4, "DEADLINE_EXCEEDED"},
}

func rpcCode(status int) (int, string) {
	if c, ok := rpcCodes[status]; ok {
		return c.code, c.name
	}
	if status >= 500 {
		return 13, "INTERNAL"
	}
	return 2, "UNKNOWN"
}

// rpcStatus holds the HTTP status in an error response but the rpc code in an operation.
type rpcStatus struct {
	Code    int          `json:"code"`
	Message string       `json:"message"`
	Status  string       `json:"status,omitzero"`
	Details []*errorInfo `json:"details,omitzero"`
}

type errorInfo struct {
	Type     string            `json:"@type"`
	Reason   string            `json:"reason"`
	Domain   string            `json:"domain"`
	Metadata map[string]string `json:"metadata,omitzero"`
}

func (e *apiError) details() []*errorInfo {
	if e.Reason == "" {
		return nil
	}
	return []*errorInfo{{Type: "type.googleapis.com/google.rpc.ErrorInfo", Reason: e.Reason, Domain: e.Domain, Metadata: e.Metadata}}
}

func (e *apiError) operationStatus() *rpcStatus {
	code, _ := rpcCode(e.Status)
	return &rpcStatus{Code: code, Message: e.Message, Details: e.details()}
}

func writeError(w http.ResponseWriter, e *apiError) {
	_, name := rpcCode(e.Status)
	writeJSON(w, e.Status, struct {
		Error rpcStatus `json:"error"`
	}{rpcStatus{Code: e.Status, Message: e.Message, Status: name, Details: e.details()}})
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	b, err := json.Marshal(body)
	if err != nil {
		panic("squall: encoding a response: " + err.Error())
	}
	w.Header().Set("Content-Type", "application/json; charset=UTF-8")
	w.WriteHeader(status)
	// Once the status is sent, a client that went away is all a write error can mean.
	_, _ = w.Write(b)
}
