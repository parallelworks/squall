// Package ec2 is a stand-in for the Amazon EC2 API, for testing code that
// calls it through the official SDKs.
//
// [Server] is an [http.Handler] that keeps EC2 resources in memory and answers
// the EC2 Query API: form-encoded calls with an Action and Version 2016-11-15,
// answered in EC2's XML. Serve it with [net/http/httptest] and point a client
// at it with the SDK's base endpoint setting, or the AWS_ENDPOINT_URL_EC2 or
// AWS_ENDPOINT_URL environment variable, and any static credentials. A
// [fault.Injector] makes chosen calls fail, including after they took effect,
// so tests can check that code which retries converges.
//
// It covers Elastic IP addresses (AllocateAddress, DescribeAddresses,
// ReleaseAddress, AssociateAddress, DisassociateAddress) and network
// interfaces (CreateNetworkInterface, DescribeNetworkInterfaces,
// DeleteNetworkInterface), with the real API's error codes. Other actions
// answer 404 with the code InvalidAction. Where it differs from EC2:
//
//   - Signatures are not checked. The region comes from the credential scope
//     of the SigV4 Authorization header (or the X-Amz-Credential query
//     parameter), and each region keeps its own resources. There is one
//     account: the server keeps no owner IDs.
//   - Subnet and security group IDs are not validated, and
//     there are no VPCs: interfaces report no VPC ID, and their availability
//     zone is the region with "a" appended. Private IP addresses are unique
//     within a subnet and come from 10.0.0.0/8; public ones are unique across
//     every region and come from 100.64.0.0/10, never from routable ranges.
//   - Instances are not modeled. [Server.AttachNetworkInterface] marks an
//     interface in use by an instance ID of the caller's choosing.
//   - There is no quota on addresses unless [WithAddressLimit] sets one.
//   - Each interface has one private IP address; an address associates with
//     it. Associating an address with an interface that already has another
//     disassociates the other, and associating it again with the interface it
//     is on returns its current association.
//   - A CreateNetworkInterface that repeats a ClientToken in the same region
//     with the same parameters gets the interface it created, as first
//     created, without taking effect again; with other parameters it fails
//     with IdempotentParameterMismatch. The server keeps the last 10,000
//     tokens.
//   - Filter values take the * and ? wildcards. A filter name the server does
//     not support, like any parameter it does not support, is rejected with
//     400 rather than ignored.
//
// Fault method names are "ec2." and the action, such as "ec2.AllocateAddress".
// A call's resource is the region and the ID it acts on, such as
// "us-east-1/eipalloc-00000000000000001"; a create call names the resource it
// creates, and a describe call names the collection, such as
// "us-east-1/addresses". EC2 has no long-running operations, so
// [fault.ModeFailOperation] fails a call as [fault.ModeReject] does. A fault's
// Reason is the error code, except that the fault package's generic reasons
// become InternalError, Unavailable (for 503) and RequestLimitExceeded.
//
// A lost response aborts the handler with [http.ErrAbortHandler] after
// sending the response header, so the [http.Server] serving it closes the
// connection. Calling ServeHTTP directly propagates that panic.
package ec2

import (
	"encoding/xml"
	"fmt"
	"net/http"
	"net/netip"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/parallelworks/squall/fault"
)

const (
	apiVersion = "2016-11-15"
	namespace  = "http://ec2.amazonaws.com/doc/" + apiVersion + "/"

	// maxBody bounds a request body the server reads.
	maxBody = 1 << 20
	// maxTokens bounds how many client tokens the server remembers.
	maxTokens = 10_000
)

// Server is a stand-in for the Amazon EC2 API. Create one with [New]. It is
// safe for concurrent use.
type Server struct {
	faults       *fault.Injector
	addressLimit int
	actions      map[string]action
	requests     atomic.Uint64

	mu         sync.Mutex
	nextID     uint64
	regions    map[string]*regionState
	publicIPs  map[string]string
	privateIPs map[string]string
	nextPublic netip.Addr
	nextLocal  netip.Addr
	tokens     map[string]*idempotent
	tokenOrder []string
	tokenNext  int
}

type regionState struct {
	addresses  map[string]*Address
	interfaces map[string]*NetworkInterface
}

// Option configures a [Server].
type Option func(*Server)

// WithFaults makes the server ask in which calls to fail.
func WithFaults(in *fault.Injector) Option {
	return func(s *Server) { s.faults = in }
}

// WithAddressLimit caps the Elastic IP addresses a region holds, as the
// account quota does. AllocateAddress past it fails with
// AddressLimitExceeded. The default, 0, is no limit.
func WithAddressLimit(n int) Option {
	return func(s *Server) { s.addressLimit = n }
}

// New returns an empty server.
func New(opts ...Option) *Server {
	s := &Server{
		regions:    map[string]*regionState{},
		publicIPs:  map[string]string{},
		privateIPs: map[string]string{},
		// Neither range is routable on the internet, so a test cannot reach a real host by mistake.
		nextPublic: netip.MustParseAddr("100.64.0.1"),
		nextLocal:  netip.MustParseAddr("10.0.0.4"),
		tokens:     map[string]*idempotent{},
	}
	for _, opt := range opts {
		opt(s)
	}
	s.actions = map[string]action{
		"AllocateAddress":           {s.allocateTarget, s.allocateAddress},
		"DescribeAddresses":         {collection("addresses"), s.describeAddresses},
		"ReleaseAddress":            {named("AllocationId"), s.releaseAddress},
		"AssociateAddress":          {named("AllocationId"), s.associateAddress},
		"DisassociateAddress":       {named("AssociationId"), s.disassociateAddress},
		"CreateNetworkInterface":    {s.createInterfaceTarget, s.createNetworkInterface},
		"DescribeNetworkInterfaces": {collection("network-interfaces"), s.describeNetworkInterfaces},
		"DeleteNetworkInterface":    {named("NetworkInterfaceId"), s.deleteNetworkInterface},
	}
	return s
}

// call is one request to an action.
type call struct {
	region string
	p      *params
	// id is the ID a create call's resource gets, chosen before the call's
	// fault is decided so the fault can name it.
	id string
}

// response is an action's answer. Every response embeds meta.
type response interface{ setRequestID(id string) }

type meta struct {
	RequestID string `xml:"requestId"`
}

func (m *meta) setRequestID(id string) { m.RequestID = id }

// returnResponse is the answer of an action that only reports success.
type returnResponse struct {
	meta
	Return bool `xml:"return"`
}

// action answers one EC2 action. target names the call's resource for the
// fault injector.
type action struct {
	target func(c *call) string
	handle func(c *call) (response, *apiError)
}

func named(param string) func(*call) string {
	return func(c *call) string { return c.region + "/" + c.p.values.Get(param) }
}

func collection(name string) func(*call) string {
	return func(c *call) string { return c.region + "/" + name }
}

// ServeHTTP answers an EC2 Query API call.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	requestID := s.requestID()
	name, a, c, err := s.parse(w, r)
	if err != nil {
		writeError(w, requestID, err)
		return
	}
	f, _ := s.faults.Decide(fault.Call{Method: "ec2." + name, Resource: a.target(c)})
	if f.Delay > 0 {
		select {
		case <-time.After(f.Delay):
		case <-r.Context().Done():
			return
		}
	}
	if f.Mode == fault.ModeReject || f.Mode == fault.ModeFailOperation {
		writeError(w, requestID, faultError(f))
		return
	}
	body, err := a.handle(c)
	switch {
	case f.Mode == fault.ModeLoseResponse:
		loseResponse(w)
	case err != nil:
		writeError(w, requestID, err)
	case f.Mode == fault.ModeFailAfter:
		writeError(w, requestID, faultError(f))
	default:
		body.setRequestID(requestID)
		writeXML(w, http.StatusOK, xml.StartElement{Name: xml.Name{Space: namespace, Local: name + "Response"}}, body)
	}
}

// parse reads the call's action and region.
func (s *Server) parse(w http.ResponseWriter, r *http.Request) (string, action, *call, *apiError) {
	if r.URL.Path != "/" {
		return "", action{}, nil, &apiError{http.StatusNotFound, "InvalidAction", "squall: no EC2 action is served at " + r.URL.Path}
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxBody)
	if err := r.ParseForm(); err != nil {
		return "", action{}, nil, clientError("MalformedQueryString", "The request could not be parsed: %v", err)
	}
	name := r.Form.Get("Action")
	if name == "" {
		return "", action{}, nil, clientError("MissingAction", "The request must contain the parameter Action")
	}
	if v := r.Form.Get("Version"); v != apiVersion {
		if v == "" {
			return "", action{}, nil, missing("Version")
		}
		return "", action{}, nil, clientError("NoSuchVersion", "The requested version (%s) of service AmazonEC2 does not exist", v)
	}
	region, err := regionOf(r)
	if err != nil {
		return "", action{}, nil, err
	}
	a, ok := s.actions[name]
	if !ok {
		return "", action{}, nil, &apiError{http.StatusNotFound, "InvalidAction", fmt.Sprintf("The action %s is not valid for this web service.", name)}
	}
	return name, a, &call{region: region, p: newParams(r.Form)}, nil
}

// regionOf reads the region from the SigV4 credential scope,
// "AKID/20260101/us-east-1/ec2/aws4_request", without checking the signature.
func regionOf(r *http.Request) (string, *apiError) {
	credential := r.Form.Get("X-Amz-Credential")
	if h := r.Header.Get("Authorization"); h != "" {
		algorithm, fields, _ := strings.Cut(h, " ")
		if algorithm != "AWS4-HMAC-SHA256" {
			return "", clientError("IncompleteSignature", "Unsupported signing algorithm %q", algorithm)
		}
		for field := range strings.SplitSeq(fields, ",") {
			if v, ok := strings.CutPrefix(strings.TrimSpace(field), "Credential="); ok {
				credential = v
			}
		}
	}
	if credential == "" {
		return "", &apiError{http.StatusUnauthorized, "MissingAuthenticationToken", "The request must contain valid credentials."}
	}
	scope := strings.Split(credential, "/")
	if len(scope) != 5 || scope[2] == "" || scope[4] != "aws4_request" {
		return "", clientError("IncompleteSignature", "The credential scope %q is malformed.", credential)
	}
	return scope[2], nil
}

// requestID returns a fresh request ID in the UUID form the API uses.
func (s *Server) requestID() string {
	n := s.requests.Add(1)
	return fmt.Sprintf("%08x-0000-4000-8000-%012x", n>>48, n&(1<<48-1))
}

// loseResponse ends the response partway through, as a connection lost after
// the server committed would. Without the header on the wire, the client's
// transport would silently retry an idempotent request on a reused connection.
func loseResponse(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/xml;charset=UTF-8")
	w.WriteHeader(http.StatusOK)
	_ = http.NewResponseController(w).Flush()
	panic(http.ErrAbortHandler)
}

// newID returns a fresh resource ID with prefix. The caller holds s.mu.
func (s *Server) newID(prefix string) string {
	s.nextID++
	return fmt.Sprintf("%s-%017x", prefix, s.nextID)
}

// reserveID returns a fresh resource ID with prefix.
func (s *Server) reserveID(prefix string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.newID(prefix)
}

// region returns the state of region, creating it. The caller holds s.mu.
func (s *Server) region(region string) *regionState {
	rs, ok := s.regions[region]
	if !ok {
		rs = &regionState{addresses: map[string]*Address{}, interfaces: map[string]*NetworkInterface{}}
		s.regions[region] = rs
	}
	return rs
}

// idempotent is what a client token replays.
type idempotent struct {
	key         string
	fingerprint string
	created     NetworkInterface
}

// rememberToken keeps the call that used a client token. The caller holds s.mu.
func (s *Server) rememberToken(t *idempotent) {
	s.tokens[t.key] = t
	// tokenOrder is a ring of the kept tokens; tokenNext is the oldest once it is full.
	if len(s.tokenOrder) < maxTokens {
		s.tokenOrder = append(s.tokenOrder, t.key)
		return
	}
	delete(s.tokens, s.tokenOrder[s.tokenNext])
	s.tokenOrder[s.tokenNext] = t.key
	s.tokenNext = (s.tokenNext + 1) % maxTokens
}
