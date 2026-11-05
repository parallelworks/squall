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
// (PutBucketPolicy), CORS rules (PutBucketCors), Object Lock configuration
// (PutObjectLockConfiguration, GetObjectLockConfiguration) and request
// payment (PutBucketRequestPayment, GetBucketRequestPayment), the objects in
// them (PutObject, DeleteObject, DeleteObjects, ListObjectsV2,
// ListObjectVersions) with their retention (PutObjectRetention,
// GetObjectRetention) and legal holds (PutObjectLegalHold,
// GetObjectLegalHold), and multipart uploads (CreateMultipartUpload,
// UploadPart, CompleteMultipartUpload, AbortMultipartUpload,
// ListMultipartUploads, ListParts), with the real API's error codes. Other
// operations answer 404 with the code NotImplemented. Where it differs from
// S3:
//
//   - Signatures are not checked. The region comes from the credential scope
//     of the SigV4 Authorization header (or the X-Amz-Credential query
//     parameter); a request with neither fails with AccessDenied. There is
//     one account. Bucket names are global, as in S3: a call on a bucket in
//     another region than the request's fails with 301 PermanentRedirect and
//     the bucket's region in the x-amz-bucket-region header, which the SDK
//     does not follow.
//   - [Server.PutForeignBucket] holds a name for another account: creating
//     it fails with BucketAlreadyExists, HeadBucket with 403 and the bucket's
//     region, and every other call on it with AccessDenied. ListBuckets
//     lists only the account's own. [WithPolicy] denies calls by IAM
//     action, and other access denials are a fault rule away, such as
//     fault.Reject(403, "AccessDenied").
//   - CreateBucket takes a LocationConstraint that must name the request's
//     region, and none for us-east-1, where naming us-east-1 fails with
//     InvalidLocationConstraint. Any other mismatch fails with
//     IllegalLocationConstraintException. Creating a bucket the account
//     already owns succeeds in us-east-1 and fails with
//     BucketAlreadyOwnedByYou elsewhere. Object ownership is always bucket
//     owner enforced: an ACL other than private fails, and directory
//     buckets are not supported.
//   - ListBuckets returns the account's buckets ordered by name, in every
//     region, and filters by prefix and bucket-region, which must be the
//     request's region. As in S3, a request with any of its parameters pages,
//     10,000 buckets at a time unless max-buckets says otherwise, and reports
//     each bucket's region; one with none returns every bucket without regions.
//   - Objects record their size and ETag, the hex MD5 of their data, not the
//     data itself, and keys are up to 1024 bytes. Objects are up to 16 MiB
//     and other bodies up to 1 MiB. Checksums and Content-MD5 are not
//     verified, and bodies sent with aws-chunked encoding are decoded without
//     checking their chunk signatures. Version IDs are 32 hex digits, and
//     "null" for an object written while versioning is off or suspended.
//     Copies, storage classes, object tags, lifecycle and replication are
//     not supported.
//   - A multipart upload's parts record their size and ETag like objects,
//     and are up to 16 MiB each. CompleteMultipartUpload checks the parts
//     are in ascending order, were uploaded with the ETags given, and are at
//     least 5 MiB but for the last, and makes a version whose ETag is the MD5
//     of the parts' MD5s and their count, the form S3's take, which it does
//     not document. Completing an upload
//     ends it, so a retry after a lost response fails with NoSuchUpload.
//     Uploads of one key list in the order they began, and upload IDs are
//     32 hex digits; ListMultipartUploads takes S3's prefix, delimiter,
//     markers and max-uploads, and counts common prefixes toward a page as
//     ListObjectsV2 does, which is inferred. A part's checksums are not kept, and ListParts and
//     ListMultipartUploads report no checksum algorithm; UploadPartCopy and
//     the If-Match and If-None-Match headers are not supported. Parts are
//     stored data, which S3 bills until the upload is completed or
//     aborted, so [Server.MultipartUploads] reports them with their sizes.
//     S3 documents in-progress uploads as blocking DeleteBucket only for
//     directory buckets, so deleting a general purpose bucket discards its
//     uploads, and only versions and delete markers block it.
//   - Object Lock follows S3's rules. Creating a bucket with
//     x-amz-bucket-object-lock-enabled turns on versioning, which can then
//     not be suspended (409 InvalidBucketState, whose message is inferred);
//     PutObjectLockConfiguration enables Object Lock on a bucket whose
//     versioning is Enabled, and on one whose versioning is not fails with
//     409 InvalidBucketState, inferred. A default retention needs a mode and
//     one of Days and Years (MalformedXML otherwise), and gives each new
//     version, from PutObject or CompleteMultipartUpload, a retention until
//     that long after the version is made. PutObject and
//     CreateMultipartUpload take a mode and retain-until date together and
//     a legal hold, on a bucket with Object Lock only (400 InvalidRequest,
//     inferred). PutObject that sets a retention, explicitly or by default,
//     needs Content-MD5 or a checksum, as S3 documents; its error message is
//     inferred. Retention runs by the server's clock ([WithClock]).
//     Deleting a version under an unexpired retention or a legal hold fails
//     with 403 AccessDenied, key by key in DeleteObjects; its message is
//     inferred from S3's answers. A COMPLIANCE retention never yields, and a
//     GOVERNANCE one yields to x-amz-bypass-governance-retention: true from a
//     caller [WithPolicy] grants s3:BypassGovernanceRetention. A delete
//     without a version ID adds a delete marker, as S3 does, and DeleteBucket
//     fails while any version remains. PutObjectRetention may extend a
//     retention in the same mode; shortening it, changing its mode or
//     removing it needs the same bypass, which a COMPLIANCE retention
//     refuses. A version with no retention or no legal hold answers
//     GetObjectRetention or GetObjectLegalHold with 404
//     NoSuchObjectLockConfiguration, and a call naming a delete marker's
//     version fails with 405 MethodNotAllowed; both are inferred, as are the
//     InvalidArgument messages for a retain-until date in the past, an
//     unknown mode or legal hold status, and a mode without a date. Event
//     holds, variable retention and default event holds are rejected with
//     400 InvalidArgument; the x-amz-bucket-object-lock-token header is
//     ignored, as S3 no longer needs it. [Server.SetRetention] and
//     [Server.SetLegalHold] lock a version directly, whatever its lock.
//   - Request payment is recorded. There is one account, which owns every
//     bucket it can reach, and S3 does not ask a bucket owner for
//     x-amz-request-payer, so a Requester Pays bucket answers as any other
//     and responses carry no x-amz-request-charged.
//   - DeleteBucket fails with BucketNotEmpty while any object version or
//     delete marker remains. ListObjectVersions orders versions by key and
//     then newest first, and its markers are positions, so a page may follow
//     one whose versions were deleted; it takes no delimiter or
//     encoding-type.
//   - ListObjectsV2 lists each key whose newest version is not a delete
//     marker, in UTF-8 binary order, and rolls keys up into CommonPrefixes
//     by delimiter. Its continuation token encodes the last key or common
//     prefix a page listed, so like ListObjectVersions' markers it is a
//     position. With
//     encoding-type=url, keys and prefixes are query escaped with "/" kept.
//     Owners are the account's canonical ID, without a display name.
//   - Tags follow S3's rules: up to 50, keys of 1 to 128 characters, values
//     of up to 256, Unicode letters, numbers, separators and "+-=._:/@", no
//     duplicates and no "aws:" prefix. PutBucketTagging with an empty tag set removes
//     the tags, so GetBucketTagging fails with NoSuchTagSet, as it does for
//     a bucket never tagged. Bucket policies must be JSON and are recorded,
//     not enforced; CORS rules are checked for an allowed method and origin
//     and recorded.
//   - Query parameters the server does not support are rejected with 400
//     InvalidArgument rather than ignored, as are the
//     x-amz-expected-bucket-owner and x-amz-mfa headers.
//
// [WithPolicy] grants the caller a set of IAM actions and refuses a call that
// needs one it lacks with 403 AccessDenied, before any fault. Each operation
// needs the action the S3 user guide lists for it, such as s3:ListBucket for
// HeadBucket and ListObjectsV2; deleting a version needs s3:DeleteObjectVersion instead of
// s3:DeleteObject, an ACL header on PutObject s3:PutObjectAcl, an object
// ownership header on CreateBucket s3:PutBucketOwnershipControls, and
// enabling Object Lock there s3:PutBucketObjectLockConfiguration and
// s3:PutBucketVersioning. A retention header on PutObject or
// CreateMultipartUpload needs s3:PutObjectRetention and a legal hold header
// s3:PutObjectLegalHold. Bypassing a governance retention needs
// s3:BypassGovernanceRetention, checked only when the bypass decides the
// call, and refused with 403 AccessDenied naming it.
// DeleteObjects is checked key by key, as S3 checks it: a key the policy may
// not delete is an AccessDenied error in the result, and the rest are
// deleted. Actions are not checked against the resources they act on or
// against condition keys: a grant covers every bucket.
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
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/parallelworks/squall/access"
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
	policy   *access.Policy
	now      func() time.Time
	requests atomic.Uint64

	ops map[string]operation

	mu          sync.Mutex
	buckets     map[string]*bucket
	foreign     map[string]string
	nextVersion uint64
	nextUpload  uint64
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
		"GET service":               {"ListBuckets", listBucketsParams, s.listBuckets},
		"PUT bucket":                {"CreateBucket", nil, s.createBucket},
		"HEAD bucket":               {"HeadBucket", nil, s.headBucket},
		"DELETE bucket":             {"DeleteBucket", nil, s.deleteBucket},
		"GET bucket?tagging":        {"GetBucketTagging", nil, s.getBucketTagging},
		"PUT bucket?tagging":        {"PutBucketTagging", nil, s.putBucketTagging},
		"DELETE bucket?tagging":     {"DeleteBucketTagging", nil, s.deleteBucketTagging},
		"GET bucket?versioning":     {"GetBucketVersioning", nil, s.getBucketVersioning},
		"PUT bucket?versioning":     {"PutBucketVersioning", nil, s.putBucketVersioning},
		"PUT bucket?policy":         {"PutBucketPolicy", nil, s.putBucketPolicy},
		"PUT bucket?cors":           {"PutBucketCors", nil, s.putBucketCors},
		"GET bucket?versions":       {"ListObjectVersions", []string{"prefix", "key-marker", "version-id-marker", "max-keys"}, s.listObjectVersions},
		"GET bucket?list-type":      {"ListObjectsV2", listObjectsV2Params, s.listObjectsV2},
		"POST bucket?delete":        {"DeleteObjects", nil, s.deleteObjects},
		"GET bucket?object-lock":    {"GetObjectLockConfiguration", nil, s.getObjectLockConfiguration},
		"PUT bucket?object-lock":    {"PutObjectLockConfiguration", nil, s.putObjectLockConfiguration},
		"GET bucket?requestPayment": {"GetBucketRequestPayment", nil, s.getBucketRequestPayment},
		"PUT bucket?requestPayment": {"PutBucketRequestPayment", nil, s.putBucketRequestPayment},
		"GET bucket?uploads":        {"ListMultipartUploads", listUploadsParams, s.listMultipartUploads},
		"PUT object":                {"PutObject", nil, s.putObject},
		"DELETE object":             {"DeleteObject", []string{"versionId"}, s.deleteObject},
		"POST object?uploads":       {"CreateMultipartUpload", nil, s.createMultipartUpload},
		"PUT object?partNumber":     {"UploadPart", []string{"uploadId"}, s.uploadPart},
		"POST object?uploadId":      {"CompleteMultipartUpload", nil, s.completeMultipartUpload},
		"DELETE object?uploadId":    {"AbortMultipartUpload", nil, s.abortMultipartUpload},
		"GET object?uploadId":       {"ListParts", []string{"max-parts", "part-number-marker"}, s.listParts},
		"GET object?retention":      {"GetObjectRetention", []string{"versionId"}, s.getObjectRetention},
		"PUT object?retention":      {"PutObjectRetention", []string{"versionId"}, s.putObjectRetention},
		"GET object?legal-hold":     {"GetObjectLegalHold", []string{"versionId"}, s.getObjectLegalHold},
		"PUT object?legal-hold":     {"PutObjectLegalHold", []string{"versionId"}, s.putObjectLegalHold},
		"HEAD service":              {},
		"PUT service":               {},
		"DELETE service":            {},
		"POST service":              {},
		"GET bucket?location":       {},
		"GET object":                {},
		"HEAD object":               {},
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
	resource := c.resource()
	if s.policy != nil {
		if perms := needs(op.name, c); perms != nil {
			if missing := s.policy.Check(access.Call{Method: "s3." + op.name, Resource: resource, Needs: perms}); missing != nil {
				writeError(w, r, requestID, notAuthorized(missing[0], resource))
				return
			}
		}
	}
	f, _ := s.faults.Decide(fault.Call{Method: "s3." + op.name, Resource: resource})
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
	limit, tooLarge := int64(maxBody), "MaxMessageLengthExceeded"
	if op.name == "PutObject" || op.name == "UploadPart" {
		limit, tooLarge = maxObject, "EntityTooLarge"
	}
	c.body, err = readBody(w, r, limit, tooLarge)
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

// readBody reads r's body, decoding aws-chunked encoding, up to limit bytes; tooLarge is the code for a longer one.
func readBody(w http.ResponseWriter, r *http.Request, limit int64, tooLarge string) ([]byte, *apiError) {
	tooLong := &apiError{Status: http.StatusBadRequest, Code: tooLarge, Message: fmt.Sprintf("squall: bodies are limited to %d bytes", limit)}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, limit+limit/8+4096))
	if _, ok := errors.AsType[*http.MaxBytesError](err); ok {
		return nil, tooLong
	}
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
		return nil, tooLong
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
		size, err := strconv.ParseUint(string(sizeHex), 16, 32)
		if err != nil {
			return nil, fmt.Errorf("bad chunk size %q", sizeHex)
		}
		if size == 0 {
			return out, nil
		}
		if uint64(len(rest)) < size+2 || string(rest[size:size+2]) != "\r\n" {
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
