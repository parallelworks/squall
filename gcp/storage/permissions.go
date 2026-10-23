package storage

import (
	"fmt"
	"net/http"
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
	"storage.buckets.insert": {"storage.buckets.create"},
	"storage.buckets.list":   {"storage.buckets.list"},
	"storage.buckets.get":    {"storage.buckets.get"},
	"storage.buckets.patch":  {"storage.buckets.update"},
	"storage.buckets.delete": {"storage.buckets.delete"},
	"storage.objects.list":   {"storage.objects.list"},
	"storage.objects.delete": {"storage.objects.delete"},
	"storage.objects.insert": {"storage.objects.create"},
}

// needs returns every permission a call to method on resource needs.
func (s *Server) needs(method, resource string, r *http.Request) []string {
	perms := append([]string(nil), basePermissions[method]...)
	q := r.URL.Query()
	add := func(cond bool, more ...string) {
		if cond {
			perms = append(perms, more...)
		}
	}
	full := q.Get("projection") == "full"
	switch method {
	case "storage.buckets.insert":
		add(q.Get("enableObjectRetention") == "true", "storage.buckets.enableObjectRetention")
	case "storage.buckets.list", "storage.buckets.get":
		add(full, "storage.buckets.getIamPolicy")
	case "storage.buckets.patch":
		add(q.Get("predefinedAcl") != "" || q.Get("predefinedDefaultObjectAcl") != "", "storage.buckets.setIamPolicy")
		add(full, "storage.buckets.getIamPolicy")
	case "storage.objects.list":
		add(full, "storage.objects.getIamPolicy")
	case "storage.objects.insert":
		add(s.exists(resource), "storage.objects.delete")
	}
	return perms
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
	perm := strings.Split(missing[0], "|")[0]
	return &apiError{
		Status:  http.StatusForbidden,
		Reason:  "forbidden",
		Message: fmt.Sprintf("The caller does not have %s access to %s. Permission '%s' denied on resource (or it may not exist).", perm, resource, perm),
	}
}
