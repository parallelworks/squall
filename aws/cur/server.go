// Package cur is a stand-in for the AWS Cost and Usage Reports API, for
// testing code that defines billing reports through the official SDKs.
//
// [Server] is an [http.Handler] that keeps report definitions in memory and
// answers the AWS JSON 1.1 protocol, with the operation in the X-Amz-Target
// header (AWSOrigamiServiceGatewayService.PutReportDefinition, say). Serve it
// with [net/http/httptest] and point a client at it with the SDK's base
// endpoint setting, or the AWS_ENDPOINT_URL_COST_AND_USAGE_REPORT_SERVICE or
// AWS_ENDPOINT_URL environment variable, and any static credentials. A
// [fault.Injector] makes chosen calls fail, including after they took effect,
// so tests can check that code which retries converges.
//
// It covers report definitions (PutReportDefinition,
// DescribeReportDefinitions, DeleteReportDefinition), with the real API's
// error types. Other operations, ModifyReportDefinition and the tag
// operations among them, answer 404 with the code UnknownOperationException.
// Where it differs from the real API:
//
//   - Signatures are not checked, and a request needs none. There is one
//     account, and the request's region is not checked.
//   - PutReportDefinition checks the report definition's fields against the
//     API reference's patterns, lengths and enumerations, and that Parquet
//     format goes with Parquet compression and textORcsv format with ZIP or
//     GZIP, failing with ValidationException in the "N validation errors
//     detected" form. The combinations of AdditionalArtifacts, schema
//     elements, time units and report versioning the user guide describes are
//     not checked; those fields are recorded as sent. A ReportStatus in the
//     request is ignored, and definitions are described without one, as for
//     a report never delivered. Tags are checked for count (up to 200) and
//     length and recorded, but not returned by any operation.
//   - An existing name fails with DuplicateReportNameException, and a sixth
//     definition with ReportLimitReachedException, as the error's
//     documentation says the account holds five; [WithReportLimit] changes
//     the limit.
//   - [WithBucketPolicy] makes PutReportDefinition verify the S3 bucket as
//     the real service does, failing with ValidationException "Failed to
//     verify customer bucket permission." when the bucket does not exist or
//     its policy does not let the service principal
//     billingreports.amazonaws.com call s3:GetBucketAcl and
//     s3:GetBucketPolicy on the bucket and s3:PutObject on every object in
//     it. The policy is read for Allow statements naming the service, or
//     "*", as principal, with actions and resources matched with IAM's * and
//     ? wildcards, and a Deny statement that matches overrides them.
//     Conditions are not evaluated, statements with NotPrincipal, NotAction
//     or NotResource are skipped, and the bucket's region is not compared
//     with S3Region. Without the option every bucket passes.
//   - DescribeReportDefinitions lists definitions ordered by name. MaxResults
//     is fixed at 5, as the API reference says, and any other value fails with
//     ValidationException. NextToken is the stand-in's own opaque token.
//   - DeleteReportDefinition succeeds with a ResponseMessage whether or not
//     the report exists, as the API reference lists no not-found error for it.
//     The message's wording is the stand-in's own.
//   - Request bodies must be JSON objects of the operation's input, up to
//     1 MiB; unknown members are rejected with SerializationException rather
//     than ignored.
//
// [WithPolicy] grants the caller a set of IAM actions and refuses a call that
// needs one it lacks with 403 AccessDeniedException, before any fault. Each
// operation needs cur: and its name, such as cur:PutReportDefinition. Actions
// are not checked against the resources they act on or against condition
// keys: a grant covers every report.
//
// Fault method names are "cur." and the operation, such as
// "cur.PutReportDefinition". A call's resource is the report's name, and
// empty for DescribeReportDefinitions. The API has no long-running
// operations, so [fault.ModeFailOperation] fails a call as [fault.ModeReject]
// does. A fault's Reason is the error code, except that the fault package's
// generic reasons become InternalErrorException (500), ServiceUnavailable
// (503) and ThrottlingException (always 400).
//
// A lost response aborts the handler with [http.ErrAbortHandler] after
// sending the response header, so the [http.Server] serving it closes the
// connection. Calling ServeHTTP directly propagates that panic.
package cur

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/parallelworks/squall/access"
	"github.com/parallelworks/squall/fault"
	"github.com/parallelworks/squall/internal/stub"
)

const (
	targetPrefix = "AWSOrigamiServiceGatewayService."
	contentType  = "application/x-amz-json-1.1"

	maxBody = 1 << 20

	defaultReportLimit = 5
)

// Server is a stand-in for the AWS Cost and Usage Reports API. Create one
// with [New]. It is safe for concurrent use.
type Server struct {
	faults   *fault.Injector
	policy   *access.Policy
	buckets  func(bucket string) (policy string, exists bool)
	limit    int
	requests atomic.Uint64

	mu      sync.Mutex
	reports map[string]*ReportDefinition
}

// Option configures a [Server].
type Option func(*Server)

// WithFaults makes the server ask in which calls to fail.
func WithFaults(in *fault.Injector) Option {
	return func(s *Server) { s.faults = in }
}

// WithBucketPolicy makes PutReportDefinition verify a report's S3 bucket:
// lookup reports whether the bucket exists and its policy JSON, empty for
// none. A bucket that does not exist, or whose policy does not grant the
// service principal billingreports.amazonaws.com what report delivery
// needs, fails with ValidationException. Without it every bucket passes. An
// [github.com/parallelworks/squall/aws/s3] server's LookupBucket supplies
// both.
func WithBucketPolicy(lookup func(bucket string) (policy string, exists bool)) Option {
	return func(s *Server) { s.buckets = lookup }
}

// WithReportLimit sets how many report definitions the account may hold
// before PutReportDefinition fails with ReportLimitReachedException. The
// default is 5.
func WithReportLimit(n int) Option {
	return func(s *Server) { s.limit = n }
}

// New returns an empty server.
func New(opts ...Option) *Server {
	s := &Server{limit: defaultReportLimit, reports: map[string]*ReportDefinition{}}
	for _, opt := range opts {
		opt(s)
	}
	return s
}

type operation struct {
	name string
	// input returns a fresh value to decode the request body into.
	input func() any
	// target names the report a call acts on, for faults.
	target func(in any) string
	handle func(in any) (any, *apiError)
}

func (s *Server) operation(name string) (operation, bool) {
	switch name {
	case "PutReportDefinition":
		return operation{name, func() any { return new(putInput) }, func(in any) string {
			if d := in.(*putInput).ReportDefinition; d != nil && d.ReportName != nil {
				return *d.ReportName
			}
			return ""
		}, s.putReportDefinition}, true
	case "DescribeReportDefinitions":
		return operation{name, func() any { return new(describeInput) }, func(any) string { return "" }, s.describeReportDefinitions}, true
	case "DeleteReportDefinition":
		return operation{name, func() any { return new(deleteInput) }, func(in any) string {
			if n := in.(*deleteInput).ReportName; n != nil {
				return *n
			}
			return ""
		}, s.deleteReportDefinition}, true
	}
	return operation{}, false
}

// ServeHTTP answers a Cost and Usage Reports call.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	requestID := s.requestID()
	w.Header().Set("x-amzn-RequestId", requestID)
	op, in, err := s.parse(w, r)
	if err != nil {
		writeError(w, err)
		return
	}
	resource := op.target(in)
	if s.policy != nil {
		action := "cur:" + op.name
		if missing := s.policy.Check(access.Call{Method: "cur." + op.name, Resource: resource, Needs: []string{action}}); missing != nil {
			writeError(w, notAuthorized(missing[0]))
			return
		}
	}
	f, _ := s.faults.Decide(fault.Call{Method: "cur." + op.name, Resource: resource})
	reject := f.Mode == fault.ModeReject || f.Mode == fault.ModeFailOperation
	stub.Serve(w, r, f, reject, contentType, func() { writeError(w, faultError(f)) }, func() (func(), bool) {
		out, err := op.handle(in)
		if err != nil {
			return func() { writeError(w, err) }, false
		}
		return func() { writeJSON(w, http.StatusOK, out) }, true
	})
}

func (s *Server) parse(w http.ResponseWriter, r *http.Request) (operation, any, *apiError) {
	name, ok := strings.CutPrefix(r.Header.Get("X-Amz-Target"), targetPrefix)
	if r.Method != http.MethodPost || r.URL.Path != "/" || !ok {
		return operation{}, nil, unknownOperation(r.Method + " " + r.URL.Path + " " + r.Header.Get("X-Amz-Target"))
	}
	op, ok := s.operation(name)
	if !ok {
		return operation{}, nil, unknownOperation(name)
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBody))
	if _, ok := errors.AsType[*http.MaxBytesError](err); ok {
		return operation{}, nil, &apiError{Status: http.StatusRequestEntityTooLarge, Code: "RequestEntityTooLargeException", Message: fmt.Sprintf("squall: bodies are limited to %d bytes", maxBody)}
	}
	if err != nil {
		return operation{}, nil, serializationError("The request body could not be read: %v", err)
	}
	in := op.input()
	if len(bytes.TrimSpace(body)) == 0 {
		body = []byte("{}")
	}
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	if err := dec.Decode(in); err != nil {
		return operation{}, nil, serializationError("squall: %s could not read the request body: %v", op.name, err)
	}
	if dec.More() {
		return operation{}, nil, serializationError("squall: %s found more than one JSON value in the request body", op.name)
	}
	return op, in, nil
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", contentType)
	w.WriteHeader(status)
	// The status is already sent, so a client that went away is all an error here can mean.
	_ = json.NewEncoder(w).Encode(body)
}

func (s *Server) requestID() string {
	n := s.requests.Add(1)
	return fmt.Sprintf("%08x-0000-4000-8000-%012x", n>>48, n&(1<<48-1))
}
