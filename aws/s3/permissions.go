package s3

import (
	"fmt"
	"net/http"

	"github.com/parallelworks/squall/access"
)

// WithPolicy makes the server refuse calls that need an IAM action p lacks,
// as S3 refuses them, and records what each call needed. Without it every
// call is allowed.
func WithPolicy(p *access.Policy) Option {
	return func(s *Server) { s.policy = p }
}

// actions are the IAM actions each operation needs where they differ from
// "s3:" and its name, from the S3 user guide's policy actions by operation.
var actions = map[string]string{
	"ListBuckets":         "s3:ListAllMyBuckets",
	"HeadBucket":          "s3:ListBucket",
	"DeleteBucketTagging": "s3:PutBucketTagging",
	"PutBucketCors":       "s3:PutBucketCORS",
	"ListObjectVersions":  "s3:ListBucketVersions",
}

// needs returns the IAM actions a call to op needs, or nil for DeleteObjects,
// which S3 authorizes key by key.
func needs(op string, c *call) []string {
	switch op {
	case "DeleteObjects":
		return nil
	case "DeleteObject":
		return []string{deleteAction(c.query.Get("versionId"))}
	}
	perms := []string{"s3:" + op}
	if a, ok := actions[op]; ok {
		perms[0] = a
	}
	switch op {
	case "CreateBucket":
		if c.r.Header.Get("x-amz-object-ownership") != "" {
			perms = append(perms, "s3:PutBucketOwnershipControls")
		}
	case "PutObject":
		if c.r.Header.Get("x-amz-acl") != "" {
			perms = append(perms, "s3:PutObjectAcl")
		}
	}
	return perms
}

// deleteAction is what deleting a key needs: deleting a version is its own action.
func deleteAction(versionID string) string {
	if versionID != "" {
		return "s3:DeleteObjectVersion"
	}
	return "s3:DeleteObject"
}

func notAuthorized(action, resource string) *apiError {
	if resource == "" {
		resource = "*"
	}
	return &apiError{
		Status:  http.StatusForbidden,
		Code:    "AccessDenied",
		Message: fmt.Sprintf(`User is not authorized to perform: %s on resource: "arn:aws:s3:::%s" because no identity-based policy allows the %s action`, action, resource, action),
	}
}
