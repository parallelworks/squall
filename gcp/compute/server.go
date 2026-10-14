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
// It covers regional addresses (get, list, insert, delete, setLabels) and the
// region operations that report on them. Long-running operations finish
// before the call returns, and the server keeps the last 10,000. A call that
// repeats an earlier call's requestId in the same project, for the same
// method, gets that call's operation back without taking effect again. An
// external IP address is unique across every project and region, an internal
// one within its region. Other methods answer 404.
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

// Server is a stand-in for the Compute Engine API. Create one with [New]. It is
// safe for concurrent use.
type Server struct {
	mux    *http.ServeMux
	faults *fault.Injector
	now    func() time.Time

	mu         sync.Mutex
	nextID     uint64
	addresses  map[string]*addressSet
	ipHolders  map[string]string
	operations map[string]*operation
	opOrder    []string
	opNext     int
	requests   map[string]*operation
	nextIP     map[string]netip.Addr
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

// New returns an empty server.
func New(opts ...Option) *Server {
	s := &Server{
		mux:        http.NewServeMux(),
		now:        time.Now,
		nextID:     1_000_000_000_000_000_000,
		addresses:  map[string]*addressSet{},
		ipHolders:  map[string]string{},
		operations: map[string]*operation{},
		requests:   map[string]*operation{},
		nextIP: map[string]netip.Addr{
			// Neither range is routable on the internet, so a test cannot reach a real host by mistake.
			addressExternal: netip.MustParseAddr("100.64.0.1"),
			addressInternal: netip.MustParseAddr("10.0.0.2"),
		},
	}
	for _, opt := range opts {
		opt(s)
	}
	const region = "/compute/v1/projects/{project}/regions/{region}"
	s.route("GET "+region+"/addresses", "compute.addresses.list", "", s.listAddresses, nil)
	s.route("POST "+region+"/addresses", "compute.addresses.insert", "", s.insertAddress, withNameFromBody)
	s.route("GET "+region+"/addresses/{name}", "compute.addresses.get", "", s.getAddress, nil)
	s.route("DELETE "+region+"/addresses/{name}", "compute.addresses.delete", "", s.deleteAddress, nil)
	s.route("POST "+region+"/addresses/{name}/setLabels", "compute.addresses.setLabels", "/setLabels", s.setAddressLabels, nil)
	s.route("GET "+region+"/operations/{name}", "compute.regionOperations.get", "", s.getOperation, nil)
	s.mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		writeError(w, notFound(strings.TrimPrefix(r.URL.Path, "/compute/v1/")))
	})
	return s
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
	startsOperation := !strings.HasPrefix(pattern, http.MethodGet+" ")
	s.mux.HandleFunc(pattern, func(w http.ResponseWriter, r *http.Request) {
		resource := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/compute/v1/"), verb)
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
			body, err := h(r, opFault)
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
func requestID(r *http.Request, method string) (string, *apiError) {
	id := r.URL.Query().Get("requestId")
	if id == "" {
		return "", nil
	}
	if !validUUID(id) {
		return "", invalid("Invalid value for field 'requestId': '%s'. The request ID must be a valid UUID other than the zero UUID.", id)
	}
	return r.PathValue("project") + "/" + method + "/" + id, nil
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
