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
//   - buckets: insert, get, list, patch, delete, lockRetentionPolicy, restore;
//   - objects: insert by multipart or media upload, get (metadata only),
//     patch, update, list, delete, restore;
//   - managedFolders: insert, get, list, delete.
//
// Other methods answer 404. Where it differs from Cloud Storage:
//
//   - There is one caller, which can reach every project. Bucket names are
//     global, as in Cloud Storage. [Server.PutForeignBucket] holds a name for
//     someone the caller cannot reach: inserting it fails with 409 conflict
//     and every other call on it with 403 forbidden. buckets.list lists only
//     the project's own. [WithPolicy] denies calls by permission, and
//     other denials are a fault rule away, such as
//     fault.Reject(403, "forbidden") for "storage.buckets.list".
//   - Inserting a bucket the caller already has fails with 409 conflict, as a
//     client's retry of a create that took effect does. Bucket names follow
//     the naming rules, and labels the label rules; locations are not
//     checked against the real list. A location is stored upper-cased, US by
//     default; US, EU and ASIA are multi-regions, NAM4, EUR4, EUR5, EUR7, EUR8
//     and ASIA1 and any location with a customPlacementConfig dual-regions,
//     and the rest regions. Project numbers are derived from project IDs.
//   - Writable bucket fields other than name, location, storageClass,
//     labels, versioning, cors, customPlacementConfig, encryption, rpo,
//     retentionPolicy, defaultEventBasedHold, softDeletePolicy, billing,
//     objectRetention and iamConfiguration are rejected with 400, as are
//     other unknown fields and ACLs other than private and projectPrivate.
//     cors, encryption, rpo and iamConfiguration.publicAccessPrevention are
//     recorded, not enforced. There are no IAM policies on buckets, objects
//     or managed folders, and no lifecycle, autoclass, hardDeletePause or
//     hierarchical namespace. buckets.patch has patch semantics: it merges
//     labels, deleting one set to null, and clears labels, versioning,
//     cors, encryption, rpo, retentionPolicy, defaultEventBasedHold or
//     billing set to null; a null softDeletePolicy is rejected with 400.
//     Insert and patch ignore the fields the server sets, effectiveTime and
//     isLocked in the policies included, and a patch accepts the
//     insert-only name, location and customPlacementConfig only unchanged,
//     so the body a get returns can be sent back. objectRetention is set
//     only by enableObjectRetention on insert, since the docs give only the
//     console for turning it on for an existing bucket, so a body's
//     objectRetention must match it: on insert what the parameter gives,
//     and on patch what the bucket has, with 400 otherwise. Every bucket
//     reports a generation (inferred).
//   - Objects record their size, MD5 and CRC32C, not their data; a crc32c or
//     md5Hash in the upload's metadata must match the data. Resumable
//     uploads, media reads (objects.get with alt=media answers 404), copies,
//     composes and object ACLs are not supported. Uploads take up to 16 MiB.
//   - With versioning on, writing or deleting an object makes its live
//     generation noncurrent, and objects.list with versions=true lists those
//     too, ordered by name and then generation. Deleting a bucket fails with
//     409 conflict while any generation, live or noncurrent, remains.
//   - iamConfiguration takes uniformBucketLevelAccess and its former name
//     bucketPolicyOnly; when a body has both, uniformBucketLevelAccess
//     wins, as the discovery document and the Go client say, so the body a
//     get returns can be sent back with only it changed. It also takes
//     publicAccessPrevention, inherited by default or enforced. Every
//     bucket reports all three, as Cloud Storage does. Turning uniform
//     bucket-level access on sets a lockedTime 90 days later, after which
//     turning it off fails with 400 invalid, and while managed folders
//     remain with 409 conflict (statuses and messages inferred), as the
//     docs say they must be deleted first. While it is on, a predefinedAcl
//     on an upload, objects.patch or objects.update fails with 400 invalid
//     and the message "Cannot insert legacy ACL for an object when uniform
//     bucket-level access is enabled. Read more at
//     https://cloud.google.com/storage/docs/uniform-bucket-level-access",
//     which is what Cloud Storage answers, though the status codes
//     reference lists the case under badRequest. objects.restore with
//     copySourceAcl=true, which its reference applies only to buckets
//     without uniform access, fails the same way (inferred). A
//     predefinedAcl or predefinedDefaultObjectAcl on buckets.insert or
//     buckets.patch fails with 400 badRequest, as the status codes
//     reference gives it (message inferred). acl and defaultObjectAcl
//     bodies are rejected with 400 on every bucket, and copies, rewrites
//     and composes are not supported.
//   - Managed folders can be used only in a bucket with uniform
//     bucket-level access, as the docs require. Every managedFolders call
//     on another bucket fails with 409 conflict: the Go client library's
//     tests expect FAILED_PRECONDITION for this over gRPC, and the JSON
//     API's 409, as it answers a non-empty bucket's delete, and the message
//     are inferred. A name ends with "/", and one sent without it gets one,
//     as the reference's examples name folders without it (inferred). It is
//     1 to 1,024 bytes of UTF-8 with at most 15 "/"s, no carriage return or
//     line feed, not "." or ".." and not under .well-known/acme-challenge/,
//     with 400 invalid otherwise (message inferred). Inserting one that
//     exists fails with 409 conflict, and a missing one answers 404
//     notFound (both messages inferred). id is the bucket and name joined
//     by "/", and selfLink the folder's path with the name escaped (both
//     inferred); metageneration stays 1, as nothing changes a managed
//     folder. Deleting one that holds an object or another managed folder
//     under its name fails with 409 conflict (status, reason and message
//     inferred); noncurrent generations count (inferred), and soft-deleted
//     ones do not. allowNonEmpty=true deletes it anyway and leaves the
//     objects in place, as the simulated folder the reference says is
//     retained implies, and nested managed folders too (inferred). A bucket
//     with a managed folder left is not empty, as the bucket deletion docs
//     imply, so deleting it fails with the 409 conflict for objects, whose
//     message is inferred to be the same; a soft-deleted bucket so holds no
//     managed folders to restore, and the docs say managed folders cannot
//     be recovered. managedFolders.list takes prefix, pageToken and a page
//     size of at most 1,000, as pageSize, which the discovery document and
//     the Go client use, or maxResults, which the reference names; both, if
//     set, must agree. It lists in name order. update, the IAM policy
//     methods and rapidCacheConfig are not supported.
//   - A retention policy's retentionPeriod is greater than 0 and at most
//     3,155,760,000 seconds (100 years), and its effectiveTime is when it
//     was set or its period last changed (inferred). Each object reports
//     retentionExpirationTime, its time in the bucket plus the period.
//     Deleting or replacing an object before then, or before its own
//     retention's retainUntilTime, fails with 403 retentionPolicyNotMet and
//     Cloud Storage's message; the time in it is in UTC, where Cloud Storage
//     writes Pacific time. Making a live object noncurrent in a versioned
//     bucket is allowed, as documented. lockRetentionPolicy needs
//     ifMetagenerationMatch (400 required without it, 412 conditionNotMet on
//     a mismatch), fails with 400 invalid without an unlocked policy, and
//     changes the bucket's metageneration (inferred). A locked policy cannot
//     be removed (403 retentionPolicyNotMet) or shortened (403 forbidden),
//     with Cloud Storage's messages: the bucket lock page says these fail
//     with 400, but Cloud Storage answers 403, the PERMISSION_DENIED that
//     the retentionPolicy field description in the JSON API discovery
//     document gives for them. Only an emptied bucket can be deleted, so a
//     locked one needs no further check.
//   - temporaryHold and eventBasedHold are set by an upload's metadata,
//     objects.patch and objects.update, where null releases one, and
//     defaultEventBasedHold puts an event-based hold on each uploaded
//     object. A held generation, live or noncurrent, cannot be deleted,
//     replaced or made noncurrent: 403 forbidden, with Cloud Storage's
//     message. While an event-based hold is on, an object reports no
//     retentionExpirationTime, and releasing it starts the object's time in
//     the bucket again. objects.update replaces every writable field, so
//     leaving a hold out releases it and leaving retention out removes it,
//     which fails with 403 for Locked retention and for Unlocked retention
//     without overrideUnlockedRetention=true.
//   - enableObjectRetention on buckets.insert reports objectRetention.mode
//     Enabled, and objects in such a bucket take retention with mode
//     Unlocked or Locked and a future retainUntilTime, with Cloud Storage's
//     400s for a missing or invalid one. Unlocked retention can be
//     shortened, removed or changed to Locked only with
//     overrideUnlockedRetention=true, and locked retention only extended,
//     each refused with 403 forbidden; a change to Locked gets the message
//     for shortening, as no message for it is known (inferred). Retention in
//     a bucket without objectRetention fails with 403 forbidden naming
//     storage.objects.setRetention, and retention beside an event-based hold
//     with 400 invalid (both inferred). The 100-year cap on retainUntilTime
//     is not checked.
//   - softDeletePolicy.retentionDurationSeconds is 0, which turns soft
//     delete off, or 7 to 90 days, and a new bucket keeps deletions for 7
//     days. Its effectiveTime is when the latest unbroken run of durations
//     at least the current one began, as "the policy, or one with a greater
//     retention" in the discovery document defines it. A generation that
//     leaves the bucket, by a delete, by being replaced in a bucket without
//     versioning or by a delete of a noncurrent generation, is kept
//     soft-deleted with softDeleteTime and hardDeleteTime, on the policy at
//     the time; it has a timeDeleted only if it was noncurrent first, as
//     Cloud Storage answers. objects.list with softDeleted=true lists them
//     (400 invalid with soft delete off and none kept), objects.get with
//     softDeleted=true and a generation reads one, and objects.restore makes
//     a copy the live generation, in STANDARD with the metadata, holds and
//     retention it had when deleted, retiring the live one as an overwrite
//     would; the soft-deleted generation stays until its hardDeleteTime
//     (inferred). A restore of a live or noncurrent generation fails with
//     412 objectNotSoftDeleted, and in a bucket with soft delete off and
//     none kept with 400 SoftDeletePolicyRequired; both messages are
//     inferred. ifGenerationNotMatch fails with 412 when there is no live
//     object, and so does the metageneration pair (inferred). A soft-deleted
//     generation cannot be deleted by objects.delete, which answers 404
//     (inferred). A bucket holding only soft-deleted generations can be
//     deleted, and is then kept soft-deleted for its policy's duration or
//     until the last of its soft-deleted generations goes, whichever is
//     later, even with soft delete since turned off (inferred); once
//     restored, those generations can still be listed and restored.
//     buckets.list and buckets.get with softDeleted=true read it;
//     buckets.get takes a generation only with softDeleted=true and answers
//     400 required otherwise, as its reference says. buckets.restore brings
//     it back with its generation, metageneration and soft-deleted objects
//     (inferred), failing with 409 conflict while a live bucket has its name
//     (message inferred). Everything soft-deleted goes for good at its
//     hardDeleteTime on the server's clock, and is held in memory until
//     then: with the 7-day default, every deleted or replaced generation
//     stays, so a long-running test that churns objects may set
//     softDeletePolicy.retentionDurationSeconds to 0 to keep none.
//   - billing.requesterPays is recorded and, under [WithPolicy], enforced: a
//     call on a Requester Pays bucket with a userProject also needs
//     serviceusage.services.use, and one without needs
//     resourcemanager.projects.createBillingAssignment, lacking which it
//     fails with 400 and Cloud Storage's message "Bucket is a requester pays
//     bucket but no user project provided." The page names the error
//     UserProjectMissing, which is the XML API's code; the JSON API's reason
//     here, required, is inferred. Without a policy, the caller may bill the
//     bucket's project, so nothing is refused.
//   - objects.list takes prefix, startOffset, endOffset, versions,
//     softDeleted, pageToken and maxResults; a delimiter, matchGlob, filter,
//     includeTrailingDelimiter or includeFoldersAsPrefixes is rejected with
//     400. objects.patch and objects.update change contentType,
//     contentEncoding, metadata, the holds and retention, and ignore the
//     fields the server sets, so the body a get returns can be sent back; a
//     different storageClass is rejected with 400. Page tokens are
//     positions, so a page may follow one whose items were deleted. Bucket
//     preconditions are ifMetagenerationMatch and ifMetagenerationNotMatch,
//     and object ones ifGenerationMatch, ifGenerationNotMatch (which fails
//     an upload with no live object) and, but for uploads and deletes, the
//     metageneration pair. The fields parameter is accepted and ignored:
//     responses are always whole, and userProject is checked only as the
//     Requester Pays item says. Other parameters the server does not know
//     are rejected with 400.
//
// Fault method names are the API's method IDs, such as
// "storage.buckets.insert". A call's resource is its path below
// /storage/v1/, such as "b/my-bucket", "b/my-bucket/o/my-object" or
// "b/my-bucket/managedFolders/logs/", unescaped and with the managed
// folder's trailing "/"; an insert names the bucket, object or managed
// folder it creates, and buckets.list names
// "projects/my-project/b". The JSON API has no long-running operations, so
// [fault.ModeFailOperation] fails a call as [fault.ModeReject] does.
//
// [WithPolicy] grants the caller a set of IAM permissions and refuses a call
// that needs one it lacks with 403 forbidden, before any fault. A call needs
// its method's permission, and those its parameters add, as each method's
// reference lists them: a predefined ACL on a patch needs
// storage.buckets.setIamPolicy, and an upload that replaces a live object
// storage.objects.delete, as does a restore that replaces one; a body that
// includes retention, even null, needs storage.objects.setRetention, as does
// an objects.update that removes it by leaving it out (inferred), and
// overrideUnlockedRetention=true storage.objects.overrideUnlockedRetention;
// a managedFolders.delete with allowNonEmpty=true needs
// storage.managedFolders.setIamPolicy, and an objects.restore with
// copySourceAcl=true in a bucket without uniform bucket-level access
// storage.objects.setIamPolicy.
// projection=full needs nothing more: Cloud Storage leaves out the ACLs a
// caller may not read rather than refusing the call, and the official
// clients ask for it on every get and list. Permissions are not checked
// against the bucket they act on: a grant covers every bucket. They are
// worked out before the call takes the server's lock, so a concurrent call
// that changes what they depend on, such as an object's retention, in
// between can leave a call checked against the state just before it.
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
	"net/url"
	"path"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/parallelworks/squall/access"
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
	policy *access.Policy
	now    func() time.Time

	mu             sync.Mutex
	buckets        map[string]*bucket
	softBuckets    map[string][]*bucket
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
	s := &Server{mux: http.NewServeMux(), now: time.Now, buckets: map[string]*bucket{}, softBuckets: map[string][]*bucket{}, foreign: map[string]bool{}}
	for _, opt := range opts {
		opt(s)
	}
	s.registerRoutes()
	return s
}

// ServeHTTP answers a Cloud Storage JSON API call.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// ServeMux would redirect a path that is not clean, which the API never does.
	if p := r.URL.EscapedPath(); p != cleanPath(p) {
		notImplemented(w, r)
		return
	}
	s.mux.ServeHTTP(w, r)
}

// cleanPath is p as ServeMux cleans it, keeping a trailing slash.
func cleanPath(p string) string {
	clean := path.Clean("/" + p)
	if strings.HasSuffix(p, "/") && clean != "/" {
		clean += "/"
	}
	return clean
}

func notImplemented(w http.ResponseWriter, r *http.Request) {
	writeError(w, &apiError{Status: http.StatusNotFound, Reason: "notFound", Message: "squall: " + r.Method + " " + r.URL.Path + " is not implemented"})
}

// commonParams are the query parameters every method takes.
var commonParams = []string{"alt", "prettyPrint", "fields", "userProject", "quotaUser"}

func (s *Server) registerRoutes() {
	const (
		buckets = "/storage/v1/b"
		bucket  = buckets + "/{bucket}"
		object  = bucket + "/o/{object...}"
	)
	objectConds := []string{"generation", "ifGenerationMatch", "ifGenerationNotMatch", "ifMetagenerationMatch", "ifMetagenerationNotMatch"}
	s.route("POST "+buckets, "storage.buckets.insert", s.insertBucket, insertTarget,
		"project", "projection", "predefinedAcl", "predefinedDefaultObjectAcl", "enableObjectRetention")
	s.route("GET "+buckets, "storage.buckets.list", s.listBuckets, listTarget,
		"project", "projection", "prefix", "pageToken", "maxResults", "returnPartialSuccess", "softDeleted")
	s.route("GET "+bucket, "storage.buckets.get", s.getBucket, nil,
		"projection", "ifMetagenerationMatch", "ifMetagenerationNotMatch", "generation", "softDeleted")
	s.route("PATCH "+bucket, "storage.buckets.patch", s.patchBucket, nil,
		"projection", "ifMetagenerationMatch", "ifMetagenerationNotMatch", "predefinedAcl", "predefinedDefaultObjectAcl")
	s.route("DELETE "+bucket, "storage.buckets.delete", s.deleteBucket, nil, "ifMetagenerationMatch", "ifMetagenerationNotMatch")
	s.route("POST "+bucket+"/lockRetentionPolicy", "storage.buckets.lockRetentionPolicy", s.lockRetentionPolicy, bucketTarget,
		"ifMetagenerationMatch")
	s.route("POST "+bucket+"/restore", "storage.buckets.restore", s.restoreBucket, bucketTarget, "generation", "projection")
	s.route("POST "+bucket+"/managedFolders", "storage.managedFolders.insert", s.insertFolder, folderInsertTarget)
	s.route("GET "+bucket+"/managedFolders", "storage.managedFolders.list", s.listFolders, nil,
		"prefix", "pageToken", "pageSize", "maxResults")
	s.route("GET "+bucket+"/managedFolders/{folder}", "storage.managedFolders.get", s.getFolder, folderTarget,
		"ifMetagenerationMatch", "ifMetagenerationNotMatch")
	s.route("DELETE "+bucket+"/managedFolders/{folder}", "storage.managedFolders.delete", s.deleteFolder, folderTarget,
		"allowNonEmpty", "ifMetagenerationMatch", "ifMetagenerationNotMatch")
	s.route("GET "+bucket+"/o", "storage.objects.list", s.listObjects, nil,
		"projection", "prefix", "delimiter", "pageToken", "maxResults", "versions", "startOffset", "endOffset",
		"includeTrailingDelimiter", "matchGlob", "includeFoldersAsPrefixes", "softDeleted", "filter")
	get := s.handle("storage.objects.get", s.getObject, objectTarget, slices.Concat(objectConds, []string{"projection", "softDeleted"})...)
	s.mux.HandleFunc("GET "+object, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("alt") == "media" {
			notImplemented(w, r)
			return
		}
		get(w, r)
	})
	s.route("PATCH "+object, "storage.objects.patch", s.patchObject, objectTarget,
		slices.Concat(objectConds, []string{"projection", "predefinedAcl", "overrideUnlockedRetention"})...)
	s.route("PUT "+object, "storage.objects.update", s.updateObject, objectTarget,
		slices.Concat(objectConds, []string{"projection", "predefinedAcl", "overrideUnlockedRetention"})...)
	s.route("DELETE "+object, "storage.objects.delete", s.deleteObject, objectTarget,
		"generation", "ifGenerationMatch", "ifGenerationNotMatch")
	restore := s.handle("storage.objects.restore", s.restoreObject, objectTarget,
		slices.Concat(objectConds, []string{"projection", "copySourceAcl"})...)
	// ServeMux allows a wildcard only at the end, and objects.restore puts a verb after the object name.
	s.mux.HandleFunc("POST "+object, func(w http.ResponseWriter, r *http.Request) {
		_, escaped, _ := strings.Cut(r.URL.EscapedPath(), "/o/")
		escaped, ok := strings.CutSuffix(escaped, "/restore")
		name, err := url.PathUnescape(escaped)
		if !ok || strings.Contains(escaped, "/") || err != nil {
			notImplemented(w, r)
			return
		}
		r.SetPathValue("object", name)
		restore(w, r)
	})
	s.route("POST /upload"+bucket+"/o", "storage.objects.insert", s.insertObject, uploadTarget,
		"uploadType", "name", "projection", "predefinedAcl", "ifGenerationMatch", "ifGenerationNotMatch", "contentEncoding")
	// Without these, ServeMux would redirect an object path with no name to the one with an empty name.
	for _, method := range []string{http.MethodDelete, http.MethodPatch, http.MethodPut, http.MethodPost} {
		s.mux.HandleFunc(method+" "+bucket+"/o", notImplemented)
	}
	s.mux.HandleFunc("/", notImplemented)
}

// handler answers one method with a body to encode, or nil for an empty 204.
type handler func(r *http.Request) (any, *apiError)

// route registers h for pattern as method, which takes params; a non-nil target names the call's resource.
func (s *Server) route(pattern, method string, h handler, target func(*http.Request) string, params ...string) {
	s.mux.HandleFunc(pattern, s.handle(method, h, target, params...))
}

// handle answers method with h, which takes params; a non-nil target names the call's resource.
func (s *Server) handle(method string, h handler, target func(*http.Request) string, params ...string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
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
		if s.policy != nil {
			if missing := s.policy.Check(access.Call{Method: method, Resource: resource, Needs: s.needs(method, resource, r)}); missing != nil {
				writeError(w, permissionDenied(missing, resource))
				return
			}
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
	}
}

// insertTarget names the bucket a buckets.insert creates, from its body.
func insertTarget(r *http.Request) string {
	var named struct {
		Name string `json:"name"`
	}
	if json.Unmarshal(peekBody(r, maxBody), &named) != nil || named.Name == "" {
		return "b"
	}
	return "b/" + named.Name
}

func bucketTarget(r *http.Request) string {
	return "b/" + r.PathValue("bucket")
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
	meta, _, err := splitMultipart(r.Header.Get("Content-Type"), peekBody(r, maxObject+maxBody))
	var named struct {
		Name string `json:"name"`
	}
	if err != nil || json.Unmarshal(meta, &named) != nil || named.Name == "" {
		return prefix
	}
	return prefix + "/" + named.Name
}

// peekBody reads up to limit bytes of r's body and puts them back, to name a call's resource before it is handled.
func peekBody(r *http.Request, limit int64) []byte {
	body, err := io.ReadAll(io.LimitReader(r.Body, limit))
	r.Body = struct {
		io.Reader
		io.Closer
	}{io.MultiReader(bytes.NewReader(body), r.Body), r.Body}
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
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return invalid("Invalid JSON payload received: data after the top-level value")
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

// page returns the key-ordered items after the page token, and the last key of the page before, up to the page size sizeParams give, which must agree.
func page[T any](r *http.Request, items []T, key func(T) string, sizeParams ...string) ([]T, string, *apiError) {
	limit := maxList
	var size int64
	sized := ""
	for _, p := range sizeParams {
		n, ok, err := int64Param(r, p)
		if err != nil {
			return nil, "", err
		}
		if !ok {
			continue
		}
		if n < 0 {
			return nil, "", invalid("Invalid value for %s: %d", p, n)
		}
		if n == 0 {
			continue
		}
		if sized != "" && n != size {
			return nil, "", invalid("squall: %s and %s disagree", sized, p)
		}
		size, sized = n, p
	}
	if size > 0 && size < maxList {
		limit = int(size)
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
