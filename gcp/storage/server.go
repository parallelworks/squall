// Package storage is a stand-in for the Cloud Storage JSON API, for testing
// code that calls it through the official client libraries.
//
// [Server] is an [http.Handler] that keeps buckets and their object
// generations in memory and answers the JSON API calls the Go client library
// makes. Serve it with [net/http/httptest] and point a client at it with
// option.WithEndpoint(srv.URL + "/storage/v1/") and
// option.WithoutAuthentication(), or by setting STORAGE_EMULATOR_HOST to
// srv.URL. A [fault.Injector] makes chosen calls fail, including after they
// took effect, so tests can check that code which retries converges.
//
// It covers:
//
//   - buckets: insert, get, list, patch, delete;
//   - objects: insert by multipart or media upload, list, delete.
//
// Other methods answer 404. Where it differs from Cloud Storage:
//
//   - There is one caller, which can reach every project. Bucket names are
//     global, as in Cloud Storage. [Server.PutForeignBucket] holds a name for
//     someone the caller cannot reach: inserting it fails with 409 conflict
//     and every other call on it with 403 forbidden. buckets.list lists only
//     the project's own. Other denials are a fault rule away, such as
//     fault.Reject(403, "forbidden") for "storage.buckets.list".
//   - Inserting a bucket the caller already has fails with 409 conflict, as a
//     client's retry of a create that took effect does. Bucket names follow
//     the naming rules, and labels the label rules; locations are not
//     checked against the real list. A location is stored upper-cased, US by
//     default; US, EU and ASIA are multi-regions, NAM4, EUR4, EUR5, EUR7, EUR8
//     and ASIA1 and any location with a customPlacementConfig dual-regions,
//     and the rest regions. Project numbers are derived from project IDs.
//   - Bucket fields other than name, location, storageClass, labels,
//     versioning, cors, customPlacementConfig, encryption and rpo are
//     rejected with 400, as are ACLs other than private and projectPrivate.
//     cors, encryption and rpo are recorded, not enforced. There is no IAM,
//     soft delete, retention, lifecycle, autoclass or hierarchical namespace.
//   - Objects record their size, MD5 and CRC32C, not their data; a crc32c or
//     md5Hash in the upload's metadata must match the data. Resumable
//     uploads, reads, copies, composes and object ACLs are not supported.
//     Uploads take up to 16 MiB.
//   - With versioning on, writing or deleting an object makes its live
//     generation noncurrent, and objects.list with versions=true lists those
//     too, ordered by name and then generation. Deleting a bucket fails with
//     409 conflict while any generation, live or noncurrent, remains.
//   - objects.list takes prefix, startOffset, endOffset, versions, pageToken
//     and maxResults; a delimiter, matchGlob, filter, softDeleted,
//     includeTrailingDelimiter or includeFoldersAsPrefixes is rejected with
//     400. Page tokens are positions, so a page may follow one whose items
//     were deleted. Bucket preconditions are ifMetagenerationMatch and
//     ifMetagenerationNotMatch, and object ones ifGenerationMatch and
//     ifGenerationNotMatch. The fields and userProject parameters are
//     accepted and ignored: responses are always whole. Other parameters
//     the server does not know are rejected with 400.
//
// Fault method names are the API's method IDs, such as
// "storage.buckets.insert". A call's resource is its path below
// /storage/v1/, such as "b/my-bucket" or "b/my-bucket/o/my-object"; an
// insert names the bucket or object it creates, and buckets.list names
// "projects/my-project/b". The JSON API has no long-running operations, so
// [fault.ModeFailOperation] fails a call as [fault.ModeReject] does.
//
// A lost response aborts the handler with [http.ErrAbortHandler] after
// sending the response header, so the [http.Server] serving it closes the
// connection. Calling ServeHTTP directly propagates that panic.
package storage

import (
	"bytes"
	"cmp"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/parallelworks/squall/fault"
	"github.com/parallelworks/squall/internal/stub"
)

const (
	maxBody   = 1 << 20
	maxObject = 16 << 20
	maxList   = 1000

	apiBase      = "https://www.googleapis.com/storage/v1/"
	downloadBase = "https://storage.googleapis.com/download/storage/v1/"
	timeFormat   = "2006-01-02T15:04:05.000Z"
)

// Server is a stand-in for the Cloud Storage JSON API. Create one with [New]. It is safe for concurrent use.
type Server struct {
	mux    *http.ServeMux
	faults *fault.Injector
	now    func() time.Time

	mu             sync.Mutex
	buckets        map[string]*bucket
	foreign        map[string]bool
	lastGeneration int64
}

// Option configures a [Server].
type Option func(*Server)

// WithFaults makes the server ask in which calls to fail.
func WithFaults(in *fault.Injector) Option {
	return func(s *Server) { s.faults = in }
}

// WithClock sets the clock for times and generations. The default is time.Now.
func WithClock(now func() time.Time) Option {
	return func(s *Server) { s.now = now }
}

// New returns an empty server.
func New(opts ...Option) *Server {
	s := &Server{mux: http.NewServeMux(), now: time.Now, buckets: map[string]*bucket{}, foreign: map[string]bool{}}
	for _, opt := range opts {
		opt(s)
	}
	s.registerRoutes()
	return s
}

// ServeHTTP answers a Cloud Storage JSON API call.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mux.ServeHTTP(w, r)
}

// commonParams are the query parameters every method takes.
var commonParams = []string{"alt", "prettyPrint", "fields", "userProject", "quotaUser"}

func (s *Server) registerRoutes() {
	const (
		buckets = "/storage/v1/b"
		bucket  = buckets + "/{bucket}"
	)
	s.route("POST "+buckets, "storage.buckets.insert", s.insertBucket, insertTarget,
		"project", "projection", "predefinedAcl", "predefinedDefaultObjectAcl", "enableObjectRetention")
	s.route("GET "+buckets, "storage.buckets.list", s.listBuckets, listTarget,
		"project", "projection", "prefix", "pageToken", "maxResults", "returnPartialSuccess")
	s.route("GET "+bucket, "storage.buckets.get", s.getBucket, nil, "projection", "ifMetagenerationMatch", "ifMetagenerationNotMatch")
	s.route("PATCH "+bucket, "storage.buckets.patch", s.patchBucket, nil,
		"projection", "ifMetagenerationMatch", "ifMetagenerationNotMatch", "predefinedAcl", "predefinedDefaultObjectAcl")
	s.route("DELETE "+bucket, "storage.buckets.delete", s.deleteBucket, nil, "ifMetagenerationMatch", "ifMetagenerationNotMatch")
	s.route("GET "+bucket+"/o", "storage.objects.list", s.listObjects, nil,
		"projection", "prefix", "delimiter", "pageToken", "maxResults", "versions", "startOffset", "endOffset",
		"includeTrailingDelimiter", "matchGlob", "includeFoldersAsPrefixes", "softDeleted", "filter")
	s.route("DELETE "+bucket+"/o/{object...}", "storage.objects.delete", s.deleteObject, objectTarget,
		"generation", "ifGenerationMatch", "ifGenerationNotMatch")
	s.route("POST /upload"+bucket+"/o", "storage.objects.insert", s.insertObject, uploadTarget,
		"uploadType", "name", "projection", "predefinedAcl", "ifGenerationMatch", "ifGenerationNotMatch", "contentEncoding")
	s.mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		writeError(w, &apiError{Status: http.StatusNotFound, Reason: "notFound", Message: "squall: " + r.Method + " " + r.URL.Path + " is not implemented"})
	})
}

// handler answers one method with a body to encode, or nil for an empty 204.
type handler func(r *http.Request) (any, *apiError)

// route registers h for pattern as method, which takes params; a non-nil target names the call's resource.
func (s *Server) route(pattern, method string, h handler, target func(*http.Request) string, params ...string) {
	s.mux.HandleFunc(pattern, func(w http.ResponseWriter, r *http.Request) {
		for name := range r.URL.Query() {
			if !slices.Contains(params, name) && !slices.Contains(commonParams, name) {
				writeError(w, invalid("squall: %s does not take the parameter %s", method, name))
				return
			}
		}
		if alt := r.URL.Query().Get("alt"); alt != "" && alt != "json" {
			writeError(w, invalid("squall: alt=%s is not supported", alt))
			return
		}
		resource := strings.TrimPrefix(r.URL.Path, "/storage/v1/")
		if target != nil {
			resource = target(r)
		}
		f, _ := s.faults.Decide(fault.Call{Method: method, Resource: resource})
		reject := f.Mode == fault.ModeReject || f.Mode == fault.ModeFailOperation
		stub.Serve(w, r, f, reject, "application/json; charset=UTF-8", func() { writeError(w, faultError(f)) }, func() (func(), bool) {
			body, err := h(r)
			if err != nil {
				return func() { writeError(w, err) }, false
			}
			return func() {
				if body == nil {
					w.WriteHeader(http.StatusNoContent)
					return
				}
				writeJSON(w, http.StatusOK, body)
			}, true
		})
	})
}

// insertTarget names the bucket a buckets.insert creates, from its body.
func insertTarget(r *http.Request) string {
	var named struct {
		Name string `json:"name"`
	}
	if json.Unmarshal(peekBody(r), &named) != nil || named.Name == "" {
		return "b"
	}
	return "b/" + named.Name
}

func listTarget(r *http.Request) string {
	return "projects/" + r.URL.Query().Get("project") + "/b"
}

func objectTarget(r *http.Request) string {
	return "b/" + r.PathValue("bucket") + "/o/" + r.PathValue("object")
}

// uploadTarget names the object an upload creates, from its name parameter or metadata.
func uploadTarget(r *http.Request) string {
	prefix := "b/" + r.PathValue("bucket") + "/o"
	if name := r.URL.Query().Get("name"); name != "" {
		return prefix + "/" + name
	}
	if r.URL.Query().Get("uploadType") != "multipart" {
		return prefix
	}
	meta, _, err := splitMultipart(r.Header.Get("Content-Type"), peekBody(r))
	var named struct {
		Name string `json:"name"`
	}
	if err != nil || json.Unmarshal(meta, &named) != nil || named.Name == "" {
		return prefix
	}
	return prefix + "/" + named.Name
}

// peekBody reads r's body and puts it back, to name a call's resource before it is handled.
func peekBody(r *http.Request) []byte {
	body, err := io.ReadAll(io.LimitReader(r.Body, maxObject+maxBody))
	r.Body = io.NopCloser(bytes.NewReader(body))
	if err != nil {
		return nil
	}
	return body
}

// apiError is an error in the shape Google APIs answer with.
type apiError struct {
	Status  int
	Reason  string
	Message string
}

func (e *apiError) Error() string { return fmt.Sprintf("%d %s: %s", e.Status, e.Reason, e.Message) }

func invalid(format string, args ...any) *apiError {
	return &apiError{Status: http.StatusBadRequest, Reason: "invalid", Message: fmt.Sprintf(format, args...)}
}

func required(param string) *apiError {
	return &apiError{Status: http.StatusBadRequest, Reason: "required", Message: "Required parameter: " + param}
}

func conditionNotMet() *apiError {
	return &apiError{Status: http.StatusPreconditionFailed, Reason: "conditionNotMet", Message: "At least one of the pre-conditions you specified did not hold."}
}

func faultError(f fault.Fault) *apiError {
	status := cmp.Or(f.Status, http.StatusServiceUnavailable)
	return &apiError{Status: status, Reason: cmp.Or(f.Reason, "backendError"), Message: fmt.Sprintf("squall: injected fault (%s)", f)}
}

type errorBody struct {
	Error errorDetail `json:"error"`
}

type errorDetail struct {
	Code    int           `json:"code"`
	Message string        `json:"message"`
	Errors  []errorReason `json:"errors"`
}

type errorReason struct {
	Message string `json:"message"`
	Domain  string `json:"domain"`
	Reason  string `json:"reason"`
}

func writeError(w http.ResponseWriter, e *apiError) {
	writeJSON(w, e.Status, errorBody{errorDetail{
		Code:    e.Status,
		Message: e.Message,
		Errors:  []errorReason{{Message: e.Message, Domain: "global", Reason: e.Reason}},
	}})
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

// unmarshalStrict decodes data into v, rejecting fields v does not have.
func unmarshalStrict(data []byte, v any) *apiError {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		if name, ok := strings.CutPrefix(err.Error(), "json: unknown field "); ok {
			return invalid("squall: the field %s is not supported", name)
		}
		if _, ok := errors.AsType[*json.SyntaxError](err); ok {
			return invalid("Invalid JSON payload received: %v", err)
		}
		return invalid("Invalid value in the request body: %v", err)
	}
	return nil
}

func readBody(r *http.Request, limit int64) ([]byte, *apiError) {
	body, err := io.ReadAll(io.LimitReader(r.Body, limit+1))
	if err != nil {
		return nil, invalid("reading the request body: %v", err)
	}
	if int64(len(body)) > limit {
		return nil, &apiError{Status: http.StatusRequestEntityTooLarge, Reason: "uploadTooLarge", Message: fmt.Sprintf("squall: bodies are limited to %d bytes", limit)}
	}
	return body, nil
}

// int64Param reads the named query parameter, reporting whether it is set.
func int64Param(r *http.Request, name string) (int64, bool, *apiError) {
	v := r.URL.Query().Get(name)
	if v == "" {
		return 0, false, nil
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		return 0, false, invalid("Invalid value for %s: %s", name, v)
	}
	return n, true, nil
}

// boolParam reads the named query parameter, which is false when empty.
func boolParam(r *http.Request, name string) (bool, *apiError) {
	switch v := r.URL.Query().Get(name); v {
	case "", "false":
		return false, nil
	case "true":
		return true, nil
	default:
		return false, invalid("Invalid value for %s: %s", name, v)
	}
}

// checkMatch applies a ...Match and ...NotMatch precondition pair to value.
func checkMatch(r *http.Request, match, notMatch string, value int64) *apiError {
	if want, ok, err := int64Param(r, match); err != nil {
		return err
	} else if ok && want != value {
		return conditionNotMet()
	}
	if unwanted, ok, err := int64Param(r, notMatch); err != nil {
		return err
	} else if ok && unwanted == value {
		return conditionNotMet()
	}
	return nil
}

// page returns the key-ordered items after the page token, the last key of the page before, up to maxResults.
func page[T any](r *http.Request, items []T, key func(T) string) ([]T, string, *apiError) {
	limit := maxList
	if n, ok, err := int64Param(r, "maxResults"); err != nil {
		return nil, "", err
	} else if ok {
		if n < 0 {
			return nil, "", invalid("Invalid value for maxResults: %d", n)
		}
		if n > 0 && n < maxList {
			limit = int(n)
		}
	}
	if token := r.URL.Query().Get("pageToken"); token != "" {
		after, err := base64.RawURLEncoding.DecodeString(token)
		if err != nil || len(after) == 0 {
			return nil, "", invalid("Invalid page token: %s", token)
		}
		i, _ := slices.BinarySearchFunc(items, string(after), func(item T, k string) int { return strings.Compare(key(item), k) })
		for i < len(items) && key(items[i]) <= string(after) {
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
