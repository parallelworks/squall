package cur

import (
	"encoding/json"
	"strings"
)

const servicePrincipal = "billingreports.amazonaws.com"

type policyDocument struct {
	Statement oneOrMany[statement] `json:"Statement"`
}

type statement struct {
	Effect       string            `json:"Effect"`
	Principal    json.RawMessage   `json:"Principal"`
	NotPrincipal json.RawMessage   `json:"NotPrincipal"`
	Action       oneOrMany[string] `json:"Action"`
	NotAction    json.RawMessage   `json:"NotAction"`
	Resource     oneOrMany[string] `json:"Resource"`
	NotResource  json.RawMessage   `json:"NotResource"`
}

// oneOrMany reads a policy element written as one value or a list of them.
type oneOrMany[T any] []T

func (o *oneOrMany[T]) UnmarshalJSON(b []byte) error {
	var one T
	if err := json.Unmarshal(b, &one); err == nil {
		*o = []T{one}
		return nil
	}
	var many []T
	if err := json.Unmarshal(b, &many); err != nil {
		return err
	}
	*o = many
	return nil
}

// grant is one permission report delivery needs: an action on a resource, or
// with everyObject on every object whose ARN starts with the resource.
type grant struct {
	action      string
	resource    string
	everyObject bool
}

// deliveryAllowed reports whether policy lets the service principal deliver reports to bucket.
func deliveryAllowed(policy, bucket string) bool {
	var doc policyDocument
	if json.Unmarshal([]byte(policy), &doc) != nil {
		return false
	}
	arn := "arn:aws:s3:::" + bucket
	for _, g := range []grant{
		{action: "s3:GetBucketAcl", resource: arn},
		{action: "s3:GetBucketPolicy", resource: arn},
		{action: "s3:PutObject", resource: arn + "/", everyObject: true},
	} {
		allowed := false
		for _, st := range doc.Statement {
			if !st.applies(g) {
				continue
			}
			switch st.Effect {
			case "Deny":
				return false
			case "Allow":
				allowed = true
			}
		}
		if !allowed {
			return false
		}
	}
	return true
}

func (st *statement) applies(g grant) bool {
	if st.NotPrincipal != nil || st.NotAction != nil || st.NotResource != nil || !namesService(st.Principal) {
		return false
	}
	action := strings.ToLower(g.action)
	matched := false
	for _, a := range st.Action {
		if wildcardMatch(strings.ToLower(a), action, false) {
			matched = true
			break
		}
	}
	if !matched {
		return false
	}
	for _, r := range st.Resource {
		if wildcardMatch(r, g.resource, g.everyObject) {
			return true
		}
	}
	return false
}

// namesService reports whether a Principal element covers the service principal.
func namesService(raw json.RawMessage) bool {
	var star string
	if json.Unmarshal(raw, &star) == nil {
		return star == "*"
	}
	var principals map[string]oneOrMany[string]
	if json.Unmarshal(raw, &principals) != nil {
		return false
	}
	for kind, names := range principals {
		for _, name := range names {
			if (kind == "Service" && name == servicePrincipal) || (kind == "AWS" && name == "*") {
				return true
			}
		}
	}
	return false
}

// wildcardMatch reports whether pattern, with IAM's * and ? wildcards,
// matches s, or with anyTail every string that starts with s.
func wildcardMatch(pattern, s string, anyTail bool) bool {
	n := len(s)
	if anyTail {
		n++
	}
	// match[j] reports whether the pattern read so far matches the first j characters of s.
	match := make([]bool, n+1)
	match[0] = true
	for i := range len(pattern) {
		c := pattern[i]
		if c == '*' {
			for j := 1; j <= n; j++ {
				match[j] = match[j] || match[j-1]
			}
			continue
		}
		for j := n; j >= 1; j-- {
			match[j] = match[j-1] && j <= len(s) && (c == '?' || c == s[j-1])
		}
		match[0] = false
	}
	return match[n]
}
