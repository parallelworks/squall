package ec2

import (
	"fmt"
	"net/http"

	"github.com/parallelworks/squall/access"
)

// WithPolicy makes the server refuse calls that need an IAM action p lacks,
// as EC2 refuses them, and records what each call needed. Without it every
// call is allowed.
func WithPolicy(p *access.Policy) Option {
	return func(s *Server) { s.policy = p }
}

// needs returns the IAM actions a call to the EC2 action name needs: the
// action itself, and ec2:CreateTags when it tags what it creates.
func needs(name string, p *params) []string {
	perms := []string{"ec2:" + name}
	if name != "CreateTags" && len(p.indexes("TagSpecification")) > 0 {
		perms = append(perms, "ec2:CreateTags")
	}
	return perms
}

func unauthorized(missing []string) *apiError {
	return &apiError{
		Status: http.StatusForbidden,
		Code:   "UnauthorizedOperation",
		Message: fmt.Sprintf("You are not authorized to perform this operation. User is not authorized to perform: %s because no identity-based policy allows the %s action",
			missing[0], missing[0]),
	}
}
