// Package route53 is a stand-in for the Amazon Route 53 API, for testing code
// that manages private DNS through the official SDKs.
//
// [Server] is an [http.Handler] that keeps private hosted zones and their
// record sets in memory and answers the Route 53 REST API under /2013-04-01/,
// in Route 53's XML. Serve it with [net/http/httptest] and point a client at
// it with the SDK's base endpoint setting, or the AWS_ENDPOINT_URL_ROUTE_53 or
// AWS_ENDPOINT_URL environment variable, and any static credentials. A
// [fault.Injector] makes chosen calls fail, including after they took effect,
// so tests can check that code which retries converges.
//
// It covers hosted zones (CreateHostedZone, GetHostedZone,
// ListHostedZonesByName, DeleteHostedZone), their VPC associations
// (AssociateVPCWithHostedZone, DisassociateVPCFromHostedZone), their tags
// (ChangeTagsForResource, ListTagsForResource), their record sets
// (ChangeResourceRecordSets, ListResourceRecordSets) and changes (GetChange),
// with the real API's error codes. Other operations answer 404 with the code
// NotImplemented. Where it differs from Route 53:
//
//   - Signatures are not checked, and a request needs none. There is one
//     account.
//   - Only private hosted zones are supported: CreateHostedZone needs a VPC,
//     and fails with InvalidInput without one or with a DelegationSetId.
//     VPCs are any ID and region unless [WithVPCLookup] says which exist;
//     one it does not know fails with InvalidVPCId.
//   - A CallerReference stays taken after its zone is deleted, so reusing one
//     always fails with HostedZoneAlreadyExists (inferred from the API
//     reference's requirement that each be unique). A private zone whose name
//     another zone associated with the same VPC has fails with
//     ConflictingDomainExists, as does associating a VPC with a zone it is
//     already associated with or with a second zone of the same name.
//     DisassociateVPCFromHostedZone fails with VPCAssociationNotFound for a
//     VPC the zone does not have, without looking the VPC up, and with
//     LastVPCAssociation for the zone's only VPC.
//   - Each zone starts with the SOA and NS record sets Route 53 creates, which
//     ChangeResourceRecordSets may change with UPSERT but not delete.
//     DeleteHostedZone fails with HostedZoneNotEmpty while any other record
//     set remains.
//   - Changes take effect at once: ChangeInfo is always INSYNC. The server
//     keeps the last 10,000 changes for GetChange.
//   - Record sets are simple ones, keyed by name and type: SetIdentifier and
//     the routing policies, health checks and traffic policy instances are
//     rejected with InvalidInput. Alias targets are recorded, not resolved.
//     A, AAAA and CNAME values are checked; other types' values are recorded
//     as sent. A change batch is applied in order and atomically; when any
//     change fails, the answer is InvalidChangeBatch with one message per
//     failed change, in the InvalidChangeBatch shape with its Messages list.
//   - Names are lowercased, take a trailing dot, and have characters other
//     than letters, digits, hyphens and underscores written as \ooo octal
//     escapes, as Route 53 lists them, so "*" reads back as "\052".
//     ListHostedZonesByName and ListResourceRecordSets order by name with
//     its labels reversed, comparing label by label, then by hosted zone ID
//     or record type.
//   - Tags apply only to hosted zones: a healthcheck resource fails with
//     NoSuchHealthCheck. A call adds or removes up to 10 tags, and a zone
//     holds up to 50; keys are 1 to 128 characters and values up to 256.
//   - Query parameters the server does not support are rejected with 400
//     InvalidInput rather than ignored.
//
// [WithPolicy] grants the caller a set of IAM actions and refuses a call that
// needs one it lacks with 403 AccessDenied, before any fault. Each operation
// needs route53: and its name, and CreateHostedZone, AssociateVPCWithHostedZone
// and DisassociateVPCFromHostedZone also need ec2:DescribeVpcs, with
// CreateHostedZone needing route53:AssociateVPCWithHostedZone as well, as the
// Route 53 permissions reference lists for private zones. Actions are not
// checked against the resources they act on or against condition keys: a
// grant covers every resource.
//
// Fault method names are "route53." and the operation, such as
// "route53.CreateHostedZone". A call's resource is the hosted zone ID it acts
// on, without the "/hostedzone/" prefix, the zone's name for
// CreateHostedZone, such as "example.internal.", the change ID for GetChange,
// and empty for ListHostedZonesByName. Route 53 has no long-running
// operations, so [fault.ModeFailOperation] fails a call as [fault.ModeReject]
// does. A fault's Reason is the error code, except that the fault package's
// generic reasons become InternalFailure, ServiceUnavailable (for 503) and
// Throttling (always 400, as Route 53 throttles).
//
// A lost response aborts the handler with [http.ErrAbortHandler] after
// sending the response header, so the [http.Server] serving it closes the
// connection. Calling ServeHTTP directly propagates that panic.
package route53

import (
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/parallelworks/squall/access"
	"github.com/parallelworks/squall/fault"
	"github.com/parallelworks/squall/internal/stub"
)

const (
	apiVersion = "2013-04-01"
	namespace  = "https://route53.amazonaws.com/doc/" + apiVersion + "/"
	prefix     = "/" + apiVersion + "/"

	maxBody    = 1 << 20
	maxChanges = 10_000
)

// Server is a stand-in for the Amazon Route 53 API. Create one with [New]. It
// is safe for concurrent use.
type Server struct {
	faults   *fault.Injector
	policy   *access.Policy
	vpcs     func(region, vpcID string) bool
	now      func() time.Time
	requests atomic.Uint64

	mu         sync.Mutex
	zones      map[string]*zone
	references map[string]string
	changes    map[string]change
	changeIDs  []string
	nextID     uint64
}

// Option configures a [Server].
type Option func(*Server)

// WithFaults makes the server ask in which calls to fail.
func WithFaults(in *fault.Injector) Option {
	return func(s *Server) { s.faults = in }
}

// WithClock sets the clock for change submission times. The default is time.Now.
func WithClock(now func() time.Time) Option {
	return func(s *Server) { s.now = now }
}

// WithVPCLookup makes the server accept only the VPCs exists reports, by
// region and VPC ID, and fail others with InvalidVPCId. Without it every VPC
// exists.
func WithVPCLookup(exists func(region, vpcID string) bool) Option {
	return func(s *Server) { s.vpcs = exists }
}

// New returns an empty server.
func New(opts ...Option) *Server {
	s := &Server{now: time.Now, zones: map[string]*zone{}, references: map[string]string{}, changes: map[string]change{}}
	for _, opt := range opts {
		opt(s)
	}
	return s
}

// call is one parsed request.
type call struct {
	r     *http.Request
	query url.Values
	body  []byte
	// id is the hosted zone, change or tagged resource ID in the path.
	id string
	// resourceType is the tagged resource's type.
	resourceType string
}

// reply is a successful answer: a status, headers and an XML body.
type reply struct {
	status int
	header map[string]string
	body   any
}

type operation struct {
	name string
	// params are the query parameters the operation takes.
	params []string
	// target names the resource a call acts on, for faults and policies.
	target func(c *call) string
	handle func(c *call) (*reply, *apiError)
}

func byID(c *call) string { return c.id }

// route picks the operation for a method and the path below /2013-04-01/.
func (s *Server) route(method, path string, c *call) (operation, bool) {
	parts := strings.Split(path, "/")
	switch {
	case method == http.MethodPost && path == "hostedzone":
		return operation{"CreateHostedZone", nil, createTarget, s.createHostedZone}, true
	case method == http.MethodGet && path == "hostedzonesbyname":
		return operation{"ListHostedZonesByName", []string{"dnsname", "hostedzoneid", "maxitems"}, func(*call) string { return "" }, s.listHostedZonesByName}, true
	case len(parts) == 2 && parts[0] == "change" && method == http.MethodGet:
		c.id = parts[1]
		return operation{"GetChange", nil, byID, s.getChange}, true
	case len(parts) >= 3 && parts[0] == "tags":
		c.resourceType, c.id = parts[1], strings.Join(parts[2:], "/")
		switch method {
		case http.MethodPost:
			return operation{"ChangeTagsForResource", nil, byID, s.changeTags}, true
		case http.MethodGet:
			return operation{"ListTagsForResource", nil, byID, s.listTags}, true
		}
	case len(parts) >= 2 && parts[0] == "hostedzone":
		c.id = parts[1]
		sub := strings.Join(parts[2:], "/")
		switch method + " " + sub {
		case "GET ":
			return operation{"GetHostedZone", nil, byID, s.getHostedZone}, true
		case "DELETE ":
			return operation{"DeleteHostedZone", nil, byID, s.deleteHostedZone}, true
		case "POST associatevpc":
			return operation{"AssociateVPCWithHostedZone", nil, byID, s.associateVPC}, true
		case "POST disassociatevpc":
			return operation{"DisassociateVPCFromHostedZone", nil, byID, s.disassociateVPC}, true
		case "POST rrset", "POST rrset/":
			return operation{"ChangeResourceRecordSets", nil, byID, s.changeRecordSets}, true
		case "GET rrset", "GET rrset/":
			return operation{"ListResourceRecordSets", []string{"name", "type", "identifier", "maxitems"}, byID, s.listRecordSets}, true
		}
	}
	return operation{}, false
}

// ServeHTTP answers a Route 53 REST call.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	requestID := s.requestID()
	w.Header().Set("x-amzn-RequestId", requestID)
	op, c, err := s.parse(w, r)
	if err != nil {
		writeError(w, requestID, err)
		return
	}
	resource := op.target(c)
	if s.policy != nil {
		if missing := s.policy.Check(access.Call{Method: "route53." + op.name, Resource: resource, Needs: needs(op.name)}); missing != nil {
			writeError(w, requestID, notAuthorized(missing[0], op.name, resource))
			return
		}
	}
	f, _ := s.faults.Decide(fault.Call{Method: "route53." + op.name, Resource: resource})
	reject := f.Mode == fault.ModeReject || f.Mode == fault.ModeFailOperation
	stub.Serve(w, r, f, reject, "text/xml", func() { writeError(w, requestID, faultError(f)) }, func() (func(), bool) {
		rep, err := op.handle(c)
		if err != nil {
			return func() { writeError(w, requestID, err) }, false
		}
		return func() { rep.write(w) }, true
	})
}

func (s *Server) parse(w http.ResponseWriter, r *http.Request) (operation, *call, *apiError) {
	path, ok := strings.CutPrefix(r.URL.Path, prefix)
	if !ok {
		return operation{}, nil, notImplemented(r.Method + " " + r.URL.Path)
	}
	c := &call{r: r, query: r.URL.Query()}
	op, ok := s.route(r.Method, path, c)
	if !ok {
		return operation{}, nil, notImplemented(r.Method + " " + r.URL.Path)
	}
	for name := range c.query {
		if !slices.Contains(op.params, name) {
			return operation{}, nil, invalidInput("squall: %s does not support the query parameter %s", op.name, name)
		}
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBody))
	if _, ok := errors.AsType[*http.MaxBytesError](err); ok {
		return operation{}, nil, invalidInput("squall: bodies are limited to %d bytes", maxBody)
	}
	if err != nil {
		return operation{}, nil, invalidInput("The request body could not be read: %v", err)
	}
	c.body = body
	return op, c, nil
}

func (rep *reply) write(w http.ResponseWriter) {
	for k, v := range rep.header {
		w.Header().Set(k, v)
	}
	writeXML(w, max(rep.status, http.StatusOK), rep.body)
}

func writeXML(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "text/xml")
	w.WriteHeader(status)
	_, _ = w.Write([]byte(xml.Header))
	// The status is already sent, so a client that went away is all an error here can mean.
	_ = xml.NewEncoder(w).Encode(body)
}

// decodeXML reads an XML request body into v.
func decodeXML(body []byte, v any) *apiError {
	//nolint:gosec // v is a fixed struct and the body is bounded by maxBody
	if err := xml.Unmarshal(body, v); err != nil {
		return invalidInput("Invalid XML ; %v", err)
	}
	return nil
}

func (s *Server) requestID() string {
	n := s.requests.Add(1)
	return fmt.Sprintf("%08x-0000-4000-8000-%012x", n>>48, n&(1<<48-1))
}

// newID returns a fresh ID, prefix and 20 uppercase letters and digits, as Route 53's hosted zone and change IDs are. The caller holds s.mu.
func (s *Server) newID(prefix string) string {
	s.nextID++
	const digits = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZ"
	var b [20]byte
	n := s.nextID
	for i := len(b) - 1; i >= 0; i-- {
		b[i] = digits[n%36]
		n /= 36
	}
	return prefix + string(b[:])
}
