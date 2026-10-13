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
// before the call returns. Other methods answer 404.
package compute

import (
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
)

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
	operations map[string]*operation
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
		operations: map[string]*operation{},
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
	s.route("GET "+region+"/addresses", "compute.addresses.list", "", s.listAddresses)
	s.route("POST "+region+"/addresses", "compute.addresses.insert", "", s.insertAddress)
	s.route("GET "+region+"/addresses/{name}", "compute.addresses.get", "", s.getAddress)
	s.route("DELETE "+region+"/addresses/{name}", "compute.addresses.delete", "", s.deleteAddress)
	s.route("POST "+region+"/addresses/{name}/setLabels", "compute.addresses.setLabels", "/setLabels", s.setAddressLabels)
	s.route("GET "+region+"/operations/{name}", "compute.regionOperations.get", "", s.getOperation)
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

func (s *Server) route(pattern, method, verb string, h handler) {
	s.mux.HandleFunc(pattern, func(w http.ResponseWriter, r *http.Request) {
		resource := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/compute/v1/"), verb)
		f, _ := s.faults.Decide(fault.Call{Method: method, Resource: resource})
		if f.Delay > 0 {
			t := time.NewTimer(f.Delay)
			select {
			case <-t.C:
			case <-r.Context().Done():
				t.Stop()
				return
			}
		}
		var opFault *apiError
		switch f.Mode {
		case fault.Reject:
			writeError(w, faultError(f))
			return
		case fault.FailOperation:
			opFault = faultError(f)
		case fault.Pass, fault.LoseResponse, fault.FailAfter:
		}
		body, err := h(r, opFault)
		if err != nil {
			writeError(w, err)
			return
		}
		switch f.Mode {
		case fault.LoseResponse:
			// Ends the response without one, as a connection lost after the server committed would.
			panic(http.ErrAbortHandler)
		case fault.FailAfter:
			writeError(w, faultError(f))
			return
		case fault.FailOperation:
			if _, isOp := body.(*operation); !isOp {
				writeError(w, opFault)
				return
			}
		case fault.Pass, fault.Reject:
		}
		writeJSON(w, http.StatusOK, body)
	})
}

// decodeBody reads a JSON request body into v.
func decodeBody(r *http.Request, v any) *apiError {
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
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

func regionPath(r *http.Request) string {
	return fmt.Sprintf("projects/%s/regions/%s", r.PathValue("project"), r.PathValue("region"))
}
