package cur

import (
	"fmt"
	"net/http"

	"github.com/parallelworks/squall/access"
)

// WithPolicy makes the server refuse calls that need an IAM action p lacks,
// as the service refuses them, and records what each call needed. Without it
// every call is allowed.
func WithPolicy(p *access.Policy) Option {
	return func(s *Server) { s.policy = p }
}

func notAuthorized(action string) *apiError {
	return &apiError{
		Status:  http.StatusForbidden,
		Code:    "AccessDeniedException",
		Message: fmt.Sprintf("User is not authorized to perform: %s because no identity-based policy allows the %s action", action, action),
	}
}
