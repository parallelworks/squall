// Package compute is a stand-in for the Compute Engine API, for testing code
// that calls it through the official client libraries.
//
// [Server] is an [http.Handler] that keeps Compute Engine resources in memory
// and answers the REST calls the Go client library makes. Serve it with
// [net/http/httptest] and point a client at it with option.WithEndpoint and
// option.WithoutAuthentication. A [fault.Injector] makes chosen calls fail,
// including after they took effect, so tests can check that code which
// retries converges.
//
// It covers:
//
//   - regional addresses: get, list, insert, delete, setLabels;
//   - zones: list, get;
//   - instances: bulkInsert, insert, get, list, aggregatedList, delete, stop,
//     start, setLabels, setMachineType, addAccessConfig, attachDisk,
//     detachDisk;
//   - disks: get, list, aggregatedList, insert, delete;
//   - resource policies, on the beta API: list, get, insert, delete;
//   - instance templates: list, get, insert, delete;
//   - instance group managers: aggregatedList, which is always empty;
//   - routes: list, get, insert, delete;
//   - zone, region and global operations: get and wait.
//
// Other methods answer 404. Where it differs from Compute Engine:
//
//   - Changes to instances and disks finish before the call that makes them
//     returns, so the SDK's Operation.Wait returns at once. [WithSettleAfter]
//     holds them in progress, with their operation RUNNING, for more reads
//     or until [Server.Settle]: a new instance is PROVISIONING and then
//     STAGING, a stopping or deleted one STOPPING, a starting one STAGING. A
//     change to an instance that has another in progress finishes that one
//     first. Every other change finishes before its call returns.
//   - A call that repeats an earlier call's requestId in the same project, for
//     the same method, gets that call's operation back without taking effect
//     again. The server keeps the last 10,000 operations.
//   - Images and snapshots must be registered with [Server.PutImage] and
//     [Server.PutSnapshot]; a disk or instance made from any other fails as
//     the API fails one made from an image that does not exist. Machine and
//     disk types are any well-formed name, in the zone of the call; nothing
//     checks that they exist or fit together.
//   - Zones come from a fixed set per region, which [WithZones] changes.
//     Regional calls take any region.
//   - Networks, subnetworks, service accounts, accelerators and other
//     referenced resources are not checked, and fields the server only
//     records, such as guestAccelerators or reservationAffinity, keep the
//     JSON the caller sent. Fields it does not know are rejected with 400.
//     Instance group managers, deletion protection and resource policies
//     other than group placement are not supported.
//   - A bulkInsert skips each instance whose name, or a disk it would
//     create, already exists, and creates the rest when they number at least
//     minCount: its operation succeeds, counting both in its
//     perLocationStatus, with no warning for what it skipped. With fewer, its
//     operation fails with VM_MIN_COUNT_NOT_REACHED and a
//     RESOURCE_ALREADY_EXISTS error per conflict and creates nothing. It
//     takes no fixed IP addresses or disk names, as the API does not, and
//     ignores locationPolicy. A name pattern numbers new names after the
//     highest existing match. A disk created with an instance is named after
//     it: the boot disk takes the instance's name, the others the instance's
//     name and their index.
//   - IP addresses come from 100.64.0.0/10 (external) and 10.0.0.0/8
//     (internal), never from routable ranges. An external address is unique
//     across every project and region; an internal one is unique within its
//     region, which stands in for its subnetwork. An instance's ephemeral
//     external address is released when it stops and replaced when it starts.
//     An instance may take a reserved address by its IP, which makes the
//     address IN_USE until the instance goes. Deleting the address then
//     leaves the IP with the instance until it goes, as the API does.
//   - List filters support the comparisons field = value, field != value, and
//     the regular expressions field eq re and field ne re, on each
//     resource's name, description, id, status and labels, among others. The
//     = form joins with AND, OR and parentheses, with OR binding tighter than
//     AND; the eq form joins only by juxtaposition, in parentheses. Lists are
//     ordered by name. Anything else, such as a wildcard or
//     orderBy=creationTimestamp desc, is rejected with 400 rather than
//     ignored. An aggregatedList lists only the scopes that have items.
//   - A fault rule's Resource for an insert is the resource being created, so
//     a rule can target the insert of one name. For a bulkInsert, it is the
//     first instance it names in perInstanceProperties, or the zone's
//     instances when it names none.
//
// A lost response aborts the handler with [http.ErrAbortHandler] after
// sending the response header, so the [http.Server] serving it closes the
// connection. Calling ServeHTTP directly propagates that panic.
package compute

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/netip"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/parallelworks/squall/fault"
	"github.com/parallelworks/squall/internal/stub"
)

// maxBody bounds a request body the server reads.
const maxBody = 1 << 20

// selfLinkBase prefixes every selfLink, as the real API does.
const selfLinkBase = "https://www.googleapis.com/compute/v1/"

// betaSelfLinkBase prefixes the selfLinks of resources the beta API answers with.
const betaSelfLinkBase = "https://www.googleapis.com/compute/beta/"

// Server is a stand-in for the Compute Engine API. Create one with [New]. It is
// safe for concurrent use.
type Server struct {
	mux    *http.ServeMux
	faults *fault.Injector
	now    func() time.Time

	settleAfter int
	regionZones map[string][]string
	zoneRegion  map[string]string
	zoneIDs     map[string]uint64

	mu         sync.Mutex
	nextID     uint64
	addresses  map[string]*collection[*Address]
	ipHolders  map[string]string
	operations map[string]*operation
	opOrder    []string
	opNext     int
	requests   map[string]*operation
	pending    map[*operation]struct{}
	nextIP     map[string]netip.Addr
	instances  map[string]*collection[*Instance]
	disks      map[string]*collection[*Disk]
	images     map[string]*Image
	snapshots  map[string]*Snapshot
	policies   map[string]*collection[*ResourcePolicy]
	templates  map[string]*collection[*InstanceTemplate]
	routes     map[string]*collection[*Route]
}

// Option configures a [Server].
type Option func(*Server)

// WithFaults makes the server ask in which calls to fail.
func WithFaults(in *fault.Injector) Option {
	return func(s *Server) { s.faults = in }
}

// WithClock sets the clock for creation and operation times. The default is time.Now.
func WithClock(now func() time.Time) Option {
	return func(s *Server) { s.now = now }
}

// WithSettleAfter sets how many reads see a change to an instance or disk in
// progress before it finishes: gets and lists that return the resource, and
// gets of its operation. The default, 0, finishes every change before the call
// that makes it returns, so the operation it answers with is already done. A
// negative n finishes changes only on [Server.Settle]. An operations.wait
// finishes the operation it waits for unless n is negative.
func WithSettleAfter(n int) Option {
	return func(s *Server) { s.settleAfter = n }
}

// WithZones sets the zones of region, such as "us-east1" with "us-east1-b"
// and "us-east1-c", in place of the built-in ones. With no zones it removes
// the region. Zonal calls to a zone the server does not have fail as the API
// fails them for an unknown zone.
func WithZones(region string, zones ...string) Option {
	return func(s *Server) {
		if len(zones) == 0 {
			delete(s.regionZones, region)
			return
		}
		s.regionZones[region] = slices.Clone(zones)
	}
}

// New returns an empty server.
func New(opts ...Option) *Server {
	s := &Server{
		mux:         http.NewServeMux(),
		now:         time.Now,
		regionZones: defaultZones(),
		nextID:      1_000_000_000_000_000_000,
		addresses:   map[string]*collection[*Address]{},
		ipHolders:   map[string]string{},
		operations:  map[string]*operation{},
		requests:    map[string]*operation{},
		pending:     map[*operation]struct{}{},
		instances:   map[string]*collection[*Instance]{},
		disks:       map[string]*collection[*Disk]{},
		images:      map[string]*Image{},
		snapshots:   map[string]*Snapshot{},
		policies:    map[string]*collection[*ResourcePolicy]{},
		templates:   map[string]*collection[*InstanceTemplate]{},
		routes:      map[string]*collection[*Route]{},
		nextIP: map[string]netip.Addr{
			// Neither range is routable on the internet, so a test cannot reach a real host by mistake.
			addressExternal: netip.MustParseAddr("100.64.0.1"),
			addressInternal: netip.MustParseAddr("10.0.0.2"),
		},
	}
	for _, opt := range opts {
		opt(s)
	}
	s.indexZones()
	s.registerRoutes()
	return s
}

func (s *Server) registerRoutes() {
	const (
		project = "/compute/v1/projects/{project}"
		region  = project + "/regions/{region}"
		zone    = project + "/zones/{zone}"
		global  = project + "/global"
		beta    = "/compute/beta/projects/{project}/regions/{region}"
	)
	s.route("GET "+region+"/addresses", "compute.addresses.list", "", s.listAddresses, nil)
	s.route("POST "+region+"/addresses", "compute.addresses.insert", "", s.insertAddress, withNameFromBody)
	s.route("GET "+region+"/addresses/{name}", "compute.addresses.get", "", s.getAddress, nil)
	s.route("DELETE "+region+"/addresses/{name}", "compute.addresses.delete", "", s.deleteAddress, nil)
	s.route("POST "+region+"/addresses/{name}/setLabels", "compute.addresses.setLabels", "/setLabels", s.setAddressLabels, nil)

	s.route("GET "+project+"/zones", "compute.zones.list", "", s.listZones, nil)
	s.route("GET "+zone, "compute.zones.get", "", s.getZone, nil)

	s.route("POST "+zone+"/instances/bulkInsert", "compute.instances.bulkInsert", "/bulkInsert", s.bulkInsertInstances, withBulkTarget)
	s.route("POST "+zone+"/instances", "compute.instances.insert", "", s.insertInstance, withNameFromBody)
	s.route("GET "+zone+"/instances", "compute.instances.list", "", s.listInstances, nil)
	s.route("GET "+project+"/aggregated/instances", "compute.instances.aggregatedList", "", s.aggregatedInstances, nil)
	s.route("GET "+zone+"/instances/{name}", "compute.instances.get", "", s.getInstance, nil)
	s.route("DELETE "+zone+"/instances/{name}", "compute.instances.delete", "", s.deleteInstance, nil)
	for _, verb := range []string{"stop", "start", "setLabels", "setMachineType", "addAccessConfig", "attachDisk", "detachDisk"} {
		s.route("POST "+zone+"/instances/{name}/"+verb, "compute.instances."+verb, "/"+verb, s.instanceVerb(verb), nil)
	}

	s.route("GET "+zone+"/disks", "compute.disks.list", "", s.listDisks, nil)
	s.route("GET "+project+"/aggregated/disks", "compute.disks.aggregatedList", "", s.aggregatedDisks, nil)
	s.route("POST "+zone+"/disks", "compute.disks.insert", "", s.insertDisk, withNameFromBody)
	s.route("GET "+zone+"/disks/{name}", "compute.disks.get", "", s.getDisk, nil)
	s.route("DELETE "+zone+"/disks/{name}", "compute.disks.delete", "", s.deleteDisk, nil)

	s.route("GET "+beta+"/resourcePolicies", "compute.resourcePolicies.list", "", s.listPolicies, nil)
	s.route("POST "+beta+"/resourcePolicies", "compute.resourcePolicies.insert", "", s.insertPolicy, withNameFromBody)
	s.route("GET "+beta+"/resourcePolicies/{name}", "compute.resourcePolicies.get", "", s.getPolicy, nil)
	s.route("DELETE "+beta+"/resourcePolicies/{name}", "compute.resourcePolicies.delete", "", s.deletePolicy, nil)

	s.route("GET "+global+"/instanceTemplates", "compute.instanceTemplates.list", "", s.listTemplates, nil)
	s.route("POST "+global+"/instanceTemplates", "compute.instanceTemplates.insert", "", s.insertTemplate, withNameFromBody)
	s.route("GET "+global+"/instanceTemplates/{name}", "compute.instanceTemplates.get", "", s.getTemplate, nil)
	s.route("DELETE "+global+"/instanceTemplates/{name}", "compute.instanceTemplates.delete", "", s.deleteTemplate, nil)
	s.route("GET "+project+"/aggregated/instanceGroupManagers", "compute.instanceGroupManagers.aggregatedList", "", s.aggregatedGroupManagers, nil)

	s.route("GET "+global+"/routes", "compute.routes.list", "", s.listRoutes, nil)
	s.route("POST "+global+"/routes", "compute.routes.insert", "", s.insertRoute, withNameFromBody)
	s.route("GET "+global+"/routes/{name}", "compute.routes.get", "", s.getRoute, nil)
	s.route("DELETE "+global+"/routes/{name}", "compute.routes.delete", "", s.deleteRoute, nil)

	for _, scope := range []struct{ path, kind string }{{zone, "zone"}, {region, "region"}, {beta, "region"}, {global, "global"}} {
		s.route("GET "+scope.path+"/operations/{name}", "compute."+scope.kind+"Operations.get", "", s.getOperation, nil)
		s.route("POST "+scope.path+"/operations/{name}/wait", "compute."+scope.kind+"Operations.wait", "/wait", s.waitOperation, nil)
	}
	s.mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		writeError(w, notFound(resourcePath(r)))
	})
}

// resourcePath is r's path without its API version prefix.
func resourcePath(r *http.Request) string {
	if p, ok := strings.CutPrefix(r.URL.Path, "/compute/beta/"); ok {
		return p
	}
	return strings.TrimPrefix(r.URL.Path, "/compute/v1/")
}

// ServeHTTP answers a Compute Engine REST call.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mux.ServeHTTP(w, r)
}

// handler answers one method. It returns the response body, or an error.
// A non-nil opFault means the call's operation must fail without the call
// taking effect.
type handler func(r *http.Request, opFault *apiError) (any, *apiError)

// withNameFromBody makes a call that creates a resource name the resource it
// creates, from the request body, so a fault rule can target one name.
func withNameFromBody(r *http.Request, collection string) string {
	body, err := io.ReadAll(io.LimitReader(r.Body, maxBody))
	r.Body = io.NopCloser(bytes.NewReader(body))
	var named struct {
		Name string `json:"name"`
	}
	if err != nil || json.Unmarshal(body, &named) != nil || named.Name == "" {
		return collection
	}
	return collection + "/" + named.Name
}

// route registers h for pattern as method. A non-nil target names the call's
// resource from the request, in place of its path.
func (s *Server) route(pattern, method, verb string, h handler, target func(*http.Request, string) string) {
	startsOperation := !strings.HasPrefix(pattern, http.MethodGet+" ") && verb != "/wait"
	s.mux.HandleFunc(pattern, func(w http.ResponseWriter, r *http.Request) {
		resource := strings.TrimSuffix(resourcePath(r), verb)
		if target != nil {
			resource = target(r, resource)
		}
		f, _ := s.faults.Decide(fault.Call{Method: method, Resource: resource})
		reject := f.Mode == fault.ModeReject || f.Mode == fault.ModeFailOperation && !startsOperation
		stub.Serve(w, r, f, reject, "", func() { writeError(w, faultError(f)) }, func() (func(), bool) {
			var opFault *apiError
			if f.Mode == fault.ModeFailOperation {
				opFault = faultError(f)
			}
			var body any
			err := s.checkZone(r)
			if err == nil {
				body, err = h(r, opFault)
			}
			if err != nil {
				return func() { writeError(w, err) }, false
			}
			return func() { writeJSON(w, http.StatusOK, body) }, true
		})
	})
}

// decodeBody reads a JSON request body into v.
func decodeBody(r *http.Request, v any) *apiError {
	body, err := io.ReadAll(io.LimitReader(r.Body, maxBody))
	if err != nil {
		return invalid("reading the request body: %v", err)
	}
	if err := json.Unmarshal(body, v); err != nil {
		if _, ok := errors.AsType[*json.SyntaxError](err); ok {
			return invalid("Invalid JSON payload received: %v", err)
		}
		return invalid("Invalid value in the request body: %v", err)
	}
	return nil
}

// decodeStrict reads a JSON request body into v, rejecting fields v does not
// have, as the API rejects fields it does not know.
func decodeStrict(r *http.Request, v any) *apiError {
	body, err := io.ReadAll(io.LimitReader(r.Body, maxBody))
	if err != nil {
		return invalid("reading the request body: %v", err)
	}
	return unmarshalStrict(body, v)
}

// unmarshalStrict decodes data into v, rejecting fields v does not have.
func unmarshalStrict(data []byte, v any) *apiError {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		if name, ok := strings.CutPrefix(err.Error(), "json: unknown field "); ok {
			return invalid("Invalid JSON payload received. Unknown name %s: Cannot find field.", name)
		}
		if _, ok := errors.AsType[*json.SyntaxError](err); ok {
			return invalid("Invalid JSON payload received: %v", err)
		}
		return invalid("Invalid value in the request body: %v", err)
	}
	return nil
}

// newID returns a fresh resource or operation ID. The caller holds s.mu.
func (s *Server) newID() uint64 {
	s.nextID++
	return s.nextID
}

// timestamp formats t as the API does.
func timestamp(t time.Time) string { return t.Format("2006-01-02T15:04:05.000-07:00") }

// requestID returns the key a call's requestId replays under, scoped to its
// project and method, or "" when the call has none. The API requires the ID to
// be a non-zero UUID.
func requestID(r *http.Request) (string, *apiError) {
	id := r.URL.Query().Get("requestId")
	if id == "" {
		return "", nil
	}
	if !validUUID(id) {
		return "", invalid("Invalid value for field 'requestId': '%s'. The request ID must be a valid UUID other than the zero UUID.", id)
	}
	return r.PathValue("project") + " " + r.Pattern + " " + id, nil
}

func validUUID(id string) bool {
	if len(id) != 36 || id == "00000000-0000-0000-0000-000000000000" {
		return false
	}
	for i, c := range []byte(id) {
		switch i {
		case 8, 13, 18, 23:
			if c != '-' {
				return false
			}
		default:
			if !strings.ContainsRune("0123456789abcdefABCDEF", rune(c)) {
				return false
			}
		}
	}
	return true
}

func regionPath(r *http.Request) string {
	return fmt.Sprintf("projects/%s/regions/%s", r.PathValue("project"), r.PathValue("region"))
}
