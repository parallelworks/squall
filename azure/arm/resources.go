package arm

import (
	"cmp"
	"fmt"
	"maps"
	"net/http"
	"slices"
	"strings"
	"time"
)

// kind describes a resource type.
type kind struct {
	typ string
	// plural is the type's last path segment.
	plural string
	// typeName names the type in deserialization errors.
	typeName string
	// top are the top-level members the server records, beyond location and
	// tags; topReadOnly are the ones it accepts and drops.
	top, topReadOnly []string
	// props are the properties the server records; readOnly are the ones it
	// accepts, as the SDK sends them back, and drops.
	props, readOnly []string
	// located types have a location; location, when set, is the only one
	// the type is offered in.
	located  bool
	location string
	// etag formats the type's ETag from a UUID, or is nil for types without one.
	etag func(string) string
	// opName names the type in Microsoft.Network operation names, such as Nic in PutNicOperation.
	opName string
}

func (k *kind) action(verb string) string { return k.typ + "/" + verb }

// resourceType is the type without its namespace, such as privateDnsZones/A.
func (k *kind) resourceType() string {
	_, t, _ := strings.Cut(k.typ, "/")
	return t
}

func weakETag(id string) string   { return `W/"` + id + `"` }
func plainETag(id string) string  { return id }
func quotedETag(id string) string { return `"` + id + `"` }

// resource is a resource in a group.
type resource struct {
	kind                      *kind
	id, name, sub, group, loc string
	tags                      map[string]string
	top, props                map[string]any
	state                     string
	etag                      string
	// guid is the resource's resourceGuid, vmId or uniqueId.
	guid    string
	created time.Time
	pending *operation
	nic     *nicState
	pip     *pipState
	vm      *vmState
	scale   *scaleSet
	account *accountState
	pe      *peState
	vnet    *vnetState
	subnet  *subnetState
	// held is the subnet address of a resource that takes one of its own,
	// such as a NetApp volume.
	held *heldAddress
}

// inGroup reports whether r is in the group with key gk.
func (r *resource) inGroup(gk string) bool { return key(r.sub+"/"+r.group) == gk }

// group is a resource group.
type group struct {
	id, name, sub, location string
	tags                    map[string]string
	managedBy               string
	state                   string
	pending                 *operation
}

func groupKey(c *call) string { return key(c.val("sub") + "/" + c.val("rg")) }

func groupID(c *call) string {
	return "/subscriptions/" + c.val("sub") + "/resourceGroups/" + c.val("rg")
}

func resourceID(c *call, typ, name string) string {
	return groupID(c) + "/providers/" + typ + "/" + name
}

// writableGroup checks that the call's group exists and takes changes. The caller holds s.mu.
func (s *Server) writableGroup(c *call) *apiError {
	g, ok := s.groups[groupKey(c)]
	if !ok {
		return groupNotFound(c.val("rg"))
	}
	if g.state == stateDeleting {
		return groupBeingDeleted(g.name)
	}
	return nil
}

// readableGroup checks that the call's group exists. The caller holds s.mu.
func (s *Server) readableGroup(c *call) *apiError {
	if _, ok := s.groups[groupKey(c)]; !ok {
		return groupNotFound(c.val("rg"))
	}
	return nil
}

// settled returns the resource id names after finishing any change to it in
// progress, or nil. The caller holds s.mu.
func (s *Server) settled(id string) *resource {
	res := s.resources[key(id)]
	if res != nil && res.pending != nil {
		s.finish(res.pending)
		res = s.resources[key(id)]
	}
	return res
}

// claim returns the resource id names, for a change a call makes, or nil
// when there is none. A change to it already in progress is resolved as the
// resource's provider resolves it: Microsoft.Network and private DNS links
// turn the call away while the other change runs, counting the call as a read
// of that change; Compute lets the call preempt it, unless it is a deletion,
// which verb, the name Compute gives the call's operation, is not allowed
// during. Other changes in progress finish first. The caller holds s.mu.
func (s *Server) claim(id, verb string) (*resource, *apiError) {
	res := s.resources[key(id)]
	if res == nil || res.pending == nil {
		return res, nil
	}
	op := res.pending
	switch res.kind {
	case nsgKind, nicKind, pipKind, vnetKind, subnetKind, natKind, linkKind:
		s.observe(op)
		if op.complete == nil {
			break
		}
		if res.kind == linkKind {
			return nil, linkOperationPending(res, op)
		}
		return nil, anotherOperation(res, op)
	case vmKind, vmssKind:
		if res.state == stateDeleting {
			return nil, markedForDeletion(res, verb)
		}
		s.preempt(op)
	default:
		s.finish(op)
	}
	return s.resources[key(id)], nil
}

// deletion returns the deletion of the resource id in progress, if any. The caller holds s.mu.
func (s *Server) deletion(id string) *operation {
	if res := s.resources[key(id)]; res != nil && res.state == stateDeleting {
		return res.pending
	}
	return nil
}

// anotherOperation turns away a change to the network resource res while op changes it.
func anotherOperation(res *resource, op *operation) *apiError {
	verb := "Put"
	if res.state == stateDeleting {
		verb = "Delete"
	}
	e := newError(http.StatusTooManyRequests, "RetryableError", "A retryable error occurred.")
	e.Details = []errorDetail{{
		Code: "RetryableErrorDueToAnotherOperation",
		Message: fmt.Sprintf("Operation %s%sOperation (%s) is updating resource %s. The call can be retried in 1 seconds.",
			verb, res.kind.opName, op.id, res.id),
	}}
	e.Header = http.Header{"Retry-After-Ms": {retryAfterMS}}
	return e
}

// markedForDeletion turns away the Compute operation verb on res while res is being deleted.
func markedForDeletion(res *resource, verb string) *apiError {
	what := "VM"
	if res.kind == vmssKind {
		what = "VM Scale Set"
	}
	return newError(http.StatusConflict, "OperationNotAllowed",
		"Operation '%s' is not allowed on %s '%s' since the %s is marked for deletion. You can only retry the Delete operation (or wait for an ongoing one to complete).",
		verb, what, res.name, what)
}

// read returns the resource id names, counting the read toward a change in
// progress, or nil. The caller holds s.mu.
func (s *Server) read(id string) *resource {
	if res := s.resources[key(id)]; res != nil {
		s.observe(res.pending)
	}
	return s.resources[key(id)]
}

// parsed is a resource body as the server records it.
type parsed struct {
	location   string
	tags       map[string]string
	top, props map[string]any
}

// parseBody reads a create or update of a resource of kind k, rejecting
// members the server does not know.
func parseBody(c *call, k *kind) (*parsed, *apiError) {
	body, err := readObject(c.r)
	if err != nil {
		return nil, err
	}
	known := slices.Concat([]string{"id", "name", "type", "etag", "systemData", "tags", "properties"}, k.top, k.topReadOnly)
	if k.located {
		known = append(known, "location")
	}
	if err := checkMembers(body, k.typeName, "", known...); err != nil {
		return nil, err
	}
	p := &parsed{top: map[string]any{}}
	if k.located {
		p.location = normalizeLocation(str(body, "location"))
		if p.location == "" {
			return nil, locationRequired()
		}
		if k.location != "" && p.location != k.location {
			return nil, unregisteredLocation(k, p.location)
		}
	}
	if p.tags, err = tagsOf(body); err != nil {
		return nil, err
	}
	for _, name := range k.top {
		if v, ok := body[name]; ok && v != nil {
			p.top[name] = v
		}
	}
	props, err := object(body, "properties")
	if err != nil {
		return nil, err
	}
	if err := checkMembers(props, k.typeName+"Properties", "properties", slices.Concat(k.props, k.readOnly)...); err != nil {
		return nil, err
	}
	p.props = map[string]any{}
	for name, v := range props {
		if slices.Contains(k.props, name) && v != nil {
			p.props[name] = v
		}
	}
	return p, nil
}

// checkPrecondition applies the If-Match and If-None-Match headers to a
// change of existing, which is nil when the resource does not exist.
func checkPrecondition(r *http.Request, existing *resource) *apiError {
	if r.Header.Get("If-None-Match") == "*" && existing != nil {
		return preconditionFailed()
	}
	if m := r.Header.Get("If-Match"); m != "" && (existing == nil || m != "*" && m != existing.etag) {
		return preconditionFailed()
	}
	return nil
}

// newResource makes a resource of kind k from p, to be stored at id. The caller holds s.mu.
func (s *Server) newResource(c *call, k *kind, id string, p *parsed) *resource {
	res := &resource{
		kind: k, id: id, name: id[strings.LastIndex(id, "/")+1:], sub: c.val("sub"), group: c.val("rg"),
		loc: p.location, tags: p.tags, top: p.top, props: p.props, state: stateSucceeded, created: s.now(), guid: s.newUUID(),
	}
	s.touch(res)
	return res
}

// touch gives res a new ETag. The caller holds s.mu.
func (s *Server) touch(res *resource) {
	if res.kind.etag != nil {
		res.etag = res.kind.etag(s.newUUID())
	}
}

// update records p as the new body of res. The caller holds s.mu.
func (s *Server) update(res *resource, p *parsed) {
	res.tags, res.top, res.props = p.tags, p.top, p.props
	s.touch(res)
}

// envelope renders the members every resource has, and its properties.
func envelope(res *resource) (map[string]any, map[string]any) {
	m := map[string]any{"id": res.id, "name": res.name, "type": res.kind.typ}
	if res.kind.located {
		m["location"] = res.loc
	}
	if res.tags != nil {
		m["tags"] = res.tags
	}
	if res.etag != "" {
		m["etag"] = res.etag
	}
	maps.Copy(m, res.top)
	props := maps.Clone(res.props)
	props["provisioningState"] = res.state
	m["properties"] = props
	return m, props
}

// change starts an operation on res that shows it in state until it
// finishes, then Succeeded; done, when set, applies the rest of the effect.
// The caller holds s.mu.
func (s *Server) change(c *call, res *resource, paths opPaths, resultStatus int, state string, done func()) *operation {
	op := s.begin(c, paths, resultStatus, func() *apiError {
		res.state, res.pending = stateSucceeded, nil
		if done != nil {
			done()
		}
		return nil
	})
	op.target, op.state = res.id, state
	if op.status == opInProgress {
		res.state, res.pending = state, op
	}
	return op
}

// putState is the state a create or update shows while in progress.
func putState(status int) string {
	if status == http.StatusCreated {
		return stateCreating
	}
	return stateUpdating
}

// withState renders res as if its provisioning state were state, for the
// answer to a change that fails without taking effect.
func withState(res *resource, state string, render func() []byte) []byte {
	saved := res.state
	res.state = state
	defer func() { res.state = saved }()
	return render()
}

// remove deletes res and what it holds. The caller holds s.mu.
func (s *Server) remove(res *resource) {
	delete(s.resources, key(res.id))
	switch {
	case res.nic != nil:
		s.releaseNIC(res)
	case res.pip != nil:
		delete(s.publicIPs, res.pip.address)
	case res.vm != nil:
		s.detachNICs(res, true)
	case res.scale != nil:
		for _, inst := range res.scale.instances {
			s.releaseInstance(inst)
		}
	case res.account != nil:
		s.removeAccountChildren(res)
	case res.held != nil:
		s.releasePrivate(res.held.subnet, res.held.address)
	case res.kind == cognitiveAccountKind:
		s.softDelete(res)
	case res.pe != nil:
		if nic := s.resources[key(res.pe.nic)]; nic != nil {
			s.remove(nic)
		}
	case res.vnet != nil:
		for _, sn := range s.subnetsOf(res) {
			s.remove(sn)
		}
	}
}

// listed returns the resources of kind k in the call's group, ordered by name,
// counting the read toward changes in progress. The caller holds s.mu.
func (s *Server) listed(c *call, k *kind) []*resource {
	gk := groupKey(c)
	observe := s.observer()
	var out []*resource
	for _, res := range s.resources {
		if res.kind == k && res.inGroup(gk) {
			observe(res.pending)
		}
	}
	for _, res := range s.resources {
		if res.kind == k && res.inGroup(gk) {
			out = append(out, res)
		}
	}
	slices.SortFunc(out, func(a, b *resource) int { return cmp.Compare(key(a.name), key(b.name)) })
	return out
}

// listBody renders a list response.
func listBody[T any](items []T, render func(T) any) []byte {
	value := make([]any, 0, len(items))
	for _, it := range items {
		value = append(value, render(it))
	}
	return encode(map[string]any{"value": value})
}

// Resource is a snapshot of a resource the server holds.
type Resource struct {
	ID                string
	Type              string
	Location          string
	Tags              map[string]string
	ProvisioningState string
}

// Resources returns every resource the server holds, scale set VMs included,
// ordered by ID.
func (s *Server) Resources() []Resource {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []Resource
	for _, res := range s.resources {
		out = append(out, Resource{ID: res.id, Type: res.kind.typ, Location: res.loc, Tags: maps.Clone(res.tags), ProvisioningState: res.state})
		if res.scale != nil {
			for _, inst := range res.scale.instances {
				out = append(out, Resource{
					ID: inst.id(res), Type: instanceType, Location: res.loc, Tags: maps.Clone(res.tags), ProvisioningState: inst.state,
				})
			}
		}
	}
	slices.SortFunc(out, func(a, b Resource) int { return cmp.Compare(a.ID, b.ID) })
	return out
}
