package lustre

import (
	"fmt"
	"net/http"
	"strings"

	"github.com/parallelworks/squall/access"
)

// WithPolicy refuses calls that need a permission p lacks and records what each needed; without it every call is allowed.
func WithPolicy(p *access.Policy) Option {
	return func(s *Server) { s.policy = p }
}

// basePermissions are from each method's "IAM Permissions" in the REST reference.
var basePermissions = map[string][]string{
	"lustre.projects.locations.instances.create": {"lustre.instances.create"},
	"lustre.projects.locations.instances.list":   {"lustre.instances.list"},
	"lustre.projects.locations.instances.get":    {"lustre.instances.get"},
	"lustre.projects.locations.instances.patch":  {"lustre.instances.update"},
	"lustre.projects.locations.instances.delete": {"lustre.instances.delete"},

	"lustre.projects.locations.operations.list":   {"lustre.operations.list"},
	"lustre.projects.locations.operations.get":    {"lustre.operations.get"},
	"lustre.projects.locations.operations.delete": {"lustre.operations.delete"},
	"lustre.projects.locations.operations.cancel": {"lustre.operations.cancel"},
}

// iamResource is the parent for create and list, where the reference checks them.
func iamResource(method, path string) string {
	switch method {
	case "lustre.projects.locations.instances.create", "lustre.projects.locations.instances.list":
		return strings.TrimSuffix(path, "/instances")
	case "lustre.projects.locations.operations.list":
		return strings.TrimSuffix(path, "/operations")
	}
	return path
}

func permissionDenied(missing []string, resource string) *apiError {
	perm, _, _ := strings.Cut(missing[0], "|")
	full := "//lustre.googleapis.com/" + resource
	return &apiError{
		Status:   http.StatusForbidden,
		Message:  fmt.Sprintf("Permission '%s' denied on resource '%s' (or it may not exist).", perm, full),
		Reason:   "IAM_PERMISSION_DENIED",
		Domain:   "iam.googleapis.com",
		Metadata: map[string]string{"permission": perm, "resource": full},
	}
}
