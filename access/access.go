// Package access decides which API calls a stand-in server allows, as the
// cloud's access control would, and records the permissions each call needed.
//
// A [Policy] holds the permissions a test grants: Google Cloud IAM permissions
// such as "compute.instances.create", AWS IAM actions such as
// "ec2:RunInstances", or Azure RBAC actions such as
// "Microsoft.Compute/virtualMachines/write". A server asks it about every call
// with [Policy.Check] and refuses a call that needs a permission the policy
// lacks, with the error the real cloud returns. Running code under test with
// exactly the permissions it claims to need shows which it needs but lacks
// ([Policy.Denials]) and which it holds but never uses ([Policy.Unused]).
//
// A nil *Policy allows every call and records nothing.
package access

import (
	"maps"
	"slices"
	"strings"
	"sync"
)

// Call is one API request as the policy sees it.
type Call struct {
	// Method is the API method ID, as in fault.Call.
	Method string
	// Resource names what the call acts on, as in fault.Call.
	Resource string
	// Needs is every permission the call needs, including those a field of
	// the request adds, such as a disk an instance insert creates. A need
	// may list alternatives separated by |, any one of which suffices, where
	// the cloud documents "one or more of" without saying which applies.
	Needs []string
}

// Denial is a call the policy refused.
type Denial struct {
	Call
	// Missing is the permissions the call needs that the policy lacks.
	Missing []string
}

// Policy grants a set of permissions. It is safe for concurrent use.
type Policy struct {
	grants []string

	mu      sync.Mutex
	used    map[string]bool
	denials []Denial
}

// Allow returns a policy that grants exactly perms. A grant may use * to match
// any run of characters, as AWS and Azure grants do, such as "ec2:Describe*".
// Grants match without regard to case.
func Allow(perms ...string) *Policy {
	return &Policy{grants: slices.Clone(perms), used: map[string]bool{}}
}

// AllowAll returns a policy that grants every permission and records each
// one a call needs.
func AllowAll() *Policy {
	return Allow("*")
}

// Check records c and returns the permissions it needs that the policy lacks,
// or nil if the call is allowed. A server refuses a call for which Check
// returns any.
func (p *Policy) Check(c Call) []string {
	if p == nil {
		return nil
	}
	var missing, satisfied []string
	for _, need := range c.Needs {
		alternatives := strings.Split(need, "|")
		i := slices.IndexFunc(alternatives, p.granted)
		if i < 0 {
			missing = append(missing, need)
			continue
		}
		satisfied = append(satisfied, alternatives[i])
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(missing) > 0 {
		c.Needs = slices.Clone(c.Needs)
		p.denials = append(p.denials, Denial{Call: c, Missing: missing})
		return missing
	}
	for _, need := range satisfied {
		p.used[need] = true
	}
	return nil
}

// Used returns, sorted, every permission an allowed call needed.
func (p *Policy) Used() []string {
	if p == nil {
		return nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return slices.Sorted(maps.Keys(p.used))
}

// Unused returns the grants that no allowed call needed, in the order they
// were granted. A wildcard grant counts as used when it matched any need.
func (p *Policy) Unused() []string {
	if p == nil {
		return nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	var unused []string
	for _, grant := range p.grants {
		usedBy := func(need string) bool { return match(grant, need) }
		if !slices.ContainsFunc(slices.Collect(maps.Keys(p.used)), usedBy) {
			unused = append(unused, grant)
		}
	}
	return unused
}

// Denials returns the calls the policy refused, in order.
func (p *Policy) Denials() []Denial {
	if p == nil {
		return nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return slices.Clone(p.denials)
}

func (p *Policy) granted(need string) bool {
	return slices.ContainsFunc(p.grants, func(grant string) bool { return match(grant, need) })
}

// match reports whether grant covers need, where * in grant matches any run
// of characters and case is ignored.
func match(grant, need string) bool {
	grant, need = strings.ToLower(grant), strings.ToLower(need)
	parts := strings.Split(grant, "*")
	if len(parts) == 1 {
		return grant == need
	}
	if !strings.HasPrefix(need, parts[0]) {
		return false
	}
	need = need[len(parts[0]):]
	for _, part := range parts[1 : len(parts)-1] {
		i := strings.Index(need, part)
		if i < 0 {
			return false
		}
		need = need[i+len(part):]
	}
	return strings.HasSuffix(need, parts[len(parts)-1])
}
