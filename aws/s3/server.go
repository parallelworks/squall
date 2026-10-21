// Package s3 is a stand-in for the Amazon S3 API, for testing code that
// calls it through the official SDKs.
//
// [Server] is an [http.Handler] that keeps general purpose buckets and their
// object versions in memory and answers the S3 REST API with path-style
// addressing, in S3's XML. Serve it with [net/http/httptest] and point a
// client at it with the SDK's base endpoint setting, or the
// AWS_ENDPOINT_URL_S3 or AWS_ENDPOINT_URL environment variable, and any
// static credentials. The SDK addresses an IP endpoint such as httptest's by
// path; for a host name, set the client's UsePathStyle. A [fault.Injector]
// makes chosen calls fail, including after they took effect, so tests can
// check that code which retries converges.
//
// It covers buckets (CreateBucket, HeadBucket, ListBuckets, DeleteBucket),
// their tags (PutBucketTagging, GetBucketTagging, DeleteBucketTagging),
// versioning (PutBucketVersioning, GetBucketVersioning), policy
// (PutBucketPolicy) and CORS rules (PutBucketCors), and the objects in them
// (PutObject, DeleteObject, DeleteObjects, ListObjectVersions), with the real
// API's error codes. Other operations answer 404 with the code
// NotImplemented. Where it differs from S3:
//
//   - Signatures are not checked. The region comes from the credential scope
//     of the SigV4 Authorization header (or the X-Amz-Credential query
//     parameter); a request with neither fails with AccessDenied. There is
//     one account. Bucket names are global, as in S3: a call on a bucket in
//     another region than the request's fails with 301 PermanentRedirect and
//     the bucket's region in the x-amz-bucket-region header, which the SDK
//     does not follow.
//   - [Server.PutForeignBucket] holds a name for another account: creating
//     it fails with BucketAlreadyExists, HeadBucket with 403, and every other
//     call on it with AccessDenied. ListBuckets lists only the account's own.
//     Other access denials are a fault rule away, such as
//     fault.Reject(403, "AccessDenied").
//   - CreateBucket takes a LocationConstraint that must name the request's
//     region, and none for us-east-1, where naming us-east-1 fails with
//     InvalidLocationConstraint. Any other mismatch fails with
//     IllegalLocationConstraintException. Creating a bucket the account
//     already owns succeeds in us-east-1 and fails with
//     BucketAlreadyOwnedByYou elsewhere. Object ownership is always bucket
//     owner enforced: an ACL other than private fails, and object lock and
//     directory buckets are not supported.
//   - ListBuckets returns the account's buckets ordered by name, in every
//     region, and filters by prefix and bucket-region. It reports each
//     bucket's region only when max-buckets is set, and pages only then.
//   - Objects record their size and ETag, the hex MD5 of their data, not the
//     data itself. Checksums and Content-MD5 are not verified, and bodies
//     sent with aws-chunked encoding are decoded without checking their
//     chunk signatures. Version IDs are 32 hex digits, and "null" for an
//     object written while versioning is off or suspended. Multipart uploads,
//     copies, storage classes, object tags, lifecycle and replication are not
//     supported, and an incomplete multipart upload never blocks DeleteBucket.
//   - DeleteBucket fails with BucketNotEmpty while any object version or
//     delete marker remains. ListObjectVersions orders versions by key and
//     then newest first, and its markers are positions, so a page may follow
//     one whose versions were deleted; it takes no delimiter or
//     encoding-type.
//   - Tags follow S3's rules: up to 50, keys of 1 to 128 characters, values
//     of up to 256, letters, digits, spaces and "+-=._:/@", no duplicates
//     and no "aws:" prefix. PutBucketTagging with an empty tag set removes
//     the tags, so GetBucketTagging fails with NoSuchTagSet, as it does for
//     a bucket never tagged. Bucket policies must be JSON and are recorded,
//     not enforced; CORS rules are checked for an allowed method and origin
//     and recorded.
//   - Query parameters the server does not support are rejected with 400
//     InvalidArgument rather than ignored, as are the
//     x-amz-expected-bucket-owner and x-amz-mfa headers.
//
// Fault method names are "s3." and the operation, such as "s3.CreateBucket".
// A call's resource is the bucket's name, "bucket/key" for a call on one
// object, and empty for ListBuckets. S3 has no long-running operations, so
// [fault.ModeFailOperation] fails a call as [fault.ModeReject] does. A
// fault's Reason is the error code, except that the fault package's generic
// reasons become InternalError, ServiceUnavailable (for 503) and SlowDown
// (always 503).
//
// A lost response aborts the handler with [http.ErrAbortHandler] after
// sending the response header, so the [http.Server] serving it closes the
// connection. Calling ServeHTTP directly propagates that panic. A HEAD's
// header is its whole answer, so a lost one instead ends the connection
// inside its status line.
package s3

import (
	"bytes"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/parallelworks/squall/fault"
	"github.com/parallelworks/squall/internal/stub"
)

const (
	namespace  = "http://s3.amazonaws.com/doc/2006-03-01/"
	timeFormat = "2006-01-02T15:04:05.000Z"

	maxBody   = 1 << 20
	maxObject = 16 << 20

	// ownerID is the account's canonical user ID.
	ownerID = "75aa57f09aa0c8caeab4f8c24e99d10f8e7faeebf76c078efc7c6caea54ba06a"
)

// Server is a stand-in for the Amazon S3 API. Create one with [New]. It is safe for concurrent use.
type Server struct {
	faults   *fault.Injector
	now      func() time.Time
	requests atomic.Uint64

	ops map[string]operation

	mu          sync.Mutex
	buckets     map[string]*bucket
	foreign     map[string]string
	nextVersion uint64
}

// Option configures a [Server].
type Option func(*Server)

// WithFaults makes the server ask in which calls to fail.
func WithFaults(in *fault.Injector) Option {
	return func(s *Server) { s.faults = in }
}

// WithClock sets the clock for creation and modification times. The default is time.Now.
func WithClock(now func() time.Time) Option {
	return func(s *Server) { s.now = now }
}

// New returns an empty server.
func New(opts ...Option) *Server {
	s := &Server{now: time.Now, buckets: map[string]*bucket{}, foreign: map[string]string{}}
	for _, opt := range opts {
		opt(s)
	}
	s.ops = s.operations()
	return s
}

// call is one parsed request.
type call struct {
	r      *http.Request
	region string
	bucket string
	key    string
	query  url.Values
	body   []byte
}

func (c *call) resource() string {
	if c.key != "" {
		return c.bucket + "/" + c.key
	}
	return c.bucket
}

// reply is a successful answer: a status, headers, and an XML body or none.
type reply struct {
	status int
	header map[string]string
	body   any
}

type operation struct {
	name string
	// params are the query parameters the operation takes, besides its subresource.
	params []string
	handle func(c *call) (*reply, *apiError)
}

// subresources are the query parameters that select an S3 operation.
var subresources = []string{
	"accelerate", "acl", "analytics", "attributes", "cors", "delete", "encryption", "intelligent-tiering",
	"inventory", "legal-hold", "lifecycle", "list-type", "location", "logging", "metadataConfiguration",
	"metadataTable", "metrics", "notification", "object-lock", "ownershipControls", "partNumber", "policy",
	"policyStatus", "publicAccessBlock", "renameObject", "replication", "requestPayment", "restore", "retention",
	"select", "session", "tagging", "torrent", "uploadId", "uploads", "versioning", "versions", "website",
}

// operations maps a method, scope and subresource to its operation; an empty one is not implemented.
func (s *Server) operations() map[string]operation {
	return map[string]operation{
		"GET service":            {"ListBuckets", []string{"max-buckets", "continuation-token", "prefix", "bucket-region"}, s.listBuckets},
		"PUT bucket":             {"CreateBucket", nil, s.createBucket},
		"HEAD bucket":            {"HeadBucket", nil, s.headBucket},
		"DELETE bucket":          {"DeleteBucket", nil, s.deleteBucket},
		"GET bucket?tagging":     {"GetBucketTagging", nil, s.getBucketTagging},
		"PUT bucket?tagging":     {"PutBucketTagging", nil, s.putBucketTagging},
		"DELETE bucket?tagging":  {"DeleteBucketTagging", nil, s.deleteBucketTagging},
		"GET bucket?versioning":  {"GetBucketVersioning", nil, s.getBucketVersioning},
		"PUT bucket?versioning":  {"PutBucketVersioning", nil, s.putBucketVersioning},
		"PUT bucket?policy":      {"PutBucketPolicy", nil, s.putBucketPolicy},
		"PUT bucket?cors":        {"PutBucketCors", nil, s.putBucketCors},
		"GET bucket?versions":    {"ListObjectVersions", []string{"prefix", "key-marker", "version-id-marker", "max-keys"}, s.listObjectVersions},
		"POST bucket?delete":     {"DeleteObjects", nil, s.deleteObjects},
		"PUT object":             {"PutObject", nil, s.putObject},
		"DELETE object":          {"DeleteObject", []string{"versionId"}, s.deleteObject},
		"HEAD service":           {},
		"PUT service":            {},
		"DELETE service":         {},
		"POST service":           {},
		"GET bucket?location":    {},
		"GET bucket?list-type":   {},
		"GET object":             {},
		"HEAD object":            {},
		"POST object?uploads":    {},
		"GET bucket?uploads":     {},
		"PUT object?partNumber":  {},
		"DELETE object?uploadId": {},
	}
}

// operation picks the operation for c by its HTTP method, scope and subresource.
func (s *Server) operation(method string, c *call) (operation, *apiError) {
	sub := ""
	for _, name := range subresources {
		if c.query.Has(name) {
			sub = name
			break
		}
	}
	scope := "object"
	switch {
	case c.bucket == "":
		scope = "service"
	case c.key == "":
		scope = "bucket"
	}
	key := method + " " + scope
	if sub != "" {
		key += "?" + sub
	}
	op, ok := s.ops[key]
	if !ok || op.name == "" {
		what := key
		if c.bucket != "" {
			what += " " + c.resource()
		}
		return operation{}, &apiError{Status: http.StatusNotFound, Code: "NotImplemented", Message: "squall: " + what + " is not implemented"}
	}
	for name := range c.query {
		presign := len(name) > 6 && strings.EqualFold(name[:6], "X-Amz-")
		if name != sub && name != "x-id" && !presign && !slices.Contains(op.params, name) {
			return operation{}, invalidArgument("squall: %s does not support the query parameter %s", op.name, name)
		}
	}
	return op, nil
}

// ServeHTTP answers an S3 REST call.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	requestID := s.requestID()
	w.Header().Set("x-amz-request-id", requestID)
	w.Header().Set("x-amz-id-2", "squall/"+requestID)
	op, c, err := s.parse(w, r)
	if err != nil {
		writeError(w, r, requestID, err)
		return
	}
	f, _ := s.faults.Decide(fault.Call{Method: "s3." + op.name, Resource: c.resource()})
	reject := f.Mode == fault.ModeReject || f.Mode == fault.ModeFailOperation
	stub.Serve(w, r, f, reject, "application/xml", func() { writeError(w, r, requestID, faultError(f)) }, func() (func(), bool) {
		rep, err := op.handle(c)
		if err != nil {
			return func() { writeError(w, r, requestID, err) }, false
		}
		return func() { rep.write(w) }, true
	})
}

func (s *Server) parse(w http.ResponseWriter, r *http.Request) (operation, *call, *apiError) {
	region, err := regionOf(r)
	if err != nil {
		return operation{}, nil, err
	}
	bucket, key, _ := strings.Cut(strings.TrimPrefix(r.URL.Path, "/"), "/")
	c := &call{r: r, region: region, bucket: bucket, key: key, query: r.URL.Query()}
	op, err := s.operation(r.Method, c)
	if err != nil {
		return operation{}, nil, err
	}
	for _, h := range []string{"x-amz-expected-bucket-owner", "x-amz-mfa"} {
		if r.Header.Get(h) != "" {
			return operation{}, nil, invalidArgument("squall: the %s header is not supported", h)
		}
	}
	limit := int64(maxBody)
	if op.name == "PutObject" {
		limit = maxObject
	}
	c.body, err = readBody(w, r, limit)
	if err != nil {
		return operation{}, nil, err
	}
	return op, c, nil
}

// regionOf reads the region from the SigV4 credential scope without checking the signature.
func regionOf(r *http.Request) (string, *apiError) {
	credential := r.URL.Query().Get("X-Amz-Credential")
	if h := r.Header.Get("Authorization"); h != "" {
		algorithm, fields, _ := strings.Cut(h, " ")
		if algorithm != "AWS4-HMAC-SHA256" {
			return "", invalidArgument("Unsupported Authorization Type")
		}
		for field := range strings.SplitSeq(fields, ",") {
			if v, ok := strings.CutPrefix(strings.TrimSpace(field), "Credential="); ok {
				credential = v
			}
		}
	}
	if credential == "" {
		return "", accessDenied("")
	}
	scope := strings.Split(credential, "/")
	if len(scope) != 5 || scope[2] == "" || scope[4] != "aws4_request" {
		return "", &apiError{
			Status: http.StatusBadRequest, Code: "AuthorizationHeaderMalformed",
			Message: fmt.Sprintf("The authorization header is malformed; the Credential is mal-formed; expecting \"<YOUR-AKID>/YYYYMMDD/REGION/SERVICE/aws4_request\". (%s)", credential),
		}
	}
	return scope[2], nil
}

// readBody reads r's body, decoding aws-chunked encoding, up to limit bytes.
func readBody(w http.ResponseWriter, r *http.Request, limit int64) ([]byte, *apiError) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, limit+limit/8+4096))
	if err != nil {
		return nil, &apiError{Status: http.StatusBadRequest, Code: "IncompleteBody", Message: "The request body could not be read: " + err.Error()}
	}
	chunked := strings.Contains(r.Header.Get("Content-Encoding"), "aws-chunked") ||
		strings.HasPrefix(r.Header.Get("X-Amz-Content-Sha256"), "STREAMING-")
	if chunked {
		if body, err = decodeChunked(body); err != nil {
			return nil, &apiError{Status: http.StatusBadRequest, Code: "IncompleteBody", Message: "The aws-chunked body is malformed: " + err.Error()}
		}
	}
	if int64(len(body)) > limit {
		return nil, &apiError{Status: http.StatusBadRequest, Code: "EntityTooLarge", Message: fmt.Sprintf("squall: bodies are limited to %d bytes", limit)}
	}
	return body, nil
}

// decodeChunked decodes an aws-chunked body, dropping chunk extensions and trailers.
func decodeChunked(b []byte) ([]byte, error) {
	var out []byte
	for {
		line, rest, ok := bytes.Cut(b, []byte("\r\n"))
		if !ok {
			return nil, io.ErrUnexpectedEOF
		}
		sizeHex, _, _ := bytes.Cut(line, []byte(";"))
		var size int
		if _, err := fmt.Sscanf(string(sizeHex), "%x", &size); err != nil || size < 0 {
			return nil, fmt.Errorf("bad chunk size %q", sizeHex)
		}
		if size == 0 {
			return out, nil
		}
		if len(rest) < size+2 || string(rest[size:size+2]) != "\r\n" {
			return nil, io.ErrUnexpectedEOF
		}
		out = append(out, rest[:size]...)
		b = rest[size+2:]
	}
}

func (rep *reply) write(w http.ResponseWriter) {
	for k, v := range rep.header {
		w.Header().Set(k, v)
	}
	status := rep.status
	if status == 0 {
		status = http.StatusOK
	}
	if rep.body == nil {
		w.WriteHeader(status)
		return
	}
	writeXML(w, status, rep.body)
}

func writeXML(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/xml")
	w.WriteHeader(status)
	_, _ = w.Write([]byte(xml.Header))
	// The status is already sent, so a client that went away is all an error here can mean.
	_ = xml.NewEncoder(w).Encode(body)
}

// requestID returns a fresh request ID in the 16 hex digit form S3 uses.
func (s *Server) requestID() string {
	return fmt.Sprintf("%016X", s.requests.Add(1))
}

// decodeXML reads an XML request body into v.
func decodeXML(body []byte, v any) *apiError {
	//nolint:gosec // v is a fixed struct and the body is bounded by maxBody
	if err := xml.Unmarshal(body, v); err != nil {
		return malformedXML()
	}
	return nil
}
