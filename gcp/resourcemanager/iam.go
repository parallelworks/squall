package resourcemanager

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"slices"
	"strings"
)

// Policy is an IAM policy, in the API's JSON shape. Etag is base64.
type Policy struct {
	Version  int       `json:"version,omitzero"`
	Bindings []Binding `json:"bindings,omitzero"`
	Etag     string    `json:"etag,omitzero"`
}

// Binding grants a role to members, under an optional condition.
type Binding struct {
	Role      string   `json:"role"`
	Members   []string `json:"members"`
	Condition *Expr    `json:"condition,omitzero"`
}

// Expr is a binding's condition, in Common Expression Language.
type Expr struct {
	Expression  string `json:"expression"`
	Title       string `json:"title,omitzero"`
	Description string `json:"description,omitzero"`
	Location    string `json:"location,omitzero"`
}

// memberTypes are the prefixes a member may have, besides allUsers and allAuthenticatedUsers.
var memberTypes = []string{
	"user:", "serviceAccount:", "group:", "domain:", "principal:", "principalSet:",
	"deleted:user:", "deleted:serviceAccount:", "deleted:group:",
	"projectOwner:", "projectEditor:", "projectViewer:",
}

// IamPolicy returns the IAM policy of the project with ID or number ref.
func (s *Server) IamPolicy(ref string) (Policy, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.lookup(ref)
	if !ok {
		return Policy{}, false
	}
	return p.policy(), true
}

func (p *project) policy() Policy {
	out := Policy{Version: 1, Etag: base64.StdEncoding.EncodeToString(p.etag)}
	for _, b := range p.bindings {
		if b.Condition != nil {
			out.Version = 3
		}
		out.Bindings = append(out.Bindings, b.clone())
	}
	return out
}

func (b Binding) clone() Binding {
	b.Members = slices.Clone(b.Members)
	if b.Condition != nil {
		b.Condition = new(*b.Condition)
	}
	return b
}

func conditional(bindings []Binding) bool {
	return slices.ContainsFunc(bindings, func(b Binding) bool { return b.Condition != nil })
}

func checkVersion(field string, v int) *apiError {
	if v != 0 && v != 1 && v != 3 {
		return invalid("Invalid value for %s: %d. Valid values are 0, 1 and 3.", field, v)
	}
	return nil
}

func (s *Server) getIAMPolicy(c *call) (any, *apiError) {
	var req struct {
		Resource string `json:"resource"`
		Options  struct {
			RequestedPolicyVersion int `json:"requestedPolicyVersion"`
		} `json:"options"`
	}
	if err := readJSON(c.Request, &req); err != nil {
		return nil, err
	}
	if err := checkVersion("options.requested_policy_version", req.Options.RequestedPolicyVersion); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.lookup(c.ref)
	if !ok {
		return nil, notHeld
	}
	if conditional(p.bindings) && req.Options.RequestedPolicyVersion < 3 {
		return nil, invalid("The policy of %s has conditional role bindings, which need options.requested_policy_version 3.", c.resource())
	}
	return p.policy(), nil
}

func (s *Server) setIAMPolicy(c *call) (any, *apiError) {
	var req struct {
		Resource string `json:"resource"`
		Policy   *struct {
			Policy
			AuditConfigs json.RawMessage `json:"auditConfigs"`
		} `json:"policy"`
		UpdateMask string `json:"updateMask"`
	}
	if err := readJSON(c.Request, &req); err != nil {
		return nil, err
	}
	if req.Policy == nil {
		return nil, invalid("Invalid value for policy: a policy is required.")
	}
	if len(req.Policy.AuditConfigs) > 0 && string(req.Policy.AuditConfigs) != "null" && string(req.Policy.AuditConfigs) != "[]" {
		return nil, invalid("squall: audit configs are not supported")
	}
	setBindings := true
	if req.UpdateMask != "" {
		setBindings = false
		for path := range strings.SplitSeq(req.UpdateMask, ",") {
			switch strings.TrimSpace(path) {
			case "bindings":
				setBindings = true
			case "etag", "auditConfigs", "audit_configs":
			default:
				return nil, invalid("Invalid value for update_mask: %q. Only bindings, etag and auditConfigs can be updated.", path)
			}
		}
	}
	want := req.Policy.Policy
	if err := checkVersion("policy.version", want.Version); err != nil {
		return nil, err
	}
	if err := checkBindings(want); err != nil {
		return nil, err
	}
	var etag []byte
	if want.Etag != "" {
		var err error
		if etag, err = base64.StdEncoding.DecodeString(want.Etag); err != nil {
			if etag, err = base64.URLEncoding.DecodeString(want.Etag); err != nil {
				return nil, invalid("Invalid value for policy.etag: %q is not base64.", want.Etag)
			}
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.lookup(c.ref)
	if !ok {
		return nil, notHeld
	}
	if etag != nil && !bytes.Equal(etag, p.etag) {
		return nil, aborted()
	}
	if setBindings {
		p.bindings = nil
		for _, b := range want.Bindings {
			p.bindings = append(p.bindings, b.clone())
		}
	}
	p.etag = s.newEtag()
	return p.policy(), nil
}

func checkBindings(p Policy) *apiError {
	if conditional(p.Bindings) && p.Version != 3 {
		return invalid("Invalid value for policy.version: %d. A policy with conditional role bindings must be version 3.", p.Version)
	}
	for _, b := range p.Bindings {
		if !validRole(b.Role) {
			return invalid("Invalid value for binding role: %q. It must be roles/{role}, projects/{project}/roles/{role} or organizations/{organization}/roles/{role}.", b.Role)
		}
		if len(b.Members) == 0 {
			return invalid("The binding for role %s has no members.", b.Role)
		}
		for _, m := range b.Members {
			if !validMember(m) {
				return invalid("Policy members must be of the form \"<type>:<value>\". Member %q is not.", m)
			}
		}
		if b.Condition != nil && b.Condition.Expression == "" {
			return invalid("The condition of the binding for role %s has no expression.", b.Role)
		}
	}
	return nil
}

func validRole(role string) bool {
	rest := role
	for _, kind := range []string{"projects/", "organizations/"} {
		if after, ok := strings.CutPrefix(role, kind); ok {
			owner, r, found := strings.Cut(after, "/")
			if !found || owner == "" {
				return false
			}
			rest = r
		}
	}
	name, ok := strings.CutPrefix(rest, "roles/")
	return ok && name != "" && !strings.Contains(name, "/")
}

func validMember(m string) bool {
	if m == "allUsers" || m == "allAuthenticatedUsers" {
		return true
	}
	for _, prefix := range memberTypes {
		if v, ok := strings.CutPrefix(m, prefix); ok {
			return v != ""
		}
	}
	return false
}

func (s *Server) testIAMPermissions(c *call) (any, *apiError) {
	var req struct {
		Resource    string   `json:"resource"`
		Permissions []string `json:"permissions"`
	}
	if err := readJSON(c.Request, &req); err != nil {
		return nil, err
	}
	if len(req.Permissions) > 100 {
		return nil, invalid("Invalid value for permissions: %d permissions given, at most 100 allowed.", len(req.Permissions))
	}
	for _, perm := range req.Permissions {
		if perm == "" || strings.Contains(perm, "*") {
			return nil, invalid("Permissions with wildcards (such as * or storage.*) are not allowed: %q.", perm)
		}
	}
	s.mu.Lock()
	_, ok := s.lookup(c.ref)
	s.mu.Unlock()
	if !ok {
		return nil, notHeld
	}
	var out struct {
		Permissions []string `json:"permissions,omitzero"`
	}
	for _, perm := range req.Permissions {
		if s.policy.Grants(perm) && !slices.Contains(out.Permissions, perm) {
			out.Permissions = append(out.Permissions, perm)
		}
	}
	return out, nil
}
