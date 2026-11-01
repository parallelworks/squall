// Package servicenetworking is a stand-in for the Service Networking API
// (servicenetworking.googleapis.com v1), for testing code that sets up
// private services access through the official client libraries.
//
// [Server] is an [http.Handler] that keeps private service connections and
// their long-running operations in memory and answers the REST calls the Go
// client library makes. Serve it with [net/http/httptest] and point a client
// at it with servicenetworking.NewService, option.WithEndpoint(srv.URL) and
// option.WithoutAuthentication(). A [fault.Injector] makes chosen calls fail,
// including after they took effect, so tests can check that code which
// retries converges.
//
// It covers:
//
//   - services.connections: create, list, patch, deleteConnection (a POST
//     to the connection);
//   - operations: get.
//
// Other methods answer 404. Where it differs from Service Networking:
//
//   - The only service is servicenetworking.googleapis.com, and the only
//     connection on a network is servicenetworking-googleapis-com, which
//     patch and deleteConnection also take as "-".
//   - [WithCompute] makes the server check networks and allocated ranges
//     against a [compute.Server]: a connection's network must exist, and each
//     reserved peering range must name a global address on that network with
//     purpose VPC_PEERING. A connection adds a peering named
//     servicenetworking-googleapis-com to its network, which then cannot be
//     deleted until deleteConnection removes it, as Compute Engine refuses to
//     delete a network that is still peered. Without WithCompute nothing is
//     checked against Compute Engine.
//   - Networks are named projects/{project}/global/networks/{network}, where
//     {project} is a project number or ID. [WithProjectNumber] maps a number to
//     its project ID, so both name one network, and connections name their
//     network by number, as the API does. A project with no number set is
//     named by its ID.
//   - Changes finish before the call that makes them returns, so the
//     operation it answers with is already done. A create on a network that
//     already has a connection, or that names a range that does not exist,
//     ends its operation in a FAILED_PRECONDITION error, as the API does;
//     malformed requests are refused with 400 and a patch or deleteConnection
//     of a connection that does not exist with 404. A patch that removes a
//     range needs force=true. Ranges are not checked for use by producer
//     services, which the stand-in does not have, and a global address a
//     connection names can still be deleted.
//   - Error messages are the stand-in's own, except those the API documents
//     and clients match on, such as "Cannot modify allocated ranges in
//     CreateConnection. Please use UpdateConnection.".
//
// Fault method names are the API's method IDs, such as
// "servicenetworking.services.connections.create". A call's resource is its
// path below /v1/, such as
// "services/servicenetworking.googleapis.com/connections".
// [fault.FailOperation] ends a create, patch or deleteConnection's operation
// in an error without the change taking effect.
//
// [WithPolicy] grants the caller a set of IAM permissions and refuses a call
// that needs one it lacks with 403 PERMISSION_DENIED, before any fault:
// servicenetworking.services.addPeering for a create or patch,
// servicenetworking.services.get for a list,
// servicenetworking.services.deleteConnection for a deleteConnection, and
// servicenetworking.operations.get to poll an operation.
//
// A lost response aborts the handler with [http.ErrAbortHandler] after
// sending the response header, so the [http.Server] serving it closes the
// connection. Calling ServeHTTP directly propagates that panic.
package servicenetworking

import (
	"cmp"
	"encoding/json"
	"fmt"
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
	"github.com/parallelworks/squall/gcp/compute"
	"github.com/parallelworks/squall/internal/stub"
)

const (
	maxBody       = 1 << 20
	maxOperations = 10_000

	service     = "servicenetworking.googleapis.com"
	parent      = "services/" + service
	peeringName = "servicenetworking-googleapis-com"
)

// Connection is a private service connection, in the API's JSON shape.
type Connection struct {
	Network               string   `json:"network"`
	Peering               string   `json:"peering"`
	ReservedPeeringRanges []string `json:"reservedPeeringRanges"`
	Service               string   `json:"service"`
}

// Server is a stand-in for the Service Networking API, safe for concurrent
// use. Create one with [New].
type Server struct {
	mux     *http.ServeMux
	faults  *fault.Injector
	policy  *access.Policy
	now     func() time.Time
	compute *compute.Server
	// numbers maps project numbers to IDs, and ids IDs to numbers.
	numbers, ids map[string]string

	mu          sync.Mutex
	nextID      uint64
	connections map[string]*Connection
	operations  map[string]*operation
	opOrder     []string
	opNext      int
}

// Option configures a [Server].
type Option func(*Server)

// WithFaults makes the server ask in which calls to fail.
func WithFaults(in *fault.Injector) Option {
	return func(s *Server) { s.faults = in }
}

// WithClock sets the clock for operation times. The default is time.Now.
func WithClock(now func() time.Time) Option {
	return func(s *Server) { s.now = now }
}

// WithCompute checks networks and allocated ranges against c, and peers
// connected networks in it.
func WithCompute(c *compute.Server) Option {
	return func(s *Server) { s.compute = c }
}

// WithProjectNumber makes number another name for project, the project ID.
func WithProjectNumber(project string, number int64) Option {
	return func(s *Server) {
		n := strconv.FormatInt(number, 10)
		s.numbers[n], s.ids[project] = project, n
	}
}

// WithPolicy makes the server refuse calls that need a permission p lacks,
// and records what each call needed. Without it every call is allowed.
func WithPolicy(p *access.Policy) Option {
	return func(s *Server) { s.policy = p }
}

// New returns an empty server.
func New(opts ...Option) *Server {
	s := &Server{
		mux:         http.NewServeMux(),
		now:         time.Now,
		numbers:     map[string]string{},
		ids:         map[string]string{},
		connections: map[string]*Connection{},
		operations:  map[string]*operation{},
	}
	for _, opt := range opts {
		opt(s)
	}
	s.registerRoutes()
	return s
}

// ServeHTTP answers a Service Networking REST call.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mux.ServeHTTP(w, r)
}

const (
	methodCreate = "servicenetworking.services.connections.create"
	methodList   = "servicenetworking.services.connections.list"
	methodPatch  = "servicenetworking.services.connections.patch"
	methodDelete = "servicenetworking.services.connections.deleteConnection"
	methodOpGet  = "servicenetworking.operations.get"
)

// basePermissions are from the Service Networking IAM reference.
var basePermissions = map[string][]string{
	methodCreate: {"servicenetworking.services.addPeering"},
	methodList:   {"servicenetworking.services.get"},
	methodPatch:  {"servicenetworking.services.addPeering"},
	methodDelete: {"servicenetworking.services.deleteConnection"},
	methodOpGet:  {"servicenetworking.operations.get"},
}

// commonParams are the discovery document's standard parameters, and the $ spellings the client sends.
var commonParams = []string{
	"$.xgafv", "access_token", "alt", "callback", "fields", "key", "oauth_token", "prettyPrint", "quotaUser", "upload_protocol", "uploadType",
	"$alt", "$fields", "$prettyPrint",
}

// handler answers one method; a non-nil opFault fails the operation it
// starts without the call taking effect.
type handler func(r *http.Request, opFault *apiError) (any, *apiError)

func (s *Server) registerRoutes() {
	const (
		connections = "/v1/services/{service}/connections"
		connection  = connections + "/{connection}"
	)
	s.route("POST "+connections, methodCreate, true, s.create)
	s.route("GET "+connections, methodList, false, s.list, "network")
	s.route("PATCH "+connection, methodPatch, true, s.patch, "force", "updateMask")
	s.route("POST "+connection, methodDelete, true, s.deleteConnection)
	s.route("GET /v1/operations/{operation...}", methodOpGet, false, s.getOperation)
	s.mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		writeError(w, unimplemented(r))
	})
}

func (s *Server) route(pattern, method string, startsOperation bool, h handler, params ...string) {
	s.mux.HandleFunc(pattern, func(w http.ResponseWriter, r *http.Request) {
		if r.PathValue("service") != "" && r.PathValue("service") != service {
			writeError(w, unimplemented(r))
			return
		}
		if conn := r.PathValue("connection"); conn != "" && conn != peeringName && conn != "-" {
			writeError(w, notFound("Connection "+parent+"/connections/"+conn))
			return
		}
		if err := checkParams(r, params); err != nil {
			writeError(w, err)
			return
		}
		resource := strings.TrimPrefix(r.URL.Path, "/v1/")
		if s.policy != nil {
			if missing := s.policy.Check(access.Call{Method: method, Resource: resource, Needs: basePermissions[method]}); missing != nil {
				writeError(w, permissionDenied(missing))
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
			body, err := h(r, opFault)
			if err != nil {
				return func() { writeError(w, err) }, false
			}
			return func() { writeJSON(w, http.StatusOK, body) }, true
		})
	})
}

// checkParams rejects a parameter the method does not take, as the API does.
func checkParams(r *http.Request, params []string) *apiError {
	for name := range r.URL.Query() {
		if !slices.Contains(commonParams, name) && !slices.Contains(params, name) {
			return invalid("Invalid JSON payload received. Unknown name %q: Cannot bind query parameter. Field %q could not be found in request message.", name, name)
		}
	}
	return nil
}

func readJSON(r *http.Request, v any) *apiError {
	body, err := io.ReadAll(io.LimitReader(r.Body, maxBody))
	if err != nil {
		return invalid("reading the request body: %v", err)
	}
	if err := strictly(body, v); err != nil {
		return invalid("Invalid JSON payload received. %v", err)
	}
	return nil
}

func strictly(body []byte, v any) error {
	dec := json.NewDecoder(strings.NewReader(string(body)))
	dec.DisallowUnknownFields()
	return dec.Decode(v)
}

// network is a consumer network a request names.
type network struct {
	project, name string
}

// parseNetwork reads ref, projects/{project}/global/networks/{network}, with
// the project by number or ID.
func (s *Server) parseNetwork(field, ref string) (network, *apiError) {
	rest, ok := strings.CutPrefix(ref, "projects/")
	project, name, found := strings.Cut(rest, "/global/networks/")
	if !ok || !found || project == "" || name == "" || strings.Contains(project, "/") || strings.Contains(name, "/") {
		return network{}, invalid("Invalid value for %s: %q. It must be projects/{project}/global/networks/{network}.", field, ref)
	}
	if id, ok := s.numbers[project]; ok {
		project = id
	}
	return network{project, name}, nil
}

// key identifies n among the server's connections.
func (n network) key() string { return "projects/" + n.project + "/global/networks/" + n.name }

// display names n as the API does, by project number when it is known.
func (s *Server) display(n network) string {
	return "projects/" + cmp.Or(s.ids[n.project], n.project) + "/global/networks/" + n.name
}

func (c *Connection) clone() Connection {
	out := *c
	out.ReservedPeeringRanges = slices.Clone(c.ReservedPeeringRanges)
	return out
}

// checkRanges fails the ranges a connection on n would hold that are not
// VPC_PEERING ranges on n.
func (s *Server) checkRanges(n network, ranges []string) *apiError {
	if s.compute == nil {
		return nil
	}
	nw, ok := s.compute.LookupNetwork(n.project, n.name)
	if !ok {
		return failedPrecondition("Network %s does not exist.", s.display(n))
	}
	for _, name := range ranges {
		a, ok := s.compute.LookupGlobalAddress(n.project, name)
		if !ok || a.Network != nw.SelfLink {
			return failedPrecondition("Allocated IP range '%s' not found in network.", name)
		}
		if a.Purpose != "VPC_PEERING" {
			return failedPrecondition("Allocated IP range '%s' does not have purpose VPC_PEERING.", name)
		}
	}
	return nil
}

func checkRangeNames(ranges []string) *apiError {
	if len(ranges) == 0 {
		return invalid("Invalid value for reserved_peering_ranges: at least one allocated range is required.")
	}
	for i, name := range ranges {
		if name == "" || slices.Contains(ranges[:i], name) {
			return invalid("Invalid value for reserved_peering_ranges: %q is empty or repeated.", name)
		}
	}
	return nil
}

// tenantNetwork is the producer network a connection peers with.
func tenantNetwork(n network) string {
	return fmt.Sprintf("https://www.googleapis.com/compute/v1/projects/%x-tp/global/networks/servicenetworking", fnv32(n.key()))
}

func fnv32(s string) uint32 {
	h := uint32(2166136261)
	for _, c := range []byte(s) {
		h ^= uint32(c)
		h *= 16777619
	}
	return h
}

func (s *Server) create(r *http.Request, opFault *apiError) (any, *apiError) {
	var want Connection
	if err := readJSON(r, &want); err != nil {
		return nil, err
	}
	n, err := s.parseNetwork("network", want.Network)
	if err != nil {
		return nil, err
	}
	if err := checkRangeNames(want.ReservedPeeringRanges); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	failure := opFault
	if failure == nil {
		failure = s.createFailure(n, want.ReservedPeeringRanges)
	}
	var result *Connection
	if failure == nil {
		result = &Connection{Network: s.display(n), Peering: peeringName, ReservedPeeringRanges: slices.Clone(want.ReservedPeeringRanges), Service: parent}
		s.connections[n.key()] = result
	}
	return s.startOperation(n, failure, result), nil
}

// createFailure is the error a create on n ends in, or nil once it has
// peered n. The caller holds s.mu.
func (s *Server) createFailure(n network, ranges []string) *apiError {
	if _, ok := s.connections[n.key()]; ok {
		return failedPrecondition("Cannot modify allocated ranges in CreateConnection. Please use UpdateConnection.")
	}
	if err := s.checkRanges(n, ranges); err != nil {
		return err
	}
	if s.compute == nil {
		return nil
	}
	err := s.compute.AddPeering(n.project, n.name, compute.NetworkPeering{
		Name: peeringName, Network: tenantNetwork(n), State: "ACTIVE", StateDetails: "[" + s.now().UTC().Format(time.RFC3339) + "]: Connected.",
		AutoCreateRoutes: true, ExchangeSubnetRoutes: true, ExportSubnetRoutesWithPublicIP: true, StackType: "IPV4_ONLY",
	})
	if err != nil {
		return failedPrecondition("Peering network %s: %v", s.display(n), err)
	}
	return nil
}

func (s *Server) list(r *http.Request, _ *apiError) (any, *apiError) {
	ref := r.URL.Query().Get("network")
	if ref == "" {
		return nil, invalid("Invalid value for network: a network is required.")
	}
	n, err := s.parseNetwork("network", ref)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	var out struct {
		Connections []Connection `json:"connections,omitzero"`
	}
	if c, ok := s.connections[n.key()]; ok {
		out.Connections = append(out.Connections, c.clone())
	}
	return out, nil
}

func (s *Server) patch(r *http.Request, opFault *apiError) (any, *apiError) {
	q := r.URL.Query()
	force := false
	if v := q.Get("force"); v != "" {
		var err error
		if force, err = strconv.ParseBool(v); err != nil || v != "true" && v != "false" {
			return nil, invalid("Invalid value for force: %q", v)
		}
	}
	switch mask := q.Get("updateMask"); mask {
	case "", "*", "reservedPeeringRanges", "reserved_peering_ranges":
	default:
		return nil, invalid("Invalid value for update_mask: %q. Only reservedPeeringRanges can be updated.", mask)
	}
	var want Connection
	if err := readJSON(r, &want); err != nil {
		return nil, err
	}
	n, err := s.parseNetwork("network", want.Network)
	if err != nil {
		return nil, err
	}
	if err := checkRangeNames(want.ReservedPeeringRanges); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	c, ok := s.connections[n.key()]
	if !ok {
		return nil, notFound("Connection for network " + s.display(n))
	}
	failure := opFault
	if failure == nil {
		var removed []string
		for _, name := range c.ReservedPeeringRanges {
			if !slices.Contains(want.ReservedPeeringRanges, name) {
				removed = append(removed, name)
			}
		}
		if len(removed) > 0 && !force {
			failure = failedPrecondition("Removing allocated ranges %v from the connection requires force.", removed)
		} else {
			failure = s.checkRanges(n, want.ReservedPeeringRanges)
		}
	}
	var result *Connection
	if failure == nil {
		c.ReservedPeeringRanges = slices.Clone(want.ReservedPeeringRanges)
		result = c
	}
	return s.startOperation(n, failure, result), nil
}

func (s *Server) deleteConnection(r *http.Request, opFault *apiError) (any, *apiError) {
	var req struct {
		ConsumerNetwork string `json:"consumerNetwork"`
	}
	if err := readJSON(r, &req); err != nil {
		return nil, err
	}
	if req.ConsumerNetwork == "" {
		return nil, invalid("Invalid value for consumer_network: a network is required.")
	}
	n, err := s.parseNetwork("consumer_network", req.ConsumerNetwork)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.connections[n.key()]; !ok {
		return nil, notFound("Connection for network " + s.display(n))
	}
	if opFault == nil {
		delete(s.connections, n.key())
		if s.compute != nil {
			s.compute.RemovePeering(n.project, n.name, peeringName)
		}
	}
	return s.startOperation(n, opFault, nil), nil
}

// Connections returns every connection the server holds, ordered by network.
func (s *Server) Connections() []Connection {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Connection, 0, len(s.connections))
	for _, key := range slices.Sorted(maps.Keys(s.connections)) {
		out = append(out, s.connections[key].clone())
	}
	return out
}
