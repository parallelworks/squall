package route53

import (
	"fmt"
	"net/http"

	"github.com/parallelworks/squall/access"
)

// WithPolicy makes the server refuse calls that need an IAM action p lacks,
// as Route 53 refuses them, and records what each call needed. Without it
// every call is allowed.
func WithPolicy(p *access.Policy) Option {
	return func(s *Server) { s.policy = p }
}

// needs returns the IAM actions a call to op needs, from the Route 53
// permissions reference for private hosted zones.
func needs(op string) []string {
	perms := []string{"route53:" + op}
	switch op {
	case "CreateHostedZone":
		perms = append(perms, "route53:AssociateVPCWithHostedZone", "ec2:DescribeVpcs")
	case "AssociateVPCWithHostedZone", "DisassociateVPCFromHostedZone":
		perms = append(perms, "ec2:DescribeVpcs")
	}
	return perms
}

func notAuthorized(action, op, resource string) *apiError {
	arn := "*"
	switch {
	case resource == "":
	case op == "GetChange":
		arn = "arn:aws:route53:::change/" + resource
	case op != "CreateHostedZone":
		arn = "arn:aws:route53:::hostedzone/" + resource
	}
	return &apiError{
		Status: http.StatusForbidden,
		Code:   "AccessDenied",
		Message: fmt.Sprintf("User is not authorized to perform: %s on resource: %s because no identity-based policy allows the %s action",
			action, arn, action),
	}
}
