package storage

import (
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"

	"github.com/parallelworks/squall/access"
)

// WithPolicy makes the server refuse calls that need a permission p lacks,
// as Cloud Storage refuses them, and records what each call needed. Without
// it every call is allowed.
func WithPolicy(p *access.Policy) Option {
	return func(s *Server) { s.policy = p }
}

// basePermissions is what Cloud Storage checks for each method, from each
// method's "Required permissions" in the JSON API reference.
var basePermissions = map[string][]string{
	"storage.buckets.insert":              {"storage.buckets.create"},
	"storage.buckets.list":                {"storage.buckets.list"},
	"storage.buckets.get":                 {"storage.buckets.get"},
	"storage.buckets.patch":               {"storage.buckets.update"},
	"storage.buckets.delete":              {"storage.buckets.delete"},
	"storage.buckets.lockRetentionPolicy": {"storage.buckets.update"},
	"storage.buckets.restore":             {"storage.buckets.restore"},
	"storage.objects.list":                {"storage.objects.list"},
	"storage.objects.get":                 {"storage.objects.get"},
	"storage.objects.patch":               {"storage.objects.update"},
	"storage.objects.update":              {"storage.objects.update"},
	"storage.objects.delete":              {"storage.objects.delete"},
	"storage.objects.insert":              {"storage.objects.create"},
	"storage.objects.restore":             {"storage.objects.restore", "storage.objects.create"},
}

// Requester Pays calls need serviceUsage with a userProject and createBillingAssignment without one.
const (
	serviceUsage            = "serviceusage.services.use"
	createBillingAssignment = "resourcemanager.projects.createBillingAssignment"
)

// needs returns every permission a call to method on resource needs.
func (s *Server) needs(method, resource string, r *http.Request) []string {
	perms := append([]string(nil), basePermissions[method]...)
	q := r.URL.Query()
	add := func(cond bool, more ...string) {
		if cond {
			perms = append(perms, more...)
		}
	}
	switch method {
	case "storage.buckets.insert":
		add(q.Get("enableObjectRetention") == "true", "storage.buckets.enableObjectRetention")
	case "storage.buckets.patch":
		add(q.Get("predefinedAcl") != "" || q.Get("predefinedDefaultObjectAcl") != "", "storage.buckets.setIamPolicy")
	case "storage.objects.insert":
		add(s.exists(resource), "storage.objects.delete")
		add(uploadSetsRetention(r), "storage.objects.setRetention")
	case "storage.objects.patch":
		add(bodySetsRetention(peekBody(r, maxBody)), "storage.objects.setRetention")
		add(q.Get("overrideUnlockedRetention") == "true", "storage.objects.overrideUnlockedRetention")
	case "storage.objects.update":
		add(bodySetsRetention(peekBody(r, maxBody)) || s.retained(resource, q.Get("generation")), "storage.objects.setRetention")
		add(q.Get("overrideUnlockedRetention") == "true", "storage.objects.overrideUnlockedRetention")
	case "storage.objects.restore":
		add(s.exists(resource), "storage.objects.delete")
	}
	if method != "storage.buckets.insert" && method != "storage.buckets.list" && s.requesterPays(resource) {
		add(q.Get("userProject") != "", serviceUsage)
		add(q.Get("userProject") == "", createBillingAssignment)
	}
	return perms
}

// bodySetsRetention reports whether an object's metadata, as JSON, includes retention, null included.
func bodySetsRetention(meta []byte) bool {
	var fields map[string]json.RawMessage
	if json.Unmarshal(meta, &fields) != nil {
		return false
	}
	_, ok := fields["retention"]
	return ok
}

// retained reports whether resource, as "b/<bucket>/o/<object>", at generation or else live, has retention.
func (s *Server) retained(resource, generation string) bool {
	bucketName, name, ok := strings.Cut(strings.TrimPrefix(resource, "b/"), "/o/")
	if !ok {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	b := s.buckets[bucketName]
	if b == nil {
		return false
	}
	o := b.liveObject(name)
	if generation != "" {
		gen, err := strconv.ParseInt(generation, 10, 64)
		if err != nil {
			return false
		}
		o = b.generation(name, gen)
	}
	return o != nil && o.Retention != nil
}

func uploadSetsRetention(r *http.Request) bool {
	if r.URL.Query().Get("uploadType") != "multipart" {
		return false
	}
	meta, _, err := splitMultipart(r.Header.Get("Content-Type"), peekBody(r, maxObject+maxBody))
	return err == nil && bodySetsRetention(meta)
}

// requesterPays reports whether resource, as "b/<bucket>" or a path below it, is in a Requester Pays bucket.
func (s *Server) requesterPays(resource string) bool {
	name, _, _ := strings.Cut(strings.TrimPrefix(resource, "b/"), "/")
	s.mu.Lock()
	defer s.mu.Unlock()
	b := s.buckets[name]
	return b != nil && b.requesterPays()
}

// exists reports whether resource, as "b/<bucket>/o/<object>", names a live object.
func (s *Server) exists(resource string) bool {
	bucketName, name, ok := strings.Cut(strings.TrimPrefix(resource, "b/"), "/o/")
	if !ok {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	b := s.buckets[bucketName]
	return b != nil && b.liveObject(name) != nil
}

func permissionDenied(missing []string, resource string) *apiError {
	if slices.Contains(missing, createBillingAssignment) {
		return &apiError{Status: http.StatusBadRequest, Reason: "required", Message: "Bucket is a requester pays bucket but no user project provided."}
	}
	perm, _, _ := strings.Cut(missing[0], "|")
	return &apiError{
		Status:  http.StatusForbidden,
		Reason:  "forbidden",
		Message: fmt.Sprintf("The caller does not have %s access to %s. Permission '%s' denied on resource (or it may not exist).", perm, resource, perm),
	}
}
